package conv

import (
	"cmp"
	"fmt"
	"go/token"
	"go/types"
	"slices"
	"strconv"
	"strings"

	"github.com/dave/dst"
	"github.com/dave/dst/dstutil"
)

// mainProgram describes a program that parses its own command line in the
// package holding the grammar, usually main.
// Such a program is converted end to end: the application becomes a kong
// parser over the CLI struct, variables that held values or commands point
// into the CLI, and parse calls go through kongcompat.
type mainProgram struct {
	// local is the function declaring cli, nil when cli is package-level.
	local *dst.FuncDecl
	// repl maps variables to the CLI field replacing them, and tells whether
	// the variable was a pointer to that field.
	repl map[types.Object]replacement
	// usage and usageContext are what the program gave UsageTemplate and
	// UsageContext, or nil.
	usage        dst.Expr
	usageContext dst.Expr
	// vars are the run-time values the parser gets through kongcompat.Vars.
	vars []runtimeVar
	// version is what the program gave Version, or nil.
	version dst.Expr
	// name, help and author are what the program assigned to the
	// application's Name and Help fields or gave to Author, or nil.
	name, help, author dst.Expr
	// exit, stdout and stderr are what the program gave Terminate and the
	// writers of kingpin's package-level application, or nil.
	exit, stdout, stderr dst.Expr
	// extraFields are the CLI fields of flags that wrote into plain
	// variables.
	extraFields []string
	// parsers names the variable declared for each kingpin.Parse() call on
	// the package-level application.
	parsers map[*dst.CallExpr]string
	// flags is the program's variable holding the application flags, which
	// the CLI embeds a pointer to.
	flags *flagsWiring
	// packageSettings are the Terminate and writer calls on the
	// package-level application that became kong options.
	packageSettings map[*dst.CallExpr]bool
	// bound are the variables bound for hooks that now do what
	// constructors did.
	bound []boundVar
	// structSpecs declare the package-level command structs the CLI
	// replaces, and cliSpec is the one whose place a package-level cli takes
	// when it starts with their values.
	structSpecs map[*dst.ValueSpec][]int
	// instances are the variables holding a command struct, and pointers
	// those of them still read once the wiring is gone: they keep pointing
	// to the struct, now in the CLI, instead of being replaced.
	instances map[types.Object]*object
	pointers  map[types.Object]string
	cliSpec   *dst.ValueSpec
	starts    []structStart
}

// structStart is the value a CLI field starts with, from a package-level
// variable such as ls = &lsCmd{out: os.Stdout}.
type structStart struct {
	path []string // the fields leading to it from the CLI
	lit  *dst.CompositeLit
}

type flagsWiring struct {
	field string     // the embedded field of the CLI
	v     *types.Var // the program's variable
	ptr   bool       // whether the variable is already a pointer
}

type replacement struct {
	path    string
	pointer bool
}

// detectMain reports whether the package holding the grammar also parses the
// command line, which makes it a main-style program.
func (c *converter) detectMain() bool {
	root := c.tree.RootPkg
	found := false
	for _, f := range root.Syntax {
		dst.Inspect(f, func(n dst.Node) bool {
			call, ok := n.(*dst.CallExpr)
			if !ok || found {
				return !found
			}
			switch c.runtimeCall(root, call) {
			case "Parse", "MustParse", "ParseContext":
				found = true
			}
			return true
		})
	}
	return found
}

// runtimeCall returns the name of the kingpin function or application method
// that call invokes, or "".
func (c *converter) runtimeCall(p *Package, call *dst.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *dst.Ident:
		if fun.Path != "" && c.prog.isKingpinPath(fun.Path) {
			return fun.Name
		}
	case *dst.SelectorExpr:
		if c.isAppType(p.typeOf(fun.X)) {
			return fun.Sel.Name
		}
	}
	return ""
}

// boundVar is a variable bound for the hooks, as the type they take.
type boundVar struct {
	v  *types.Var
	as types.Type
}

// storageFields returns, as Go source lines, the fields holding the values of
// a command's flags and arguments declared with String() and friends.
// It records each field's path so the variables that pointed at the values
// can be replaced.
func (c *converter) storageFields(n *node, pkg *types.Package, imports map[string]string, path string, taken map[string]bool) []string {
	var lines []string
	for _, b := range c.plan.bindings {
		if !b.storage || b.cl.parent != n.cl || c.pkgFlagsOf[b.cl] {
			continue
		}
		name := c.prog.identFromName(b.cl.name)
		for taken[name] {
			name += "Value"
		}
		taken[name] = true
		t := b.cl.storageType
		if t == nil {
			c.issue(b.cl.pos, "%s %q: cannot tell the type of its value", b.cl.kind, b.cl.name)
			continue
		}
		tag := c.plan.tagFor(b, name)
		line := fmt.Sprintf("%s %s %s", name, typeExpr(t, pkg, imports), tagLiteral(tag.String()))
		lines = append(lines, line)
		c.fieldOrder[line] = 2 * b.cl.id
		if n.cl.kind == kindApp {
			c.rootFieldNames[b.cl] = name
		}
		c.storagePaths[b.cl] = path + "." + name
	}
	return lines
}

// prepareMain decides which variables of a main-style program become CLI
// fields: those holding a value, a command, a command struct or a flag
// destination.
// taken holds the CLI field names already in use.
func (c *converter) prepareMain(taken map[string]bool) {
	mp := c.main
	// (a) = x is just a = x, and simpler to match that way.
	for _, f := range c.tree.RootPkg.Syntax {
		dst.Inspect(f, func(n dst.Node) bool {
			if as, ok := n.(*dst.AssignStmt); ok {
				for i, l := range as.Lhs {
					as.Lhs[i] = unparen(l)
				}
			}
			return true
		})
	}
	c.scanAppSettings()
	c.scanPackageSettings()
	in := c.g.in
	values := map[types.Object]value{}
	for v, val := range in.globals {
		values[v] = val
	}
	for v, val := range in.varVals {
		values[v] = val
	}
	for obj, val := range values {
		switch val := val.(type) {
		case vStorage:
			if p, ok := c.storagePaths[val.c]; ok {
				mp.repl[obj] = replacement{p, true}
			}
		case vPtr:
			if p, ok := c.instancePath(val.o); ok {
				mp.repl[obj] = replacement{p, true}
				mp.instances[obj] = val.o
			}
		case vObj:
			// A copy of a command struct, as in snapshot := cmd, stays a
			// copy.
			// Only the variable kingpin wrote into is replaced.
			if p, ok := c.instancePath(val.o); ok && !c.copiesVariable(obj) {
				mp.repl[obj] = replacement{p, false}
			}
		}
	}
	// Flags written into plain variables become fields of the CLI.
	writers := map[*types.Var]*binding{}
	for _, b := range c.plan.bindings {
		if b.dstVar == nil || b.inst != nil {
			continue
		}
		if prev := writers[b.dstVar]; prev != nil {
			c.issue(b.cl.pos, "flags %q and %q write the same variable %s; kong gives each its own field, so merge them by hand (an aliases tag can keep the second name)", prev.cl.name, b.cl.name, b.dstVar.Name())
			continue
		}
		writers[b.dstVar] = b
		if b.dstVar.Pkg() != c.tree.RootPkg.Types {
			c.issue(b.cl.pos, "flag %q writes into a variable of package %s, which cannot see the CLI; make it a field of a struct of that package the CLI can embed", b.cl.name, b.dstVar.Pkg().Name())
			continue
		}
		name := c.prog.identFromName(b.cl.name)
		for taken[name] {
			name += "Value"
		}
		taken[name] = true
		tag := c.plan.tagFor(b, name)
		line := fmt.Sprintf("%s %s %s", name, typeExpr(b.dstVar.Type(), c.tree.RootPkg.Types, c.rootImports), tagLiteral(tag.String()))
		mp.extraFields = append(mp.extraFields, line)
		c.fieldOrder[line] = 2 * b.cl.id
		c.rootFieldNames[b.cl] = name
		mp.repl[b.dstVar] = replacement{c.cliVar + "." + name, false}
	}
	c.findBoundInstances()
	if mp.local == nil {
		c.planStructStarts()
	}
}

// findBoundInstances finds the locals given a command struct by a statement
// other than their last one, as a := &A{} before a = &A{} is.
func (c *converter) findBoundInstances() {
	root := c.tree.RootPkg
	for _, f := range root.Syntax {
		dst.Inspect(f, func(n dst.Node) bool {
			var names []dst.Expr
			switch s := n.(type) {
			case *dst.AssignStmt:
				names = s.Lhs
			case *dst.DeclStmt:
				names = declaredNames(s)
			default:
				return true
			}
			vals := c.g.in.stmtVals[n.(dst.Stmt)]
			if len(vals) != len(names) {
				return true
			}
			for i, l := range names {
				v, ok := root.objectOf(l).(*types.Var)
				vp, isPtr := vals[i].(vPtr)
				if !ok || !isPtr || v.Parent() == v.Pkg().Scope() || c.main.instances[v] != nil {
					continue
				}
				if path, ok := c.instancePath(vp.o); ok {
					c.main.repl[v] = replacement{path, true}
					c.main.instances[v] = vp.o
				}
			}
			return true
		})
	}
}

