package conv

import (
	"cmp"
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dave/dst"
)

// interp runs registration code symbolically.
type interp struct {
	prog *Program

	clauses []*clause
	objects []*object
	sites   []*callSite
	app     *clause // the most recent application
	cmdLine *clause // kingpin.CommandLine, once the program uses it
	apps    []*clause

	// relevant holds the functions that call kingpin builders, directly or
	// through other functions.
	relevant map[*types.Func]bool

	stack []*types.Func

	// touched holds every statement, at any call depth, that was running
	// when a kingpin builder was called.
	touched map[dst.Stmt]bool
	// stmtStack is the statement being executed at each frame level.
	stmtStack []dst.Stmt

	// escapes records kingpin values passed to code the interpreter did not
	// follow, typically the parse and usage calls of the application layer.
	escapes []escape

	// stmtVals holds the values each statement assigned or computed the last
	// time it ran.
	// The cleanup uses them to tell grammar plumbing from real work.
	stmtVals map[dst.Stmt][]value
	// globals holds the values of package-level variables whose
	// initializers registered grammar, and specVals the values of each such
	// declaration.
	globals  map[*types.Var]value
	specVals map[*dst.ValueSpec][]value
	// structVars are the package-level variables holding a struct built in
	// place, as in ls = &lsCmd{}, which globals also holds once read.
	structVars map[*types.Var]bool
	// varVals is the last value stored in each local variable.
	varVals map[types.Object]value
	// eltVals is the value of each element of the slice literals that ran,
	// or vUnknown when it changed from one run to another.
	eltVals map[dst.Expr]value
	// grammarCalls are the calls that declared grammar.
	grammarCalls map[*dst.CallExpr]bool
	// moved are the arguments of grammar calls the conversion computes
	// elsewhere, such as a default value only known at run time.
	moved map[dst.Expr]bool
	// superseded are the moved arguments a later call overrides, as Help
	// overrides the help given to Flag.
	// They no longer feed the parser, but what they do still runs.
	superseded map[dst.Expr]bool
	// evaluations counts the run-time defaults and help texts seen so far.
	evaluations int
	executed    map[dst.Stmt]bool
	// initCalled are the functions that already ran while setting up
	// package variables.
	// inInit is true during that setup.
	initCalled map[*types.Func]bool
	inInit     bool
	// globalWrites is the last value stored in each package-level variable.
	// Unlike globals, it keeps any kind of value, such as what a flag's
	// variable starts with.
	globalWrites map[*types.Var]value
	// valueCallees are the functions called through function values, as in
	// cb(app) after cb := setup, or nil when different runs called different
	// functions.
	valueCallees map[*dst.CallExpr]*types.Func

	nextID int
}

type escape struct {
	pos  token.Position
	what string
	// report is set for grammar the tool may have missed, which a person
	// has to look at.
	report bool
	// fatal is set when the application is handed to code outside the
	// program, which may register more on it or parse with it.
	// No conversion of the program alone can be right then.
	fatal bool
}

type frame struct {
	pkg   *Package
	fn    *types.Func
	vars  map[types.Object]value
	conds []cond
	ret   []value
	done  bool
}

func newInterp(prog *Program) *interp {
	in := &interp{
		prog:         prog,
		touched:      map[dst.Stmt]bool{},
		stmtVals:     map[dst.Stmt][]value{},
		grammarCalls: map[*dst.CallExpr]bool{},
		moved:        map[dst.Expr]bool{},
		superseded:   map[dst.Expr]bool{},
		eltVals:      map[dst.Expr]value{},
		executed:     map[dst.Stmt]bool{},
		globals:      map[*types.Var]value{},
		structVars:   map[*types.Var]bool{},
		specVals:     map[*dst.ValueSpec][]value{},
		varVals:      map[types.Object]value{},
		valueCallees: map[*dst.CallExpr]*types.Func{},
		globalWrites: map[*types.Var]value{},
		initCalled:   map[*types.Func]bool{},
	}
	in.relevant = in.computeRelevant()
	return in
}

func (in *interp) newObject(t types.Type, known bool, site string) *object {
	in.nextID++
	o := &object{
		id:     in.nextID,
		typ:    t,
		st:     structOf(t),
		known:  known,
		fields: map[*types.Var]value{},
		site:   site,
	}
	in.objects = append(in.objects, o)
	return o
}

func (in *interp) newClause(kind clauseKind, parent *clause, pos token.Position, fr *frame) *clause {
	in.nextID++
	c := &clause{id: in.nextID, kind: kind, parent: parent, pos: pos}
	if fr != nil {
		c.conds = append(c.conds, fr.conds...)
		c.pkg = fr.pkg
	}
	if n := len(in.stmtStack); n > 0 {
		c.stmt = in.stmtStack[n-1]
	}
	if parent != nil {
		parent.children = append(parent.children, c)
	}
	in.clauses = append(in.clauses, c)
	if kind != kindApp {
		// Creating the application is left to the application layer, which
		// keeps using it until it is ported to kong.
		in.markTouched()
	}
	return c
}

func (in *interp) markTouched() {
	for _, s := range in.stmtStack {
		if s != nil {
			in.touched[s] = true
		}
	}
}

// computeRelevant finds the functions that reach a kingpin builder call.
func (in *interp) computeRelevant() map[*types.Func]bool {
	direct := map[*types.Func]bool{}
	calls := map[*types.Func][]*types.Func{}
	for fn, fd := range in.prog.funcs {
		if fd.decl.Body == nil {
			continue
		}
		p := fd.pkg
		dst.Inspect(fd.decl.Body, func(n dst.Node) bool {
			c, ok := n.(*dst.CallExpr)
			if !ok {
				return true
			}
			if in.isKingpinCall(p, c) && !in.readsModel(p, c) {
				direct[fn] = true
			}
			if callee := in.staticCallee(p, c); callee != nil {
				calls[fn] = append(calls[fn], callee)
			}
			return true
		})
	}
	rel := map[*types.Func]bool{}
	for fn := range direct {
		rel[fn] = true
	}
	for changed := true; changed; {
		changed = false
		for fn, cs := range calls {
			if rel[fn] {
				continue
			}
			for _, c := range cs {
				if rel[c] {
					rel[fn] = true
					changed = true
					break
				}
			}
		}
	}
	return rel
}

// readsModel reports whether a call only reads kingpin's usage model or a
// parse context, such as m.FlagSummary(), which registers nothing.
func (in *interp) readsModel(p *Package, c *dst.CallExpr) bool {
	sel, ok := c.Fun.(*dst.SelectorExpr)
	if !ok {
		return false
	}
	n, ok := deref(p.typeOf(sel.X)).(*types.Named)
	if !ok || !in.prog.isKingpinType(n) {
		return false
	}
	name := n.Obj().Name()
	return strings.HasSuffix(name, "Model") || strings.HasPrefix(name, "Parse") || name == "Token"
}

