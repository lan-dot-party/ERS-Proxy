package snmpwrite

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gosnmp/gosnmp"

	"github.com/lan-dot-party/ers-proxy/internal/monitor"
	"github.com/lan-dot-party/ers-proxy/internal/monitor/snmpsrc"
	"github.com/lan-dot-party/ers-proxy/internal/snmptest"
	"github.com/lan-dot-party/ers-proxy/internal/switchcfg"
)

func TestPDUs(t *testing.T) {
	as := []switchcfg.Assignment{
		{Name: "ifAlias", Index: "7", Value: switchcfg.Text("uplink")},
		{Name: "ifAdminStatus", Index: "7", Value: switchcfg.Int(2)},
		{Name: "sysName", Value: switchcfg.Text("sw")},
		{Name: "rcVlanPortMembers", Index: "5", Value: switchcfg.Octets([]byte{0x80, 0x01})},
	}
	got, err := pdus(as, 60)
	if err != nil {
		t.Fatal(err)
	}
	ifAlias, _ := switchcfg.OID("ifAlias")
	if got[0].Name != "."+ifAlias+".7" || got[0].Type != gosnmp.OctetString || string(got[0].Value.([]byte)) != "uplink" {
		t.Fatalf("alias pdu: %+v", got[0])
	}
	if got[1].Type != gosnmp.Integer || got[1].Value.(int) != 2 {
		t.Fatalf("int pdu: %+v", got[1])
	}
	if !strings.HasSuffix(got[2].Name, ".0") {
		t.Fatalf("scalar pdu: %+v", got[2])
	}
	if got[3].Type != gosnmp.OctetString || len(got[3].Value.([]byte)) != 2 {
		t.Fatalf("members pdu: %+v", got[3])
	}

	// Gauge32 and OID kinds, via temporary object registrations.
	monitor.Objects["xGauge"] = "1.3.6.1.99.1"
	monitor.Objects["xOID"] = "1.3.6.1.99.2"
	defer delete(monitor.Objects, "xGauge")
	defer delete(monitor.Objects, "xOID")
	got, err = pdus([]switchcfg.Assignment{
		{Name: "xGauge", Index: "1", Value: monitor.Value{Kind: monitor.KindUint, Uint: 9}},
		{Name: "xOID", Index: "1", Value: monitor.Value{Kind: monitor.KindOID, OID: "1.3.6.1.2"}},
	}, 60)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Type != gosnmp.Gauge32 || got[0].Value.(uint) != 9 {
		t.Fatalf("gauge pdu: %+v", got[0])
	}
	if got[1].Type != gosnmp.ObjectIdentifier || got[1].Value.(string) != "1.3.6.1.2" {
		t.Fatalf("oid pdu: %+v", got[1])
	}

	if _, err := pdus(as[:2], 1); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("MaxOids not enforced: %v", err)
	}
	if _, err := pdus([]switchcfg.Assignment{{Name: "ifAlias", Index: "1"}}, 60); err == nil {
		t.Fatal("KindNone accepted")
	}
	if _, err := pdus([]switchcfg.Assignment{{Name: "noSuchObject", Value: switchcfg.Int(1)}}, 60); err == nil {
		t.Fatal("unknown object accepted")
	}
	if _, err := pdus(nil, 60); err == nil {
		t.Fatal("empty step accepted")
	}
}

type fakeSetter struct {
	calls int
	pkt   *gosnmp.SnmpPacket
	err   error
}

func (f *fakeSetter) Set([]gosnmp.SnmpPDU) (*gosnmp.SnmpPacket, error) {
	f.calls++
	return f.pkt, f.err
}

