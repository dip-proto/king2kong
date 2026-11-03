package conv

import (
	"cmp"
	"fmt"
	"go/token"
	"go/types"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/dave/dst"
)

// convertVars gives kong the defaults and help texts only known at run
// time, which the tags refer to as ${name}.
// When the parser is built in main, the ones main can compute go in a
// kongcompat.Vars option.
// The others go in a Vars method on the command type, or on a type it
// embeds, in a package that can compute them.
// kongcompat reads these methods when it builds the parser.
func (c *converter) convertVars() {
	if len(c.plan.vars) == 0 {
		return
	}
	// Keep the order kingpin computed them in.
	sort.SliceStable(c.plan.vars, func(i, j int) bool { return c.plan.vars[i].seq < c.plan.vars[j].seq })
	root := c.tree.RootPkg.Types
	byClause := map[*clause][]runtimeVar{}
	for _, v := range c.plan.vars {
		if c.main != nil && v.pkg.Types == root && c.movableTo(v.pkg, v.expr, root) {
			c.main.vars = append(c.main.vars, v)
			continue
		}
		byClause[v.cl] = append(byClause[v.cl], v)
	}
	var hosted []runtimeVar
	for _, n := range c.tree.nodes {
		// A command hosts its own help and the help of its flags, not the
		// help of its subcommands.
		var vars []runtimeVar
		if n.cl.kind == kindCommand {
			vars = append(vars, byClause[n.cl]...)
		}
		for _, ch := range n.cl.children {
			if ch.kind != kindCommand {
				vars = append(vars, byClause[ch]...)
			}
		}
		if len(vars) == 0 {
			continue
		}
		sort.SliceStable(vars, func(i, j int) bool { return vars[i].seq < vars[j].seq })
		hosted = append(hosted, vars...)
		if n.parent == nil && c.main == nil && c.allMovable(vars, root) {
			c.rootVars = vars
			continue
		}
		c.hostVars(n, vars)
	}
	// Each command computes its own, so calls with side effects may now run
	// in a different order than with kingpin.
	if !slices.IsSortedFunc(hosted, func(a, b runtimeVar) int { return cmp.Compare(a.seq, b.seq) }) && slices.ContainsFunc(hosted, func(v runtimeVar) bool { return hasCall(v.expr) }) {
		c.res.Notes = append(c.res.Notes, "defaults and help texts computed at run time by several commands are now computed command by command, in another order than kingpin computed them.")
	}
}

func (c *converter) allMovable(vars []runtimeVar, to *types.Package) bool {
	for _, v := range vars {
		if !c.movableTo(v.pkg, v.expr, to) {
			return false
		}
	}
	return true
}

// hostVars adds a Vars method returning vars to n's type, or to a type it
// embeds, picking the first whose package can compute them all.
func (c *converter) hostVars(n *node, vars []runtimeVar) {
	t := c.flagsType(n)
	var hosts []*types.Named
	if t != nil {
		hosts = append([]*types.Named{t}, embeddedClosure(t)...)
	}
	for _, host := range hosts {
		loc, ok := c.ed.typeSpecs[host.Obj()]
		if !ok || !c.prog.owns(host.Obj().Pkg()) || !c.allMovable(vars, host.Obj().Pkg()) {
			continue
		}
		if c.hasMethod(host, "Vars") {
			break
		}
		imports := map[string]string{}
		recv := c.receiverName(host, strings.ToLower(host.Obj().Name()[:1]))
		decls, err := snippet(loc.pkg.Name, imports, varsMethod(vars, recvType(host), recv, c.compatPath, imports))
		if err != nil {
			c.issue(token.Position{}, "%v", err)
			return
		}
		c.ed.insertAfter(loc.pkg, loc.file, c.lastMethodOrType(host, loc.gen), decls)
		c.commandVars = true
		return
	}
	for _, v := range vars {
		c.issue(v.cl.pos, "%s %q: its %s is computed from what no command type's package can see; give kong the %s variable by hand", v.cl.kind, v.cl.name, v.what, v.name)
	}
}

// varsMethod returns the source of a Vars method on typeName returning vars.
func varsMethod(vars []runtimeVar, typeName, recv, compatPath string, imports map[string]string) string {
	var entries []string
	for _, v := range vars {
		entries = append(entries, fmt.Sprintf("%s: %s,", strconv.Quote(v.name), renderIn(v.expr, imports)))
	}
	name := importName(imports, compatPath, "kongcompat")
	return fmt.Sprintf(`// Vars gives kong the default values and help texts only known at run
// time, which the tags refer to.
func (%[1]s *%[2]s) Vars() %[4]s.Vars {
	return %[4]s.Vars{
%[3]s
	}
}
`, recv, typeName, strings.Join(entries, "\n"), name)
}

// varsElts returns the kongcompat.Vars entries for vars, for a parser built
// in the package that computes them.
func varsElts(vars []runtimeVar) []dst.Expr {
	var out []dst.Expr
	for _, v := range vars {
		out = append(out, &dst.KeyValueExpr{
			Key:   &dst.BasicLit{Kind: token.STRING, Value: strconv.Quote(v.name)},
			Value: dst.Clone(v.expr).(dst.Expr),
		})
	}
	return out
}