// isKingpinCall reports whether c calls into kingpin: a kingpin package
// function, a method on a kingpin value, or any call that yields one (such as
// an interface method returning *kingpin.CmdClause).
func (in *interp) isKingpinCall(p *Package, c *dst.CallExpr) bool {
	if t := p.typeOf(c); t != nil {
		if tup, ok := t.(*types.Tuple); ok {
			for i := 0; i < tup.Len(); i++ {
				if in.prog.isKingpinType(tup.At(i).Type()) {
					return true
				}
			}
		} else if in.prog.isKingpinType(t) {
			return true
		}
	}
	switch fun := c.Fun.(type) {
	case *dst.SelectorExpr:
		if in.prog.isKingpinType(p.typeOf(fun.X)) {
			return true
		}
	case *dst.Ident:
		if fun.Path != "" && in.prog.isKingpinPath(fun.Path) {
			return true
		}
	}
	return false
}

func (in *interp) staticCallee(p *Package, c *dst.CallExpr) *types.Func {
	switch fun := c.Fun.(type) {
	case *dst.Ident:
		fn, _ := p.objectOf(fun).(*types.Func)
		return fn
	case *dst.SelectorExpr:
		fn, _ := p.objectOf(fun).(*types.Func)
		return fn
	}
	return nil
}

// runPackageVars interprets the package-level variable declarations that
// build grammar, such as `var verbose = kingpin.Flag("verbose", "").Bool()`,
// in the order Go runs them.
func (in *interp) runPackageVars() {
	for _, p := range in.prog.Packages {
		fr := &frame{pkg: p, vars: map[types.Object]value{}}
		var specs []*dst.ValueSpec
		for _, f := range p.Syntax {
			for _, d := range f.Decls {
				gd, ok := d.(*dst.GenDecl)
				if !ok || gd.Tok != token.VAR {
					continue
				}
				for _, sp := range gd.Specs {
					vs := sp.(*dst.ValueSpec)
					if len(vs.Values) > 0 && in.mentionsBuilder(p, vs) {
						specs = append(specs, vs)
					}
				}
			}
		}
		// var verbose = app.Flag(...).Bool() may be declared before
		// var app = kingpin.New(...), but Go runs it after.
		order := map[ast.Expr]int{}
		for i, init := range p.TypesInfo.InitOrder {
			order[init.Rhs] = i
		}
		rank := func(vs *dst.ValueSpec) int {
			if a, ok := p.astOf(vs.Values[0]).(ast.Expr); ok {
				if i, ok := order[a]; ok {
					return i
				}
			}
			return len(order)
		}
		slices.SortStableFunc(specs, func(a, b *dst.ValueSpec) int { return cmp.Compare(rank(a), rank(b)) })
		in.inInit = true
		for _, vs := range specs {
			var vals []value
			if len(vs.Values) == 1 && len(vs.Names) > 1 {
				vals = in.multi(fr, vs.Values[0])
			} else {
				for _, e := range vs.Values {
					vals = append(vals, in.eval(fr, e))
				}
			}
			in.specVals[vs] = vals
			for i, nm := range vs.Names {
				if v, ok := p.objectOf(nm).(*types.Var); ok && i < len(vals) {
					in.globals[v] = vals[i]
				}
			}
		}
		in.inInit = false
	}
}

// run interprets fn as a registration root with unknown parameters.
func (in *interp) run(fn *types.Func) {
	fd, ok := in.prog.funcs[fn]
	if !ok || fd.decl.Body == nil {
		return
	}
	fr := &frame{pkg: fd.pkg, fn: fn, vars: map[types.Object]value{}}
	in.bindUnknownParams(fr, fd)
	in.stack = append(in.stack, fn)
	in.block(fr, fd.decl.Body.List)
	in.stack = in.stack[:len(in.stack)-1]
}

func (in *interp) bindUnknownParams(fr *frame, fd funcDecl) {
	bind := func(fl *dst.FieldList) {
		if fl == nil {
			return
		}
		for _, f := range fl.List {
			for _, nm := range f.Names {
				if o := fd.pkg.objectOf(nm); o != nil {
					fr.vars[o] = in.opaque(o.Type(), "param "+nm.Name)
				}
			}
		}
	}
	bind(fd.decl.Recv)
	bind(fd.decl.Type.Params)
}

// opaque returns a stand-in for a value the interpreter did not see being
// built.
// Structs and struct pointers become unknown objects, so field paths through
// them can still be followed.
func (in *interp) opaque(t types.Type, site string) value {
	if t == nil {
		return vUnknown{}
	}
	if p, ok := t.Underlying().(*types.Pointer); ok {
		if structOf(p.Elem()) != nil {
			return vPtr{in.newObject(p.Elem(), false, site)}
		}
	}
	if structOf(t) != nil {
		return vObj{in.newObject(t, false, site)}
	}
	return vUnknown{}
}

func (in *interp) block(fr *frame, stmts []dst.Stmt) {
	for _, s := range stmts {
		if fr.done {
			return
		}
		in.stmt(fr, s)
	}
}

// ran reports whether the interpreter executed a statement at least once.
func (in *interp) ran(s dst.Stmt) bool { return in.executed[s] }

func (in *interp) stmt(fr *frame, s dst.Stmt) {
	in.executed[s] = true
	in.stmtStack = append(in.stmtStack, s)
	defer func() { in.stmtStack = in.stmtStack[:len(in.stmtStack)-1] }()

	switch s := s.(type) {
	case *dst.ExprStmt:
		in.stmtVals[s] = in.multi(fr, s.X)
	case *dst.AssignStmt:
		in.assign(fr, s)
	case *dst.DeclStmt:
		in.declStmt(fr, s)
	case *dst.IfStmt:
		in.ifStmt(fr, s)
	case *dst.BlockStmt:
		in.block(fr, s.List)
	case *dst.ReturnStmt:
		var vals []value
		if len(s.Results) == 1 {
			vals = in.multi(fr, s.Results[0])
		} else {
			for _, r := range s.Results {
				vals = append(vals, in.eval(fr, r))
			}
		}
		in.stmtVals[s] = vals
		fr.ret = vals
		fr.done = true
	case *dst.RangeStmt:
		in.rangeStmt(fr, s)
	case *dst.ForStmt, *dst.SwitchStmt, *dst.TypeSwitchStmt, *dst.SelectStmt, *dst.GoStmt, *dst.DeferStmt:
		if in.mentionsBuilder(fr.pkg, s) {
			in.escapes = append(in.escapes, escape{pos: fr.pkg.position(s), what: "kingpin registration inside a loop, switch, defer or goroutine, which the tool does not follow", report: true})
		}
		// We don't step into it, but if it parses, the values flag
		// variables start with are the ones they hold now.
		parses := false
		dst.Inspect(s, func(n dst.Node) bool {
			if c, ok := n.(*dst.CallExpr); ok && calleeName(c) == "Parse" && in.isKingpinCall(fr.pkg, c) {
				parses = true
			}
			return !parses
		})
		if parses {
			in.takePriors()
		}
	case *dst.LabeledStmt:
		in.stmt(fr, s.Stmt)
	}
}

