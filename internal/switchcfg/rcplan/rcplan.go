// Package rcplan plans changes for switches with the RAPID-CITY VLAN MIB
// (VSP/VOSS now; ERS later): VLANs in rcVlanTable with a PortSet of members
// (bit i = ifIndex i), PVID and tagging per port in rcVlanPortTable, port
// state and name in IF-MIB, PoE in POWER-ETHERNET-MIB.
//
// VOSS facts, from the EDM of VOSS 8.10 and checked on a VSP 8404 with VOSS
// 8.10.1 (step 8.1):
//   - rcVlanPortPerformTagging is a TruthValue (1 = tag, 2 = untag); a
//     tagging port with rcVlanPortUntagDefaultVlan = 1 sends its PVID
//     untagged (hybrid);
//   - the switch takes the steps in the order transition writes them: create
//     a VLAN with rcVlanRowStatus createAndGo, rcVlanType, rcVlanName and
//     rcVlanStgId in one request; switch tagging on, add members (whole
//     PortSet), set the PVID, then untag the default VLAN; back in reverse;
//   - a port that leaves all its VLANs gets PVID 0, and 0 can be written;
//   - rcSysActionL1.0 = 7 saves the configuration;
//   - VOSS's EDM has no PoE objects to write (POWER-ETHERNET over SNMP only).
package rcplan

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/lan-dot-party/ers-proxy/internal/monitor"
	"github.com/lan-dot-party/ers-proxy/internal/switchcfg"
)

// rcVlanPortPerformTagging values used for writing.
const (
	tagAll   = 1
	untagAll = 2
)

// TruthValue (rcVlanPortUntagDefaultVlan).
const (
	truthTrue  = 1
	truthFalse = 2
)

// VLAN interfaces of VOSS have ifIndex 2048 + VLAN ID.
const vossVLANIfIndex = 2048

// rcVlanType byPort.
const typeByPort = 1

// Planner plans for one RAPID-CITY switch family.
type Planner struct {
	driver string
	caps   switchcfg.Capabilities
	// vlanIfIndex maps a VLAN ID to the ifIndex of its VLAN interface.
	vlanIfIndex func(vid int) int
	// save makes the running configuration permanent (nil = unknown).
	save []switchcfg.Step
}

// saveRuntimeConfig is rcSysActionL1's value for "save configuration".
const saveRuntimeConfig = 7

// VOSS returns the planner for VSP switches with VOSS (driver voss).
func VOSS() *Planner {
	return &Planner{
		driver: "voss",
		caps: switchcfg.Capabilities{
			Admin: true, Alias: true, PoE: true, PortVLANs: true,
			// Tagging with the PVID untagged ("untag-port-default-vlan").
			Hybrid:     true,
			VLANCreate: true, VLANRename: true, VLANDelete: true,
			MaxAlias: 64, MaxVLANName: 64,
		},
		vlanIfIndex: func(vid int) int { return vossVLANIfIndex + vid },
		save: []switchcfg.Step{{Description: "save the configuration", Set: []switchcfg.Assignment{
			{Name: "rcSysActionL1", Value: switchcfg.Int(saveRuntimeConfig)}}}},
	}
}

// Driver implements switchcfg.Planner.
func (p *Planner) Driver() string { return p.driver }

// Capabilities implements switchcfg.Planner.
func (p *Planner) Capabilities() switchcfg.Capabilities { return p.caps }

type vlanRow struct {
	name    string
	members []byte
	typ     int // 0 = unknown
	stg     int // 0 = unknown
}

// state holds the values a plan touches.
type state struct {
	vlans map[int]*vlanRow
	pvid  map[int]int
	tag   map[int]int
	untag map[int]int    // rcVlanPortUntagDefaultVlan: 1 true, 2 false; missing = not readable
	admin map[int]int    // ifAdminStatus: 1 up, 2 down
	alias map[int]string // ifAlias
	poe   map[string]int // pethPsePortAdminEnable by "group.port": 1 on, 2 off
	size  int            // PortSet length in octets
}

