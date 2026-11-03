package store

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

// openTest returns a migrated database in a temporary directory.
func openTest(t *testing.T) *DB {
	t.Helper()
	db, err := Open(Options{Path: filepath.Join(t.TempDir(), "test.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestUsersAndSessions(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	u := &User{Username: "Admin", PasswordHash: "x", Role: RoleAdmin}
	if err := db.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateUser(ctx, &User{Username: "admin", PasswordHash: "y", Role: RoleUser}); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate (case-insensitive) username: %v", err)
	}
	if err := db.CreateUser(ctx, &User{Username: "bob", PasswordHash: "y", Role: "root"}); err == nil {
		t.Fatal("invalid role accepted")
	}
	got, err := db.UserByName(ctx, "ADMIN")
	if err != nil || got.ID != u.ID || !got.Can(RoleAdmin) || !got.Can(RoleUser) {
		t.Fatalf("lookup: %+v %v", got, err)
	}

	s := &Session{ID: "h1", UserID: u.ID, CSRFToken: "c", ExpiresAt: Now().Add(time.Hour)}
	if err := db.CreateSession(ctx, s); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.GetSession(ctx, "h1"); err != nil {
		t.Fatal(err)
	}
	if !db.SessionExists(ctx, "h1") {
		t.Fatal("session missing")
	}
	// A password change ends all sessions.
	if err := db.SetPassword(ctx, u.ID, "z"); err != nil {
		t.Fatal(err)
	}
	if db.SessionExists(ctx, "h1") {
		t.Fatal("session survived a password change")
	}
	// Disabling a user ends sessions too.
	_ = db.CreateSession(ctx, &Session{ID: "h2", UserID: u.ID, CSRFToken: "c", ExpiresAt: Now().Add(time.Hour)})
	if err := db.UpdateUser(ctx, u.ID, RoleAdmin, true); err != nil {
		t.Fatal(err)
	}
	if db.SessionExists(ctx, "h2") {
		t.Fatal("session survived disabling the user")
	}
	if n, _ := db.CountAdmins(ctx); n != 0 {
		t.Fatalf("disabled admin counted: %d", n)
	}
}

func TestAuditAndBackup(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	if err := db.Audit(ctx, AuditEntry{Actor: "cli", Action: "user.create", Target: "admin"}); err != nil {
		t.Fatal(err)
	}
	entries, err := db.ListAudit(ctx, 10)
	if err != nil || len(entries) != 1 || entries[0].Action != "user.create" {
		t.Fatalf("audit: %+v %v", entries, err)
	}
	target := filepath.Join(t.TempDir(), "backup.db")
	if err := db.Backup(ctx, target); err != nil {
		t.Fatal(err)
	}
	if err := db.Backup(ctx, target); err == nil {
		t.Fatal("backup overwrote an existing file")
	}
	v, err := db.SchemaVersion(ctx)
	if err != nil || v < 1 {
		t.Fatalf("schema version %d %v", v, err)
	}
}

func TestDeviceMonitoringFields(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	d := &Device{Name: "sw", Kind: KindERS, URL: "http://192.0.2.1", OpenMode: OpenInherit, Isolation: IsolationSandbox,
		TLSMode: TLSAuto, MonitorSource: MonitorSNMP, MonitorDriver: DriverGeneric, MonitorInterval: 120,
		SNMPVersion: SNMPv3, SNMPPort: 1161, SNMPCredentials: []byte("enc")}
	if err := db.CreateDevice(ctx, d); err != nil {
		t.Fatal(err)
	}
	got, err := db.DeviceByID(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.MonitorSource != MonitorSNMP || got.MonitorDriver != DriverGeneric || got.MonitorInterval != 120 ||
		got.SNMPVersion != SNMPv3 || got.SNMPPort != 1161 || !got.HasSNMPCredentials() {
		t.Fatalf("stored fields: %+v", got)
	}
	// UpdateDevice saves the monitoring fields but leaves the credentials alone.
	got.MonitorSource, got.MonitorInterval, got.SNMPPort = MonitorNone, 300, 161
	got.SNMPCredentials = nil
	if err := db.UpdateDevice(ctx, got); err != nil {
		t.Fatal(err)
	}
	got, _ = db.DeviceByID(ctx, d.ID)
	if got.MonitorSource != MonitorNone || got.MonitorInterval != 300 || got.SNMPPort != 161 || !got.HasSNMPCredentials() {
		t.Fatalf("after update: %+v", got)
	}
	if err := db.SetDeviceSNMPCredentials(ctx, d.ID, nil); err != nil {
		t.Fatal(err)
	}
	got, _ = db.DeviceByID(ctx, d.ID)
	if got.HasSNMPCredentials() {
		t.Fatal("SNMP credentials not cleared")
	}
	if err := db.SetDeviceSNMPCredentials(ctx, d.ID, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if got, _ = db.DeviceByID(ctx, d.ID); !got.HasSNMPCredentials() {
		t.Fatal("SNMP credentials not stored")
	}
}
