package conv

import (
	"fmt"
	"go/constant"
	"go/token"
	"go/types"

	"github.com/dave/dst"
)

// contextFree reports whether e only uses constants, package-level
// declarations and other packages, so it can be computed where the parser is
// built.
func (in *interp) contextFree(p *Package, e dst.Expr) bool {
	ok := true
	dst.Inspect(e, func(n dst.Node) bool {
		if _, isLit := n.(*dst.FuncLit); isLit {
			ok = false
		}
		if id, isIdent := n.(*dst.Ident); isIdent && ok {
			o := p.objectOf(id)
			if v, isVar := o.(*types.Var); isVar && !v.IsField() && v.Parent() != nil && v.Parent() != v.Pkg().Scope() {
				ok = false
			}
		}
		return ok
	})
	return ok
}

// hintOptionsExpr returns the values given to HintOptions as a []string
// expression.
// It reuses the call's own nodes to keep their type information.
// Rendering copies them anyway.
func hintOptionsExpr(c *dst.CallExpr) dst.Expr {
	if c.Ellipsis && len(c.Args) == 1 {
		return c.Args[0]
	}
	return &dst.CompositeLit{Type: &dst.ArrayType{Elt: dst.NewIdent("string")}, Elts: c.Args}
}

// commandLine returns the clause of kingpin.CommandLine, the application the
// package-level functions work on.
func (in *interp) commandLine(fr *frame, pos token.Position) *clause {
	if in.cmdLine == nil {
		in.cmdLine = in.newClause(kindApp, nil, pos, fr)
		in.apps = append(in.apps, in.cmdLine)
		if in.app == nil {
			in.app = in.cmdLine
		}
	}
	return in.cmdLine
}

// kingpinFunc handles calls to package-level kingpin functions.
func (in *interp) kingpinFunc(fr *frame, c *dst.CallExpr, name string, args []value) []value {
	switch name {
	case "New":
		app := in.newClause(kindApp, nil, fr.pkg.position(c), fr)
		if len(args) >= 1 {
			app.name, app.nameOK = constString(args[0])
		}
		if len(args) >= 2 {
			app.help, app.helpOK = constString(args[1])
		}
		in.app = app
		in.apps = append(in.apps, app)
		return []value{vClause{app}}
	case "Separator":
		in.grammarCalls[c] = true
		// The binder receiving this option applies it.
		if len(args) == 1 {
			if s, ok := constString(args[0]); ok {
				return []value{sepOption{s}}
			}
		}
		return []value{vUnknown{}}
	case "Command", "Flag", "Arg":
		// The package-level shortcuts register on kingpin.CommandLine.
		in.grammarCalls[c] = true
		return in.kingpinMethod(fr, c, in.commandLine(fr, fr.pkg.position(c)), name, args)
	case "Version", "UsageTemplate", "Author":
		return in.kingpinMethod(fr, c, in.commandLine(fr, fr.pkg.position(c)), name, args)
	}
	if name == "Parse" {
		in.takePriors()
	}
	in.escapes = append(in.escapes, escape{pos: fr.pkg.position(c), what: "kingpin." + name + " has no automatic kong translation", report: !packageRuntime[name]})
	return []value{vUnknown{}}
}

// setHelp applies Help(text), which replaces any help given before.
// Only commands, flags and arguments can get a help computed at run time;
// the application's help is its description.
func (in *interp) setHelp(fr *frame, c *dst.CallExpr, cl *clause, str func(int) (string, bool)) {
	cl.help, cl.helpOK = str(0)
	if cl.helpExpr != nil {
		in.superseded[cl.helpExpr] = true
		cl.helpExpr = nil
	}
	cl.localHelp = false
	if !cl.helpOK && cl.kind != kindApp && len(c.Args) == 1 {
		in.runtimeHelp(fr, cl, c.Args[0])
	}
}

// runtimeHelp records a help text of f computed at run time, which reaches
// kong through a variable.
// That only works if e can be computed where the parser is built.
func (in *interp) runtimeHelp(fr *frame, f *clause, e dst.Expr) {
	if !in.contextFree(fr.pkg, e) {
		// Reported at the end, unless a later Help replaces it.
		f.localHelp = true
		return
	}
	f.helpExpr, f.helpPkg = e, fr.pkg
	in.moved[e] = true
	in.evaluations++
	f.helpSeq = in.evaluations
}

