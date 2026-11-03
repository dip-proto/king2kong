// Command globalapp uses kingpin's package-level application.
package main

import (
	"fmt"

	"github.com/fastly/kingpin"
)

var (
	verbose = kingpin.Flag("verbose", "Verbose mode.").Short('v').Bool()
	name    = kingpin.Arg("name", "Name of user.").Required().String()
)

func main() {
	kingpin.Parse()
	fmt.Printf("%v, %s\n", *verbose, *name)
}
