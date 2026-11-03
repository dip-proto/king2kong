package conv

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"slices"
	"strconv"
	"strings"

	"github.com/dave/dst"
	"github.com/dave/dst/dstutil"
)

// prunedSig records what pruneSignature removed from a function: the
// indexes of the parameters, and whether the results went too.
type prunedSig struct {
	params map[int]bool
	// nils are the parameters kept, the function being used as a value or
	// declared by an interface, whose argument becomes nil.
	nils    map[int]bool
	results bool
}

// convertRuntime ports the run time kingpin code of a program that parses
// outside the grammar's package onto kongcompat's kingpin-like API.
// kingpin.New builds the kong parser from the CLI, kingpin's types and
// functions become kongcompat's, and calls follow the functions that lost
// their kingpin parameters or results.
func (c *converter) convertRuntime() {
	if c.compatPath == "" {
		return
	}
	c.pointRemovedCalls()
	c.declareListCLIs()
	exports := c.compatExports()
	// Hook values are bound at kingpin.New when they are in scope there,
	// and otherwise where they were passed to the wiring code.
	for _, p := range c.prog.Packages {
		for _, f := range p.Syntax {
			dst.Inspect(f, func(n dst.Node) bool {
				call, ok := n.(*dst.CallExpr)
				if !ok {
					return true
				}
				if id, ok := call.Fun.(*dst.Ident); ok && id.Name == "New" && c.prog.isKingpinPath(id.Path) {
					for _, t := range c.injected {
						if c.variableOfType(p, call, t) != nil {
							c.boundAtNew = append(c.boundAtNew, t)
						}
					}
				}
				return true
			})
		}
	}
	for _, p := range c.prog.Packages {
		c.applyRuntime(p, func(cur *dstutil.Cursor) (bool, bool) {
			if n, ok := cur.Node().(*dst.CallExpr); ok {
				return c.followPrunedCall(p, n), true
			}
			return false, true
		}, func(cur *dstutil.Cursor) (bool, bool) {
			switch n := cur.Node().(type) {
			case *dst.ExprStmt:
				c.bindAtCall(p, cur, n)
				if call, ok := n.X.(*dst.CallExpr); ok && c.emptied[c.g.in.staticCallee(p, call)] {
					cur.Delete()
					return true, false
				}
			case *dst.AssignStmt:
				c.bindAtCall(p, cur, n)
				if c.followPrunedAssign(p, cur, n) {
					return true, false
				}
			}
			return false, true
		})
	}
	for _, p := range c.prog.Packages {
		c.applyRuntime(p, func(cur *dstutil.Cursor) (bool, bool) {
			switch n := cur.Node().(type) {
			case *dst.CallExpr:
				if id, ok := n.Fun.(*dst.Ident); ok && id.Name == "New" && c.prog.isKingpinPath(id.Path) {
					c.newApplication(p, cur, n)
					return true, false
				}
			case *dst.Ident:
				if n.Path == "" || !c.prog.isKingpinPath(n.Path) {
					return false, true
				}
				return c.compatName(p, n, exports), true
			}
			return false, true
		}, nil)
	}
}

// runtimeRenames are kingpin's package-level functions whose kongcompat
// version has another name, the name they have taking other arguments.
var runtimeRenames = map[string]string{"MustParse": "Must"}

// notCarried are kingpin's package-level names whose kongcompat namesakes
// mean something else, so they are reported instead.
var notCarried = map[string]bool{"Parse": true, "Usage": true, "HelpCommand": true}

// compatName moves a use of a kingpin package-level name, such as
// kingpin.MustParse, to its kongcompat equivalent, and tells whether there is
// one.
// When there is none, the use is reported, once per name, since the program
// would still depend on kingpin.
func (c *converter) compatName(p *Package, n *dst.Ident, exports map[string]bool) bool {
	if to, ok := runtimeRenames[n.Name]; ok {
		n.Name, n.Path = to, c.compatPath
		return true
	}
	if exports[n.Name] && !notCarried[n.Name] {
		n.Path = c.compatPath
		return true
	}
	if !c.unmapped[n.Name] {
		c.unmapped[n.Name] = true
		c.issue(p.position(n), "kingpin.%s has no kongcompat equivalent; port this use to kong by hand", n.Name)
	}
	return false
}

