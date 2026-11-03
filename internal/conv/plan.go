package conv

import (
	"fmt"
	"go/token"
	"go/types"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/dave/dst"

	"king2kong/kongtag"
)

type bindMode int

const (
	// modeDirect tags the destination field itself.
	// kong reaches it from the command struct through embedded fields only.
	modeDirect bindMode = iota
	// modeEmbed tags the destination field and marks the named struct fields
	// on the way with embed:"", so kong flattens them into the command.
	modeEmbed
	// modeShadow adds a tagged field to the command struct and copies its
	// value to the real destination once kong has parsed it.
	modeShadow
)

func (m bindMode) String() string {
	return [...]string{"direct", "embed", "shadow"}[m]
}

// binding is how one registered flag or argument maps onto the kong grammar.
type binding struct {
	cl   *clause
	cmd  *clause // the command or application the flag belongs to
	inst *object // the command struct, nil for application flags

	// field carries the tag (the destination, or the was-set wrapper holding
	// it) and holder is the object containing it.
	field  *types.Var
	holder *object
	path   []step // from inst to holder

	// wrapper is set for the .Action(x.Set) was-set idiom.
	wrapper *wrapperType
	// marks is set when the flag's action marks another value as given.
	marks *mark
	// valueOfWrapper is set when the destination is the value field of a
	// was-set wrapper registered without its Set action, so kingpin never
	// marked it as set.
	valueOfWrapper bool

	mode bindMode
	// keepPrior makes a shadow's copy conditional: the destination may
	// already hold a value, so only overwrite it when the flag was given.
	keepPrior bool
	dstExpr   string // Go expression of the destination, for shadows
	dstVar    *types.Var
	// cell is set when the destination is a pointer the constructor made
	// with new(T) and stored in field: the shadow's address replaces it.
	cell bool
	// shadowField is the name of the field added for a shadow binding.
	shadowField string
	// storage is set for a value method such as String(): the value gets a
	// new field in the grammar, named after the flag.
	storage bool

	tag *kongtag.Tag
}

// wrapperType is a struct type used with the was-set idiom: it holds a value
// field and embeds (or is) a type whose Set method records that the flag was
// given.
type wrapperType struct {
	named     *types.Named
	value     *types.Var // the field the flag value goes into
	marker    *types.Named
	markField *types.Var // the bool the Set method sets
	set       *types.Func
}

// enumInHook reports whether the allowed values are checked in a hook instead
// of kong's enum tag.
// kong can't check an enum on a struct like a was-set wrapper, and a tag can't
// hold options computed at run time.
func (b *binding) enumInHook() bool {
	return b.cl.enumVar != nil || len(b.cl.enum) > 0 && b.wrapper != nil
}

// Plan is the set of decisions the conversion makes.
type Plan struct {
	g        *Grammar
	bindings []*binding
	// fieldTags are the tags placed on existing fields.
	fieldTags map[*types.Var]*fieldTag
	wrappers  map[*types.Named]*wrapperType
	// vars are the kong variables of values only known at run time.
	vars    []runtimeVar
	markers map[*types.Named]*wrapperType
	Issues  []Issue
	issued  map[Issue]bool
	// failed are the registrations the conversion could not handle.
	failed []failure
}

type failure struct {
	cl     *clause
	reason string
}

type fieldTag struct {
	field    *types.Var
	tag      string
	bindings []*binding
}

// Issue is something the conversion cannot do on its own.
type Issue struct {
	Pos    token.Position
	Reason string
}

func (p *Plan) issue(pos token.Position, format string, args ...any) {
	is := Issue{Pos: pos, Reason: fmt.Sprintf(format, args...)}
	// Tags get worked out more than once, so the same issue comes up again.
	if !p.issued[is] {
		p.issued[is] = true
		p.Issues = append(p.Issues, is)
	}
}

