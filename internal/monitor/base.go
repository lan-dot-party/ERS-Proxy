package monitor

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// IANA ifType values.
const (
	ifTypeEthernet     = 6
	ifTypeFastEther    = 62
	ifTypeFastEtherFX  = 69
	ifTypeGigabit      = 117
	ifTypeLAG          = 161
	ifTypePropVirtual  = 53
	ifTypeL2VLAN       = 135
	ifTypeSoftwareLoop = 24
)

// IsPhysical reports whether an ifType is an Ethernet front-panel port.
func IsPhysical(ifType int) bool {
	switch ifType {
	case ifTypeEthernet, ifTypeFastEther, ifTypeFastEtherFX, ifTypeGigabit:
		return true
	}
	return false
}

var ifStatus = map[int]string{1: "up", 2: "down", 3: "testing", 4: "unknown", 5: "dormant", 6: "notPresent", 7: "lowerLayerDown"}

var poeStatus = map[int]string{1: "disabled", 2: "searching", 3: "delivering", 4: "fault", 5: "test", 6: "other"}

// BaseOptions adapt the common collection to a vendor.
type BaseOptions struct {
	// Locate returns unit and front-panel number of a physical port. Nil:
	// unit 1, number = bridge port number (or position among physical ports).
	Locate func(p *Port) (unit, number int)
	// PoEIfIndex maps a pethPsePortTable index (group, port) to an ifIndex.
	// Nil: the port with Unit == group and Number == port.
	PoEIfIndex func(group, port int) int
	// SkipPoE leaves out POWER-ETHERNET-MIB; the driver reads PoE itself and
	// calls ReadPoE as a fallback (slow agents answer missing tables late).
	SkipPoE bool
}

// Base is the result of CollectBase plus intermediate data drivers reuse.
type Base struct {
	Snap *Snapshot
	// BridgePorts maps dot1dBasePort numbers to ifIndex.
	BridgePorts map[int]int
}

var ifColumns = []string{
	"ifDescr", "ifType", "ifSpeed", "ifAdminStatus", "ifOperStatus", "ifLastChange",
	"ifInOctets", "ifOutOctets", "ifInErrors", "ifOutErrors", "ifInDiscards", "ifOutDiscards",
	"ifName", "ifHCInOctets", "ifHCOutOctets", "ifHighSpeed", "ifAlias", "ifCounterDiscontinuityTime",
}

