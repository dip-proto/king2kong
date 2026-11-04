// Package conv converts Go programs that build their command line with
// kingpin into programs that declare it as a kong grammar.
//
// It works in three steps.
// First, the registration code (constructors, helpers, option structs) is
// run symbolically to recover the grammar it builds and where each flag's
// value ends up.
// Then a planner turns that grammar into source edits: kong tags on the
// destination fields, a generated grammar tree, and the removal of the
// kingpin registration.
// Finally, the edits are applied to syntax trees that keep the comments.
package conv

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/dave/dst"
	"github.com/dave/dst/decorator"
	"golang.org/x/tools/go/packages"
)

// Package pairs a type-checked package with its decorated syntax.
type Package struct {
	*decorator.Package
	// names maps every import path in the program to its package name.
	names map[string]string
}

func (p *Package) astOf(n dst.Node) ast.Node {
	if p.Decorator == nil {
		// A directory holding only tests has no package to decorate.
		return nil
	}
	return p.Decorator.Ast.Nodes[n]
}

func (p *Package) typeOf(n dst.Expr) types.Type {
	a, ok := p.astOf(n).(ast.Expr)
	if !ok {
		return nil
	}
	return p.TypesInfo.TypeOf(a)
}

func (p *Package) tv(n dst.Expr) (types.TypeAndValue, bool) {
	a, ok := p.astOf(n).(ast.Expr)
	if !ok {
		return types.TypeAndValue{}, false
	}
	tv, ok := p.TypesInfo.Types[a]
	return tv, ok
}

// importName returns the path an import names and the name the file uses
// for it.
func (p *Package) importName(imp *dst.ImportSpec) (path, name string) {
	path, _ = strconv.Unquote(imp.Path.Value)
	if imp.Name != nil {
		return path, imp.Name.Name
	}
	return path, p.names[path]
}

func (p *Package) objectOf(n dst.Node) types.Object {
	switch a := p.astOf(n).(type) {
	case *ast.Ident:
		return p.TypesInfo.ObjectOf(a)
	case *ast.SelectorExpr:
		if sel, ok := p.TypesInfo.Selections[a]; ok {
			return sel.Obj()
		}
		return p.TypesInfo.ObjectOf(a.Sel)
	}
	return nil
}

func (p *Package) selection(n *dst.SelectorExpr) *types.Selection {
	a, ok := p.astOf(n).(*ast.SelectorExpr)
	if !ok {
		return nil
	}
	return p.TypesInfo.Selections[a]
}

func (p *Package) position(n dst.Node) token.Position {
	if a := p.astOf(n); a != nil && a.Pos().IsValid() {
		return p.Fset.Position(a.Pos())
	}
	return token.Position{}
}

func (p *Package) filename(f *dst.File) string { return p.Decorator.Filenames[f] }

type funcDecl struct {
	pkg  *Package
	file *dst.File
	decl *dst.FuncDecl
}

// Program holds the loaded packages and the indexes built over them.
type Program struct {
	Dir      string
	Packages []*Package
	byPath   map[string]*Package
	funcs    map[*types.Func]funcDecl
	unkeyed  map[*types.Named]bool
	// testUnkeyed are the type names test files list fields of by
	// position, by directory.
	testUnkeyed map[string]map[string]bool
	// written holds package-level variables that are assigned or have their
	// address taken somewhere, so the rest can be treated as constants.
	written map[*types.Var]bool
	// varInit maps a package-level variable to its initializer.
	varInit map[*types.Var]varInit
	// kingpin holds the import paths of the kingpin packages the program
	// uses, recognized by their API so that forks need no configuration.
	kingpin map[string]bool
	// kingpinV3 is set for kingpin v3 and its forks.
	// Their FullCommand includes argument placeholders, and their --help
	// exits with status 1.
	kingpinV3 bool
	// kingpinV2Legacy is set for kingpin v2 releases before v2.3.0, such as
	// gopkg.in/alecthomas/kingpin.v2, whose help differs from later ones.
	kingpinV2Legacy bool
	// initialisms are the words written in upper case in identifiers.
	initialisms map[string]bool
}

type varInit struct {
	pkg  *Package
	expr dst.Expr
}

