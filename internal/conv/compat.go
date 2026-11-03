package conv

import (
	"bytes"
	"fmt"
	"go/token"
	"go/types"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/dave/dst"
	"github.com/dave/dst/dstutil"
)

// writeCompat adds the kongcompat package to the converted module, so the
// program keeps kingpin's help command, help output, error wording and shell
// completion on top of kong.
func (c *converter) writeCompat() {
	if c.opts.Compat == nil || len(c.prog.Packages) == 0 {
		return
	}
	mod := c.prog.Packages[0].Module
	if mod == nil {
		c.issue(token.Position{}, "the target is not a Go module; kongcompat was not written")
		return
	}
	c.compatPath = mod.Path + "/internal/kongcompat"
	for _, p := range c.prog.Packages {
		p.names[c.compatPath] = "kongcompat"
	}
	dir := filepath.Join(mod.Dir, "internal", "kongcompat")
	entries, err := fs.ReadDir(c.opts.Compat, ".")
	if err != nil {
		c.issue(token.Position{}, "reading kongcompat sources: %v", err)
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := fs.ReadFile(c.opts.Compat, name)
		if err != nil {
			c.issue(token.Position{}, "reading kongcompat sources: %v", err)
			continue
		}
		if name == "flavor.go" {
			src = []byte(c.flavor())
		}
		if name == "durations_v2.go" && (c.prog.kingpinV3 || c.prog.kingpinV2Legacy) {
			// Only kingpin v2.3.0 and later parsed durations with str2duration,
			// and only their programs depend on it.
			continue
		}
		if name == "app_v2.go" || name == "app_v3.go" {
			// These hold the methods whose signature changed between kingpin
			// v2 and v3.
			// Keep the one matching the program, minus the build constraint
			// that picks one for kongcompat's own tests.
			if (name == "app_v3.go") != c.prog.kingpinV3 {
				continue
			}
			src = withoutBuildConstraint(src)
		}
		c.res.NewFiles[filepath.Join(dir, name)] = string(src)
	}
}

// flavor is kongcompat's flavor.go for the program's kingpin version.
func (c *converter) flavor() string {
	v, legacy, origin := 2, false, "kingpin v2.3.0 or later"
	switch {
	case c.prog.kingpinV3:
		v, origin = 3, "kingpin v3 or a fork based on it"
	case c.prog.kingpinV2Legacy:
		legacy, origin = true, "kingpin v2 before v2.3.0"
	}
	actionParams := "element *ParseElement, context *ParseContext"
	if !c.prog.kingpinV3 {
		actionParams = "context *ParseContext"
	}
	return fmt.Sprintf(`package kongcompat

// The program was converted from %s.

// version is the major version of kingpin whose behavior the package
// reproduces.
const version = %d

// legacy is set for kingpin v2 releases before v2.3.0, whose help was
// closer to kingpin v3's.
const legacy = %t

// v2From23 reports whether the program came from kingpin v2.3.0 or later,
// whose help shows --[no-] forms, environment variables and argument
// placeholders, and which names every missing required flag.
func v2From23() bool {
	return version == 2 && !legacy
}

// Action is the type of kingpin's callbacks, for code that kept values of
// it.
type Action func(%s) error
`, origin, v, legacy, actionParams)
}