// finish settles what is only known at the end of the run.
// Destinations get their prior values if no parse was seen, and help texts
// computed from locals that no later Help replaced are reported.
func (in *interp) finish() {
	in.takePriors()
	for _, cl := range in.clauses {
		if cl.localHelp {
			cl.issue("help text is computed from local values")
		}
	}
}

// takePriors records what each destination registered so far holds when the
// program first parses.
// kingpin left that value alone when the flag was absent, so it acts as a
// default; writes after parsing don't count.
func (in *interp) takePriors() {
	for _, cl := range in.clauses {
		if cl.dst != nil && !cl.priorTaken {
			cl.prior, cl.hasPrior = in.priorDefault(cl.dst)
			cl.priorTaken = true
		}
	}
}

// sepOption is the value of kingpin.Separator(s), an accumulator option of
// kingpin v3.
type sepOption struct{ sep string }

func (sepOption) isValue() {}

// kingpinMethod applies a builder method to a clause.
func (in *interp) kingpinMethod(fr *frame, c *dst.CallExpr, cl *clause, name string, args []value) []value {
	if cl.kind == kindFlag || cl.kind == kindArg || !runtimeMethods[name] {
		in.markTouched()
		in.grammarCalls[c] = true
		if len(fr.conds) > len(cl.conds) && !creates[name] && !bindMethods[name] && !valueMethods[name] {
			// For example `if x { f.Required() }` on a flag created
			// outside the if.
			cl.issue(fmt.Sprintf("%s is applied under a condition, which a struct tag cannot express", name))
		}
	}
	pos := fr.pkg.position(c)
	self := []value{vClause{cl}}
	str := func(i int) (string, bool) {
		if i >= len(args) {
			return "", false
		}
		return constString(args[i])
	}

	switch cl.kind {
	case kindApp, kindCommand:
		switch name {
		case "Command", "Flag", "Arg":
			kind := map[string]clauseKind{"Command": kindCommand, "Flag": kindFlag, "Arg": kindArg}[name]
			nc := in.newClause(kind, cl, pos, fr)
			nc.name, nc.nameOK = str(0)
			nc.help, nc.helpOK = str(1)
			if !nc.nameOK {
				nc.issue(kind.String() + " name is not a constant")
			}
			if len(args) >= 2 && !nc.helpOK && len(c.Args) >= 2 {
				in.runtimeHelp(fr, nc, c.Args[1])
			}
			return []value{vClause{nc}}
		case "GetFlag":
			n, _ := str(0)
			for _, ch := range cl.flags() {
				if ch.name == n {
					return []value{vClause{ch}}
				}
			}
			if n == "help" {
				return []value{vClause{in.helpClause(cl)}}
			}
			return []value{vNil{}}
		case "Alias":
			if s, ok := str(0); ok {
				cl.aliases = append(cl.aliases, s)
			} else {
				cl.issue("command alias is not a constant")
			}
			return self
		case "Hidden":
			cl.hidden = true
			return self
		case "Default":
			cl.defaultCmd = true
			return self
		case "Help":
			in.setHelp(fr, c, cl, str)
			return self
		case "Version":
			cl.versionAt = in.nextID
			return self
		case "FullCommand", "Model", "Parse", "ParseContext", "Usage", "UsageForContext",
			"UsageForContextWithTemplate", "Errorf", "Fatalf", "FatalUsage", "FatalUsageContext",
			"FatalIfError", "GetCommand", "GetArg":
			if name == "Parse" {
				in.takePriors()
			}
			// Runtime use, not grammar.
			in.escapes = append(in.escapes, escape{pos: pos, what: fmt.Sprintf("runtime use of the kingpin %s: %s", cl.kind, name)})
			return []value{vUnknown{}}
		case "Action":
			if fn, ok := firstArg(args).(vFunc); ok {
				cl.actions = append(cl.actions, fn)
			} else if lit, ok := c.Args[0].(*dst.FuncLit); ok && len(c.Args) == 1 {
				cl.actionLits = append(cl.actionLits, actionLit{lit: lit, pkg: fr.pkg, before: len(cl.actions)})
			} else {
				cl.issue("its Action is not a function the tool can follow; run it by hand after parsing")
			}
			return self
		case "DefaultEnvars":
			cl.defaultEnvars = true
			return self
		case "Validate", "PreAction", "Interspersed", "OptionalSubcommands", "UsageFuncs":
			cl.issue(name + " has no automatic kong translation")
			return self
		case "Writers", "Writer", "Terminate", "UsageTemplate", "UsageWriter", "ErrorWriter", "Author", "HelpFlag", "UsageContext":
			// Settings the application layer converts.
			return self
		}
	case kindFlag, kindArg:
		switch name {
		case "Short":
			if len(args) == 1 {
				if k, ok := args[0].(vConst); ok && k.c.Kind() == constant.Int {
					if r, ok := constant.Int64Val(k.c); ok {
						cl.short = rune(r)
						return self
					}
				}
			}
			cl.issue("Short value is not a constant")
			return self
		case "Required":
			cl.required = true
			return self
		case "Hidden":
			cl.hidden = true
			return self
		case "Default":
			// A later Default replaces the earlier one.
			if cl.defaultExpr != nil {
				cl.issue("its default value is computed at run time, then replaced; the first computation no longer runs")
			}
			cl.hasDefault, cl.defaults, cl.defaultExpr = true, nil, nil
			if len(args) == 1 {
				if _, isSlice := args[0].(vSlice); !isSlice {
					if _, ok := constString(args[0]); !ok && len(c.Args) == 1 {
						if in.contextFree(fr.pkg, c.Args[0]) {
							cl.defaultExpr, cl.exprPkg = c.Args[0], fr.pkg
							in.moved[c.Args[0]] = true
							in.evaluations++
							cl.defaultSeq = in.evaluations
							cl.defaults = []string{""}
						} else {
							cl.issue("default value is computed from local values")
						}
						return self
					}
				}
			}
			for _, a := range args {
				if sl, ok := a.(vSlice); ok {
					for _, e := range sl.elems {
						s, ok := constString(e)
						if !ok {
							cl.issue("default value is not a constant")
						}
						cl.defaults = append(cl.defaults, s)
					}
					continue
				}
				s, ok := constString(a)
				if !ok {
					cl.issue("default value is not a constant")
				}
				cl.defaults = append(cl.defaults, s)
			}
			// Default with no values, as with an empty slice, sets none.
			cl.hasDefault = len(cl.defaults) > 0
			return self
		case "PlaceHolder":
			if s, ok := str(0); ok {
				cl.placeholder, cl.hasPlace = s, true
			} else {
				cl.issue("placeholder is not a constant")
			}
			return self
		case "Envar", "OverrideDefaultFromEnvar":
			if s, ok := str(0); ok {
				cl.envar, cl.hasEnvar = s, true
			} else {
				cl.issue("environment variable name is not a constant")
			}
			return self
		case "NoEnvar":
			cl.envar, cl.hasEnvar = "", false
			cl.noEnvar = true
			return self
		case "Help":
			in.setHelp(fr, c, cl, str)
			return self
		case "HintOptions":
			// Hints only feed shell completion: losing them is reported but
			// does not stop the conversion.
			var hints []string
			for _, a := range args {
				elems := []value{a}
				if sl, ok := a.(vSlice); ok {
					elems = sl.elems
				}
				for _, e := range elems {
					s, ok := constString(e)
					if !ok {
						// Not a constant: a generated Hints method returns
						// them at run time.
						cl.hintExpr, cl.hintPkg = hintOptionsExpr(c), fr.pkg
						return self
					}
					hints = append(hints, s)
				}
			}
			cl.hints = append(cl.hints, hints...)
			return self
		case "HintAction":
			if len(c.Args) == 1 {
				cl.hintExpr, cl.hintPkg = callOf(c.Args[0]), fr.pkg
			}
			return self
		case "Action", "PreAction":
			if len(args) == 1 && name == "Action" && cl.action == nil {
				cl.action = args[0]
			} else {
				cl.issue(name + " callback has no kong equivalent")
			}
			return self
		case "Model", "FullCommand":
			in.escapes = append(in.escapes, escape{pos: pos, what: "runtime use of a kingpin flag: " + name})
			return []value{vUnknown{}}
		}
		if bindMethods[name] {
			in.bindVar(fr, c, cl, name, args)
			return nil
		}
		if valueMethods[name] {
			// kingpin allocates the value and hands back a pointer to it.
			// The conversion turns that storage into a grammar field.
			in.bindVar(fr, c, cl, name+"Var", append([]value{vStorage{cl}}, args...))
			cl.bind = name
			if p, ok := fr.pkg.typeOf(c).(*types.Pointer); ok {
				cl.storageType = p.Elem()
			}
			return []value{vStorage{cl}}
		}
	}
	cl.issue(fmt.Sprintf("unsupported kingpin method %s on a %s", name, cl.kind))
	return []value{vUnknown{}}
}

