package conv

import (
	"cmp"
	"fmt"
	"go/token"
	"go/types"
	"slices"
	"strconv"
	"strings"

	"github.com/dave/dst"
)

// reorderFlags reorders the flag fields of each command to match the order
// the flags were registered in.
// kingpin's help listed flags in registration order, kong's follows the
// fields.
//
// Only flag fields move, within the command struct and the structs it
// embeds; a field embedding several flags counts as its first one.
// Other fields and the blank lines between groups stay put.
// A struct shared by commands that registered its flags in different orders
// is left alone.
func (c *converter) reorderFlags() {
	byCmd := c.bindingsByCommand()
	wanted := map[*dst.StructType]fieldLayout{}
	wantedBy := map[*dst.StructType][]*node{}
	var structs []*dst.StructType
	// The commands whose fields cannot follow the registration order get a
	// FlagOrder method instead, which kongcompat's help follows.
	var unordered []*node
	conflict := map[*dst.StructType]bool{}
	locs := map[*dst.StructType]fieldLoc{}
	for _, n := range c.tree.nodes {
		if t := c.flagsType(n); t == nil || !c.prog.owns(t.Obj().Pkg()) {
			continue
		}
		order := map[*clause]int{}
		for i, ch := range n.cl.children {
			order[ch] = i
		}
		keys := map[*dst.StructType]map[*dst.Field]int{}
		var mine []*dst.StructType
		last := map[*dst.Field]int{}
		holdsArg := map[*dst.Field]bool{}
		for _, b := range byCmd[n.cl] {
			for _, loc := range c.flagFields(b, n) {
				if b.cl.kind != kindFlag {
					holdsArg[loc.field] = true
					continue
				}
				if keys[loc.st] == nil {
					keys[loc.st] = map[*dst.Field]int{}
					mine = append(mine, loc.st)
				}
				if _, known := locs[loc.st]; !known {
					locs[loc.st] = loc
					structs = append(structs, loc.st)
				}
				if k, seen := keys[loc.st][loc.field]; !seen || order[b.cl] < k {
					keys[loc.st][loc.field] = order[b.cl]
				}
				last[loc.field] = max(last[loc.field], order[b.cl])
			}
		}
		if interleaved(keys, last) || n.parent == nil && c.splitRootFlags() {
			unordered = append(unordered, n)
		}
		for _, st := range mine {
			key := keys[st]
			plan := layoutFields(st.Fields.List, key)
			movable := true
			for f := range key {
				movable = movable && !holdsArg[f]
			}
			prev, seen := wanted[st]
			if !movable || conflict[st] || seen && !slices.Equal(prev.list, plan.list) {
				// The struct keeps its layout, so every command it does not
				// suit gets a FlagOrder.
				if seen && len(prev.moves) > 0 {
					unordered = append(unordered, wantedBy[st]...)
				}
				if len(plan.moves) > 0 {
					unordered = append(unordered, n)
				}
				conflict[st] = true
				delete(wanted, st)
				continue
			}
			wanted[st] = plan
			wantedBy[st] = append(wantedBy[st], n)
		}
	}
	for _, st := range structs {
		plan, ok := wanted[st]
		if !ok || len(plan.moves) == 0 {
			continue
		}
		owner := c.structOwner(locs[st])
		if owner == nil || !plan.ok || c.unkeyedLiterals(owner) || c.convertedStruct(owner) {
			unordered = append(unordered, wantedBy[st]...)
			continue
		}
		plan.apply(st)
		c.ed.touch(locs[st].pkg, locs[st].file)
	}
	c.addFlagOrders(unordered)
}

// flagsType is the type holding n's own flags: the command struct, or the
// struct of global flags for the application.
func (c *converter) flagsType(n *node) *types.Named {
	if n.parent == nil {
		return c.rootFlags
	}
	return n.typ
}