// NewPlan decides how every registration maps onto kong.
func NewPlan(g *Grammar) *Plan {
	p := &Plan{
		g:         g,
		fieldTags: map[*types.Var]*fieldTag{},
		wrappers:  map[*types.Named]*wrapperType{},
		markers:   map[*types.Named]*wrapperType{},
		issued:    map[Issue]bool{},
	}
	// Discover the was-set wrapper types first, so a registration of a
	// wrapper's value without its Set action is recognized wherever it is.
	for _, c := range g.in.clauses {
		if (c.kind == kindFlag || c.kind == kindArg) && c.action != nil && len(c.issues) == 0 {
			if o, f, ok := fieldOf(c.dst); ok {
				p.wrapperFor(c.action, o, f)
			}
		}
	}
	for _, c := range g.in.clauses {
		if c.kind != kindFlag && c.kind != kindArg {
			continue
		}
		if c.builtinHelp {
			continue
		}
		if b := p.bind(c); b != nil {
			p.bindings = append(p.bindings, b)
		} else if n := len(p.Issues); n > 0 {
			p.failed = append(p.failed, failure{c, p.Issues[n-1].Reason})
		}
	}
	p.collectFieldTags()
	return p
}

// bind works out where a flag's value lands relative to its command.
func (p *Plan) bind(c *clause) *binding {
	for _, is := range append(c.issues, c.notes...) {
		p.issue(c.pos, "%s %q: %s", c.kind, c.name, is)
	}
	if len(c.issues) > 0 {
		return nil
	}
	b := &binding{cl: c, cmd: c.parent}
	switch strings.TrimSuffix(c.bind, "Var") {
	case "ExistingFileOrDir", "ExistingFilesOrDirs", "TCP", "TCPList", "HexBytes", "OpenFile", "ResolvedIP":
		p.issue(c.pos, "%s %q: kong has no equivalent of kingpin's %s values", c.kind, c.name, strings.TrimSuffix(c.bind, "Var"))
		return nil
	}
	if _, ok := c.dst.(vStorage); ok {
		b.storage = true
		b.tag = p.tagFor(b, p.g.prog.identFromName(c.name))
		return b
	}
	if c.parent.kind == kindCommand {
		b.inst = c.parent.instance
		if b.inst == nil {
			p.issue(c.pos, "%s %q: its command has no struct to hold it", c.kind, c.name)
			return nil
		}
	}

	dst := c.dst
	if pv, ok := dst.(vPtr); ok && pv.o.parent != nil {
		// A pointer to a struct-typed field, such as TimeVar's *time.Time.
		dst = vFieldPtr{pv.o.parent, pv.o.pfield}
	}
	switch d := dst.(type) {
	case vFieldPtr:
		b.field, b.holder = d.f, d.o
	case vVarPtr:
		b.dstVar = d.v
	case vCell:
		if b.inst != nil {
			if o, f, ok := fieldHolding(b.inst, d); ok {
				b.field, b.holder, b.cell = f, o, true
				break
			}
		}
		p.issue(c.pos, "%s %q: its destination was made with new() somewhere the tool cannot follow", c.kind, c.name)
		return nil
	default:
		p.issue(c.pos, "%s %q: cannot tell where its value goes", c.kind, c.name)
		return nil
	}

	if c.action != nil {
		w, wrapperObj, ok := p.wrapperFor(c.action, b.holder, b.field)
		switch {
		case ok:
			b.wrapper = w
			b.field, b.holder = wrapperObj.pfield, wrapperObj.parent
		case p.foreignMark(c.action) != nil:
			// The action marks another value as given, as in
			// .Action(c.Password.Set).StringVar(&c.Pipeline.Value).
			// Keep the mark, and copy the flag's own value when it is given.
			b.marks = p.foreignMark(c.action)
			if b.holder != nil && b.holder.parent != nil {
				if w := p.wrapperOf(b.holder.typ); w != nil && w.value == b.field {
					b.valueOfWrapper = true
				}
			}
		default:
			p.issue(c.pos, "%s %q: its Action callback has no kong equivalent", c.kind, c.name)
			return nil
		}
	} else if b.holder != nil && b.holder.parent != nil {
		if w := p.wrapperOf(b.holder.typ); w != nil && w.value == b.field {
			b.valueOfWrapper = true
		}
	}

	defer func() {
		if b.marks != nil && !b.storage {
			b.mode, b.keepPrior = modeShadow, true
		}
	}()
	switch {
	case b.dstVar != nil:
		b.mode = modeShadow
		b.keepPrior = true
	case b.inst == nil:
		// Application flags: the root grammar embeds the struct holding them.
		b.mode = modeDirect
	default:
		path, ok := pathFrom(b.inst, b.holder)
		if !ok {
			p.issue(c.pos, "%s %q: its destination is not reachable from the command struct", c.kind, c.name)
			return nil
		}
		b.path = path
		b.mode = p.modeFor(b)
	}
	if b.mode == modeShadow && b.wrapper != nil {
		p.issue(c.pos, "%s %q: a was-set wrapper outside the command struct is not supported", c.kind, c.name)
		return nil
	}
	if b.valueOfWrapper || b.cell {
		b.mode = modeShadow
	}
	if b.mode == modeShadow {
		b.dstExpr, b.keepPrior = p.shadowTarget(b)
	}
	b.tag = p.tagFor(b, p.tagFieldName(b))
	return b
}

