package upstream

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// legacyServer speaks only TLS 1.0 with RSA key exchange, like an old switch.
func legacyServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "Rapid Logic/1.1")
		_, _ = io.WriteString(w, "hello from "+r.URL.Path)
	}))
	srv.TLS = &tls.Config{
		MinVersion:   tls.VersionTLS10,
		MaxVersion:   tls.VersionTLS10,
		CipherSuites: []uint16{tls.TLS_RSA_WITH_AES_128_CBC_SHA},
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func TestLegacyTLSAutoFallbackAndPinning(t *testing.T) {
	srv := legacyServer(t)
	pin := Fingerprint(srv.Certificate().Raw)
	var learned string
	c, err := New(Options{BaseURL: srv.URL, TLSMode: "auto", Pin: pin, Timeout: 5 * time.Second,
		OnProfile: func(p string) { learned = p }})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{Transport: c.Transport}).Get(srv.URL + "/index.html")
	if err != nil {
		t.Fatalf("legacy TLS 1.0 not reachable: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "hello from /index.html" || resp.Header.Get("Server") != "Rapid Logic/1.1" {
		t.Fatalf("unexpected response %q", body)
	}
	if learned != ProfileLegacy || c.Profile() != ProfileLegacy {
		t.Fatalf("profile not learned: %q %q", learned, c.Profile())
	}
	if st := c.ConnectionState(); st == nil || st.Version != tls.VersionTLS10 {
		t.Fatalf("connection state %+v", st)
	}

	// Modern-only mode must not fall back.
	modern, _ := New(Options{BaseURL: srv.URL, TLSMode: "modern", Pin: pin, Timeout: 5 * time.Second})
	if _, err := (&http.Client{Transport: modern.Transport}).Get(srv.URL); err == nil {
		t.Fatal("modern mode connected to a TLS 1.0 server")
	}

	// Wrong pin and missing pin are refused, without falling back.
	wrong, _ := New(Options{BaseURL: srv.URL, TLSMode: "legacy", Pin: strings.Repeat("0", 64), Timeout: 5 * time.Second})
	_, err = (&http.Client{Transport: wrong.Transport}).Get(srv.URL)
	var pm *PinMismatchError
	if !errors.As(err, &pm) || pm.Got != pin {
		t.Fatalf("wrong pin: %v", err)
	}
	none, _ := New(Options{BaseURL: srv.URL, TLSMode: "auto", Timeout: 5 * time.Second})
	if _, err := (&http.Client{Transport: none.Transport}).Get(srv.URL); !errors.Is(err, ErrNotPinned) {
		t.Fatalf("missing pin: %v", err)
	}
}

func TestProbe(t *testing.T) {
	srv := legacyServer(t)
	addr := strings.TrimPrefix(srv.URL, "https://")
	res := Probe(context.Background(), addr, "127.0.0.1", 3*time.Second)
	if res.Best == nil || res.Best.Version != "TLS 1.0" || res.Best.Profile != ProfileLegacy ||
		res.Best.Cipher != "TLS_RSA_WITH_AES_128_CBC_SHA" {
		t.Fatalf("best %+v, attempts %+v", res.Best, res.Attempts)
	}
	if res.Cert == nil || res.Cert.Fingerprint != Fingerprint(srv.Certificate().Raw) || res.Cert.KeyBits < 1024 {
		t.Fatalf("cert %+v", res.Cert)
	}
}

func TestMalformedStatusLine(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			buf := make([]byte, 4096)
			_, _ = conn.Read(buf)
			_, _ = io.WriteString(conn, "HTTP/1.1 205.1 Change Password\r\nServer: RapidLogic/1.1\r\nContent-Length: 2\r\n\r\nok")
			conn.Close()
		}
	}()
	c, err := New(Options{BaseURL: "http://" + ln.Addr().String(), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{Transport: c.Transport}).Post("http://"+ln.Addr().String()+"/", "application/x-www-form-urlencoded",
		strings.NewReader("encoded=02RW:x"))
	if err != nil {
		t.Fatalf("malformed status line not tolerated: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 205 || ReasonPhrase(resp) != "Change Password" {
		t.Fatalf("status %q", resp.Status)
	}
}

func TestIsHandshakeIncompatibility(t *testing.T) {
	if IsHandshakeIncompatibility(ErrNotPinned) || IsHandshakeIncompatibility(&PinMismatchError{}) {
		t.Fatal("certificate errors must not trigger a fallback")
	}
	if !IsHandshakeIncompatibility(io.EOF) {
		t.Fatal("EOF during handshake should trigger a fallback")
	}
}