// splitRootFlags reports whether the global flags held by rootFlags were
// registered around flags that main holds in the CLI itself.
// The CLI embeds rootFlags in one place, so their order cannot be kept.
func (c *converter) splitRootFlags() bool {
	held := map[*clause]bool{}
	for _, b := range c.appFlagBindings() {
		held[b.cl] = b.holder != nil
	}
	first, last, count := -1, -1, 0
	for i, f := range c.tree.Root.cl.flags() {
		if !held[f] {
			continue
		}
		if first < 0 {
			first = i
		}
		last = i
		count++
	}
	return count > 0 && last-first+1 != count
}

// addFlagOrders gives the commands a FlagOrder method listing their flags in
// registration order, unless a struct they embed already gives the same.
// A command embedding one of those types gets its own FlagOrder too, when
// the one it would inherit lists other flags.
func (c *converter) addFlagOrders(nodes []*node) {
	orders := map[*types.Named][]string{}
	var typs []*types.Named
	flagNames := func(n *node) []string {
		var names []string
		for _, ch := range n.cl.flags() {
			names = append(names, ch.name)
		}
		return names
	}
	add := func(n *node) {
		t, names := c.flagsType(n), flagNames(n)
		prev, seen := orders[t]
		switch {
		case !seen:
			typs = append(typs, t)
			orders[t] = names
		case !slices.Equal(prev, names):
			c.issue(n.cl.pos, "%s is shared by commands that register its flags in different orders; its help follows the first one", t.Obj().Name())
		}
	}
	for _, n := range nodes {
		add(n)
	}
	// A command with no flags has an empty order, which still counts, so
	// orders are looked up by key.
	ordered := func(t *types.Named) bool {
		_, ok := orders[t]
		return ok
	}
	for added := len(typs) > 0; added; {
		added = false
		for _, n := range c.tree.nodes {
			t := c.flagsType(n)
			if t == nil || ordered(t) || !c.prog.owns(t.Obj().Pkg()) {
				continue
			}
			names := flagNames(n)
			if slices.ContainsFunc(embeddedClosure(t), func(e *types.Named) bool {
				return ordered(e) && !slices.Equal(orders[e], names)
			}) {
				add(n)
				added = true
			}
		}
	}
	// Checked before adding any, which would then count.
	taken := c.programUses("FlagOrder")
	for _, t := range typs {
		inherited := slices.ContainsFunc(embeddedClosure(t), func(e *types.Named) bool {
			return slices.Equal(orders[e], orders[t])
		})
		loc, ok := c.ed.typeSpecs[t.Obj()]
		if inherited || !ok || !c.prog.owns(t.Obj().Pkg()) {
			continue
		}
		// The name must be new to the program: a field or method of t, or
		// one a type embedding t also reaches, would clash.
		if obj, _, _ := types.LookupFieldOrMethod(t, true, t.Obj().Pkg(), "FlagOrder"); obj != nil || taken {
			c.issue(loc.pkg.position(loc.spec), "%s cannot get a FlagOrder method, a name the program already uses; its help lists the flags in field order", t.Obj().Name())
			continue
		}
		decls, err := snippet(loc.pkg.Name, map[string]string{}, flagOrderMethod(recvType(t), c.receiverName(t, strings.ToLower(t.Obj().Name()[:1])), orders[t]))
		if err != nil {
			c.issue(token.Position{}, "%v", err)
			continue
		}
		c.ed.insertAfter(loc.pkg, loc.file, c.lastMethodOrType(t, loc.gen), decls)
	}
}

// flagOrderMethod is the source of a FlagOrder method returning names.
func flagOrderMethod(typeName, recv string, names []string) string {
	var lines []string
	line := ""
	for _, name := range names {
		item := strconv.Quote(name) + ","
		if line != "" && len(line)+len(item) > 70 {
			lines = append(lines, line)
			line = ""
		}
		line = strings.TrimSpace(line + " " + item)
	}
	lines = append(lines, line)
	return fmt.Sprintf(`// FlagOrder lists the flags in the order kingpin showed them in help, which
// the fields do not follow.
func (%s *%s) FlagOrder() []string {
	return []string{
%s
	}
}
`, recv, typeName, strings.Join(lines, "\n"))
}

