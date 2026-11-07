package drivers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/lan-dot-party/ers-proxy/internal/monitor"
	"github.com/lan-dot-party/ers-proxy/internal/monitor/swossrc"
	"github.com/lan-dot-party/ers-proxy/internal/swos"
	"github.com/lan-dot-party/ers-proxy/internal/swos/swostest"
	"github.com/lan-dot-party/ers-proxy/internal/upstream"
)

// TestSwOSWebSource collects through the SwOS web API (swossrc) of the fake
// switch: VLANs, tagging, duplex and LAGs come from there.
func TestSwOSWebSource(t *testing.T) {
	srv := httptest.NewServer(swostest.New())
	defer srv.Close()
	c := &swos.Client{Transport: upstream.WithDigest(http.DefaultTransport,
		upstream.Login{Username: swostest.User, Password: swostest.Password}), Base: srv.URL}
	ctx := context.Background()
	r := &swossrc.Reader{C: c}
	sys, err := monitor.ReadSystem(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	d, err := monitor.NewRegistry(All()...).Detect(sys)
	if err != nil || d.Name() != "swos" {
		t.Fatalf("detect: %v %v", d, err)
	}
	s, err := d.Collect(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if s.System.Name != "swos-test" || s.System.Descr != "CRS305-1G-4S+ SwOS v2.18" || s.Units[0].Serial != "TEST0001" {
		t.Fatalf("system %+v units %+v", s.System, s.Units)
	}
	p1 := port(t, s, 1)
	if p1.Name != "ETH/BOOT" || p1.Alias != "office" || p1.Duplex != "full" || p1.Speed != 1e9 || p1.PVID != 20 ||
		!reflect.DeepEqual(p1.Untagged, []int{20}) || len(p1.Tagged) != 0 || p1.Mode != "access" || p1.InErrors != 1 {
		t.Errorf("port 1: %+v", p1)
	}
	p2 := port(t, s, 2)
	if p2.Name != "SFP1" || len(p2.Untagged) != 0 || !reflect.DeepEqual(p2.Tagged, []int{10, 20}) || p2.Mode != "trunk" ||
		p2.LAG != 129 || p2.LAGName != "Trunk1" || p2.InOctets != 1<<32|0x10 || !p2.HasHC || p2.InDiscards != 3 {
		t.Errorf("SFP1: %+v", p2)
	}
	if p3 := port(t, s, 3); p3.LAGName != "Trunk1" || p3.OutErrors != 2 {
		t.Errorf("SFP2: %+v", p3)
	}
	// "optional" mode, receive any, default VLAN 10: untagged member of 10.
	if p4 := port(t, s, 4); p4.Up() || p4.Duplex != "" || p4.PVID != 10 || !reflect.DeepEqual(p4.Untagged, []int{10}) {
		t.Errorf("SFP3: %+v", p4)
	}
	// VLAN mode disabled: no VLAN data; admin down.
	if p5 := port(t, s, 5); p5.Admin != "down" || p5.PVID != 0 || len(p5.Untagged)+len(p5.Tagged) != 0 {
		t.Errorf("SFP4: %+v", p5)
	}
	if len(s.VLANs) != 2 || s.VLANs[0] != (monitor.VLAN{ID: 10, Name: "servers"}) {
		t.Errorf("VLANs %+v", s.VLANs)
	}
	if !strings.Contains(strings.Join(s.Notes, " "), "derived") {
		t.Errorf("notes %v", s.Notes)
	}
}
