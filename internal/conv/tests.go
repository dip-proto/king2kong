package conv

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/dave/dst"
	"github.com/dave/dst/decorator"
	"github.com/dave/dst/decorator/resolver/guess"
	"github.com/dave/dst/dstutil"
	"golang.org/x/tools/go/packages"
)

// convertTests ports the test files that used kingpin to the converted
// program.
// kingpin.New becomes kongcompat.New on the CLI, kingpin's types become
// kongcompat's, calls and field names follow the converted code, and the
// grammar the tests built on their own goes.
// Tests are not part of the loaded program, so their packages are loaded
// again with tests.
func (c *converter) convertTests(renames map[*types.Var]string) error {
	if c.compatPath == "" {
		return nil
	}
	dirs := map[string]bool{}
	for path := range c.kingpinTests {
		dirs[filepath.Dir(path)] = true
	}
	// Tests calling a function that lost parameters, as c.run(nil) for a
	// former Action, need converting too, though they don't use kingpin.
	for dir := range c.prunedCallers() {
		dirs[dir] = true
	}
	if len(dirs) == 0 {
		return nil
	}
	var patterns []string
	matched := map[string]bool{}
	for _, p := range c.prog.Packages {
		for _, f := range p.Syntax {
			if dir := filepath.Dir(p.filename(f)); dirs[dir] {
				patterns = append(patterns, p.PkgPath)
				matched[dir] = true
				break
			}
		}
	}
	base, _ := filepath.Abs(c.prog.Dir)
	for dir := range dirs {
		// A directory holding only tests is not in the program, but its
		// tests are.
		if rel, err := filepath.Rel(base, dir); !matched[dir] && err == nil && !strings.HasPrefix(rel, "..") && onlyTests(dir) {
			patterns = append(patterns, "./"+filepath.ToSlash(rel))
		}
	}
	if len(patterns) == 0 {
		return nil
	}
	sort.Strings(patterns)
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax | packages.NeedTypes |
			packages.NeedTypesInfo | packages.NeedImports | packages.NeedDeps | packages.NeedModule,
		Dir:   c.prog.Dir,
		Tests: true,
	}
	dpkgs, err := decorator.Load(cfg, patterns...)
	if err != nil {
		return err
	}
	fset := c.prog.Packages[0].Fset
	key := func(fs *token.FileSet, pos token.Pos) string {
		p := fs.Position(pos)
		return fmt.Sprintf("%s:%d", p.Filename, p.Offset)
	}
	pruned := map[string]prunedSig{}
	for fn, sig := range c.pruned {
		pruned[key(fset, fn.Pos())] = sig
	}
	emptied := map[string]bool{}
	for fn := range c.emptied {
		emptied[key(fset, fn.Pos())] = true
	}
	renamed := map[string]string{}
	for v, name := range renames {
		renamed[key(fset, v.Pos())] = name
	}
	// The fields that held a command's kingpin clause are gone; a test still
	// reaching for one gets the command from the application.
	clauseFields := map[string]bool{}
	for obj := range c.ed.typeSpecs {
		st := structOf(obj.Type())
		if st == nil || !c.prog.owns(obj.Pkg()) {
			continue
		}
		for i := 0; i < st.NumFields(); i++ {
			if f := st.Field(i); c.isClauseType(f.Type()) {
				clauseFields[key(fset, f.Pos())] = true
			}
		}
	}
	// Kept constructors that do nothing but build their command.
	pureCtors := map[string]bool{}
	for _, ci := range c.keptCtors {
		pure := true
		for _, it := range ci.items {
			pure = pure && it.kind != ctorResidue
		}
		if pure {
			pureCtors[key(fset, ci.fn.Pos())] = true
		}
	}
	done := map[string]bool{}
	for _, dp := range dpkgs {
		if !strings.Contains(dp.ID, ".test]") && !strings.HasSuffix(dp.PkgPath, "_test") {
			continue
		}
		p := &Package{Package: dp, names: c.prog.Packages[0].names}
		for _, f := range dp.Syntax {
			name := dp.Decorator.Filenames[f]
			// The other tests of the package may use the application a
			// kingpin test builds, as with a shared newTestApp helper.
			if !dirs[filepath.Dir(name)] || !strings.HasSuffix(name, "_test.go") || done[name] {
				continue
			}
			done[name] = true
			t := &testConversion{c: c, p: p, pruned: pruned, emptied: emptied, renamed: renamed, clauseFields: clauseFields, pureCtors: pureCtors, key: func(pos token.Pos) string { return key(dp.Fset, pos) }}
			src, err := t.convert(f, name)
			if err != nil {
				c.issue(token.Position{Filename: name}, "converting this test: %v", err)
				continue
			}
			if !c.kingpinTests[name] {
				if old, err := os.ReadFile(name); err == nil && string(old) == src {
					continue
				}
			}
			c.res.Files[name] = src
		}
	}
	return nil
}