// planStructStarts finds the package-level variables of the root package
// holding command structs, which the CLI replaces.
// The values they start with go in the declaration of a package-level cli,
// which takes the place of the first of them.
// Values that call functions or read package-level variables stay where
// they are, since moving them could change when they are computed or make
// cli depend on itself; the report lists them.
func (c *converter) planStructStarts() {
	mp := c.main
	root := c.tree.RootPkg
	mp.structSpecs = map[*dst.ValueSpec][]int{}
	var file *dst.File
	for _, f := range root.Syntax {
		for _, d := range f.Decls {
			gd, ok := d.(*dst.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, sp := range gd.Specs {
				vs := sp.(*dst.ValueSpec)
				var cmds []int
				for i, nm := range vs.Names {
					v, _ := root.objectOf(nm).(*types.Var)
					if _, replaced := mp.repl[v]; replaced && c.g.in.structVars[v] && len(vs.Names) == len(vs.Values) {
						cmds = append(cmds, i)
					}
				}
				var starts []structStart
				movable := len(cmds) > 0
				for _, i := range cmds {
					lit := initialValues(vs.Values[i])
					if lit == nil {
						continue
					}
					path, ok := c.startPath(mp.repl[root.objectOf(vs.Names[i])].path)
					movable = movable && ok && (file == nil || file == f) && !callsBesides(root, c.g.in, lit) && !c.readsChangingVars(root, lit)
					starts = append(starts, structStart{path: path, lit: lit})
				}
				if !movable {
					for _, i := range cmds {
						if initialValues(vs.Values[i]) != nil {
							c.issue(root.position(vs), "%s starts with values that cannot move to the declaration of %s; set them on %s by hand before parsing", vs.Names[i].Name, c.cliVar, mp.repl[root.objectOf(vs.Names[i])].path)
						}
					}
					continue
				}
				mp.structSpecs[vs] = cmds
				mp.starts = append(mp.starts, starts...)
				if len(starts) > 0 {
					file = f
					if mp.cliSpec == nil {
						mp.cliSpec = vs
					}
				}
			}
		}
	}
	// A command's own values come before those of its subcommands, which
	// nest inside them and need them keyed.
	slices.SortStableFunc(mp.starts, func(a, b structStart) int { return cmp.Compare(len(a.path), len(b.path)) })
	var unkeyed [][]string
	mp.starts = slices.DeleteFunc(mp.starts, func(st structStart) bool {
		for _, u := range unkeyed {
			if len(st.path) > len(u) && slices.Equal(st.path[:len(u)], u) {
				c.issue(c.tree.Root.cl.pos, "the CLI's %s starts with unkeyed values, so those of %s cannot go in it; give them by hand", strings.Join(u, "."), strings.Join(st.path, "."))
				return true
			}
		}
		if len(st.lit.Elts) > 0 {
			if _, keyed := st.lit.Elts[0].(*dst.KeyValueExpr); !keyed {
				unkeyed = append(unkeyed, st.path)
			}
		}
		return false
	})
}

// startPath splits a replacement path such as cli.Db.Migrate into the
// fields a composite literal of the CLI can set, which needs every command
// on the way to hold its subcommands in its own struct.
func (c *converter) startPath(path string) ([]string, bool) {
	fields := strings.Split(strings.TrimPrefix(path, c.cliVar+"."), ".")
	n := c.tree.Root
	for i, f := range fields {
		var next *node
		for _, ch := range n.children {
			if ch.field == f {
				next = ch
			}
		}
		if next == nil || next.typ == nil || i < len(fields)-1 && !next.containerOwn {
			return nil, false
		}
		n = next
	}
	return fields, true
}

func isApp(v value) bool {
	cl, ok := v.(vClause)
	return ok && cl.c.kind == kindApp
}

// readsChangingVars reports whether e reads a package-level variable that
// may hold another value once e moves into the declaration of cli: one of
// the program's, which could also depend on cli, or any other while the
// root package's initializers call functions that could change it.
func (c *converter) readsChangingVars(p *Package, e dst.Expr) bool {
	found := false
	dst.Inspect(e, func(n dst.Node) bool {
		if id, ok := n.(*dst.Ident); ok {
			if v, ok := p.objectOf(id).(*types.Var); ok && !v.IsField() && v.Pkg() != nil && v.Parent() == v.Pkg().Scope() {
				found = found || c.prog.owns(v.Pkg()) || c.rootInitCalls()
			}
		}
		return !found
	})
	return found
}

// rootInitCalls reports whether a package-level variable of the root
// package is initialized with a call that could have side effects, other
// than building the kingpin grammar.
func (c *converter) rootInitCalls() bool {
	root := c.tree.RootPkg
	for _, f := range root.Syntax {
		for _, d := range f.Decls {
			if gd, ok := d.(*dst.GenDecl); ok && gd.Tok == token.VAR {
				for _, sp := range gd.Specs {
					vs := sp.(*dst.ValueSpec)
					vals := c.g.in.specVals[vs]
					for i, e := range vs.Values {
						grammar := i < len(vals) && len(vs.Values) == len(vals) && (c.isGrammarValue(vals[i]) || isApp(vals[i]))
						if !grammar && callsBesides(root, c.g.in, e) {
							return true
						}
					}
				}
			}
		}
	}
	return false
}

// cliStart builds the composite literal a package-level cli starts with.
func (c *converter) cliStart() *dst.CompositeLit {
	lit := c.cliLiteral()
	if lit == nil {
		lit = &dst.CompositeLit{Type: dst.NewIdent(c.cliName)}
	}
	cli := lit.(*dst.CompositeLit)
	for _, st := range c.main.starts {
		cur, n := cli, c.tree.Root
		for i, f := range st.path {
			for _, ch := range n.children {
				if ch.field == f {
					n = ch
				}
			}
			if i == len(st.path)-1 {
				st.lit.Type = c.typeIdent(n.typ)
				cur.Elts = append(cur.Elts, &dst.KeyValueExpr{Key: dst.NewIdent(f), Value: st.lit})
				break
			}
			var inner *dst.CompositeLit
			for _, e := range cur.Elts {
				if kv, ok := e.(*dst.KeyValueExpr); ok && kv.Key.(*dst.Ident).Name == f {
					inner, _ = kv.Value.(*dst.CompositeLit)
				}
			}
			if inner == nil {
				inner = &dst.CompositeLit{Type: c.typeIdent(n.typ)}
				cur.Elts = append(cur.Elts, &dst.KeyValueExpr{Key: dst.NewIdent(f), Value: inner})
			}
			cur = inner
		}
	}
	for _, e := range cli.Elts {
		e.Decorations().Before, e.Decorations().After = dst.NewLine, dst.NewLine
	}
	return cli
}

func (c *converter) typeIdent(t *types.Named) *dst.Ident {
	id := dst.NewIdent(t.Obj().Name())
	if path := t.Obj().Pkg().Path(); path != c.tree.RootPkg.PkgPath {
		id.Path = path
	}
	return id
}

// copiesVariable reports whether a variable of the root package is
// initialized from another variable.
func (c *converter) copiesVariable(o types.Object) bool {
	root := c.tree.RootPkg
	copied := false
	// A variable or what a pointer variable points to, as in *serve.
	isVar := func(e dst.Expr) bool {
		e = unparen(e)
		if star, ok := e.(*dst.StarExpr); ok {
			e = unparen(star.X)
		}
		switch e.(type) {
		case *dst.Ident, *dst.SelectorExpr:
			_, ok := root.objectOf(e).(*types.Var)
			return ok
		}
		return false
	}
	for _, f := range root.Syntax {
		dst.Inspect(f, func(n dst.Node) bool {
			switch n := n.(type) {
			case *dst.AssignStmt:
				if n.Tok == token.DEFINE && len(n.Lhs) == len(n.Rhs) {
					for i, l := range n.Lhs {
						if root.objectOf(l) == o && isVar(n.Rhs[i]) {
							copied = true
						}
					}
				}
			case *dst.ValueSpec:
				if len(n.Names) == len(n.Values) {
					for i, nm := range n.Names {
						if root.objectOf(nm) == o && isVar(n.Values[i]) {
							copied = true
						}
					}
				}
			}
			return !copied
		})
	}
	return copied
}

// nodePaths maps every command to its path from the cli variable.
func (c *converter) nodePaths() map[*clause]string {
	out := map[*clause]string{}
	var walk func(n *node, path string)
	walk = func(n *node, path string) {
		out[n.cl] = path
		for _, ch := range n.children {
			walk(ch, path+"."+ch.field)
		}
	}
	walk(c.tree.Root, c.cliVar)
	return out
}

// instancePath is the path from the cli variable to a command's struct.
func (c *converter) instancePath(o *object) (string, bool) {
	path, ok := c.cmdStructPath(o)
	return c.cliVar + path, ok
}

// findPointers finds the locals holding a command struct that code left
// after the wiring still reads, other than a plain definition the CLI can
// take over (a := &A{...}).
// They keep holding a pointer, to the struct in the CLI, so the code using
// them stays as it is.
func (c *converter) findPointers() {
	mp := c.main
	mp.pointers = map[types.Object]string{}
	root := c.tree.RootPkg
	pure := func(s dst.Stmt) bool {
		var names, values []dst.Expr
		switch s := s.(type) {
		case *dst.AssignStmt:
			names, values = s.Lhs, s.Rhs
		case *dst.DeclStmt:
			gd, ok := s.Decl.(*dst.GenDecl)
			if !ok || len(gd.Specs) != 1 {
				return false
			}
			vs := gd.Specs[0].(*dst.ValueSpec)
			names, values = identExprs(vs.Names), vs.Values
		default:
			return false
		}
		vals := c.g.in.stmtVals[s]
		if len(names) != len(values) || len(vals) != len(names) {
			return false
		}
		for i, n := range names {
			o := root.objectOf(n)
			_, replaced := mp.repl[o]
			vp, isPtr := vals[i].(vPtr)
			if !replaced || !isPtr || vp.o != mp.instances[o] || !builtInPlace(root, values[i]) && callsBesides(root, c.g.in, values[i]) {
				return false
			}
		}
		return true
	}
	for _, f := range root.Syntax {
		for _, d := range f.Decls {
			fd, ok := d.(*dst.FuncDecl)
			if !ok || fd.Body == nil || c.ed.removed[d] {
				continue
			}
			var stack []dst.Node
			dst.Inspect(fd.Body, func(n dst.Node) bool {
				if n == nil {
					stack = stack[:len(stack)-1]
					return true
				}
				if id, ok := n.(*dst.Ident); ok {
					o := root.objectOf(id)
					if r, ok := mp.repl[o]; ok && mp.instances[o] != nil && o.Parent() != o.Pkg().Scope() {
						if s := innermostStmt(stack); s != nil && !pure(s) {
							mp.pointers[o] = r.path
						}
					}
				}
				stack = append(stack, n)
				return true
			})
		}
	}
	for o := range mp.pointers {
		delete(mp.repl, o)
	}
}

func innermostStmt(stack []dst.Node) dst.Stmt {
	for i := len(stack) - 1; i >= 0; i-- {
		if s, ok := stack[i].(dst.Stmt); ok {
			return s
		}
	}
	return nil
}

// rewriteMain rewrites the main-style program around the CLI.
func (c *converter) rewriteMain() {
	root := c.tree.RootPkg
	c.findPointers()
	wired := c.main.flags == nil
	for _, f := range root.Syntax {
		// The flags struct is wired first, so the parser declared right
		// before kingpin.Parse() comes after it.
		changed := false
		if !wired && c.wireFlags(root, f) {
			wired, changed = true, true
		}
		changed = c.declareParsers(root, f) || changed
		for _, d := range f.Decls {
			if c.ed.removed[d] {
				continue
			}
			if gd, ok := d.(*dst.GenDecl); ok && gd.Tok == token.VAR {
				if c.rewriteGlobals(root, gd) {
					changed = true
					if len(gd.Specs) == 0 {
						c.ed.removeDecl(root, f, gd)
						continue
					}
				}
			}
			dstutil.Apply(d, func(cur *dstutil.Cursor) bool {
				node := cur.Node()
				if c.rewriteNode(root, cur) {
					changed = true
					// A statement removed or replaced is not walked further.
					if _, isStmt := node.(*dst.ExprStmt); isStmt {
						return false
					}
				}
				return true
			}, nil)
		}
		for _, d := range f.Decls {
			fd, ok := d.(*dst.FuncDecl)
			if !ok || fd.Body == nil || c.ed.removed[d] {
				continue
			}
			if c.dropReplacedLocals(root, fd.Body) {
				changed = true
			}
		}
		if changed {
			c.ed.touch(root, f)
		}
	}
	if !wired {
		c.issue(c.tree.Root.cl.pos, "the CLI's %s must point at %s before the parser is built, which the tool could not arrange", c.main.flags.field, c.main.flags.v.Name())
	}
	for _, p := range c.prog.Packages {
		for _, f := range p.Syntax {
			read := map[*dst.CallExpr]string{}
			dstutil.Apply(f, func(cur *dstutil.Cursor) bool {
				switch n := cur.Node().(type) {
				case *dst.SelectorExpr:
					if call, ok := unparen(n.X).(*dst.CallExpr); ok {
						read[call] = n.Sel.Name
					}
					if p != root {
						c.reportMethodValue(p, cur, n)
						if c.adaptErrors(p, n) {
							c.ed.touch(p, f)
						}
					}
				case *dst.CallExpr:
					// Calls in other packages also drop the arguments of
					// pruned parameters.
					if p != root && c.followPrunedCall(p, n) {
						c.ed.touch(p, f)
					}
					// A conversion whose result is read from, as in
					// (*kingpin.Application)(nil).Name, keeps the kingpin
					// type, since the kong parser has none of its fields.
					if tv, ok := p.tv(n.Fun); ok && tv.IsType() && read[n] != "" {
						if c.isAppType(tv.Type) && slices.ContainsFunc(n.Args, func(a dst.Expr) bool { return c.isAppType(p.typeOf(a)) && !isNilIdent(p, a) }) {
							c.issue(p.position(n), "this reads the kingpin application's %s, which the kong parser that replaced it does not have; rewrite it by hand", read[n])
						}
						for _, a := range n.Args {
							c.retypeApplication(p, f, a)
						}
						return false
					}
				case *dst.Ident:
					// Kept code now passes the kong parser around where it
					// passed the application.
					if n.Name == "Application" && c.prog.isKingpinPath(n.Path) {
						n.Name, n.Path = "Kong", "github.com/alecthomas/kong"
						c.ed.touch(p, f)
					}
				}
				return true
			}, nil)
		}
	}
}

// retypeApplication is the retyping of rewriteMain for the node n.
func (c *converter) retypeApplication(p *Package, f *dst.File, n dst.Node) {
	dstutil.Apply(n, func(cur *dstutil.Cursor) bool {
		if call, ok := cur.Node().(*dst.CallExpr); ok {
			if tv, ok := p.tv(call.Fun); ok && tv.IsType() {
				if sel, read := cur.Parent().(*dst.SelectorExpr); read && sel.X == dst.Expr(call) {
					for _, a := range call.Args {
						c.retypeApplication(p, f, a)
					}
					return false
				}
			}
		}
		if id, ok := cur.Node().(*dst.Ident); ok && id.Name == "Application" && c.prog.isKingpinPath(id.Path) {
			id.Name, id.Path = "Kong", "github.com/alecthomas/kong"
			c.ed.touch(p, f)
		}
		return true
	}, nil)
}

// rewriteGlobals drops package-level variables that only held grammar and
// rewrites the creation of a package-level application.
func (c *converter) rewriteGlobals(p *Package, gd *dst.GenDecl) bool {
	changed := false
	var specs []dst.Spec
	for _, sp := range gd.Specs {
		vs := sp.(*dst.ValueSpec)
		if cmds, ok := c.main.structSpecs[vs]; ok {
			if vs == c.main.cliSpec {
				cli := &dst.ValueSpec{Names: []*dst.Ident{dst.NewIdent(c.cliVar)}, Values: []dst.Expr{c.cliStart()}}
				cli.Decs.Before = dst.EmptyLine
				cli.Decs.Start.Append("// " + c.cliVar + " holds the parsed command line.")
				specs = append(specs, cli)
			}
			changed = true
			// The other variables declared with the commands stay.
			var names []*dst.Ident
			var values []dst.Expr
			for i := range vs.Names {
				if !slices.Contains(cmds, i) {
					names, values = append(names, vs.Names[i]), append(values, vs.Values[i])
				}
			}
			if len(names) > 0 {
				vs.Names, vs.Values = names, values
				specs = append(specs, vs)
			}
			continue
		}
		vals := c.g.in.specVals[vs]
		// A variable without initializer that the CLI replaces, such as a
		// flag destination, goes too.
		drop := len(vals) > 0 || len(vs.Values) == 0
		for i, nm := range vs.Names {
			o := p.objectOf(nm)
			if _, ok := c.main.repl[o]; ok {
				continue
			}
			if i < len(vals) {
				if cl, ok := vals[i].(vClause); ok && cl.c.kind != kindApp {
					continue
				}
			}
			drop = false
		}
		if drop {
			changed = true
			for _, v := range vs.Values {
				if c.reportInitWork(p, vs.Names[0].Name, v) {
					break
				}
			}
			continue
		}
		specs = append(specs, sp)
	}
	if changed {
		gd.Specs = specs
	}
	return changed
}

// reportMethodValue reports an app method used as a value, as in
// usage := app.Usage, since the kong parser doesn't have it.
// Errorf, Fatalf and FatalIfError are handled by adaptErrors.
func (c *converter) reportMethodValue(p *Package, cur *dstutil.Cursor, sel *dst.SelectorExpr) {
	if call, ok := cur.Parent().(*dst.CallExpr); ok && call.Fun == dst.Expr(sel) {
		return
	}
	switch sel.Sel.Name {
	case "Errorf", "Fatalf", "FatalIfError":
		return
	}
	if s := p.selection(sel); s != nil && s.Kind() == types.MethodVal && c.isAppType(s.Recv()) {
		c.issue(p.position(sel), "%s is used as a value, but the kong parser has no such method; rewrite it by hand", render(sel))
	}
}

// adaptErrors routes the application's Errorf, Fatalf and FatalIfError
// through kongcompat.Errors, whether they are called or used as values.
// kong's methods with the same names print and exit a little differently,
// and return the parser.
func (c *converter) adaptErrors(p *Package, sel *dst.SelectorExpr) bool {
	switch sel.Sel.Name {
	case "Errorf", "Fatalf", "FatalIfError":
	default:
		return false
	}
	s := p.selection(sel)
	if s == nil || !c.isAppType(s.Recv()) {
		return false
	}
	if s.Kind() != types.MethodVal {
		c.issue(p.position(sel), "the method expression %s has no kong equivalent; call kongcompat.Errors(app).%s instead", render(sel), sel.Sel.Name)
		return false
	}
	app := sel.X
	if _, ptr := typeUnder(p.typeOf(sel.X)).(*types.Pointer); !ptr {
		app = &dst.UnaryExpr{Op: token.AND, X: app}
	}
	sel.X = callOf(c.compatIdent("Errors"), app)
	return true
}

// reportInitWork raises an issue when v, a package-level initializer the
// conversion drops, also does other work, and reports whether it did.
func (c *converter) reportInitWork(p *Package, name string, v dst.Expr) bool {
	if !c.doesSomething(p, v) {
		return false
	}
	c.issue(p.position(v), "the initializer of %s also does other work, which no longer runs; run what %s does besides registering before parsing by hand", name, render(v))
	return true
}

// rewriteNode rewrites one node of the main-style program: the application's
// creation and runtime calls, FullCommand comparisons, and references to
// replaced variables.
func (c *converter) rewriteNode(p *Package, cur *dstutil.Cursor) bool {
	switch n := cur.Node().(type) {
	case *dst.ExprStmt:
		call, ok := n.X.(*dst.CallExpr)
		if !ok {
			return false
		}
		chain, recv := c.configChain(p, call)
		if len(chain) == 0 {
			return false
		}
		var stmts []dst.Stmt
		for _, link := range chain {
			s, ok := c.configStmt(p, link, recv)
			if !ok {
				return false
			}
			stmts = append(stmts, s...)
		}
		for _, s := range stmts {
			cur.InsertBefore(s)
		}
		cur.Delete()
		return true
	case *dst.AssignStmt:
		if c.isAppSetting(p, n) {
			cur.Delete()
			return true
		}
	case *dst.CallExpr:
		return c.rewriteCall(p, cur, n)
	case *dst.StarExpr:
		if r, ok := c.replaced(p, n.X); ok && r.pointer {
			cur.Replace(pathExpr(r.path))
			return true
		}
	case *dst.SelectorExpr:
		if r, ok := c.replaced(p, n.X); ok {
			n.X = pathExpr(r.path)
			return true
		}
		c.reportMethodValue(p, cur, n)
		return c.adaptErrors(p, n)
	case *dst.Ident:
		// Only the names being declared stay; their uses are replaced.
		if _, isDecl := cur.Parent().(*dst.ValueSpec); isDecl && cur.Name() == "Names" {
			return false
		}
		if as, ok := cur.Parent().(*dst.AssignStmt); ok && cur.Name() == "Lhs" {
			if as.Tok == token.DEFINE {
				return false
			}
			if r, ok := c.replaced(p, n); ok && r.pointer {
				// A new struct, as in a = &A{}, becomes cli.A = A{}.
				if c.rebinds(p, as) && c.definesOnlyReplaced(p, as) {
					return false
				}
				c.issue(p.position(as), "%s is reassigned, but it now points into the CLI; assign the field instead", n.Name)
				return false
			}
		}
		if r, ok := c.replaced(p, n); ok {
			if r.pointer {
				cur.Replace(&dst.UnaryExpr{Op: token.AND, X: pathExpr(r.path)})
			} else {
				cur.Replace(pathExpr(r.path))
			}
			return true
		}
	}
	return false
}

// settingsChain rewrites settings chained onto an application, as in
// app := kingpin.New(name, help).Version(v).
// Settings already collected by scanAppSettings are dropped.
// On kingpin.New, Terminate and the writers become kong.Must options.
func (c *converter) settingsChain(p *Package, call *dst.CallExpr) (dst.Expr, bool) {
	var opts []dst.Expr
	var e dst.Expr = call
	for {
		link, ok := e.(*dst.CallExpr)
		if !ok {
			break
		}
		name := c.runtimeCall(p, link)
		if name == "New" {
			var appName, help dst.Expr
			if len(link.Args) >= 1 {
				appName = link.Args[0]
			}
			if len(link.Args) >= 2 {
				help = link.Args[1]
			}
			return c.mustCall(append(c.parserArgs(appName, help), opts...)), true
		}
		if !configNames[name] && (name != "Action" || c.genActions[c.tree.Root] == nil) {
			break
		}
		switch name {
		case "Terminate":
			opts = append([]dst.Expr{callOf(kongIdent("Exit"), exitFunc(link.Args[0]))}, opts...)
		case "Writers":
			opts = append([]dst.Expr{callOf(kongIdent("Writers"), link.Args...)}, opts...)
		case "Writer":
			opts = append([]dst.Expr{callOf(kongIdent("Writers"), link.Args[0], dst.Clone(link.Args[0]).(dst.Expr))}, opts...)
		case "UsageWriter":
			if c.main.stdout == nil {
				c.main.stdout = link.Args[0]
			}
		case "ErrorWriter":
			if c.main.stderr == nil {
				c.main.stderr = link.Args[0]
			}
		}
		sel, ok := link.Fun.(*dst.SelectorExpr)
		if !ok {
			// A package-level setting such as kingpin.Version(v), whose
			// result is kingpin.CommandLine.
			if len(opts) > 0 {
				return nil, false
			}
			return c.parserExpr(), true
		}
		e = sel.X
	}
	if len(opts) > 0 {
		c.issue(p.position(call), "kingpin settings chained onto an existing application: set the parser's Exit, Stdout or Stderr by hand")
		return nil, false
	}
	return e, true
}

// exitFunc turns kingpin's Terminate argument into kong's Exit.
// kingpin took nil to mean "don't exit", kong needs a function doing nothing.
func exitFunc(arg dst.Expr) dst.Expr {
	if id, ok := arg.(*dst.Ident); ok && id.Name == "nil" {
		return &dst.FuncLit{
			Type: &dst.FuncType{Params: &dst.FieldList{List: []*dst.Field{{Type: dst.NewIdent("int")}}}},
			Body: &dst.BlockStmt{},
		}
	}
	return arg
}

// holderVar returns the variable of the program that holds o, the struct the
// application flags are written to, and whether it holds a pointer to it.
func (c *converter) holderVar(o *object) (*types.Var, bool) {
	in := c.g.in
	// Take the first variable declared with the struct's own type.
	// One of an interface type couldn't be assigned to the CLI's field.
	var best *types.Var
	bestPtr := false
	check := func(obj types.Object, val value) {
		v, ok := obj.(*types.Var)
		if !ok || v.Pkg() != c.tree.RootPkg.Types || best != nil && best.Pos() <= v.Pos() {
			return
		}
		switch val := val.(type) {
		case vObj:
			if val.o == o && (o.typ == nil || types.Identical(v.Type(), o.typ)) {
				best, bestPtr = v, false
			}
		case vPtr:
			if val.o == o && (o.typ == nil || types.Identical(deref(v.Type()), o.typ)) {
				best, bestPtr = v, true
			}
		}
	}
	for obj, val := range in.varVals {
		check(obj, val)
	}
	if best != nil {
		return best, bestPtr
	}
	for obj, val := range in.globals {
		check(obj, val)
	}
	return best, bestPtr
}

// wireFlags points the CLI's embedded flags struct at the program's own
// variable, right before the statement of f that builds the parser.
// That is when kong reads the struct.
// A package-level variable is wired where cli is declared instead.
func (c *converter) wireFlags(p *Package, f *dst.File) bool {
	w := c.main.flags
	if w.v.Parent() == w.v.Pkg().Scope() && c.main.local == nil {
		return true
	}
	var rhs dst.Expr = dst.NewIdent(w.v.Name())
	if !w.ptr {
		rhs = &dst.UnaryExpr{Op: token.AND, X: rhs}
	}
	assign := &dst.AssignStmt{
		Lhs: []dst.Expr{&dst.SelectorExpr{X: dst.NewIdent(c.cliVar), Sel: dst.NewIdent(w.field)}},
		Tok: token.ASSIGN,
		Rhs: []dst.Expr{rhs},
	}
	done := false
	for _, d := range f.Decls {
		fd, ok := d.(*dst.FuncDecl)
		if !ok || fd.Body == nil || c.ed.removed[d] {
			continue
		}
		fd.Body.List = insertBefore(fd.Body.List, func(s dst.Stmt, calls []*dst.CallExpr) []dst.Stmt {
			builds := slices.ContainsFunc(calls, func(call *dst.CallExpr) bool {
				_, packageLevel := call.Fun.(*dst.Ident)
				name := c.runtimeCall(p, call)
				return name == "New" || name == "Parse" && packageLevel
			})
			if done || !builds || !c.namesAt(p, s, w.v) {
				return nil
			}
			done = true
			return []dst.Stmt{assign}
		})
	}
	// kong keeps the struct the variable held when the parser was built.
	dst.Inspect(f, func(n dst.Node) bool {
		if as, ok := n.(*dst.AssignStmt); ok && done {
			for _, l := range as.Lhs {
				if p.objectOf(l) == w.v && !c.declares(p, l) {
					c.issue(p.position(as), "%s is assigned again, but kong writes the flags into the struct it held when the parser was built; point cli.%s at the right one by hand", w.v.Name(), w.field)
				}
			}
		}
		return true
	})
	return done
}

// declareParsers declares the kong parser in its own variable before each
// statement calling kingpin.Parse() on the package-level application.
// That reads better than building it inside the call.
func (c *converter) declareParsers(p *Package, f *dst.File) bool {
	if _, ok := c.parserExpr().(*dst.Ident); ok {
		// The program keeps its parser in a package-level variable.
		return false
	}
	changed := false
	for _, d := range f.Decls {
		fd, ok := d.(*dst.FuncDecl)
		if !ok || fd.Body == nil || c.ed.removed[d] {
			continue
		}
		// With a goto around, a new variable could be jumped over, so the
		// parser is built inside the call instead.
		//
		// The parser's name must not clash with any name in the function.
		used := map[string]bool{}
		hasGoto := false
		dst.Inspect(fd.Body, func(n dst.Node) bool {
			switch n := n.(type) {
			case *dst.Ident:
				used[n.Name] = true
			case *dst.BranchStmt:
				hasGoto = hasGoto || n.Tok == token.GOTO
			}
			return true
		})
		if hasGoto {
			continue
		}
		fd.Body.List = insertBefore(fd.Body.List, func(s dst.Stmt, calls []*dst.CallExpr) []dst.Stmt {
			i := slices.IndexFunc(calls, func(call *dst.CallExpr) bool {
				_, ok := call.Fun.(*dst.Ident)
				return ok && c.runtimeCall(p, call) == "Parse" && len(call.Args) == 0
			})
			if i < 0 {
				return nil
			}
			name := c.freeLocalBesides(p, s, used, "parser", "kongParser")
			used[name] = true
			if c.main.parsers == nil {
				c.main.parsers = map[*dst.CallExpr]string{}
			}
			c.main.parsers[calls[i]] = name
			changed = true
			return []dst.Stmt{&dst.AssignStmt{
				Lhs: []dst.Expr{dst.NewIdent(name)},
				Tok: token.DEFINE,
				Rhs: []dst.Expr{c.parserExpr()},
			}}
		})
	}
	return changed
}

// insertBefore inserts, right before each statement of list, the statements
// that before returns for it, going into nested blocks and case clauses.
// before only sees the calls a statement makes outside its nested
// statements, which get their own turn first.
func insertBefore(list []dst.Stmt, before func(s dst.Stmt, calls []*dst.CallExpr) []dst.Stmt) []dst.Stmt {
	var out []dst.Stmt
	for _, s := range list {
		var calls []*dst.CallExpr
		dst.Inspect(s, func(n dst.Node) bool {
			switch n := n.(type) {
			case *dst.BlockStmt:
				n.List = insertBefore(n.List, before)
				return false
			case *dst.CaseClause:
				n.Body = insertBefore(n.Body, before)
				return false
			case *dst.CommClause:
				n.Body = insertBefore(n.Body, before)
				return false
			case *dst.CallExpr:
				calls = append(calls, n)
			}
			return true
		})
		out = append(append(out, before(s, calls)...), s)
	}
	return out
}

// freeLocal returns the first candidate, or the first one with a number
// appended, that names nothing visible at s.
func (c *converter) freeLocal(p *Package, s dst.Stmt, candidates ...string) string {
	return c.freeLocalBesides(p, s, nil, candidates...)
}

// freeLocalBesides is freeLocal, also avoiding the names in added, which
// the conversion declared and type information knows nothing about.
func (c *converter) freeLocalBesides(p *Package, s dst.Stmt, added map[string]bool, candidates ...string) string {
	n := p.astOf(s)
	var scope *types.Scope
	if n != nil {
		scope = p.Types.Scope().Innermost(n.Pos())
	}
	taken := func(name string) bool {
		if added[name] {
			return true
		}
		if scope == nil {
			return false
		}
		_, obj := scope.LookupParent(name, n.Pos())
		return obj != nil
	}
	return firstFree(taken, candidates...)
}

// firstFree returns the first candidate that is not taken, or else the first
// one with the smallest number that makes it free.
func firstFree(taken func(string) bool, candidates ...string) string {
	for _, cand := range candidates {
		if !taken(cand) {
			return cand
		}
	}
	for i := 2; ; i++ {
		if cand := fmt.Sprintf("%s%d", candidates[0], i); !taken(cand) {
			return cand
		}
	}
}

// configNames are the kingpin calls that configure the application at run
// time, possibly chained.
var configNames = map[string]bool{
	"Terminate": true, "Writers": true, "Writer": true, "UsageWriter": true, "ErrorWriter": true,
	"Version": true, "UsageTemplate": true, "Author": true, "DefaultEnvars": true, "UsageContext": true,
}

// configChain splits a statement such as app.UsageTemplate(t).Version(v)
// into its configuration calls, innermost first.
// It also returns the application they configure, nil for kingpin's
// package-level one.
func (c *converter) configChain(p *Package, call *dst.CallExpr) ([]*dst.CallExpr, dst.Expr) {
	var chain []*dst.CallExpr
	for {
		if !configNames[c.runtimeCall(p, call)] {
			return nil, nil
		}
		chain = append([]*dst.CallExpr{call}, chain...)
		sel, ok := call.Fun.(*dst.SelectorExpr)
		if !ok {
			return chain, nil
		}
		inner, ok := sel.X.(*dst.CallExpr)
		if !ok || c.runtimeCall(p, inner) == "" {
			if id, ok := sel.X.(*dst.Ident); ok && id.Name == "CommandLine" && c.prog.isKingpinPath(id.Path) {
				return chain, nil
			}
			return chain, sel.X
		}
		call = inner
	}
}

// configStmt returns the statements replacing one configuration call on the
// application recv.
// Settings collected by scanAppSettings leave nothing behind.
func (c *converter) configStmt(p *Package, call *dst.CallExpr, recv dst.Expr) ([]dst.Stmt, bool) {
	name := c.runtimeCall(p, call)
	switch name {
	case "Version", "UsageTemplate", "Author", "DefaultEnvars", "UsageContext":
		return nil, true
	}
	if recv == nil {
		// scanPackageSettings already made these kong.Must options when it
		// could.
		if c.main.packageSettings[call] {
			return nil, true
		}
		c.issue(p.position(call), "kingpin %s on the package-level application, under a condition: set the parser's Exit, Stdout or Stderr by hand", name)
		return nil, false
	}
	switch name {
	case "Terminate":
		if len(call.Args) != 1 {
			return nil, false
		}
		return []dst.Stmt{&dst.AssignStmt{Lhs: []dst.Expr{&dst.SelectorExpr{X: dst.Clone(recv).(dst.Expr), Sel: dst.NewIdent("Exit")}}, Tok: token.ASSIGN, Rhs: []dst.Expr{exitFunc(call.Args[0])}}}, true
	}
	// kongcompat prints usage on the parser's Stdout and errors on its
	// Stderr; kingpin v2's Writer set both.
	var fields []string
	args := call.Args
	switch name {
	case "Writers":
		fields = []string{"Stdout", "Stderr"}
	case "Writer":
		fields = []string{"Stdout", "Stderr"}
		if len(args) == 1 {
			args = []dst.Expr{args[0], dst.Clone(args[0]).(dst.Expr)}
		}
	case "UsageWriter":
		fields = []string{"Stdout"}
	case "ErrorWriter":
		fields = []string{"Stderr"}
	}
	if len(args) != len(fields) {
		return nil, false
	}
	var lhs []dst.Expr
	for _, f := range fields {
		lhs = append(lhs, &dst.SelectorExpr{X: dst.Clone(recv).(dst.Expr), Sel: dst.NewIdent(f)})
	}
	return []dst.Stmt{&dst.AssignStmt{Lhs: lhs, Tok: token.ASSIGN, Rhs: args}}, true
}

// scanAppSettings collects the non-grammar settings kong takes where the
// parser is built: the version, usage template, usage context, author, and
// assignments to the Name and Help fields.
func (c *converter) scanAppSettings() {
	mp := c.main
	root := c.tree.RootPkg
	// Only settings made unconditionally, from values that exist where the
	// parser is built, can move there.
	movable := func(n dst.Node, top bool, arg dst.Expr) bool {
		if !top {
			c.issue(root.position(n), "kingpin setting under a condition or in a loop: kong takes it where the parser is built, so apply it there by hand")
			return false
		}
		local := ""
		dst.Inspect(arg, func(x dst.Node) bool {
			if id, ok := x.(*dst.Ident); ok {
				if v, ok := root.objectOf(id).(*types.Var); ok && !v.IsField() && v.Parent() != v.Pkg().Scope() {
					local = id.Name
				}
			}
			return local == ""
		})
		if local != "" {
			c.issue(root.position(n), "kingpin setting computed from the local variable %s: kong takes it where the parser is built, so move it there by hand", local)
			return false
		}
		return true
	}
	visit := func(n dst.Node, top bool) {
		dst.Inspect(n, func(x dst.Node) bool {
			switch x := x.(type) {
			case *dst.FuncLit:
				return false
			case *dst.CallExpr:
				if len(x.Args) != 1 {
					return true
				}
				name := c.runtimeCall(root, x)
				if name != "Version" && name != "UsageTemplate" && name != "Author" && name != "UsageContext" {
					return true
				}
				if !movable(x, top, x.Args[0]) {
					return true
				}
				arg := dst.Clone(x.Args[0]).(dst.Expr)
				switch name {
				case "Version":
					mp.version = arg
				case "UsageTemplate":
					mp.usage = c.compatTemplate(arg)
				case "Author":
					mp.author = arg
				case "UsageContext":
					// kongcompat.UsageContext has the same fields.
					dst.Inspect(arg, func(n dst.Node) bool {
						if id, ok := n.(*dst.Ident); ok && c.prog.isKingpinPath(id.Path) {
							id.Path = c.compatPath
						}
						return true
					})
					mp.usageContext = arg
				}
			case *dst.AssignStmt:
				if !c.isAppSetting(root, x) || !movable(x, top, x.Rhs[0]) {
					return true
				}
				rhs := dst.Clone(x.Rhs[0]).(dst.Expr)
				if x.Lhs[0].(*dst.SelectorExpr).Sel.Name == "Name" {
					mp.name = rhs
				} else {
					mp.help = rhs
				}
			}
			return true
		})
	}
	for _, f := range root.Syntax {
		for _, d := range f.Decls {
			fd, ok := d.(*dst.FuncDecl)
			if !ok {
				visit(d, true)
				continue
			}
			if fd.Body == nil {
				continue
			}
			for _, s := range fd.Body.List {
				switch s.(type) {
				case *dst.ExprStmt, *dst.AssignStmt, *dst.DeclStmt:
					visit(s, true)
				default:
					visit(s, false)
				}
			}
		}
	}
}

// scanPackageSettings collects the Terminate and writer calls made on
// kingpin's package-level application as top-level statements of a function.
// They become kong.Must options.
func (c *converter) scanPackageSettings() {
	mp := c.main
	root := c.tree.RootPkg
	for _, f := range root.Syntax {
		for _, d := range f.Decls {
			fd, ok := d.(*dst.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			for _, s := range fd.Body.List {
				es, ok := s.(*dst.ExprStmt)
				if !ok {
					continue
				}
				call, ok := es.X.(*dst.CallExpr)
				if !ok {
					continue
				}
				chain, recv := c.configChain(root, call)
				if recv != nil {
					continue
				}
				for _, link := range chain {
					args := link.Args
					switch c.runtimeCall(root, link) {
					case "Terminate":
						mp.exit = dst.Clone(args[0]).(dst.Expr)
					case "Writers":
						mp.stdout, mp.stderr = dst.Clone(args[0]).(dst.Expr), dst.Clone(args[1]).(dst.Expr)
					case "Writer":
						mp.stdout, mp.stderr = dst.Clone(args[0]).(dst.Expr), dst.Clone(args[0]).(dst.Expr)
					case "UsageWriter":
						mp.stdout = dst.Clone(args[0]).(dst.Expr)
					case "ErrorWriter":
						mp.stderr = dst.Clone(args[0]).(dst.Expr)
					default:
						continue
					}
					if mp.packageSettings == nil {
						mp.packageSettings = map[*dst.CallExpr]bool{}
					}
					mp.packageSettings[link] = true
				}
			}
		}
	}
}

// isAppSetting reports whether s assigns the Name or Help of a kingpin
// application.
func (c *converter) isAppSetting(p *Package, s *dst.AssignStmt) bool {
	if len(s.Lhs) != 1 || len(s.Rhs) != 1 || s.Tok != token.ASSIGN {
		return false
	}
	sel, ok := s.Lhs[0].(*dst.SelectorExpr)
	if !ok || sel.Sel.Name != "Name" && sel.Sel.Name != "Help" {
		return false
	}
	if id, ok := sel.X.(*dst.Ident); ok && id.Name == "CommandLine" && c.prog.isKingpinPath(id.Path) {
		return true
	}
	return c.isAppType(p.typeOf(sel.X))
}

func (c *converter) replaced(p *Package, e dst.Expr) (replacement, bool) {
	id, ok := e.(*dst.Ident)
	if !ok || id.Path != "" {
		return replacement{}, false
	}
	r, ok := c.main.repl[p.objectOf(id)]
	return r, ok
}

const kongPath = "github.com/alecthomas/kong"

func (c *converter) compatIdent(name string) *dst.Ident {
	return &dst.Ident{Name: name, Path: c.compatPath}
}

func kongIdent(name string) *dst.Ident {
	return &dst.Ident{Name: name, Path: kongPath}
}

// mustCall is kong.Must(&cli, options...), with one option per line when
// there are more than two.
func (c *converter) mustCall(args []dst.Expr) *dst.CallExpr {
	call := callOf(kongIdent("Must"), args...)
	c.parserCalls = append(c.parserCalls, call)
	if len(args) > 3 {
		for _, a := range args[1:] {
			a.Decorations().Before = dst.NewLine
		}
		args[len(args)-1].Decorations().After = dst.NewLine
	}
	return call
}

func callOf(fun dst.Expr, args ...dst.Expr) *dst.CallExpr {
	return &dst.CallExpr{Fun: fun, Args: args}
}

// usageExpr is the usage context passed to kongcompat: the one the program
// set, one built from its template, or nil for kingpin's default.
func (c *converter) usageExpr() dst.Expr {
	if c.main.usageContext != nil {
		return dst.Clone(c.main.usageContext).(dst.Expr)
	}
	if c.main.usage == nil {
		return dst.NewIdent("nil")
	}
	return &dst.UnaryExpr{Op: token.AND, X: &dst.CompositeLit{
		Type: c.compatIdent("UsageContext"),
		Elts: []dst.Expr{&dst.KeyValueExpr{Key: dst.NewIdent("Template"), Value: dst.Clone(c.main.usage).(dst.Expr)}},
	}}
}

// rewriteCall rewrites the kingpin calls of a main-style program.
func (c *converter) rewriteCall(p *Package, cur *dstutil.Cursor, call *dst.CallExpr) bool {
	if c.followPrunedCall(p, call) {
		return true
	}
	sel, _ := call.Fun.(*dst.SelectorExpr)
	// cmd.FullCommand() on a command becomes the string kingpin returned.
	if sel != nil && sel.Sel.Name == "FullCommand" && len(call.Args) == 0 {
		if cl, ok := c.clauseOf(p, sel.X); ok && cl.kind == kindCommand {
			full := c.kingpinFullCommand(cl)
			if full != cl.fullName() {
				c.issue(p.position(call), "%s.FullCommand() is %q in this kingpin, never equal to what Parse returns (%q); kept as is, which is probably a bug", render(sel.X), full, cl.fullName())
			}
			cur.Replace(&dst.BasicLit{Kind: token.STRING, Value: strconv.Quote(full)})
			return true
		}
	}
	osArgs := &dst.SliceExpr{X: &dst.Ident{Name: "Args", Path: "os"}, Low: &dst.BasicLit{Kind: token.INT, Value: "1"}}
	switch name := c.runtimeCall(p, call); name {
	case "New":
		var name, help dst.Expr
		if len(call.Args) >= 1 {
			name = call.Args[0]
		}
		if len(call.Args) >= 2 {
			help = call.Args[1]
		}
		args := c.parserArgs(name, help)
		cur.Replace(c.mustCall(args))
		return true
	case "Parse":
		if sel == nil {
			// kingpin.Parse() on the package-level application.
			parser := c.parserExpr()
			if name, ok := c.main.parsers[call]; ok {
				parser = dst.NewIdent(name)
			}
			cur.Replace(callOf(c.compatIdent("MustParse"), parser, osArgs, c.usageExpr()))
			return true
		}
		if len(call.Args) == 1 {
			cur.Replace(callOf(c.compatIdent("Parse"), sel.X, call.Args[0], c.usageExpr()))
			return true
		}
	case "MustParse":
		if len(call.Args) == 1 {
			if inner, ok := call.Args[0].(*dst.CallExpr); ok {
				if isel, ok := inner.Fun.(*dst.SelectorExpr); ok && c.runtimeCall(p, inner) == "Parse" && len(inner.Args) == 1 {
					cur.Replace(callOf(c.compatIdent("MustParse"), isel.X, inner.Args[0], c.usageExpr()))
					return true
				}
			}
		}
	case "Version", "UsageTemplate", "Author", "DefaultEnvars", "Terminate", "Writers", "Writer", "UsageWriter", "ErrorWriter":
		if e, ok := c.settingsChain(p, call); ok {
			cur.Replace(e)
			return true
		}
	case "Usage":
		if sel != nil && len(call.Args) == 1 {
			cur.Replace(callOf(c.compatIdent("PrintUsage"), sel.X, call.Args[0], c.usageExpr()))
			return true
		}
		if sel == nil && len(call.Args) == 0 {
			// kingpin.Usage() prints the default application's usage for
			// the program's own command line.
			cur.Replace(callOf(c.compatIdent("PrintUsage"), c.parserExpr(), osArgs, c.usageExpr()))
			return true
		}
	case "Fatalf", "Errorf", "FatalIfError":
		// kingpin.Fatalf and friends print through the default application.
		// When the program doesn't otherwise use it, kongcompat's functions
		// do the same.
		// Method calls are handled with their selector, by adaptErrors.
		if sel == nil && c.g.in.cmdLine == nil {
			call.Fun = c.compatIdent(name)
			return true
		}
		return false
	case "":
		return false
	case "Action":
		// The application's own Action, chained onto kingpin.New, is now
		// the CLI's AfterApply hook.
		if c.genActions[c.tree.Root] != nil {
			if e, ok := c.settingsChain(p, call); ok {
				cur.Replace(e)
				return true
			}
		}
		return false
	case "Validate", "PreAction", "Interspersed", "OptionalSubcommands", "UsageFuncs", "UsageContext":
		// Reported with the grammar.
		return false
	default:
		if c.g.in.grammarCalls[call] {
			// Grammar left in place is removed with its statement.
			return false
		}
		c.issue(p.position(call), "kingpin %s has no automatic kong translation", name)
	}
	return false
}

// clauseOf returns the command a variable held, if any.
func (c *converter) clauseOf(p *Package, e dst.Expr) (*clause, bool) {
	o := p.objectOf(e)
	if o == nil {
		return nil, false
	}
	v, ok := c.g.in.globals[asVar(o)]
	if !ok {
		v, ok = c.g.in.varVals[o]
	}
	if !ok {
		return nil, false
	}
	cl, ok := v.(vClause)
	if !ok {
		return nil, false
	}
	return cl.c, true
}

func asVar(o types.Object) *types.Var {
	v, _ := o.(*types.Var)
	return v
}

// parserExpr is the expression of the kong parser for a program that used
// kingpin's package-level application.
func (c *converter) parserExpr() dst.Expr {
	for v, val := range c.g.in.globals {
		if cl, ok := val.(vClause); ok && cl.c.kind == kindApp {
			return dst.NewIdent(v.Name())
		}
	}
	return c.mustCall(c.parserArgs(nil, nil))
}

// parserArgs returns the kong.Must arguments for the application kingpin.New
// created with name and help, both nil for the package-level application.
// Later assignments to the application's Name and Help win.
func (c *converter) parserArgs(name, help dst.Expr) []dst.Expr {
	mp := c.main
	if mp.name != nil {
		name = mp.name
	}
	if mp.help != nil {
		help = mp.help
	}
	args := []dst.Expr{&dst.UnaryExpr{Op: token.AND, X: dst.NewIdent(c.cliVar)}}
	if name != nil {
		args = append(args, callOf(kongIdent("Name"), dst.Clone(name).(dst.Expr)))
	}
	if help != nil {
		args = append(args, callOf(kongIdent("Description"), dst.Clone(help).(dst.Expr)))
	}
	args = append(args, c.compatOptions()...)
	if c.conditionalName != "" {
		args = append(args, callOf(dst.NewIdent(c.conditionalName)))
	}
	if c.commandVars {
		args = append(args, callOf(c.compatIdent("CommandVars")))
	}
	var binds []dst.Expr
	var typs []types.Type
	for _, b := range mp.bound {
		binds, typs = append(binds, dst.NewIdent(b.v.Name())), append(typs, b.as)
	}
	args = append(args, c.bindCalls(c.tree.Root.cl.pos, binds, typs, c.tree.RootPkg.PkgPath, kongOption)...)
	if mp.exit != nil {
		args = append(args, callOf(kongIdent("Exit"), exitFunc(dst.Clone(mp.exit).(dst.Expr))))
	}
	if mp.stdout != nil || mp.stderr != nil {
		// kingpin v2 printed usage on standard error unless told otherwise.
		stdout, stderr := mp.stdout, mp.stderr
		if stdout == nil {
			stdout = &dst.Ident{Name: "Stdout", Path: "os"}
			if !c.prog.kingpinV3 {
				stdout = &dst.Ident{Name: "Stderr", Path: "os"}
			}
		}
		if stderr == nil {
			stderr = &dst.Ident{Name: "Stderr", Path: "os"}
		}
		args = append(args, callOf(kongIdent("Writers"), dst.Clone(stdout).(dst.Expr), dst.Clone(stderr).(dst.Expr)))
	}
	var vars []dst.Expr
	for _, v := range []struct {
		key  string
		expr dst.Expr
	}{{"version", mp.version}, {"author", mp.author}} {
		if v.expr != nil {
			vars = append(vars, &dst.KeyValueExpr{Key: &dst.BasicLit{Kind: token.STRING, Value: strconv.Quote(v.key)}, Value: dst.Clone(v.expr).(dst.Expr)})
		}
	}
	if len(vars) > 0 {
		args = append(args, &dst.CompositeLit{Type: kongIdent("Vars"), Elts: onePerLine(vars)})
	}
	if len(mp.vars) > 0 {
		// Values computed at run time, which kong must not interpolate.
		args = append(args, &dst.CompositeLit{Type: c.compatIdent("Vars"), Elts: onePerLine(varsElts(mp.vars))})
	}
	return args
}

// onePerLine puts the elements of a composite literal on their own lines
// when there are several.
func onePerLine(elts []dst.Expr) []dst.Expr {
	if len(elts) > 1 {
		for _, e := range elts {
			e.Decorations().Before = dst.NewLine
		}
		elts[len(elts)-1].Decorations().After = dst.NewLine
	}
	return elts
}

// needsValues reports whether a flag or argument needs kongcompat.Values
// because kingpin decoded it differently from kong.
// That covers ints and []int (kingpin parsed them as floats), []bool,
// map[string]string, counters, files, existing files and directories,
// positional booleans, and kingpin v3 booleans named no-x.
func (c *converter) needsValues() bool {
	for _, b := range c.plan.bindings {
		if b.tag != nil {
			switch b.tag.Get("type") {
			case "existingfile", "existingdir", "noflag", "boolarg", "counter":
				return true
			}
		}
		var t types.Type
		switch {
		case b.storage:
			t = b.cl.storageType
		case b.field != nil:
			t = b.field.Type()
		case b.dstVar != nil:
			t = b.dstVar.Type()
		}
		if t == nil {
			continue
		}
		if types.TypeString(t, nil) == "*os.File" {
			return true
		}
		switch u := t.Underlying().(type) {
		case *types.Slice:
			if basic, ok := u.Elem().(*types.Basic); ok && (basic.Kind() == types.Int || basic.Kind() == types.Bool) {
				return true
			}
		case *types.Map:
			if types.Identical(u, types.NewMap(types.Typ[types.String], types.Typ[types.String])) {
				return true
			}
		case *types.Basic:
			if u.Kind() == types.Int {
				return true
			}
		}
	}
	return false
}

// compatOptions returns the kongcompat options that keep kingpin's help flag,
// output streams and value parsing.
func (c *converter) compatOptions() []dst.Expr {
	var opts []dst.Expr
	switch short := c.helpShort(); short {
	case 'h':
		// kong's help flag already has -h.
	case 0:
		opts = append(opts, callOf(c.compatIdent("NoHelpShortFlag")))
	default:
		opts = append(opts, callOf(c.compatIdent("HelpShortFlag"), &dst.BasicLit{Kind: token.CHAR, Value: strconv.QuoteRune(short)}))
	}
	switch {
	case c.prog.kingpinV3:
	case c.prog.kingpinV2Legacy:
		opts = append(opts, callOf(c.compatIdent("UsageToStderr")))
	default:
		opts = append(opts, callOf(c.compatIdent("UsageToStderr")), callOf(c.compatIdent("Durations")))
	}
	if c.needsValues() {
		opts = append(opts, callOf(c.compatIdent("Values")))
	}
	if c.helpHidden() {
		opts = append(opts, callOf(c.compatIdent("HideHelp")))
	}

	help := &clause{kind: kindFlag, parent: c.tree.Root.cl, name: "help"}
	if env := defaultEnvar(help); env != "" {
		opts = append(opts, callOf(c.compatIdent("HelpEnvar"), &dst.BasicLit{Kind: token.STRING, Value: strconv.Quote(env)}))
	}
	return opts
}

func (c *converter) helpHidden() bool {
	app := c.tree.Root.cl
	return app.helpFlag != nil && app.helpFlag.hidden
}

// helpShort is the short form of kingpin's help flag, 0 for none.
func (c *converter) helpShort() rune {
	if app := c.tree.Root.cl; app.helpFlag != nil {
		return app.helpFlag.short
	}
	return 0
}

// kingpinFullCommand returns what kingpin's FullCommand returned for a
// command: its path, plus the argument placeholders of every level in kingpin
// v3 and its forks.
func (c *converter) kingpinFullCommand(cl *clause) string {
	if !c.prog.kingpinV3 {
		return cl.fullName()
	}
	var out []string
	for cur := cl; cur != nil && cur.kind == kindCommand; cur = cur.parent {
		part := []string{cur.name}
		for _, a := range cur.args() {
			text := "<" + a.name + ">"
			if a.isCumulative() {
				text += " ..."
			}
			if !a.required {
				text = "[" + text + "]"
			}
			part = append(part, text)
		}
		out = append(part, out...)
	}
	return strings.Join(out, " ")
}

// compatTemplate maps kingpin's template variables to kongcompat's.
func (c *converter) compatTemplate(e dst.Expr) dst.Expr {
	if id, ok := e.(*dst.Ident); ok && id.Path != "" && c.prog.isKingpinPath(id.Path) {
		return c.compatIdent(id.Name)
	}
	return e
}

// dropReplacedLocals removes the declarations of local variables replaced by
// fields of the CLI.
// pointInto makes the locals s gives a command struct point to that struct
// in the CLI: a := &A{label: 7} becomes cli.A = A{label: 7} and
// a := &cli.A, and other shapes, such as a, n := pair(), are followed by
// cli.A = *a and a = &cli.A.
// It returns false when s binds none of them.
func (c *converter) pointInto(p *Package, s dst.Stmt, vals []value) ([]dst.Stmt, bool) {
	var names, values []dst.Expr
	declares := true
	switch s := s.(type) {
	case *dst.AssignStmt:
		names, values, declares = s.Lhs, s.Rhs, s.Tok == token.DEFINE
	case *dst.DeclStmt:
		gd, ok := s.Decl.(*dst.GenDecl)
		if !ok || gd.Tok != token.VAR || len(gd.Specs) != 1 {
			return nil, false
		}
		vs := gd.Specs[0].(*dst.ValueSpec)
		names, values = identExprs(vs.Names), vs.Values
	default:
		return nil, false
	}
	// Where the local gets a struct kingpin was given, possibly one of
	// several over time.
	var bound []int
	paths := map[int]string{}
	for i, n := range names {
		if _, ok := c.main.pointers[p.objectOf(n)]; !ok || len(vals) != len(names) {
			continue
		}
		if vp, ok := vals[i].(vPtr); ok {
			if path, ok := c.instancePath(vp.o); ok {
				bound = append(bound, i)
				paths[i] = path
			}
		}
	}
	if len(bound) == 0 {
		return nil, false
	}
	pointTo := func(i int) dst.Expr {
		return &dst.UnaryExpr{Op: token.AND, X: pathExpr(paths[i])}
	}
	setValue := func(i int, v dst.Expr) {
		switch s := s.(type) {
		case *dst.AssignStmt:
			s.Rhs[i] = v
		case *dst.DeclStmt:
			s.Decl.(*dst.GenDecl).Specs[0].(*dst.ValueSpec).Values[i] = v
		}
	}
	if len(names) == 1 && len(values) == 1 && declares && builtInPlace(p, values[0]) {
		var out []dst.Stmt
		if lit := initialValues(values[0]); lit != nil {
			path := paths[0]
			out = append(out, &dst.AssignStmt{Lhs: []dst.Expr{pathExpr(path)}, Tok: token.ASSIGN, Rhs: []dst.Expr{c.keepSubcommands(lit, path, pathExpr(path))}})
			out[0].Decorations().Before, out[0].Decorations().Start = s.Decorations().Before, s.Decorations().Start
			s.Decorations().Before, s.Decorations().Start = dst.NewLine, nil
		}
		setValue(0, pointTo(0))
		return append(out, s), true
	}
	out := []dst.Stmt{s}
	for _, i := range bound {
		if len(names) == len(values) {
			if call, ok := unparen(values[i]).(*dst.CallExpr); ok && c.removedCtors[c.g.in.staticCallee(p, call)] {
				// The constructor is gone, and the CLI holds what it built.
				setValue(i, pointTo(i))
				continue
			}
		}
		path := paths[i]
		local := dst.NewIdent(render(names[i]))
		if n := c.nodeAt(path); n != nil && n.containerOwn {
			for _, ch := range n.children {
				out = append(out, &dst.AssignStmt{Lhs: []dst.Expr{selectPath(dst.Clone(local).(dst.Expr), ch.field)}, Tok: token.ASSIGN, Rhs: []dst.Expr{selectPath(pathExpr(path), ch.field)}})
			}
		}
		out = append(out,
			&dst.AssignStmt{Lhs: []dst.Expr{pathExpr(path)}, Tok: token.ASSIGN, Rhs: []dst.Expr{&dst.StarExpr{X: dst.Clone(local).(dst.Expr)}}},
			&dst.AssignStmt{Lhs: []dst.Expr{dst.Clone(local).(dst.Expr)}, Tok: token.ASSIGN, Rhs: []dst.Expr{pointTo(i)}})
	}
	return out, true
}

// spliceMarker marks a block made only to carry statements into the list
// around it.
const spliceMarker = "// king2kong:splice"

func (c *converter) dropReplacedLocals(p *Package, body *dst.BlockStmt) bool {
	changed := false
	branches := labelJumps(body, func(*dst.BranchStmt) bool { return true })
	// lower returns what replaces a statement defining variables the CLI
	// replaces, and false for any other statement.
	lower := func(s dst.Stmt) ([]dst.Stmt, bool) {
		if _, decl := s.(*dst.DeclStmt); !decl {
			if stmts, ok := c.pointInto(p, s, c.g.in.stmtVals[s]); ok {
				return stmts, true
			}
		}
		var out []dst.Stmt
		switch s := s.(type) {
		case *dst.DeclStmt:
			gd, ok := s.Decl.(*dst.GenDecl)
			if !ok || gd.Tok != token.VAR || !slices.ContainsFunc(gd.Specs, func(sp dst.Spec) bool {
				names := identExprs(sp.(*dst.ValueSpec).Names)
				return c.replacesAny(p, names) || slices.ContainsFunc(names, func(n dst.Expr) bool { _, ok := c.main.pointers[p.objectOf(n)]; return ok })
			}) {
				return nil, false
			}
			// One statement per spec, so each can be split, with the
			// values the interpreter saw for it.
			vals, offset := c.g.in.stmtVals[s], 0
			for _, sp := range gd.Specs {
				vs := sp.(*dst.ValueSpec)
				decl := &dst.DeclStmt{Decl: &dst.GenDecl{Tok: token.VAR, Specs: []dst.Spec{vs}}}
				n := len(vs.Values)
				if n == 1 && len(vs.Names) > 1 {
					n = len(vs.Names)
				}
				var specVals []value
				if offset+n <= len(vals) {
					specVals = vals[offset : offset+n]
				}
				offset += n
				if stmts, ok := c.pointInto(p, decl, specVals); ok {
					out = append(out, stmts...)
					continue
				}
				if c.declaresOnlyReplaced(p, decl) {
					out = append(out, c.initialAssignments(p, decl, identExprs(vs.Names), vs.Values)...)
					continue
				}
				after := c.splitCommands(p, decl, identExprs(vs.Names), vs.Values)
				out = append(append(out, decl), after...)
			}
			if len(out) > 0 {
				out[0].Decorations().Before, out[0].Decorations().Start = s.Decs.Before, s.Decs.Start
				out[len(out)-1].Decorations().After = s.Decs.After
			}
		case *dst.AssignStmt:
			reassigns := c.rebinds(p, s)
			switch {
			case (s.Tok == token.DEFINE || reassigns) && c.definesOnlyReplaced(p, s):
				out = c.initialAssignments(p, s, s.Lhs, s.Rhs)
			case s.Tok == token.DEFINE && c.replacesAny(p, s.Lhs):
				after := c.splitCommands(p, s, s.Lhs, s.Rhs)
				out = append([]dst.Stmt{s}, after...)
			default:
				return nil, false
			}
		default:
			return nil, false
		}
		return out, true
	}
	// lowerInit does the same for the initializer of an if, switch or for
	// statement: what it becomes goes right before the statement, in a block
	// around both when it declares variables.
	lowerInit := func(init *dst.Stmt, s dst.Stmt) dst.Stmt {
		stmts, ok := lower(*init)
		if !ok {
			return s
		}
		changed = true
		*init = nil
		if len(stmts) == 0 {
			return s
		}
		block := &dst.BlockStmt{List: append(stmts, s)}
		if !slices.ContainsFunc(stmts, func(st dst.Stmt) bool {
			as, ok := st.(*dst.AssignStmt)
			return !ok || as.Tok == token.DEFINE
		}) {
			// Not a real block: walk splices its statements in.
			block.Decs.Start.Append(spliceMarker)
		}
		return block
	}
	var walk func(list []dst.Stmt) []dst.Stmt
	var nested func(s dst.Stmt) dst.Stmt
	nested = func(s dst.Stmt) dst.Stmt {
		switch s := s.(type) {
		case *dst.BlockStmt:
			s.List = walk(s.List)
		case *dst.LabeledStmt:
			if stmts, ok := lower(s.Stmt); ok {
				// The label goes on the first statement, so a jump runs
				// them all.
				changed = true
				if len(stmts) == 0 {
					stmts = []dst.Stmt{&dst.EmptyStmt{Implicit: true}}
				}
				s.Stmt = stmts[0]
				block := &dst.BlockStmt{List: append([]dst.Stmt{s}, stmts[1:]...)}
				block.Decs.Start.Append(spliceMarker)
				return block
			}
			_, wasBlock := s.Stmt.(*dst.BlockStmt)
			inner := nested(s.Stmt)
			b, ok := inner.(*dst.BlockStmt)
			if !ok || wasBlock {
				s.Stmt = inner
				break
			}
			// What was the initializer now comes first: a goto must still
			// run it, while break and continue need the label on the loop
			// or switch itself.
			jumps := branches[s.Label.Name]
			switch {
			case jumps[token.GOTO] && (jumps[token.BREAK] || jumps[token.CONTINUE]):
				c.issue(p.position(s), "label %s is the target of both goto and break or continue, and its statement's initializer moved before it; check the jumps by hand", s.Label.Name)
				fallthrough
			case !jumps[token.GOTO]:
				s.Stmt = b.List[len(b.List)-1]
				b.List[len(b.List)-1] = s
			case slices.Contains(b.Decs.Start, spliceMarker):
				s.Stmt = b.List[0]
				b.List[0] = s
			default:
				s.Stmt = b
				return s
			}
			return b
		case *dst.IfStmt:
			s.Body.List = walk(s.Body.List)
			if s.Else != nil {
				s.Else = nested(s.Else)
				if b, ok := s.Else.(*dst.BlockStmt); ok {
					b.Decs.Start = slices.DeleteFunc(b.Decs.Start, func(d string) bool { return d == spliceMarker })
				}
			}
			return lowerInit(&s.Init, s)
		case *dst.ForStmt:
			s.Body.List = walk(s.Body.List)
			return lowerInit(&s.Init, s)
		case *dst.RangeStmt:
			s.Body.List = walk(s.Body.List)
		case *dst.SwitchStmt:
			for _, cc := range s.Body.List {
				cc.(*dst.CaseClause).Body = walk(cc.(*dst.CaseClause).Body)
			}
			return lowerInit(&s.Init, s)
		case *dst.TypeSwitchStmt:
			for _, cc := range s.Body.List {
				cc.(*dst.CaseClause).Body = walk(cc.(*dst.CaseClause).Body)
			}
			return lowerInit(&s.Init, s)
		case *dst.SelectStmt:
			for _, cc := range s.Body.List {
				cc.(*dst.CommClause).Body = walk(cc.(*dst.CommClause).Body)
			}
		}
		return s
	}
	walk = func(list []dst.Stmt) []dst.Stmt {
		var out []dst.Stmt
		for _, s := range list {
			if stmts, ok := lower(s); ok {
				changed = true
				out = append(out, stmts...)
				continue
			}
			s = nested(s)
			if b, ok := s.(*dst.BlockStmt); ok && slices.Contains(b.Decs.Start, spliceMarker) {
				out = append(out, b.List...)
				continue
			}
			out = append(out, s)
		}
		return out
	}
	body.List = walk(body.List)
	return changed
}

// declaresOnlyReplaced reports whether s only declares variables the CLI
// replaces.
// In a mixed declaration such as var name, other string, it removes the
// replaced names that have no initializer.
func (c *converter) declaresOnlyReplaced(p *Package, s *dst.DeclStmt) bool {
	gd, ok := s.Decl.(*dst.GenDecl)
	if !ok || gd.Tok != token.VAR {
		return false
	}
	var specs []dst.Spec
	for _, sp := range gd.Specs {
		vs := sp.(*dst.ValueSpec)
		if len(vs.Values) > 0 {
			specs = append(specs, sp)
			continue
		}
		var names []*dst.Ident
		for _, nm := range vs.Names {
			if _, ok := c.main.repl[p.objectOf(nm)]; !ok {
				names = append(names, nm)
			}
		}
		if len(names) > 0 {
			vs.Names = names
			specs = append(specs, vs)
		}
	}
	gd.Specs = specs
	for _, sp := range gd.Specs {
		for _, nm := range sp.(*dst.ValueSpec).Names {
			if _, ok := c.main.repl[p.objectOf(nm)]; !ok {
				return false
			}
		}
	}
	return true
}

// initialAssignments gives the CLI fields that replace the locals s defines
// the values s built them with, as in cli.Ls = lsCmd{out: os.Stdout}.
// Several of them make one assignment, which still computes every value
// before storing any.
func (c *converter) initialAssignments(p *Package, s dst.Stmt, names, values []dst.Expr) []dst.Stmt {
	as := &dst.AssignStmt{Tok: token.ASSIGN}
	for i, l := range names {
		r, replaced := c.main.repl[p.objectOf(l)]
		if i >= len(values) || !replaced {
			continue
		}
		lit := initialValues(values[i])
		if lit == nil && !c.declares(p, l) {
			// A command built anew over one built before starts empty.
			lit = emptyLiteral(values[i])
		}
		if lit == nil {
			continue
		}
		as.Lhs = append(as.Lhs, pathExpr(r.path))
		as.Rhs = append(as.Rhs, c.keepSubcommands(lit, r.path, pathExpr(r.path)))
	}
	if len(as.Lhs) == 0 {
		return nil
	}
	as.Decs.Before, as.Decs.Start, as.Decs.After = s.Decorations().Before, s.Decorations().Start, s.Decorations().After
	return []dst.Stmt{as}
}

// keepSubcommands adds the subcommands of the command at path to its
// literal, as in Parent{label: "x", Child: cli.A.Child}, so assigning it
// keeps what they hold.
func (c *converter) keepSubcommands(lit *dst.CompositeLit, path string, from dst.Expr) *dst.CompositeLit {
	n := c.nodeAt(path)
	if n == nil || !n.containerOwn || len(lit.Elts) > 0 && !isKeyed(lit) {
		return lit
	}
	for _, ch := range n.children {
		lit.Elts = append(lit.Elts, &dst.KeyValueExpr{Key: dst.NewIdent(ch.field), Value: selectPath(dst.Clone(from).(dst.Expr), ch.field)})
	}
	return lit
}

// emptyLiteral returns T{} for e building an empty T, as &T{} or new(T) do,
// or nil.
func emptyLiteral(e dst.Expr) *dst.CompositeLit {
	if lit := structLiteral(e); lit != nil && len(lit.Elts) == 0 {
		return &dst.CompositeLit{Type: dst.Clone(lit.Type).(dst.Expr)}
	}
	if call, ok := unparen(e).(*dst.CallExpr); ok && len(call.Args) == 1 {
		if id, ok := call.Fun.(*dst.Ident); ok && id.Name == "new" && id.Path == "" {
			return &dst.CompositeLit{Type: dst.Clone(call.Args[0]).(dst.Expr)}
		}
	}
	return nil
}

func isKeyed(lit *dst.CompositeLit) bool {
	return !slices.ContainsFunc(lit.Elts, func(e dst.Expr) bool { _, ok := e.(*dst.KeyValueExpr); return !ok })
}

// splitCommands takes the command structs the CLI replaces out of a
// definition that also defines other variables, as in
// a, count := &A{value: "keep"}, 7.
// The definition stays, so everything is computed in the same order, and
// an assignment after it hands the struct to the CLI: cli.A = *a.
// It returns the statements going after the definition.
func (c *converter) splitCommands(p *Package, s dst.Stmt, names, values []dst.Expr) []dst.Stmt {
	var after []dst.Stmt
	var ns, vs []dst.Expr
	tuple := len(names) != len(values)
	for i, l := range names {
		r, replaced := c.main.repl[p.objectOf(l)]
		var value dst.Expr
		if !tuple {
			value = values[i]
		}
		rebuilt := !tuple && !c.declares(p, l) && emptyLiteral(value) != nil
		switch {
		case !replaced:
		case !tuple && initialValues(value) != nil || rebuilt:
			if lit := structLiteral(value); lit != nil {
				c.keepSubcommands(lit, r.path, pathExpr(r.path))
			}
			var from dst.Expr = dst.NewIdent(render(l))
			if isAddress(value) {
				from = &dst.StarExpr{X: from}
			}
			after = append(after, &dst.AssignStmt{Lhs: []dst.Expr{pathExpr(r.path)}, Tok: token.ASSIGN, Rhs: []dst.Expr{from}})
		case !tuple && !callsBesides(p, c.g.in, value):
			// An empty struct, or grammar such as app.Flag(...).Bool().
			continue
		default:
			c.issue(p.position(l), "%s starts with values that cannot be given to %s here; set them there by hand", render(l), r.path)
			l = dst.NewIdent("_")
		}
		ns, vs = append(ns, l), append(vs, value)
	}
	if tuple {
		vs = values
	}
	switch s := s.(type) {
	case *dst.AssignStmt:
		s.Lhs, s.Rhs = ns, vs
		// := needs a variable it declares.
		if !slices.ContainsFunc(ns, func(n dst.Expr) bool { return c.declares(p, n) }) {
			s.Tok = token.ASSIGN
		}
	case *dst.DeclStmt:
		spec := s.Decl.(*dst.GenDecl).Specs[0].(*dst.ValueSpec)
		spec.Names, spec.Values = nil, vs
		for _, n := range ns {
			spec.Names = append(spec.Names, n.(*dst.Ident))
		}
	}
	return after
}

// declares reports whether the identifier e declares its variable, rather
// than assigning one declared before, as count does in a, count := f() after
// count := 1.
func (c *converter) declares(p *Package, e dst.Expr) bool {
	id, ok := e.(*dst.Ident)
	if !ok || id.Name == "_" {
		return false
	}
	a, o := p.astOf(id), p.objectOf(id)
	return a != nil && o != nil && o.Pos() == a.Pos()
}

func (c *converter) replacesAny(p *Package, names []dst.Expr) bool {
	return slices.ContainsFunc(names, func(n dst.Expr) bool {
		_, ok := c.main.repl[p.objectOf(n)]
		return ok
	})
}

// nodeAt returns the command at a path from cli, such as cli.DB.Migrate or
// .DB.Migrate.
func (c *converter) nodeAt(path string) *node {
	n := c.tree.Root
	for f := range strings.SplitSeq(strings.TrimPrefix(strings.TrimPrefix(path, c.cliVar), "."), ".") {
		var next *node
		for _, ch := range n.children {
			if ch.field == f {
				next = ch
			}
		}
		if next == nil {
			return nil
		}
		n = next
	}
	return n
}

// definesOnlyReplaced reports whether s only defines locals the CLI replaces,
// computing nothing else than the initial values initialAssignments keeps.
func (c *converter) definesOnlyReplaced(p *Package, s *dst.AssignStmt) bool {
	for _, l := range s.Lhs {
		if _, ok := c.main.repl[p.objectOf(l)]; !ok {
			return false
		}
	}
	if len(s.Lhs) != len(s.Rhs) {
		return !hasCallBesides(p, c.g.in, s)
	}
	for _, r := range s.Rhs {
		if initialValues(r) == nil && callsBesides(p, c.g.in, r) {
			return false
		}
	}
	return true
}

// rebinds reports whether as gives variables the CLI replaces a value their
// CLI field can take over: a new command struct, as in a = &A{}, or the
// flag's own value, as in verbose = app.Flag(...).Bool().
func (c *converter) rebinds(p *Package, as *dst.AssignStmt) bool {
	if as.Tok != token.ASSIGN || len(as.Lhs) != len(as.Rhs) {
		return false
	}
	return !slices.ContainsFunc(as.Rhs, func(e dst.Expr) bool { return !builtInPlace(p, e) }) || c.storesFlagValues(as)
}

// storesFlagValues reports whether an assignment stores the values of flags,
// as in verbose = app.Flag(...).Bool().
func (c *converter) storesFlagValues(as *dst.AssignStmt) bool {
	vals := c.g.in.stmtVals[as]
	return len(vals) > 0 && len(vals) == len(as.Lhs) && !slices.ContainsFunc(vals, func(v value) bool {
		_, ok := v.(vStorage)
		return !ok
	})
}

// pathExpr builds a selector chain such as cli.DB.Migrate.
func pathExpr(path string) dst.Expr {
	first, rest, _ := strings.Cut(path, ".")
	return selectPath(dst.NewIdent(first), rest)
}

// selectPath selects the fields of path, such as Compute.Build, from x.
func selectPath(x dst.Expr, path string) dst.Expr {
	for name := range strings.SplitSeq(strings.TrimPrefix(path, "."), ".") {
		if name != "" {
			x = &dst.SelectorExpr{X: x, Sel: dst.NewIdent(name)}
		}
	}
	return x
}

// localCLI returns the function that should declare the cli variable.
// It returns nil when cli has to be package-level, for instance when the
// application or its values live in package-level variables, as in kingpin's
// examples.
func (c *converter) localCLI() *dst.FuncDecl {
	for v, val := range c.g.in.globals {
		if !c.g.in.structVars[v] || c.isCommandValue(val) {
			return nil
		}
	}
	root := c.tree.RootPkg
	var fn *dst.FuncDecl
	for _, f := range root.Syntax {
		for _, d := range f.Decls {
			fd, ok := d.(*dst.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			dst.Inspect(fd.Body, func(n dst.Node) bool {
				if call, ok := n.(*dst.CallExpr); ok && c.runtimeCall(root, call) == "New" {
					fn = fd
				}
				return true
			})
		}
	}
	if fn == nil {
		return nil
	}
	// Every variable the CLI takes the place of must be a local of that
	// function, or code elsewhere would refer to a cli it cannot see.
	n := root.astOf(fn)
	inside := func(o types.Object) bool {
		return o != nil && n != nil && o.Pos() >= n.Pos() && o.Pos() <= n.End()
	}
	for _, b := range c.plan.bindings {
		if b.dstVar != nil && b.inst == nil && !inside(b.dstVar) {
			return nil
		}
	}
	for o, v := range c.g.in.varVals {
		switch v.(type) {
		case vStorage, vPtr, vObj:
			if ov, ok := o.(*types.Var); ok && ov.Pkg() == root.Types && !ov.IsField() && !inside(o) && c.usedAsGrammar(o) {
				return nil
			}
		}
	}
	return fn
}

// usedAsGrammar reports whether a variable held a value the CLI replaces: a
// flag's storage, or a command struct.
func (c *converter) usedAsGrammar(o types.Object) bool {
	v := c.g.in.varVals[o]
	_, storage := v.(vStorage)
	return storage || c.isCommandValue(v)
}

func (c *converter) isCommandValue(v value) bool {
	switch v := v.(type) {
	case vPtr:
		return c.isCommandInstance(v.o)
	case vObj:
		return c.isCommandInstance(v.o)
	}
	return false
}

func (c *converter) isCommandInstance(o *object) bool {
	for _, n := range c.tree.nodes {
		if n.cl.instance == o {
			return true
		}
	}
	return false
}

// cliLiteral is the CLI composite literal cli starts with, or nil when it
// starts empty.
func (c *converter) cliLiteral() dst.Expr {
	if len(c.cliInits) == 0 {
		return nil
	}
	lit := &dst.CompositeLit{Type: dst.NewIdent(c.cliName)}
	for _, ci := range c.cliInits {
		lit.Elts = append(lit.Elts, &dst.KeyValueExpr{Key: dst.NewIdent(ci.field), Value: ci.expr(c.tree.RootPkg.PkgPath)})
	}
	return lit
}

// declareLocalCLI declares the cli variable at the top of the function that
// builds the parser.
func (c *converter) declareLocalCLI() {
	fd := c.main.local
	if fd == nil {
		return
	}
	spec := &dst.ValueSpec{Names: []*dst.Ident{dst.NewIdent(c.cliVar)}, Type: dst.NewIdent(c.cliName)}
	if lit := c.cliLiteral(); lit != nil {
		spec.Type, spec.Values = nil, []dst.Expr{lit}
	}
	decl := &dst.DeclStmt{Decl: &dst.GenDecl{Tok: token.VAR, Specs: []dst.Spec{spec}}}
	fd.Body.List = append([]dst.Stmt{decl}, fd.Body.List...)
	for _, p := range c.prog.Packages {
		for _, f := range p.Syntax {
			for _, d := range f.Decls {
				if d == fd {
					c.ed.touch(p, f)
				}
			}
		}
	}
}