func (s *state) clone() *state {
	c := &state{vlans: map[int]*vlanRow{}, pvid: map[int]int{}, tag: map[int]int{}, untag: map[int]int{},
		admin: map[int]int{}, alias: map[int]string{}, poe: map[string]int{}, size: s.size}
	for k, v := range s.vlans {
		r := *v
		r.members = append([]byte(nil), v.members...)
		c.vlans[k] = &r
	}
	for k, v := range s.pvid {
		c.pvid[k] = v
	}
	for k, v := range s.tag {
		c.tag[k] = v
	}
	for k, v := range s.untag {
		c.untag[k] = v
	}
	for k, v := range s.admin {
		c.admin[k] = v
	}
	for k, v := range s.alias {
		c.alias[k] = v
	}
	for k, v := range s.poe {
		c.poe[k] = v
	}
	return c
}

func atoi(idx string) int {
	if p := monitor.Ints(idx); len(p) == 1 {
		return p[0]
	}
	return 0
}

// read loads the current values from the switch.
func (p *Planner) read(ctx context.Context, r monitor.Reader, c switchcfg.Change) (*state, map[int]bool, error) {
	s := &state{vlans: map[int]*vlanRow{}, pvid: map[int]int{}, tag: map[int]int{}, untag: map[int]int{},
		admin: map[int]int{}, alias: map[int]string{}, poe: map[string]int{}}
	vt, err := r.Columns(ctx, "rcVlanName", "rcVlanPortMembers", "rcVlanType", "rcVlanStgId")
	if err != nil {
		return nil, nil, fmt.Errorf("read VLANs: %w", err)
	}
	for idx, row := range vt {
		vid := atoi(idx)
		if vid == 0 {
			continue
		}
		v := &vlanRow{name: row["rcVlanName"].String(), members: row["rcVlanPortMembers"].Bytes}
		v.typ, _ = row["rcVlanType"].AsInt()
		v.stg, _ = row["rcVlanStgId"].AsInt()
		s.vlans[vid] = v
		if len(v.members) > s.size {
			s.size = len(v.members)
		}
	}
	if len(s.vlans) == 0 {
		return nil, nil, fmt.Errorf("the switch returned no VLANs (rcVlanTable)")
	}
	pt, err := r.Columns(ctx, "rcVlanPortDefaultVlanId", "rcVlanPortPerformTagging")
	if err != nil {
		return nil, nil, fmt.Errorf("read port VLAN settings: %w", err)
	}
	for idx, row := range pt {
		i := atoi(idx)
		s.pvid[i], _ = row["rcVlanPortDefaultVlanId"].AsInt()
		s.tag[i], _ = row["rcVlanPortPerformTagging"].AsInt()
	}
	if p.caps.Hybrid {
		// Its own request: a switch that cannot read it still gets the
		// other columns, and hybrid changes are then refused.
		if ut, err := r.Columns(ctx, "rcVlanPortUntagDefaultVlan"); err == nil {
			for idx, row := range ut {
				if v, ok := row["rcVlanPortUntagDefaultVlan"].AsInt(); ok && (v == truthTrue || v == truthFalse) {
					s.untag[atoi(idx)] = v
				}
			}
		}
	}
	var needIf, needPoE bool
	for _, pc := range c.Ports {
		needIf = needIf || pc.Admin != nil || pc.Alias != nil
		needPoE = needPoE || pc.PoE != nil
	}
	if needIf {
		it, err := r.Columns(ctx, "ifAdminStatus", "ifAlias")
		if err != nil {
			return nil, nil, fmt.Errorf("read port state: %w", err)
		}
		for idx, row := range it {
			i := atoi(idx)
			if v, ok := row["ifAdminStatus"].AsInt(); ok {
				s.admin[i] = v
			}
			if v, ok := row["ifAlias"]; ok {
				s.alias[i] = v.String()
			}
		}
	}
	if needPoE {
		et, err := r.Columns(ctx, "pethPsePortAdminEnable")
		if err != nil {
			return nil, nil, fmt.Errorf("read PoE: %w", err)
		}
		for idx, row := range et {
			if v, ok := row["pethPsePortAdminEnable"].AsInt(); ok {
				s.poe[idx] = v
			}
		}
	}
	// Interfaces with an IP address: the VLANs that carry management. A
	// switch that is managed over IP has at least one; none means the table
	// could not be read (EDM does not serve it), and the result is nil.
	var mgmt map[int]bool
	if at, err := r.Columns(ctx, "ipAdEntIfIndex"); err == nil {
		for _, row := range at {
			if i, ok := row["ipAdEntIfIndex"].AsInt(); ok {
				if mgmt == nil {
					mgmt = map[int]bool{}
				}
				mgmt[i] = true
			}
		}
	}
	return s, mgmt, nil
}

