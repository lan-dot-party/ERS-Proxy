package upstream

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sync"
	"time"
)

// Options configure a Client.
type Options struct {
	// BaseURL is scheme://host[:port] of the device.
	BaseURL string
	// TLSMode is auto, modern or legacy (https only).
	TLSMode string
	// Pin is the hex SHA-256 of the pinned certificate.
	Pin string
	// Profile is the TLS profile that worked last ("" = unknown).
	Profile string
	// MaxConns limits parallel connections to the device.
	MaxConns int
	// Timeout is the dial, handshake and response header timeout.
	Timeout time.Duration
	// OnProfile is called when auto mode settles on another profile.
	OnProfile func(profile string)
	// KeepAlive sends "Connection: keep-alive" instead of "Connection:
	// close"; the client still closes the connection after each answer. The
	// web server of VSP switches (VOSS) resets a connection it is asked to
	// close right after the response headers, which loses the body.
	KeepAlive bool
}

// Client talks to one device.
type Client struct {
	opts Options
	base *url.URL
	// Transport sends requests to the device. Reads that the device drops
	// (EOF, reset) are repeated a few times, see retry.go.
	Transport http.RoundTripper

	mu      sync.Mutex
	profile string
	state   *tls.ConnectionState
}

// New creates a client for one device.
func New(opts Options) (*Client, error) {
	u, err := url.Parse(opts.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("invalid device address %q", opts.BaseURL)
	}
	if opts.MaxConns <= 0 {
		opts.MaxConns = 4
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 30 * time.Second
	}
	c := &Client{opts: opts, base: u, profile: opts.Profile}
	switch opts.TLSMode {
	case "modern":
		c.profile = ProfileModern
	case "legacy":
		c.profile = ProfileLegacy
	}
	raw := &http.Transport{
		Proxy:                 nil, // never through an environment proxy
		DialContext:           c.dialPlain,
		DialTLSContext:        c.dialTLS,
		MaxConnsPerHost:       opts.MaxConns,
		MaxIdleConnsPerHost:   -1,
		DisableKeepAlives:     !opts.KeepAlive, // old embedded web servers; also lets fixConn see each status line
		DisableCompression:    true,
		ResponseHeaderTimeout: opts.Timeout,
		TLSHandshakeTimeout:   opts.Timeout,
		ForceAttemptHTTP2:     false,
		TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{},
	}
	// MaxIdleConnsPerHost -1 keeps no idle connection either way, so even with
	// KeepAlive every request gets its own connection.
	var next http.RoundTripper = raw
	if opts.KeepAlive {
		next = keepAliveTransport{raw}
	}
	c.Transport = &retryTransport{next: next, delays: retryDelays, fast: fastDrop}
	return c, nil
}

// Base returns the device's base URL.
func (c *Client) Base() *url.URL { return c.base }

// Profile returns the TLS profile in use ("" before the first HTTPS connection).
func (c *Client) Profile() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.profile
}

// ConnectionState returns the TLS state of the last connection, if any.
func (c *Client) ConnectionState() *tls.ConnectionState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

func (c *Client) dialer() *net.Dialer {
	return &net.Dialer{Timeout: c.opts.Timeout, KeepAlive: 30 * time.Second}
}

func (c *Client) dialPlain(ctx context.Context, network, addr string) (net.Conn, error) {
	conn, err := c.dialer().DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	return &fixConn{Conn: conn}, nil
}

// dialTLS connects with the remembered profile. In auto mode it tries the
// modern profile first and falls back to legacy when the stacks do not agree.
func (c *Client) dialTLS(ctx context.Context, network, addr string) (net.Conn, error) {
	profiles := []string{ProfileModern, ProfileLegacy}
	switch c.Profile() {
	case ProfileModern:
		if c.opts.TLSMode == "modern" {
			profiles = []string{ProfileModern}
		}
	case ProfileLegacy:
		profiles = []string{ProfileLegacy}
		if c.opts.TLSMode != "legacy" {
			profiles = append(profiles, ProfileModern) // the device may have been upgraded
		}
	}
	var firstErr error
	for _, p := range profiles {
		conn, err := c.handshake(ctx, network, addr, p)
		if err == nil {
			c.mu.Lock()
			changed := c.profile != p
			c.profile = p
			st := conn.ConnectionState()
			c.state = &st
			c.mu.Unlock()
			if changed && c.opts.OnProfile != nil {
				c.opts.OnProfile(p)
			}
			return &fixConn{Conn: conn}, nil
		}
		if firstErr == nil {
			firstErr = err
		}
		if !IsHandshakeIncompatibility(err) {
			return nil, err
		}
	}
	return nil, firstErr
}

func (c *Client) handshake(ctx context.Context, network, addr, profile string) (*tls.Conn, error) {
	raw, err := c.dialer().DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	conn := tls.Client(raw, TLSConfig(profile, c.base.Hostname(), c.opts.Pin))
	hctx, cancel := context.WithTimeout(ctx, c.opts.Timeout)
	defer cancel()
	if err := conn.HandshakeContext(hctx); err != nil {
		raw.Close()
		return nil, err
	}
	return conn, nil
}

// --- tolerant status line ---

// badStatus matches status lines with a code Go's parser rejects, e.g.
// "HTTP/1.1 205.1 Change Password" (seen in ERS firmware strings).
var badStatus = regexp.MustCompile(`^(HTTP/1\.[01]) (\d{3})\.\d+( .*)?$`)

// fixConn rewrites a malformed status line at the start of the response into
// a valid one ("HTTP/1.1 205 Change Password"). With keep-alive disabled
// there is one response per connection, so only the first line matters.
type fixConn struct {
	net.Conn
	once sync.Once
	r    io.Reader
	err  error
}

func (c *fixConn) Read(p []byte) (int, error) {
	c.once.Do(func() {
		br := bufio.NewReaderSize(c.Conn, 1024)
		line, err := br.ReadSlice('\n')
		if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
			c.r = io.MultiReader(bytes.NewReader(append([]byte(nil), line...)), br)
			return
		}
		trimmed := string(bytes.TrimRight(line, "\r\n"))
		if m := badStatus.FindStringSubmatch(trimmed); m != nil {
			line = []byte(m[1] + " " + m[2] + m[3] + "\r\n")
		} else {
			line = append([]byte(nil), line...)
		}
		c.r = io.MultiReader(bytes.NewReader(line), br)
	})
	if c.r == nil {
		return 0, c.err
	}
	return c.r.Read(p)
}

// ReasonPhrase returns the reason phrase of a response if it differs from the
// standard text (EDM checks "Change Password" after a 205).
func ReasonPhrase(resp *http.Response) string {
	if len(resp.Status) <= 4 {
		return ""
	}
	reason := resp.Status[4:]
	if reason == http.StatusText(resp.StatusCode) {
		return ""
	}
	return reason
}

// keepAliveTransport asks the device to keep the connection open (see
// Options.KeepAlive).
type keepAliveTransport struct{ next http.RoundTripper }

func (t keepAliveTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header.Set("Connection", "keep-alive")
	return t.next.RoundTrip(r)
}
