package switchcfg_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/lan-dot-party/ers-proxy/internal/monitor"
	"github.com/lan-dot-party/ers-proxy/internal/monitor/poller"
	"github.com/lan-dot-party/ers-proxy/internal/store"
	"github.com/lan-dot-party/ers-proxy/internal/switchcfg"
	"github.com/lan-dot-party/ers-proxy/internal/switchcfg/rcplan"
	"github.com/lan-dot-party/ers-proxy/internal/switchcfg/switchcfgtest"
)

// fakeMonitor implements switchcfg.Monitor.
type fakeMonitor struct {
	mu       sync.Mutex
	snap     *monitor.Snapshot
	holds    int
	releases int
	polled   []int64
}

func (f *fakeMonitor) Status(id int64) (poller.Status, bool) {
	return poller.Status{DeviceID: id, Driver: "voss", Snap: f.snap}, true
}

func (f *fakeMonitor) Hold(_ context.Context, _ int64) (func(), error) {
	f.mu.Lock()
	f.holds++
	f.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			f.mu.Lock()
			f.releases++
			f.mu.Unlock()
		})
	}, nil
}

func (f *fakeMonitor) PollNow(id int64) {
	f.mu.Lock()
	f.polled = append(f.polled, id)
	f.mu.Unlock()
}

func (f *fakeMonitor) CollectNow(context.Context, *store.Device, monitor.Reader) (*monitor.Snapshot, error) {
	return f.snap, nil
}

func (f *fakeMonitor) counts() (holds, releases, polls int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.holds, f.releases, len(f.polled)
}

// unsavedPlanner wraps a planner that does not know how its switch saves.
type unsavedPlanner struct{ switchcfg.Planner }

func (p unsavedPlanner) Plan(ctx context.Context, r monitor.Reader, snap *monitor.Snapshot, c switchcfg.Change) (*switchcfg.Plan, error) {
	plan, err := p.Planner.Plan(ctx, r, snap, c)
	if err != nil {
		return nil, err
	}
	plan.SaveKnown, plan.Save = false, nil
	return plan, nil
}

type env struct {
	t       *testing.T
	db      *store.DB
	dev     *store.Device
	sw      *switchcfgtest.Switch
	mon     *fakeMonitor
	orig    map[string]map[string]monitor.Value
	mgr     *switchcfg.Manager
	clock   time.Time
	audits  []string
	who     switchcfg.Actor
	other   switchcfg.Actor
	timeout time.Duration
}

