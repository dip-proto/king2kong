package conv

import (
	"fmt"
	"go/token"
	"go/types"
	"sort"
	"strings"

	"github.com/dave/dst"

	"king2kong/kongtag"
)

// node is a command in the kong grammar.
type node struct {
	cl       *clause
	parent   *node
	children []*node

	// typ is the command struct (nil for an application or for a command
	// registered without one).
	typ *types.Named

	// containerOwn is set when the subcommand fields go on typ itself.
	// Otherwise they go on a struct generated in the root package that
	// embeds typ.
	containerOwn bool

	// field is the name of this node's field in its parent's container.
	field string
	tag   *kongtag.Tag
}

// Tree is the kong grammar to generate.
type Tree struct {
	Root    *node
	RootPkg *Package
	nodes   []*node
}

// planTree turns the recovered commands into a kong grammar tree and decides
// where each command's subcommand fields go.
func (p *Plan) planTree() *Tree {
	if len(p.g.Apps) == 0 {
		p.issue(p.g.firstPos(), "found no kingpin application")
		return nil
	}
	if len(p.g.Apps) > 1 {
		// Several programs: each one is converted on its own, by naming
		// its packages.
		for _, a := range p.g.Apps {
			p.issue(a.pos, "one of %d kingpin applications; convert one program at a time by naming its packages, such as ./cmd/x/... ./pkg/...", len(p.g.Apps))
		}
		return nil
	}
	app := p.g.Apps[0]
	t := &Tree{}
	var build func(c *clause, parent *node) *node
	build = func(c *clause, parent *node) *node {
		n := &node{cl: c, parent: parent}
		if c.instance != nil {
			if named, ok := c.instance.typ.(*types.Named); ok {
				n.typ = named
			}
		}
		t.nodes = append(t.nodes, n)
		for _, ch := range c.commands() {
			n.children = append(n.children, build(ch, n))
		}
		return n
	}
	t.Root = build(app, nil)
	for _, n := range t.nodes {
		if n.typ != nil && hasSignature(n.typ) {
			p.issue(n.cl.pos, "%s: its struct %s has a Signature method, which kong reads as the command's tags; check it", describeClause(n.cl), n.typ.Obj().Name())
		}
	}
	t.RootPkg = p.rootPackage(t)
	if t.RootPkg == nil {
		p.issue(app.pos, "cannot tell which package registers the top-level commands")
		return nil
	}
	p.placeContainers(t)
	p.nameFields(t)
	return t
}

// rootPackage returns the package wiring the top-level commands, which is
// where the root grammar struct goes.
func (p *Plan) rootPackage(t *Tree) *Package {
	counts := map[*Package]int{}
	for _, ch := range t.Root.children {
		if s := p.g.outermostSite(ch.cl); s != nil {
			counts[s.pkg]++
		} else if ch.cl.instance == nil {
			// An inline command: wired where it is registered.
			if pkg := p.g.prog.packageAt(ch.cl.pos); pkg != nil {
				counts[pkg]++
			}
		}
	}
	if len(counts) == 0 {
		// No commands: the grammar goes where the command line is parsed, or
		// else where the application is built.
		// kingpin.CommandLine is built by whichever package registers a flag
		// first.
		if pkg := p.parsingPackage(); pkg != nil {
			return pkg
		}
		if pkg := p.g.prog.packageAt(t.Root.cl.pos); pkg != nil {
			counts[pkg] = 1
		}
	}
	var best *Package
	for pkg, n := range counts {
		if best == nil || n > counts[best] || n == counts[best] && pkg.PkgPath < best.PkgPath {
			best = pkg
		}
	}
	return best
}