// applyRuntime walks the kept declarations of p, calling stmt (when not nil)
// and then pre on each node.
// Both return whether they changed something and whether to descend.
func (c *converter) applyRuntime(p *Package, pre, stmt func(*dstutil.Cursor) (bool, bool)) {
	for _, f := range p.Syntax {
		changed := false
		for _, d := range f.Decls {
			if c.ed.removed[d] {
				continue
			}
			dstutil.Apply(d, func(cur *dstutil.Cursor) bool {
				if stmt != nil {
					if ch, cont := stmt(cur); ch || !cont {
						changed = changed || ch
						if !cont {
							return false
						}
					}
				}
				ch, cont := pre(cur)
				changed = changed || ch
				return cont
			}, nil)
		}
		if changed {
			c.ed.touch(p, f)
		}
	}
}

// bindAtCall adds app.Bind(cfg) before a call such as Define(app, cfg),
// when the callee lost its application parameter or keeps a command list.
// Only the arguments whose types the hooks take are bound.
func (c *converter) bindAtCall(p *Package, cur *dstutil.Cursor, s dst.Stmt) {
	call := stmtCall(s)
	if call == nil {
		return
	}
	fn := c.g.in.staticCallee(p, call)
	sig, ok := c.pruned[fn]
	kept := c.cmdLists[fn] != nil
	if (!ok || len(sig.params) == 0) && !kept {
		return
	}
	params := fn.Type().(*types.Signature).Params()
	if len(call.Args) != params.Len() || cur.Index() < 0 {
		return
	}
	app := -1
	var bound []int
	for i := range call.Args {
		t := params.At(i).Type()
		switch {
		case (sig.params[i] || sig.nils[i] || kept) && c.isAppType(t):
			app = i
		case !sig.params[i] && c.isInjectedType(t) && !containsType(c.boundAtNew, t):
			bound = append(bound, i)
			c.boundAtCall = append(c.boundAtCall, t)
		}
	}
	if app < 0 || len(bound) == 0 {
		return
	}
	appExpr, binds := c.sharedArgs(p, cur, s, call, app, bound, params)
	var typs []types.Type
	for j, i := range bound {
		t := params.At(i).Type()
		typs = append(typs, t)
		// kong keeps the last value bound for a type, for every hook.
		key := renderExact(appExpr) + " " + types.TypeString(t, nil)
		if prev, ok := c.boundValues[key]; ok && prev != renderExact(binds[j]) {
			c.issue(p.position(call), "%s and %s are both bound for the hooks as %s, but kong keeps only the last one; set the value by hand", prev, renderExact(binds[j]), types.TypeString(t, shortQual))
		}
		c.boundValues[key] = renderExact(binds[j])
	}
	for _, b := range c.bindCalls(p.position(call), binds, typs, p.PkgPath, methodOn(appExpr)) {
		cur.InsertBefore(&dst.ExprStmt{X: b})
	}
}

// ifaceArg returns (*T)(nil) when t is an interface such as io.Writer, or nil.
// kong binds a value by its dynamic type, so one a hook takes as an
// interface has to be bound to that interface with BindTo.
// It reports false for an interface it can't write in from, the package
// the expression is written in: one unexported or generic, and an unnamed
// one tied to a package through its method types or unexported methods.
func ifaceArg(t types.Type, from string) (dst.Expr, bool) {
	if !types.IsInterface(t) {
		return nil, true
	}
	var typ dst.Expr
	switch t := types.Unalias(t).(type) {
	case *types.Named:
		var ok bool
		if typ, ok = typeAt(t, from); !ok {
			return nil, false
		}
	case *types.Interface:
		if t.Empty() {
			typ = dst.NewIdent("any")
			break
		}
		// One naming no package, as interface{ Write([]byte) (int, error) },
		// is the same type wherever it is written.
		// Unexported methods belong to their package, though.
		local := false
		text := types.TypeString(t, func(*types.Package) string {
			local = true
			return ""
		})
		for i := range t.NumMethods() {
			local = local || !t.Method(i).Exported()
		}
		if local {
			return nil, false
		}
		typ = dst.NewIdent(text)
	}
	return callOf(&dst.ParenExpr{X: &dst.StarExpr{X: typ}}, dst.NewIdent("nil")), true
}

