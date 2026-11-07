package switchcfg

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/lan-dot-party/ers-proxy/internal/monitor"
	"github.com/lan-dot-party/ers-proxy/internal/monitor/poller"
	"github.com/lan-dot-party/ers-proxy/internal/store"
)

// Errors of the manager. Planner refusals are *Refused.
var (
	// ErrDisabled: configure.enabled is off, or the device has no
	// configuration channel.
	ErrDisabled = errors.New("changing switch settings is not enabled")
	// ErrBusy: the device has a change that is not finished.
	ErrBusy = errors.New("the switch has a change that is not confirmed or reverted yet")
	// ErrPreview: the preview is unknown, expired, of another user, or its
	// hash does not match.
	ErrPreview = errors.New("the preview has expired or does not match; preview the change again")
	// ErrAck: the plan has warnings that were not acknowledged.
	ErrAck = errors.New("the warnings of this change must be acknowledged")
	// ErrStale: the switch changed since the preview.
	ErrStale = errors.New("the switch changed since the preview; preview the change again")
	// ErrNotPending: the change is no longer waiting for confirmation.
	ErrNotPending = errors.New("the change is no longer waiting for confirmation")
	// ErrNoPlanner: no planner for the switch's driver.
	ErrNoPlanner = errors.New("changing settings is not supported for this switch")
)

// Store is the part of the database the manager uses (*store.DB).
type Store interface {
	DeviceByID(ctx context.Context, id int64) (*store.Device, error)
	CreateConfigChange(ctx context.Context, c *store.ConfigChange) (int64, error)
	GetConfigChange(ctx context.Context, id int64) (*store.ConfigChange, error)
	TransitionConfigChange(ctx context.Context, id int64, from []string, to string, u store.ChangeUpdate) (bool, error)
	OpenConfigChanges(ctx context.Context) ([]store.ConfigChange, error)
	OpenConfigChangeForDevice(ctx context.Context, deviceID int64) (*store.ConfigChange, error)
}

// Monitor is the part of the poller the manager uses (*poller.Poller).
type Monitor interface {
	Status(id int64) (poller.Status, bool)
	Hold(ctx context.Context, id int64) (release func(), err error)
	PollNow(id int64)
	CollectNow(ctx context.Context, d *store.Device, r monitor.Reader) (*monitor.Snapshot, error)
}

// OpenFunc opens the configuration channel of a device (its config_source):
// a reader and a writer over the same connection or session. close is
// called when the operation ends.
type OpenFunc func(ctx context.Context, d *store.Device) (r monitor.Reader, w Writer, closeFn func(), err error)

// Actor is the user behind an operation.
type Actor struct {
	UserID int64
	Name   string
}

// Preview is a planned change waiting to be applied.
type Preview struct {
	ID       string
	DeviceID int64
	UserID   int64
	Change   Change
	Plan     *Plan
	Hash     string
	Created  time.Time
}

// Manager applies changes "commit confirmed". See the package doc and
// docs/switch-configuration.md.
type Manager struct {
	// Enabled mirrors configure.enabled; nothing is written when it is false.
	Enabled bool
	// Timeout is how long a change waits for confirmation.
	Timeout  time.Duration
	DB       Store
	Monitor  Monitor
	Open     OpenFunc
	Planners Planners
	Log      *slog.Logger
	// Audit records automatic actions (revert on timeout, recovery). Actions
	// of users are audited by the caller. Optional.
	Audit func(ctx context.Context, action, target, details string)
	// Tick is the interval of the background loop (default 5 s).
	Tick time.Duration
	// Now returns the current time (tests).
	Now func() time.Time

	mu       sync.Mutex
	locks    map[int64]*sync.Mutex
	previews map[string]*Preview
	retry    map[int64]retryState
}

type retryState struct {
	next  time.Time
	delay time.Duration
}

// previewTTL is how long a preview can be applied.
const previewTTL = 15 * time.Minute

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) log() *slog.Logger {
	if m.Log != nil {
		return m.Log
	}
	return slog.Default()
}

func (m *Manager) timeout() time.Duration {
	if m.Timeout > 0 {
		return m.Timeout
	}
	return 2 * time.Minute
}

func (m *Manager) lock(id int64) func() {
	m.mu.Lock()
	if m.locks == nil {
		m.locks = map[int64]*sync.Mutex{}
	}
	l := m.locks[id]
	if l == nil {
		l = &sync.Mutex{}
		m.locks[id] = l
	}
	m.mu.Unlock()
	l.Lock()
	return l.Unlock
}

