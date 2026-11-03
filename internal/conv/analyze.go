package conv

import (
	"fmt"
	"go/token"
	"go/types"
	"io"
	"slices"
	"sort"
	"strings"

	"github.com/dave/dst"
)

// Grammar is the kingpin grammar recovered from a program.
type Grammar struct {
	prog    *Program
	in      *interp
	Apps    []*clause
	roots   []*types.Func
	Issues  []string
	touched map[dst.Stmt]bool
}

// Analyze finds the functions that create a kingpin application and runs them
// symbolically to recover the grammar they build.
func Analyze(prog *Program) *Grammar {
	in := newInterp(prog)
	in.runPackageVars()
	roots := in.findRoots()
	for _, r := range roots {
		in.run(r)
	}
	g := &Grammar{prog: prog, in: in, Apps: in.apps, roots: roots, touched: in.touched}
	in.resolveInstances()
	return g
}

// appMakers are the kingpin package-level names that create an application,
// or declare grammar or settings on kingpin.CommandLine.
var appMakers = map[string]bool{
	"New": true, "Command": true, "Flag": true, "Arg": true, "Version": true,
	"UsageTemplate": true, "Author": true, "CommandLine": true, "HelpFlag": true, "VersionFlag": true,
}

// findRoots returns the outermost functions that get a kingpin application.
// A function that builds an application and returns it is not a root.
// Its callers are, since they go on to register commands on it.
func (in *interp) findRoots() []*types.Func {
	makers := map[*types.Func]bool{}
	creates := map[*types.Func]bool{}
	for fn, fd := range in.prog.funcs {
		if fd.decl.Body == nil {
			continue
		}
		dst.Inspect(fd.decl.Body, func(n dst.Node) bool {
			id, ok := n.(*dst.Ident)
			if !ok {
				return true
			}
			if id.Path != "" && in.prog.isKingpinPath(id.Path) && appMakers[id.Name] {
				makers[fn] = true
				creates[fn] = creates[fn] || id.Name == "New"
			}
			// Registering more on a package-level application or command
			// counts too.
			if v, ok := fd.pkg.objectOf(id).(*types.Var); ok {
				if _, isClause := in.globals[v].(vClause); isClause {
					makers[fn] = true
				}
			}
			return true
		})
	}
	returnsApp := func(fn *types.Func) bool {
		sig, ok := fn.Type().(*types.Signature)
		if !ok {
			return false
		}
		for i := 0; i < sig.Results().Len(); i++ {
			if t := sig.Results().At(i).Type(); in.prog.isKingpinType(t) {
				if n, ok := deref(t).(*types.Named); ok && n.Obj().Name() == "Application" {
					return true
				}
			}
		}
		return false
	}
	callers := map[*types.Func][]*types.Func{}
	for fn, fd := range in.prog.funcs {
		if fd.decl.Body == nil {
			continue
		}
		dst.Inspect(fd.decl.Body, func(n dst.Node) bool {
			if c, ok := n.(*dst.CallExpr); ok {
				if callee := in.staticCallee(fd.pkg, c); callee != nil {
					callers[callee] = append(callers[callee], fn)
				}
			}
			return true
		})
	}
	// runPackageVars already ran the functions package-level initializers
	// call, as in var verbose = flag().
	for _, p := range in.prog.Packages {
		for _, f := range p.Syntax {
			for _, d := range f.Decls {
				gd, ok := d.(*dst.GenDecl)
				if !ok || gd.Tok != token.VAR {
					continue
				}
				for _, sp := range gd.Specs {
					if !in.mentionsBuilder(p, sp) {
						continue
					}
					dst.Inspect(sp, func(n dst.Node) bool {
						if c, ok := n.(*dst.CallExpr); ok {
							if callee := in.staticCallee(p, c); callee != nil {
								callers[callee] = append(callers[callee], nil)
							}
						}
						return true
					})
				}
			}
		}
	}
	// A function registering on an application it did not create, such as
	// kingpin.CommandLine or a package-level one, is run from its callers
	// too, so the statements calling it are known to build grammar.
	seen := map[*types.Func]bool{}
	var roots []*types.Func
	var visit func(fn *types.Func)
	visit = func(fn *types.Func) {
		if seen[fn] {
			return
		}
		seen[fn] = true
		others := slices.DeleteFunc(slices.Clone(callers[fn]), func(c *types.Func) bool { return c == fn })
		if len(others) > 0 && !slices.ContainsFunc(others, func(c *types.Func) bool { return c != nil }) {
			return
		}
		if (returnsApp(fn) || makers[fn] && !creates[fn]) && len(others) > 0 {
			for _, c := range others {
				if c != nil {
					visit(c)
				}
			}
			return
		}
		roots = append(roots, fn)
	}
	var makerList []*types.Func
	for fn := range makers {
		makerList = append(makerList, fn)
	}
	sort.Slice(makerList, func(i, j int) bool { return makerList[i].FullName() < makerList[j].FullName() })
	for _, fn := range makerList {
		visit(fn)
	}
	return roots
}