// typeAt returns the expression of t as written in package from, if it can
// be written there.
func typeAt(t types.Type, from string) (dst.Expr, bool) {
	switch t := types.Unalias(t).(type) {
	case *types.Basic:
		return dst.NewIdent(t.Name()), true
	case *types.Named:
		id := dst.NewIdent(t.Obj().Name())
		if pkg := t.Obj().Pkg(); pkg != nil && pkg.Path() != from {
			if !t.Obj().Exported() || !importable(pkg.Path(), from) {
				return nil, false
			}
			id.Path = pkg.Path()
		}
		return id, t.TypeArgs().Len() == 0
	case *types.Pointer:
		elem, ok := typeAt(t.Elem(), from)
		return &dst.StarExpr{X: elem}, ok
	case *types.Slice:
		elem, ok := typeAt(t.Elem(), from)
		return &dst.ArrayType{Elt: elem}, ok
	case *types.Map:
		key, ok1 := typeAt(t.Key(), from)
		elem, ok2 := typeAt(t.Elem(), from)
		return &dst.MapType{Key: key, Value: elem}, ok1 && ok2
	}
	return nil, false
}

// asParam gives a constant or nil argument the type of the parameter it
// was passed to, as time.Duration(5) for Define(app, 5).
// Bound as it is, 5 would be an int and nil would have no type at all.
// arg is the argument in the call, and bound the copy to bind.
// It reports false when that type can't be written in p.
func asParam(p *Package, arg, bound dst.Expr, t types.Type) (dst.Expr, bool) {
	tv, ok := p.tv(arg)
	if !ok || tv.Value == nil && !tv.IsNil() || types.IsInterface(t) {
		return bound, true
	}
	if n := naturalType(p, arg); n != nil && types.Identical(n, t) {
		return bound, true
	}
	typ, ok := typeAt(t, p.PkgPath)
	if !ok {
		return bound, false
	}
	if _, ptr := typ.(*dst.StarExpr); ptr {
		typ = &dst.ParenExpr{X: typ}
	}
	return callOf(typ, bound), true
}

// naturalType returns the type a constant has on its own, as int for 5, or
// nil when it is not a plain literal or named constant.
func naturalType(p *Package, e dst.Expr) types.Type {
	switch e := unparen(e).(type) {
	case *dst.BasicLit:
		kinds := map[token.Token]types.BasicKind{token.INT: types.Int, token.FLOAT: types.Float64, token.IMAG: types.Complex128, token.CHAR: types.Rune, token.STRING: types.String}
		if k, ok := kinds[e.Kind]; ok {
			return types.Typ[k]
		}
	case *dst.Ident:
		if k, ok := p.objectOf(e).(*types.Const); ok {
			return types.Default(k.Type())
		}
	}
	return nil
}

// importable reports whether package from may import path, which is not
// the case for an internal package outside its parent.
func importable(path, from string) bool {
	p := "/" + path + "/"
	i := strings.LastIndex(p, "/internal/")
	if i < 0 {
		return true
	}
	root := strings.TrimPrefix(p[:i], "/")
	return root == "" || from == root || strings.HasPrefix(from, root+"/")
}

// bindCalls returns the calls binding values, of types typs, for the hooks:
// one Bind for most, and a BindTo for each interface.
// call builds each one from its name, as kong.Bind or app.Bind.
// pos is where to report what cannot be bound.
func (c *converter) bindCalls(pos token.Position, values []dst.Expr, typs []types.Type, from string, call func(name string, args ...dst.Expr) dst.Expr) []dst.Expr {
	var out, plain []dst.Expr
	for i, v := range values {
		iface, ok := ifaceArg(typs[i], from)
		if !ok {
			c.issue(pos, "the hooks take %s as %s, an interface kong can only bind by its concrete type here; bind it with kong.BindTo by hand", render(v), types.TypeString(typs[i], shortQual))
		}
		if iface != nil {
			out = append(out, call("BindTo", v, iface))
			continue
		}
		plain = append(plain, v)
	}
	if len(plain) > 0 {
		out = append([]dst.Expr{call("Bind", plain...)}, out...)
	}
	return out
}

// kongOption builds a kong option call, as kong.Bind(x).
func kongOption(name string, args ...dst.Expr) dst.Expr {
	return callOf(kongIdent(name), args...)
}

// methodOn builds calls of methods of app, as app.Bind(x).
func methodOn(app dst.Expr) func(name string, args ...dst.Expr) dst.Expr {
	return func(name string, args ...dst.Expr) dst.Expr {
		return callOf(&dst.SelectorExpr{X: dst.Clone(app).(dst.Expr), Sel: dst.NewIdent(name)}, args...)
	}
}

