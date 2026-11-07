package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMigration4Columns(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	d := &Device{Name: "sw", Kind: KindERS, URL: "192.0.2.1", OpenMode: OpenInherit, Isolation: IsolationSandbox,
		TLSMode: TLSAuto, MonitorSource: MonitorNone, MonitorDriver: DriverAuto, MonitorInterval: 60, SNMPVersion: SNMPv2c, SNMPPort: 161}
	if err := db.CreateDevice(ctx, d); err != nil {
		t.Fatal(err)
	}
	got, err := db.DeviceByID(ctx, d.ID)
	if err != nil || got.ConfigSource != ConfigNone || got.HasSNMPWriteCredentials() {
		t.Fatalf("default: %+v %v", got, err)
	}
	// Column default for rows inserted without config_source.
	if _, err := db.sql.ExecContext(ctx, `INSERT INTO devices (name, kind, url, open_mode, isolation, tls_mode, created_at, updated_at)
		VALUES ('x', 'link', 'https://x.example', 'inherit', 'sandbox', 'auto', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	var src string
	if err := db.sql.QueryRowContext(ctx, `SELECT config_source FROM devices WHERE name = 'x'`).Scan(&src); err != nil || src != "none" {
		t.Fatalf("column default = %q, %v", src, err)
	}

	got.ConfigSource = ConfigSNMP
	if err := db.UpdateDevice(ctx, got); err != nil {
		t.Fatal(err)
	}
	if err := db.SetDeviceSNMPWriteCredentials(ctx, d.ID, []byte("secret")); err != nil {
		t.Fatal(err)
	}
	got, _ = db.DeviceByID(ctx, d.ID)
	if got.ConfigSource != ConfigSNMP || string(got.SNMPWriteCredentials) != "secret" || !got.HasSNMPWriteCredentials() {
		t.Fatalf("round trip: %+v", got)
	}
	// UpdateDevice must not touch the write credentials.
	if err := db.UpdateDevice(ctx, got); err != nil {
		t.Fatal(err)
	}
	if err := db.SetDeviceSNMPWriteCredentials(ctx, d.ID, nil); err != nil {
		t.Fatal(err)
	}
	got, _ = db.DeviceByID(ctx, d.ID)
	if got.HasSNMPWriteCredentials() {
		t.Fatal("write credentials not removed")
	}
	list, err := db.ListDevices(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("list: %d %v", len(list), err)
	}
}

func TestConfigChanges(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	mk := func(name string) *Device {
		d := &Device{Name: name, Kind: KindERS, URL: name, OpenMode: OpenInherit, Isolation: IsolationSandbox,
			TLSMode: TLSAuto, MonitorSource: MonitorNone, MonitorDriver: DriverAuto, MonitorInterval: 60, SNMPVersion: SNMPv2c, SNMPPort: 161, ConfigSource: ConfigEDM}
		if err := db.CreateDevice(ctx, d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	d1, d2 := mk("a"), mk("b")

	if _, err := db.OpenConfigChangeForDevice(ctx, d1.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty: %v", err)
	}
	if _, err := db.GetConfigChange(ctx, 99); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get missing: %v", err)
	}

	dl := Now().Add(2 * time.Minute)
	c1 := &ConfigChange{DeviceID: d1.ID, Actor: "admin", State: ChangeApplying, Summary: "port 1 vlan 10", Plan: `{"a":1}`,
		CreatedAt: Now().Add(-time.Hour)}
	if id, err := db.CreateConfigChange(ctx, c1); err != nil || id == 0 || c1.ID != id {
		t.Fatalf("create: %d %v", id, err)
	}
	c2 := &ConfigChange{DeviceID: d1.ID, State: ChangeConfirmed}
	db.CreateConfigChange(ctx, c2)
	c3 := &ConfigChange{DeviceID: d2.ID, State: ChangePending, Deadline: dl}
	db.CreateConfigChange(ctx, c3)
	if c2.CreatedAt.IsZero() {
		t.Fatal("CreatedAt not defaulted")
	}

	got, err := db.GetConfigChange(ctx, c1.ID)
	if err != nil || got.Summary != "port 1 vlan 10" || got.Plan != `{"a":1}` || got.UserID != 0 ||
		!got.Deadline.IsZero() || !got.FinishedAt.IsZero() || !got.CreatedAt.Equal(c1.CreatedAt) {
		t.Fatalf("get: %+v %v", got, err)
	}
	if g, _ := db.GetConfigChange(ctx, c3.ID); !g.Deadline.Equal(dl) {
		t.Fatalf("deadline: %v", g.Deadline)
	}

	open, err := db.OpenConfigChanges(ctx)
	if err != nil || len(open) != 2 || open[0].ID != c1.ID || open[1].ID != c3.ID {
		t.Fatalf("open: %+v %v", open, err)
	}
	if o, err := db.OpenConfigChangeForDevice(ctx, d1.ID); err != nil || o.ID != c1.ID {
		t.Fatalf("open for device: %+v %v", o, err)
	}
	list, err := db.ListConfigChanges(ctx, d1.ID, 10)
	if err != nil || len(list) != 2 || list[0].ID != c2.ID {
		t.Fatalf("list: %+v %v", list, err)
	}
	if list, _ := db.ListConfigChanges(ctx, d1.ID, 1); len(list) != 1 {
		t.Fatalf("limit: %d", len(list))
	}

	for s, want := range map[string]bool{ChangeApplying: true, ChangePending: true, ChangeReverting: true, ChangeRevertFailed: true,
		ChangeConfirmed: false, ChangeConfirmedUnsaved: false, ChangeReverted: false, ChangeFailed: false} {
		if ChangeOpen(s) != want {
			t.Errorf("ChangeOpen(%s) = %v", s, !want)
		}
	}

	// Compare-and-swap.
	msg := "boom"
	ok, err := db.TransitionConfigChange(ctx, c1.ID, []string{ChangeApplying}, ChangePending, ChangeUpdate{Deadline: &dl})
	if err != nil || !ok {
		t.Fatalf("transition: %v %v", ok, err)
	}
	ok, err = db.TransitionConfigChange(ctx, c1.ID, []string{ChangeApplying}, ChangeFailed, ChangeUpdate{Error: &msg, Finished: true})
	if err != nil || ok {
		t.Fatalf("second transition from old state: %v %v", ok, err)
	}
	got, _ = db.GetConfigChange(ctx, c1.ID)
	if got.State != ChangePending || !got.Deadline.Equal(dl) || got.Error != "" || !got.FinishedAt.IsZero() {
		t.Fatalf("after: %+v", got)
	}
	ok, err = db.TransitionConfigChange(ctx, c1.ID, []string{ChangeApplying, ChangePending}, ChangeFailed, ChangeUpdate{Error: &msg, Finished: true})
	if err != nil || !ok {
		t.Fatalf("multi-state: %v %v", ok, err)
	}
	got, _ = db.GetConfigChange(ctx, c1.ID)
	if got.State != ChangeFailed || got.Error != "boom" || got.FinishedAt.IsZero() {
		t.Fatalf("after: %+v", got)
	}

	// Cascade with the device.
	if err := db.DeleteDevice(ctx, d1.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ := db.ListConfigChanges(ctx, d1.ID, 10); len(list) != 0 {
		t.Fatalf("changes survived device delete: %d", len(list))
	}
	if _, err := db.GetConfigChange(ctx, c3.ID); err != nil {
		t.Fatalf("other device's change lost: %v", err)
	}
}
