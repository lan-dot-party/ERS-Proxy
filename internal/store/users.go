package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// User roles, from least to most privileged.
const (
	// RoleUser may open devices and change their own account.
	RoleUser = "user"
	// RoleAdmin also manages devices, groups and users.
	RoleAdmin = "admin"
)

// RoleLevel orders roles for permission checks.
func RoleLevel(role string) int {
	switch role {
	case RoleUser:
		return 1
	case RoleAdmin:
		return 2
	}
	return 0
}

// ErrExists is returned when a unique name is taken.
var ErrExists = errors.New("already exists")

// User is a web user.
type User struct {
	ID           int64
	Username     string
	PasswordHash string
	Role         string
	Language     string
	// Theme is the UI color theme: auto, light or dark.
	Theme string
	// OpenMode is the user's default open mode; empty means the global default.
	OpenMode          string
	Disabled          bool
	CreatedAt         time.Time
	LastLogin         *time.Time
	PasswordChangedAt time.Time
}

// Can reports whether the user has at least the given role.
func (u *User) Can(role string) bool { return u != nil && RoleLevel(u.Role) >= RoleLevel(role) }

const userCols = `id, username, password_hash, role, language, theme, open_mode, disabled, created_at, last_login, password_changed_at`

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	var created, changed int64
	var last sql.NullInt64
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.Language, &u.Theme, &u.OpenMode, &u.Disabled,
		&created, &last, &changed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	u.CreatedAt = fromUnix(created)
	u.LastLogin = fromNullUnix(last)
	u.PasswordChangedAt = fromUnix(changed)
	return &u, nil
}

func isUnique(err error) bool { return err != nil && strings.Contains(err.Error(), "UNIQUE") }

// CreateUser inserts a user.
func (db *DB) CreateUser(ctx context.Context, u *User) error {
	if RoleLevel(u.Role) == 0 {
		return errors.New("invalid role")
	}
	u.Username = strings.TrimSpace(u.Username)
	if u.Username == "" || len(u.Username) > 64 {
		return errors.New("username must have 1 to 64 characters")
	}
	if u.Language == "" {
		u.Language = "en"
	}
	if u.Theme == "" {
		u.Theme = "auto"
	}
	u.CreatedAt = Now()
	u.PasswordChangedAt = u.CreatedAt
	res, err := db.sql.ExecContext(ctx, `INSERT INTO users (username, password_hash, role, language, theme, created_at,
		password_changed_at) VALUES (?, ?, ?, ?, ?, ?, ?)`, u.Username, u.PasswordHash, u.Role, u.Language, u.Theme,
		unix(u.CreatedAt), unix(u.PasswordChangedAt))
	if isUnique(err) {
		return ErrExists
	}
	if err != nil {
		return err
	}
	u.ID, err = res.LastInsertId()
	return err
}

// UserByName returns a user by username (case-insensitive).
func (db *DB) UserByName(ctx context.Context, name string) (*User, error) {
	return scanUser(db.sql.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE username = ?`, strings.TrimSpace(name)))
}

// UserByID returns a user.
func (db *DB) UserByID(ctx context.Context, id int64) (*User, error) {
	return scanUser(db.sql.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id))
}

// ListUsers returns all users.
func (db *DB) ListUsers(ctx context.Context) ([]*User, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT `+userCols+` FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// CountAdmins returns the number of enabled admins.
func (db *DB) CountAdmins(ctx context.Context) (int, error) {
	var n int
	err := db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role = 'admin' AND disabled = 0`).Scan(&n)
	return n, err
}

// UpdateUser saves role and disabled state. Disabling ends all sessions.
func (db *DB) UpdateUser(ctx context.Context, id int64, role string, disabled bool) error {
	if RoleLevel(role) == 0 {
		return errors.New("invalid role")
	}
	return db.InTx(ctx, func(tx *Tx) error {
		if _, err := tx.tx.ExecContext(ctx, `UPDATE users SET role = ?, disabled = ? WHERE id = ?`,
			role, boolInt(disabled), id); err != nil {
			return err
		}
		if !disabled {
			return nil
		}
		_, err := tx.tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, id)
		return err
	})
}

// SetUserPrefs changes language, theme and default open mode of a user.
func (db *DB) SetUserPrefs(ctx context.Context, id int64, language, theme, openMode string) error {
	_, err := db.sql.ExecContext(ctx, `UPDATE users SET language = ?, theme = ?, open_mode = ? WHERE id = ?`,
		language, theme, openMode, id)
	return err
}