// convertCommandNames replaces a kingpin command clause kept in a struct only
// for its FullCommand with a string field holding the command's full name.
// kongcompat fills that field when the parser is built.
func (c *converter) convertCommandNames() {
	for f, loc := range c.ed.fields {
		if !c.prog.isKingpinType(f.Type()) {
			continue
		}
		owner := c.nm.declaringType(f)
		if owner == nil || c.isRemovedType(owner) {
			continue
		}
		// Every remaining use must be x.Field.FullCommand(): check them all
		// before changing anything.
		type use struct {
			p    *Package
			file *dst.File
		}
		var uses []use
		other := 0
		for _, p := range c.prog.Packages {
			for _, file := range p.Syntax {
				for _, d := range file.Decls {
					if c.ed.removed[d] {
						continue
					}
					full := map[*dst.Ident]bool{}
					dst.Inspect(d, func(n dst.Node) bool {
						if call, ok := n.(*dst.CallExpr); ok && len(call.Args) == 0 {
							if sel, ok := call.Fun.(*dst.SelectorExpr); ok && sel.Sel.Name == "FullCommand" {
								if inner, ok := sel.X.(*dst.SelectorExpr); ok && p.objectOf(inner) == f {
									full[inner.Sel] = true
								}
							}
						}
						return true
					})
					dst.Inspect(d, func(n dst.Node) bool {
						if id, ok := n.(*dst.Ident); ok && id.Name == f.Name() && p.objectOf(id) == f && !isDeclName(loc, id) && !full[id] {
							other++
						}
						return true
					})
					if len(full) > 0 {
						uses = append(uses, use{p, file})
					}
				}
			}
		}
		if len(uses) == 0 && other == 0 {
			// Nothing reads the clause any more: drop the field, which kong
			// would otherwise try to turn into a flag.
			var kept []*dst.Field
			for _, fl := range loc.st.Fields.List {
				if fl != loc.field {
					kept = append(kept, fl)
				}
			}
			loc.st.Fields.List = kept
			c.ed.touch(loc.pkg, loc.file)
			continue
		}
		if other > 0 {
			c.issue(c.prog.byPath[f.Pkg().Path()].position(loc.field), "field %s holds a kingpin clause that is still used for more than its name", f.Name())
			continue
		}
		if c.compatPath == "" {
			c.issue(c.prog.byPath[f.Pkg().Path()].position(loc.field), "field %s holds a kingpin clause for its name; without kongcompat nothing can give it", f.Name())
			continue
		}
		name := c.nm.newField(owner, "CommandName")
		if name == "" {
			continue
		}
		for _, u := range uses {
			for _, d := range u.file.Decls {
				if c.ed.removed[d] {
					continue
				}
				dstutil.Apply(d, func(cur *dstutil.Cursor) bool {
					call, ok := cur.Node().(*dst.CallExpr)
					if !ok || len(call.Args) != 0 {
						return true
					}
					sel, ok := call.Fun.(*dst.SelectorExpr)
					if !ok || sel.Sel.Name != "FullCommand" {
						return true
					}
					inner, ok := sel.X.(*dst.SelectorExpr)
					if !ok || u.p.objectOf(inner) != f {
						return true
					}
					cur.Replace(&dst.SelectorExpr{X: inner.X, Sel: dst.NewIdent(name)})
					return false
				}, nil)
			}
			c.ed.touch(u.p, u.file)
		}
		// kongcompat fills the field when the parser is built, so the name
		// is there from the start, as FullCommand was.
		loc.field.Names = []*dst.Ident{dst.NewIdent(name)}
		loc.field.Type = dst.NewIdent("string")
		loc.field.Tag = &dst.BasicLit{Kind: token.STRING, Value: "`kong:\"-\" kongcompat:\"command-name\"`"}
		loc.field.Decs.Start.Replace("// " + name + " is the command's full name, as kingpin's FullCommand gave it;\n// kongcompat sets it when the parser is built.")
		// A commented field stands apart from the others.
		if len(loc.st.Fields.List) > 0 && loc.st.Fields.List[0] == loc.field {
			loc.field.Decs.Before, loc.field.Decs.After = dst.NewLine, dst.EmptyLine
		} else {
			loc.field.Decs.Before = dst.EmptyLine
		}
		c.ed.touch(loc.pkg, loc.file)
		c.commandNames = true
	}
}

func isDeclName(loc fieldLoc, id *dst.Ident) bool {
	for _, n := range loc.field.Names {
		if n == id {
			return true
		}
	}
	return false
}

// isRemovedType reports whether the declaration of a type was removed.
func (c *converter) isRemovedType(t *types.Named) bool {
	loc, ok := c.ed.typeSpecs[t.Obj()]
	return ok && c.ed.removed[loc.gen]
}

// withoutBuildConstraint removes the //go:build line starting a file.
func withoutBuildConstraint(src []byte) []byte {
	if !bytes.HasPrefix(src, []byte("//go:build ")) {
		return src
	}
	_, rest, _ := bytes.Cut(src, []byte("\n"))
	return bytes.TrimLeft(rest, "\n")
}
