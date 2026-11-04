package conv

import (
	"bytes"
	"cmp"
	"fmt"
	"go/ast"
	"go/constant"
	"go/format"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"maps"
	"os"
	pathpkg "path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/dave/dst"
	"github.com/dave/dst/decorator"
	"github.com/dave/dst/dstutil"
)

// cleanupWiring removes the leftover registration statements from the
// functions that wired commands together.
// Locals and results that only carried kingpin values or command lists go too.
func (c *converter) cleanupWiring() {
	dests := map[*types.Var]bool{}
	for _, b := range c.plan.bindings {
		dests[b.field] = true
	}
	for _, p := range c.prog.Packages {
		for _, f := range p.Syntax {
			for _, d := range f.Decls {
				fd, ok := d.(*dst.FuncDecl)
				if !ok || fd.Body == nil || c.ed.removed[d] {
					continue
				}
				if !c.hasTouched(fd.Body) {
					continue
				}
				fn, _ := p.objectOf(fd.Name).(*types.Func)
				testsCall := fn != nil && c.testRefs.mentions(filepath.Dir(p.filename(f)), p.PkgPath, fn.Name(), fn.Type().(*types.Signature).Recv() != nil)
				if c.regOnly[fn] && fd.Name.Name != "init" && !testsCall {
					// removeDeadCode takes care of it, but never of init.
					for _, s := range fd.Body.List {
						c.reportDroppedFields(p, fd, s, dests)
					}
					continue
				}
				if c.regOnly[fn] && testsCall {
					// Emptied like the rest, but the tests that call it lose
					// what it registered for them.
					c.issue(p.position(fd), "tests call %s, which only registered flags and now does nothing; check those tests by hand", fn.Name())
				}
				del, lowered := c.deletableStmts(p, fd.Body)
				if c.ctors[fn] == nil {
					for _, s := range fd.Body.List {
						dst.Inspect(s, func(n dst.Node) bool {
							if s, ok := n.(dst.Stmt); ok && (del[s] || lowered[s] != nil) {
								c.reportDroppedFields(p, fd, s, dests)
								return false
							}
							return true
						})
					}
				}
				dead := map[types.Object]bool{}
				c.jumpKinds = labelJumps(fd.Body, func(br *dst.BranchStmt) bool { return !del[br] })
				c.reassigned = map[types.Object]bool{}
				c.fixedResults = c.fixedSignature(p, fd)
				dst.Inspect(fd.Body, func(n dst.Node) bool {
					if as, ok := n.(*dst.AssignStmt); ok {
						for _, l := range as.Lhs {
							if !c.declares(p, l) {
								c.reassigned[p.objectOf(l)] = true
							}
						}
					}
					return true
				})
				fd.Body.List = c.pruneStmts(p, fd.Body.List, del, lowered, dead)
				for changed := true; changed; {
					fd.Body.List, changed = c.pruneDeadUses(p, fd.Body.List, dead)
				}
				hadResults := fd.Type.Results != nil && len(fd.Type.Results.List) > 0
				c.pruneSignature(p, fd)
				tidyBody(fd)
				c.ed.touch(p, f)
				if c.dropIfEmpty(p, f, fd) {
					continue
				}
				if hadResults && fd.Type.Results == nil && len(fd.Body.List) > 0 {
					c.res.Notes = append(c.res.Notes, fmt.Sprintf("%s no longer builds commands; what is left of it (%s) may deserve another name and comment.",
						fd.Name.Name, p.position(fd).String()))
				}
			}
		}
	}
	c.dropEmptiedCalls()
}

// reportDroppedFields reports the fields a deleted statement sets in a
// command struct literal, like cfg in h := &hello{cfg: cfg}.
// Those values are lost, so code reading them would see zero values.
// Flag and argument fields, in dests, get their values from kong.
func (c *converter) reportDroppedFields(p *Package, fd *dst.FuncDecl, s dst.Stmt, dests map[*types.Var]bool) {
	dst.Inspect(s, func(n dst.Node) bool {
		if _, ok := n.(*dst.FuncLit); ok {
			return false
		}
		lit, ok := n.(*dst.CompositeLit)
		if !ok {
			return true
		}
		t, ok := deref(p.typeOf(lit)).(*types.Named)
		if !ok || !c.cmdTypes[t] {
			return true
		}
		st := structOf(t)
		for i, el := range lit.Elts {
			var f *types.Var
			val := el
			if kv, ok := el.(*dst.KeyValueExpr); ok {
				f, _ = p.objectOf(kv.Key).(*types.Var)
				val = kv.Value
			} else if i < st.NumFields() {
				f = st.Field(i)
			}
			if f == nil || c.prog.isKingpinType(f.Type()) || dests[f] || isZeroValue(p, val) || !c.fieldRead(f) {
				continue
			}
			c.issue(p.position(el), "%s builds a %s with %s set, which goes away with the registration; set %s in a hook by hand", fd.Name.Name, t.Obj().Name(), f.Name(), f.Name())
		}
		return true
	})
}

// isZeroValue reports whether e is nil or a zero constant.
func isZeroValue(p *Package, e dst.Expr) bool {
	tv, ok := p.tv(e)
	return ok && (tv.IsNil() || tv.Value != nil && isZeroConst(vConst{tv.Value, tv.Type}))
}

// singleStmt merges list into one statement, for spots such as an if header
// that only take one.
// Several statements become a function literal called on the spot, and an
// empty list gives nil.
func singleStmt(list []dst.Stmt) dst.Stmt {
	switch len(list) {
	case 0:
		return nil
	case 1:
		return list[0]
	}
	return &dst.ExprStmt{X: &dst.CallExpr{Fun: &dst.FuncLit{Type: &dst.FuncType{}, Body: &dst.BlockStmt{List: list}}}}
}

// setHeader puts s, which may be nil, in place of the statement at cur when
// that is the header of an if, switch or for, or the statement of a label.
// It reports false for any other spot.
func setHeader(cur *dstutil.Cursor, s dst.Stmt) bool {
	switch par := cur.Parent().(type) {
	case *dst.IfStmt:
		par.Init = s
	case *dst.SwitchStmt:
		par.Init = s
	case *dst.TypeSwitchStmt:
		par.Init = s
	case *dst.ForStmt:
		if cur.Name() == "Post" {
			par.Post = s
		} else {
			par.Init = s
		}
	case *dst.LabeledStmt:
		if s == nil {
			s = &dst.EmptyStmt{Implicit: true}
		}
		par.Stmt = s
	default:
		return false
	}
	return true
}

// labelJumps maps each label of body to the kinds of branch statements,
// such as break or goto, that jump to it.
// Only branch statements accepted by keep are counted.
func labelJumps(body *dst.BlockStmt, keep func(*dst.BranchStmt) bool) map[string]map[token.Token]bool {
	jumps := map[string]map[token.Token]bool{}
	dst.Inspect(body, func(n dst.Node) bool {
		if br, ok := n.(*dst.BranchStmt); ok && br.Label != nil && keep(br) {
			if jumps[br.Label.Name] == nil {
				jumps[br.Label.Name] = map[token.Token]bool{}
			}
			jumps[br.Label.Name][br.Tok] = true
		}
		return true
	})
	return jumps
}

// dropIfEmpty removes fd when nothing is left in its body, and reports
// whether it did.
// dropEmptiedCalls removes the calls to it later; an init has none.
// main, methods, and functions with results or used as values are kept.
func (c *converter) dropIfEmpty(p *Package, f *dst.File, fd *dst.FuncDecl) bool {
	fn, ok := p.objectOf(fd.Name).(*types.Func)
	if !ok || len(fd.Body.List) > 0 || fd.Type.Results != nil || fd.Recv != nil || fd.Name.Name == "main" || c.funcValues()[fn] {
		return false
	}
	c.ed.removeDecl(p, f, fd)
	c.emptied[fn] = true
	return true
}

// dropEmptiedCalls removes the calls still made to functions dropIfEmpty
// deleted, keeping any argument that does something.
// That can leave a caller empty in turn, so it repeats until nothing
// changes.
func (c *converter) dropEmptiedCalls() {
	for changed := len(c.emptied) > 0; changed; {
		changed = false
		for _, p := range c.prog.Packages {
			for _, f := range p.Syntax {
				for _, d := range f.Decls {
					fd, ok := d.(*dst.FuncDecl)
					if !ok || fd.Body == nil || c.ed.removed[d] {
						continue
					}
					dropped := false
					dstutil.Apply(fd.Body, func(cur *dstutil.Cursor) bool {
						var call *dst.CallExpr
						switch s := cur.Node().(type) {
						case *dst.ExprStmt:
							call, _ = s.X.(*dst.CallExpr)
						case *dst.DeferStmt:
							call = s.Call
						case *dst.GoStmt:
							call = s.Call
						}
						if call == nil || !c.emptied[c.g.in.staticCallee(p, call)] {
							return true
						}
						var left []dst.Stmt
						for _, a := range call.Args {
							if c.doesSomething(p, a) {
								left = append(left, c.evaluation(p, a))
							}
						}
						if cur.Index() < 0 {
							// A header takes a single statement or none.
							if !setHeader(cur, singleStmt(left)) {
								return true
							}
						} else {
							for _, st := range left {
								cur.InsertBefore(st)
							}
							cur.Delete()
						}
						dropped = true
						return false
					}, nil)
					if !dropped {
						continue
					}
					c.ed.touch(p, f)
					changed = c.dropIfEmpty(p, f, fd) || changed
				}
			}
		}
	}
}

