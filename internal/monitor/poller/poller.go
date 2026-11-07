// Package poller runs one loop per monitored device: open a reader (SNMP,
// EDM, SwOS or GWN), let the driver collect a snapshot, compute rates and events against
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
	// OpenSwOS opens the SwOS web reader for source "swos" (nil: not available).
	OpenSwOS OpenFunc
	// OpenGWN opens the Grandstream GWN web reader for source "gwn" (nil: not available).
	OpenGWN OpenFunc
	// Timeout of one poll (SNMP requests are retried inside).
	Timeout time.Duration

	mu     sync.Mutex
	loops  map[int64]*loop
	status map[int64]*Status
	wake   chan struct{}
	// gates holds one token per device: a poll and a Hold (a configuration
	// change in progress) never run at the same time.
	gates map[int64]chan struct{}
}

type loop struct {
	sig    string
	cancel context.CancelFunc
	now    chan struct{}
}

// New creates a poller.
func New(db *store.DB, key *secret.Key, reg *monitor.Registry, log *slog.Logger) *Poller {
	return &Poller{DB: db, Key: key, Registry: reg, Log: log.With("component", "monitor"), Timeout: 2 * time.Minute,
		loops: map[int64]*loop{}, status: map[int64]*Status{}, wake: make(chan struct{}, 1),
		gates: map[int64]chan struct{}{}}
}

func (p *Poller) gate(id int64) chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	g := p.gates[id]
	if g == nil {
		g = make(chan struct{}, 1)
		p.gates[id] = g
	}
	return g
}

// Hold pauses polling of a device until release is called; it waits for a
// poll in progress to end. A configuration change holds the device while it
// writes and reads back, so that no half-applied state ends up in a snapshot
// or in the history.
func (p *Poller) Hold(ctx context.Context, id int64) (release func(), err error) {
	g := p.gate(id)
	select {
	case g <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	var once sync.Once
	return func() { once.Do(func() { <-g }) }, nil
}

// PollNow polls a device as soon as possible (after a configuration change).
func (p *Poller) PollNow(id int64) {
	p.mu.Lock()
	l := p.loops[id]
	p.mu.Unlock()
	if l == nil {
		return
	}
	select {
	case l.now <- struct{}{}:
	default:
	}
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
		l := &loop{sig: signature(d), cancel: cancel, now: make(chan struct{}, 1)}
		p.loops[id] = l
		go p.run(lctx, d, l.now)
	}
}

func (p *Poller) run(ctx context.Context, d *store.Device, now <-chan struct{}) {
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
		case <-now:
		}
		g := p.gate(d.ID)
		select {
		case g <- struct{}{}:
		case <-ctx.Done():
			return
		}
		start := time.Now()
		snap, used, err := p.pollOnce(ctx, d, drv)
		<-g
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
	return p.collect(ctx, d, r, drv)
}

// CollectNow collects a snapshot of a device through r (the channel of a
// configuration change), with the device's driver.
func (p *Poller) CollectNow(ctx context.Context, d *store.Device, r monitor.Reader) (*monitor.Snapshot, error) {
	snap, _, err := p.collect(ctx, d, r, nil)
	return snap, err
}

func (p *Poller) collect(ctx context.Context, d *store.Device, r monitor.Reader, drv monitor.Driver) (*monitor.Snapshot, monitor.Driver, error) {
	var err error
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
		return OpenSNMP(ctx, d, creds)
	case store.MonitorEDM:
		if p.OpenEDM == nil {
			return nil, nil, errors.New("the EDM data source is not available")
		}
		return p.OpenEDM(ctx, d)
	case store.MonitorSwOS:
		if p.OpenSwOS == nil {
			return nil, nil, errors.New("the SwOS data source is not available")
		}
		return p.OpenSwOS(ctx, d)
	case store.MonitorGWN:
		if p.OpenGWN == nil {
			return nil, nil, errors.New("the GWN data source is not available")
		}
		return p.OpenGWN(ctx, d)
	}
	return nil, nil, fmt.Errorf("unknown source %q", d.MonitorSource)
}

// OpenSNMP opens an SNMP reader for a device with the given credentials (the
// read-only ones for polling, the write credentials for a configuration
// change).
func OpenSNMP(ctx context.Context, d *store.Device, creds snmp.Credentials) (monitor.Reader, func(), error) {
	host, _ := devices.HostPort(d.URL)
	r, err := snmpsrc.Open(ctx, snmp.Target{Host: host, Port: d.SNMPPort, Version: d.SNMPVersion, Creds: creds,
		Timeout: 5 * time.Second, Retries: 1})
	if err != nil {
		return nil, nil, err
	}
	return r, r.Close, nil
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