// resolveInstances links each command to the struct that represents it.
// That is the outermost struct holding the command's clause through embedded
// fields or, failing that, the single struct holding all its flags.
func (in *interp) resolveInstances() {
	holders := map[*clause][]*object{}
	for _, o := range in.objects {
		for _, c := range in.embeddedClauses(o, 0, map[*object]bool{}) {
			holders[c] = append(holders[c], o)
		}
	}
	for c, objs := range holders {
		if c.kind != kindCommand {
			continue
		}
		// Skip holders embedded in another holder of the same clause.
		var top []*object
		for _, o := range objs {
			inner := false
			for _, other := range objs {
				if other != o && embeds(other, o, 0) {
					inner = true
					break
				}
			}
			if !inner {
				top = append(top, o)
			}
		}
		if len(top) == 1 {
			c.instance = top[0]
		} else if len(top) > 1 {
			c.issue(fmt.Sprintf("command is held by %d different structs", len(top)))
		}
	}
	for _, c := range in.clauses {
		if c.kind != kindCommand || c.instance != nil {
			continue
		}
		owners := map[*object]int{}
		for _, f := range c.children {
			if o, _, ok := fieldOf(f.dst); ok {
				owners[o.root()]++
			}
		}
		if len(owners) == 1 {
			for o := range owners {
				c.instance = o
			}
		}
	}
}

// embeddedClauses returns the commands stored in o's fields or in the structs
// it embeds.
// That is how a struct embedding a CmdClause acts as a command.
func (in *interp) embeddedClauses(o *object, depth int, seen map[*object]bool) []*clause {
	if o == nil || o.st == nil || depth > 6 || seen[o] {
		return nil
	}
	seen[o] = true
	var out []*clause
	for i := 0; i < o.st.NumFields(); i++ {
		f := o.st.Field(i)
		v, ok := o.fields[f]
		if !ok {
			continue
		}
		switch v := v.(type) {
		case vClause:
			if v.c.kind == kindCommand {
				out = append(out, v.c)
			}
		case vObj:
			if f.Anonymous() {
				out = append(out, in.embeddedClauses(v.o, depth+1, seen)...)
			}
		case vPtr:
			if f.Anonymous() {
				out = append(out, in.embeddedClauses(v.o, depth+1, seen)...)
			}
		}
	}
	return out
}

// embeds reports whether outer contains inner through embedded fields.
func embeds(outer, inner *object, depth int) bool {
	if outer == nil || outer.st == nil || depth > 6 {
		return false
	}
	for i := 0; i < outer.st.NumFields(); i++ {
		f := outer.st.Field(i)
		if !f.Anonymous() {
			continue
		}
		var o *object
		switch v := outer.fields[f].(type) {
		case vObj:
			o = v.o
		case vPtr:
			o = v.o
		}
		if o == nil {
			continue
		}
		if o == inner || embeds(o, inner, depth+1) {
			return true
		}
	}
	return false
}

// Dump writes a readable listing of the recovered grammar, for debugging.
func (g *Grammar) Dump(w io.Writer) {
	for _, r := range g.roots {
		fmt.Fprintf(w, "root: %s\n", r.FullName())
	}
	var walk func(c *clause, indent string)
	walk = func(c *clause, indent string) {
		inst := ""
		if c.instance != nil {
			inst = " instance=" + types.TypeString(c.instance.typ, shortQual)
		}
		extra := ""
		if len(c.conds) > 0 {
			extra += " [conditional]"
		}
		if c.hidden {
			extra += " hidden"
		}
		if len(c.aliases) > 0 {
			extra += " aliases=" + strings.Join(c.aliases, ",")
		}
		switch c.kind {
		case kindApp, kindCommand:
			fmt.Fprintf(w, "%s%s %q%s%s\n", indent, c.kind, c.name, inst, extra)
		default:
			fmt.Fprintf(w, "%s%s %q %s dst=%s%s%s\n", indent, c.kind, c.name, c.bind, describeDst(c.dst), extra, flagExtra(c))
		}
		for _, is := range c.issues {
			fmt.Fprintf(w, "%s  ! %s\n", indent, is)
		}
		for _, ch := range c.children {
			walk(ch, indent+"  ")
		}
	}
	for _, a := range g.Apps {
		walk(a, "")
	}
	for _, e := range g.in.escapes {
		fmt.Fprintf(w, "escape %s: %s\n", e.pos, e.what)
	}
}

func flagExtra(c *clause) string {
	var parts []string
	if c.short != 0 {
		parts = append(parts, "short="+string(c.short))
	}
	if c.required {
		parts = append(parts, "required")
	}
	if c.hasDefault {
		parts = append(parts, fmt.Sprintf("default=%q", c.defaults))
	}
	if c.action != nil {
		parts = append(parts, "action="+describeDst(c.action))
	}
	if len(c.enum) > 0 {
		parts = append(parts, "enum="+strings.Join(c.enum, ","))
	}
	if c.hasSep {
		parts = append(parts, "sep="+c.separator)
	}
	if len(parts) == 0 {
		return ""
	}
	return " " + strings.Join(parts, " ")
}

func shortQual(p *types.Package) string { return p.Name() }

func describeDst(v value) string {
	switch v := v.(type) {
	case vFieldPtr:
		var parts []string
		for _, f := range v.o.path() {
			parts = append(parts, f.Name())
		}
		parts = append(parts, v.f.Name())
		return fmt.Sprintf("&%s{%s}.%s", types.TypeString(v.o.root().typ, shortQual), v.o.root().site, strings.Join(parts, "."))
	case vVarPtr:
		return "&var " + v.v.Name()
	case vFunc:
		if v.fn == nil {
			return "func?"
		}
		return "method " + v.fn.Name() + " on " + describeDst(v.recv)
	case vStorage:
		return "new value"
	case vPtr:
		return "ptr " + types.TypeString(v.o.typ, shortQual)
	case vObj:
		return "obj " + types.TypeString(v.o.typ, shortQual)
	case nil:
		return "<none>"
	}
	return fmt.Sprintf("%T", v)
}
