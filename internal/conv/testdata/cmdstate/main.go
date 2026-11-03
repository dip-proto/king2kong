// Command cmdstate gives its command structs some state before parsing:
// a writer, a label, and links between commands.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/alecthomas/kingpin/v2"
)

type serveCmd struct {
	out  io.Writer
	port int
}

type statusCmd struct {
	serve   *serveCmd
	verbose bool
}

type pingCmd struct {
	label  string
	status *statusCmd
	count  int `json:"count"`
}

func (s *serveCmd) run(*kingpin.ParseContext) error {
	fmt.Fprintln(s.out, "serving on", s.port)
	return nil
}

func (s *statusCmd) run(*kingpin.ParseContext) error {
	fmt.Println("checking port", s.serve.port, s.verbose)
	return nil
}

func (p *pingCmd) run(*kingpin.ParseContext) error {
	fmt.Println(p.label, p.count, p.status.verbose)
	return nil
}

func main() {
	app := kingpin.New("cmdstate", "Commands with state.")
	serve := &serveCmd{out: os.Stdout, port: 8080}
	status := &statusCmd{serve: serve}
	var ping = &pingCmd{label: "ping:"}
	ping.status = status

	sc := app.Command("serve", "Serve.").Action(serve.run)
	sc.Flag("port", "Port.").IntVar(&serve.port)
	st := app.Command("status", "Status.").Action(status.run)
	st.Flag("verbose", "Verbose.").BoolVar(&status.verbose)
	pc := app.Command("ping", "Ping.").Action(ping.run)
	pc.Flag("count", "Count.").IntVar(&ping.count)

	kingpin.MustParse(app.Parse(os.Args[1:]))
	fmt.Println("after:", serve.port, status.verbose, ping.label)
}