// foldDeadNilChecks replaces comparisons of removed kingpin values with nil,
// as in cmd != nil, by their result: kingpin never returned nil for them.
// Other uses of them in e are reported.
func (c *converter) foldDeadNilChecks(p *Package, e dst.Expr, dead map[types.Object]bool) dst.Expr {
	e = c.foldNils(p, e, dead).(dst.Expr)
	// A method call such as cmd.FullCommand() is rewritten later.
	dst.Inspect(e, func(n dst.Node) bool {
		if sel, ok := n.(*dst.SelectorExpr); ok {
			if _, isIdent := sel.X.(*dst.Ident); isIdent {
				return false
			}
		}
		if id, ok := n.(*dst.Ident); ok && dead[p.objectOf(id)] {
			c.issue(p.position(id), "%s was a kingpin value, and the condition still reads it; rewrite the condition by hand", id.Name)
		}
		return true
	})
	return e
}

func isIfOrBlock(s dst.Stmt) bool {
	switch s.(type) {
	case *dst.IfStmt, *dst.BlockStmt:
		return true
	}
	return false
}

// callsLeft is keptCalls with the removed values they read folded.
func (c *converter) callsLeft(p *Package, s dst.Stmt, dead map[types.Object]bool) []dst.Stmt {
	calls := c.keptCalls(p, s)
	for _, st := range calls {
		c.foldNils(p, st, dead)
		c.nilDeadRefs(p, st, dead)
	}
	return calls
}

// nilDeadRefs replaces what code kept still reads of a removed kingpin
// value, once nil checks are folded, with nil, and reports it.
// A method call on it, as in cmd.FullCommand(), is rewritten later.
func (c *converter) nilDeadRefs(p *Package, s dst.Node, dead map[types.Object]bool) {
	dstutil.Apply(s, func(cur *dstutil.Cursor) bool {
		switch n := cur.Node().(type) {
		case *dst.SelectorExpr:
			if _, ok := n.X.(*dst.Ident); ok {
				return false
			}
		case *dst.AssignStmt:
			// Its targets are handled where it is removed.
			for _, r := range n.Rhs {
				c.nilDeadRefs(p, r, dead)
			}
			return false
		case *dst.Ident:
			if dead[p.objectOf(n)] {
				c.issue(p.position(n), "%s was a kingpin value the conversion removed, and code kept still reads it; it is nil now, check it by hand", n.Name)
				cur.Replace(dst.NewIdent("nil"))
			}
		}
		return true
	}, nil)
}

// evaluation returns a statement that evaluates e for its side effects.
// A call stands on its own, since it may return several values; anything
// else, conversions and builtins included, becomes _ = e.
func (c *converter) evaluation(p *Package, e dst.Expr) dst.Stmt {
	if call, ok := unparen(e).(*dst.CallExpr); ok {
		_, builtin := p.objectOf(call.Fun).(*types.Builtin)
		if tv, conv := p.tv(call.Fun); !builtin && (!conv || !tv.IsType()) {
			return &dst.ExprStmt{X: call}
		}
	}
	return &dst.AssignStmt{Lhs: []dst.Expr{dst.NewIdent("_")}, Tok: token.ASSIGN, Rhs: []dst.Expr{e}}
}

// keptCalls returns, in order, what a removed statement must still run,
// such as setup(app) in f := setup(app) when setup also prints something.
func (c *converter) keptCalls(p *Package, s dst.Stmt) []dst.Stmt {
	var values []dst.Expr
	var out []dst.Stmt
	switch s := s.(type) {
	case *dst.ExprStmt:
		values = []dst.Expr{s.X}
	case *dst.AssignStmt:
		// The operands of the targets come first, as in flags[key()] = f.
		for _, l := range s.Lhs {
			for _, e := range targetOperands(l) {
				if c.doesSomething(p, e) {
					out = append(out, c.evaluation(p, e))
				}
			}
		}
		values = s.Rhs
	case *dst.DeclStmt:
		for _, sp := range s.Decl.(*dst.GenDecl).Specs {
			if vs, ok := sp.(*dst.ValueSpec); ok {
				values = append(values, vs.Values...)
			}
		}
	}
	for _, v := range values {
		out = append(out, c.effectsLeft(p, v)...)
	}
	if len(out) > 0 {
		out[0].Decorations().Before, out[0].Decorations().Start = s.Decorations().Before, s.Decorations().Start
	}
	return out
}

// targetOperands returns the parts of an assignment target that get
// evaluated before the store, such as m and k in m[k] = v.
// A selector counts too, since it may go through a pointer.
func targetOperands(l dst.Expr) []dst.Expr {
	switch l := unparen(l).(type) {
	case *dst.IndexExpr:
		return []dst.Expr{l.X, l.Index}
	case *dst.StarExpr:
		return []dst.Expr{l.X}
	case *dst.SelectorExpr:
		return []dst.Expr{l.X}
	}
	return nil
}

// effectsLeft returns the statements that must still run once the value v
// is removed.
// A call to a helper that also does other work is kept whole.
// In a chain of removed calls, such as builder().Register(app) or
// app.Flag("x", help()).Bool(), the receivers and arguments that do
// something are kept, receivers first.
func (c *converter) effectsLeft(p *Package, v dst.Expr) []dst.Stmt {
	switch e := unparen(v).(type) {
	case *dst.CallExpr:
		if c.doesOtherWork(p, e) {
			return []dst.Stmt{&dst.ExprStmt{X: e}}
		}
	case *dst.CompositeLit:
		// Elements are evaluated in order, as in []*Flag{setup(app)}.
		_, isMap := typeUnder(p.typeOf(e)).(*types.Map)
		var out []dst.Stmt
		for _, el := range e.Elts {
			if kv, ok := el.(*dst.KeyValueExpr); ok {
				if isMap {
					out = append(out, c.effectsLeft(p, kv.Key)...)
				}
				el = kv.Value
			}
			out = append(out, c.effectsLeft(p, el)...)
		}
		return out
	case *dst.UnaryExpr:
		if e.Op != token.ARROW {
			return c.effectsLeft(p, e.X)
		}
	case *dst.BinaryExpr:
		// The right operand of && and || only runs under a condition, so it
		// is reported rather than kept.
		if e.Op == token.LAND || e.Op == token.LOR {
			out := c.effectsLeft(p, e.X)
			if c.doesSomething(p, e.Y) {
				c.issue(p.position(e), "%s runs %s only under a condition, and the conversion removed what it computes; check it by hand", render(e), render(e.Y))
			}
			return out
		}
		return append(c.effectsLeft(p, e.X), c.effectsLeft(p, e.Y)...)
	case *dst.StarExpr:
		return c.effectsLeft(p, e.X)
	case *dst.IndexExpr:
		return append(c.effectsLeft(p, e.X), c.effectsLeft(p, e.Index)...)
	case *dst.SelectorExpr:
		if sel := p.selection(e); sel != nil && sel.Kind() == types.FieldVal {
			return c.effectsLeft(p, e.X)
		}
	}
	var exprs []dst.Expr
	var collect func(e dst.Expr, removed bool)
	collect = func(e dst.Expr, removed bool) {
		call, ok := unparen(e).(*dst.CallExpr)
		if !ok || !removed && !c.grammarCall(call) {
			exprs = append(exprs, e)
			return
		}
		if sel, ok := call.Fun.(*dst.SelectorExpr); ok {
			if s := p.selection(sel); s != nil && s.Kind() == types.MethodVal {
				collect(sel.X, false)
			}
		}
		exprs = append(exprs, call.Args...)
	}
	collect(v, true)
	var out []dst.Stmt
	for _, a := range exprs {
		if a != v && c.doesSomething(p, a) {
			out = append(out, c.evaluation(p, a))
		}
	}
	return out
}

// doesOtherWork reports whether call runs a function of the program that
// does more than register grammar, and is therefore kept.
func (c *converter) doesOtherWork(p *Package, call *dst.CallExpr) bool {
	fn := c.g.in.staticCallee(p, call)
	if fn == nil {
		var seen bool
		fn, seen = c.g.in.valueCallees[call]
		if seen && fn == nil {
			// Whichever function it was, it stays.
			return true
		}
	}
	// What a constructor did besides registering moved to hooks.
	if fn == nil || c.regOnly[fn] || c.ctors[fn] != nil || c.emptied[fn] {
		return false
	}
	_, ok := c.prog.funcs[fn]
	return ok
}

// foldNils is the folding of foldDeadNilChecks, in any node.
func (c *converter) foldNils(p *Package, n dst.Node, dead map[types.Object]bool) dst.Node {
	return dstutil.Apply(n, func(cur *dstutil.Cursor) bool {
		b, ok := cur.Node().(*dst.BinaryExpr)
		if !ok || b.Op != token.EQL && b.Op != token.NEQ {
			return true
		}
		x, y := unparen(b.X), unparen(b.Y)
		// Two removed values compare as the clauses they held.
		if dead[p.objectOf(x)] && dead[p.objectOf(y)] {
			cx, okx := c.g.in.varVals[p.objectOf(x)].(vClause)
			cy, oky := c.g.in.varVals[p.objectOf(y)].(vClause)
			if okx && oky && !c.reassigned[p.objectOf(x)] && !c.reassigned[p.objectOf(y)] {
				cur.Replace(c.boolLit(p, b, (cx.c == cy.c) == (b.Op == token.EQL)))
				return false
			}
		}
		for _, pair := range [][2]dst.Expr{{x, y}, {y, x}} {
			if _, isNil := p.objectOf(pair[1]).(*types.Nil); isNil && dead[p.objectOf(pair[0])] {
				if c.reassigned[p.objectOf(pair[0])] {
					c.issue(p.position(b), "%s was a kingpin value the program assigns again, and %s now reads %v as if it still held it; check it by hand", render(pair[0]), render(b), b.Op == token.NEQ)
				}
				cur.Replace(c.boolLit(p, b, b.Op == token.NEQ))
				return false
			}
		}
		return true
	}, nil)
}

