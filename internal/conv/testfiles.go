package conv

import (
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
)

// posKey identifies a declaration across separately loaded copies of a package by where it is.
type posKey struct {
	file   string
	offset int
}

func keyOf(fset *token.FileSet, pos token.Pos) posKey {
	p := fset.Position(pos)
	return posKey{p.Filename, p.Offset}
}

// renameInTests applies field renames to the test files of the packages
// declaring the renamed fields.
// Renamed fields were unexported, so only tests in the same package can refer
// to them.
// The test variants are type-checked separately and edited byte by byte,
// which leaves their formatting alone.
func (c *converter) renameInTests(renames map[*types.Var]string) error {
	if len(renames) == 0 {
		return nil
	}
	byKey := map[posKey]string{}
	pkgPaths := map[string]bool{}
	for v, name := range renames {
		if v.Pkg() == nil {
			continue
		}
		p := c.prog.byPath[v.Pkg().Path()]
		if p == nil {
			continue
		}
		byKey[keyOf(p.Fset, v.Pos())] = name
		pkgPaths[v.Pkg().Path()] = true
	}
	var patterns []string
	for path := range pkgPaths {
		if hasInternalTests(c.prog.byPath[path]) {
			patterns = append(patterns, path)
		}
	}
	if len(patterns) == 0 {
		return nil
	}
	sort.Strings(patterns)
	cfg := &packages.Config{
		Mode:  packages.NeedName | packages.NeedFiles | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo,
		Dir:   c.prog.Dir,
		Tests: true,
	}
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		return err
	}
	for _, p := range pkgs {
		if !strings.Contains(p.ID, ".test]") || strings.HasSuffix(p.PkgPath, "_test") {
			continue
		}
		for _, f := range p.Syntax {
			name := p.Fset.Position(f.Pos()).Filename
			if !strings.HasSuffix(name, "_test.go") {
				continue
			}
			type edit struct {
				off  int
				old  string
				name string
			}
			var edits []edit
			ast.Inspect(f, func(n ast.Node) bool {
				id, ok := n.(*ast.Ident)
				if !ok {
					return true
				}
				v, ok := p.TypesInfo.ObjectOf(id).(*types.Var)
				if !ok || !v.IsField() {
					return true
				}
				if newName, ok := byKey[keyOf(p.Fset, v.Pos())]; ok && id.Name != newName {
					edits = append(edits, edit{p.Fset.Position(id.Pos()).Offset, id.Name, newName})
				}
				return true
			})
			if len(edits) == 0 {
				continue
			}
			src, err := os.ReadFile(name)
			if err != nil {
				return err
			}
			sort.Slice(edits, func(i, j int) bool { return edits[i].off > edits[j].off })
			for _, e := range edits {
				if e.off+len(e.old) > len(src) || string(src[e.off:e.off+len(e.old)]) != e.old {
					continue
				}
				src = append(src[:e.off], append([]byte(e.name), src[e.off+len(e.old):]...)...)
			}
			c.res.Files[name] = string(src)
		}
	}
	return nil
}

func hasInternalTests(p *Package) bool {
	if p == nil || len(p.GoFiles) == 0 {
		return false
	}
	dir := filepath.Dir(p.GoFiles[0])
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), "_test.go") {
			return true
		}
	}
	return false
}
