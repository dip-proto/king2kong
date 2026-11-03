package conv

import (
	"fmt"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	"github.com/dave/dst"
	"github.com/dave/dst/decorator"
	"github.com/dave/dst/decorator/resolver"
	"github.com/dave/dst/decorator/resolver/goast"
	"github.com/dave/dst/decorator/resolver/guess"
)

// editor applies changes to the decorated syntax of the loaded packages and
// remembers which files changed.
type editor struct {
	prog  *Program
	dirty map[*dst.File]*Package

	typeSpecs map[*types.TypeName]typeLoc
	fields    map[*types.Var]fieldLoc
	methods   map[*types.Named]map[string]funcDecl
	// removed holds declarations scheduled for deletion.
	removed map[dst.Decl]bool
	// appended records structs that already received new fields, so later
	// additions join the same group.
	appended map[*dst.StructType]bool
}

type typeLoc struct {
	pkg  *Package
	file *dst.File
	gen  *dst.GenDecl
	spec *dst.TypeSpec
}

type fieldLoc struct {
	pkg   *Package
	file  *dst.File
	st    *dst.StructType
	field *dst.Field
}

func newEditor(prog *Program) *editor {
	e := &editor{
		prog:      prog,
		dirty:     map[*dst.File]*Package{},
		typeSpecs: map[*types.TypeName]typeLoc{},
		fields:    map[*types.Var]fieldLoc{},
		methods:   map[*types.Named]map[string]funcDecl{},
		removed:   map[dst.Decl]bool{},
		appended:  map[*dst.StructType]bool{},
	}
	for _, p := range prog.Packages {
		for _, f := range p.Syntax {
			for _, d := range f.Decls {
				switch d := d.(type) {
				case *dst.GenDecl:
					if d.Tok != token.TYPE {
						continue
					}
					for _, s := range d.Specs {
						ts := s.(*dst.TypeSpec)
						if tn, ok := p.objectOf(ts.Name).(*types.TypeName); ok {
							e.typeSpecs[tn] = typeLoc{p, f, d, ts}
						}
					}
				case *dst.FuncDecl:
					if d.Recv == nil || len(d.Recv.List) != 1 {
						continue
					}
					fn, ok := p.objectOf(d.Name).(*types.Func)
					if !ok {
						continue
					}
					recv := fn.Type().(*types.Signature).Recv()
					if n, ok := deref(recv.Type()).(*types.Named); ok {
						if e.methods[n] == nil {
							e.methods[n] = map[string]funcDecl{}
						}
						e.methods[n][d.Name.Name] = funcDecl{p, f, d}
					}
				}
			}
			dst.Inspect(f, func(n dst.Node) bool {
				st, ok := n.(*dst.StructType)
				if !ok || st.Fields == nil {
					return true
				}
				for _, fld := range st.Fields.List {
					if len(fld.Names) == 0 {
						if v, ok := p.objectOf(embeddedIdent(fld.Type)).(*types.Var); ok && v.IsField() {
							e.fields[v] = fieldLoc{p, f, st, fld}
						}
						continue
					}
					for _, nm := range fld.Names {
						if v, ok := p.objectOf(nm).(*types.Var); ok && v.IsField() {
							e.fields[v] = fieldLoc{p, f, st, fld}
						}
					}
				}
				return true
			})
		}
	}
	return e
}

// embeddedIdent returns the identifier naming an embedded field's type.
func embeddedIdent(t dst.Expr) dst.Node {
	switch t := t.(type) {
	case *dst.StarExpr:
		return embeddedIdent(t.X)
	case *dst.SelectorExpr:
		return t.Sel
	case *dst.IndexExpr:
		return embeddedIdent(t.X)
	case *dst.IndexListExpr:
		return embeddedIdent(t.X)
	}
	return t
}

func (e *editor) touch(p *Package, f *dst.File) { e.dirty[f] = p }

