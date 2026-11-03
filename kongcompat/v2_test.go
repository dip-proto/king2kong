package kongcompat

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	kingpinv2 "github.com/alecthomas/kingpin/v2"
	"github.com/alecthomas/kong"
	kingpinv22 "gopkg.in/alecthomas/kingpin.v2"
)

// The tests in this file check the kingpin v2 flavors against kingpin v2.4.0
// and v2.2.6, with the command line of compat_test.go.

// kingpinV2Release is a kingpin v2 release and the flavor that matches it.
type kingpinV2Release struct {
	name      string
	legacy    bool
	templates map[string]string
	// app builds the application with w as its writer, calling exit where
	// kingpin would exit.
	app func(st *kpState, w io.Writer, exit func(int)) v2App
}

// v2App is what the tests use of an application.
// Each release has its own Application type, hence the closures.
type v2App struct {
	parse        func(args []string) (string, error)
	parseContext func(args []string) error
	usage        func(template string, args []string)
}

var kingpinV2Releases = []kingpinV2Release{
	{
		name: "v2.4.0",
		templates: map[string]string{
			"default":  kingpinv2.DefaultUsageTemplate,
			"compact":  kingpinv2.CompactUsageTemplate,
			"separate": kingpinv2.SeparateOptionalFlagsUsageTemplate,
			"long":     kingpinv2.LongHelpTemplate,
			"man":      kingpinv2.ManPageTemplate,
		},
		app: kingpinV24App,
	},
	{
		name:   "v2.2.6",
		legacy: true,
		templates: map[string]string{
			"default":  kingpinv22.DefaultUsageTemplate,
			"compact":  kingpinv22.CompactUsageTemplate,
			"separate": kingpinv22.SeparateOptionalFlagsUsageTemplate,
			"long":     kingpinv22.LongHelpTemplate,
			"man":      kingpinv22.ManPageTemplate,
		},
		app: kingpinV22App,
	},
}

// forEachV2 runs f once per release, with the matching flavor selected.
func forEachV2(t *testing.T, f func(*testing.T, kingpinV2Release)) {
	for _, r := range kingpinV2Releases {
		t.Run(r.name, func(t *testing.T) {
			version, legacy = 2, r.legacy
			t.Cleanup(func() { version, legacy = 3, false })
			f(t, r)
		})
	}
}

func kingpinV24App(st *kpState, w io.Writer, exit func(int)) v2App {
	app := kingpinv2.New("tool", "A tool to manage stores")
	app.Version("1.0")
	app.Terminate(exit)
	app.Writer(w)
	app.Flag("verbose", "Verbose logging").Short('v').BoolVar(&st.verbose)
	app.Flag("token", "API token").Short('t').Envar("TOOL_TOKEN").StringVar(&st.token)
	app.Flag("debug", "Debug level").Short('d').CounterVar(&st.debug)
	store := app.Command("store", "Manage stores").Alias("stores")
	create := store.Command("create", "Create a store")
	create.Flag("name", "Name of the store").Short('n').Required().StringVar(&st.name)
	create.Flag("location", "Where the store lives").Default("US").EnumVar(&st.location, "US", "EU")
	create.Flag("tag", "A tag, repeatable").StringsVar(&st.tags)
	list := store.Command("list", "List stores")
	list.Flag("limit", "Maximum number of stores").Default("50").IntVar(&st.limit)
	del := store.Command("delete", "Delete a store")
	del.Arg("id", "Store ID").Required().StringVar(&st.id)
	del.Flag("force", "Do not ask").BoolVar(&st.force)
	up := app.Command("upload", "Upload files")
	up.Arg("files", "Files to upload").StringsVar(&st.files)
	app.Command("secret", "Hidden command").Hidden()
	return v2App{
		parse: app.Parse,
		parseContext: func(args []string) error {
			_, err := app.ParseContext(args)
			return err
		},
		usage: func(template string, args []string) {
			app.UsageTemplate(template)
			app.Usage(args)
		},
	}
}

func kingpinV22App(st *kpState, w io.Writer, exit func(int)) v2App {
	app := kingpinv22.New("tool", "A tool to manage stores")
	app.Version("1.0")
	app.Terminate(exit)
	app.Writer(w)
	app.Flag("verbose", "Verbose logging").Short('v').BoolVar(&st.verbose)
	app.Flag("token", "API token").Short('t').Envar("TOOL_TOKEN").StringVar(&st.token)
	app.Flag("debug", "Debug level").Short('d').CounterVar(&st.debug)
	store := app.Command("store", "Manage stores").Alias("stores")
	create := store.Command("create", "Create a store")
	create.Flag("name", "Name of the store").Short('n').Required().StringVar(&st.name)
	create.Flag("location", "Where the store lives").Default("US").EnumVar(&st.location, "US", "EU")
	create.Flag("tag", "A tag, repeatable").StringsVar(&st.tags)
	list := store.Command("list", "List stores")
	list.Flag("limit", "Maximum number of stores").Default("50").IntVar(&st.limit)
	del := store.Command("delete", "Delete a store")
	del.Arg("id", "Store ID").Required().StringVar(&st.id)
	del.Flag("force", "Do not ask").BoolVar(&st.force)
	up := app.Command("upload", "Upload files")
	up.Arg("files", "Files to upload").StringsVar(&st.files)
	app.Command("secret", "Hidden command").Hidden()
	return v2App{
		parse: app.Parse,
		parseContext: func(args []string) error {
			_, err := app.ParseContext(args)
			return err
		},
		usage: func(template string, args []string) {
			app.UsageTemplate(template)
			app.Usage(args)
		},
	}
}

