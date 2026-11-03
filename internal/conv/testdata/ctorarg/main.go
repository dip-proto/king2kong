// Command ctorarg builds its command with a constructor that stores a
// constant and a value of main, and discards the result.
package main

import (
	"fmt"
	"os"

	"github.com/alecthomas/kingpin/v2"
)

type settings struct{ endpoint string }

type job struct {
	label    string
	cfg      *settings
	parallel int
}

func (j *job) run(*kingpin.ParseContext) error {
	fmt.Println("job", j.label, j.cfg.endpoint, j.parallel)
	return nil
}

func newJob(app *kingpin.Application, name, label string, cfg *settings) *job {
	j := &job{label: label, cfg: cfg}
	cmd := app.Command(name, "Run a job.").Action(j.run)
	cmd.Flag("parallel", "Parallelism.").Default("1").IntVar(&j.parallel)
	return j
}

func main() {
	cfg := &settings{endpoint: "https://example.com"}
	app := kingpin.New("ctorarg", "Constructor arguments.")
	newJob(app, "build", "production", cfg)
	kingpin.MustParse(app.Parse(os.Args[1:]))
}
