package upstream

import (
	"crypto/md5" //nolint:gosec // HTTP Digest test
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// TestDigestRFCExample checks the response against RFC 2617, section 3.5.
func TestDigestRFCExample(t *testing.T) {
	c := &digestChallenge{realm: "testrealm@host.com", nonce: "dcd98b7102dd2f0e8b11d0f600bfb0c093", qopAuth: true}
	h := c.authorization(Login{"Mufasa", "Circle Of Life"}, "GET", "/dir/index.html", 1, "0a4f113b")
	if !strings.Contains(h, `response="6629fae49393a05397450978507c4ef1"`) {
		t.Fatalf("wrong response: %s", h)
	}
	if !strings.Contains(h, "nc=00000001") || !strings.Contains(h, `cnonce="0a4f113b"`) {
		t.Fatalf("missing qop fields: %s", h)
	}
}

func TestParseDigest(t *testing.T) {
	c, ok := parseDigest([]string{`Basic realm="x"`, `Digest realm="CSS305-1G-4S+", qop="auth", nonce="79b4c90a", stale=FALSE`})
	if !ok || c.realm != "CSS305-1G-4S+" || c.nonce != "79b4c90a" || !c.qopAuth {
		t.Fatalf("parse: %+v %v", c, ok)
	}
	if _, ok := parseDigest([]string{`Digest realm="r", nonce="n", algorithm=SHA-512`}); ok {
		t.Fatal("unsupported algorithm accepted")
	}
	if _, ok := parseDigest([]string{`Digest realm="r", qop="auth-int", nonce="n"`}); ok {
		t.Fatal("auth-int only accepted")
	}
	p := authParams(`realm="a, \"b\"", nonce=xyz`)
	if p["realm"] != `a, "b"` || p["nonce"] != "xyz" {
		t.Fatalf("params: %v", p)
	}
}

// digestServer accepts user "admin"/"secret" and counts requests.
type digestServer struct {
	nonce    atomic.Value
	requests atomic.Int32
	mu       sync.Mutex
	bodies   []string
}

func (d *digestServer) lastBody() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.bodies) == 0 {
		return ""
	}
	return d.bodies[len(d.bodies)-1]
}

func md5hex(s string) string {
	h := md5.Sum([]byte(s)) //nolint:gosec // test
	return hex.EncodeToString(h[:])
}

func (d *digestServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.requests.Add(1)
	body, _ := io.ReadAll(r.Body)
	nonce := d.nonce.Load().(string)
	p := authParams(strings.TrimPrefix(r.Header.Get("Authorization"), "Digest "))
	ha1 := md5hex("admin:switch:secret")
	ha2 := md5hex(r.Method + ":" + r.URL.RequestURI())
	want := md5hex(ha1 + ":" + nonce + ":" + p["nc"] + ":" + p["cnonce"] + ":auth:" + ha2)
	if p["nonce"] != nonce || p["response"] != want || p["uri"] != r.URL.RequestURI() {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Digest realm="switch", qop="auth", nonce=%q, stale=FALSE`, nonce))
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	d.mu.Lock()
	d.bodies = append(d.bodies, string(body))
	d.mu.Unlock()
	_, _ = io.WriteString(w, "ok "+r.URL.Path)
}

func TestDigestTransport(t *testing.T) {
	ds := &digestServer{}
	ds.nonce.Store("n1")
	srv := httptest.NewServer(ds)
	defer srv.Close()
	rt := WithDigest(http.DefaultTransport, Login{"admin", "secret"})
	get := func(path string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		resp, err := rt.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	// First request: challenge, then signed.
	if code, body := get("/sys.b"); code != 200 || body != "ok /sys.b" {
		t.Fatalf("first: %d %q", code, body)
	}
	if n := ds.requests.Load(); n != 2 {
		t.Fatalf("first request took %d round trips, want 2", n)
	}
	// Second request: signed in advance.
	if code, _ := get("/link.b?x=1"); code != 200 {
		t.Fatalf("second: %d", code)
	}
	if n := ds.requests.Load(); n != 3 {
		t.Fatalf("pre-signed request took %d round trips in total, want 3", n)
	}
	// New nonce: one 401, then success.
	ds.nonce.Store("n2")
	if code, _ := get("/fwd.b"); code != 200 {
		t.Fatalf("after nonce change: %d", code)
	}
	// POST bodies are repeated after a challenge.
	ds.nonce.Store("n3")
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/logout", strings.NewReader("*"))
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || ds.lastBody() != "*" {
		t.Fatalf("post: %d %q", resp.StatusCode, ds.lastBody())
	}
}

func TestDigestWrongLogin(t *testing.T) {
	ds := &digestServer{}
	ds.nonce.Store("n1")
	srv := httptest.NewServer(ds)
	defer srv.Close()
	rt := WithDigest(http.DefaultTransport, Login{"admin", "wrong"})
	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/sys.b", nil)
		resp, err := rt.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status %d", resp.StatusCode)
		}
	}
	// 1st: unsigned + signed; 2nd: signed only (same nonce refused, no repeat).
	if n := ds.requests.Load(); n != 3 {
		t.Fatalf("%d requests, want 3", n)
	}
}