// mentionsBuilder reports whether n contains a call that registers grammar.
func (in *interp) mentionsBuilder(p *Package, n dst.Node) bool {
	found := false
	dst.Inspect(n, func(n dst.Node) bool {
		if found {
			return false
		}
		if c, ok := n.(*dst.CallExpr); ok {
			if in.isKingpinCall(p, c) && !in.readsModel(p, c) && !runtimeMethods[calleeName(c)] && calleeName(c) != "MustParse" {
				found = true
			} else if fn := in.staticCallee(p, c); fn != nil && in.relevant[fn] {
				found = true
			}
		}
		return true
	})
	return found
}

func (in *interp) declStmt(fr *frame, s *dst.DeclStmt) {
	gd, ok := s.Decl.(*dst.GenDecl)
	if !ok || gd.Tok != token.VAR {
		return
	}
	for _, sp := range gd.Specs {
		vs := sp.(*dst.ValueSpec)
		var vals []value
		if len(vs.Values) == 1 && len(vs.Names) > 1 {
			vals = in.multi(fr, vs.Values[0])
		} else {
			for _, e := range vs.Values {
				vals = append(vals, in.eval(fr, e))
			}
		}
		in.stmtVals[s] = append(in.stmtVals[s], vals...)
		for i, nm := range vs.Names {
			o := fr.pkg.objectOf(nm)
			if o == nil {
				continue
			}
			if i < len(vals) {
				fr.vars[o] = vals[i]
				in.varVals[o] = vals[i]
				continue
			}
			fr.vars[o] = in.zeroOf(o.Type(), "var "+nm.Name)
			in.varVals[o] = fr.vars[o]
		}
	}
}

// zeroOf is the value of a freshly declared variable: a known zero object for
// structs, the zero constant for basic types.
func (in *interp) zeroOf(t types.Type, site string) value {
	if structOf(t) != nil {
		if _, isPtr := t.Underlying().(*types.Pointer); !isPtr {
			return vObj{in.newObject(t, true, site)}
		}
	}
	return zeroValue(t)
}

func (in *interp) assign(fr *frame, s *dst.AssignStmt) {
	var vals []value
	if len(s.Rhs) == 1 && len(s.Lhs) > 1 {
		vals = in.multi(fr, s.Rhs[0])
	} else {
		for _, e := range s.Rhs {
			vals = append(vals, in.eval(fr, e))
		}
	}
	in.stmtVals[s] = vals
	if s.Tok != token.ASSIGN && s.Tok != token.DEFINE {
		// Compound assignment: the result is not tracked.
		for _, l := range s.Lhs {
			in.store(fr, l, vUnknown{})
		}
		return
	}
	for i, l := range s.Lhs {
		v := value(vUnknown{})
		if i < len(vals) {
			v = vals[i]
		}
		in.store(fr, l, v)
	}
}

func (in *interp) store(fr *frame, lhs dst.Expr, v value) {
	switch l := lhs.(type) {
	case *dst.Ident:
		if l.Name == "_" {
			return
		}
		if o := fr.pkg.objectOf(l); o != nil {
			if vr, ok := o.(*types.Var); ok && vr.Parent() == vr.Pkg().Scope() {
				// A package-level variable keeps the flag value or clause a
				// function gave it, as in verbose = kingpin.Flag(...).Bool()
				// in init, and becomes unknown if it gets a different one.
				// Any other value only matters as the starting value of a
				// flag writing into it.
				in.globalWrites[vr] = v
				switch v.(type) {
				case vStorage, vClause:
					if prev, seen := in.globals[vr]; seen && prev != v {
						in.globals[vr] = vUnknown{}
					} else {
						in.globals[vr] = v
					}
				}
				return
			}
			fr.vars[o] = v
			in.varVals[o] = v
		}
	case *dst.SelectorExpr:
		obj, f := in.fieldLoc(fr, l)
		if obj != nil && f != nil {
			if vo, ok := v.(vObj); ok && vo.o.parent == nil && vo.o != obj && structOf(f.Type()) != nil {
				if _, isPtr := f.Type().Underlying().(*types.Pointer); !isPtr && vo.o.typ != nil {
					vo.o.parent, vo.o.pfield = obj, f
				}
			}
			obj.fields[f] = v
		}
	case *dst.StarExpr:
		// Ignored: only structs are tracked through pointers, and they
		// are shared, not copied.
	case *dst.ParenExpr:
		in.store(fr, l.X, v)
	}
}

// fieldLoc resolves a field selector to the object holding the field and the
// field itself, following embedded fields.
func (in *interp) fieldLoc(fr *frame, sel *dst.SelectorExpr) (*object, *types.Var) {
	s := fr.pkg.selection(sel)
	if s == nil || s.Kind() != types.FieldVal {
		return nil, nil
	}
	base := in.eval(fr, sel.X)
	idx := s.Index()
	cur := base
	t := s.Recv()
	for i, fi := range idx {
		o := in.asObject(cur, t)
		if o == nil {
			return nil, nil
		}
		st := o.st
		if st == nil || fi >= st.NumFields() {
			return nil, nil
		}
		f := st.Field(fi)
		if i == len(idx)-1 {
			return o, f
		}
		cur = in.readField(o, f)
		t = f.Type()
	}
	return nil, nil
}

// asObject returns the object behind a struct value or struct pointer,
// creating an unknown one when needed.
func (in *interp) asObject(v value, t types.Type) *object {
	switch v := v.(type) {
	case vObj:
		return v.o
	case vPtr:
		return v.o
	case vUnknown:
		if t != nil && structOf(deref(t)) != nil {
			return in.newObject(deref(t), false, "unknown")
		}
	}
	return nil
}

func (in *interp) readField(o *object, f *types.Var) value {
	if v, ok := o.fields[f]; ok {
		return v
	}
	ft := f.Type()
	if structOf(ft) != nil {
		if _, isPtr := ft.Underlying().(*types.Pointer); !isPtr {
			sub := in.newObject(ft, o.known, o.site+"."+f.Name())
			sub.parent, sub.pfield = o, f
			o.fields[f] = vObj{sub}
			return vObj{sub}
		}
	}
	if o.known {
		return zeroValue(ft)
	}
	// Remember opaque values so repeated reads see the same object.
	v := in.opaque(ft, o.site+"."+f.Name())
	if p, ok := v.(vPtr); ok {
		p.o.parent, p.o.pfield = nil, nil
	}
	o.fields[f] = v
	return v
}