type testConversion struct {
	c *converter
	p *Package
	// drop holds the test's registrations of grammar the converted program
	// already declares.
	drop map[dst.Stmt]bool
	// clauseFields are the fields that held kingpin clauses, pureCtors the
	// kept constructors that only build their command, and used the
	// variables the conversion made the test use.
	clauseFields map[string]bool
	pureCtors    map[string]bool
	used         map[types.Object]bool
	// injected are the names of the types hooks take.
	injected map[string]bool
	pruned   map[string]prunedSig
	emptied  map[string]bool
	renamed  map[string]string
	key      func(token.Pos) string
}

func (t *testConversion) convert(f *dst.File, name string) (string, error) {
	c, p := t.c, t.p
	exports := c.compatExports()
	t.drop = map[dst.Stmt]bool{}
	t.used = map[types.Object]bool{}
	dstutil.Apply(f, func(cur *dstutil.Cursor) bool {
		switch n := cur.Node().(type) {
		case *dst.AssignStmt:
			t.bindAtCall(cur, n)
			if t.followPrunedAssign(cur, n) {
				return false
			}
		case *dst.ExprStmt:
			if call, ok := n.X.(*dst.CallExpr); ok {
				if fn := c.g.in.staticCallee(p, call); fn != nil && t.emptied[t.key(fn.Pos())] {
					cur.Delete()
					return false
				}
			}
			t.bindAtCall(cur, n)
		case *dst.CallExpr:
			t.followPrunedCall(n)
			if id, ok := n.Fun.(*dst.Ident); ok && id.Name == "New" && c.prog.isKingpinPath(id.Path) {
				cur.Replace(t.newApplication(f, n))
				return false
			}
			if fun, ok := n.Fun.(*dst.Ident); ok && fun.Name == "ValueOf" && fun.Path == "reflect" && len(n.Args) == 1 {
				if c.prog.isKingpinType(p.typeOf(n.Args[0])) && !t.mirroredReflection(f, n) {
					c.issue(p.position(n), "this test reflects on kingpin's private fields; port it by hand")
				}
			}
		case *dst.Ident:
			if n.Path != "" && c.prog.isKingpinPath(n.Path) {
				c.compatName(p, n, exports)
			} else if n.Path == "" {
				if o := p.objectOf(n); o != nil {
					if v, ok := o.(*types.Var); ok && v.IsField() {
						if name, ok := t.renamed[t.key(v.Pos())]; ok {
							n.Name = name
						}
					}
				}
			}
		case *dst.SelectorExpr:
			if v, ok := p.objectOf(n).(*types.Var); ok && v.IsField() {
				if name, ok := t.renamed[t.key(v.Pos())]; ok {
					n.Sel.Name = name
				}
				if t.clauseFields[t.key(v.Pos())] {
					if app := onlyVarAt(p, n, func(v *types.Var) bool { return c.isAppType(v.Type()) }); app != nil {
						// x.CmdClause becomes app.CommandOf(x): the command
						// whose struct has x's type.
						t.used[app] = true
						cur.Replace(callOf(&dst.SelectorExpr{X: dst.NewIdent(app.Name()), Sel: dst.NewIdent("CommandOf")}, n.X))
						return false
					}
				}
			}
		}
		return true
	}, nil)
	dstutil.Apply(f, func(cur *dstutil.Cursor) bool {
		if s, ok := cur.Node().(dst.Stmt); ok && t.drop[s] {
			cur.Delete()
			return false
		}
		return true
	}, nil)
	t.dropUnusedGrammar(f)
	var buf bytes.Buffer
	r := decorator.NewRestorerWithImports(p.PkgPath, importResolver{names: p.names, fallback: guess.New()})
	if err := r.Fprint(&buf, f); err != nil {
		return "", err
	}
	orig, _ := os.ReadFile(name)
	return string(regroupImports(orig, buf.Bytes(), p.modulePath(), c.prog.isKingpinPath)), nil
}