// modeFor picks the least intrusive way to let kong reach the field.
func (p *Plan) modeFor(b *binding) bindMode {
	if !p.g.prog.owns(b.field.Pkg()) {
		return modeShadow
	}
	mode := modeDirect
	for _, s := range b.path {
		switch {
		case s.f.Anonymous():
			// kong flattens embedded fields whether they are values or
			// pointers, and the planner turns pointers into values anyway.
		case s.ptr:
			return modeShadow
		default:
			if !p.g.prog.owns(s.f.Pkg()) || !p.g.prog.owns(namedPkg(s.f.Type())) {
				return modeShadow
			}
			mode = modeEmbed
		}
	}
	return mode
}

func namedPkg(t types.Type) *types.Package {
	if n, ok := deref(t).(*types.Named); ok {
		return n.Obj().Pkg()
	}
	return nil
}

// shadowTarget renders the destination as an expression on the command's
// receiver, and decides whether the copy must be conditional.
func (p *Plan) shadowTarget(b *binding) (string, bool) {
	if b.dstVar != nil {
		return b.dstVar.Pkg().Name() + "." + b.dstVar.Name(), true
	}
	var parts []string
	shared := false
	for _, s := range b.path {
		if s.ptr && !s.f.Anonymous() {
			shared = true
		}
		parts = append(parts, s.f.Name())
	}
	field := b.field.Name()
	parts = append(parts, field)
	// The prior value only matters when it can differ from the zero value
	// kong starts the shadow with.
	prior := shared || !b.holder.root().known
	if v, ok := b.holder.fields[b.field]; ok {
		if _, isZero := v.(vConst); !isZero {
			prior = true
		} else if !isZeroConst(v) {
			prior = true
		}
	}
	return "c." + strings.Join(parts, "."), prior
}

func isZeroConst(v value) bool {
	c, ok := v.(vConst)
	if !ok {
		return false
	}
	return zeroValue(c.t) == v || c.c.String() == "0" || c.c.String() == `""` || c.c.String() == "false"
}

// wrapperFor recognizes .Action(x.Set) paired with a destination inside x, and
// returns the wrapper type and the object of x.
func (p *Plan) wrapperFor(action value, holder *object, field *types.Var) (*wrapperType, *object, bool) {
	fv, ok := action.(vFunc)
	if !ok || fv.fn == nil || holder == nil || field == nil {
		return nil, nil, false
	}
	var recv *object
	switch r := fv.recv.(type) {
	case vObj:
		recv = r.o
	case vPtr:
		recv = r.o
	}
	if recv == nil {
		return nil, nil, false
	}
	// The receiver is the wrapper itself or a marker embedded in it.
	w := holder
	cur := recv
	for cur != nil && cur != w {
		if cur.pfield == nil || !cur.pfield.Anonymous() {
			cur = nil
			break
		}
		cur = cur.parent
	}
	if cur == nil {
		return nil, nil, false
	}
	named, ok := w.typ.(*types.Named)
	if !ok {
		return nil, nil, false
	}
	// The flag field is the outermost struct the value holder is embedded
	// in, such as an OptionalVersion type embedding an OptionalString.
	for w.pfield != nil && w.pfield.Anonymous() && w.parent != nil && w.parent.parent != nil {
		w = w.parent
	}
	if w.parent == nil || w.pfield == nil {
		return nil, nil, false
	}
	marker, ok := recv.typ.(*types.Named)
	if !ok {
		return nil, nil, false
	}
	wt := p.wrapperOf(named)
	if wt == nil {
		mf := p.markerField(fv.fn)
		if mf == nil {
			return nil, nil, false
		}
		wt = &wrapperType{named: named, value: field, marker: marker, markField: mf, set: fv.fn}
		p.wrappers[named] = wt
		p.markers[marker] = wt
	}
	if wt.value != field {
		return nil, nil, false
	}
	return wt, w, true
}

