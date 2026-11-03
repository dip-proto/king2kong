package conv

import (
	"fmt"
	"go/scanner"
	"go/token"
	"go/types"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/dave/dst"
	"github.com/dave/dst/dstutil"

	"king2kong/kongtag"
)

// convertTree adds the subcommand fields to the command structs, generates
// the structs that cannot live next to their command, and writes the root
// grammar.
func (c *converter) convertTree() {
	t := c.tree
	// A command whose child uses a generated struct needs one too: its own
	// package cannot import the root package where that struct lives.
	for changed := true; changed; {
		changed = false
		for _, n := range t.nodes {
			if n.parent == nil || !n.containerOwn {
				continue
			}
			for _, ch := range n.children {
				if len(ch.children) > 0 && !ch.containerOwn {
					n.containerOwn = false
					changed = true
				}
			}
		}
	}
	c.res.Stats.Commands = len(t.nodes) - 1

	root := t.RootPkg
	if app := t.Root.cl; app.defaultEnvars && (!app.nameOK || app.name == "") {
		c.issue(app.pos, "DefaultEnvars names environment variables after the application, whose name is not a constant; add env tags by hand")
	}
	c.cliName = c.freeName(root, "CLI", "KongCLI")
	if c.main != nil {
		// cli must not be shadowed where the program refers to it.
		c.reserveLocals(root)
	}
	c.cliVar = c.freeName(root, "cli", "kongCLI")
	var generated []string
	imports := map[string]string{"github.com/alecthomas/kong": "kong"}
	c.rootImports = imports
	paths := c.nodePaths()

	// Commands without a struct of their own get one in the root package when
	// they need fields: subcommands, or values declared with String() and
	// friends.
	names := map[*node]string{}
	storage := map[*node][]string{}
	for _, n := range t.nodes {
		if n.parent == nil {
			continue
		}
		if n.typ == nil {
			taken := map[string]bool{}
			for _, ch := range n.children {
				taken[ch.field] = true
			}
			storage[n] = c.storageFields(n, root.Types, imports, paths[n.cl], taken)
		} else if c.hasStorage(n) {
			c.issue(n.cl.pos, "command %q mixes a struct of its own with values declared by String() and friends", n.cl.fullName())
		}
		switch {
		case len(n.children) > 0 && !n.containerOwn:
			names[n] = c.freeName(root, c.containerName(n))
		case n.typ == nil && (len(storage[n]) > 0 || c.genActions[n] != nil || len(n.cl.actionLits) > 0):
			names[n] = c.freeName(root, unexported(c.prog.identFromName(strings.ReplaceAll(n.cl.fullName(), " ", "-")))+"Command")
		}
	}
	rootTaken := map[string]bool{"Help": true}
	for _, ch := range t.Root.children {
		rootTaken[ch.field] = true
	}
	// The struct holding the app's flags is embedded under its type name,
	// so no flag field may use that name, and no command should.
	commands := maps.Clone(rootTaken)
	for _, b := range c.appFlagBindings() {
		if b.holder == nil {
			continue
		}
		if named, ok := b.holder.typ.(*types.Named); ok {
			name := named.Obj().Name()
			if commands[name] {
				c.issue(b.cl.pos, "the CLI embeds %s, which holds the application's flags, and a command uses the same field name; rename one of them by hand", name)
				break
			}
			rootTaken[name] = true
		}
	}
	var pkgFlagLines []string
	if c.main != nil {
		groups := c.collectPkgFlags()
		c.pkgFlagsOf = map[*clause]bool{}
		for _, g := range groups {
			for _, pf := range g.flags {
				c.pkgFlagsOf[pf.b.cl] = true
			}
		}
		pkgFlagLines = c.convertPkgFlags(groups, imports, rootTaken)
	}
	rootStorage := c.storageFields(t.Root, root.Types, imports, c.cliVar, rootTaken)
	if c.main != nil {
		c.prepareMain(rootTaken)
	}
	c.convertActionLits()

	for _, n := range t.nodes {
		if len(n.children) == 0 && n.parent != nil && len(storage[n]) == 0 && names[n] == "" {
			continue
		}
		lineImports := imports
		if n.parent != nil && n.containerOwn {
			lineImports = map[string]string{}
		}
		var lines []string
		for _, ch := range n.children {
			lines = append(lines, fmt.Sprintf("%s %s %s", ch.field, c.nodeType(ch, names, n, lineImports), tagLiteral(ch.tag.String())))
		}
		switch {
		case n.parent == nil:
			// The root grammar, generated below.
		case n.containerOwn && n.typ != nil:
			c.addCmdFields(n, lines, lineImports)
		default:
			var b strings.Builder
			if len(n.children) > 0 {
				fmt.Fprintf(&b, "// %s is the %q command and its subcommands.\n", names[n], n.cl.fullName())
			} else {
				fmt.Fprintf(&b, "// %s is the %q command.\n", names[n], n.cl.fullName())
			}
			var all []string
			all = append(all, storage[n]...)
			if len(storage[n]) > 0 && len(lines) > 0 {
				all = append(all, "")
			}
			all = append(all, lines...)
			if n.typ == nil && len(all) == 0 {
				fmt.Fprintf(&b, "type %s struct{}\n", names[n])
			} else {
				fmt.Fprintf(&b, "type %s struct {\n", names[n])
				if n.typ != nil {
					fmt.Fprintf(&b, "%s\n\n", typeExpr(n.typ, root.Types, imports))
				}
				b.WriteString(strings.Join(all, "\n"))
				b.WriteString("\n}\n")
			}
			if g := c.genActions[n]; g != nil {
				b.WriteString("\n" + afterApplySource(names[n], g))
			}
			generated = append(generated, b.String())
		}
	}

	rootLines := []string{}
	flagLines := append(append([]string{}, rootStorage...), pkgFlagLines...)
	if embed := c.rootFlagsEmbed(imports); embed != "" {
		flagLines = append(flagLines, embed)
	}
	if c.main != nil {
		flagLines = append(flagLines, c.main.extraFields...)
		if c.main.version != nil || c.hasVersionCall() {
			// A command or flag may already be named Version.
			field := firstFree(func(n string) bool { return rootTaken[n] }, "Version", "VersionFlag")
			line := field + " kong.VersionFlag " + tagLiteral(versionTag(t.Root.cl, field).String())
			flagLines = append(flagLines, line)
			c.fieldOrder[line] = 2*t.Root.cl.versionAt + 1
		}
	}
	sort.SliceStable(flagLines, func(i, j int) bool {
		return c.fieldOrder[flagLines[i]] < c.fieldOrder[flagLines[j]]
	})
	rootLines = append(rootLines, flagLines...)
	if len(rootLines) > 0 {
		rootLines = append(rootLines, "")
	}
	if c.compatPath != "" && len(t.Root.children) > 0 && !c.rootHasCommand("help") {
		// kingpin gives every application with commands a help command, listed
		// first.
		name := importName(imports, c.compatPath, "kongcompat")
		rootLines = append(rootLines, fmt.Sprintf("Help %s.HelpCommand `cmd:\"\" help:\"Show help.\"`\n", name))
	}
	for _, ch := range t.Root.children {
		rootLines = append(rootLines, fmt.Sprintf("%s %s %s", ch.field, c.nodeType(ch, names, t.Root, imports), tagLiteral(ch.tag.String())))
	}
	cliName := c.cliName
	var src strings.Builder
	app := t.Root.cl
	desc := "the command line"
	if app.nameOK && app.name != "" {
		desc = fmt.Sprintf("the %s command line", app.name)
	}
	fmt.Fprintf(&src, "// %s is %s, declared as a kong grammar.\n", cliName, desc)
	fmt.Fprintf(&src, "type %s struct {\n%s\n}\n", cliName, strings.TrimRight(strings.Join(rootLines, "\n"), "\n"))
	if c.main != nil && c.main.local == nil {
		if w := c.main.flags; w != nil && w.v.Parent() == w.v.Pkg().Scope() {
			c.cliInits = append(c.cliInits, cliInit{field: w.field, varName: w.v.Name(), pointer: w.ptr})
		}
		// With command structs to start with, cli is declared where they were.
		switch {
		case c.main.cliSpec == nil && len(c.cliInits) > 0:
			var inits []string
			for _, ci := range c.cliInits {
				inits = append(inits, ci.field+": "+renderIn(ci.expr(root.PkgPath), imports))
			}
			fmt.Fprintf(&src, "\n// %s holds the parsed command line.\nvar %s = %s{%s}\n", c.cliVar, c.cliVar, cliName, strings.Join(inits, ", "))
		case c.main.cliSpec == nil:
			fmt.Fprintf(&src, "\n// %s holds the parsed command line.\nvar %s %s\n", c.cliVar, c.cliVar, cliName)
		}
	}
	for _, g := range generated {
		src.WriteString("\n" + g)
	}
	if g := c.genActions[t.Root]; g != nil {
		src.WriteString("\n" + afterApplySource(cliName, g))
	}
	if len(c.rootVars) > 0 {
		src.WriteString("\n" + varsMethod(c.rootVars, cliName, "c", c.compatPath, imports))
	}
	if h, ok := c.hintsMethod(t.Root, root.Types, cliName, "c", imports); !ok {
		c.reportHints(t.Root)
	} else if h != "" {
		src.WriteString("\n" + h)
	}
	if opts := c.conditionalOptions(root, cliName, imports); opts != "" {
		src.WriteString("\n" + opts)
	}
	c.newFile(root, c.freeFile(root, "cli.go", "kong_cli.go"), imports, src.String())
}

