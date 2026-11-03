package conv

import (
	"fmt"
	"go/types"
	"slices"
	"sort"
	"strings"

	"github.com/dave/dst"
)

// convertActions turns the Action callbacks of commands and of the
// application into AfterApply hooks.
// kingpin ran them from Parse once the command line was parsed and validated,
// which is when kong runs AfterApply.
// The callback loses its *kingpin.ParseContext parameter, so it must not use
// it.
func (c *converter) convertActions() {
	c.checkSharedTypes()
	elsewhere := c.usedElsewhere()
	// Commands sharing a struct share its AfterApply, which runs their
	// actions once.
	ran := map[*types.Named]bool{}
	for _, n := range c.tree.nodes {
		var calls []string
		for _, fn := range n.cl.actions {
			if call := c.convertAction(n, fn, elsewhere[fn.fn]); call != "" {
				calls = append(calls, call)
			}
		}
		if len(calls) == 0 {
			continue
		}
		// checkSharedTypes reports commands sharing a struct but not their
		// actions.
		if ran[n.typ] {
			continue
		}
		ran[n.typ] = true
		h := c.hookFor(n.typ)
		h.actions = append(h.actions, calls...)
		// The receiver must not hide what the calls name, as in run().
		if h.outer == nil {
			h.outer = map[string]bool{}
		}
		for _, call := range calls {
			if name, _, _ := strings.Cut(call, "("); !strings.HasPrefix(name, c.recvPlaceholder+".") {
				name, _, _ = strings.Cut(name, ".")
				h.outer[name] = true
			}
		}
	}
}

// convertAction prepares the Action fn of n for a hook, returning the call
// to add to the AfterApply of n's struct when it has one.
func (c *converter) convertAction(n *node, fn vFunc, elsewhere bool) string {
	cl := n.cl
	what := fmt.Sprintf("the Action of %s", describeClause(cl))
	fd, ok := c.prog.funcs[fn.fn]
	if !ok {
		c.issue(cl.pos, "%s, %s, is declared outside the converted packages", what, fn.fn.Name())
		return ""
	}
	sig := fn.fn.Type().(*types.Signature)
	if sig.Params().Len() != 1 || sig.Results().Len() != 1 || !c.prog.isKingpinType(sig.Params().At(0).Type()) {
		c.issue(cl.pos, "%s, %s, does not have kingpin's Action signature", what, fn.fn.Name())
		return ""
	}
	param := sig.Params().At(0)
	if c.usesObject(fd, param) {
		c.issue(fd.pkg.position(fd.decl), "%s, %s, reads its *kingpin.ParseContext; run it by hand after parsing", what, fn.fn.Name())
		return ""
	}
	if elsewhere {
		c.issue(fd.pkg.position(fd.decl), "%s, %s, is also used elsewhere with its kingpin signature", what, fn.fn.Name())
		return ""
	}

	call := ""
	if sig := fn.fn.Type().(*types.Signature); sig.Recv() != nil {
		if _, ptr := sig.Recv().Type().(*types.Pointer); !ptr {
			c.issue(fd.pkg.position(fd.decl), "%s, %s, has a value receiver, so kingpin ran it on a copy made when the command was registered, without the parsed flags; the hook runs it on the parsed command", what, fn.fn.Name())
		}
	}
	if fn.recv != nil {
		if !c.isObject(fn.recv, cl.instance) || n.typ == nil {
			c.issue(cl.pos, "%s is a method of another value than the command's own struct", what)
			return ""
		}
		call = fmt.Sprintf("%s.%s()", c.recvPlaceholder, fn.fn.Name())
	} else if n.typ != nil {
		// The hook goes in the package of the command's struct.
		if to := n.typ.Obj().Pkg(); fn.fn.Pkg() != to {
			if !fn.fn.Exported() || fn.fn.Pkg().Name() == "main" || !c.importsAllow(to, fn.fn.Pkg()) {
				c.issue(cl.pos, "%s, %s, is declared in package %s, which the package of %s cannot call; run it from an AfterApply method of %s by hand", what, fn.fn.Name(), fn.fn.Pkg().Name(), n.typ.Obj().Name(), n.typ.Obj().Name())
				return ""
			}
			c.imports.add(to.Path(), fn.fn.Pkg().Path())
		}
		call = c.funcRef(fn.fn, n.typ.Obj().Pkg(), c.hookFor(n.typ).imports) + "()"
	} else {
		// Commands without a struct and the application run their actions
		// from the hook convertTree writes on a generated struct or the CLI.
		if fd.pkg != c.tree.RootPkg {
			c.issue(cl.pos, "%s, %s, is declared in another package than the CLI; call it from an AfterApply method of the command by hand", what, fn.fn.Name())
			return ""
		}
		g := c.actionsOf(n)
		g.steps = append(g.steps, actionStep{call: fn.fn.Name() + "()"})
	}
	fd.decl.Type.Params.List = nil
	c.ed.touch(fd.pkg, fd.file)
	c.keep[fn.fn] = true
	return call
}

