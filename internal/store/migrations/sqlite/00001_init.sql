-- +goose Up
CREATE TABLE users (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    username            TEXT    NOT NULL UNIQUE COLLATE NOCASE,
    password_hash       TEXT    NOT NULL,
    role                TEXT    NOT NULL,              -- user, admin
    language            TEXT    NOT NULL DEFAULT 'en',
    theme               TEXT    NOT NULL DEFAULT 'auto',
    open_mode           TEXT    NOT NULL DEFAULT '',   -- '' = global default; embedded, tab, window
    disabled            INTEGER NOT NULL DEFAULT 0,
    created_at          INTEGER NOT NULL,
    last_login          INTEGER,
    password_changed_at INTEGER NOT NULL
);

CREATE TABLE sessions (
    id         TEXT    PRIMARY KEY,  -- SHA-256 of the cookie value
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    csrf_token TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
);
CREATE INDEX sessions_expires ON sessions (expires_at);

CREATE TABLE device_groups (
    id   INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT    NOT NULL UNIQUE COLLATE NOCASE,
    sort INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE devices (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    group_id       INTEGER REFERENCES device_groups(id) ON DELETE SET NULL,
    name           TEXT    NOT NULL,
    kind           TEXT    NOT NULL,                    -- ers-edm, link
    url            TEXT    NOT NULL,                    -- ers-edm: scheme://host[:port]; link: full URL
    open_mode      TEXT    NOT NULL DEFAULT 'inherit',  -- inherit, embedded, tab, window
    isolation      TEXT    NOT NULL DEFAULT 'sandbox',  -- sandbox, off (ers-edm only)
    tls_mode       TEXT    NOT NULL DEFAULT 'auto',     -- auto, modern, legacy (https only)
    tls_pin        TEXT    NOT NULL DEFAULT '',         -- SHA-256 of the certificate, hex
    tls_profile    TEXT    NOT NULL DEFAULT '',         -- profile that worked last: modern, legacy
    credentials    BLOB,                                -- AES-GCM, bound to id|host|port
    auto_login     INTEGER NOT NULL DEFAULT 0,
    health_enabled INTEGER NOT NULL DEFAULT 1,
    notes          TEXT    NOT NULL DEFAULT '',
    sort           INTEGER NOT NULL DEFAULT 0,
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL
);
CREATE INDEX devices_group ON devices (group_id);

CREATE TABLE audit_log (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER,
    actor   TEXT    NOT NULL,
    action  TEXT    NOT NULL,
    target  TEXT    NOT NULL DEFAULT '',
    details TEXT    NOT NULL DEFAULT '',
    ip      TEXT    NOT NULL DEFAULT '',
    at      INTEGER NOT NULL
);
CREATE INDEX audit_log_at ON audit_log (at);

-- +goose Down
DROP TABLE audit_log;
DROP TABLE devices;
DROP TABLE device_groups;
DROP TABLE sessions;
DROP TABLE users;
