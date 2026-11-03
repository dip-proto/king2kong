// Package collector gathers metrics, configured by flags it declares itself.
package collector

import (
	"fmt"

	"github.com/alecthomas/kingpin/v2"
)

var (
	textfileDir = kingpin.Flag("collector.textfile.directory", "Directory to read text files from.").Default("/var/lib/textfile").String()
	enableCPU   = kingpin.Flag("collector.cpu", "Enable the cpu collector.").Default("true").Bool()
)

// Describe prints the collector configuration.
func Describe() {
	fmt.Println("textfile:", *textfileDir, "cpu:", *enableCPU)
}
