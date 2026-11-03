// Package compat checks that the kong tags the tool emits parse command
// lines like the kingpin calls they replace.
// The same command lines must give the same values or fail the same way,
// with the --x=y, --x y and -x y forms, defaults, enums, separators and
// repeats.
package compat

import (
	"reflect"
	"strings"
	"testing"

	kongpkg "github.com/alecthomas/kong"
	"github.com/fastly/kingpin"
)

// result is what parsing a command line gave: the values, or a failure.
type result struct {
	Name     string
	Verbose  bool
	Count    int
	Type     string
	Services []string
	Failed   bool
}

func parseKingpin(args []string) result {
	var r result
	app := kingpin.New("app", "")
	app.Terminate(nil)
	app.Flag("name", "Name").Short('n').StringVar(&r.Name)
	app.Flag("verbose", "Verbose").Short('v').BoolVar(&r.Verbose)
	app.Flag("count", "Count").Default("3").IntVar(&r.Count)
	app.Flag("type", "Type").Default("vcl").EnumVar(&r.Type, "wasm", "vcl")
	app.Flag("services", "Services").StringsVar(&r.Services, kingpin.Separator(","))
	if _, err := app.Parse(args); err != nil {
		r.Failed = true
	}
	return r
}

// kongConfig has the tags the tool emits for the flags of parseKingpin.
type kongConfig struct {
	Name     string   `short:"n" help:"Name"`
	Verbose  bool     `short:"v" help:"Verbose"`
	Count    int      `default:"3" help:"Count"`
	Type     string   `default:"vcl" enum:"wasm,vcl" help:"Type"`
	Services []string `sep:"," help:"Services"`
}

func parseKong(args []string) result {
	var c kongConfig
	parser, err := kongpkg.New(&c, kongpkg.Name("app"), kongpkg.Exit(func(int) {}))
	if err != nil {
		return result{Failed: true}
	}
	if _, err := parser.Parse(args); err != nil {
		return result{Failed: true}
	}
	return result{Name: c.Name, Verbose: c.Verbose, Count: c.Count, Type: c.Type, Services: c.Services}
}

func TestCommandLineParity(t *testing.T) {
	batteries := [][]string{
		{},
		{"--name", "foo"},
		{"--name=foo"},
		{"-n", "foo"},
		{"--verbose"},
		{"-v"},
		{"--count", "5"},
		{"--count=5"},
		{"--type", "wasm"},
		{"--type=wasm"},
		{"--services", "a,b,c"},
		{"--services=a,b,c"},
		{"--services", "a", "--services", "b"},
		{"-n", "x", "-v", "--count=7", "--type=wasm"},
		{"--type", "bogus"},
		{"--count", "notnum"},
		{"--unknown"},
	}
	for _, args := range batteries {
		k := parseKingpin(args)
		g := parseKong(args)
		if k.Failed != g.Failed {
			t.Errorf("args %v: kingpin failed=%v but kong failed=%v", args, k.Failed, g.Failed)
			continue
		}
		if k.Failed {
			continue
		}
		if !reflect.DeepEqual(k, g) {
			t.Errorf("args %v: parsed differently\n  kingpin: %+v\n  kong:    %+v", args, k, g)
		}
	}
}

// selectKingpin and selCLI declare the same commands, to check that kong
// selects the same command as kingpin.
func selectKingpin(args []string) (cmd string, failed bool) {
	app := kingpin.New("app", "")
	app.Terminate(nil)
	serve := app.Command("serve", "Run server")
	var port int
	serve.Flag("port", "Port").Default("8080").IntVar(&port)
	db := app.Command("db", "Database")
	db.Command("migrate", "Migrate")
	get := app.Command("get", "Get a key")
	var key string
	get.Arg("key", "Key").Required().StringVar(&key)
	c, err := app.Parse(args)
	return c, err != nil
}

type serveCmd struct {
	Port int `kong:"name='port',help='Port',default='8080'"`
}
type dbMigrateCmd struct{}
type dbCmd struct {
	Migrate dbMigrateCmd `kong:"cmd,name='migrate',help='Migrate'"`
}
type getCmd struct {
	Key string `kong:"arg,name='key',help='Key'"`
}
type selCLI struct {
	Serve serveCmd `kong:"cmd,name='serve',help='Run server'"`
	DB    dbCmd    `kong:"cmd,name='db',help='Database'"`
	Get   getCmd   `kong:"cmd,name='get',help='Get a key'"`
}

// stripPlaceholders drops the <arg> words from kong's Command(), which the
// command kingpin's Parse returns does not have.
func stripPlaceholders(s string) string {
	fields := strings.Fields(s)
	kept := fields[:0]
	for _, f := range fields {
		if strings.HasPrefix(f, "<") {
			continue
		}
		kept = append(kept, f)
	}
	return strings.Join(kept, " ")
}

func selectKong(args []string) (cmd string, failed bool) {
	var cli selCLI
	p, err := kongpkg.New(&cli, kongpkg.Name("app"), kongpkg.Exit(func(int) {}))
	if err != nil {
		return "", true
	}
	ctx, err := p.Parse(args)
	if err != nil {
		return "", true
	}
	return stripPlaceholders(ctx.Command()), false
}

func TestCommandSelectionParity(t *testing.T) {
	batteries := [][]string{
		{"serve"},
		{"serve", "--port", "9090"},
		{"db", "migrate"},
		{"get", "mykey"},
		{"db"},  // needs a subcommand
		{"get"}, // needs a key
		{"bogus"},
		{},
	}
	for _, args := range batteries {
		kc, kf := selectKingpin(args)
		gc, gf := selectKong(args)
		if kf != gf {
			t.Errorf("args %v: kingpin failed=%v but kong failed=%v", args, kf, gf)
			continue
		}
		if kf {
			continue
		}
		if kc != gc {
			t.Errorf("args %v: kingpin selected %q but kong selected %q", args, kc, gc)
		}
	}
}
