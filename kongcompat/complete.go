package kongcompat

import (
	"io"
	"strings"
	"text/template"

	"github.com/alecthomas/kong"
)

// CompletionScript writes the bash or zsh script that wires the shell's
// completion to the program's --completion-bash flag.
func CompletionScript(w io.Writer, appName, shell string) error {
	tmpl := pick(v2LegacyBashCompletionTemplate, v2BashCompletionTemplate, v3BashCompletionTemplate)
	if shell == "zsh" {
		tmpl = pick(v2LegacyZshCompletionTemplate, v2ZshCompletionTemplate, v3ZshCompletionTemplate)
	}
	tmpl = withContinuations(tmpl)
	t, err := template.New("completion").Parse(tmpl)
	if err != nil {
		return err
	}
	return t.Execute(w, map[string]any{"App": map[string]string{"Name": appName}})
}

// Complete returns the completion candidates for a partial command line,
// following kingpin's --completion-bash rules.
// It offers flags when the last word starts with "--", the next argument's
// options when one is expected, and otherwise the subcommands.
func Complete(k *kong.Kong, args []string) []string {
	ctx, _ := Trace(k, args)
	node := k.Model.Node
	if ctx != nil {
		node = ctx.Selected()
		if node == nil {
			node = k.Model.Node
		}
	}

	var currArg, prevArg string
	if len(args) > 0 {
		currArg = args[len(args)-1]
	}
	if len(args) > 1 {
		prevArg = args[len(args)-2]
	}

	if strings.HasPrefix(currArg, "--") || strings.HasPrefix(prevArg, "--") {
		var flagName, flagValue string
		if strings.HasPrefix(prevArg, "--") && !strings.HasPrefix(currArg, "--") {
			flagName, flagValue = prevArg[2:], currArg
		} else if strings.HasPrefix(currArg, "--") {
			flagName = currArg[2:]
		}
		options, flagMatched, valueMatched := flagCompletion(commandFlags(node), flagName, flagValue, hintsOf(node))
		if valueMatched {
			return cmdCompletion(node, ctx, args)
		}
		if node != k.Model.Node && !flagMatched {
			top, topMatched, topValueMatched := flagCompletion(k.Model.Flags, flagName, flagValue, hintsOf(k.Model.Node))
			if topValueMatched {
				return cmdCompletion(node, ctx, args)
			}
			if topMatched {
				options = top
			} else {
				options = append(options, top...)
			}
		}
		return options
	}
	return cmdCompletion(node, ctx, args)
}

func flagCompletion(flags []*kong.Flag, name, value string, hints map[string][]string) (options []string, flagMatch, valueMatch bool) {
	for _, f := range flags {
		if f.Name == name {
			opts := enumOptions(f.Value)
			if h, ok := hints[f.Name]; ok && f.Value.Tag.Get("hints") == "" {
				opts = h
			}
			if len(opts) == 0 {
				return opts, true, true
			}
			isPrefix, matched := false, false
			for _, o := range opts {
				if value == o {
					matched = true
				} else if strings.HasPrefix(o, value) {
					isPrefix = true
				}
			}
			return opts, true, !isPrefix && matched
		}
		if !f.Hidden {
			options = append(options, "--"+f.Name)
		}
	}
	return options, false, false
}

// hinter is a command struct that gives completion hints only known at run
// time, by flag or argument name.
type hinter interface {
	Hints() map[string][]string
}

func hintsOf(n *kong.Node) map[string][]string {
	if n == nil || !n.Target.IsValid() {
		return nil
	}
	v := n.Target
	if v.CanAddr() {
		v = v.Addr()
	}
	if h, ok := v.Interface().(hinter); ok {
		return h.Hints()
	}
	return nil
}

func named(cmd *kong.Node, args []string) bool {
	for _, a := range args {
		if a == cmd.Name {
			return true
		}
		for _, alias := range cmd.Aliases {
			if a == alias {
				return true
			}
		}
	}
	return false
}

// enumOptions returns the completions of a value: its hints if the program
// gave some, which kingpin preferred over the enum's options, or else the
// options of a single-valued enum.
// kingpin v2 only offered enum options for flags.
func enumOptions(v *kong.Value) []string {
	if hints := v.Tag.Get("hints"); hints != "" {
		return strings.Split(hints, ",")
	}
	if v.Enum == "" || v.IsSlice() || version == 2 && v.Flag == nil {
		return nil
	}
	var out []string
	for _, o := range v.EnumSlice() {
		if o != "" {
			out = append(out, o)
		}
	}
	return out
}

func cmdCompletion(n *kong.Node, ctx *kong.Context, args []string) []string {
	var options []string
	satisfied := 0
	if ctx != nil {
		for _, p := range ctx.Path {
			if p.Positional != nil {
				satisfied++
			}
			// A default command entered without being named also offers
			// every command at its level.
			if p.Command != nil && p.Command.Parent != nil && !named(p.Command, args) {
				for _, c := range p.Command.Parent.Children {
					if c.Type == kong.CommandNode {
						options = append(options, c.Name)
					}
				}
			}
		}
	}
	if satisfied < len(n.Positional) {
		arg := n.Positional[satisfied]
		if h, ok := hintsOf(n)[arg.Name]; ok && arg.Tag.Get("hints") == "" {
			return append(options, h...)
		}
		return append(options, enumOptions(arg)...)
	}
	for _, c := range n.Children {
		if c.Type == kong.CommandNode && !c.Hidden {
			options = append(options, c.Name)
		}
	}
	return options
}
