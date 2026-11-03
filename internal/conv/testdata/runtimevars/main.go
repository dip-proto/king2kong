// Command runtimevars computes defaults and help texts when it starts: from
// the environment, from a counter, with names that read alike.
package main

import (
	"fmt"
	"math"
	"os"
	"strconv"

	"github.com/alecthomas/kingpin/v2"
)

var calls int

func next() string {
	calls++
	return strconv.Itoa(calls)
}

var unit = os.Getenv("RUNTIMEVARS_UNIT") + "s"

func main() {
	app := kingpin.New("runtimevars", "Run-time defaults.")
	z := app.Flag("z", "Registered first.").Default(next()).String()
	a := app.Flag("a", "Registered second.").Default(next()).String()
	first := app.Flag("foo-bar", "One.").Default(os.Getenv("RUNTIMEVARS_FIRST")).String()
	second := app.Flag("foo_bar", "Two.").Default(os.Getenv("RUNTIMEVARS_SECOND")).String()
	limit := app.Flag("limit", "Limit, in "+unit+".").Default(strconv.FormatInt(math.MaxInt32, 10)).Int64()
	kingpin.MustParse(app.Parse(os.Args[1:]))
	fmt.Printf("z=%s a=%s first=%q second=%q limit=%d\n", *z, *a, *first, *second, *limit)
}