func (in *interp) ifStmt(fr *frame, s *dst.IfStmt) {
	if s.Init != nil {
		in.stmt(fr, s.Init)
	}
	b, known := in.truth(fr, s.Cond)
	if known {
		if b {
			in.block(fr, s.Body.List)
		} else if s.Else != nil {
			in.stmt(fr, s.Else)
		}
		return
	}
	if !in.mentionsBuilder(fr.pkg, s.Body) && (s.Else == nil || !in.mentionsBuilder(fr.pkg, s.Else)) {
		return
	}
	saved := fr.conds
	fr.conds = append(append([]cond{}, saved...), cond{expr: s.Cond, fn: fr.fn, pkg: fr.pkg})
	in.block(fr, s.Body.List)
	fr.done = false
	if s.Else != nil {
		fr.conds = append(append([]cond{}, saved...), cond{expr: s.Cond, neg: true, fn: fr.fn, pkg: fr.pkg})
		in.stmt(fr, s.Else)
		fr.done = false
	}
	fr.conds = saved
}

func (in *interp) rangeStmt(fr *frame, s *dst.RangeStmt) {
	if !in.mentionsBuilder(fr.pkg, s.Body) {
		return
	}
	sl, ok := in.eval(fr, s.X).(vSlice)
	if !ok {
		in.escapes = append(in.escapes, escape{pos: fr.pkg.position(s), what: "kingpin registration in a loop over values the tool cannot resolve", report: true})
		return
	}
	for i, e := range sl.elems {
		if k, ok := s.Key.(*dst.Ident); ok && k.Name != "_" {
			if o := fr.pkg.objectOf(k); o != nil {
				fr.vars[o] = vConst{constant.MakeInt64(int64(i)), types.Typ[types.Int]}
			}
		}
		if v, ok := s.Value.(*dst.Ident); ok && v.Name != "_" {
			if o := fr.pkg.objectOf(v); o != nil {
				fr.vars[o] = e
			}
		}
		in.block(fr, s.Body.List)
		if fr.done {
			return
		}
	}
}

// truth evaluates a condition when it can be decided from known values.
func (in *interp) truth(fr *frame, e dst.Expr) (bool, bool) {
	v := in.eval(fr, e)
	if c, ok := v.(vConst); ok && c.c.Kind() == constant.Bool {
		return constant.BoolVal(c.c), true
	}
	return false, false
}

// eval evaluates an expression to a single value.
func (in *interp) eval(fr *frame, e dst.Expr) value {
	vals := in.multi(fr, e)
	if len(vals) == 0 {
		return vUnknown{}
	}
	return vals[0]
}

// multi evaluates an expression that may yield several values (a call).
func (in *interp) multi(fr *frame, e dst.Expr) []value {
	p := fr.pkg
	if tv, ok := p.tv(e); ok && tv.Value != nil {
		return []value{vConst{tv.Value, tv.Type}}
	}
	switch e := e.(type) {
	case *dst.ParenExpr:
		return in.multi(fr, e.X)
	case *dst.BasicLit:
		return []value{vUnknown{}}
	case *dst.Ident:
		return []value{in.ident(fr, e)}
	case *dst.SelectorExpr:
		return []value{in.selector(fr, e)}
	case *dst.UnaryExpr:
		return []value{in.unary(fr, e)}
	case *dst.BinaryExpr:
		return []value{in.binary(fr, e)}
	case *dst.StarExpr:
		switch v := in.eval(fr, e.X).(type) {
		case vPtr:
			return []value{vObj{v.o}}
		}
		return []value{vUnknown{}}
	case *dst.CompositeLit:
		return []value{in.composite(fr, e, p.typeOf(e))}
	case *dst.CallExpr:
		return in.call(fr, e)
	case *dst.FuncLit:
		return []value{vUnknown{}}
	case *dst.IndexExpr:
		if sl, ok := in.eval(fr, e.X).(vSlice); ok {
			if c, ok := in.eval(fr, e.Index).(vConst); ok {
				if i, ok := constant.Int64Val(c.c); ok && int(i) < len(sl.elems) && i >= 0 {
					return []value{sl.elems[i]}
				}
			}
		}
		return []value{vUnknown{}}
	}
	return []value{vUnknown{}}
}

func (in *interp) ident(fr *frame, id *dst.Ident) value {
	p := fr.pkg
	if id.Name == "nil" && id.Path == "" {
		if _, ok := p.objectOf(id).(*types.Nil); ok {
			return vNil{}
		}
	}
	o := p.objectOf(id)
	if o == nil {
		return vUnknown{}
	}
	if v, ok := fr.vars[o]; ok {
		return v
	}
	switch o := o.(type) {
	case *types.Func:
		return vFunc{fn: o}
	case *types.Var:
		if v, ok := in.globals[o]; ok {
			return v
		}
		if o.Pkg() != nil && in.prog.isKingpinPath(o.Pkg().Path()) {
			switch o.Name() {
			case "CommandLine":
				return vClause{in.commandLine(fr, p.position(id))}
			case "HelpFlag":
				return vClause{in.helpClause(in.commandLine(fr, p.position(id)))}
			case "VersionFlag":
				return vClause{in.versionClause(in.commandLine(fr, p.position(id)))}
			}
		}
		return in.packageVar(o)
	case *types.Nil:
		return vNil{}
	}
	return vUnknown{}
}

// packageVar returns the initial value of a package-level variable that is
// never written to, when it is a constant or a slice.
func (in *interp) packageVar(v *types.Var) value {
	if v.Pkg() == nil || v.Parent() != v.Pkg().Scope() || in.prog.written[v] {
		return vUnknown{}
	}
	if in.structVars[v] {
		return in.globals[v]
	}
	init, ok := in.prog.varInit[v]
	if !ok {
		return vUnknown{}
	}
	fr := &frame{pkg: init.pkg, vars: map[types.Object]value{}}
	switch val := in.eval(fr, init.expr).(type) {
	case vConst, vSlice:
		return val
	case vPtr, vObj:
		// Every reader shares the one struct.
		if builtInPlace(init.pkg, init.expr) {
			in.globals[v], in.structVars[v] = val, true
			return val
		}
	}
	return vUnknown{}
}

// builtInPlace reports whether e builds a struct with a literal or new.
func builtInPlace(p *Package, e dst.Expr) bool {
	e = unparen(e)
	if u, ok := e.(*dst.UnaryExpr); ok && u.Op == token.AND {
		e = unparen(u.X)
	}
	switch e := e.(type) {
	case *dst.CompositeLit:
		return true
	case *dst.CallExpr:
		id, ok := e.Fun.(*dst.Ident)
		return ok && id.Name == "new" && len(e.Args) == 1 && types.Universe.Lookup("new") == p.objectOf(id)
	}
	return false
}

