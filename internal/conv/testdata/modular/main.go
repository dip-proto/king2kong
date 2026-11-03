// Command modular follows kingpin v2's README: each command is a struct that
// registers itself and runs through an Action, the application has a
// version, a -h short flag, environment variables and the usual value types.
package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/alecthomas/kingpin/v2"
)

type lsCommand struct {
	all     bool
	sortBy  string
	columns []string
}

func (l *lsCommand) run(c *kingpin.ParseContext) error {
	fmt.Printf("ls all=%v sort=%s columns=%s\n", l.all, l.sortBy, strings.Join(l.columns, ","))
	return nil
}

func configureLsCommand(app *kingpin.Application) {
	c := &lsCommand{}
	ls := app.Command("ls", "List files.").Action(c.run)
	ls.Flag("all", "List all files.").Short('a').BoolVar(&c.all)
	ls.Flag("sort", "Sort order.").Default("name").EnumVar(&c.sortBy, "name", "size", "time")
	ls.Flag("column", "Columns to show.").StringsVar(&c.columns)
}

type fetchCommand struct {
	url     string
	timeout time.Duration
	retries int
	headers map[string]string
}

func (f *fetchCommand) run(c *kingpin.ParseContext) error {
	fmt.Printf("fetch %s timeout=%s retries=%d headers=%d\n", f.url, f.timeout, f.retries, len(f.headers))
	return nil
}

func configureFetchCommand(app *kingpin.Application) {
	c := &fetchCommand{}
	fetch := app.Command("fetch", "Fetch a URL.").Action(c.run)
	fetch.Arg("url", "URL to fetch.").Required().StringVar(&c.url)
	fetch.Flag("timeout", "Request timeout.").Default("5s").Envar("MODULAR_TIMEOUT").DurationVar(&c.timeout)
	fetch.Flag("retries", "Number of retries.").Short('r').Default("3").IntVar(&c.retries)
	fetch.Flag("header", "Extra header.").Short('H').StringMapVar(&c.headers)
}

func main() {
	app := kingpin.New("modular", "My modular application.")
	app.Version("1.2.3")
	app.HelpFlag.Short('h')
	verbose := app.Flag("verbose", "Verbosity, repeatable.").Short('v').Counter()
	configureLsCommand(app)
	configureFetchCommand(app)
	cmd := kingpin.MustParse(app.Parse(os.Args[1:]))
	fmt.Printf("done %s verbose=%d\n", cmd, *verbose)
}
