package conv

import (
	"go/token"
	"go/types"
	"slices"

	"github.com/dave/dst"
	"github.com/dave/dst/dstutil"
)

// cmdList is a function like Define(app, cfg) []Command, which registers
// commands on app and returns their structs.
//
// Its lists are kept, with each element pointing at the command's struct
// inside the CLI, while the registrations go.
type cmdList struct {
	pkg   *Package
	file  *dst.File
	fd    *dst.FuncDecl
	app   string
	local string
}

// findCommandLists rewrites the command lists of a program that parses
// outside the grammar's package.
func (c *converter) findCommandLists() {
	if c.main != nil || c.compatPath == "" {
		return
	}
	for _, ci := range c.keptCtors {
		c.attachKeptCtor(ci)
	}
	for _, p := range c.prog.Packages {
		if !c.canNameCLI(p) {
			continue
		}
		for _, f := range p.Syntax {
			for _, d := range f.Decls {
				if fd, ok := d.(*dst.FuncDecl); ok && fd.Body != nil {
					c.findCommandList(p, f, fd)
				}
			}
		}
	}
}

func (c *converter) findCommandList(p *Package, f *dst.File, fd *dst.FuncDecl) {
	fn, ok := p.objectOf(fd.Name).(*types.Func)
	if !ok {
		return
	}
	// Find which result holds the commands; the others may be an error.
	sig := fn.Type().(*types.Signature)
	var listType types.Type
	dst.Inspect(fd.Body, func(n dst.Node) bool {
		if _, ok := n.(*dst.FuncLit); ok {
			return false
		}
		if ret, ok := n.(*dst.ReturnStmt); ok && listType == nil {
			vals := c.g.in.stmtVals[ret]
			for i := 0; i < sig.Results().Len() && i < len(vals); i++ {
				if _, isSlice := sig.Results().At(i).Type().Underlying().(*types.Slice); isSlice && c.isGrammarList(vals[i]) {
					listType = sig.Results().At(i).Type()
				}
			}
		}
		return true
	})
	if listType == nil {
		return
	}
	var app *dst.Ident
	for _, field := range fd.Type.Params.List {
		for _, nm := range field.Names {
			if o := p.objectOf(nm); o != nil && nm.Name != "_" && c.isAppType(o.Type()) {
				app = nm
			}
		}
	}
	if app == nil {
		return
	}
	var lits []*dst.CompositeLit
	var stmts []dst.Stmt
	listed := false
	ok = true
	dst.Inspect(fd.Body, func(n dst.Node) bool {
		switch n := n.(type) {
		case *dst.FuncLit:
			return false
		case *dst.CompositeLit:
			if t := p.typeOf(n); t != nil && types.Identical(t, listType) {
				lits = append(lits, n)
			}
		case *dst.ReturnStmt:
			stmts = append(stmts, n)
			listed = true
		case *dst.AssignStmt:
			for _, l := range n.Lhs {
				if o := p.objectOf(l); o != nil && types.Identical(o.Type(), listType) {
					stmts = append(stmts, n)
					break
				}
			}
		case *dst.DeclStmt:
			dst.Inspect(n, func(m dst.Node) bool {
				if vs, isSpec := m.(*dst.ValueSpec); isSpec {
					for _, nm := range vs.Names {
						if o := p.objectOf(nm); o != nil && types.Identical(o.Type(), listType) {
							stmts = append(stmts, n)
							return false
						}
					}
				}
				return true
			})
		}
		return true
	})
	if !listed || len(lits) == 0 {
		return
	}
	local := c.freeLocalIn(fd, "cli", "kongCLI")
	rewritten := map[*dst.CompositeLit][]dst.Expr{}
	for _, lit := range lits {
		var elts []dst.Expr
		for _, e := range lit.Elts {
			ptr, isPtr := c.g.in.eltVals[e].(vPtr)
			if !isPtr {
				return
			}
			path, found := c.cmdStructPath(ptr.o)
			if !found {
				return
			}
			elt := &dst.UnaryExpr{Op: token.AND, X: pathExpr(local + path)}
			elt.Decs.NodeDecs = *e.Decorations()
			elts = append(elts, elt)
		}
		rewritten[lit] = elts
	}
	locals := c.usedCommandLocals(p, fd, lits)
	for lit, elts := range rewritten {
		lit.Elts = elts
	}
	stmts = append(stmts, pointLocals(locals, local)...)
	for _, s := range stmts {
		c.keepStmts[s] = true
	}
	c.listParams[p.objectOf(app)] = true
	c.cmdLists[fn] = &cmdList{pkg: p, file: f, fd: fd, app: app.Name, local: local}
}