// Load type-checks and decorates the packages matching patterns under dir.
func Load(dir string, patterns ...string) (*Program, error) {
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
			packages.NeedImports | packages.NeedDeps | packages.NeedTypes |
			packages.NeedSyntax | packages.NeedTypesInfo | packages.NeedModule,
		Dir: dir,
	}
	dpkgs, err := decorator.Load(cfg, patterns...)
	if err != nil {
		return nil, fmt.Errorf("loading packages: %w", err)
	}
	prog := &Program{
		Dir:     dir,
		byPath:  map[string]*Package{},
		funcs:   map[*types.Func]funcDecl{},
		written: map[*types.Var]bool{},
		varInit: map[*types.Var]varInit{},
		kingpin: map[string]bool{},
	}
	var errs []string
	for _, dp := range dpkgs {
		for _, e := range dp.Errors {
			errs = append(errs, e.Error())
		}
		p := &Package{Package: dp}
		prog.Packages = append(prog.Packages, p)
		prog.byPath[dp.PkgPath] = p
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("type errors in target:\n%s", strings.Join(errs, "\n"))
	}
	names := map[string]string{}
	seen := map[*types.Package]bool{}
	var visit func(t *types.Package)
	visit = func(t *types.Package) {
		if t == nil || seen[t] {
			return
		}
		seen[t] = true
		names[t.Path()] = t.Name()
		if isKingpin(t) {
			prog.kingpin[t.Path()] = true
			// v3 merged FlagClause and ArgClause into Clause.
			if _, ok := t.Scope().Lookup("Clause").(*types.TypeName); ok {
				prog.kingpinV3 = true
			} else if model, ok := t.Scope().Lookup("FlagModel").(*types.TypeName); ok {
				// HelpWithEnvar came with the help changes of v2.3.0.
				if obj, _, _ := types.LookupFieldOrMethod(types.NewPointer(model.Type()), true, t, "HelpWithEnvar"); obj == nil {
					prog.kingpinV2Legacy = true
				}
			}
		}
		for _, imp := range t.Imports() {
			visit(imp)
		}
	}
	for _, p := range prog.Packages {
		visit(p.Types)
		p.names = names
	}
	prog.index()
	prog.learnInitialisms()
	return prog, nil
}

func (prog *Program) index() {
	for _, p := range prog.Packages {
		for _, f := range p.Syntax {
			for _, imp := range f.Imports {
				if imp.Name != nil && imp.Name.Name != "_" && imp.Name.Name != "." {
					if path, err := strconv.Unquote(imp.Path.Value); err == nil {
						preferredAliases[path] = imp.Name.Name
					}
				}
			}
			for _, d := range f.Decls {
				switch d := d.(type) {
				case *dst.FuncDecl:
					if fn, ok := p.objectOf(d.Name).(*types.Func); ok {
						prog.funcs[fn] = funcDecl{pkg: p, file: f, decl: d}
					}
				case *dst.GenDecl:
					if d.Tok != token.VAR {
						continue
					}
					for _, s := range d.Specs {
						vs := s.(*dst.ValueSpec)
						for i, nm := range vs.Names {
							v, ok := p.objectOf(nm).(*types.Var)
							if !ok || i >= len(vs.Values) {
								continue
							}
							prog.varInit[v] = varInit{pkg: p, expr: vs.Values[i]}
						}
					}
				}
			}
			dst.Inspect(f, func(n dst.Node) bool {
				switch n := n.(type) {
				case *dst.AssignStmt:
					for _, lhs := range n.Lhs {
						prog.markWritten(p, lhs)
					}
				case *dst.IncDecStmt:
					prog.markWritten(p, n.X)
				case *dst.UnaryExpr:
					if n.Op == token.AND {
						prog.markWritten(p, n.X)
					}
				}
				return true
			})
		}
	}
}

func (prog *Program) markWritten(p *Package, e dst.Expr) {
	if v, ok := p.objectOf(e).(*types.Var); ok && !v.IsField() && v.Parent() == v.Pkg().Scope() {
		prog.written[v] = true
	}
}

func (prog *Program) isKingpinPath(path string) bool {
	return prog.kingpin[path]
}

// isKingpin reports whether a package is kingpin or a fork of it, going by its
// API: New(name, help string) *Application and a CmdClause type.
func isKingpin(t *types.Package) bool {
	scope := t.Scope()
	newFn, ok := scope.Lookup("New").(*types.Func)
	if !ok {
		return false
	}
	app, ok := scope.Lookup("Application").(*types.TypeName)
	if !ok {
		return false
	}
	if _, ok := scope.Lookup("CmdClause").(*types.TypeName); !ok {
		return false
	}
	sig := newFn.Type().(*types.Signature)
	if sig.Params().Len() != 2 || sig.Results().Len() != 1 {
		return false
	}
	ptr, ok := sig.Results().At(0).Type().(*types.Pointer)
	return ok && types.Identical(ptr.Elem(), app.Type())
}

// isKingpinType reports whether t, after dereferencing, is a named type from a
// kingpin package.
func (prog *Program) isKingpinType(t types.Type) bool {
	if t == nil {
		return false
	}
	if n, ok := deref(t).(*types.Named); ok && n.Obj().Pkg() != nil {
		return prog.isKingpinPath(n.Obj().Pkg().Path())
	}
	return false
}

func (prog *Program) owns(pkg *types.Package) bool {
	return pkg != nil && prog.byPath[pkg.Path()] != nil
}