// stmtCall is the call a statement makes, as f(x) or v := f(x), or nil.
func stmtCall(s dst.Stmt) *dst.CallExpr {
	var call *dst.CallExpr
	switch s := s.(type) {
	case *dst.ExprStmt:
		call, _ = s.X.(*dst.CallExpr)
	case *dst.AssignStmt:
		if len(s.Rhs) == 1 {
			call, _ = s.Rhs[0].(*dst.CallExpr)
		}
	}
	return call
}

// sharedArgs returns copies of the application argument and of the
// arguments at indexes, to be used a second time before call.
// Arguments that are not plain variables, such as &Config{} or getApp(),
// are first moved into variables named after their parameters, so every
// argument is still evaluated once and in its original order.
// Constants and nil get the type of their parameter.
func (c *converter) sharedArgs(p *Package, cur *dstutil.Cursor, s dst.Stmt, call *dst.CallExpr, app int, indexes []int, params *types.Tuple) (dst.Expr, []dst.Expr) {
	last := app
	for _, i := range indexes {
		last = max(last, i)
	}
	for i := 0; i <= last; i++ {
		a := call.Args[i]
		if plainExpr(a) {
			continue
		}
		// Constants have no side effects, and as variables they would lose
		// the type the parameter gives them.
		if tv, ok := p.tv(a); ok && (tv.Value != nil || tv.IsNil()) {
			continue
		}
		name := c.freeLocalAt(p, cur, s, params.At(i).Name(), "value")
		cur.InsertBefore(&dst.AssignStmt{Lhs: []dst.Expr{dst.NewIdent(name)}, Tok: token.DEFINE, Rhs: []dst.Expr{a}})
		call.Args[i] = dst.NewIdent(name)
	}
	var out []dst.Expr
	for _, i := range indexes {
		t := params.At(i).Type()
		a, ok := asParam(p, call.Args[i], dst.Clone(call.Args[i]).(dst.Expr), t)
		if !ok {
			c.issue(p.position(call), "%s is bound for the hooks as %s, a type that can't be written here; bind it by hand", render(a), types.TypeString(t, shortQual))
		}
		out = append(out, a)
	}
	return dst.Clone(call.Args[app]).(dst.Expr), out
}

// freeLocalAt picks a name for a variable declared before s: the first
// candidate, or the first one followed by a number, that s's block does not
// declare anywhere and that the conversion has not already used there.
func (c *converter) freeLocalAt(p *Package, cur *dstutil.Cursor, s dst.Stmt, candidates ...string) string {
	if c.declared[cur.Parent()] == nil {
		c.declared[cur.Parent()] = map[string]bool{}
	}
	used := c.declared[cur.Parent()]
	var block *types.Scope
	if a := p.astOf(s); a != nil {
		block = p.Types.Scope().Innermost(a.Pos())
	}
	taken := func(name string) bool {
		return name == "" || name == "_" || used[name] || c.freeLocal(p, s, name) != name ||
			block != nil && block.Lookup(name) != nil
	}
	candidates = slices.DeleteFunc(candidates, func(name string) bool { return name == "" || name == "_" })
	name := firstFree(taken, candidates...)
	used[name] = true
	return name
}

// plainExpr reports whether e only reads variables, as x, x.y or &x.y do.
func plainExpr(e dst.Expr) bool {
	switch e := e.(type) {
	case *dst.Ident:
		return true
	case *dst.SelectorExpr:
		return plainExpr(e.X)
	case *dst.UnaryExpr:
		return e.Op == token.AND && plainExpr(e.X)
	case *dst.ParenExpr:
		return plainExpr(e.X)
	}
	return false
}

func (c *converter) isInjectedType(t types.Type) bool {
	return containsType(c.injected, t)
}

func containsType(list []types.Type, t types.Type) bool {
	return slices.ContainsFunc(list, func(i types.Type) bool { return types.Identical(i, t) })
}