// Plan implements switchcfg.Planner.
func (p *Planner) Plan(ctx context.Context, r monitor.Reader, snap *monitor.Snapshot, c switchcfg.Change) (*switchcfg.Plan, error) {
	if snap == nil {
		return nil, switchcfg.Refuse("no_snapshot", "the switch has not been polled yet")
	}
	if c.Empty() {
		return nil, switchcfg.Refuse("empty", "nothing to change")
	}
	before, mgmtIf, err := p.read(ctx, r, c)
	if err != nil {
		return nil, err
	}
	mgmt := func(vid int) bool { return mgmtIf[p.vlanIfIndex(vid)] }
	after := before.clone()
	plan := &switchcfg.Plan{Driver: p.driver}
	warn := func(w switchcfg.Warning) {
		for _, x := range plan.Warnings {
			if x == w {
				return
			}
		}
		plan.Warnings = append(plan.Warnings, w)
	}
	// Without the IP address table the management VLAN is unknown: every
	// change that could cut the switch off needs an acknowledgement instead.
	unknownMgmt := func() {
		if mgmtIf == nil {
			warn(switchcfg.Warning{Code: "mgmt_unknown"})
		}
	}

	// VLANs first: ports may use VLANs created in the same change.
	seen := map[int]bool{}
	deleted := map[int]bool{}
	for _, vc := range c.VLANs {
		if vc.ID < 1 || vc.ID > 4094 {
			return nil, switchcfg.Refuse("vlan_id", "VLAN %d is outside 1-4094", vc.ID)
		}
		if seen[vc.ID] {
			return nil, switchcfg.Refuse("vlan_twice", "VLAN %d is changed twice", vc.ID)
		}
		seen[vc.ID] = true
		cur := after.vlans[vc.ID]
		switch vc.Op {
		case switchcfg.VLANCreate:
			if cur != nil {
				return nil, switchcfg.Refuse("vlan_exists", "VLAN %d exists", vc.ID)
			}
			if err := p.checkName(vc.Name); err != nil {
				return nil, err
			}
			row := &vlanRow{name: vc.Name, typ: typeByPort}
			if def := before.vlans[1]; def != nil {
				row.stg = def.stg // the default spanning tree, like the CLI
			}
			after.vlans[vc.ID] = row
			plan.Summary = append(plan.Summary, fmt.Sprintf("create VLAN %d %q", vc.ID, vc.Name))
		case switchcfg.VLANRename:
			if cur == nil {
				return nil, switchcfg.Refuse("vlan_missing", "VLAN %d does not exist", vc.ID)
			}
			if err := p.checkName(vc.Name); err != nil {
				return nil, err
			}
			cur.name = vc.Name
			plan.Summary = append(plan.Summary, fmt.Sprintf("rename VLAN %d to %q", vc.ID, vc.Name))
		case switchcfg.VLANDelete:
			if cur == nil {
				return nil, switchcfg.Refuse("vlan_missing", "VLAN %d does not exist", vc.ID)
			}
			if vc.ID == 1 {
				return nil, switchcfg.Refuse("vlan_default", "VLAN 1 cannot be deleted")
			}
			if mgmt(vc.ID) {
				return nil, switchcfg.Refuse("vlan_mgmt", "VLAN %d has an IP address (management)", vc.ID)
			}
			if cur.typ == 0 {
				return nil, switchcfg.Refuse("vlan_restore", "VLAN %d: its type could not be read, so it could not be restored", vc.ID)
			}
			if len(monitor.BitsMSB(cur.members)) > 0 {
				warn(switchcfg.Warning{Code: "vlan_members", VLAN: vc.ID})
			}
			deleted[vc.ID] = true
			unknownMgmt()
			plan.Summary = append(plan.Summary, fmt.Sprintf("delete VLAN %d %q", vc.ID, cur.name))
		default:
			return nil, switchcfg.Refuse("vlan_op", "unknown VLAN operation %q", vc.Op)
		}
	}

	ports := map[int]bool{}
	for _, pc := range c.Ports {
		port := snap.Port(pc.IfIndex)
		if port == nil || !port.Physical || port.IsLAG {
			return nil, switchcfg.Refuse("port", "ifIndex %d is not a front-panel port", pc.IfIndex)
		}
		if ports[pc.IfIndex] {
			return nil, switchcfg.Refuse("port_twice", "port %s is changed twice", port.Name)
		}
		ports[pc.IfIndex] = true
		name := portName(port)
		if port.Neighbor != nil {
			warn(switchcfg.Warning{Code: "uplink", IfIndex: pc.IfIndex, Detail: port.Neighbor.SysName})
		}
		if port.LAG != 0 && (pc.VLANs != nil || pc.Admin != nil) {
			warn(switchcfg.Warning{Code: "lag", IfIndex: pc.IfIndex, Detail: port.LAGName})
		}
		inMgmt := func() []int {
			var out []int
			for vid, v := range before.vlans {
				if mgmt(vid) && switchcfg.HasBit(v.members, pc.IfIndex) {
					out = append(out, vid)
				}
			}
			return out
		}
		if pc.Admin != nil {
			if _, ok := before.admin[pc.IfIndex]; !ok {
				return nil, switchcfg.Refuse("port_state", "port %s: ifAdminStatus could not be read", name)
			}
			if *pc.Admin {
				after.admin[pc.IfIndex] = 1
				plan.Summary = append(plan.Summary, fmt.Sprintf("enable port %s", name))
			} else {
				after.admin[pc.IfIndex] = 2
				plan.Summary = append(plan.Summary, fmt.Sprintf("disable port %s", name))
				unknownMgmt()
				for _, vid := range inMgmt() {
					warn(switchcfg.Warning{Code: "mgmt", IfIndex: pc.IfIndex, VLAN: vid})
				}
			}
		}
		if pc.Alias != nil {
			if _, ok := before.alias[pc.IfIndex]; !ok {
				return nil, switchcfg.Refuse("port_state", "port %s: ifAlias could not be read", name)
			}
			if err := checkText(*pc.Alias, p.caps.MaxAlias, "alias"); err != nil {
				return nil, err
			}
			after.alias[pc.IfIndex] = *pc.Alias
			plan.Summary = append(plan.Summary, fmt.Sprintf("name port %s %q", name, *pc.Alias))
		}
		if pc.PoE != nil {
			idx := fmt.Sprintf("%d.%d", port.Unit, port.Number)
			if _, ok := before.poe[idx]; !ok {
				return nil, switchcfg.Refuse("no_poe", "port %s has no PoE", name)
			}
			if *pc.PoE {
				after.poe[idx] = 1
				plan.Summary = append(plan.Summary, fmt.Sprintf("enable PoE on port %s", name))
			} else {
				after.poe[idx] = 2
				plan.Summary = append(plan.Summary, fmt.Sprintf("disable PoE on port %s", name))
			}
		}
		if pc.VLANs != nil {
			if _, ok := before.tag[pc.IfIndex]; !ok {
				return nil, switchcfg.Refuse("port_state", "port %s: its VLAN settings could not be read", name)
			}
			want, err := p.portVLANs(after, deleted, *pc.VLANs)
			if err != nil {
				return nil, err
			}
			if _, ok := before.untag[pc.IfIndex]; want.untag == truthTrue && !ok {
				return nil, switchcfg.Refuse("hybrid", "port %s: rcVlanPortUntagDefaultVlan could not be read", name)
			}
			if _, ok := before.untag[pc.IfIndex]; ok {
				after.untag[pc.IfIndex] = want.untag
			}
			for vid, v := range after.vlans {
				v.members = switchcfg.SetBit(v.members, pc.IfIndex, want.member[vid], after.size)
			}
			after.pvid[pc.IfIndex] = want.pvid
			after.tag[pc.IfIndex] = want.tag
			for _, vid := range inMgmt() {
				if !want.member[vid] {
					warn(switchcfg.Warning{Code: "mgmt", IfIndex: pc.IfIndex, VLAN: vid})
				}
			}
			unknownMgmt()
			plan.Summary = append(plan.Summary, fmt.Sprintf("port %s: %s", name, describeVLANs(*pc.VLANs)))
		}
	}

	// Deleted VLANs lose their members first; ports that still use one as
	// PVID would be left pointing at nothing.
	for vid := range deleted {
		for i, pv := range after.pvid {
			if pv == vid {
				return nil, switchcfg.Refuse("vlan_pvid", "VLAN %d is the PVID of ifIndex %d; change that port first", vid, i)
			}
		}
		delete(after.vlans, vid)
	}

	plan.Steps = transition(before, after)
	plan.Inverse = transition(after, before)
	if len(plan.Steps) == 0 {
		return nil, switchcfg.Refuse("no_change", "the switch already has these settings")
	}
	plan.Expected = expected(after, before)
	plan.Before = expected(before, after)
	plan.Save, plan.SaveKnown = p.save, p.save != nil
	return plan, nil
}

