// Command conditional registers a command and a flag only when an
// environment variable is set.
package main

import (
	"fmt"
	"os"

	"github.com/alecthomas/kingpin/v2"
)

func main() {
	app := kingpin.New("cond", "Conditional commands.")
	debug := app.Flag("debug", "Debug.").Bool()
	app.Command("run", "Run.")
	if os.Getenv("ENABLE_SECRET") != "" {
		app.Command("secret", "Secret command.")
		app.Flag("token", "Token.").String()
	}
	cmd := kingpin.MustParse(app.Parse(os.Args[1:]))
	fmt.Println(cmd, *debug)
}
