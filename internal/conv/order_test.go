package conv

import (
	"bytes"
	"strings"
	"testing"

	"github.com/dave/dst"
	"github.com/dave/dst/decorator"
)

// TestLayoutFields checks the field order and comments of a struct whose
// flags were registered in another order than the fields.
func TestLayoutFields(t *testing.T) {
	cases := []struct {
		name, src, order, want string
		ok                     bool
	}{
		{
			name:  "fields move within their group, under its heading",
			src:   "// Output.\njson bool\nlimit int\nverbose bool",
			order: "limit json verbose",
			want:  "// Output.\nlimit int\njson bool\nverbose bool",
			ok:    true,
		},
		{
			name:  "a field never leaves its commented group",
			src:   "// Required.\nservice string\nversion string\n\n// Optional.\naddress string\nport int",
			order: "version address port service",
			ok:    false,
		},
		{
			name:  "fields moved to either end of a commented group stay out of it",
			src:   "json bool\n\n// Required.\nid string\nname string\n\nverbose bool\n\n// Output.\nformat string",
			order: "verbose id name format json",
			want:  "verbose bool\n\n// Required.\nid string\nname string\n\n// Output.\nformat string\n\njson bool",
			ok:    true,
		},
		{
			name:  "a field never takes its heading into another group",
			src:   "host string\nport int\n\n// Output.\ndebug bool",
			order: "debug host port",
			ok:    false,
		},
		{
			name:  "a field keeps its doc comment",
			src:   "// Timeout is the deadline.\ntimeout int\nhost string\nport int",
			order: "host port timeout",
			want:  "host string\nport int\n// Timeout is the deadline.\ntimeout int",
			ok:    true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := decorator.Parse("package p\n\ntype T struct {\n" + tc.src + "\n}\n")
			if err != nil {
				t.Fatal(err)
			}
			st := f.Decls[0].(*dst.GenDecl).Specs[0].(*dst.TypeSpec).Type.(*dst.StructType)
			key := map[*dst.Field]int{}
			for i, name := range strings.Fields(tc.order) {
				for _, fl := range st.Fields.List {
					if fl.Names[0].Name == name {
						key[fl] = i
					}
				}
			}
			l := layoutFields(st.Fields.List, key)
			if ok := len(l.crossing) == 0; ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if !tc.ok {
				return
			}
			l.apply(st)
			var buf bytes.Buffer
			if err := decorator.Fprint(&buf, f); err != nil {
				t.Fatal(err)
			}
			got := strings.TrimSpace(buf.String())
			got = got[strings.Index(got, "{")+2 : strings.LastIndex(got, "}")]
			var lines []string
			for _, line := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
				lines = append(lines, strings.Join(strings.Fields(line), " "))
			}
			if g := strings.Join(lines, "\n"); g != tc.want {
				t.Errorf("got\n%s\nwant\n%s", g, tc.want)
			}
		})
	}
}
