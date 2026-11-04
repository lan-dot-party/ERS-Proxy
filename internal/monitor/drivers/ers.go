package drivers

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/lan-dot-party/ers-proxy/internal/monitor"
)

// ERS covers Avaya/Nortel/Extreme Ethernet Routing Switches (BayStack family).
//
// The ERS 5600 firmware does not implement the Q-BRIDGE VLAN tables; VLANs
// come from the RAPID-CITY MIB (rcVlan*). Facts taken from the EDM code of
// firmware 6.6.3; verified on a single ERS 5632FD except the numbering of
// units 2-8 (see docs/compatibility.md):
//   - ifIndex = (unit-1)*portsPerUnit + port, with 128 ports per unit on
//     ERS 5500/5600 and 64 on the other families; interfaces beyond 8 units
//     (out-of-band port 55001) are no front-panel ports;
//   - PortSet bitmaps (rcVlanPortMembers, rcMltPortMembers): bit i, counted
//     from the most significant bit of the first octet, is ifIndex i;
//   - rcVlanPortPerformTagging: 1 tagAll (trunk), 2 untagAll (access),
//     3 tagPvidOnly, 4 untagPvidOnly.
type ERS struct{}

// Name implements monitor.Driver.
func (ERS) Name() string { return "ers" }

// Match implements monitor.Driver: SynOptics/Nortel/Avaya enterprise 45.
func (ERS) Match(sys monitor.System) int {
	if strings.HasPrefix(sys.ObjectID, "1.3.6.1.4.1.45.") {
		return 100
	}
	if strings.Contains(sys.Descr, "Ethernet Routing Switch") {
		return 90
	}
	return 0
}

// maxStackUnits is the largest ERS stack.
const maxStackUnits = 8

// PortsPerUnit returns the ifIndex block size of a stack unit.
func PortsPerUnit(sys monitor.System) int {
	d := sys.Descr
	if strings.Contains(d, "Ethernet Routing Switch 55") || strings.Contains(d, "Ethernet Routing Switch 56") ||
		strings.Contains(d, "Ethernet Routing Switch 5698") || strings.Contains(d, "Ethernet Routing Switch 5650") ||
		strings.Contains(d, "Ethernet Routing Switch 5632") {
		return 128
	}
	return 64
}

// Collect implements monitor.Driver.
func (ERS) Collect(ctx context.Context, r monitor.Reader) (*monitor.Snapshot, error) {
	sys, err := monitor.ReadSystem(ctx, r)
	if err != nil {
		return nil, err
	}
	ppu := PortsPerUnit(sys)
	b, err := monitor.CollectBase(ctx, r, monitor.BaseOptions{
		Locate: func(p *monitor.Port) (int, int) {
			return (p.IfIndex-1)/ppu + 1, (p.IfIndex-1)%ppu + 1
		},
		PoEIfIndex: func(group, port int) int { return (group-1)*ppu + port },
	})
	if err != nil {
		return nil, err
	}
	snap := b.Snap
	snap.Driver = "ers"
	// Interfaces beyond the largest stack are no front-panel ports, e.g. the
	// out-of-band management port (ifIndex 55001 on an ERS 5632FD).
	for _, p := range snap.Ports {
		if p.Physical && p.IfIndex > maxStackUnits*ppu {
			p.Physical, p.Unit, p.Number = false, 0, 0
		}
	}

	ersUnits(ctx, r, snap)
	if stacked(snap) {
		snap.Notes = append(snap.Notes, fmt.Sprintf("ERS port numbering of stack units 2-8 (%d ifIndexes per unit) follows the EDM code; verified on a single unit only", ppu))
	}
	ersPortExtras(ctx, r, snap)
	if err := ersVLANs(ctx, r, snap); err != nil {
		snap.Notes = append(snap.Notes, "VLANs: "+err.Error())
	}
	ersMLT(ctx, r, snap)
	return snap, nil
}

// ersUnits reads the stack members: s5ChasComTable group 3, index 3.<unit*10>.0.
// On an ERS 5632FD (firmware 6.6.3) s5ChasComDescr is a description
// ("24 SFP plus 8 10Gig XFP non-PoE, 1.5U form factor"), s5ChasComVer the
// model with hardware revision ("5632FD HW:06") and s5ChasComRunningSoftwareVer
// the software ("v6.6.3.015").
func ersUnits(ctx context.Context, r monitor.Reader, snap *monitor.Snapshot) {
	t, err := r.Columns(ctx, "s5ChasComDescr", "s5ChasComVer", "s5ChasComSerNum", "s5ChasComRunningSoftwareVer")
	if err != nil {
		return
	}
	for idx, row := range t {
		parts := monitor.Ints(idx)
		if len(parts) != 3 || parts[0] != 3 || parts[2] != 0 || parts[1]%10 != 0 {
			continue
		}
		model, _, _ := strings.Cut(strings.TrimSpace(row["s5ChasComVer"].String()), " HW:")
		if model == "" || strings.Contains(model, " ") {
			model = strings.TrimSpace(row["s5ChasComDescr"].String())
		}
		snap.Units = append(snap.Units, monitor.Unit{Number: parts[1] / 10, Model: model,
			Firmware: strings.TrimSpace(row["s5ChasComRunningSoftwareVer"].String()),
			Serial:   strings.TrimSpace(row["s5ChasComSerNum"].String())})
	}
	sort.Slice(snap.Units, func(i, j int) bool { return snap.Units[i].Number < snap.Units[j].Number })
}

