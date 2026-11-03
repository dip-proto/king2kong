// Command helporder declares its flag fields in another order than it
// registers the flags, embeds a struct holding a flag that only one command
// registers, and lists the same command under two names.
package main

import (
	"fmt"
	"os"

	"github.com/alecthomas/kingpin/v2"
)

// JSONOutput holds --json, for the commands that offer it.
type JSONOutput struct {
	json bool
}

type listCommand struct {
	// Filtering.
	filter string
	limit  int

	// Output.
	JSONOutput
	verbose bool
}

func (l *listCommand) register(parent *kingpin.CmdClause) {
	parent.Flag("filter", "Only show matching entries.").StringVar(&l.filter)
	parent.Flag("json", "Render output as JSON.").BoolVar(&l.json)
	parent.Flag("limit", "Show at most this many entries.").Default("10").IntVar(&l.limit)
	parent.Flag("verbose", "Show more.").Short('v').BoolVar(&l.verbose)
}

type showCommand struct {
	JSONOutput

	id     string
	format string
}

// BuildFlags are the flags every build shares.
type BuildFlags struct {
	Dir  string
	Lang string
}

// buildCommand registers its own flag between two of the shared ones,
// which no field order can show.
type buildCommand struct {
	Flags   BuildFlags
	verbose bool
}

// publishCommand declares its flags in commented groups, and registers
// them in alphabetical order across the groups.
type publishCommand struct {
	// Build fields.
	arch    string
	cache   string
	release string

	// Deploy fields.
	bucket string
	domain string
	stage  string
}

func main() {
	app := kingpin.New("helporder", "Flags in registration order.")
	list := &listCommand{}
	list.register(app.Command("list", "List entries."))
	legacy := &listCommand{}
	legacy.register(app.Command("ls", "List entries (deprecated)."))
	show := &showCommand{}
	showCmd := app.Command("show", "Show an entry.")
	showCmd.Flag("format", "Output format.").StringVar(&show.format)
	showCmd.Flag("id", "Entry ID.").Required().StringVar(&show.id)
	build := &buildCommand{}
	buildCmd := app.Command("build", "Build.")
	buildCmd.Flag("dir", "Directory.").StringVar(&build.Flags.Dir)
	buildCmd.Flag("verbose", "Verbose.").BoolVar(&build.verbose)
	buildCmd.Flag("lang", "Language.").StringVar(&build.Flags.Lang)
	publish := &publishCommand{}
	publishCmd := app.Command("publish", "Publish.")
	publishCmd.Flag("arch", "Architecture.").StringVar(&publish.arch)
	publishCmd.Flag("bucket", "Bucket.").StringVar(&publish.bucket)
	publishCmd.Flag("cache", "Cache.").StringVar(&publish.cache)
	publishCmd.Flag("domain", "Domain.").StringVar(&publish.domain)
	publishCmd.Flag("release", "Release.").StringVar(&publish.release)
	publishCmd.Flag("stage", "Stage.").StringVar(&publish.stage)
	switch kingpin.MustParse(app.Parse(os.Args[1:])) {
	case "list":
		fmt.Println("list", list.filter, list.json, list.limit, list.verbose)
	case "ls":
		fmt.Println("ls", legacy.filter, legacy.json, legacy.limit, legacy.verbose)
	case "show":
		fmt.Println("show", show.id, show.format, show.json)
	case "build":
		fmt.Println("build", build.Flags, build.verbose)
	case "publish":
		fmt.Println("publish", *publish)
	}
}