// versionTag returns the tag of the --version flag, including what the
// program changed through VersionFlag.
func versionTag(app *clause, field string) *kongtag.Tag {
	t := new(kongtag.Tag)
	if kongName(field) != "version" {
		t.Set("name", "version")
	}
	vf := app.versionFlag
	if vf == nil {
		vf = &clause{kind: kindFlag, parent: app, name: "version"}
	}
	if vf.short != 0 {
		t.Set("short", string(vf.short))
	}
	if env := defaultEnvar(vf); env != "" {
		t.Set("env", env)
	}
	if vf.hidden {
		t.Flag("hidden")
	}
	return t.Set("help", "Show application version.")
}

func (c *converter) hasStorage(n *node) bool {
	for _, b := range c.plan.bindings {
		if b.storage && b.cl.parent == n.cl {
			return true
		}
	}
	return false
}

// hasVersionCall reports whether the program calls the application's Version
// method, which kingpin turned into a --version flag.
func (c *converter) hasVersionCall() bool {
	return c.versionArg() != nil
}

// versionArg returns the argument of the application's Version call, or nil.
func (c *converter) versionArg() dst.Expr {
	var arg dst.Expr
	root := c.tree.RootPkg
	for _, f := range root.Syntax {
		dst.Inspect(f, func(n dst.Node) bool {
			if call, ok := n.(*dst.CallExpr); ok && len(call.Args) == 1 && c.runtimeCall(root, call) == "Version" {
				arg = call.Args[0]
			}
			return arg == nil
		})
	}
	return arg
}

