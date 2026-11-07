package switchcfg_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestOnlyWebAndCmdImport keeps writing to switches contained: apart from
// internal/switchcfg itself, only the web layer and the server command may
// import it. Monitoring in particular must never reach a Writer.
func TestOnlyWebAndCmdImport(t *testing.T) {
	root := filepath.Join("..", "..")
	const mod = "github.com/lan-dot-party/ers-proxy/"
	allowed := []string{"internal/switchcfg", "internal/web", "cmd/"}
	err := filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			if n := e.Name(); n == ".git" || n == "testdata" || n == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if !strings.HasPrefix(p, mod+"internal/switchcfg") {
				continue
			}
			ok := false
			for _, a := range allowed {
				if strings.HasPrefix(rel, a) {
					ok = true
				}
			}
			if !ok {
				t.Errorf("%s imports %s; only internal/web and cmd may", rel, p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repository root not found: %v", err)
	}
}