// commandLocal is a local variable holding a command struct, declared as
// def.names[index].
type commandLocal struct {
	def   *definition
	index int
	path  string
	used  bool
}

// definition is a := statement, or one spec of a var statement.
type definition struct {
	stmt   dst.Stmt
	names  []dst.Expr
	values []dst.Expr
	set    func(names, values []dst.Expr)
}

// usedCommandLocals finds the local variables of fd holding a command
// struct, and marks those that code other than the lists and the
// registrations still uses, as in fmt.Println(first.Value()).
func (c *converter) usedCommandLocals(p *Package, fd *dst.FuncDecl, lits []*dst.CompositeLit) map[types.Object]*commandLocal {
	inList := map[dst.Node]bool{}
	for _, lit := range lits {
		for _, e := range lit.Elts {
			dst.Inspect(e, func(n dst.Node) bool {
				inList[n] = true
				return true
			})
		}
	}
	locals := map[types.Object]*commandLocal{}
	record := func(d *definition) {
		for i, name := range d.names {
			o := p.objectOf(name)
			ptr, ok := c.g.in.varVals[o].(vPtr)
			if !ok || !c.isInstance(ptr.o) {
				continue
			}
			if path, ok := c.cmdStructPath(ptr.o); ok {
				locals[o] = &commandLocal{def: d, index: i, path: path}
			}
		}
	}
	dst.Inspect(fd.Body, func(n dst.Node) bool {
		if _, ok := n.(*dst.FuncLit); ok {
			return false
		}
		if s, ok := n.(dst.Stmt); ok {
			for _, d := range definitions(s) {
				record(d)
			}
		}
		return true
	})
	var walk func(n dst.Node, registration bool)
	walk = func(n dst.Node, registration bool) {
		dst.Inspect(n, func(m dst.Node) bool {
			if s, isStmt := m.(dst.Stmt); isStmt && m != n && c.g.touched[s] && !c.keepStmts[s] {
				walk(m, true)
				return false
			}
			id, isIdent := m.(*dst.Ident)
			if !isIdent || inList[m] || registration {
				return true
			}
			if l := locals[p.objectOf(id)]; l != nil && l.def.names[l.index] != id {
				l.used = true
			}
			return true
		})
	}
	walk(fd.Body, false)
	return locals
}

// pointLocals makes the command variables still in use point into the CLI,
// as in first := &cli.First, and drops the unused ones.
// It returns the statements to keep.
func pointLocals(locals map[types.Object]*commandLocal, cli string) []dst.Stmt {
	byDef := map[*definition][]*commandLocal{}
	kept := map[dst.Stmt]bool{}
	for _, l := range locals {
		byDef[l.def] = append(byDef[l.def], l)
		kept[l.def.stmt] = kept[l.def.stmt] || l.used
	}
	for d, ls := range byDef {
		if !kept[d.stmt] {
			continue
		}
		point := map[int]string{}
		used := map[int]bool{}
		for _, l := range ls {
			point[l.index], used[l.index] = l.path, l.used
		}
		// first, second := pair(app) can only be rewritten when both are
		// commands.
		if len(d.values) != len(d.names) && len(point) != len(d.names) {
			continue
		}
		var names, values []dst.Expr
		for i, name := range d.names {
			path, isCmd := point[i]
			switch {
			case isCmd && used[i]:
				names = append(names, name)
				values = append(values, &dst.UnaryExpr{Op: token.AND, X: pathExpr(cli + path)})
			case !isCmd:
				names = append(names, name)
				values = append(values, d.values[i])
			}
		}
		d.set(names, values)
	}
	var out []dst.Stmt
	for s, keep := range kept {
		if !keep {
			continue
		}
		// Dropping names may have left empty var specs.
		if ds, ok := s.(*dst.DeclStmt); ok {
			gd := ds.Decl.(*dst.GenDecl)
			var specs []dst.Spec
			for _, sp := range gd.Specs {
				if len(sp.(*dst.ValueSpec).Names) > 0 {
					specs = append(specs, sp)
				}
			}
			gd.Specs = specs
		}
		out = append(out, s)
	}
	return out
}