// fieldDecl returns the declaration of a field, splitting a grouped
// declaration such as `a, b string` so the field can carry its own tag.
func (e *editor) fieldDecl(v *types.Var) (fieldLoc, bool) {
	loc, ok := e.fields[v]
	if !ok {
		return loc, false
	}
	if len(loc.field.Names) <= 1 {
		return loc, true
	}
	var list []*dst.Field
	for _, fld := range loc.st.Fields.List {
		if fld != loc.field {
			list = append(list, fld)
			continue
		}
		for i, nm := range fld.Names {
			nf := &dst.Field{Names: []*dst.Ident{nm}, Type: dst.Clone(fld.Type).(dst.Expr)}
			if fld.Tag != nil {
				nf.Tag = dst.Clone(fld.Tag).(*dst.BasicLit)
			}
			if i == 0 {
				nf.Decs = fld.Decs
				nf.Decs.After = dst.NewLine
			} else {
				nf.Decs.Before = dst.NewLine
				nf.Decs.After = dst.NewLine
			}
			list = append(list, nf)
			if o, ok := loc.pkg.objectOf(nm).(*types.Var); ok {
				e.fields[o] = fieldLoc{loc.pkg, loc.file, loc.st, nf}
			}
		}
	}
	loc.st.Fields.List = list
	e.touch(loc.pkg, loc.file)
	return e.fields[v], true
}

// addTag merges kong keys into a field's struct tag.
// A key the field already had, such as default for another library, gives
// way to the new one.
// It returns the keys of the old tag that kong reads too, replaced or not.
func (e *editor) addTag(v *types.Var, tag string) ([]string, error) {
	loc, ok := e.fieldDecl(v)
	if !ok {
		return nil, fmt.Errorf("declaration of field %s not found", v.Name())
	}
	existing := ""
	if loc.field.Tag != nil {
		s, err := strconv.Unquote(loc.field.Tag.Value)
		if err != nil {
			return nil, fmt.Errorf("field %s has an unreadable tag: %w", v.Name(), err)
		}
		existing = s
	}
	if _, has := reflect.StructTag(existing).Lookup("kong"); has {
		return nil, fmt.Errorf("field %s already has a kong tag", v.Name())
	}
	added := map[string]bool{}
	for _, kv := range tagPairs(tag) {
		added[kv[0]] = true
	}
	var kept, shared []string
	for _, kv := range tagPairs(existing) {
		if kongKeys[kv[0]] {
			shared = append(shared, kv[0])
		}
		if !added[kv[0]] {
			kept = append(kept, kv[0]+":"+strconv.Quote(kv[1]))
		}
	}
	tag = strings.TrimSpace(strings.Join(append(kept, tag), " "))
	if tag == "" {
		return nil, nil
	}
	loc.field.Tag = &dst.BasicLit{Kind: token.STRING, Value: tagLiteral(tag)}
	e.touch(loc.pkg, loc.file)
	return shared, nil
}

// kongKeys are the struct tag keys kong reads outside a kong:"..." tag.
var kongKeys = map[string]bool{
	"aliases": true, "and": true, "arg": true, "cmd": true, "default": true, "embed": true, "enum": true,
	"env": true, "envprefix": true, "format": true, "group": true, "help": true, "hidden": true,
	"lastwins": true, "mapsep": true, "name": true, "negatable": true, "optional": true,
	"passthrough": true, "placeholder": true, "prefix": true, "required": true, "sep": true,
	"set": true, "short": true, "type": true, "xor": true, "xorprefix": true,
}