func (c *converter) rootHasCommand(name string) bool {
	for _, ch := range c.tree.Root.children {
		if ch.cl.name == name || ch.field == "Help" {
			return true
		}
	}
	return false
}

// nodeType is the Go type of a command's field in its parent container.
func (c *converter) nodeType(n *node, names map[*node]string, parent *node, imports map[string]string) string {
	if name, ok := names[n]; ok {
		return name
	}
	if n.typ == nil {
		return "struct{}"
	}
	var pkg *types.Package
	if parent.parent == nil || !parent.containerOwn {
		pkg = c.tree.RootPkg.Types
	} else {
		pkg = parent.typ.Obj().Pkg()
	}
	return typeExpr(n.typ, pkg, imports)
}

// freeName returns the first candidate, or the first one with a number
// appended, that is unused at the top level of p.
// Declarations, import names and names this conversion added all count.
func (c *converter) freeName(p *Package, candidates ...string) string {
	if c.added == nil {
		c.added = map[*Package]map[string]bool{}
	}
	if c.added[p] == nil {
		c.added[p] = map[string]bool{}
	}
	taken := func(name string) bool {
		if c.added[p][name] || p.Types.Scope().Lookup(name) != nil {
			return true
		}
		for _, f := range p.Syntax {
			for _, imp := range f.Imports {
				if _, local := p.importName(imp); local == name {
					return true
				}
			}
		}
		return false
	}
	name := firstFree(taken, candidates...)
	c.added[p][name] = true
	return name
}

