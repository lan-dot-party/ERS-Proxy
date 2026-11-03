// Package monitor collects the state of every port of a switch into a common
// model (Snapshot). Drivers know where a vendor keeps its data; they read MIB
// objects by name through a Reader, which hides the transport (SNMP, or the
// EDM HTTP interface of ERS switches). Everything here is read-only: nothing
// in this package or its drivers writes to a device.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Kind is the type of a Value.
type Kind int

// Value kinds.
const (
	KindNone Kind = iota
	KindInt
	KindUint // counters, gauges, timeticks
	KindBytes
	KindOID
)

// Value is one object value, independent of the transport.
type Value struct {
	Kind  Kind
	Int   int64
	Uint  uint64
	Bytes []byte
	OID   string
}

// AsInt returns the value as int (0, false if it is not numeric).
func (v Value) AsInt() (int, bool) {
	switch v.Kind {
	case KindInt:
		return int(v.Int), true
	case KindUint:
		return int(v.Uint), true
	}
	return 0, false
}

// AsUint returns the value as uint64.
func (v Value) AsUint() (uint64, bool) {
	switch v.Kind {
	case KindUint:
		return v.Uint, true
	case KindInt:
		if v.Int >= 0 {
			return uint64(v.Int), true
		}
	}
	return 0, false
}

// String returns a printable form: text for octet strings, numbers as decimal.
func (v Value) String() string {
	switch v.Kind {
	case KindBytes:
		return strings.ToValidUTF8(strings.TrimRight(string(v.Bytes), "\x00"), "?")
	case KindInt:
		return strconv.FormatInt(v.Int, 10)
	case KindUint:
		return strconv.FormatUint(v.Uint, 10)
	case KindOID:
		return v.OID
	}
	return ""
}

// Row holds the columns of one table row by object name.
type Row map[string]Value

// Table maps the row index ("12", "1.5", …) to its columns.
type Table map[string]Row

// Ints parses a row index into its components.
func Ints(index string) []int {
	parts := strings.Split(index, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil
		}
		out = append(out, n)
	}
	return out
}

// Reader reads MIB objects by name (see Objects). Missing objects are not an
// error: they are simply absent from the result, because agents implement
// different subsets.
type Reader interface {
	// Scalars returns the values of scalar objects.
	Scalars(ctx context.Context, names ...string) (map[string]Value, error)
	// Columns walks table columns and merges them by row index.
	Columns(ctx context.Context, names ...string) (Table, error)
}

// System describes the device as a whole.
type System struct {
	Name     string
	Descr    string
	ObjectID string
	Location string
	Uptime   time.Duration
}

// Unit is one member of a stack (or the only one).
type Unit struct {
	Number   int
	Model    string
	Serial   string
	Firmware string
}

// PoE is the power state of a port.
type PoE struct {
	// Status: disabled, searching, delivering, fault, test, other.
	Status  string
	Class   int
	PowerMW int // 0 if the agent does not report it
}

// Neighbor is the LLDP neighbour of a port.
type Neighbor struct {
	SysName  string
	PortID   string
	PortDesc string
	// Bridge is set when the neighbour announces bridge (switch) capability:
	// the port is probably an uplink.
	Bridge bool
}

// Port is one interface of the switch.
type Port struct {
	IfIndex int
	// Unit and Number locate the port on the front panel (1-based). Number is
	// 0 for interfaces that are not front-panel ports (VLAN interfaces, LAGs).
	Unit, Number       int
	Name, Descr, Alias string
	Type               int
	Physical           bool
	IsLAG              bool

	Admin, Oper string // up, down, testing, …
	Speed       uint64 // bit/s
	Duplex      string // full, half or ""

	PVID     int
	Untagged []int
	Tagged   []int
	// Mode: access, trunk, hybrid or "".
	Mode string

	PoE      *PoE
	LAG      int    // ifIndex or ID of the aggregate this port belongs to (0 = none)
	LAGName  string // name of the aggregate, if known
	Neighbor *Neighbor

	InOctets, OutOctets     uint64
	InErrors, OutErrors     uint64
	InDiscards, OutDiscards uint64
	HasHC                   bool          // 64-bit counters available
	LastChange              time.Duration // sysUpTime at the last state change
	Discontinuity           uint64        // ifCounterDiscontinuityTime

	// Filled by the poller from two snapshots.
	InBps, OutBps float64
	// Utilization is the higher direction in percent of the speed.
	Utilization float64
}

