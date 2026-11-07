package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/lan-dot-party/ers-proxy/internal/devices"
	"github.com/lan-dot-party/ers-proxy/internal/monitor"
	"github.com/lan-dot-party/ers-proxy/internal/store"
	"github.com/lan-dot-party/ers-proxy/internal/switchcfg"
)

// fakeCfg is a Configurator that records calls and keeps changes in the store.
type fakeCfg struct {
	h          *harness
	caps       switchcfg.Capabilities
	capsOK     bool
	previewErr error
	warnings   []switchcfg.Warning
	previews   map[string]*switchcfg.Preview
	lastChange switchcfg.Change
	applied    int
	applyErr   error
}

func newFakeCfg(h *harness) *fakeCfg {
	return &fakeCfg{h: h, capsOK: true, previews: map[string]*switchcfg.Preview{},
		caps: switchcfg.Capabilities{Admin: true, Alias: true, PoE: true, PortVLANs: true, Hybrid: true,
			VLANCreate: true, VLANRename: true, VLANDelete: true, MaxAlias: 32, MaxVLANName: 20}}
}

func (f *fakeCfg) Capabilities(context.Context, int64) (switchcfg.Capabilities, bool) {
	return f.caps, f.capsOK
}

func (f *fakeCfg) Preview(_ context.Context, id int64, who switchcfg.Actor, c switchcfg.Change) (*switchcfg.Preview, error) {
	if f.previewErr != nil {
		return nil, f.previewErr
	}
	f.lastChange = c
	plan := &switchcfg.Plan{Driver: "voss", Summary: []string{"set port name to uplink"}, SaveKnown: false, Warnings: f.warnings,
		Steps: []switchcfg.Step{{Description: "write alias", Set: []switchcfg.Assignment{{Name: "ifAlias", Index: "1", Value: switchcfg.Text("uplink")}}}}}
	pv := &switchcfg.Preview{ID: "pv" + itoa(int64(len(f.previews)+1)), DeviceID: id, UserID: who.UserID, Change: c, Plan: plan, Hash: plan.Hash()}
	f.previews[pv.ID] = pv
	return pv, nil
}

func (f *fakeCfg) PreviewByID(id string, who switchcfg.Actor) (*switchcfg.Preview, bool) {
	pv := f.previews[id]
	if pv == nil || pv.UserID != who.UserID {
		return nil, false
	}
	return pv, true
}

func (f *fakeCfg) Apply(ctx context.Context, pid, hash string, ack bool, who switchcfg.Actor) (*store.ConfigChange, error) {
	f.applied++
	if f.applyErr != nil {
		return nil, f.applyErr
	}
	pv, ok := f.PreviewByID(pid, who)
	if !ok || pv.Hash != hash {
		return nil, switchcfg.ErrPreview
	}
	c := &store.ConfigChange{DeviceID: pv.DeviceID, UserID: who.UserID, Actor: who.Name, Deadline: time.Now().Add(2 * time.Minute),
		State: store.ChangePending, Summary: strings.Join(pv.Plan.Summary, "; "), Plan: "{}"}
	id, err := f.h.db.CreateConfigChange(ctx, c)
	if err != nil {
		return nil, err
	}
	return f.h.db.GetConfigChange(ctx, id)
}

func (f *fakeCfg) move(ctx context.Context, id int64, to string) (*store.ConfigChange, error) {
	ok, err := f.h.db.TransitionConfigChange(ctx, id, []string{store.ChangePending}, to, store.ChangeUpdate{Finished: true})
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, switchcfg.ErrNotPending
	}
	return f.h.db.GetConfigChange(ctx, id)
}

func (f *fakeCfg) Confirm(ctx context.Context, id int64, _ switchcfg.Actor) (*store.ConfigChange, error) {
	return f.move(ctx, id, store.ChangeConfirmedUnsaved)
}

func (f *fakeCfg) Revert(ctx context.Context, id int64, _ switchcfg.Actor) (*store.ConfigChange, error) {
	return f.move(ctx, id, store.ChangeReverted)
}