func (in *interp) selector(fr *frame, sel *dst.SelectorExpr) value {
	p := fr.pkg
	s := p.selection(sel)
	if s == nil {
		// A qualified identifier the decorator left as a selector.
		if o := p.objectOf(sel); o != nil {
			switch o := o.(type) {
			case *types.Func:
				return vFunc{fn: o}
			case *types.Var:
				return in.packageVar(o)
			}
		}
		return vUnknown{}
	}
	switch s.Kind() {
	case types.FieldVal:
		if app, ok := in.eval(fr, sel.X).(vClause); ok && app.c.kind == kindApp {
			switch sel.Sel.Name {
			case "HelpFlag":
				return vClause{in.helpClause(app.c)}
			case "VersionFlag":
				return vClause{in.versionClause(app.c)}
			}
		}
		obj, f := in.fieldLoc(fr, sel)
		if obj == nil {
			return vUnknown{}
		}
		return in.readField(obj, f)
	case types.MethodVal:
		fn, _ := s.Obj().(*types.Func)
		recv := in.methodRecv(fr, sel, s)
		return vFunc{fn: fn, recv: recv}
	}
	return vUnknown{}
}

// methodRecv evaluates the receiver of a method selection, walking the
// embedded fields the method was promoted through.
func (in *interp) methodRecv(fr *frame, sel *dst.SelectorExpr, s *types.Selection) value {
	cur := in.eval(fr, sel.X)
	idx := s.Index()
	t := s.Recv()
	for _, fi := range idx[:len(idx)-1] {
		if _, ok := cur.(vClause); ok {
			// Methods promoted from kingpin's own embedded types still act
			// on the clause.
			return cur
		}
		o := in.asObject(cur, t)
		if o == nil || o.st == nil || fi >= o.st.NumFields() {
			return vUnknown{}
		}
		f := o.st.Field(fi)
		cur = in.readField(o, f)
		t = f.Type()
	}
	return cur
}

func (in *interp) unary(fr *frame, u *dst.UnaryExpr) value {
	switch u.Op {
	case token.AND:
		switch x := u.X.(type) {
		case *dst.CompositeLit:
			if o, ok := in.composite(fr, x, fr.pkg.typeOf(x)).(vObj); ok {
				return vPtr{o.o}
			}
			return vUnknown{}
		case *dst.ParenExpr:
			return in.unary(fr, &dst.UnaryExpr{Op: token.AND, X: x.X})
		case *dst.SelectorExpr:
			if s := fr.pkg.selection(x); s != nil && s.Kind() == types.FieldVal {
				obj, f := in.fieldLoc(fr, x)
				if obj == nil {
					return vUnknown{}
				}
				if structOf(f.Type()) != nil {
					if _, isPtr := f.Type().Underlying().(*types.Pointer); !isPtr {
						if o, ok := in.readField(obj, f).(vObj); ok {
							return vPtr{o.o}
						}
					}
				}
				return vFieldPtr{obj, f}
			}
			if v, ok := fr.pkg.objectOf(x).(*types.Var); ok {
				return vVarPtr{v}
			}
		case *dst.Ident:
			o := fr.pkg.objectOf(x)
			if cur, ok := fr.vars[o]; ok {
				if ob, ok := cur.(vObj); ok {
					return vPtr{ob.o}
				}
			}
			if v, ok := o.(*types.Var); ok {
				return vVarPtr{v}
			}
		}
		return vUnknown{}
	case token.NOT:
		if c, ok := in.eval(fr, u.X).(vConst); ok && c.c.Kind() == constant.Bool {
			return vConst{constant.MakeBool(!constant.BoolVal(c.c)), c.t}
		}
	case token.SUB, token.ADD, token.XOR:
		if c, ok := in.eval(fr, u.X).(vConst); ok {
			return vConst{constant.UnaryOp(u.Op, c.c, 0), c.t}
		}
	}
	return vUnknown{}
}

func (in *interp) binary(fr *frame, b *dst.BinaryExpr) value {
	switch b.Op {
	case token.LAND, token.LOR:
		l, lok := in.truth(fr, b.X)
		if lok && (b.Op == token.LAND && !l || b.Op == token.LOR && l) {
			return vConst{constant.MakeBool(l), types.Typ[types.Bool]}
		}
		r, rok := in.truth(fr, b.Y)
		if lok && rok {
			return vConst{constant.MakeBool(r), types.Typ[types.Bool]}
		}
		return vUnknown{}
	}
	x, y := in.eval(fr, b.X), in.eval(fr, b.Y)
	if b.Op == token.EQL || b.Op == token.NEQ {
		_, xNil := x.(vNil)
		_, yNil := y.(vNil)
		if xNil || yNil {
			other := x
			if xNil {
				other = y
			}
			nonNil, known := isNonNil(other)
			if !known {
				return vUnknown{}
			}
			eq := !nonNil
			if b.Op == token.NEQ {
				eq = !eq
			}
			return vConst{constant.MakeBool(eq), types.Typ[types.Bool]}
		}
	}
	xc, ok1 := x.(vConst)
	yc, ok2 := y.(vConst)
	if !ok1 || !ok2 {
		return vUnknown{}
	}
	switch b.Op {
	case token.EQL, token.NEQ, token.LSS, token.LEQ, token.GTR, token.GEQ:
		if xc.c.Kind() == yc.c.Kind() || isNumeric(xc.c) && isNumeric(yc.c) {
			return vConst{constant.MakeBool(constant.Compare(xc.c, b.Op, yc.c)), types.Typ[types.Bool]}
		}
		return vUnknown{}
	case token.SHL, token.SHR:
		if s, ok := constant.Uint64Val(yc.c); ok {
			return vConst{constant.Shift(xc.c, b.Op, uint(s)), xc.t}
		}
		return vUnknown{}
	}
	if xc.c.Kind() == constant.Unknown || yc.c.Kind() == constant.Unknown {
		return vUnknown{}
	}
	t := xc.t
	if t == nil || isUntyped(t) {
		t = yc.t
	}
	op := b.Op
	if op == token.QUO && isInteger(t) {
		// go/constant spells integer division QUO_ASSIGN.
		op = token.QUO_ASSIGN
	}
	defer func() { _ = recover() }()
	return fitType(constant.BinaryOp(xc.c, op, yc.c), t)
}

// convertConst converts a constant to the numeric, string or boolean type t,
// truncating a float converted to an integer.
// Non-constants and non-basic types are returned as they are, and any other
// conversion gives vUnknown.
func convertConst(v value, t types.Type) value {
	c, ok := v.(vConst)
	if !ok || c.c == nil {
		return v
	}
	basic, ok := t.Underlying().(*types.Basic)
	if !ok {
		return v
	}
	info := basic.Info()
	switch {
	case info&types.IsInteger != 0 && isNumeric(c.c):
		n := constant.ToInt(c.c)
		if n.Kind() != constant.Int {
			f, _ := constant.Float64Val(c.c)
			n = constant.MakeInt64(int64(f))
		}
		return fitType(n, t)
	case info&types.IsFloat != 0 && isNumeric(c.c):
		return fitType(constant.ToFloat(c.c), t)
	case info&types.IsString != 0 && c.c.Kind() == constant.String,
		info&types.IsBoolean != 0 && c.c.Kind() == constant.Bool:
		return vConst{c.c, t}
	}
	return vUnknown{}
}

