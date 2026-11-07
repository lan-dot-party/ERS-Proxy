// Package health checks periodically whether devices are reachable, whether
// an HTTPS switch still presents its pinned certificate, and whether a link
// device may be embedded in a frame.
//
// ERS switches are only probed on the TCP/TLS level: an HTTP request to their
// web server would create a new login nonce on the switch every time.
package health

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/lan-dot-party/ers-proxy/internal/config"
	"github.com/lan-dot-party/ers-proxy/internal/devices"
	"github.com/lan-dot-party/ers-proxy/internal/store"
	"github.com/lan-dot-party/ers-proxy/internal/upstream"
)

// States of a device.
const (
	StateUnknown = "unknown"
	StateUp      = "up"
	StateDown    = "down"
	// StateCert means: reachable, but the HTTPS certificate differs from the pin.
	StateCert = "cert"
)

// Status is the last result for a device.
type Status struct {
	State      string
	Embeddable bool
	Checked    time.Time
	Detail     string
}

// Checker runs the checks.
type Checker struct {
	db     *store.DB
	cfg    *config.Config
	log    *slog.Logger
	origin string // the portal's origin, for frame-ancestors checks

	mu     sync.Mutex
	status map[int64]Status
	wake   chan struct{}
}

// New creates a checker.
func New(db *store.DB, cfg *config.Config, log *slog.Logger) *Checker {
	return &Checker{db: db, cfg: cfg, log: log.With("component", "health"), origin: cfg.Origin(),
		status: map[int64]Status{}, wake: make(chan struct{}, 1)}
}

// Status implements web.StatusSource.
func (c *Checker) Status(id int64) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.status[id]
	if !ok {
		return StateUnknown, true
	}
	return s.State, s.Embeddable
}

// Detail returns the full status of a device.
func (c *Checker) Detail(id int64) (Status, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.status[id]
	return s, ok
}

// Trigger runs a round soon (after devices changed).
func (c *Checker) Trigger() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// Run checks all devices every health.interval until ctx ends.
func (c *Checker) Run(ctx context.Context) {
	if !c.cfg.Health.Enabled {
		return
	}
	t := time.NewTicker(c.cfg.Health.Interval)
	defer t.Stop()
	for {
		c.Round(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-c.wake:
			// Debounce bursts of changes (imports).
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
	}
}

// Round checks every device once, at most 8 at a time.
func (c *Checker) Round(ctx context.Context) {
	devs, err := c.db.ListDevices(ctx)
	if err != nil {
		c.log.Warn("list devices", "err", err)
		return
	}
	seen := map[int64]bool{}
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for _, d := range devs {
		seen[d.ID] = true
		if !d.HealthEnabled {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(d *store.Device) {
			defer wg.Done()
			defer func() { <-sem }()
			st := c.check(ctx, d)
			c.mu.Lock()
			prev := c.status[d.ID]
			c.status[d.ID] = st
			c.mu.Unlock()
			if prev.State != "" && prev.State != st.State {
				c.log.Info("device state changed", "device", d.Name, "from", prev.State, "to", st.State, "detail", st.Detail)
			}
		}(d)
	}
	wg.Wait()
	c.mu.Lock()
	for id := range c.status {
		if !seen[id] {
			delete(c.status, id)
		}
	}
	c.mu.Unlock()
}

func (c *Checker) check(ctx context.Context, d *store.Device) Status {
	st := Status{State: StateDown, Embeddable: true, Checked: time.Now()}
	if !d.Upstream() {
		return c.checkLink(ctx, d, st)
	}
	host, port := devices.HostPort(d.URL)
	addr := net.JoinHostPort(host, port)
	timeout := c.cfg.Health.Timeout
	if !strings.HasPrefix(d.URL, "https://") {
		conn, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", addr)
		if err != nil {
			st.Detail = err.Error()
			return st
		}
		conn.Close()
		st.State = StateUp
		return st
	}
	// HTTPS switch: a handshake shows reachability and the certificate.
	profiles := []string{upstream.ProfileModern, upstream.ProfileLegacy}
	if d.TLSProfile == upstream.ProfileLegacy || d.TLSMode == store.TLSLegacy {
		profiles = []string{upstream.ProfileLegacy}
	}
	var lastErr error
	for _, p := range profiles {
		cfg := upstream.TLSConfig(p, host, d.TLSPin)
		if d.TLSPin == "" {
			cfg.VerifyConnection = nil // not confirmed yet: only reachability
		}
		err := handshake(ctx, addr, cfg, timeout)
		var pm *upstream.PinMismatchError
		switch {
		case err == nil:
			st.State = StateUp
			return st
		case errors.As(err, &pm):
			st.State, st.Detail = StateCert, err.Error()
			return st
		}
		lastErr = err
		if !upstream.IsHandshakeIncompatibility(err) {
			break
		}
	}
	st.Detail = lastErr.Error()
	return st
}

func handshake(ctx context.Context, addr string, cfg *tls.Config, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	raw, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer raw.Close()
	return tls.Client(raw, cfg).HandshakeContext(ctx)
}

// checkLink requests the link's URL and looks at its framing headers.
func (c *Checker) checkLink(ctx context.Context, d *store.Device, st Status) Status {
	client := &http.Client{
		Timeout: c.cfg.Health.Timeout,
		Transport: &http.Transport{
			Proxy: nil,
			// Only reachability and headers are read and nothing is sent, so
			// self-signed certificates of appliances are accepted here.
			TLSClientConfig:   &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS10}, //nolint:gosec
			DisableKeepAlives: true,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.URL, nil)
	if err != nil {
		st.Detail = err.Error()
		return st
	}
	req.Header.Set("User-Agent", "ERS-Proxy health check")
	resp, err := client.Do(req)
	if err != nil {
		st.Detail = err.Error()
		return st
	}
	resp.Body.Close()
	st.State = StateUp
	st.Embeddable = Embeddable(resp.Header, c.origin)
	if !st.Embeddable {
		st.Detail = "the device forbids being shown in a frame"
	}
	return st
}

// Embeddable reports whether a response allows framing by origin.
func Embeddable(h http.Header, origin string) bool {
	if xfo := strings.ToUpper(strings.TrimSpace(h.Get("X-Frame-Options"))); xfo == "DENY" || xfo == "SAMEORIGIN" ||
		strings.HasPrefix(xfo, "ALLOW-FROM") {
		return false
	}
	for _, csp := range h.Values("Content-Security-Policy") {
		for _, dir := range strings.Split(csp, ";") {
			f := strings.Fields(strings.TrimSpace(dir))
			if len(f) == 0 || !strings.EqualFold(f[0], "frame-ancestors") {
				continue
			}
			ok := false
			for _, src := range f[1:] {
				src = strings.Trim(src, "'")
				if src == "*" || strings.EqualFold(strings.TrimRight(src, "/"), origin) || matchWildcard(src, origin) {
					ok = true
				}
			}
			if !ok {
				return false
			}
		}
	}
	return true
}

// matchWildcard handles sources like https://*.example.org.
func matchWildcard(src, origin string) bool {
	if !strings.Contains(src, "*.") {
		return false
	}
	su, err1 := url.Parse(strings.Replace(src, "*.", "wildcard.", 1))
	ou, err2 := url.Parse(origin)
	if err1 != nil || err2 != nil || su.Scheme != ou.Scheme {
		return false
	}
	suffix := strings.TrimPrefix(su.Hostname(), "wildcard")
	return strings.HasSuffix(ou.Hostname(), suffix)
}
