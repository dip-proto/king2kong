package conv

import (
	"cmp"
	"fmt"
	"go/constant"
	"go/token"
	"go/types"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/dave/dst"
	"github.com/dave/dst/decorator/resolver/guess"
	"github.com/dave/dst/dstutil"
)

// Result is the outcome of a conversion: rewritten files, new files, and what
// is left to do by hand.
type Result struct {
	Files    map[string]string
	NewFiles map[string]string
	Issues   []Issue
	Notes    []string
	Stats    Stats
}

// Stats counts what the conversion did.
type Stats struct {
	Commands, Flags, Args int
	Shadows               int
	Renamed               int
	RemovedFuncs          int
}

// converter holds the state of one conversion.
type converter struct {
	prog  *Program
	g     *Grammar
	plan  *Plan
	tree  *Tree
	ctors map[*types.Func]*ctorInfo
	ed    *editor
	nm    *namer
	res   *Result

	// tagged marks the fields that got a kong tag.
	tagged map[*types.Var]bool
	hooks  map[*types.Named]*hookSet
	// valueEmbeds are embedded pointers turned into values.
	valueEmbeds map[*types.Var]bool
	// cmdTypes are the struct types of grammar nodes.
	cmdTypes map[*types.Named]bool
	// rootFlags is the struct holding the application's own flags.
	rootFlags *types.Named
	// main is set when the program parses its command line in the package
	// holding the grammar, so the whole program is converted.
	main         *mainProgram
	storagePaths map[*clause]string
	rootImports  map[string]string
	// cliName and cliVar name the generated root struct and the variable
	// holding the parsed command line.
	cliName string
	cliVar  string
	// cliInits are the fields the cli variable starts with: pointers to the
	// structs holding flags elsewhere.
	cliInits []cliInit
	// pkgFlagsOf marks the application flags held by another package.
	pkgFlagsOf map[*clause]bool
	// injected are the types hooks take as parameters, which kong.Bind must
	// provide where the parser is built, unless boundAtCall already does.
	injected    []types.Type
	boundAtNew  []types.Type
	boundAtCall []types.Type
	// pruned are functions that lost parameters or results with the wiring,
	// so their callers must be updated too.
	pruned map[*types.Func]prunedSig
	// kingpinTests are the test files that import kingpin.
	kingpinTests map[string]bool
	// keptCtors are the constructors tests call, kept without their
	// registrations.
	keptCtors []*ctorInfo
	// keepStmts are statements that look like registration but stay: the
	// command lists functions return, and what kept constructors build.
	keepStmts map[dst.Stmt]bool
	// nodeOf maps a command struct instance to its node.
	// cliDeps are the packages the CLI's package imports, directly or not.
	nodeOf  map[*object]*node
	exports map[string]bool
	cliDeps map[*types.Package]bool
	// structConversions are struct types converted to other types.
	// unkeyed are struct types that some literal builds without field names.
	structConversions map[*types.Named]bool
	readFields        map[*types.Var]bool
	pureCalls         map[*types.Func]bool
	// recvPlaceholder stands for the receiver in hook statements built from
	// the program's code until writeHooks picks its name, so that code keeps
	// its own names; the program never uses it.
	recvPlaceholder string
	ifaceMethods    map[string]bool
	callsTo         map[*types.Func][]callRef
	// funcWork caches doesSomething for the bodies of the program's
	// functions.
	funcWork   map[*types.Func]bool
	valueFuncs map[*types.Func]bool
	imports    *importGraph
	// genErr is why a file the conversion generates could not be written.
	genErr error
	// jumpKinds are the labels of the function being cleaned up that branch
	// statements still use, with the kinds of those branches.
	jumpKinds map[string]map[token.Token]bool
	// reassigned are its locals assigned somewhere other than where they
	// are declared.
	reassigned map[types.Object]bool
	// boundValues are what each parser binds for the hooks, by type, in the
	// runtime conversion, as source.
	boundValues map[string]string
	// unmapped are the kingpin names already reported as having no
	// kongcompat equivalent.
	unmapped map[string]bool
	// pkgFlagVars are the package-level variables of other packages that
	// now point into their package's flags struct, whose assignments go.
	pkgFlagVars map[types.Object]bool
	// fixedResults is set when it keeps its signature, so its returns keep
	// their values.
	fixedResults bool
	// declared are the names the conversion declared in each block.
	declared map[dst.Node]map[string]bool
	// hiddenEmbeds are the embedded structs tagged kong:"-" because their
	// command registered none of their flags.
	hiddenEmbeds map[*types.Var]bool
	// cmdLists are the functions whose command lists are kept, and
	// listParams the application parameters they keep.
	cmdLists   map[*types.Func]*cmdList
	listParams map[types.Object]bool
	// rootVars are the run-time variables of the root's flags and
	// arguments, returned by the CLI's Vars method.
	// commandVars is set when a command type has a Vars method.
	rootVars    []runtimeVar
	commandVars bool
	// parserCalls are the calls building parsers the conversion wrote.
	parserCalls []*dst.CallExpr
	// commandNames is set when command structs have a field kongcompat
	// fills with the command's name (see kongcompat.CommandNames).
	commandNames bool
	// emptied are the wiring functions nothing was left of, removed with
	// their calls.
	emptied      map[*types.Func]bool
	removedCtors map[*types.Func]bool
	// conditionalName names the generated function returning the option
	// that leaves out conditional commands and flags, when there is one.
	conditionalName string
	// rootFieldNames are the names of the CLI fields generated for
	// application flags.
	rootFieldNames map[*clause]string
	// fieldOrder ranks generated root field lines by when kingpin registered
	// their flag, which is the order help lists them in.
	fieldOrder map[string]int
	// added holds the package-level names this conversion declares.
	added map[*Package]map[string]bool
	// genActions are the former Actions of the application and of commands
	// without a struct of their own, which convertTree hooks to the
	// generated structs.
	genActions map[*node]*genActions
	// keep holds declarations generated code calls.
	keep map[types.Object]bool

	instances map[*object]bool
	regOnly   map[*types.Func]bool
	testRefs  *testRefs
	renames   map[*types.Var]string
	opts      Options
	// compatPath is the import path of the kongcompat package written into
	// the module.
	compatPath string
}

// cliInit is a field the cli variable is initialized with: the address of
// varName, a variable of the package pkgPath ("" for the CLI's own), or
// varName itself when it is already a pointer.
type cliInit struct {
	field, pkgPath, varName string
	pointer                 bool
}

// expr builds the field's value.
func (ci cliInit) expr(rootPath string) dst.Expr {
	id := &dst.Ident{Name: ci.varName}
	if ci.pkgPath != "" && ci.pkgPath != rootPath {
		id.Path = ci.pkgPath
	}
	if ci.pointer {
		return id
	}
	return &dst.UnaryExpr{Op: token.AND, X: id}
}

// hookSet gathers the statements of the hooks generated for one type.
type hookSet struct {
	typ          *types.Named
	before       []string
	beforeParams map[string]string // parameter name -> type source
	after        []string
	checks       []string
	// actions are calls of former kingpin Actions, run last in AfterApply.
	actions []string
	imports map[string]string
	anchor  *funcDecl // where to insert, usually the deleted constructor
	// outer are the names the moved constructor code uses from outside the
	// constructor, which the receiver must not hide.
	outer map[string]bool
}

// paramOfType returns the name of the parameter of type typ, or "".
func (h *hookSet) paramOfType(typ string) string {
	for n, t := range h.beforeParams {
		if t == typ {
			return n
		}
	}
	return ""
}

// paramToLocal replaces the BeforeApply parameter name of type typ with
// the local that decl declares, and reports whether there was one.
// It is for setup moved from a constructor that still reads a parameter
// whose value is known.
func (h *hookSet) paramToLocal(name, typ, decl string) bool {
	if h.beforeParams[name] != typ {
		return false
	}
	delete(h.beforeParams, name)
	h.before = append([]string{decl}, h.before...)
	return true
}

// param adds a parameter to the BeforeApply hook and returns its name, with a
// number appended when a parameter of another type already uses it.
func (h *hookSet) param(name, typ string) string {
	for n, i := name, 2; ; i++ {
		if t, ok := h.beforeParams[n]; !ok || t == typ {
			h.beforeParams[n] = typ
			return n
		}
		n = fmt.Sprintf("%s%d", name, i)
	}
}

