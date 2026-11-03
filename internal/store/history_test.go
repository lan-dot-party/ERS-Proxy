package store

import (
	"context"
	"math"
	"testing"
	"time"
)

func newDevice(t *testing.T, db *DB, name string) int64 {
	t.Helper()
	d := &Device{Name: name, Kind: KindLink, URL: "http://" + name + ".example", OpenMode: OpenInherit,
		Isolation: IsolationSandbox, TLSMode: TLSAuto, MonitorSource: MonitorNone, MonitorDriver: DriverAuto,
		MonitorInterval: 60, SNMPVersion: SNMPv2c, SNMPPort: 161}
	if err := db.CreateDevice(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	return d.ID
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestRollUpWeights(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	dev := newDevice(t, db, "a")
	base := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	// Two minute buckets in the 10:00 quarter: n=1 and n=3 (weights 1:3).
	in := []Sample{
		{DeviceID: dev, IfIndex: 1, Res: 60, TS: base, InAvg: 100, InMax: 100, OutAvg: 10, OutMax: 20, UtilMax: 5, InErrors: 1, OutErrors: 2, UpFrac: 1, N: 1},
		{DeviceID: dev, IfIndex: 1, Res: 60, TS: base.Add(time.Minute), InAvg: 200, InMax: 400, OutAvg: 30, OutMax: 50, UtilMax: 9, InErrors: 3, OutErrors: 0, UpFrac: 0, N: 3},
		// Belongs to the next, incomplete quarter.
		{DeviceID: dev, IfIndex: 1, Res: 60, TS: base.Add(15 * time.Minute), InAvg: 1, InMax: 1, N: 1},
	}
	if err := db.UpsertSamples(ctx, in); err != nil {
		t.Fatal(err)
	}
	before := base.Add(20 * time.Minute) // 10:15 bucket ends 10:30 > before
	for i := 0; i < 2; i++ {             // idempotent
		n, err := db.RollUp(ctx, 60, 900, before)
		if err != nil || n != 1 {
			t.Fatalf("rollup %d: n=%d err=%v", i, n, err)
		}
	}
	got, err := db.QuerySamples(ctx, dev, 1, 900, base.Add(-time.Hour), base.Add(time.Hour))
	if err != nil || len(got) != 1 {
		t.Fatalf("query: %v %v", got, err)
	}
	s := got[0]
	if !s.TS.Equal(base) || s.N != 4 || !near(s.InAvg, 175) || s.InMax != 400 || !near(s.OutAvg, 25) || s.OutMax != 50 ||
		s.UtilMax != 9 || s.InErrors != 4 || s.OutErrors != 2 || !near(s.UpFrac, 0.25) {
		t.Fatalf("unexpected rollup: %+v", s)
	}
	// Once the quarter is complete, the late minute joins it (replace, not add).
	if err := db.UpsertSamples(ctx, []Sample{{DeviceID: dev, IfIndex: 1, Res: 60, TS: base.Add(14 * time.Minute), InAvg: 0, N: 4, UpFrac: 1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RollUp(ctx, 60, 900, before); err != nil {
		t.Fatal(err)
	}
	got, _ = db.QuerySamples(ctx, dev, 1, 900, base, base)
	if len(got) != 1 || got[0].N != 8 || !near(got[0].InAvg, 87.5) {
		t.Fatalf("second rollup: %+v", got)
	}
	// Hour roll-up from the quarter rows.
	if n, err := db.RollUp(ctx, 900, 3600, base.Add(2*time.Hour)); err != nil || n != 1 {
		t.Fatalf("hour: %d %v", n, err)
	}
	if _, err := db.RollUp(ctx, 900, 1000, before); err == nil {
		t.Fatal("invalid resolution accepted")
	}
}

func TestSamplePruneAndCascade(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	a, b := newDevice(t, db, "a"), newDevice(t, db, "b")
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for _, dev := range []int64{a, b} {
		if err := db.UpsertSamples(ctx, []Sample{
			{DeviceID: dev, IfIndex: 1, Res: 60, TS: now.Add(-3 * time.Hour), N: 1},
			{DeviceID: dev, IfIndex: 1, Res: 60, TS: now, N: 1},
		}); err != nil {
			t.Fatal(err)
		}
		if err := db.InsertEvents(ctx, dev, []PortEvent{
			{IfIndex: 1, Port: "1/1", At: now.Add(-2 * time.Hour), Kind: "link", From: "up", To: "down"},
			{IfIndex: 2, Port: "1/2", At: now, Kind: "link", From: "down", To: "up"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := db.PruneSamples(ctx, 60, now.Add(-time.Hour)); err != nil || n != 2 {
		t.Fatalf("prune: %d %v", n, err)
	}
	if n, err := db.PruneSamples(ctx, 900, now); err != nil || n != 0 {
		t.Fatalf("prune other res: %d %v", n, err)
	}
	ev, err := db.ListEvents(ctx, a, 0, 10)
	if err != nil || len(ev) != 2 || ev[0].IfIndex != 2 || ev[1].Kind != "link" || ev[1].From != "up" || !ev[0].At.Equal(now) {
		t.Fatalf("events: %+v %v", ev, err)
	}
	if ev, _ = db.ListEvents(ctx, a, 1, 10); len(ev) != 1 || ev[0].Port != "1/1" {
		t.Fatalf("filtered events: %+v", ev)
	}
	if ev, _ = db.ListEvents(ctx, a, 0, 1); len(ev) != 1 {
		t.Fatalf("limit: %+v", ev)
	}
	if n, err := db.PruneEvents(ctx, now.Add(-time.Hour)); err != nil || n != 2 {
		t.Fatalf("prune events: %d %v", n, err)
	}

	// Deleting a device removes its history, not the other device's.
	if err := db.DeleteDevice(ctx, a); err != nil {
		t.Fatal(err)
	}
	if s, _ := db.QuerySamples(ctx, a, 1, 60, now.Add(-24*time.Hour), now); len(s) != 0 {
		t.Fatalf("samples survived device delete: %+v", s)
	}
	if ev, _ := db.ListEvents(ctx, a, 0, 10); len(ev) != 0 {
		t.Fatalf("events survived device delete: %+v", ev)
	}
	if s, _ := db.QuerySamples(ctx, b, 1, 60, now.Add(-24*time.Hour), now); len(s) != 1 {
		t.Fatalf("other device lost samples: %+v", s)
	}
}