// CollectBase reads everything that standard MIBs provide: system group,
// interfaces with counters, duplex, link aggregation, LLDP neighbours and PoE.
func CollectBase(ctx context.Context, r Reader, opts BaseOptions) (*Base, error) {
	sys, err := ReadSystem(ctx, r)
	if err != nil {
		return nil, err
	}
	snap := &Snapshot{Time: time.Now(), System: sys}
	ifs, err := r.Columns(ctx, ifColumns...)
	if err != nil {
		return nil, fmt.Errorf("read interfaces: %w", err)
	}
	for idx, row := range ifs {
		ifIndex, err := strconv.Atoi(idx)
		if err != nil {
			continue
		}
		p := &Port{IfIndex: ifIndex, Descr: row["ifDescr"].String(), Name: row["ifName"].String(), Alias: row["ifAlias"].String()}
		p.Type, _ = row["ifType"].AsInt()
		p.Physical = IsPhysical(p.Type)
		p.IsLAG = p.Type == ifTypeLAG
		if v, ok := row["ifAdminStatus"].AsInt(); ok {
			p.Admin = ifStatus[v]
		}
		if v, ok := row["ifOperStatus"].AsInt(); ok {
			p.Oper = ifStatus[v]
		}
		if v, ok := row["ifHighSpeed"].AsUint(); ok && v > 0 {
			p.Speed = v * 1_000_000
		} else if v, ok := row["ifSpeed"].AsUint(); ok && v < 4_294_967_295 {
			p.Speed = v
		}
		if p.Oper != "up" {
			p.Speed = 0 // many agents report the nominal speed of a port without link
		}
		if v, ok := row["ifLastChange"].AsUint(); ok {
			p.LastChange = Ticks(v)
		}
		if in, ok := row["ifHCInOctets"].AsUint(); ok {
			out, _ := row["ifHCOutOctets"].AsUint()
			p.InOctets, p.OutOctets, p.HasHC = in, out, true
		} else {
			p.InOctets, _ = row["ifInOctets"].AsUint()
			p.OutOctets, _ = row["ifOutOctets"].AsUint()
		}
		p.InErrors, _ = row["ifInErrors"].AsUint()
		p.OutErrors, _ = row["ifOutErrors"].AsUint()
		p.InDiscards, _ = row["ifInDiscards"].AsUint()
		p.OutDiscards, _ = row["ifOutDiscards"].AsUint()
		p.Discontinuity, _ = row["ifCounterDiscontinuityTime"].AsUint()
		if p.Name == "" {
			p.Name = p.Descr
		}
		snap.Ports = append(snap.Ports, p)
	}
	if len(snap.Ports) == 0 {
		return nil, fmt.Errorf("the device returned no interfaces")
	}
	sort.Slice(snap.Ports, func(i, j int) bool { return snap.Ports[i].IfIndex < snap.Ports[j].IfIndex })
	b := &Base{Snap: snap, BridgePorts: map[int]int{}}

	if t, err := r.Columns(ctx, "dot1dBasePortIfIndex"); err == nil {
		for idx, row := range t {
			bp, err1 := strconv.Atoi(idx)
			ifIndex, ok := row["dot1dBasePortIfIndex"].AsInt()
			if err1 == nil && ok {
				b.BridgePorts[bp] = ifIndex
			}
		}
	}
	b.locate(opts)

	if t, err := r.Columns(ctx, "dot3StatsDuplexStatus"); err == nil {
		for idx, row := range t {
			if p := snap.Port(atoi(idx)); p != nil {
				switch v, _ := row["dot3StatsDuplexStatus"].AsInt(); v {
				case 2:
					p.Duplex = "half"
				case 3:
					p.Duplex = "full"
				}
			}
		}
	}
	if t, err := r.Columns(ctx, "dot3adAggPortAttachedAggID"); err == nil {
		for idx, row := range t {
			p := snap.Port(atoi(idx))
			agg, ok := row["dot3adAggPortAttachedAggID"].AsInt()
			if p == nil || !ok || agg == 0 || agg == p.IfIndex {
				continue
			}
			p.LAG = agg
			if a := snap.Port(agg); a != nil {
				p.LAGName = a.Name
			}
		}
	}
	b.lldp(ctx, r)
	if !opts.SkipPoE {
		b.poe(ctx, r, opts)
	}
	snap.SortPorts()
	return b, nil
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

func (b *Base) locate(opts BaseOptions) {
	if opts.Locate != nil {
		for _, p := range b.Snap.Ports {
			if p.Physical {
				p.Unit, p.Number = opts.Locate(p)
			}
		}
		return
	}
	byIf := map[int]int{}
	for bp, ifIndex := range b.BridgePorts {
		byIf[ifIndex] = bp
	}
	n := 0
	for _, p := range b.Snap.Ports { // sorted by ifIndex
		if !p.Physical {
			continue
		}
		n++
		p.Unit, p.Number = 1, n
		if bp, ok := byIf[p.IfIndex]; ok {
			p.Number = bp
		}
	}
}

// lldp fills the neighbours. lldpRem* is indexed by timeMark.localPortNum.index;
// the local port number is matched to an interface by lldpLocPortId or
// lldpLocPortDesc, else via the bridge port map, else taken as ifIndex.
func (b *Base) lldp(ctx context.Context, r Reader) {
	rem, err := r.Columns(ctx, "lldpRemSysName", "lldpRemPortId", "lldpRemPortDesc", "lldpRemSysCapEnabled")
	if err != nil || len(rem) == 0 {
		return
	}
	byName := map[string]int{}
	for _, p := range b.Snap.Ports {
		for _, n := range []string{p.Name, p.Descr, p.Alias} {
			if n = strings.ToLower(strings.TrimSpace(n)); n != "" {
				if _, dup := byName[n]; !dup {
					byName[n] = p.IfIndex
				}
			}
		}
	}
	local := map[int]int{}
	if loc, err := r.Columns(ctx, "lldpLocPortId", "lldpLocPortDesc"); err == nil {
		for idx, row := range loc {
			num := atoi(idx)
			for _, col := range []string{"lldpLocPortId", "lldpLocPortDesc"} {
				if ifIndex, ok := byName[strings.ToLower(strings.TrimSpace(row[col].String()))]; ok {
					local[num] = ifIndex
					break
				}
			}
		}
	}
	for idx, row := range rem {
		parts := Ints(idx)
		if len(parts) != 3 {
			continue
		}
		num := parts[1]
		ifIndex, ok := local[num]
		if !ok {
			if bpIf, ok2 := b.BridgePorts[num]; ok2 {
				ifIndex = bpIf
			} else {
				ifIndex = num
			}
		}
		p := b.Snap.Port(ifIndex)
		if p == nil {
			continue
		}
		n := &Neighbor{SysName: row["lldpRemSysName"].String(), PortID: printableID(row["lldpRemPortId"]),
			PortDesc: row["lldpRemPortDesc"].String()}
		if c := row["lldpRemSysCapEnabled"].Bytes; len(c) > 0 && c[0]&0x20 != 0 { // bit 2 = bridge
			n.Bridge = true
		}
		if p.Neighbor == nil || (n.Bridge && !p.Neighbor.Bridge) {
			p.Neighbor = n
		}
	}
}

// printableID shows MAC-like binary IDs as hex, text as text.
func printableID(v Value) string {
	if v.Kind != KindBytes {
		return v.String()
	}
	for _, c := range v.Bytes {
		if c < 0x20 || c > 0x7e {
			parts := make([]string, len(v.Bytes))
			for i, x := range v.Bytes {
				parts[i] = fmt.Sprintf("%02x", x)
			}
			return strings.Join(parts, ":")
		}
	}
	return string(v.Bytes)
}

// ReadPoE reads POWER-ETHERNET-MIB (for drivers that set SkipPoE).
func (b *Base) ReadPoE(ctx context.Context, r Reader, opts BaseOptions) { b.poe(ctx, r, opts) }

func (b *Base) poe(ctx context.Context, r Reader, opts BaseOptions) {
	t, err := r.Columns(ctx, "pethPsePortDetectionStatus", "pethPsePortPowerClassifications", "pethPsePortAdminEnable")
	if err != nil || len(t) == 0 {
		return
	}
	find := opts.PoEIfIndex
	if find == nil {
		find = func(group, port int) int {
			for _, p := range b.Snap.Ports {
				if p.Physical && p.Unit == group && p.Number == port {
					return p.IfIndex
				}
			}
			return 0
		}
	}
	for idx, row := range t {
		parts := Ints(idx)
		if len(parts) != 2 {
			continue
		}
		p := b.Snap.Port(find(parts[0], parts[1]))
		if p == nil {
			continue
		}
		st, _ := row["pethPsePortDetectionStatus"].AsInt()
		poe := &PoE{Status: poeStatus[st]}
		if c, ok := row["pethPsePortPowerClassifications"].AsInt(); ok && c >= 1 {
			poe.Class = c - 1 // class0(1) … class4(5)
		}
		if a, ok := row["pethPsePortAdminEnable"].AsInt(); ok && a == 2 {
			poe.Status = "disabled"
		}
		p.PoE = poe
	}
	if m, err := r.Columns(ctx, "pethMainPsePower", "pethMainPseConsumptionPower"); err == nil && len(m) > 0 {
		bud := &PoEBudget{}
		for _, row := range m {
			tw, _ := row["pethMainPsePower"].AsInt()
			uw, _ := row["pethMainPseConsumptionPower"].AsInt()
			bud.TotalW += tw
			bud.UsedW += uw
		}
		b.Snap.PoE = bud
	}
}

// SetMode derives access/trunk from the VLAN lists.
func SetMode(p *Port) {
	switch {
	case len(p.Tagged) > 0:
		p.Mode = "trunk"
	case len(p.Untagged) > 0 || p.PVID > 0:
		p.Mode = "access"
	}
}