// callee returns the function a call calls and what it lost, if anything.
func (t *testConversion) callee(call *dst.CallExpr) (*types.Func, prunedSig, bool) {
	fn := t.c.g.in.staticCallee(t.p, call)
	if fn == nil {
		return nil, prunedSig{}, false
	}
	sig, ok := t.pruned[t.key(fn.Pos())]
	return fn, sig, ok
}

func (t *testConversion) followPrunedCall(call *dst.CallExpr) {
	if fn, sig, ok := t.callee(call); ok {
		pruneArgs(t.p, call, fn, sig)
	}
}

// followPrunedAssign is converter.followPrunedAssign for tests.
func (t *testConversion) followPrunedAssign(cur *dstutil.Cursor, as *dst.AssignStmt) bool {
	if len(as.Rhs) != 1 || len(as.Lhs) != 1 {
		return false
	}
	call, ok := as.Rhs[0].(*dst.CallExpr)
	if !ok {
		return false
	}
	fn, sig, ok := t.callee(call)
	if !ok || !sig.results {
		return false
	}
	orig := fn.Type().(*types.Signature)
	slice, ok := orig.Results().At(0).Type().(*types.Slice)
	if !ok {
		return false
	}
	var app dst.Expr
	for i, a := range call.Args {
		if i < orig.Params().Len() && t.c.prog.isKingpinType(orig.Params().At(i).Type()) {
			app = a
		}
	}
	if app == nil {
		return false
	}
	if cur.Index() < 0 && !t.emptied[t.key(fn.Pos())] {
		t.c.issue(t.p.position(as), "%s no longer returns its commands; move this call to a statement of its own, or port it by hand", fn.Name())
		return false
	}
	t.followPrunedCall(call)
	if !t.emptied[t.key(fn.Pos())] {
		cur.InsertBefore(&dst.ExprStmt{X: call})
	}
	list := &dst.IndexExpr{X: t.c.compatIdent("Commands"), Index: typeNode(slice.Elem())}
	as.Rhs[0] = callOf(list, dst.Clone(app).(dst.Expr))
	return true
}

// newApplication builds kongcompat.New on the CLI for a test, with fresh
// global flags and the values of the types hooks take that are at hand.
func (t *testConversion) newApplication(f *dst.File, call *dst.CallExpr) dst.Expr {
	c := t.c
	cli := &dst.CompositeLit{Type: &dst.Ident{Name: c.cliName, Path: c.tree.RootPkg.PkgPath}}
	if c.rootFlags != nil {
		obj := c.rootFlags.Obj()
		holder := t.ownRootFlags(f, call)
		if holder == nil {
			holder = callOf(dst.NewIdent("new"), &dst.Ident{Name: obj.Name(), Path: obj.Pkg().Path()})
		}
		cli.Elts = append(cli.Elts, &dst.KeyValueExpr{Key: dst.NewIdent(obj.Name()), Value: holder})
	}
	args := append([]dst.Expr{&dst.UnaryExpr{Op: token.AND, X: cli}}, call.Args...)
	var binds []dst.Expr
	var typs []types.Type
	for _, typ := range c.injected {
		// The tests are type-checked apart: types compare by name.
		want := types.TypeString(typ, nil)
		if v := onlyVarAt(t.p, call, func(v *types.Var) bool { return types.TypeString(v.Type(), nil) == want }); v != nil {
			binds, typs = append(binds, dst.NewIdent(v.Name())), append(typs, typ)
		}
	}
	args = append(args, c.bindCalls(t.p.position(call), binds, typs, t.p.PkgPath, kongOption)...)
	args = append(args, c.newOptions(c.tree.RootPkg.PkgPath)...)
	return callOf(c.compatIdent("New"), args...)
}

