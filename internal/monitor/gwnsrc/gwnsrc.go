// Package gwnsrc is a monitor.Reader over the web API of Grandstream GWN
// switches (internal/gwn), optionally combined with their SNMP agent.
//
// The SNMP agent of a GWN switch has no VLAN table, LAGs, LLDP or duplex;
// the web API has all of these but no byte counters. The reader therefore
// serves from the web API (loaded once per poll) under the names of the
// standard MIB objects:
//
//   - system group, the scalars gwnProductModel and gwnFirmwareVersion, and
//     gwnSerialNumber (web API only, not in monitor.Objects);
//   - IF-MIB without counters (ifIndex = port number, ifName = the web
//     interface's label "2.5GE1", ifAlias = the port's description);
//   - dot3StatsDuplexStatus, dot1dBasePortIfIndex, dot3adAggPortAttachedAggID
//     (LAGBase + LAG number);
//   - Q-BRIDGE: dot1qPvid, dot1qVlanStaticName and the current egress and
//     untagged port lists, from the 802.1Q VLAN table with its tagged and
//     untagged port maps;
//   - LLDP-MIB: lldpRemSysName, lldpRemPortId, lldpRemPortDesc (index
//     0.<port>.1).
//
// Counters (octets, errors, discards, ifLastChange) come from SNMP when an
// SNMP reader is set; without one only the error counters of the web API are
// served, and the dashboard shows no traffic rates. A failing SNMP read does
// not fail the poll; it is reported in SourceNotes.
package gwnsrc

import (
	"context"
	"strconv"
	"sync"

	"github.com/lan-dot-party/ers-proxy/internal/gwn"
	"github.com/lan-dot-party/ers-proxy/internal/monitor"
)

// LAGBase is added to GWN LAG numbers to form the aggregate ID.
const LAGBase = 1000

// ObjectID is the sysObjectID GWN switches report (Grandstream's enterprise OID).
const ObjectID = "1.3.6.1.4.1.42397"

// counters are read from SNMP when it is available.
var counters = map[string]bool{
	"ifInOctets": true, "ifOutOctets": true, "ifHCInOctets": true, "ifHCOutOctets": true,
	"ifInErrors": true, "ifOutErrors": true, "ifInDiscards": true, "ifOutDiscards": true,
	"ifLastChange": true, "ifCounterDiscontinuityTime": true,
}

// Reader serves one round of requests (one poll). Open a new one per poll.
type Reader struct {
	C *gwn.Client
	// SNMP, if set, serves the counters and every object the web API does
	// not provide.
	SNMP monitor.Reader

	once  sync.Once
	st    *gwn.State
	err   error
	objs  map[string]map[string]monitor.Value
	mu    sync.Mutex
	notes []string
}

// State returns the loaded state (loading it on the first call).
func (r *Reader) State(ctx context.Context) (*gwn.State, error) {
	r.once.Do(func() {
		r.st, r.err = r.C.Load(ctx)
		if r.err == nil {
			r.objs = objects(r.st)
			r.notes = append(r.notes, r.st.Notes...)
			if !r.st.VLANs802 {
				r.notes = append(r.notes, "VLANs: the switch runs port-based VLANs; only 802.1Q VLANs are shown")
			}
			if r.SNMP == nil {
				r.notes = append(r.notes, "Traffic: the web API has no byte counters; store SNMP credentials for the device to see traffic rates")
			}
		}
	})
	return r.st, r.err
}

// SourceNotes returns remarks about data that could not be read.
func (r *Reader) SourceNotes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.notes...)
}

func (r *Reader) note(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, n := range r.notes {
		if n == s {
			return
		}
	}
	r.notes = append(r.notes, s)
}

// fromSNMP reports whether an object is read from SNMP.
func (r *Reader) fromSNMP(name string) bool {
	if r.SNMP == nil {
		return false
	}
	if counters[name] {
		return true
	}
	_, web := r.objs[name]
	return !web && !webObjects[name]
}