type portWant struct {
	member map[int]bool
	pvid   int
	tag    int
	untag  int // rcVlanPortUntagDefaultVlan
}

func (p *Planner) portVLANs(after *state, deleted map[int]bool, v switchcfg.PortVLANs) (portWant, error) {
	w := portWant{member: map[int]bool{}, untag: truthFalse}
	exists := func(vid int) bool { return after.vlans[vid] != nil && !deleted[vid] }
	if v.Untagged == 0 && len(v.Tagged) == 0 {
		return w, switchcfg.Refuse("no_vlan", "a port needs at least one VLAN")
	}
	hybrid := v.Untagged != 0 && (v.Hybrid || len(v.Tagged) > 0)
	if v.Hybrid && v.Untagged == 0 {
		return w, switchcfg.Refuse("no_untagged", "a hybrid port needs an untagged VLAN")
	}
	if hybrid && !p.caps.Hybrid {
		return w, switchcfg.Refuse("hybrid", "an untagged VLAN together with tagged VLANs is not supported")
	}
	if v.Untagged != 0 {
		if !exists(v.Untagged) {
			return w, switchcfg.Refuse("vlan_missing", "VLAN %d does not exist", v.Untagged)
		}
		w.member[v.Untagged] = true
	}
	for _, vid := range v.Tagged {
		if w.member[vid] && vid != v.Untagged {
			return w, switchcfg.Refuse("vlan_dup", "VLAN %d is listed twice", vid)
		}
		if vid == v.Untagged {
			return w, switchcfg.Refuse("vlan_both", "VLAN %d is both untagged and tagged", vid)
		}
		if !exists(vid) {
			return w, switchcfg.Refuse("vlan_missing", "VLAN %d does not exist", vid)
		}
		w.member[vid] = true
	}
	switch {
	case hybrid:
		w.tag, w.pvid, w.untag = tagAll, v.Untagged, truthTrue
	case len(v.Tagged) == 0:
		w.tag, w.pvid = untagAll, v.Untagged
	default:
		w.tag = tagAll
		w.pvid = v.Untagged
		if w.pvid == 0 {
			// Untagged frames go to the PVID; keep the lowest tagged VLAN.
			t := append([]int(nil), v.Tagged...)
			sort.Ints(t)
			w.pvid = t[0]
		}
	}
	return w, nil
}

