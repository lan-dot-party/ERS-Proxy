package drivers

import (
	"context"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/lan-dot-party/ers-proxy/internal/monitor"
)

// TPLink covers TP-Link JetStream and Omada switches (verified on a T1500G-8T
// and an SG2210P). Facts from the hardware:
//   - every interface reports ifType 6, also "Vlan-interface100" and
//     "port-channel 1"; front-panel ports are recognised by their ifName
//     ("gigabitEthernet 1/0/5" = unit 1, port 5);
//   - there is no Q-BRIDGE-MIB; VLANs come from TPLINK-DOT1Q-VLAN-MIB, whose
//     VLAN table lists member ports as text ("1/0/1-3,1/0/6,LAG1");
//   - LAG membership is in the same MIB's port table ("LAG1"), not in
//     IEEE8023-LAG-MIB;
//   - PoE models (SG2210P) do not answer POWER-ETHERNET-MIB; PoE comes from
//     TPLINK-POWER-OVER-ETHERNET-MIB (power in 0.1 W, port index = number).
//
// Q-BRIDGE is still read as a fallback for models that implement it.
type TPLink struct{}

// Name implements monitor.Driver.
func (TPLink) Name() string { return "tplink" }

// Match implements monitor.Driver: sysObjectID below enterprise 11863.
func (TPLink) Match(sys monitor.System) int {
	if strings.HasPrefix(sys.ObjectID, "1.3.6.1.4.1.11863.") {
		return 100
	}
	return 0
}

// tpPort matches the ifName of a front-panel port: "gigabitEthernet 1/0/5",
// "ten-gigabitEthernet 1/0/10", "fastEthernet 1/0/3".
var tpPort = regexp.MustCompile(`(?i)ethernet\s*(\d+)/(\d+)/(\d+)$`)

// tpLAG matches the ifName of an aggregate: "port-channel 1".
var tpLAG = regexp.MustCompile(`(?i)^port-channel\s*(\d+)$`)

// Collect implements monitor.Driver.
func (TPLink) Collect(ctx context.Context, r monitor.Reader) (*monitor.Snapshot, error) {
	b, err := monitor.CollectBase(ctx, r, monitor.BaseOptions{SkipPoE: true})
	if err != nil {
		return nil, err
	}
	snap := b.Snap
	snap.Driver = "tplink"
	tpLocate(snap)
	if !tpPoE(ctx, r, snap) {
		b.ReadPoE(ctx, r, monitor.BaseOptions{})
	}
	if !tpVLANs(ctx, r, snap) {
		if err := QBridge(ctx, r, b); err != nil {
			snap.Notes = append(snap.Notes, "VLANs: "+err.Error())
		}
	}
	return snap, nil
}

// tpLocate numbers the front-panel ports from their names and marks VLAN
// interfaces and port channels as non-physical.
func tpLocate(snap *monitor.Snapshot) {
	for _, p := range snap.Ports {
		name := strings.TrimSpace(p.Name)
		if m := tpPort.FindStringSubmatch(name); m != nil {
			p.Physical = true
			p.Unit, _ = strconv.Atoi(m[1])
			p.Number, _ = strconv.Atoi(m[3])
			continue
		}
		if tpLAG.MatchString(name) {
			p.IsLAG = true
		}
		if p.Physical && (p.IsLAG || strings.HasPrefix(strings.ToLower(name), "vlan")) {
			p.Physical, p.Unit, p.Number = false, 0, 0
		}
	}
	snap.SortPorts()
}

