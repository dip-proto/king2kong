package conv

import (
	"go/token"
	"go/types"
	"slices"

	"github.com/dave/dst"
)

// isGrammarValue reports whether a value only describes kingpin grammar: a
// command, flag or argument clause, or a command struct whose wiring the kong
// grammar replaces.
// The application doesn't count, since the rest of the program keeps using it
// until someone ports it by hand.
func (c *converter) isGrammarValue(v value) bool {
	switch v := v.(type) {
	case vClause:
		return v.c.kind != kindApp
	case vPtr:
		return c.isInstance(v.o)
	case sepOption:
		return true
	}
	return false
}

func (c *converter) isInstance(o *object) bool {
	if c.instances == nil {
		c.instances = map[*object]bool{}
		for _, cl := range c.g.in.clauses {
			if cl.instance != nil {
				c.instances[cl.instance] = true
			}
		}
	}
	return c.instances[o]
}

// isPureValue reports whether a value can be dropped once nothing uses it,
// like a folded constant or an options struct literal.
func isPureValue(v value) bool {
	switch v.(type) {
	case vConst, vObj, vSlice, vNil:
		return true
	}
	return false
}

// deletableStmts finds the statements of a function body that only served
// kingpin registration.
// That covers statements declaring or storing grammar, calls to functions
// that only register grammar, pure values only used by such statements, and
// ifs or blocks made only of such statements.
//
// It also returns the statements to lower instead: definitions of kingpin
// values that call helpers doing other work, as in f := setup(app) where
// setup prints.
// Those are replaced by the calls they keep, so the blocks holding them
// are not deletable.
func (c *converter) deletableStmts(p *Package, body *dst.BlockStmt) (map[dst.Stmt]bool, map[dst.Stmt][]dst.Stmt) {
	d := map[dst.Stmt]bool{}
	lowered := map[dst.Stmt][]dst.Stmt{}
	lowers := func(s dst.Stmt) bool {
		calls := c.keptCalls(p, s)
		if len(calls) > 0 {
			lowered[s] = calls
		}
		return len(calls) > 0
	}
	in := c.g.in
	var all []dst.Stmt
	dst.Inspect(body, func(n dst.Node) bool {
		if _, ok := n.(*dst.FuncLit); ok {
			return false
		}
		if s, ok := n.(dst.Stmt); ok && s != body {
			all = append(all, s)
		}
		return true
	})
	// uses maps a local to the statements reading it.
	uses := map[types.Object][]dst.Stmt{}
	for _, s := range all {
		var read dst.Node = s
		switch s := s.(type) {
		case *dst.BlockStmt:
			continue
		case *dst.IfStmt:
			// Its own uses are in its condition; the rest are statements
			// of their own.
			read = s.Cond
		}
		dst.Inspect(read, func(n dst.Node) bool {
			id, ok := n.(*dst.Ident)
			if !ok || id.Path != "" {
				return true
			}
			if v, ok := p.objectOf(id).(*types.Var); ok && !v.IsField() && v.Parent() != v.Pkg().Scope() {
				uses[v] = append(uses[v], s)
			}
			return true
		})
	}
	defines := func(s dst.Stmt) []types.Object {
		var out []types.Object
		switch s := s.(type) {
		case *dst.AssignStmt:
			if s.Tok == token.DEFINE {
				for _, l := range s.Lhs {
					if o := p.objectOf(l); o != nil {
						out = append(out, o)
					}
				}
			}
		case *dst.DeclStmt:
			dst.Inspect(s, func(n dst.Node) bool {
				if vs, ok := n.(*dst.ValueSpec); ok {
					for _, nm := range vs.Names {
						if o := p.objectOf(nm); o != nil {
							out = append(out, o)
						}
					}
				}
				return true
			})
		}
		return out
	}
	// readElsewhere reports whether s stores a command struct, or a list of
	// them, in a local of a converted main that kept code still reads.
	// That local then stays, pointing into the CLI.
	readElsewhere := func(s dst.Stmt, lhs []dst.Expr) bool {
		if c.main == nil {
			return false
		}
		for _, l := range lhs {
			o := p.objectOf(l)
			if _, replaced := c.main.repl[o]; !replaced && !c.isGrammarList(in.varVals[o]) {
				continue
			}
			for _, u := range uses[o] {
				if u != s && !d[u] {
					return true
				}
			}
		}
		return false
	}
	// A statement lowered to the calls it kept reads only what they read.
	stillReads := func(u dst.Stmt, o types.Object) bool {
		calls, ok := lowered[u]
		return !ok || slices.ContainsFunc(calls, func(k dst.Stmt) bool { return c.usedIn(p, k, o) })
	}
	// onlyDeletableRead reports whether the statements other than s that
	// read objs are all deletable.
	onlyDeletableRead := func(s dst.Stmt, objs ...types.Object) bool {
		for _, o := range objs {
			for _, u := range uses[o] {
				if u != s && !d[u] && stillReads(u, o) {
					return false
				}
			}
		}
		return true
	}
	usedOnlyByDeletable := func(s dst.Stmt) bool {
		return onlyDeletableRead(s, defines(s)...)
	}
	for changed := true; changed; {
		// A statement newly lowered to calls may free the locals it read.
		before := len(lowered)
		changed = false
		for _, s := range all {
			if d[s] || c.keepStmts[s] {
				continue
			}
			ok := false
			switch s := s.(type) {
			case *dst.ExprStmt:
				if call, isCall := s.X.(*dst.CallExpr); isCall {
					callee := in.staticCallee(p, call)
					// The result of a deleted constructor is ignored here, so
					// the call can go.
					ok = in.grammarCalls[call] || c.regOnly[callee] || c.removedCtors[callee] || c.emptied[callee]
					if !ok && !in.ran(s) {
						ok = c.staticGrammarCall(p, call)
					}
					// If some arguments do something, the call is lowered
					// to them instead.
					if ok && lowers(s) {
						ok = false
					}
				}
			case *dst.AssignStmt:
				vals := in.stmtVals[s]
				if len(vals) == 0 {
					// Never ran, like a branch no caller takes, so judge by
					// the static types.
					ok = !in.ran(s) && c.staticGrammarAssign(p, s)
					break
				}
				grammar, pure := true, true
				for i, v := range vals {
					// A list of commands built right here, such as
					// []Command{NewServeCommand(app, cfg)}.
					if i < len(s.Rhs) && literalOrAppend(s.Rhs[i]) && c.isGrammarList(v) {
						continue
					}
					if !c.isGrammarValue(v) {
						grammar = false
					}
					if !isPureValue(v) && !c.isGrammarValue(v) {
						pure = false
					}
				}
				if s.Tok == token.ASSIGN && c.storesFlagValues(s) && (c.main != nil && p == c.tree.RootPkg && c.definesOnlyReplaced(p, s) || c.allPkgFlagVars(p, s.Lhs)) {
					// The variables become, or point into, the CLI's fields.
					ok = true
					break
				}
				if grammar {
					// Storing a clause or a command struct, such as
					// c.CmdClause = parent.Command(...) or a constructor call.
					ok = !c.linksCommands(p, body, s) && !c.initializesCommand(p, s.Lhs, s.Rhs) && !readElsewhere(s, s.Lhs)
					if ok && lowers(s) {
						ok = false
					}
				} else if pure && s.Tok == token.DEFINE && !hasCallBesides(p, in, s) {
					ok = usedOnlyByDeletable(s)
				} else if root := localRoot(p, s.Lhs); pure && root != nil && !hasCallBesides(p, in, s) {
					// Changing a local only registrations read, as in
					// spec.Name += "-copy".
					ok = onlyDeletableRead(s, root)
				}
			case *dst.DeclStmt:
				vals := in.stmtVals[s]
				if len(vals) > 0 {
					grammar := true
					for _, v := range vals {
						if !c.isGrammarValue(v) {
							grammar = false
						}
					}
					ok = grammar && !c.declaresCommand(p, s) && !readElsewhere(s, declaredNames(s)) && !lowers(s)
				}
				if !ok {
					ok = c.declaresKingpin(p, s) && usedOnlyByDeletable(s)
				}
			case *dst.ReturnStmt:
				// A final return of a command list built in place, as in
				// return []Runner{NewServeCommand(app, cfg)}.
				vals := in.stmtVals[s]
				ok = len(body.List) > 0 && body.List[len(body.List)-1] == s && len(s.Results) == 1 &&
					len(vals) == 1 && literalOrAppend(s.Results[0]) && c.isGrammarList(vals[0])
			case *dst.IfStmt:
				ok = (s.Init == nil || d[s.Init]) && allIn(d, s.Body.List) && (s.Else == nil || d[s.Else]) && !c.hasEffects(p, s.Cond, true)
				if ok && s.Init == nil && len(s.Body.List) == 0 && s.Else == nil {
					ok = false
				}
			case *dst.BlockStmt:
				ok = len(s.List) > 0 && allIn(d, s.List)
			}
			if ok {
				d[s] = true
				changed = true
			}
		}
		changed = changed || len(lowered) > before
	}
	return d, lowered
}

