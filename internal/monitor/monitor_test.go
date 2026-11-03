package monitor

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestBitsMSB(t *testing.T) {
	if got := BitsMSB([]byte{0x80, 0x01, 0x60}); !reflect.DeepEqual(got, []int{0, 15, 17, 18}) {
		t.Fatalf("BitsMSB = %v", got)
	}
	if BitsMSB(nil) != nil {
		t.Fatal("empty bitmap")
	}
}

func TestObjectsResolve(t *testing.T) {
	for name, oid := range Objects {
		if !strings.HasPrefix(oid, "1.") || strings.HasSuffix(oid, ".") {
			t.Errorf("%s: bad OID %q", name, oid)
		}
	}
}

// The dashboard must never change a device: no SNMP SET and no EDM
// setVariables anywhere in the monitoring code or the SNMP package.
func TestReadOnly(t *testing.T) {
	// SET calls are searched in the SNMP code paths; the EDM reader (which uses
	// url.Values.Set and http.Header.Set) is covered by the string check.
	roots := []string{".", "../snmp"}
	astRoots := map[string]bool{".": true, "../snmp": true, "snmpsrc": true, "drivers": true, "poller": true, "history": true}
	fset := token.NewFileSet()
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if strings.Contains(string(src), "setVariables") || strings.Contains(string(src), "setFormMibs") {
				t.Errorf("%s mentions an EDM write command", path)
			}
			f, err := parser.ParseFile(fset, path, src, 0)
			if err != nil {
				return err
			}
			dir := filepath.Dir(path)
			if dir != "../snmp" && dir != "." {
				dir = strings.SplitN(filepath.ToSlash(path), "/", 2)[0]
			}
			if !astRoots[dir] {
				return nil
			}
			ast.Inspect(f, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok && (sel.Sel.Name == "Set" || sel.Sel.Name == "SetRequest") {
					t.Errorf("%s: call of %s", fset.Position(sel.Pos()), sel.Sel.Name)
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
