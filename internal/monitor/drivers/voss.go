package drivers

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/lan-dot-party/ers-proxy/internal/monitor"
)

// VOSS covers Extreme (formerly Avaya) Virtual Services Platform switches
// running VOSS / Fabric Engine (VSP 4000, 7200, 7400, 8000 series), enterprise
// 2272 (RAPID-CITY). Verified on a VSP 4850GTS with VOSS 7.1.10:
//   - front-panel ports are named "slot/port" in ifName (ifIndex 192 = 1/1,
//     64 ifIndexes per slot); channelized ports are "slot/port/channel";
//   - VLANs, tagging, speed/duplex and MLTs use the same RAPID-CITY tables as
//     ERS switches (rcVlan*, rcPortOper*, rcMlt*); PortSet bit i is ifIndex i;
//   - VLAN interfaces have ifIndex 2048 + VLAN ID (ifType l2vlan).
type VOSS struct{}

// Name implements monitor.Driver.
func (VOSS) Name() string { return "voss" }

// Match implements monitor.Driver: RAPID-CITY enterprise 2272 (VSP, and the
// older ERS 8000 series that share its MIB).
func (VOSS) Match(sys monitor.System) int {
	if strings.HasPrefix(sys.ObjectID, "1.3.6.1.4.1.2272.") {
		return 100
	}
	d := sys.Descr
	if strings.HasPrefix(d, "VSP-") || strings.Contains(d, "Virtual Services Platform") || strings.Contains(d, "Fabric Engine") {
		return 90
	}
	return 0
}

// vossPortName matches "1/1" and "1/49/2" in ifName (or the end of ifDescr).
var vossPortName = regexp.MustCompile(`(?:^|\s)(\d{1,2})/(\d{1,3})(?:/(\d{1,2}))?$`)

// vossPortDescr matches the ifDescr of a front-panel port ("Extreme Networks
// Virtual Services Platform 4850GTS - 1000BaseTX Port 1/1").
var vossPortDescr = regexp.MustCompile(`\sPort \d{1,2}/\d{1,3}(?:/\d{1,2})?$`)

// vossLocate returns slot, port and channel (0 if not channelized) of a
// front-panel port, or ok = false for other interfaces.
func vossLocate(p *monitor.Port) (slot, port, channel int, ok bool) {
	for _, s := range []string{p.Name, p.Descr} {
		m := vossPortName.FindStringSubmatch(strings.TrimSpace(s))
		if m == nil {
			continue
		}
		slot, _ = strconv.Atoi(m[1])
		port, _ = strconv.Atoi(m[2])
		if m[3] != "" {
			channel, _ = strconv.Atoi(m[3])
		}
		return slot, port, channel, slot > 0 && port > 0
	}
	// No name: VOSS numbers ports 64 per slot from ifIndex 192 (slot 1).
	if p.IfIndex >= 192 && p.IfIndex < 2048 {
		return p.IfIndex/64 - 2, p.IfIndex%64 + 1, 0, true
	}
	return 0, 0, 0, false
}

// Collect implements monitor.Driver.
func (VOSS) Collect(ctx context.Context, r monitor.Reader) (*monitor.Snapshot, error) {
	channelized := false
	b, err := monitor.CollectBase(ctx, r, monitor.BaseOptions{
		Locate: func(p *monitor.Port) (int, int) {
			slot, port, ch, ok := vossLocate(p)
			if !ok || ch > 1 {
				// Not a front-panel port; sub-ports 2-4 of a channelized
				// port share the faceplate position of sub-port 1.
				channelized = channelized || ch > 1
				return 0, 0
			}
			return slot, port
		},
	})
	if err != nil {
		return nil, err
	}
	snap := b.Snap
	snap.Driver = "voss"
	for _, p := range snap.Ports {
		// VOSS's EDM resolves neither ifType nor ifName. Without ifType, a port
		// whose ifDescr ends in "Port <slot>/<port>" is a front-panel port.
		if p.Type == 0 && vossPortDescr.MatchString(p.Descr) {
			if slot, port, ch, ok := vossLocate(p); ok {
				p.Physical = true
				if ch <= 1 {
					p.Unit, p.Number = slot, port
				}
				channelized = channelized || ch > 1
			}
		}
		// Without ifName the name falls back to the long ifDescr; use the
		// CLI's slot/port notation instead.
		if p.Name == p.Descr && p.Physical {
			if slot, port, ch, ok := vossLocate(p); ok {
				p.Name = fmt.Sprintf("%d/%d", slot, port)
				if ch > 0 {
					p.Name += fmt.Sprintf("/%d", ch)
				}
			}
		}
		if p.Physical && p.Number == 0 {
			p.Physical, p.Unit = false, 0
		}
	}
	if channelized {
		snap.Notes = append(snap.Notes, "channelized ports: only sub-port 1 is shown on the faceplate, the other sub-ports are listed with the interfaces")
	}
	vossUnits(ctx, r, snap)
	ersPortExtras(ctx, r, snap)
	if err := ersVLANs(ctx, r, snap); err != nil {
		snap.Notes = append(snap.Notes, "VLANs: "+err.Error())
	}
	ersMLT(ctx, r, snap)
	for _, p := range snap.Ports {
		if p.Physical && p.LAG != 0 {
			snap.Notes = append(snap.Notes, "MLT membership on VOSS is read like on ERS switches (PortSet bit = ifIndex); not verified on hardware yet")
			break
		}
	}
	return snap, nil
}

// vossVersion takes the release from sysDescr ("VSP-4850GTS (7.1.10.0)  HW
// Base: VSP 4850") or rcSysVersion ("7.1.10.0 build on …").
var vossVersion = regexp.MustCompile(`\(([0-9][0-9A-Za-z._-]*)\)`)

// vossUnits describes the chassis once per slot with front-panel ports. VOSS
// switches are not stacked; a slot is a line card or the fixed port block.
func vossUnits(ctx context.Context, r monitor.Reader, snap *monitor.Snapshot) {
	model, _, _ := strings.Cut(strings.TrimSpace(snap.System.Descr), " ")
	if strings.HasPrefix(model, "(") {
		model = ""
	}
	firmware := ""
	if m := vossVersion.FindStringSubmatch(snap.System.Descr); m != nil {
		firmware = m[1]
	}
	serial := ""
	if v, err := r.Scalars(ctx, "rcChasSerialNumber", "rcSysVersion"); err == nil {
		serial = strings.TrimSpace(v["rcChasSerialNumber"].String())
		if firmware == "" {
			firmware, _, _ = strings.Cut(strings.TrimSpace(v["rcSysVersion"].String()), " ")
		}
	}
	slots := map[int]bool{}
	for _, p := range snap.Ports {
		if p.Physical && p.Unit > 0 {
			slots[p.Unit] = true
		}
	}
	for s := range slots {
		snap.Units = append(snap.Units, monitor.Unit{Number: s, Model: model, Serial: serial, Firmware: firmware})
	}
	sort.Slice(snap.Units, func(i, j int) bool { return snap.Units[i].Number < snap.Units[j].Number })
}