// helpClause returns the help flag kingpin creates for every application.
func (in *interp) helpClause(app *clause) *clause {
	if app.helpFlag == nil {
		app.helpFlag = &clause{kind: kindFlag, parent: app, name: "help", nameOK: true, builtinHelp: true}
	}
	return app.helpFlag
}

// versionClause returns the --version flag added by Version, which a program
// can change through the application's VersionFlag field.
func (in *interp) versionClause(app *clause) *clause {
	if app.versionFlag == nil {
		app.versionFlag = &clause{kind: kindFlag, parent: app, name: "version", nameOK: true, builtinHelp: true}
	}
	return app.versionFlag
}

// packageRuntime are the package-level kingpin functions that use the command
// line rather than declare it.
// The conversion rewrites them where it can.
var packageRuntime = map[string]bool{
	"Parse": true, "MustParse": true, "Fatalf": true, "Errorf": true, "FatalIfError": true,
	"FatalUsage": true, "FatalUsageContext": true, "Usage": true,
}

// creates are the methods that make a new clause rather than change one.
var creates = map[string]bool{"Command": true, "Flag": true, "Arg": true, "GetFlag": true}

// runtimeMethods use or configure a built application rather than declare
// grammar, so statements calling them are left for the application layer.
var runtimeMethods = map[string]bool{
	"FullCommand": true, "Model": true, "Parse": true, "ParseContext": true, "Usage": true,
	"UsageForContext": true, "UsageForContextWithTemplate": true, "Errorf": true, "Fatalf": true,
	"FatalUsage": true, "FatalUsageContext": true, "FatalIfError": true, "GetCommand": true,
	"GetArg": true, "Writers": true, "Terminate": true, "UsageContext": true, "UsageTemplate": true,
	"UsageWriter": true, "ErrorWriter": true, "Writer": true, "Interspersed": true,
	"Author": true, "Validate": true, "PreAction": true, "UsageFuncs": true,
	"GetFlag": true,
}