// parsingPackage returns the only package that parses the command line with
// kingpin, if there is one.
func (p *Plan) parsingPackage() *Package {
	parsing := map[*Package]bool{}
	for _, pkg := range p.g.prog.Packages {
		for _, f := range pkg.Syntax {
			dst.Inspect(f, func(n dst.Node) bool {
				call, ok := n.(*dst.CallExpr)
				if !ok {
					return true
				}
				name := ""
				switch fun := call.Fun.(type) {
				case *dst.Ident:
					if fun.Path != "" && p.g.prog.isKingpinPath(fun.Path) {
						name = fun.Name
					}
				case *dst.SelectorExpr:
					if t, ok := deref(pkg.typeOf(fun.X)).(*types.Named); ok && p.g.prog.isKingpinType(t) && t.Obj().Name() == "Application" {
						name = fun.Sel.Name
					}
				}
				if name == "Parse" || name == "MustParse" {
					parsing[pkg] = true
				}
				return true
			})
		}
	}
	if len(parsing) != 1 {
		return nil
	}
	for pkg := range parsing {
		return pkg
	}
	return nil
}

// placeContainers decides, for every command with subcommands, whether the
// subcommand fields can be added to the command's own struct.
func (p *Plan) placeContainers(t *Tree) {
	uses := map[*types.Named][]*node{}
	for _, n := range t.nodes {
		if n.typ != nil {
			uses[n.typ] = append(uses[n.typ], n)
		}
	}
	embedded := p.embeddedTypes()
	imports := newImportGraph(p.g.prog)
	for _, n := range t.nodes {
		if n.parent == nil || len(n.children) == 0 {
			continue
		}
		own := p.canOwn(n, uses, embedded)
		if own {
			from := n.typ.Obj().Pkg().Path()
			for _, ch := range n.children {
				if ch.typ == nil || len(ch.children) > 0 && !p.canOwn(ch, uses, embedded) {
					continue
				}
				to := ch.typ.Obj().Pkg().Path()
				if !imports.canAdd(from, to) {
					own = false
					break
				}
			}
			if own {
				for _, ch := range n.children {
					if ch.typ != nil {
						imports.add(from, ch.typ.Obj().Pkg().Path())
					}
				}
			}
		}
		n.containerOwn = own
	}
}

// canOwn reports whether n's subcommand fields can go on its own struct.
// The struct must be the program's, used by this command only, embedded
// nowhere, and never built with an unkeyed literal, which a new field breaks.
func (p *Plan) canOwn(n *node, uses map[*types.Named][]*node, embedded map[*types.Named]bool) bool {
	return n.typ != nil && p.g.prog.owns(n.typ.Obj().Pkg()) && len(uses[n.typ]) == 1 && !embedded[n.typ] && !p.g.prog.unkeyedLiterals(n.typ)
}

// embeddedTypes returns the named types embedded in some struct of the
// program.
// Adding subcommand fields to one of them would also add them to every struct
// embedding it.
func (p *Plan) embeddedTypes() map[*types.Named]bool {
	out := map[*types.Named]bool{}
	for _, pkg := range p.g.prog.Packages {
		scope := pkg.Types.Scope()
		for _, name := range scope.Names() {
			tn, ok := scope.Lookup(name).(*types.TypeName)
			if !ok {
				continue
			}
			st := structOf(tn.Type())
			if st == nil {
				continue
			}
			for i := 0; i < st.NumFields(); i++ {
				f := st.Field(i)
				if !f.Anonymous() {
					continue
				}
				if n, ok := deref(f.Type()).(*types.Named); ok {
					out[n] = true
				}
			}
		}
	}
	return out
}

// nameFields picks each command's field name in its parent container and
// renders its tag.
func (p *Plan) nameFields(t *Tree) {
	for _, n := range t.nodes {
		// Methods the conversion may add to the struct.
		taken := map[string]bool{"Hints": true, "Vars": true, "BeforeApply": true, "AfterApply": true}
		if n.containerOwn && n.typ != nil {
			// An unexported field may get exported later, like config to
			// Config.
			for _, name := range memberNames(n.typ) {
				taken[name] = true
				taken[p.g.prog.exported(name)] = true
			}
		}
		for _, ch := range n.children {
			name := p.g.prog.identFromName(ch.cl.name)
			for taken[name] {
				name += "Cmd"
			}
			taken[name] = true
			ch.field = name
			ch.tag = p.commandTag(ch, name)
		}
	}
}