func (p *Planner) checkName(name string) error {
	if strings.TrimSpace(name) == "" {
		return switchcfg.Refuse("vlan_name", "a VLAN needs a name")
	}
	return checkText(name, p.caps.MaxVLANName, "vlan_name")
}

func checkText(s string, max int, code string) error {
	if len(s) > max {
		return switchcfg.Refuse(code, "at most %d characters", max)
	}
	// EDM's agent decodes text that starts with "0x" as hex octets.
	if strings.HasPrefix(strings.ToLower(s), "0x") {
		return switchcfg.Refuse(code, "must not start with 0x")
	}
	for _, c := range s {
		if c < 0x20 || c > 0x7e {
			return switchcfg.Refuse(code, "only printable ASCII characters")
		}
	}
	return nil
}

func portName(p *monitor.Port) string {
	if p.Name != "" {
		return p.Name
	}
	return "ifIndex " + strconv.Itoa(p.IfIndex)
}

func describeVLANs(v switchcfg.PortVLANs) string {
	var parts []string
	if v.Untagged != 0 {
		parts = append(parts, fmt.Sprintf("untagged %d", v.Untagged))
	}
	if v.Hybrid && len(v.Tagged) == 0 {
		parts = append(parts, "tagging on")
	}
	if len(v.Tagged) > 0 {
		t := append([]int(nil), v.Tagged...)
		sort.Ints(t)
		s := make([]string, len(t))
		for i, x := range t {
			s[i] = strconv.Itoa(x)
		}
		parts = append(parts, "tagged "+strings.Join(s, ","))
	}
	return strings.Join(parts, ", ")
}