// dropUnusedLocals removes the definitions of locals nothing reads any more,
// such as c := &Command{} once its registrations are gone, when their
// values compute nothing.
// The program compiled before, so such locals come from the conversion.
func (c *converter) dropUnusedLocals() {
	for f, p := range c.ed.dirty {
		for _, d := range f.Decls {
			fd, ok := d.(*dst.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			for changed := true; changed; {
				changed = c.dropUnusedIn(p, fd.Body)
				if changed {
					c.ed.touch(p, f)
				}
			}
		}
	}
}

// dropUnusedIn does one pass of dropUnusedLocals over body.
// Reads count by object, and by name for code the conversion wrote, which
// has no type information.
func (c *converter) dropUnusedIn(p *Package, body *dst.BlockStmt) bool {
	reads := map[types.Object]int{}
	byName := map[string]int{}
	dst.Inspect(body, func(n dst.Node) bool {
		if id, ok := n.(*dst.Ident); ok && id.Path == "" {
			if o := p.objectOf(id); o != nil {
				reads[o]++
			} else {
				byName[id.Name]++
			}
		}
		return true
	})
	// A blank name was there before the conversion.
	unused := func(e dst.Expr) bool {
		id, ok := e.(*dst.Ident)
		if !ok || id.Name == "_" {
			return false
		}
		if o := p.objectOf(id); o != nil {
			if c.boundLater(o) {
				return false
			}
			return reads[o] <= 1 && byName[id.Name] == 0
		}
		return byName[id.Name] <= 1
	}
	effects := func(values []dst.Expr) bool {
		return slices.ContainsFunc(values, func(e dst.Expr) bool { return c.hasEffects(p, e, false) })
	}
	blank := func(names []dst.Expr) {
		for _, n := range names {
			if unused(n) {
				n.(*dst.Ident).Name = "_"
			}
		}
	}
	changed := false
	dstutil.Apply(body, func(cur *dstutil.Cursor) bool {
		switch s := cur.Node().(type) {
		case *dst.AssignStmt:
			// A type switch guard's variable is declared again in each
			// case, so its reads do not show on it.
			if _, guard := cur.Parent().(*dst.TypeSwitchStmt); guard && cur.Name() == "Assign" {
				return false
			}
			if s.Tok != token.DEFINE || !slices.ContainsFunc(s.Lhs, unused) {
				return true
			}
			changed = true
			if !slices.ContainsFunc(s.Lhs, func(n dst.Expr) bool { return !unused(n) && !isBlank(n) }) && !effects(s.Rhs) && removeStmt(cur) {
				return false
			}
			// What the values do still happens: keep := receive()
			// becomes _ = receive().
			blank(s.Lhs)
			if !slices.ContainsFunc(s.Lhs, func(n dst.Expr) bool { return c.declares(p, n) }) {
				s.Tok = token.ASSIGN
			}
			return false
		case *dst.DeclStmt:
			gd, ok := s.Decl.(*dst.GenDecl)
			if !ok || gd.Tok != token.VAR {
				return true
			}
			var specs []dst.Spec
			for _, sp := range gd.Specs {
				vs := sp.(*dst.ValueSpec)
				names := identExprs(vs.Names)
				if slices.ContainsFunc(names, unused) {
					changed = true
					if !slices.ContainsFunc(names, func(n dst.Expr) bool { return !unused(n) && !isBlank(n) }) && !effects(vs.Values) {
						continue
					}
					blank(names)
				}
				specs = append(specs, vs)
			}
			if len(specs) > 0 || !removeStmt(cur) {
				gd.Specs = specs
			}
			return false
		case *dst.RangeStmt:
			if s.Tok != token.DEFINE {
				return true
			}
			for _, v := range []*dst.Expr{&s.Key, &s.Value} {
				if *v != nil && unused(*v) {
					changed = true
					*v = dst.NewIdent("_")
				}
			}
			if isBlank(s.Value) {
				s.Value = nil
				if isBlank(s.Key) {
					s.Key, s.Tok = nil, token.ILLEGAL
				}
			}
		}
		return true
	}, nil)
	return changed
}

// removeStmt removes the statement at cur, leaving an empty statement
// where a label still needs one.
// It reports false where neither fits, as in the header of an if.
func removeStmt(cur *dstutil.Cursor) bool {
	if cur.Index() >= 0 {
		cur.Delete()
		return true
	}
	if _, labeled := cur.Parent().(*dst.LabeledStmt); labeled {
		cur.Replace(&dst.EmptyStmt{Implicit: true})
		return true
	}
	return false
}

func isBlank(e dst.Expr) bool {
	id, ok := e.(*dst.Ident)
	return e == nil || ok && id.Name == "_"
}

func declaresVars(s dst.Stmt) bool {
	switch s := s.(type) {
	case *dst.DeclStmt:
		return true
	case *dst.AssignStmt:
		return s.Tok == token.DEFINE
	}
	return false
}

// boolLit is true or false as a literal at node n, or 0 == 0 and 0 != 0
// where a local named true or false hides the predeclared one.
func (c *converter) boolLit(p *Package, n dst.Node, v bool) dst.Expr {
	name := strconv.FormatBool(v)
	if a := p.astOf(n); a != nil {
		if scope := p.Types.Scope().Innermost(a.Pos()); scope != nil {
			if _, obj := scope.LookupParent(name, a.Pos()); obj != types.Universe.Lookup(name) {
				op := token.EQL
				if !v {
					op = token.NEQ
				}
				return &dst.BinaryExpr{X: &dst.BasicLit{Kind: token.INT, Value: "0"}, Op: op, Y: &dst.BasicLit{Kind: token.INT, Value: "0"}}
			}
		}
	}
	return dst.NewIdent(name)
}

func (c *converter) hasTouched(n dst.Node) bool {
	found := false
	dst.Inspect(n, func(n dst.Node) bool {
		if s, ok := n.(dst.Stmt); ok && c.g.touched[s] {
			found = true
		}
		return !found
	})
	return found
}

// pruneStmts drops the statements in del, descending into blocks and
// control statements so the other work inside them survives.
// The statements in lowered go too, but leave their kept calls behind.
func (c *converter) pruneStmts(p *Package, list []dst.Stmt, del map[dst.Stmt]bool, lowered map[dst.Stmt][]dst.Stmt, dead map[types.Object]bool) []dst.Stmt {
	var out []dst.Stmt
	for _, s := range list {
		if del[s] || lowered[s] != nil {
			c.markDefinedDead(p, s, dead)
			out = append(out, c.callsLeft(p, s, dead)...)
			continue
		}
		switch s := s.(type) {
		case *dst.ReturnStmt:
			// A helper returning a clause, as in return app.Flag(...), now
			// returns nothing, or nil if its signature must stay.
			// What the clause's arguments did still runs.
			if vals := c.g.in.stmtVals[s]; len(vals) > 0 && len(vals) == len(s.Results) && !slices.ContainsFunc(vals, func(v value) bool {
				cl, ok := v.(vClause)
				return !ok || cl.c.kind == kindApp
			}) {
				for _, r := range s.Results {
					out = append(out, c.effectsLeft(p, r)...)
				}
				if c.fixedResults {
					for i := range s.Results {
						s.Results[i] = dst.NewIdent("nil")
					}
				} else {
					s.Results = nil
				}
			}
			c.foldNils(p, s, dead)
			c.nilDeadRefs(p, s, dead)
		case *dst.ExprStmt, *dst.AssignStmt, *dst.DeclStmt, *dst.SendStmt, *dst.GoStmt, *dst.DeferStmt:
			c.foldNils(p, s, dead)
			c.nilDeadRefs(p, s, dead)
		}
		prune := func(list []dst.Stmt) []dst.Stmt { return c.pruneStmts(p, list, del, lowered, dead) }
		// A registration in an if, for or switch header goes too.
		// A single kept call stays in the header, and several go right
		// before the statement.
		var before []dst.Stmt
		dropHeader := func(st *dst.Stmt) {
			if *st == nil || !del[*st] && lowered[*st] == nil {
				return
			}
			c.markDefinedDead(p, *st, dead)
			calls := c.callsLeft(p, *st, dead)
			*st = nil
			switch {
			case len(calls) == 1:
				*st = calls[0]
			case len(calls) > 1:
				before = append(before, calls...)
			}
		}
		// A for post statement runs after each iteration, so its kept calls
		// must stay there.
		dropPost := func(st *dst.Stmt) {
			if *st == nil || !del[*st] && lowered[*st] == nil {
				return
			}
			c.markDefinedDead(p, *st, dead)
			*st = singleStmt(c.callsLeft(p, *st, dead))
		}
		switch s := s.(type) {
		case *dst.IfStmt:
			init := s.Init
			dropHeader(&s.Init)
			s.Cond = c.foldDeadNilChecks(p, s.Cond, dead)
			s.Body.List = prune(s.Body.List)
			switch e := s.Else.(type) {
			case *dst.BlockStmt:
				if e.List = prune(e.List); len(e.List) == 0 {
					s.Else = nil
				}
			case *dst.IfStmt:
				// What it became may be several statements, or no if.
				s.Else = nil
				switch rest := prune([]dst.Stmt{e}); {
				case len(rest) == 1 && isIfOrBlock(rest[0]):
					s.Else = rest[0]
				case len(rest) > 0:
					s.Else = &dst.BlockStmt{List: rest}
				}
			}
			if len(s.Body.List) == 0 && s.Else == nil && !c.hasEffects(p, s.Cond, false) && (init == nil || del[init]) {
				// Only the calls kept from its header are left.
				out = append(out, before...)
				if s.Init != nil {
					out = append(out, s.Init)
				}
				continue
			}
		case *dst.BlockStmt:
			s.List = prune(s.List)
		case *dst.ForStmt:
			dropHeader(&s.Init)
			dropPost(&s.Post)
			if s.Cond != nil {
				s.Cond = c.foldDeadNilChecks(p, s.Cond, dead)
			}
			s.Body.List = prune(s.Body.List)
		case *dst.RangeStmt:
			s.X = c.foldDeadNilChecks(p, s.X, dead)
			s.Body.List = prune(s.Body.List)
			// A loop assigning existing variables still sets them.
			if len(s.Body.List) == 0 && s.Tok != token.ASSIGN && p.typeOf(s.X) != nil && !rangeCalls(p, s) && !c.hasEffects(p, s.X, false) {
				continue
			}
		case *dst.SwitchStmt:
			dropHeader(&s.Init)
			if s.Tag != nil {
				s.Tag = c.foldDeadNilChecks(p, s.Tag, dead)
			}
			for _, cc := range s.Body.List {
				cc := cc.(*dst.CaseClause)
				for i, e := range cc.List {
					cc.List[i] = c.foldDeadNilChecks(p, e, dead)
				}
				cc.Body = prune(cc.Body)
			}
		case *dst.TypeSwitchStmt:
			dropHeader(&s.Init)
			for _, cc := range s.Body.List {
				cc.(*dst.CaseClause).Body = prune(cc.(*dst.CaseClause).Body)
			}
		case *dst.SelectStmt:
			for _, cc := range s.Body.List {
				cc := cc.(*dst.CommClause)
				if cc.Comm != nil {
					c.foldNils(p, cc.Comm, dead)
				}
				cc.Body = prune(cc.Body)
			}
		case *dst.LabeledStmt:
			rest := prune([]dst.Stmt{s.Stmt})
			switch {
			case len(rest) > 1:
				// A goto must land on the first of them, but break and
				// continue need the label on the loop or switch, which
				// comes last.
				jumps := c.jumpKinds[s.Label.Name]
				if jumps[token.GOTO] && (jumps[token.BREAK] || jumps[token.CONTINUE]) {
					c.issue(p.position(s), "label %s is the target of both goto and break or continue, and its statement's header now runs before it; check the jumps by hand", s.Label.Name)
				}
				if jumps[token.GOTO] && !jumps[token.BREAK] && !jumps[token.CONTINUE] {
					s.Stmt = rest[0]
					out = append(append(out, s), rest[1:]...)
				} else {
					s.Stmt = rest[len(rest)-1]
					out = append(append(out, rest[:len(rest)-1]...), s)
				}
				continue
			case len(rest) > 0:
				s.Stmt = rest[0]
			case c.jumpKinds[s.Label.Name] != nil:
				// A jump still goes there.
				s.Stmt = &dst.EmptyStmt{Implicit: true}
			default:
				continue
			}
		}
		out = append(append(out, before...), s)
	}
	// A label kept for a jump goes on the statement after it, unless that
	// one declares variables, which cleanup may still remove.
	for i := 0; i+1 < len(out); i++ {
		if l, ok := out[i].(*dst.LabeledStmt); ok && !declaresVars(out[i+1]) {
			if _, empty := l.Stmt.(*dst.EmptyStmt); empty {
				l.Stmt = out[i+1]
				out = slices.Delete(out, i+1, i+2)
			}
		}
	}
	return out
}

func (c *converter) markDefinedDead(p *Package, s dst.Stmt, dead map[types.Object]bool) {
	dst.Inspect(s, func(n dst.Node) bool {
		switch n := n.(type) {
		case *dst.AssignStmt:
			for _, l := range n.Lhs {
				if id, ok := l.(*dst.Ident); ok {
					if o := p.objectOf(id); o != nil {
						dead[o] = true
					}
				}
			}
		case *dst.ValueSpec:
			for _, nm := range n.Names {
				if o := p.objectOf(nm); o != nil {
					dead[o] = true
				}
			}
		}
		return true
	})
}

// pruneDeadUses removes statements that only build or return values derived
// from removed locals, like a list of commands a wiring function returned.
func (c *converter) pruneDeadUses(p *Package, list []dst.Stmt, dead map[types.Object]bool) ([]dst.Stmt, bool) {
	changed := false
	var out []dst.Stmt
	uses := func(n dst.Node) bool {
		found := false
		dst.Inspect(n, func(n dst.Node) bool {
			if id, ok := n.(*dst.Ident); ok && dead[p.objectOf(id)] {
				found = true
			}
			return !found
		})
		return found
	}
	for _, s := range list {
		switch s := s.(type) {
		case *dst.AssignStmt:
			if uses(s) && c.pureAssign(p, s) {
				for _, l := range s.Lhs {
					if o := p.objectOf(l); o != nil {
						dead[o] = true
					}
				}
				changed = true
				continue
			}
		case *dst.DeclStmt:
			if gd, ok := s.Decl.(*dst.GenDecl); ok && gd.Tok == token.VAR && !c.declaresCommand(p, s) {
				allDead := true
				for _, sp := range gd.Specs {
					for _, nm := range sp.(*dst.ValueSpec).Names {
						// A blank name was there before the conversion.
						if nm.Name == "_" || !c.unusedOutside(p, nm, list, s) {
							allDead = false
						}
					}
				}
				noInit := true
				for _, sp := range gd.Specs {
					if len(sp.(*dst.ValueSpec).Values) > 0 && c.hasEffects(p, sp, false) {
						noInit = false
					}
				}
				if allDead && (uses(s) || noInit) {
					for _, sp := range gd.Specs {
						for _, nm := range sp.(*dst.ValueSpec).Names {
							dead[p.objectOf(nm)] = true
						}
					}
					changed = true
					continue
				}
			}
		case *dst.IfStmt:
			var ch bool
			s.Body.List, ch = c.pruneDeadUses(p, s.Body.List, dead)
			changed = changed || ch
			if len(s.Body.List) == 0 && s.Else == nil && s.Init == nil && !c.hasEffects(p, s.Cond, false) {
				changed = true
				continue
			}
		case *dst.ReturnStmt:
			if uses(s) {
				s.Results = nil
				changed = true
			}
		}
		out = append(out, s)
	}
	// Also drop locals that are never read and only hold the result of a
	// side-effect free call.
	var final []dst.Stmt
	for _, s := range out {
		if as, ok := s.(*dst.AssignStmt); ok && as.Tok == token.DEFINE && len(as.Lhs) == 1 {
			if id, ok := as.Lhs[0].(*dst.Ident); ok && c.unusedOutside(p, id, out, s) && c.pureCall(p, as.Rhs[0]) {
				dead[p.objectOf(id)] = true
				changed = true
				continue
			}
		}
		final = append(final, s)
	}
	return final, changed
}

// unusedOutside reports whether the local named by id is never read outside
// its own declaration statement.
func (c *converter) unusedOutside(p *Package, id *dst.Ident, list []dst.Stmt, decl dst.Stmt) bool {
	obj := p.objectOf(id)
	if obj == nil || c.boundLater(obj) {
		return false
	}
	used := false
	for _, s := range list {
		if s == decl {
			continue
		}
		dst.Inspect(s, func(n dst.Node) bool {
			if x, ok := n.(*dst.Ident); ok && x != id && p.objectOf(x) == obj {
				used = true
			}
			return !used
		})
	}
	return !used
}

// boundLater reports whether the parser, written after cleanup, binds o for
// the hooks.
func (c *converter) boundLater(o types.Object) bool {
	v, ok := o.(*types.Var)
	return ok && c.main != nil && slices.ContainsFunc(c.main.bound, func(b boundVar) bool { return b.v == v })
}

func (c *converter) pureAssign(p *Package, s *dst.AssignStmt) bool {
	for _, l := range s.Lhs {
		if _, ok := l.(*dst.Ident); !ok {
			return false
		}
	}
	return !slices.ContainsFunc(s.Rhs, func(r dst.Expr) bool { return c.hasEffects(p, r, false) })
}

func hasCall(e dst.Node) bool {
	found := false
	dst.Inspect(e, func(n dst.Node) bool {
		if _, ok := n.(*dst.CallExpr); ok {
			found = true
		}
		return !found
	})
	return found
}

// argumentDoesWork reports whether a call to fn passes, at index i, an
// argument whose evaluation does something besides building kingpin
// grammar, as in setup(getApp()) or setup(apps[0]): its parameter stays, so
// the call keeps computing it.
func (c *converter) argumentDoesWork(fn *types.Func, i int) bool {
	if c.callsTo == nil {
		c.callsTo = map[*types.Func][]callRef{}
		for _, p := range c.prog.Packages {
			for _, f := range p.Syntax {
				dst.Inspect(f, func(n dst.Node) bool {
					if call, ok := n.(*dst.CallExpr); ok {
						if callee := c.g.in.staticCallee(p, call); callee != nil {
							c.callsTo[callee] = append(c.callsTo[callee], callRef{p, call})
						}
					}
					return true
				})
			}
		}
	}
	for _, r := range c.callsTo[fn] {
		at := i + methodExprShift(r.pkg, r.call)
		if at < len(r.call.Args) && c.effectsBesides(r.pkg, r.call.Args[at], true, c.grammarCall) {
			return true
		}
	}
	return false
}

// methodExprShift is 1 for a method expression call such as
// Builder.Setup(b, app), whose receiver comes first, and 0 otherwise.
func methodExprShift(p *Package, call *dst.CallExpr) int {
	if sel, ok := call.Fun.(*dst.SelectorExpr); ok {
		if s := p.selection(sel); s != nil && s.Kind() == types.MethodExpr {
			return 1
		}
	}
	return 0
}

// grammarCall reports whether call builds kingpin grammar: a kingpin
// builder, as the interpreter saw it run.
func (c *converter) grammarCall(call *dst.CallExpr) bool {
	return c.g.in.grammarCalls[call]
}

// doesSomething reports whether evaluating e, typically a registration
// argument, does more than compute a value.
// That means calling something outside the program other than kingpin or a
// pure standard function, directly or through the program's own functions,
// using a channel, starting a goroutine, or writing anything but a local
// variable.
//
// Methods that only build option structs, as JSONFlag() does, don't count.
func (c *converter) doesSomething(p *Package, e dst.Node) bool {
	found := false
	dst.Inspect(e, func(n dst.Node) bool {
		if found {
			return false
		}
		// What the conversion moved, such as a help text computed at run
		// time, is evaluated where it went.
		if x, ok := n.(dst.Expr); ok && c.g.in.moved[x] && !c.g.in.superseded[x] {
			return false
		}
		switch n := n.(type) {
		case *dst.FuncLit:
			return false
		case *dst.CallExpr:
			if tv, ok := p.tv(n.Fun); ok && tv.IsType() {
				break
			}
			if b, ok := p.objectOf(n.Fun).(*types.Builtin); ok {
				// append may write into the array of the slice it is given.
				switch b.Name() {
				case "print", "println", "panic", "close", "delete", "copy", "clear":
					found = true
				case "append":
					found = !freshSlice(p, n.Args[0])
				}
				break
			}
			if c.grammarCall(n) || foldsPlainly(p, n) {
				break
			}
			fn := c.g.in.staticCallee(p, n)
			switch {
			case fn == nil:
				found = true
			case pureStd[fn.FullName()] || formatsStd(fn):
			case c.prog.funcs[fn].decl != nil:
				found = c.bodyDoesSomething(fn)
			default:
				found = fn.Pkg() == nil || !c.prog.isKingpinPath(fn.Pkg().Path())
			}
		case *dst.UnaryExpr:
			found = n.Op == token.ARROW
		case *dst.SendStmt, *dst.GoStmt:
			found = true
		case *dst.RangeStmt:
			found = rangeCalls(p, n)
		case *dst.IncDecStmt:
			found = c.writesOutside(p, n.X)
		case *dst.AssignStmt:
			found = slices.ContainsFunc(n.Lhs, func(l dst.Expr) bool { return c.writesOutside(p, l) })
		}
		return !found
	})
	return found
}

// rangeCalls reports whether a range statement receives from a channel or
// calls a function to get its values.
func rangeCalls(p *Package, r *dst.RangeStmt) bool {
	switch typeUnder(p.typeOf(r.X)).(type) {
	case *types.Chan, *types.Signature:
		return true
	}
	return false
}

// freshSlice reports whether e is nil or a slice literal.
// Nothing else shares such a slice, so appending to it changes nothing
// anyone sees.
func freshSlice(p *Package, e dst.Expr) bool {
	_, lit := unparen(e).(*dst.CompositeLit)
	return lit || isNilIdent(p, e)
}

// formatsStd reports whether fn is the String method of a standard
// library type that only formats its value, such as time.Duration's.
// An interface's, such as net.Addr's, may run anything.
func formatsStd(fn *types.Func) bool {
	sig := fn.Type().(*types.Signature)
	return fn.Name() == "String" && sig.Recv() != nil && !types.IsInterface(sig.Recv().Type()) && sig.Params().Len() == 0 && fn.Pkg() != nil && plainFormatters[fn.Pkg().Path()]
}

// plainFormatters are the standard packages whose String, Error and Format
// methods only format their values.
// That rules out packages such as expvar, whose Func calls back.
var plainFormatters = map[string]bool{
	"bytes": true, "errors": true, "fmt": true, "io/fs": true, "math/big": true, "net": true,
	"net/netip": true, "net/url": true, "os": true, "reflect": true, "regexp": true, "strings": true,
	"time": true,
}

// bodyDoesSomething is doesSomething for the body of fn, assuming, while
// it is being looked at, that a recursive call does something.
func (c *converter) bodyDoesSomething(fn *types.Func) bool {
	if v, seen := c.funcWork[fn]; seen {
		return v
	}
	c.funcWork[fn] = true
	fd := c.prog.funcs[fn]
	work := fd.decl.Body == nil || c.doesSomething(fd.pkg, fd.decl.Body)
	c.funcWork[fn] = work
	return work
}

// writesOutside reports whether assigning e may change something the
// caller sees: a package-level variable, or anything reached through a
// pointer, a field or an element, as in *p = 1 or c.n++.
// Only a plain local variable is the function's own.
func (c *converter) writesOutside(p *Package, e dst.Expr) bool {
	id, ok := unparen(e).(*dst.Ident)
	if !ok {
		return true
	}
	if id.Name == "_" {
		return false
	}
	v, ok := p.objectOf(id).(*types.Var)
	return !ok || v.Pkg() != nil && v.Parent() == v.Pkg().Scope()
}

type callRef struct {
	pkg  *Package
	call *dst.CallExpr
}

// interfaceMethods are the method names of the interfaces the program can
// see, like fmt.Stringer, through which a method may be called.
// kingpin's own interfaces don't count.
func (c *converter) interfaceMethods() map[string]bool {
	if c.ifaceMethods != nil {
		return c.ifaceMethods
	}
	ifaceMethods := map[string]bool{}
	seen := map[*types.Package]bool{}
	var visit func(t *types.Package)
	visit = func(t *types.Package) {
		if seen[t] || c.prog.isKingpinPath(t.Path()) {
			return
		}
		seen[t] = true
		scope := t.Scope()
		for _, name := range scope.Names() {
			tn, ok := scope.Lookup(name).(*types.TypeName)
			if !ok {
				continue
			}
			it, ok := tn.Type().Underlying().(*types.Interface)
			if !ok || c.registersOnly(it) {
				continue
			}
			for i := 0; i < it.NumMethods(); i++ {
				ifaceMethods[it.Method(i).Name()] = true
			}
		}
		for _, imp := range t.Imports() {
			visit(imp)
		}
	}
	for _, p := range c.prog.Packages {
		visit(p.Types)
		// Interfaces written in place, as in var x interface{ Setup() },
		// or declared inside a function.
		if p.TypesInfo == nil {
			continue
		}
		for _, tv := range p.TypesInfo.Types {
			if it, ok := typeUnder(tv.Type).(*types.Interface); ok && !c.registersOnly(it) {
				for i := range it.NumMethods() {
					ifaceMethods[it.Method(i).Name()] = true
				}
			}
		}
	}
	c.ifaceMethods = ifaceMethods
	return ifaceMethods
}

// funcValues are the functions the program uses other than by calling
// them, as in f := setup, whose signature must stay.
func (c *converter) funcValues() map[*types.Func]bool {
	if c.valueFuncs != nil {
		return c.valueFuncs
	}
	c.valueFuncs = map[*types.Func]bool{}
	for _, p := range c.prog.Packages {
		for _, f := range p.Syntax {
			dstutil.Apply(f, func(cur *dstutil.Cursor) bool {
				id, ok := cur.Node().(*dst.Ident)
				if !ok {
					return true
				}
				fn, ok := p.objectOf(id).(*types.Func)
				if !ok {
					return true
				}
				switch parent := cur.Parent().(type) {
				case *dst.CallExpr:
					if parent.Fun == dst.Expr(id) {
						return true
					}
				case *dst.SelectorExpr:
					return true
				case *dst.FuncDecl:
					return true
				}
				c.valueFuncs[fn] = true
				return true
			}, nil)
			// A method value such as c.Register, not called.
			dstutil.Apply(f, func(cur *dstutil.Cursor) bool {
				sel, ok := cur.Node().(*dst.SelectorExpr)
				if !ok {
					return true
				}
				// Builder.setup(b, app) shifts the arguments, so it counts.
				if tv, ok := p.tv(sel.X); !ok || !tv.IsType() {
					if call, isCall := cur.Parent().(*dst.CallExpr); isCall && call.Fun == dst.Expr(sel) {
						return true
					}
				}
				if fn, ok := p.objectOf(sel.Sel).(*types.Func); ok {
					c.valueFuncs[fn] = true
				}
				return true
			}, nil)
		}
	}
	return c.valueFuncs
}

// pureStd are the standard library functions taken as computing a value and
// nothing else: no callbacks, no panics on any argument.
var pureStd = map[string]bool{
	"os.Getenv": true, "os.LookupEnv": true,
	"strings.Contains": true, "strings.ContainsAny": true, "strings.ContainsRune": true, "strings.Count": true,
	"strings.Cut": true, "strings.CutPrefix": true, "strings.CutSuffix": true, "strings.EqualFold": true,
	"strings.Fields": true, "strings.HasPrefix": true, "strings.HasSuffix": true, "strings.Index": true,
	"strings.IndexByte": true, "strings.IndexRune": true, "strings.Join": true, "strings.LastIndex": true,
	"strings.Replace": true, "strings.ReplaceAll": true, "strings.Split": true, "strings.SplitN": true,
	"strings.ToLower": true, "strings.ToUpper": true, "strings.Trim": true, "strings.TrimLeft": true,
	"strings.TrimPrefix": true, "strings.TrimRight": true, "strings.TrimSpace": true, "strings.TrimSuffix": true,
	"strconv.Atoi": true, "strconv.FormatBool": true, "strconv.Itoa": true, "strconv.ParseBool": true,
	"strconv.ParseFloat": true, "strconv.ParseInt": true, "strconv.ParseUint": true, "strconv.Quote": true,
	"strconv.Unquote": true,
}

// hasEffects reports whether evaluating e in p may do something besides
// computing a value: call a function, receive from a channel, or panic, as
// indexing, dereferencing (explicitly or to reach a field), asserting a type
// or dividing may.
// With pure set, calls pureStd and pureCall accept are fine too.
func (c *converter) hasEffects(p *Package, e dst.Node, pure bool) bool {
	return c.effectsBesides(p, e, pure, nil)
}

// effectsBesides is hasEffects, not counting the calls skip accepts.
func (c *converter) effectsBesides(p *Package, e dst.Node, pure bool, skip func(*dst.CallExpr) bool) bool {
	found := false
	dst.Inspect(e, func(n dst.Node) bool {
		if found {
			return false
		}
		// A type, such as []*T, computes nothing.
		if x, ok := n.(dst.Expr); ok {
			if tv, ok := p.tv(x); ok && tv.IsType() {
				return false
			}
		}
		switch n := n.(type) {
		case *dst.FuncLit:
			return false
		case *dst.CallExpr:
			// A skipped call's arguments and receiver still count.
			if skip != nil && skip(n) {
				parts := append([]dst.Expr{}, n.Args...)
				if sel, ok := n.Fun.(*dst.SelectorExpr); ok {
					parts = append(parts, sel.X)
				}
				found = slices.ContainsFunc(parts, func(e dst.Expr) bool { return c.effectsBesides(p, e, pure, skip) })
				return false
			}
			if tv, ok := p.tv(n.Fun); ok && tv.IsType() {
				// Converting a slice to an array panics if it is too short.
				found = len(n.Args) == 1 && isSlice(p.typeOf(n.Args[0])) && !isSlice(tv.Type)
				break
			}
			if b, ok := p.objectOf(n.Fun).(*types.Builtin); ok {
				switch b.Name() {
				case "len", "cap", "new":
					break
				case "make":
					// Only a size that is too large or negative panics.
					found = slices.ContainsFunc(n.Args[1:], func(a dst.Expr) bool { return !smallConstant(p, a) })
				default:
					found = true
				}
				break
			}
			fn, _ := p.objectOf(n.Fun).(*types.Func)
			found = !pure || fn == nil || !pureStd[fn.FullName()] && !c.pureCall(p, n)
		case *dst.UnaryExpr:
			found = n.Op == token.ARROW
		case *dst.RangeStmt:
			found = rangeCalls(p, n)
		case *dst.BinaryExpr:
			found = n.Op == token.QUO || n.Op == token.REM || n.Op == token.SHL || n.Op == token.SHR
			// Comparing interfaces holding slices or maps panics.
			if (n.Op == token.EQL || n.Op == token.NEQ) && !isNilIdent(p, n.X) && !isNilIdent(p, n.Y) {
				found = found || holdsInterface(p.typeOf(n.X)) || holdsInterface(p.typeOf(n.Y))
			}
		case *dst.CompositeLit:
			// So does hashing such a key into a map.
			if m, ok := typeUnder(p.typeOf(n)).(*types.Map); ok && len(n.Elts) > 0 {
				found = holdsInterface(m.Key())
			}
		case *dst.IndexExpr, *dst.IndexListExpr, *dst.SliceExpr, *dst.StarExpr, *dst.TypeAssertExpr:
			found = true
		case *dst.SelectorExpr:
			// Going through a nil pointer or taking a method from a nil
			// interface panics too.
			// A kingpin clause reached through a command, as in
			// c.CmdClause, is just grammar plumbing.
			if sel := p.selection(n); sel != nil && (sel.Indirect() && !c.prog.isKingpinType(sel.Type()) || sel.Kind() == types.MethodVal && types.IsInterface(sel.Recv())) {
				found = true
			}
		}
		return !found
	})
	return found
}

func smallConstant(p *Package, e dst.Expr) bool {
	tv, ok := p.tv(e)
	if !ok || tv.Value == nil {
		return false
	}
	n, exact := constant.Int64Val(constant.ToInt(tv.Value))
	return exact && n >= 0 && n <= 1<<16
}

func isNilIdent(p *Package, e dst.Expr) bool {
	_, ok := p.objectOf(unparen(e)).(*types.Nil)
	return ok
}

func typeUnder(t types.Type) types.Type {
	if t == nil {
		return nil
	}
	return t.Underlying()
}

func isSlice(t types.Type) bool {
	_, ok := typeUnder(t).(*types.Slice)
	return ok
}

// holdsInterface reports whether a value of type t may hold an interface,
// directly or in a field or an array element, whose dynamic value cannot
// be compared.
func holdsInterface(t types.Type) bool {
	switch u := typeUnder(t).(type) {
	case *types.Interface:
		return true
	case *types.Struct:
		for i := range u.NumFields() {
			if holdsInterface(u.Field(i).Type()) {
				return true
			}
		}
	case *types.Array:
		return holdsInterface(u.Elem())
	}
	return false
}

// pureCall reports whether e calls, without arguments, a function of the
// program that only computes a value from pureStd calls, as a helper
// reading an environment variable does.
func (c *converter) pureCall(p *Package, e dst.Expr) bool {
	call, ok := e.(*dst.CallExpr)
	if !ok || slices.ContainsFunc(call.Args, func(a dst.Expr) bool { return c.hasEffects(p, a, true) }) {
		return false
	}
	// A method would have its receiver to evaluate too.
	id, ok := call.Fun.(*dst.Ident)
	if !ok {
		return false
	}
	fn, ok := p.objectOf(id).(*types.Func)
	if !ok {
		return false
	}
	if pure, seen := c.pureCalls[fn]; seen {
		return pure
	}
	fd, ok := c.prog.funcs[fn]
	if !ok || !simpleBody(fd.decl) {
		return false
	}
	// A function calling itself may never return, so it is not pure while
	// it is being looked at.
	c.pureCalls[fn] = false
	pure := !c.hasEffects(fd.pkg, fd.decl.Body, true)
	c.pureCalls[fn] = pure
	return pure
}

// pruneSignature drops parameters of kingpin types that are no longer used,
// and results that only returned removed values.
// When fd must keep its signature, its callers pass nil to those
// parameters instead.
func (c *converter) pruneSignature(p *Package, fd *dst.FuncDecl) {
	fn, _ := p.objectOf(fd.Name).(*types.Func)
	sig := prunedSig{params: map[int]bool{}, nils: map[int]bool{}}
	fixed := c.fixedResults
	if fd.Type.Params != nil {
		var kept []*dst.Field
		idx := 0
		for _, f := range fd.Type.Params.List {
			if len(f.Names) == 0 {
				idx++
			}
			var names []*dst.Ident
			for _, nm := range f.Names {
				o := p.objectOf(nm)
				idx++
				// The parameter stays if some caller passes an argument
				// that does something.
				if o != nil && (c.prog.isKingpinType(o.Type()) || c.registersOnly(o.Type())) && !c.usedIn(p, fd.Body, o) && !c.listParams[o] && !c.argumentDoesWork(fn, idx-1) {
					if fixed {
						sig.nils[idx-1] = true
					} else {
						sig.params[idx-1] = true
						continue
					}
				}
				names = append(names, nm)
			}
			if len(names) == 0 && len(f.Names) > 0 {
				continue
			}
			f.Names = names
			kept = append(kept, f)
		}
		fd.Type.Params.List = kept
	}
	// Results go once no return gives them a value.
	// Named ones must also be unused, since a bare return returns them.
	results := fd.Type.Results
	if !fixed && results != nil && len(results.List) > 0 && !returnsValues(fd.Body) &&
		(len(results.List[0].Names) == 0 || c.deadNamedResults(p, fd)) {
		fd.Type.Results = nil
		sig.results = true
	}
	if fn != nil && (len(sig.params) > 0 || len(sig.nils) > 0 || sig.results) {
		c.pruned[fn] = sig
	}
}

// returnsValues reports whether body has a return with values, not counting
// function literals.
func returnsValues(body *dst.BlockStmt) bool {
	found := false
	dst.Inspect(body, func(n dst.Node) bool {
		if _, ok := n.(*dst.FuncLit); ok {
			return false
		}
		if r, ok := n.(*dst.ReturnStmt); ok && len(r.Results) > 0 {
			found = true
		}
		return !found
	})
	return found
}

// deadNamedResults reports whether fd's named results are all kingpin
// values that nothing uses any more.
func (c *converter) deadNamedResults(p *Package, fd *dst.FuncDecl) bool {
	for _, f := range fd.Type.Results.List {
		for _, nm := range f.Names {
			o := p.objectOf(nm)
			if o == nil || !c.prog.isKingpinType(o.Type()) || c.usedIn(p, fd.Body, o) {
				return false
			}
		}
	}
	return true
}

// fixedSignature reports whether fd must keep its signature, because it is
// used as a value or is a method an interface may call.
func (c *converter) fixedSignature(p *Package, fd *dst.FuncDecl) bool {
	fn, _ := p.objectOf(fd.Name).(*types.Func)
	return fn != nil && (c.funcValues()[fn] || fd.Recv != nil && c.interfaceMethods()[fn.Name()])
}

// registersOnly reports whether t is an interface whose methods all build
// kingpin grammar, such as an interface with a single
// Register(*kingpin.Application) method.
func (c *converter) registersOnly(t types.Type) bool {
	it, ok := t.Underlying().(*types.Interface)
	if !ok || it.NumMethods() == 0 {
		return false
	}
	for i := 0; i < it.NumMethods(); i++ {
		sig := it.Method(i).Type().(*types.Signature)
		kp := false
		for j := 0; j < sig.Results().Len(); j++ {
			if c.prog.isKingpinType(sig.Results().At(j).Type()) {
				kp = true
			}
		}
		if !kp {
			return false
		}
	}
	return true
}

func (c *converter) usedIn(p *Package, n dst.Node, o types.Object) bool {
	used := false
	dst.Inspect(n, func(n dst.Node) bool {
		if id, ok := n.(*dst.Ident); ok && p.objectOf(id) == o {
			used = true
		}
		return !used
	})
	return used
}

type declRef struct {
	pkg  *Package
	file *dst.File
	decl dst.Decl
	objs []types.Object
	refs []types.Object
}

// removeDeadCode deletes unreferenced declarations that belonged to the
// kingpin registration: helper functions, option structs, flag helpers and
// kingpin Set actions.
// It repeats until nothing else becomes unreferenced.
func (c *converter) removeDeadCode() {
	ifaceMethods := c.interfaceMethods()
	removedObjs := map[types.Object]bool{}
	for d := range c.ed.removed {
		if fd, ok := d.(*dst.FuncDecl); ok {
			for _, p := range c.prog.Packages {
				if o := p.objectOf(fd.Name); o != nil {
					removedObjs[o] = true
				}
			}
		}
	}
	for round := 0; round < 20; round++ {
		var decls []*declRef
		byObj := map[types.Object]*declRef{}
		for _, p := range c.prog.Packages {
			for _, f := range p.Syntax {
				for _, d := range f.Decls {
					if c.ed.removed[d] {
						continue
					}
					dr := &declRef{pkg: p, file: f, decl: d}
					own := map[types.Object]bool{}
					switch d := d.(type) {
					case *dst.FuncDecl:
						if o := p.objectOf(d.Name); o != nil {
							dr.objs = append(dr.objs, o)
							own[o] = true
						}
					case *dst.GenDecl:
						for _, s := range d.Specs {
							switch s := s.(type) {
							case *dst.TypeSpec:
								if o := p.objectOf(s.Name); o != nil {
									dr.objs = append(dr.objs, o)
									own[o] = true
								}
							case *dst.ValueSpec:
								for _, nm := range s.Names {
									if o := p.objectOf(nm); o != nil {
										dr.objs = append(dr.objs, o)
										own[o] = true
									}
								}
							}
						}
					}
					dst.Inspect(d, func(n dst.Node) bool {
						id, ok := n.(*dst.Ident)
						if !ok {
							return true
						}
						o := p.objectOf(id)
						if o == nil || own[o] {
							return true
						}
						dr.refs = append(dr.refs, o)
						// The identifier of an embedded field resolves to the
						// field, but it names the embedded type as well.
						if v, ok := o.(*types.Var); ok && v.Anonymous() {
							if n, ok := deref(v.Type()).(*types.Named); ok {
								dr.refs = append(dr.refs, n.Obj())
							}
						}
						return true
					})
					for _, o := range dr.objs {
						byObj[o] = dr
					}
					decls = append(decls, dr)
				}
			}
		}
		// A declaration is live if it is protected (main, init, an interface
		// method, something a test mentions, generated code) or if a live
		// declaration refers to it.
		// Everything else is dead, including cycles of dead code.
		live := map[*declRef]bool{}
		var queue []*declRef
		for _, dr := range decls {
			if c.protected(dr, ifaceMethods) {
				live[dr] = true
				queue = append(queue, dr)
			}
		}
		for len(queue) > 0 {
			dr := queue[0]
			queue = queue[1:]
			for _, o := range dr.refs {
				if t := byObj[o]; t != nil && !live[t] {
					live[t] = true
					queue = append(queue, t)
				}
			}
		}
		// Only remove dead code tied to kingpin: code that mentions kingpin,
		// or that uses dead code that does.
		tainted := map[*declRef]bool{}
		for changed := true; changed; {
			changed = false
			for _, dr := range decls {
				if live[dr] || tainted[dr] {
					continue
				}
				t := c.tainted(dr.pkg, dr.decl, removedObjs)
				for _, o := range dr.refs {
					if r := byObj[o]; r != nil && tainted[r] {
						t = true
					}
				}
				if t {
					tainted[dr] = true
					changed = true
				}
			}
		}
		if len(tainted) == 0 {
			return
		}
		for _, dr := range decls {
			if !tainted[dr] {
				continue
			}
			c.ed.removeDecl(dr.pkg, dr.file, dr.decl)
			for _, o := range dr.objs {
				removedObjs[o] = true
			}
			c.res.Stats.RemovedFuncs++
		}
	}
}

// protected reports whether a declaration must be kept even when nothing in
// the loaded code refers to it.
func (c *converter) protected(dr *declRef, ifaceMethods map[string]bool) bool {
	if len(dr.objs) == 0 {
		return true
	}
	// A package-level initializer doing anything but build kingpin grammar
	// may have effects the program needs, a panic included, even if its
	// value is never read.
	if gd, ok := dr.decl.(*dst.GenDecl); ok && gd.Tok == token.VAR {
		grammar := func(call *dst.CallExpr) bool { return c.g.in.isKingpinCall(dr.pkg, call) }
		if c.effectsBesides(dr.pkg, gd, true, grammar) {
			return true
		}
	}
	for _, o := range dr.objs {
		// Types the generated grammar and hooks refer to.
		if tn, ok := o.(*types.TypeName); ok {
			if n, ok := tn.Type().(*types.Named); ok && c.generatedUses(n) {
				return true
			}
		}
		if o.Name() == "main" || o.Name() == "init" || o.Name() == "_" || c.keep[o] {
			return true
		}
		method := false
		if fn, ok := o.(*types.Func); ok && fn.Type().(*types.Signature).Recv() != nil {
			method = true
			if ifaceMethods[fn.Name()] {
				return true
			}
		}
		dir := filepath.Dir(dr.pkg.filename(dr.file))
		if c.testRefs.mentions(dir, dr.pkg.PkgPath, o.Name(), method) {
			return true
		}
	}
	return false
}

// generatedUses reports whether generated code refers to t: command structs in
// the grammar, the struct holding the application's flags, was-set wrappers
// and hooks.
func (c *converter) generatedUses(t *types.Named) bool {
	if c.cmdTypes[t] || c.rootFlags == t || c.plan.wrappers[t] != nil || c.plan.markers[t] != nil || c.hooks[t] != nil {
		return true
	}
	return false
}

// tainted reports whether a declaration belonged to the kingpin wiring.
// That is the case when it mentions kingpin or a value of a kingpin type, only
// registers grammar, or refers to something already removed.
func (c *converter) tainted(p *Package, d dst.Decl, removed map[types.Object]bool) bool {
	if fd, ok := d.(*dst.FuncDecl); ok {
		if fn, ok := p.objectOf(fd.Name).(*types.Func); ok && c.regOnly[fn] {
			return true
		}
	}
	found := false
	dst.Inspect(d, func(n dst.Node) bool {
		id, ok := n.(*dst.Ident)
		if !ok {
			return true
		}
		if id.Path != "" && c.prog.isKingpinPath(id.Path) {
			found = true
		}
		if o := p.objectOf(id); o != nil {
			if removed[o] {
				found = true
			}
			if v, ok := o.(*types.Var); ok && c.prog.isKingpinType(v.Type()) {
				found = true
			}
		}
		return !found
	})
	return found
}

// testRefs holds the names used by Go files the conversion does not load:
// tests, and files excluded by build constraints.
// Code they mention is never deleted.
type testRefs struct {
	// bare holds identifiers used in those files, by directory.
	bare map[string]map[string]bool
	// qualified holds pkg.Name references, by imported package path.
	qualified map[string]map[string]bool
	// selectors holds every x.Name selector name, which may be a method.
	selectors map[string]bool
}

func (r *testRefs) mentions(pkgDir, pkgPath, name string, method bool) bool {
	if method {
		return r.selectors[name]
	}
	return r.bare[pkgDir][name] || r.qualified[pkgPath][name]
}

// scanUnloaded collects the names used by the module's Go files the
// conversion does not load, and reports the files still importing kingpin.
func (c *converter) scanUnloaded(dir string) *testRefs {
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	loaded := map[string]bool{}
	loadedDirs := map[string]bool{}
	for _, p := range c.prog.Packages {
		for _, f := range p.Syntax {
			loaded[p.filename(f)] = true
			loadedDirs[filepath.Dir(p.filename(f))] = true
		}
	}
	outside := map[string]bool{}
	r := &testRefs{bare: map[string]map[string]bool{}, qualified: map[string]map[string]bool{}, selectors: map[string]bool{}}
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if path != dir && (strings.HasPrefix(name, ".") || name == "vendor" || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || loaded[path] {
			return nil
		}
		test := strings.HasSuffix(path, "_test.go")
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return nil
		}
		// Another program's files, outside the packages converted, stay as
		// they are and keep nothing alive.
		if fileDir := filepath.Dir(path); !loadedDirs[fileDir] && !onlyTests(fileDir) {
			for _, imp := range f.Imports {
				if ip, _ := strconv.Unquote(imp.Path.Value); c.prog.isKingpinPath(ip) && !outside[fileDir] {
					outside[fileDir] = true
					c.res.Notes = append(c.res.Notes, fmt.Sprintf("%s uses kingpin too, outside the packages converted.", fileDir))
				}
			}
			return nil
		}
		imports := map[string]string{}
		var dotImports []string
		for _, imp := range f.Imports {
			ip, _ := strconv.Unquote(imp.Path.Value)
			if c.prog.isKingpinPath(ip) && !test {
				c.issue(token.Position{Filename: path}, "this file is left out of the build by its build constraints and still uses kingpin; convert it by hand")
			}
			if c.prog.isKingpinPath(ip) && test {
				c.kingpinTests[path] = true
			}
			name := filepath.Base(ip)
			if imp.Name != nil {
				name = imp.Name.Name
			}
			if name == "." {
				dotImports = append(dotImports, ip)
				continue
			}
			imports[name] = ip
		}
		d2 := filepath.Dir(path)
		if r.bare[d2] == nil {
			r.bare[d2] = map[string]bool{}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.Ident:
				r.bare[d2][n.Name] = true
				for _, ip := range dotImports {
					if r.qualified[ip] == nil {
						r.qualified[ip] = map[string]bool{}
					}
					r.qualified[ip][n.Name] = true
				}
			case *ast.SelectorExpr:
				r.selectors[n.Sel.Name] = true
				if x, ok := n.X.(*ast.Ident); ok {
					if ip, ok := imports[x.Name]; ok {
						if r.qualified[ip] == nil {
							r.qualified[ip] = map[string]bool{}
						}
						r.qualified[ip][n.Sel.Name] = true
					}
				}
			}
			return true
		})
		return nil
	})
	return r
}