// localRoot returns the local variable or parameter that assigning to lhs
// changes, when nothing outside the function can see the change.
func localRoot(p *Package, lhs []dst.Expr) *types.Var {
	var root *types.Var
	for _, l := range lhs {
		e := unparen(l)
		for {
			sel, ok := e.(*dst.SelectorExpr)
			if !ok {
				break
			}
			if _, ptr := typeUnder(p.typeOf(sel.X)).(*types.Pointer); ptr {
				return nil
			}
			e = unparen(sel.X)
		}
		id, ok := e.(*dst.Ident)
		if !ok {
			return nil
		}
		v, ok := p.objectOf(id).(*types.Var)
		if !ok || v.IsField() || v.Pkg() == nil || v.Parent() == v.Pkg().Scope() || root != nil && root != v {
			return nil
		}
		if _, ptr := typeUnder(v.Type()).(*types.Pointer); ptr {
			return nil
		}
		root = v
	}
	return root
}

// linksCommands reports whether an assignment outside a constructor hands a
// command object to a field the program reads, as in a.other = b.
// The field is still read once kong has filled the commands, so the
// assignment stays, pointing into the CLI.
func (c *converter) linksCommands(p *Package, body *dst.BlockStmt, s *dst.AssignStmt) bool {
	if s.Tok != token.ASSIGN {
		return false
	}
	for _, ci := range c.ctors {
		if ci.decl.decl.Body == body {
			return false
		}
	}
	for _, l := range s.Lhs {
		sel, ok := l.(*dst.SelectorExpr)
		if !ok {
			return false
		}
		f, ok := p.objectOf(sel.Sel).(*types.Var)
		if !ok || !f.IsField() || c.prog.isKingpinType(f.Type()) || !c.fieldRead(f) {
			return false
		}
	}
	return true
}