// tpVLANs reads TPLINK-DOT1Q-VLAN-MIB. It reports false if the device does not
// have it.
func tpVLANs(ctx context.Context, r monitor.Reader, snap *monitor.Snapshot) bool {
	ports, err := r.Columns(ctx, "tpVlanPortName", "tpVlanPortPvid", "tpVlanPortLag")
	if err != nil || len(ports) == 0 {
		return false
	}
	byName := map[string]*monitor.Port{}
	lagByName := map[string]*monitor.Port{}
	for idx, row := range ports {
		id := monitor.Ints(idx)
		if len(id) != 1 {
			continue
		}
		p := snap.Port(id[0])
		if p == nil {
			continue
		}
		name := strings.TrimSpace(row["tpVlanPortName"].String())
		byName[strings.ToUpper(name)] = p
		if strings.HasPrefix(strings.ToUpper(name), "LAG") {
			lagByName[strings.ToUpper(name)] = p
			p.IsLAG, p.Physical = true, false
		}
		p.PVID, _ = row["tpVlanPortPvid"].AsInt()
	}
	for idx, row := range ports {
		lag := strings.ToUpper(strings.TrimSpace(row["tpVlanPortLag"].String()))
		id := monitor.Ints(idx)
		if lag == "" || lag == "N/A" || len(id) != 1 {
			continue
		}
		if p := snap.Port(id[0]); p != nil && !p.IsLAG {
			p.LAGName = lag
			if agg := lagByName[lag]; agg != nil {
				p.LAG = agg.IfIndex
			}
		}
	}
	vlans, err := r.Columns(ctx, "tpVlanName", "tpVlanTagPorts", "tpVlanUntagPorts")
	if err != nil {
		return false
	}
	for idx, row := range vlans {
		id := monitor.Ints(idx)
		if len(id) != 1 {
			continue
		}
		vid := id[0]
		name := row["tpVlanName"].String()
		if name == "N/A" {
			name = ""
		}
		snap.VLANs = append(snap.VLANs, monitor.VLAN{ID: vid, Name: name})
		for _, p := range tpPortList(row["tpVlanUntagPorts"].String(), byName) {
			p.Untagged = append(p.Untagged, vid)
		}
		for _, p := range tpPortList(row["tpVlanTagPorts"].String(), byName) {
			p.Tagged = append(p.Tagged, vid)
		}
	}
	sort.Slice(snap.VLANs, func(i, j int) bool { return snap.VLANs[i].ID < snap.VLANs[j].ID })
	// Members of a LAG inherit its VLANs (the switch lists only "LAG1").
	for _, p := range snap.Ports {
		if agg := lagByName[strings.ToUpper(p.LAGName)]; agg != nil && p != agg && len(p.Untagged)+len(p.Tagged) == 0 {
			p.Untagged = append([]int(nil), agg.Untagged...)
			p.Tagged = append([]int(nil), agg.Tagged...)
		}
	}
	for _, p := range snap.Ports {
		sort.Ints(p.Untagged)
		sort.Ints(p.Tagged)
		monitor.SetMode(p)
	}
	return true
}

// tpPortList resolves a TP-Link port list ("1/0/1-3,1/0/6,LAG1") to ports.
func tpPortList(s string, byName map[string]*monitor.Port) []*monitor.Port {
	var out []*monitor.Port
	seen := map[*monitor.Port]bool{}
	add := func(name string) {
		if p := byName[strings.ToUpper(name)]; p != nil && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		slash := strings.LastIndex(item, "/")
		from, to, isRange := strings.Cut(item[slash+1:], "-")
		if !isRange {
			add(item)
			continue
		}
		a, err1 := strconv.Atoi(from)
		z, err2 := strconv.Atoi(to)
		if err1 != nil || err2 != nil || z < a || z-a > 1024 {
			continue
		}
		for n := a; n <= z; n++ {
			add(item[:slash+1] + strconv.Itoa(n))
		}
	}
	return out
}

// tpPoE reads TPLINK-POWER-OVER-ETHERNET-MIB and reports whether the device
// has it. Port status: 0 off, 1 turning
// on, 2 on, higher values are faults (overload, short, …).
func tpPoE(ctx context.Context, r monitor.Reader, snap *monitor.Snapshot) bool {
	t, err := r.Columns(ctx, "tpPoePortStatus", "tpPoePortPower", "tpPoePortClass", "tpPoePortPowerStatus")
	if err != nil || len(t) == 0 {
		return false
	}
	for idx, row := range t {
		n := monitor.Ints(idx)
		if len(n) != 1 {
			continue
		}
		var p *monitor.Port
		for _, q := range snap.Ports {
			if q.Physical && q.Unit == 1 && q.Number == n[0] {
				p = q
			}
		}
		if p == nil {
			continue
		}
		enabled, _ := row["tpPoePortStatus"].AsInt()
		st, _ := row["tpPoePortPowerStatus"].AsInt()
		poe := &monitor.PoE{}
		switch {
		case enabled != 1:
			poe.Status = "disabled"
		case st == 2:
			poe.Status = "delivering"
		case st == 0 || st == 1:
			poe.Status = "searching"
		default:
			poe.Status = "fault"
		}
		if poe.Status == "delivering" {
			if c, ok := row["tpPoePortClass"].AsInt(); ok && c >= 0 && c <= 8 {
				poe.Class = c
			}
			if w, ok := row["tpPoePortPower"].AsInt(); ok && w > 0 {
				poe.PowerMW = w * 100
			}
		}
		p.PoE = poe
	}
	if m, err := r.Scalars(ctx, "tpPoeSystemPowerLimit", "tpPoeSystemPowerConsumption"); err == nil {
		limit, ok1 := m["tpPoeSystemPowerLimit"].AsInt()
		used, ok2 := m["tpPoeSystemPowerConsumption"].AsInt()
		if ok1 && ok2 && limit > 0 {
			snap.PoE = &monitor.PoEBudget{TotalW: (limit + 5) / 10, UsedW: (used + 5) / 10}
		}
	}
	return true
}
