package conv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReports checks that programs the tool can't fully convert get a report instead of a silent change.
func TestReports(t *testing.T) {
	cmdPkg := `package cmd

import (
	"fmt"
	"os"

	"github.com/alecthomas/kingpin/v2"
)

type Config struct{ Greeting string }

type Runner interface{ Name() string }

type hello struct {
	cfg *Config
	who string
}

func newHello(app *kingpin.Application, cfg *Config) *hello {
	h := &hello{cfg: cfg}
	app.Command("hello", "").Arg("who", "").StringVar(&h.who)
	return h
}

func (h *hello) Name() string { fmt.Println(h.cfg.Greeting); return "hello" }

type wave struct {
	cfg *Config
	who string
}

func newWave(app *kingpin.Application, cfg *Config) *wave {
	w := &wave{cfg: cfg}
	app.Command("wave", "").Arg("who", "").StringVar(&w.who)
	return w
}

func (w *wave) Name() string { fmt.Println(w.cfg.Greeting); return "wave" }

type bye struct {
	out  interface{ Write([]byte) (int, error); Stat() (os.FileInfo, error) }
	loud bool
}

func newBye(app *kingpin.Application, out interface{ Write([]byte) (int, error); Stat() (os.FileInfo, error) }) *bye {
	b := &bye{out: out}
	app.Command("bye", "").Flag("loud", "").BoolVar(&b.loud)
	return b
}

func (b *bye) Name() string { fmt.Fprintln(b.out, "bye"); return "bye" }

func Hello(app *kingpin.Application, cfg *Config) []Runner { return []Runner{newHello(app, cfg)} }

func Wave(app *kingpin.Application, cfg *Config) []Runner { return []Runner{newWave(app, cfg)} }

func Bye(app *kingpin.Application, out interface{ Write([]byte) (int, error); Stat() (os.FileInfo, error) }) []Runner {
	return []Runner{newBye(app, out)}
}
`
	runtimeMain := func(calls string) string {
		return `package main

import (
	"os"

	"github.com/alecthomas/kingpin/v2"

	"probe/cmd"
)

func main() {
	app := kingpin.New("probe", "")
` + calls + `
	name := kingpin.MustParse(app.Parse(os.Args[1:]))
	for _, r := range runners {
		if r.Name() == name {
			return
		}
	}
}
`
	}
	cases := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{
			name: "field set by a registering function",
			files: map[string]string{"main.go": `package main

import (
	"fmt"
	"os"

	"github.com/alecthomas/kingpin/v2"
)

type Config struct{ Greeting string }

type hello struct {
	cfg *Config
	who string
}

func newHello(app *kingpin.Application, cfg *Config) {
	h := &hello{cfg: cfg}
	c := app.Command("hello", "")
	c.Arg("who", "").StringVar(&h.who)
	c.Action(h.run)
}

func (h *hello) run(*kingpin.ParseContext) error {
	fmt.Println(h.cfg.Greeting, h.who)
	return nil
}

func main() {
	app := kingpin.New("probe", "")
	newHello(app, &Config{Greeting: "hello"})
	kingpin.MustParse(app.Parse(os.Args[1:]))
}
`},
			want: "newHello builds a hello with cfg set",
		},
		{
			name: "constructors given different values of one type",
			files: map[string]string{"cmd/cmd.go": cmdPkg, "main.go": runtimeMain(`
	runners := cmd.Hello(app, &cmd.Config{Greeting: "hello"})
	runners = append(runners, cmd.Wave(app, &cmd.Config{Greeting: "yo"})...)`)},
			want: "constructors newHello and newWave get different *cmd.Config values",
		},
		{
			name: "unexported interface, bound at a call",
			files: map[string]string{
				"cmd/cmd.go": strings.ReplaceAll(cmdPkg, "interface{ Write([]byte) (int, error); Stat() (os.FileInfo, error) }", "output") +
					"\ntype output interface{ Write([]byte) (int, error); Stat() (os.FileInfo, error) }\n",
				"main.go": runtimeMain(`
	runners := cmd.Bye(app, os.Stdout)`),
			},
			want: "the hooks take os.Stdout as cmd.output",
		},
		{
			name: "interface naming a package, bound at a call",
			files: map[string]string{"cmd/cmd.go": cmdPkg, "main.go": runtimeMain(`
	runners := cmd.Bye(app, os.Stdout)`)},
			want: "the hooks take os.Stdout as interface{Stat()",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := convertProbe(t, tc.files)
			var got []string
			for _, is := range res.Issues {
				if strings.Contains(is.Reason, tc.want) {
					return
				}
				got = append(got, is.Reason)
			}
			t.Errorf("no issue says %q; got %q", tc.want, got)
		})
	}
}

// convertProbe converts a kingpin v2 module named probe, made of the given files.
func convertProbe(t *testing.T, files map[string]string) *Result {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"go.mod", "go.sum"} {
		b, err := os.ReadFile(filepath.Join("testdata", "classic", name))
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, name), strings.Replace(string(b), "module classic", "module probe", 1))
	}
	for name, src := range files {
		writeFile(t, filepath.Join(dir, name), src)
	}
	prog, err := Load(dir, "./...")
	if err != nil {
		t.Fatal(err)
	}
	res, err := Convert(prog, Options{Compat: compatFS(t)})
	if err != nil {
		t.Fatal(err)
	}
	return res
}
