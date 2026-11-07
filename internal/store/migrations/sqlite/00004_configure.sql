-- +goose Up
ALTER TABLE devices ADD COLUMN config_source          TEXT NOT NULL DEFAULT 'none';  -- none, edm, snmp
ALTER TABLE devices ADD COLUMN snmp_write_credentials BLOB;                          -- AES-GCM, bound to id|host|snmp_port

CREATE TABLE config_changes (
    id          INTEGER PRIMARY KEY,
    device_id   INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    user_id     INTEGER REFERENCES users(id) ON DELETE SET NULL,
    actor       TEXT    NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,             -- Unix seconds (UTC)
    deadline    INTEGER NOT NULL DEFAULT 0,   -- Unix seconds (UTC), 0 = none
    finished_at INTEGER NOT NULL DEFAULT 0,   -- Unix seconds (UTC), 0 = not finished
    state       TEXT    NOT NULL,
    summary     TEXT    NOT NULL DEFAULT '',
    plan        TEXT    NOT NULL DEFAULT '',  -- JSON
    error       TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX config_changes_device ON config_changes(device_id, created_at);
CREATE INDEX config_changes_state ON config_changes(state);

-- +goose Down
DROP TABLE config_changes;
ALTER TABLE devices DROP COLUMN snmp_write_credentials;
ALTER TABLE devices DROP COLUMN config_source;
