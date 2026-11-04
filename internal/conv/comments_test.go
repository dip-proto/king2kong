package conv

import (
	"strings"
	"testing"
)

// TestRegistrationComments checks that comments above flag registrations end up on the flags' fields, and that comments on moved setup code follow it into the hooks.
func TestRegistrationComments(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		file  string
		want  []string
	}{
		{
			name: "commands",
			files: map[string]string{
				"cmd/cmd.go": `package cmd

import (
	"io"

	"github.com/alecthomas/kingpin/v2"
)

type Runner interface{ Name() string }

type Globals struct {
	Token string
	Debug bool
}

type Output struct{ JSON bool }

func (o *Output) register(cmd *kingpin.CmdClause) {
	cmd.Flag("json", "JSON output").BoolVar(&o.JSON)
}

type create struct {
	Output
	cmd *kingpin.CmdClause
	g   *Globals
	out io.Writer
	// name is what to call it.
	name    string
	region  string
	force   bool
	read    int
	write   int
	host    string
	port    int
}

func (c *create) connection(cmd *kingpin.CmdClause) {
	cmd.Flag("host", "").StringVar(&c.host)
	cmd.Flag("port", "").IntVar(&c.port)
}

// newCreate registers the create command.
// Callers must not share the command between parses.
func newCreate(app *kingpin.Application, g *Globals, out io.Writer) *create {
	c := &create{g: g}
	// Diagnostics go there so stdout stays valid JSON.
	c.out = out
	cmd := app.Command("create", "")
	c.cmd = cmd

	// Required.
	cmd.Flag("name", "").Required().StringVar(&c.name)
	cmd.Flag("token", "").StringVar(&g.Token)
	// NOTE: it never changes after creation.
	cmd.Flag("region", "").Required().StringVar(&c.region)

	// Optional.
	cmd.Flag("force", "").BoolVar(&c.force)
	cmd.Flag("debug", "").BoolVar(&g.Debug)
	c.register(cmd)

	// Zero disables the timeout.
	cmd.Flag("read-timeout", "").IntVar(&c.read)

	// Zero disables the timeout.
	cmd.Flag("write-timeout", "").IntVar(&c.write)
	// Keep these in sync with the configuration file.
	c.connection(cmd)
	return c
}

func (c *create) Name() string { return "create" }

type list struct {
	Output
	cmd *kingpin.CmdClause
}

func newList(app *kingpin.Application) *list {
	l := &list{}
	cmd := app.Command("list", "")
	l.cmd = cmd
	// Output.
	l.register(cmd)
	return l
}

func (l *list) Name() string { return "list" }

func Commands(app *kingpin.Application, g *Globals, out io.Writer) []Runner {
	return []Runner{newCreate(app, g, out), newList(app)}
}
`,
				"cmd/cmd_test.go": `package cmd

import (
	"os"
	"testing"

	"github.com/alecthomas/kingpin/v2"
)

func TestCreate(t *testing.T) {
	if newCreate(kingpin.New("t", ""), &Globals{}, os.Stderr).Name() != "create" {
		t.Fail()
	}
}
`,
				"main.go": `package main

import (
	"os"

	"github.com/alecthomas/kingpin/v2"

	"probe/cmd"
)

func main() {
	app := kingpin.New("probe", "")
	runners := cmd.Commands(app, &cmd.Globals{}, os.Stderr)
	name := kingpin.MustParse(app.Parse(os.Args[1:]))
	for _, r := range runners {
		if r.Name() == name {
			return
		}
	}
}
`,
			},
			file: "cmd.go",
			want: []string{
				"\t// Required.\n\n\t// name is what to call it.\n\tNameFlag ",
				// The fields added for flags writing into Globals join the groups of their headings.
				"`name:\"name\" required:\"\"`\n\tToken    *string\n\t// NOTE: it never changes after creation.\n\tRegion ",
				"\n\n\t// Optional.\n\tForce bool  `negatable:\"\"`\n\tDebug *bool `negatable:\"\"`\n\tOutput\n\n",
				"\n\n\t// Zero disables the timeout.\n\tRead ",
				"\n\n\t// Zero disables the timeout.\n\tWrite ",
				"\n\n\t// Keep these in sync with the configuration file.\n\tHost ",
				"type list struct {\n\t// Output.\n\tOutput",
				"type Output struct {\n\tJSON bool",
				"\t// Diagnostics go there so stdout stays valid JSON.\n\tc.out = ",
				"// Its flags now come from the struct's tags.\n// Callers must not share the command between parses.\nfunc newCreate",
			},
		},
		{
			name: "main",
			files: map[string]string{"main.go": `package main

import (
	"fmt"

	"github.com/alecthomas/kingpin/v2"
)

func main() {
	app := kingpin.New("probe", "")

	// Required.
	host := app.Flag("host", "").Required().String()
	// Keep this below the upstream idle timeout.
	timeout := app.Flag("timeout", "").Default("30s").Duration()
	kingpin.MustParse(app.Parse(nil))
	fmt.Println(*host, *timeout)
}
`},
			file: "cli.go",
			want: []string{
				"\t// Required.\n\tHost ",
				"\t// Keep this below the upstream idle timeout.\n\tTimeout ",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := convertProbe(t, tc.files)
			var src string
			for _, files := range []map[string]string{res.Files, res.NewFiles} {
				for path, s := range files {
					if strings.HasSuffix(path, "/"+tc.file) {
						src = s
					}
				}
			}
			for _, want := range tc.want {
				if !strings.Contains(src, want) {
					t.Errorf("converted code lacks %q in:\n%s", want, src)
				}
			}
		})
	}
}