// enableConfigure turns the feature on for a harness (routes are registered
// when the router is built).
func (h *harness) enableConfigure() *fakeCfg {
	f := newFakeCfg(h)
	h.ui.Configure = f
	h.srv.Config.Handler = h.ui.SecurityHeaders(h.ui.SessionMiddleware(h.ui.Routes()))
	return f
}

func (h *harness) postForm(path string, form url.Values) *http.Response {
	form.Set("_csrf", h.csrf)
	return h.post(path, form, map[string]string{"Origin": h.srv.URL})
}

func (h *harness) postBody(path string, form url.Values) (*http.Response, string) {
	form.Set("_csrf", h.csrf)
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", h.srv.URL)
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp, b.String()
}

const configMarker = "/configure/"

func portForm() url.Values {
	return url.Values{"kind": {"port"}, "ifindex": {"1"}, "admin": {""}, "alias": {"uplink"}, "vlan_mode": {"access"}, "untagged": {"10"}}
}

func TestConfigureOffChangesNothing(t *testing.T) {
	h, id := switchHarness(t, nil)
	base := "/dashboard/" + itoa(id)
	for _, p := range []string{base, base + "/port/1", "/admin/devices/" + itoa(id) + "/edit"} {
		_, body := h.get(p)
		for _, bad := range []string{configMarker, "config-", "config_source", "snmp-write"} {
			if strings.Contains(body, bad) {
				t.Errorf("%s contains %q although configure is off", p, bad)
			}
		}
	}
	for _, p := range []string{base + "/configure/preview", base + "/configure/apply", base + "/configure/1/confirm",
		base + "/configure/1/revert", "/admin/devices/" + itoa(id) + "/snmp-write"} {
		if r := h.postForm(p, url.Values{"x": {"y"}}); r.StatusCode != http.StatusNotFound {
			t.Errorf("POST %s: %d", p, r.StatusCode)
		}
	}
	if resp, _ := h.get(base + "/configure/preview/abc"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET preview: %d", resp.StatusCode)
	}
	// Updating a device keeps its stored configuration source.
	ctx := context.Background()
	d, _ := h.db.DeviceByID(ctx, id)
	d.ConfigSource = store.ConfigSNMP
	if err := h.db.UpdateDevice(ctx, d); err != nil {
		t.Fatal(err)
	}
	form := url.Values{"name": {"sw1"}, "kind": {"ers-edm"}, "url": {"https://192.0.2.1"}, "monitor_source": {"snmp"},
		"monitor_interval": {"60"}, "snmp_port": {"161"}, "config_source": {"none"}, "open_mode": {"inherit"}}
	if r := h.postForm("/admin/devices/"+itoa(id), form); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("update: %d", r.StatusCode)
	}
	if d, _ = h.db.DeviceByID(ctx, id); d.ConfigSource != store.ConfigSNMP {
		t.Fatalf("config source changed to %q", d.ConfigSource)
	}
}

func TestConfigureNeedsAdmin(t *testing.T) {
	h, id := switchHarness(t, nil)
	h.enableConfigure()
	h.user("bob", store.RoleUser)
	b := newHarnessShared(t, h)
	b.login("bob")
	base := "/dashboard/" + itoa(id)
	for _, p := range []string{base, base + "/port/1"} {
		resp, body := b.get(p)
		if resp.StatusCode != 200 || strings.Contains(body, configMarker) || strings.Contains(body, "config-") {
			t.Errorf("%s shows configuration to a non-admin (%d)", p, resp.StatusCode)
		}
	}
	for _, p := range []string{base + "/configure/preview", base + "/configure/apply", base + "/configure/1/confirm",
		base + "/configure/1/revert", "/admin/devices/" + itoa(id) + "/snmp-write"} {
		if r := b.postForm(p, portForm()); r.StatusCode != http.StatusForbidden {
			t.Errorf("POST %s: %d", p, r.StatusCode)
		}
	}
	if resp, _ := b.get(base + "/configure/preview/x"); resp.StatusCode != http.StatusForbidden {
		t.Errorf("GET preview: %d", resp.StatusCode)
	}
}