// foreignMark recognizes an action that is the Set method of a was-set marker,
// `o.Field = true; return nil`, and returns what it marks.
func (p *Plan) foreignMark(action value) *mark {
	fv, ok := action.(vFunc)
	if !ok || fv.fn == nil {
		return nil
	}
	var recv *object
	switch r := fv.recv.(type) {
	case vObj:
		recv = r.o
	case vPtr:
		recv = r.o
	}
	if recv == nil {
		return nil
	}
	f := p.markerField(fv.fn)
	if f == nil {
		return nil
	}
	return &mark{recv: recv, field: f, set: fv.fn}
}

// mark is the bool field of recv that a flag's action sets by calling set.
type mark struct {
	recv  *object
	field *types.Var
	set   *types.Func
}

// wrapperOf returns the wrapper description for a type already seen with the
// was-set idiom.
func (p *Plan) wrapperOf(t types.Type) *wrapperType {
	if n, ok := t.(*types.Named); ok {
		return p.wrappers[n]
	}
	return nil
}

// markerField checks that a Set method only records that it ran, by setting
// a bool field to true, and returns that field.
func (p *Plan) markerField(set *types.Func) *types.Var {
	fd, ok := p.g.prog.funcs[set]
	if !ok || fd.decl.Body == nil {
		return nil
	}
	recv := set.Type().(*types.Signature).Recv()
	if recv == nil {
		return nil
	}
	if structOf(deref(recv.Type())) == nil {
		return nil
	}
	return markerAssign(fd)
}

// markerAssign matches a body of the form `o.Field = true; return nil` and
// returns Field.
func markerAssign(fd funcDecl) *types.Var {
	body := fd.decl.Body.List
	if len(body) != 2 {
		return nil
	}
	as, ok := body[0].(*dst.AssignStmt)
	if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 || as.Tok != token.ASSIGN {
		return nil
	}
	if id, ok := as.Rhs[0].(*dst.Ident); !ok || id.Name != "true" {
		return nil
	}
	sel, ok := as.Lhs[0].(*dst.SelectorExpr)
	if !ok {
		return nil
	}
	f, ok := fd.pkg.objectOf(sel).(*types.Var)
	if !ok || !f.IsField() {
		return nil
	}
	ret, ok := body[1].(*dst.ReturnStmt)
	if !ok || len(ret.Results) != 1 {
		return nil
	}
	if id, ok := ret.Results[0].(*dst.Ident); !ok || id.Name != "nil" {
		return nil
	}
	return f
}

// collectFieldTags groups direct and embed bindings by field.
// Every command sharing a field must register it the same way, since a struct
// tag is the same for every use of the type.
func (p *Plan) collectFieldTags() {
	for _, b := range p.bindings {
		if b.mode == modeShadow || b.field == nil {
			continue
		}
		ft := p.fieldTags[b.field]
		tag := b.tag.String()
		if ft == nil {
			ft = &fieldTag{field: b.field, tag: tag}
			p.fieldTags[b.field] = ft
		} else if ft.tag != tag {
			p.issue(b.cl.pos, "field %s is registered with different flag settings by different commands (%s vs %s)", b.field.Name(), ft.tag, tag)
		}
		ft.bindings = append(ft.bindings, b)
	}
}

