package kongcompat

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/alecthomas/kong"
)

// TestParseMatchesKingpin runs the same command lines through kingpin and
// kongcompat.Parse.
// The output, the exit status and the returned command or error must match.
func TestParseMatchesKingpin(t *testing.T) {
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
		{"--completion-bash"},
		{"store", "--completion-bash"},
		{"store", "create", "--", "--completion-bash"},
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
	for _, args := range cases {
		var st kpState
		app := kingpinApp(&st)
		var kout bytes.Buffer
		app.Writers(&kout, io.Discard)
		kexit := -1
		app.Terminate(func(code int) { kexit = code; panic(exited{}) })
		// kingpin writes completion to os.Stdout directly.
		kcmd, kerr := captureStdout(t, func() (string, error) {
			defer recoverExit()
			return app.Parse(args)
		})
		kingpinOut := strings.TrimSpace(kout.String()) + kcmd.stdout

		var cli kongCLI
		var cout bytes.Buffer
		cexit := -1
		k, err := kong.New(&cli, kong.Name("tool"), kong.Description("A tool to manage stores"), kong.Vars{"version": "1.0"},
			kong.Writers(&cout, io.Discard), kong.Exit(func(code int) { cexit = code; panic(exited{}) }))
		if err != nil {
			t.Fatal(err)
		}
		cparsed, cerr := captureStdout(t, func() (string, error) {
			defer recoverExit()
			return Parse(k, args, nil)
		})
		ccmd := cparsed.cmd
		kongOut := strings.TrimSpace(cout.String()) + cparsed.stdout

		if normalize(kongOut) != normalize(kingpinOut) {
			t.Errorf("%q: output differs\n--- kongcompat\n%s\n--- kingpin\n%s", args, cout.String(), kingpinOut)
		}
		if cexit != kexit {
			t.Errorf("%q: exit %d, kingpin %d", args, cexit, kexit)
		}
		if kexit == -1 && (errString(cerr) != errString(kerr) || cerr == nil && ccmd != kcmd.cmd) {
			t.Errorf("%q: got (%q, %v), kingpin (%q, %v)", args, ccmd, cerr, kcmd.cmd, kerr)
		}
	}
}

// exited stops a parse where a real program would have exited.
type exited struct{}

func recoverExit() {
	if r := recover(); r != nil {
		if _, ok := r.(exited); !ok {
			panic(r)
		}
	}
}

type parsed struct {
	cmd    string
	stdout string
}

func captureStdout(t *testing.T, f func() (string, error)) (parsed, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	cmd, ferr := func() (string, error) {
		defer func() { os.Stdout = old }()
		return f()
	}()
	w.Close()
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	return parsed{cmd, strings.TrimSpace(buf.String())}, ferr
}

// decodes counts calls to countingValue.Decode.
// It is global because decoding a value may have effects outside it.
var decodes int

type countingValue struct{ value string }

func (c *countingValue) Decode(ctx *kong.DecodeContext) error {
	decodes++
	return ctx.Scan.PopValueInto("value", &c.value)
}

// TestParseLeavesValuesToKong checks that a value is decoded once, as kingpin
// set it once.
// It also checks that existingfile and existingdir values are kept: kong skips
// them when an earlier pass marked them as set.
func TestParseLeavesValuesToKong(t *testing.T) {
	var cli struct {
		File  string        `arg:"" type:"existingfile"`
		Dir   string        `type:"existingdir"`
		Token countingValue `help:"Token."`
	}
	k, err := kong.New(&cli, kong.Exit(func(int) {}))
	if err != nil {
		t.Fatal(err)
	}
	decodes = 0
	if _, err := Parse(k, []string{"parse_test.go", "--dir", ".", "--token", "abc"}, nil); err != nil {
		t.Fatal(err)
	}
	if cli.File == "" || cli.Dir == "" {
		t.Errorf("file %q, dir %q: values lost", cli.File, cli.Dir)
	}
	if decodes != 1 || cli.Token.value != "abc" {
		t.Errorf("decoded %d times, value %q", decodes, cli.Token.value)
	}
}

type hostSet map[string]bool

func (h *hostSet) Decode(ctx *kong.DecodeContext) error {
	var s string
	if err := ctx.Scan.PopValueInto("host", &s); err != nil {
		return err
	}
	if *h == nil {
		*h = hostSet{}
	}
	(*h)[s] = true
	return nil
}

// TestCustomMapValue checks that only StringMap values get kingpin's
// KEY=VALUE syntax and an empty map when left out: a custom map value is
// decoded by its own code, and left nil.
func TestCustomMapValue(t *testing.T) {
	for _, args := range [][]string{nil, {"--host", "example.com"}} {
		var cli struct {
			Host hostSet `help:"Allowed host."`
		}
		k, err := kong.New(&cli, kong.Exit(func(int) {}), Values())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Parse(k, args, nil); err != nil {
			t.Fatalf("%q: %v", args, err)
		}
		if (cli.Host == nil) != (args == nil) || args != nil && !cli.Host["example.com"] {
			t.Errorf("%q: hosts = %#v", args, cli.Host)
		}
	}
}