// bindMethods store the parsed value through a destination pointer.
var bindMethods = map[string]bool{
	"StringVar": true, "BoolVar": true, "NegatableBoolVar": true,
	"IntVar": true, "Int8Var": true, "Int16Var": true, "Int32Var": true, "Int64Var": true,
	"UintVar": true, "Uint8Var": true, "Uint16Var": true, "Uint32Var": true, "Uint64Var": true,
	"Float32Var": true, "Float64Var": true, "DurationVar": true, "TimeVar": true,
	"StringsVar": true, "IntsVar": true, "Int64ListVar": true, "BoolListVar": true,
	"EnumVar": true, "EnumsVar": true, "StringMapVar": true,
	"ExistingFileVar": true, "ExistingDirVar": true, "ExistingFileOrDirVar": true,
	"URLVar": true, "IPVar": true, "TCPVar": true, "SetValue": true, "CounterVar": true,
	"FileVar": true, "BytesVar": true,
	"ExistingFilesVar": true, "ExistingDirsVar": true, "ExistingFilesOrDirsVar": true,
	"RegexpVar": true, "RegexpListVar": true, "URLListVar": true, "IPListVar": true,
	"Float64ListVar": true, "Float32ListVar": true, "Int8ListVar": true, "Int16ListVar": true,
	"Int32ListVar": true, "UintListVar": true, "Uint8ListVar": true, "Uint16ListVar": true,
	"Uint32ListVar": true, "Uint64ListVar": true, "DurationListVar": true, "HexBytesVar": true,
	"ResolvedIPVar": true, "TCPListVar": true, "OpenFileVar": true,
}

