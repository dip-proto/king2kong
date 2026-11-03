// Command actions runs commands through Action callbacks: function literals,
// a named function, and one reading a flag.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/alecthomas/kingpin/v2"
)

var (
	app     = kingpin.New("actions", "Commands run by actions.")
	verbose = app.Flag("verbose", "Verbose output.").Short('v').Bool()
	name    = app.Flag("name", "Name to greet.").Default("world").String()
)

func fail(*kingpin.ParseContext) error {
	return errors.New("this command always fails")
}

func main() {
	app.Command("version", "Show the version.").Action(func(*kingpin.ParseContext) error {
		fmt.Println("actions 1.0")
		return nil
	})
	app.Command("greet", "Greet someone.").Action(func(_ *kingpin.ParseContext) error {
		if *verbose {
			fmt.Println("greeting...")
		}
		fmt.Printf("hello %s\n", *name)
		return nil
	})
	app.Command("fail", "Fail.").Action(fail)
	cmd := kingpin.MustParse(app.Parse(os.Args[1:]))
	fmt.Println("selected:", cmd)
}
