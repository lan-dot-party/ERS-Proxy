// Package poller runs one loop per monitored device: open a reader (SNMP or
// EDM), let the driver collect a snapshot, compute rates and events against
// the previous snapshot, and hand everything to the history sink.
package poller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/lan-dot-party/ers-proxy/internal/devices"
	"github.com/lan-dot-party/ers-proxy/internal/monitor"
	"github.com/lan-dot-party/ers-proxy/internal/monitor/snmpsrc"
	"github.com/lan-dot-party/ers-proxy/internal/secret"
	"github.com/lan-dot-party/ers-proxy/internal/snmp"
	"github.com/lan-dot-party/ers-proxy/internal/store"
)

// Sink receives the results of every poll (history, events). Optional.
type Sink interface {
	// Samples is called with a snapshot that carries rates.
	Samples(ctx context.Context, deviceID int64, snap *monitor.Snapshot) error
	// Events is called with the changes since the previous poll.
	Events(ctx context.Context, deviceID int64, events []monitor.Event) error
}

// OpenFunc opens a reader for a device. close is called after the poll.
type OpenFunc func(ctx context.Context, d *store.Device) (r monitor.Reader, closeFn func(), err error)

// Status is the monitoring state of one device.
type Status struct {
	DeviceID int64
	Name     string
	Source   string
	Driver   string
	LastPoll time.Time
	Duration time.Duration
	Err      string
	ErrAt    time.Time
	// Snap is the latest successful snapshot (nil before the first one).
	Snap *monitor.Snapshot
}

// Poller supervises the device loops.
type Poller struct {
	DB       *store.DB
	Key      *secret.Key
	Registry *monitor.Registry
	Log      *slog.Logger
	Sink     Sink
	// OpenEDM opens the EDM reader for source "edm" (nil: not available).
	OpenEDM OpenFunc
	// Timeout of one poll (SNMP requests are retried inside).
	Timeout time.Duration

	mu     sync.Mutex
	loops  map[int64]*loop
	status map[int64]*Status
	wake   chan struct{}
}

type loop struct {
	sig    string
	cancel context.CancelFunc
}

// New creates a poller.
func New(db *store.DB, key *secret.Key, reg *monitor.Registry, log *slog.Logger) *Poller {
	return &Poller{DB: db, Key: key, Registry: reg, Log: log.With("component", "monitor"), Timeout: 2 * time.Minute,
		loops: map[int64]*loop{}, status: map[int64]*Status{}, wake: make(chan struct{}, 1)}
}

// Trigger re-reads the device list soon (after a device changed).
func (p *Poller) Trigger() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// Run supervises the loops until ctx ends.
func (p *Poller) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		p.sync(ctx)
		select {
		case <-ctx.Done():
			p.mu.Lock()
			for _, l := range p.loops {
				l.cancel()
			}
			p.mu.Unlock()
			return
		case <-t.C:
		case <-p.wake:
		}
	}
}

// signature changes whenever anything that affects polling changes.
func signature(d *store.Device) string {
	h := sha256.Sum256(d.SNMPCredentials)
	return fmt.Sprintf("%s|%s|%d|%s|%s|%d|%s|%d", d.MonitorSource, d.MonitorDriver, d.MonitorInterval, d.URL, d.SNMPVersion,
		d.SNMPPort, hex.EncodeToString(h[:8]), len(d.Credentials))
}

