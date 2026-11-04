// Command quirks registers values that kingpin parses in its own way.
// There's a positional bool, an enum that accepts the empty value, a default read from the environment, a default file path, a counter with an environment variable, and a file.
// Lists split their environment variables at newlines, but not their command-line values.
// The custom values are a list with defaults, a list taking the remaining arguments, and a bool with a third state.
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/alecthomas/kingpin/v2"
)

type listValue []string

func (l *listValue) Set(s string) error {
	*l = append(*l, s)
	return nil
}

func (l *listValue) String() string { return strings.Join(*l, ",") }

func (l *listValue) IsCumulative() bool { return true }

type colorMode string

func (c *colorMode) Set(s string) error {
	switch s {
	case "true":
		s = "always"
	case "false":
		s = "never"
	}
	*c = colorMode(s)
	return nil
}

func (c *colorMode) String() string { return string(*c) }

func (c *colorMode) IsBoolFlag() bool { return true }

func main() {
	app := kingpin.New("quirks", "Value quirks.")
	format := app.Flag("format", "Format.").Enum("", "json", "text")
	port := app.Flag("port", "Port.").Default(os.Getenv("QUIRKS_PORT")).Int()
	docs := app.Flag("doc", "Documents.").Envar("QUIRKS_DOCS").Strings()
	labels := app.Flag("label", "Labels.").Envar("QUIRKS_LABELS").StringMap()
	conf := app.Flag("conf", "Configuration file.").Default("missing.toml").ExistingFile()
	verbose := app.Flag("verbose", "Verbosity.").Short('v').Envar("QUIRKS_VERBOSE").Counter()
	input := app.Flag("input", "Input file.").File()
	var tags, files listValue
	app.Flag("tag", "Tags.").Default("first", "second").SetValue(&tags)
	color := colorMode("auto")
	app.Flag("color", "Color.").SetValue(&color)
	enabled := app.Arg("enabled", "Enabled.").Required().Bool()
	app.Arg("files", "Files.").SetValue(&files)
	app.Version("1.0")
	if _, err := app.Parse(os.Args[1:]); err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("%q %d %q %q %q %d %v\n", *format, *port, *docs, *labels, *conf, *verbose, *enabled)
	fmt.Printf("%q %q %s\n", tags, files, color)
	if *input != nil {
		fmt.Println((*input).Name())
	}
}