func idx(i int) string { return strconv.Itoa(i) }

func sortedKeys[V any](m map[int]V) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

// transition returns the steps that turn from into to. The order keeps every
// intermediate state valid: create VLANs, rename, switch ports to tagging,
// remove members, add members, set PVIDs, switch ports to untagged, delete
// VLANs, then port state, name and PoE. The same function computes the
// inverse with from and to swapped.
func transition(from, to *state) []switchcfg.Step {
	var steps []switchcfg.Step
	add := func(desc string, as []switchcfg.Assignment) {
		if len(as) > 0 {
			steps = append(steps, switchcfg.Step{Description: desc, Set: as})
		}
	}
	// Create VLANs, each in one request (RowStatus with its columns).
	for _, vid := range sortedKeys(to.vlans) {
		if from.vlans[vid] != nil {
			continue
		}
		v := to.vlans[vid]
		as := []switchcfg.Assignment{
			{Name: "rcVlanRowStatus", Index: idx(vid), Value: switchcfg.Int(switchcfg.RowCreateAndGo)},
			{Name: "rcVlanType", Index: idx(vid), Value: switchcfg.Int(v.typ)},
			{Name: "rcVlanName", Index: idx(vid), Value: switchcfg.Text(v.name)},
		}
		if v.stg > 0 {
			as = append(as, switchcfg.Assignment{Name: "rcVlanStgId", Index: idx(vid), Value: switchcfg.Int(v.stg)})
		}
		add(fmt.Sprintf("create VLAN %d", vid), as)
	}
	var as []switchcfg.Assignment
	for _, vid := range sortedKeys(to.vlans) {
		if f := from.vlans[vid]; f != nil && f.name != to.vlans[vid].name {
			as = append(as, switchcfg.Assignment{Name: "rcVlanName", Index: idx(vid), Value: switchcfg.Text(to.vlans[vid].name)})
		}
	}
	add("rename VLANs", as)

	as = nil
	for _, i := range sortedKeys(to.tag) {
		if to.tag[i] == tagAll && from.tag[i] != tagAll {
			as = append(as, switchcfg.Assignment{Name: "rcVlanPortPerformTagging", Index: idx(i), Value: switchcfg.Int(tagAll)})
		}
	}
	add("enable tagging", as)

	as = nil
	for _, i := range sortedKeys(to.untag) {
		if to.untag[i] == truthFalse && from.untag[i] == truthTrue {
			as = append(as, switchcfg.Assignment{Name: "rcVlanPortUntagDefaultVlan", Index: idx(i), Value: switchcfg.Int(truthFalse)})
		}
	}
	add("tag the default VLAN", as)

	// Remove members (VLANs that disappear lose all of theirs).
	mid := map[int][]byte{}
	as = nil
	for _, vid := range sortedKeys(from.vlans) {
		f := from.vlans[vid]
		var target []byte
		if t := to.vlans[vid]; t != nil {
			target = t.members
		}
		m := append([]byte(nil), f.members...)
		for _, bit := range monitor.BitsMSB(f.members) {
			if !switchcfg.HasBit(target, bit) {
				m = switchcfg.SetBit(m, bit, false, from.size)
			}
		}
		mid[vid] = m
		if !bytes.Equal(m, f.members) {
			as = append(as, switchcfg.Assignment{Name: "rcVlanPortMembers", Index: idx(vid), Value: switchcfg.Octets(m)})
		}
	}
	add("remove VLAN members", as)

	as = nil
	for _, vid := range sortedKeys(to.vlans) {
		t := to.vlans[vid]
		cur, ok := mid[vid]
		if !ok {
			cur = nil // created in this transition: no members yet
		}
		if len(monitor.BitsMSB(t.members)) == 0 && len(monitor.BitsMSB(cur)) == 0 {
			continue
		}
		if !sameBits(cur, t.members) {
			as = append(as, switchcfg.Assignment{Name: "rcVlanPortMembers", Index: idx(vid), Value: switchcfg.Octets(pad(t.members, to.size))})
		}
	}
	add("add VLAN members", as)

	as = nil
	for _, i := range sortedKeys(to.pvid) {
		if to.pvid[i] != from.pvid[i] {
			as = append(as, switchcfg.Assignment{Name: "rcVlanPortDefaultVlanId", Index: idx(i), Value: switchcfg.Int(to.pvid[i])})
		}
	}
	add("set PVIDs", as)

	as = nil
	for _, i := range sortedKeys(to.untag) {
		if to.untag[i] == truthTrue && from.untag[i] != truthTrue {
			as = append(as, switchcfg.Assignment{Name: "rcVlanPortUntagDefaultVlan", Index: idx(i), Value: switchcfg.Int(truthTrue)})
		}
	}
	add("untag the default VLAN", as)

	as = nil
	for _, i := range sortedKeys(to.tag) {
		if to.tag[i] != tagAll && to.tag[i] != from.tag[i] {
			as = append(as, switchcfg.Assignment{Name: "rcVlanPortPerformTagging", Index: idx(i), Value: switchcfg.Int(to.tag[i])})
		}
	}
	add("disable tagging", as)

	for _, vid := range sortedKeys(from.vlans) {
		if to.vlans[vid] == nil {
			add(fmt.Sprintf("delete VLAN %d", vid), []switchcfg.Assignment{
				{Name: "rcVlanRowStatus", Index: idx(vid), Value: switchcfg.Int(switchcfg.RowDestroy)},
			})
		}
	}

	as = nil
	for _, i := range sortedKeys(to.admin) {
		if to.admin[i] != from.admin[i] {
			as = append(as, switchcfg.Assignment{Name: "ifAdminStatus", Index: idx(i), Value: switchcfg.Int(to.admin[i])})
		}
	}
	for _, i := range sortedKeys(to.alias) {
		if to.alias[i] != from.alias[i] {
			as = append(as, switchcfg.Assignment{Name: "ifAlias", Index: idx(i), Value: switchcfg.Text(to.alias[i])})
		}
	}
	add("set port state and name", as)

	as = nil
	keys := make([]string, 0, len(to.poe))
	for k := range to.poe {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if to.poe[k] != from.poe[k] {
			as = append(as, switchcfg.Assignment{Name: "pethPsePortAdminEnable", Index: k, Value: switchcfg.Int(to.poe[k])})
		}
	}
	add("set PoE", as)
	return steps
}

