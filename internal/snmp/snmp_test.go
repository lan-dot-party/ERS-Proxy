package snmp_test

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lan-dot-party/ers-proxy/internal/snmp"
	"github.com/lan-dot-party/ers-proxy/internal/snmptest"
)

func TestSimulator(t *testing.T) {
	target := snmptest.Simulator(t, snmptest.DataDir(t), "basic")
	g, err := snmp.Dial(context.Background(), target, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer snmp.Close(g)

	res, err := g.Get([]string{"1.3.6.1.2.1.1.5.0"})
	if err != nil || len(res.Variables) != 1 {
		t.Fatalf("get sysName: %v %+v", err, res)
	}
	if got := snmp.String(res.Variables[0]); got != "sim-switch" {
		t.Fatalf("sysName = %q", got)
	}
	rows, err := snmp.WalkColumn(g, "1.3.6.1.2.1.2.2.1.2")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || snmp.String(rows[0].PDU) != "Port 1" || rows[1].Index[0] != 2 {
		t.Fatalf("ifDescr rows: %+v", rows)
	}
}

func TestValidate(t *testing.T) {
	ok := snmp.Target{Host: "192.0.2.1", Version: "v2c", Creds: snmp.Credentials{Community: "c"}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := []snmp.Target{
		{Version: "v2c", Creds: snmp.Credentials{Community: "c"}},
		{Host: "h", Version: "v1", Creds: snmp.Credentials{Community: "c"}},
		{Host: "h", Version: "v2c"},
		{Host: "h", Version: "v3", Creds: snmp.Credentials{Username: "u", SecurityLevel: "authNoPriv", AuthProtocol: "SHA", AuthPassword: "short"}},
	}
	for i, b := range bad {
		if b.Validate() == nil {
			t.Errorf("case %d accepted", i)
		}
	}
}

// TestReadOnly keeps the package read-only: no calls to Set/SetRequest on a
// gosnmp value and no exported Set* function.
func TestReadOnly(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob: %v", err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.FuncDecl:
				if x.Name.IsExported() && strings.HasPrefix(x.Name.Name, "Set") {
					t.Errorf("%s: exported func %s starts with Set", fset.Position(x.Pos()), x.Name.Name)
				}
			case *ast.SelectorExpr:
				if x.Sel.Name == "Set" || x.Sel.Name == "SetRequest" {
					t.Errorf("%s: call to %s is not allowed (read-only package)", fset.Position(x.Pos()), x.Sel.Name)
				}
			}
			return true
		})
	}
}
