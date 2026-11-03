// Command exporter follows the Prometheus exporters: packages declare their
// flags in package-level variables of kingpin.CommandLine, and main parses.
package main

import (
	"fmt"
	"os"

	"github.com/alecthomas/kingpin/v2"

	"exporter/collector"
)

func main() {
	listen := kingpin.Flag("web.listen-address", "Address to listen on.").Default(":9100").String()
	kingpin.Version("1.0.0")
	kingpin.CommandLine.UsageWriter(os.Stdout)
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()
	fmt.Println("listen:", *listen)
	collector.Describe()
}
