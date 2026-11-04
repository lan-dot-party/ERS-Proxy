// Package drivers contains the vendor drivers of the port dashboard. Every
// driver builds on monitor.CollectBase (standard MIBs) and adds what the
// vendor keeps elsewhere, mostly VLANs. See docs/drivers.md.
package drivers

import (
	"context"
	"sort"

	"github.com/lan-dot-party/ers-proxy/internal/monitor"
)

// All returns every driver, for monitor.NewRegistry.
func All() []monitor.Driver {
	return []monitor.Driver{Generic{}, ERS{}, Cisco{}, TPLink{}, VOSS{}}
}

// Generic covers switches with the standard Q-BRIDGE-MIB.
type Generic struct{}

// Name implements monitor.Driver.
func (Generic) Name() string { return "generic" }

// Match implements monitor.Driver: the fallback for every device.
func (Generic) Match(monitor.System) int { return 10 }

// Collect implements monitor.Driver.
func (Generic) Collect(ctx context.Context, r monitor.Reader) (*monitor.Snapshot, error) {
	b, err := monitor.CollectBase(ctx, r, monitor.BaseOptions{})
	if err != nil {
		return nil, err
	}
	b.Snap.Driver = "generic"
	if err := QBridge(ctx, r, b); err != nil {
		b.Snap.Notes = append(b.Snap.Notes, "VLANs: "+err.Error())
	}
	return b.Snap, nil
}

// QBridge fills PVID, VLAN membership and VLAN names from the Q-BRIDGE-MIB.
// Port lists there count bridge ports (bit 0 = bridge port 1).
func QBridge(ctx context.Context, r monitor.Reader, b *monitor.Base) error {
	snap := b.Snap
	ifOf := func(bridgePort int) *monitor.Port {
		if ifIndex, ok := b.BridgePorts[bridgePort]; ok {
			return snap.Port(ifIndex)
		}
		return snap.Port(bridgePort) // agents without BRIDGE-MIB: bridge port = ifIndex
	}
	if t, err := r.Columns(ctx, "dot1qPvid"); err == nil {
		for idx, row := range t {
			parts := monitor.Ints(idx)
			if len(parts) != 1 {
				continue
			}
			if p := ifOf(parts[0]); p != nil {
				p.PVID, _ = row["dot1qPvid"].AsInt()
			}
		}
	}
	names := map[int]string{}
	static, err := r.Columns(ctx, "dot1qVlanStaticName", "dot1qVlanStaticEgressPorts", "dot1qVlanStaticUntaggedPorts")
	if err != nil {
		return err
	}
	for idx, row := range static {
		if id := monitor.Ints(idx); len(id) == 1 {
			names[id[0]] = row["dot1qVlanStaticName"].String()
		}
	}
	// Current membership (index timeMark.vlanId); static configuration as fallback.
	members := map[int][2][]byte{}
	if cur, err := r.Columns(ctx, "dot1qVlanCurrentEgressPorts", "dot1qVlanCurrentUntaggedPorts"); err == nil {
		for idx, row := range cur {
			parts := monitor.Ints(idx)
			if len(parts) != 2 {
				continue
			}
			members[parts[1]] = [2][]byte{row["dot1qVlanCurrentEgressPorts"].Bytes, row["dot1qVlanCurrentUntaggedPorts"].Bytes}
		}
	}
	if len(members) == 0 {
		for idx, row := range static {
			if id := monitor.Ints(idx); len(id) == 1 {
				members[id[0]] = [2][]byte{row["dot1qVlanStaticEgressPorts"].Bytes, row["dot1qVlanStaticUntaggedPorts"].Bytes}
			}
		}
	}
	ids := map[int]bool{}
	for id := range names {
		ids[id] = true
	}
	for vid, m := range members {
		ids[vid] = true
		untagged := map[int]bool{}
		for _, bit := range monitor.BitsMSB(m[1]) {
			untagged[bit+1] = true
		}
		for _, bit := range monitor.BitsMSB(m[0]) {
			p := ifOf(bit + 1)
			if p == nil {
				continue
			}
			if untagged[bit+1] {
				p.Untagged = append(p.Untagged, vid)
			} else {
				p.Tagged = append(p.Tagged, vid)
			}
		}
	}
	for id := range ids {
		snap.VLANs = append(snap.VLANs, monitor.VLAN{ID: id, Name: names[id]})
	}
	sort.Slice(snap.VLANs, func(i, j int) bool { return snap.VLANs[i].ID < snap.VLANs[j].ID })
	for _, p := range snap.Ports {
		sort.Ints(p.Untagged)
		sort.Ints(p.Tagged)
		monitor.SetMode(p)
	}
	return nil
}
