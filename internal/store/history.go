package store

import (
	"context"
	"errors"
	"time"
)

var errInvalidRes = errors.New("roll-up: target resolution must be a multiple of the source resolution")

// Sample is one bucket of port history. Res is the bucket size in seconds
// (60, 900 or 3600); TS is the bucket start.
type Sample struct {
	DeviceID  int64
	IfIndex   int
	Res       int
	TS        time.Time
	InAvg     float64 // bits per second
	InMax     float64
	OutAvg    float64
	OutMax    float64
	UtilMax   float64 // percent
	InErrors  int64
	OutErrors int64
	UpFrac    float64 // share of samples with link up, 0..1
	N         int     // raw samples aggregated
}

// PortEvent is a stored change of a port.
type PortEvent struct {
	ID       int64
	DeviceID int64
	IfIndex  int
	Port     string
	At       time.Time
	Kind     string
	From     string
	To       string
}

const sampleColumns = `device_id, if_index, res, ts, in_avg, in_max, out_avg, out_max, util_max, in_errors, out_errors, up_frac, n`

// UpsertSamples writes samples in one transaction, replacing existing buckets.
func (db *DB) UpsertSamples(ctx context.Context, samples []Sample) error {
	if len(samples) == 0 {
		return nil
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	stmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO port_samples (`+sampleColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, s := range samples {
		if _, err := stmt.ExecContext(ctx, s.DeviceID, s.IfIndex, s.Res, unix(s.TS), s.InAvg, s.InMax, s.OutAvg, s.OutMax,
			s.UtilMax, s.InErrors, s.OutErrors, s.UpFrac, s.N); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RollUp aggregates every complete toRes bucket that ends at or before
// before from the fromRes rows into toRes rows (averages and up_frac
// weighted by n, maxima as maxima, errors summed). It replaces the whole
// bucket, so it can run repeatedly. It returns the number of rows written.
func (db *DB) RollUp(ctx context.Context, fromRes, toRes int, before time.Time) (int64, error) {
	if fromRes <= 0 || toRes <= fromRes || toRes%fromRes != 0 {
		return 0, errInvalidRes
	}
	limit := unix(before)
	limit -= limit % int64(toRes) // only buckets that end before "before"
	res, err := db.sql.ExecContext(ctx, `INSERT OR REPLACE INTO port_samples (`+sampleColumns+`)
		SELECT device_id, if_index, ?1, (ts / ?1) * ?1,
			SUM(in_avg * n) / SUM(n), MAX(in_max), SUM(out_avg * n) / SUM(n), MAX(out_max), MAX(util_max),
			SUM(in_errors), SUM(out_errors), SUM(up_frac * n) / SUM(n), SUM(n)
		FROM port_samples
		WHERE res = ?2 AND ts < ?3 AND n > 0
		GROUP BY device_id, if_index, ts / ?1`, toRes, fromRes, limit)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PruneSamples deletes rows of a resolution whose bucket starts before before.
func (db *DB) PruneSamples(ctx context.Context, res int, before time.Time) (int64, error) {
	r, err := db.sql.ExecContext(ctx, `DELETE FROM port_samples WHERE res = ? AND ts < ?`, res, unix(before))
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

// QuerySamples returns the buckets of one port with from <= ts <= to, oldest first.
func (db *DB) QuerySamples(ctx context.Context, deviceID int64, ifIndex, res int, from, to time.Time) ([]Sample, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT `+sampleColumns+` FROM port_samples
		WHERE device_id = ? AND if_index = ? AND res = ? AND ts >= ? AND ts <= ? ORDER BY ts`,
		deviceID, ifIndex, res, unix(from), unix(to))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Sample
	for rows.Next() {
		var s Sample
		var ts int64
		if err := rows.Scan(&s.DeviceID, &s.IfIndex, &s.Res, &ts, &s.InAvg, &s.InMax, &s.OutAvg, &s.OutMax,
			&s.UtilMax, &s.InErrors, &s.OutErrors, &s.UpFrac, &s.N); err != nil {
			return nil, err
		}
		s.TS = fromUnix(ts)
		out = append(out, s)
	}
	return out, rows.Err()
}

// InsertEvents stores port events of a device in one transaction.
func (db *DB) InsertEvents(ctx context.Context, deviceID int64, events []PortEvent) error {
	if len(events) == 0 {
		return nil
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	for _, e := range events {
		if _, err := tx.ExecContext(ctx, `INSERT INTO port_events (device_id, if_index, port, at, kind, from_value, to_value)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, deviceID, e.IfIndex, e.Port, unix(e.At), e.Kind, e.From, e.To); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListEvents returns the newest events of a device (of one port unless
// ifIndex is 0), newest first.
func (db *DB) ListEvents(ctx context.Context, deviceID int64, ifIndex, limit int) ([]PortEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.sql.QueryContext(ctx, `SELECT id, device_id, if_index, port, at, kind, from_value, to_value
		FROM port_events WHERE device_id = ? AND (? = 0 OR if_index = ?)
		ORDER BY at DESC, id DESC LIMIT ?`, deviceID, ifIndex, ifIndex, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PortEvent
	for rows.Next() {
		var e PortEvent
		var at int64
		if err := rows.Scan(&e.ID, &e.DeviceID, &e.IfIndex, &e.Port, &at, &e.Kind, &e.From, &e.To); err != nil {
			return nil, err
		}
		e.At = fromUnix(at)
		out = append(out, e)
	}
	return out, rows.Err()
}

// PruneEvents deletes events older than before.
func (db *DB) PruneEvents(ctx context.Context, before time.Time) (int64, error) {
	r, err := db.sql.ExecContext(ctx, `DELETE FROM port_events WHERE at < ?`, unix(before))
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}
