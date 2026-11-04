package conv

import (
	"bytes"
	"fmt"
	"go/token"
	"go/types"
	"slices"
	"strings"

	"github.com/dave/dst"
	"github.com/dave/dst/decorator"
	"github.com/dave/dst/decorator/resolver/guess"
)

// ctorKind classifies a statement or an initialization in a constructor.
type ctorKind int

const (
	ctorRegistration ctorKind = iota // kingpin registration, removed
	ctorStructure                    // declares or returns the command struct
	ctorInjection                    // stores a value every command receives, moved to a hook
	ctorCell                         // new(T) made only for kingpin to write into
	ctorEmbed                        // builds an embedded command (alias wrappers)
	ctorCrossRef                     // keeps a pointer to another command
	ctorResidue                      // anything else, which must be preserved
)

// ctorItem is one piece of a constructor body.
type ctorItem struct {
	kind  ctorKind
	stmt  dst.Stmt
	field *types.Var // for initializations
	// path are the fields from the command variable to field, as Settings
	// then Retries in d.Settings.Retries.
	path  []*types.Var
	param *types.Var // the parameter stored, for injections and cross-refs
	expr  dst.Expr
	text  string
	// comments are the ones written above an initialization, which go
	// with it into a hook.
	comments []string
}

// ctorInfo is the analysis of one command constructor.
type ctorInfo struct {
	fn     *types.Func
	decl   funcDecl
	inst   *types.Named
	local  types.Object // the variable holding the command struct
	items  []ctorItem
	params map[types.Object]int
}

// analyzeCtors classifies the body of every function that builds a command
// struct.
func (p *Plan) analyzeCtors(t *Tree) map[*types.Func]*ctorInfo {
	out := map[*types.Func]*ctorInfo{}
	for _, n := range t.nodes {
		if n.typ == nil {
			continue
		}
		for _, s := range p.g.in.sites {
			if s.result != n.cl.instance || out[s.callee] != nil {
				continue
			}
			fd, ok := p.g.prog.funcs[s.callee]
			if !ok {
				continue
			}
			out[s.callee] = p.analyzeCtor(s.callee, fd, n.typ)
		}
	}
	return out
}

func (p *Plan) analyzeCtor(fn *types.Func, fd funcDecl, inst *types.Named) *ctorInfo {
	ci := &ctorInfo{fn: fn, decl: fd, inst: inst, params: map[types.Object]int{}}
	pkg := fd.pkg
	idx := 0
	for _, f := range fd.decl.Type.Params.List {
		for _, nm := range f.Names {
			if o := pkg.objectOf(nm); o != nil {
				ci.params[o] = idx
			}
			idx++
		}
	}
	for _, s := range fd.decl.Body.List {
		ci.items = append(ci.items, p.classifyStmt(ci, s)...)
	}
	return ci
}