// tagFor renders the kong tag for a binding whose field is called fieldName.
func (p *Plan) tagFor(b *binding, fieldName string) *kongtag.Tag {
	c := b.cl
	t := new(kongtag.Tag)
	if c.kind == kindArg {
		t.Flag("arg")
	}
	if kongName(fieldName) != c.name {
		t.Set("name", c.name)
	}
	if c.kind == kindFlag && c.short != 0 {
		t.Set("short", string(c.short))
	}
	switch {
	case c.required:
		t.Flag("required")
	case c.kind == kindArg && !c.hasDefault:
		t.Flag("optional")
	}
	isSlice := p.isSliceDest(b)
	hookEnum := b.enumInHook()
	// kong splits a list's default and environment value on its command-line
	// separator, while kingpin took each value whole.
	// kingpin v2 split environment values on newlines, which "\n" reproduces,
	// and several defaults need a separator too.
	sep := "none"
	switch {
	case c.hasSep:
		sep = c.separator
	case isSlice && (len(c.defaults) > 1 || !p.g.prog.kingpinV3 && (c.hasEnvar || defaultEnvar(c) != "")):
		sep = "\n"
	}
	// A map takes one key=value per flag, so its defaults and kingpin v2's
	// newline-separated environment entries need a separator of their own.
	isMap := strings.TrimSuffix(c.bind, "Var") == "StringMap"
	mapSep := "none"
	if isMap && (len(c.defaults) > 1 || !p.g.prog.kingpinV3 && (c.hasEnvar || defaultEnvar(c) != "")) {
		mapSep = "\n"
	}
	if isSlice && p.g.prog.kingpinV3 && (c.hasEnvar || defaultEnvar(c) != "") && !c.hasSep {
		p.issue(c.pos, "%s %q: kingpin split its environment variable on %q, kong takes it whole", c.kind, c.name, string(os.PathListSeparator))
	}
	if !c.hasDefault && !c.required && !isSlice {
		// kingpin left a destination alone when its flag was absent, but
		// kong resets it, so the value it held becomes the default.
		// It hides behind the usual placeholder, since kingpin never
		// displayed it.
		prior, ok := c.prior, c.hasPrior
		// kong validates a default like any value, but kingpin never checked
		// what it left alone.
		checked := len(c.enum) > 0 && !slices.Contains(c.enum, prior) || strings.HasPrefix(c.bind, "Existing")
		switch {
		case ok && checked:
			p.issue(c.pos, "%s %q: kingpin kept %q when it was absent, which kong would check as a default and reject; set it after parsing by hand", c.kind, c.name, prior)
		case ok:
			c.defaults, c.hasDefault = []string{prior}, true
			if !c.hasPlace && strings.TrimSuffix(c.bind, "Var") != "Bool" && c.kind == kindFlag {
				c.placeholder, c.hasPlace = strings.ToUpper(c.name), true
			}
		}
	}
	if c.defaultExpr != nil && c.registeredUnderCondition() {
		// kong would compute it whether or not the condition holds.
		p.issue(c.pos, "%s %q: its default value is computed at run time under a condition; the flag lost its default, give it back by hand", c.kind, c.name)
	} else if c.defaultExpr != nil {
		t.Set("default", "${"+p.runtimeVar(c, "default", c.defaultExpr)+"}")
	} else if c.hasDefault {
		joined := c.defaults[0]
		switch {
		case isMap && mapSep != "none":
			joined = strings.Join(c.defaults, "\n")
		case sep != "none":
			joined = strings.Join(c.defaults, sep)
		}
		t.Set("default", kongtag.Interpolated(joined))
	} else if len(c.enum) > 0 && !c.required && !isSlice && !hookEnum {
		// kong only accepts an enum on an optional flag with a default, and
		// kingpin leaves the value empty when the flag is absent.
		t.Set("default", "")
	}
	if slices.ContainsFunc(c.enum, func(v string) bool { return strings.Contains(v, ",") }) {
		p.issue(c.pos, "%s %q: an enum value contains a comma, which kong's enum tag would split; check the values by hand", c.kind, c.name)
	}
	if len(c.enum) > 0 && !hookEnum {
		vals := append([]string{}, c.enum...)
		// kong checks an empty default against the enum like any value, so
		// an optional enum gets an empty option at the end.
		// kongcompat drops it again when checking what was given, and
		// relies on it being there whenever the default may be empty.
		emptyDefault := !c.hasDefault && c.defaultExpr == nil || c.hasDefault && len(c.defaults) == 1 && c.defaults[0] == ""
		if !c.required && !isSlice && (emptyDefault || c.defaultExpr != nil && c.enum[len(c.enum)-1] == "") {
			vals = append(vals, "")
		}
		t.Set("enum", kongtag.Interpolated(strings.Join(vals, ",")))
	}
	if isSlice {
		t.Set("sep", sep)
	}
	if c.hasPlace {
		t.Set("placeholder", kongtag.Interpolated(c.placeholder))
	} else if ph := wrappedStringPlaceholder(b); ph != "" {
		t.Set("placeholder", kongtag.Interpolated(ph))
	}
	if c.hasEnvar {
		t.Set("env", kongtag.Interpolated(c.envar))
	} else if env := defaultEnvar(c); env != "" {
		t.Set("env", kongtag.Interpolated(env))
	}
	if len(c.hints) > 0 {
		// kongcompat's completion offers them, as kingpin did.
		t.Set("hints", kongtag.Interpolated(strings.Join(c.hints, ",")))
	}
	if c.hasFormat {
		t.Set("format", c.format)
	}
	switch strings.TrimSuffix(c.bind, "Var") {
	case "NegatableBool":
		t.Flag("negatable")
	case "Bool":
		switch {
		case c.kind == kindArg:
			t.Set("type", "boolarg")
		case !p.g.prog.kingpinV3 && c.kind == kindFlag:
			// kingpin v2 accepts --no-x for every boolean flag.
			t.Flag("negatable")
		case c.kind == kindFlag && strings.HasPrefix(c.name, "no-") && b.wrapper == nil:
			// kingpin v3 sets a flag named no-x to false when it is given.
			t.Set("type", "noflag")
		}
	case "Counter":
		t.Set("type", "counter")
	case "ExistingFile", "ExistingFiles":
		t.Set("type", "existingfile")
	case "ExistingDir", "ExistingDirs":
		t.Set("type", "existingdir")
	case "StringMap":
		// kingpin took one key=value per flag; kong also splits on ";".
		t.Set("mapsep", mapSep)
	}
	if c.hidden {
		t.Flag("hidden")
	}
	p.helpTag(t, c)
	return t
}

