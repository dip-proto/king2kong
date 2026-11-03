package dbmigrate

import (
	"fmt"

	"fanout/cli"
)

type Cmd struct {
	Steps int
}

func (c *Cmd) Run() error {
	fmt.Printf("db migrate steps=%d\n", c.Steps)
	return nil
}

func New(parent cli.Registerer) *Cmd {
	c := &Cmd{}
	clause := parent.Command("migrate", "Run migrations")
	clause.Flag("steps", "How many steps").Default("1").IntVar(&c.Steps)
	return c
}
