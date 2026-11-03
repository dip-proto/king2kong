// Package kongtag renders kong struct tags in kong's usual form, one tag key
// per directive:
//
//	Name string `short:"n" required:"" help:"Name of the store."`
//
// Values are escaped like strconv.Quote does.
// kong expands ${var} in help, defaults, enums, placeholders and env names,
// so a literal "${" has to be written "$${".
package kongtag

import (
	"strconv"
	"strings"
)

// Tag accumulates directives for one struct field, in the order they are
// added.
type Tag struct {
	entries []entry
}

type entry struct {
	key, value string
}

// Flag adds a valueless directive such as required, hidden, arg or cmd.
// Kong spells those with an empty value: required:"".
func (t *Tag) Flag(key string) *Tag {
	t.entries = append(t.entries, entry{key: key})
	return t
}

// Set adds a key with a value.
// Values kong interpolates (help, default, enum, placeholder, env) should
// go through Interpolated first.
func (t *Tag) Set(key, value string) *Tag {
	t.entries = append(t.entries, entry{key: key, value: value})
	return t
}

// Get returns the value of the first directive with key, or "".
func (t *Tag) Get(key string) string {
	for _, e := range t.entries {
		if e.key == key {
			return e.value
		}
	}
	return ""
}

// Empty reports whether no directive was added.
func (t *Tag) Empty() bool { return len(t.entries) == 0 }

// String renders the struct tag content, without the surrounding backticks.
func (t *Tag) String() string {
	parts := make([]string, 0, len(t.entries))
	for _, e := range t.entries {
		q := strconv.Quote(e.value)
		// kong reads a backslash right before a quote as escaping it, so a
		// value ending with an escaped backslash would lose its closing quote.
		// Go unquotes \x5c to the same backslash, and kong leaves it alone.
		if strings.HasSuffix(q, `\\"`) {
			q = strings.TrimSuffix(q, `\\"`) + `\x5c"`
		}
		parts = append(parts, e.key+":"+q)
	}
	return strings.Join(parts, " ")
}

// Literal returns the tag as Go source: a raw string literal when possible,
// or an interpreted one when the content contains a backtick.
func (t *Tag) Literal() string {
	s := t.String()
	if strings.ContainsRune(s, '`') {
		return strconv.Quote(s)
	}
	return "`" + s + "`"
}

// Interpolated escapes a value kong would otherwise treat as containing ${var}
// references or the $$ escape.
func Interpolated(v string) string {
	if strings.Contains(v, "${") || strings.Contains(v, "$$") {
		return strings.ReplaceAll(v, "$", "$$")
	}
	return v
}
