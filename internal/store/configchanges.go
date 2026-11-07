package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// States of a configuration change (see docs/switch-configuration.md).
const (
	ChangeApplying         = "applying"
	ChangePending          = "pending"
	ChangeConfirmed        = "confirmed"
	ChangeConfirmedUnsaved = "confirmed_unsaved"
	ChangeReverting        = "reverting"
	ChangeReverted         = "reverted"
	ChangeRevertFailed     = "revert_failed"
	ChangeFailed           = "failed"
)

// ChangeOpen reports whether a change still needs work by the manager
// (applying, pending, reverting, revert_failed).
func ChangeOpen(state string) bool {
	switch state {
	case ChangeApplying, ChangePending, ChangeReverting, ChangeRevertFailed:
		return true
	}
	return false
}

// openStates is the SQL list of the states for which ChangeOpen is true.
const openStates = `('applying', 'pending', 'reverting', 'revert_failed')`

// ConfigChange is one change of switch settings and its progress.
type ConfigChange struct {
	ID       int64
	DeviceID int64
	UserID   int64 // 0 = unknown or user deleted
	Actor    string
	// CreatedAt defaults to now on create. Deadline is when an unconfirmed
	// change is reverted; FinishedAt is set when the change reached an end state.
	CreatedAt, Deadline, FinishedAt time.Time
	State                           string
	Summary                         string
	// Plan is the JSON description of the change (what was changed and how to revert it).
	Plan  string
	Error string
}

// ChangeUpdate lists fields TransitionConfigChange sets besides the state.
type ChangeUpdate struct {
	Deadline *time.Time
	Error    *string
	// Finished sets finished_at to now.
	Finished bool
}

// zeroUnix stores a zero time as 0.
func zeroUnix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return unix(t)
}

func fromZeroUnix(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return fromUnix(v)
}

const changeCols = `id, device_id, COALESCE(user_id, 0), actor, created_at, deadline, finished_at, state, summary, plan, error`

func scanChange(row interface{ Scan(...any) error }) (*ConfigChange, error) {
	var c ConfigChange
	var created, deadline, finished int64
	err := row.Scan(&c.ID, &c.DeviceID, &c.UserID, &c.Actor, &created, &deadline, &finished, &c.State, &c.Summary, &c.Plan, &c.Error)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	c.CreatedAt, c.Deadline, c.FinishedAt = fromUnix(created), fromZeroUnix(deadline), fromZeroUnix(finished)
	return &c, nil
}

func (db *DB) queryChanges(ctx context.Context, query string, args ...any) ([]ConfigChange, error) {
	rows, err := db.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConfigChange
	for rows.Next() {
		c, err := scanChange(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// CreateConfigChange inserts a change and sets c.ID. CreatedAt defaults to now.
func (db *DB) CreateConfigChange(ctx context.Context, c *ConfigChange) (int64, error) {
	if c.CreatedAt.IsZero() {
		c.CreatedAt = Now()
	}
	res, err := db.sql.ExecContext(ctx, `INSERT INTO config_changes
		(device_id, user_id, actor, created_at, deadline, finished_at, state, summary, plan, error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.DeviceID, nullID(c.UserID), c.Actor, unix(c.CreatedAt), zeroUnix(c.Deadline), zeroUnix(c.FinishedAt),
		c.State, c.Summary, c.Plan, c.Error)
	if err != nil {
		return 0, err
	}
	c.ID, err = res.LastInsertId()
	return c.ID, err
}

// GetConfigChange returns a change or ErrNotFound.
func (db *DB) GetConfigChange(ctx context.Context, id int64) (*ConfigChange, error) {
	return scanChange(db.sql.QueryRowContext(ctx, `SELECT `+changeCols+` FROM config_changes WHERE id = ?`, id))
}

// TransitionConfigChange moves a change from one of the states in from to the
// state to, atomically (compare-and-swap in one UPDATE ... WHERE id=? AND
// state IN (...)). It reports false when the change was not in one of those states.
func (db *DB) TransitionConfigChange(ctx context.Context, id int64, from []string, to string, u ChangeUpdate) (bool, error) {
	if len(from) == 0 {
		return false, errors.New("no source states")
	}
	sets := []string{"state = ?"}
	args := []any{to}
	if u.Deadline != nil {
		sets = append(sets, "deadline = ?")
		args = append(args, zeroUnix(*u.Deadline))
	}
	if u.Error != nil {
		sets = append(sets, "error = ?")
		args = append(args, *u.Error)
	}
	if u.Finished {
		sets = append(sets, "finished_at = ?")
		args = append(args, unix(Now()))
	}
	args = append(args, id)
	for _, s := range from {
		args = append(args, s)
	}
	// args: SET values, then id, then the allowed source states.
	res, err := db.sql.ExecContext(ctx, `UPDATE config_changes SET `+strings.Join(sets, ", ")+
		` WHERE id = ? AND state IN (?`+strings.Repeat(", ?", len(from)-1)+`)`, args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// OpenConfigChanges returns all changes in an open state, oldest first.
func (db *DB) OpenConfigChanges(ctx context.Context) ([]ConfigChange, error) {
	return db.queryChanges(ctx, `SELECT `+changeCols+` FROM config_changes WHERE state IN `+openStates+` ORDER BY created_at, id`)
}

// OpenConfigChangeForDevice returns the newest open change of a device or ErrNotFound.
func (db *DB) OpenConfigChangeForDevice(ctx context.Context, deviceID int64) (*ConfigChange, error) {
	return scanChange(db.sql.QueryRowContext(ctx, `SELECT `+changeCols+` FROM config_changes
		WHERE device_id = ? AND state IN `+openStates+` ORDER BY created_at DESC, id DESC LIMIT 1`, deviceID))
}

// ListConfigChanges returns the changes of a device, newest first.
func (db *DB) ListConfigChanges(ctx context.Context, deviceID int64, limit int) ([]ConfigChange, error) {
	if limit <= 0 {
		limit = 50
	}
	return db.queryChanges(ctx, `SELECT `+changeCols+` FROM config_changes WHERE device_id = ?
		ORDER BY created_at DESC, id DESC LIMIT ?`, deviceID, limit)
}