// newFile records a generated file for p.
func (c *converter) newFile(p *Package, name string, imports map[string]string, src string) {
	decls, err := snippet(p.Name, imports, src)
	if err != nil {
		c.genErr = fmt.Errorf("generating %s: %w", name, err)
		return
	}
	f := &dst.File{Name: dst.NewIdent(p.Name), Decls: decls}
	dir := ""
	for _, sf := range p.Syntax {
		dir = filepath.Dir(p.filename(sf))
		break
	}
	path := filepath.Join(dir, name)
	aliases := map[string]string{}
	for ip, a := range imports {
		name := p.names[ip]
		if name == "" {
			name = knownPackageNames[ip]
		}
		if name != a {
			aliases[ip] = a
		}
	}
	src2, err := p.renderFileWithAliases(f, aliases)
	if err != nil {
		c.genErr = fmt.Errorf("rendering %s: %w", path, err)
		return
	}
	module := p.modulePath()
	// Group imports the usual way: standard library, other modules, then this
	// module's own packages.
	layout := fmt.Sprintf("package p\n\nimport (\n\t\"fmt\"\n\n\t\"github.com/alecthomas/kong\"\n\n\t\"%s/x\"\n)\n", module)
	c.res.NewFiles[path] = string(regroupImports([]byte(layout), []byte(src2), module, c.prog.isKingpinPath))
}