// isGrammarList reports whether v is a list of command structs.
func (c *converter) isGrammarList(v value) bool {
	sl, ok := v.(vSlice)
	if !ok || len(sl.elems) == 0 {
		return false
	}
	for _, e := range sl.elems {
		if !c.isGrammarValue(e) {
			return false
		}
	}
	return true
}

// cmdStructPath returns the selector path from the CLI to the struct of the
// command o is an instance of, such as .Compute.Build.
func (c *converter) cmdStructPath(o *object) (string, bool) {
	if c.nodeOf == nil {
		c.nodeOf = map[*object]*node{}
		for _, n := range c.tree.nodes {
			if n.cl.instance != nil && n.cl.kind == kindCommand {
				c.nodeOf[n.cl.instance] = n
			}
		}
	}
	target := c.nodeOf[o]
	if target == nil || target.typ == nil {
		return "", false
	}
	path := ""
	if len(target.children) > 0 && !target.containerOwn {
		// A struct generated in the root package holds the subcommands and
		// embeds the command's own.
		path = "." + target.typ.Obj().Name()
	}
	for n := target; n.parent != nil; n = n.parent {
		path = "." + n.field + path
	}
	return path, true
}

// pointRemovedCalls replaces the calls to removed constructors that kept code
// still makes, as in a helper returning newFirst(app), 7, with the command
// the CLI now holds: &cli.First.
func (c *converter) pointRemovedCalls() {
	results := map[*dst.CallExpr]*object{}
	for _, s := range c.g.in.sites {
		if prev, seen := results[s.call]; seen && prev != s.result {
			results[s.call] = nil
		} else {
			results[s.call] = s.result
		}
	}
	for _, p := range c.prog.Packages {
		for _, f := range p.Syntax {
			for _, d := range f.Decls {
				fd, ok := d.(*dst.FuncDecl)
				if !ok || fd.Body == nil || c.ed.removed[d] {
					continue
				}
				c.pointCallsIn(p, f, fd, results)
			}
		}
	}
}