// initializesCommand reports whether a definition gives a command struct of
// a converted main its initial values, as in ls := &lsCmd{out: os.Stdout}.
// rewriteMain turns it into an assignment of the CLI's field.
func (c *converter) initializesCommand(p *Package, names, values []dst.Expr) bool {
	if c.main == nil || len(names) != len(values) {
		return false
	}
	for i, l := range names {
		if _, ok := c.main.repl[p.objectOf(l)]; ok && (initialValues(values[i]) != nil || structCopy(p, values[i])) {
			return true
		}
	}
	return false
}

// structCopy reports whether e is a struct variable copied by value, as
// defaults in serve := defaults.
func structCopy(p *Package, e dst.Expr) bool {
	switch unparen(e).(type) {
	case *dst.Ident, *dst.SelectorExpr:
		_, isStruct := typeUnder(p.typeOf(e)).(*types.Struct)
		_, isVar := p.objectOf(unparen(e)).(*types.Var)
		return isStruct && isVar
	}
	return false
}

// declaresCommand is initializesCommand for var declarations.
func (c *converter) declaresCommand(p *Package, s *dst.DeclStmt) bool {
	gd, ok := s.Decl.(*dst.GenDecl)
	if !ok || gd.Tok != token.VAR {
		return false
	}
	for _, sp := range gd.Specs {
		vs := sp.(*dst.ValueSpec)
		if c.initializesCommand(p, identExprs(vs.Names), vs.Values) {
			return true
		}
	}
	return false
}

func declaredNames(s *dst.DeclStmt) []dst.Expr {
	var out []dst.Expr
	if gd, ok := s.Decl.(*dst.GenDecl); ok {
		for _, sp := range gd.Specs {
			if vs, ok := sp.(*dst.ValueSpec); ok {
				out = append(out, identExprs(vs.Names)...)
			}
		}
	}
	return out
}

