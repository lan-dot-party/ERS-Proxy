package rcplan

import (
	"bytes"
	"context"
	"testing"

	"github.com/lan-dot-party/ers-proxy/internal/monitor"
	"github.com/lan-dot-party/ers-proxy/internal/switchcfg"
	"github.com/lan-dot-party/ers-proxy/internal/switchcfg/switchcfgtest"
)

func ptr[T any](v T) *T { return &v }

// same compares two switch states, ignoring trailing zero octets.
func same(a, b map[string]map[string]monitor.Value) bool {
	for _, pair := range [][2]map[string]map[string]monitor.Value{{a, b}, {b, a}} {
		for name, m := range pair[0] {
			for idx, v := range m {
				w, ok := pair[1][name][idx]
				if !ok {
					return false
				}
				if v.Kind == monitor.KindBytes {
					if !bytes.Equal(bytes.TrimRight(v.Bytes, "\x00"), bytes.TrimRight(w.Bytes, "\x00")) {
						return false
					}
				} else if v.Kind != w.Kind || v.Int != w.Int || v.Uint != w.Uint || v.OID != w.OID {
					return false
				}
			}
		}
	}
	return true
}

func apply(t *testing.T, sw *switchcfgtest.Switch, steps []switchcfg.Step) {
	t.Helper()
	for _, s := range steps {
		if err := sw.Set(context.Background(), s.Set); err != nil {
			t.Fatalf("step %q: %v", s.Description, err)
		}
	}
}

func verify(t *testing.T, sw *switchcfgtest.Switch, want []switchcfg.Assignment) {
	t.Helper()
	ms, err := switchcfg.Verify(context.Background(), sw, want)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) > 0 {
		t.Fatal(switchcfg.MismatchError(ms))
	}
}

// roundTrip plans c, applies it, checks the read-back, applies the inverse
// and checks that the switch is exactly as before.
func roundTrip(t *testing.T, c switchcfg.Change) (*switchcfg.Plan, *switchcfgtest.Switch, map[string]map[string]monitor.Value) {
	t.Helper()
	sw := switchcfgtest.New()
	snap := sw.VOSS()
	orig := sw.Snapshot()
	plan, err := VOSS().Plan(context.Background(), sw, snap, c)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	apply(t, sw, plan.Steps)
	verify(t, sw, plan.Expected)
	changed := sw.Snapshot()
	apply(t, sw, plan.Inverse)
	verify(t, sw, plan.Before)
	if !same(orig, sw.Snapshot()) {
		t.Fatalf("inverse did not restore the switch:\nbefore %v\nafter  %v", orig, sw.Snapshot())
	}
	return plan, sw, changed
}

func members(t *testing.T, st map[string]map[string]monitor.Value, vid string) []int {
	t.Helper()
	return monitor.BitsMSB(st["rcVlanPortMembers"][vid].Bytes)
}

func TestAccessPortMove(t *testing.T) {
	plan, _, st := roundTrip(t, switchcfg.Change{Ports: []switchcfg.PortChange{
		{IfIndex: 193, VLANs: &switchcfg.PortVLANs{Untagged: 10}},
	}})
	if got := members(t, st, "1"); len(got) != 1 || got[0] != 192 {
		t.Errorf("VLAN 1 members = %v", got)
	}
	if got := members(t, st, "10"); len(got) != 2 {
		t.Errorf("VLAN 10 members = %v", got)
	}
	if v, _ := st["rcVlanPortDefaultVlanId"]["193"].AsInt(); v != 10 {
		t.Errorf("PVID = %d", v)
	}
	// Untagged-only port: remove from the old VLAN before adding to the new.
	var order []string
	for _, s := range plan.Steps {
		order = append(order, s.Description)
	}
	if len(order) != 3 || order[0] != "remove VLAN members" || order[1] != "add VLAN members" || order[2] != "set PVIDs" {
		t.Errorf("steps = %v", order)
	}
	if len(plan.Warnings) != 0 {
		t.Errorf("warnings = %v", plan.Warnings)
	}
}

func TestAccessToTrunk(t *testing.T) {
	plan, _, st := roundTrip(t, switchcfg.Change{Ports: []switchcfg.PortChange{
		{IfIndex: 194, VLANs: &switchcfg.PortVLANs{Tagged: []int{30, 20}}},
	}})
	if v, _ := st["rcVlanPortPerformTagging"]["194"].AsInt(); v != tagAll {
		t.Errorf("tagging = %d", v)
	}
	if v, _ := st["rcVlanPortDefaultVlanId"]["194"].AsInt(); v != 20 {
		t.Errorf("PVID = %d, want the lowest tagged VLAN", v)
	}
	if plan.Steps[0].Description != "enable tagging" {
		t.Errorf("first step %q: tagging must come before the second membership", plan.Steps[0].Description)
	}
	// The inverse switches tagging off only after the memberships are back.
	if last := plan.Inverse[len(plan.Inverse)-1].Description; last != "disable tagging" {
		t.Errorf("last inverse step %q", last)
	}
}

