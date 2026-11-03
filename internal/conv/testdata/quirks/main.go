// Command quirks registers values kingpin parsed in its own way: a
// positional boolean, an enum that lists the empty value, a default read
// from the environment, a default path, lists whose environment variables
// are split at newlines but whose command-line values are not, a counter
// with an environment variable, and a file.
package main

import (
	"fmt"
	"os"

	"github.com/alecthomas/kingpin/v2"
)

func main() {
	app := kingpin.New("quirks", "Value quirks.")
	format := app.Flag("format", "Format.").Enum("", "json", "text")
	port := app.Flag("port", "Port.").Default(os.Getenv("QUIRKS_PORT")).Int()
	docs := app.Flag("doc", "Documents.").Envar("QUIRKS_DOCS").Strings()
	labels := app.Flag("label", "Labels.").Envar("QUIRKS_LABELS").StringMap()
	conf := app.Flag("conf", "Configuration file.").Default("missing.toml").ExistingFile()
	verbose := app.Flag("verbose", "Verbosity.").Short('v').Envar("QUIRKS_VERBOSE").Counter()
	input := app.Flag("input", "Input file.").File()
	enabled := app.Arg("enabled", "Enabled.").Required().Bool()
	app.Version("1.0")
	if _, err := app.Parse(os.Args[1:]); err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("%q %d %q %q %q %d %v\n", *format, *port, *docs, *labels, *conf, *verbose, *enabled)
	if *input != nil {
		fmt.Println((*input).Name())
	}
}