// valueMethods return a pointer to fresh storage rather than binding a field.
var valueMethods = map[string]bool{
	"String": true, "Bool": true, "Int": true, "Int8": true, "Int16": true, "Int32": true,
	"Int64": true, "Uint": true, "Uint8": true, "Uint16": true, "Uint32": true, "Uint64": true,
	"Float32": true, "Float64": true, "Duration": true, "Strings": true, "Ints": true,
	"Int64List": true, "Enum": true, "Enums": true, "Counter": true, "ExistingFile": true,
	"ExistingDir": true, "ExistingFileOrDir": true, "URL": true, "IP": true, "StringMap": true,
	"NegatableBool": true, "Time": true, "File": true, "TCP": true, "Bytes": true,
	"ExistingFiles": true, "ExistingDirs": true, "ExistingFilesOrDirs": true, "Regexp": true,
	"RegexpList": true, "URLList": true, "IPList": true, "BoolList": true, "Float64List": true,
	"Float32List": true, "Int8List": true, "Int16List": true, "Int32List": true, "UintList": true,
	"Uint8List": true, "Uint16List": true, "Uint32List": true, "Uint64List": true,
	"DurationList": true, "HexBytes": true, "ResolvedIP": true, "TCPList": true, "OpenFile": true,
}

func (in *interp) bindVar(fr *frame, call *dst.CallExpr, cl *clause, method string, args []value) {
	cl.bind = method
	target := 0
	if method == "TimeVar" {
		// TimeVar(layout, &dst) puts the layout first.
		if len(args) >= 1 {
			if s, ok := constString(args[0]); ok {
				cl.format, cl.hasFormat = s, true
			} else {
				cl.issue("time layout is not a constant")
			}
		}
		target = 1
	}
	if len(args) <= target {
		cl.issue(method + " has no destination")
		return
	}
	cl.dst = args[target]
	if target < len(call.Args) {
		cl.dstExpr, cl.dstPkg = call.Args[target], fr.pkg
	}
	switch method {
	case "EnumVar", "EnumsVar":
		opts := args[target+1:]
		if len(opts) == 1 {
			if sl, ok := opts[0].(vSlice); ok {
				opts = sl.elems
			}
		}
		var options []string
		for _, o := range opts {
			s, ok := constString(o)
			if !ok {
				options = nil
				break
			}
			options = append(options, s)
		}
		if options != nil || len(opts) == 0 {
			cl.enum = options
			break
		}
		// Options from a package-level variable are only known at run
		// time, so the conversion checks them in a hook, not a tag.
		if call.Ellipsis && len(call.Args) == target+2 {
			if v, ok := fr.pkg.objectOf(call.Args[target+1]).(*types.Var); ok && v.Parent() == v.Pkg().Scope() {
				cl.enumVar = v
				break
			}
		}
		cl.issue("enum options are not constants")
	default:
		for _, o := range args[target+1:] {
			if sl, ok := o.(vSlice); ok && len(sl.elems) == 1 {
				o = sl.elems[0]
			}
			switch o := o.(type) {
			case sepOption:
				cl.separator, cl.hasSep = o.sep, true
			case vSlice:
				if len(o.elems) > 0 {
					cl.issue("unsupported accumulator option")
				}
			default:
				cl.issue("unsupported accumulator option")
			}
		}
	}
}

// fieldOf returns the struct field a destination points at, if any.
func fieldOf(v value) (*object, *types.Var, bool) {
	if fp, ok := v.(vFieldPtr); ok {
		return fp.o, fp.f, true
	}
	return nil, nil, false
}

// firstArg returns the only argument of a call, or nil.
func firstArg(args []value) value {
	if len(args) != 1 {
		return nil
	}
	return args[0]
}
