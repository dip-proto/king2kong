package conv

import (
	"bytes"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
)

// importSpec is one import line, with the comments written next to it.
type importSpec struct {
	name, path string
	doc        string
	comment    string
}

// regroupImports rewrites the imports of rendered to follow the grouping of
// the original file.
// The restorer puts every import in a single block, which would lose the
// blank-line groups people maintain.
//
// Imports that are gone leave their group.
// New ones join the standard library group, or the group sharing the longest
// path prefix with them.
// kong takes the place of the imports isKingpin recognizes.
func regroupImports(original, rendered []byte, module string, isKingpin func(path string) bool) []byte {
	fset := token.NewFileSet()
	of, err := parser.ParseFile(fset, "orig.go", original, parser.ImportsOnly|parser.ParseComments)
	if err != nil {
		return rendered
	}
	groups := importGroups(fset, of, original)
	if len(groups) == 0 {
		return rendered
	}
	rset := token.NewFileSet()
	rf, err := parser.ParseFile(rset, "new.go", rendered, parser.ImportsOnly|parser.ParseComments)
	if err != nil || len(rf.Imports) == 0 {
		return rendered
	}
	final := map[string]importSpec{}
	for _, s := range rf.Imports {
		final[specPath(s)] = importSpec{name: specName(s), path: specPath(s)}
	}

	placed := map[string]bool{}
	var out [][]importSpec
	for _, g := range groups {
		var kept []importSpec
		for _, s := range g {
			if f, ok := final[s.path]; ok {
				s.name = f.name
				kept = append(kept, s)
				placed[s.path] = true
			}
		}
		out = append(out, kept)
	}
	var added []string
	for p := range final {
		if !placed[p] {
			added = append(added, p)
		}
	}
	sort.Strings(added)
	kingpinGroup := -1
	for i, g := range groups {
		for _, s := range g {
			if isKingpin(s.path) {
				kingpinGroup = i
			}
		}
	}
	isLocal := func(path string) bool {
		return module != "" && (path == module || strings.HasPrefix(path, module+"/"))
	}
	for _, p := range added {
		spec := final[p]
		if isLocal(p) {
			// A package of the module itself joins the group of the module's
			// other packages, or a new group at the end.
			placedLocal := false
			for i, g := range groups {
				for _, s := range g {
					if isLocal(s.path) && !placedLocal {
						out[i] = append(out[i], spec)
						placedLocal = true
					}
				}
			}
			if !placedLocal {
				out = append(out, []importSpec{spec})
				groups = append(groups, []importSpec{spec})
			}
			continue
		}
		if p == "github.com/alecthomas/kong" && kingpinGroup >= 0 {
			// kong takes the place kingpin had.
			out[kingpinGroup] = append(out[kingpinGroup], spec)
			continue
		}
		best, bestScore := -1, 1
		for i, g := range groups {
			for _, s := range g {
				score := sharedPrefix(s.path, p)
				if isStd(s.path) && isStd(p) {
					score = 100
				}
				if isStd(s.path) != isStd(p) {
					score = 0
				}
				if score > bestScore {
					best, bestScore = i, score
				}
			}
		}
		if best < 0 {
			if isStd(p) {
				out = append([][]importSpec{{spec}}, out...)
				groups = append([][]importSpec{{spec}}, groups...)
				continue
			}
			// A new third-party import goes in its own group after the
			// standard library.
			pos := 0
			if len(groups) > 0 && allStd(groups[0]) {
				pos = 1
			}
			out = append(out[:pos], append([][]importSpec{{spec}}, out[pos:]...)...)
			groups = append(groups[:pos], append([][]importSpec{{spec}}, groups[pos:]...)...)
			continue
		}
		out[best] = append(out[best], spec)
	}

	var b strings.Builder
	b.WriteString("import (\n")
	first := true
	for _, g := range out {
		if len(g) == 0 {
			continue
		}
		sort.SliceStable(g, func(i, j int) bool { return g[i].path < g[j].path })
		if !first {
			b.WriteString("\n")
		}
		first = false
		for _, s := range g {
			if s.doc != "" {
				b.WriteString(s.doc)
			}
			b.WriteString("\t")
			if s.name != "" {
				b.WriteString(s.name + " ")
			}
			b.WriteString(strconv.Quote(s.path))
			if s.comment != "" {
				b.WriteString(" " + s.comment)
			}
			b.WriteString("\n")
		}
	}
	b.WriteString(")")

	// Replace every import declaration of the rendered file with the
	// regrouped block.
	var start, end token.Pos
	for _, d := range rf.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.IMPORT {
			continue
		}
		if !start.IsValid() {
			start = gd.Pos()
			if gd.Doc != nil {
				start = gd.Doc.Pos()
			}
		}
		end = gd.End()
	}
	if !start.IsValid() {
		return rendered
	}
	s := rset.Position(start).Offset
	e := rset.Position(end).Offset
	var buf bytes.Buffer
	buf.Write(rendered[:s])
	buf.WriteString(b.String())
	buf.Write(rendered[e:])
	out2, err := format.Source(buf.Bytes())
	if err != nil {
		return rendered
	}
	return out2
}

// importGroups splits a file's imports into groups separated by blank lines.
func importGroups(fset *token.FileSet, f *ast.File, src []byte) [][]importSpec {
	var groups [][]importSpec
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.IMPORT {
			continue
		}
		var cur []importSpec
		lastLine := -1
		for _, sp := range gd.Specs {
			is := sp.(*ast.ImportSpec)
			line := fset.Position(is.Pos()).Line
			if is.Doc != nil {
				line = fset.Position(is.Doc.Pos()).Line
			}
			if lastLine >= 0 && line > lastLine+1 && len(cur) > 0 {
				groups = append(groups, cur)
				cur = nil
			}
			spec := importSpec{name: specName(is), path: specPath(is)}
			if is.Doc != nil {
				spec.doc = commentText(fset, is.Doc, src) + "\n"
			}
			if is.Comment != nil {
				spec.comment = commentText(fset, is.Comment, src)
			}
			cur = append(cur, spec)
			lastLine = fset.Position(is.End()).Line
			if is.Comment != nil {
				lastLine = fset.Position(is.Comment.End()).Line
			}
		}
		if len(cur) > 0 {
			groups = append(groups, cur)
		}
	}
	return groups
}

func commentText(fset *token.FileSet, cg *ast.CommentGroup, src []byte) string {
	s := fset.Position(cg.Pos()).Offset
	e := fset.Position(cg.End()).Offset
	return "\t" + strings.TrimSpace(string(src[s:e]))
}

func specName(s *ast.ImportSpec) string {
	if s.Name == nil {
		return ""
	}
	return s.Name.Name
}

func specPath(s *ast.ImportSpec) string {
	p, _ := strconv.Unquote(s.Path.Value)
	return p
}

func isStd(path string) bool {
	first := path
	if i := strings.IndexByte(path, '/'); i >= 0 {
		first = path[:i]
	}
	return !strings.Contains(first, ".")
}

func allStd(g []importSpec) bool {
	for _, s := range g {
		if !isStd(s.path) {
			return false
		}
	}
	return len(g) > 0
}

// sharedPrefix counts the leading path elements two import paths share.
func sharedPrefix(a, b string) int {
	as, bs := strings.Split(a, "/"), strings.Split(b, "/")
	n := 0
	for n < len(as) && n < len(bs) && as[n] == bs[n] {
		n++
	}
	return n
}