func (c *converter) pointCallsIn(p *Package, f *dst.File, fd *dst.FuncDecl, results map[*dst.CallExpr]*object) {
	var app *types.Var
	for _, field := range fd.Type.Params.List {
		for _, nm := range field.Names {
			if v, ok := p.objectOf(nm).(*types.Var); ok && nm.Name != "_" && c.isAppType(v.Type()) {
				app = v
			}
		}
	}
	listCLI := ""
	if fn, _ := p.objectOf(fd.Name).(*types.Func); c.cmdLists[fn] != nil {
		listCLI = c.cmdLists[fn].local
	}
	dstutil.Apply(fd.Body, func(cur *dstutil.Cursor) bool {
		call, ok := cur.Node().(*dst.CallExpr)
		if !ok || !c.removedCtors[c.g.in.staticCallee(p, call)] {
			return true
		}
		path, found := "", false
		if o := results[call]; o != nil {
			path, found = c.cmdStructPath(o)
		}
		// The application the call was given leads to the CLI; other
		// arguments must have nothing to compute.
		var given dst.Expr
		for _, arg := range call.Args {
			if c.isAppType(p.typeOf(arg)) {
				given = arg
			} else if callsBesides(p, c.g.in, arg) {
				found = false
			}
		}
		var cli dst.Expr
		switch {
		case given != nil:
			cli = &dst.TypeAssertExpr{
				X:    &dst.SelectorExpr{X: given, Sel: dst.NewIdent("Grammar")},
				Type: &dst.StarExpr{X: &dst.Ident{Name: c.cliName, Path: c.qualifier(p, c.tree.RootPkg)}},
			}
		case listCLI != "":
			cli = dst.NewIdent(listCLI)
		case app != nil && c.namesAt(p, call, app):
			cli = &dst.TypeAssertExpr{
				X:    &dst.SelectorExpr{X: dst.NewIdent(app.Name()), Sel: dst.NewIdent("Grammar")},
				Type: &dst.StarExpr{X: &dst.Ident{Name: c.cliName, Path: c.qualifier(p, c.tree.RootPkg)}},
			}
		}
		if !found || cli == nil || !c.canNameCLI(p) {
			c.issue(p.position(call), "%s calls %s, which no longer exists; use the command the CLI holds instead", fd.Name.Name, render(call.Fun))
			return false
		}
		cur.Replace(&dst.UnaryExpr{Op: token.AND, X: selectPath(cli, path)})
		c.ed.touch(p, f)
		return false
	}, nil)
}

// namesAt reports whether v's name means v at node n.
func (c *converter) namesAt(p *Package, n dst.Node, v *types.Var) bool {
	a := p.astOf(n)
	if a == nil {
		return false
	}
	scope := p.Types.Scope().Innermost(a.Pos())
	if scope == nil {
		return false
	}
	_, obj := scope.LookupParent(v.Name(), a.Pos())
	return obj == v
}

// declareListCLIs adds cli := app.Grammar.(*CLI) at the top of each function
// keeping command lists.
func (c *converter) declareListCLIs() {
	for _, l := range c.cmdLists {
		cliType := &dst.StarExpr{X: &dst.Ident{Name: c.cliName, Path: c.qualifier(l.pkg, c.tree.RootPkg)}}
		decl := &dst.AssignStmt{
			Lhs: []dst.Expr{dst.NewIdent(l.local)},
			Tok: token.DEFINE,
			Rhs: []dst.Expr{&dst.TypeAssertExpr{
				X:    &dst.SelectorExpr{X: dst.NewIdent(l.app), Sel: dst.NewIdent("Grammar")},
				Type: cliType,
			}},
		}
		decl.Decs.After = dst.EmptyLine
		l.fd.Body.List = append([]dst.Stmt{decl}, l.fd.Body.List...)
		c.ed.touch(l.pkg, l.file)
	}
}

// freeLocalIn returns the first candidate no identifier of fd uses.
func (c *converter) freeLocalIn(fd *dst.FuncDecl, candidates ...string) string {
	used := map[string]bool{}
	dst.Inspect(fd, func(n dst.Node) bool {
		if id, ok := n.(*dst.Ident); ok {
			used[id.Name] = true
		}
		return true
	})
	return firstFree(func(name string) bool { return used[name] }, candidates...)
}

// canNameCLI reports whether code in p can refer to the CLI type without an
// import cycle.
func (c *converter) canNameCLI(p *Package) bool {
	if c.cliDeps == nil {
		c.cliDeps = map[*types.Package]bool{}
		var visit func(t *types.Package)
		visit = func(t *types.Package) {
			for _, imp := range t.Imports() {
				if !c.cliDeps[imp] {
					c.cliDeps[imp] = true
					visit(imp)
				}
			}
		}
		visit(c.tree.RootPkg.Types)
	}
	return p.Types == c.tree.RootPkg.Types || !c.cliDeps[p.Types]
}