func (m *Manager) audit(ctx context.Context, action, target, details string) {
	if m.Audit != nil {
		m.Audit(ctx, action, target, details)
	}
}

// device loads a device and checks that changes are allowed on it.
func (m *Manager) device(ctx context.Context, id int64) (*store.Device, error) {
	if !m.Enabled {
		return nil, ErrDisabled
	}
	d, err := m.DB.DeviceByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if d.ConfigSource == "" || d.ConfigSource == store.ConfigNone {
		return nil, ErrDisabled
	}
	return d, nil
}

// Capabilities returns what can be changed on a device (ok = false if
// nothing: the feature is off, the device has no channel, or its driver no
// planner).
func (m *Manager) Capabilities(ctx context.Context, id int64) (Capabilities, bool) {
	d, err := m.device(ctx, id)
	if err != nil {
		return Capabilities{}, false
	}
	st, ok := m.Monitor.Status(d.ID)
	if !ok || st.Driver == "" {
		return Capabilities{}, false
	}
	p, ok := m.Planners.For(st.Driver)
	if !ok {
		return Capabilities{}, false
	}
	return p.Capabilities(), true
}

// Preview plans a change without writing anything.
func (m *Manager) Preview(ctx context.Context, deviceID int64, who Actor, c Change) (*Preview, error) {
	d, err := m.device(ctx, deviceID)
	if err != nil {
		return nil, err
	}
	if open, err := m.DB.OpenConfigChangeForDevice(ctx, d.ID); err == nil && open != nil {
		return nil, ErrBusy
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	r, _, closeFn, err := m.Open(ctx, d)
	if err != nil {
		return nil, fmt.Errorf("connect to the switch: %w", err)
	}
	defer closeFn()
	var snap *monitor.Snapshot
	if st, ok := m.Monitor.Status(d.ID); ok && st.Snap != nil {
		snap = st.Snap
	} else if snap, err = m.Monitor.CollectNow(ctx, d, r); err != nil {
		return nil, fmt.Errorf("read the switch: %w", err)
	}
	p, ok := m.Planners.For(snap.Driver)
	if !ok {
		return nil, ErrNoPlanner
	}
	plan, err := p.Plan(ctx, r, snap, c)
	if err != nil {
		return nil, err
	}
	pv := &Preview{ID: token(), DeviceID: d.ID, UserID: who.UserID, Change: c, Plan: plan, Hash: plan.Hash(), Created: m.now()}
	m.mu.Lock()
	if m.previews == nil {
		m.previews = map[string]*Preview{}
	}
	for id, old := range m.previews {
		if m.now().Sub(old.Created) > previewTTL {
			delete(m.previews, id)
		}
	}
	m.previews[pv.ID] = pv
	m.mu.Unlock()
	return pv, nil
}

// PreviewByID returns a preview of a user.
func (m *Manager) PreviewByID(id string, who Actor) (*Preview, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	pv := m.previews[id]
	if pv == nil || pv.UserID != who.UserID || m.now().Sub(pv.Created) > previewTTL {
		return nil, false
	}
	return pv, true
}

func token() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Apply writes a previewed change and leaves it pending. hash must be the
// preview's hash; ack must be set when the plan has warnings.
func (m *Manager) Apply(ctx context.Context, previewID, hash string, ack bool, who Actor) (*store.ConfigChange, error) {
	pv, ok := m.PreviewByID(previewID, who)
	if !ok || pv.Hash != hash {
		return nil, ErrPreview
	}
	if len(pv.Plan.Warnings) > 0 && !ack {
		return nil, ErrAck
	}
	d, err := m.device(ctx, pv.DeviceID)
	if err != nil {
		return nil, err
	}
	unlock := m.lock(d.ID)
	defer unlock()
	if open, err := m.DB.OpenConfigChangeForDevice(ctx, d.ID); err == nil && open != nil {
		return nil, ErrBusy
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	// The preview is used up whatever happens next.
	m.mu.Lock()
	delete(m.previews, previewID)
	m.mu.Unlock()

	release, err := m.Monitor.Hold(ctx, d.ID)
	if err != nil {
		return nil, err
	}
	defer release()
	r, w, closeFn, err := m.Open(ctx, d)
	if err != nil {
		return nil, fmt.Errorf("connect to the switch: %w", err)
	}
	defer closeFn()
	if ms, err := Verify(ctx, r, pv.Plan.Before); err != nil {
		return nil, fmt.Errorf("read the switch: %w", err)
	} else if len(ms) > 0 {
		return nil, fmt.Errorf("%w (%s)", ErrStale, ms[0])
	}

	planJSON, err := json.Marshal(pv.Plan)
	if err != nil {
		return nil, err
	}
	rec := &store.ConfigChange{DeviceID: d.ID, UserID: who.UserID, Actor: who.Name, CreatedAt: m.now(),
		State: store.ChangeApplying, Summary: summary(pv.Plan), Plan: string(planJSON)}
	// Stored before the first write: after a crash, recovery reverts it.
	if rec.ID, err = m.DB.CreateConfigChange(ctx, rec); err != nil {
		return nil, fmt.Errorf("store the change: %w", err)
	}

	fail := func(cause error) (*store.ConfigChange, error) {
		if !m.move(ctx, rec.ID, []string{store.ChangeApplying}, store.ChangeReverting, nil, "") {
			return nil, cause
		}
		if rerr := m.revertWith(ctx, r, w, pv.Plan); rerr != nil {
			m.move(ctx, rec.ID, []string{store.ChangeReverting}, store.ChangeRevertFailed, nil,
				cause.Error()+"; revert: "+rerr.Error())
			m.scheduleRetry(rec.ID)
			return m.reload(ctx, rec.ID), fmt.Errorf("%v; the revert failed too: %w", cause, rerr)
		}
		m.move(ctx, rec.ID, []string{store.ChangeReverting}, store.ChangeFailed, nil, cause.Error())
		return m.reload(ctx, rec.ID), cause
	}
	for _, s := range pv.Plan.Steps {
		if err := w.Set(ctx, s.Set); err != nil {
			return fail(fmt.Errorf("%s: %w", s.Description, err))
		}
	}
	ms, err := Verify(ctx, r, pv.Plan.Expected)
	if err != nil {
		return fail(fmt.Errorf("read back: %w", err))
	}
	if len(ms) > 0 {
		return fail(MismatchError(ms))
	}
	deadline := m.now().Add(m.timeout())
	if !m.move(ctx, rec.ID, []string{store.ChangeApplying}, store.ChangePending, &deadline, "") {
		return nil, fmt.Errorf("the change %d left the state applying", rec.ID)
	}
	m.Monitor.PollNow(d.ID)
	return m.reload(ctx, rec.ID), nil
}

// Confirm keeps a pending change and saves the configuration. It first
// reads the switch: a switch that cannot be reached is not confirmed (the
// change will be reverted at its deadline).
func (m *Manager) Confirm(ctx context.Context, id int64, who Actor) (*store.ConfigChange, error) {
	rec, err := m.DB.GetConfigChange(ctx, id)
	if err != nil {
		return nil, err
	}
	d, err := m.device(ctx, rec.DeviceID)
	if err != nil {
		return nil, err
	}
	unlock := m.lock(d.ID)
	defer unlock()
	plan, err := decodePlan(rec)
	if err != nil {
		return nil, err
	}
	if cur, err := m.DB.GetConfigChange(ctx, id); err != nil {
		return nil, err
	} else if cur.State != store.ChangePending {
		return nil, ErrNotPending
	}
	r, w, closeFn, err := m.Open(ctx, d)
	if err != nil {
		return nil, fmt.Errorf("connect to the switch: %w", err)
	}
	defer closeFn()
	ms, err := Verify(ctx, r, plan.Expected)
	if err != nil {
		return nil, fmt.Errorf("the switch cannot be read, so the change cannot be confirmed: %w", err)
	}
	if len(ms) > 0 {
		return nil, fmt.Errorf("the switch no longer has the change, so it cannot be confirmed: %w", MismatchError(ms))
	}
	// The compare-and-swap disarms the deadline: a revert that runs at the
	// same moment finds the change no longer pending.
	if !m.move(ctx, id, []string{store.ChangePending}, store.ChangeConfirmed, nil, "") {
		return nil, ErrNotPending
	}
	if !plan.SaveKnown {
		m.move(ctx, id, []string{store.ChangeConfirmed}, store.ChangeConfirmedUnsaved, nil,
			"ERS-Proxy cannot save the configuration of this switch yet; save it on the switch")
		return m.reload(ctx, id), nil
	}
	for _, s := range plan.Save {
		if err := w.Set(ctx, s.Set); err != nil {
			m.move(ctx, id, []string{store.ChangeConfirmed}, store.ChangeConfirmedUnsaved, nil, "save: "+err.Error())
			return m.reload(ctx, id), fmt.Errorf("the change is kept but was not saved: %w", err)
		}
	}
	return m.reload(ctx, id), nil
}

// Revert undoes a pending change now.
func (m *Manager) Revert(ctx context.Context, id int64, who Actor) (*store.ConfigChange, error) {
	rec, err := m.DB.GetConfigChange(ctx, id)
	if err != nil {
		return nil, err
	}
	if _, err := m.device(ctx, rec.DeviceID); err != nil {
		return nil, err
	}
	return m.revert(ctx, rec, []string{store.ChangePending})
}

// revert moves a change from one of the states in from to reverting and
// writes its inverse.
func (m *Manager) revert(ctx context.Context, rec *store.ConfigChange, from []string) (*store.ConfigChange, error) {
	unlock := m.lock(rec.DeviceID)
	defer unlock()
	plan, err := decodePlan(rec)
	if err != nil {
		return nil, err
	}
	if !m.move(ctx, rec.ID, from, store.ChangeReverting, nil, "") {
		return nil, ErrNotPending
	}
	d, err := m.DB.DeviceByID(ctx, rec.DeviceID)
	if err == nil {
		err = m.revertDevice(ctx, d, plan)
	}
	if err != nil {
		m.move(ctx, rec.ID, []string{store.ChangeReverting}, store.ChangeRevertFailed, nil, err.Error())
		m.scheduleRetry(rec.ID)
		return m.reload(ctx, rec.ID), err
	}
	m.clearRetry(rec.ID)
	m.move(ctx, rec.ID, []string{store.ChangeReverting}, store.ChangeReverted, nil, "")
	m.Monitor.PollNow(rec.DeviceID)
	return m.reload(ctx, rec.ID), nil
}

func (m *Manager) revertDevice(ctx context.Context, d *store.Device, plan *Plan) error {
	release, err := m.Monitor.Hold(ctx, d.ID)
	if err != nil {
		return err
	}
	defer release()
	r, w, closeFn, err := m.Open(ctx, d)
	if err != nil {
		return fmt.Errorf("connect to the switch: %w", err)
	}
	defer closeFn()
	return m.revertWith(ctx, r, w, plan)
}

// revertWith writes every inverse step, even after an error (the values are
// absolute, and a step may fail only because the change was applied in
// part), then checks that the switch is as before.
func (m *Manager) revertWith(ctx context.Context, r monitor.Reader, w Writer, plan *Plan) error {
	var firstErr error
	for _, s := range plan.Inverse {
		if err := w.Set(ctx, s.Set); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("%s: %w", s.Description, err)
		}
	}
	ms, err := Verify(ctx, r, plan.Before)
	if err != nil {
		if firstErr != nil {
			return fmt.Errorf("%v; read back: %w", firstErr, err)
		}
		return fmt.Errorf("read back: %w", err)
	}
	if len(ms) > 0 {
		if firstErr != nil {
			return fmt.Errorf("%v; %w", firstErr, MismatchError(ms))
		}
		return MismatchError(ms)
	}
	return nil
}

// Run reverts changes whose deadline passed and retries failed reverts until
// ctx ends. Changes left open by a previous run (a crash or restart while
// applying, pending or reverting) are handled first.
func (m *Manager) Run(ctx context.Context) {
	tick := m.Tick
	if tick <= 0 {
		tick = 5 * time.Second
	}
	m.Recover(ctx)
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.Sweep(ctx)
		}
	}
}

