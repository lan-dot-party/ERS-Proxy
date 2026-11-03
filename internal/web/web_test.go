package web

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/lan-dot-party/ers-proxy/internal/app"
	"github.com/lan-dot-party/ers-proxy/internal/auth"
	"github.com/lan-dot-party/ers-proxy/internal/config"
	"github.com/lan-dot-party/ers-proxy/internal/secret"
	"github.com/lan-dot-party/ers-proxy/internal/store"
)

// Every key used in a template exists, with German and English text.
func TestTemplateKeysExist(t *testing.T) {
	re := regexp.MustCompile(`\.T "([a-z0-9_.-]+)"`)
	err := fs.WalkDir(templateFS, "templates", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(templateFS, path)
		if err != nil {
			return err
		}
		for _, m := range re.FindAllStringSubmatch(string(data), -1) {
			if !hasKey(m[1]) {
				t.Errorf("%s: unknown translation key %q", path, m[1])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range messages {
		if v[0] == "" || v[1] == "" {
			t.Errorf("key %q lacks a translation", k)
		}
	}
	// Keys built with print in templates.
	for _, prefix := range []string{"kind.", "open.", "role.", "theme.", "tls.", "warn.", "status."} {
		found := false
		for k := range messages {
			if strings.HasPrefix(k, prefix) {
				found = true
			}
		}
		if !found {
			t.Errorf("no keys with prefix %q", prefix)
		}
	}
}

type harness struct {
	t      *testing.T
	srv    *httptest.Server
	db     *store.DB
	ui     *Server
	client *http.Client
	csrf   string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(store.Options{Path: filepath.Join(dir, "t.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := db.Migrate(context.Background(), log); err != nil {
		t.Fatal(err)
	}
	key, err := secret.LoadOrCreate(filepath.Join(dir, "k"))
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, db: db}
	h.srv = httptest.NewUnstartedServer(nil)
	cfg := config.Default()
	cfg.PublicURL = "http://" + h.srv.Listener.Addr().String()
	a := app.New(cfg, db, key, log)
	ui, err := New(a)
	if err != nil {
		t.Fatal(err)
	}
	h.ui = ui
	h.srv.Config.Handler = a.SecurityHeaders(a.SessionMiddleware(ui.Routes()))
	h.srv.Start()
	t.Cleanup(h.srv.Close)
	jar, _ := cookiejar.New(nil)
	h.client = &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return h
}

func (h *harness) user(name, role string) {
	hash, err := auth.HashPassword("correct horse battery")
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.db.CreateUser(context.Background(), &store.User{Username: name, PasswordHash: hash, Role: role}); err != nil {
		h.t.Fatal(err)
	}
}

var reLoginCSRF = regexp.MustCompile(`name="_login_csrf" value="([0-9a-f]+)"`)
var reCSRF = regexp.MustCompile(`data-csrf="([0-9a-f]+)"`)

func (h *harness) get(path string) (*http.Response, string) {
	h.t.Helper()
	resp, err := h.client.Get(h.srv.URL + path)
	if err != nil {
		h.t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(body)
}

func (h *harness) post(path string, form url.Values, hdr map[string]string) *http.Response {
	h.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func (h *harness) login(name string) {
	h.t.Helper()
	_, body := h.get("/login")
	m := reLoginCSRF.FindStringSubmatch(body)
	if m == nil {
		h.t.Fatal("no login CSRF token")
	}
	resp := h.post("/login", url.Values{"username": {name}, "password": {"correct horse battery"}, "_login_csrf": {m[1]}, "next": {"/"}},
		map[string]string{"Origin": h.srv.URL})
	if resp.StatusCode != http.StatusSeeOther {
		h.t.Fatalf("login status %d", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == app.SessionCookie && c.SameSite != http.SameSiteStrictMode {
			h.t.Fatal("session cookie is not SameSite=Strict")
		}
	}
	_, body = h.get("/")
	m = reCSRF.FindStringSubmatch(body)
	if m == nil {
		h.t.Fatal("no CSRF token on the workspace")
	}
	h.csrf = m[1]
}

func TestLoginAndWorkspace(t *testing.T) {
	h := newHarness(t)
	h.user("admin", store.RoleAdmin)
	resp, _ := h.get("/")
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/login") {
		t.Fatalf("anonymous workspace: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") ||
		strings.Contains(csp, "unsafe-inline") {
		t.Fatalf("CSP %q", csp)
	}
	h.login("admin")
	resp, body := h.get("/")
	if resp.StatusCode != 200 || !strings.Contains(body, `id="workspace"`) {
		t.Fatalf("workspace: %d", resp.StatusCode)
	}
}

func TestStateChangesNeedCSRFAndOrigin(t *testing.T) {
	h := newHarness(t)
	h.user("admin", store.RoleAdmin)
	h.login("admin")
	form := url.Values{"name": {"Building A"}}
	if r := h.post("/admin/groups", form, nil); r.StatusCode != http.StatusForbidden {
		t.Fatalf("missing CSRF token: %d", r.StatusCode)
	}
	form.Set("_csrf", h.csrf)
	// A sandboxed EDM document sends Origin: null – never accepted.
	if r := h.post("/admin/groups", form, map[string]string{"Origin": "null"}); r.StatusCode != http.StatusForbidden {
		t.Fatalf("Origin null accepted: %d", r.StatusCode)
	}
	if r := h.post("/admin/groups", form, map[string]string{"Origin": "https://evil.example"}); r.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign origin accepted: %d", r.StatusCode)
	}
	if r := h.post("/admin/groups", form, map[string]string{"Sec-Fetch-Site": "cross-site"}); r.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site fetch accepted: %d", r.StatusCode)
	}
	if r := h.post("/admin/groups", form, map[string]string{"Origin": h.srv.URL}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("legit request rejected: %d", r.StatusCode)
	}
	groups, _ := h.db.ListGroups(context.Background())
	if len(groups) != 1 {
		t.Fatalf("groups: %d", len(groups))
	}
}

func TestRolesAndDevices(t *testing.T) {
	h := newHarness(t)
	h.user("admin", store.RoleAdmin)
	h.user("bob", store.RoleUser)
	h.login("bob")
	if resp, _ := h.get("/admin/devices"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("user reached admin page: %d", resp.StatusCode)
	}

	h2 := newHarnessShared(t, h)
	h2.login("admin")
	r := h2.post("/admin/devices", url.Values{"_csrf": {h2.csrf}, "name": {"Core <b>1</b>"}, "kind": {"ers-edm"},
		"url": {"192.0.2.10"}, "open_mode": {"inherit"}, "health_enabled": {"1"}}, map[string]string{"Origin": h2.srv.URL})
	if r.StatusCode != http.StatusSeeOther {
		t.Fatalf("create device: %d", r.StatusCode)
	}
	r = h2.post("/admin/devices", url.Values{"_csrf": {h2.csrf}, "name": {"FW"}, "kind": {"link"},
		"url": {"https://fw.example/"}}, map[string]string{"Origin": h2.srv.URL})
	if r.StatusCode != http.StatusSeeOther {
		t.Fatalf("create link: %d", r.StatusCode)
	}
	resp, body := h2.get("/")
	if strings.Contains(body, "<b>1</b>") || !strings.Contains(body, "Core &lt;b&gt;1&lt;/b&gt;") {
		t.Fatal("device name not escaped")
	}
	if !strings.Contains(body, `href="/open/1"`) || !strings.Contains(body, `href="https://fw.example/"`) {
		t.Fatal("device links missing")
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-src 'self' https://fw.example") {
		t.Fatalf("link origin not in frame-src: %q", csp)
	}
	// /open refuses navigation from another site (e.g. a sandboxed EDM document).
	req, _ := http.NewRequest(http.MethodGet, h2.srv.URL+"/open/2", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	res, err := h2.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site open: %d", res.StatusCode)
	}
	resp, _ = h2.get("/open/2")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "https://fw.example/" {
		t.Fatalf("open link: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
}

// newHarnessShared returns a second client against the same server.
func newHarnessShared(t *testing.T, h *harness) *harness {
	jar, _ := cookiejar.New(nil)
	return &harness{t: t, srv: h.srv, db: h.db, ui: h.ui,
		client: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func TestEffectiveOpenMode(t *testing.T) {
	h := newHarness(t)
	u := &store.User{OpenMode: config.OpenWindow}
	ers := &store.Device{Kind: store.KindERS, OpenMode: store.OpenInherit}
	if m := h.ui.EffectiveOpenMode(u, ers); m != config.OpenWindow {
		t.Fatalf("user default not used: %s", m)
	}
	ers.OpenMode = config.OpenTab
	if m := h.ui.EffectiveOpenMode(u, ers); m != config.OpenTab {
		t.Fatalf("device mode not used: %s", m)
	}
	if m := h.ui.EffectiveOpenMode(&store.User{}, &store.Device{Kind: store.KindERS, OpenMode: store.OpenInherit}); m != config.OpenEmbedded {
		t.Fatalf("global default not used: %s", m)
	}
	h.ui.Cfg.PublicURL = "https://ers.example"
	link := &store.Device{Kind: store.KindLink, URL: "http://old.example/", OpenMode: config.OpenEmbedded}
	if m := h.ui.EffectiveOpenMode(u, link); m != config.OpenTab {
		t.Fatalf("http link in https portal must open as tab: %s", m)
	}
}

func TestSNMPCredentialsEndpoint(t *testing.T) {
	h := newHarness(t)
	h.user("admin", store.RoleAdmin)
	h.login("admin")
	origin := map[string]string{"Origin": h.srv.URL}
	r := h.post("/admin/devices", url.Values{"_csrf": {h.csrf}, "name": {"Core"}, "kind": {"ers-edm"}, "url": {"192.0.2.10"},
		"monitor_source": {"snmp"}, "monitor_interval": {"60"}, "snmp_port": {"161"}, "snmp_version": {"v2c"}}, origin)
	if r.StatusCode != http.StatusSeeOther {
		t.Fatalf("create device: %d", r.StatusCode)
	}
	ctx := context.Background()
	d, err := h.db.DeviceByID(ctx, 1)
	if err != nil || d.MonitorSource != "snmp" {
		t.Fatalf("monitoring fields not stored: %+v %v", d, err)
	}

	// Without the password nothing is stored.
	r = h.post("/admin/devices/1/snmp", url.Values{"_csrf": {h.csrf}, "community": {"s3cr3t-community"}}, origin)
	if loc := r.Header.Get("Location"); !strings.Contains(loc, "err=wrong_password") {
		t.Fatalf("no password: %d %q", r.StatusCode, loc)
	}
	if d, _ = h.db.DeviceByID(ctx, 1); d.HasSNMPCredentials() {
		t.Fatal("stored without password confirmation")
	}
	r = h.post("/admin/devices/1/snmp", url.Values{"_csrf": {h.csrf}, "community": {"s3cr3t-community"},
		"confirm_password": {"wrong password"}}, origin)
	if loc := r.Header.Get("Location"); !strings.Contains(loc, "err=wrong_password") {
		t.Fatalf("wrong password: %q", loc)
	}

	r = h.post("/admin/devices/1/snmp", url.Values{"_csrf": {h.csrf}, "community": {"s3cr3t-community"},
		"confirm_password": {"correct horse battery"}}, origin)
	if loc := r.Header.Get("Location"); !strings.Contains(loc, "ok=saved") {
		t.Fatalf("set: %d %q", r.StatusCode, loc)
	}
	if d, _ = h.db.DeviceByID(ctx, 1); !d.HasSNMPCredentials() || bytes.Contains(d.SNMPCredentials, []byte("s3cr3t-community")) {
		t.Fatal("credentials missing or stored in plain text")
	}
	for _, p := range []string{"/admin/devices/1/edit", "/admin/devices", "/admin/audit"} {
		_, body := h.get(p)
		if strings.Contains(body, "s3cr3t-community") {
			t.Fatalf("%s echoes the community", p)
		}
		if p == "/admin/devices/1/edit" && !strings.Contains(body, "stored encrypted") && !strings.Contains(body, "verschlüsselt gespeichert") {
			t.Fatal("edit page does not say that credentials are stored")
		}
	}

	// Changing the SNMP port drops the credentials (bound to host and port).
	r = h.post("/admin/devices/1", url.Values{"_csrf": {h.csrf}, "name": {"Core"}, "kind": {"ers-edm"}, "url": {"192.0.2.10"},
		"monitor_source": {"snmp"}, "monitor_interval": {"60"}, "snmp_port": {"1161"}, "snmp_version": {"v2c"}}, origin)
	if r.StatusCode != http.StatusSeeOther {
		t.Fatalf("update: %d", r.StatusCode)
	}
	if d, _ = h.db.DeviceByID(ctx, 1); d.HasSNMPCredentials() || d.SNMPPort != 1161 {
		t.Fatalf("credentials survived a port change: %+v", d)
	}

	// Invalid input is refused; clear needs the password too.
	r = h.post("/admin/devices/1/snmp", url.Values{"_csrf": {h.csrf}, "confirm_password": {"correct horse battery"}}, origin)
	if loc := r.Header.Get("Location"); !strings.Contains(loc, "err=snmp_invalid") {
		t.Fatalf("empty community: %q", loc)
	}
	h.post("/admin/devices/1/snmp", url.Values{"_csrf": {h.csrf}, "community": {"abc"}, "confirm_password": {"correct horse battery"}}, origin)
	h.post("/admin/devices/1/snmp", url.Values{"_csrf": {h.csrf}, "clear": {"1"}}, origin)
	if d, _ = h.db.DeviceByID(ctx, 1); !d.HasSNMPCredentials() {
		t.Fatal("cleared without password")
	}
	h.post("/admin/devices/1/snmp", url.Values{"_csrf": {h.csrf}, "clear": {"1"}, "confirm_password": {"correct horse battery"}}, origin)
	if d, _ = h.db.DeviceByID(ctx, 1); d.HasSNMPCredentials() {
		t.Fatal("not cleared")
	}
}