// keepStructure marks, in a constructor kept for tests, the statements that
// build the command struct: its definition and the parameters stored in it,
// as in c.build = build.
func (c *converter) keepStructure(ci *ctorInfo) {
	if ci.local == nil {
		return
	}
	p := ci.decl.pkg
	dst.Inspect(ci.decl.decl.Body, func(n dst.Node) bool {
		switch s := n.(type) {
		case *dst.FuncLit:
			return false
		case *dst.DeclStmt:
			if c.usedIn(p, s, ci.local) {
				c.keepStmts[s] = true
			}
		case *dst.AssignStmt:
			if len(s.Lhs) != 1 || len(s.Rhs) != 1 {
				return true
			}
			if p.objectOf(s.Lhs[0]) == ci.local {
				c.keepStmts[s] = true
			}
			if sel, ok := s.Lhs[0].(*dst.SelectorExpr); ok && s.Tok == token.ASSIGN {
				_, param := ci.params[p.objectOf(s.Rhs[0])]
				if x, ok := sel.X.(*dst.Ident); ok && param && p.objectOf(x) == ci.local {
					c.keepStmts[s] = true
				}
			}
		}
		return true
	})
}

// attachKeptCtor makes a constructor kept for tests return the command's
// struct inside the application's CLI, like the kingpin version returned
// the struct it registered.
// c := &Run{Globals: g} becomes c := &app.Grammar.(*CLI).Run followed by
// c.Globals = g.
// Only constructors that take the application and build a single command
// qualify.
func (c *converter) attachKeptCtor(ci *ctorInfo) {
	if c.main != nil || c.compatPath == "" || ci.local == nil {
		return
	}
	p, fd := ci.decl.pkg, ci.decl.decl
	if !c.canNameCLI(p) {
		return
	}
	var app *dst.Ident
	for _, field := range fd.Type.Params.List {
		for _, nm := range field.Names {
			if o := p.objectOf(nm); o != nil && nm.Name != "_" && c.isAppType(o.Type()) {
				app = nm
			}
		}
	}
	var inst *object
	for _, s := range c.g.in.sites {
		if s.callee == ci.fn {
			if inst != nil && inst != s.result {
				return
			}
			inst = s.result
		}
	}
	if app == nil || inst == nil {
		return
	}
	path, ok := c.cmdStructPath(inst)
	if !ok {
		return
	}
	for i, s := range fd.Body.List {
		rhs, ok := c.localDefinition(p, s, ci.local)
		if !ok {
			continue
		}
		var typ dst.Expr
		var elts []dst.Expr
		switch x := rhs.(type) {
		case *dst.UnaryExpr:
			lit, isLit := x.X.(*dst.CompositeLit)
			if x.Op != token.AND || !isLit || !isKeyed(lit) {
				return
			}
			typ, elts = lit.Type, lit.Elts
		case *dst.CallExpr:
			if id, isIdent := x.Fun.(*dst.Ident); !isIdent || id.Name != "new" || id.Path != "" || len(x.Args) != 1 {
				return
			}
			typ = x.Args[0]
		default:
			return
		}
		// Every call starts the command over from these values, as the
		// constructor built a new one, keeping the subcommands it holds.
		local := ci.local.Name()
		start := dst.Clone(&dst.CompositeLit{Type: typ, Elts: elts}).(*dst.CompositeLit)
		var before, inits []dst.Stmt
		if slices.ContainsFunc(elts, func(e dst.Expr) bool { return mentionsName(e, local) }) {
			// Once redefined, the name refers to the local, so the values
			// are computed first.
			initial := c.freeLocalIn(fd, "initial", "initialValues")
			before = append(before, &dst.AssignStmt{Lhs: []dst.Expr{dst.NewIdent(initial)}, Tok: token.DEFINE, Rhs: []dst.Expr{start}})
			if n := c.nodeAt(path); n != nil && n.containerOwn {
				for _, ch := range n.children {
					inits = append(inits, &dst.AssignStmt{Lhs: []dst.Expr{selectPath(dst.NewIdent(initial), ch.field)}, Tok: token.ASSIGN, Rhs: []dst.Expr{selectPath(dst.NewIdent(local), ch.field)}})
				}
			}
			inits = append(inits, &dst.AssignStmt{Lhs: []dst.Expr{&dst.StarExpr{X: dst.NewIdent(local)}}, Tok: token.ASSIGN, Rhs: []dst.Expr{dst.NewIdent(initial)}})
		} else {
			inits = append(inits, &dst.AssignStmt{Lhs: []dst.Expr{&dst.StarExpr{X: dst.NewIdent(local)}}, Tok: token.ASSIGN, Rhs: []dst.Expr{c.keepSubcommands(start, path, dst.NewIdent(local))}})
		}
		cli := &dst.TypeAssertExpr{
			X:    &dst.SelectorExpr{X: dst.NewIdent(app.Name), Sel: dst.NewIdent("Grammar")},
			Type: &dst.StarExpr{X: &dst.Ident{Name: c.cliName, Path: c.qualifier(p, c.tree.RootPkg)}},
		}
		def := &dst.AssignStmt{
			Lhs: []dst.Expr{dst.NewIdent(ci.local.Name())},
			Tok: token.DEFINE,
			Rhs: []dst.Expr{&dst.UnaryExpr{Op: token.AND, X: selectPath(cli, path)}},
		}
		def.Decs.NodeDecs = *s.Decorations()
		list := append([]dst.Stmt{}, fd.Body.List[:i]...)
		list = append(list, before...)
		list = append(list, def)
		list = append(list, inits...)
		fd.Body.List = append(list, fd.Body.List[i+1:]...)
		c.keepStmts[def] = true
		c.listParams[p.objectOf(app)] = true
		c.ed.touch(p, ci.decl.file)
		return
	}
}

