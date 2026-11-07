package gwnsrc_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/lan-dot-party/ers-proxy/internal/gwn"
	"github.com/lan-dot-party/ers-proxy/internal/gwn/gwntest"
	"github.com/lan-dot-party/ers-proxy/internal/monitor"
	"github.com/lan-dot-party/ers-proxy/internal/monitor/gwnsrc"
)

func newReader(t *testing.T, snmp monitor.Reader) *gwnsrc.Reader {
	t.Helper()
	srv := httptest.NewServer(gwntest.New())
	t.Cleanup(srv.Close)
	c := &gwn.Client{Transport: http.DefaultTransport, Base: srv.URL, Username: gwntest.User, Password: gwntest.Password}
	return &gwnsrc.Reader{C: c, SNMP: snmp}
}

// fakeSNMP serves counters for ifIndexes 1..10 and a stray row 99.
type fakeSNMP struct {
	err   error
	calls []string
}

func (f *fakeSNMP) Scalars(ctx context.Context, names ...string) (map[string]monitor.Value, error) {
	f.calls = append(f.calls, "scalars:"+strings.Join(names, ","))
	if f.err != nil {
		return nil, f.err
	}
	return map[string]monitor.Value{}, nil
}

func (f *fakeSNMP) Columns(ctx context.Context, names ...string) (monitor.Table, error) {
	f.calls = append(f.calls, "columns:"+strings.Join(names, ","))
	if f.err != nil {
		return nil, f.err
	}
	t := monitor.Table{}
	for _, i := range []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 99} {
		row := monitor.Row{}
		for _, n := range names {
			switch n {
			case "ifHCInOctets":
				row[n] = monitor.Value{Kind: monitor.KindUint, Uint: uint64(1000 * i)}
			case "ifHCOutOctets":
				row[n] = monitor.Value{Kind: monitor.KindUint, Uint: uint64(2000 * i)}
			case "ifInErrors":
				row[n] = monitor.Value{Kind: monitor.KindUint, Uint: uint64(500 + i)}
			}
		}
		t[strconv.Itoa(i)] = row
	}
	return t, nil
}

func notes(r *gwnsrc.Reader) string { return strings.Join(r.SourceNotes(), " | ") }

func TestWebOnly(t *testing.T) {
	r := newReader(t, nil)
	ctx := context.Background()
	sc, err := r.Scalars(ctx, "sysObjectID", "sysName", "gwnProductModel", "gwnFirmwareVersion")
	if err != nil {
		t.Fatal(err)
	}
	if sc["sysObjectID"].OID != "1.3.6.1.4.1.42397" || sc["sysObjectID"].Kind != monitor.KindOID {
		t.Errorf("sysObjectID: %+v", sc["sysObjectID"])
	}
	if sc["gwnProductModel"].String() != "GWN7721" || sc["gwnFirmwareVersion"].String() != "1.0.7.128" {
		t.Errorf("scalars: %+v", sc)
	}
	tab, err := r.Columns(ctx, "ifName", "ifAlias", "dot1qPvid", "dot3adAggPortAttachedAggID", "ifInErrors", "ifHCInOctets")
	if err != nil {
		t.Fatal(err)
	}
	if len(tab) != 10 {
		t.Errorf("rows: %d", len(tab))
	}
	if tab["1"]["ifName"].String() != "2.5GE1" || tab["1"]["ifAlias"].String() != "office" || tab["9"]["ifName"].String() != "SFP+9" {
		t.Errorf("names: %+v %+v", tab["1"], tab["9"])
	}
	if v, _ := tab["2"]["dot1qPvid"].AsInt(); v != 100 {
		t.Errorf("pvid 2: %+v", tab["2"])
	}
	for _, i := range []string{"3", "4"} {
		if v, _ := tab[i]["dot3adAggPortAttachedAggID"].AsInt(); v != 1001 {
			t.Errorf("LAG of port %s: %+v", i, tab[i])
		}
	}
	if _, ok := tab["1"]["dot3adAggPortAttachedAggID"]; ok {
		t.Errorf("port 1 has a LAG")
	}
	if v, _ := tab["1"]["ifInErrors"].AsInt(); v != 7 {
		t.Errorf("ifInErrors 1: %+v", tab["1"]["ifInErrors"])
	}
	if _, ok := tab["1"]["ifHCInOctets"]; ok {
		t.Errorf("web-only reader serves byte counters")
	}
	if !strings.Contains(notes(r), "SNMP") || !strings.Contains(notes(r), "Traffic") {
		t.Errorf("notes: %s", notes(r))
	}
}