// reserveLocals marks the names declared inside the functions of p as taken
// for freeName.
func (c *converter) reserveLocals(p *Package) {
	if c.added == nil {
		c.added = map[*Package]map[string]bool{}
	}
	if c.added[p] == nil {
		c.added[p] = map[string]bool{}
	}
	for id, o := range p.TypesInfo.Defs {
		if o != nil && o.Parent() != nil && o.Parent() != p.Types.Scope() {
			c.added[p][id.Name] = true
		}
	}
}

// freeFile returns the first candidate file name, or the first one with a
// number before the extension, that does not exist in the package directory.
func (c *converter) freeFile(p *Package, candidates ...string) string {
	dir := ""
	for _, f := range p.Syntax {
		dir = filepath.Dir(p.filename(f))
		break
	}
	exists := func(name string) bool {
		path := filepath.Join(dir, name)
		if _, ok := c.res.NewFiles[path]; ok {
			return true
		}
		_, err := os.Stat(path)
		return err == nil
	}
	for _, cand := range candidates {
		if !exists(cand) {
			return cand
		}
	}
	base := strings.TrimSuffix(candidates[0], ".go")
	for i := 2; ; i++ {
		if cand := fmt.Sprintf("%s%d.go", base, i); !exists(cand) {
			return cand
		}
	}
}

func (c *converter) containerName(n *node) string {
	return unexported(c.prog.identFromName(strings.ReplaceAll(n.cl.fullName(), " ", "-"))) + "Commands"
}

// unexported lowers the first word of an identifier, keeping initialisms
// whole: DBMigrate becomes dbMigrate, Status becomes status.
func unexported(name string) string {
	words := camelCase(name)
	if len(words) == 0 {
		return name
	}
	words[0] = strings.ToLower(words[0])
	return strings.Join(words, "")
}

// addCmdFields appends subcommand fields to a command's own struct.
func (c *converter) addCmdFields(n *node, lines []string, imports map[string]string) {
	pkg := n.typ.Obj().Pkg()
	decls, err := snippet(pkg.Name(), imports, fmt.Sprintf("type _ struct {\n%s\n}", strings.Join(lines, "\n")))
	if err != nil {
		c.issue(n.cl.pos, "%v", err)
		return
	}
	st := decls[0].(*dst.GenDecl).Specs[0].(*dst.TypeSpec).Type.(*dst.StructType)
	if !c.ed.appendFields(n.typ, st.Fields.List) {
		c.issue(n.cl.pos, "cannot find the declaration of %s", n.typ.Obj().Name())
	}
}

// appFlagBindings returns the application flags that go to a struct of the
// program, rather than to variables that become fields of a converted main.
func (c *converter) appFlagBindings() []*binding {
	var out []*binding
	for _, b := range c.plan.bindings {
		if b.cmd.kind == kindApp && !b.storage && (b.dstVar == nil || c.main == nil) {
			out = append(out, b)
		}
	}
	return out
}

// findRootFlags sets rootFlags when the application flags all go to one
// struct type of the program.
// rootFlagsEmbed reports the other cases.
func (c *converter) findRootFlags() {
	var holder *object
	for _, b := range c.appFlagBindings() {
		if b.holder == nil {
			continue
		}
		if holder != nil && holder != b.holder {
			return
		}
		holder = b.holder
	}
	if holder == nil {
		return
	}
	if named, ok := holder.typ.(*types.Named); ok && c.prog.owns(named.Obj().Pkg()) {
		c.rootFlags = named
	}
}

