package devices

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lan-dot-party/ers-proxy/internal/secret"
	"github.com/lan-dot-party/ers-proxy/internal/snmp"
	"github.com/lan-dot-party/ers-proxy/internal/store"
)

func TestNormalize(t *testing.T) {
	cases := []struct {
		kind, in, want string
		ok             bool
	}{
		{store.KindERS, "192.0.2.10", "http://192.0.2.10", true},
		{store.KindERS, "HTTPS://Switch1.example:8443/", "https://switch1.example:8443", true},
		{store.KindERS, "http://192.0.2.10/login.html", "", false},
		{store.KindERS, "ftp://192.0.2.10", "", false},
		{store.KindERS, "http://admin:pw@192.0.2.10", "", false},
		{store.KindERS, "http://192.0.2.10:99999", "", false},
		{store.KindERS, "http://[2001:db8::1]:8080", "http://[2001:db8::1]:8080", true},
		{store.KindLink, "https://fw.example/ui/?x=1", "https://fw.example/ui/?x=1", true},
		{store.KindLink, "https://ap.example", "https://ap.example/", true},
		{"router", "https://ap.example", "", false},
	}
	for _, c := range cases {
		d := &store.Device{Name: "  sw  ", Kind: c.kind, URL: c.in}
		err := Normalize(d)
		if (err == nil) != c.ok {
			t.Errorf("%s %q: err=%v, want ok=%v", c.kind, c.in, err, c.ok)
			continue
		}
		if c.ok && (d.URL != c.want || d.Name != "sw" || d.OpenMode != store.OpenInherit) {
			t.Errorf("%s %q: got %q name %q mode %q", c.kind, c.in, d.URL, d.Name, d.OpenMode)
		}
	}
	var fe *FieldError
	if err := Normalize(&store.Device{Name: "x", Kind: store.KindERS, URL: "a", TLSPin: "zz"}); !errors.As(err, &fe) || fe.Key != "tls_pin" {
		t.Fatalf("pin validation: %v", err)
	}
	d := &store.Device{Name: "x", Kind: store.KindERS, URL: "https://a", TLSPin: strings.Repeat("AB:", 31) + "AB"}
	if err := Normalize(d); err != nil || d.TLSPin != strings.Repeat("ab", 32) {
		t.Fatalf("pin with colons: %q %v", d.TLSPin, err)
	}
}

