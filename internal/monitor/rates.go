package monitor

import (
	"fmt"
	"math"
	"reflect"
	"time"
)

// Rates fills InBps, OutBps and Utilization of cur from the counters of prev.
// A delta is only used when it is trustworthy: same boot (uptime grew), no
// counter discontinuity, and a counter that did not go backwards (64-bit) or
// wrapped plausibly once (32-bit). Otherwise the rate stays 0 for this round:
// a reset must never produce a spike.
func Rates(prev, cur *Snapshot) {
	if prev == nil || cur == nil {
		return
	}
	dt := cur.Time.Sub(prev.Time).Seconds()
	if dt <= 0 || cur.System.Uptime < prev.System.Uptime {
		return // clock problem or the device rebooted
	}
	for _, p := range cur.Ports {
		q := prev.Port(p.IfIndex)
		if q == nil || q.HasHC != p.HasHC || q.Discontinuity != p.Discontinuity {
			continue
		}
		in, okIn := delta(q.InOctets, p.InOctets, p.HasHC, p.Speed, dt)
		out, okOut := delta(q.OutOctets, p.OutOctets, p.HasHC, p.Speed, dt)
		if okIn {
			p.InBps = float64(in) * 8 / dt
		}
		if okOut {
			p.OutBps = float64(out) * 8 / dt
		}
		if p.Speed > 0 {
			p.Utilization = math.Min(100, math.Max(p.InBps, p.OutBps)/float64(p.Speed)*100)
		}
	}
}

// delta returns cur-prev of an octet counter, handling one wrap of a 32-bit
// counter. A result above 1.5 × line rate (or 100 Gbit/s if the speed is
// unknown) is rejected as implausible.
func delta(prev, cur uint64, hc bool, speed uint64, dt float64) (uint64, bool) {
	var d uint64
	switch {
	case cur >= prev:
		d = cur - prev
	case hc:
		return 0, false // 64-bit counters do not wrap in practice: reset
	default:
		d = cur + (1 << 32) - prev
	}
	limit := float64(speed)
	if limit == 0 {
		limit = 100e9
	}
	if float64(d)*8/dt > limit*1.5 {
		return 0, false
	}
	return d, true
}

// Event is a change of a port between two snapshots.
type Event struct {
	Time    time.Time
	IfIndex int
	Port    string // e.g. "1/12" or the interface name
	Kind    string // link, admin, speed, vlan, poe
	From    string
	To      string
}

// Label is the display name of a port: unit/number for front-panel ports.
func (p *Port) Label() string {
	if p.Number > 0 {
		return fmt.Sprintf("%d/%d", p.Unit, p.Number)
	}
	return p.Name
}

// Diff lists the changes between two snapshots of the same device.
func Diff(prev, cur *Snapshot) []Event {
	if prev == nil || cur == nil {
		return nil
	}
	var out []Event
	add := func(p *Port, kind, from, to string) {
		out = append(out, Event{Time: cur.Time, IfIndex: p.IfIndex, Port: p.Label(), Kind: kind, From: from, To: to})
	}
	for _, p := range cur.Ports {
		q := prev.Port(p.IfIndex)
		if q == nil || !p.Physical {
			continue
		}
		if q.Oper != p.Oper {
			add(p, "link", q.Oper, p.Oper)
		}
		if q.Admin != p.Admin {
			add(p, "admin", q.Admin, p.Admin)
		}
		if q.Oper == "up" && p.Oper == "up" && q.Speed != p.Speed {
			add(p, "speed", speedText(q.Speed), speedText(p.Speed))
		}
		if q.PVID != p.PVID || !reflect.DeepEqual(q.Untagged, p.Untagged) || !reflect.DeepEqual(q.Tagged, p.Tagged) {
			add(p, "vlan", vlanText(q), vlanText(p))
		}
		if poeText(q) != poeText(p) {
			add(p, "poe", poeText(q), poeText(p))
		}
	}
	return out
}

func speedText(bps uint64) string {
	switch {
	case bps >= 1e9 && bps%1e9 == 0:
		return fmt.Sprintf("%dG", bps/1e9)
	case bps >= 1e6:
		return fmt.Sprintf("%dM", bps/1e6)
	}
	return fmt.Sprintf("%d", bps)
}

// SpeedText formats a link speed (1G, 100M, …).
func SpeedText(bps uint64) string { return speedText(bps) }

func vlanText(p *Port) string {
	return fmt.Sprintf("pvid %d untagged %v tagged %v", p.PVID, p.Untagged, p.Tagged)
}

func poeText(p *Port) string {
	if p.PoE == nil {
		return ""
	}
	return p.PoE.Status
}
