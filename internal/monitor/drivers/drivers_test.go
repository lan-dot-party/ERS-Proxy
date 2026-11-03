package drivers

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/lan-dot-party/ers-proxy/internal/monitor"
	"github.com/lan-dot-party/ers-proxy/internal/monitor/snmpsrc"
	"github.com/lan-dot-party/ers-proxy/internal/snmptest"
)

func collect(t *testing.T, community string) (*monitor.Snapshot, monitor.Driver) {
	t.Helper()
	target := snmptest.Simulator(t, snmptest.DataDir(t), community)
	target.Timeout = 3 * time.Second
	ctx := context.Background()
	r, err := snmpsrc.Open(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	sys, err := monitor.ReadSystem(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	d, err := monitor.NewRegistry(All()...).Detect(sys)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := d.Collect(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	return snap, d
}

func port(t *testing.T, s *monitor.Snapshot, ifIndex int) *monitor.Port {
	t.Helper()
	p := s.Port(ifIndex)
	if p == nil {
		t.Fatalf("port %d missing", ifIndex)
	}
	return p
}

func TestGenericAgainstSimulator(t *testing.T) {
	s, d := collect(t, "generic")
	if d.Name() != "generic" || s.System.Name != "gen-sw1" || s.System.Uptime != 24*time.Hour {
		t.Fatalf("driver %s, system %+v", d.Name(), s.System)
	}
	p1 := port(t, s, 1)
	if !p1.Up() || p1.Speed != 1_000_000_000 || p1.Duplex != "full" || p1.Unit != 1 || p1.Number != 1 || p1.Alias != "printer" {
		t.Errorf("port 1: %+v", p1)
	}
	if p1.PoE == nil || p1.PoE.Status != "delivering" || p1.PoE.Class != 2 {
		t.Errorf("port 1 PoE: %+v", p1.PoE)
	}
	if p4 := port(t, s, 4); p4.Duplex != "half" || p4.Speed != 100_000_000 || p4.PVID != 10 ||
		!reflect.DeepEqual(p4.Untagged, []int{10}) || p4.Mode != "access" {
		t.Errorf("port 4: %+v", p4)
	}
	if p6 := port(t, s, 6); p6.Admin != "down" || p6.Up() {
		t.Errorf("port 6 should be admin down: %+v", p6)
	}
	p8 := port(t, s, 8)
	if !reflect.DeepEqual(p8.Untagged, []int{1}) || !reflect.DeepEqual(p8.Tagged, []int{10, 20}) || p8.Mode != "trunk" {
		t.Errorf("port 8 VLANs: untagged %v tagged %v mode %s", p8.Untagged, p8.Tagged, p8.Mode)
	}
	if p8.LAG != 100 || p8.LAGName != "Po1" {
		t.Errorf("port 8 LAG: %d %q", p8.LAG, p8.LAGName)
	}
	if p8.Neighbor == nil || p8.Neighbor.SysName != "core-sw" || !p8.Neighbor.Bridge || p8.Neighbor.PortID != "1/49" {
		t.Errorf("port 8 neighbour: %+v", p8.Neighbor)
	}
	if port(t, s, 2).InErrors != 3 || port(t, s, 3).InOctets != 3_000_000 || !port(t, s, 3).HasHC {
		t.Error("counters not read")
	}
	if got := len(s.VLANs); got != 3 || s.VLANs[1].Name != "office" {
		t.Errorf("VLANs %+v", s.VLANs)
	}
	if s.PoE == nil || s.PoE.TotalW != 370 || s.PoE.UsedW != 12 {
		t.Errorf("PoE budget %+v", s.PoE)
	}
	if lag := port(t, s, 100); lag.Physical || !lag.IsLAG || lag.Number != 0 {
		t.Errorf("LAG interface %+v", lag)
	}
	if s.Ports[0].IfIndex != 1 || s.Ports[len(s.Ports)-1].Number != 0 {
		t.Error("front-panel ports must come first")
	}
}

func TestERSAgainstSimulator(t *testing.T) {
	s, d := collect(t, "ers5600")
	if d.Name() != "ers" {
		t.Fatalf("detected %s", d.Name())
	}
	if len(s.Units) != 2 || s.Units[1].Number != 2 || s.Units[1].Model != "5650TD" || s.Units[1].Firmware != "v6.6.3.015" {
		t.Fatalf("units %+v", s.Units)
	}
	u2p1 := port(t, s, 129)
	if u2p1.Unit != 2 || u2p1.Number != 1 || !reflect.DeepEqual(u2p1.Untagged, []int{1}) || u2p1.Mode != "access" {
		t.Errorf("unit 2 port 1: %+v", u2p1)
	}
	trunk := port(t, s, 1) // tagAll
	if !reflect.DeepEqual(trunk.Tagged, []int{1, 20}) || len(trunk.Untagged) != 0 || trunk.Mode != "trunk" {
		t.Errorf("port 1/1: tagged %v untagged %v mode %s", trunk.Tagged, trunk.Untagged, trunk.Mode)
	}
	hybrid := port(t, s, 2) // untagPvidOnly, PVID 1
	if !reflect.DeepEqual(hybrid.Untagged, []int{1}) || !reflect.DeepEqual(hybrid.Tagged, []int{20}) || hybrid.Mode != "hybrid" {
		t.Errorf("port 1/2: tagged %v untagged %v mode %s", hybrid.Tagged, hybrid.Untagged, hybrid.Mode)
	}
	if hybrid.PoE == nil || hybrid.PoE.Status != "delivering" {
		t.Errorf("port 1/2 PoE %+v", hybrid.PoE)
	}
	if p := port(t, s, 3); p.LAG != 1 || p.LAGName != "to-core" || !reflect.DeepEqual(p.Untagged, []int{30}) {
		t.Errorf("port 1/3: %+v", p)
	}
	if p := port(t, s, 1); p.Neighbor == nil || p.Neighbor.SysName != "core-sw" {
		t.Errorf("LLDP neighbour on 1/1: %+v", p.Neighbor)
	}
	if p := port(t, s, 55001); p.Physical || p.Unit != 0 { // out-of-band port, not 430/89
		t.Errorf("OOB port counted as front-panel port: %+v", p)
	}
	if p := port(t, s, 130); p.Up() || p.Speed != 0 {
		t.Errorf("unit 2 port 2 should be down: %+v", p)
	}
	if len(s.VLANs) != 3 || s.VLANs[2].Name != "cameras" || len(s.Notes) == 0 {
		t.Errorf("VLANs %+v notes %v", s.VLANs, s.Notes)
	}
}

func TestERSTagging(t *testing.T) {
	cases := []struct {
		mode, vid, pvid int
		want            bool
	}{
		{1, 5, 1, true}, {2, 5, 1, false}, {3, 1, 1, true}, {3, 5, 1, false}, {4, 1, 1, false}, {4, 5, 1, true}, {0, 5, 1, false},
	}
	for _, c := range cases {
		if got := ersTagged(c.mode, c.vid, c.pvid); got != c.want {
			t.Errorf("ersTagged(%d,%d,%d) = %v", c.mode, c.vid, c.pvid, got)
		}
	}
	if PortsPerUnit(monitor.System{Descr: "Ethernet Routing Switch 5698TFD-PWR"}) != 128 ||
		PortsPerUnit(monitor.System{Descr: "Ethernet Routing Switch 4850GTS"}) != 64 {
		t.Error("PortsPerUnit")
	}
}

func TestCiscoAgainstSimulator(t *testing.T) {
	s, d := collect(t, "cisco")
	if d.Name() != "cisco" || s.Driver != "cisco" {
		t.Fatalf("detected %s", d.Name())
	}
	if len(s.VLANs) != 3 || s.VLANs[0].ID != 1 || s.VLANs[1].Name != "users" || s.VLANs[2].Name != "voice" {
		t.Errorf("VLANs (1002-1005 must be skipped): %+v", s.VLANs)
	}
	if p := port(t, s, 1); p.PVID != 10 || !reflect.DeepEqual(p.Untagged, []int{10}) || len(p.Tagged) != 0 || p.Mode != "access" {
		t.Errorf("port 1: %+v", p)
	}
	if p := port(t, s, 3); p.PVID != 1 || p.Mode != "access" {
		t.Errorf("port 3: %+v", p)
	}
	// 1002 is reserved and 1500 does not exist: neither may appear.
	if p := port(t, s, 5); p.PVID != 1 || !reflect.DeepEqual(p.Untagged, []int{1}) ||
		!reflect.DeepEqual(p.Tagged, []int{10, 20}) || p.Mode != "trunk" {
		t.Errorf("port 5: pvid %d untagged %v tagged %v mode %s", p.PVID, p.Untagged, p.Tagged, p.Mode)
	}
	if p := port(t, s, 6); p.PVID != 20 || !reflect.DeepEqual(p.Untagged, []int{20}) || len(p.Tagged) != 0 || p.Mode != "access" {
		t.Errorf("port 6 (notTrunking): %+v", p)
	}
}

func TestCiscoTrunkBitmaps(t *testing.T) {
	bm := func(size int, pos ...int) []byte {
		b := make([]byte, size)
		for _, i := range pos {
			b[i/8] |= 0x80 >> (i % 8)
		}
		return b
	}
	got := ciscoTrunkVLANs([4][]byte{bm(128, 0, 1, 1023), bm(128, 0, 476), bm(128, 5), bm(128, 1023)})
	want := []int{1, 1023, 1024, 1500, 2053, 4095}
	if !reflect.DeepEqual(got, want) { // VLAN 0 is dropped
		t.Errorf("got %v, want %v", got, want)
	}
	if got := ciscoTrunkVLANs([4][]byte{}); len(got) != 0 {
		t.Errorf("empty: %v", got)
	}
}

func TestTPLinkAgainstSimulator(t *testing.T) {
	// Modelled on an Omada SG2210P: no Q-BRIDGE, TPLINK-DOT1Q-VLAN-MIB.
	s, d := collect(t, "tplink")
	if d.Name() != "tplink" || s.Driver != "tplink" {
		t.Fatalf("detected %s", d.Name())
	}
	if p := port(t, s, 100); p.Physical { // Vlan-interface100 has ifType 6
		t.Errorf("VLAN interface counted as port: %+v", p)
	}
	if p := port(t, s, 32769); p.Physical || !p.IsLAG {
		t.Errorf("port-channel: %+v", p)
	}
	if p := port(t, s, 49157); p.Unit != 1 || p.Number != 5 || p.PVID != 140 ||
		!reflect.DeepEqual(p.Untagged, []int{140}) || len(p.Tagged) != 0 || p.Mode != "access" {
		t.Errorf("port 1/0/5: %+v", p)
	}
	if p := port(t, s, 49154); p.Number != 2 || !reflect.DeepEqual(p.Untagged, []int{111}) ||
		!reflect.DeepEqual(p.Tagged, []int{7, 55, 100, 140}) || p.Mode != "trunk" {
		t.Errorf("port 1/0/2: %+v", p)
	}
	if p := port(t, s, 49161); p.LAG != 32769 || p.LAGName != "LAG1" || !reflect.DeepEqual(p.Tagged, []int{7, 55, 100, 140}) {
		t.Errorf("LAG member 1/0/9: %+v", p)
	}
	if p := port(t, s, 49157); p.PoE == nil || p.PoE.Status != "delivering" || p.PoE.Class != 2 || p.PoE.PowerMW != 1500 {
		t.Errorf("PoE 1/0/5: %+v", p.PoE)
	}
	if p := port(t, s, 49156); p.PoE == nil || p.PoE.Status != "disabled" {
		t.Errorf("PoE 1/0/4: %+v", p.PoE)
	}
	if p := port(t, s, 49153); p.PoE == nil || p.PoE.Status != "searching" || p.PoE.PowerMW != 0 {
		t.Errorf("PoE 1/0/1: %+v", p.PoE)
	}
	if p := port(t, s, 49161); p.PoE != nil {
		t.Errorf("PoE on 1/0/9 (no PoE port): %+v", p.PoE)
	}
	if s.PoE == nil || s.PoE.TotalW != 61 || s.PoE.UsedW != 2 {
		t.Errorf("PoE budget %+v", s.PoE)
	}
	phys := 0
	for _, p := range s.Ports {
		if p.Physical {
			phys++
		}
	}
	if phys != 10 || len(s.VLANs) != 6 || s.VLANs[4].Name != "users" || s.VLANs[1].Name != "" {
		t.Errorf("%d ports, VLANs %+v", phys, s.VLANs)
	}
}

func TestTPLinkQBridgeFallback(t *testing.T) {
	s, _ := collect(t, "tplink-qbridge")
	if p := port(t, s, 3); p.PVID != 10 || !reflect.DeepEqual(p.Untagged, []int{10}) || p.Mode != "access" {
		t.Errorf("port 3: %+v", p)
	}
	if p := port(t, s, 8); !reflect.DeepEqual(p.Tagged, []int{10, 20}) || !reflect.DeepEqual(p.Untagged, []int{1}) || p.Mode != "trunk" {
		t.Errorf("port 8: %+v", p)
	}
	if len(s.VLANs) != 3 {
		t.Errorf("VLANs %+v", s.VLANs)
	}
}

func TestTPPortList(t *testing.T) {
	byName := map[string]*monitor.Port{}
	for _, n := range []string{"1/0/1", "1/0/2", "1/0/3", "1/0/6", "LAG1"} {
		byName[n] = &monitor.Port{Name: n}
	}
	var got []string
	for _, p := range tpPortList("1/0/1-3, 1/0/6,lag1,1/0/9,garbage-x", byName) {
		got = append(got, p.Name)
	}
	if !reflect.DeepEqual(got, []string{"1/0/1", "1/0/2", "1/0/3", "1/0/6", "LAG1"}) {
		t.Fatalf("got %v", got)
	}
}
