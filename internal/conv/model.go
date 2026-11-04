package conv

import (
	"go/token"
	"go/types"
	"slices"

	"github.com/dave/dst"
)

type clauseKind int

const (
	kindApp clauseKind = iota
	kindCommand
	kindFlag
	kindArg
)

func (k clauseKind) String() string {
	switch k {
	case kindApp:
		return "app"
	case kindCommand:
		return "command"
	case kindFlag:
		return "flag"
	case kindArg:
		return "arg"
	}
	return "?"
}

// clause is one node of the recovered kingpin grammar.
type clause struct {
	id     int
	kind   clauseKind
	parent *clause
	// children are the commands, flags and args, in registration order.
	children []*clause

	name, help     string
	nameOK, helpOK bool

	aliases     []string
	hidden      bool
	defaultCmd  bool
	instance    *object
	builtinHelp bool
	// helpFlag is the application's built-in --help flag, set when the
	// program changes it, for example to add -h.
	helpFlag *clause
	// versionFlag is the --version flag, set when the program changes it.
	versionFlag *clause
	// defaultEnvars is set when the application gives every flag an
	// environment variable named after it.
	defaultEnvars bool
	// versionAt is the id of the last clause registered before Version was
	// called, to keep --version at the same place among the flags.
	versionAt int
	// dstExpr is the destination argument of a binder, in dstPkg.
	dstExpr dst.Expr
	dstPkg  *Package
	// actions are the named functions given to Action, actionLits the
	// function literals.
	actions    []vFunc
	actionLits []actionLit

	short       rune
	required    bool
	defaults    []string
	hasDefault  bool
	placeholder string
	hasPlace    bool
	envar       string
	hasEnvar    bool
	separator   string
	hasSep      bool
	// hints are the constant values given to HintOptions.
	hints []string
	// defaultExpr, in exprPkg, and helpExpr, in helpPkg, compute a default
	// value and a help text only known at run time.
	// kong gets them through variables.
	defaultExpr, helpExpr dst.Expr
	// defaultSeq and helpSeq keep those expressions evaluated in program
	// order.
	defaultSeq, helpSeq int
	exprPkg             *Package
	helpPkg             *Package
	// localHelp is set when the help text is computed from local values, so
	// it cannot move to where the parser is built.
	localHelp bool
	// hintSources are the hints computed at run time, in the order kingpin
	// asked them.
	hintSources []hintSource
	// notes are reported but, unlike issues, don't block the conversion.
	notes   []string
	noEnvar bool
	action  value
	bind    string
	dst     value
	// prior is what dst held when the program parsed, if hasPrior.
	// priorTaken is set once that has been looked at.
	prior      string
	hasPrior   bool
	priorTaken bool
	// seeded is set when dst already held values kong would reset: a list
	// or a map kingpin added to, or a custom value with fields set.
	seeded  bool
	enum    []string
	enumVar *types.Var // enum options computed at run time
	// storageType is the type behind the pointer a value method such as
	// String() returns.
	storageType types.Type
	format      string
	hasFormat   bool

	// conds are the run-time conditions the registration happened under,
	// outermost first.
	conds []cond

	pos    token.Position
	issues []string
	// stmt is the statement that registered the clause, quoted when the
	// conversion cannot handle it.
	stmt dst.Stmt
	// stack holds the statements being run when the clause was registered,
	// outermost first: the calls leading to stmt, then stmt.
	stack []dst.Stmt
	pkg   *Package
}

func (c *clause) issue(msg string) {
	if !slices.Contains(c.issues, msg) {
		c.issues = append(c.issues, msg)
	}
}

// fullName returns the command path the way kingpin's FullCommand does.
func (c *clause) fullName() string {
	if c == nil || c.kind == kindApp {
		return ""
	}
	if c.kind != kindCommand {
		return c.parent.fullName()
	}
	p := c.parent.fullName()
	if p == "" {
		return c.name
	}
	return p + " " + c.name
}

func (c *clause) commands() []*clause {
	var out []*clause
	for _, ch := range c.children {
		if ch.kind == kindCommand {
			out = append(out, ch)
		}
	}
	return out
}

func (c *clause) flags() []*clause {
	var out []*clause
	for _, ch := range c.children {
		if ch.kind == kindFlag {
			out = append(out, ch)
		}
	}
	return out
}

func (c *clause) args() []*clause {
	var out []*clause
	for _, ch := range c.children {
		if ch.kind == kindArg {
			out = append(out, ch)
		}
	}
	return out
}

// registeredUnderCondition reports whether c, or a command holding it, was
// registered under a run-time condition.
func (c *clause) registeredUnderCondition() bool {
	for cl := c; cl != nil; cl = cl.parent {
		if len(cl.conds) > 0 {
			return true
		}
	}
	return false
}

// cond is a run-time condition guarding a registration.
// It is kept as source so the generated code can check it the same way.
type cond struct {
	expr dst.Expr
	neg  bool
	fn   *types.Func
	pkg  *Package
}

// callSite is a call that created kingpin clauses and returned a struct
// pointer.
// It is used to find the constructor of a command's struct.
type callSite struct {
	pkg     *Package
	call    *dst.CallExpr
	fn      *types.Func // the function containing the call
	callee  *types.Func
	args    []value
	result  *object
	clauses []*clause // clauses created during the call
	depth   int
}

// isCumulative reports whether an argument or flag accepts repeated values.
func (c *clause) isCumulative() bool {
	switch c.bind {
	case "StringsVar", "Strings", "IntsVar", "Ints", "EnumsVar", "Enums", "StringMapVar", "StringMap", "Int64ListVar", "BoolListVar":
		return true
	}
	return false
}

// actionLit is a function literal given to Action.
type actionLit struct {
	lit *dst.FuncLit
	pkg *Package
	// before counts the named actions registered before it.
	before int
}

// hintSource is an expression, in pkg, computing the hints given to
// HintOptions at run time, or the function given to HintAction when action
// is set.
type hintSource struct {
	expr   dst.Expr
	pkg    *Package
	action bool
}