// rootFlagsEmbed returns the embedded field that carries the application's
// own flags.
// It is a pointer to the struct they were bound into, so kong writes into the
// same place kingpin did.
func (c *converter) rootFlagsEmbed(imports map[string]string) (line string) {
	var holder *object
	// The embedded struct is sorted with the other flags by its first flag.
	first := -1
	defer func() {
		if line != "" && first >= 0 {
			c.fieldOrder[line] = 2 * first
		}
	}()
	for _, b := range c.appFlagBindings() {
		if b.holder == nil {
			c.issue(b.cl.pos, "application flag %q is not bound to a struct field", b.cl.name)
			continue
		}
		if first < 0 || b.cl.id < first {
			first = b.cl.id
		}
		if holder == nil {
			holder = b.holder
		} else if holder != b.holder {
			c.issue(b.cl.pos, "application flags are bound to several structs; only one is supported")
			return ""
		}
		owner := c.nm.declaringType(b.field)
		if owner == nil || c.tagged[b.field] {
			continue
		}
		name := c.nm.exportField(owner, b.field)
		tag := c.plan.tagFor(b, name)
		shared, err := c.ed.addTag(b.field, tag.String())
		if err != nil {
			c.issue(b.cl.pos, "%v", err)
			continue
		}
		c.reportSharedKeys(b, b.field, shared)
		c.rename(b.field, name)
		c.tagged[b.field] = true
	}
	if holder == nil {
		return ""
	}
	named, ok := holder.typ.(*types.Named)
	if !ok || !c.prog.owns(named.Obj().Pkg()) {
		c.issue(token.Position{}, "application flags are bound to a struct the tool does not own")
		return ""
	}
	c.rootFlags = named
	if c.main != nil {
		if v, ptr := c.holderVar(holder); v != nil {
			c.main.flags = &flagsWiring{field: named.Obj().Name(), v: v, ptr: ptr}
			return fmt.Sprintf("// %s holds the global flags, in the program's own %s.\n*%s\n",
				named.Obj().Name(), v.Name(), typeExpr(named, c.tree.RootPkg.Types, imports))
		}
		c.issue(c.tree.Root.cl.pos, "application flags are bound to a struct no variable of the program holds; point the CLI's %s at it before parsing", named.Obj().Name())
	}
	return fmt.Sprintf("// %s holds the global flags. Point it at the program's own %s before\n// parsing, so kong writes them where the rest of the code reads them.\n*%s\n",
		named.Obj().Name(), named.Obj().Name(), typeExpr(named, c.tree.RootPkg.Types, imports))
}

// conditionalOptions returns the source of a function whose kong option
// removes the commands and flags kingpin only registered under a condition.
func (c *converter) conditionalOptions(root *Package, cliName string, imports map[string]string) string {
	type group struct {
		cond   string
		fields []string
	}
	groups := map[string]*group{}
	var order []string
	add := func(cl *clause, field string) {
		if len(cl.conds) == 0 {
			return
		}
		var parts []string
		for _, cd := range cl.conds {
			s, ok := c.renderCond(cd, imports)
			if !ok {
				c.issue(cl.pos, "%s %q is registered under a condition the tool cannot move: %s", cl.kind, cl.name, render(cd.expr))
				return
			}
			parts = append(parts, s)
		}
		cond := strings.Join(parts, " || ")
		g := groups[cond]
		if g == nil {
			g = &group{cond: cond}
			groups[cond] = g
			order = append(order, cond)
		}
		g.fields = append(g.fields, field)
	}
	for _, ch := range c.tree.Root.children {
		add(ch.cl, ch.field)
	}
	for _, b := range c.plan.bindings {
		if b.cmd.kind != kindApp {
			continue
		}
		if name, ok := c.rootFieldNames[b.cl]; ok {
			add(b.cl, name)
		} else if b.field != nil {
			add(b.cl, c.finalName(b.field))
		}
	}
	for _, n := range c.tree.nodes {
		if n.parent != nil && n.parent.parent != nil && len(n.cl.conds) > len(n.parent.cl.conds) {
			c.issue(n.cl.pos, "command %q is registered under a condition, which is only supported for top-level commands", n.cl.fullName())
		}
	}
	for _, b := range c.plan.bindings {
		if b.cmd.kind == kindCommand && len(b.cl.conds) > len(b.cmd.conds) {
			c.issue(b.cl.pos, "%s %q of command %q is registered under a condition, which is only supported for the application's own flags and top-level commands; kong always accepts it", b.cl.kind, b.cl.name, b.cmd.fullName())
		}
	}
	if len(order) == 0 {
		return ""
	}
	c.conditionalName = c.freeName(root, "Conditional", "KongConditional")
	var b strings.Builder
	fmt.Fprintf(&b, "// %s returns the kong option that leaves out the commands and flags\n", c.conditionalName)
	b.WriteString("// the command line only offers under some conditions.\n")
	fmt.Fprintf(&b, "func %s() kong.Option {\n", c.conditionalName)
	b.WriteString("return kong.OptionFunc(func(k *kong.Kong) error {\n")
	for _, cond := range order {
		g := groups[cond]
		ignore := fmt.Sprintf("kong.IgnoreFields(`^%s\\.(%s)$`).Apply(k)", cliName, strings.Join(quoteAll(g.fields), "|"))
		if len(order) == 1 {
			fmt.Fprintf(&b, "if %s {\nreturn %s\n}\n", cond, ignore)
			continue
		}
		fmt.Fprintf(&b, "if %s {\nif err := %s; err != nil {\nreturn err\n}\n}\n", cond, ignore)
	}
	b.WriteString("return nil\n})\n}\n")
	return b.String()
}