func TestCreateVLANAndUseIt(t *testing.T) {
	plan, _, st := roundTrip(t, switchcfg.Change{
		VLANs: []switchcfg.VLANChange{{Op: switchcfg.VLANCreate, ID: 40, Name: "Guests"}},
		Ports: []switchcfg.PortChange{{IfIndex: 193, VLANs: &switchcfg.PortVLANs{Untagged: 40}}},
	})
	if st["rcVlanName"]["40"].String() != "Guests" {
		t.Errorf("VLAN 40 = %v", st["rcVlanName"]["40"])
	}
	if plan.Steps[0].Description != "create VLAN 40" {
		t.Errorf("first step = %q", plan.Steps[0].Description)
	}
	if plan.Inverse[len(plan.Inverse)-1].Description != "delete VLAN 40" {
		t.Errorf("inverse must delete the VLAN last: %v", plan.Inverse)
	}
}

func TestDeleteVLANWithMembers(t *testing.T) {
	plan, _, st := roundTrip(t, switchcfg.Change{
		VLANs: []switchcfg.VLANChange{{Op: switchcfg.VLANDelete, ID: 30}},
	})
	if _, ok := st["rcVlanName"]["30"]; ok {
		t.Error("VLAN 30 still exists")
	}
	if len(plan.Warnings) != 1 || plan.Warnings[0].Code != "vlan_members" {
		t.Errorf("warnings = %v", plan.Warnings)
	}
}

func TestPortStateNameAndPoE(t *testing.T) {
	plan, _, st := roundTrip(t, switchcfg.Change{Ports: []switchcfg.PortChange{
		{IfIndex: 192, Admin: ptr(false), Alias: ptr("uplink core")},
		{IfIndex: 194, PoE: ptr(false)},
	}})
	if v, _ := st["ifAdminStatus"]["192"].AsInt(); v != 2 {
		t.Errorf("admin = %d", v)
	}
	if st["ifAlias"]["192"].String() != "uplink core" {
		t.Errorf("alias = %q", st["ifAlias"]["192"].String())
	}
	if v, _ := st["pethPsePortAdminEnable"]["1.3"].AsInt(); v != 2 {
		t.Errorf("PoE = %d", v)
	}
	// 1/1 is in the management VLAN 100: disabling it needs an acknowledgement.
	if len(plan.Warnings) != 1 || plan.Warnings[0].Code != "mgmt" || plan.Warnings[0].VLAN != 100 {
		t.Errorf("warnings = %v", plan.Warnings)
	}
}

func TestWarnings(t *testing.T) {
	sw := switchcfgtest.New()
	snap := sw.VOSS()
	snap.Port(193).Neighbor = &monitor.Neighbor{SysName: "core"}
	snap.Port(193).LAG, snap.Port(193).LAGName = 1, "MLT 1"
	plan, err := VOSS().Plan(context.Background(), sw, snap, switchcfg.Change{Ports: []switchcfg.PortChange{
		{IfIndex: 193, VLANs: &switchcfg.PortVLANs{Untagged: 10}},
		{IfIndex: 192, VLANs: &switchcfg.PortVLANs{Untagged: 1}}, // leaves VLAN 100
	}})
	if err != nil {
		t.Fatal(err)
	}
	codes := map[string]bool{}
	for _, w := range plan.Warnings {
		codes[w.Code] = true
	}
	for _, c := range []string{"uplink", "lag", "mgmt"} {
		if !codes[c] {
			t.Errorf("missing warning %q in %v", c, plan.Warnings)
		}
	}
}

