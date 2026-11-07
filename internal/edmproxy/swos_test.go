package edmproxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lan-dot-party/ers-proxy/internal/devices"
	"github.com/lan-dot-party/ers-proxy/internal/mockswitch"
	"github.com/lan-dot-party/ers-proxy/internal/store"
	"github.com/lan-dot-party/ers-proxy/internal/swos/swostest"
)

// addSwOS adds a swos device for a fake SwOS switch, with a stored login if
// password is not empty.
func (e *env) addSwOS(password string) (*swostest.Server, int64) {
	e.t.Helper()
	fake := swostest.New()
	srv := httptest.NewServer(fake)
	e.t.Cleanup(srv.Close)
	ctx := context.Background()
	d := &store.Device{Name: "mikrotik", Kind: store.KindSwOS, URL: srv.URL, OpenMode: store.OpenInherit,
		Isolation: store.IsolationSandbox, TLSMode: store.TLSAuto}
	if err := e.db.CreateDevice(ctx, d); err != nil {
		e.t.Fatal(err)
	}
	if password != "" {
		enc, err := devices.EncryptCredentials(e.key, d, devices.Credentials{Username: swostest.User, Password: password})
		if err != nil {
			e.t.Fatal(err)
		}
		if err := e.db.SetDeviceCredentials(ctx, d.ID, enc); err != nil {
			e.t.Fatal(err)
		}
	}
	return fake, d.ID
}

func (e *env) openID(id int64) (*http.Response, string) {
	e.t.Helper()
	return e.do("GET", fmt.Sprintf("/open/%d", id), "", map[string]string{"Sec-Fetch-Site": "same-origin"})
}

func TestSwOSNeedsStoredLogin(t *testing.T) {
	e := setup(t, mockswitch.Options{})
	_, id := e.addSwOS("")
	resp, body := e.openID(id)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(body, "web login") {
		t.Fatalf("open without login: %d %q", resp.StatusCode, body)
	}
}

func TestSwOSThroughProxy(t *testing.T) {
	e := setup(t, mockswitch.Options{})
	fake, id := e.addSwOS(swostest.Password)
	resp, _ := e.openID(id)
	base := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(base, "/d/") {
		t.Fatalf("open: %d %q", resp.StatusCode, base)
	}
	// "/" redirects to index.html, kept inside the proxy path.
	resp, _ = e.do("GET", base, "", doc)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != base+"index.html" {
		t.Fatalf("root: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, body := e.do("GET", base+"index.html", "", doc)
	if resp.StatusCode != 200 || !strings.Contains(body, base+"__ersproxy/shim.js") ||
		!strings.Contains(resp.Header.Get("Content-Security-Policy"), "sandbox allow-scripts") {
		t.Fatalf("index: %d %v\n%s", resp.StatusCode, resp.Header, body)
	}
	// The shim maps SwOS's absolute XHR paths below the proxy path.
	_, shim := e.do("GET", base+"__ersproxy/shim.js", "", map[string]string{"Sec-Fetch-Dest": "script"})
	if !strings.Contains(shim, `var base = "`+base+`"`) || !strings.Contains(shim, "if (true && window.XMLHttpRequest)") {
		t.Fatalf("shim does not rebase XHR paths:\n%.600s", shim)
	}
	// Data: the proxy logs in with HTTP Digest; the browser's own
	// Authorization header is not forwarded.
	resp, body = e.do("GET", base+"sys.b", "", map[string]string{"Sec-Fetch-Dest": "empty", "Origin": "null",
		"Authorization": `Digest username="browser", response="bogus"`})
	if resp.StatusCode != 200 || !strings.Contains(body, "brd:") {
		t.Fatalf("sys.b: %d %q", resp.StatusCode, body)
	}
	for _, h := range fake.AuthHeaders() {
		if strings.Contains(h, "bogus") || strings.Contains(h, "browser") {
			t.Fatalf("browser credentials reached the switch: %q", h)
		}
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "null" || resp.Header.Get("Www-Authenticate") != "" {
		t.Fatalf("headers %v", resp.Header)
	}
	// SwOS's logout ends reuse of the proxy session.
	resp, _ = e.do("POST", base+"logout", "*", map[string]string{"Sec-Fetch-Dest": "empty", "Origin": "null"})
	if resp.StatusCode != 200 || len(fake.Posts("/logout")) != 1 {
		t.Fatalf("logout: %d %v", resp.StatusCode, fake.Posts("/logout"))
	}
	resp, _ = e.openID(id)
	if again := resp.Header.Get("Location"); again == base {
		t.Fatal("session reused after SwOS logout")
	}
}

func TestSwOSWrongStoredLogin(t *testing.T) {
	e := setup(t, mockswitch.Options{})
	_, id := e.addSwOS("wrong-password")
	resp, _ := e.openID(id)
	base := resp.Header.Get("Location")
	resp, _ = e.do("GET", base+"link.b", "", map[string]string{"Sec-Fetch-Dest": "empty", "Origin": "null"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if resp.Header.Get("Www-Authenticate") != "" {
		t.Fatal("the browser would show a login dialog")
	}
	if dg := e.proxy.Diagnostics(id); dg == nil || !strings.Contains(dg.LastError, "web login") {
		t.Fatalf("diagnostics %+v", dg)
	}
}