func TestConfigureFlow(t *testing.T) {
	h, id := switchHarness(t, nil)
	f := h.enableConfigure()
	base := "/dashboard/" + itoa(id)
	ctx := context.Background()

	// The port page offers the form; the switch page the VLAN cards and history.
	_, body := h.get(base + "/port/1")
	for _, want := range []string{`class="card config-port"`, `name="vlan_mode"`, `name="alias"`, `maxlength="32"`, `name="tagged"`, `name="admin"`} {
		if !strings.Contains(body, want) {
			t.Errorf("port page lacks %q", want)
		}
	}
	if strings.Contains(body, `name="poe"`) {
		t.Error("PoE offered for a port without PoE")
	}
	_, body = h.get(base)
	for _, want := range []string{`config-vlans`, `name="vlan_op" value="create"`, `value="rename"`, `value="delete"`, `config-history`} {
		if !strings.Contains(body, want) {
			t.Errorf("switch page lacks %q", want)
		}
	}

	// Preview: only the changed field goes into the change.
	r := h.postForm(base+"/configure/preview", portForm())
	loc := r.Header.Get("Location")
	if r.StatusCode != http.StatusSeeOther || !strings.HasPrefix(loc, base+"/configure/preview/pv") {
		t.Fatalf("preview: %d %q", r.StatusCode, loc)
	}
	pc := f.lastChange.Ports
	if len(pc) != 1 || pc[0].Alias == nil || *pc[0].Alias != "uplink" || pc[0].VLANs != nil || pc[0].Admin != nil || pc[0].PoE != nil {
		t.Fatalf("change: %+v", f.lastChange)
	}
	_, body = h.get(loc)
	for _, want := range []string{"set port name to uplink", "write alias", `ifAlias.1 = &#34;uplink&#34;`, `name="hash"`, `name="preview"`,
		"cannot save this switch", "2:00 min"} {
		if !strings.Contains(body, want) {
			t.Errorf("preview page lacks %q", want)
		}
	}
	if strings.Contains(body, `name="ack"`) {
		t.Error("acknowledgement offered without warnings")
	}

	// Apply -> banner with countdown, forms hidden.
	pv := f.previews["pv1"]
	r = h.postForm(base+"/configure/apply", url.Values{"preview": {"pv1"}, "hash": {pv.Hash}})
	if r.StatusCode != http.StatusSeeOther || !strings.HasPrefix(r.Header.Get("Location"), base) {
		t.Fatalf("apply: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	open, err := h.db.OpenConfigChangeForDevice(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{base, base + "/port/1"} {
		_, body = h.get(p)
		for _, want := range []string{"config-countdown", `data-deadline="`, base + "/configure/" + itoa(open.ID) + "/confirm", "/revert", "set port name to uplink"} {
			if !strings.Contains(body, want) {
				t.Errorf("%s lacks %q while pending", p, want)
			}
		}
		if strings.Contains(body, `class="card config-port"`) || strings.Contains(body, "config-vlans") {
			t.Errorf("%s still offers forms while a change is open", p)
		}
	}

	// Confirm.
	r = h.postForm(base+"/configure/"+itoa(open.ID)+"/confirm", url.Values{})
	if r.StatusCode != http.StatusSeeOther {
		t.Fatalf("confirm: %d", r.StatusCode)
	}
	_, body = h.get(base)
	if strings.Contains(body, "config-countdown") || !strings.Contains(body, "config-banner") || !strings.Contains(body, "config-history") {
		t.Error("banner after confirm")
	}
	if !strings.Contains(body, `class="card config-port"`) && !strings.Contains(body, "config-vlans") {
		t.Error("forms not back after confirm")
	}

	// Second change, reverted.
	r = h.postForm(base+"/configure/preview", portForm())
	pv = f.previews["pv2"]
	h.postForm(base+"/configure/apply", url.Values{"preview": {"pv2"}, "hash": {pv.Hash}})
	open, _ = h.db.OpenConfigChangeForDevice(ctx, id)
	if open == nil {
		t.Fatal("no open change")
	}
	r = h.postForm(base+"/configure/"+itoa(open.ID)+"/revert", url.Values{})
	if r.StatusCode != http.StatusSeeOther {
		t.Fatalf("revert: %d", r.StatusCode)
	}
	if c, _ := h.db.GetConfigChange(ctx, open.ID); c.State != store.ChangeReverted {
		t.Fatalf("state %s", c.State)
	}
	// A change of another device is not reachable through this one's URL.
	if r = h.postForm("/dashboard/999/configure/"+itoa(open.ID)+"/confirm", url.Values{}); r.StatusCode != http.StatusNotFound {
		t.Fatalf("foreign device: %d", r.StatusCode)
	}

	var actions []string
	au, _ := h.db.ListAudit(ctx, 50)
	for _, e := range au {
		actions = append(actions, e.Action)
	}
	joined := strings.Join(actions, " ")
	for _, want := range []string{"config.apply", "config.confirm", "config.revert"} {
		if !strings.Contains(joined, want) {
			t.Errorf("audit lacks %s: %s", want, joined)
		}
	}
}

func TestConfigureBannerStates(t *testing.T) {
	h, id := switchHarness(t, nil)
	h.enableConfigure()
	ctx := context.Background()
	base := "/dashboard/" + itoa(id)
	mk := func(state, errText string) {
		if _, err := h.db.CreateConfigChange(ctx, &store.ConfigChange{DeviceID: id, Actor: "admin", State: state, Summary: "sum", Error: errText,
			Plan: "{}", Deadline: time.Now().Add(time.Minute), FinishedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	mk(store.ChangeRevertFailed, "switch unreachable")
	_, body := h.get(base)
	if !strings.Contains(body, "alert-error") || !strings.Contains(body, "switch unreachable") || strings.Contains(body, "config-countdown") {
		t.Error("revert_failed banner")
	}
	if strings.Contains(body, `name="vlan_op" value="create"`) {
		t.Error("forms shown while revert is failing")
	}
}

func TestConfigureRefusedAndErrors(t *testing.T) {
	h, id := switchHarness(t, nil)
	f := h.enableConfigure()
	base := "/dashboard/" + itoa(id)

	f.previewErr = switchcfg.Refuse("vlan_default", "VLAN 1")
	resp, body := h.postBody(base+"/configure/preview", portForm())
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "Das Standard-VLAN 1") && !strings.Contains(body, "default VLAN 1 cannot") {
		t.Fatalf("refused: %d %.300s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "(VLAN 1)") || !strings.Contains(body, base+"/port/1") {
		t.Error("detail or back link missing")
	}
	f.previewErr = switchcfg.ErrBusy
	if resp, body = h.postBody(base+"/configure/preview", portForm()); resp.StatusCode != http.StatusConflict || strings.Contains(body, "the switch has a change") {
		t.Fatalf("busy: %d", resp.StatusCode)
	}
	f.previewErr = nil

	// Nothing changed: refused before the manager is asked.
	form := portForm()
	form.Set("alias", "")
	form.Set("vlan_mode", "keep")
	port := h.ui.Monitor.(fakeMonitor).st.Snap.Port(1)
	form.Set("alias", port.Alias)
	if resp, body = h.postBody(base+"/configure/preview", form); resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "alert-error") {
		t.Fatalf("no change: %d", resp.StatusCode)
	}
	if len(f.previews) != 0 {
		t.Error("manager asked for an empty change")
	}
	// Access mode without a VLAN.
	form = portForm()
	form.Set("untagged", "0")
	if resp, _ = h.postBody(base+"/configure/preview", form); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("access without VLAN: %d", resp.StatusCode)
	}
	// VLAN forms.
	if r := h.postForm(base+"/configure/preview", url.Values{"kind": {"vlan"}, "vlan_op": {"create"}, "vlan_id": {"30"}, "vlan_name": {"guests"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("vlan create: %d", r.StatusCode)
	}
	if v := f.lastChange.VLANs; len(v) != 1 || v[0].Op != "create" || v[0].ID != 30 || v[0].Name != "guests" {
		t.Fatalf("vlan change %+v", f.lastChange)
	}
	if resp, _ = h.postBody(base+"/configure/preview", url.Values{"kind": {"vlan"}, "vlan_op": {"delete"}, "vlan_id": {"5000"}}); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("vlan id: %d", resp.StatusCode)
	}
	// A trunk with tagged VLANs.
	form = portForm()
	form.Set("vlan_mode", "trunk")
	form["tagged"] = []string{"10"}
	form.Set("alias", h.ui.Monitor.(fakeMonitor).st.Snap.Port(1).Alias)
	if r := h.postForm(base+"/configure/preview", form); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("trunk: %d", r.StatusCode)
	}
	if v := f.lastChange.Ports[0].VLANs; v == nil || v.Untagged != 0 || len(v.Tagged) != 1 || v.Tagged[0] != 10 {
		t.Fatalf("trunk change %+v", f.lastChange.Ports[0])
	}
	// Hybrid with only an untagged VLAN: the port tags, but carries the VLAN
	// untagged (VOSS "untag-port-default-vlan").
	form = portForm()
	form.Set("vlan_mode", "hybrid")
	form.Set("alias", h.ui.Monitor.(fakeMonitor).st.Snap.Port(1).Alias)
	if r := h.postForm(base+"/configure/preview", form); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("hybrid: %d", r.StatusCode)
	}
	if v := f.lastChange.Ports[0].VLANs; v == nil || v.Untagged != 10 || len(v.Tagged) != 0 || !v.Hybrid {
		t.Fatalf("hybrid change %+v", f.lastChange.Ports[0].VLANs)
	}
	form.Set("untagged", "0")
	if resp, _ = h.postBody(base+"/configure/preview", form); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("hybrid without untagged VLAN: %d", resp.StatusCode)
	}
}

