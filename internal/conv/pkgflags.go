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
	bindings []*binding
	specs    map[*binding]*dst.ValueSpec
	vars     map[*binding]*types.Var
	// specDecls are the declarations holding the specs.
	specDecls map[*dst.ValueSpec]*dst.GenDecl
	file      *dst.File
	decl      *dst.GenDecl // the struct is inserted right before it
	order     int          // earliest registration, to sort the CLI fields
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
			g = &pkgFlags{pkg: p, specs: map[*binding]*dst.ValueSpec{}, vars: map[*binding]*types.Var{}, specDecls: map[*dst.ValueSpec]*dst.GenDecl{}, file: file, decl: gd, order: 2 * b.cl.id}
			out[p] = g
		}
		g.bindings = append(g.bindings, b)
		g.specs[b] = spec
		g.vars[b], g.specDecls[spec] = v, gd
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

// pointIntoFlags makes the variable of b start as field, the address of its
// value in the package's flags struct.
// A variable declared with others and no values gets a declaration of its
// own, and the assignment that gave it the flag's value goes.
func (c *converter) pointIntoFlags(g *pkgFlags, b *binding, field dst.Expr) {
	spec, v := g.specs[b], g.vars[b]
	i := slices.IndexFunc(spec.Names, func(nm *dst.Ident) bool { return g.pkg.objectOf(nm) == v })
	if len(spec.Values) > 0 {
		c.reportInitWork(g.pkg, v.Name(), spec.Values[i])
		spec.Values[i] = field
		return
	}
	c.pkgFlagVars[v] = true
	if len(spec.Names) == 1 {
		if t := g.pkg.typeOf(spec.Type); t != nil && types.Identical(t, types.NewPointer(b.cl.storageType)) {
			spec.Type = nil
		}
		spec.Values = []dst.Expr{field}
		return
	}
	own := &dst.ValueSpec{Names: []*dst.Ident{spec.Names[i]}, Values: []dst.Expr{field}}
	spec.Names = slices.Delete(spec.Names, i, i+1)
	gd := g.specDecls[spec]
	at := slices.Index(gd.Specs, dst.Spec(spec))
	gd.Specs = slices.Insert(gd.Specs, at, dst.Spec(own))
	gd.Lparen = true
}

// convertPkgFlags writes each package's flags struct, points its variables
// into it, and returns the CLI lines embedding the structs.
func (c *converter) convertPkgFlags(groups map[*Package]*pkgFlags, imports map[string]string) []string {
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
		g.varName = c.freeName(g.pkg, "Flag", "FlagValues")
		pkgImports := map[string]string{}
		var fields []string
		taken := map[string]bool{}
		names := map[*binding]string{}
		for _, b := range g.bindings {
			name := c.prog.identFromName(b.cl.name)
			for taken[name] {
				name += "Value"
			}
			taken[name] = true
			names[b] = name
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
			c.issue(g.bindings[0].cl.pos, "%v", err)
			continue
		}
		c.ed.insertBefore(g.pkg, g.file, g.decl, decls)
		for _, b := range g.bindings {
			c.pointIntoFlags(g, b, &dst.UnaryExpr{Op: token.AND, X: &dst.SelectorExpr{X: dst.NewIdent(g.varName), Sel: dst.NewIdent(names[b])}})
		}
		for _, f := range g.pkg.Syntax {
			if f == g.file {
				c.ed.touch(g.pkg, f)
			}
		}
		for _, b := range g.bindings {
			for _, f := range g.pkg.Syntax {
				for _, d := range f.Decls {
					if gd, ok := d.(*dst.GenDecl); ok {
						for _, sp := range gd.Specs {
							if sp == g.specs[b] {
								c.ed.touch(g.pkg, f)
							}
						}
					}
				}
			}
		}
		pkgName := importName(imports, g.pkg.PkgPath, g.pkg.Name)
		line := fmt.Sprintf("*%s.%s", pkgName, g.typeName)
		c.fieldOrder[line] = g.order
		lines = append(lines, line)
		c.cliInits = append(c.cliInits, cliInit{field: g.typeName, pkgPath: g.pkg.PkgPath, varName: g.varName})
	}
	return lines
}
