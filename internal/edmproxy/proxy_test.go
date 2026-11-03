package edmproxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/go-chi/chi/v5"

	"github.com/lan-dot-party/ers-proxy/internal/app"
	"github.com/lan-dot-party/ers-proxy/internal/auth"
	"github.com/lan-dot-party/ers-proxy/internal/config"
	"github.com/lan-dot-party/ers-proxy/internal/devices"
	"github.com/lan-dot-party/ers-proxy/internal/mockswitch"
	"github.com/lan-dot-party/ers-proxy/internal/secret"
	"github.com/lan-dot-party/ers-proxy/internal/store"
	"github.com/lan-dot-party/ers-proxy/internal/upstream"
	"github.com/lan-dot-party/ers-proxy/internal/web"
)

// Synthetic EDM files: only the strings the patches and the test look for.
var fakeEDM = fstest.MapFS{
	"login.html": {Data: []byte(`<html><head><title>login</title></head><body>
<script>var supportedBrowser = false;
function check(){ xmlhttp.open("POST","/",false); if (xmlhttp.statusText.indexOf("Change Password")>=0){} window.location.href="/"; }</script>
</body></html>`)},
	"index.html": {Data: []byte(`<html><head><script src="ext/adapter/ext/ext-base.js"></script></head><body>
<script>var supportedBrowser = false;</script>EDM</body></html>`)},
	"ext/adapter/ext/ext-base.js": {Data: []byte(`if(g>=0){p[l[k].substring(0,g)]=l[k].substring(g+2)}`)},
	"images/logo.gif":             {Data: []byte("GIF89a")},
}

type env struct {
	t      *testing.T
	portal *httptest.Server
	mock   *mockswitch.Mock
	sw     *httptest.Server
	db     *store.DB
	proxy  *Proxy
	client *http.Client
	csrf   string
	devID  int64
	key    *secret.Key
}

