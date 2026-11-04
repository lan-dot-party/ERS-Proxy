package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Device kinds.
const (
	// KindERS is an Avaya/Extreme ERS switch whose EDM is served through the proxy.
	KindERS = "ers-edm"
	// KindVSP is a VSP switch with VOSS whose EDM is served through the proxy
	// (embedded with auto-login, like an ERS).
	KindVSP = "vsp-edm"
	// KindLink is any other device, opened directly by its URL.
	KindLink = "link"
	// KindEDMDirect is a switch with an EDM that works in current browsers
	// (VSP with VOSS): the browser opens it directly like a link, while the
	// server still talks to its EDM for the dashboard's EDM data source.
	KindEDMDirect = "edm-direct"
)

// Isolation modes for proxied devices.
const (
	IsolationSandbox = "sandbox"
	IsolationOff     = "off"
)

// TLS modes for HTTPS connections to a switch.
const (
	TLSAuto   = "auto"
	TLSModern = "modern"
	TLSLegacy = "legacy"
)

// Monitoring data sources.
const (
	MonitorNone = "none"
	MonitorSNMP = "snmp"
	MonitorEDM  = "edm"
)

// Monitoring drivers.
const (
	DriverAuto    = "auto"
	DriverGeneric = "generic"
	DriverERS     = "ers"
	DriverCisco   = "cisco"
	DriverTPLink  = "tplink"
	DriverVOSS    = "voss"
)

// SNMP versions.
const (
	SNMPv2c = "v2c"
	SNMPv3  = "v3"
)

// OpenInherit means: use the user's or the global default open mode.
const OpenInherit = "inherit"

// Group is a folder in the sidebar.
type Group struct {
	ID   int64
	Name string
	Sort int
}