func TestRefusals(t *testing.T) {
	cases := map[string]switchcfg.Change{
		"empty":        {},
		"vlan_default": {VLANs: []switchcfg.VLANChange{{Op: switchcfg.VLANDelete, ID: 1}}},
		"vlan_mgmt":    {VLANs: []switchcfg.VLANChange{{Op: switchcfg.VLANDelete, ID: 100}}},
		"vlan_pvid":    {VLANs: []switchcfg.VLANChange{{Op: switchcfg.VLANDelete, ID: 20}}},
		"vlan_exists":  {VLANs: []switchcfg.VLANChange{{Op: switchcfg.VLANCreate, ID: 10, Name: "x"}}},
		"vlan_missing": {Ports: []switchcfg.PortChange{{IfIndex: 193, VLANs: &switchcfg.PortVLANs{Untagged: 99}}}},
		"vlan_id":      {VLANs: []switchcfg.VLANChange{{Op: switchcfg.VLANCreate, ID: 4095, Name: "x"}}},
		"vlan_name":    {VLANs: []switchcfg.VLANChange{{Op: switchcfg.VLANCreate, ID: 50, Name: " "}}},
		// 1/4 has no rcVlanPortUntagDefaultVlan.
		"hybrid":      {Ports: []switchcfg.PortChange{{IfIndex: 195, VLANs: &switchcfg.PortVLANs{Untagged: 20, Tagged: []int{30}}}}},
		"no_untagged": {Ports: []switchcfg.PortChange{{IfIndex: 193, VLANs: &switchcfg.PortVLANs{Hybrid: true, Tagged: []int{10}}}}},
		"no_vlan":     {Ports: []switchcfg.PortChange{{IfIndex: 193, VLANs: &switchcfg.PortVLANs{}}}},
		"vlan_dup":    {Ports: []switchcfg.PortChange{{IfIndex: 195, VLANs: &switchcfg.PortVLANs{Tagged: []int{20, 20}}}}},
		"port":        {Ports: []switchcfg.PortChange{{IfIndex: 999, Admin: ptr(true)}}},
		"no_change":   {Ports: []switchcfg.PortChange{{IfIndex: 192, Admin: ptr(true)}}},
		"alias":       {Ports: []switchcfg.PortChange{{IfIndex: 192, Alias: ptr("tab\there")}}},
		"vlan_twice": {VLANs: []switchcfg.VLANChange{
			{Op: switchcfg.VLANCreate, ID: 50, Name: "a"}, {Op: switchcfg.VLANRename, ID: 50, Name: "b"}}},
	}
	for code, c := range cases {
		sw := switchcfgtest.New()
		snap := sw.VOSS()
		_, err := VOSS().Plan(context.Background(), sw, snap, c)
		var r *switchcfg.Refused
		if !switchcfg.IsRefused(err) {
			t.Errorf("%s: err = %v, want a refusal", code, err)
			continue
		}
		r = err.(*switchcfg.Refused)
		if r.Code != code {
			t.Errorf("%s: refused with %q (%v)", code, r.Code, err)
		}
		if sw.WriteCount() != 0 {
			t.Errorf("%s: planning wrote to the switch", code)
		}
	}
}

func TestHash(t *testing.T) {
	sw := switchcfgtest.New()
	snap := sw.VOSS()
	c := switchcfg.Change{Ports: []switchcfg.PortChange{{IfIndex: 193, VLANs: &switchcfg.PortVLANs{Untagged: 10}}}}
	a, err := VOSS().Plan(context.Background(), sw, snap, c)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := VOSS().Plan(context.Background(), sw, snap, c)
	if a.Hash() != b.Hash() {
		t.Error("same change, different hash")
	}
	c.Ports[0].VLANs.Untagged = 1
	c.Ports[0].IfIndex = 194
	d, _ := VOSS().Plan(context.Background(), sw, snap, c)
	if d.Hash() == a.Hash() {
		t.Error("different change, same hash")
	}
}

// Over EDM the IP address table is not available: the planner cannot tell
// the management VLAN, so risky changes need an acknowledgement.
func TestManagementUnknown(t *testing.T) {
	sw := switchcfgtest.New()
	snap := sw.VOSS()
	sw.Delete("ipAdEntIfIndex", "10.0.0.2")
	risky := []switchcfg.Change{
		{Ports: []switchcfg.PortChange{{IfIndex: 193, VLANs: &switchcfg.PortVLANs{Untagged: 10}}}},
		{Ports: []switchcfg.PortChange{{IfIndex: 193, Admin: ptr(false)}}},
		{VLANs: []switchcfg.VLANChange{{Op: switchcfg.VLANDelete, ID: 30}}},
		// Without the table even the management VLAN itself is not refused,
		// only warned about.
		{VLANs: []switchcfg.VLANChange{{Op: switchcfg.VLANDelete, ID: 100}}},
	}
	for i, c := range risky {
		plan, err := VOSS().Plan(context.Background(), sw, snap, c)
		if err != nil {
			t.Fatalf("%d: %v", i, err)
		}
		found := false
		for _, w := range plan.Warnings {
			found = found || w.Code == "mgmt_unknown"
		}
		if !found {
			t.Errorf("%d: no mgmt_unknown warning in %v", i, plan.Warnings)
		}
	}
	plan, err := VOSS().Plan(context.Background(), sw, snap, switchcfg.Change{Ports: []switchcfg.PortChange{
		{IfIndex: 193, Alias: ptr("printer")}}})
	if err != nil || len(plan.Warnings) != 0 {
		t.Errorf("renaming a port needs no acknowledgement: %v %v", plan, err)
	}
}

