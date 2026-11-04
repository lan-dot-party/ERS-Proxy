package web

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/lan-dot-party/ers-proxy/internal/config"
	"github.com/lan-dot-party/ers-proxy/internal/store"
	"github.com/lan-dot-party/ers-proxy/internal/upstream"
)

const testFingerprint = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func (h *harness) addDevice(d *store.Device) *store.Device {
	h.t.Helper()
	if d.OpenMode == "" {
		d.OpenMode = store.OpenInherit
	}
	if d.Isolation == "" {
		d.Isolation = store.IsolationSandbox
	}
	if d.TLSMode == "" {
		d.TLSMode = store.TLSAuto
	}
	if err := h.db.CreateDevice(context.Background(), d); err != nil {
		h.t.Fatal(err)
	}
	return d
}

func (h *harness) fakeProbe() {
	h.ui.Probe = func(ctx context.Context, d *store.Device) *upstream.ProbeResult {
		return &upstream.ProbeResult{Cert: &upstream.Certificate{Fingerprint: testFingerprint, Subject: "CN=vsp",
			NotAfter: time.Now().Add(time.Hour), KeyBits: 2048, SelfSigned: true}}
	}
}

func TestEDMDirectSidebarAndOpen(t *testing.T) {
	h := newHarness(t)
	h.user("admin", store.RoleAdmin)
	h.login("admin")
	h.fakeProbe()
	h.ui.Opener = stubOpener{}
	vsp := h.addDevice(&store.Device{Name: "VSP", Kind: store.KindEDMDirect, URL: "https://vsp.example"})
	ers := h.addDevice(&store.Device{Name: "ERS", Kind: store.KindERS, URL: "https://ers.example"})
	h.ui.UpdateFrameSources(context.Background())

	resp, body := h.get("/")
	if !strings.Contains(body, `href="https://vsp.example"`) || strings.Contains(body, `href="/open/1"`) {
		t.Fatalf("edm-direct sidebar href is not the device URL")
	}
	if !strings.Contains(body, `href="/open/2"`) {
		t.Fatal("ers-edm sidebar href is not /open/2")
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "https://vsp.example") {
		t.Fatalf("edm-direct origin missing from frame-src: %q", csp)
	}

	// edm-direct: direct redirect, no trust page, although https and unpinned.
	resp, _ = h.get("/open/" + itoa(vsp.ID))
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "https://vsp.example" {
		t.Fatalf("open edm-direct: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	// ers-edm: trust page continuing at /open/{id}.
	resp, body = h.get("/open/" + itoa(ers.ID))
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, testFingerprint) ||
		!strings.Contains(trustForm(body, ers.ID), `name="back" value="/open/`+itoa(ers.ID)+`"`) {
		t.Fatalf("open ers-edm trust page: %d\n%s", resp.StatusCode, body)
	}
}

func TestDeviceTrustRoute(t *testing.T) {
	h := newHarness(t)
	h.user("admin", store.RoleAdmin)
	h.user("bob", store.RoleUser)
	h.fakeProbe()
	vsp := h.addDevice(&store.Device{Name: "VSP", Kind: store.KindEDMDirect, URL: "https://vsp.example"})
	plain := h.addDevice(&store.Device{Name: "VSP http", Kind: store.KindEDMDirect, URL: "http://vsp.example"})
	link := h.addDevice(&store.Device{Name: "FW", Kind: store.KindLink, URL: "https://fw.example/"})

	u := newHarnessShared(t, h)
	u.login("bob")
	if resp, _ := u.get("/admin/devices/" + itoa(vsp.ID) + "/trust"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("user reached trust route: %d", resp.StatusCode)
	}

	h.login("admin")
	resp, body := h.get("/admin/devices/" + itoa(vsp.ID) + "/trust")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, testFingerprint) {
		t.Fatalf("trust page: %d", resp.StatusCode)
	}
	if strings.Contains(trustForm(body, vsp.ID), `name="back"`) {
		t.Fatal("trust page for edm-direct has a back field")
	}
	if trustForm(body, vsp.ID) == "" {
		t.Fatal("trust form does not post to the tls endpoint")
	}
	for name, id := range map[string]int64{"http edm-direct": plain.ID, "link": link.ID} {
		if resp, _ := h.get("/admin/devices/" + itoa(id) + "/trust"); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", name, resp.StatusCode)
		}
	}
}