func TestPortLists(t *testing.T) {
	r := newReader(t, nil)
	tab, err := r.Columns(context.Background(), "dot1qVlanStaticName", "dot1qVlanCurrentEgressPorts",
		"dot1qVlanCurrentUntaggedPorts", "lldpRemSysName", "lldpRemPortId")
	if err != nil {
		t.Fatal(err)
	}
	check := func(idx, col string, want []byte) {
		t.Helper()
		if got := tab[idx][col].Bytes; !reflect.DeepEqual(got, want) {
			t.Errorf("%s[%s] = %x, want %x", col, idx, got, want)
		}
	}
	// MSB of the first octet is port 1.
	check("0.1", "dot1qVlanCurrentUntaggedPorts", []byte{0xBF, 0xC0})
	check("0.1", "dot1qVlanCurrentEgressPorts", []byte{0xBF, 0xC0})
	check("0.100", "dot1qVlanCurrentEgressPorts", []byte{0x60, 0x80})
	check("0.100", "dot1qVlanCurrentUntaggedPorts", []byte{0x40, 0x00})
	if tab["1"]["dot1qVlanStaticName"].String() != "default" || tab["100"]["dot1qVlanStaticName"].String() != "office" {
		t.Errorf("VLAN names: %+v %+v", tab["1"], tab["100"])
	}
	if tab["0.9.1"]["lldpRemSysName"].String() != "core-1" || tab["0.9.1"]["lldpRemPortId"].String() != "1/47" {
		t.Errorf("LLDP: %+v", tab["0.9.1"])
	}
}

func TestWithSNMP(t *testing.T) {
	f := &fakeSNMP{}
	r := newReader(t, f)
	tab, err := r.Columns(context.Background(), "ifName", "ifHCInOctets", "ifHCOutOctets", "ifInErrors")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tab["99"]; ok {
		t.Errorf("row 99 not dropped")
	}
	if len(tab) != 10 {
		t.Errorf("rows: %d", len(tab))
	}
	for i := 1; i <= 10; i++ {
		row := tab[strconv.Itoa(i)]
		in, _ := row["ifHCInOctets"].AsInt()
		out, _ := row["ifHCOutOctets"].AsInt()
		errs, _ := row["ifInErrors"].AsInt()
		if in != 1000*i || out != 2000*i || errs != 500+i {
			t.Errorf("port %d: %+v", i, row)
		}
		if row["ifName"].String() == "" {
			t.Errorf("port %d without name", i)
		}
	}
	for _, c := range f.calls {
		if strings.Contains(c, "ifName") {
			t.Errorf("web object asked from SNMP: %s", c)
		}
	}
	if strings.Contains(notes(r), "Traffic") {
		t.Errorf("notes: %s", notes(r))
	}
}

func TestSNMPFailure(t *testing.T) {
	r := newReader(t, &fakeSNMP{err: errors.New("timeout")})
	tab, err := r.Columns(context.Background(), "ifName", "ifHCInOctets", "ifInErrors")
	if err != nil {
		t.Fatalf("poll failed: %v", err)
	}
	if len(tab) != 10 || tab["1"]["ifName"].String() != "2.5GE1" {
		t.Errorf("table: %+v", tab)
	}
	if !strings.Contains(notes(r), "SNMP: timeout") {
		t.Errorf("notes: %s", notes(r))
	}
}
