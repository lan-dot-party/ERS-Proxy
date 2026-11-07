// Package switchcfg changes switch settings: port VLANs, port state and name,
// VLANs and PoE. It is the only part of ERS-Proxy that writes to a switch.
// Monitoring (internal/monitor, internal/snmp) stays read-only.
//
// A change goes through a Planner (one per driver), which reads the current
// values, checks the change and returns a Plan: ordered steps, the inverse
// steps that restore the values read before, and the values expected after
// each direction. The Manager applies a plan "commit confirmed": it writes,
// reads back, and reverts unless the change is confirmed in time.
//
// Nothing here is reachable unless configure.enabled is set in config.yaml.
// See docs/switch-configuration.md.
package switchcfg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/lan-dot-party/ers-proxy/internal/monitor"
)

// Change is what an admin asked for. Nil fields stay as they are.
type Change struct {
	Ports []PortChange `json:"ports,omitempty"`
	VLANs []VLANChange `json:"vlans,omitempty"`
}

// Empty reports whether the change asks for nothing.
func (c Change) Empty() bool {
	for _, p := range c.Ports {
		if p.Admin != nil || p.Alias != nil || p.PoE != nil || p.VLANs != nil {
			return false
		}
	}
	return len(c.VLANs) == 0
}

// PortChange changes one port.
type PortChange struct {
	IfIndex int `json:"ifIndex"`
	// Admin true enables the port, false disables it.
	Admin *bool `json:"admin,omitempty"`
	// Alias is the port name (ifAlias).
	Alias *string `json:"alias,omitempty"`
	// PoE true enables power on the port, false disables it.
	PoE *bool `json:"poe,omitempty"`
	// VLANs replaces the port's VLAN membership.
	VLANs *PortVLANs `json:"vlans,omitempty"`
}

// PortVLANs is the VLAN membership of a port. The untagged VLAN is also the
// port's PVID.
type PortVLANs struct {
	// Untagged is the VLAN of untagged frames (0 = none: the port carries
	// tagged frames only).
	Untagged int `json:"untagged"`
	// Tagged lists the VLANs the port carries tagged.
	Tagged []int `json:"tagged,omitempty"`
	// Hybrid keeps the port tagging even when Tagged is empty: it sends
	// Untagged untagged and is ready for tagged VLANs (VOSS:
	// "encapsulation dot1q" with "untag-port-default-vlan"). An untagged VLAN
	// together with tagged ones is always hybrid.
	Hybrid bool `json:"hybrid,omitempty"`
}

// VLAN operations.
const (
	VLANCreate = "create"
	VLANRename = "rename"
	VLANDelete = "delete"
)

// VLANChange creates, renames or deletes one VLAN.
type VLANChange struct {
	Op   string `json:"op"`
	ID   int    `json:"id"`
	Name string `json:"name,omitempty"`
}

// Assignment sets one object instance. Name is a MIB object name (see
// monitor.Objects and Objects); Index is the row index, empty for a scalar.
type Assignment struct {
	Name  string        `json:"name"`
	Index string        `json:"index,omitempty"`
	Value monitor.Value `json:"value"`
}

// Instance returns "name.index" (or "name.0" for a scalar), the form EDM
// expects.
func (a Assignment) Instance() string {
	if a.Index == "" {
		return a.Name + ".0"
	}
	return a.Name + "." + a.Index
}

// String describes the assignment for logs and the audit log.
func (a Assignment) String() string {
	return a.Instance() + " = " + valueText(a.Value)
}

func valueText(v monitor.Value) string {
	if v.Kind == monitor.KindBytes {
		printable := true
		for _, c := range v.Bytes {
			if c < 0x20 || c > 0x7e {
				printable = false
				break
			}
		}
		if printable {
			return fmt.Sprintf("%q", string(v.Bytes))
		}
		return "0x" + hex.EncodeToString(v.Bytes)
	}
	return v.String()
}

// Step is one logical write: one SNMP SET PDU or one EDM setVariables
// request. A switch applies a step as a whole or rejects it.
type Step struct {
	Description string       `json:"description"`
	Set         []Assignment `json:"set"`
}