// tagPairs splits a struct tag into its key and value pairs, the way
// reflect.StructTag.Lookup reads it.
func tagPairs(tag string) [][2]string {
	var out [][2]string
	for tag != "" {
		tag = strings.TrimLeft(tag, " ")
		i := 0
		for i < len(tag) && tag[i] > ' ' && tag[i] != ':' && tag[i] != '"' && tag[i] != 0x7f {
			i++
		}
		if i == 0 || i+1 >= len(tag) || tag[i] != ':' || tag[i+1] != '"' {
			break
		}
		name := tag[:i]
		tag = tag[i+1:]
		i = 1
		for i < len(tag) && tag[i] != '"' {
			if tag[i] == '\\' {
				i++
			}
			i++
		}
		if i >= len(tag) {
			break
		}
		value, err := strconv.Unquote(tag[:i+1])
		if err != nil {
			break
		}
		out = append(out, [2]string{name, value})
		tag = tag[i+1:]
	}
	return out
}

func tagLiteral(tag string) string {
	if strings.ContainsRune(tag, '`') {
		return strconv.Quote(tag)
	}
	return "`" + tag + "`"
}

// renameField renames a field everywhere it is used in the loaded packages.
// Test files are handled separately.
func (e *editor) renameField(v *types.Var, name string) {
	for _, p := range e.prog.Packages {
		for _, f := range p.Syntax {
			changed := false
			dst.Inspect(f, func(n dst.Node) bool {
				id, ok := n.(*dst.Ident)
				if !ok || id.Name != v.Name() {
					return true
				}
				if p.objectOf(id) == v {
					id.Name = name
					changed = true
				}
				return true
			})
			if changed {
				e.touch(p, f)
			}
		}
	}
}

// snippet parses Go declarations written against the given imports, and
// returns them with package references resolved, ready to go into a file of
// package pkgName.
// The restorer adds whatever imports the inserted code needs.
func snippet(pkgName string, imports map[string]string, src string) ([]dst.Decl, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "package %s\n\n", pkgName)
	if len(imports) > 0 {
		b.WriteString("import (\n")
		for path, name := range imports {
			fmt.Fprintf(&b, "\t%s %q\n", name, path)
		}
		b.WriteString(")\n\n")
	}
	b.WriteString(src)
	fset := token.NewFileSet()
	af, err := parser.ParseFile(fset, "snippet.go", b.String(), parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("generated code does not parse: %w\n%s", err, b.String())
	}
	d := decorator.NewDecoratorWithImports(fset, pkgName, goast.New())
	df, err := d.DecorateFile(af)
	if err != nil {
		return nil, err
	}
	var out []dst.Decl
	for _, decl := range df.Decls {
		if gd, ok := decl.(*dst.GenDecl); ok && gd.Tok == token.IMPORT {
			continue
		}
		out = append(out, decl)
	}
	return out, nil
}

// insertAfter places decls in file right after the anchor declaration, or at
// the end of the file when anchor is nil.
func (e *editor) insertAfter(p *Package, file *dst.File, anchor dst.Decl, decls []dst.Decl) {
	var out []dst.Decl
	placed := false
	for _, d := range file.Decls {
		out = append(out, d)
		if d == anchor {
			out = append(out, decls...)
			placed = true
		}
	}
	if !placed {
		out = append(out, decls...)
	}
	for _, d := range decls {
		spaceBefore(d)
	}
	file.Decls = out
	e.touch(p, file)
}

// replaceDecl swaps a declaration for others, keeping its position.
func (e *editor) replaceDecl(p *Package, file *dst.File, old dst.Decl, decls []dst.Decl) {
	var out []dst.Decl
	for _, d := range file.Decls {
		if d == old {
			out = append(out, decls...)
			continue
		}
		out = append(out, d)
	}
	for _, d := range decls {
		spaceBefore(d)
	}
	file.Decls = out
	e.touch(p, file)
}

func spaceBefore(d dst.Decl) {
	switch d := d.(type) {
	case *dst.FuncDecl:
		d.Decs.Before = dst.EmptyLine
	case *dst.GenDecl:
		d.Decs.Before = dst.EmptyLine
	}
}

// removeDecl schedules a top-level declaration for deletion.
// It stays in the file until rendering, so generated code can take its place.
func (e *editor) removeDecl(p *Package, file *dst.File, decl dst.Decl) {
	e.removed[decl] = true
	e.touch(p, file)
}

