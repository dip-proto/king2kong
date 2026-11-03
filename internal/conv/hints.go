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

// hintsMethod returns the source of a Hints method on typeName, in package
// pkg, giving the completion hints that kingpin's HintOptions or HintAction
// computed at run time for n's flags and arguments.
// kongcompat's completion reads it.
// It returns "" when there are none, and false when one of them cannot be
// computed from pkg.
func (c *converter) hintsMethod(n *node, pkg *types.Package, typeName, recv string, imports map[string]string) (string, bool) {
	var entries []string
	for _, ch := range n.cl.children {
		if ch.hintExpr == nil {
			continue
		}
		if !c.movableTo(ch.hintPkg, ch.hintExpr, pkg) {
			return "", false
		}
		entries = append(entries, fmt.Sprintf("%s: %s,", strconv.Quote(ch.name), renderIn(ch.hintExpr, imports)))
	}
	if len(entries) == 0 {
		return "", true
	}
	sort.Strings(entries)
	return fmt.Sprintf(`// Hints returns the completion hints of the flags whose values are only
// known at run time, as kingpin's HintOptions and HintAction gave them.
func (%s *%s) Hints() map[string][]string {
	return map[string][]string{
%s
	}
}
`, recv, typeName, strings.Join(entries, "\n")), true
}

// reportHints reports the hints of n that could not be kept.
func (c *converter) reportHints(n *node) {
	for _, ch := range n.cl.children {
		if ch.hintExpr != nil {
			c.issue(ch.pos, "%s %q: its completion hints are computed from values the command's package cannot see; shell completion no longer offers them", ch.kind, ch.name)
		}
	}
}

// movableTo reports whether e, written in package from, means the same in
// package to.
// That holds when it only names package-level things of to, exported things
// of other packages, and what it declares itself.
func (c *converter) movableTo(from *Package, e dst.Expr, to *types.Package) bool {
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
			ok = false
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
	done := map[*types.Named]bool{}
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
			if src == "" || done[host] {
				break
			}
			done[host] = true
			if c.hasMethod(host, "Hints") {
				c.issue(n.cl.pos, "%s already has a Hints method; completion hints computed at run time are not kept", host.Obj().Name())
				break
			}
			decls, err := snippet(loc.pkg.Name, imports, src)
			if err != nil {
				c.issue(token.Position{}, "%v", err)
				break
			}
			c.ed.insertAfter(loc.pkg, loc.file, c.lastMethodOrType(host, loc.gen), decls)
			break
		}
		if !hosted {
			c.reportHints(n)
		}
	}
}
