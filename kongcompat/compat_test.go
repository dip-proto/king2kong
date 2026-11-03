package kongcompat

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/alecthomas/kong"
	"github.com/fastly/kingpin"
)

// kingpinApp and kongCLI declare the same command line, so kongcompat can be
// checked against kingpin itself.

type kpState struct {
	verbose  bool
	token    string
	name     string
	location string
	limit    int
	tags     []string
	id       string
	files    []string
	force    bool
	debug    int
}

func kingpinApp(st *kpState) *kingpin.Application {
	app := kingpin.New("tool", "A tool to manage stores")
	app.Version("1.0")
	app.Terminate(nil)
	app.Writers(&bytes.Buffer{}, &bytes.Buffer{})
	app.GetFlag("help").Short('h')
	app.Flag("verbose", "Verbose logging").Short('v').BoolVar(&st.verbose)
	app.Flag("token", "API token").Short('t').StringVar(&st.token)
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
	return app
}

type kongCLI struct {
	Version kong.VersionFlag `help:"Show application version."`
	Verbose bool             `short:"v" help:"Verbose logging"`
	Token   string           `short:"t" help:"API token"`
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
			Force bool   `help:"Do not ask"`
		} `cmd:"" help:"Delete a store"`
	} `cmd:"" aliases:"stores" help:"Manage stores"`
	Upload struct {
		Files []string `arg:"" optional:"" sep:"none" help:"Files to upload"`
	} `cmd:"" help:"Upload files"`
	Secret struct{} `cmd:"" hidden:"" help:"Hidden command"`
}

func kongParser(t *testing.T) *kong.Kong {
	t.Helper()
	var cli kongCLI
	k, err := kong.New(&cli, kong.Name("tool"), kong.Description("A tool to manage stores"), kong.Vars{"version": "1.0"}, kong.Exit(func(int) {}))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// checkCases are the command lines the Check tests compare with every kingpin
// release.
// Each test adds the ones about boolean flags, which the releases spell
// differently.
var checkCases = [][]string{
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
	{"store"},
	{"bogus"},
	{"store", "bogus"},
	{"--bogus"},
	{"-x"},
	{"upload"},
	{"upload", "a", "b", "c"},
	{"store", "create", "--name", "x", "--", "rest"},
	{"store", "list", "-v", "-t", "abc"},
	{"secret"},
	{"help"},
	{"help", "store"},
}

func TestCheckMatchesKingpin(t *testing.T) {
	cases := slices.Concat(checkCases, [][]string{
		{"store", "create", "--name", "x", "--name", "y"},
		{"store", "delete", "abc", "--force"},
		{"store", "delete", "abc", "--force=false"},
		{"upload", "a", "--verbose", "b"},
		{"--verbose", "store", "list"},
		{"store", "create", "--help"},
	})
	k := kongParser(t)
	for _, args := range cases {
		var st kpState
		_, kerr := kingpinApp(&st).Parse(args)
		cerr := Check(k, args)
		got, want := errString(cerr), errString(kerr)
		if got != want {
			t.Errorf("%q: kongcompat says %q, kingpin says %q", args, got, want)
		}
		_, kerr = kingpinApp(&st).ParseContext(args)
		cerr = CheckSyntax(k, args)
		got, want = errString(cerr), errString(kerr)
		if got != want {
			t.Errorf("%q: kongcompat syntax check says %q, kingpin's ParseContext says %q", args, got, want)
		}
	}
}

func errString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

func TestUsageMatchesKingpin(t *testing.T) {
	for _, tc := range []struct {
		name     string
		template string
		args     []string
	}{
		{"default app", kingpin.DefaultUsageTemplate, nil},
		{"default command", kingpin.DefaultUsageTemplate, []string{"store", "create"}},
		{"default branch", kingpin.DefaultUsageTemplate, []string{"store"}},
		{"compact app", kingpin.CompactUsageTemplate, nil},
		{"compact command", kingpin.CompactUsageTemplate, []string{"store", "delete"}},
		{"compact branch", kingpin.CompactUsageTemplate, []string{"store"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var st kpState
			app := kingpinApp(&st)
			var want bytes.Buffer
			app.Writers(&want, &bytes.Buffer{})
			app.UsageTemplate(tc.template)
			// kingpin wraps to the terminal width.
			t.Setenv("COLUMNS", "80")
			app.Usage(tc.args)

			k := kongParser(t)
			ctx, _ := kong.Trace(k, tc.args)
			var got bytes.Buffer
			tmpl := strings.ReplaceAll(tc.template, "kingpin", "kongcompat")
			if err := Usage(&got, k, ctx, &UsageContext{Template: tmpl}); err != nil {
				t.Fatal(err)
			}
			if normalize(got.String()) != normalize(want.String()) {
				t.Errorf("usage differs.\n--- kongcompat\n%s\n--- kingpin\n%s", got.String(), want.String())
			}
		})
	}
}

// normalize drops the flags kingpin always registers for its own shell
// completion, which kong does not have.
func normalize(s string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, "completion-") {
			continue
		}
		out = append(out, strings.TrimRight(l, " "))
	}
	return strings.Join(out, "\n")
}

func TestCompleteMatchesKingpin(t *testing.T) {
	k := kongParser(t)
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{nil, []string{"help", "store", "upload"}},
		{[]string{"store"}, []string{"create", "list", "delete"}},
		{[]string{"store", "create", "--"}, []string{"--name", "--location", "--tag", "--help", "--version", "--verbose", "--token", "--debug"}},
		{[]string{"store", "create", "--location"}, []string{"US", "EU"}},
	} {
		got := Complete(k, tc.args)
		if strings.Join(got, " ") != strings.Join(tc.want, " ") {
			t.Errorf("%q: got %q, want %q", tc.args, got, tc.want)
		}
	}
}