// Up reports whether the link is up.
func (p *Port) Up() bool { return p.Oper == "up" }

// VLAN is a VLAN known to the switch.
type VLAN struct {
	ID   int
	Name string
}

// PoEBudget is the power supply of the switch (sum over all PSEs).
type PoEBudget struct {
	TotalW, UsedW int
}

// Snapshot is the state of one device at one point in time.
type Snapshot struct {
	Time   time.Time
	Driver string
	System System
	Units  []Unit
	Ports  []*Port
	VLANs  []VLAN
	PoE    *PoEBudget
	// Notes lists things a driver could not determine (shown in diagnostics).
	Notes []string
}

// Port returns the port with an ifIndex.
func (s *Snapshot) Port(ifIndex int) *Port {
	for _, p := range s.Ports {
		if p.IfIndex == ifIndex {
			return p
		}
	}
	return nil
}

// SortPorts orders ports by unit, number, then ifIndex.
func (s *Snapshot) SortPorts() {
	sort.SliceStable(s.Ports, func(i, j int) bool {
		a, b := s.Ports[i], s.Ports[j]
		if (a.Number == 0) != (b.Number == 0) {
			return a.Number != 0
		}
		if a.Unit != b.Unit {
			return a.Unit < b.Unit
		}
		if a.Number != b.Number {
			return a.Number < b.Number
		}
		return a.IfIndex < b.IfIndex
	})
}

// Driver knows where a vendor keeps port data.
type Driver interface {
	// Name is the identifier stored with a device (generic, ers, cisco, tplink).
	Name() string
	// Match rates how well the driver fits a device: 0 = not at all, 10 = the
	// generic fallback, 100 = made for this vendor.
	Match(sys System) int
	// Collect reads a complete snapshot.
	Collect(ctx context.Context, r Reader) (*Snapshot, error)
}

// Registry holds the available drivers.
type Registry struct {
	drivers []Driver
}

// NewRegistry creates a registry with the given drivers.
func NewRegistry(drivers ...Driver) *Registry { return &Registry{drivers: drivers} }

// Names lists the driver names.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.drivers))
	for _, d := range r.drivers {
		out = append(out, d.Name())
	}
	return out
}

// Get returns a driver by name.
func (r *Registry) Get(name string) (Driver, error) {
	for _, d := range r.drivers {
		if d.Name() == name {
			return d, nil
		}
	}
	return nil, fmt.Errorf("unknown driver %q", name)
}

// ErrNoDriver is returned when no driver accepts a device.
var ErrNoDriver = errors.New("no driver matches this device")

// Detect picks the best driver for a device.
func (r *Registry) Detect(sys System) (Driver, error) {
	var best Driver
	score := 0
	for _, d := range r.drivers {
		if s := d.Match(sys); s > score {
			best, score = d, s
		}
	}
	if best == nil {
		return nil, ErrNoDriver
	}
	return best, nil
}

// ReadSystem reads the system group.
func ReadSystem(ctx context.Context, r Reader) (System, error) {
	v, err := r.Scalars(ctx, "sysDescr", "sysObjectID", "sysUpTime", "sysName", "sysLocation")
	if err != nil {
		return System{}, err
	}
	sys := System{Name: v["sysName"].String(), Descr: v["sysDescr"].String(), Location: v["sysLocation"].String()}
	if o := v["sysObjectID"]; o.Kind == KindOID {
		sys.ObjectID = o.OID
	}
	if t, ok := v["sysUpTime"].AsUint(); ok {
		sys.Uptime = Ticks(t)
	}
	if sys.Name == "" && sys.Descr == "" && sys.ObjectID == "" {
		return sys, errors.New("the device returned no system information")
	}
	return sys, nil
}

// Ticks converts TimeTicks (1/100 s).
func Ticks(t uint64) time.Duration { return time.Duration(t) * 10 * time.Millisecond }

// BitsMSB returns the positions of all set bits, counting from the most
// significant bit of the first octet as 0. Both Q-BRIDGE PortList (position+1
// = bridge port) and ERS PortSet (position = ifIndex) use this order.
func BitsMSB(b []byte) []int {
	var out []int
	for i, octet := range b {
		for j := 7; j >= 0; j-- {
			if octet&(1<<uint(j)) != 0 {
				out = append(out, i*8+(7-j))
			}
		}
	}
	return out
}
