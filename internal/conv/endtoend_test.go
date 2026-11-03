package conv

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestEndToEnd converts the programs in testdata and runs the same command
// lines through the original and the converted build.
// The output, errors and exit status must not change.
func TestEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs programs")
	}
	fixtures := map[string][][]string{
		"chat": {
			{"register", "nick", "Nick Name"},
			{"--debug", "register", "a", "b"},
			{"--server", "10.0.0.1", "register", "a", "b"},
			{"register", "a"},
			{"register"},
			{"post", "general", "hello", "world"},
			{"post", "-c", "3", "--image", "cat.png", "general", "hi"},
			{"post", "--count=2", "general"},
			{"post", "--format", "markdown", "general"},
			{"post", "--format", "bogus", "general"},
			{"post", "--count", "x", "general"},
			{"post"},
			{"bogus"},
			{},
			{"--help"},
			{"post", "--help"},
			{"help"},
			{"help", "post"},
			{"--completion-bash"},
			{"post", "--completion-bash", "--"},
			{"--completion-script-bash"},
			{"register", "a", "b", "c"},
			{"--debug", "--debug", "register", "a", "b"},
			{"--no-cache", "register", "a", "b"},
			{"--no-no-cache", "register", "a", "b"},
		},
		"modular": {
			{"ls"},
			{"ls", "-a", "--sort", "size", "--column", "a", "--column", "b"},
			{"ls", "--no-all"},
			{"--no-all", "ls"},
			{"fetch", "http://x", "-r", "5"},
			{"fetch", "--timeout", "1m", "http://x"},
			{"fetch"},
			{"-vvv", "ls"},
			{"--version"},
			{"ls", "--version", "--bogus"},
			{"--help"},
			{"ls", "--help"},
			{"help", "fetch"},
			{"ls", "--sort", "bogus"},
			{"bogus"},
			{},
			{"--help-long"},
			{"--help-man"},
			{"--completion-bash"},
			{"ls", "--completion-bash", "--"},
		},
		"classic": {
			{},
			{"ping", "1.2.3.4"},
			{"ping", "1.2.3.4", "3"},
			{"1.2.3.4"},
			{"post", "hello", "world"},
			{"send", "hi"},
			{"post", "--level", "warn", "-T", "a", "-T", "b", "msg"},
			{"post", "--level", "bogus", "x"},
			{"post", "--ratio", "x", "m"},
			{"ping", "notanip"},
			{"--timeout", "2d", "ping", "1.1.1.1"},
			{"--timeout", "bad", "ping", "1.1.1.1"},
			{"post", "--server", "h:1", "m"},
			{"--secret", "s", "ping", "1.1.1.1"},
			{"--timeout", "1s", "ping", "1.1.1.1"},
			{"env:PING_TIMEOUT=2s", "ping", "1.1.1.1"},
			{"env:PING_TIMEOUT=", "ping", "1.1.1.1"},
			{"env:PING_TIMEOUT=broken", "--timeout", "3s", "ping", "1.1.1.1"},
			{"--no-debug", "ping", "1.1.1.1"},
			{"--help"},
			{"-h"},
			{"--version"},
			{"post", "--help"},
			{"help", "post"},
			{"--help-long"},
			{"--help-man"},
			{"--completion-bash"},
		},
		"valuetypes": {
			{},
			{"--item", "a,b", "--item", "c"},
			{"--size", "2MB"},
			{"--url", "http://x/y"},
			{"--mode", "a", "--mode", "c"},
			{"--mode", "d"},
			{"--color", "blue"},
			{"--file", "endtoend_test.go"},
			{"--count", "0x10"},
			{"-V"},
			{"--help"},
			{"--completion-bash", "--mode"},
			{"--completion-bash", "--color"},
			{"--completion-bash", "--"},
			{"env:VALUES_COLOR=x", "env:VALUES_COUNT=7"},
			{"env:VALUES_MODE=b"},
			{"--tag", "a,b"},
			{"env:VALUES_TAG=a\nb"},
			{"env:VALUES_TAG=a,b"},
			{"--bool", "true", "--bool", "false"},
			{"--bool", "x"},
			{"--label", "a:b", "--label", "c=d=e"},
			{"--label", "a:b=c"},
			{"--label", "x"},
			{"--conf", "./endtoend_test.go"},
			{"--conf", "missing.toml"},
			{"--conf", "."},
			{"--conf", "go.mod/x"},
		},
		"conditional": {
			{"run"},
			{"secret"},
			{"--token", "x", "run"},
			{"--help"},
			{"env:ENABLE_SECRET=1", "secret"},
			{"env:ENABLE_SECRET=1", "--token", "x", "run"},
			{"env:ENABLE_SECRET=1", "--help"},
		},
		"rootflags": {
			{},
			{"--name", "bob", "-v"},
			{"--no-verbose"},
			{"--help"},
		},
		"scope": {
			{},
			{"--name", "bob", "--count", "3"},
			{"--count", "x"},
			{"--count", "2.7"},
		},
		"edges": {
			{},
			{"--name", "bob", "--count", "3"},
		},
		"exporter": {
			{},
			{"--collector.textfile.directory", "/x", "--no-collector.cpu"},
			{"--web.listen-address", ":1"},
			{"--help"},
			{"-h"},
			{"--version"},
			{"--bogus"},
		},
		"actions": {
			{"version"},
			{"greet"},
			{"-v", "greet", "--name", "bob"},
			{"fail"},
			{"--help"},
			{},
			{"help", "greet"},
		},
		"cmdstate": {
			{"serve"},
			{"serve", "--port", "9090"},
			{"status", "--verbose"},
			{"ping", "--count", "2"},
			{"--help"},
		},
		"pkgstate": {
			{"db", "migrate"},
			{"db", "--dsn", "remote", "migrate", "--steps", "3"},
			{"list", "--all"},
			{"--help"},
		},
		"prior": {
			{},
			{"--region", "eu", "--port", "1", "--timeout", "1m", "--no-debug"},
			{"--help"},
		},
		"ctorarg": {
			{"build"},
			{"build", "--parallel", "3"},
			{"--help"},
		},
		"runtimevars": {
			{},
			{"--a", "x", "--limit", "3"},
			{"env:RUNTIMEVARS_FIRST=${literal}", "env:RUNTIMEVARS_SECOND=$5"},
			{"env:RUNTIMEVARS_UNIT=$", "--help"},
			{"--help"},
		},
		"helporder": {
			{"list", "--help"},
			{"ls", "--help"},
			{"show", "--help"},
			{"list", "--json", "--limit", "3", "-v"},
			{"show", "--id", "x"},
			{"show", "--id", "x", "--json"},
			{"build", "--help"},
			{"build", "--lang", "go", "--dir", "src", "--verbose"},
			{"publish", "--help"},
			{"publish", "--stage", "prod", "--arch", "arm"},
			{"publish", "--completion-bash", "--"},
			{"--help"},
		},
		"applayer": {
			{},
			{"greet", "bob"},
			{"greet", "bob", "--times", "2"},
			{"env:APPLAYER_EXTRA=xx", "greet", "bob", "--separator", ", "},
			{"greet", "--help"},
			{"admin"},
			{"admin", "purge", "--force"},
			{"greet"},
			{"list", "--all"},
			{"bogus"},
			{"--help"},
			{"help", "greet"},
		},
		"argorder": {
			{"copy", "a", "b"},
			{"copy", "a"},
			{"--help"},
		},
		"globalapp": {
			{"bob"},
			{"-v", "bob"},
			{"--verbose", "bob"},
			{},
			{"bob", "extra"},
			{"--help"},
			{"--bogus", "bob"},
		},
		"fanout": {
			{"serve"},
			{"serve", "--port", "9090"},
			{"db", "migrate"},
			{"db", "migrate", "--steps", "4"},
			{"db"},
			{"bogus"},
			{"help", "db"},
			{"--help"},
		},
		"execapp": {
			{"serve"},
			{"serve", "--port=81"},
			{"db", "migrate", "--steps", "2"},
			{"db"},
			{},
			{"help"},
		},
		"treeapp": {
			{"serve"},
			{"serve", "--port", "9090", "--host", "0.0.0.0"},
			{"serve", "--port=9090"},
			{"serve", "--label", "prod"},
			{"db", "migrate"},
			{"db", "migrate", "--steps", "3"},
			{"greet", "world"},
			{"greet"},
			{"db"},
			{"bogus"},
			{"serve", "--port", "notanum"},
			{"serve", "--workers", "1.5", "--retries", "7", "--max", "18446744073709551615", "--ratio", "0.5"},
			{"serve", "--retries", "128"},
			{"serve", "--max", "-1"},
			{"serve", "--workers", "x"},
			{"serve", "--ratio", "1e40"},
			{"--help"},
			{"help", "db"},
		},
	}
	for name, cases := range fixtures {
		t.Run(name, func(t *testing.T) {
			orig, err := filepath.Abs(filepath.Join("testdata", name))
			if err != nil {
				t.Fatal(err)
			}
			compare(t, orig, cases, expectedIssues[name])
		})
		for _, v := range variants[name] {
			t.Run(name+"/"+v.name, func(t *testing.T) {
				orig := filepath.Join(t.TempDir(), name)
				copyDir(t, filepath.Join("testdata", name), orig)
				switchKingpin(t, orig, v.module, v.path)
				compare(t, orig, append(cases, v.cases[name]...), v.issues[name])
			})
		}
	}
}