// runtimeVar returns the name of the kong variable holding a run-time value
// of c, computed by e, and records it so the parser setup can provide it.
func (p *Plan) runtimeVar(c *clause, what string, e dst.Expr) string {
	for _, v := range p.vars {
		if v.cl == c && v.expr == e {
			return v.name
		}
	}
	var parts []string
	if c.parent != nil && c.parent.kind == kindCommand {
		parts = append(parts, strings.Fields(c.parent.fullName())...)
	}
	parts = append(parts, c.name, what)
	name := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		if r >= 'A' && r <= 'Z' {
			return r + 'a' - 'A'
		}
		return '_'
	}, strings.Join(parts, "_"))
	// foo-bar and foo_bar map to the same name.
	name = firstFree(func(n string) bool {
		return slices.ContainsFunc(p.vars, func(v runtimeVar) bool { return v.name == n })
	}, name)
	desc := "default value"
	if what == "help" {
		desc = "help text"
	}
	seq := c.defaultSeq
	if what == "help" {
		seq = c.helpSeq
	}
	pkg := c.exprPkg
	if what == "help" {
		pkg = c.helpPkg
	}
	p.vars = append(p.vars, runtimeVar{name: name, expr: e, pkg: pkg, cl: c, what: desc, seq: seq})
	return name
}

