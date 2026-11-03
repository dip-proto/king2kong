// Command applayer drives kingpin from an application layer, apart from the
// package declaring the commands: it checks the command line with
// ParseContext, then parses, selects the command by name and runs it.
package main

import (
	"fmt"
	"os"

	"github.com/alecthomas/kingpin/v2"

	"applayer/cmd"
)

func configure() *kingpin.Application {
	app := kingpin.New("applayer", "An application layer.")
	app.Terminate(nil)
	return app
}

func main() {
	cfg := &cmd.Config{Greeting: "hello"}
	app := configure()
	commands := cmd.Define(app, cfg)
	ctx, err := app.ParseContext(os.Args[1:])
	if err != nil {
		fmt.Println("error:", err)
		app.Usage(nil)
		os.Exit(2)
	}
	if ctx.SelectedCommand == nil {
		fmt.Println("commands:", len(app.Model().Commands))
		for _, c := range commands {
			fmt.Println("listed:", c.Name())
		}
		os.Exit(0)
	}
	name, err := app.Parse(os.Args[1:])
	if err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
	for _, c := range commands {
		if c.Name() == ctx.SelectedCommand.FullCommand() {
			fmt.Println("running", name)
			if err := c.Run(); err != nil {
				os.Exit(1)
			}
			return
		}
	}
	fmt.Println("not a command to run:", name)
	os.Exit(1)
}
