// Command rootflags writes its application flags into a struct of its own.
package main

import (
	"fmt"
	"os"

	"github.com/alecthomas/kingpin/v2"
)

type options struct {
	// Who runs it.
	Name string
	Team string

	// Output.
	Verbose bool
}

func main() {
	var opts options
	app := kingpin.New("rootflags", "Flags stored in a struct.")
	app.Flag("verbose", "Verbose.").Short('v').BoolVar(&opts.Verbose)
	app.Flag("name", "Name.").Default("anon").StringVar(&opts.Name)
	app.Flag("team", "Team.").StringVar(&opts.Team)
	kingpin.MustParse(app.Parse(os.Args[1:]))
	fmt.Println(opts.Name, opts.Team, opts.Verbose)
}