func (p *Poller) sync(ctx context.Context) {
	devs, err := p.DB.ListDevices(ctx)
	if err != nil {
		p.Log.Warn("list devices", "err", err)
		return
	}
	want := map[int64]*store.Device{}
	for _, d := range devs {
		if d.MonitorSource != "" && d.MonitorSource != store.MonitorNone {
			want[d.ID] = d
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, l := range p.loops {
		if d, ok := want[id]; !ok || signature(d) != l.sig {
			l.cancel()
			delete(p.loops, id)
			if !ok {
				delete(p.status, id)
			}
		}
	}
	for id, d := range want {
		if p.status[id] == nil {
			p.status[id] = &Status{DeviceID: id}
		}
		// A rename does not change the signature: the loop keeps running, but
		// the dashboard shows the new name.
		p.status[id].Name, p.status[id].Source = d.Name, d.MonitorSource
		if _, running := p.loops[id]; running {
			continue
		}
		lctx, cancel := context.WithCancel(ctx)
		p.loops[id] = &loop{sig: signature(d), cancel: cancel}
		go p.run(lctx, d)
	}
}

func (p *Poller) run(ctx context.Context, d *store.Device) {
	interval := time.Duration(d.MonitorInterval) * time.Second
	if interval < 30*time.Second {
		interval = 60 * time.Second
	}
	var prev *monitor.Snapshot
	var drv monitor.Driver
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		start := time.Now()
		snap, used, err := p.pollOnce(ctx, d, drv)
		if ctx.Err() != nil {
			return
		}
		p.mu.Lock()
		st := p.status[d.ID]
		if st == nil {
			st = &Status{DeviceID: d.ID, Name: d.Name, Source: d.MonitorSource}
			p.status[d.ID] = st
		}
		st.LastPoll, st.Duration = start, time.Since(start)
		if err != nil {
			st.Err, st.ErrAt = err.Error(), time.Now()
		} else {
			st.Err = ""
		}
		p.mu.Unlock()
		if err != nil {
			p.Log.Warn("poll failed", "device", d.Name, "err", err)
		} else {
			drv = used
			monitor.Rates(prev, snap)
			events := monitor.Diff(prev, snap)
			if p.Sink != nil {
				if len(events) > 0 {
					if err := p.Sink.Events(ctx, d.ID, events); err != nil {
						p.Log.Warn("store events", "device", d.Name, "err", err)
					}
				}
				if prev != nil {
					if err := p.Sink.Samples(ctx, d.ID, snap); err != nil {
						p.Log.Warn("store samples", "device", d.Name, "err", err)
					}
				}
			}
			prev = snap
			p.mu.Lock()
			st.Snap, st.Driver = snap, used.Name()
			p.mu.Unlock()
		}
		// A slow agent may need longer than the interval: keep a pause of at
		// least a quarter interval instead of polling back to back.
		next := interval - time.Since(start)
		if next < interval/4 {
			next = interval / 4
		}
		t.Reset(next)
	}
}

// pollOnce opens a reader, picks the driver (detected once, unless fixed for
// the device) and collects a snapshot.
func (p *Poller) pollOnce(ctx context.Context, d *store.Device, drv monitor.Driver) (*monitor.Snapshot, monitor.Driver, error) {
	ctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()
	r, closeFn, err := p.open(ctx, d)
	if err != nil {
		return nil, nil, err
	}
	defer closeFn()
	if drv == nil {
		if d.MonitorDriver != "" && d.MonitorDriver != store.DriverAuto {
			drv, err = p.Registry.Get(d.MonitorDriver)
		} else {
			var sys monitor.System
			if sys, err = monitor.ReadSystem(ctx, r); err == nil {
				drv, err = p.Registry.Detect(sys)
			}
		}
		if err != nil {
			return nil, nil, err
		}
	}
	snap, err := drv.Collect(ctx, r)
	if err != nil {
		return nil, nil, err
	}
	snap.Driver = drv.Name()
	return snap, drv, nil
}

func (p *Poller) open(ctx context.Context, d *store.Device) (monitor.Reader, func(), error) {
	switch d.MonitorSource {
	case store.MonitorSNMP:
		if !d.HasSNMPCredentials() {
			return nil, nil, errors.New("no SNMP credentials stored")
		}
		creds, err := devices.DecryptSNMP(p.Key, d)
		if err != nil {
			return nil, nil, err
		}
		host, _ := devices.HostPort(d.URL)
		r, err := snmpsrc.Open(ctx, snmp.Target{Host: host, Port: d.SNMPPort, Version: d.SNMPVersion, Creds: creds,
			Timeout: 5 * time.Second, Retries: 1})
		if err != nil {
			return nil, nil, err
		}
		return r, r.Close, nil
	case store.MonitorEDM:
		if p.OpenEDM == nil {
			return nil, nil, errors.New("the EDM data source is not available")
		}
		return p.OpenEDM(ctx, d)
	}
	return nil, nil, fmt.Errorf("unknown source %q", d.MonitorSource)
}

// Status returns a copy of a device's monitoring state.
func (p *Poller) Status(id int64) (Status, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st, ok := p.status[id]
	if !ok {
		return Status{}, false
	}
	return *st, true
}

// All returns the states of all monitored devices, ordered by name.
func (p *Poller) All() []Status {
	p.mu.Lock()
	out := make([]Status, 0, len(p.status))
	for _, st := range p.status {
		out = append(out, *st)
	}
	p.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
