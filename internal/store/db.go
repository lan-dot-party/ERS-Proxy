// Package store persists ERS-Proxy data in SQLite (pure Go, no CGO).
// SQLite runs in WAL mode; Backup creates consistent copies while running.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

//go:embed migrations/sqlite/*.sql
var sqliteMigrations embed.FS

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// DB is the database handle.
type DB struct {
	sql *sql.DB
}

// Options for Open.
type Options struct {
	// Path is the SQLite database file; its directory is created if missing.
	Path string
}

// Open connects to the database. It does not run migrations.
func Open(opts Options) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(opts.Path), 0o750); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	dsn := "file:" + opts.Path +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)" +
		"&_pragma=synchronous(NORMAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	db.SetConnMaxIdleTime(5 * time.Minute)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", opts.Path, err)
	}
	return &DB{sql: db}, nil
}

// Close closes the database.
func (db *DB) Close() error { return db.sql.Close() }

// Migrate applies all pending migrations.
func (db *DB) Migrate(ctx context.Context, log *slog.Logger) error {
	sub, err := fs.Sub(sqliteMigrations, "migrations/sqlite")
	if err != nil {
		return err
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db.sql, sub)
	if err != nil {
		return err
	}
	results, err := provider.Up(ctx)
	for _, r := range results {
		log.Info("migration applied", "version", r.Source.Version, "file", filepath.Base(r.Source.Path), "duration", r.Duration)
	}
	return err
}

// SchemaVersion returns the current migration version.
func (db *DB) SchemaVersion(ctx context.Context) (int64, error) {
	sub, err := fs.Sub(sqliteMigrations, "migrations/sqlite")
	if err != nil {
		return 0, err
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db.sql, sub)
	if err != nil {
		return 0, err
	}
	return provider.GetDBVersion(ctx)
}

// Backup writes a consistent copy of the database to path while it is in use.
func (db *DB) Backup(ctx context.Context, path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("backup target %s already exists", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	_, err := db.sql.ExecContext(ctx, "VACUUM INTO ?", path)
	return err
}

// Ping checks the connection (used by /health).
func (db *DB) Ping(ctx context.Context) error { return db.sql.PingContext(ctx) }

// Tx is a write transaction.
type Tx struct {
	tx *sql.Tx
}

// InTx runs fn in a transaction and commits if fn returns nil.
func (db *DB) InTx(ctx context.Context, fn func(*Tx) error) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(&Tx{tx: tx}); err != nil {
		_ = tx.Rollback() // the error of fn is the one that matters
		return err
	}
	return tx.Commit()
}

// unix converts a time to the stored representation.
func unix(t time.Time) int64 { return t.UTC().Unix() }

// fromUnix converts a stored timestamp.
func fromUnix(v int64) time.Time { return time.Unix(v, 0).UTC() }

func fromNullUnix(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := fromUnix(v.Int64)
	return &t
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Now returns the current time truncated to seconds, in UTC.
func Now() time.Time { return time.Now().UTC().Truncate(time.Second) }