func TestSetResponses(t *testing.T) {
	as := []switchcfg.Assignment{
		{Name: "ifAdminStatus", Index: "1", Value: switchcfg.Int(2)},
		{Name: "ifAlias", Index: "1", Value: switchcfg.Text("a")},
	}
	f := &fakeSetter{pkt: &gosnmp.SnmpPacket{Error: gosnmp.WrongType, ErrorIndex: 2}}
	w := &Writer{s: f, maxOid: 60}
	err := w.Set(context.Background(), as)
	var se *SetError
	if !errors.As(err, &se) || se.Status != gosnmp.WrongType || se.Index != 2 || se.Instance != "ifAlias.1" {
		t.Fatalf("got %v", err)
	}
	f.pkt = &gosnmp.SnmpPacket{}
	if err := w.Set(context.Background(), as); err != nil {
		t.Fatal(err)
	}
	f.err = errors.New("timeout")
	err = w.Set(context.Background(), as)
	if err == nil || errors.As(err, &se) {
		t.Fatalf("transport error: %v", err)
	}
	if f.calls != 3 {
		t.Fatalf("calls %d: one SET per call, no retry", f.calls)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.Set(ctx, as); !errors.Is(err, context.Canceled) || f.calls != 3 {
		t.Fatalf("cancelled context: %v calls %d", err, f.calls)
	}
}

// simulator starts snmpsim on a fresh copy of the writable fixture and
// returns write and read access to it.
func simulator(t *testing.T) (*Writer, *snmpsrc.Reader) {
	t.Helper()
	dir := t.TempDir()
	src, err := os.ReadFile(filepath.Join(snmptest.DataDir(t), "..", "snmpwrite", "writable.snmprec"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "writable.snmprec"), src, 0o600); err != nil {
		t.Fatal(err)
	}
	target := snmptest.Simulator(t, dir, "writable")
	w, err := Open(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Close)
	if w.g.Retries != 0 {
		t.Fatalf("retries %d", w.g.Retries)
	}
	r, err := snmpsrc.Open(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	return w, r
}

func TestSetAgainstSimulator(t *testing.T) {
	w, r := simulator(t)
	ctx := context.Background()
	as := []switchcfg.Assignment{
		{Name: "ifAlias", Index: "1", Value: switchcfg.Text("uplink")},
		{Name: "ifAdminStatus", Index: "2", Value: switchcfg.Int(2)},
		{Name: "rcVlanPortDefaultVlanId", Index: "1", Value: switchcfg.Int(10)},
		{Name: "rcVlanPortMembers", Index: "5", Value: switchcfg.Octets([]byte{0xC0, 0x20})},
	}
	if err := w.Set(ctx, as); err != nil {
		t.Fatal(err)
	}
	mm, err := switchcfg.Verify(ctx, r, as)
	if err != nil || len(mm) != 0 {
		t.Fatalf("verify: %v %v", mm, err)
	}
	// The other rows are untouched.
	mm, err = switchcfg.Verify(ctx, r, []switchcfg.Assignment{
		{Name: "ifAlias", Index: "2", Value: switchcfg.Text("port two")},
		{Name: "ifAdminStatus", Index: "1", Value: switchcfg.Int(1)},
	})
	if err != nil || len(mm) != 0 {
		t.Fatalf("untouched rows: %v %v", mm, err)
	}
}

// TestSetOnlyHere keeps SNMP writes contained: no non-test Go file outside
// this package may call gosnmp's Set.
func TestSetOnlyHere(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	const gosnmpPath = "github.com/gosnmp/gosnmp"
	here, _ := filepath.Abs(".")
	found := 0
	err := filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			if n := e.Name(); n == ".git" || n == "testdata" || n == "node_modules" || n == ".venv" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		imports := false
		for _, imp := range f.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p == gosnmpPath {
				imports = true
			}
		}
		if !imports {
			return nil
		}
		abs, _ := filepath.Abs(path)
		inside := filepath.Dir(abs) == here
		ast.Inspect(f, func(n ast.Node) bool {
			if s, ok := n.(*ast.SelectorExpr); ok && (s.Sel.Name == "Set" || s.Sel.Name == "SetRequest") {
				if inside {
					found++
				} else {
					t.Errorf("%s: gosnmp Set outside internal/switchcfg/snmpwrite", path)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found == 0 {
		t.Fatal("the scan found no Set call in the writer: it is not looking at the right files")
	}
}
