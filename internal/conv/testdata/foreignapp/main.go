// Command foreignapp hands its kingpin application to code outside the
// program, which could register flags on it or parse with it.
package main

import (
	"fmt"
	"os"

	"github.com/alecthomas/kingpin/v2"
)

func main() {
	app := kingpin.New("foreignapp", "Hands its application away.")
	verbose := app.Flag("verbose", "Verbose.").Bool()
	fmt.Fprintln(os.Stderr, app)
	kingpin.MustParse(app.Parse(os.Args[1:]))
	fmt.Println(*verbose)
}