// newApplication replaces kingpin.New(name, help) with kongcompat.New on the
// CLI, pointing its embedded flags at the program's own struct and binding
// the values the commands' hooks take.
func (c *converter) newApplication(p *Package, cur *dstutil.Cursor, call *dst.CallExpr) {
	root := c.tree.RootPkg
	cli := &dst.CompositeLit{Type: &dst.Ident{Name: c.cliName, Path: c.qualifier(p, root)}}
	if c.rootFlags != nil {
		holder := c.flagsHolderAt(p, call)
		if holder == nil {
			c.issue(p.position(call), "the CLI embeds %s, but no expression for the program's own %s is at hand here; set it in the CLI literal by hand", c.rootFlags.Obj().Name(), c.rootFlags.Obj().Name())
		} else {
			cli.Elts = append(cli.Elts, &dst.KeyValueExpr{Key: dst.NewIdent(c.rootFlags.Obj().Name()), Value: holder})
		}
	}
	args := []dst.Expr{&dst.UnaryExpr{Op: token.AND, X: cli}}
	args = append(args, call.Args...)
	var binds []dst.Expr
	var typs []types.Type
	for _, t := range c.injected {
		if containsType(c.boundAtCall, t) {
			continue
		}
		v := c.variableOfType(p, call, t)
		if v == nil {
			c.issue(p.position(call), "the commands' hooks take a value of type %s, but no single variable of that type is at hand here; add it to kong.Bind by hand", types.TypeString(t, shortQual))
			continue
		}
		binds, typs = append(binds, dst.NewIdent(v.Name())), append(typs, t)
	}
	args = append(args, c.bindCalls(p.position(call), binds, typs, p.PkgPath, kongOption)...)
	args = append(args, c.newOptions(c.qualifier(p, root))...)
	newCall := callOf(c.compatIdent("New"), args...)
	c.parserCalls = append(c.parserCalls, newCall)
	if len(args) > 3 {
		for _, a := range args[1:] {
			a.Decorations().Before = dst.NewLine
		}
		args[len(args)-1].Decorations().After = dst.NewLine
	}
	cur.Replace(newCall)
}

// newOptions returns the options kongcompat.New takes after the bindings:
// the conditional commands and the help flag settings.
// rootPath qualifies the root package's names.
func (c *converter) newOptions(rootPath string) []dst.Expr {
	var opts []dst.Expr
	if c.conditionalName != "" {
		opts = append(opts, callOf(&dst.Ident{Name: c.conditionalName, Path: rootPath}))
	}
	if short := c.helpShort(); short != 0 {
		// kongcompat.New, like kingpin, gives --help no short form.
		opts = append(opts, callOf(c.compatIdent("HelpShortFlag"), &dst.BasicLit{Kind: token.CHAR, Value: strconv.QuoteRune(short)}))
	}
	if c.helpHidden() {
		opts = append(opts, callOf(c.compatIdent("HideHelp")))
	}
	return opts
}

// addCommandNamesOption adds the CommandNames option to the parsers built
// in main, so they fill the command name fields from convertCommandNames.
// That pass runs after the parsers are built, hence this separate step.
// kongcompat.New fills those fields by itself.
func (c *converter) addCommandNamesOption() {
	if !c.commandNames || c.main == nil {
		return
	}
	for _, call := range c.parserCalls {
		opt := callOf(c.compatIdent("CommandNames"))
		if len(call.Args) > 0 && call.Args[len(call.Args)-1].Decorations().After == dst.NewLine {
			opt.Decs.Before, opt.Decs.After = dst.NewLine, dst.NewLine
			call.Args[len(call.Args)-1].Decorations().After = dst.None
		}
		call.Args = append(call.Args, opt)
	}
}

// qualifier returns the import path for naming something of package to from
// p, or "" when they are the same package.
func (c *converter) qualifier(p, to *Package) string {
	if p == to {
		return ""
	}
	return to.PkgPath
}

// flagsHolderAt returns the address of the struct the application flags
// were registered into, as an expression valid at call.
// It comes from a registration in the same function: &data.Flags from
// BoolVar(&data.Flags.Verbose).
func (c *converter) flagsHolderAt(p *Package, call *dst.CallExpr) dst.Expr {
	fn := enclosingFunc(p, call)
	if fn == nil {
		return nil
	}
	for _, b := range c.plan.bindings {
		if b.cmd.kind != kindApp || b.cl.dstExpr == nil || b.cl.dstPkg != p {
			continue
		}
		if enclosingFunc(p, b.cl.dstExpr) != fn {
			continue
		}
		u, ok := b.cl.dstExpr.(*dst.UnaryExpr)
		if !ok || u.Op != token.AND {
			continue
		}
		sel, ok := u.X.(*dst.SelectorExpr)
		if !ok {
			continue
		}
		if id, ok := sel.X.(*dst.Ident); ok && !c.declaredBefore(p, id, call) {
			return nil
		}
		if _, ptr := p.typeOf(sel.X).Underlying().(*types.Pointer); ptr {
			return dst.Clone(sel.X).(dst.Expr)
		}
		return &dst.UnaryExpr{Op: token.AND, X: dst.Clone(sel.X).(dst.Expr)}
	}
	return nil
}