// Device is a managed device.
type Device struct {
	ID      int64
	GroupID int64 // 0 = no group
	// GroupName is filled by list queries.
	GroupName string
	Name      string
	Kind      string
	// URL is scheme://host[:port] for ers-edm, a full URL for link devices.
	URL       string
	OpenMode  string
	Isolation string
	TLSMode   string
	// TLSPin is the hex SHA-256 of the switch certificate (empty = not pinned yet).
	TLSPin string
	// TLSProfile is the TLS profile that worked last (modern or legacy).
	TLSProfile string
	// Credentials is the encrypted login for auto-login (nil = none stored).
	Credentials   []byte
	AutoLogin     bool
	HealthEnabled bool
	Notes         string
	Sort          int
	// MonitorSource is none, snmp or edm; MonitorDriver auto, generic, ers, cisco or tplink.
	MonitorSource   string
	MonitorDriver   string
	MonitorInterval int // seconds
	SNMPVersion     string
	SNMPPort        int
	// SNMPCredentials is the encrypted community or v3 login (nil = none stored).
	SNMPCredentials []byte
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// HasCredentials reports whether a login is stored.
func (d *Device) HasCredentials() bool { return len(d.Credentials) > 0 }

// HasSNMPCredentials reports whether SNMP credentials are stored.
func (d *Device) HasSNMPCredentials() bool { return len(d.SNMPCredentials) > 0 }

// Proxied reports whether the device is served through the EDM proxy.
func (d *Device) Proxied() bool { return d.Kind == KindERS || d.Kind == KindVSP }

// VOSS reports whether the device runs VOSS (vsp-edm and edm-direct): its
// web server resets connections it is asked to close.
func (d *Device) VOSS() bool { return d.Kind == KindVSP || d.Kind == KindEDMDirect }

// Direct reports whether the browser opens the device by its URL (link and
// edm-direct devices).
func (d *Device) Direct() bool { return d.Kind == KindLink || d.Kind == KindEDMDirect }

// EDM reports whether the server talks to the device's EDM: TLS settings,
// certificate pin, stored login and the EDM data source apply (ers-edm,
// vsp-edm and edm-direct devices).
func (d *Device) EDM() bool { return d.Proxied() || d.Kind == KindEDMDirect }

const deviceCols = `d.id, COALESCE(d.group_id, 0), COALESCE(g.name, ''), d.name, d.kind, d.url, d.open_mode, d.isolation,
	d.tls_mode, d.tls_pin, d.tls_profile, d.credentials, d.auto_login, d.health_enabled, d.notes, d.sort,
	d.monitor_source, d.monitor_driver, d.monitor_interval, d.snmp_version, d.snmp_port, d.snmp_credentials,
	d.created_at, d.updated_at`

const deviceFrom = ` FROM devices d LEFT JOIN device_groups g ON g.id = d.group_id`

func scanDevice(row interface{ Scan(...any) error }) (*Device, error) {
	var d Device
	var created, updated int64
	err := row.Scan(&d.ID, &d.GroupID, &d.GroupName, &d.Name, &d.Kind, &d.URL, &d.OpenMode, &d.Isolation,
		&d.TLSMode, &d.TLSPin, &d.TLSProfile, &d.Credentials, &d.AutoLogin, &d.HealthEnabled, &d.Notes, &d.Sort,
		&d.MonitorSource, &d.MonitorDriver, &d.MonitorInterval, &d.SNMPVersion, &d.SNMPPort, &d.SNMPCredentials,
		&created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	d.CreatedAt, d.UpdatedAt = fromUnix(created), fromUnix(updated)
	return &d, nil
}

func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// CreateDevice inserts a device. Validation happens in internal/devices.
func (db *DB) CreateDevice(ctx context.Context, d *Device) error {
	d.CreatedAt = Now()
	d.UpdatedAt = d.CreatedAt
	res, err := db.sql.ExecContext(ctx, `INSERT INTO devices (group_id, name, kind, url, open_mode, isolation, tls_mode,
		tls_pin, tls_profile, credentials, auto_login, health_enabled, notes, sort, monitor_source, monitor_driver, monitor_interval, snmp_version, snmp_port,
		snmp_credentials, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		nullID(d.GroupID), d.Name, d.Kind, d.URL, d.OpenMode, d.Isolation, d.TLSMode, d.TLSPin, d.TLSProfile,
		d.Credentials, boolInt(d.AutoLogin), boolInt(d.HealthEnabled), d.Notes, d.Sort,
		d.MonitorSource, d.MonitorDriver, d.MonitorInterval, d.SNMPVersion, d.SNMPPort, d.SNMPCredentials, unix(d.CreatedAt), unix(d.UpdatedAt))
	if err != nil {
		return err
	}
	d.ID, err = res.LastInsertId()
	return err
}

// UpdateDevice saves all editable fields except the credentials (login and
// SNMP) and TLS state, which have their own methods.
func (db *DB) UpdateDevice(ctx context.Context, d *Device) error {
	d.UpdatedAt = Now()
	res, err := db.sql.ExecContext(ctx, `UPDATE devices SET group_id = ?, name = ?, kind = ?, url = ?, open_mode = ?,
		isolation = ?, tls_mode = ?, auto_login = ?, health_enabled = ?, notes = ?, sort = ?, monitor_source = ?, monitor_driver = ?,
		monitor_interval = ?, snmp_version = ?, snmp_port = ?, updated_at = ? WHERE id = ?`,
		nullID(d.GroupID), d.Name, d.Kind, d.URL, d.OpenMode, d.Isolation, d.TLSMode, boolInt(d.AutoLogin),
		boolInt(d.HealthEnabled), d.Notes, d.Sort, d.MonitorSource, d.MonitorDriver, d.MonitorInterval,
		d.SNMPVersion, d.SNMPPort, unix(d.UpdatedAt), d.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetDeviceCredentials stores (or with nil removes) the encrypted login.
func (db *DB) SetDeviceCredentials(ctx context.Context, id int64, enc []byte) error {
	// Storing a login turns auto-login on for proxied devices; removing it
	// turns it off. edm-direct devices have no auto-login (the browser logs in
	// at the switch itself).
	_, err := db.sql.ExecContext(ctx, `UPDATE devices SET credentials = ?,
		auto_login = CASE WHEN kind IN (?, ?) THEN ? ELSE 0 END, updated_at = ? WHERE id = ?`,
		enc, KindERS, KindVSP, boolInt(enc != nil), unix(Now()), id)
	return err
}

// SetDeviceSNMPCredentials stores (or with nil removes) the encrypted SNMP
// community or v3 login.
func (db *DB) SetDeviceSNMPCredentials(ctx context.Context, id int64, enc []byte) error {
	_, err := db.sql.ExecContext(ctx, `UPDATE devices SET snmp_credentials = ?, updated_at = ? WHERE id = ?`,
		enc, unix(Now()), id)
	return err
}

// SetDeviceTLS stores the pinned certificate and the profile that worked.
func (db *DB) SetDeviceTLS(ctx context.Context, id int64, pin, profile string) error {
	_, err := db.sql.ExecContext(ctx, `UPDATE devices SET tls_pin = ?, tls_profile = ? WHERE id = ?`, pin, profile, id)
	return err
}

// SetDeviceTLSProfile remembers the TLS profile that worked last.
func (db *DB) SetDeviceTLSProfile(ctx context.Context, id int64, profile string) error {
	_, err := db.sql.ExecContext(ctx, `UPDATE devices SET tls_profile = ? WHERE id = ?`, profile, id)
	return err
}

// DeleteDevice removes a device.
func (db *DB) DeleteDevice(ctx context.Context, id int64) error {
	_, err := db.sql.ExecContext(ctx, `DELETE FROM devices WHERE id = ?`, id)
	return err
}

// DeviceByID returns a device.
func (db *DB) DeviceByID(ctx context.Context, id int64) (*Device, error) {
	return scanDevice(db.sql.QueryRowContext(ctx, `SELECT `+deviceCols+deviceFrom+` WHERE d.id = ?`, id))
}

// ListDevices returns all devices ordered by group, sort and name. Devices
// without a group come last.
func (db *DB) ListDevices(ctx context.Context) ([]*Device, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT `+deviceCols+deviceFrom+`
		ORDER BY g.id IS NULL, g.sort, g.name COLLATE NOCASE, d.sort, d.name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Device
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// CreateGroup inserts a group.
func (db *DB) CreateGroup(ctx context.Context, g *Group) error {
	g.Name = strings.TrimSpace(g.Name)
	if g.Name == "" || len(g.Name) > 80 {
		return errors.New("group name must have 1 to 80 characters")
	}
	res, err := db.sql.ExecContext(ctx, `INSERT INTO device_groups (name, sort) VALUES (?, ?)`, g.Name, g.Sort)
	if isUnique(err) {
		return ErrExists
	}
	if err != nil {
		return err
	}
	g.ID, err = res.LastInsertId()
	return err
}

// UpdateGroup renames a group and changes its position.
func (db *DB) UpdateGroup(ctx context.Context, g *Group) error {
	g.Name = strings.TrimSpace(g.Name)
	if g.Name == "" || len(g.Name) > 80 {
		return errors.New("group name must have 1 to 80 characters")
	}
	_, err := db.sql.ExecContext(ctx, `UPDATE device_groups SET name = ?, sort = ? WHERE id = ?`, g.Name, g.Sort, g.ID)
	if isUnique(err) {
		return ErrExists
	}
	return err
}

// DeleteGroup removes a group; its devices move to "no group".
func (db *DB) DeleteGroup(ctx context.Context, id int64) error {
	_, err := db.sql.ExecContext(ctx, `DELETE FROM device_groups WHERE id = ?`, id)
	return err
}

// GroupByName returns a group by name (case-insensitive).
func (db *DB) GroupByName(ctx context.Context, name string) (*Group, error) {
	var g Group
	err := db.sql.QueryRowContext(ctx, `SELECT id, name, sort FROM device_groups WHERE name = ?`, strings.TrimSpace(name)).
		Scan(&g.ID, &g.Name, &g.Sort)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &g, err
}

// ListGroups returns all groups in display order.
func (db *DB) ListGroups(ctx context.Context) ([]*Group, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT id, name, sort FROM device_groups ORDER BY sort, name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Group
	for rows.Next() {
		var g Group
		if err := rows.Scan(&g.ID, &g.Name, &g.Sort); err != nil {
			return nil, err
		}
		out = append(out, &g)
	}
	return out, rows.Err()
}
