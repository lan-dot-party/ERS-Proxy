// Package swossrc is a monitor.Reader over the web API of MikroTik SwOS
// (internal/swos). SwOS's SNMP agent has no VLAN, duplex or LAG data; its
// web API has. The reader loads the switch's state once and serves it under
// the names of the standard MIB objects, so that drivers stay independent of
// the transport:
//
//   - system group and the MikroTik scalars mtxrSerialNumber and
//     mtxrFirmwareVersion;
//   - IF-MIB (ifIndex = port number, ifName = SwOS's port label, ifAlias =
//     the port's name, 64-bit byte counters);
//   - dot3StatsDuplexStatus, dot1dBasePortIfIndex, dot3adAggPortAttachedAggID
//     (128 + trunk number, as SwOS numbers trunks in its host table);
//   - Q-BRIDGE: dot1qPvid, dot1qVlanStaticName and the current egress and
//     untagged port lists. SwOS has no per-VLAN tagging; it is derived from
//     each port's "VLAN Receive" and "Default VLAN ID" (see untagged).
package swossrc

import (
	"context"
	"strconv"
	"sync"

	"github.com/lan-dot-party/ers-proxy/internal/monitor"
	"github.com/lan-dot-party/ers-proxy/internal/swos"
)

// ObjectID is the sysObjectID SwOS's SNMP agent reports.
const ObjectID = "1.3.6.1.4.1.14988.2"

// LAGBase is added to SwOS trunk numbers to form the aggregate ID.
const LAGBase = 128

// Reader serves one round of requests (one poll). Open a new one per poll.
type Reader struct {
	C *swos.Client

	once sync.Once
	st   *swos.State
	err  error
	objs map[string]map[string]monitor.Value
}

// State returns the loaded state (loading it on the first call).
func (r *Reader) State(ctx context.Context) (*swos.State, error) {
	r.once.Do(func() {
		r.st, r.err = r.C.Load(ctx)
		if r.err == nil {
			r.objs = objects(r.st)
		}
	})
	return r.st, r.err
}

// Scalars implements monitor.Reader.
func (r *Reader) Scalars(ctx context.Context, names ...string) (map[string]monitor.Value, error) {
	if _, err := r.State(ctx); err != nil {
		return nil, err
	}
	out := map[string]monitor.Value{}
	for _, n := range names {
		if v, ok := r.objs[n][""]; ok {
			out[n] = v
		}
	}
	return out, nil
}

// Columns implements monitor.Reader.
func (r *Reader) Columns(ctx context.Context, names ...string) (monitor.Table, error) {
	if _, err := r.State(ctx); err != nil {
		return nil, err
	}
	t := monitor.Table{}
	for _, n := range names {
		for idx, v := range r.objs[n] {
			if idx == "" {
				continue
			}
			if t[idx] == nil {
				t[idx] = monitor.Row{}
			}
			t[idx][n] = v
		}
	}
	return t, nil
}

func text(s string) monitor.Value { return monitor.Value{Kind: monitor.KindBytes, Bytes: []byte(s)} }
func integer(n int) monitor.Value { return monitor.Value{Kind: monitor.KindInt, Int: int64(n)} }
func unsigned(n uint64) monitor.Value {
	return monitor.Value{Kind: monitor.KindUint, Uint: n}
}

// untagged reports whether a port sends VLAN vid without a tag. SwOS
// (CRS3xx) has no egress setting per VLAN: frames of the port's default VLAN
// leave untagged. A port that only accepts tagged frames is taken as a trunk
// (everything tagged); this is an assumption, see docs/drivers.md.
func untagged(p swos.Port, vid int) bool {
	return p.VLANReceive != "only tagged" && p.DefaultVLAN == vid
}

// objects maps the state to MIB object names; index "" holds scalars.
func objects(st *swos.State) map[string]map[string]monitor.Value {
	o := map[string]map[string]monitor.Value{}
	set := func(name, idx string, v monitor.Value) {
		if o[name] == nil {
			o[name] = map[string]monitor.Value{}
		}
		o[name][idx] = v
	}
	descr := st.Model
	if st.Version != "" {
		descr += " SwOS v" + st.Version
	}
	set("sysDescr", "", text(descr))
	set("sysObjectID", "", monitor.Value{Kind: monitor.KindOID, OID: ObjectID})
	set("sysUpTime", "", unsigned(st.Uptime))
	set("sysName", "", text(st.Identity))
	if st.Serial != "" {
		set("mtxrSerialNumber", "", text(st.Serial))
	}
	if st.Version != "" {
		set("mtxrFirmwareVersion", "", text(st.Version))
	}
	for _, p := range st.Ports {
		idx := strconv.Itoa(p.Index + 1)
		set("ifDescr", idx, text(p.Label))
		set("ifName", idx, text(p.Label))
		set("ifAlias", idx, text(p.Name))
		set("ifType", idx, integer(6)) // ethernetCsmacd
		admin, oper := 2, 2
		if p.Enabled {
			admin = 1
		}
		if p.Link {
			oper = 1
		}
		set("ifAdminStatus", idx, integer(admin))
		set("ifOperStatus", idx, integer(oper))
		if p.Link && p.Speed > 0 {
			set("ifHighSpeed", idx, unsigned(p.Speed/1_000_000))
			set("ifSpeed", idx, unsigned(min(p.Speed, 4_294_967_295)))
			duplex := 2
			if p.FullDuplex {
				duplex = 3
			}
			set("dot3StatsDuplexStatus", idx, integer(duplex))
		}
		if p.HasStats {
			set("ifHCInOctets", idx, unsigned(p.RxBytes))
			set("ifHCOutOctets", idx, unsigned(p.TxBytes))
			set("ifInErrors", idx, unsigned(p.RxErrors))
			set("ifOutErrors", idx, unsigned(p.TxErrors))
			set("ifInDiscards", idx, unsigned(p.RxDrops))
		}
		set("dot1dBasePortIfIndex", idx, integer(p.Index+1))
		if p.Trunk > 0 {
			set("dot3adAggPortAttachedAggID", idx, integer(LAGBase+p.Trunk))
		}
		if st.HasVLANs && p.VLANMode != "" && p.VLANMode != "disabled" && p.DefaultVLAN > 0 {
			set("dot1qPvid", idx, integer(p.DefaultVLAN))
		}
	}
	if !st.HasVLANs {
		return o
	}
	size := (len(st.Ports) + 7) / 8
	for _, v := range st.VLANs {
		vid := strconv.Itoa(v.ID)
		set("dot1qVlanStaticName", vid, text(v.Name))
		egress, untag := make([]byte, size), make([]byte, size)
		for _, i := range v.Members {
			if i < 0 || i >= len(st.Ports) {
				continue
			}
			p := st.Ports[i]
			if p.VLANMode == "disabled" {
				continue // the port does not use the VLAN table
			}
			egress[i/8] |= 0x80 >> uint(i%8) // Q-BRIDGE PortList: bit 0 = bridge port 1
			if untagged(p, v.ID) {
				untag[i/8] |= 0x80 >> uint(i%8)
			}
		}
		set("dot1qVlanCurrentEgressPorts", "0."+vid, monitor.Value{Kind: monitor.KindBytes, Bytes: egress})
		set("dot1qVlanCurrentUntaggedPorts", "0."+vid, monitor.Value{Kind: monitor.KindBytes, Bytes: untag})
	}
	return o
}
