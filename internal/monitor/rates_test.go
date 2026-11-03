package monitor

import (
	"testing"
	"time"
)

func snapAt(t0 time.Time, sec int, uptime time.Duration, ports ...*Port) *Snapshot {
	return &Snapshot{Time: t0.Add(time.Duration(sec) * time.Second), System: System{Uptime: uptime}, Ports: ports}
}

func TestRates(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	gig := uint64(1_000_000_000)
	prev := snapAt(t0, 0, time.Hour,
		&Port{IfIndex: 1, Speed: gig, HasHC: true, InOctets: 1_000, OutOctets: 0},
		&Port{IfIndex: 2, Speed: gig, InOctets: 4_294_967_000, OutOctets: 10},         // 32-bit, about to wrap
		&Port{IfIndex: 3, Speed: gig, HasHC: true, InOctets: 5_000_000, OutOctets: 0}, // will be reset
		&Port{IfIndex: 4, Speed: gig, HasHC: true, InOctets: 0, Discontinuity: 1},
	)
	cur := snapAt(t0, 10, time.Hour+10*time.Second,
		&Port{IfIndex: 1, Speed: gig, HasHC: true, InOctets: 1_000 + 125_000_000, OutOctets: 12_500_000}, // 100 Mbit/s in, 10 out
		&Port{IfIndex: 2, Speed: gig, InOctets: 704, OutOctets: 10},                                      // wrapped: 1000 octets
		&Port{IfIndex: 3, Speed: gig, HasHC: true, InOctets: 10, OutOctets: 0},
		&Port{IfIndex: 4, Speed: gig, HasHC: true, InOctets: 1_000_000_000, Discontinuity: 2},
	)
	Rates(prev, cur)
	if p := cur.Port(1); p.InBps != 100e6 || p.OutBps != 10e6 || p.Utilization != 10 {
		t.Errorf("port 1: %+v", p)
	}
	if p := cur.Port(2); p.InBps != 800 {
		t.Errorf("32-bit wrap: %v", p.InBps)
	}
	if p := cur.Port(3); p.InBps != 0 {
		t.Errorf("64-bit reset must not count: %v", p.InBps)
	}
	if p := cur.Port(4); p.InBps != 0 {
		t.Errorf("discontinuity must not count: %v", p.InBps)
	}

	// Reboot: uptime went down, nothing is computed.
	rebooted := snapAt(t0, 20, 5*time.Second, &Port{IfIndex: 1, Speed: gig, HasHC: true, InOctets: 2_000_000_000})
	Rates(cur, rebooted)
	if rebooted.Port(1).InBps != 0 {
		t.Error("rate after reboot")
	}
	// Implausible jump (above 1.5 × line rate) is dropped.
	jump := snapAt(t0, 30, time.Hour+30*time.Second, &Port{IfIndex: 1, Speed: gig, HasHC: true, InOctets: cur.Port(1).InOctets + 1e12})
	Rates(cur, jump)
	if jump.Port(1).InBps != 0 {
		t.Error("implausible rate accepted")
	}
}

func TestDiff(t *testing.T) {
	t0 := time.Now()
	a := snapAt(t0, 0, time.Hour, &Port{IfIndex: 1, Unit: 1, Number: 1, Physical: true, Oper: "up", Admin: "up", Speed: 1e9, PVID: 1,
		Untagged: []int{1}, PoE: &PoE{Status: "delivering"}})
	b := snapAt(t0, 60, time.Hour, &Port{IfIndex: 1, Unit: 1, Number: 1, Physical: true, Oper: "up", Admin: "up", Speed: 1e8, PVID: 10,
		Untagged: []int{10}, PoE: &PoE{Status: "searching"}})
	ev := Diff(a, b)
	kinds := map[string]Event{}
	for _, e := range ev {
		kinds[e.Kind] = e
	}
	if len(ev) != 3 || kinds["speed"].From != "1G" || kinds["speed"].To != "100M" || kinds["poe"].To != "searching" ||
		kinds["vlan"].Port != "1/1" {
		t.Fatalf("events %+v", ev)
	}
	c := snapAt(t0, 120, time.Hour, &Port{IfIndex: 1, Unit: 1, Number: 1, Physical: true, Oper: "down", Admin: "up", PVID: 10,
		Untagged: []int{10}, PoE: &PoE{Status: "searching"}})
	if ev := Diff(b, c); len(ev) != 1 || ev[0].Kind != "link" || ev[0].To != "down" {
		t.Fatalf("link down: %+v", ev)
	}
}
