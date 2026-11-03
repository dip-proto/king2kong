package conv

import (
	"fmt"
	"go/token"
	"go/types"
	"slices"
	"sort"
	"strings"

	"github.com/dave/dst"
)

// pkgFlags holds the application flags that a package other than the CLI's
// declares in package-level variables, as in
//
//	var dir = kingpin.Flag("dir", "...").String()
//
// That package cannot import the CLI, so it gets its own struct for the
// values.
// The CLI embeds that struct and the variables point into it.
type pkgFlags struct {
	pkg      *Package
	typeName string // name of the generated struct
	varName  string // name of the variable holding the values
	flags    []pkgFlag
	file     *dst.File
	decl     *dst.GenDecl // the struct is inserted right before it
	order    int          // earliest registration, to sort the CLI fields
}

// pkgFlag is a flag of a pkgFlags and the variable holding its value.
type pkgFlag struct {
	b    *binding
	v    *types.Var
	spec *dst.ValueSpec
	decl *dst.GenDecl
	file *dst.File
	// field is its name in the struct.
	field string
}

// collectPkgFlags groups by package the application flags held in
// package-level variables outside the root package.
func (c *converter) collectPkgFlags() map[*Package]*pkgFlags {
	root := c.tree.RootPkg
	out := map[*Package]*pkgFlags{}
	for _, b := range c.plan.bindings {
		if !b.storage || b.cl.parent != c.tree.Root.cl {
			continue
		}
		v, p, spec, file, gd := c.storageVar(b.cl)
		if v == nil || p == root {
			continue
		}
		g := out[p]
		if g == nil {
			g = &pkgFlags{pkg: p, file: file, decl: gd, order: 2 * b.cl.id}
			out[p] = g
		}
		g.flags = append(g.flags, pkgFlag{b: b, v: v, spec: spec, decl: gd, file: file})
		if 2*b.cl.id < g.order {
			g.order, g.file, g.decl = 2*b.cl.id, file, gd
		}
	}
	return out
}

// storageVar returns the package-level variable that holds a flag's value
// and where it is declared, when exactly one does.
func (c *converter) storageVar(cl *clause) (*types.Var, *Package, *dst.ValueSpec, *dst.File, *dst.GenDecl) {
	var found *types.Var
	for v, val := range c.g.in.globals {
		if s, ok := val.(vStorage); ok && s.c == cl {
			if found != nil {
				return nil, nil, nil, nil, nil
			}
			found = v
		}
	}
	if found == nil {
		return nil, nil, nil, nil, nil
	}
	p := c.prog.byPath[found.Pkg().Path()]
	if p == nil {
		return nil, nil, nil, nil, nil
	}
	for _, f := range p.Syntax {
		for _, d := range f.Decls {
			gd, ok := d.(*dst.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, sp := range gd.Specs {
				vs := sp.(*dst.ValueSpec)
				// Without an initializer, a function gave it its value, as
				// init does with verbose = kingpin.Flag(...).Bool().
				named := slices.ContainsFunc(vs.Names, func(nm *dst.Ident) bool { return p.objectOf(nm) == found })
				if named && (len(vs.Values) == 0 || len(vs.Values) == len(vs.Names)) {
					return found, p, vs, f, gd
				}
			}
		}
	}
	return nil, nil, nil, nil, nil
}

// pointIntoFlags makes pf's variable start out as field, the address of its
// value in the package's flags struct.
// A variable without a value got the flag from an assignment, which cleanup
// removes, and gets a spec of its own if it shares one with other names.
func (c *converter) pointIntoFlags(p *Package, pf pkgFlag, field dst.Expr) {
	spec, v := pf.spec, pf.v
	c.ed.touch(p, pf.file)
	i := slices.IndexFunc(spec.Names, func(nm *dst.Ident) bool { return p.objectOf(nm) == v })
	if len(spec.Values) > 0 {
		c.reportInitWork(p, v.Name(), spec.Values[i])
		spec.Values[i] = field
		return
	}
	c.pkgFlagVars[v] = true
	if len(spec.Names) == 1 {
		if t := p.typeOf(spec.Type); t != nil && types.Identical(t, types.NewPointer(pf.b.cl.storageType)) {
			spec.Type = nil
		}
		spec.Values = []dst.Expr{field}
		return
	}
	own := &dst.ValueSpec{Names: []*dst.Ident{spec.Names[i]}, Values: []dst.Expr{field}}
	spec.Names = slices.Delete(spec.Names, i, i+1)
	at := slices.Index(pf.decl.Specs, dst.Spec(spec))
	pf.decl.Specs = slices.Insert(pf.decl.Specs, at, dst.Spec(own))
	pf.decl.Lparen = true
}

// convertPkgFlags writes each package's flags struct, points its variables
// into it, and returns the CLI lines embedding the structs.
// rootTaken are the names of the CLI's fields, which the embedded structs
// must not use.
func (c *converter) convertPkgFlags(groups map[*Package]*pkgFlags, imports map[string]string, rootTaken map[string]bool) []string {
	var list []*pkgFlags
	for _, g := range groups {
		list = append(list, g)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].order < list[j].order })
	var lines []string
	for _, g := range list {
		// An embedded field is named after its type, so two packages cannot
		// both use Flags.
		base := "Flags"
		if len(list) > 1 {
			base = c.prog.exported(g.pkg.Name) + "Flags"
		}
		g.typeName = c.freeName(g.pkg, base)
		for rootTaken[g.typeName] {
			g.typeName = c.freeName(g.pkg, g.typeName+"Values")
		}
		rootTaken[g.typeName] = true
		g.varName = c.freeName(g.pkg, "Flag", "FlagValues")
		pkgImports := map[string]string{}
		var fields []string
		taken := map[string]bool{}
		for i := range g.flags {
			b := g.flags[i].b
			name := c.prog.identFromName(b.cl.name)
			for taken[name] {
				name += "Value"
			}
			taken[name] = true
			g.flags[i].field = name
			tag := c.plan.tagFor(b, name)
			fields = append(fields, fmt.Sprintf("%s %s %s", name, typeExpr(b.cl.storageType, g.pkg.Types, pkgImports), tagLiteral(tag.String())))
		}
		src := fmt.Sprintf(`// %[1]s are the command-line flags the %[3]s package declares. The
// program's CLI embeds them.
type %[1]s struct {
%[4]s
}

// %[2]s holds the values of the %[3]s package's flags.
var %[2]s %[1]s
`, g.typeName, g.varName, g.pkg.Name, strings.Join(fields, "\n"))
		decls, err := snippet(g.pkg.Name, pkgImports, src)
		if err != nil {
			c.issue(g.flags[0].b.cl.pos, "%v", err)
			continue
		}
		c.ed.insertBefore(g.pkg, g.file, g.decl, decls)
		for _, pf := range g.flags {
			c.pointIntoFlags(g.pkg, pf, &dst.UnaryExpr{Op: token.AND, X: &dst.SelectorExpr{X: dst.NewIdent(g.varName), Sel: dst.NewIdent(pf.field)}})
		}
		pkgName := importName(imports, g.pkg.PkgPath, g.pkg.Name)
		line := fmt.Sprintf("*%s.%s", pkgName, g.typeName)
		c.fieldOrder[line] = g.order
		lines = append(lines, line)
		c.cliInits = append(c.cliInits, cliInit{field: g.typeName, pkgPath: g.pkg.PkgPath, varName: g.varName})
	}
	return lines
}