// Recover handles the changes a previous run left open: changes interrupted
// while applying or reverting are reverted, expired pending ones too. Run
// calls it first.
func (m *Manager) Recover(ctx context.Context) {
	open, err := m.DB.OpenConfigChanges(ctx)
	if err != nil {
		m.log().Warn("list open configuration changes", "err", err)
		return
	}
	for i := range open {
		rec := &open[i]
		switch rec.State {
		case store.ChangeApplying, store.ChangeReverting:
			// Interrupted while writing: restore the values from before.
			m.log().Warn("reverting a configuration change interrupted by a restart", "change", rec.ID, "state", rec.State)
			m.autoRevert(ctx, rec, []string{rec.State}, "interrupted by a restart")
		}
	}
	m.Sweep(ctx)
}

// Sweep reverts expired pending changes and retries failed reverts once
// their backoff has passed. Run calls it periodically.
func (m *Manager) Sweep(ctx context.Context) {
	open, err := m.DB.OpenConfigChanges(ctx)
	if err != nil {
		m.log().Warn("list open configuration changes", "err", err)
		return
	}
	now := m.now()
	for i := range open {
		rec := &open[i]
		switch rec.State {
		case store.ChangePending:
			if !rec.Deadline.IsZero() && !now.Before(rec.Deadline) {
				m.autoRevert(ctx, rec, []string{store.ChangePending}, "not confirmed in time")
			}
		case store.ChangeRevertFailed:
			if m.retryDue(rec.ID, now) {
				m.autoRevert(ctx, rec, []string{store.ChangeRevertFailed}, "retry")
			}
		}
	}
}