// insertBefore adds decls right before anchor, or at the end of the file.
func (e *editor) insertBefore(p *Package, file *dst.File, anchor dst.Decl, decls []dst.Decl) {
	var out []dst.Decl
	placed := false
	for _, d := range file.Decls {
		if d == anchor && !placed {
			out = append(out, decls...)
			placed = true
		}
		out = append(out, d)
	}
	if !placed {
		out = append(out, decls...)
	}
	file.Decls = out
	e.touch(p, file)
}

// insertAt places decls where a declaration scheduled for deletion is.
func (e *editor) insertAt(p *Package, file *dst.File, at dst.Decl, decls []dst.Decl) {
	e.insertAfter(p, file, at, decls)
}

// structType returns the struct type declaration of a named type.
func (e *editor) structType(n *types.Named) (typeLoc, *dst.StructType, bool) {
	loc, ok := e.typeSpecs[n.Obj()]
	if !ok {
		return loc, nil, false
	}
	st, ok := loc.spec.Type.(*dst.StructType)
	return loc, st, ok
}

// appendFields adds fields at the end of a struct, after an empty line.
func (e *editor) appendFields(n *types.Named, fields []*dst.Field) bool {
	loc, st, ok := e.structType(n)
	if !ok {
		return false
	}
	for i, f := range fields {
		f.Decs.Before = dst.NewLine
		if i == 0 && len(st.Fields.List) > 0 && !e.appended[st] {
			f.Decs.Before = dst.EmptyLine
		}
		f.Decs.After = dst.NewLine
	}
	st.Fields.List = append(st.Fields.List, fields...)
	if len(fields) > 0 {
		e.appended[st] = true
	}
	e.touch(loc.pkg, loc.file)
	return true
}

// preferredAliases are the import aliases the program itself uses, reused for
// generated code so it reads like the code around it.
var preferredAliases = map[string]string{}

// typeExpr renders a type as source relative to pkg, recording the imports it
// needs.
// imports maps an import path to the name generated code uses for it.
// Two packages with the same name get distinct aliases.
func typeExpr(t types.Type, pkg *types.Package, imports map[string]string) string {
	return types.TypeString(t, func(p *types.Package) string {
		if p == pkg {
			return ""
		}
		return importName(imports, p.Path(), p.Name())
	})
}

func importName(imports map[string]string, path, name string) string {
	return importNameAvoiding(imports, path, name, nil)
}

// importNameAvoiding is importName, also avoiding the names in taken when it
// picks a new one.
func importNameAvoiding(imports map[string]string, path, name string, taken map[string]bool) string {
	if a, ok := imports[path]; ok {
		return a
	}
	used := func(a string) bool {
		if taken[a] {
			return true
		}
		for p, b := range imports {
			if b == a && p != path {
				return true
			}
		}
		return false
	}
	alias := name
	if used(alias) {
		alias = preferredAliases[path]
		if alias == "" || used(alias) {
			parent := filepath.Base(filepath.Dir(path))
			alias = strings.ToLower(parent) + name
			for i := 2; used(alias); i++ {
				alias = fmt.Sprintf("%s%d", name, i)
			}
		}
	}
	imports[path] = alias
	return alias
}

// importNames resolves import paths to the names the package's imports
// actually declare, falling back to the last path element.
func (p *Package) importNames() resolver.RestorerResolver {
	return importResolver{names: p.names, fallback: guess.New()}
}

type importResolver struct {
	names    map[string]string
	fallback resolver.RestorerResolver
}

func (r importResolver) ResolvePackage(path string) (string, error) {
	if n, ok := r.names[path]; ok {
		return n, nil
	}
	if n, ok := knownPackageNames[path]; ok {
		return n, nil
	}
	return r.fallback.ResolvePackage(path)
}

// knownPackageNames covers packages generated code imports whose name differs
// from the last element of their path.
var knownPackageNames = map[string]string{
	"github.com/alecthomas/kong": "kong",
}
