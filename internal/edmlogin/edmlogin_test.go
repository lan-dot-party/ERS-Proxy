package edmlogin

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

func gz(s string) []byte {
	var z bytes.Buffer
	zw := gzip.NewWriter(&z)
	_, _ = zw.Write([]byte(s))
	_ = zw.Close()
	return z.Bytes()
}

const (
	ersPage  = `<html><body><form method="POST" name="submitForm" onSubmit="return encode_ers2500()"><input name="x"></form></body></html>`
	vossPage = `<html><body><form method="POST" name="submitForm" onSubmit="encode_vrf()"><input name="x"></form></body></html>`
)

func TestStyle(t *testing.T) {
	cases := []struct {
		name string
		page []byte
		want string
	}{
		{"ers plain", []byte(ersPage), StyleERS},
		{"voss plain", []byte(vossPage), StyleVOSS},
		{"voss gzip", gz(vossPage), StyleVOSS},
		{"ers gzip", gz(ersPage), StyleERS},
		{"no form", []byte("<html><body>hello</body></html>"), StyleERS},
		{"empty", nil, StyleERS},
	}
	for _, c := range cases {
		if got := Style(c.page); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

type vossRecord struct {
	method, ctype, cookie, body string
}

// vossServer implements the VOSS login flow: GET / sets auth=<nonce>, one form POST logs in.
func vossServer(t *testing.T, status int, pass string) (*httptest.Server, func() []vossRecord) {
	return vossServerNonce(t, status, pass, "nonce123")
}

// vossServerNonce is vossServer with another nonce ("" = no auth cookie on
// the login page, like VOSS 8).
func vossServerNonce(t *testing.T, status int, pass, nonce string) (*httptest.Server, func() []vossRecord) {
	t.Helper()
	var mu sync.Mutex
	var recs []vossRecord
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		recs = append(recs, vossRecord{r.Method, r.Header.Get("Content-Type"), r.Header.Get("Cookie"), string(b)})
		mu.Unlock()
		if r.Method == http.MethodGet {
			if nonce != "" {
				http.SetCookie(w, &http.Cookie{Name: "auth", Value: nonce, Path: "/"})
			}
			_, _ = w.Write([]byte(vossPage))
			return
		}
		form, _ := url.ParseQuery(string(b))
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		if form.Get("encoded") != "admin:"+pass {
			_, _ = w.Write(gz("<html>Authentication Failed!</html>"))
			return
		}
		_, _ = w.Write([]byte("<html>EDM</html>"))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []vossRecord {
		mu.Lock()
		defer mu.Unlock()
		return append([]vossRecord(nil), recs...)
	}
}

func TestLoginVOSS(t *testing.T) {
	ctx := context.Background()
	t.Run("success", func(t *testing.T) {
		srv, recs := vossServer(t, 200, "secret")
		if err := Login(ctx, http.DefaultTransport, srv.URL, NewJar(), "admin", "secret"); err != nil {
			t.Fatal(err)
		}
		r := recs()
		if len(r) != 2 || r[0].method != "GET" || r[1].method != "POST" {
			t.Fatalf("requests: %+v", r)
		}
		if r[1].ctype != "application/x-www-form-urlencoded" || !strings.Contains(r[1].cookie, "auth=nonce123") {
			t.Fatalf("POST: %+v", r[1])
		}
		form, _ := url.ParseQuery(r[1].body)
		if form.Get("encoded") != "admin:secret" || form.Get("nonce") != "nonce123" || form.Get("URL") != "/" {
			t.Fatalf("form: %v", form)
		}
	})
	t.Run("wrong password", func(t *testing.T) {
		srv, recs := vossServer(t, 200, "secret")
		err := Login(ctx, http.DefaultTransport, srv.URL, NewJar(), "admin", "nope")
		if !errors.Is(err, ErrRejected) {
			t.Fatalf("err = %v", err)
		}
		if len(recs()) != 2 {
			t.Fatalf("requests: %d", len(recs()))
		}
	})
	t.Run("change password", func(t *testing.T) {
		srv, _ := vossServer(t, 205, "secret")
		if err := Login(ctx, http.DefaultTransport, srv.URL, NewJar(), "admin", "secret"); !errors.Is(err, ErrChangePassword) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("other status", func(t *testing.T) {
		srv, _ := vossServer(t, 500, "secret")
		if err := Login(ctx, http.DefaultTransport, srv.URL, NewJar(), "admin", "secret"); !errors.Is(err, ErrRejected) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("VOSS 8 without nonce cookie", func(t *testing.T) {
		srv, recs := vossServerNonce(t, 200, "secret", "")
		if err := Login(ctx, http.DefaultTransport, srv.URL, NewJar(), "admin", "secret"); err != nil {
			t.Fatal(err)
		}
		form, _ := url.ParseQuery(recs()[1].body)
		if form.Get("encoded") != "admin:secret" || !form.Has("nonce") || form.Get("nonce") != "" {
			t.Fatalf("form: %v", form)
		}
	})
	t.Run("long password", func(t *testing.T) {
		long := strings.Repeat("p", 20)
		srv, _ := vossServer(t, 200, long)
		if err := Login(ctx, http.DefaultTransport, srv.URL, NewJar(), "admin", long); err != nil {
			t.Fatalf("20 characters: %v", err)
		}
	})
}

func TestLoginERSTooLong(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "auth", Value: "n", Path: "/"})
		_, _ = w.Write([]byte(ersPage))
	}))
	defer srv.Close()
	if err := Login(context.Background(), http.DefaultTransport, srv.URL, NewJar(), "admin", strings.Repeat("p", 16)); !errors.Is(err, ErrTooLong) {
		t.Fatalf("err = %v", err)
	}
}

func TestAuthFailed(t *testing.T) {
	page := []byte(`<html><body><b>Authentication Failed!</b> <a href="login.html">Return to Login page</a></body></html>`)
	var z bytes.Buffer
	zw := gzip.NewWriter(&z)
	_, _ = zw.Write(page)
	_ = zw.Close()
	if !authFailed(page) || !authFailed(z.Bytes()) {
		t.Fatal("authError page not recognised (plain or gzip)")
	}
	if authFailed([]byte("<html><title>Embedded Element Manager</title></html>")) || authFailed(nil) {
		t.Fatal("index page taken as failure")
	}
}