// render turns every changed file back into source.
func (c *converter) render() error {
	var errs []string
	reported := map[string]bool{}
	for _, is := range c.res.Issues {
		reported[is.Pos.Filename] = true
	}
	for f, p := range c.ed.dirty {
		var decls []dst.Decl
		for _, d := range f.Decls {
			if !c.ed.removed[d] {
				decls = append(decls, d)
			}
		}
		f.Decls = decls
		if name := p.filename(f); !reported[name] && !strings.HasSuffix(name, "_test.go") {
			c.reportKingpinLeft(p, f)
		}
		src, err := p.renderFile(f)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		if orig, err := os.ReadFile(p.filename(f)); err == nil {
			src = string(regroupImports(orig, []byte(src), p.modulePath(), c.prog.isKingpinPath))
		}
		c.res.Files[p.filename(f)] = src
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return fmt.Errorf("%s", strings.Join(errs, "\n"))
	}
	return nil
}

// renderFile prints a decorated file with its imports recomputed from the
// package references it contains.
func (p *Package) renderFile(f *dst.File) (string, error) {
	return p.renderFileWithAliases(f, nil)
}

func (p *Package) renderFileWithAliases(f *dst.File, aliases map[string]string) (string, error) {
	r := decorator.NewRestorerWithImports(p.PkgPath, p.importNames())
	fr := r.FileRestorer()
	for path, a := range sharedImportNames(p, f) {
		fr.Alias[path] = a
	}
	for path, a := range aliases {
		fr.Alias[path] = a
	}
	var buf bytes.Buffer
	if err := fr.Fprint(&buf, f); err != nil {
		return "", fmt.Errorf("restoring %s: %w", p.PkgPath, err)
	}
	out, err := format.Source(buf.Bytes())
	if err != nil {
		return buf.String(), fmt.Errorf("formatting %s: %w", p.PkgPath, err)
	}
	return string(out), nil
}