func (m *Manager) autoRevert(ctx context.Context, rec *store.ConfigChange, from []string, why string) {
	d, _ := m.DB.DeviceByID(ctx, rec.DeviceID)
	target := fmt.Sprintf("device %d", rec.DeviceID)
	if d != nil {
		target = d.Name
	}
	_, err := m.revert(ctx, rec, from)
	switch {
	case errors.Is(err, ErrNotPending):
		return // confirmed or reverted meanwhile
	case err != nil:
		m.log().Warn("configuration revert failed", "change", rec.ID, "device", target, "err", err)
		if rec.State != store.ChangeRevertFailed {
			m.audit(ctx, "config.revert_failed", target, fmt.Sprintf("change %d (%s): %v", rec.ID, why, err))
		}
	default:
		m.log().Info("configuration change reverted", "change", rec.ID, "device", target, "reason", why)
		m.audit(ctx, "config.revert", target, fmt.Sprintf("change %d reverted automatically (%s): %s", rec.ID, why, rec.Summary))
	}
}

func (m *Manager) scheduleRetry(id int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.retry == nil {
		m.retry = map[int64]retryState{}
	}
	rs := m.retry[id]
	if rs.delay == 0 {
		rs.delay = 15 * time.Second
	} else if rs.delay *= 2; rs.delay > 5*time.Minute {
		rs.delay = 5 * time.Minute
	}
	rs.next = m.now().Add(rs.delay)
	m.retry[id] = rs
}

