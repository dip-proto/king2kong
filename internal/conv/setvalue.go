package conv

import (
	"fmt"
	"go/token"
	"go/types"
	"sort"
	"strings"
)

// convertCustomValues adds a Decode method to the types registered with
// SetValue, so kong can read them.
// Decode calls the type's own Set, like kingpin did.
func (c *converter) convertCustomValues() {
	types_ := map[*types.Named]*binding{}
	for _, b := range c.plan.bindings {
		if strings.TrimSuffix(b.cl.bind, "Var") != "SetValue" {
			continue
		}
		var t types.Type
		switch {
		case b.field != nil:
			t = b.field.Type()
		case b.dstVar != nil:
			t = b.dstVar.Type()
		}
		named, ok := deref(t).(*types.Named)
		if !ok {
			c.issue(b.cl.pos, "%s %q: SetValue on a value the tool cannot follow", b.cl.kind, b.cl.name)
			continue
		}
		if _, seen := types_[named]; !seen {
			types_[named] = b
		}
	}
	var list []*types.Named
	for t := range types_ {
		list = append(list, t)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Obj().Name() < list[j].Obj().Name() })
	for _, t := range list {
		c.addSetDecode(t, types_[t])
	}
}

func (c *converter) addSetDecode(t *types.Named, b *binding) {
	if c.hasMethod(t, "Decode") {
		return
	}
	loc, ok := c.ed.typeSpecs[t.Obj()]
	if !ok || !c.prog.owns(t.Obj().Pkg()) {
		c.issue(b.cl.pos, "%s %q: kong cannot decode %s, which is declared elsewhere; register a kong mapper for it", b.cl.kind, b.cl.name, t.Obj().Name())
		return
	}
	if !c.hasMethod(t, "Set") {
		c.issue(b.cl.pos, "%s %q: %s has no Set method", b.cl.kind, b.cl.name, t.Obj().Name())
		return
	}
	recv := c.methodRecv(t, strings.ToLower(t.Obj().Name()[:1]))
	name := recvType(t)
	var body string
	if c.hasMethod(t, "IsBoolFlag") {
		body = fmt.Sprintf(`// Decode implements kong.MapperValue with Set, which kingpin called with
// "true" when the flag was given without a value.
func (%[1]s *%[2]s) Decode(ctx *kong.DecodeContext) error {
	if ctx.Scan.Peek().Type != kong.FlagValueToken {
		return %[1]s.Set("true")
	}
	var value string
	if err := ctx.Scan.PopValueInto("value", &value); err != nil {
		return err
	}
	return %[1]s.Set(value)
}

// IsBool tells kong whether the flag takes a value, as IsBoolFlag told kingpin.
func (%[1]s *%[2]s) IsBool() bool {
	return %[1]s.IsBoolFlag()
}`, recv, name)
	} else {
		body = fmt.Sprintf(`// Decode implements kong.MapperValue with Set, which kingpin called for
// each value.
func (%[1]s *%[2]s) Decode(ctx *kong.DecodeContext) error {
	var value string
	if err := ctx.Scan.PopValueInto("value", &value); err != nil {
		return err
	}
	return %[1]s.Set(value)
}`, recv, name)
	}
	imports := map[string]string{"github.com/alecthomas/kong": "kong"}
	decls, err := snippet(loc.pkg.Name, imports, body)
	if err != nil {
		c.issue(token.Position{}, "%v", err)
		return
	}
	c.ed.insertAfter(loc.pkg, loc.file, c.lastMethodOrType(t, loc.gen), decls)
	c.cmdTypes[t] = true
}
