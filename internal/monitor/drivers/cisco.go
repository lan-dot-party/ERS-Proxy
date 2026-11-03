package drivers

import (
	"context"
	"sort"
	"strings"

	"github.com/lan-dot-party/ers-proxy/internal/monitor"
)

// Cisco reads VLANs from the CISCO-VTP-MIB and CISCO-VLAN-MEMBERSHIP-MIB.
// Small-business models without VTP fall back to the Q-BRIDGE-MIB.
type Cisco struct{}

// Name implements monitor.Driver.
func (Cisco) Name() string { return "cisco" }

// Match implements monitor.Driver: sysObjectID below enterprise 9.
func (Cisco) Match(sys monitor.System) int {
	if strings.HasPrefix(sys.ObjectID, "1.3.6.1.4.1.9.") {
		return 100
	}
	return 0
}

// Collect implements monitor.Driver.
func (Cisco) Collect(ctx context.Context, r monitor.Reader) (*monitor.Snapshot, error) {
	b, err := monitor.CollectBase(ctx, r, monitor.BaseOptions{})
	if err != nil {
		return nil, err
	}
	b.Snap.Driver = "cisco"
	found, err := ciscoVLANs(ctx, r, b.Snap)
	if err != nil {
		b.Snap.Notes = append(b.Snap.Notes, "VLANs (VTP): "+err.Error())
	}
	if !found {
		if err := QBridge(ctx, r, b); err != nil {
			b.Snap.Notes = append(b.Snap.Notes, "VLANs: "+err.Error())
		}
	}
	return b.Snap, nil
}

// ciscoReserved reports the VLANs 1002-1005 (FDDI/Token Ring defaults).
func ciscoReserved(id int) bool { return id >= 1002 && id <= 1005 }

// ciscoTrunkVLANs decodes the four VlansEnabled bitmaps (VLANs 0-1023,
// 1024-2047, 2048-3071, 3072-4095; bit position = VLAN ID, MSB first).
func ciscoTrunkVLANs(maps [4][]byte) []int {
	var out []int
	for i, m := range maps {
		for _, bit := range monitor.BitsMSB(m) {
			if id := bit + i*1024; id > 0 {
				out = append(out, id)
			}
		}
	}
	sort.Ints(out)
	return out
}

// ciscoVLANs fills VLANs, PVID and membership from VTP; it reports whether
// VTP returned any VLAN.
func ciscoVLANs(ctx context.Context, r monitor.Reader, snap *monitor.Snapshot) (bool, error) {
	vtp, err := r.Columns(ctx, "vtpVlanState", "vtpVlanName")
	if err != nil {
		return false, err
	}
	exists := map[int]bool{}
	for idx, row := range vtp {
		parts := monitor.Ints(idx) // managementDomainIndex.vlanId
		if len(parts) != 2 || ciscoReserved(parts[1]) {
			continue
		}
		if st, _ := row["vtpVlanState"].AsInt(); st != 1 {
			continue
		}
		if !exists[parts[1]] {
			exists[parts[1]] = true
			snap.VLANs = append(snap.VLANs, monitor.VLAN{ID: parts[1], Name: row["vtpVlanName"].String()})
		}
	}
	if len(exists) == 0 {
		return false, nil
	}
	sort.Slice(snap.VLANs, func(i, j int) bool { return snap.VLANs[i].ID < snap.VLANs[j].ID })

	if t, err := r.Columns(ctx, "vmVlan"); err == nil {
		for idx, row := range t {
			parts := monitor.Ints(idx)
			vlan, ok := row["vmVlan"].AsInt()
			if len(parts) != 1 || !ok || vlan <= 0 {
				continue
			}
			if p := snap.Port(parts[0]); p != nil {
				p.PVID = vlan
				p.Untagged = []int{vlan}
			}
		}
	} else {
		snap.Notes = append(snap.Notes, "access VLANs: "+err.Error())
	}

	trunks, err := r.Columns(ctx, "vlanTrunkPortDynamicStatus", "vlanTrunkPortNativeVlan",
		"vlanTrunkPortVlansEnabled", "vlanTrunkPortVlansEnabled2k", "vlanTrunkPortVlansEnabled3k", "vlanTrunkPortVlansEnabled4k")
	if err != nil {
		snap.Notes = append(snap.Notes, "trunks: "+err.Error())
	}
	for idx, row := range trunks {
		parts := monitor.Ints(idx)
		if len(parts) != 1 {
			continue
		}
		if st, _ := row["vlanTrunkPortDynamicStatus"].AsInt(); st != 1 {
			continue
		}
		p := snap.Port(parts[0])
		if p == nil {
			continue
		}
		native, _ := row["vlanTrunkPortNativeVlan"].AsInt()
		p.PVID = native
		p.Untagged = []int{native}
		p.Tagged = nil
		for _, id := range ciscoTrunkVLANs([4][]byte{
			row["vlanTrunkPortVlansEnabled"].Bytes, row["vlanTrunkPortVlansEnabled2k"].Bytes,
			row["vlanTrunkPortVlansEnabled3k"].Bytes, row["vlanTrunkPortVlansEnabled4k"].Bytes,
		}) {
			if id != native && exists[id] {
				p.Tagged = append(p.Tagged, id)
			}
		}
	}
	for _, p := range snap.Ports {
		sort.Ints(p.Untagged)
		sort.Ints(p.Tagged)
		monitor.SetMode(p)
	}
	return true, nil
}