// helpTag sets the help of c's tag, through a kong variable when the program
// computes it.
func (p *Plan) helpTag(t *kongtag.Tag, c *clause) {
	switch {
	case c.helpExpr != nil && c.registeredUnderCondition():
		p.issue(c.pos, "%s %q: its help text is computed at run time under a condition; it lost its help, give it back by hand", c.kind, c.name)
	case c.helpExpr != nil:
		t.Set("help", "${"+p.runtimeVar(c, "help", c.helpExpr)+"}")
	case c.help != "":
		t.Set("help", kongtag.Interpolated(c.help))
	}
}

// runtimeVar is a kong variable whose value the program computes at run
// time, given to the parser where it is built.
type runtimeVar struct {
	name string
	expr dst.Expr
	pkg  *Package
	cl   *clause
	// what is "default value" or "help text".
	what string
	// seq is the order in which kingpin computed it.
	seq int
}

// defaultEnvar is the environment variable kingpin's DefaultEnvars gives a
// flag that has none: APP_FLAG in upper case, with anything other than
// letters, digits and underscores turned into underscores.
func defaultEnvar(c *clause) string {
	if c.kind != kindFlag || c.noEnvar {
		return ""
	}
	app := c.parent
	for app != nil && app.kind != kindApp {
		app = app.parent
	}
	if app == nil || !app.defaultEnvars || !app.nameOK || app.name == "" {
		return ""
	}
	return strings.ToUpper(nonEnvarChars.ReplaceAllString(app.name+"_"+c.name, "_"))
}

var nonEnvarChars = regexp.MustCompile(`[^a-zA-Z0-9_]+`)

// wrappedStringPlaceholder returns the quoted default kingpin showed as the
// placeholder of a string flag that now lands in a was-set wrapper.
// kong's help sees the wrapper instead of a string and would drop the quotes.
func wrappedStringPlaceholder(b *binding) string {
	c := b.cl
	if b.wrapper == nil || !c.hasDefault || len(c.enum) > 0 || strings.TrimSuffix(c.bind, "Var") != "String" {
		return ""
	}
	ph := strconv.Quote(c.defaults[0])
	if len(c.defaults) > 1 {
		ph += "..."
	}
	return ph
}

// tagFieldName is the name the tagged field will have after conversion.
func (p *Plan) tagFieldName(b *binding) string {
	if b.mode == modeShadow || b.field == nil || b.storage {
		return p.g.prog.identFromName(b.cl.name)
	}
	return p.g.prog.exported(b.field.Name())
}

func (p *Plan) isSliceDest(b *binding) bool {
	if b.storage {
		_, ok := b.cl.storageType.Underlying().(*types.Slice)
		return ok
	}
	if b.wrapper != nil {
		_, ok := b.wrapper.value.Type().Underlying().(*types.Slice)
		return ok
	}
	var t types.Type
	if b.field != nil {
		t = b.field.Type()
	} else if b.dstVar != nil {
		t = b.dstVar.Type()
	}
	if t == nil {
		return false
	}
	_, ok := t.Underlying().(*types.Slice)
	return ok
}

// Summary writes the plan's statistics, for debugging.
func (p *Plan) Summary() string {
	modes := map[string]int{}
	for _, b := range p.bindings {
		k := b.mode.String()
		if b.wrapper != nil {
			k += "+wrapper"
		}
		if b.valueOfWrapper {
			k += "+unmarked"
		}
		if b.keepPrior {
			k += "+keep"
		}
		modes[k]++
	}
	var keys []string
	for k := range modes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&sb, "%6d %s\n", modes[k], k)
	}
	fmt.Fprintf(&sb, "%6d tagged fields\n", len(p.fieldTags))
	fmt.Fprintf(&sb, "%6d wrapper types\n", len(p.wrappers))
	reasons := map[string]int{}
	for _, is := range p.Issues {
		reasons[is.Reason]++
	}
	var rs []string
	for r := range reasons {
		rs = append(rs, r)
	}
	sort.Slice(rs, func(i, j int) bool { return reasons[rs[i]] > reasons[rs[j]] })
	for _, r := range rs {
		fmt.Fprintf(&sb, "issue x%d: %s\n", reasons[r], r)
	}
	return sb.String()
}