// memberNames lists the fields and methods of t and *t, including promoted
// ones.
func memberNames(t *types.Named) []string {
	seen := map[string]bool{}
	var out []string
	add := func(n string) {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	ms := types.NewMethodSet(types.NewPointer(t))
	for i := 0; i < ms.Len(); i++ {
		add(ms.At(i).Obj().Name())
	}
	var walk func(st *types.Struct, depth int)
	walk = func(st *types.Struct, depth int) {
		if st == nil || depth > 6 {
			return
		}
		for i := 0; i < st.NumFields(); i++ {
			f := st.Field(i)
			add(f.Name())
			if f.Anonymous() {
				walk(structOf(deref(f.Type())), depth+1)
			}
		}
	}
	walk(structOf(t), 0)
	sort.Strings(out)
	return out
}

func (p *Plan) commandTag(n *node, field string) *kongtag.Tag {
	c := n.cl
	t := new(kongtag.Tag).Flag("cmd")
	if kongName(field) != c.name {
		t.Set("name", c.name)
	}
	if len(c.aliases) > 0 {
		t.Set("aliases", strings.Join(c.aliases, ","))
	}
	if c.defaultCmd {
		// kingpin parsed whatever followed into the default command, which
		// kong only allows with "withargs".
		if len(c.args()) > 0 || len(n.children) > 0 {
			t.Set("default", "withargs")
		} else {
			t.Set("default", "1")
		}
	}
	if c.hidden {
		t.Flag("hidden")
	}
	p.helpTag(t, c)
	return t
}

// outermostSite returns the outermost call that returned the command's
// struct, which is where the command is wired into the tree.
func (g *Grammar) outermostSite(c *clause) *callSite {
	var best *callSite
	for _, s := range g.in.sites {
		if s.result != c.instance {
			continue
		}
		if best == nil || s.depth < best.depth {
			best = s
		}
	}
	return best
}

func (g *Grammar) firstPos() (pos token.Position) {
	if len(g.Apps) > 0 {
		return g.Apps[0].pos
	}
	return pos
}

// importGraph tracks package imports so new ones never create a cycle.
type importGraph struct {
	edges map[string]map[string]bool
}

func newImportGraph(prog *Program) *importGraph {
	g := &importGraph{edges: map[string]map[string]bool{}}
	seen := map[*types.Package]bool{}
	var visit func(p *types.Package)
	visit = func(p *types.Package) {
		if seen[p] {
			return
		}
		seen[p] = true
		for _, imp := range p.Imports() {
			g.add(p.Path(), imp.Path())
			visit(imp)
		}
	}
	for _, pkg := range prog.Packages {
		visit(pkg.Types)
	}
	return g
}

func (g *importGraph) add(from, to string) {
	if g.edges[from] == nil {
		g.edges[from] = map[string]bool{}
	}
	g.edges[from][to] = true
}

// canAdd reports whether from may import to without creating a cycle.
func (g *importGraph) canAdd(from, to string) bool {
	if from == to {
		return true
	}
	seen := map[string]bool{}
	var reaches func(a string) bool
	reaches = func(a string) bool {
		if a == from {
			return true
		}
		if seen[a] {
			return false
		}
		seen[a] = true
		for b := range g.edges[a] {
			if reaches(b) {
				return true
			}
		}
		return false
	}
	return !reaches(to)
}

// Describe writes the planned grammar, for debugging.
func (t *Tree) Describe() string {
	var out string
	var walk func(n *node, indent string)
	walk = func(n *node, indent string) {
		where := ""
		if len(n.children) > 0 {
			if n.containerOwn {
				where = " (subcommands on the command struct)"
			} else {
				where = " (subcommands on a generated struct)"
			}
		}
		typ := "<none>"
		if n.typ != nil {
			typ = types.TypeString(n.typ, shortQual)
		}
		if n.parent == nil {
			out += fmt.Sprintf("%sroot in %s%s\n", indent, t.RootPkg.PkgPath, where)
		} else {
			out += fmt.Sprintf("%s%s %s `%s`%s\n", indent, n.field, typ, n.tag.String(), where)
		}
		for _, ch := range n.children {
			walk(ch, indent+"  ")
		}
	}
	walk(t.Root, "")
	return out
}
