package conv

import (
	"go/token"
	"strings"
	"unicode"
	"unicode/utf8"
)

// kongName returns the flag or command name kong derives from a field name.
func kongName(field string) string {
	return strings.ToLower(strings.Join(camelCase(field), "-"))
}

// camelCase splits a Go identifier the way kong does.
// Copied from kong, which took it from https://github.com/fatih/camelcase
// (MIT license).
func camelCase(src string) []string {
	if !utf8.ValidString(src) {
		return []string{src}
	}
	var runes [][]rune
	lastClass := 0
	for _, r := range src {
		var class int
		switch {
		case unicode.IsLower(r):
			class = 1
		case unicode.IsUpper(r):
			class = 2
		case unicode.IsDigit(r):
			class = 3
		default:
			class = 4
		}
		if class == lastClass {
			runes[len(runes)-1] = append(runes[len(runes)-1], r)
		} else {
			runes = append(runes, []rune{r})
		}
		lastClass = class
	}
	for i := 0; i < len(runes)-1; i++ {
		if unicode.IsUpper(runes[i][0]) && unicode.IsLower(runes[i+1][0]) {
			runes[i+1] = append([]rune{runes[i][len(runes[i])-1]}, runes[i+1]...)
			runes[i] = runes[i][:len(runes[i])-1]
		}
	}
	entries := []string{}
	for _, s := range runes {
		if len(s) > 0 {
			entries = append(entries, string(s))
		}
	}
	return entries
}

// commonInitialisms are written in upper case in Go identifiers.
// This is staticcheck's default list; learnInitialisms adds the ones the
// converted program already uses.
var commonInitialisms = []string{
	"ACL", "AMQP", "API", "ASCII", "CPU", "CSS", "DB", "DNS", "EOF", "GID", "GUID",
	"HTML", "HTTP", "HTTPS", "ID", "IP", "JSON", "QPS", "RAM", "RPC", "RTP", "SIP",
	"SLA", "SMTP", "SQL", "SSH", "TCP", "TLS", "TS", "TTL", "UDP", "UI", "UID",
	"URI", "URL", "UTF8", "UUID", "VM", "XML", "XMPP", "XSRF", "XSS",
}

// learnInitialisms adds the initialisms the program already uses in its
// identifiers, such as VCL in VCLSnippet or SSO in useSSO.
func (prog *Program) learnInitialisms() {
	prog.initialisms = map[string]bool{}
	for _, w := range commonInitialisms {
		prog.initialisms[w] = true
	}
	for _, p := range prog.Packages {
		for id := range p.TypesInfo.Defs {
			for _, w := range upperWords(id.Name) {
				prog.initialisms[w] = true
			}
		}
	}
}

// upperWords returns the upper case words of a mixed-case identifier.
// A run of capitals followed by lower case letters ends one letter early, as
// in VCLSnippet, and is skipped when only one lower case letter follows, as
// in VCLs, where the split is ambiguous.
func upperWords(name string) []string {
	if strings.ToUpper(name) == name {
		// SCREAMING_CASE constants say nothing about initialisms.
		return nil
	}
	r := []rune(name)
	var out []string
	for i := 0; i < len(r); {
		if !unicode.IsUpper(r[i]) {
			i++
			continue
		}
		j := i
		for j < len(r) && (unicode.IsUpper(r[j]) || j > i && unicode.IsDigit(r[j])) {
			j++
		}
		word := r[i:j]
		if j < len(r) && unicode.IsLower(r[j]) {
			k := j
			for k < len(r) && unicode.IsLower(r[k]) {
				k++
			}
			if k-j < 2 {
				word = nil
			} else {
				word = word[:len(word)-1]
			}
		}
		if len(word) >= 2 && len(word) <= 5 {
			out = append(out, string(word))
		}
		i = j
	}
	return out
}

// identFromName turns a command or flag name such as "tls-config" into an
// exported Go identifier ("TLSConfig").
// The planner checks the result with kongName and adds an explicit name when
// kong would not derive the same one back.
func (prog *Program) identFromName(name string) string {
	parts := strings.FieldsFunc(name, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	var b strings.Builder
	for _, p := range parts {
		if prog.initialisms[strings.ToUpper(p)] {
			b.WriteString(strings.ToUpper(p))
			continue
		}
		r, size := utf8.DecodeRuneInString(p)
		b.WriteRune(unicode.ToUpper(r))
		b.WriteString(p[size:])
	}
	out := b.String()
	if out == "" || !unicode.IsLetter([]rune(out)[0]) {
		out = "X" + out
	}
	if !token.IsIdentifier(out) {
		return "X"
	}
	return out
}

// exported returns the exported spelling of an identifier, with initialisms
// in upper case: httpMethods becomes HTTPMethods, sslCACert becomes SSLCACert.
func (prog *Program) exported(name string) string {
	words := camelCase(name)
	var b strings.Builder
	for i, w := range words {
		if prog.initialisms[strings.ToUpper(w)] {
			b.WriteString(strings.ToUpper(w))
			continue
		}
		if i == 0 {
			r, size := utf8.DecodeRuneInString(w)
			b.WriteRune(unicode.ToUpper(r))
			b.WriteString(w[size:])
			continue
		}
		b.WriteString(w)
	}
	return b.String()
}