// fieldLayout is a new field order for a struct.
// Each flag field out of registration order moves right after the flag
// registered before it, or right before the one registered after it.
type fieldLayout struct {
	list  []*dst.Field
	moves map[*dst.Field]fieldMove
	// crossing lists the fields that would leave a group under a heading
	// comment, or take a comment of their own to another group.
	// ok is false when there are so many that the headings would end up
	// describing the wrong fields.
	ok       bool
	crossing []*dst.Field
	// headingMoves counts the moved fields that start a commented group.
	headingMoves int
}

// fieldMove is where a field goes: right before next when set, otherwise
// right after the flag registered before it.
// group is the field group it lands in.
type fieldMove struct {
	next  *dst.Field
	group int
}

// layoutFields keeps in place a longest run of flag fields already in
// registration order (key), and moves the others.
// Of the runs it tries, it picks the one with the lowest cost.
func layoutFields(old []*dst.Field, key map[*dst.Field]int) fieldLayout {
	var units []*dst.Field
	for _, f := range old {
		if _, ok := key[f]; ok {
			units = append(units, f)
		}
	}
	var best fieldLayout
	for variant := 0; variant < 4; variant++ {
		l := layoutKeeping(old, key, longestRun(units, key, variant&1 == 1, variant&2 == 2))
		if variant == 0 || l.cost() < best.cost() {
			best = l
		}
	}
	return best
}

// longestRun returns a longest subsequence of units in increasing key
// order.
// laterPrev and laterEnd break ties toward later fields, so layoutFields
// gets different candidates to compare.
func longestRun(units []*dst.Field, key map[*dst.Field]int, laterPrev, laterEnd bool) map[*dst.Field]bool {
	best := make([]int, len(units))
	prev := make([]int, len(units))
	end := -1
	for i := range units {
		best[i], prev[i] = 1, -1
		for j := 0; j < i; j++ {
			if key[units[j]] >= key[units[i]] {
				continue
			}
			if best[j]+1 > best[i] || laterPrev && best[j]+1 == best[i] {
				best[i], prev[i] = best[j]+1, j
			}
		}
		if end < 0 || best[i] > best[end] || laterEnd && best[i] == best[end] {
			end = i
		}
	}
	stays := map[*dst.Field]bool{}
	for i := end; i >= 0; i = prev[i] {
		stays[units[i]] = true
	}
	return stays
}

// layoutKeeping moves the flag fields not in stays.
func layoutKeeping(old []*dst.Field, key map[*dst.Field]int, stays map[*dst.Field]bool) fieldLayout {
	group, headed := fieldGroups(old)
	var list, moving []*dst.Field
	for _, f := range old {
		if _, unit := key[f]; unit && !stays[f] {
			moving = append(moving, f)
			continue
		}
		list = append(list, f)
	}
	slices.SortFunc(moving, func(a, b *dst.Field) int { return cmp.Compare(key[a], key[b]) })
	layout := fieldLayout{moves: map[*dst.Field]fieldMove{}}
	landed := func(f *dst.Field) int {
		if m, moved := layout.moves[f]; moved {
			return m.group
		}
		return group[f]
	}
	for _, f := range moving {
		after, before := -1, -1
		for i, g := range list {
			k, unit := key[g]
			if !unit {
				continue
			}
			if k < key[f] && (after < 0 || k > key[list[after]]) {
				after = i
			}
			if k > key[f] && before < 0 {
				before = i
			}
		}
		// A field joins its neighbor's group, wherever that neighbor ended
		// up.
		at, m := after+1, fieldMove{}
		if after >= 0 {
			m.group = landed(list[after])
		} else {
			at, m.next, m.group = before, list[before], landed(list[before])
		}
		if m.group != group[f] && (headed[group[f]] || len(f.Decs.Start) > 0 && !documents(f)) {
			layout.crossing = append(layout.crossing, f)
		}
		if i := slices.Index(old, f); headed[group[f]] && (i == 0 || group[old[i-1]] != group[f]) {
			layout.headingMoves++
		}
		list = slices.Insert(list, at, f)
		layout.moves[f] = m
	}
	layout.list = list
	// A field leaving its commented group would end up under the wrong
	// comment; FlagOrder keeps the help right instead.
	layout.ok = len(layout.crossing) == 0
	return layout
}