func identExprs(ids []*dst.Ident) []dst.Expr {
	out := make([]dst.Expr, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}

func (c *converter) allPkgFlagVars(p *Package, lhs []dst.Expr) bool {
	return !slices.ContainsFunc(lhs, func(l dst.Expr) bool { return !c.pkgFlagVars[p.objectOf(l)] })
}

// initialValues returns the composite literal e builds, as in &T{...} or
// T{...}, when it sets some fields.
func initialValues(e dst.Expr) *dst.CompositeLit {
	if lit := structLiteral(e); lit != nil && len(lit.Elts) > 0 {
		return lit
	}
	return nil
}

// structLiteral returns the composite literal e builds, as in &T{...},
// (T{...}) or T{...}.
func structLiteral(e dst.Expr) *dst.CompositeLit {
	e = unparen(e)
	if u, ok := e.(*dst.UnaryExpr); ok && u.Op == token.AND {
		e = unparen(u.X)
	}
	lit, _ := e.(*dst.CompositeLit)
	return lit
}

func unparen(e dst.Expr) dst.Expr {
	for {
		p, ok := e.(*dst.ParenExpr)
		if !ok {
			return e
		}
		e = p.X
	}
}

// isAddress reports whether e takes an address, as in &T{...}.
func isAddress(e dst.Expr) bool {
	u, ok := unparen(e).(*dst.UnaryExpr)
	return ok && u.Op == token.AND
}

// fieldRead reports whether the program reads field f anywhere, rather than
// only assigning it.
func (c *converter) fieldRead(f *types.Var) bool {
	if c.readFields == nil {
		c.readFields = map[*types.Var]bool{}
		for _, p := range c.prog.Packages {
			for _, file := range p.Syntax {
				assigned := map[dst.Expr]bool{}
				dst.Inspect(file, func(n dst.Node) bool {
					switch n := n.(type) {
					case *dst.AssignStmt:
						for _, l := range n.Lhs {
							assigned[l] = true
						}
					case *dst.SelectorExpr:
						if v, ok := p.objectOf(n.Sel).(*types.Var); ok && v.IsField() && !assigned[n] {
							c.readFields[v] = true
						}
					}
					return true
				})
			}
		}
	}
	return c.readFields[f]
}

func allIn(d map[dst.Stmt]bool, list []dst.Stmt) bool {
	for _, s := range list {
		if !d[s] {
			return false
		}
	}
	return true
}

// hasCallBesides reports whether an assignment calls anything other than
// kingpin grammar and the side-effect free functions the interpreter folds.
// Arguments the conversion moves elsewhere don't count.
func hasCallBesides(p *Package, in *interp, s *dst.AssignStmt) bool {
	return slices.ContainsFunc(s.Rhs, func(r dst.Expr) bool { return callsBesides(p, in, r) })
}

func callsBesides(p *Package, in *interp, r dst.Expr) bool {
	found := false
	dst.Inspect(r, func(n dst.Node) bool {
		if e, ok := n.(dst.Expr); ok && in.moved[e] {
			return false
		}
		if call, ok := n.(*dst.CallExpr); ok && !in.grammarCalls[call] {
			if tv, ok := p.tv(call.Fun); ok && tv.IsType() {
				return true
			}
			if foldsPlainly(p, call) {
				return true
			}
			found = true
		}
		return !found
	})
	return found
}

// foldable are the standard library functions the interpreter folds.
var foldable = map[string]bool{
	"fmt.Sprintf": true, "fmt.Sprint": true, "fmt.Sprintln": true, "strconv.Itoa": true,
	"strings.Join": true, "strings.ToLower": true, "strings.ToUpper": true, "strings.TrimSpace": true,
}

// foldsPlainly reports whether call is one the interpreter folds, such as
// fmt.Sprintf, and cannot run any method outside plainFormatters.
// fmt calls the String, Error, Format and GoString methods of what it
// formats, down to fields and elements, so all those types are checked.
func foldsPlainly(p *Package, call *dst.CallExpr) bool {
	if id, ok := call.Fun.(*dst.Ident); !ok || !foldable[id.Path+"."+id.Name] {
		return false
	}
	seen := map[types.Type]bool{}
	var plain func(t types.Type) bool
	plain = func(t types.Type) bool {
		if t == nil {
			return false
		}
		if seen[t] {
			return true
		}
		seen[t] = true
		ms := types.NewMethodSet(t)
		for i := range ms.Len() {
			fn := ms.At(i).Obj()
			switch fn.Name() {
			case "String", "Error", "Format", "GoString":
				if fn.Pkg() == nil || !plainFormatters[fn.Pkg().Path()] {
					return false
				}
			}
		}
		switch u := t.Underlying().(type) {
		case *types.Basic, *types.Chan, *types.Signature:
			return true
		case *types.Pointer:
			return plain(u.Elem())
		case *types.Slice:
			return plain(u.Elem())
		case *types.Array:
			return plain(u.Elem())
		case *types.Map:
			return plain(u.Key()) && plain(u.Elem())
		case *types.Struct:
			for i := range u.NumFields() {
				if !plain(u.Field(i).Type()) {
					return false
				}
			}
			return true
		}
		// An interface may hold anything.
		return false
	}
	return !slices.ContainsFunc(call.Args, func(a dst.Expr) bool { return !plain(p.typeOf(a)) })
}

// declaresKingpin reports whether a var declaration declares a variable of a
// kingpin type.
func (c *converter) declaresKingpin(p *Package, s *dst.DeclStmt) bool {
	found := false
	dst.Inspect(s, func(n dst.Node) bool {
		if vs, ok := n.(*dst.ValueSpec); ok {
			for _, nm := range vs.Names {
				if o := p.objectOf(nm); o != nil && c.prog.isKingpinType(o.Type()) {
					found = true
				}
			}
		}
		return true
	})
	return found
}

// computeRegOnly finds the functions whose whole body only registers grammar,
// so a call to one of them counts as registration too.
func (c *converter) computeRegOnly() {
	c.regOnly = map[*types.Func]bool{}
	for changed := true; changed; {
		changed = false
		for fn := range c.g.in.relevant {
			if c.regOnly[fn] {
				continue
			}
			fd, ok := c.prog.funcs[fn]
			if !ok || fd.decl.Body == nil || len(fd.decl.Body.List) == 0 {
				continue
			}
			if fd.decl.Type.Results != nil && len(fd.decl.Type.Results.List) > 0 {
				continue
			}
			d, _ := c.deletableStmts(fd.pkg, fd.decl.Body)
			if allIn(d, fd.decl.Body.List) {
				c.regOnly[fn] = true
				changed = true
			}
		}
	}
}

// staticGrammarCall reports whether a call that never ran is a kingpin builder
// call, judging from its types.
func (c *converter) staticGrammarCall(p *Package, call *dst.CallExpr) bool {
	sel, ok := call.Fun.(*dst.SelectorExpr)
	if !ok || runtimeMethods[sel.Sel.Name] {
		return false
	}
	return c.prog.isKingpinType(p.typeOf(sel.X)) && !c.isAppType(p.typeOf(sel.X)) ||
		c.prog.isKingpinType(p.typeOf(sel.X)) && (sel.Sel.Name == "Flag" || sel.Sel.Name == "Arg" || sel.Sel.Name == "Command")
}

// staticGrammarAssign reports whether an assignment that never ran stores
// kingpin clauses, judging from its types.
func (c *converter) staticGrammarAssign(p *Package, s *dst.AssignStmt) bool {
	for _, r := range s.Rhs {
		if !c.isClauseType(p.typeOf(r)) {
			return false
		}
	}
	return len(s.Rhs) > 0
}

// isClauseType reports whether t is a kingpin command, flag or argument
// clause, as opposed to the application, its models or a parse context.
func (c *converter) isClauseType(t types.Type) bool {
	n, ok := deref(t).(*types.Named)
	if !ok || !c.prog.isKingpinType(t) {
		return false
	}
	switch n.Obj().Name() {
	case "CmdClause", "FlagClause", "ArgClause", "Clause":
		return true
	}
	return false
}

func (c *converter) isAppType(t types.Type) bool {
	n, ok := deref(t).(*types.Named)
	return ok && n.Obj().Name() == "Application" && c.prog.isKingpinType(t)
}

// literalOrAppend reports whether e is a composite literal or an append call,
// the ways a list of commands is put together.
func literalOrAppend(e dst.Expr) bool {
	switch e := e.(type) {
	case *dst.CompositeLit:
		return true
	case *dst.CallExpr:
		id, ok := e.Fun.(*dst.Ident)
		return ok && id.Name == "append" && id.Path == ""
	}
	return false
}
