package drivers

import (
	"context"
	"strconv"
	"strings"

	"github.com/lan-dot-party/ers-proxy/internal/monitor"
)

// SwOS covers MikroTik switches running SwOS (CRS3xx, CSS3xx, …), verified on
// a CRS305-1G-4S+ with SwOS 2.18. Facts from the hardware:
//   - sysObjectID is 1.3.6.1.4.1.14988.2 (RouterOS reports .14988.1);
//   - the SNMP agent implements only the system group, IF-MIB (ifIndex = port
//     number, ifName = ifDescr = the port's configured name), BRIDGE-MIB
//     without Q-BRIDGE, and the MikroTik serial number, firmware version and
//     SFP table. VLANs, duplex and LAGs are only in the web API, which the
//     "swos" data source (internal/monitor/swossrc) reads and serves under
//     the standard object names;
//   - SwOS numbers trunks (LAGs) 1–15; the data source reports them as
//     aggregate ID 128 + trunk, which this driver names "Trunk<n>".
type SwOS struct{}

// Name implements monitor.Driver.
func (SwOS) Name() string { return "swos" }

// Match implements monitor.Driver.
func (SwOS) Match(sys monitor.System) int {
	switch {
	case sys.ObjectID == "1.3.6.1.4.1.14988.2" || strings.HasPrefix(sys.ObjectID, "1.3.6.1.4.1.14988.2."):
		return 100
	case strings.Contains(sys.Descr, "SwOS"):
		return 90
	}
	return 0
}

// swosLAGBase must match swossrc.LAGBase.
const swosLAGBase = 128

// Collect implements monitor.Driver.
func (SwOS) Collect(ctx context.Context, r monitor.Reader) (*monitor.Snapshot, error) {
	b, err := monitor.CollectBase(ctx, r, monitor.BaseOptions{
		Locate: func(p *monitor.Port) (int, int) { return 1, p.IfIndex },
	})
	if err != nil {
		return nil, err
	}
	snap := b.Snap
	snap.Driver = "swos"
	unit := monitor.Unit{Number: 1, Model: swosModel(snap.System.Descr)}
	if v, err := r.Scalars(ctx, "mtxrSerialNumber", "mtxrFirmwareVersion"); err == nil {
		unit.Serial = v["mtxrSerialNumber"].String()
		unit.Firmware = v["mtxrFirmwareVersion"].String()
	}
	snap.Units = []monitor.Unit{unit}
	for _, p := range snap.Ports {
		if p.LAG > swosLAGBase && p.LAGName == "" {
			p.LAGName = "Trunk" + strconv.Itoa(p.LAG-swosLAGBase)
		}
	}
	if err := QBridge(ctx, r, b); err != nil {
		snap.Notes = append(snap.Notes, "VLANs: "+err.Error())
	}
	if len(snap.VLANs) == 0 {
		snap.Notes = append(snap.Notes, "VLANs: SwOS reports no VLANs over SNMP; choose the data source \"SwOS (web)\" for VLANs, duplex and LAGs")
	} else {
		snap.Notes = append(snap.Notes, "VLAN tagging is derived: SwOS sends a port's default VLAN untagged unless the port only accepts tagged frames")
	}
	return snap, nil
}

// swosModel takes the model from sysDescr ("CRS305-1G-4S+ SwOS v2.18").
func swosModel(descr string) string {
	if i := strings.Index(descr, " SwOS"); i > 0 {
		return strings.TrimSpace(descr[:i])
	}
	return ""
}
