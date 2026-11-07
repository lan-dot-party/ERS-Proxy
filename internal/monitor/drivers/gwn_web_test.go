package drivers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/lan-dot-party/ers-proxy/internal/gwn"
	"github.com/lan-dot-party/ers-proxy/internal/gwn/gwntest"
	"github.com/lan-dot-party/ers-proxy/internal/monitor"
	"github.com/lan-dot-party/ers-proxy/internal/monitor/gwnsrc"
)

// TestGrandstreamWebSource collects through the GWN web API (gwnsrc) of the
// fake switch: VLANs, tagging, duplex, LAGs and LLDP come from there.
func TestGrandstreamWebSource(t *testing.T) {
	srv := httptest.NewServer(gwntest.New())
	defer srv.Close()
	c := &gwn.Client{Transport: http.DefaultTransport, Base: srv.URL, Username: gwntest.User, Password: gwntest.Password}
	ctx := context.Background()
	r := &gwnsrc.Reader{C: c}
	sys, err := monitor.ReadSystem(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	d, err := monitor.NewRegistry(All()...).Detect(sys)
	if err != nil || d.Name() != "grandstream" {
		t.Fatalf("detect: %v %v", d, err)
	}
	s, err := d.Collect(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Units) != 1 || s.Units[0].Model != "GWN7721" || s.Units[0].Firmware != "1.0.7.128" {
		t.Fatalf("units %+v", s.Units)
	}
	p1 := port(t, s, 1)
	if p1.Name != "2.5GE1" || p1.Alias != "office" || p1.Duplex != "full" || p1.Speed != 2.5e9 || p1.PVID != 1 ||
		!reflect.DeepEqual(p1.Untagged, []int{1}) || len(p1.Tagged) != 0 || p1.Mode != "access" || p1.InErrors != 7 {
		t.Errorf("port 1: %+v", p1)
	}
	// SetMode: any tagged VLAN makes the port a trunk.
	p9 := port(t, s, 9)
	if p9.Name != "SFP+9" || !reflect.DeepEqual(p9.Tagged, []int{100}) || !reflect.DeepEqual(p9.Untagged, []int{1}) ||
		p9.Mode != "trunk" || p9.Speed != 10e9 {
		t.Errorf("port 9: %+v", p9)
	}
	if p9.Neighbor == nil || p9.Neighbor.SysName != "core-1" {
		t.Errorf("port 9 neighbour: %+v", p9.Neighbor)
	}
	for _, i := range []int{3, 4} {
		if p := port(t, s, i); p.LAGName != "LAG1" {
			t.Errorf("port %d: LAG %d name %q", i, p.LAG, p.LAGName)
		}
	}
	if p5 := port(t, s, 5); p5.Admin != "down" || p5.Up() {
		t.Errorf("port 5: %+v", p5)
	}
	names := map[int]string{}
	for _, v := range s.VLANs {
		names[v.ID] = v.Name
	}
	if len(names) != 2 || names[1] != "default" || names[100] != "office" {
		t.Errorf("VLANs %+v", s.VLANs)
	}
}