// Options tunes a conversion.
type Options struct {
	// Compat holds the source of the kongcompat package, written into the
	// converted module so it keeps kingpin's help, errors and completion.
	Compat fs.FS
}

// Convert rewrites the program to declare its command line with kong.
func Convert(prog *Program, opts Options) (*Result, error) {
	c := &converter{
		prog:           prog,
		ed:             newEditor(prog),
		nm:             newNamer(prog),
		res:            &Result{Files: map[string]string{}, NewFiles: map[string]string{}},
		tagged:         map[*types.Var]bool{},
		pureCalls:      map[*types.Func]bool{},
		pkgFlagVars:    map[types.Object]bool{},
		unmapped:       map[string]bool{},
		boundValues:    map[string]string{},
		funcWork:       map[*types.Func]bool{},
		hooks:          map[*types.Named]*hookSet{},
		valueEmbeds:    map[*types.Var]bool{},
		cmdTypes:       map[*types.Named]bool{},
		renames:        map[*types.Var]string{},
		opts:           opts,
		storagePaths:   map[*clause]string{},
		genActions:     map[*node]*genActions{},
		fieldOrder:     map[string]int{},
		rootFieldNames: map[*clause]string{},
		removedCtors:   map[*types.Func]bool{},
		emptied:        map[*types.Func]bool{},
		pruned:         map[*types.Func]prunedSig{},
		kingpinTests:   map[string]bool{},
		keep:           map[types.Object]bool{},
		keepStmts:      map[dst.Stmt]bool{},
		hiddenEmbeds:   map[*types.Var]bool{},
		declared:       map[dst.Node]map[string]bool{},
		cmdLists:       map[*types.Func]*cmdList{},
		listParams:     map[types.Object]bool{},
	}
	c.g = Analyze(prog)
	c.plan = NewPlan(c.g)
	c.tree = c.plan.planTree()
	if c.tree == nil {
		c.res.Issues = c.plan.Issues
		return c.res, fmt.Errorf("the command tree could not be recovered")
	}
	c.ctors = c.plan.analyzeCtors(c.tree)
	c.recvPlaceholder = firstFree(c.programUses, "king2kongRecv")
	renderPackage = prog.packageOf
	if c.detectMain() {
		c.main = &mainProgram{repl: map[types.Object]replacement{}, instances: map[types.Object]*object{}}
		c.main.local = c.localCLI()
	}
	for _, n := range c.tree.nodes {
		if n.typ == nil {
			continue
		}
		c.cmdTypes[n.typ] = true
		if n.containerOwn {
			for _, ch := range n.children {
				c.nm.claim(n.typ, ch.field)
			}
		}
		// convertHints may add a Hints method here or on an embedded type.
		if slices.ContainsFunc(n.cl.children, func(ch *clause) bool { return ch.hintExpr != nil }) {
			for _, t := range append([]*types.Named{n.typ}, embeddedClosure(n.typ)...) {
				c.nm.claim(t, "Hints")
			}
		}
	}

	fatal := false
	// A loop the interpreter unrolls meets the same escape once per value.
	reported := map[escape]bool{}
	for _, e := range c.g.in.escapes {
		if e.report && !reported[e] {
			reported[e] = true
			c.issue(e.pos, "%s", e.what)
		}
		fatal = fatal || e.fatal
	}
	if fatal {
		return c.res, fmt.Errorf("the kingpin application is handed to code outside the program; convert or replace that code first, nothing was changed")
	}
	// Flags report their own issues when they are bound; commands and the
	// application are reported here.
	for _, n := range c.tree.nodes {
		for _, is := range append(n.cl.issues, n.cl.notes...) {
			c.issue(n.cl.pos, "%s: %s", describeClause(n.cl), is)
		}
	}
	c.computeRegOnly()
	c.writeCompat()
	c.testRefs = c.scanUnloaded(prog.Dir)
	c.convertFlags()
	c.checkArgOrder()
	c.markFailures()
	c.convertEnumChecks()
	c.convertWrappers()
	c.convertCustomValues()
	c.convertConstructors()
	c.convertActions()
	c.findRootFlags()
	c.reorderFlags()
	c.convertVars()
	c.convertTree()
	c.convertHints()
	c.hideOtherFields()
	c.hideUnusedEmbeds()
	c.findCommandLists()
	c.cleanupWiring()
	if c.main != nil {
		c.rewriteMain()
		c.declareLocalCLI()
	} else {
		c.convertRuntime()
	}
	c.removeDeadCode()
	c.dropUnusedLocals()
	c.convertCommandNames()
	c.addCommandNamesOption()
	c.writeHooks()

	c.res.Issues = append(c.res.Issues, c.plan.Issues...)
	if c.genErr != nil {
		// The rest of the code relies on the generated files.
		return c.res, fmt.Errorf("%w; nothing was written", c.genErr)
	}
	if err := c.render(); err != nil {
		return c.res, err
	}
	if err := c.renameInTests(c.renames); err != nil {
		c.issue(token.Position{}, "renaming fields in test files: %v", err)
	}
	if err := c.convertTests(c.renames); err != nil {
		c.issue(token.Position{}, "converting test files: %v", err)
	}
	return c.res, nil
}

// reportAncestorTags reports the tags kong reads on the embedded structs on
// the way to a flag's field, such as prefix:"other-", which change the flag.
// A Signature method on a type on the way does the same.
func (c *converter) reportAncestorTags(b *binding, reported map[*types.Var]bool) {
	for _, s := range append(slices.Clone(b.path), step{f: b.field}) {
		if reported[s.f] {
			continue
		}
		reported[s.f] = true
		if hasSignature(s.f.Type()) {
			c.issue(b.cl.pos, "%s %q: the type of field %s has a Signature method, which kong reads as tags; check it against the flag", b.cl.kind, b.cl.name, s.f.Name())
		}
		loc, ok := c.ed.fields[s.f]
		if !s.f.Anonymous() || !ok || loc.field.Tag == nil {
			continue
		}
		tag, err := strconv.Unquote(loc.field.Tag.Value)
		if err != nil {
			continue
		}
		var keys []string
		for _, kv := range tagPairs(tag) {
			if kongKeys[kv[0]] || kv[0] == "kong" {
				keys = append(keys, kv[0])
			}
		}
		c.reportSharedKeys(b, s.f, keys)
	}
}

// hasSignature reports whether t has kong's Signature() string method.
func hasSignature(t types.Type) bool {
	if _, ptr := t.(*types.Pointer); !ptr {
		t = types.NewPointer(t)
	}
	m, _, _ := types.LookupFieldOrMethod(t, true, nil, "Signature")
	fn, ok := m.(*types.Func)
	if !ok {
		return false
	}
	sig := fn.Type().(*types.Signature)
	return sig.Params().Len() == 0 && sig.Results().Len() == 1 && types.Identical(sig.Results().At(0).Type(), types.Typ[types.String])
}

// reportSharedKeys reports the keys a field's tag already had that kong
// reads too: replaced by the flag's own, or left for kong to apply.
func (c *converter) reportSharedKeys(b *binding, field *types.Var, keys []string) {
	if len(keys) > 0 {
		c.issue(b.cl.pos, "%s %q: the tag of field %s already had %s, which kong reads too; check them against the flag", b.cl.kind, b.cl.name, field.Name(), strings.Join(keys, ", "))
	}
}

func (c *converter) issue(pos token.Position, format string, args ...any) {
	c.res.Issues = append(c.res.Issues, Issue{Pos: pos, Reason: fmt.Sprintf(format, args...)})
}

func (c *converter) hookFor(t *types.Named) *hookSet {
	h := c.hooks[t]
	if h == nil {
		h = &hookSet{typ: t, beforeParams: map[string]string{}, imports: map[string]string{}}
		c.hooks[t] = h
	}
	return h
}

// host returns the command type that gets a binding's extra field and hook.
// It picks the deepest command struct among the leading embedded fields of
// the path, so an alias wrapping another command shares its field instead of
// declaring the flag twice.
func (c *converter) host(b *binding) (*types.Named, []step) {
	hostType, _ := b.inst.typ.(*types.Named)
	rest := b.path
	for i, s := range b.path {
		if !s.f.Anonymous() {
			break
		}
		if n, ok := deref(s.f.Type()).(*types.Named); ok && c.cmdTypes[n] {
			hostType, rest = n, b.path[i+1:]
		}
	}
	return hostType, rest
}