func quoteAll(s []string) []string {
	out := make([]string, len(s))
	for i, v := range s {
		out[i] = regexp.QuoteMeta(v)
	}
	return out
}

// renderCond renders the condition under which a registration was skipped.
// The generated option cannot see locals, so a local set from a side-effect
// free call is replaced by the call.
func (c *converter) renderCond(cd cond, imports map[string]string) (string, bool) {
	e := dst.Clone(cd.expr).(dst.Expr)
	ok := true
	e = dstutil.Apply(e, func(cur *dstutil.Cursor) bool {
		id, isIdent := cur.Node().(*dst.Ident)
		if !isIdent || id.Path != "" {
			return true
		}
		// Identifiers in the clone lost their type information, so look
		// them up in the original expression by name.
		v := c.localIn(cd, id.Name)
		if v == nil {
			return true
		}
		init := c.localInit(cd, v)
		if init == nil {
			ok = false
			return true
		}
		cur.Replace(dst.Clone(init))
		return true
	}, nil).(dst.Expr)
	if !ok {
		return "", false
	}
	neg := !cd.neg
	if u, isNot := e.(*dst.UnaryExpr); isNot && u.Op == token.NOT && neg {
		return renderIn(u.X, imports), true
	}
	if !neg {
		return renderIn(e, imports), true
	}
	switch x := e.(type) {
	case *dst.BinaryExpr:
		// == and != negate exactly; other comparisons do not with NaN.
		switch x.Op {
		case token.EQL:
			x.Op = token.NEQ
			return renderIn(x, imports), true
		case token.NEQ:
			x.Op = token.EQL
			return renderIn(x, imports), true
		}
	case *dst.Ident, *dst.CallExpr, *dst.SelectorExpr:
		return "!" + renderIn(e, imports), true
	}
	return "!(" + renderIn(e, imports) + ")", true
}

// localIn finds a local variable named name used in a condition.
func (c *converter) localIn(cd cond, name string) *types.Var {
	var found *types.Var
	dst.Inspect(cd.expr, func(n dst.Node) bool {
		if id, ok := n.(*dst.Ident); ok && id.Name == name && id.Path == "" {
			if v, ok := cd.pkg.objectOf(id).(*types.Var); ok && v.Parent() != v.Pkg().Scope() && !v.IsField() {
				found = v
			}
		}
		return true
	})
	return found
}

// localInit returns the initializer of a local variable when it is assigned
// once, from a call without arguments to a package-level function.
// Otherwise it returns nil.
func (c *converter) localInit(cd cond, v *types.Var) dst.Expr {
	fd, ok := c.prog.funcs[cd.fn]
	if !ok {
		return nil
	}
	var init dst.Expr
	count := 0
	dst.Inspect(fd.decl, func(n dst.Node) bool {
		as, ok := n.(*dst.AssignStmt)
		if !ok {
			return true
		}
		for i, l := range as.Lhs {
			if fd.pkg.objectOf(l) == v {
				count++
				if i < len(as.Rhs) {
					init = as.Rhs[i]
				}
			}
		}
		return true
	})
	if count != 1 || init == nil {
		return nil
	}
	call, ok := init.(*dst.CallExpr)
	if !ok || len(call.Args) != 0 {
		return nil
	}
	if id, ok := call.Fun.(*dst.Ident); !ok || id.Path == "" {
		return nil
	}
	return init
}