// ownsType reports whether t is a named type of the program.
// Unnamed types, such as anonymous structs, count as the program's.
func (prog *Program) ownsType(t types.Type) bool {
	n, ok := deref(t).(*types.Named)
	if !ok {
		return true
	}
	return prog.owns(n.Obj().Pkg())
}

func deref(t types.Type) types.Type {
	for {
		p, ok := t.(*types.Pointer)
		if !ok {
			return t
		}
		t = p.Elem()
	}
}

func structOf(t types.Type) *types.Struct {
	if t == nil {
		return nil
	}
	s, _ := t.Underlying().(*types.Struct)
	return s
}

// packageAt returns the loaded package containing the file at pos.
func (prog *Program) packageAt(pos token.Position) *Package {
	for _, p := range prog.Packages {
		for _, f := range p.Syntax {
			if p.filename(f) == pos.Filename {
				return p
			}
		}
	}
	return nil
}

// unkeyedLiterals reports whether a composite literal of t lists its fields
// by position somewhere, which adding or reordering fields would break.
func (prog *Program) unkeyedLiterals(t *types.Named) bool {
	if prog.unkeyed == nil {
		prog.unkeyed = map[*types.Named]bool{}
		for _, p := range prog.Packages {
			for _, f := range p.Syntax {
				dst.Inspect(f, func(n dst.Node) bool {
					cl, ok := n.(*dst.CompositeLit)
					if !ok || len(cl.Elts) == 0 {
						return true
					}
					// The type of an elided literal, as in []*T{{...}}, may
					// be a pointer.
					if _, keyed := cl.Elts[0].(*dst.KeyValueExpr); !keyed {
						if lt := namedType(p.typeOf(cl)); lt != nil {
							prog.unkeyed[lt] = true
						}
					}
					return true
				})
			}
		}
	}
	return prog.unkeyed[t.Origin()] || prog.unkeyedInTests(t.Origin())
}

// unkeyedInTests reports whether the tests next to t's package build a t
// listing its fields by position, as configCommand{"us-east-1"} does.
// Tests are not loaded, so this only looks at the syntax, by type name.
func (prog *Program) unkeyedInTests(t *types.Named) bool {
	var p *Package
	for _, q := range prog.Packages {
		if q.Types == t.Obj().Pkg() {
			p = q
		}
	}
	if p == nil || len(p.Syntax) == 0 {
		return false
	}
	dir := filepath.Dir(p.filename(p.Syntax[0]))
	if prog.testUnkeyed == nil {
		prog.testUnkeyed = map[string]map[string]bool{}
	}
	names, ok := prog.testUnkeyed[dir]
	if !ok {
		names = map[string]bool{}
		prog.testUnkeyed[dir] = names
		tests, _ := filepath.Glob(filepath.Join(dir, "*_test.go"))
		for _, path := range tests {
			f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				continue
			}
			// The literals in a table such as []T{{"a"}, {"b"}} leave
			// their type out.
			elided := map[*ast.CompositeLit]string{}
			ast.Inspect(f, func(n ast.Node) bool {
				cl, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				var elem ast.Expr
				switch typ := cl.Type.(type) {
				case *ast.ArrayType:
					elem = typ.Elt
				case *ast.MapType:
					elem = typ.Value
				}
				for _, e := range cl.Elts {
					if kv, ok := e.(*ast.KeyValueExpr); ok {
						e = kv.Value
					}
					if u, ok := e.(*ast.UnaryExpr); ok && u.Op == token.AND {
						e = u.X
					}
					if inner, ok := e.(*ast.CompositeLit); ok && inner.Type == nil && elem != nil {
						elided[inner] = typeName(elem)
					}
				}
				if len(cl.Elts) == 0 {
					return true
				}
				if _, keyed := cl.Elts[0].(*ast.KeyValueExpr); keyed {
					return true
				}
				if name := typeName(cl.Type); name != "" {
					names[name] = true
				} else if name := elided[cl]; name != "" {
					names[name] = true
				}
				return true
			})
		}
	}
	return names[t.Obj().Name()]
}

// typeName is the name of a type written as T, pkg.T or *T, or "".
func typeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return t.Sel.Name
	case *ast.StarExpr:
		return typeName(t.X)
	}
	return ""
}

// namedType returns the generic or plain named type behind aliases and
// pointers, as in *Alias or Ptr, or nil.
func namedType(t types.Type) *types.Named {
	for t != nil {
		switch u := types.Unalias(t).(type) {
		case *types.Pointer:
			t = u.Elem()
		case *types.Named:
			return u.Origin()
		default:
			return nil
		}
	}
	return nil
}

// packageOf returns the package holding node n, or nil.
func (prog *Program) packageOf(n dst.Node) *Package {
	for _, p := range prog.Packages {
		if p.astOf(n) != nil {
			return p
		}
	}
	return nil
}