// convertFlags tags destination fields, exports them, and adds shadow fields
// for destinations kong cannot reach.
func (c *converter) convertFlags() {
	type shadowKey struct {
		host *types.Named
		dst  string
	}
	shadows := map[shadowKey]*binding{}
	shadowName := map[shadowKey]string{}

	reported := map[*types.Var]bool{}
	for _, b := range c.plan.bindings {
		if b.cl.kind == kindArg && len(b.cmd.commands()) > 0 {
			c.issue(b.cl.pos, "%s has both positional arguments and commands, which kong does not allow", describeClause(b.cmd))
		}
		c.res.Stats.Flags++
		if b.cl.kind == kindArg {
			c.res.Stats.Args++
		}
		if b.storage {
			// Added as a new field later, with the command tree.
			continue
		}
		switch b.mode {
		case modeDirect, modeEmbed:
			c.reportAncestorTags(b, reported)
			owner := c.nm.declaringType(b.field)
			if owner == nil {
				c.issue(b.cl.pos, "cannot find the struct declaring field %s", b.field.Name())
				continue
			}
			if b.mode == modeEmbed {
				for _, s := range b.path {
					if s.f.Anonymous() || c.tagged[s.f] {
						continue
					}
					sOwner := c.nm.declaringType(s.f)
					name := c.nm.exportField(sOwner, s.f)
					shared, err := c.ed.addTag(s.f, `embed:""`)
					if err != nil {
						c.issue(b.cl.pos, "%v", err)
						continue
					}
					c.reportSharedKeys(b, s.f, shared)
					c.rename(s.f, name)
					c.tagged[s.f] = true
				}
			}
			if c.tagged[b.field] {
				continue
			}
			name := c.nm.exportField(owner, b.field)
			if name == "" {
				c.issue(b.cl.pos, "no free name to export field %s", b.field.Name())
				continue
			}
			tag := c.plan.tagFor(b, name)
			shared, err := c.ed.addTag(b.field, tag.String())
			if err != nil {
				c.issue(b.cl.pos, "%v", err)
				continue
			}
			c.reportSharedKeys(b, b.field, shared)
			c.rename(b.field, name)
			c.tagged[b.field] = true
		case modeShadow:
			if b.inst == nil {
				if c.main != nil && b.dstVar != nil {
					// A field of the CLI takes the variable's place.
					continue
				}
				c.issue(b.cl.pos, "application flag %q writes somewhere kong cannot reach", b.cl.name)
				continue
			}
			host, rest := c.host(b)
			if host == nil || !c.prog.owns(host.Obj().Pkg()) {
				c.issue(b.cl.pos, "flag %q: no command struct to add a field to", b.cl.name)
				continue
			}
			expr := c.relativeExpr(rest, b)
			key := shadowKey{host, expr}
			if prev := shadows[key]; prev != nil {
				if prev.tag.String() != b.tag.String() {
					c.issue(b.cl.pos, "flag %q is registered differently by commands sharing %s", b.cl.name, host.Obj().Name())
				}
				b.shadowField = shadowName[key]
				continue
			}
			shadows[key] = b
			name := c.nm.newField(host, c.prog.identFromName(b.cl.name))
			if name == "" {
				c.issue(b.cl.pos, "no free field name for flag %q", b.cl.name)
				continue
			}
			shadowName[key] = name
			c.addShadow(host, name, expr, b)
			b.shadowField = name
		}
	}
}

// checkArgOrder reports commands whose positional arguments kong would read
// in a different order than kingpin.
// kong reads them in field order, kingpin in registration order.
func (c *converter) checkArgOrder() {
	seq := 0
	keys := map[*binding][]int{}
	for _, b := range c.plan.bindings {
		if b.cl.kind != kindArg || b.inst == nil || b.storage {
			continue
		}
		var key []int
		for _, s := range b.path {
			key = append(key, b.indexOf(s.f))
		}
		if b.mode == modeShadow {
			// Added fields go after the existing ones, in registration
			// order.
			seq++
			key = append(key, 1<<20+seq)
		} else {
			key = append(key, b.indexOf(b.field))
		}
		keys[b] = key
	}
	byCmd := map[*clause][]*binding{}
	for _, b := range c.plan.bindings {
		if _, ok := keys[b]; ok {
			byCmd[b.cmd] = append(byCmd[b.cmd], b)
		}
	}
	for cmd, list := range byCmd {
		sorted := true
		for i := 1; i < len(list); i++ {
			if lessKey(keys[list[i]], keys[list[i-1]]) {
				sorted = false
			}
		}
		if sorted || c.reorderArgs(list) {
			continue
		}
		c.issue(cmd.pos, "command %q: kong reads positional arguments in field order, which differs from the order they were registered in; reorder the fields", cmd.fullName())
	}
}

// reorderArgs puts a command's argument fields in registration order, when
// they are plain fields of one struct that no unkeyed literal builds.
// Other fields stay where they are.
func (c *converter) reorderArgs(list []*binding) bool {
	var st *dst.StructType
	var owner *types.Named
	var fields []*dst.Field
	for _, b := range list {
		if b.mode != modeDirect || len(b.path) > 0 {
			return false
		}
		loc, ok := c.ed.fieldDecl(b.field)
		if !ok || st != nil && loc.st != st {
			return false
		}
		st = loc.st
		owner = c.nm.declaringType(b.field)
		fields = append(fields, loc.field)
	}
	if st == nil || owner == nil || c.unkeyedLiterals(owner) || c.convertedStruct(owner) {
		return false
	}
	isArg := map[*dst.Field]bool{}
	for _, f := range fields {
		isArg[f] = true
	}
	next := 0
	for i, f := range st.Fields.List {
		if isArg[f] {
			moved := fields[next]
			moved.Decs, f.Decs = f.Decs, moved.Decs
			st.Fields.List[i] = moved
			next++
		}
	}
	loc, _ := c.ed.fieldDecl(list[0].field)
	c.ed.touch(loc.pkg, loc.file)
	return true
}

// unkeyedLiterals reports whether a composite literal of t lists its fields
// by position somewhere, which adding or reordering fields would break.
func (c *converter) unkeyedLiterals(t *types.Named) bool {
	return c.prog.unkeyedLiterals(t)
}

func lessKey(a, b []int) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) < len(b)
}

// indexOf returns the position of a field in the struct declaring it.
func (b *binding) indexOf(f *types.Var) int {
	if b.holder != nil && b.holder.st != nil {
		for i := 0; i < b.holder.st.NumFields(); i++ {
			if b.holder.st.Field(i) == f {
				return i
			}
		}
	}
	if f.Pkg() != nil {
		if p := f.Pkg().Scope(); p != nil {
			return int(f.Pos())
		}
	}
	return 0
}

// markFailures adds a TODO with the original kingpin code next to the
// destination of every registration the conversion could not handle.
// Otherwise that code would be lost along with its constructor.
func (c *converter) markFailures() {
	seen := map[*types.Var]bool{}
	for _, f := range c.plan.failed {
		field := c.failureField(f.cl)
		if field == nil || seen[field] {
			continue
		}
		seen[field] = true
		loc, ok := c.ed.fieldDecl(field)
		if !ok {
			continue
		}
		quoted := "(unknown)"
		if f.cl.stmt != nil {
			quoted = render(f.cl.stmt)
		}
		reason := f.reason
		if i := strings.Index(reason, ": "); i >= 0 {
			reason = reason[i+2:]
		}
		loc.field.Decs.Start.Append("// TODO: king2kong could not convert this flag (" + reason + ").")
		loc.field.Decs.Start.Append("// The kingpin registration was:")
		loc.field.Decs.Start.Append("// " + quoted)
		loc.field.Decs.Before = dst.NewLine
		c.ed.touch(loc.pkg, loc.file)
	}
}

// failureField returns the field of the command struct a failed registration
// wrote to: the destination itself, or the struct field holding it.
func (c *converter) failureField(cl *clause) *types.Var {
	o, f, ok := fieldOf(cl.dst)
	if !ok {
		return nil
	}
	for o.parent != nil && o.pfield != nil && o.parent.parent != nil {
		o = o.parent
	}
	if o.parent != nil && o.pfield != nil {
		return o.pfield
	}
	return f
}

