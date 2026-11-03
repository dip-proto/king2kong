package dbmigrate

import (
	"fmt"
	"io"

	"execapp/app"
)

type Cmd struct {
	app.Base
	Steps int
}

func (c *Cmd) Exec(_ io.Reader, out io.Writer) error {
	fmt.Fprintf(out, "db migrate steps=%d\n", c.Steps)
	return nil
}

func New(parent app.Registerer) *Cmd {
	c := &Cmd{}
	c.CmdClause = parent.Command("migrate", "Run migrations")
	c.CmdClause.Flag("steps", "How many steps").Default("1").IntVar(&c.Steps)
	return c
}