// A variant runs a fixture with another kingpin release, with extra command
// lines and the issues the conversion should report.
type variant struct {
	name, module, path string
	cases              map[string][][]string
	issues             map[string][]string
}

var (
	kingpinV2 = variant{
		name: "v2", module: "github.com/alecthomas/kingpin/v2@v2.4.0", path: "github.com/alecthomas/kingpin/v2",
		cases: v2Cases,
	}
	kingpinV22 = variant{
		name: "v2.2", module: "gopkg.in/alecthomas/kingpin.v2@v2.2.6", path: "gopkg.in/alecthomas/kingpin.v2",
		cases: v2Cases,
	}
	kingpinFork = variant{
		name: "fork", module: "github.com/fastly/kingpin@v2.1.12-0.20191105091915-95d230a53780+incompatible",
		path: "github.com/fastly/kingpin",
	}
	kingpinV3 = variant{
		name: "v3", module: "gopkg.in/alecthomas/kingpin.v3-unstable@v3.0.0-20191105091915-95d230a53780",
		path: "gopkg.in/alecthomas/kingpin.v3-unstable", issues: expectedIssues,
	}
)

// variants lists the other releases each fixture also runs with, when it only
// uses API they have too.
var variants = map[string][]variant{
	"chat":        {kingpinV2, kingpinV22, kingpinV3},
	"argorder":    {kingpinV2, kingpinV22},
	"globalapp":   {kingpinV2, kingpinV22},
	"fanout":      {kingpinV2, kingpinV22},
	"execapp":     {kingpinV2, kingpinV22},
	"modular":     {kingpinV22},
	"classic":     {kingpinV22},
	"valuetypes":  {kingpinV22},
	"conditional": {kingpinV22},
	"rootflags":   {kingpinV22},
	"scope":       {kingpinV22},
	"edges":       {kingpinV22},
	"exporter":    {kingpinV22},
	"actions":     {kingpinV22},
	"prior":       {kingpinV22},
	"cmdstate":    {kingpinV22},
	"pkgstate":    {kingpinV22},
	"ctorarg":     {kingpinV22},
	"applayer":    {kingpinV22, kingpinFork},
	"helporder":   {kingpinV22, kingpinFork},
	"runtimevars": {kingpinV22, kingpinFork},
}