func setup(t *testing.T, opts mockswitch.Options) *env {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(store.Options{Path: filepath.Join(dir, "t.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := db.Migrate(ctx, log); err != nil {
		t.Fatal(err)
	}
	key, _ := secret.LoadOrCreate(filepath.Join(dir, "k"))
	e := &env{t: t, db: db, key: key}
	e.mock = mockswitch.New(fakeEDM, opts)
	e.sw = httptest.NewServer(e.mock)
	t.Cleanup(e.sw.Close)

	e.portal = httptest.NewUnstartedServer(nil)
	cfg := config.Default()
	cfg.PublicURL = "http://" + e.portal.Listener.Addr().String()
	a := app.New(cfg, db, key, log)
	ui, err := web.New(a)
	if err != nil {
		t.Fatal(err)
	}
	e.proxy = New(ctx, a)
	ui.Opener = e.proxy
	ui.OnDeviceChange = e.proxy.ForgetDevice
	r := chi.NewRouter()
	r.Handle("/d/*", e.proxy)
	r.With(a.SecurityHeaders, a.SessionMiddleware).Mount("/", ui.Routes())
	e.portal.Config.Handler = r
	e.portal.Start()
	t.Cleanup(e.portal.Close)

	hash, _ := auth.HashPassword("correct horse battery")
	if err := db.CreateUser(ctx, &store.User{Username: "admin", PasswordHash: hash, Role: store.RoleAdmin}); err != nil {
		t.Fatal(err)
	}
	d := &store.Device{Name: "sw1", Kind: store.KindERS, URL: e.sw.URL, OpenMode: store.OpenInherit,
		Isolation: store.IsolationSandbox, TLSMode: store.TLSAuto, HealthEnabled: true}
	if err := db.CreateDevice(ctx, d); err != nil {
		t.Fatal(err)
	}
	e.devID = d.ID
	jar, _ := cookiejar.New(nil)
	e.client = &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	e.login()
	return e
}

var reLoginCSRF = regexp.MustCompile(`name="_login_csrf" value="([0-9a-f]+)"`)
var reCSRF = regexp.MustCompile(`data-csrf="([0-9a-f]+)"`)

func (e *env) do(method, path string, body string, hdr map[string]string) (*http.Response, string) {
	e.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, e.portal.URL+path, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(data)
}

func (e *env) login() {
	_, body := e.do("GET", "/login", "", nil)
	m := reLoginCSRF.FindStringSubmatch(body)
	form := url.Values{"username": {"admin"}, "password": {"correct horse battery"}, "_login_csrf": {m[1]}, "next": {"/"}}
	if resp, _ := e.do("POST", "/login", form.Encode(), nil); resp.StatusCode != http.StatusSeeOther {
		e.t.Fatalf("login: %d", resp.StatusCode)
	}
	_, body = e.do("GET", "/", "", nil)
	e.csrf = reCSRF.FindStringSubmatch(body)[1]
}

// open returns the proxy base path /d/{token}/.
func (e *env) open() string {
	resp, _ := e.do("GET", "/open/1", "", map[string]string{"Sec-Fetch-Site": "same-origin"})
	loc := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(loc, "/d/") || len(loc) != len("/d/")+32+1 {
		e.t.Fatalf("open: %d %q", resp.StatusCode, loc)
	}
	return loc
}

var doc = map[string]string{"Sec-Fetch-Dest": "document"}

func TestProxyLoginFlowAndIsolation(t *testing.T) {
	e := setup(t, mockswitch.Options{Gzip: true})
	base := e.open()

	// Login page: patched, shim injected, sandboxed, no switch cookie in the browser.
	resp, body := e.do("GET", base, "", doc)
	if resp.StatusCode != 200 {
		t.Fatalf("login page: %d %s", resp.StatusCode, body)
	}
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "sandbox allow-scripts") || strings.Contains(csp, "allow-same-origin") {
		t.Fatalf("CSP %q", csp)
	}
	if resp.Header.Get("Set-Cookie") != "" {
		t.Fatal("switch cookie leaked to the browser")
	}
	for k, v := range resp.Header {
		if len(v) > 1 && k != "Set-Cookie" {
			t.Errorf("header %s appears %d times: %v", k, len(v), v)
		}
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "null" || resp.Header.Get("Server") != "Rapid Logic/1.1" {
		t.Fatalf("headers %v", resp.Header)
	}
	if resp.Header.Get("Content-Encoding") != "" {
		t.Fatal("gzip not removed after patching")
	}
	for _, want := range []string{`<head><script src="` + base + `__ersproxy/shim.js"></script>`, "supportedBrowser = true",
		`xmlhttp.open("POST","` + base + `",false)`, "ersStatusText(xmlhttp)"} {
		if !strings.Contains(body, want) {
			t.Errorf("login page lacks %q:\n%s", want, body)
		}
	}

	// The shim exposes the switch's auth cookie to the page.
	_, shim := e.do("GET", base+"__ersproxy/shim.js", "", map[string]string{"Sec-Fetch-Dest": "script"})
	if !strings.Contains(shim, "URL.createObjectURL") {
		t.Error("shim lacks the export window handling")
	}
	m := regexp.MustCompile(`var jar = "auth=([0-9a-f]+)"`).FindStringSubmatch(shim)
	if m == nil {
		t.Fatalf("no auth cookie in shim:\n%s", shim)
	}
	nonce := m[1]

	// Login check (XHR): the proxy sends the cookie from its jar, never the portal session.
	resp, body = e.do("POST", base, "encoded=02RW:secret&nonce="+nonce, map[string]string{"Origin": "null", "Sec-Fetch-Dest": "empty"})
	if resp.StatusCode != 200 || body != "OK" {
		t.Fatalf("login check: %d %q", resp.StatusCode, body)
	}
	if strings.Contains(e.mock.LastCookie, app.SessionCookie) || !strings.Contains(e.mock.LastCookie, "auth="+nonce) {
		t.Fatalf("cookies sent to the switch: %q", e.mock.LastCookie)
	}

	// Form submit after the check: browsers add "Cache-Control: max-age=0", which
	// makes the switch answer with an empty 304. The proxy must not forward it.
	resp, body = e.do("POST", base, "encoded=02RW:secret&nonce="+nonce, map[string]string{"Origin": "null",
		"Sec-Fetch-Dest": "iframe", "Cache-Control": "max-age=0", "Pragma": "no-cache"})
	if resp.StatusCode != 200 || !strings.Contains(body, "__ersproxy/shim.js") {
		t.Fatalf("form submit after login: %d %q", resp.StatusCode, body)
	}

	// SNMP bridge (XHR): no shim, not patched.
	resp, body = e.do("POST", base+"jsp/getFormMibs.jsp", "command=getVariables&varNames=sysName&userId="+nonce,
		map[string]string{"Origin": "null", "Sec-Fetch-Dest": "empty", "X-Requested-With": "XMLHttpRequest"})
	if resp.StatusCode != 200 || body != "sysName:mock-ers5698\r\n" {
		t.Fatalf("jsp: %d %q", resp.StatusCode, body)
	}
	if v := resp.Header.Values("Access-Control-Allow-Origin"); len(v) != 1 || v[0] != "null" {
		t.Fatalf("Access-Control-Allow-Origin on XHR answer: %v", v)
	}

	// Ext's header map is patched in scripts.
	_, body = e.do("GET", base+"ext/adapter/ext/ext-base.js", "", map[string]string{"Sec-Fetch-Dest": "script"})
	if !strings.Contains(body, "p[hn.toLowerCase()]=hv") {
		t.Fatalf("ext-base.js not patched: %s", body)
	}

	// CORS preflight is answered by the proxy.
	resp, _ = e.do("OPTIONS", base+"jsp/getFormMibs.jsp", "", map[string]string{"Origin": "null",
		"Access-Control-Request-Method": "POST", "Access-Control-Request-Headers": "x-requested-with"})
	if resp.StatusCode != http.StatusNoContent || resp.Header.Get("Access-Control-Allow-Headers") != "x-requested-with" {
		t.Fatalf("preflight: %d %v", resp.StatusCode, resp.Header)
	}

	// The sandboxed document cannot use the portal: its requests carry Origin: null.
	resp, _ = e.do("POST", "/admin/groups", "name=x&_csrf="+e.csrf, map[string]string{"Origin": "null"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("portal accepted a request from the sandbox: %d", resp.StatusCode)
	}

	// Diagnostics record the patches.
	dg := e.proxy.Diagnostics(e.devID)
	if dg == nil || dg.Patches["ext-header-map"] != 1 || dg.Server != "Rapid Logic/1.1" {
		t.Fatalf("diagnostics %+v", dg)
	}
}

func TestProxySessionEnds(t *testing.T) {
	e := setup(t, mockswitch.Options{})
	if resp, _ := e.do("GET", "/d/"+strings.Repeat("a", 32)+"/", "", doc); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown token: %d", resp.StatusCode)
	}
	base := e.open()
	if resp, _ := e.do("GET", base, "", doc); resp.StatusCode != 200 {
		t.Fatalf("open: %d", resp.StatusCode)
	}
	// Logout ends the proxy session (checked at least every 30 seconds).
	ps := e.proxy.reg.get(strings.Split(base, "/")[2])
	if resp, _ := e.do("POST", "/logout", "_csrf="+e.csrf, nil); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("logout: %d", resp.StatusCode)
	}
	ps.validated.Store(0)
	if resp, _ := e.do("GET", base, "", doc); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("proxy session survived logout: %d", resp.StatusCode)
	}
}