func (p *Plan) classifyStmt(ci *ctorInfo, s dst.Stmt) []ctorItem {
	pkg := ci.decl.pkg
	touched := p.g.touched[s]
	switch s := s.(type) {
	case *dst.DeclStmt:
		gd, ok := s.Decl.(*dst.GenDecl)
		if ok && gd.Tok == token.VAR && len(gd.Specs) == 1 {
			vs := gd.Specs[0].(*dst.ValueSpec)
			if len(vs.Names) == 1 && p.isInstanceType(pkg.objectOf(vs.Names[0]), ci.inst) {
				ci.local = pkg.objectOf(vs.Names[0])
				items := []ctorItem{{kind: ctorStructure, stmt: s}}
				if len(vs.Values) == 1 {
					items = append(items, p.literalInits(ci, vs.Values[0])...)
				}
				return items
			}
		}
	case *dst.AssignStmt:
		if s.Tok == token.DEFINE && len(s.Lhs) == 1 && len(s.Rhs) == 1 {
			if p.isInstanceType(pkg.objectOf(s.Lhs[0]), ci.inst) {
				ci.local = pkg.objectOf(s.Lhs[0])
				items := []ctorItem{{kind: ctorStructure, stmt: s}}
				return append(items, p.literalInits(ci, s.Rhs[0])...)
			}
		}
		if s.Tok == token.ASSIGN && len(s.Lhs) == 1 && len(s.Rhs) == 1 {
			if f, path := p.instanceField(ci, s.Lhs[0]); f != nil {
				// Storing the command's clause, as in c.cmd = cmd, goes
				// with the registrations.
				if p.g.prog.isKingpinType(pkg.typeOf(s.Rhs[0])) {
					return []ctorItem{{kind: ctorRegistration, stmt: s}}
				}
				item := p.classifyInit(ci, f, s.Rhs[0])
				item.stmt, item.path = s, path
				item.comments = hookComments(s.Decs.Start)
				if len(path) > 1 && item.kind != ctorResidue && item.kind != ctorCrossRef {
					// Only plain values are moved to a nested field.
					item.kind = ctorResidue
				}
				return []ctorItem{item}
			}
		}
	case *dst.ExprStmt:
		if touched {
			return []ctorItem{{kind: ctorRegistration, stmt: s}}
		}
	case *dst.ReturnStmt:
		if len(s.Results) == 1 {
			r := s.Results[0]
			if u, ok := r.(*dst.UnaryExpr); ok && u.Op == token.AND {
				r = u.X
			}
			if ci.local != nil && pkg.objectOf(r) == ci.local {
				return []ctorItem{{kind: ctorStructure, stmt: s}}
			}
		}
	}
	if touched {
		return []ctorItem{{kind: ctorResidue, stmt: s, text: "statement mixes kingpin registration with other work: " + render(s)}}
	}
	return []ctorItem{{kind: ctorResidue, stmt: s, text: render(s)}}
}

func (p *Plan) isInstanceType(o types.Object, inst *types.Named) bool {
	if o == nil {
		return false
	}
	t := deref(o.Type())
	return types.Identical(t, inst)
}

// instanceField returns the field e writes when e selects a field, possibly
// promoted, of the constructor's command variable, and the fields leading to
// it from that variable.
func (p *Plan) instanceField(ci *ctorInfo, e dst.Expr) (*types.Var, []*types.Var) {
	var path []*types.Var
	root := e
	for {
		s, ok := root.(*dst.SelectorExpr)
		if !ok {
			break
		}
		f, _ := ci.decl.pkg.objectOf(s).(*types.Var)
		if f == nil || !f.IsField() {
			return nil, nil
		}
		path = append([]*types.Var{f}, path...)
		root = s.X
	}
	if len(path) == 0 || ci.local == nil || ci.decl.pkg.objectOf(root) != ci.local {
		return nil, nil
	}
	return path[len(path)-1], path
}

// literalInits classifies the fields set by the composite literal that
// creates the command struct.
func (p *Plan) literalInits(ci *ctorInfo, e dst.Expr) []ctorItem {
	if u, ok := e.(*dst.UnaryExpr); ok && u.Op == token.AND {
		e = u.X
	}
	cl, ok := e.(*dst.CompositeLit)
	if !ok {
		return nil
	}
	return p.compositeInits(ci, cl, ci.decl.pkg.typeOf(cl))
}

func (p *Plan) compositeInits(ci *ctorInfo, cl *dst.CompositeLit, t types.Type) []ctorItem {
	st := structOf(deref(t))
	if st == nil {
		return nil
	}
	var items []ctorItem
	for i, el := range cl.Elts {
		var f *types.Var
		val := el
		if kv, ok := el.(*dst.KeyValueExpr); ok {
			if id, ok := kv.Key.(*dst.Ident); ok {
				for j := 0; j < st.NumFields(); j++ {
					if st.Field(j).Name() == id.Name {
						f = st.Field(j)
					}
				}
			}
			val = kv.Value
		} else if i < st.NumFields() {
			f = st.Field(i)
		}
		if f == nil {
			continue
		}
		// Fields of an embedded struct are promoted, so they are handled
		// one by one; a named struct field is handled as a whole.
		if inner, ok := val.(*dst.CompositeLit); ok && structOf(f.Type()) != nil && f.Anonymous() {
			items = append(items, p.compositeInits(ci, inner, f.Type())...)
			continue
		}
		item := p.classifyInit(ci, f, val)
		item.comments = hookComments(el.Decorations().Start)
		items = append(items, item)
	}
	return items
}