// dropUnusedGrammar removes the local applications and clauses a test built
// that nothing uses any more, such as the parent command a constructor
// used to take.
func (t *testConversion) dropUnusedGrammar(f *dst.File) {
	for changed := true; changed; {
		changed = false
		uses := map[types.Object]int{}
		dst.Inspect(f, func(n dst.Node) bool {
			if id, ok := n.(*dst.Ident); ok {
				if o := t.p.objectOf(id); o != nil {
					if def, isDef := t.p.TypesInfo.Defs[t.p.astIdent(id)]; !isDef || def == nil {
						uses[o]++
					}
				}
			}
			return true
		})
		dstutil.Apply(f, func(cur *dstutil.Cursor) bool {
			as, ok := cur.Node().(*dst.AssignStmt)
			if !ok || as.Tok != token.DEFINE || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
				return true
			}
			o := t.p.objectOf(as.Lhs[0])
			if o == nil || uses[o] > 0 || t.used[o] {
				return true
			}
			if t.c.prog.isKingpinType(o.Type()) {
				cur.Delete()
				changed = true
			}
			// A command a pure constructor built only for its clause, such
			// as the parent passed to its subcommands' constructors.
			if call, ok := as.Rhs[0].(*dst.CallExpr); ok {
				if fn := t.c.g.in.staticCallee(t.p, call); fn != nil && t.pureCtors[t.key(fn.Pos())] {
					cur.Delete()
					changed = true
				}
			}
			return true
		}, nil)
	}
}

// astIdent is the syntax tree identifier behind id, or nil.
func (p *Package) astIdent(id *dst.Ident) *ast.Ident {
	a, _ := p.astOf(id).(*ast.Ident)
	return a
}