func (m *Manager) clearRetry(id int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.retry, id)
}

func (m *Manager) retryDue(id int64, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	rs, ok := m.retry[id]
	return !ok || !now.Before(rs.next)
}

// move changes the state of a change; it reports false when the change was
// not in one of the states in from.
func (m *Manager) move(ctx context.Context, id int64, from []string, to string, deadline *time.Time, errText string) bool {
	u := store.ChangeUpdate{Deadline: deadline}
	if errText != "" {
		u.Error = &errText
	}
	switch to {
	case store.ChangeConfirmedUnsaved, store.ChangeReverted, store.ChangeFailed, store.ChangeConfirmed:
		u.Finished = true
	}
	// The state is changed even if the caller's request was canceled: a
	// write already happened.
	ok, err := m.DB.TransitionConfigChange(context.WithoutCancel(ctx), id, from, to, u)
	if err != nil {
		m.log().Error("store configuration change state", "change", id, "state", to, "err", err)
		return false
	}
	return ok
}

func (m *Manager) reload(ctx context.Context, id int64) *store.ConfigChange {
	rec, err := m.DB.GetConfigChange(context.WithoutCancel(ctx), id)
	if err != nil {
		m.log().Error("load configuration change", "change", id, "err", err)
		return nil
	}
	return rec
}

func decodePlan(rec *store.ConfigChange) (*Plan, error) {
	var p Plan
	if err := json.Unmarshal([]byte(rec.Plan), &p); err != nil {
		return nil, fmt.Errorf("change %d: stored plan: %w", rec.ID, err)
	}
	return &p, nil
}

// PlanOf decodes the plan stored with a change.
func PlanOf(rec *store.ConfigChange) (*Plan, error) { return decodePlan(rec) }

func summary(p *Plan) string {
	s := ""
	for i, line := range p.Summary {
		if i > 0 {
			s += "; "
		}
		s += line
	}
	return s
}
