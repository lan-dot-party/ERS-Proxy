package drivers

import (
	"context"
	"regexp"
	"strconv"
	"strings"

	"github.com/lan-dot-party/ers-proxy/internal/monitor"
)

// Grandstream covers Grandstream GWN switches, verified on a GWN7721
// (firmware 1.0.7.128). Facts from the hardware:
//   - sysObjectID is the bare enterprise OID 1.3.6.1.4.1.42397 (no product
//     subtree); sysDescr and sysName are the model ("GWN7721");
//   - the SNMP agent implements the system group, IF-MIB with 64-bit
//     counters (ifIndex = port number, ifName "2.5GE1" … "SFP+10"), BRIDGE-MIB
//     and from Q-BRIDGE only the scalars and dot1qPvid: no VLAN table, so
//     VLAN membership and tagging are only in the web API, which the "gwn"
//     data source (internal/monitor/gwnsrc) reads and serves under the
//     standard object names;
//   - no EtherLike, LLDP, IEEE8023-LAG or POWER-ETHERNET objects;
//   - model and firmware are in Grandstream's own scalars below
//     1.3.6.1.4.1.42397.1.3.1.
type Grandstream struct{}

// Name implements monitor.Driver.
func (Grandstream) Name() string { return "grandstream" }

// gwnObjectID is Grandstream's enterprise OID, which GWN switches report as
// sysObjectID.
const gwnObjectID = "1.3.6.1.4.1.42397"

// Match implements monitor.Driver.
func (Grandstream) Match(sys monitor.System) int {
	switch {
	case sys.ObjectID == gwnObjectID || strings.HasPrefix(sys.ObjectID, gwnObjectID+"."):
		return 100
	case strings.HasPrefix(sys.Descr, "GWN"):
		return 90
	}
	return 0
}

// gwnPort matches the ifName of a front-panel port and captures its number:
// "2.5GE1", "GE12", "SFP+10", "10GE2".
var gwnPort = regexp.MustCompile(`^(?:[0-9.]*GE|SFP\+?|XGE|FE)(\d+)$`)

// Collect implements monitor.Driver.
func (Grandstream) Collect(ctx context.Context, r monitor.Reader) (*monitor.Snapshot, error) {
	b, err := monitor.CollectBase(ctx, r, monitor.BaseOptions{
		Locate: func(p *monitor.Port) (int, int) {
			if m := gwnPort.FindStringSubmatch(p.Name); m != nil {
				if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
					return 1, n
				}
			}
			return 1, p.IfIndex
		},
	})
	if err != nil {
		return nil, err
	}
	snap := b.Snap
	snap.Driver = "grandstream"
	unit := monitor.Unit{Number: 1, Model: snap.System.Descr}
	if v, err := r.Scalars(ctx, "gwnProductModel", "gwnFirmwareVersion"); err == nil {
		if m := v["gwnProductModel"].String(); m != "" {
			unit.Model = m
		}
		unit.Firmware = v["gwnFirmwareVersion"].String()
	}
	// Only the web data source knows the serial number; over SNMP the name
	// is unknown and the read fails.
	if v, err := r.Scalars(ctx, "gwnSerialNumber"); err == nil {
		unit.Serial = v["gwnSerialNumber"].String()
	}
	snap.Units = []monitor.Unit{unit}
	for _, p := range snap.Ports {
		if p.LAG > gwnLAGBase && p.LAGName == "" {
			p.LAGName = "LAG" + strconv.Itoa(p.LAG-gwnLAGBase)
		}
	}
	if err := QBridge(ctx, r, b); err != nil {
		snap.Notes = append(snap.Notes, "VLANs: "+err.Error())
	}
	if src, web := r.(interface{ SourceNotes() []string }); web {
		snap.Notes = append(snap.Notes, src.SourceNotes()...)
	} else if len(snap.VLANs) == 0 {
		snap.Notes = append(snap.Notes, "VLANs: the switch reports only each port's PVID over SNMP; choose the data source \"GWN (web + SNMP)\" for VLANs, tagging, LAGs and LLDP")
	}
	return snap, nil
}

// gwnLAGBase must match gwnsrc.LAGBase.
const gwnLAGBase = 1000