// ownRootFlags finds the application flags a test registers itself on the
// application call builds, such as
// app.Flag("debug", "Debug").BoolVar(&flags.Debug).
// Those registrations are dropped, and a pointer to the struct holding the
// flags is returned for the CLI to use.
func (t *testConversion) ownRootFlags(f *dst.File, call *dst.CallExpr) dst.Expr {
	p := t.p
	var app types.Object
	dst.Inspect(f, func(n dst.Node) bool {
		if as, ok := n.(*dst.AssignStmt); ok && len(as.Rhs) == 1 && as.Rhs[0] == call && len(as.Lhs) == 1 {
			app = p.objectOf(as.Lhs[0])
		}
		return app == nil
	})
	if app == nil {
		return nil
	}
	flags := map[string]*clause{}
	for _, fl := range t.c.tree.Root.cl.flags() {
		flags[fl.name] = fl
	}
	want := types.TypeString(t.c.rootFlags, nil)
	var holder dst.Expr
	var stmts []dst.Stmt
	ok := true
	dst.Inspect(f, func(n dst.Node) bool {
		s, isExpr := n.(*dst.ExprStmt)
		if !isExpr || !ok {
			return ok
		}
		outer, isCall := s.X.(*dst.CallExpr)
		if !isCall {
			return true
		}
		base, flagName := chainBase(outer)
		if base == nil || p.objectOf(base) != app || flagName == "" {
			return true
		}
		if flags[flagName] == nil || len(outer.Args) != 1 {
			ok = false
			return false
		}
		if !sameSettings(p, outer, flags[flagName]) {
			t.c.issue(p.position(s), "the test registers flag %q with other settings than the program, which the converted grammar has; port this test by hand", flagName)
			ok = false
			return false
		}
		u, isAddr := outer.Args[0].(*dst.UnaryExpr)
		if !isAddr {
			ok = false
			return false
		}
		sel, isSel := u.X.(*dst.SelectorExpr)
		if !isSel || types.TypeString(deref(p.typeOf(sel.X)), nil) != want {
			ok = false
			return false
		}
		if holder != nil && renderExact(holder) != renderExact(sel.X) {
			ok = false
			return false
		}
		holder = sel.X
		stmts = append(stmts, s)
		return true
	})
	if !ok || holder == nil {
		return nil
	}
	root := holder
	for {
		if sel, isSel := root.(*dst.SelectorExpr); isSel {
			root = sel.X
			continue
		}
		break
	}
	if id, isIdent := root.(*dst.Ident); isIdent {
		if v := p.objectOf(id); v != nil && p.astOf(call) != nil && v.Pos() > p.astOf(call).Pos() {
			t.c.issue(p.position(call), "the test declares %s after creating the application, where the converted grammar needs it; declare it first", id.Name)
			return nil
		}
	}
	for _, s := range stmts {
		t.drop[s] = true
	}
	if _, ptr := p.typeOf(holder).Underlying().(*types.Pointer); ptr {
		return dst.Clone(holder).(dst.Expr)
	}
	return &dst.UnaryExpr{Op: token.AND, X: dst.Clone(holder).(dst.Expr)}
}

// sameSettings reports whether a test's registration chain, such as
// app.Flag("debug", "...").Short('d').BoolVar(&x), gives the flag the
// default, environment variable, short form and requirement the program
// gave cl.
func sameSettings(p *Package, call *dst.CallExpr, cl *clause) bool {
	var defaults []string
	var envar string
	var short rune
	required := false
	for e := dst.Expr(call); ; {
		x, isCall := e.(*dst.CallExpr)
		if !isCall {
			break
		}
		sel, isSel := x.Fun.(*dst.SelectorExpr)
		if !isSel {
			break
		}
		var args []string
		for _, a := range x.Args {
			if tv, ok := p.tv(a); ok && tv.Value != nil && tv.Value.Kind() == constant.String {
				args = append(args, constant.StringVal(tv.Value))
			}
		}
		switch sel.Sel.Name {
		case "Default":
			defaults = append(args, defaults...)
		case "Envar":
			if len(args) == 1 {
				envar = args[0]
			}
		case "Short":
			if tv, ok := p.tv(x.Args[0]); ok && tv.Value != nil {
				if r, exact := constant.Int64Val(tv.Value); exact {
					short = rune(r)
				}
			}
		case "Required":
			required = true
		}
		e = sel.X
	}
	return slices.Equal(defaults, cl.defaults) && envar == cl.envar && short == cl.short && required == cl.required
}

// chainBase returns the variable a builder chain such as
// app.Flag("debug", "...").Short('d').BoolVar(&x) starts from, and the
// name given to its Flag call.
func chainBase(call *dst.CallExpr) (*dst.Ident, string) {
	name := ""
	var e dst.Expr = call
	for {
		switch x := e.(type) {
		case *dst.CallExpr:
			if sel, ok := x.Fun.(*dst.SelectorExpr); ok && sel.Sel.Name == "Flag" && len(x.Args) > 0 {
				if lit, ok := x.Args[0].(*dst.BasicLit); ok && lit.Kind == token.STRING {
					name, _ = strconv.Unquote(lit.Value)
				}
			}
			e = x.Fun
		case *dst.SelectorExpr:
			e = x.X
		case *dst.Ident:
			return x, name
		default:
			return nil, ""
		}
	}
}

