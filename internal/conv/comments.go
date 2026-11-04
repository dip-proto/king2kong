package conv

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/dave/dst"
)

// keepRegistrationComments moves the comments above removed flag registrations onto the fields that now declare those flags.
// Otherwise notes like "Required." would go away with the constructors.
//
// A comment with an empty line before it is a heading.
// It goes above the fields of every flag registered below it, up to the next empty line.
// Any other comment is a note about the flag right below it, and goes into that field's doc.
func (c *converter) keepRegistrationComments() {
	bindings := map[*clause]*binding{}
	for _, b := range c.plan.bindings {
		bindings[b.cl] = b
	}
	registered := c.registeredBy()
	fields := map[*binding][]fieldLoc{}
	through := map[*dst.Field][]*binding{}
	original := map[*dst.StructType][]string{}
	for _, b := range c.plan.bindings {
		fields[b] = c.fieldsOf(b)
		for _, loc := range fields[b] {
			through[loc.field] = append(through[loc.field], b)
			if _, ok := original[loc.st]; !ok {
				original[loc.st] = structComments(loc.st)
			}
		}
	}
	docs := map[*dst.StructType][]string{}
	for _, loc := range c.ed.typeSpecs {
		if st, ok := loc.spec.Type.(*dst.StructType); ok && loc.gen != nil {
			docs[st] = append(slices.Clone(loc.gen.Decs.Start), loc.spec.Decs.Start...)
		}
	}
	added := map[*dst.Field]bool{}
	for _, loc := range c.shadowFields {
		added[loc.field] = true
	}

	live, liveDecs := c.liveStmts()
	done := map[dst.Stmt]bool{}
	for _, cl := range c.g.in.clauses {
		for _, s := range cl.stack {
			if s == nil || done[s] {
				continue
			}
			done[s] = true
			start := s.Decorations().Start
			if len(start) == 0 || live[s] || liveDecs[&start[0]] {
				continue
			}
			heading := s.Decorations().Before == dst.EmptyLine
			scope := []dst.Stmt{s}
			if heading && c.groups[s] != nil {
				scope = c.groups[s]
			}
			inScope := map[*binding]bool{}
			for _, t := range scope {
				for _, cl := range registered[t] {
					if b := bindings[cl]; b != nil {
						inScope[b] = true
					}
				}
			}
			// A note goes on the innermost field holding only this statement's flags, such as the field embedding a shared options struct.
			// A heading goes on the outermost one in the same package, so it also covers the struct of flags it heads.
			ownField := func(b *binding, outer bool) (fieldLoc, bool) {
				var found fieldLoc
				for _, loc := range fields[b] {
					mine := !slices.ContainsFunc(through[loc.field], func(o *binding) bool { return !inScope[o] })
					switch {
					case found.field == nil && mine:
						found = loc
					case found.field == nil:
					case !mine || loc.pkg != found.pkg:
						return found, true
					default:
						found = loc
					}
					if found.field != nil && !outer {
						return found, true
					}
				}
				return found, found.field != nil
			}
			targetsOf := func(outer bool) []fieldLoc {
				var targets []fieldLoc
				for _, cl := range registered[s] {
					b := bindings[cl]
					if b == nil {
						return nil
					}
					loc, ok := ownField(b, outer)
					if !ok {
						return nil
					}
					if !slices.ContainsFunc(targets, func(t fieldLoc) bool { return t.field == loc.field }) {
						targets = append(targets, loc)
					}
				}
				if len(targets) > 0 && slices.ContainsFunc(targets, func(t fieldLoc) bool { return t.st != targets[0].st }) {
					return nil
				}
				return targets
			}
			comments := slices.Clone(trimNewlines(start))
			targets := targetsOf(false)
			if len(targets) == 1 && !heading {
				comments = c.rebasePaths(comments, s, targets[0])
				addNote(targets[0], comments)
				c.ed.touch(targets[0].pkg, targets[0].file)
				continue
			}
			targets = targetsOf(true)
			if len(targets) == 0 {
				continue
			}
			members := map[*dst.Field]bool{}
			for b := range inScope {
				if loc, ok := ownField(b, true); ok {
					members[loc.field] = true
				}
			}
			comments = c.rebasePaths(comments, s, targets[0])
			addHeading(targets[0], comments, members, added, original[targets[0].st], docs[targets[0].st])
			c.ed.touch(targets[0].pkg, targets[0].file)
		}
	}
}