// v2Cases are command lines that only mean something to kingpin v2.
var v2Cases = map[string][][]string{
	"chat": {
		{"--no-debug", "register", "a", "b"},
		{"--help-long"},
		{"--help-man"},
		{"--completion-script-zsh"},
	},
}

// compare converts a copy of the program in orig, builds both and runs the
// same command lines through them.
func compare(t *testing.T, orig string, cases [][]string, expected []string) {
	t.Helper()
	conv := filepath.Join(t.TempDir(), filepath.Base(orig))
	copyDir(t, orig, conv)
	convertDir(t, conv, expected)
	origBin := goBuild(t, orig)
	convBin := goBuild(t, conv)
	// The fixture's own tests must pass before and after the conversion.
	if hasTests(t, conv) {
		for _, dir := range []string{orig, conv} {
			cmd := exec.Command("go", "test", "./...")
			cmd.Dir = dir
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("tests of %s: %v\n%s", dir, err, out)
			}
		}
	}
	for _, args := range cases {
		want := run(origBin, args)
		got := run(convBin, args)
		if got != want {
			t.Errorf("%q differs\n--- kingpin\n%s\n--- kong\n%s", args, want, got)
		}
	}
}

func hasTests(t *testing.T, dir string) bool {
	t.Helper()
	found := false
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, "_test.go") {
			found = true
		}
		return nil
	})
	return found
}