func TestConfigureWarningsNeedAck(t *testing.T) {
	h, id := switchHarness(t, nil)
	f := h.enableConfigure()
	f.warnings = []switchcfg.Warning{{Code: "uplink", IfIndex: 1, Detail: "core"}, {Code: "mgmt_unknown"}}
	base := "/dashboard/" + itoa(id)
	r := h.postForm(base+"/configure/preview", portForm())
	loc := r.Header.Get("Location")
	_, body := h.get(loc)
	if !strings.Contains(body, `name="ack"`) || !strings.Contains(body, "uplink") || !strings.Contains(body, "(core)") {
		t.Fatal("warnings or acknowledgement missing")
	}
	pv := f.previews["pv1"]
	resp, body := h.postBody(base+"/configure/apply", url.Values{"preview": {"pv1"}, "hash": {pv.Hash}})
	if resp.StatusCode != http.StatusUnprocessableEntity || f.applied != 0 {
		t.Fatalf("applied without ack: %d, %d calls", resp.StatusCode, f.applied)
	}
	if !strings.Contains(body, "alert-error") || !strings.Contains(body, `name="ack"`) {
		t.Error("error not shown on the preview page")
	}
	if r = h.postForm(base+"/configure/apply", url.Values{"preview": {"pv1"}, "hash": {pv.Hash}, "ack": {"1"}}); r.StatusCode != http.StatusSeeOther || f.applied != 1 {
		t.Fatalf("apply with ack: %d, %d", r.StatusCode, f.applied)
	}
	// A failing apply is audited.
	f.applyErr = switchcfg.ErrStale
	h.postForm(base+"/configure/apply", url.Values{"preview": {"pv1"}, "hash": {pv.Hash}, "ack": {"1"}})
	au, _ := h.db.ListAudit(context.Background(), 20)
	found := false
	for _, e := range au {
		found = found || e.Action == "config.apply_failed"
	}
	if !found {
		t.Error("config.apply_failed not audited")
	}
}