// rebasePaths fixes relative paths in comments moved to a field in another directory, so ./usage.go can become ../app/usage.go.
// Paths to files that don't exist are left alone.
func (c *converter) rebasePaths(comments []string, s dst.Stmt, to fieldLoc) []string {
	from := c.stmtDirs[s]
	dir := filepath.Dir(to.pkg.filename(to.file))
	if from == "" || from == dir {
		return comments
	}
	for i, l := range comments {
		comments[i] = relPath.ReplaceAllStringFunc(l, func(p string) string {
			target := filepath.Join(from, p)
			if _, err := os.Stat(target); err != nil {
				return p
			}
			rel, err := filepath.Rel(dir, target)
			if err != nil {
				return p
			}
			if !strings.HasPrefix(rel, "..") {
				rel = "./" + rel
			}
			return filepath.ToSlash(rel)
		})
	}
	return comments
}

var relPath = regexp.MustCompile(`\.\.?/[\w./-]*\w`)

// registeredBy returns the clauses each statement registered, directly or through the functions it called.
// A statement that runs more than once, like one in a constructor two commands share, collects the clauses of every run.
func (c *converter) registeredBy() map[dst.Stmt][]*clause {
	if c.registered != nil {
		return c.registered
	}
	c.registered = map[dst.Stmt][]*clause{}
	for _, cl := range c.g.in.clauses {
		seen := map[dst.Stmt]bool{}
		for _, s := range cl.stack {
			if s != nil && !seen[s] {
				seen[s] = true
				c.registered[s] = append(c.registered[s], cl)
			}
		}
	}
	return c.registered
}

// ownComments takes the comments off the statements that registered only b's flag, for a field generated as source.
// A heading comes back with an empty line in front.
func (c *converter) ownComments(b *binding) string {
	var lines []string
	heading := false
	for _, s := range b.cl.stack {
		if cls := c.registeredBy()[s]; s == nil || len(cls) != 1 || cls[0] != b.cl {
			continue
		}
		decs := s.Decorations()
		comments := trimNewlines(decs.Start)
		if len(comments) == 0 {
			continue
		}
		heading = heading || decs.Before == dst.EmptyLine
		lines = append(lines, comments...)
		decs.Start = nil
	}
	if len(lines) == 0 {
		return ""
	}
	out := strings.Join(lines, "\n") + "\n"
	if heading {
		out = "\n" + out
	}
	return out
}

// sections maps each statement that follows an empty line to the statements of its group, up to the next empty line.
// It also records the directory of every statement, so it must run before the conversion edits anything.
func (c *converter) sections() map[dst.Stmt][]dst.Stmt {
	out := map[dst.Stmt][]dst.Stmt{}
	c.stmtDirs = map[dst.Stmt]string{}
	for _, p := range c.prog.Packages {
		for _, f := range p.Syntax {
			dir := filepath.Dir(p.filename(f))
			dst.Inspect(f, func(n dst.Node) bool {
				block, ok := n.(*dst.BlockStmt)
				if !ok {
					return true
				}
				var head dst.Stmt
				for _, s := range block.List {
					c.stmtDirs[s] = dir
					if s.Decorations().Before == dst.EmptyLine {
						head = s
					}
					if head != nil {
						out[head] = append(out[head], s)
					}
				}
				return true
			})
		}
	}
	return out
}

// fieldsOf returns the fields a flag goes through, innermost first: the field declaring it, then the embedded fields leading to it.
func (c *converter) fieldsOf(b *binding) []fieldLoc {
	if b.mode == modeStorage {
		return nil
	}
	if b.mode == modeShadow {
		if loc, ok := c.shadowFields[b]; ok {
			return []fieldLoc{loc}
		}
		return nil
	}
	var out []fieldLoc
	if loc, ok := c.ed.fields[b.field]; ok && len(loc.field.Names) <= 1 {
		out = append(out, loc)
	}
	for i := len(b.path) - 1; i >= 0; i-- {
		if loc, ok := c.ed.fields[b.path[i].f]; ok && len(loc.field.Names) <= 1 {
			out = append(out, loc)
		}
	}
	return out
}

// liveStmts returns the statements still in the program, and the comments still attached to a node.
// A rewritten statement can keep the comments of the one it replaced.
func (c *converter) liveStmts() (map[dst.Stmt]bool, map[*string]bool) {
	stmts := map[dst.Stmt]bool{}
	decs := map[*string]bool{}
	for _, p := range c.prog.Packages {
		for _, f := range p.Syntax {
			for _, d := range f.Decls {
				if c.ed.removed[d] {
					continue
				}
				dst.Inspect(d, func(n dst.Node) bool {
					if n == nil {
						return false
					}
					if s, ok := n.(dst.Stmt); ok {
						stmts[s] = true
					}
					if start := n.Decorations().Start; len(start) > 0 {
						decs[&start[0]] = true
					}
					return true
				})
			}
		}
	}
	return stmts, decs
}

// structComments returns every comment line of a struct type.
func structComments(st *dst.StructType) []string {
	out := slices.Clone(st.Fields.Decs.Opening)
	dst.Inspect(st, func(n dst.Node) bool {
		if n != nil {
			out = append(out, n.Decorations().Start...)
			out = append(out, n.Decorations().End...)
		}
		return true
	})
	return out
}

