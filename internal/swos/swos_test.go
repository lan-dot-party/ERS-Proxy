package swos_test

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/lan-dot-party/ers-proxy/internal/swos"
	"github.com/lan-dot-party/ers-proxy/internal/swos/swostest"
	"github.com/lan-dot-party/ers-proxy/internal/upstream"
)

func TestParse(t *testing.T) {
	v, err := swos.Parse(` {a:0x1f, b:[0x01,2,'6869'], c:{d:''}, e:[] , f:[{g:0x0}]} `)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"a": uint64(31), "b": []any{uint64(1), uint64(2), "6869"}, "c": map[string]any{"d": ""},
		"e": []any{}, "f": []any{map[string]any{"g": uint64(0)}}}
	if !reflect.DeepEqual(v, want) {
		t.Fatalf("got %#v", v)
	}
	for _, bad := range []string{"", "{", "{a}", "{a:}", "[1,", "{a:1}x", "{a:'x}", "{a:-1}", "{a:0xzz}", "{a:'\\x'}",
		"alert(1)", strings.Repeat("[", 20) + strings.Repeat("]", 20)} {
		if _, err := swos.Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
}

func TestHelpers(t *testing.T) {
	if swos.Text("53573033") != "SW03" || swos.Text("not hex") != "not hex" || swos.Text("410a42") != "AB" {
		t.Fatal("Text")
	}
	if !swos.Bit(uint64(0x05), 0) || swos.Bit(uint64(0x05), 1) || !swos.Bit(uint64(0x05), 2) {
		t.Fatal("Bit number")
	}
	// Arrays: the last word holds bits 0–31.
	words := []any{uint64(0x1), uint64(0x80000000)}
	if !swos.Bit(words, 31) || !swos.Bit(words, 32) || swos.Bit(words, 0) || swos.Bit(words, 64) {
		t.Fatal("Bit array")
	}
}

func client(t *testing.T, fake *swostest.Server, pass string) *swos.Client {
	t.Helper()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	return &swos.Client{Transport: upstream.WithDigest(http.DefaultTransport, upstream.Login{Username: swostest.User, Password: pass}),
		Base: srv.URL}
}

func TestLoad(t *testing.T) {
	fake := swostest.New()
	st, err := client(t, fake, swostest.Password).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Model != "CRS305-1G-4S+" || st.Identity != "swos-test" || st.Version != "2.18" || st.Serial != "TEST0001" ||
		st.Uptime != 0x12345 {
		t.Fatalf("system: %+v", st)
	}
	if len(st.Ports) != 5 || len(st.Notes) != 0 {
		t.Fatalf("ports %d, notes %v", len(st.Ports), st.Notes)
	}
	labels := []string{}
	for _, p := range st.Ports {
		labels = append(labels, p.Label)
	}
	if strings.Join(labels, ",") != "ETH/BOOT,SFP1,SFP2,SFP3,SFP4" {
		t.Fatalf("labels %v", labels)
	}
	p0, p1, p4 := st.Ports[0], st.Ports[1], st.Ports[4]
	if p0.Name != "office" || !p0.Enabled || !p0.Link || !p0.FullDuplex || p0.Speed != 1e9 ||
		p0.VLANMode != "strict" || p0.VLANReceive != "only untagged" || p0.DefaultVLAN != 20 || p0.Trunk != 0 {
		t.Fatalf("port 1: %+v", p0)
	}
	if p1.Speed != 10e9 || p1.Trunk != 1 || p1.VLANReceive != "only tagged" || p1.RxBytes != 1<<32|0x10 ||
		p1.TxBytes != 2<<32|0x20 || p1.RxDrops != 3 {
		t.Fatalf("SFP1: %+v", p1)
	}
	if p4.Enabled || p4.Link || p4.Speed != 0 || p4.VLANMode != "disabled" {
		t.Fatalf("SFP4: %+v", p4)
	}
	if st.Ports[0].RxErrors != 1 || st.Ports[2].TxErrors != 2 {
		t.Fatal("errors")
	}
	if !st.HasVLANs || len(st.VLANs) != 2 || st.VLANs[0].ID != 10 || st.VLANs[0].Name != "servers" ||
		!reflect.DeepEqual(st.VLANs[0].Members, []int{1, 2, 3}) || !reflect.DeepEqual(st.VLANs[1].Members, []int{0, 1, 2}) {
		t.Fatalf("vlans %+v", st.VLANs)
	}
	// Only the known files, only GET.
	for _, f := range []string{"/sys.b", "/link.b", "/fwd.b", "/vlan.b", "/lacp.b", "/stats.b"} {
		if fake.Requests(f) == 0 {
			t.Errorf("%s not read", f)
		}
	}
	if fake.Requests("/snmp.b") != 0 {
		t.Error("snmp.b read")
	}
}

func TestLoadOptionalMissing(t *testing.T) {
	fake := swostest.New()
	fake.SetFile("/vlan.b", "garbage(")
	fake.SetFile("/stats.b", "{rb:'x'}")
	st, err := client(t, fake, swostest.Password).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.HasVLANs || len(st.Notes) != 1 || !strings.Contains(st.Notes[0], "vlan.b") {
		t.Fatalf("notes %v, vlans %v", st.Notes, st.HasVLANs)
	}
	if st.Ports[1].RxBytes != 0 {
		t.Fatal("stats of a malformed file")
	}
}

func TestLoadWrongLogin(t *testing.T) {
	_, err := client(t, swostest.New(), "wrong").Load(context.Background())
	if !errors.Is(err, swos.ErrLogin) {
		t.Fatalf("err %v", err)
	}
}

func TestGetOnlyKnownFiles(t *testing.T) {
	c := &swos.Client{Transport: http.DefaultTransport, Base: "http://192.0.2.1"}
	if _, err := c.Get(context.Background(), "snmp.b"); err == nil {
		t.Fatal("snmp.b allowed")
	}
}

// TestReadOnly: the reader only sends GET requests; SwOS applies settings
// with POSTs to the same files.
func TestReadOnly(t *testing.T) {
	for _, f := range []string{"swos.go", "parse.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{"MethodPost", "MethodPut", "MethodDelete", `"POST"`, "PostForm"} {
			if strings.Contains(string(src), bad) {
				t.Errorf("%s mentions %s", f, bad)
			}
		}
	}
}

func TestLabels(t *testing.T) {
	fake := swostest.New()
	// A 10-port switch with two SFP cages at the end and no model quirks.
	fake.SetFile("/sys.b", `{upt:0x1,brd:'`+hex.EncodeToString([]byte("CSS610-8G-2S+"))+`'}`)
	fake.SetFile("/link.b", `{en:0x0,lnk:0x0,dpx:0x0,prt:0x0a,sfp:0x02,nm:[]}`)
	st, err := client(t, fake, swostest.Password).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range st.Ports {
		got = append(got, p.Label)
	}
	if strings.Join(got, ",") != "Port1,Port2,Port3,Port4,Port5,Port6,Port7,Port8,SFP1,SFP2" {
		t.Fatalf("labels %v", got)
	}
}