// fitType returns v the way a variable of type t would hold it.
// A float32 is rounded, and a sized integer wraps around, as Go does.
func fitType(v constant.Value, t types.Type) value {
	b, ok := typeUnder(t).(*types.Basic)
	if !ok || b.Info()&types.IsUntyped != 0 {
		return vConst{v, t}
	}
	switch {
	case b.Kind() == types.Float32:
		f, _ := constant.Float64Val(constant.ToFloat(v))
		return vConst{constant.MakeFloat64(float64(float32(f))), t}
	case b.Info()&types.IsInteger != 0:
		bits := map[types.BasicKind]uint{types.Int8: 8, types.Int16: 16, types.Int32: 32, types.Uint8: 8, types.Uint16: 16, types.Uint32: 32}[b.Kind()]
		if bits == 0 {
			bits = 64
		}
		v = constant.ToInt(v)
		if v.Kind() != constant.Int {
			return vUnknown{}
		}
		mod := constant.Shift(constant.MakeInt64(1), token.SHL, bits)
		v = constant.BinaryOp(v, token.REM, mod)
		if constant.Sign(v) < 0 {
			v = constant.BinaryOp(v, token.ADD, mod)
		}
		half := constant.Shift(constant.MakeInt64(1), token.SHL, bits-1)
		if b.Info()&types.IsUnsigned == 0 && !constant.Compare(v, token.LSS, half) {
			v = constant.BinaryOp(v, token.SUB, mod)
		}
	}
	return vConst{v, t}
}

func isUntyped(t types.Type) bool {
	b, ok := t.(*types.Basic)
	return ok && b.Info()&types.IsUntyped != 0
}

func isInteger(t types.Type) bool {
	b, ok := typeUnder(t).(*types.Basic)
	return ok && b.Info()&types.IsInteger != 0
}

func isNumeric(c constant.Value) bool {
	k := c.Kind()
	return k == constant.Int || k == constant.Float
}

func (in *interp) composite(fr *frame, cl *dst.CompositeLit, t types.Type) value {
	if t == nil {
		return vUnknown{}
	}
	switch u := t.Underlying().(type) {
	case *types.Struct:
		o := in.newObject(t, true, fmt.Sprintf("%s literal", types.TypeString(t, nil)))
		for i, el := range cl.Elts {
			var f *types.Var
			var ve dst.Expr
			if kv, ok := el.(*dst.KeyValueExpr); ok {
				key, _ := kv.Key.(*dst.Ident)
				if key == nil {
					continue
				}
				for j := 0; j < u.NumFields(); j++ {
					if u.Field(j).Name() == key.Name {
						f = u.Field(j)
					}
				}
				ve = kv.Value
			} else if i < u.NumFields() {
				f = u.Field(i)
				ve = el
			}
			if f == nil {
				continue
			}
			v := in.evalTyped(fr, ve, f.Type())
			if kc, ok := v.(vClause); ok && !in.prog.ownsType(t) {
				in.escapes = append(in.escapes, escape{pos: fr.pkg.position(cl), what: fmt.Sprintf("the kingpin %s is stored in a %s, which belongs to code the tool does not follow: it may register more grammar or parse there", kc.c.kind, types.TypeString(t, nil)), report: true, fatal: kc.c.kind == kindApp})
			}
			if vo, ok := v.(vObj); ok && vo.o.parent == nil && structOf(f.Type()) != nil {
				if _, isPtr := f.Type().Underlying().(*types.Pointer); !isPtr {
					vo.o.parent, vo.o.pfield = o, f
				}
			}
			o.fields[f] = v
		}
		return vObj{o}
	case *types.Slice, *types.Array:
		var elemT types.Type
		if s, ok := u.(*types.Slice); ok {
			elemT = s.Elem()
		} else {
			elemT = u.(*types.Array).Elem()
		}
		var elems []value
		for _, el := range cl.Elts {
			if _, ok := el.(*dst.KeyValueExpr); ok {
				return vUnknown{}
			}
			v := in.evalTyped(fr, el, elemT)
			if prev, seen := in.eltVals[el]; !seen {
				in.eltVals[el] = v
			} else if a, ok := prev.(vPtr); !ok || v != value(a) {
				in.eltVals[el] = vUnknown{}
			}
			elems = append(elems, v)
		}
		if elems == nil {
			elems = []value{}
		}
		return vSlice{elems}
	case *types.Pointer:
		if v, ok := in.composite(fr, cl, u.Elem()).(vObj); ok {
			return vPtr{v.o}
		}
	}
	return vUnknown{}
}

// evalTyped evaluates an element of a composite literal, which may itself be
// an elided composite literal.
func (in *interp) evalTyped(fr *frame, e dst.Expr, t types.Type) value {
	if cl, ok := e.(*dst.CompositeLit); ok && cl.Type == nil {
		return in.composite(fr, cl, t)
	}
	if u, ok := e.(*dst.UnaryExpr); ok && u.Op == token.AND {
		if cl, ok := u.X.(*dst.CompositeLit); ok && cl.Type == nil {
			if v, ok := in.composite(fr, cl, deref(t)).(vObj); ok {
				return vPtr{v.o}
			}
		}
	}
	return in.eval(fr, e)
}

func (in *interp) call(fr *frame, c *dst.CallExpr) []value {
	p := fr.pkg

	// Type conversions keep the value, as the new type reads it.
	if tv, ok := p.tv(c.Fun); ok && tv.IsType() {
		if len(c.Args) == 1 {
			return []value{convertConst(in.eval(fr, c.Args[0]), tv.Type)}
		}
		return []value{vUnknown{}}
	}
	if id, ok := c.Fun.(*dst.Ident); ok && id.Path == "" {
		if b, ok := p.objectOf(id).(*types.Builtin); ok {
			return []value{in.builtin(fr, b.Name(), c)}
		}
	}

	var (
		fn         *types.Func
		recv       value
		fv         value
		methodExpr bool
	)
	switch fun := c.Fun.(type) {
	case *dst.Ident:
		fv = in.ident(fr, fun)
	case *dst.SelectorExpr:
		// Builder.Setup(b, app) calls Setup on b.
		if s := p.selection(fun); s != nil && s.Kind() == types.MethodExpr {
			fn, _ = s.Obj().(*types.Func)
			methodExpr = fn != nil
		} else {
			fv = in.selector(fr, fun)
		}
	default:
		fv = in.eval(fr, c.Fun)
	}
	if f, ok := fv.(vFunc); ok {
		fn, recv = f.fn, f.recv
		if in.staticCallee(p, c) == nil {
			if prev, seen := in.valueCallees[c]; seen && prev != fn {
				in.valueCallees[c] = nil
			} else {
				in.valueCallees[c] = fn
			}
		}
	}

	args := make([]value, len(c.Args))
	for i, a := range c.Args {
		args[i] = in.eval(fr, a)
	}
	if methodExpr && len(args) > 0 {
		recv, args = args[0], args[1:]
	}

	if rc, ok := recv.(vClause); ok && fn != nil {
		return in.kingpinMethod(fr, c, rc.c, fn.Name(), args)
	}
	if fn != nil && fn.Pkg() != nil && in.prog.isKingpinPath(fn.Pkg().Path()) {
		if sig, ok := fn.Type().(*types.Signature); ok && sig.Recv() == nil {
			return in.kingpinFunc(fr, c, fn.Name(), args)
		}
		// A method on a kingpin value the interpreter does not track.
		return []value{in.opaque(resultType(fn, 0), "kingpin")}
	}
	if fn == nil {
		in.noteEscape(fr, c, args)
		return []value{vUnknown{}}
	}
	if v, ok := foldCall(fn, args, c.Ellipsis); ok {
		return []value{v}
	}
	if v, ok := foldMethod(fn, recv); ok {
		return []value{v}
	}
	fd, declared := in.prog.funcs[fn]
	if declared && fd.decl.Body != nil && (in.relevant[fn] || simpleBody(fd.decl)) {
		return in.inline(fr, c, fd, fn, recv, args, c.Ellipsis)
	}
	in.noteEscape(fr, c, args)
	n := 1
	if sig, ok := fn.Type().(*types.Signature); ok && sig.Results().Len() > 1 {
		n = sig.Results().Len()
	}
	out := make([]value, n)
	for i := range out {
		out[i] = in.opaque(resultType(fn, i), "result of "+fn.Name())
	}
	return out
}