// addNote adds comments to a field's doc, unless it already says the same thing.
func addNote(loc fieldLoc, comments []string) {
	f := loc.field
	if !slices.ContainsFunc(comments, func(l string) bool { return !slices.Contains(f.Decs.Start, l) }) {
		return
	}
	if len(f.Decs.Start) > 0 {
		f.Decs.Start = append(f.Decs.Start, "//")
	}
	f.Decs.Start = append(f.Decs.Start, comments...)
	onOwnLine(f)
}

// addHeading puts a heading above a field and its doc.
// Fields the tool added for the heading's flags join its group, and one added for another flag ends it.
// A heading the struct already had, as when both the struct and the registrations were split into required and optional parts, isn't repeated.
func addHeading(loc fieldLoc, comments []string, members, added map[*dst.Field]bool, original, doc []string) {
	in := func(l string, lines []string) bool {
		return slices.ContainsFunc(lines, func(m string) bool { return sameComment(l, m) })
	}
	// Skip what the struct's own doc already says.
	for len(comments) > 0 && in(comments[0], doc) {
		comments = comments[1:]
	}
	if len(comments) == 0 {
		return
	}
	has := func(lines []string) bool {
		return !slices.ContainsFunc(comments, func(l string) bool { return !in(l, lines) })
	}
	f := loc.field
	list := loc.st.Fields.List
	// A heading stranded after the last field is the one moving here.
	if last, i := trailing(loc.st); i >= 0 && slices.Equal(trimNewlines(last.Decs.End[i:]), comments) {
		last.Decs.End = last.Decs.End[:i]
	} else if has(original) {
		// The struct already has this heading over its own fields, so only a field the tool added may need it.
		// That field joins the group right before it when that group has the heading, and gets a copy otherwise.
		if !added[f] {
			f = nil
			for i, g := range list {
				if members[g] && added[g] && !has(headingOf(list, i)) {
					f = g
					break
				}
			}
			if f == nil {
				return
			}
		}
		if has(headingOf(list, slices.Index(list, f)-1)) {
			f.Decs.Before = dst.NewLine
			return
		}
	}
	if len(f.Decs.Start) > 0 {
		comments = append(slices.Clone(comments), "\n")
	}
	f.Decs.Start = append(comments, f.Decs.Start...)
	i := slices.Index(list, f)
	if i > 0 {
		f.Decs.Before = dst.EmptyLine
	}
	for _, g := range list[i+1:] {
		if start := g.Decs.Start; members[g] && g.Decs.Before == dst.EmptyLine && startsWith(start, comments) {
			// The same heading further down, over more of these flags, merges into this one.
			g.Decs.Start, g.Decs.Before = trimNewlines(start[len(comments):]), dst.NewLine
			continue
		}
		if !members[g] {
			// The struct's own layout stays, but a field added for another flag is set apart.
			if added[g] {
				g.Decs.Before = dst.EmptyLine
			}
			break
		}
		if added[g] && len(g.Decs.Start) == 0 {
			g.Decs.Before = dst.NewLine
		} else if g.Decs.Before == dst.EmptyLine {
			break
		}
	}
	onOwnLine(f)
}

// sameComment reports whether two comment lines say the same thing, as "// Optional." and "// optional" do.
func sameComment(a, b string) bool {
	norm := func(s string) string {
		return strings.ToLower(strings.TrimRight(strings.TrimSpace(strings.TrimPrefix(s, "//")), ".:"))
	}
	return norm(a) == norm(b)
}

// startsWith reports whether comments start with the lines of heading, compared with sameComment.
func startsWith(comments, heading []string) bool {
	if len(comments) < len(heading) {
		return false
	}
	for i, l := range heading {
		if !sameComment(comments[i], l) {
			return false
		}
	}
	return true
}

// headingOf returns the comments above the group of fields holding list[i], or nil when i is negative.
func headingOf(list []*dst.Field, i int) []string {
	for ; i >= 0; i-- {
		if i == 0 || list[i].Decs.Before == dst.EmptyLine {
			return list[i].Decs.Start
		}
	}
	return nil
}

// onOwnLine keeps a field with comments off the line of the struct's brace, as in struct{ Output }.
func onOwnLine(f *dst.Field) {
	if f.Decs.Before == dst.None {
		f.Decs.Before = dst.NewLine
	}
	f.Decs.After = dst.NewLine
}

// trimNewlines drops the empty lines around comments.
func trimNewlines(decs []string) []string {
	for len(decs) > 0 && strings.TrimSpace(decs[0]) == "" {
		decs = decs[1:]
	}
	for len(decs) > 0 && strings.TrimSpace(decs[len(decs)-1]) == "" {
		decs = decs[:len(decs)-1]
	}
	return decs
}