func newEnv(t *testing.T, planners switchcfg.Planners) *env {
	t.Helper()
	db, err := store.Open(store.Options{Path: filepath.Join(t.TempDir(), "t.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	d := &store.Device{Name: "vsp", Kind: store.KindVSP, URL: "https://192.0.2.1", OpenMode: store.OpenInherit,
		Isolation: store.IsolationSandbox, TLSMode: store.TLSAuto, MonitorSource: store.MonitorNone,
		MonitorDriver: store.DriverAuto, MonitorInterval: 60, SNMPVersion: store.SNMPv2c, SNMPPort: 161,
		ConfigSource: store.ConfigEDM}
	if err := db.CreateDevice(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	u1, u2 := &store.User{Username: "admin", PasswordHash: "x", Role: store.RoleAdmin},
		&store.User{Username: "other", PasswordHash: "x", Role: store.RoleAdmin}
	for _, u := range []*store.User{u1, u2} {
		if err := db.CreateUser(context.Background(), u); err != nil {
			t.Fatal(err)
		}
	}
	sw := switchcfgtest.New()
	snap := sw.VOSS()
	e := &env{t: t, db: db, dev: d, sw: sw, mon: &fakeMonitor{snap: snap}, orig: sw.Snapshot(),
		clock: time.Unix(1_700_000_000, 0), who: switchcfg.Actor{UserID: u1.ID, Name: "admin"}, other: switchcfg.Actor{UserID: u2.ID, Name: "other"}, timeout: 2 * time.Minute}
	e.mgr = e.newManager(planners)
	return e
}

func (e *env) newManager(planners switchcfg.Planners) *switchcfg.Manager {
	return &switchcfg.Manager{
		Enabled: true, Timeout: e.timeout, DB: e.db, Monitor: e.mon, Planners: planners,
		Open: func(context.Context, *store.Device) (monitor.Reader, switchcfg.Writer, func(), error) {
			return e.sw, e.sw, func() {}, nil
		},
		Audit: func(_ context.Context, action, _, _ string) { e.audits = append(e.audits, action) },
		Now:   func() time.Time { return e.clock },
	}
}

func (e *env) advance(d time.Duration) { e.clock = e.clock.Add(d) }

func (e *env) preview(c switchcfg.Change) *switchcfg.Preview {
	e.t.Helper()
	pv, err := e.mgr.Preview(context.Background(), e.dev.ID, e.who, c)
	if err != nil {
		e.t.Fatalf("Preview: %v", err)
	}
	return pv
}

func (e *env) apply(pv *switchcfg.Preview) (*store.ConfigChange, error) {
	return e.mgr.Apply(context.Background(), pv.ID, pv.Hash, false, e.who)
}

func (e *env) state(id int64) *store.ConfigChange {
	e.t.Helper()
	rec, err := e.db.GetConfigChange(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return rec
}

func (e *env) wantOriginal() {
	e.t.Helper()
	if !sameState(e.orig, e.sw.Snapshot()) {
		e.t.Fatalf("switch differs from the original:\nwant %v\ngot  %v", e.orig, e.sw.Snapshot())
	}
}

func (e *env) wantReleased() {
	e.t.Helper()
	if h, r, _ := e.mon.counts(); h != r {
		e.t.Errorf("holds = %d, releases = %d", h, r)
	}
}

func (e *env) noChangeRow() {
	e.t.Helper()
	if _, err := e.db.OpenConfigChangeForDevice(context.Background(), e.dev.ID); !errors.Is(err, store.ErrNotFound) {
		e.t.Errorf("open change: err = %v, want ErrNotFound", err)
	}
}

// sameState compares two switch states, ignoring trailing zero octets.
func sameState(a, b map[string]map[string]monitor.Value) bool {
	for _, pair := range [][2]map[string]map[string]monitor.Value{{a, b}, {b, a}} {
		for name, m := range pair[0] {
			for idx, v := range m {
				w, ok := pair[1][name][idx]
				if !ok {
					return false
				}
				if v.Kind == monitor.KindBytes {
					if !bytes.Equal(bytes.TrimRight(v.Bytes, "\x00"), bytes.TrimRight(w.Bytes, "\x00")) {
						return false
					}
				} else if v.Kind != w.Kind || v.Int != w.Int || v.Uint != w.Uint || v.OID != w.OID {
					return false
				}
			}
		}
	}
	return true
}

func vossPlanners() switchcfg.Planners { return switchcfg.NewPlanners(rcplan.VOSS()) }

func ptr[T any](v T) *T { return &v }

// moveAccess moves ifIndex 193 (untagged VLAN 1) to VLAN 10: three steps, one of them the PVID.
var moveAccess = switchcfg.Change{Ports: []switchcfg.PortChange{{IfIndex: 193, VLANs: &switchcfg.PortVLANs{Untagged: 10}}}}

// toTrunk turns ifIndex 194 (untagged VLAN 10) into a tagged port of VLANs 20 and 30.
var toTrunk = switchcfg.Change{Ports: []switchcfg.PortChange{{IfIndex: 194, VLANs: &switchcfg.PortVLANs{Tagged: []int{20, 30}}}}}

func TestApplyConfirmUnsaved(t *testing.T) {
	e := newEnv(t, switchcfg.NewPlanners(unsavedPlanner{rcplan.VOSS()}))
	ctx := context.Background()
	pv := e.preview(moveAccess)
	if e.sw.WriteCount() != 0 {
		t.Fatal("Preview wrote to the switch")
	}
	rec, err := e.apply(pv)
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != store.ChangePending || !rec.Deadline.Equal(e.clock.Add(e.timeout)) {
		t.Fatalf("state = %s, deadline = %v", rec.State, rec.Deadline)
	}
	if _, _, polls := e.mon.counts(); polls != 1 {
		t.Errorf("PollNow calls = %d", polls)
	}
	e.wantReleased()
	if sameState(e.orig, e.sw.Snapshot()) {
		t.Fatal("the switch was not changed")
	}
	got, err := e.mgr.Confirm(ctx, rec.ID, e.who)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.ChangeConfirmedUnsaved || got.Error == "" {
		t.Errorf("state = %s, error = %q", got.State, got.Error)
	}
	if st := e.state(rec.ID); st.State != store.ChangeConfirmedUnsaved {
		t.Errorf("stored state = %s", st.State)
	}
	if sameState(e.orig, e.sw.Snapshot()) {
		t.Error("a confirmed change was undone")
	}
}

func TestApplyConfirmSaved(t *testing.T) {
	e := newEnv(t, vossPlanners())
	pv := e.preview(moveAccess)
	rec, err := e.apply(pv)
	if err != nil {
		t.Fatal(err)
	}
	before := e.sw.WriteCount()
	got, err := e.mgr.Confirm(context.Background(), rec.ID, e.who)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.ChangeConfirmed || got.Error != "" {
		t.Errorf("state = %s, error = %q", got.State, got.Error)
	}
	if e.sw.WriteCount() != before+1 {
		t.Fatalf("writes = %d, want %d", e.sw.WriteCount(), before+1)
	}
	if v, ok := e.sw.Get("rcSysActionL1", ""); !ok || v.Int != 7 {
		t.Errorf("rcSysActionL1 = %v %v", v, ok)
	}
}

func TestApplyRevert(t *testing.T) {
	e := newEnv(t, vossPlanners())
	rec, err := e.apply(e.preview(moveAccess))
	if err != nil {
		t.Fatal(err)
	}
	got, err := e.mgr.Revert(context.Background(), rec.ID, e.who)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.ChangeReverted || e.state(rec.ID).State != store.ChangeReverted {
		t.Errorf("state = %s", got.State)
	}
	e.wantOriginal()
	e.wantReleased()
	e.noChangeRow()
}

func TestDeadlineSweepReverts(t *testing.T) {
	e := newEnv(t, vossPlanners())
	rec, err := e.apply(e.preview(moveAccess))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	e.advance(e.timeout - time.Second)
	e.mgr.Sweep(ctx)
	if st := e.state(rec.ID).State; st != store.ChangePending {
		t.Fatalf("reverted before the deadline: %s", st)
	}
	e.advance(2 * time.Second)
	e.mgr.Sweep(ctx)
	if st := e.state(rec.ID).State; st != store.ChangeReverted {
		t.Fatalf("state = %s", st)
	}
	e.wantOriginal()
	e.wantReleased()
	if len(e.audits) != 1 || e.audits[0] != "config.revert" {
		t.Errorf("audits = %v", e.audits)
	}
}

func TestWriteFailureMidway(t *testing.T) {
	e := newEnv(t, vossPlanners())
	pv := e.preview(toTrunk)
	if len(pv.Plan.Steps) < 2 {
		t.Fatalf("need at least 2 steps, got %d", len(pv.Plan.Steps))
	}
	e.sw.FailWrite = 2
	rec, err := e.apply(pv)
	if err == nil {
		t.Fatal("Apply succeeded")
	}
	if rec == nil || rec.State != store.ChangeFailed || rec.Error == "" {
		t.Fatalf("record = %+v", rec)
	}
	if st := e.state(rec.ID).State; st != store.ChangeFailed {
		t.Errorf("stored state = %s", st)
	}
	if e.sw.WriteCount() < 2 {
		t.Errorf("the first step was not even written (%d writes)", e.sw.WriteCount())
	}
	e.wantOriginal()
	e.wantReleased()
	e.noChangeRow()
}

func TestReadBackMismatch(t *testing.T) {
	e := newEnv(t, vossPlanners())
	pv := e.preview(moveAccess)
	e.sw.Ignore = map[string]bool{"rcVlanPortDefaultVlanId": true}
	rec, err := e.apply(pv)
	if err == nil {
		t.Fatal("Apply succeeded")
	}
	if rec == nil || rec.State != store.ChangeFailed {
		t.Fatalf("record = %+v", rec)
	}
	if v, _ := e.sw.Get("rcVlanPortDefaultVlanId", "193"); v.Int != 1 {
		t.Errorf("PVID = %d", v.Int)
	}
	e.wantOriginal()
	e.wantReleased()
}

func TestCutOffAfterFirstWrite(t *testing.T) {
	e := newEnv(t, vossPlanners())
	pv := e.preview(toTrunk)
	e.sw.UnreachableAfterWrites = 1
	rec, err := e.apply(pv)
	if err == nil {
		t.Fatal("Apply succeeded")
	}
	if rec == nil || rec.State != store.ChangeRevertFailed {
		t.Fatalf("record = %+v", rec)
	}
	e.wantReleased()
	// Still unreachable: a sweep after the backoff does not help and keeps the state.
	ctx := context.Background()
	e.advance(20 * time.Second)
	e.mgr.Sweep(ctx)
	if st := e.state(rec.ID).State; st != store.ChangeRevertFailed {
		t.Fatalf("state = %s", st)
	}
	e.sw.UnreachableAfterWrites = 0
	e.sw.SetReachable(true)
	e.advance(5 * time.Minute)
	e.mgr.Sweep(ctx)
	if st := e.state(rec.ID).State; st != store.ChangeReverted {
		t.Fatalf("state = %s", st)
	}
	e.wantOriginal()
	e.wantReleased()
	e.noChangeRow()
}

func TestConfirmWhileUnreachable(t *testing.T) {
	e := newEnv(t, vossPlanners())
	ctx := context.Background()
	rec, err := e.apply(e.preview(moveAccess))
	if err != nil {
		t.Fatal(err)
	}
	e.sw.SetReachable(false)
	if _, err := e.mgr.Confirm(ctx, rec.ID, e.who); err == nil {
		t.Fatal("Confirm succeeded on an unreachable switch")
	}
	if st := e.state(rec.ID).State; st != store.ChangePending {
		t.Fatalf("state = %s", st)
	}
	e.sw.SetReachable(true)
	e.advance(e.timeout + time.Second)
	e.mgr.Sweep(ctx)
	if st := e.state(rec.ID).State; st != store.ChangeReverted {
		t.Fatalf("state = %s", st)
	}
	e.wantOriginal()
}

func TestConfirmRevertRace(t *testing.T) {
	ctx := context.Background()
	t.Run("revert then confirm", func(t *testing.T) {
		e := newEnv(t, vossPlanners())
		rec, err := e.apply(e.preview(moveAccess))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.mgr.Revert(ctx, rec.ID, e.who); err != nil {
			t.Fatal(err)
		}
		if _, err := e.mgr.Confirm(ctx, rec.ID, e.who); !errors.Is(err, switchcfg.ErrNotPending) {
			t.Errorf("Confirm: %v", err)
		}
		if st := e.state(rec.ID).State; st != store.ChangeReverted {
			t.Errorf("state = %s", st)
		}
		e.wantOriginal()
	})
	t.Run("confirm then revert", func(t *testing.T) {
		e := newEnv(t, vossPlanners())
		rec, err := e.apply(e.preview(moveAccess))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.mgr.Confirm(ctx, rec.ID, e.who); err != nil {
			t.Fatal(err)
		}
		if _, err := e.mgr.Revert(ctx, rec.ID, e.who); !errors.Is(err, switchcfg.ErrNotPending) {
			t.Errorf("Revert: %v", err)
		}
		// The VOSS planner knows how to save (rcSysActionL1).
		if st := e.state(rec.ID).State; st != store.ChangeConfirmed {
			t.Errorf("state = %s", st)
		}
		if sameState(e.orig, e.sw.Snapshot()) {
			t.Error("a confirmed change was undone")
		}
	})
}

func TestRecoverInterruptedApply(t *testing.T) {
	e := newEnv(t, vossPlanners())
	ctx := context.Background()
	pv := e.preview(toTrunk)
	planJSON, err := json.Marshal(pv.Plan)
	if err != nil {
		t.Fatal(err)
	}
	id, err := e.db.CreateConfigChange(ctx, &store.ConfigChange{DeviceID: e.dev.ID, UserID: e.who.UserID, Actor: "admin",
		CreatedAt: e.clock, State: store.ChangeApplying, Summary: "crash", Plan: string(planJSON)})
	if err != nil {
		t.Fatal(err)
	}
	// The process died after the first step.
	if err := e.sw.Set(ctx, pv.Plan.Steps[0].Set); err != nil {
		t.Fatal(err)
	}
	if sameState(e.orig, e.sw.Snapshot()) {
		t.Fatal("the first step changed nothing")
	}
	m2 := e.newManager(vossPlanners())
	m2.Recover(ctx)
	if st := e.state(id).State; st != store.ChangeReverted {
		t.Fatalf("state = %s", st)
	}
	e.wantOriginal()
	e.wantReleased()
	if len(e.audits) != 1 || e.audits[0] != "config.revert" {
		t.Errorf("audits = %v", e.audits)
	}
}

func TestRecoverExpiredPending(t *testing.T) {
	e := newEnv(t, vossPlanners())
	ctx := context.Background()
	rec, err := e.apply(e.preview(moveAccess))
	if err != nil {
		t.Fatal(err)
	}
	m2 := e.newManager(vossPlanners())
	m2.Recover(ctx)
	if st := e.state(rec.ID).State; st != store.ChangePending {
		t.Fatalf("a pending change before its deadline was touched: %s", st)
	}
	e.advance(e.timeout + time.Second)
	m2.Recover(ctx)
	if st := e.state(rec.ID).State; st != store.ChangeReverted {
		t.Fatalf("state = %s", st)
	}
	e.wantOriginal()
}

func TestGuards(t *testing.T) {
	ctx := context.Background()

	t.Run("disabled", func(t *testing.T) {
		e := newEnv(t, vossPlanners())
		e.mgr.Enabled = false
		if _, err := e.mgr.Preview(ctx, e.dev.ID, e.who, moveAccess); !errors.Is(err, switchcfg.ErrDisabled) {
			t.Errorf("Preview: %v", err)
		}
		if _, ok := e.mgr.Capabilities(ctx, e.dev.ID); ok {
			t.Error("Capabilities available while disabled")
		}
		if e.sw.WriteCount() != 0 {
			t.Error("wrote to the switch")
		}
		e.noChangeRow()
	})

	t.Run("config source none", func(t *testing.T) {
		e := newEnv(t, vossPlanners())
		e.dev.ConfigSource = store.ConfigNone
		if err := e.db.UpdateDevice(ctx, e.dev); err != nil {
			t.Fatal(err)
		}
		if _, err := e.mgr.Preview(ctx, e.dev.ID, e.who, moveAccess); !errors.Is(err, switchcfg.ErrDisabled) {
			t.Errorf("Preview: %v", err)
		}
		e.noChangeRow()
	})

	t.Run("capabilities", func(t *testing.T) {
		e := newEnv(t, vossPlanners())
		c, ok := e.mgr.Capabilities(ctx, e.dev.ID)
		if !ok || !c.PortVLANs {
			t.Errorf("capabilities = %+v %v", c, ok)
		}
		e.mgr.Planners = switchcfg.Planners{}
		if _, ok := e.mgr.Capabilities(ctx, e.dev.ID); ok {
			t.Error("capabilities without a planner")
		}
		if _, err := e.mgr.Preview(ctx, e.dev.ID, e.who, moveAccess); !errors.Is(err, switchcfg.ErrNoPlanner) {
			t.Errorf("Preview: %v", err)
		}
	})

	t.Run("wrong hash", func(t *testing.T) {
		e := newEnv(t, vossPlanners())
		pv := e.preview(moveAccess)
		if _, err := e.mgr.Apply(ctx, pv.ID, "deadbeef", false, e.who); !errors.Is(err, switchcfg.ErrPreview) {
			t.Errorf("Apply: %v", err)
		}
		if e.sw.WriteCount() != 0 {
			t.Error("wrote to the switch")
		}
		// A wrong hash does not use up the preview.
		if _, err := e.apply(pv); err != nil {
			t.Errorf("Apply with the right hash: %v", err)
		}
	})

	t.Run("other user", func(t *testing.T) {
		e := newEnv(t, vossPlanners())
		pv := e.preview(moveAccess)
		other := e.other
		if _, ok := e.mgr.PreviewByID(pv.ID, other); ok {
			t.Error("PreviewByID returned another user's preview")
		}
		if _, err := e.mgr.Apply(ctx, pv.ID, pv.Hash, false, other); !errors.Is(err, switchcfg.ErrPreview) {
			t.Errorf("Apply: %v", err)
		}
		if _, ok := e.mgr.PreviewByID(pv.ID, e.who); !ok {
			t.Error("own preview not found")
		}
		if e.sw.WriteCount() != 0 {
			t.Error("wrote to the switch")
		}
	})

	t.Run("expired preview", func(t *testing.T) {
		e := newEnv(t, vossPlanners())
		pv := e.preview(moveAccess)
		e.advance(time.Hour)
		if _, err := e.apply(pv); !errors.Is(err, switchcfg.ErrPreview) {
			t.Errorf("Apply: %v", err)
		}
	})

	t.Run("warnings need ack", func(t *testing.T) {
		e := newEnv(t, vossPlanners())
		pv := e.preview(switchcfg.Change{Ports: []switchcfg.PortChange{{IfIndex: 192, Admin: ptr(false)}}})
		if len(pv.Plan.Warnings) == 0 {
			t.Fatal("expected a warning")
		}
		if _, err := e.apply(pv); !errors.Is(err, switchcfg.ErrAck) {
			t.Fatalf("Apply: %v", err)
		}
		if e.sw.WriteCount() != 0 {
			t.Error("wrote to the switch")
		}
		rec, err := e.mgr.Apply(ctx, pv.ID, pv.Hash, true, e.who)
		if err != nil || rec.State != store.ChangePending {
			t.Fatalf("Apply with ack: %v %+v", err, rec)
		}
	})

	t.Run("busy", func(t *testing.T) {
		e := newEnv(t, vossPlanners())
		first := e.preview(moveAccess)
		second := e.preview(toTrunk)
		if _, err := e.apply(first); err != nil {
			t.Fatal(err)
		}
		writes := e.sw.WriteCount()
		if _, err := e.mgr.Preview(ctx, e.dev.ID, e.who, toTrunk); !errors.Is(err, switchcfg.ErrBusy) {
			t.Errorf("Preview: %v", err)
		}
		if _, err := e.apply(second); !errors.Is(err, switchcfg.ErrBusy) {
			t.Errorf("Apply: %v", err)
		}
		if e.sw.WriteCount() != writes {
			t.Error("wrote to the switch while busy")
		}
	})

	t.Run("preview used up", func(t *testing.T) {
		e := newEnv(t, vossPlanners())
		pv := e.preview(moveAccess)
		rec, err := e.apply(pv)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.mgr.Revert(ctx, rec.ID, e.who); err != nil {
			t.Fatal(err)
		}
		if _, err := e.apply(pv); !errors.Is(err, switchcfg.ErrPreview) {
			t.Errorf("second Apply: %v", err)
		}
	})
}

func TestStalePreview(t *testing.T) {
	e := newEnv(t, vossPlanners())
	pv := e.preview(moveAccess)
	e.sw.Put("rcVlanPortDefaultVlanId", "193", switchcfg.Int(5))
	if _, err := e.apply(pv); !errors.Is(err, switchcfg.ErrStale) {
		t.Fatalf("Apply: %v", err)
	}
	if e.sw.WriteCount() != 0 {
		t.Error("wrote to the switch")
	}
	e.noChangeRow()
	e.wantReleased()
}

func TestPlannerRefusal(t *testing.T) {
	e := newEnv(t, vossPlanners())
	_, err := e.mgr.Preview(context.Background(), e.dev.ID, e.who, switchcfg.Change{})
	if err == nil || !switchcfg.IsRefused(err) {
		t.Fatalf("Preview: %v", err)
	}
	_, err = e.mgr.Preview(context.Background(), e.dev.ID, e.who, switchcfg.Change{
		VLANs: []switchcfg.VLANChange{{Op: switchcfg.VLANDelete, ID: 100}}})
	if !switchcfg.IsRefused(err) {
		t.Fatalf("deleting the management VLAN: %v", err)
	}
	e.noChangeRow()
}