// convertEnumChecks writes the checks of enums kong cannot check itself,
// worded as kingpin's errors were.
func (c *converter) convertEnumChecks() {
	type checkKey struct {
		host *types.Named
		val  string
	}
	done := map[checkKey]bool{}
	for _, b := range c.plan.bindings {
		if !b.enumInHook() {
			continue
		}
		if b.inst == nil {
			c.issue(b.cl.pos, "flag %q: its enum values are only known at run time, which the tool checks in a command's hook, and it has none; check the value by hand", b.cl.name)
			continue
		}
		host, rest := c.host(b)
		if host == nil {
			continue
		}
		h := c.hookFor(host)
		var val string
		switch {
		case b.mode == modeShadow && b.shadowField != "":
			val = "c." + b.shadowField
			if b.keepPrior {
				c.issue(b.cl.pos, "flag %q: cannot check its enum values", b.cl.name)
				continue
			}
		default:
			val = c.relativeExpr(rest, b)
		}
		if b.wrapper != nil {
			val += "." + b.wrapper.value.Name()
		}
		if done[checkKey{host, val}] {
			continue
		}
		done[checkKey{host, val}] = true
		var list, shown string
		if v := b.cl.enumVar; v != nil {
			switch {
			case v.Pkg() == host.Obj().Pkg():
				list = v.Name()
			case v.Exported():
				list = importName(h.imports, v.Pkg().Path(), v.Pkg().Name()) + "." + v.Name()
			default:
				c.issue(b.cl.pos, "flag %q: its enum options are not visible from %s", b.cl.name, host.Obj().Name())
				continue
			}
			shown = ""
		} else {
			quoted := make([]string, len(b.cl.enum))
			for i, e := range b.cl.enum {
				quoted[i] = fmt.Sprintf("%q", e)
			}
			list = "[]string{" + strings.Join(quoted, ", ") + "}"
			shown = strings.Join(b.cl.enum, ",")
		}
		h.imports["slices"] = "slices"
		h.imports["fmt"] = "fmt"
		// errorf builds kingpin's error for a value outside the enum.
		errorf := func(v string) string {
			if shown != "" {
				return "fmt.Errorf(" + strconv.Quote("enum value must be one of "+shown+", got '%s'") + ", " + v + ")"
			}
			h.imports["strings"] = "strings"
			return "fmt.Errorf(" + strconv.Quote("enum value must be one of %s, got '%s'") + ", strings.Join(" + list + `, ","), ` + v + ")"
		}
		if c.plan.isSliceDest(b) {
			h.checks = append(h.checks, "for _, v := range "+val+" {\nif !slices.Contains("+list+", v) {\nreturn "+errorf("v")+"\n}\n}")
			continue
		}
		guard := ""
		if !b.cl.required {
			guard = val + ` != "" && `
		}
		h.checks = append(h.checks, "if "+guard+"!slices.Contains("+list+", "+val+") {\nreturn "+errorf(val)+"\n}")
	}
}

// relativeExpr renders the destination of a shadow relative to its host's
// receiver, which is spelled "c" here and renamed when the hook is written.
func (c *converter) relativeExpr(rest []step, b *binding) string {
	if b.dstVar != nil {
		return b.dstVar.Pkg().Name() + "." + b.dstVar.Name()
	}
	parts := []string{"c"}
	for _, s := range rest {
		if s.f.Anonymous() {
			continue
		}
		parts = append(parts, c.finalName(s.f))
	}
	parts = append(parts, c.finalName(b.field))
	return strings.Join(parts, ".")
}

// markStmt returns the statement setting the marker that a flag's action set
// when the flag was given, or "" if there is none.
func (c *converter) markStmt(b *binding) string {
	if b.marks == nil {
		return ""
	}
	steps, ok := pathFrom(b.inst, b.marks.recv)
	if !ok {
		c.issue(b.cl.pos, "flag %q: its action marks a value the command cannot reach", b.cl.name)
		return ""
	}
	parts := []string{"c"}
	for _, s := range steps {
		if !s.f.Anonymous() {
			parts = append(parts, c.finalName(s.f))
		}
	}
	c.res.Notes = append(c.res.Notes, fmt.Sprintf("flag %q (%s) marks %s as given rather than its own value, as kingpin did with its Action; this looks like a mistake in the program.",
		b.cl.name, b.cl.pos, strings.Join(parts[1:], ".")))
	return fmt.Sprintf("\n%s.%s = true", strings.Join(parts, "."), c.finalName(b.marks.field))
}

func (c *converter) finalName(f *types.Var) string {
	if n, ok := c.nm.final[f]; ok {
		return n
	}
	return f.Name()
}

func (c *converter) rename(f *types.Var, name string) {
	if name == f.Name() || name == "" {
		return
	}
	c.ed.renameField(f, name)
	c.renames[f] = name
	c.res.Stats.Renamed++
}

// addShadow adds a tagged field to host and the statement copying it to the
// real destination.
func (c *converter) addShadow(host *types.Named, name, dstExpr string, b *binding) {
	pkg := host.Obj().Pkg()
	h := c.hookFor(host)
	var dt types.Type
	switch {
	case b.dstVar != nil:
		dt = b.dstVar.Type()
	case b.cell:
		p, ok := b.field.Type().Underlying().(*types.Pointer)
		if !ok {
			c.issue(b.cl.pos, "flag %q: unexpected destination type", b.cl.name)
			return
		}
		dt = p.Elem()
	default:
		dt = b.field.Type()
	}
	if b.valueOfWrapper {
		dt = b.holder.typ.Underlying().(*types.Struct).Field(fieldIndex(b.holder.st, b.field)).Type()
	}
	_, isSlice := dt.Underlying().(*types.Slice)
	_, isMap := dt.Underlying().(*types.Map)
	fieldType := typeExpr(dt, pkg, h.imports)
	switch {
	case b.cell:
		h.after = append(h.after, fmt.Sprintf("%s = &c.%s", dstExpr, name))
	case b.keepPrior && (isSlice || isMap):
		h.after = append(h.after, fmt.Sprintf("if c.%s != nil {\n%s = c.%s%s\n}", name, dstExpr, name, c.markStmt(b)))
	case b.keepPrior:
		fieldType = "*" + fieldType
		h.after = append(h.after, fmt.Sprintf("if c.%s != nil {\n%s = *c.%s%s\n}", name, dstExpr, name, c.markStmt(b)))
	default:
		h.after = append(h.after, fmt.Sprintf("%s = c.%s", dstExpr, name))
	}
	tag := c.plan.tagFor(b, name)
	decls, err := snippet(pkg.Name(), h.imports, fmt.Sprintf("type _ struct {\n%s %s %s\n}", name, fieldType, tagLiteral(tag.String())))
	if err != nil {
		c.issue(b.cl.pos, "%v", err)
		return
	}
	st := decls[0].(*dst.GenDecl).Specs[0].(*dst.TypeSpec).Type.(*dst.StructType)
	field := st.Fields.List[0]
	field.Decs = dst.FieldDecorations{}
	if c.unkeyedLiterals(host) {
		c.issue(b.cl.pos, "%s gets a field for flag %q, so its unkeyed composite literals need keys; add them by hand", host.Obj().Name(), b.cl.name)
	}
	if !c.ed.appendFields(host, []*dst.Field{field}) {
		c.issue(b.cl.pos, "cannot find the declaration of %s", host.Obj().Name())
		return
	}
	c.res.Stats.Shadows++
}

func fieldIndex(st *types.Struct, f *types.Var) int {
	for i := 0; i < st.NumFields(); i++ {
		if st.Field(i) == f {
			return i
		}
	}
	return 0
}

// convertWrappers makes the was-set wrapper types decode kong values and
// moves the "was set" marking into a hook.
func (c *converter) convertWrappers() {
	var names []*types.Named
	for n := range c.plan.wrappers {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool { return names[i].Obj().Name() < names[j].Obj().Name() })
	markers := map[*types.Named]*wrapperType{}
	for _, n := range names {
		w := c.plan.wrappers[n]
		markers[w.marker] = w
		c.addDecode(w)
	}
	for m, w := range markers {
		c.addMarkerHook(m, w)
	}
}