// reportKingpinLeft reports the first use of kingpin left in a converted
// file.
// render skips files that already have an issue, and test files, which the
// test conversion handles.
func (c *converter) reportKingpinLeft(p *Package, f *dst.File) {
	var left *dst.Ident
	dst.Inspect(f, func(n dst.Node) bool {
		if id, ok := n.(*dst.Ident); ok && left == nil && c.prog.isKingpinPath(id.Path) {
			left = id
		}
		return left == nil
	})
	if left == nil {
		return
	}
	pos := p.position(left)
	if !pos.IsValid() {
		pos = token.Position{Filename: p.filename(f)}
	}
	c.issue(pos, "this file still uses kingpin's %s after the conversion; finish it by hand", left.Name)
}

// sharedImportNames picks a single name for each package f imports under
// several names, a dot import included.
// The restorer writes only one import per package, so every reference has
// to use the same name.
//
// It is the first name that f doesn't declare and no other import uses,
// or a numbered one when none is free.
// "" stands for the package's own name, which needs no alias.
func sharedImportNames(p *Package, f *dst.File) map[string]string {
	names := map[string][]string{}
	var paths []string
	for _, d := range f.Decls {
		gd, ok := d.(*dst.GenDecl)
		if !ok || gd.Tok != token.IMPORT {
			continue
		}
		for _, s := range gd.Specs {
			imp := s.(*dst.ImportSpec)
			path, name := p.importName(imp)
			if names[path] == nil {
				paths = append(paths, path)
			}
			names[path] = append(names[path], name)
		}
	}
	declared := map[string]bool{}
	dst.Inspect(f, func(n dst.Node) bool {
		switch n := n.(type) {
		case *dst.ImportSpec:
			return false
		case *dst.Ident:
			if n.Path == "" {
				declared[n.Name] = true
			}
		}
		return true
	})
	out := map[string]string{}
	chosen := map[string]bool{}
	for _, path := range paths {
		own := p.names[path]
		if len(names[path]) < 2 || own == "" {
			continue
		}
		var candidates []string
		for _, n := range names[path] {
			if n != "." && n != "_" {
				candidates = append(candidates, n)
			}
		}
		candidates = append(candidates, own)
		taken := func(n string) bool {
			if declared[n] || chosen[n] {
				return true
			}
			for other, ons := range names {
				if other != path && slices.Contains(ons, n) {
					return true
				}
			}
			return false
		}
		name := firstFree(taken, candidates...)
		chosen[name] = true
		if name == own {
			name = ""
		}
		out[path] = name
	}
	// A package the conversion refers to anew, such as kong or os for
	// os.Args, must not be hidden by a local of the same name.
	added := map[string]bool{}
	dst.Inspect(f, func(n dst.Node) bool {
		if id, ok := n.(*dst.Ident); ok && id.Path != "" && id.Path != p.PkgPath && (names[id.Path] == nil || len(names[id.Path]) == 1 && p.astOf(id) == nil) {
			added[id.Path] = true
		}
		return true
	})
	for _, path := range slices.Sorted(maps.Keys(added)) {
		own := cmp.Or(knownPackageNames[path], p.names[path], pathpkg.Base(path))
		if ns := names[path]; len(ns) == 1 {
			if ns[0] == "." || ns[0] == "_" {
				continue
			}
			own = ns[0]
		}
		taken := func(n string) bool {
			return declared[n] || chosen[n] || slices.ContainsFunc(slices.Collect(maps.Values(names)), func(ns []string) bool { return n != own && slices.Contains(ns, n) })
		}
		if taken(own) {
			name := firstFree(taken, own)
			chosen[name] = true
			out[path] = name
		}
	}
	return out
}

// tidyBody drops a trailing bare return in a function without results, and
// the blank line removed statements leave at the top of the body.
func tidyBody(fd *dst.FuncDecl) {
	list := fd.Body.List
	if fd.Type.Results == nil && len(list) > 0 {
		if r, ok := list[len(list)-1].(*dst.ReturnStmt); ok && len(r.Results) == 0 {
			list = list[:len(list)-1]
		}
	}
	if len(list) > 0 {
		switch s := list[0].(type) {
		case *dst.AssignStmt:
			s.Decs.Before = dst.NewLine
		case *dst.IfStmt:
			s.Decs.Before = dst.NewLine
		case *dst.ExprStmt:
			s.Decs.Before = dst.NewLine
		case *dst.DeclStmt:
			s.Decs.Before = dst.NewLine
		case *dst.ReturnStmt:
			s.Decs.Before = dst.NewLine
		}
	}
	fd.Body.List = list
}

func (p *Package) modulePath() string {
	if p.Module != nil {
		return p.Module.Path
	}
	return p.PkgPath
}