// hideOtherFields tags with kong:"-" the exported fields kong would otherwise
// turn into flags.
// That is everything in a command struct, or in a struct embedded in one,
// that is not part of the grammar.
func (c *converter) hideOtherFields() {
	seen := map[*types.Named]bool{}
	var visit func(t *types.Named)
	visit = func(t *types.Named) {
		if t == nil || seen[t] || !c.prog.owns(t.Obj().Pkg()) {
			return
		}
		seen[t] = true
		if c.plan.wrappers[t] != nil {
			return
		}
		st := structOf(t)
		if st == nil {
			return
		}
		for i := 0; i < st.NumFields(); i++ {
			f := st.Field(i)
			if c.tagged[f] {
				if f.Anonymous() || c.isEmbedTagged(f) {
					if n, ok := deref(f.Type()).(*types.Named); ok {
						visit(n)
					}
				}
				continue
			}
			if f.Anonymous() {
				n, ok := deref(f.Type()).(*types.Named)
				if ok && structOf(n) != nil && c.prog.owns(n.Obj().Pkg()) && !c.prog.isKingpinType(n) {
					visit(n)
					continue
				}
				if !f.Exported() || structOf(deref(f.Type())) == nil && !isInterface(f.Type()) {
					continue
				}
			} else if !f.Exported() {
				continue
			}
			if c.prog.isKingpinType(f.Type()) {
				continue
			}
			if _, err := c.ed.addTag(f, `kong:"-"`); err != nil {
				c.issue(token.Position{}, "%s.%s: %v", t.Obj().Name(), f.Name(), err)
			}
		}
	}
	for _, n := range c.tree.nodes {
		visit(n.typ)
	}
	if c.rootFlags != nil {
		visit(c.rootFlags)
	}
}

func isInterface(t types.Type) bool {
	_, ok := t.Underlying().(*types.Interface)
	return ok
}

func (c *converter) isEmbedTagged(f *types.Var) bool {
	loc, ok := c.ed.fields[f]
	if !ok || loc.field.Tag == nil {
		return false
	}
	tag, err := strconv.Unquote(loc.field.Tag.Value)
	_, embed := reflect.StructTag(tag).Lookup("embed")
	return err == nil && embed
}

// writeHooks adds the BeforeApply and AfterApply methods collected in c.hooks
// to their types.
func (c *converter) writeHooks() {
	var types_ []*types.Named
	for t := range c.hooks {
		types_ = append(types_, t)
	}
	sort.Slice(types_, func(i, j int) bool {
		return types_[i].Obj().Pkg().Path()+"."+types_[i].Obj().Name() < types_[j].Obj().Pkg().Path()+"."+types_[j].Obj().Name()
	})
	for _, t := range types_ {
		h := c.hooks[t]
		if len(h.before) == 0 && len(h.after) == 0 && len(h.checks) == 0 && len(h.actions) == 0 {
			continue
		}
		loc, ok := c.ed.typeSpecs[t.Obj()]
		if !ok {
			continue
		}
		conflict := false
		for m, needed := range map[string]bool{"BeforeApply": len(h.before) > 0, "AfterApply": len(h.after)+len(h.checks)+len(h.actions) > 0} {
			if _, exists := c.ed.methods[t][m]; exists && needed {
				c.issue(loc.pkg.position(loc.spec), "%s already has a %s method; the generated one has to be merged by hand", t.Obj().Name(), m)
				conflict = true
			}
		}
		if conflict {
			continue
		}
		recv := c.receiverName(t, strings.ToLower(t.Obj().Name()[:1]))
		if h.outer[recv] {
			// The moved constructor code uses that name from outside.
			recv = firstFree(func(name string) bool { return h.outer[name] || h.beforeParams[name] != "" || isTypeParam(t, name) }, strings.ToLower(t.Obj().Name()[:1]), "cmd", "command")
		}
		if _, clash := h.beforeParams[recv]; clash && len(h.before) > 0 {
			c.issue(loc.pkg.position(loc.spec), "the BeforeApply hook of %s would need a parameter named like its receiver, %s; move what the constructor did by hand", t.Obj().Name(), recv)
			continue
		}
		var src strings.Builder
		if len(h.before) > 0 {
			var params []string
			var pnames []string
			for name := range h.beforeParams {
				pnames = append(pnames, name)
			}
			sort.Strings(pnames)
			for _, name := range pnames {
				params = append(params, name+" "+h.beforeParams[name])
			}
			src.WriteString(hookDoc("BeforeApply", t, len(params) > 0))
			fmt.Fprintf(&src, "func (%s *%s) BeforeApply(%s) error {\n", recv, recvType(t), strings.Join(params, ", "))
			for _, s := range h.before {
				src.WriteString(renameIdent(s, c.recvPlaceholder, recv) + "\n")
			}
			src.WriteString("return nil\n}\n\n")
		}
		if len(h.after) > 0 || len(h.checks) > 0 || len(h.actions) > 0 {
			src.WriteString(afterDoc(len(h.checks) > 0, len(h.after) > 0, len(h.actions) > 0))
			fmt.Fprintf(&src, "func (%s *%s) AfterApply() error {\n", recv, recvType(t))
			for _, s := range h.checks {
				src.WriteString(withReceiver(s, recv) + "\n")
			}
			for _, s := range h.after {
				src.WriteString(withReceiver(s, recv) + "\n")
			}
			// Actions name the receiver with the placeholder, and may call a
			// function named c.
			src.WriteString(renameIdent(runActions(h.actions), c.recvPlaceholder, recv))
			src.WriteString("}\n\n")
		}
		decls, err := snippet(loc.pkg.Name, h.imports, src.String())
		if err != nil {
			c.issue(loc.pkg.position(loc.spec), "%v", err)
			continue
		}
		if h.anchor != nil {
			c.ed.insertAt(h.anchor.pkg, h.anchor.file, h.anchor.decl, decls)
		} else {
			c.ed.insertAfter(loc.pkg, loc.file, c.lastMethodOrType(t, loc.gen), decls)
		}
	}
}

