-- +goose Up
ALTER TABLE devices ADD COLUMN monitor_source    TEXT    NOT NULL DEFAULT 'none';  -- none, snmp, edm
ALTER TABLE devices ADD COLUMN monitor_driver    TEXT    NOT NULL DEFAULT 'auto';  -- auto, generic, ers, cisco, tplink
ALTER TABLE devices ADD COLUMN monitor_interval  INTEGER NOT NULL DEFAULT 60;      -- seconds
ALTER TABLE devices ADD COLUMN snmp_version      TEXT    NOT NULL DEFAULT 'v2c';   -- v2c, v3
ALTER TABLE devices ADD COLUMN snmp_port         INTEGER NOT NULL DEFAULT 161;
ALTER TABLE devices ADD COLUMN snmp_credentials  BLOB;                             -- AES-GCM, bound to id|host|snmp_port

-- +goose Down
ALTER TABLE devices DROP COLUMN snmp_credentials;
ALTER TABLE devices DROP COLUMN snmp_port;
ALTER TABLE devices DROP COLUMN snmp_version;
ALTER TABLE devices DROP COLUMN monitor_interval;
ALTER TABLE devices DROP COLUMN monitor_driver;
ALTER TABLE devices DROP COLUMN monitor_source;
