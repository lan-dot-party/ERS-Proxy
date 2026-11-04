package health

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lan-dot-party/ers-proxy/internal/config"
	"github.com/lan-dot-party/ers-proxy/internal/store"
	"github.com/lan-dot-party/ers-proxy/internal/upstream"
)

func TestEmbeddable(t *testing.T) {
	origin := "https://ers.example.org"
	cases := []struct {
		h    http.Header
		want bool
	}{
		{http.Header{}, true},
		{http.Header{"X-Frame-Options": {"DENY"}}, false},
		{http.Header{"X-Frame-Options": {"sameorigin"}}, false},
		{http.Header{"Content-Security-Policy": {"default-src 'self'; frame-ancestors 'self'"}}, false},
		{http.Header{"Content-Security-Policy": {"frame-ancestors https://ers.example.org"}}, true},
		{http.Header{"Content-Security-Policy": {"frame-ancestors https://*.example.org"}}, true},
		{http.Header{"Content-Security-Policy": {"frame-ancestors *"}}, true},
		{http.Header{"Content-Security-Policy": {"default-src 'self'"}}, true},
	}
	for _, c := range cases {
		if got := Embeddable(c.h, origin); got != c.want {
			t.Errorf("%v: got %v", c.h, got)
		}
	}
}

func TestRound(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(store.Options{Path: filepath.Join(t.TempDir(), "t.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := db.Migrate(ctx, log); err != nil {
		t.Fatal(err)
	}
	link := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
	}))
	defer link.Close()
	sw := httptest.NewUnstartedServer(http.NotFoundHandler())
	sw.TLS = &tls.Config{MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS10,
		CipherSuites: []uint16{tls.TLS_RSA_WITH_AES_128_CBC_SHA}}
	sw.StartTLS()
	defer sw.Close()
	pin := upstream.Fingerprint(sw.Certificate().Raw)

	add := func(d *store.Device) int64 {
		d.HealthEnabled = true
		if d.OpenMode == "" {
			d.OpenMode, d.Isolation, d.TLSMode = store.OpenInherit, store.IsolationSandbox, store.TLSAuto
		}
		if err := db.CreateDevice(ctx, d); err != nil {
			t.Fatal(err)
		}
		return d.ID
	}
	linkID := add(&store.Device{Name: "fw", Kind: store.KindLink, URL: link.URL + "/"})
	okID := add(&store.Device{Name: "sw", Kind: store.KindERS, URL: sw.URL, TLSPin: pin})
	certID := add(&store.Device{Name: "sw-changed", Kind: store.KindERS, URL: sw.URL, TLSPin: strings.Repeat("ab", 32)})
	downID := add(&store.Device{Name: "down", Kind: store.KindERS, URL: "http://127.0.0.1:1"})

	cfg := config.Default()
	cfg.PublicURL = "https://ers.example.org"
	cfg.Health.Timeout = 3 * time.Second
	c := New(db, cfg, log)
	c.Round(ctx)
	if s, emb := c.Status(linkID); s != StateUp || emb {
		t.Errorf("link: %s embeddable=%v", s, emb)
	}
	if s, _ := c.Status(okID); s != StateUp {
		d, _ := c.Detail(okID)
		t.Errorf("legacy TLS switch: %s %s", s, d.Detail)
	}
	if s, _ := c.Status(certID); s != StateCert {
		t.Errorf("changed certificate: %s", s)
	}
	if s, _ := c.Status(downID); s != StateDown {
		t.Errorf("unreachable: %s", s)
	}
	if s, emb := c.Status(999); s != StateUnknown || !emb {
		t.Errorf("unknown device: %s %v", s, emb)
	}
}

// An edm-direct device is checked like an ERS switch (TCP connect), not with
// an HTTP request like a link.
func TestRoundEDMDirectNoHTTP(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(store.Options{Path: filepath.Join(t.TempDir(), "t.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := db.Migrate(ctx, log); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var got atomic.Int64
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_ = c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
				n, _ := c.Read(make([]byte, 512))
				got.Add(int64(n))
			}()
		}
	}()
	defer ln.Close()
	d := &store.Device{Name: "vsp", Kind: store.KindEDMDirect, URL: "http://" + ln.Addr().String(), HealthEnabled: true,
		OpenMode: store.OpenInherit, Isolation: store.IsolationSandbox, TLSMode: store.TLSAuto}
	if err := db.CreateDevice(ctx, d); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Health.Timeout = 3 * time.Second
	c := New(db, cfg, log)
	c.Round(ctx)
	if s, _ := c.Status(d.ID); s != StateUp {
		t.Fatalf("edm-direct: %s", s)
	}
	time.Sleep(700 * time.Millisecond)
	if got.Load() != 0 {
		t.Fatalf("health check sent %d bytes; expected a plain TCP connect", got.Load())
	}
}
