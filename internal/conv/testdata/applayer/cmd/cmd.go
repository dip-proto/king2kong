// Package cmd declares the commands, each registered by its constructor.
package cmd

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/alecthomas/kingpin/v2"
)

// Runner is a command the application layer runs.
type Runner interface {
	Name() string
	Run() error
}

// Config is what every command receives.
type Config struct{ Greeting string }

// defaultTimes and separatorName are only known when the program starts.
var (
	defaultTimes  = strconv.Itoa(1 + len(os.Getenv("APPLAYER_EXTRA")))
	separatorName = strings.Repeat("space", 1)
)

// GreetCommand greets someone.
type GreetCommand struct {
	clause    *kingpin.CmdClause
	cfg       *Config
	who       string
	times     int
	separator string
}

// NewGreetCommand registers the greet command.
func NewGreetCommand(app *kingpin.Application, cfg *Config) *GreetCommand {
	c := &GreetCommand{cfg: cfg}
	c.clause = app.Command("greet", "Greet someone.")
	c.clause.Arg("who", "Who to greet.").Required().StringVar(&c.who)
	c.clause.Flag("times", "How many times.").Default(defaultTimes).IntVar(&c.times)
	c.clause.Flag("separator", "Separator, "+separatorName+" by default.").Default(" ").StringVar(&c.separator)
	return c
}

// Name is the command's name.
func (c *GreetCommand) Name() string { return c.clause.FullCommand() }

// Run greets.
func (c *GreetCommand) Run() error {
	for i := 0; i < c.times; i++ {
		fmt.Printf("%s%s%s\n", c.cfg.Greeting, c.separator, c.who)
	}
	return nil
}

// ListCommand lists things.
type ListCommand struct {
	clause *kingpin.CmdClause
	cfg    *Config
	all    bool
}

// NewListCommand registers the list command.
func NewListCommand(app *kingpin.Application, cfg *Config) *ListCommand {
	c := &ListCommand{cfg: cfg}
	c.clause = app.Command("list", "List things.")
	c.clause.Flag("all", "List everything.").BoolVar(&c.all)
	return c
}

// Name is the command's name.
func (c *ListCommand) Name() string { return c.clause.FullCommand() }

// Run lists.
func (c *ListCommand) Run() error {
	fmt.Println("list all:", c.all)
	return nil
}

// AdminCommand groups the administration commands; it only has
// subcommands, so Define does not list it.
type AdminCommand struct {
	clause *kingpin.CmdClause
}

// Name is the command's name.
func (c *AdminCommand) Name() string { return c.clause.FullCommand() }

// Run does nothing.
func (c *AdminCommand) Run() error { return nil }

// PurgeCommand purges things.
type PurgeCommand struct {
	clause *kingpin.CmdClause
	force  bool
}

// Name is the command's name.
func (c *PurgeCommand) Name() string { return c.clause.FullCommand() }

// Run purges.
func (c *PurgeCommand) Run() error {
	fmt.Println("purge force:", c.force)
	return nil
}

// Define registers every command and returns those that run, listed first.
func Define(app *kingpin.Application, cfg *Config) []Runner {
	admin := &AdminCommand{}
	admin.clause = app.Command("admin", "Administration.")
	purge := &PurgeCommand{}
	purge.clause = admin.clause.Command("purge", "Purge everything.")
	purge.clause.Flag("force", "Do not ask.").BoolVar(&purge.force)
	var greet = NewGreetCommand(app, cfg)
	list, admins := NewListCommand(app, cfg), purge
	if greet.Name() == "" {
		panic("greet has no name")
	}
	return []Runner{
		list,
		admins,
		greet,
	}
}
