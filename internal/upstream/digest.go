package upstream

import (
	"bytes"
	"crypto/md5" //nolint:gosec // HTTP Digest (RFC 7616) as implemented by MikroTik SwOS uses MD5
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"strings"
	"sync"
)

// maxDigestReplay is the largest request body the digest transport keeps in
// memory so that it can repeat the request after a new challenge.
const maxDigestReplay = 8 << 20

// Login is a user name and password for HTTP authentication at the device.
type Login struct {
	Username, Password string
}

// digestTransport answers HTTP Digest challenges (RFC 7616, qop "auth") with
// the stored login. The browser never sees the challenge or the password:
// requests are signed on the server. After the first challenge, requests are
// signed in advance with the last nonce; a 401 with a new nonce is answered
// once.
type digestTransport struct {
	next  http.RoundTripper
	login Login

	mu   sync.Mutex
	chal *digestChallenge
	nc   uint32
}

type digestChallenge struct {
	realm, nonce, opaque, algorithm string
	qopAuth                         bool
}

// WithDigest wraps a transport so that it logs in with HTTP Digest.
func WithDigest(next http.RoundTripper, l Login) http.RoundTripper {
	return &digestTransport{next: next, login: l}
}

func (t *digestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req, err := replayable(req)
	if err != nil {
		return nil, err
	}
	signed := t.sign(req)
	resp, err := t.next.RoundTrip(signed)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	chal, ok := parseDigest(resp.Header.Values("Www-Authenticate"))
	if !ok {
		return resp, nil // not a digest challenge: let the caller see the 401
	}
	t.mu.Lock()
	old := t.chal
	t.chal, t.nc = chal, 0
	t.mu.Unlock()
	// The same nonce was used and refused: the login is wrong. Repeating it
	// would only count as another failed attempt on the device.
	if old != nil && old.nonce == chal.nonce && signed.Header.Get("Authorization") != "" {
		return resp, nil
	}
	if req.Body != nil && req.GetBody == nil {
		return resp, nil // body too large to repeat
	}
	again := req.Clone(req.Context())
	if req.GetBody != nil {
		if again.Body, err = req.GetBody(); err != nil {
			return resp, nil
		}
	}
	drain(resp)
	return t.next.RoundTrip(t.sign(again))
}

// replayable buffers a small request body so that the request can be sent a
// second time.
func replayable(req *http.Request) (*http.Request, error) {
	if req.Body == nil || req.Body == http.NoBody || req.GetBody != nil {
		return req, nil
	}
	if req.ContentLength < 0 || req.ContentLength > maxDigestReplay {
		return req, nil
	}
	data, err := io.ReadAll(io.LimitReader(req.Body, maxDigestReplay+1))
	req.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("read request body: %w", err)
	}
	if len(data) > maxDigestReplay {
		return nil, errors.New("request body too large")
	}
	out := req.Clone(req.Context())
	out.Body = io.NopCloser(bytes.NewReader(data))
	out.ContentLength = int64(len(data))
	out.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }
	return out, nil
}

// sign returns req with an Authorization header for the current challenge
// (req itself if there is none yet).
func (t *digestTransport) sign(req *http.Request) *http.Request {
	t.mu.Lock()
	chal := t.chal
	if chal == nil {
		t.mu.Unlock()
		return req
	}
	t.nc++
	nc := t.nc
	t.mu.Unlock()
	out := req.Clone(req.Context())
	out.Header.Set("Authorization", chal.authorization(t.login, req.Method, req.URL.RequestURI(), nc, cnonce()))
	return out
}

func (c *digestChallenge) authorization(l Login, method, uri string, nc uint32, cn string) string {
	h := c.hash()
	ha1 := h(l.Username + ":" + c.realm + ":" + l.Password)
	if strings.EqualFold(c.algorithm, "MD5-sess") || strings.EqualFold(c.algorithm, "SHA-256-sess") {
		ha1 = h(ha1 + ":" + c.nonce + ":" + cn)
	}
	ha2 := h(method + ":" + uri)
	ncs := fmt.Sprintf("%08x", nc)
	var b strings.Builder
	fmt.Fprintf(&b, `Digest username=%q, realm=%q, nonce=%q, uri=%q`, l.Username, c.realm, c.nonce, uri)
	if c.qopAuth {
		fmt.Fprintf(&b, `, qop=auth, nc=%s, cnonce=%q, response=%q`, ncs, cn, h(ha1+":"+c.nonce+":"+ncs+":"+cn+":auth:"+ha2))
	} else {
		fmt.Fprintf(&b, `, response=%q`, h(ha1+":"+c.nonce+":"+ha2))
	}
	if c.algorithm != "" {
		b.WriteString(", algorithm=" + c.algorithm)
	}
	if c.opaque != "" {
		fmt.Fprintf(&b, `, opaque=%q`, c.opaque)
	}
	return b.String()
}

func (c *digestChallenge) hash() func(string) string {
	var newHash func() hash.Hash = md5.New
	if strings.HasPrefix(strings.ToUpper(c.algorithm), "SHA-256") {
		newHash = sha256.New
	}
	return func(s string) string {
		h := newHash()
		_, _ = io.WriteString(h, s)
		return hex.EncodeToString(h.Sum(nil))
	}
}

func cnonce() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// parseDigest reads the first Digest challenge with a supported algorithm.
func parseDigest(headers []string) (*digestChallenge, bool) {
	for _, h := range headers {
		scheme, rest, _ := strings.Cut(strings.TrimSpace(h), " ")
		if !strings.EqualFold(scheme, "Digest") {
			continue
		}
		p := authParams(rest)
		c := &digestChallenge{realm: p["realm"], nonce: p["nonce"], opaque: p["opaque"], algorithm: p["algorithm"]}
		switch strings.ToUpper(c.algorithm) {
		case "", "MD5", "MD5-SESS", "SHA-256", "SHA-256-SESS":
		default:
			continue
		}
		for _, q := range strings.Split(p["qop"], ",") {
			if strings.EqualFold(strings.TrimSpace(q), "auth") {
				c.qopAuth = true
			}
		}
		if p["qop"] != "" && !c.qopAuth {
			continue // only auth-int offered
		}
		if c.nonce == "" {
			continue
		}
		return c, true
	}
	return nil, false
}

// authParams parses `a="x", b=y` (quoted strings may contain commas).
func authParams(s string) map[string]string {
	out := map[string]string{}
	for len(s) > 0 {
		s = strings.TrimLeft(s, " ,\t")
		k, rest, ok := strings.Cut(s, "=")
		if !ok {
			break
		}
		k = strings.ToLower(strings.TrimSpace(k))
		rest = strings.TrimLeft(rest, " \t")
		var v string
		if strings.HasPrefix(rest, `"`) {
			var b strings.Builder
			i := 1
			for ; i < len(rest); i++ {
				c := rest[i]
				if c == '\\' && i+1 < len(rest) {
					i++
					b.WriteByte(rest[i])
					continue
				}
				if c == '"' {
					break
				}
				b.WriteByte(c)
			}
			v = b.String()
			if i < len(rest) {
				i++
			}
			s = rest[i:]
		} else {
			v, s, _ = strings.Cut(rest, ",")
			v = strings.TrimSpace(v)
		}
		out[k] = v
	}
	return out
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
}
