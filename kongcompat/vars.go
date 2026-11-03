package kongcompat

import (
	"maps"
	"strings"

	"github.com/alecthomas/kong"
)

// Vars holds defaults and help texts computed at run time, for flags and
// arguments whose tag names one of them, as in default:"${limit_default}".
// Unlike kong.Vars, a value is used as is, and a $ in it stays a plain $.
// Only a default or help that is exactly ${name} gets replaced.
type Vars map[string]string

// Apply implements kong.Option.
// kong would interpolate its own variables again in every value, so these
// are written straight into the grammar instead, with $ escaped for the one
// interpolation defaults and help still go through.
func (v Vars) Apply(k *kong.Kong) error {
	return kong.PostBuild(func(k *kong.Kong) error {
		v.fill(k.Model.Node)
		return nil
	}).Apply(k)
}

func (v Vars) fill(n *kong.Node) {
	values(n, func(val *kong.Value) {
		if s, ok := v.lookup(val.Default); ok {
			val.Default = s
		}
		if s, ok := v.lookup(val.Help); ok {
			val.Help = s
		}
	})
}

// lookup returns the escaped variable that ref, such as ${name}, refers to.
func (v Vars) lookup(ref string) (string, bool) {
	name, ok := strings.CutPrefix(ref, "${")
	if !ok || !strings.HasSuffix(name, "}") {
		return "", false
	}
	s, ok := v[strings.TrimSuffix(name, "}")]
	return strings.ReplaceAll(s, "$", "$$"), ok
}

// CommandVars applies the Vars returned by every command struct that has a
// Vars method.
func CommandVars() kong.Option {
	return kong.PostBuild(func(k *kong.Kong) error {
		vars := Vars{}
		collectVars(k.Model.Node, vars)
		vars.fill(k.Model.Node)
		return nil
	})
}

func collectVars(root *kong.Node, vars Vars) {
	nodes(root, func(n *kong.Node) {
		if n.Target.IsValid() && n.Target.CanAddr() {
			if v, ok := n.Target.Addr().Interface().(interface{ Vars() Vars }); ok {
				maps.Copy(vars, v.Vars())
			}
		}
	})
}
