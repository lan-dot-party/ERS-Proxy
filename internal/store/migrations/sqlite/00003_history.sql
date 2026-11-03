-- +goose Up
CREATE TABLE port_samples (
    device_id  INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    if_index   INTEGER NOT NULL,
    res        INTEGER NOT NULL,  -- bucket size in seconds: 60, 900 or 3600
    ts         INTEGER NOT NULL,  -- bucket start, unix seconds UTC
    in_avg     REAL    NOT NULL,
    in_max     REAL    NOT NULL,
    out_avg    REAL    NOT NULL,
    out_max    REAL    NOT NULL,
    util_max   REAL    NOT NULL,
    in_errors  INTEGER NOT NULL,
    out_errors INTEGER NOT NULL,
    up_frac    REAL    NOT NULL,
    n          INTEGER NOT NULL,  -- raw samples aggregated
    PRIMARY KEY (device_id, if_index, res, ts)
) WITHOUT ROWID;
CREATE INDEX idx_port_samples_res_ts ON port_samples (res, ts);

CREATE TABLE port_events (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    device_id  INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    if_index   INTEGER NOT NULL,
    port       TEXT    NOT NULL,
    at         INTEGER NOT NULL,
    kind       TEXT    NOT NULL,
    from_value TEXT    NOT NULL,
    to_value   TEXT    NOT NULL
);
CREATE INDEX idx_port_events_device_at ON port_events (device_id, at);

-- +goose Down
DROP TABLE port_events;
DROP TABLE port_samples;