func TestConfigureNeedsCSRF(t *testing.T) {
	h, id := switchHarness(t, nil)
	f := h.enableConfigure()
	base := "/dashboard/" + itoa(id)
	form := portForm() // no _csrf
	for _, p := range []string{base + "/configure/preview", base + "/configure/apply", base + "/configure/1/confirm", base + "/configure/1/revert"} {
		if r := h.post(p, form, map[string]string{"Origin": h.srv.URL}); r.StatusCode != http.StatusForbidden {
			t.Errorf("%s without CSRF: %d", p, r.StatusCode)
		}
		form.Set("_csrf", h.csrf)
		if r := h.post(p, form, map[string]string{"Origin": "null"}); r.StatusCode != http.StatusForbidden {
			t.Errorf("%s with Origin null: %d", p, r.StatusCode)
		}
		form.Del("_csrf")
	}
	if f.applied != 0 || len(f.previews) != 0 {
		t.Error("manager called")
	}
}

func TestConfigureDeviceSettings(t *testing.T) {
	h, id := switchHarness(t, nil)
	h.enableConfigure()
	ctx := context.Background()
	path := "/admin/devices/" + itoa(id)
	_, body := h.get(path + "/edit")
	for _, want := range []string{`name="config_source"`, `value="edm"`, `/snmp-write`, `id="snmp-write-form"`} {
		if !strings.Contains(body, want) {
			t.Errorf("edit page lacks %q", want)
		}
	}
	form := func(src, pw string) url.Values {
		v := url.Values{"name": {"sw1"}, "kind": {"ers-edm"}, "url": {"https://192.0.2.1"}, "monitor_source": {"snmp"},
			"monitor_interval": {"60"}, "snmp_port": {"161"}, "config_source": {src}, "open_mode": {"inherit"}}
		if pw != "" {
			v.Set("confirm_password", pw)
		}
		return v
	}
	// Turning it on needs the password.
	if resp, _ := h.postBody(path, form("snmp", "")); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("no password: %d", resp.StatusCode)
	}
	if d, _ := h.db.DeviceByID(ctx, id); d.ConfigSource != store.ConfigNone {
		t.Fatalf("source %q", d.ConfigSource)
	}
	if r := h.postForm(path, form("snmp", "correct horse battery")); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("with password: %d", r.StatusCode)
	}
	if d, _ := h.db.DeviceByID(ctx, id); d.ConfigSource != store.ConfigSNMP {
		t.Fatalf("source %q", d.ConfigSource)
	}
	// Changing it while on, or turning it off, needs no password.
	if r := h.postForm(path, form("none", "")); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("off: %d", r.StatusCode)
	}
	if d, _ := h.db.DeviceByID(ctx, id); d.ConfigSource != store.ConfigNone {
		t.Fatalf("source %q", d.ConfigSource)
	}
	// Bogus value.
	if resp, _ := h.postBody(path, form("bogus", "correct horse battery")); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("bogus: %d", resp.StatusCode)
	}
	au, _ := h.db.ListAudit(ctx, 20)
	found := false
	for _, e := range au {
		found = found || (e.Action == "device.update" && strings.Contains(e.Details, "configuration source none -> snmp"))
	}
	if !found {
		t.Error("source change not in the audit details")
	}
}

