package conv

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/dave/dst"
)

// keepRegistrationComments moves the comments written above flag
// registrations the conversion removed onto the fields that now declare those
// flags.
// Notes such as "Required." or "NOTE: this defaults to read-only" would
// otherwise go away with the constructors.
//
// A comment set apart by an empty line is a heading for the registrations
// that follow it, up to the next empty line, and goes above the fields of
// those flags.
// Another comment is a note on the flag registered right below it, added to
// the documentation of its field.
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
	sections := c.groups
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
			if heading && sections[s] != nil {
				scope = sections[s]
			}
			inScope := map[*binding]bool{}
			for _, t := range scope {
				for _, cl := range registered[t] {
					if b := bindings[cl]; b != nil {
						inScope[b] = true
					}
				}
			}
			// A note goes to the innermost field holding nothing but flags
			// of its statement, such as the field embedding a shared
			// options struct rather than the options' own fields.
			// A heading goes to the outermost one in the same package, so
			// it covers a struct of flags it heads and its neighbors.
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
			addHeading(targets[0], comments, members, added, original[targets[0].st])
			c.ed.touch(targets[0].pkg, targets[0].file)
		}
	}
}

// rebasePaths rewrites the relative file paths of comments moved from
// statement s to a field in another directory, as ./usage.go becoming
// ../app/usage.go.
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

// registeredBy returns the clauses each statement registered, directly or
// through the calls it made.
// A statement run twice, as by a constructor two commands share, counts its
// clauses twice.
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

// ownComments returns the comments above the statements that registered b's
// flag alone, and takes them off those statements, for a field generated
// from source: a heading starts with an empty line.
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

// sections maps each statement following an empty line to the statements of
// its group, up to the next empty line, before the conversion changes them.
// It also records the directory of every statement.
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

// fieldsOf returns the fields a flag goes through, innermost first: the field
// declaring it, then the embedded fields leading to it.
func (c *converter) fieldsOf(b *binding) []fieldLoc {
	if b.storage {
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

// liveStmts returns the statements left in the program, and the comments
// still attached to some node, which a rewritten statement may have taken
// over.
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

// addNote adds comments to the documentation of a field, unless it already
// says the same.
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

// addHeading puts a heading above a field and its documentation.
// The fields the tool added for members of the heading join its group, and
// one added for another flag ends it.
// A heading the struct had before conversion, as when the struct and the
// registrations were both split into required and optional parts, is not
// repeated.
func addHeading(loc fieldLoc, comments []string, members, added map[*dst.Field]bool, original []string) {
	// A heading left after the last field, with no field under it, is
	// the one moving here.
	f := loc.field
	has := func(lines []string) bool {
		return !slices.ContainsFunc(comments, func(l string) bool {
			return !slices.ContainsFunc(lines, func(m string) bool { return sameComment(l, m) })
		})
	}
	if last, i := trailing(loc.st); i >= 0 && slices.Equal(trimNewlines(last.Decs.End[i:]), comments) {
		last.Decs.End = last.Decs.End[:i]
	} else if has(original) {
		// The struct's own fields are under that heading already.
		// A field added for the same flags joins the group right before it
		// when that group has the heading, and repeats it elsewhere.
		if !added[f] {
			return
		}
		if has(headingBefore(loc.st, f)) {
			f.Decs.Before = dst.NewLine
			return
		}
	}
	if len(f.Decs.Start) > 0 {
		comments = append(slices.Clone(comments), "\n")
	}
	f.Decs.Start = append(comments, f.Decs.Start...)
	list := loc.st.Fields.List
	i := slices.Index(list, f)
	if i > 0 {
		f.Decs.Before = dst.EmptyLine
	}
	for _, g := range list[i+1:] {
		if start := g.Decs.Start; members[g] && g.Decs.Before == dst.EmptyLine && startsWith(start, comments) {
			// The same heading further down, over more of these flags,
			// merges with this one.
			g.Decs.Start, g.Decs.Before = trimNewlines(start[len(comments):]), dst.NewLine
			continue
		}
		if !members[g] {
			// The struct's own layout stays, but a field added
			// for another flag is set apart.
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

// sameComment reports whether two comment lines say the same, as
// "// Optional." and "// Optional".
func sameComment(a, b string) bool {
	norm := func(s string) string {
		return strings.ToLower(strings.TrimRight(strings.TrimSpace(strings.TrimPrefix(s, "//")), ".:"))
	}
	return norm(a) == norm(b)
}

// startsWith reports whether comments start with the lines of heading.
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

// headingBefore returns the comments above the group of fields that ends
// right before f, or holds it.
func headingBefore(st *dst.StructType, f *dst.Field) []string {
	list := st.Fields.List
	for j := slices.Index(list, f) - 1; j >= 0; j-- {
		if j == 0 || list[j].Decs.Before == dst.EmptyLine {
			return list[j].Decs.Start
		}
	}
	return nil
}

// onOwnLine makes sure a field with comments is not on the line of the
// struct's brace, as in struct{ Output }.
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