// Warning is something the admin has to acknowledge before a plan is applied.
type Warning struct {
	// Code is a short identifier; the UI translates "config.warn.<code>".
	Code    string `json:"code"`
	IfIndex int    `json:"ifIndex,omitempty"`
	VLAN    int    `json:"vlan,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// Plan is a checked change, ready to apply.
type Plan struct {
	Driver string `json:"driver"`
	// Summary lists the change in words (one line per item).
	Summary []string `json:"summary"`
	// Steps apply the change, in order.
	Steps []Step `json:"steps"`
	// Expected are the values after Steps.
	Expected []Assignment `json:"expected"`
	// Inverse restores the values read before planning, in order.
	Inverse []Step `json:"inverse"`
	// Before are the values after Inverse (the state before the change).
	Before []Assignment `json:"before"`
	// Save makes the running configuration permanent (empty = the switch
	// saves by itself or the driver does not know how; see SaveKnown).
	Save      []Step    `json:"save,omitempty"`
	SaveKnown bool      `json:"saveKnown"`
	Warnings  []Warning `json:"warnings,omitempty"`
}

// Hash identifies exactly what a plan writes. The preview shows a plan with
// its hash; applying needs the same hash, so what is written is what was
// shown.
func (p *Plan) Hash() string {
	b, _ := json.Marshal(struct {
		D string
		S []Step
		E []Assignment
		I []Step
		B []Assignment
		V []Step
	}{p.Driver, p.Steps, p.Expected, p.Inverse, p.Before, p.Save})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Refused is returned by a planner for a change it will not make.
type Refused struct {
	// Code is a short identifier; the UI translates "config.refuse.<code>".
	Code   string
	Detail string
}

func (e *Refused) Error() string {
	if e.Detail == "" {
		return "change refused: " + e.Code
	}
	return "change refused: " + e.Code + ": " + e.Detail
}

// Refuse builds a Refused error.
func Refuse(code, format string, args ...any) error {
	return &Refused{Code: code, Detail: fmt.Sprintf(format, args...)}
}

// IsRefused reports whether err is a Refused error.
func IsRefused(err error) bool {
	var r *Refused
	return errors.As(err, &r)
}

// Capabilities tell the UI what a planner can change.
type Capabilities struct {
	Admin     bool `json:"admin"`
	Alias     bool `json:"alias"`
	PoE       bool `json:"poe"`
	PortVLANs bool `json:"portVLANs"`
	// Hybrid: a port can carry an untagged VLAN and tagged VLANs together.
	Hybrid     bool `json:"hybrid"`
	VLANCreate bool `json:"vlanCreate"`
	VLANRename bool `json:"vlanRename"`
	VLANDelete bool `json:"vlanDelete"`
	// MaxAlias and MaxVLANName are the longest names the switch takes.
	MaxAlias    int `json:"maxAlias"`
	MaxVLANName int `json:"maxVLANName"`
}

// Planner turns a change into a plan for one kind of switch.
type Planner interface {
	// Driver is the monitor driver the planner belongs to (voss, …).
	Driver() string
	Capabilities() Capabilities
	// Plan reads the current values it needs through r (the channel that will
	// write) and checks c against them and against snap, the latest
	// snapshot of the switch (ports, neighbours, LAGs).
	Plan(ctx context.Context, r monitor.Reader, snap *monitor.Snapshot, c Change) (*Plan, error)
}

// Writer sets objects on a switch. Each call is one step; it must not be
// repeated automatically after an error, because the switch may have applied
// it.
type Writer interface {
	Set(ctx context.Context, as []Assignment) error
}

// Planners maps driver names to planners.
type Planners map[string]Planner

// NewPlanners builds the registry.
func NewPlanners(ps ...Planner) Planners {
	m := Planners{}
	for _, p := range ps {
		m[p.Driver()] = p
	}
	return m
}

// For returns the planner of a driver.
func (m Planners) For(driver string) (Planner, bool) {
	p, ok := m[driver]
	return p, ok
}

// Objects are the objects that are only written, never read. Everything a
// planner reads is in monitor.Objects, because the SNMP reader resolves
// names there.
var Objects = map[string]string{
	// RAPID-CITY rcVlanTable (1.3.6.1.4.1.2272.1.3.2.1.x).
	"rcVlanRowStatus": "1.3.6.1.4.1.2272.1.3.2.1.20",
	// RAPID-CITY rcSysActionL1.0: 7 saves the running configuration (VOSS,
	// what EDM's "Save configuration" sends).
	"rcSysActionL1": "1.3.6.1.4.1.2272.1.1.86",
}

// OID returns the OID of an object name (without instance).
func OID(name string) (string, bool) {
	if o, ok := Objects[name]; ok {
		return o, true
	}
	o, ok := monitor.Objects[name]
	return o, ok
}

// RowStatus values (SNMPv2-TC).
const (
	RowActive      = 1
	RowCreateAndGo = 4
	RowDestroy     = 6
)

// Int, Text and Octets build values.
func Int(n int) monitor.Value { return monitor.Value{Kind: monitor.KindInt, Int: int64(n)} }

// Text is an octet string value.
func Text(s string) monitor.Value { return monitor.Value{Kind: monitor.KindBytes, Bytes: []byte(s)} }

// Octets is an octet string value from bytes.
func Octets(b []byte) monitor.Value {
	return monitor.Value{Kind: monitor.KindBytes, Bytes: append([]byte(nil), b...)}
}

// Describe lists the steps of a plan for the audit log.
func Describe(steps []Step) string {
	var b strings.Builder
	for i, s := range steps {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(s.Description)
	}
	return b.String()
}