// expected lists the values of s for every instance that differs between s
// and other (deleted VLANs as switchcfg.Absent).
func expected(s, other *state) []switchcfg.Assignment {
	var out []switchcfg.Assignment
	vids := map[int]bool{}
	for vid := range s.vlans {
		vids[vid] = true
	}
	for vid := range other.vlans {
		vids[vid] = true
	}
	for _, vid := range sortedKeys(vids) {
		a, b := s.vlans[vid], other.vlans[vid]
		switch {
		case a == nil:
			out = append(out, switchcfg.Assignment{Name: "rcVlanName", Index: idx(vid), Value: switchcfg.Absent})
		case b == nil || a.name != b.name || !sameBits(a.members, b.members):
			out = append(out,
				switchcfg.Assignment{Name: "rcVlanName", Index: idx(vid), Value: switchcfg.Text(a.name)},
				switchcfg.Assignment{Name: "rcVlanPortMembers", Index: idx(vid), Value: switchcfg.Octets(a.members)})
		}
	}
	for _, i := range sortedKeys(s.pvid) {
		if s.pvid[i] != other.pvid[i] {
			out = append(out, switchcfg.Assignment{Name: "rcVlanPortDefaultVlanId", Index: idx(i), Value: switchcfg.Int(s.pvid[i])})
		}
	}
	for _, i := range sortedKeys(s.tag) {
		if s.tag[i] != other.tag[i] {
			out = append(out, switchcfg.Assignment{Name: "rcVlanPortPerformTagging", Index: idx(i), Value: switchcfg.Int(s.tag[i])})
		}
	}
	for _, i := range sortedKeys(s.untag) {
		if s.untag[i] != other.untag[i] {
			out = append(out, switchcfg.Assignment{Name: "rcVlanPortUntagDefaultVlan", Index: idx(i), Value: switchcfg.Int(s.untag[i])})
		}
	}
	for _, i := range sortedKeys(s.admin) {
		if s.admin[i] != other.admin[i] {
			out = append(out, switchcfg.Assignment{Name: "ifAdminStatus", Index: idx(i), Value: switchcfg.Int(s.admin[i])})
		}
	}
	for _, i := range sortedKeys(s.alias) {
		if s.alias[i] != other.alias[i] {
			out = append(out, switchcfg.Assignment{Name: "ifAlias", Index: idx(i), Value: switchcfg.Text(s.alias[i])})
		}
	}
	keys := make([]string, 0, len(s.poe))
	for k := range s.poe {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if s.poe[k] != other.poe[k] {
			out = append(out, switchcfg.Assignment{Name: "pethPsePortAdminEnable", Index: k, Value: switchcfg.Int(s.poe[k])})
		}
	}
	return out
}

func sameBits(a, b []byte) bool {
	return bytes.Equal(bytes.TrimRight(a, "\x00"), bytes.TrimRight(b, "\x00"))
}

func pad(b []byte, size int) []byte {
	if len(b) >= size {
		return b
	}
	out := make([]byte, size)
	copy(out, b)
	return out
}
