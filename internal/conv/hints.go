package conv

import (
	"fmt"
	"go/token"
	"go/types"
	"sort"
	"strconv"
	"strings"

	"github.com/dave/dst"
)

// hintsMethod returns the source of the methods giving the completion hints
// of n's flags and arguments that kingpin computed at run time, for
// typeName in package pkg: Hints for HintOptions values, and HintActions
// for HintAction functions, which completion only calls when it needs them.
// kongcompat's completion reads both.
// It returns "" when there are none, and false when one of them cannot be
// computed from pkg.
func (c *converter) hintsMethod(n *node, pkg *types.Package, typeName, recv string, imports map[string]string) (string, bool) {
	var options, actions []string
	for _, ch := range n.cl.children {
		var values, funcs []string
		for _, h := range ch.hintSources {
			if !c.movableTo(h.pkg, h.expr, pkg) {
				return "", false
			}
			if h.action {
				funcs = append(funcs, renderIn(h.expr, imports))
			} else {
				values = append(values, renderIn(h.expr, imports))
			}
		}
		// kingpin offered the hints of every call, one after the other.
		if len(values) > 0 {
			entry := values[0]
			if len(values) > 1 {
				entry = "append([]string{}, " + values[0] + "...)"
				for _, v := range values[1:] {
					entry = "append(" + entry + ", " + v + "...)"
				}
			}
			options = append(options, fmt.Sprintf("%s: %s,", strconv.Quote(ch.name), entry))
		}
		if len(funcs) > 0 {
			entry := funcs[0]
			if len(funcs) > 1 {
				calls := "append([]string{}, " + funcs[0] + "()...)"
				for _, f := range funcs[1:] {
					calls = "append(" + calls + ", " + f + "()...)"
				}
				entry = "func() []string {\nreturn " + calls + "\n}"
			}
			actions = append(actions, fmt.Sprintf("%s: %s,", strconv.Quote(ch.name), entry))
		}
	}
	var src []string
	if len(options) > 0 {
		sort.Strings(options)
		src = append(src, fmt.Sprintf(`// Hints returns the completion hints of the flags whose values are only
// known at run time, as kingpin's HintOptions gave them.
func (%s *%s) Hints() map[string][]string {
	return map[string][]string{
%s
	}
}
`, recv, typeName, strings.Join(options, "\n")))
	}
	if len(actions) > 0 {
		sort.Strings(actions)
		src = append(src, fmt.Sprintf(`// HintActions returns the functions completing the flags whose values
// are only known at run time, as kingpin's HintAction did.
func (%s *%s) HintActions() map[string]func() []string {
	return map[string]func() []string{
%s
	}
}
`, recv, typeName, strings.Join(actions, "\n")))
	}
	return strings.Join(src, "\n"), true
}

// reportHints reports the hints of n that could not be kept.
func (c *converter) reportHints(n *node) {
	for _, ch := range n.cl.children {
		if len(ch.hintSources) > 0 {
			c.issue(ch.pos, "%s %q: its completion hints are computed from values the command's package cannot see; shell completion no longer offers them", ch.kind, ch.name)
		}
	}
}

// movableTo reports whether e, written in package from, means the same in
// package to.
// That holds when it only names package-level things of to, exported things
// of other packages, and what it declares itself.
func (c *converter) movableTo(from *Package, e dst.Expr, to *types.Package) bool {
	return c.movableWith(from, e, to, nil)
}

// movableWith is movableTo where the locals local accepts are fine too.
func (c *converter) movableWith(from *Package, e dst.Expr, to *types.Package, local func(*types.Var) bool) bool {
	ok := true
	a := from.astOf(e)
	dst.Inspect(e, func(n dst.Node) bool {
		id, isIdent := n.(*dst.Ident)
		if !isIdent || !ok {
			return ok
		}
		o := from.objectOf(id)
		switch {
		case o == nil || o.Pkg() == nil:
		case o.Parent() == nil:
			// A field or a method, reached through what precedes it.
		case o.Parent() == o.Pkg().Scope():
			// What from declares is named without its package, which only
			// works in from.
			if o.Pkg() != to && (!o.Exported() || o.Pkg() == from.Types) {
				ok = false
			}
		case a != nil && o.Pos() >= a.Pos() && o.Pos() <= a.End():
		default:
			v, isVar := o.(*types.Var)
			ok = isVar && local != nil && local(v)
		}
		return ok
	})
	return ok
}

// convertHints adds a Hints method to every command that has a struct.
// The method goes on the command's own type.
// When only the package of a type it embeds can compute the hints, it goes on
// that type instead, and the command gets it through the embedding.
func (c *converter) convertHints() {
	done := map[*types.Named]string{}
	for _, n := range c.tree.nodes {
		if n.parent == nil || n.typ == nil {
			continue
		}
		hosted := false
		for _, host := range append([]*types.Named{n.typ}, embeddedClosure(n.typ)...) {
			loc, ok := c.ed.typeSpecs[host.Obj()]
			if !ok || !c.prog.owns(host.Obj().Pkg()) {
				continue
			}
			imports := map[string]string{}
			recv := c.receiverName(host, strings.ToLower(host.Obj().Name()[:1]))
			src, ok := c.hintsMethod(n, host.Obj().Pkg(), recvType(host), recv, imports)
			if !ok {
				continue
			}
			hosted = true
			if src == "" {
				break
			}
			if prev, ok := done[host]; ok {
				if prev != src {
					c.issue(n.cl.pos, "commands share %s but complete their values differently; only the first one's hints are kept", host.Obj().Name())
				}
				break
			}
			done[host] = src
			if lookupMethod(host, "Hints") != nil || lookupMethod(host, "HintActions") != nil {
				c.issue(n.cl.pos, "%s already has a member named Hints or HintActions; completion hints computed at run time are not kept", host.Obj().Name())
				break
			}
			decls, err := snippet(loc.pkg.Name, imports, src)
			if err != nil {
				c.issue(token.Position{}, "%v", err)
				break
			}
			c.addMethods(host, decls)
			break
		}
		if !hosted {
			c.reportHints(n)
		}
	}
}
