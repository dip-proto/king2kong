// Command valuetypes registers flags of most kingpin v2 value types, a custom
// value with SetValue, completion hints and default environment variables.
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/alecthomas/kingpin/v2"
)

// listValue is a custom kingpin value, the way kingpin's README shows.
type listValue []string

func (l *listValue) Set(value string) error {
	*l = append(*l, strings.Split(value, ",")...)
	return nil
}

func (l *listValue) String() string { return strings.Join(*l, ",") }

func (l *listValue) IsCumulative() bool { return true }

func main() {
	app := kingpin.New("values", "Value types.")
	app.Version("2.0").VersionFlag.Short('V')
	app.DefaultEnvars()
	var list listValue
	app.Flag("item", "Items.").SetValue(&list)
	size := app.Flag("size", "Size.").Default("1KB").Bytes()
	url := app.Flag("url", "URL.").URL()
	modes := app.Flag("mode", "Modes.").Enums("a", "b", "c")
	color := app.Flag("color", "Color.").HintOptions("red", "green").String()
	file := app.Flag("file", "File.").File()
	count := app.Flag("count", "Count.").Uint8()
	tags := app.Flag("tag", "Tags.").Default("x", "y").Strings()
	bools := app.Flag("bool", "Booleans.").BoolList()
	labels := app.Flag("label", "Labels.").StringMap()
	conf := app.Flag("conf", "Configuration file.").ExistingFile()
	kingpin.MustParse(app.Parse(os.Args[1:]))
	fmt.Println(list, *size, *url, *modes, *color, *file != nil, *count, *tags, len(*tags))
	fmt.Println(*bools, *labels, *conf)
}