// switchKingpin makes the program in dir use another kingpin.
// module is what go get fetches, importPath the package to import.
func switchKingpin(t *testing.T, dir, module, importPath string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".go" {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, from := range []string{"github.com/fastly/kingpin", "github.com/alecthomas/kingpin/v2"} {
			b = bytes.ReplaceAll(b, []byte(strconv.Quote(from)), []byte(strconv.Quote(importPath)))
		}
		return os.WriteFile(path, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"get", module}, {"mod", "tidy"}} {
		cmd := exec.Command("go", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
}

// expectedIssues are the reports a fixture must produce, by substring.
var expectedIssues = map[string][]string{
	// kingpin's chat example compares the parsed command with FullCommand().
	// In kingpin v3 and the fastly fork, that includes the arguments, so
	// those branches never ran.
	// The tool keeps that and says so.
	"chat": {
		`register.FullCommand() is "register <nick> <name>"`,
		`post.FullCommand() is "post <channel> [<text> ...]"`,
	},
}

func convertDir(t *testing.T, dir string, expected []string) {
	t.Helper()
	prog, err := Load(dir, "./...")
	if err != nil {
		t.Fatal(err)
	}
	res, err := Convert(prog, Options{Compat: compatFS(t)})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, is := range res.Issues {
		ok := false
		for _, e := range expected {
			if strings.Contains(is.Reason, e) {
				ok, seen[e] = true, true
			}
		}
		if !ok {
			t.Errorf("issue: %s: %s", is.Pos, is.Reason)
		}
	}
	for _, e := range expected {
		if !seen[e] {
			t.Errorf("missing expected issue %q", e)
		}
	}
	for path, src := range res.Files {
		writeFile(t, path, src)
	}
	for path, src := range res.NewFiles {
		writeFile(t, path, src)
	}
	get := exec.Command("go", "get", "github.com/alecthomas/kong@v1.17.0")
	get.Dir = dir
	if out, err := get.CombinedOutput(); err != nil {
		t.Fatalf("go get: %v\n%s", err, out)
	}
}

// compatFS returns the kongcompat sources, which the tool embeds.
func compatFS(t *testing.T) fs.FS {
	t.Helper()
	dir := filepath.Join("..", "..", "kongcompat")
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}
	return os.DirFS(dir)
}

func writeFile(t *testing.T, path, src string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
}

func copyDir(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, path)
		target := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func goBuild(t *testing.T, dir string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "prog")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building %s: %v\n%s", dir, err, out)
	}
	return bin
}

// run executes a program and summarizes what a user sees.
// Leading arguments written env:NAME=value set environment variables instead.
func run(bin string, args []string) string {
	env := append(os.Environ(), "COLUMNS=80")
	for len(args) > 0 && strings.HasPrefix(args[0], "env:") {
		env = append(env, strings.TrimPrefix(args[0], "env:"))
		args = args[1:]
	}
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code = exit.ExitCode()
	}
	return "exit " + strconv.Itoa(code) + "\nstdout:\n" + stdout.String() + "stderr:\n" + stderr.String()
}
