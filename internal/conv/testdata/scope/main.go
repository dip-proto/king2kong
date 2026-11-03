// Command scope reads a flag destination from another function than the one
// building the application, which it calls cli.
package main

import (
	"fmt"
	"os"

	"github.com/alecthomas/kingpin/v2"
)

var name string

func show() { fmt.Println("name:", name) }

func main() {
	cli := kingpin.New("scope", "Scope test.")
	cli.Flag("name", "Name.").StringVar(&name)
	count := cli.Flag("count", "Count.").Int()
	kingpin.MustParse(cli.Parse(os.Args[1:]))
	show()
	fmt.Println(*count)
}