// mirroredReflection reports whether call, reflect.ValueOf on a command
// clause, only reaches what kongcompat's CmdClause mirrors of kingpin's
// private fields: cmdMixin.flagGroup.long, the command's flags by name.
func (t *testConversion) mirroredReflection(f *dst.File, call *dst.CallExpr) bool {
	if n, ok := deref(t.p.typeOf(call.Args[0])).(*types.Named); !ok || n.Obj().Name() != "CmdClause" {
		return false
	}
	mirrored := map[string]bool{"cmdMixin": true, "flagGroup": true, "long": true}
	ok := true
	for _, d := range f.Decls {
		fd, isFunc := d.(*dst.FuncDecl)
		if !isFunc || !containsNode(fd, call) {
			continue
		}
		dst.Inspect(fd, func(n dst.Node) bool {
			c, isCall := n.(*dst.CallExpr)
			if !isCall {
				return true
			}
			if sel, isSel := c.Fun.(*dst.SelectorExpr); isSel && strings.HasPrefix(sel.Sel.Name, "Field") {
				lit, isLit := firstArgLit(c)
				if !isLit || sel.Sel.Name != "FieldByName" || !mirrored[lit] {
					ok = false
				}
			}
			return ok
		})
	}
	return ok
}

func firstArgLit(c *dst.CallExpr) (string, bool) {
	if len(c.Args) != 1 {
		return "", false
	}
	lit, ok := c.Args[0].(*dst.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}

// bindAtCall is converter.bindAtCall for tests, applied to any call that
// passes the application, whether the callee kept that parameter or not.
func (t *testConversion) bindAtCall(cur *dstutil.Cursor, s dst.Stmt) {
	call := stmtCall(s)
	if call == nil || cur.Index() < 0 {
		return
	}
	fn := t.c.g.in.staticCallee(t.p, call)
	if fn == nil {
		return
	}
	params := fn.Type().(*types.Signature).Params()
	if len(call.Args) != params.Len() {
		return
	}
	sig := t.pruned[t.key(fn.Pos())]
	if t.injected == nil {
		t.injected = map[string]bool{}
		for _, typ := range t.c.injected {
			t.injected[types.TypeString(typ, nil)] = true
		}
	}
	injected := t.injected
	app := -1
	var bound []int
	for i := range call.Args {
		typ := params.At(i).Type()
		switch {
		case t.c.isAppType(typ):
			app = i
		case !sig.params[i] && injected[types.TypeString(typ, nil)]:
			bound = append(bound, i)
		}
	}
	if app < 0 || len(bound) == 0 {
		return
	}
	appExpr, binds := t.c.sharedArgs(t.p, cur, s, call, app, bound, params)
	var typs []types.Type
	for _, i := range bound {
		typs = append(typs, params.At(i).Type())
	}
	for _, b := range t.c.bindCalls(t.p.position(call), binds, typs, t.p.PkgPath, methodOn(appExpr)) {
		cur.InsertBefore(&dst.ExprStmt{X: b})
	}
}

// prunedCallers returns the directories of the program whose tests call a
// function by the name of one that lost parameters.
func (c *converter) prunedCallers() map[string]bool {
	out := map[string]bool{}
	if len(c.pruned) == 0 {
		return out
	}
	for _, p := range c.prog.Packages {
		for _, f := range p.Syntax {
			dir := filepath.Dir(p.filename(f))
			if out[dir] {
				continue
			}
			tests, _ := filepath.Glob(filepath.Join(dir, "*_test.go"))
			for _, t := range tests {
				src, err := os.ReadFile(t)
				if err != nil {
					continue
				}
				for fn := range c.pruned {
					if bytes.Contains(src, []byte(fn.Name()+"(")) {
						out[dir] = true
					}
				}
			}
		}
	}
	return out
}

// onlyTests reports whether the Go files of dir are all tests.
func onlyTests(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if name := e.Name(); strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
			return false
		}
	}
	return true
}
