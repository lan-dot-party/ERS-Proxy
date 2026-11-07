package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/lan-dot-party/ers-proxy/internal/app"
	"github.com/lan-dot-party/ers-proxy/internal/devices"
	"github.com/lan-dot-party/ers-proxy/internal/edmproxy"
	"github.com/lan-dot-party/ers-proxy/internal/monitor"
	"github.com/lan-dot-party/ers-proxy/internal/monitor/edmsrc"
	"github.com/lan-dot-party/ers-proxy/internal/monitor/swossrc"
	"github.com/lan-dot-party/ers-proxy/internal/secret"
	"github.com/lan-dot-party/ers-proxy/internal/store"
	"github.com/lan-dot-party/ers-proxy/internal/swos"
	"github.com/lan-dot-party/ers-proxy/internal/web"
)

// edmSessions keeps one logged-in EDM session per device for the dashboard's
// EDM data source, so that a poll does not log in every time.
type edmSessions struct {
	proxy *edmproxy.Proxy
	key   *secret.Key
	mu    sync.Mutex
	m     map[int64]edmEntry
}

type edmEntry struct {
	sig string
	s   *edmsrc.Session
}

// open implements poller.OpenFunc.
func (e *edmSessions) open(ctx context.Context, d *store.Device) (monitor.Reader, func(), error) {
	s, err := e.session(ctx, d)
	if err != nil {
		return nil, nil, err
	}
	return edmsrc.Reader{S: s}, func() {}, nil
}

// session returns the device's shared EDM session, creating it (and logging
// out a session that settings changes made obsolete) when needed.
func (e *edmSessions) session(ctx context.Context, d *store.Device) (*edmsrc.Session, error) {
	if !d.EDM() {
		return nil, errors.New("the EDM data source needs an EDM switch (kind ers-edm, vsp-edm or edm-direct)")
	}
	if !d.HasCredentials() {
		return nil, errors.New("the EDM data source needs a stored EDM login (device settings)")
	}
	c, err := e.proxy.Client(d)
	if err != nil {
		return nil, err
	}
	sig := fmt.Sprintf("%s|%s|%x", d.URL, d.TLSPin, d.Credentials)
	e.mu.Lock()
	defer e.mu.Unlock()
	ent, ok := e.m[d.ID]
	if ok && ent.sig == sig {
		return ent.s, nil
	}
	if ok {
		// Settings or login changed: free the old session on the switch.
		go func(old *edmsrc.Session) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			old.Logout(ctx)
		}(ent.s)
	}
	cred, err := devices.DecryptCredentials(e.key, d)
	if err != nil {
		return nil, err
	}
	s := edmsrc.NewSession(c.Transport, c.Base().String(), cred.Username, cred.Password)
	e.m[d.ID] = edmEntry{sig: sig, s: s}
	return s, nil
}

// newProxy builds the EDM proxy and connects it to the UI.
// It also serves the EDM channel of configuration changes, so both share one
// session per switch.
func newEDMSource(p *edmproxy.Proxy, key *secret.Key) *edmSessions {
	return &edmSessions{proxy: p, key: key, m: map[int64]edmEntry{}}
}

// newSwOSSource returns the poller.OpenFunc of the SwOS data source. It uses
// the proxy's client of the device, which logs in with the stored web login
// (HTTP Digest); SwOS has no sessions to keep.
func newSwOSSource(p *edmproxy.Proxy) func(ctx context.Context, d *store.Device) (monitor.Reader, func(), error) {
	return func(_ context.Context, d *store.Device) (monitor.Reader, func(), error) {
		if !d.SwOS() {
			return nil, nil, errors.New("the SwOS data source needs a device of kind swos")
		}
		if !d.HasCredentials() {
			return nil, nil, errors.New("the SwOS data source needs a stored web login (device settings)")
		}
		c, err := p.Client(d)
		if err != nil {
			return nil, nil, err
		}
		return &swossrc.Reader{C: &swos.Client{Transport: c.Transport, Base: c.Base().String()}}, func() {}, nil
	}
}

func newProxy(ctx context.Context, a *app.App, ui *web.Server) (*edmproxy.Proxy, error) {
	p := edmproxy.New(ctx, a)
	ui.Opener = p
	ui.OnDeviceChange = p.ForgetDevice
	ui.Diagnostics = func(d *store.Device) []web.DiagRow {
		dg := p.Diagnostics(d.ID)
		if dg == nil {
			return nil
		}
		rows := []web.DiagRow{{Label: "diag.last_opened", Value: dg.LastOpened.Local().Format(time.DateTime)}}
		if dg.Server != "" {
			rows = append(rows, web.DiagRow{Label: "diag.server", Value: dg.Server})
		}
		if dg.TLSVersion != "" {
			old := dg.TLSVersion != "TLS 1.2" && dg.TLSVersion != "TLS 1.3"
			rows = append(rows, web.DiagRow{Label: "diag.tls", Value: fmt.Sprintf("%s, %s (%s)", dg.TLSVersion, dg.TLSCipher, dg.TLSProfile), Warn: old})
		}
		var applied []string
		for _, n := range dg.PatchNames() {
			applied = append(applied, fmt.Sprintf("%s ×%d", n, dg.Patches[n]))
		}
		if len(applied) == 0 {
			applied = []string{"–"}
		}
		rows = append(rows, web.DiagRow{Label: "diag.patches", Value: strings.Join(applied, ", ")})
		if len(dg.Ambiguous) > 0 {
			var amb []string
			for n := range dg.Ambiguous {
				amb = append(amb, n)
			}
			rows = append(rows, web.DiagRow{Label: "diag.ambiguous", Value: strings.Join(amb, ", "), Warn: true})
		}
		rows = append(rows, web.DiagRow{Label: "diag.requests", Value: fmt.Sprint(dg.Requests)})
		if !dg.AutoLogin.IsZero() {
			rows = append(rows, web.DiagRow{Label: "diag.auto_login", Value: dg.AutoLogin.Local().Format(time.DateTime)})
		}
		if dg.LastError != "" {
			rows = append(rows, web.DiagRow{Label: "diag.last_error",
				Value: dg.LastErrorAt.Local().Format(time.DateTime) + ": " + dg.LastError, Warn: true})
		}
		return rows
	}
	return p, nil
}