// declaredBefore reports whether id's variable is declared before call.
// A declaration later in the same block that only sets a plain value, such
// as var flags Flags or flags := &Flags{}, is moved up right before the
// statement holding call.
func (c *converter) declaredBefore(p *Package, id *dst.Ident, call *dst.CallExpr) bool {
	o, a := p.objectOf(id), p.astOf(call)
	if o == nil || a == nil || o.Pos() < a.Pos() {
		return true
	}
	var moved bool
	for _, f := range p.Syntax {
		dst.Inspect(f, func(n dst.Node) bool {
			b, ok := n.(*dst.BlockStmt)
			if !ok || moved {
				return !moved
			}
			at, decl := -1, -1
			for i, s := range b.List {
				if containsNode(s, call) {
					at = i
				}
				if c.declaresOnly(p, s, o) {
					decl = i
				}
			}
			if at < 0 || decl <= at {
				return true
			}
			s := b.List[decl]
			list := append([]dst.Stmt{}, b.List[:at]...)
			list = append(list, s)
			list = append(list, b.List[at:decl]...)
			b.List = append(list, b.List[decl+1:]...)
			moved = true
			c.ed.touch(p, f)
			return false
		})
		if moved {
			return true
		}
	}
	return false
}

// declaresOnly reports whether s declares o with its zero value or a
// composite literal of constants, which can run earlier.
func (c *converter) declaresOnly(p *Package, s dst.Stmt, o types.Object) bool {
	d := onlyDefinition(s)
	if d == nil || p.objectOf(d.names[0]) != o {
		return false
	}
	pure := true
	for _, v := range d.values {
		dst.Inspect(v, func(n dst.Node) bool {
			switch n := n.(type) {
			case *dst.CallExpr:
				pure = false
			case *dst.Ident:
				if obj := p.objectOf(n); obj != nil {
					if _, isVar := obj.(*types.Var); isVar && !obj.(*types.Var).IsField() {
						pure = false
					}
				}
			}
			return pure
		})
	}
	return pure
}

// containsNode reports whether n is inside root.
func containsNode(root, n dst.Node) bool {
	found := false
	dst.Inspect(root, func(m dst.Node) bool {
		if m == n {
			found = true
		}
		return !found
	})
	return found
}

// enclosingFunc returns the function declaration n is in.
func enclosingFunc(p *Package, n dst.Node) *ast.FuncDecl {
	a := p.astOf(n)
	if a == nil {
		return nil
	}
	for _, f := range p.Package.Package.Syntax {
		if a.Pos() < f.Pos() || a.Pos() > f.End() {
			continue
		}
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && a.Pos() >= fd.Pos() && a.Pos() <= fd.End() {
				return fd
			}
		}
	}
	return nil
}

// variableOfType returns the only variable of type t that can be named
// at n, or nil.
func (c *converter) variableOfType(p *Package, n dst.Node, t types.Type) *types.Var {
	return onlyVarAt(p, n, func(v *types.Var) bool { return types.Identical(v.Type(), t) })
}

// onlyVarAt returns the variable matching match that can be named at n, in
// the innermost scope holding one, when that scope holds only one.
func onlyVarAt(p *Package, n dst.Node, match func(*types.Var) bool) *types.Var {
	a := p.astOf(n)
	if a == nil {
		return nil
	}
	inner := p.Types.Scope().Innermost(a.Pos())
	// Only a variable that its name still reaches at n, not hidden by a
	// nearer one.
	visible := func(v *types.Var) bool {
		_, obj := inner.LookupParent(v.Name(), a.Pos())
		return obj == v
	}
	for scope := inner; scope != nil && scope != types.Universe; scope = scope.Parent() {
		var found []*types.Var
		for _, name := range scope.Names() {
			v, ok := scope.Lookup(name).(*types.Var)
			if ok && match(v) && visible(v) {
				found = append(found, v)
			}
		}
		if len(found) == 1 {
			return found[0]
		}
		if len(found) > 1 {
			return nil
		}
	}
	return nil
}

