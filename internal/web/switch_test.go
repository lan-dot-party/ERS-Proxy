package web

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lan-dot-party/ers-proxy/internal/monitor"
	"github.com/lan-dot-party/ers-proxy/internal/monitor/poller"
	"github.com/lan-dot-party/ers-proxy/internal/store"
)

const xss = `<script>alert(1)</script>`

type fakeMonitor struct{ st poller.Status }

func (f fakeMonitor) All() []poller.Status { return []poller.Status{f.st} }
func (f fakeMonitor) Status(id int64) (poller.Status, bool) {
	return f.st, id == f.st.DeviceID
}

type fakeHistory struct{ samples []store.Sample }

func (f fakeHistory) Series(_ context.Context, _ int64, _ int, _ time.Duration) ([]store.Sample, error) {
	return f.samples, nil
}

func switchHarness(t *testing.T, samples []store.Sample) (*harness, int64) {
	t.Helper()
	h := newHarness(t)
	h.user("admin", store.RoleAdmin)
	d := &store.Device{Name: "sw1", Kind: store.KindERS, URL: "https://192.0.2.1", OpenMode: "tab", Isolation: "on",
		TLSMode: store.TLSAuto, MonitorSource: store.MonitorSNMP, MonitorDriver: store.DriverAuto, SNMPVersion: store.SNMPv2c, SNMPPort: 161}
	if err := h.db.CreateDevice(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	snap := &monitor.Snapshot{
		Driver: "ers",
		System: monitor.System{Name: "sw1", Descr: "ERS test", Uptime: 90 * time.Minute},
		Units:  []monitor.Unit{{Number: 1, Model: "ERS-X", Firmware: "1.2", Serial: "SN1"}},
		VLANs:  []monitor.VLAN{{ID: 10, Name: xss}},
		Ports: []*monitor.Port{
			{IfIndex: 1, Unit: 1, Number: 1, Name: "1/1", Alias: xss, Physical: true, Admin: "up", Oper: "up", Speed: 1e9,
				Duplex: "full", PVID: 10, Untagged: []int{10}, Mode: "access", InBps: 2e6, OutBps: 1e5, Utilization: 0.2,
				Neighbor: &monitor.Neighbor{SysName: xss, PortID: "p1", Bridge: true}},
			{IfIndex: 2, Unit: 1, Number: 2, Name: "1/2", Physical: true, Admin: "up", Oper: "down"},
		},
	}
	h.ui.Monitor = fakeMonitor{poller.Status{DeviceID: d.ID, Name: xss, Driver: "ers", Source: "snmp", LastPoll: time.Now(), Snap: snap}}
	h.ui.History = fakeHistory{samples}
	h.login("admin")
	return h, d.ID
}

func sampleSet() []store.Sample {
	now := time.Now().Truncate(time.Minute)
	var out []store.Sample
	for i := 10; i >= 1; i-- {
		out = append(out, store.Sample{Res: 60, TS: now.Add(-time.Duration(i) * time.Minute), InAvg: float64(i) * 1e6, OutAvg: 5e5,
			InMax: 2e7, OutMax: 1e6, InErrors: int64(i % 3), UpFrac: 1, N: 6})
	}
	return out
}

func TestSwitchAndPortPages(t *testing.T) {
	h, id := switchHarness(t, sampleSet())
	base := "/dashboard/" + itoa(id)
	for _, path := range []string{base, base + "/port/1", base + "/port/1?span=7d", base + "/port/2?span=bogus"} {
		resp, body := h.get(path)
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d", path, resp.StatusCode)
		}
		if strings.Contains(body, xss) {
			t.Errorf("%s: unescaped device text", path)
		}
		if !strings.Contains(body, "&lt;script&gt;alert(1)") {
			t.Errorf("%s: escaped text missing", path)
		}
	}
	_, body := h.get(base)
	for _, want := range []string{`class="tile s1g`, base + "/port/1", `/?open=` + itoa(id), `/admin/devices/` + itoa(id) + `/edit`, "port-table", "ERS-X"} {
		if !strings.Contains(body, want) {
			t.Errorf("switch page lacks %q", want)
		}
	}
	_, body = h.get(base + "/port/1")
	if !strings.Contains(body, `<svg class="chart"`) || !strings.Contains(body, "line-in") || strings.Contains(body, "empty-hist") {
		t.Error("port page without chart")
	}
	if strings.Contains(body, "line-in-max") {
		t.Error("max lines at 60 s resolution")
	}
}

func TestPortPageEmptyHistoryAndMaxLines(t *testing.T) {
	h, id := switchHarness(t, nil)
	_, body := h.get("/dashboard/" + itoa(id) + "/port/1")
	if !strings.Contains(body, "empty-hist") || strings.Contains(body, `<svg class="chart"`) {
		t.Error("expected empty state")
	}
	s := sampleSet()
	for i := range s {
		s[i].Res = 900
	}
	h.ui.History = fakeHistory{s}
	_, body = h.get("/dashboard/" + itoa(id) + "/port/1?span=7d")
	if !strings.Contains(body, "line-in-max") {
		t.Error("expected max lines for coarse resolution")
	}
}

func TestSwitchPagesNotFoundAndLogin(t *testing.T) {
	h, id := switchHarness(t, nil)
	for _, p := range []string{"/dashboard/9999", "/dashboard/x", "/dashboard/" + itoa(id) + "/port/99", "/dashboard/" + itoa(id) + "/port/x", "/dashboard/9999/port/1"} {
		if resp, _ := h.get(p); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", p, resp.StatusCode)
		}
	}
	h2 := newHarness(t)
	h2.ui.Monitor = h.ui.Monitor
	for _, p := range []string{"/dashboard/" + itoa(id), "/dashboard/" + itoa(id) + "/port/1"} {
		resp, _ := h2.get(p)
		if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/login") {
			t.Errorf("%s anonymous: %d", p, resp.StatusCode)
		}
	}
}

func itoa(i int64) string { return strconv.FormatInt(i, 10) }
