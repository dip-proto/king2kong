// Command edges declares a flag destination next to another variable, reads
// it before parsing and assigns it afterwards.
package main

import (
	"fmt"
	"os"

	"github.com/alecthomas/kingpin/v2"
)

func main() {
	var name, other string
	other = "kept"
	app := kingpin.New("edges", "Edge cases.")
	app.Flag("name", "Name.").StringVar(&name)
	count := app.Flag("count", "Count.").Default("1").Int()
	before := name
	kingpin.MustParse(app.Parse(os.Args[1:]))
	if name == "" {
		name = "anonymous"
	}
	copied := *count
	fmt.Println(name, other, before == "", copied)
}
