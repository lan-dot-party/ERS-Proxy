package history

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/lan-dot-party/ers-proxy/internal/config"
	"github.com/lan-dot-party/ers-proxy/internal/monitor"
	"github.com/lan-dot-party/ers-proxy/internal/store"
)

func setup(t *testing.T, ret config.Retention) (*Recorder, *store.DB, int64) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := store.Open(store.Options{Path: filepath.Join(t.TempDir(), "t.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	if err := db.Migrate(ctx, log); err != nil {
		t.Fatal(err)
	}
	d := &store.Device{Name: "sw", Kind: store.KindLink, URL: "http://sw.example", OpenMode: store.OpenInherit,
		Isolation: store.IsolationSandbox, TLSMode: store.TLSAuto, MonitorSource: store.MonitorNone,
		MonitorDriver: store.DriverAuto, MonitorInterval: 60, SNMPVersion: store.SNMPv2c, SNMPPort: 161}
	if err := db.CreateDevice(ctx, d); err != nil {
		t.Fatal(err)
	}
	return New(db, ret, log), db, d.ID
}

func defaults() config.Retention { return config.Default().Monitor.Retention }

func snap(ts time.Time, ports ...*monitor.Port) *monitor.Snapshot {
	return &monitor.Snapshot{Time: ts, Ports: ports}
}

func TestSamplesErrorDeltasAndFilter(t *testing.T) {
	ctx := context.Background()
	r, db, dev := setup(t, defaults())
	t0 := time.Date(2026, 3, 1, 12, 0, 20, 0, time.UTC)
	mk := func(in, out uint64, oper string) []*monitor.Port {
		return []*monitor.Port{
			{IfIndex: 1, Physical: true, Oper: oper, InBps: 1000, OutBps: 500, Utilization: 12, InErrors: in, OutErrors: out},
			{IfIndex: 100, IsLAG: true, Oper: "up", InBps: 7},
			{IfIndex: 200, Oper: "up", InBps: 9}, // VLAN interface etc.
		}
	}
	for i, c := range []struct {
		in, out uint64
		oper    string
	}{{10, 5, "up"}, {15, 5, "down"}, {2, 9, "up"}} { // third: in counter decreased
		s := snap(t0.Add(time.Duration(i)*time.Minute), mk(c.in, c.out, c.oper)...)
		if err := r.Samples(ctx, dev, s); err != nil {
			t.Fatal(err)
		}
	}
	got, err := db.QuerySamples(ctx, dev, 1, 60, t0.Add(-time.Hour), t0.Add(time.Hour))
	if err != nil || len(got) != 3 {
		t.Fatalf("%v %v", got, err)
	}
	if !got[0].TS.Equal(t0.Truncate(time.Minute)) || got[0].InErrors != 0 || got[0].OutErrors != 0 {
		t.Fatalf("first: %+v", got[0])
	}
	if got[1].InErrors != 5 || got[1].OutErrors != 0 || got[1].UpFrac != 0 {
		t.Fatalf("second: %+v", got[1])
	}
	if got[2].InErrors != 0 || got[2].OutErrors != 4 || got[2].UpFrac != 1 || got[2].N != 1 ||
		got[2].InAvg != 1000 || got[2].InMax != 1000 || got[2].OutMax != 500 || got[2].UtilMax != 12 {
		t.Fatalf("third: %+v", got[2])
	}
	if lag, _ := db.QuerySamples(ctx, dev, 100, 60, t0.Add(-time.Hour), t0.Add(time.Hour)); len(lag) != 3 {
		t.Fatalf("LAG samples: %d", len(lag))
	}
	if v, _ := db.QuerySamples(ctx, dev, 200, 60, t0.Add(-time.Hour), t0.Add(time.Hour)); len(v) != 0 {
		t.Fatalf("virtual interface recorded: %+v", v)
	}
}

func TestEvents(t *testing.T) {
	ctx := context.Background()
	r, db, dev := setup(t, defaults())
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	if err := r.Events(ctx, dev, []monitor.Event{{Time: at, IfIndex: 3, Port: "1/3", Kind: "link", From: "up", To: "down"}}); err != nil {
		t.Fatal(err)
	}
	ev, err := db.ListEvents(ctx, dev, 0, 10)
	if err != nil || len(ev) != 1 || ev[0].Port != "1/3" || ev[0].To != "down" || !ev[0].At.Equal(at) {
		t.Fatalf("%+v %v", ev, err)
	}
}

func TestMaintainRollUpAndPrune(t *testing.T) {
	ctx := context.Background()
	ret := config.Retention{Minute: 2 * time.Hour, Quarter: 24 * time.Hour, Hour: 72 * time.Hour, Events: 48 * time.Hour}
	r, db, dev := setup(t, ret)
	now := time.Date(2026, 3, 10, 12, 7, 0, 0, time.UTC)
	r.now = func() time.Time { return now }

	// 3 hours of minute samples: in = minute index, errors 1 per minute.
	start := now.Add(-3 * time.Hour).Truncate(time.Hour)
	var in []store.Sample
	for i := 0; i < 180+7; i++ {
		in = append(in, store.Sample{DeviceID: dev, IfIndex: 1, Res: 60, TS: start.Add(time.Duration(i) * time.Minute),
			InAvg: float64(i), InMax: float64(i), InErrors: 1, UpFrac: 1, N: 1})
	}
	// Old data beyond all retentions.
	in = append(in,
		store.Sample{DeviceID: dev, IfIndex: 1, Res: 900, TS: now.Add(-48 * time.Hour), N: 1},
		store.Sample{DeviceID: dev, IfIndex: 1, Res: 3600, TS: now.Add(-100 * time.Hour), N: 1})
	if err := db.UpsertSamples(ctx, in); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertEvents(ctx, dev, []store.PortEvent{
		{IfIndex: 1, Port: "1/1", At: now.Add(-72 * time.Hour), Kind: "link"},
		{IfIndex: 1, Port: "1/1", At: now.Add(-time.Hour), Kind: "link"},
	}); err != nil {
		t.Fatal(err)
	}

	r.Maintain(ctx)
	r.Maintain(ctx) // idempotent

	q, err := db.QuerySamples(ctx, dev, 1, 900, start, now)
	if err != nil {
		t.Fatal(err)
	}
	// Quarters from start to 12:00 are complete: 3h + 7 min = 12 full quarters (12:00-12:15 incomplete).
	if len(q) != 12 {
		t.Fatalf("quarters: %d", len(q))
	}
	if q[0].N != 15 || q[0].InErrors != 15 || q[0].InAvg != 7 || q[0].InMax != 14 || q[0].UpFrac != 1 {
		t.Fatalf("first quarter: %+v", q[0])
	}
	h, _ := db.QuerySamples(ctx, dev, 1, 3600, start, now)
	if len(h) != 3 || h[0].N != 60 || h[0].InErrors != 60 {
		t.Fatalf("hours: %+v", h)
	}
	// Minute rows older than 2h are pruned.
	m, _ := db.QuerySamples(ctx, dev, 1, 60, start, now)
	if len(m) == 0 || m[0].TS.Before(now.Add(-ret.Minute)) {
		t.Fatalf("minute rows not pruned: first %v", m[0].TS)
	}
	if old, _ := db.QuerySamples(ctx, dev, 1, 900, now.Add(-49*time.Hour), now.Add(-47*time.Hour)); len(old) != 0 {
		t.Fatalf("old quarter survived: %+v", old)
	}
	if old, _ := db.QuerySamples(ctx, dev, 1, 3600, now.Add(-101*time.Hour), now.Add(-99*time.Hour)); len(old) != 0 {
		t.Fatalf("old hour survived: %+v", old)
	}
	if ev, _ := db.ListEvents(ctx, dev, 0, 10); len(ev) != 1 {
		t.Fatalf("events: %+v", ev)
	}
}

func TestResolutionAndSeries(t *testing.T) {
	for span, want := range map[time.Duration]int{
		time.Hour: 60, 24 * time.Hour: 60, 48 * time.Hour: 60, 49 * time.Hour: 900, 30 * 24 * time.Hour: 900, 31 * 24 * time.Hour: 3600, 365 * 24 * time.Hour: 3600,
	} {
		if got := Resolution(span); got != want {
			t.Errorf("Resolution(%v) = %d, want %d", span, got, want)
		}
	}
	ctx := context.Background()
	r, db, dev := setup(t, defaults())
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	if err := db.UpsertSamples(ctx, []store.Sample{
		{DeviceID: dev, IfIndex: 1, Res: 60, TS: now.Add(-30 * time.Minute), N: 1},
		{DeviceID: dev, IfIndex: 1, Res: 60, TS: now.Add(-3 * time.Hour), N: 1},
		{DeviceID: dev, IfIndex: 1, Res: 900, TS: now.Add(-3 * time.Hour), N: 15},
	}); err != nil {
		t.Fatal(err)
	}
	s, err := r.Series(ctx, dev, 1, time.Hour)
	if err != nil || len(s) != 1 || s[0].Res != 60 {
		t.Fatalf("%+v %v", s, err)
	}
	// Within the minute retention (48 h) the minute samples are used, so a
	// 12 h chart is not empty before the first roll-up.
	if s, _ = r.Series(ctx, dev, 1, 12*time.Hour); len(s) != 2 || s[0].Res != 60 {
		t.Fatalf("%+v", s)
	}
	if s, _ = r.Series(ctx, dev, 1, 72*time.Hour); len(s) != 1 || s[0].Res != 900 {
		t.Fatalf("%+v", s)
	}
}