func afterDoc(checks, copies, runs bool) string {
	var parts []string
	if checks {
		parts = append(parts, "checks the values kong cannot check by itself")
	}
	if copies {
		parts = append(parts, "hands the parsed flags over to the fields the command reads them from")
	}
	if runs {
		parts = append(parts, "runs the command, as kingpin ran its Action after parsing")
	}
	return lineComment("AfterApply " + strings.Join(parts, ", then ") + ".")
}

// runActions returns code that calls the former Actions in order and returns
// the first error.
func runActions(calls []string) string {
	if len(calls) == 0 {
		return "return nil\n"
	}
	var b strings.Builder
	for _, call := range calls[:len(calls)-1] {
		fmt.Fprintf(&b, "if err := %s; err != nil {\nreturn err\n}\n", call)
	}
	fmt.Fprintf(&b, "return %s\n", calls[len(calls)-1])
	return b.String()
}

// lineComment wraps text into // comment lines of at most 80 columns.
func lineComment(text string) string {
	var b strings.Builder
	line := "//"
	for _, w := range strings.Fields(text) {
		if len(line)+1+len(w) > 80 {
			b.WriteString(line + "\n")
			line = "//"
		}
		line += " " + w
	}
	b.WriteString(line + "\n")
	return b.String()
}

func hookDoc(hook string, t *types.Named, params bool) string {
	switch hook {
	case "BeforeApply":
		if params {
			return "// BeforeApply sets the command up with the values bound when the parser\n// was built, before kong applies the command line.\n"
		}
		return "// BeforeApply sets the command up before kong applies the command line.\n"
	default:
		return "// AfterApply hands the parsed flags over to the fields the command\n// reads them from.\n"
	}
}

// withReceiver renames the placeholder receiver c of generated statements.
func withReceiver(s, recv string) string {
	if recv == "c" {
		return s
	}
	return renameIdent(s, "c", recv)
}

// renameIdent renames the identifier from in Go source, leaving selectors
// such as x.from and string literals alone.
func renameIdent(s, from, to string) string {
	var sc scanner.Scanner
	fset := token.NewFileSet()
	src := []byte(s)
	sc.Init(fset.AddFile("", -1, len(src)), src, nil, 0)
	var out strings.Builder
	last := 0
	prevPeriod := false
	for {
		pos, tok, lit := sc.Scan()
		if tok == token.EOF {
			break
		}
		off := fset.Position(pos).Offset
		if tok == token.IDENT && lit == from && !prevPeriod {
			out.Write(src[last:off])
			out.WriteString(to)
			last = off + len(from)
		}
		prevPeriod = tok == token.PERIOD
	}
	out.Write(src[last:])
	return out.String()
}