// Themes are the valid UI color themes.
var Themes = []string{"auto", "light", "dark"}

// ValidTheme reports whether t is a known theme.
func ValidTheme(t string) bool {
	for _, v := range Themes {
		if t == v {
			return true
		}
	}
	return false
}

// SetPassword replaces the password hash and ends all sessions of the user.
func (db *DB) SetPassword(ctx context.Context, id int64, hash string) error {
	return db.InTx(ctx, func(tx *Tx) error {
		if _, err := tx.tx.ExecContext(ctx, `UPDATE users SET password_hash = ?, password_changed_at = ? WHERE id = ?`,
			hash, unix(Now()), id); err != nil {
			return err
		}
		_, err := tx.tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, id)
		return err
	})
}

// DeleteUser removes a user and their sessions.
func (db *DB) DeleteUser(ctx context.Context, id int64) error {
	_, err := db.sql.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
	return err
}

// TouchLogin records a successful login.
func (db *DB) TouchLogin(ctx context.Context, id int64) error {
	_, err := db.sql.ExecContext(ctx, `UPDATE users SET last_login = ? WHERE id = ?`, unix(Now()), id)
	return err
}

// Session is a login session. ID is the SHA-256 hash of the cookie value.
type Session struct {
	ID        string
	UserID    int64
	CSRFToken string
	ExpiresAt time.Time
}

// CreateSession stores a session.
func (db *DB) CreateSession(ctx context.Context, s *Session) error {
	_, err := db.sql.ExecContext(ctx, `INSERT INTO sessions (id, user_id, csrf_token, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
		s.ID, s.UserID, s.CSRFToken, unix(Now()), unix(s.ExpiresAt))
	return err
}

// GetSession returns a valid session with its user.
func (db *DB) GetSession(ctx context.Context, id string) (*Session, *User, error) {
	var s Session
	var exp int64
	err := db.sql.QueryRowContext(ctx, `SELECT id, user_id, csrf_token, expires_at FROM sessions WHERE id = ? AND expires_at > ?`,
		id, unix(Now())).Scan(&s.ID, &s.UserID, &s.CSRFToken, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	s.ExpiresAt = fromUnix(exp)
	u, err := db.UserByID(ctx, s.UserID)
	if err != nil {
		return nil, nil, err
	}
	if u.Disabled {
		return nil, nil, ErrNotFound
	}
	return &s, u, nil
}

// SessionExists reports whether a session is still valid (used by the proxy
// to end proxy sessions together with the login session).
func (db *DB) SessionExists(ctx context.Context, id string) bool {
	var n int
	err := db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.id = ? AND s.expires_at > ? AND u.disabled = 0`, id, unix(Now())).Scan(&n)
	return err == nil && n == 1
}

// ExtendSession moves the expiry of a session.
func (db *DB) ExtendSession(ctx context.Context, id string, expires time.Time) error {
	_, err := db.sql.ExecContext(ctx, `UPDATE sessions SET expires_at = ? WHERE id = ?`, unix(expires), id)
	return err
}

// DeleteSession ends a session.
func (db *DB) DeleteSession(ctx context.Context, id string) error {
	_, err := db.sql.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
	return err
}

// PurgeSessions deletes expired sessions.
func (db *DB) PurgeSessions(ctx context.Context) error {
	_, err := db.sql.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, unix(Now()))
	return err
}

// AuditEntry is one line of the audit log.
type AuditEntry struct {
	ID      int64
	UserID  int64
	Actor   string
	Action  string
	Target  string
	Details string
	IP      string
	At      time.Time
}

// Audit writes an audit log entry. Never put secrets into it.
func (db *DB) Audit(ctx context.Context, e AuditEntry) error {
	var uid any
	if e.UserID != 0 {
		uid = e.UserID
	}
	_, err := db.sql.ExecContext(ctx, `INSERT INTO audit_log (user_id, actor, action, target, details, ip, at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, uid, e.Actor, e.Action, e.Target, e.Details, e.IP, unix(Now()))
	return err
}

// ListAudit returns the newest audit entries.
func (db *DB) ListAudit(ctx context.Context, limit int) ([]AuditEntry, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT id, COALESCE(user_id, 0), actor, action, target, details, ip, at
		FROM audit_log ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var at int64
		if err := rows.Scan(&e.ID, &e.UserID, &e.Actor, &e.Action, &e.Target, &e.Details, &e.IP, &at); err != nil {
			return nil, err
		}
		e.At = fromUnix(at)
		out = append(out, e)
	}
	return out, rows.Err()
}
