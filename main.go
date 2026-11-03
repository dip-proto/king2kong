// Command kingpin2kong rewrites a Go program that builds its command line
// with kingpin so that it declares it as a kong grammar instead.
//
//	kingpin2kong [-dir DIR] [-write] [-v] [-dump] [packages...]
//
// Without -write, it only reports what it would change.
// Packages default to ./... under -dir, the module to convert.
package main

import (
	"embed"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"

	"kingpin2kong/internal/conv"
)

// compatSources is the kongcompat package, written into converted modules.
//
//go:embed kongcompat/*.go
var compatSources embed.FS

// kongVersion is the kong release converted programs depend on.
const kongVersion = "v1.17.0"

func main() {
	write := flag.Bool("write", false, "write the converted files (default: report only)")
	dir := flag.String("dir", ".", "directory of the module to convert")
	verbose := flag.Bool("v", false, "list every issue instead of a summary")
	dump := flag.Bool("dump", false, "print the kingpin grammar the tool recovers, and stop")
	flag.Parse()

	patterns := flag.Args()
	if len(patterns) == 0 {
		patterns = []string{"./..."}
	}
	if *dump {
		prog, err := conv.Load(*dir, patterns...)
		if err != nil {
			fmt.Fprintln(os.Stderr, "kingpin2kong:", err)
			os.Exit(1)
		}
		conv.Analyze(prog).Dump(os.Stdout)
		return
	}
	if err := run(*dir, patterns, *write, *verbose, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "kingpin2kong:", err)
		os.Exit(1)
	}
}

func run(dir string, patterns []string, write, verbose bool, out io.Writer) error {
	prog, err := conv.Load(dir, patterns...)
	if err != nil {
		return err
	}
	compat, err := fs.Sub(compatSources, "kongcompat")
	if err != nil {
		return err
	}
	res, err := conv.Convert(prog, conv.Options{Compat: compat})
	if err != nil {
		report(out, res, verbose)
		return err
	}
	report(out, res, verbose)
	if !write {
		fmt.Fprintln(out, "\nNothing written; pass -write to apply the changes.")
		return nil
	}
	paths := make([]string, 0, len(res.Files)+len(res.NewFiles))
	for p := range res.Files {
		paths = append(paths, p)
	}
	for p := range res.NewFiles {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		src, ok := res.Files[p]
		if !ok {
			src = res.NewFiles[p]
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "\nWrote %d file(s).\n", len(paths))
	get := exec.Command("go", "get", "github.com/alecthomas/kong@"+kongVersion)
	get.Dir = dir
	if msg, err := get.CombinedOutput(); err != nil {
		return fmt.Errorf("adding kong to go.mod: %v\n%s", err, msg)
	}
	fmt.Fprintf(out, "Added github.com/alecthomas/kong %s to go.mod.\n", kongVersion)
	return nil
}

func report(out io.Writer, res *conv.Result, verbose bool) {
	if res == nil {
		return
	}
	s := res.Stats
	fmt.Fprintf(out, "Converted %d command(s), %d flag and argument registration(s) (%d positional).\n", s.Commands, s.Flags, s.Args)
	fmt.Fprintf(out, "Exported %d field(s), added %d field(s) for destinations kong cannot reach, removed %d declaration(s).\n",
		s.Renamed, s.Shadows, s.RemovedFuncs)
	fmt.Fprintf(out, "%d file(s) changed, %d new.\n", len(res.Files), len(res.NewFiles))
	for _, n := range res.Notes {
		fmt.Fprintf(out, "\nNote: %s\n", n)
	}
	if len(res.Issues) == 0 {
		return
	}
	fmt.Fprintf(out, "\n%d thing(s) need a person:\n", len(res.Issues))
	if verbose {
		issues := append([]conv.Issue{}, res.Issues...)
		sort.Slice(issues, func(i, j int) bool {
			if issues[i].Pos.Filename != issues[j].Pos.Filename {
				return issues[i].Pos.Filename < issues[j].Pos.Filename
			}
			return issues[i].Pos.Line < issues[j].Pos.Line
		})
		for _, is := range issues {
			fmt.Fprintf(out, "  %s: %s\n", is.Pos, is.Reason)
		}
		return
	}
	counts := map[string]int{}
	for _, is := range res.Issues {
		counts[is.Reason]++
	}
	reasons := make([]string, 0, len(counts))
	for r := range counts {
		reasons = append(reasons, r)
	}
	sort.Slice(reasons, func(i, j int) bool { return counts[reasons[i]] > counts[reasons[j]] })
	for _, r := range reasons {
		fmt.Fprintf(out, "  %4d  %s\n", counts[r], r)
	}
}