func (c *converter) addDecode(w *wrapperType) {
	loc, ok := c.ed.typeSpecs[w.named.Obj()]
	if !ok {
		c.issue(token.Position{}, "cannot find the declaration of %s", w.named.Obj().Name())
		return
	}
	if c.hasMethod(w.named, "Decode") {
		return
	}
	recv := c.methodRecv(w.named, "o")
	t := recvType(w.named)
	v := w.value.Name()
	imports := map[string]string{"github.com/alecthomas/kong": "kong"}
	var body string
	switch u := w.value.Type().Underlying().(type) {
	case *types.Basic:
		switch {
		case u.Kind() == types.String:
			body = fmt.Sprintf(`// Decode implements kong.MapperValue, so the flag value lands in %[3]s.
func (%[1]s *%[2]s) Decode(ctx *kong.DecodeContext) error {
	return ctx.Scan.PopValueInto("value", &%[1]s.%[3]s)
}`, recv, t, v)
		case u.Kind() == types.Bool:
			imports["strconv"] = "strconv"
			imports["fmt"] = "fmt"
			given := "true"
			if c.negatedNoFlag(w) {
				imports["strings"] = "strings"
				given = `!strings.HasPrefix(ctx.Value.Name, "no-")`
			}
			doc, valueless := "The flag takes no value, but --flag=false is still understood.", "ctx.Scan.Peek().Type != kong.FlagValueToken"
			if c.wrapsArgument(w) {
				doc = "A flag takes no value, but --flag=false is still understood, and an\n// argument is true or false."
				valueless = "ctx.Value.Flag != nil && " + valueless
			}
			body = fmt.Sprintf(`// Decode implements kong.MapperValue, so the value lands in %[3]s.
// %[6]s%[4]s
func (%[1]s *%[2]s) Decode(ctx *kong.DecodeContext) error {
	if %[7]s {
		%[1]s.%[3]s = %[5]s
		return nil
	}
	switch v := ctx.Scan.Pop().Value.(type) {
	case bool:
		%[1]s.%[3]s = v
	case string:
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("expected a boolean but got %%q", v)
		}
		%[1]s.%[3]s = b
	}
	return nil
}

// IsBool tells kong the flag does not take a value.
func (%[1]s *%[2]s) IsBool() bool {
	return true
}`, recv, t, v, negatedNote(given != "true"), given, doc, valueless)
		case u.Info()&types.IsNumeric != 0:
			imports["strconv"] = "strconv"
			body = numericDecode(recv, t, v, u, typeExpr(w.value.Type(), w.named.Obj().Pkg(), imports))
		}
	case *types.Slice:
		if b, ok := u.Elem().Underlying().(*types.Basic); ok && b.Kind() == types.String {
			imports["strings"] = "strings"
			body = fmt.Sprintf(`// Decode implements kong.MapperValue, so the flag values accumulate in
// %[3]s.
// A value is split on the separator given by the field's sep tag.
func (%[1]s *%[2]s) Decode(ctx *kong.DecodeContext) error {
	var s string
	if err := ctx.Scan.PopValueInto("value", &s); err != nil {
		return err
	}
	if sep := ctx.Value.Tag.Sep; sep != -1 {
		%[1]s.%[3]s = append(%[1]s.%[3]s, strings.Split(s, string(sep))...)
	} else {
		%[1]s.%[3]s = append(%[1]s.%[3]s, s)
	}
	return nil
}

// IsCumulative tells kongcompat the flag may be repeated, as kingpin allowed.
func (%[1]s *%[2]s) IsCumulative() bool {
	return true
}`, recv, t, v)
		}
	}
	if body == "" {
		c.issue(token.Position{}, "was-set wrapper %s has a value type the tool cannot decode", t)
		return
	}
	decls, err := snippet(loc.pkg.Name, imports, body)
	if err != nil {
		c.issue(token.Position{}, "%v", err)
		return
	}
	c.ed.insertAfter(loc.pkg, loc.file, c.lastMethodOrType(w.named, loc.gen), decls)
}

// numericDecode is the Decode method of a was-set wrapper holding a number,
// parsed as kingpin's value of that type did, with kingpin's errors.
func numericDecode(recv, typeName, field string, u *types.Basic, typ string) string {
	bits := map[types.BasicKind]int{
		types.Int8: 8, types.Int16: 16, types.Int32: 32,
		types.Uint8: 8, types.Uint16: 16, types.Uint32: 32, types.Float32: 32,
	}[u.Kind()]
	if bits == 0 {
		bits = 64
	}
	var parse, note string
	switch {
	case u.Kind() == types.Int:
		parse = "strconv.ParseFloat(s, 64)"
		note = "\n// kingpin parsed an int as a float and dropped the fraction."
	case u.Info()&types.IsFloat != 0:
		parse = fmt.Sprintf("strconv.ParseFloat(s, %d)", bits)
	case u.Info()&types.IsUnsigned != 0:
		parse = fmt.Sprintf("strconv.ParseUint(s, 0, %d)", bits)
	default:
		parse = fmt.Sprintf("strconv.ParseInt(s, 0, %d)", bits)
	}
	value := typ + "(n)"
	if typ == "float64" || typ == "int64" || typ == "uint64" {
		value = "n"
	}
	return fmt.Sprintf(`// Decode implements kong.MapperValue, so the flag value lands in %[3]s.%[6]s
func (%[1]s *%[2]s) Decode(ctx *kong.DecodeContext) error {
	var s string
	if err := ctx.Scan.PopValueInto("value", &s); err != nil {
		return err
	}
	n, err := %[4]s
	if err != nil {
		return err
	}
	%[1]s.%[3]s = %[5]s
	return nil
}`, recv, typeName, field, parse, value, note)
}

// wrapsArgument reports whether a positional argument lands in wrapper w.
func (c *converter) wrapsArgument(w *wrapperType) bool {
	return slices.ContainsFunc(c.plan.bindings, func(b *binding) bool { return b.wrapper == w && b.cl.kind == kindArg })
}

// negatedNoFlag reports whether a boolean flag named no-x lands in wrapper w
// of a kingpin v3 program.
// kingpin v3 set such a flag to false when it was given.
func (c *converter) negatedNoFlag(w *wrapperType) bool {
	if !c.prog.kingpinV3 {
		return false
	}
	for _, b := range c.plan.bindings {
		if b.wrapper == w && strings.HasPrefix(b.cl.name, "no-") {
			return true
		}
	}
	return false
}

func negatedNote(negated bool) string {
	if !negated {
		return ""
	}
	return "\n// As in kingpin v3, a flag named no-x given alone is false."
}

// addMarkerHook replaces the kingpin Set action with a kong hook on the type
// that records whether a flag was given.
func (c *converter) addMarkerHook(m *types.Named, w *wrapperType) {
	loc, ok := c.ed.typeSpecs[m.Obj()]
	if !ok || c.hasMethod(m, "AfterApply") || c.hasMethod(w.named, "Decode") {
		return
	}
	recv := c.methodRecv(m, "o")
	imports := map[string]string{"github.com/alecthomas/kong": "kong", "slices": "slices"}
	body := fmt.Sprintf(`// AfterApply records that the flag was given on the command line.
// Kong also calls it for flags that only received a default or an environment
// variable, which do not count, hence the check of the parse path.
func (%[1]s *%[2]s) AfterApply(ctx *kong.Context, path *kong.Path) error {
	if !path.Resolved && slices.Contains(ctx.Path, path) {
		%[1]s.%[3]s = true
	}
	return nil
}`, recv, m.Obj().Name(), w.markField.Name())
	decls, err := snippet(loc.pkg.Name, imports, body)
	if err != nil {
		c.issue(token.Position{}, "%v", err)
		return
	}
	anchor := dst.Decl(loc.gen)
	if fd, ok := c.ed.methods[m][w.set.Name()]; ok {
		c.ed.replaceDecl(fd.pkg, fd.file, fd.decl, decls)
		c.ed.removed[fd.decl] = true
		return
	}
	c.ed.insertAfter(loc.pkg, loc.file, anchor, decls)
}

// hasMethod reports whether t or *t declares or promotes a method.
func (c *converter) hasMethod(t *types.Named, name string) bool {
	obj, _, _ := types.LookupFieldOrMethod(types.NewPointer(t), true, t.Obj().Pkg(), name)
	_, isFunc := obj.(*types.Func)
	return isFunc
}

// recvType is t's name as a method receiver type, with its type parameters.
func recvType(t *types.Named) string {
	params := t.Origin().TypeParams()
	if params.Len() == 0 {
		return t.Obj().Name()
	}
	var names []string
	for i := range params.Len() {
		names = append(names, params.At(i).Obj().Name())
	}
	return t.Obj().Name() + "[" + strings.Join(names, ", ") + "]"
}