// cost ranks layouts: fewer fields leaving their commented group, then
// fewer group headings to hand over, then fewer moves.
func (l fieldLayout) cost() int {
	return len(l.crossing)*10000 + l.headingMoves*100 + len(l.moves)
}

// fieldGroups numbers the groups of fields separated by empty lines, and
// reports which groups start with a heading comment over several fields.
func fieldGroups(fields []*dst.Field) (map[*dst.Field]int, map[int]bool) {
	group := map[*dst.Field]int{}
	size := map[int]int{}
	g := 0
	for i, f := range fields {
		if i > 0 && (f.Decs.Before == dst.EmptyLine || fields[i-1].Decs.After == dst.EmptyLine) {
			g++
		}
		group[f] = g
		size[g]++
	}
	headed := map[int]bool{}
	for i, f := range fields {
		if (i == 0 || group[f] != group[fields[i-1]]) && len(f.Decs.Start) > 0 && size[group[f]] > 1 && !documents(f) {
			headed[group[f]] = true
		}
	}
	return group, headed
}

// documents reports whether the comment above f is f's own doc comment, which
// starts with the field's name by Go convention ("// Timeout is ...").
func documents(f *dst.Field) bool {
	name := render(f.Type)
	if len(f.Names) > 0 {
		name = f.Names[0].Name
	}
	name = name[strings.LastIndex(name, ".")+1:]
	words := strings.Fields(strings.TrimPrefix(f.Decs.Start[0], "//"))
	return len(words) > 0 && strings.EqualFold(words[0], name)
}

// apply reorders the fields of st and keeps the groups visually apart.
// When the first field of a group moves, its blank line and heading comment
// go to the next field of the group.
// A field moved to the start of a group takes them over, unless the group
// has a heading comment: a field from elsewhere moved to either end of
// such a group stays out of it.
func (l fieldLayout) apply(st *dst.StructType) {
	old := st.Fields.List
	for i, f := range old {
		if f.Decs.After == dst.EmptyLine && i+1 < len(old) {
			f.Decs.After, old[i+1].Decs.Before = dst.NewLine, dst.EmptyLine
		}
	}
	group, headed := fieldGroups(old)
	starts := map[*dst.Field]bool{}
	// A group of one field with a heading counts here too.
	commented := map[int]bool{}
	for i, f := range old {
		starts[f] = i == 0 || group[old[i-1]] != group[f]
		if starts[f] && len(f.Decs.Start) > 0 && !documents(f) {
			commented[group[f]] = true
		}
	}
	for i, f := range old {
		_, moved := l.moves[f]
		if !moved || i > 0 && group[old[i-1]] == group[f] {
			continue
		}
		for _, g := range old[i+1:] {
			if group[g] != group[f] {
				break
			}
			if _, moved := l.moves[g]; !moved {
				g.Decs.Before = f.Decs.Before
				if headed[group[f]] {
					g.Decs.Start, f.Decs.Start = append(append(dst.Decorations{}, f.Decs.Start...), g.Decs.Start...), nil
				}
				break
			}
		}
	}
	landed := func(f *dst.Field) int {
		if m, moved := l.moves[f]; moved {
			return m.group
		}
		return group[f]
	}
	for i, f := range l.list {
		m, moved := l.moves[f]
		if !moved {
			continue
		}
		f.Decs.Before = dst.NewLine
		switch {
		case m.next != nil && starts[m.next] && commented[group[m.next]] && group[f] != group[m.next]:
			// Right before a commented group it does not belong to: it
			// stays out of it.
			f.Decs.Before = m.next.Decs.Before
			m.next.Decs.Before = dst.EmptyLine
		case m.next != nil && starts[m.next]:
			f.Decs.Before, m.next.Decs.Before = m.next.Decs.Before, dst.NewLine
			if headed[group[m.next]] {
				f.Decs.Start, m.next.Decs.Start = append(append(dst.Decorations{}, m.next.Decs.Start...), f.Decs.Start...), nil
			}
		case m.next == nil && i > 0 && commented[landed(l.list[i-1])] && group[f] != landed(l.list[i-1]) &&
			(i+1 == len(l.list) || landed(l.list[i+1]) != landed(l.list[i-1])):
			// Right after the end of a commented group it does not belong
			// to: it gets its own.
			f.Decs.Before = dst.EmptyLine
		}
	}
	if len(l.list) > 0 && l.list[0].Decs.Before == dst.EmptyLine {
		l.list[0].Decs.Before = dst.NewLine
	}
	st.Fields.List = l.list
}

