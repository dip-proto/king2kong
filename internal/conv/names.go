package conv

import (
	"go/types"
	"strings"
)

// namer picks new exported field names that don't collide with a field or
// method of the struct, or of any struct that embeds it.
type namer struct {
	prog *Program
	// embedders maps a struct type to the named types that embed it, directly
	// or through other embedded types.
	embedders map[*types.Named][]*types.Named
	// taken records names already assigned in a struct by this conversion.
	taken map[*types.Named]map[string]bool
	final map[*types.Var]string
	owner map[*types.Var]*types.Named
}

func newNamer(prog *Program) *namer {
	n := &namer{
		prog:      prog,
		embedders: map[*types.Named][]*types.Named{},
		taken:     map[*types.Named]map[string]bool{},
		final:     map[*types.Var]string{},
		owner:     map[*types.Var]*types.Named{},
	}
	var all []*types.Named
	for _, p := range prog.Packages {
		scope := p.Types.Scope()
		for _, name := range scope.Names() {
			if tn, ok := scope.Lookup(name).(*types.TypeName); ok {
				if named, ok := tn.Type().(*types.Named); ok && structOf(named) != nil {
					all = append(all, named)
				}
			}
		}
	}
	for _, t := range all {
		st := structOf(t)
		for i := 0; i < st.NumFields(); i++ {
			n.owner[st.Field(i)] = t
		}
	}
	for _, outer := range all {
		for _, inner := range embeddedClosure(outer) {
			n.embedders[inner] = append(n.embedders[inner], outer)
		}
	}
	return n
}

// embeddedClosure returns every named struct type t embeds, at any depth.
func embeddedClosure(t *types.Named) []*types.Named {
	var out []*types.Named
	seen := map[*types.Named]bool{}
	var walk func(st *types.Struct, depth int)
	walk = func(st *types.Struct, depth int) {
		if st == nil || depth > 8 {
			return
		}
		for i := 0; i < st.NumFields(); i++ {
			f := st.Field(i)
			if !f.Anonymous() {
				continue
			}
			if n, ok := deref(f.Type()).(*types.Named); ok && !seen[n] {
				seen[n] = true
				out = append(out, n)
				walk(structOf(n), depth+1)
			}
		}
	}
	walk(structOf(t), 0)
	return out
}

// free reports whether name can be added as a member of owner.
func (n *namer) free(owner *types.Named, name string) bool {
	check := append([]*types.Named{owner}, n.embedders[owner]...)
	for _, t := range check {
		if n.taken[t][name] {
			return false
		}
		obj, _, _ := types.LookupFieldOrMethod(types.NewPointer(t), true, t.Obj().Pkg(), name)
		if obj != nil {
			return false
		}
	}
	return true
}

func (n *namer) claim(owner *types.Named, name string) {
	for _, t := range append([]*types.Named{owner}, n.embedders[owner]...) {
		if n.taken[t] == nil {
			n.taken[t] = map[string]bool{}
		}
		n.taken[t][name] = true
	}
}

// exportField picks the exported name of field f declared in owner, or ""
// when every candidate is taken.
func (n *namer) exportField(owner *types.Named, f *types.Var) string {
	if name, ok := n.final[f]; ok {
		return name
	}
	if f.Exported() {
		n.final[f] = f.Name()
		return f.Name()
	}
	for _, cand := range []string{n.prog.exported(f.Name()), n.prog.exported(f.Name()) + "Flag", n.prog.exported(f.Name()) + "Value"} {
		if n.free(owner, cand) {
			n.claim(owner, cand)
			n.final[f] = cand
			return cand
		}
	}
	return ""
}

// newField picks the name of a field added to owner, preferring base.
func (n *namer) newField(owner *types.Named, base string) string {
	for _, cand := range []string{base, base + "Flag", base + "Value", base + "Option"} {
		if n.free(owner, cand) && !caseClash(owner, cand) {
			n.claim(owner, cand)
			return cand
		}
	}
	return ""
}

// caseClash reports whether owner has a field that differs from name only by
// case, like serviceID and ServiceID: legal, but easy to confuse.
func caseClash(owner *types.Named, name string) bool {
	st, ok := owner.Underlying().(*types.Struct)
	if !ok {
		return false
	}
	for i := 0; i < st.NumFields(); i++ {
		if f := st.Field(i).Name(); f != name && strings.EqualFold(f, name) {
			return true
		}
	}
	return false
}

func (n *namer) declaringType(f *types.Var) *types.Named {
	return n.owner[f]
}
