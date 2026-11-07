// Package switchcfgtest is an in-memory switch for tests of internal/switchcfg:
// a monitor.Reader and a switchcfg.Writer over the same objects, with
// RowStatus handling for rcVlanTable and failure injection.
package switchcfgtest

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/lan-dot-party/ers-proxy/internal/monitor"
	"github.com/lan-dot-party/ers-proxy/internal/switchcfg"
)

// ErrUnreachable is returned while the switch is "unreachable".
var ErrUnreachable = errors.New("switchcfgtest: switch not reachable")

// ErrRejected is returned for an injected failing write.
var ErrRejected = errors.New("switchcfgtest: write rejected")

// Switch is the fake.
type Switch struct {
	mu      sync.Mutex
	objects map[string]map[string]monitor.Value // name → index → value ("" = scalar)
	// Writes records every accepted step.
	Writes [][]switchcfg.Assignment
	// FailWrite rejects the n-th write from now (1 = the next one); 0 = never.
	FailWrite int
	// Unreachable makes every read and write fail.
	Unreachable bool
	// UnreachableAfterWrites makes the switch unreachable once that many
	// writes were accepted (0 = never): a change that cuts the proxy off.
	UnreachableAfterWrites int
	// Ignore lists object names whose writes are accepted but not applied
	// (a switch that answers OK and does nothing).
	Ignore map[string]bool
}

// New returns an empty switch.
func New() *Switch { return &Switch{objects: map[string]map[string]monitor.Value{}} }

// Put sets a value directly.
func (s *Switch) Put(name, index string, v monitor.Value) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.put(name, index, v)
}

func (s *Switch) put(name, index string, v monitor.Value) {
	if s.objects[name] == nil {
		s.objects[name] = map[string]monitor.Value{}
	}
	s.objects[name][index] = v
}

// Delete removes a value directly.
func (s *Switch) Delete(name, index string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects[name], index)
}

// Get returns a value directly.
func (s *Switch) Get(name, index string) (monitor.Value, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.objects[name][index]
	return v, ok
}

// Snapshot copies all objects (to compare states).
func (s *Switch) Snapshot() map[string]map[string]monitor.Value {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]map[string]monitor.Value{}
	for n, m := range s.objects {
		out[n] = map[string]monitor.Value{}
		for i, v := range m {
			out[n][i] = v
		}
	}
	return out
}

// Scalars implements monitor.Reader.
func (s *Switch) Scalars(_ context.Context, names ...string) (map[string]monitor.Value, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Unreachable {
		return nil, ErrUnreachable
	}
	out := map[string]monitor.Value{}
	for _, n := range names {
		if v, ok := s.objects[n][""]; ok {
			out[n] = v
		}
	}
	return out, nil
}

// Columns implements monitor.Reader.
func (s *Switch) Columns(_ context.Context, names ...string) (monitor.Table, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Unreachable {
		return nil, ErrUnreachable
	}
	t := monitor.Table{}
	for _, n := range names {
		for idx, v := range s.objects[n] {
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

// Set implements switchcfg.Writer. A step is applied as a whole or not at all.
func (s *Switch) Set(_ context.Context, as []switchcfg.Assignment) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Unreachable {
		return ErrUnreachable
	}
	if s.FailWrite > 0 {
		s.FailWrite--
		if s.FailWrite == 0 {
			return ErrRejected
		}
	}
	for _, a := range as {
		if s.Ignore[a.Name] {
			continue
		}
		if a.Name == "rcVlanRowStatus" {
			n, _ := a.Value.AsInt()
			switch n {
			case switchcfg.RowCreateAndGo:
				s.put("rcVlanPortMembers", a.Index, monitor.Value{Kind: monitor.KindBytes, Bytes: make([]byte, 8)})
			case switchcfg.RowDestroy:
				for name, m := range s.objects {
					if strings.HasPrefix(name, "rcVlan") && !strings.HasPrefix(name, "rcVlanPort") {
						delete(m, a.Index)
					}
				}
				delete(s.objects["rcVlanPortMembers"], a.Index)
			}
			continue
		}
		s.put(a.Name, a.Index, a.Value)
	}
	s.Writes = append(s.Writes, as)
	if s.UnreachableAfterWrites > 0 && len(s.Writes) >= s.UnreachableAfterWrites {
		s.Unreachable = true
	}
	return nil
}

// SetReachable switches reachability.
func (s *Switch) SetReachable(ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Unreachable = !ok
}

// WriteCount returns the number of accepted writes.
func (s *Switch) WriteCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.Writes)
}

// VOSS fills the switch with a small VSP: ports 1/1-1/4 (ifIndex 192-195),
// VLAN 1 (ports 1/1, 1/2 untagged), VLAN 10 (port 1/3 untagged), VLAN 20
// and 30 (port 1/4 tagged), VLAN 100 with an IP address (management, port
// 1/1 is also member), PoE on 1/1-1/4, rcVlanPortUntagDefaultVlan false on
// 1/1-1/3 (1/4 has none). It returns the matching snapshot.
func (s *Switch) VOSS() *monitor.Snapshot {
	bits := func(ifIndexes ...int) monitor.Value {
		b := make([]byte, 64)
		for _, i := range ifIndexes {
			b = switchcfg.SetBit(b, i, true, 64)
		}
		return monitor.Value{Kind: monitor.KindBytes, Bytes: b}
	}
	vlan := func(vid, name string, members monitor.Value) {
		s.Put("rcVlanName", vid, switchcfg.Text(name))
		s.Put("rcVlanType", vid, switchcfg.Int(1))
		s.Put("rcVlanStgId", vid, switchcfg.Int(1))
		s.Put("rcVlanPortMembers", vid, members)
	}
	vlan("1", "Default", bits(192, 193))
	vlan("10", "Clients", bits(194))
	vlan("20", "Voice", bits(195))
	vlan("30", "Cameras", bits(195))
	vlan("100", "Mgmt", bits(192, 195))
	snap := &monitor.Snapshot{Driver: "voss"}
	for n, ifi := range []int{192, 193, 194, 195} {
		idx := itoa(ifi)
		s.Put("ifAdminStatus", idx, switchcfg.Int(1))
		s.Put("ifAlias", idx, switchcfg.Text(""))
		s.Put("pethPsePortAdminEnable", "1."+itoa(n+1), switchcfg.Int(1))
		pvid, tag := 1, 2
		switch ifi {
		case 194:
			pvid = 10
		case 195:
			pvid, tag = 20, 1
		}
		s.Put("rcVlanPortDefaultVlanId", idx, switchcfg.Int(pvid))
		s.Put("rcVlanPortPerformTagging", idx, switchcfg.Int(tag))
		if ifi != 195 { // 1/4 has none, like a firmware without it
			s.Put("rcVlanPortUntagDefaultVlan", idx, switchcfg.Int(2))
		}
		snap.Ports = append(snap.Ports, &monitor.Port{IfIndex: ifi, Unit: 1, Number: n + 1, Name: "1/" + itoa(n+1),
			Physical: true, Admin: "up", Oper: "up", PVID: pvid})
	}
	s.Put("ipAdEntIfIndex", "10.0.0.2", switchcfg.Int(2048+100))
	return snap
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