// steps lists the descriptions of a plan's steps.
func steps(ss []switchcfg.Step) []string {
	var out []string
	for _, s := range ss {
		out = append(out, s.Description)
	}
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestHybridUntaggedOnly is the change recorded on a VSP 8404 (VOSS 8.10.1):
// a port tags, carries one VLAN untagged and is ready for tagged ones.
func TestHybridUntaggedOnly(t *testing.T) {
	plan, _, st := roundTrip(t, switchcfg.Change{Ports: []switchcfg.PortChange{
		{IfIndex: 193, VLANs: &switchcfg.PortVLANs{Untagged: 10, Hybrid: true}},
	}})
	if v, _ := st["rcVlanPortPerformTagging"]["193"].AsInt(); v != tagAll {
		t.Errorf("tagging = %d", v)
	}
	if v, _ := st["rcVlanPortUntagDefaultVlan"]["193"].AsInt(); v != truthTrue {
		t.Errorf("untag default VLAN = %d", v)
	}
	if v, _ := st["rcVlanPortDefaultVlanId"]["193"].AsInt(); v != 10 {
		t.Errorf("PVID = %d", v)
	}
	if got := members(t, st, "10"); len(got) != 2 || got[0] != 193 {
		t.Errorf("VLAN 10 members = %v", got)
	}
	if got := members(t, st, "1"); len(got) != 1 {
		t.Errorf("VLAN 1 members = %v", got)
	}
	want := []string{"enable tagging", "remove VLAN members", "add VLAN members", "set PVIDs", "untag the default VLAN"}
	if got := steps(plan.Steps); !eq(got, want) {
		t.Errorf("steps = %v, want %v", got, want)
	}
	want = []string{"tag the default VLAN", "remove VLAN members", "add VLAN members", "set PVIDs", "disable tagging"}
	if got := steps(plan.Inverse); !eq(got, want) {
		t.Errorf("inverse = %v, want %v", got, want)
	}
	if !plan.SaveKnown || len(plan.Save) != 1 || plan.Save[0].Set[0].Instance() != "rcSysActionL1.0" || plan.Save[0].Set[0].Value.Int != 7 {
		t.Errorf("save = %v %+v", plan.SaveKnown, plan.Save)
	}
}

func TestHybridWithTagged(t *testing.T) {
	_, _, st := roundTrip(t, switchcfg.Change{Ports: []switchcfg.PortChange{
		{IfIndex: 194, VLANs: &switchcfg.PortVLANs{Untagged: 10, Tagged: []int{30, 20}}},
	}})
	if v, _ := st["rcVlanPortUntagDefaultVlan"]["194"].AsInt(); v != truthTrue {
		t.Errorf("untag default VLAN = %d", v)
	}
	for _, vid := range []string{"10", "20", "30"} {
		found := false
		for _, i := range members(t, st, vid) {
			found = found || i == 194
		}
		if !found {
			t.Errorf("1/3 not in VLAN %s", vid)
		}
	}
}

// From hybrid back to access and to trunk: the default VLAN is tagged again
// before the memberships change.
func TestFromHybrid(t *testing.T) {
	for name, to := range map[string]switchcfg.PortVLANs{"access": {Untagged: 1}, "trunk": {Tagged: []int{10, 20}}} {
		sw := switchcfgtest.New()
		snap := sw.VOSS()
		sw.Put("rcVlanPortPerformTagging", "193", switchcfg.Int(tagAll))
		sw.Put("rcVlanPortUntagDefaultVlan", "193", switchcfg.Int(truthTrue))
		plan, err := VOSS().Plan(context.Background(), sw, snap, switchcfg.Change{Ports: []switchcfg.PortChange{{IfIndex: 193, VLANs: &to}}})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		apply(t, sw, plan.Steps)
		verify(t, sw, plan.Expected)
		if v, _ := sw.Get("rcVlanPortUntagDefaultVlan", "193"); v.Int != truthFalse {
			t.Errorf("%s: untag default VLAN = %d", name, v.Int)
		}
		if got := steps(plan.Steps); got[0] != "tag the default VLAN" {
			t.Errorf("%s: steps = %v", name, got)
		}
	}
}

func TestHexTextRefused(t *testing.T) {
	sw := switchcfgtest.New()
	snap := sw.VOSS()
	for _, c := range []switchcfg.Change{
		{Ports: []switchcfg.PortChange{{IfIndex: 193, Alias: ptr("0x41")}}},
		{VLANs: []switchcfg.VLANChange{{Op: switchcfg.VLANCreate, ID: 50, Name: "0Xff"}}},
	} {
		if _, err := VOSS().Plan(context.Background(), sw, snap, c); !switchcfg.IsRefused(err) {
			t.Errorf("%+v: err = %v", c, err)
		}
	}
	// An empty name is allowed (EDM sends it as "0x00:").
	if _, err := VOSS().Plan(context.Background(), sw, snap, switchcfg.Change{Ports: []switchcfg.PortChange{
		{IfIndex: 192, Alias: ptr("")}}}); err != nil && !switchcfg.IsRefused(err) {
		t.Error(err)
	}
}
