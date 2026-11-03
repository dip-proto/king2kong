// Command argorder declares its positional arguments in another order than
// it registers them.
package main

import (
	"fmt"
	"os"

	"github.com/fastly/kingpin"
)

type copyCmd struct {
	dst string
	src string
}

func main() {
	app := kingpin.New("argorder", "Copy things")
	var cp copyCmd
	c := app.Command("copy", "Copy src to dst")
	c.Arg("src", "Source").Required().StringVar(&cp.src)
	c.Arg("dst", "Destination").Required().StringVar(&cp.dst)
	kingpin.MustParse(app.Parse(os.Args[1:]))
	fmt.Println(cp.src, "->", cp.dst)
}