// interleaved reports whether the flags of a field embedding several of
// them were registered around those of another field of the same struct.
func interleaved(keys map[*dst.StructType]map[*dst.Field]int, last map[*dst.Field]int) bool {
	for _, key := range keys {
		for f, first := range key {
			for g, other := range key {
				if f != g && first < other && other < last[f] {
					return true
				}
			}
		}
	}
	return false
}

// flagFields returns the fields on the way from command n to the value of
// b's flag or argument: the command's field embedding a struct, that
// struct's field, and so on down to the value's own field or the field
// added for it.
func (c *converter) flagFields(b *binding, n *node) []fieldLoc {
	switch {
	case b.storage:
		return nil
	case b.mode == modeShadow && b.inst == nil:
		// A global flag has no command struct holding its copy.
		return nil
	case b.mode == modeShadow:
		host, rest := c.host(b)
		if host == nil || b.shadowField == "" {
			return nil
		}
		// The field was added to the command struct the value is reached
		// from, which n's struct may embed.
		var out []fieldLoc
		for _, s := range b.path[:len(b.path)-len(rest)] {
			loc, ok := c.ed.fieldDecl(s.f)
			if !ok {
				return nil
			}
			out = append(out, loc)
		}
		loc, st, ok := c.ed.structType(host)
		if !ok {
			return nil
		}
		for _, f := range st.Fields.List {
			if len(f.Names) == 1 && f.Names[0].Name == b.shadowField {
				return append(out, fieldLoc{pkg: loc.pkg, file: loc.file, st: st, field: f})
			}
		}
		return nil
	}
	var out []fieldLoc
	for _, s := range b.path {
		loc, ok := c.ed.fieldDecl(s.f)
		if !ok {
			return out
		}
		out = append(out, loc)
	}
	if loc, ok := c.ed.fieldDecl(b.field); ok {
		out = append(out, loc)
	}
	return out
}

// structOwner is the named type whose struct holds the field at loc.
func (c *converter) structOwner(loc fieldLoc) *types.Named {
	for _, nm := range loc.field.Names {
		if v, ok := loc.pkg.objectOf(nm).(*types.Var); ok {
			return c.nm.declaringType(v)
		}
	}
	for obj, tl := range c.ed.typeSpecs {
		if st, ok := tl.spec.Type.(*dst.StructType); ok && st == loc.st {
			n, _ := obj.Type().(*types.Named)
			return n
		}
	}
	return nil
}