// Scalars implements monitor.Reader.
func (r *Reader) Scalars(ctx context.Context, names ...string) (map[string]monitor.Value, error) {
	if _, err := r.State(ctx); err != nil {
		return nil, err
	}
	out := map[string]monitor.Value{}
	var viaSNMP []string
	for _, n := range names {
		if r.fromSNMP(n) {
			viaSNMP = append(viaSNMP, n)
		} else if v, ok := r.objs[n][""]; ok {
			out[n] = v
		}
	}
	if len(viaSNMP) > 0 {
		v, err := r.SNMP.Scalars(ctx, viaSNMP...)
		if err != nil {
			r.note("SNMP: " + err.Error())
		}
		for k, x := range v {
			out[k] = x
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
	var viaSNMP []string
	for _, n := range names {
		if r.fromSNMP(n) {
			viaSNMP = append(viaSNMP, n)
			continue
		}
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
	if len(viaSNMP) > 0 {
		s, err := r.SNMP.Columns(ctx, viaSNMP...)
		if err != nil {
			r.note("SNMP: " + err.Error() + " (no traffic rates in this poll)")
		}
		for idx, row := range s {
			if _, known := t[idx]; !known && !r.isPort(idx) {
				continue // only the ports the web API reported
			}
			if t[idx] == nil {
				t[idx] = monitor.Row{}
			}
			for k, v := range row {
				t[idx][k] = v
			}
		}
	}
	return t, nil
}

func (r *Reader) isPort(idx string) bool {
	_, ok := r.objs["ifName"][idx]
	return ok
}

// webObjects are served from the web API even when the loaded state has no
// rows for them (an empty VLAN table must not fall back to SNMP).
var webObjects = map[string]bool{
	"sysDescr": true, "sysObjectID": true, "sysUpTime": true, "sysName": true, "sysLocation": true,
	"gwnProductModel": true, "gwnFirmwareVersion": true, "gwnSerialNumber": true,
	"ifDescr": true, "ifName": true, "ifAlias": true, "ifType": true, "ifAdminStatus": true, "ifOperStatus": true,
	"ifSpeed": true, "ifHighSpeed": true, "dot3StatsDuplexStatus": true, "dot1dBasePortIfIndex": true,
	"dot3adAggPortAttachedAggID": true, "dot1qPvid": true, "dot1qVlanStaticName": true,
	"dot1qVlanStaticEgressPorts": true, "dot1qVlanStaticUntaggedPorts": true,
	"dot1qVlanCurrentEgressPorts": true, "dot1qVlanCurrentUntaggedPorts": true,
	"lldpRemSysName": true, "lldpRemPortId": true, "lldpRemPortDesc": true, "lldpRemSysCapEnabled": true,
	"lldpLocPortId": true, "lldpLocPortDesc": true,
}

func text(s string) monitor.Value { return monitor.Value{Kind: monitor.KindBytes, Bytes: []byte(s)} }
func integer(n int) monitor.Value { return monitor.Value{Kind: monitor.KindInt, Int: int64(n)} }
func unsigned(n uint64) monitor.Value {
	return monitor.Value{Kind: monitor.KindUint, Uint: n}
}

// objects maps the state to MIB object names; index "" holds scalars.
func objects(st *gwn.State) map[string]map[string]monitor.Value {
	o := map[string]map[string]monitor.Value{}
	set := func(name, idx string, v monitor.Value) {
		if o[name] == nil {
			o[name] = map[string]monitor.Value{}
		}
		o[name][idx] = v
	}
	objectID := st.ObjectID
	if objectID == "" {
		objectID = ObjectID
	}
	set("sysDescr", "", text(st.Model))
	set("sysObjectID", "", monitor.Value{Kind: monitor.KindOID, OID: objectID})
	set("sysUpTime", "", unsigned(st.Uptime))
	set("sysName", "", text(st.Name))
	set("sysLocation", "", text(st.Location))
	if st.Model != "" {
		set("gwnProductModel", "", text(st.Model))
	}
	if st.Firmware != "" {
		set("gwnFirmwareVersion", "", text(st.Firmware))
	}
	if st.Serial != "" {
		set("gwnSerialNumber", "", text(st.Serial))
	}
	maxID := 0
	for _, p := range st.Ports {
		maxID = max(maxID, p.ID)
		idx := strconv.Itoa(p.ID)
		set("ifDescr", idx, text(p.Label))
		set("ifName", idx, text(p.Label))
		set("ifAlias", idx, text(p.Descr))
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
			set("ifInErrors", idx, unsigned(p.RxBad))
			set("ifOutErrors", idx, unsigned(p.TxBad))
		}
		set("dot1dBasePortIfIndex", idx, integer(p.ID))
		if p.LAG > 0 {
			set("dot3adAggPortAttachedAggID", idx, integer(LAGBase+p.LAG))
		}
		if st.HasVLANs && p.PVID > 0 {
			set("dot1qPvid", idx, integer(p.PVID))
		}
	}
	for _, n := range st.Neighbors {
		idx := "0." + strconv.Itoa(n.LocalPort) + ".1"
		set("lldpRemSysName", idx, text(n.SysName))
		set("lldpRemPortId", idx, text(n.PortID))
		set("lldpRemPortDesc", idx, text(""))
	}
	if !st.HasVLANs {
		return o
	}
	size := (maxID + 7) / 8
	bits := func(b []byte, ports []int) {
		for _, id := range ports {
			if id >= 1 && id <= maxID {
				b[(id-1)/8] |= 0x80 >> uint((id-1)%8) // Q-BRIDGE PortList: bit 0 = bridge port 1
			}
		}
	}
	for _, v := range st.VLANs {
		vid := strconv.Itoa(v.ID)
		set("dot1qVlanStaticName", vid, text(v.Name))
		egress, untag := make([]byte, size), make([]byte, size)
		bits(egress, v.Tagged)
		bits(egress, v.Untagged)
		bits(untag, v.Untagged)
		set("dot1qVlanCurrentEgressPorts", "0."+vid, monitor.Value{Kind: monitor.KindBytes, Bytes: egress})
		set("dot1qVlanCurrentUntaggedPorts", "0."+vid, monitor.Value{Kind: monitor.KindBytes, Bytes: untag})
	}
	return o
}