func TestEDMDirectOpenModeFallback(t *testing.T) {
	h := newHarness(t)
	u := &store.User{OpenMode: config.OpenEmbedded}
	h.ui.Cfg.PublicURL = "https://portal.example"
	if m := h.ui.EffectiveOpenMode(u, &store.Device{Kind: store.KindEDMDirect, URL: "http://vsp.example", OpenMode: config.OpenEmbedded}); m != config.OpenTab {
		t.Fatalf("http edm-direct in https portal: %s", m)
	}
	// Never embedded: the switch's login cookie would be a third-party cookie.
	if m := h.ui.EffectiveOpenMode(u, &store.Device{Kind: store.KindEDMDirect, URL: "https://vsp.example", OpenMode: config.OpenEmbedded}); m != config.OpenTab {
		t.Fatalf("https edm-direct: %s", m)
	}
	if m := h.ui.EffectiveOpenMode(u, &store.Device{Kind: store.KindEDMDirect, URL: "https://vsp.example", OpenMode: config.OpenWindow}); m != config.OpenWindow {
		t.Fatalf("edm-direct window: %s", m)
	}
	// Through the proxy a VSP can be embedded.
	if m := h.ui.EffectiveOpenMode(u, &store.Device{Kind: store.KindVSP, URL: "https://vsp.example", OpenMode: config.OpenEmbedded}); m != config.OpenEmbedded {
		t.Fatalf("vsp-edm: %s", m)
	}
}

func TestDeviceFormEDMDirect(t *testing.T) {
	h := newHarness(t)
	h.user("admin", store.RoleAdmin)
	h.login("admin")
	secure := h.addDevice(&store.Device{Name: "VSP", Kind: store.KindEDMDirect, URL: "https://vsp.example"})
	pinned := h.addDevice(&store.Device{Name: "VSP2", Kind: store.KindEDMDirect, URL: "https://vsp2.example", TLSPin: testFingerprint})
	plain := h.addDevice(&store.Device{Name: "VSP3", Kind: store.KindEDMDirect, URL: "http://vsp3.example"})
	link := h.addDevice(&store.Device{Name: "FW", Kind: store.KindLink, URL: "https://fw.example/"})

	_, body := h.get("/admin/devices/new")
	if !strings.Contains(body, `<option value="edm-direct"`) || !strings.Contains(body, `<option value="vsp-edm"`) {
		t.Fatal("kind option edm-direct or vsp-edm missing")
	}
	vsp := h.addDevice(&store.Device{Name: "VSP4", Kind: store.KindVSP, URL: "https://vsp4.example"})
	_, vb := h.get("/admin/devices/" + itoa(vsp.ID) + "/edit")
	if !strings.Contains(vb, `<option value="vsp-edm" selected>`) || !strings.Contains(vb, `name="password" type="password" maxlength="64"`) {
		t.Fatal("vsp-edm: not selected or login limited to 15 characters (that is an ERS limit)")
	}
	_, body = h.get("/admin/devices/" + itoa(secure.ID) + "/edit")
	if !strings.Contains(body, `<option value="edm-direct" selected>`) {
		t.Fatal("edm-direct not selected")
	}
	if !strings.Contains(body, `action="/admin/devices/`+itoa(secure.ID)+`/credentials"`) {
		t.Fatal("login card missing")
	}
	if !strings.Contains(body, `href="/admin/devices/`+itoa(secure.ID)+`/trust"`) {
		t.Fatal("trust link missing for unpinned https edm-direct")
	}
	_, body = h.get("/admin/devices/" + itoa(pinned.ID) + "/edit")
	if strings.Contains(body, `/trust"`) || !strings.Contains(body, testFingerprint) {
		t.Fatal("pinned device: trust link shown or pin missing")
	}
	_, body = h.get("/admin/devices/" + itoa(plain.ID) + "/edit")
	if strings.Contains(body, `/trust"`) || !strings.Contains(body, `action="/admin/devices/`+itoa(plain.ID)+`/credentials"`) {
		t.Fatal("http edm-direct: trust link shown or login card missing")
	}
	_, body = h.get("/admin/devices/" + itoa(link.ID) + "/edit")
	if strings.Contains(body, `/credentials"`) {
		t.Fatal("link device shows the login card")
	}
}

type stubOpener struct{}

func (stubOpener) Open(w http.ResponseWriter, r *http.Request, d *store.Device) {
	http.Error(w, "opened", http.StatusTeapot)
}

// trustForm returns the confirmation form of a trust page (the layout has
// its own "back" field in the theme switcher).
func trustForm(body string, id int64) string {
	start := strings.Index(body, `action="/admin/devices/`+itoa(id)+`/tls"`)
	if start < 0 {
		return ""
	}
	end := strings.Index(body[start:], "</form>")
	if end < 0 {
		return body[start:]
	}
	return body[start : start+end]
}