// hideUnusedEmbeds tags with kong:"-" the embedded structs whose flags a
// command never registered, such as a shared JSONOutput holding --json,
// since kong would otherwise give the command those flags.
// A command registering only some of them is reported.
func (c *converter) hideUnusedEmbeds() {
	byCmd := c.bindingsByCommand()
	containers := map[*types.Named]bool{}
	for _, n := range c.tree.nodes {
		if n.typ != nil && n.containerOwn && len(n.children) > 0 {
			containers[n.typ] = true
		}
	}
	bound := map[*types.Var]map[*clause]map[*types.Var]bool{}
	var order []*types.Var
	for _, n := range c.tree.nodes {
		if n.typ == nil || n.parent == nil || !c.prog.owns(n.typ.Obj().Pkg()) {
			continue
		}
		st := structOf(n.typ)
		if st == nil {
			continue
		}
		for i := 0; i < st.NumFields(); i++ {
			f := st.Field(i)
			if !f.Anonymous() && !c.isEmbedTagged(f) {
				continue
			}
			et, ok := deref(f.Type()).(*types.Named)
			// Hiding a struct holding subcommands would lose them.
			if !ok || containers[et] || len(c.grammarFields(et, map[*types.Named]bool{})) == 0 {
				continue
			}
			if bound[f] == nil {
				bound[f] = map[*clause]map[*types.Var]bool{}
				order = append(order, f)
			}
			set := map[*types.Var]bool{}
			for _, b := range byCmd[n.cl] {
				if len(b.path) > 0 && b.path[0].f == f {
					set[b.field] = true
				}
			}
			bound[f][n.cl] = set
		}
	}
	// Hide first, so the report below leaves out the flags of hidden
	// structs.
	var partial []*types.Var
	for _, f := range order {
		none := true
		for _, set := range bound[f] {
			none = none && len(set) == 0
		}
		if !none {
			partial = append(partial, f)
			continue
		}
		if _, err := c.ed.addTag(f, `kong:"-"`); err != nil {
			c.issue(token.Position{}, "%s: %v", f.Name(), err)
		}
		c.hiddenEmbeds[f] = true
	}
	for _, f := range partial {
		et := deref(f.Type()).(*types.Named)
		all := c.grammarFields(et, map[*types.Named]bool{})
		var cmds []*clause
		for cmd := range bound[f] {
			cmds = append(cmds, cmd)
		}
		slices.SortFunc(cmds, func(a, b *clause) int { return cmp.Compare(a.fullName(), b.fullName()) })
		for _, cmd := range cmds {
			var missing []string
			for _, g := range all {
				if !bound[f][cmd][g] {
					missing = append(missing, g.Name())
				}
			}
			if len(missing) > 0 {
				c.issue(cmd.pos, "command %q does not register all the flags of the %s it embeds; kong also accepts %s there", cmd.fullName(), et.Obj().Name(), strings.Join(missing, ", "))
			}
		}
	}
}

// bindingsByCommand groups the flag and argument bindings by command.
func (c *converter) bindingsByCommand() map[*clause][]*binding {
	out := map[*clause][]*binding{}
	for _, b := range c.plan.bindings {
		out[b.cmd] = append(out[b.cmd], b)
	}
	return out
}

// grammarFields are the tagged flag and argument fields of t, including
// those of the structs it embeds.
func (c *converter) grammarFields(t *types.Named, seen map[*types.Named]bool) []*types.Var {
	if seen[t] {
		return nil
	}
	seen[t] = true
	st := structOf(t)
	if st == nil || c.plan.wrappers[t] != nil {
		return nil
	}
	var out []*types.Var
	for i := 0; i < st.NumFields(); i++ {
		f := st.Field(i)
		if c.hiddenEmbeds[f] {
			continue
		}
		if f.Anonymous() || c.isEmbedTagged(f) {
			if n, ok := deref(f.Type()).(*types.Named); ok {
				out = append(out, c.grammarFields(n, seen)...)
			}
			continue
		}
		if c.tagged[f] {
			out = append(out, f)
		}
	}
	return out
}

// convertedStruct reports whether a value of struct type t is converted to or
// from another struct type somewhere, which reordering t's fields breaks.
func (c *converter) convertedStruct(t *types.Named) bool {
	if c.structConversions == nil {
		c.structConversions = map[*types.Named]bool{}
		for _, p := range c.prog.Packages {
			for _, f := range p.Syntax {
				dst.Inspect(f, func(n dst.Node) bool {
					call, ok := n.(*dst.CallExpr)
					if !ok || len(call.Args) != 1 {
						return true
					}
					if tv, ok := p.tv(call.Fun); !ok || !tv.IsType() {
						return true
					}
					for _, t := range []types.Type{p.typeOf(call.Fun), p.typeOf(call.Args[0])} {
						if named := namedType(t); named != nil && structOf(named) != nil {
							c.structConversions[named] = true
						}
					}
					return true
				})
			}
		}
	}
	return c.structConversions[t.Origin()]
}

// programUses reports whether any identifier of the program is spelled name.
func (c *converter) programUses(name string) bool {
	for _, p := range c.prog.Packages {
		for _, f := range p.Syntax {
			found := false
			dst.Inspect(f, func(n dst.Node) bool {
				if id, ok := n.(*dst.Ident); ok && id.Name == name {
					found = true
				}
				return !found
			})
			if found {
				return true
			}
		}
	}
	return false
}