// checkSharedTypes reports command structs shared by several commands that
// register different flags or actions on them.
// A struct tag or a hook belongs to the type, so every command would get all
// of them.
func (c *converter) checkSharedTypes() {
	type shape struct {
		flags   string
		actions string
		node    *node
	}
	seen := map[*types.Named]shape{}
	// Where each flag writes, as a path of fields.
	dest := map[*clause]string{}
	for _, b := range c.plan.bindings {
		if b.field == nil {
			continue
		}
		var path []string
		for _, s := range b.path {
			path = append(path, s.f.Name())
		}
		dest[b.cl] = strings.Join(append(path, b.field.Name()), ".")
	}
	for _, n := range c.tree.nodes {
		if n.typ == nil {
			continue
		}
		var flags, args, actions []string
		for _, ch := range n.cl.children {
			switch ch.kind {
			case kindFlag:
				flags = append(flags, ch.name+" "+dest[ch])
			case kindArg:
				args = append(args, ch.name+" "+dest[ch])
			}
		}
		for _, a := range n.cl.actions {
			actions = append(actions, a.fn.FullName())
		}
		// Flags in any order, arguments in theirs.
		sort.Strings(flags)
		flags = append(flags, args...)
		s := shape{strings.Join(flags, ","), strings.Join(actions, ","), n}
		prev, ok := seen[n.typ]
		if !ok {
			seen[n.typ] = s
			continue
		}
		if prev.flags != s.flags {
			c.issue(n.cl.pos, "commands %q and %q share the struct %s but register different flags or arguments; kong would give both all of them", prev.node.cl.fullName(), n.cl.fullName(), n.typ.Obj().Name())
		}
		if prev.actions != s.actions {
			c.issue(n.cl.pos, "commands %q and %q share the struct %s but run different actions; its AfterApply would run them for both", prev.node.cl.fullName(), n.cl.fullName(), n.typ.Obj().Name())
		}
	}
}

// importsAllow reports whether from may import to without a cycle.
func (c *converter) importsAllow(from, to *types.Package) bool {
	if c.imports == nil {
		c.imports = newImportGraph(c.prog)
	}
	return c.imports.canAdd(from.Path(), to.Path())
}

// funcRef is how code in package pkg refers to the function fn.
func (c *converter) funcRef(fn *types.Func, pkg *types.Package, imports map[string]string) string {
	if fn.Pkg() == pkg {
		return fn.Name()
	}
	return importName(imports, fn.Pkg().Path(), fn.Pkg().Name()) + "." + fn.Name()
}

func (c *converter) isObject(v value, o *object) bool {
	switch v := v.(type) {
	case vPtr:
		return v.o == o
	case vObj:
		return v.o == o
	}
	return false
}

// usesObject reports whether the body of fd refers to o.
func (c *converter) usesObject(fd funcDecl, o types.Object) bool {
	for id, used := range fd.pkg.TypesInfo.Uses {
		if used == o && id.Pos() >= fd.pkg.astOf(fd.decl).Pos() && id.Pos() <= fd.pkg.astOf(fd.decl).End() {
			return true
		}
	}
	return false
}