// kongV2CLI is kongCLI as kingpin v2 declares it: bools are negatable and
// the token has an environment variable.
type kongV2CLI struct {
	Version kong.VersionFlag `help:"Show application version."`
	Verbose bool             `short:"v" negatable:"" help:"Verbose logging"`
	Token   string           `short:"t" env:"TOOL_TOKEN" help:"API token"`
	Debug   int              `short:"d" type:"counter" help:"Debug level"`

	Help  HelpCommand `cmd:"" help:"Show help."`
	Store struct {
		Create struct {
			Name     string   `short:"n" required:"" help:"Name of the store"`
			Location string   `default:"US" enum:"US,EU" help:"Where the store lives"`
			Tag      []string `sep:"none" help:"A tag, repeatable"`
		} `cmd:"" help:"Create a store"`
		List struct {
			Limit int `default:"50" help:"Maximum number of stores"`
		} `cmd:"" help:"List stores"`
		Delete struct {
			ID    string `arg:"" required:"" help:"Store ID"`
			Force bool   `negatable:"" help:"Do not ask"`
		} `cmd:"" help:"Delete a store"`
	} `cmd:"" aliases:"stores" help:"Manage stores"`
	Upload struct {
		Files []string `arg:"" optional:"" sep:"none" help:"Files to upload"`
	} `cmd:"" help:"Upload files"`
	Secret struct{} `cmd:"" hidden:"" help:"Hidden command"`
}