// templateNames are the names used inside the generated Decode and
// AfterApply methods.
var templateNames = map[string]bool{
	"ctx": true, "path": true, "value": true, "s": true, "v": true, "b": true, "n": true, "sep": true, "err": true,
	"kong": true, "fmt": true, "strconv": true, "strings": true, "slices": true,
}

// methodRecv is receiverName for a generated method, avoiding the names
// the method uses inside.
func (c *converter) methodRecv(t *types.Named, fallback string) string {
	if name := c.receiverName(t, fallback); !templateNames[name] {
		return name
	}
	return firstFree(func(name string) bool { return templateNames[name] || isTypeParam(t, name) }, strings.ToLower(t.Obj().Name()[:1]), "o", "x", "recv")
}

// receiverName returns the receiver name most of t's methods already use, or
// fallback.
func (c *converter) receiverName(t *types.Named, fallback string) string {
	names := map[string]int{}
	for _, fd := range c.ed.methods[t] {
		if r := fd.decl.Recv.List[0]; len(r.Names) == 1 && r.Names[0].Name != "_" {
			names[r.Names[0].Name]++
		}
	}
	best, n := fallback, 0
	for name, k := range names {
		// A receiver named string or len would hide them in generated code.
		if types.Universe.Lookup(name) != nil {
			continue
		}
		if k > n || k == n && name < best {
			best, n = name, k
		}
	}
	// It cannot be one of t's type parameters, as in func (c *Cmd[c]).
	return firstFree(func(name string) bool { return isTypeParam(t, name) }, best, "cmd", "command")
}

func isTypeParam(t *types.Named, name string) bool {
	params := t.Origin().TypeParams()
	for i := range params.Len() {
		if params.At(i).Obj().Name() == name {
			return true
		}
	}
	return false
}

// lastMethodOrType returns the declaration after which new methods of t read
// best: its last method in the file declaring the type, or the type itself.
func (c *converter) lastMethodOrType(t *types.Named, gen *dst.GenDecl) dst.Decl {
	loc := c.ed.typeSpecs[t.Obj()]
	var last dst.Decl = gen
	seen := false
	for _, d := range loc.file.Decls {
		if d == dst.Decl(gen) {
			seen = true
			continue
		}
		if !seen {
			continue
		}
		fd, ok := d.(*dst.FuncDecl)
		if !ok || fd.Recv == nil {
			continue
		}
		if fn, ok := loc.pkg.objectOf(fd.Name).(*types.Func); ok {
			if n, ok := deref(fn.Type().(*types.Signature).Recv().Type()).(*types.Named); ok && n == t {
				last = d
			}
		}
	}
	return last
}

// convertConstructors removes the functions that built commands, moving what
// they did besides registration into hooks.
func (c *converter) convertConstructors() {
	// Values every command receives, such as shared configuration, are injected
	// once through a hook on the type declaring the field.
	type inj struct {
		field *types.Var
		param *types.Var
		ci    *ctorInfo
	}
	injections := map[*types.Var][]inj{}
	var fns []*types.Func
	for fn := range c.ctors {
		fns = append(fns, fn)
	}
	sort.Slice(fns, func(i, j int) bool { return fns[i].FullName() < fns[j].FullName() })
	// Commands sharing a type share its hook, so their constructors must set
	// it up the same way.
	setups := map[*types.Named]*ctorInfo{}
	setup := func(ci *ctorInfo) string {
		var lines []string
		for _, it := range ci.items {
			if it.kind == ctorResidue || it.kind == ctorCrossRef {
				lines = append(lines, it.text)
			}
		}
		return strings.Join(lines, "\n")
	}
	for _, fn := range fns {
		ci := c.ctors[fn]
		ok := true
		if other := setups[ci.inst]; other != nil && setup(other) != setup(ci) {
			c.issue(ci.decl.pkg.position(ci.decl.decl), "constructors %s and %s set up %s differently, but the commands share one hook; set them up by hand", other.fn.Name(), fn.Name(), ci.inst.Obj().Name())
			ok = false
		} else if other == nil {
			setups[ci.inst] = ci
		}
		deletable, _ := c.deletableStmts(ci.decl.pkg, ci.decl.decl.Body)
		// The hook sets injected fields after the other ones, so a field
		// set both ways would end with the wrong value.
		set := map[*types.Var]ctorKind{}
		for _, it := range ci.items {
			if it.field == nil || it.kind != ctorInjection && it.kind != ctorResidue {
				continue
			}
			if k, seen := set[it.field]; seen && k != it.kind {
				c.issue(ci.decl.pkg.position(ci.decl.decl), "constructor %s sets %s both from a parameter and otherwise, which the hook cannot keep in order", fn.Name(), it.field.Name())
				ok = false
			}
			set[it.field] = it.kind
		}
		for i, it := range ci.items {
			if it.kind == ctorResidue && it.stmt != nil && it.field == nil && deletable[it.stmt] {
				// Registration through a local clause, such as
				// clause := parent.Command(...).
				ci.items[i].kind = ctorRegistration
				it.kind = ctorRegistration
			}
			if it.kind == ctorRegistration && !deletable[it.stmt] {
				c.issue(ci.decl.pkg.position(it.stmt), "statement mixes kingpin registration with other work: %s", render(it.stmt))
				ok = false
			}
			switch it.kind {
			case ctorInjection:
				injections[it.field] = append(injections[it.field], inj{it.field, it.param, ci})
			case ctorEmbed:
				c.valueEmbeds[it.field] = true
			case ctorResidue, ctorCrossRef:
				if !c.residueHook(ci, it) {
					ok = false
				}
			}
		}
		if !ok {
			c.issue(ci.decl.pkg.position(ci.decl.decl), "constructor %s does work the tool cannot move into a hook; it is kept", fn.Name())
			continue
		}
		h := c.hookFor(ci.inst)
		if h.anchor == nil {
			fd := ci.decl
			h.anchor = &fd
		}
		if c.testRefs.mentions(filepath.Dir(ci.decl.pkg.filename(ci.decl.file)), ci.decl.pkg.PkgPath, fn.Name(), false) {
			// Tests use it to build the command, so keep it and only strip
			// its registrations with the rest of the wiring.
			c.keepStructure(ci)
			c.keptCtors = append(c.keptCtors, ci)
			c.redocKeptCtor(ci)
			continue
		}
		c.ed.removeDecl(ci.decl.pkg, ci.decl.file, ci.decl.decl)
		c.removedCtors[fn] = true
		c.res.Stats.RemovedFuncs++
	}
	// Fields in source order, so hooks and bindings come out the same each
	// run.
	byPos := func(a, b *types.Var) int { return cmp.Compare(a.Pos(), b.Pos()) }
	for _, f := range slices.SortedFunc(maps.Keys(c.valueEmbeds), byPos) {
		c.embedByValue(f)
	}
	// callArg is what a call passed to an injected parameter.
	type callArg struct {
		expr dst.Expr
		v    value
		pkg  *Package
		fn   *types.Func
	}
	argsOf := func(list []inj) []callArg {
		var out []callArg
		for _, in := range list {
			idx, ok := in.ci.params[in.param]
			if !ok {
				continue
			}
			for _, s := range c.g.in.sites {
				if s.callee != in.ci.fn || idx >= len(s.call.Args) {
					continue
				}
				a := callArg{s.call.Args[idx], vUnknown{}, s.pkg, in.ci.fn}
				if idx < len(s.args) {
					a.v = s.args[idx]
				}
				out = append(out, a)
			}
		}
		return out
	}
	// kong gives every hook the same value for a type, so all constructors
	// must get the same one.
	// Main mode checks this itself, further down.
	firstGiven := map[string]callArg{}
	clashes := func(f *types.Var, list []inj, args []callArg) bool {
		t := list[0].param.Type()
		key := types.TypeString(t, nil)
		for _, a := range args {
			prev, seen := firstGiven[key]
			if !seen {
				firstGiven[key] = a
				continue
			}
			if eq, known := equalValues(prev.v, a.v); eq || !known {
				continue
			}
			who := "constructors " + prev.fn.Name() + " and " + a.fn.Name() + " get"
			if prev.fn == a.fn {
				who = "constructor " + a.fn.Name() + " gets"
			}
			c.issue(list[0].ci.decl.pkg.position(list[0].ci.decl.decl), "%s different %s values, but kong hands every hook the one value bound for its type; set %s by hand", who, types.TypeString(t, shortQual), c.finalName(f))
			return true
		}
		return false
	}
	// kong can't pass a nil interface to a hook.
	// If every caller passes nil, the field is simply not set, since it
	// starts out nil.
	// A nil pointer inside an interface is bound like any value.
	nilInterface := func(f *types.Var, list []inj, args []callArg, h *hookSet, pkg *types.Package) bool {
		t := list[0].param.Type()
		if !types.IsInterface(t) {
			return false
		}
		nils := 0
		for _, a := range args {
			if _, isNil := a.v.(vNil); isNil {
				if at := a.pkg.typeOf(a.expr); at == nil || types.IsInterface(at) || at == types.Typ[types.UntypedNil] {
					nils++
				}
			}
		}
		switch {
		case nils == 0:
			return false
		case nils < len(args):
			c.issue(list[0].ci.decl.pkg.position(list[0].ci.decl.decl), "constructor %s is sometimes given a nil %s, which kong can't hand to a hook; set %s by hand", list[0].ci.fn.Name(), types.TypeString(t, shortQual), c.finalName(f))
		default:
			// Setup moved from the constructor may still read it, as in
			// quiet: out == nil.
			name, typ := paramName(list[0].param.Name()), typeExpr(t, pkg, h.imports)
			h.paramToLocal(name, typ, fmt.Sprintf("var %s %s", name, typ))
		}
		return true
	}
	for _, f := range slices.SortedFunc(maps.Keys(injections), byPos) {
		list := injections[f]
		owner := c.nm.declaringType(f)
		if owner == nil {
			continue
		}
		h := c.hookFor(owner)
		pkg := owner.Obj().Pkg()
		given := argsOf(list)
		if nilInterface(f, list, given, h, pkg) || c.main == nil && clashes(f, list, given) {
			continue
		}
		param := list[0].param
		if c.main != nil {
			// When main parses the command line, the hook uses the constant
			// the constructor was called with, or binds the variable it came
			// from where the parser is built.
			var args []dst.Expr
			var argPkg *Package
			for _, a := range given {
				args, argPkg = append(args, a.expr), a.pkg
			}
			if lit, t, ok := sameConstant(argPkg, args); ok {
				// A constant of a named type, as time.Second, keeps its type.
				n := naturalType(argPkg, args[0])
				if _, basic := t.(*types.Basic); t != nil && !basic {
					lit = typeExpr(t, pkg, h.imports) + "(" + lit + ")"
					n = t
				}
				// Setup moved from the constructor may still read the
				// parameter, as in prefix: strings.ToUpper(region), so it
				// becomes a local.
				name, typ := paramName(param.Name()), typeExpr(param.Type(), pkg, h.imports)
				decl := fmt.Sprintf("var %s %s = %s", name, typ, lit)
				if n != nil && types.Identical(n, param.Type()) {
					decl = fmt.Sprintf("%s := %s", name, lit)
				}
				if h.paramToLocal(name, typ, decl) {
					lit = name
				}
				h.before = append(h.before, fmt.Sprintf("%s.%s = %s", c.recvPlaceholder, c.finalName(f), lit))
				continue
			}
			v, ok := sameVariable(argPkg, args)
			// kong gives a hook one value per type, and that type may be
			// an interface the variable implements.
			var other *types.Var
			again := false
			for _, b := range c.main.bound {
				if ok && types.Identical(b.as, param.Type()) {
					if b.v == v {
						again = true
					} else {
						other = b.v
					}
				}
			}
			switch {
			case ok && other != nil:
				c.issue(list[0].ci.decl.pkg.position(list[0].ci.decl.decl), "constructor %s stores %s, while another one stores %s of the same type, which kong cannot tell apart; set %s by hand", list[0].ci.fn.Name(), v.Name(), other.Name(), c.finalName(f))
				continue
			case ok && c.visibleAtParser(v):
				if !again {
					c.main.bound = append(c.main.bound, boundVar{v, param.Type()})
				}
			default:
				c.issue(list[0].ci.decl.pkg.position(list[0].ci.decl.decl), "constructor %s stores its %s parameter, which no single constant or variable visible where the parser is built provides; set %s by hand", list[0].ci.fn.Name(), param.Name(), c.finalName(f))
				continue
			}
		}
		// kong passes hook values by type, so two values of the same type
		// can't be told apart.
		typ := typeExpr(param.Type(), pkg, h.imports)
		if other := h.paramOfType(typ); other != "" && other != paramName(param.Name()) {
			c.issue(list[0].ci.decl.pkg.position(list[0].ci.decl.decl), "constructor %s stores two %s values, %s and %s, which kong cannot tell apart; set %s by hand", list[0].ci.fn.Name(), typ, other, param.Name(), c.finalName(f))
			continue
		}
		pname := h.param(paramName(param.Name()), typ)
		h.before = append(h.before, fmt.Sprintf("%s.%s = %s", c.recvPlaceholder, c.finalName(f), pname))
		if c.main == nil {
			c.addInjected(param.Type())
		}
	}
}