// usedElsewhere reports, for each function passed to Action as in
// cmd.Action(run), whether the source also refers to it somewhere else.
// Those other uses still need the kingpin signature that the hook drops.
//
// Both sides are counted in the source, so code that never runs counts too,
// and a helper registering one function on several commands counts once.
func (c *converter) usedElsewhere() map[*types.Func]bool {
	registered := map[*types.Func]int{}
	for _, p := range c.prog.Packages {
		for _, f := range p.Syntax {
			dst.Inspect(f, func(node dst.Node) bool {
				call, ok := node.(*dst.CallExpr)
				if !ok || len(call.Args) != 1 {
					return true
				}
				if sel, ok := call.Fun.(*dst.SelectorExpr); ok && sel.Sel.Name == "Action" && c.prog.isKingpinType(p.typeOf(sel.X)) {
					if fn, ok := p.objectOf(unparen(call.Args[0])).(*types.Func); ok {
						registered[fn]++
					}
				}
				return true
			})
		}
	}
	uses := map[*types.Func]int{}
	for _, p := range c.prog.Packages {
		for _, used := range p.TypesInfo.Uses {
			if fn, ok := used.(*types.Func); ok && registered[fn] > 0 {
				uses[fn]++
			}
		}
	}
	out := map[*types.Func]bool{}
	for fn, n := range registered {
		out[fn] = uses[fn] != n
	}
	return out
}

// actionsOf returns the actions run by the generated AfterApply of n.
func (c *converter) actionsOf(n *node) *genActions {
	g := c.genActions[n]
	if g == nil {
		g = &genActions{}
		c.genActions[n] = g
	}
	return g
}

func describeClause(cl *clause) string {
	if cl.kind == kindApp {
		return "the application"
	}
	return fmt.Sprintf("command %q", cl.fullName())
}

// genActions lists the calls to former Action functions that a generated
// AfterApply makes, in registration order.
// It is used for the CLI and for commands without a struct of their own.
type genActions struct {
	steps []actionStep
}

// actionStep is a call such as run().
// moved is set when the function used to be a literal.
type actionStep struct {
	call  string
	moved bool
}

// convertActionLits moves the function literals given to Action into
// functions of their own, which the hooks call.
// The rest of the conversion then handles their bodies like any other code.
func (c *converter) convertActionLits() {
	moved := map[dst.Decl]bool{}
	// A literal in a helper registering several commands becomes one
	// function they all call.
	names := map[*dst.FuncLit]string{}
	uses := map[*dst.FuncLit]int{}
	for _, n := range c.tree.nodes {
		for _, al := range n.cl.actionLits {
			uses[al.lit]++
		}
	}
	for _, n := range c.tree.nodes {
		for _, al := range n.cl.actionLits {
			what := fmt.Sprintf("the Action of %s", describeClause(n.cl))
			if n.typ != nil || al.pkg != c.tree.RootPkg {
				c.issue(al.pkg.position(al.lit), "%s is a function literal; move its body to a method of the command and run it from AfterApply", what)
				continue
			}
			name := names[al.lit]
			if name == "" {
				if why := c.actionLitProblem(al); why != "" {
					c.issue(al.pkg.position(al.lit), "%s %s; run it by hand after parsing", what, why)
					continue
				}
				name = c.moveActionLit(n.cl, al, uses[al.lit] > 1, moved)
				names[al.lit] = name
			}
			g := c.actionsOf(n)
			// It goes after the named actions registered before it.
			at, named := 0, 0
			for at < len(g.steps) && (named < al.before || g.steps[at].moved) {
				if !g.steps[at].moved {
					named++
				}
				at++
			}
			g.steps = slices.Insert(g.steps, at, actionStep{call: name + "()", moved: true})
		}
	}
}