func resultType(fn *types.Func, i int) types.Type {
	sig, ok := fn.Type().(*types.Signature)
	if !ok || sig.Results().Len() <= i {
		return nil
	}
	return sig.Results().At(i).Type()
}

func (in *interp) noteEscape(fr *frame, c *dst.CallExpr, args []value) {
	for _, a := range args {
		if cl, ok := a.(vClause); ok {
			fatal := false
			if fn := in.staticCallee(fr.pkg, c); cl.c.kind == kindApp && (fn == nil || !in.prog.owns(fn.Pkg())) {
				fatal = true
			}
			in.escapes = append(in.escapes, escape{pos: fr.pkg.position(c), what: fmt.Sprintf("the kingpin %s is passed to %s, which the tool does not follow: it may register more grammar or parse there", cl.c.kind, render(c.Fun)), report: true, fatal: fatal})
		}
	}
}

// simpleBody reports whether a function body is a single return statement.
// Such a function is cheap and safe to inline when its result feeds the
// registration, as with a method returning an options struct.
func simpleBody(fd *dst.FuncDecl) bool {
	if fd.Body == nil || len(fd.Body.List) != 1 {
		return false
	}
	_, ok := fd.Body.List[0].(*dst.ReturnStmt)
	return ok
}

func (in *interp) inline(fr *frame, c *dst.CallExpr, fd funcDecl, fn *types.Func, recv value, args []value, packed bool) []value {
	if in.inInit {
		in.initCalled[fn] = true
	}
	count := 0
	for _, s := range in.stack {
		if s == fn {
			count++
		}
	}
	if count > 1 || len(in.stack) > 40 {
		return []value{vUnknown{}}
	}
	nf := &frame{pkg: fd.pkg, fn: fn, vars: map[types.Object]value{}, conds: fr.conds}
	if fd.decl.Recv != nil && len(fd.decl.Recv.List) == 1 && len(fd.decl.Recv.List[0].Names) == 1 {
		if o := fd.pkg.objectOf(fd.decl.Recv.List[0].Names[0]); o != nil {
			nf.vars[o] = adaptRecv(recv, o.Type())
		}
	}
	sig, _ := fn.Type().(*types.Signature)
	var params []types.Object
	if fd.decl.Type.Params != nil {
		for _, f := range fd.decl.Type.Params.List {
			if len(f.Names) == 0 {
				params = append(params, nil)
			}
			for _, nm := range f.Names {
				params = append(params, fd.pkg.objectOf(nm))
			}
		}
	}
	for i, po := range params {
		var v value = vUnknown{}
		switch {
		case sig != nil && sig.Variadic() && i == len(params)-1 && !packed:
			var rest []value
			if i < len(args) {
				rest = append(rest, args[i:]...)
			}
			v = vSlice{rest}
		case i < len(args):
			v = args[i]
		}
		if po != nil {
			nf.vars[po] = v
		}
	}
	if fd.decl.Type.Results != nil {
		for _, f := range fd.decl.Type.Results.List {
			for _, nm := range f.Names {
				if o := fd.pkg.objectOf(nm); o != nil {
					nf.vars[o] = in.zeroOf(o.Type(), "result "+nm.Name)
				}
			}
		}
	}

	mark := len(in.clauses)
	in.stack = append(in.stack, fn)
	saved := in.stmtStack
	in.block(nf, fd.decl.Body.List)
	in.stmtStack = saved
	in.stack = in.stack[:len(in.stack)-1]

	out := nf.ret
	if out == nil && fd.decl.Type.Results != nil {
		for _, f := range fd.decl.Type.Results.List {
			for _, nm := range f.Names {
				out = append(out, nf.vars[fd.pkg.objectOf(nm)])
			}
		}
	}
	if len(out) == 0 {
		out = []value{vUnknown{}}
	}
	if len(in.clauses) > mark {
		if pv, ok := out[0].(vPtr); ok {
			in.sites = append(in.sites, &callSite{
				pkg:     fr.pkg,
				call:    c,
				fn:      fr.fn,
				callee:  fn,
				result:  pv.o,
				clauses: append([]*clause{}, in.clauses[mark:]...),
				depth:   len(in.stack),
			})
		}
	}
	return out
}

// adaptRecv converts a receiver between value and pointer form.
// Objects are shared, so only the wrapper changes.
func adaptRecv(v value, t types.Type) value {
	_, wantPtr := t.Underlying().(*types.Pointer)
	switch r := v.(type) {
	case vObj:
		if wantPtr {
			return vPtr(r)
		}
	case vPtr:
		if !wantPtr {
			return vObj(r)
		}
	}
	return v
}

func (in *interp) builtin(fr *frame, name string, c *dst.CallExpr) value {
	switch name {
	case "new":
		if len(c.Args) == 1 {
			if tv, ok := fr.pkg.tv(c.Args[0]); ok && tv.Type != nil {
				if structOf(tv.Type) != nil {
					return vPtr{in.newObject(tv.Type, true, "new")}
				}
				in.nextID++
				return vCell{in.nextID}
			}
		}
	case "append":
		if len(c.Args) == 0 {
			return vUnknown{}
		}
		base, ok := in.eval(fr, c.Args[0]).(vSlice)
		if !ok {
			if _, isNil := in.eval(fr, c.Args[0]).(vNil); !isNil {
				return vUnknown{}
			}
		}
		elems := append([]value{}, base.elems...)
		for i, a := range c.Args[1:] {
			v := in.eval(fr, a)
			if c.Ellipsis && i == len(c.Args)-2 {
				s, ok := v.(vSlice)
				if !ok {
					return vUnknown{}
				}
				elems = append(elems, s.elems...)
				continue
			}
			elems = append(elems, v)
		}
		return vSlice{elems}
	}
	for _, a := range c.Args {
		in.eval(fr, a)
	}
	return vUnknown{}
}