func TestChangePasswordReason(t *testing.T) {
	e := setup(t, mockswitch.Options{ChangePassword: true})
	base := e.open()
	e.do("GET", base, "", doc)
	_, shim := e.do("GET", base+"__ersproxy/shim.js", "", nil)
	nonce := regexp.MustCompile(`auth=([0-9a-f]+)`).FindStringSubmatch(shim)[1]
	resp, _ := e.do("POST", base, "encoded=02RW:secret&nonce="+nonce, map[string]string{"Sec-Fetch-Dest": "empty"})
	if resp.StatusCode != 205 || resp.Header.Get("X-ERSProxy-Status-Text") != "Change Password" {
		t.Fatalf("205 reason not passed on: %d %v", resp.StatusCode, resp.Header)
	}
}

func TestRewriteLocation(t *testing.T) {
	target, _ := url.Parse("http://192.0.2.10")
	cases := map[string]string{
		"/":                               "/d/t/",
		"/index.html?x=1":                 "/d/t/index.html?x=1",
		"http://192.0.2.10/login.html":    "/d/t/login.html",
		"http://192.0.2.10:80/login.html": "/d/t/login.html",
		"login.html":                      "login.html",
		"https://example.org/x":           "https://example.org/x",
	}
	for in, want := range cases {
		if got := rewriteLocation(in, target, "/d/t/"); got != want {
			t.Errorf("rewriteLocation(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestJar(t *testing.T) {
	j := newJar()
	j.store(&http.Response{Header: http.Header{"Set-Cookie": {"auth=abc; Path=/", "sid=1; HttpOnly"}}})
	if j.header() != "auth=abc; sid=1" || j.script() != "auth=abc" {
		t.Fatalf("jar: %q / %q", j.header(), j.script())
	}
	j.set("lang=de; path=/")
	j.set("auth=; expires=Thu, 01 Jan 1970 00:00:00 GMT")
	if j.header() != "lang=de; sid=1" {
		t.Fatalf("after script writes: %q", j.header())
	}
}

func (e *env) storeLogin() {
	e.t.Helper()
	ctx := context.Background()
	d, err := e.db.DeviceByID(ctx, e.devID)
	if err != nil {
		e.t.Fatal(err)
	}
	enc, err := devices.EncryptCredentials(e.key, d, devices.Credentials{Username: "RW", Password: "secure-pw"})
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.db.SetDeviceCredentials(ctx, e.devID, enc); err != nil {
		e.t.Fatal(err)
	}
}

func TestAutoLogin(t *testing.T) {
	e := setup(t, mockswitch.Options{})
	e.storeLogin()
	base := e.open()
	_, body := e.do("GET", base, "", doc)
	if !strings.Contains(body, "EDM</body>") {
		t.Fatalf("auto-login did not reach the device view:\n%s", body)
	}
	if dg := e.proxy.Diagnostics(e.devID); dg == nil || dg.AutoLogin.IsZero() || dg.LastError != "" {
		t.Fatalf("diagnostics %+v", dg)
	}
}

func TestAutoLoginWrongPassword(t *testing.T) {
	// The real switch answers a wrong password with 200 and authError.html.
	e := setup(t, mockswitch.Options{Password: "other-pw"})
	e.storeLogin()
	base := e.open()
	_, body := e.do("GET", base, "", doc)
	if strings.Contains(body, "EDM</body>") {
		t.Fatalf("wrong password reached the device view:\n%s", body)
	}
	if dg := e.proxy.Diagnostics(e.devID); dg == nil || !dg.AutoLogin.IsZero() || !strings.Contains(dg.LastError, "rejected") {
		t.Fatalf("diagnostics %+v", dg)
	}
}

func TestAutoLoginChangePassword(t *testing.T) {
	e := setup(t, mockswitch.Options{ChangePassword: true})
	e.storeLogin()
	base := e.open()
	_, body := e.do("GET", base, "", doc)
	if !strings.Contains(body, "login") {
		t.Fatalf("expected the EDM login page after a failed auto-login:\n%s", body)
	}
	if dg := e.proxy.Diagnostics(e.devID); dg == nil || !strings.Contains(dg.LastError, "password change") {
		t.Fatalf("diagnostics %+v", dg)
	}
}

func TestIsolationHeaders(t *testing.T) {
	h := http.Header{"X-Frame-Options": {"DENY"}, "Content-Security-Policy": {"default-src 'none'"},
		"Strict-Transport-Security": {"max-age=1"}}
	isolationHeaders(h, true)
	if h.Get("Content-Security-Policy") != sandboxCSP || h.Get("X-Frame-Options") != "" ||
		h.Get("Strict-Transport-Security") != "" || h.Get("Access-Control-Allow-Origin") != "null" {
		t.Fatalf("sandbox headers %v", h)
	}
	if strings.Contains(sandboxCSP, "allow-same-origin") || strings.Contains(sandboxCSP, "allow-top-navigation") {
		t.Fatal("the sandbox must never allow same-origin or top navigation")
	}
	isolationHeaders(h, false)
	if strings.Contains(h.Get("Content-Security-Policy"), "sandbox") || h.Get("Access-Control-Allow-Origin") != "" ||
		h.Get("Content-Security-Policy") != "frame-ancestors 'self'" {
		t.Fatalf("isolation off headers %v", h)
	}
}

func TestImageCache(t *testing.T) {
	e := setup(t, mockswitch.Options{})
	base := e.open()
	for i := 0; i < 2; i++ {
		resp, body := e.do("GET", base+"images/logo.gif", "", map[string]string{"Sec-Fetch-Dest": "image"})
		if resp.StatusCode != 200 || body != "GIF89a" || !strings.HasPrefix(resp.Header.Get("Content-Type"), "image/gif") ||
			!strings.Contains(resp.Header.Get("Content-Security-Policy"), "sandbox") {
			t.Fatalf("image %d: %d %q %v", i, resp.StatusCode, body, resp.Header)
		}
	}
	if n := e.mock.Requests["/images/logo.gif"]; n != 1 {
		t.Fatalf("switch asked %d times for a cached image", n)
	}
	// A new proxy session of the same device uses the cache, a changed device does not.
	base = e.open()
	e.do("GET", base+"images/logo.gif", "", nil)
	e.proxy.ForgetDevice(e.devID)
	base = e.open()
	e.do("GET", base+"images/logo.gif", "", nil)
	if n := e.mock.Requests["/images/logo.gif"]; n != 2 {
		t.Fatalf("switch asked %d times, want 2", n)
	}
}

func TestMarkEDMRead(t *testing.T) {
	for body, want := range map[string]bool{
		"command=getVariables&varNames=sysName&userId=1": true,
		"command=getTableVariables&varNames=rcVlanName":  true,
		"command=setVariables&varNames=sysName&values=x": false,
		"command=logout": false,
	} {
		r := httptest.NewRequest(http.MethodPost, "/d/x/jsp/getFormMibs.jsp", strings.NewReader(body))
		r, err := markEDMRead(r)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(r.Body)
		again, _ := r.GetBody()
		replay, _ := io.ReadAll(again)
		if string(got) != body || string(replay) != body {
			t.Fatalf("body changed: %q / %q", got, replay)
		}
		if marked := upstream.RetryMarked(r.Context()); marked != want {
			t.Errorf("%q: marked=%v", body, marked)
		}
	}
}