// classifyInit decides what an initialization of field f with e means.
func (p *Plan) classifyInit(ci *ctorInfo, f *types.Var, e dst.Expr) ctorItem {
	pkg := ci.decl.pkg
	item := ctorItem{field: f, expr: e, text: f.Name() + " = " + render(e)}
	if v, ok := pkg.objectOf(e).(*types.Var); ok {
		if _, isParam := ci.params[v]; isParam {
			item.param = v
			if p.isCommandType(v.Type()) {
				item.kind = ctorCrossRef
			} else {
				item.kind = ctorInjection
			}
			return item
		}
	}
	if c, ok := e.(*dst.CallExpr); ok {
		if id, ok := c.Fun.(*dst.Ident); ok && id.Name == "new" && id.Path == "" && p.isCellField(f) {
			item.kind = ctorCell
			return item
		}
		if fn, ok := pkg.objectOf(c.Fun).(*types.Func); ok && p.ctorReturnsCommand(fn) && f.Anonymous() {
			item.kind = ctorEmbed
			return item
		}
	}
	item.kind = ctorResidue
	return item
}

func (p *Plan) isCommandType(t types.Type) bool {
	n, ok := deref(t).(*types.Named)
	if !ok {
		return false
	}
	for _, c := range p.g.in.clauses {
		if c.instance != nil && types.Identical(c.instance.typ, n) {
			return true
		}
	}
	return false
}

func (p *Plan) ctorReturnsCommand(fn *types.Func) bool {
	for _, s := range p.g.in.sites {
		if s.callee == fn {
			return true
		}
	}
	return false
}

// render prints a node as Go source on one line, for reports.
func render(n dst.Node) string {
	return compact(renderExact(n))
}

// renderExact prints a node as Go source, keeping literals intact, for code
// the conversion generates.
func renderExact(n dst.Node) string {
	var buf bytes.Buffer
	f := &dst.File{Name: dst.NewIdent("p")}
	switch n := n.(type) {
	case dst.Stmt:
		f.Decls = []dst.Decl{&dst.FuncDecl{Name: dst.NewIdent("f"), Type: &dst.FuncType{}, Body: &dst.BlockStmt{List: []dst.Stmt{dst.Clone(n).(dst.Stmt)}}}}
	case dst.Expr:
		f.Decls = []dst.Decl{&dst.GenDecl{Tok: token.VAR, Specs: []dst.Spec{&dst.ValueSpec{Names: []*dst.Ident{dst.NewIdent("_")}, Values: []dst.Expr{dst.Clone(n).(dst.Expr)}}}}}
	default:
		return fmt.Sprintf("%T", n)
	}
	r := decorator.NewRestorerWithImports("p", guess.New())
	if err := r.Fprint(&buf, f); err != nil {
		return fmt.Sprintf("%T", n)
	}
	s := buf.String()
	if _, isExpr := n.(dst.Expr); isExpr {
		if i := strings.Index(s, "var _ = "); i >= 0 {
			s = s[i+len("var _ = "):]
		}
		return strings.TrimSpace(s)
	}
	if i := strings.Index(s, "func f() {"); i >= 0 {
		s = s[i+len("func f() {"):]
		s = strings.TrimSuffix(strings.TrimSpace(s), "}")
	}
	return strings.TrimSpace(s)
}

// compact squeezes rendered source onto one line, for reports.
// It can change literals, so never use it for code.
func compact(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// isCellField reports whether f holds a pointer made with new() for a flag to
// write into, which the generated hook replaces.
func (p *Plan) isCellField(f *types.Var) bool {
	for _, b := range p.bindings {
		if b.cell && b.field == f {
			return true
		}
	}
	return false
}

// hookComments are the comments of constructor code that go with it into a
// kong hook.
// One about kingpin, as "Kingpin requires the following to be
// initialized", no longer holds there.
func hookComments(decs []string) []string {
	comments := trimNewlines(decs)
	if slices.ContainsFunc(comments, func(l string) bool { return strings.Contains(strings.ToLower(l), "kingpin") }) {
		return nil
	}
	return comments
}