// addInjected records a type the hooks take, which the parser has to bind.
func (c *converter) addInjected(t types.Type) {
	for _, seen := range c.injected {
		if types.Identical(seen, t) {
			return
		}
	}
	c.injected = append(c.injected, t)
}

// sameConstant returns the source of args when they are all the same
// constant, and its type when that source is a value rather than a literal.
func sameConstant(p *Package, args []dst.Expr) (string, types.Type, bool) {
	if len(args) == 0 || p == nil {
		return "", nil, false
	}
	first := ""
	var t types.Type
	for i, a := range args {
		tv, ok := p.tv(a)
		if !ok || tv.Value == nil {
			return "", nil, false
		}
		// The hook may live in another package, which can't see a named
		// constant of the caller, so only literals are copied as written.
		s := renderExact(a)
		if _, lit := unparen(a).(*dst.BasicLit); !lit {
			s, t = constLiteral(tv.Value), tv.Type
		}
		if i == 0 {
			first = s
		} else if s != first {
			return "", nil, false
		}
	}
	return first, t, true
}

// constLiteral writes a constant value as Go source.
func constLiteral(v constant.Value) string {
	switch v.Kind() {
	case constant.String:
		return strconv.Quote(constant.StringVal(v))
	case constant.Float:
		if n := constant.ToInt(v); n.Kind() == constant.Int {
			return n.ExactString()
		}
		f, _ := constant.Float64Val(v)
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
	return v.ExactString()
}

// sameVariable returns the variable all args name, if they name one.
func sameVariable(p *Package, args []dst.Expr) (*types.Var, bool) {
	if len(args) == 0 || p == nil {
		return nil, false
	}
	var v *types.Var
	for _, a := range args {
		id, ok := a.(*dst.Ident)
		if !ok {
			return nil, false
		}
		av, ok := p.objectOf(id).(*types.Var)
		if !ok || v != nil && av != v {
			return nil, false
		}
		v = av
	}
	return v, true
}

// visibleAtParser reports whether v can be named where kingpin.New was
// called, which is where the kong parser is built.
func (c *converter) visibleAtParser(v *types.Var) bool {
	if v.Parent() == v.Pkg().Scope() {
		return v.Pkg() == c.tree.RootPkg.Types
	}
	root := c.tree.RootPkg
	visible := false
	for _, f := range root.Syntax {
		dst.Inspect(f, func(n dst.Node) bool {
			call, ok := n.(*dst.CallExpr)
			if !ok || c.runtimeCall(root, call) != "New" {
				return true
			}
			if a := root.astOf(call); a != nil {
				if scope := root.Types.Scope().Innermost(a.Pos()); scope != nil {
					if _, obj := scope.LookupParent(v.Name(), a.Pos()); obj == v {
						visible = true
					}
				}
			}
			return true
		})
	}
	return visible
}

func paramName(n string) string {
	if n == "" || n == "_" {
		return "v"
	}
	return n
}

// residueHook moves a constructor initialization into a BeforeApply hook.
func (c *converter) residueHook(ci *ctorInfo, it ctorItem) bool {
	if it.field == nil || it.expr == nil {
		return false
	}
	h := c.hookFor(ci.inst)
	pkg := ci.inst.Obj().Pkg()
	// A hook only sees injected parameters.
	// A command handed to another one, like a deploy command keeping the
	// build command it runs first, is rebuilt the way its own constructor
	// built it.
	if it.kind == ctorCrossRef {
		target, ok := deref(it.param.Type()).(*types.Named)
		if !ok {
			return false
		}
		var tctor *ctorInfo
		for _, other := range c.ctors {
			if other.inst == target {
				tctor = other
			}
		}
		if tctor == nil {
			return false
		}
		tname := typeExpr(target, pkg, h.imports)
		field := c.recvPlaceholder + "." + c.itemPath(it)
		h.before = append(h.before, fmt.Sprintf("%s = &%s{}", field, tname))
		for _, ti := range tctor.items {
			switch ti.kind {
			case ctorInjection:
				pname := h.param(paramName(ti.param.Name()), typeExpr(ti.param.Type(), pkg, h.imports))
				if c.main == nil {
					c.addInjected(ti.param.Type())
				}
				h.before = append(h.before, fmt.Sprintf("%s.%s = %s", field, c.finalName(ti.field), pname))
			case ctorResidue, ctorCrossRef:
				return false
			}
		}
		return true
	}
	// A residue may only use the constructor parameters that are injected.
	ok := true
	dst.Inspect(it.expr, func(n dst.Node) bool {
		id, isIdent := n.(*dst.Ident)
		if !isIdent || id.Path != "" {
			return true
		}
		v, isVar := ci.decl.pkg.objectOf(id).(*types.Var)
		if !isVar {
			return true
		}
		if _, isParam := ci.params[v]; isParam {
			if !c.isInjected(ci, v) {
				ok = false
			}
			// The residue keeps the parameter's name, so it cannot be renamed.
			name, typ := paramName(v.Name()), typeExpr(v.Type(), pkg, h.imports)
			if t, taken := h.beforeParams[name]; taken && t != typ {
				ok = false
			} else {
				h.beforeParams[name] = typ
				if c.main == nil {
					c.addInjected(v.Type())
				}
			}
			return true
		}
		if v.Parent() != v.Pkg().Scope() && !v.IsField() {
			ok = false
		}
		return true
	})
	if !ok {
		return false
	}
	// What the hook now calls must survive dead code removal.
	dst.Inspect(it.expr, func(n dst.Node) bool {
		if id, ok := n.(*dst.Ident); ok {
			if o := ci.decl.pkg.objectOf(id); o != nil && o.Pkg() != nil && c.prog.owns(o.Pkg()) {
				c.keep[o] = true
			}
		}
		return true
	})
	if h.outer == nil {
		h.outer = map[string]bool{}
	}
	// The local becomes the receiver: renamed in place, by identity, just
	// for rendering.
	var renamed []*dst.Ident
	dst.Inspect(it.expr, func(n dst.Node) bool {
		id, ok := n.(*dst.Ident)
		if !ok || id.Path != "" {
			return true
		}
		switch o := ci.decl.pkg.objectOf(id).(type) {
		case nil:
		case *types.Var:
			if ci.local != nil && o == ci.local {
				renamed = append(renamed, id)
			} else if !o.IsField() {
				h.outer[id.Name] = true
			}
		case *types.Func:
			if o.Type().(*types.Signature).Recv() == nil {
				h.outer[id.Name] = true
			}
		default:
			h.outer[id.Name] = true
		}
		return true
	})
	for _, name := range h.imports {
		h.outer[name] = true
	}
	name := ""
	if len(renamed) > 0 {
		name = renamed[0].Name
	}
	for _, id := range renamed {
		id.Name = c.recvPlaceholder
	}
	expr := renderIn(it.expr, h.imports)
	for _, id := range renamed {
		id.Name = name
	}
	h.before = append(h.before, fmt.Sprintf("%s.%s = %s", c.recvPlaceholder, c.itemPath(it), expr))
	return true
}

// itemPath is the field a constructor item sets, from the command, as the
// conversion names it: Settings.Retries for d.Settings.Retries.
func (c *converter) itemPath(it ctorItem) string {
	path := it.path
	if len(path) == 0 {
		path = []*types.Var{it.field}
	}
	var names []string
	for _, f := range path {
		names = append(names, c.finalName(f))
	}
	return strings.Join(names, ".")
}

// redocKeptCtor rewrites the doc comment of a constructor kept for tests
// when it speaks of registering, which it no longer does.
func (c *converter) redocKeptCtor(ci *ctorInfo) {
	fd := ci.decl.decl
	doc := strings.ToLower(strings.Join(fd.Decs.Start, " "))
	if !strings.Contains(doc, "regist") && !strings.Contains(doc, "parent") {
		return
	}
	what := "command"
	if ci.inst != nil {
		what = ci.inst.Obj().Name()
	}
	fd.Decs.Start = dst.Decorations{fmt.Sprintf("// %s returns a %s set up as it was before parsing.", fd.Name.Name, what), "// Its flags now come from the struct's tags."}
	c.ed.touch(ci.decl.pkg, ci.decl.file)
}

func (c *converter) isInjected(ci *ctorInfo, v *types.Var) bool {
	for _, it := range ci.items {
		if it.kind == ctorInjection && it.param == v {
			return true
		}
	}
	return false
}

// embedByValue turns an embedded pointer into an embedded value, so kong
// allocates the wrapped command along with the wrapper.
func (c *converter) embedByValue(f *types.Var) {
	loc, ok := c.ed.fields[f]
	if !ok {
		return
	}
	if star, ok := loc.field.Type.(*dst.StarExpr); ok {
		loc.field.Type = star.X
		c.ed.touch(loc.pkg, loc.file)
	}
}

// renderIn prints an expression for use in generated code, recording the
// imports its package references need.
func renderIn(e dst.Expr, imports map[string]string) string {
	locals := map[string]bool{}
	// renamable are the names of e that e declares itself, in order of
	// appearance, which renaming may change; the others name fields or
	// things outside e.
	var renamable []bool
	declaredIn := func(id *dst.Ident) bool {
		p := renderPackage(id)
		if p == nil {
			return false
		}
		outer := p.astOf(e)
		o := p.objectOf(id)
		if o == nil || outer == nil {
			return false
		}
		if v, ok := o.(*types.Var); ok && v.IsField() {
			return false
		}
		return o.Pos() >= outer.Pos() && o.Pos() < outer.End()
	}
	dst.Inspect(e, func(n dst.Node) bool {
		if id, ok := n.(*dst.Ident); ok && id.Path == "" {
			locals[id.Name] = true
			renamable = append(renamable, declaredIn(id))
		}
		return true
	})
	// Each package gets a name of its own, crypto/rand and math/rand too,
	// the one the program gives it if any, and none of the names e uses.
	used := map[string]bool{}
	e = dstutil.Apply(dst.Clone(e), func(cur *dstutil.Cursor) bool {
		if id, ok := cur.Node().(*dst.Ident); ok && id.Path != "" {
			name := preferredAliases[id.Path]
			if name == "" {
				guessed, err := guess.New().ResolvePackage(id.Path)
				if err != nil {
					guessed = filepath.Base(id.Path)
				}
				name = guessed
			}
			name = importNameAvoiding(imports, id.Path, name, locals)
			used[name] = true
			cur.Replace(&dst.SelectorExpr{X: &dst.Ident{Name: name, Obj: importMarker}, Sel: &dst.Ident{Name: id.Name, Obj: importMarker}})
		}
		return true
	}, nil).(dst.Expr)
	// A package that already had its name in this file hides what e
	// declares under that name, which is renamed to a name nothing here
	// uses.
	taken := func(n string) bool {
		if locals[n] || used[n] {
			return true
		}
		for _, a := range imports {
			if a == n {
				return true
			}
		}
		return false
	}
	fresh := map[string]string{}
	i := 0
	dst.Inspect(e, func(n dst.Node) bool {
		id, ok := n.(*dst.Ident)
		if !ok || id.Path != "" || id.Obj == importMarker {
			return true
		}
		at := i
		i++
		if !used[id.Name] || at >= len(renamable) || !renamable[at] {
			return true
		}
		if fresh[id.Name] == "" {
			fresh[id.Name] = firstFree(taken, id.Name+"Local")
			locals[fresh[id.Name]] = true
		}
		id.Name = fresh[id.Name]
		return true
	})
	return renderExact(e)
}

// renderPackage finds the package of a node of the program, for renderIn,
// which only has the node.
var renderPackage = func(dst.Node) *Package { return nil }

// importMarker tags the package names renderIn writes.
var importMarker = &dst.Object{Name: "import"}