func TestSNMPWriteCard(t *testing.T) {
	h, id := switchHarness(t, nil)
	h.enableConfigure()
	ctx := context.Background()
	path := "/admin/devices/" + itoa(id) + "/snmp-write"
	secret := "w-s3cr3t-community"
	if r := h.postForm(path, url.Values{"community": {secret}}); !strings.Contains(r.Header.Get("Location"), "err=wrong_password") {
		t.Fatalf("no password: %q", r.Header.Get("Location"))
	}
	if d, _ := h.db.DeviceByID(ctx, id); d.HasSNMPWriteCredentials() {
		t.Fatal("stored without password")
	}
	if r := h.postForm(path, url.Values{"community": {secret}, "confirm_password": {"correct horse battery"}}); !strings.Contains(r.Header.Get("Location"), "ok=saved") {
		t.Fatalf("set: %q", r.Header.Get("Location"))
	}
	d, _ := h.db.DeviceByID(ctx, id)
	if !d.HasSNMPWriteCredentials() || strings.Contains(string(d.SNMPWriteCredentials), secret) || d.HasSNMPCredentials() {
		t.Fatal("write credentials missing, in plain text, or in the read slot")
	}
	c, err := devices.DecryptSNMPWrite(h.ui.Key, d)
	if err != nil || c.Community != secret {
		t.Fatalf("decrypt: %+v %v", c, err)
	}
	for _, p := range []string{"/admin/devices/" + itoa(id) + "/edit", "/admin/audit"} {
		if _, body := h.get(p); strings.Contains(body, secret) {
			t.Fatalf("%s echoes the community", p)
		}
	}
	if r := h.postForm(path, url.Values{"confirm_password": {"correct horse battery"}}); !strings.Contains(r.Header.Get("Location"), "err=snmp_invalid") {
		t.Fatalf("empty: %q", r.Header.Get("Location"))
	}
	h.postForm(path, url.Values{"clear": {"1"}})
	if d, _ = h.db.DeviceByID(ctx, id); !d.HasSNMPWriteCredentials() {
		t.Fatal("cleared without password")
	}
	h.postForm(path, url.Values{"clear": {"1"}, "confirm_password": {"correct horse battery"}})
	if d, _ = h.db.DeviceByID(ctx, id); d.HasSNMPWriteCredentials() {
		t.Fatal("not cleared")
	}
	var actions []string
	au, _ := h.db.ListAudit(ctx, 20)
	for _, e := range au {
		actions = append(actions, e.Action)
	}
	if j := strings.Join(actions, " "); !strings.Contains(j, "device.snmp_write.set") || !strings.Contains(j, "device.snmp_write.clear") {
		t.Fatalf("audit: %s", j)
	}
}