// foldCall evaluates calls to a few pure standard library functions when all
// their arguments are known.
// That way, help strings built with fmt.Sprintf from constants become
// constants.
func foldCall(fn *types.Func, args []value, packed bool) (value, bool) {
	if fn.Pkg() == nil {
		return nil, false
	}
	str := types.Typ[types.String]
	switch fn.Pkg().Path() + "." + fn.Name() {
	case "fmt.Sprintf", "fmt.Sprint", "fmt.Sprintln":
		var goArgs []any
		var format string
		rest := args
		if fn.Name() == "Sprintf" {
			if len(args) == 0 {
				return nil, false
			}
			f, ok := constString(args[0])
			if !ok {
				return nil, false
			}
			format, rest = f, args[1:]
		}
		if len(rest) == 1 {
			if s, ok := rest[0].(vSlice); ok {
				rest = s.elems
			}
		}
		for _, a := range rest {
			g, ok := goValue(a)
			if !ok {
				return nil, false
			}
			goArgs = append(goArgs, g)
		}
		var out string
		switch fn.Name() {
		case "Sprintf":
			out = fmt.Sprintf(format, goArgs...)
		case "Sprint":
			out = fmt.Sprint(goArgs...)
		default:
			out = fmt.Sprintln(goArgs...)
		}
		return vConst{constant.MakeString(out), str}, true
	case "strconv.Itoa":
		if len(args) == 1 {
			if c, ok := args[0].(vConst); ok {
				if i, ok := constant.Int64Val(c.c); ok {
					return vConst{constant.MakeString(strconv.FormatInt(i, 10)), str}, true
				}
			}
		}
	case "strings.Join":
		if len(args) == 2 {
			sl, ok := args[0].(vSlice)
			sep, ok2 := constString(args[1])
			if ok && ok2 {
				parts := make([]string, 0, len(sl.elems))
				for _, e := range sl.elems {
					s, ok := constString(e)
					if !ok {
						return nil, false
					}
					parts = append(parts, s)
				}
				return vConst{constant.MakeString(strings.Join(parts, sep)), str}, true
			}
		}
	case "strings.ToLower", "strings.ToUpper", "strings.TrimSpace":
		if len(args) == 1 {
			if s, ok := constString(args[0]); ok {
				switch fn.Name() {
				case "ToLower":
					s = strings.ToLower(s)
				case "ToUpper":
					s = strings.ToUpper(s)
				default:
					s = strings.TrimSpace(s)
				}
				return vConst{constant.MakeString(s), str}, true
			}
		}
	}
	return nil, false
}

// foldMethod evaluates String on a constant of a few standard types, such as
// a time.Duration default rendered with defaultTTL.String().
func foldMethod(fn *types.Func, recv value) (value, bool) {
	c, ok := recv.(vConst)
	if !ok || fn.Pkg() == nil || fn.Name() != "String" {
		return nil, false
	}
	named, ok := c.t.(*types.Named)
	if !ok || named.Obj().Pkg() == nil {
		return nil, false
	}
	str := types.Typ[types.String]
	switch named.Obj().Pkg().Path() + "." + named.Obj().Name() {
	case "time.Duration":
		if i, ok := constant.Int64Val(c.c); ok {
			return vConst{constant.MakeString(time.Duration(i).String()), str}, true
		}
	}
	return nil, false
}

func constString(v value) (string, bool) {
	c, ok := v.(vConst)
	if !ok || c.c.Kind() != constant.String {
		return "", false
	}
	return constant.StringVal(c.c), true
}

// goValue converts a constant to the Go value fmt would see at run time.
func goValue(v value) (any, bool) {
	c, ok := v.(vConst)
	if !ok {
		return nil, false
	}
	switch c.c.Kind() {
	case constant.String:
		return constant.StringVal(c.c), true
	case constant.Bool:
		return constant.BoolVal(c.c), true
	case constant.Int:
		i, ok := constant.Int64Val(c.c)
		if !ok {
			return nil, false
		}
		if b, isBasic := c.t.(*types.Basic); isBasic {
			switch b.Kind() {
			case types.Int32, types.UntypedRune:
				return int32(i), true
			case types.Uint8:
				return uint8(i), true
			}
		}
		return int(i), true
	case constant.Float:
		f, _ := constant.Float64Val(c.c)
		return f, true
	}
	return nil, false
}

// calleeName returns the name of the called function or method, as written.
func calleeName(c *dst.CallExpr) string {
	switch fun := c.Fun.(type) {
	case *dst.Ident:
		return fun.Name
	case *dst.SelectorExpr:
		return fun.Sel.Name
	}
	return ""
}

// priorDefault returns the non-zero constant a flag's destination already
// holds before parsing, written the way a kong default tag expects it.
func (in *interp) priorDefault(dst value) (string, bool) {
	var v value
	var t types.Type
	switch d := dst.(type) {
	case vVarPtr:
		t = d.v.Type()
		if val, ok := in.varVals[d.v]; ok {
			v = val
		} else if val, ok := in.globalWrites[d.v]; ok {
			v = val
		} else if val, ok := in.globals[d.v]; ok {
			v = val
		} else {
			v = in.packageVar(d.v)
		}
	case vFieldPtr:
		t = d.f.Type()
		v = d.o.fields[d.f]
	}
	c, ok := v.(vConst)
	if !ok || c.c == nil || t == nil {
		return "", false
	}
	// A time.Duration is written the way kong parses it, as in 1m30s.
	if named, ok := types.Unalias(t).(*types.Named); ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == "time" && named.Obj().Name() == "Duration" {
		n, exact := constant.Int64Val(constant.ToInt(c.c))
		if !exact || n == 0 {
			return "", false
		}
		return time.Duration(n).String(), true
	}
	basic, ok := t.Underlying().(*types.Basic)
	if !ok {
		return "", false
	}
	info := basic.Info()
	switch {
	case info&types.IsString != 0 && c.c.Kind() == constant.String:
		s := constant.StringVal(c.c)
		return s, s != ""
	case info&types.IsBoolean != 0 && c.c.Kind() == constant.Bool:
		return "true", constant.BoolVal(c.c)
	case info&types.IsInteger != 0:
		n := constant.ToInt(c.c)
		if n.Kind() != constant.Int || constant.Sign(n) == 0 {
			return "", false
		}
		return n.ExactString(), true
	case info&types.IsFloat != 0:
		f, _ := constant.Float64Val(constant.ToFloat(c.c))
		if f == 0 || math.IsInf(f, 0) {
			return "", false
		}
		bits := 64
		if basic.Kind() == types.Float32 {
			bits = 32
		}
		return strconv.FormatFloat(f, 'g', -1, bits), true
	}
	return "", false
}
