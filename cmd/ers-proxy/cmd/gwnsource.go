package cmd

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"

	"github.com/lan-dot-party/ers-proxy/internal/devices"
	"github.com/lan-dot-party/ers-proxy/internal/edmproxy"
	"github.com/lan-dot-party/ers-proxy/internal/gwn"
	"github.com/lan-dot-party/ers-proxy/internal/monitor"
	"github.com/lan-dot-party/ers-proxy/internal/monitor/gwnsrc"
	"github.com/lan-dot-party/ers-proxy/internal/monitor/poller"
	"github.com/lan-dot-party/ers-proxy/internal/secret"
	"github.com/lan-dot-party/ers-proxy/internal/store"
)

// gwnSources keeps one web API client per Grandstream GWN device for the
// dashboard's "gwn" data source. The client logs in and out within each poll
// (the switch allows one session per account) and remembers a rejected
// login, so a wrong password is tried only once (the switch locks the
// account after a few failed logins) until the stored login or the address
// changes.
type gwnSources struct {
	proxy *edmproxy.Proxy
	key   *secret.Key
	mu    sync.Mutex
	m     map[int64]gwnEntry
}

type gwnEntry struct {
	sig string
	c   *gwn.Client
}

func newGWNSource(p *edmproxy.Proxy, key *secret.Key) *gwnSources {
	return &gwnSources{proxy: p, key: key, m: map[int64]gwnEntry{}}
}

// open implements poller.OpenFunc: the web reader, combined with an SNMP
// reader for the counters when SNMP credentials are stored.
func (g *gwnSources) open(ctx context.Context, d *store.Device) (monitor.Reader, func(), error) {
	if !d.GWN() {
		return nil, nil, errors.New("the GWN data source needs a device of kind gwn")
	}
	if !d.HasCredentials() {
		return nil, nil, errors.New("the GWN data source needs a stored web login (device settings)")
	}
	c, err := g.client(d)
	if err != nil {
		return nil, nil, err
	}
	r := &gwnsrc.Reader{C: c}
	closeR := func() {}
	if d.HasSNMPCredentials() {
		creds, err := devices.DecryptSNMP(g.key, d)
		if err != nil {
			return nil, nil, fmt.Errorf("open the stored SNMP credentials: %w", err)
		}
		s, closeS, err := poller.OpenSNMP(ctx, d, creds)
		if err != nil {
			return nil, nil, err
		}
		r.SNMP, closeR = s, closeS
	}
	return r, closeR, nil
}

func (g *gwnSources) client(d *store.Device) (*gwn.Client, error) {
	h := sha256.Sum256(d.Credentials)
	sig := fmt.Sprintf("%s|%s|%s|%x", d.URL, d.TLSMode, d.TLSPin, h[:8])
	g.mu.Lock()
	defer g.mu.Unlock()
	if e, ok := g.m[d.ID]; ok && e.sig == sig {
		return e.c, nil
	}
	cred, err := devices.DecryptCredentials(g.key, d)
	if err != nil {
		return nil, fmt.Errorf("open the stored web login: %w", err)
	}
	uc, err := g.proxy.Client(d)
	if err != nil {
		return nil, err
	}
	c := &gwn.Client{Transport: uc.Transport, Base: uc.Base().String(), Username: cred.Username, Password: cred.Password}
	g.m[d.ID] = gwnEntry{sig: sig, c: c}
	return c, nil
}
