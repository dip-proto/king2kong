package serve

import (
	"fmt"
	"io"

	"execapp/app"
)

type Cmd struct {
	app.Base
	Port int
}

func (c *Cmd) Exec(_ io.Reader, out io.Writer) error {
	fmt.Fprintf(out, "serve port=%d\n", c.Port)
	return nil
}

func New(parent app.Registerer) *Cmd {
	c := &Cmd{}
	c.CmdClause = parent.Command("serve", "Run the server")
	c.CmdClause.Flag("port", "Port to listen on").Default("8080").IntVar(&c.Port)
	return c
}
