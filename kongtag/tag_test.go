package kongtag

import (
	"reflect"
	"testing"

	"github.com/alecthomas/kong"
)

func TestString(t *testing.T) {
	for _, tc := range []struct {
		tag  *Tag
		want string
	}{
		{new(Tag).Flag("required"), `required:""`},
		{new(Tag).Set("short", "n").Set("help", "Name"), `short:"n" help:"Name"`},
		{new(Tag).Set("help", `a "quoted" value`), `help:"a \"quoted\" value"`},
		{new(Tag).Set("help", "two\nlines"), `help:"two\nlines"`},
	} {
		if got := tc.tag.String(); got != tc.want {
			t.Errorf("String() = %s, want %s", got, tc.want)
		}
	}
}

func TestLiteral(t *testing.T) {
	plain := new(Tag).Set("help", "plain")
	if got := plain.Literal(); got != "`help:\"plain\"`" {
		t.Errorf("Literal() = %s", got)
	}
	tick := new(Tag).Set("help", "use `x`")
	if got := tick.Literal(); got != `"help:\"use `+"`x`"+`\""` {
		t.Errorf("Literal() = %s", got)
	}
}

// TestKongReadsTags checks that kong reads back awkward values exactly, as
// help and as default.
func TestKongReadsTags(t *testing.T) {
	values := []string{
		"plain",
		"with, commas, and spaces",
		`the user's "token"`,
		"use `--hint=x` here",
		"costs $5, see ${not_a_var} and $$",
		"line one\nline two\ttabbed",
		"unicode “acme coffee” café",
		`C:\tmp\`,
	}
	for _, v := range values {
		tag := new(Tag).Set("help", Interpolated(v)).Set("default", Interpolated(v)).Set("sep", "none")
		typ := reflect.StructOf([]reflect.StructField{{
			Name: "Value",
			Type: reflect.TypeOf(""),
			Tag:  reflect.StructTag(tag.String()),
		}})
		cli := reflect.New(typ)
		parser, err := kong.New(cli.Interface())
		if err != nil {
			t.Errorf("%q: kong.New: %v", v, err)
			continue
		}
		var flag *kong.Flag
		for _, f := range parser.Model.Flags {
			if f.Name == "value" {
				flag = f
			}
		}
		if flag == nil {
			t.Errorf("%q: no flag", v)
			continue
		}
		if flag.Help != v {
			t.Errorf("help: kong read %q, want %q", flag.Help, v)
		}
		if _, err := parser.Parse(nil); err != nil {
			t.Errorf("%q: parse: %v", v, err)
			continue
		}
		if got := cli.Elem().Field(0).String(); got != v {
			t.Errorf("default: kong set %q, want %q", got, v)
		}
	}
}