// localDefinition returns the value s gives local, when s declares local
// alone with := or var.
func (c *converter) localDefinition(p *Package, s dst.Stmt, local types.Object) (dst.Expr, bool) {
	if d := onlyDefinition(s); d != nil && len(d.values) == 1 && p.objectOf(d.names[0]) == local {
		return d.values[0], true
	}
	return nil, false
}

// definitions returns the definitions of a := or var statement.
func definitions(s dst.Stmt) []*definition {
	var out []*definition
	switch s := s.(type) {
	case *dst.AssignStmt:
		if s.Tok == token.DEFINE {
			out = append(out, &definition{stmt: s, names: s.Lhs, values: s.Rhs, set: func(names, values []dst.Expr) {
				s.Lhs, s.Rhs = names, values
			}})
		}
	case *dst.DeclStmt:
		gd, ok := s.Decl.(*dst.GenDecl)
		if !ok || gd.Tok != token.VAR {
			return nil
		}
		for _, sp := range gd.Specs {
			vs := sp.(*dst.ValueSpec)
			var names []dst.Expr
			for _, nm := range vs.Names {
				names = append(names, nm)
			}
			out = append(out, &definition{stmt: s, names: names, values: vs.Values, set: func(names, values []dst.Expr) {
				vs.Names = nil
				for _, nm := range names {
					vs.Names = append(vs.Names, nm.(*dst.Ident))
				}
				vs.Values = values
			}})
		}
	}
	return out
}

// onlyDefinition returns the definition of s when it declares a single
// variable, or nil.
func onlyDefinition(s dst.Stmt) *definition {
	if ds := definitions(s); len(ds) == 1 && len(ds[0].names) == 1 {
		return ds[0]
	}
	return nil
}

// mentionsName reports whether e uses an identifier spelled name.
func mentionsName(e dst.Expr, name string) bool {
	found := false
	dst.Inspect(e, func(n dst.Node) bool {
		if id, ok := n.(*dst.Ident); ok && id.Name == name && id.Path == "" {
			found = true
		}
		return !found
	})
	return found
}