func TestCredentialsBoundToAddress(t *testing.T) {
	k, err := secret.LoadOrCreate(filepath.Join(t.TempDir(), "k"))
	if err != nil {
		t.Fatal(err)
	}
	d := &store.Device{ID: 7, URL: "https://192.0.2.10"}
	enc, err := EncryptCredentials(k, d, Credentials{Username: "RW", Password: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	d.Credentials = enc
	c, err := DecryptCredentials(k, d)
	if err != nil || c.Password != "secret" {
		t.Fatalf("decrypt: %+v %v", c, err)
	}
	moved := *d
	moved.URL = "https://198.51.100.9"
	if _, err := DecryptCredentials(k, &moved); err == nil {
		t.Fatal("credentials usable after the host changed")
	}
	if !AddressChanged(d, &moved) {
		t.Fatal("address change not detected")
	}
	same := *d
	same.URL = "https://192.0.2.10:443"
	if AddressChanged(d, &same) {
		t.Fatal("explicit default port counted as a change")
	}
	scheme := *d
	scheme.URL = "http://192.0.2.10:443"
	if !AddressChanged(d, &scheme) {
		t.Fatal("scheme change not detected")
	}
}

func TestImportExport(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(store.Options{Path: filepath.Join(t.TempDir(), "t.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	csvIn := "\ufeffname,group,url,kind\nCore,Building A,192.0.2.1,\nFirewall,,https://fw.example,link\nbad,,ftp://x,\n"
	e, err := ReadCSV(strings.NewReader(csvIn))
	if err != nil {
		t.Fatal(err)
	}
	res, err := Import(ctx, db, e)
	if err != nil {
		t.Fatal(err)
	}
	if res.Created != 2 || res.GroupsCreated != 1 || len(res.Errors) != 1 {
		t.Fatalf("import result %+v", res)
	}
	res, err = Import(ctx, db, e)
	if err != nil || res.Created != 0 || res.Skipped != 2 {
		t.Fatalf("second import not idempotent: %+v %v", res, err)
	}

	out, err := Build(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := WriteJSON(&buf, out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "credentials") {
		t.Fatal("export contains credentials")
	}
	back, err := ReadJSON(&buf)
	if err != nil || len(back.Devices) != 2 || back.Devices[0].Group != "Building A" {
		t.Fatalf("round trip: %+v %v", back, err)
	}
	buf.Reset()
	if err := WriteCSV(&buf, out); err != nil || !strings.HasPrefix(buf.String(), "name,group,kind,url") {
		t.Fatalf("csv: %q %v", buf.String(), err)
	}
}

func TestNormalizeMonitoring(t *testing.T) {
	base := func(mut func(d *store.Device)) *store.Device {
		d := &store.Device{Name: "sw", Kind: store.KindERS, URL: "192.0.2.1"}
		mut(d)
		return d
	}
	cases := []struct {
		name string
		d    *store.Device
		key  string // expected FieldError key, "" = valid
	}{
		{"defaults", base(func(d *store.Device) {}), ""},
		{"snmp", base(func(d *store.Device) { d.MonitorSource, d.MonitorInterval = store.MonitorSNMP, 30 }), ""},
		{"edm on ers", base(func(d *store.Device) { d.MonitorSource, d.MonitorInterval = store.MonitorEDM, 60 }), ""},
		{"edm on link", base(func(d *store.Device) {
			d.Kind, d.URL, d.MonitorSource = store.KindLink, "https://x.example", store.MonitorEDM
		}), "monitor_source"},
		{"edm fast", base(func(d *store.Device) { d.MonitorSource, d.MonitorInterval = store.MonitorEDM, 45 }), "monitor_interval"},
		{"bad source", base(func(d *store.Device) { d.MonitorSource = "ping" }), "monitor_source"},
		{"bad driver", base(func(d *store.Device) { d.MonitorDriver = "juniper" }), "monitor_driver"},
		{"tplink", base(func(d *store.Device) { d.MonitorDriver = store.DriverTPLink }), ""},
		{"interval low", base(func(d *store.Device) { d.MonitorInterval = 29 }), "monitor_interval"},
		{"interval high", base(func(d *store.Device) { d.MonitorInterval = 3601 }), "monitor_interval"},
		{"port high", base(func(d *store.Device) { d.SNMPPort = 65536 }), "snmp_port"},
		{"port negative", base(func(d *store.Device) { d.SNMPPort = -1 }), "snmp_port"},
		{"v3", base(func(d *store.Device) { d.SNMPVersion = store.SNMPv3 }), ""},
		{"v1", base(func(d *store.Device) { d.SNMPVersion = "v1" }), "snmp_version"},
	}
	for _, c := range cases {
		err := Normalize(c.d)
		var fe *FieldError
		switch {
		case c.key == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.key != "" && (!errors.As(err, &fe) || fe.Key != c.key):
			t.Errorf("%s: err=%v, want key %s", c.name, err, c.key)
		}
	}
	d := base(func(d *store.Device) {})
	if err := Normalize(d); err != nil || d.MonitorSource != store.MonitorNone || d.MonitorDriver != store.DriverAuto ||
		d.MonitorInterval != 60 || d.SNMPVersion != store.SNMPv2c || d.SNMPPort != 161 {
		t.Fatalf("defaults: %+v %v", d, err)
	}
}

func TestSNMPCredentialsBound(t *testing.T) {
	k, err := secret.LoadOrCreate(filepath.Join(t.TempDir(), "k"))
	if err != nil {
		t.Fatal(err)
	}
	d := &store.Device{ID: 3, URL: "https://192.0.2.10", SNMPPort: 161}
	if got := string(SNMPAAD(d)); got != "ers-proxy snmp|3|192.0.2.10|161" {
		t.Fatalf("aad %q", got)
	}
	enc, err := EncryptSNMP(k, d, snmp.Credentials{Community: "public-ro"})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(enc, []byte("public-ro")) {
		t.Fatal("community stored in plain text")
	}
	d.SNMPCredentials = enc
	c, err := DecryptSNMP(k, d)
	if err != nil || c.Community != "public-ro" {
		t.Fatalf("decrypt: %+v %v", c, err)
	}
	host := *d
	host.URL = "https://198.51.100.9"
	if _, err := DecryptSNMP(k, &host); err == nil {
		t.Fatal("usable after the host changed")
	}
	port := *d
	port.SNMPPort = 1161
	if _, err := DecryptSNMP(k, &port); err == nil {
		t.Fatal("usable after the SNMP port changed")
	}
	// The HTTP port and scheme are not part of the binding.
	web := *d
	web.URL = "http://192.0.2.10:8080"
	if _, err := DecryptSNMP(k, &web); err != nil {
		t.Fatalf("HTTP port change broke SNMP credentials: %v", err)
	}
	if _, err := EncryptSNMP(k, &store.Device{URL: "http://x"}, snmp.Credentials{}); err == nil {
		t.Fatal("encrypted for an unsaved device")
	}
}

func TestMonitoringExportImport(t *testing.T) {
	ctx := context.Background()
	open := func() *store.DB {
		db, err := store.Open(store.Options{Path: filepath.Join(t.TempDir(), "t.db")})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		if err := db.Migrate(ctx, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
			t.Fatal(err)
		}
		return db
	}
	src := open()
	in := &Export{Version: ExportVersion, Devices: []ExportDevice{{Name: "a", Kind: store.KindERS, URL: "192.0.2.1",
		MonitorSource: store.MonitorSNMP, MonitorDriver: store.DriverCisco, MonitorInterval: 120, SNMPVersion: store.SNMPv3, SNMPPort: 1161}}}
	if res, err := Import(ctx, src, in); err != nil || res.Created != 1 {
		t.Fatalf("import: %+v %v", res, err)
	}
	out, err := Build(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := WriteJSON(&buf, out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"monitor_source": "snmp"`, `"monitor_driver": "cisco"`, `"monitor_interval": 120`, `"snmp_version": "v3"`, `"snmp_port": 1161`} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("export lacks %s:\n%s", want, buf.String())
		}
	}
	if strings.Contains(buf.String(), "snmp_credentials") {
		t.Fatal("export carries SNMP credentials")
	}
	back, err := ReadJSON(&buf)
	if err != nil {
		t.Fatal(err)
	}
	dst := open()
	if res, err := Import(ctx, dst, back); err != nil || res.Created != 1 {
		t.Fatalf("re-import: %+v %v", res, err)
	}
	devs, _ := dst.ListDevices(ctx)
	d := devs[0]
	if d.MonitorSource != store.MonitorSNMP || d.MonitorDriver != store.DriverCisco || d.MonitorInterval != 120 ||
		d.SNMPVersion != store.SNMPv3 || d.SNMPPort != 1161 || d.HasSNMPCredentials() {
		t.Fatalf("round trip: %+v", d)
	}
}