func kongV2Parser(t *testing.T, opts ...kong.Option) *kong.Kong {
	t.Helper()
	var cli kongV2CLI
	opts = append([]kong.Option{kong.Name("tool"), kong.Description("A tool to manage stores"), NoHelpShortFlag(), kong.Vars{"version": "1.0"}, kong.Exit(func(int) {})}, opts...)
	k, err := kong.New(&cli, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestCheckMatchesKingpinV2(t *testing.T) {
	t.Setenv("TOOL_TOKEN", "")
	cases := [][]string{
		{"store", "create", "--name", "x"},
		{"store", "create"},
		{"store", "create", "--name"},
		{"store", "create", "--name", "x", "--location", "MARS"},
		{"store", "create", "-n", "x", "--tag", "a", "--tag", "b"},
		{"store", "create", "-nx", "-v"},
		{"stores", "list"},
		{"store", "list", "--limit=10", "extra"},
		{"store", "delete"},
		{"store", "delete", "abc", "def"},
		{"store", "delete", "abc", "--no-force"},
		{"store"},
		{"bogus"},
		{"store", "bogus"},
		{"--bogus"},
		{"-x"},
		{"upload"},
		{"upload", "a", "b", "c"},
		{"store", "create", "--name", "x", "--", "rest"},
		{"--no-verbose", "store", "list"},
		{"store", "list", "-v", "-t", "abc"},
		{"secret"},
		{"help"},
		{"help", "store"},
	}
	forEachV2(t, func(t *testing.T, r kingpinV2Release) {
		k := kongV2Parser(t)
		for _, args := range cases {
			var st kpState
			_, kerr := r.app(&st, io.Discard, nil).parse(args)
			if got, want := errString(Check(k, args)), errString(kerr); got != want {
				t.Errorf("%q: kongcompat says %q, kingpin says %q", args, got, want)
			}
			kerr = r.app(&st, io.Discard, nil).parseContext(args)
			if got, want := errString(CheckSyntax(k, args)), errString(kerr); got != want {
				t.Errorf("%q: kongcompat syntax check says %q, kingpin's ParseContext says %q", args, got, want)
			}
		}
	})
}

func TestUsageMatchesKingpinV2(t *testing.T) {
	t.Setenv("COLUMNS", "80")
	forEachV2(t, func(t *testing.T, r kingpinV2Release) {
		for _, tc := range []struct {
			name, template string
			args           []string
		}{
			{"default app", "default", nil},
			{"default command", "default", []string{"store", "create"}},
			{"default branch", "default", []string{"store"}},
			{"default args", "default", []string{"store", "delete"}},
			{"compact app", "compact", nil},
			{"compact command", "compact", []string{"store", "delete"}},
			{"separate optional", "separate", []string{"store", "create"}},
			{"long help", "long", nil},
			{"man page", "man", nil},
		} {
			t.Run(tc.name, func(t *testing.T) {
				tmpl, ok := r.templates[tc.template]
				if !ok {
					t.Fatalf("no %s template", tc.template)
				}
				var st kpState
				var want bytes.Buffer
				r.app(&st, &want, nil).usage(tmpl, tc.args)

				k := kongV2Parser(t)
				ctx, _ := kong.Trace(k, tc.args)
				var got bytes.Buffer
				if err := Usage(&got, k, ctx, &UsageContext{Template: tmpl}); err != nil {
					t.Fatal(err)
				}
				if got.String() != want.String() {
					t.Errorf("usage differs.\n--- kongcompat\n%s\n--- kingpin\n%s", got.String(), want.String())
				}
			})
		}
	})
}

func TestParseMatchesKingpinV2(t *testing.T) {
	t.Setenv("COLUMNS", "80")
	cases := [][]string{
		{"store", "create", "--name", "x"},
		{"store", "list"},
		{"--help"},
		{"store", "--help"},
		{"store", "create", "--help"},
		{"help"},
		{"help", "store"},
		{"help", "store", "delete"},
		{"--help-long"},
		{"--help-man"},
		{"--completion-bash"},
		{"store", "--completion-bash"},
		{"--completion-script-bash"},
		{"--completion-script-zsh"},
		{"store", "create"},
		{"bogus"},
		{},
		{"store"},
		{"--version"},
		{"store", "--version", "--bogus"},
		{"store", "list", "--limit", "bad", "--help"},
	}
	forEachV2(t, func(t *testing.T, r kingpinV2Release) {
		for _, args := range cases {
			var st kpState
			var kout bytes.Buffer
			kexit := -1
			app := r.app(&st, &kout, func(code int) { kexit = code; panic(exited{}) })
			// kingpin v2 writes completion, long help and man pages to os.Stdout.
			kcmd, kerr := captureStdout(t, func() (string, error) {
				defer recoverExit()
				return app.parse(args)
			})
			kingpinOut := kout.String() + kcmd.stdout

			var cout bytes.Buffer
			cexit := -1
			k := kongV2Parser(t, kong.Writers(&cout, io.Discard), kong.Exit(func(code int) { cexit = code; panic(exited{}) }))
			ccmd, cerr := captureStdout(t, func() (string, error) {
				defer recoverExit()
				return Parse(k, args, nil)
			})
			kongOut := cout.String() + ccmd.stdout

			if strings.TrimSpace(kongOut) != strings.TrimSpace(kingpinOut) {
				t.Errorf("%q: output differs\n--- kongcompat\n%s\n--- kingpin\n%s", args, kongOut, kingpinOut)
			}
			if cexit != kexit {
				t.Errorf("%q: exit %d, kingpin %d", args, cexit, kexit)
			}
			if kexit == -1 && (errString(cerr) != errString(kerr) || cerr == nil && ccmd.cmd != kcmd.cmd) {
				t.Errorf("%q: got (%q, %v), kingpin (%q, %v)", args, ccmd.cmd, cerr, kcmd.cmd, kerr)
			}
		}
	})
}

// TestExpandArgsMatchesKingpinV2 parses @file arguments and compares the
// selected command, the values and the errors.
func TestExpandArgsMatchesKingpinV2(t *testing.T) {
	dir := t.TempDir()
	args := filepath.Join(dir, "args")
	if err := os.WriteFile(args, []byte("store\n# a comment\nlist\n\n--limit=5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	forEachV2(t, func(t *testing.T, r kingpinV2Release) {
		for _, line := range [][]string{
			{"@" + args},
			{"-v", "@" + args},
			{"@" + filepath.Join(dir, "missing")},
			{"@"},
			{"upload", "--", "@" + args},
		} {
			var kst kpState
			kcmd, kerr := r.app(&kst, io.Discard, nil).parse(line)

			var cli kongV2CLI
			k, err := kong.New(&cli, kong.Name("tool"), NoHelpShortFlag(), kong.Vars{"version": "1.0"}, kong.Exit(func(int) {}), kong.Writers(io.Discard, io.Discard))
			if err != nil {
				t.Fatal(err)
			}
			ccmd, cerr := Parse(k, line, nil)
			if errString(cerr) != errString(kerr) || cerr == nil && (ccmd != kcmd || kcmd == "store list" && cli.Store.List.Limit != kst.limit || cli.Verbose != kst.verbose || strings.Join(cli.Upload.Files, " ") != strings.Join(kst.files, " ")) {
				t.Errorf("%q: got (%q, %v, limit %d), kingpin (%q, %v, limit %d)", line, ccmd, cerr, cli.Store.List.Limit, kcmd, kerr, kst.limit)
			}
		}
	})
}
