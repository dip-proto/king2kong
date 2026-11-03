package kongcompat

import (
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestGo120 keeps kongcompat buildable with Go 1.20, the oldest release
// kong supports, since it is copied into converted programs as is.
// It rejects the standard packages and builtins added after it.
func TestGo120(t *testing.T) {
	newer := map[string]bool{"cmp": true, "slices": true, "maps": true, "iter": true, "log/slog": true, "math/rand/v2": true, "unique": true}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := gotoken.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			if path, _ := strconv.Unquote(imp.Path.Value); newer[path] {
				t.Errorf("%s imports %s, which Go 1.20 does not have", name, path)
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok && (id.Name == "min" || id.Name == "max" || id.Name == "clear") {
					t.Errorf("%s: %s calls the builtin %s, which Go 1.20 does not have", name, fset.Position(call.Pos()), id.Name)
				}
			}
			return true
		})
	}
}