// followPrunedCall drops the arguments of the parameters the callee lost.
func (c *converter) followPrunedCall(p *Package, call *dst.CallExpr) bool {
	fn := c.g.in.staticCallee(p, call)
	sig, ok := c.pruned[fn]
	if !ok || len(sig.params) == 0 && len(sig.nils) == 0 {
		return false
	}
	// A method expression, as in Builder.Setup(b, app), takes the receiver
	// first; a variadic tail can have any number of arguments, none pruned.
	shift := methodExprShift(p, call)
	params := fn.Type().(*types.Signature)
	if n := params.Params().Len() + shift; len(call.Args) != n && !(params.Variadic() && len(call.Args) >= n-1) {
		return false
	}
	var args []dst.Expr
	for i, a := range call.Args {
		switch {
		case i < shift:
			args = append(args, a)
		case sig.params[i-shift]:
		case sig.nils[i-shift]:
			args = append(args, dst.NewIdent("nil"))
		default:
			args = append(args, a)
		}
	}
	call.Args = args
	return true
}

// followPrunedAssign handles x := f(...) when f no longer returns what x
// held: a list of command structs, which now comes from the parser.
func (c *converter) followPrunedAssign(p *Package, cur *dstutil.Cursor, as *dst.AssignStmt) bool {
	if len(as.Rhs) != 1 || len(as.Lhs) != 1 {
		return false
	}
	call, ok := as.Rhs[0].(*dst.CallExpr)
	if !ok {
		return false
	}
	fn := c.g.in.staticCallee(p, call)
	sig, ok := c.pruned[fn]
	if !ok || !sig.results {
		return false
	}
	orig := fn.Type().(*types.Signature)
	slice, ok := orig.Results().At(0).Type().(*types.Slice)
	if !ok || orig.Results().Len() != 1 {
		c.issue(p.position(as), "%s no longer returns anything; this use of its result has to be ported by hand", fn.Name())
		return false
	}
	// The commands now come from the application the call received.
	var app dst.Expr
	for i, a := range call.Args {
		if i < orig.Params().Len() && c.isAppType(orig.Params().At(i).Type()) {
			app = dst.Clone(a).(dst.Expr)
		}
	}
	if app == nil {
		c.issue(p.position(as), "%s no longer returns its commands, and no application is at hand to list them; port this by hand", fn.Name())
		return false
	}
	if cur.Index() < 0 && !c.emptied[fn] {
		// Not in a statement list, such as the init of an if statement.
		c.issue(p.position(as), "%s no longer returns its commands; move this call to a statement of its own, or port it by hand", fn.Name())
		return false
	}
	c.followPrunedCall(p, call)
	if !c.emptied[fn] {
		cur.InsertBefore(&dst.ExprStmt{X: call})
	}
	list := &dst.IndexExpr{X: c.compatIdent("Commands"), Index: typeNode(slice.Elem())}
	as.Rhs[0] = callOf(list, app)
	return true
}

// typeNode builds the expression of a named type, or a pointer to one.
func typeNode(t types.Type) dst.Expr {
	switch t := t.(type) {
	case *types.Pointer:
		return &dst.StarExpr{X: typeNode(t.Elem())}
	case *types.Named:
		path := ""
		if t.Obj().Pkg() != nil {
			path = t.Obj().Pkg().Path()
		}
		return &dst.Ident{Name: t.Obj().Name(), Path: path}
	}
	return dst.NewIdent(types.TypeString(t, nil))
}

// compatExports returns the exported top-level names of kongcompat.
func (c *converter) compatExports() map[string]bool {
	if c.exports != nil {
		return c.exports
	}
	out := map[string]bool{}
	c.exports = out
	entries, err := fs.ReadDir(c.opts.Compat, ".")
	if err != nil {
		return out
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := fs.ReadFile(c.opts.Compat, name)
		if err != nil {
			continue
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			continue
		}
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil && d.Name.IsExported() {
					out[d.Name.Name] = true
				}
			case *ast.GenDecl:
				for _, s := range d.Specs {
					switch s := s.(type) {
					case *ast.TypeSpec:
						if s.Name.IsExported() {
							out[s.Name.Name] = true
						}
					case *ast.ValueSpec:
						for _, n := range s.Names {
							if n.IsExported() {
								out[n.Name] = true
							}
						}
					}
				}
			}
		}
	}
	return out
}