// ersPortExtras fills speed and duplex from rcPortTable where the standard
// MIBs left them empty.
func ersPortExtras(ctx context.Context, r monitor.Reader, snap *monitor.Snapshot) {
	t, err := r.Columns(ctx, "rcPortOperSpeed", "rcPortOperDuplex")
	if err != nil {
		return
	}
	for idx, row := range t {
		p := snap.Port(atoiIdx(idx))
		if p == nil {
			continue
		}
		if p.Speed == 0 && p.Up() {
			if mbps, ok := row["rcPortOperSpeed"].AsUint(); ok {
				p.Speed = mbps * 1_000_000
			}
		}
		// Ports without link report half duplex; show nothing for them.
		if p.Duplex == "" && p.Up() {
			switch d, _ := row["rcPortOperDuplex"].AsInt(); d {
			case 1:
				p.Duplex = "half"
			case 2:
				p.Duplex = "full"
			}
		}
	}
}

func atoiIdx(idx string) int {
	if p := monitor.Ints(idx); len(p) == 1 {
		return p[0]
	}
	return 0
}

func ersVLANs(ctx context.Context, r monitor.Reader, snap *monitor.Snapshot) error {
	vlans, err := r.Columns(ctx, "rcVlanName", "rcVlanPortMembers")
	if err != nil {
		return err
	}
	ports, err := r.Columns(ctx, "rcVlanPortDefaultVlanId", "rcVlanPortPerformTagging")
	if err != nil {
		return err
	}
	tagging := map[int]int{}
	for idx, row := range ports {
		p := snap.Port(atoiIdx(idx))
		if p == nil {
			continue
		}
		p.PVID, _ = row["rcVlanPortDefaultVlanId"].AsInt()
		tagging[p.IfIndex], _ = row["rcVlanPortPerformTagging"].AsInt()
	}
	for idx, row := range vlans {
		vid := atoiIdx(idx)
		if vid == 0 {
			continue
		}
		snap.VLANs = append(snap.VLANs, monitor.VLAN{ID: vid, Name: row["rcVlanName"].String()})
		for _, ifIndex := range monitor.BitsMSB(row["rcVlanPortMembers"].Bytes) {
			p := snap.Port(ifIndex)
			if p == nil {
				continue
			}
			if ersTagged(tagging[ifIndex], vid, p.PVID) {
				p.Tagged = append(p.Tagged, vid)
			} else {
				p.Untagged = append(p.Untagged, vid)
			}
		}
	}
	sort.Slice(snap.VLANs, func(i, j int) bool { return snap.VLANs[i].ID < snap.VLANs[j].ID })
	for _, p := range snap.Ports {
		sort.Ints(p.Untagged)
		sort.Ints(p.Tagged)
		switch tagging[p.IfIndex] {
		case 1:
			p.Mode = "trunk"
		case 2:
			p.Mode = "access"
		case 3, 4:
			p.Mode = "hybrid"
		default:
			monitor.SetMode(p)
		}
	}
	return nil
}

// ersTagged applies rcVlanPortPerformTagging to one VLAN of a port.
func ersTagged(mode, vid, pvid int) bool {
	switch mode {
	case 1: // tagAll
		return true
	case 3: // tagPvidOnly
		return vid == pvid
	case 4: // untagPvidOnly
		return vid != pvid
	}
	return false // untagAll and unknown
}

// ersMLT marks the members of MultiLink Trunks.
func ersMLT(ctx context.Context, r monitor.Reader, snap *monitor.Snapshot) {
	t, err := r.Columns(ctx, "rcMltName", "rcMltPortMembers")
	if err != nil {
		return
	}
	for idx, row := range t {
		id := atoiIdx(idx)
		name := strings.TrimSpace(row["rcMltName"].String())
		if name == "" {
			name = fmt.Sprintf("MLT %d", id)
		}
		for _, ifIndex := range monitor.BitsMSB(row["rcMltPortMembers"].Bytes) {
			if p := snap.Port(ifIndex); p != nil && p.LAG == 0 {
				p.LAG, p.LAGName = id, name
			}
		}
	}
}

// stacked reports whether a snapshot has front-panel ports beyond unit 1.
func stacked(snap *monitor.Snapshot) bool {
	for _, p := range snap.Ports {
		if p.Physical && p.Unit > 1 {
			return true
		}
	}
	return len(snap.Units) > 1
}