// Every code the UI can meet has a German and an English text.
func TestConfigureKeys(t *testing.T) {
	var keys []string
	for _, c := range []string{"uplink", "lag", "mgmt", "mgmt_unknown", "vlan_members"} {
		keys = append(keys, "config.warn."+c)
	}
	for _, c := range []string{"no_snapshot", "empty", "vlan_id", "vlan_twice", "vlan_exists", "vlan_name", "vlan_missing", "vlan_default",
		"vlan_mgmt", "vlan_restore", "vlan_op", "port", "port_twice", "port_state", "alias", "no_poe", "no_vlan", "hybrid", "vlan_dup",
		"vlan_both", "vlan_pvid", "no_change", "no_untagged"} {
		keys = append(keys, "config.refuse."+c)
	}
	for _, c := range []string{"disabled", "busy", "preview", "ack", "stale", "not_pending", "no_planner"} {
		keys = append(keys, "config.err."+c)
	}
	for _, c := range []string{store.ChangeApplying, store.ChangePending, store.ChangeConfirmed, store.ChangeConfirmedUnsaved,
		store.ChangeReverting, store.ChangeReverted, store.ChangeRevertFailed, store.ChangeFailed} {
		keys = append(keys, "config.state."+c)
	}
	for _, k := range keys {
		m, ok := messages[k]
		if !ok || m[0] == "" || m[1] == "" || m[0] == m[1] && !strings.HasPrefix(k, "config.state.") {
			t.Errorf("key %q missing or not translated: %v", k, m)
		}
	}
}

func TestAssignmentTextPortSet(t *testing.T) {
	snap := &monitor.Snapshot{Ports: []*monitor.Port{{IfIndex: 193, Name: "1/2"}, {IfIndex: 194, Name: "1/3"}}}
	b := switchcfg.SetBit(switchcfg.SetBit(nil, 193, true, 64), 194, true, 64)
	a := switchcfg.Assignment{Name: "rcVlanPortMembers", Index: "10", Value: switchcfg.Octets(b)}
	if got := assignmentText(a, snap); got != "rcVlanPortMembers.10 = 1/2, 1/3" {
		t.Errorf("got %q", got)
	}
	a.Value = switchcfg.Octets(make([]byte, 64))
	if got := assignmentText(a, snap); got != "rcVlanPortMembers.10 = –" {
		t.Errorf("empty: got %q", got)
	}
	n := switchcfg.Assignment{Name: "rcVlanPortDefaultVlanId", Index: "194", Value: switchcfg.Int(10)}
	if got := assignmentText(n, snap); got != "rcVlanPortDefaultVlanId.194 = 10" {
		t.Errorf("int: got %q", got)
	}
}
