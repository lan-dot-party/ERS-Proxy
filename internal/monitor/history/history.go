// Package history records port samples and events of the poller in the
// database, rolls them up to coarser resolutions and deletes old rows.
package history

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/lan-dot-party/ers-proxy/internal/config"
	"github.com/lan-dot-party/ers-proxy/internal/monitor"
	"github.com/lan-dot-party/ers-proxy/internal/monitor/poller"
	"github.com/lan-dot-party/ers-proxy/internal/store"
)

// Resolutions in seconds.
const (
	ResMinute  = 60
	ResQuarter = 900
	ResHour    = 3600
)

// MaintenanceInterval is the time between roll-up and prune runs.
const MaintenanceInterval = 5 * time.Minute

var _ poller.Sink = (*Recorder)(nil)

type portKey struct {
	device  int64
	ifIndex int
}

type errCounters struct{ in, out uint64 }

// Recorder is a poller.Sink that stores history.
type Recorder struct {
	db  *store.DB
	cfg config.Retention
	log *slog.Logger

	mu   sync.Mutex
	prev map[portKey]errCounters
	now  func() time.Time
}

// New creates a Recorder.
func New(db *store.DB, cfg config.Retention, log *slog.Logger) *Recorder {
	if log == nil {
		log = slog.Default()
	}
	return &Recorder{db: db, cfg: cfg, log: log, prev: map[portKey]errCounters{}, now: time.Now}
}

// Samples stores one 1-minute sample per physical port and LAG. Samples of
// the same minute replace each other (the last poll wins).
func (r *Recorder) Samples(ctx context.Context, deviceID int64, snap *monitor.Snapshot) error {
	ts := snap.Time.UTC().Truncate(time.Minute)
	var out []store.Sample
	r.mu.Lock()
	for _, p := range snap.Ports {
		if !p.Physical && !p.IsLAG {
			continue
		}
		k := portKey{deviceID, p.IfIndex}
		var inD, outD int64
		if prev, ok := r.prev[k]; ok {
			if p.InErrors >= prev.in {
				inD = int64(p.InErrors - prev.in)
			}
			if p.OutErrors >= prev.out {
				outD = int64(p.OutErrors - prev.out)
			}
		}
		r.prev[k] = errCounters{p.InErrors, p.OutErrors}
		up := 0.0
		if p.Oper == "up" {
			up = 1
		}
		out = append(out, store.Sample{
			DeviceID: deviceID, IfIndex: p.IfIndex, Res: ResMinute, TS: ts,
			InAvg: p.InBps, InMax: p.InBps, OutAvg: p.OutBps, OutMax: p.OutBps,
			UtilMax: p.Utilization, InErrors: inD, OutErrors: outD, UpFrac: up, N: 1,
		})
	}
	r.mu.Unlock()
	return r.db.UpsertSamples(ctx, out)
}

// Events stores port events.
func (r *Recorder) Events(ctx context.Context, deviceID int64, events []monitor.Event) error {
	evs := make([]store.PortEvent, 0, len(events))
	for _, e := range events {
		evs = append(evs, store.PortEvent{IfIndex: e.IfIndex, Port: e.Port, At: e.Time, Kind: e.Kind, From: e.From, To: e.To})
	}
	return r.db.InsertEvents(ctx, deviceID, evs)
}

// Run does the maintenance once and then every five minutes until ctx ends.
func (r *Recorder) Run(ctx context.Context) {
	r.Maintain(ctx)
	t := time.NewTicker(MaintenanceInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.Maintain(ctx)
		}
	}
}

// Maintain rolls samples up and deletes rows past their retention.
func (r *Recorder) Maintain(ctx context.Context) {
	now := r.now()
	step := func(name string, n int64, err error) {
		if err != nil {
			if ctx.Err() == nil {
				r.log.Warn("history maintenance failed", "step", name, "err", err)
			}
			return
		}
		r.log.Debug("history maintenance", "step", name, "rows", n)
	}
	n, err := r.db.RollUp(ctx, ResMinute, ResQuarter, now)
	step("rollup 60->900", n, err)
	n, err = r.db.RollUp(ctx, ResQuarter, ResHour, now)
	step("rollup 900->3600", n, err)
	n, err = r.db.PruneSamples(ctx, ResMinute, now.Add(-r.cfg.Minute))
	step("prune 60", n, err)
	n, err = r.db.PruneSamples(ctx, ResQuarter, now.Add(-r.cfg.Quarter))
	step("prune 900", n, err)
	n, err = r.db.PruneSamples(ctx, ResHour, now.Add(-r.cfg.Hour))
	step("prune 3600", n, err)
	n, err = r.db.PruneEvents(ctx, now.Add(-r.cfg.Events))
	step("prune events", n, err)
}

// Resolution picks the sample resolution for a time span.
func Resolution(span time.Duration) int {
	return resolutionFor(span, config.Retention{Minute: 48 * time.Hour, Quarter: 720 * time.Hour})
}

// resolutionFor uses the finest resolution whose retention covers the span,
// so that a chart is never empty only because roll-ups have not run yet.
func resolutionFor(span time.Duration, ret config.Retention) int {
	switch {
	case span <= ret.Minute:
		return ResMinute
	case span <= ret.Quarter:
		return ResQuarter
	default:
		return ResHour
	}
}

// Series returns the samples of one port for the last span.
func (r *Recorder) Series(ctx context.Context, deviceID int64, ifIndex int, span time.Duration) ([]store.Sample, error) {
	now := r.now()
	return r.db.QuerySamples(ctx, deviceID, ifIndex, resolutionFor(span, r.cfg), now.Add(-span), now)
}