// actionLitProblem says why an Action function literal cannot become a
// function of its own, or returns "" if it can.
func (c *converter) actionLitProblem(al actionLit) string {
	p := al.pkg
	lit := al.lit
	params := lit.Type.Params.List
	if len(params) > 1 || lit.Type.Results == nil || len(lit.Type.Results.List) != 1 {
		return "does not have kingpin's Action signature"
	}
	n := p.astOf(lit)
	if f, _ := c.declOf(p, lit); n == nil || f == nil {
		return "cannot be located"
	}
	inside := func(o types.Object) bool { return o.Pos() >= n.Pos() && o.Pos() <= n.End() }
	var paramObj types.Object
	if len(params) == 1 && len(params[0].Names) == 1 {
		paramObj = p.objectOf(params[0].Names[0])
	}
	why := ""
	dst.Inspect(lit.Body, func(node dst.Node) bool {
		id, ok := node.(*dst.Ident)
		if !ok || why != "" {
			return why == ""
		}
		o := p.objectOf(id)
		switch {
		case c.main != nil && c.main.local != nil && o != nil && c.main.repl[o] != (replacement{}):
			why = "uses " + id.Name + ", which became a field of a cli local to " + c.main.local.Name.Name
		case o == nil || o.Pkg() == nil || o.Parent() == nil || o.Parent() == o.Pkg().Scope() || inside(o) && o != paramObj:
		case o == paramObj:
			why = "reads its *kingpin.ParseContext"
		default:
			why = "uses " + id.Name + " from its surroundings"
		}
		return true
	})
	return why
}

// declOf returns the file of p holding n, and the index of the top-level
// declaration that contains it.
func (c *converter) declOf(p *Package, n dst.Node) (*dst.File, int) {
	for _, f := range p.Syntax {
		if i := slices.IndexFunc(f.Decls, func(d dst.Decl) bool { return containsNode(d, n) }); i >= 0 {
			return f, i
		}
	}
	return nil, -1
}

// moveActionLit moves the body of an Action function literal into a new
// function and returns its name.
// The function goes right after the one registering it, and after the
// literals moved there before.
// A literal shared by several commands is named after the function that
// registers them.
//
// The emptied literal is removed later, along with the registration.
func (c *converter) moveActionLit(cl *clause, al actionLit, shared bool, moved map[dst.Decl]bool) string {
	p, lit := al.pkg, al.lit
	f, i := c.declOf(p, lit)
	anchor := f.Decls[i]
	base, doc := "app", "the command line"
	switch {
	case shared:
		base, doc = "shared", "one of the commands sharing it"
		if fd, ok := anchor.(*dst.FuncDecl); ok {
			base, doc = fd.Name.Name, "a command registered by "+fd.Name.Name
		}
	case cl.kind != kindApp:
		base = unexported(c.prog.identFromName(strings.ReplaceAll(cl.fullName(), " ", "-")))
		doc = fmt.Sprintf("the %q command", cl.fullName())
	}
	name := c.freeName(p, base+"Action")
	results := lit.Type.Results
	// Whatever the literal called it, the result is kingpin's error.
	results.List[0].Type = dst.NewIdent("error")
	fd := &dst.FuncDecl{
		Name: dst.NewIdent(name),
		Type: &dst.FuncType{Func: true, Params: &dst.FieldList{}, Results: results},
		Body: lit.Body,
	}
	fd.Decs.Start.Append(fmt.Sprintf("// %s runs once %s is parsed.", name, doc))
	lit.Type.Results, lit.Body = nil, &dst.BlockStmt{}
	for i+1 < len(f.Decls) && moved[f.Decls[i+1]] {
		i++
		anchor = f.Decls[i]
	}
	c.ed.insertAfter(p, f, anchor, []dst.Decl{fd})
	moved[fd] = true
	return name
}

// afterApplySource renders the AfterApply method of the generated type
// typeName, which runs a node's former Actions.
func afterApplySource(typeName string, g *genActions) string {
	var b strings.Builder
	b.WriteString(afterDoc(false, false, true))
	fmt.Fprintf(&b, "func (*%s) AfterApply() error {\n", typeName)
	var calls []string
	for _, st := range g.steps {
		calls = append(calls, st.call)
	}
	b.WriteString(runActions(calls))
	b.WriteString("}\n")
	return b.String()
}
