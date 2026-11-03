package poller

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/lan-dot-party/ers-proxy/internal/devices"
	"github.com/lan-dot-party/ers-proxy/internal/monitor"
	"github.com/lan-dot-party/ers-proxy/internal/monitor/drivers"
	"github.com/lan-dot-party/ers-proxy/internal/secret"
	"github.com/lan-dot-party/ers-proxy/internal/snmp"
	"github.com/lan-dot-party/ers-proxy/internal/snmptest"
	"github.com/lan-dot-party/ers-proxy/internal/store"
)

type recSink struct{ samples, events int }

func (s *recSink) Samples(context.Context, int64, *monitor.Snapshot) error { s.samples++; return nil }
func (s *recSink) Events(context.Context, int64, []monitor.Event) error    { s.events++; return nil }

func TestPollerAgainstSimulator(t *testing.T) {
	target := snmptest.Simulator(t, snmptest.DataDir(t), "ers5600")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	db, err := store.Open(store.Options{Path: filepath.Join(dir, "t.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := db.Migrate(ctx, log); err != nil {
		t.Fatal(err)
	}
	key, _ := secret.LoadOrCreate(filepath.Join(dir, "k"))
	d := &store.Device{Name: "stack", Kind: store.KindERS, URL: "http://127.0.0.1", OpenMode: store.OpenInherit,
		Isolation: store.IsolationSandbox, TLSMode: store.TLSAuto, MonitorSource: store.MonitorSNMP,
		MonitorDriver: store.DriverAuto, MonitorInterval: 60, SNMPVersion: store.SNMPv2c, SNMPPort: target.Port}
	if err := db.CreateDevice(ctx, d); err != nil {
		t.Fatal(err)
	}
	enc, err := devices.EncryptSNMP(key, d, snmp.Credentials{Community: "ers5600"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetDeviceSNMPCredentials(ctx, d.ID, enc); err != nil {
		t.Fatal(err)
	}
	// An unmonitored device must not get a loop.
	if err := db.CreateDevice(ctx, &store.Device{Name: "link", Kind: store.KindLink, URL: "https://x.example/",
		OpenMode: store.OpenInherit, Isolation: store.IsolationSandbox, TLSMode: store.TLSAuto, MonitorSource: store.MonitorNone}); err != nil {
		t.Fatal(err)
	}

	p := New(db, key, monitor.NewRegistry(drivers.All()...), log)
	sink := &recSink{}
	p.Sink = sink
	go p.Run(ctx)
	deadline := time.Now().Add(20 * time.Second)
	var st Status
	for time.Now().Before(deadline) {
		if s, ok := p.Status(d.ID); ok && (s.Snap != nil || s.Err != "") {
			st = s
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if st.Snap == nil {
		t.Fatalf("no snapshot: %+v", st)
	}
	if st.Driver != "ers" || st.Snap.Port(129) == nil || st.Snap.Port(129).Unit != 2 {
		t.Fatalf("status %+v", st)
	}
	if len(p.All()) != 1 {
		t.Fatalf("only the monitored device should be polled: %d", len(p.All()))
	}
	// A rename shows up although the loop keeps running.
	d.Name = "renamed"
	if err := db.UpdateDevice(ctx, d); err != nil {
		t.Fatal(err)
	}
	p.Trigger()
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && p.All()[0].Name != "renamed" {
		time.Sleep(100 * time.Millisecond)
	}
	if p.All()[0].Name != "renamed" {
		t.Fatalf("dashboard keeps the old name: %q", p.All()[0].Name)
	}
	// Changing the device restarts its loop; turning monitoring off removes it.
	d.MonitorSource = store.MonitorNone
	if err := db.UpdateDevice(ctx, d); err != nil {
		t.Fatal(err)
	}
	p.Trigger()
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(p.All()) != 0 {
		time.Sleep(100 * time.Millisecond)
	}
	if len(p.All()) != 0 {
		t.Fatal("loop not stopped after monitoring was turned off")
	}
}
