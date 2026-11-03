package serve

import (
	"fmt"

	"fanout/cli"
)

type Cmd struct {
	Port int
}

func (c *Cmd) Run() error {
	fmt.Printf("serve port=%d\n", c.Port)
	return nil
}

func New(parent cli.Registerer) *Cmd {
	c := &Cmd{}
	clause := parent.Command("serve", "Run the server")
	clause.Flag("port", "Port to listen on").Default("8080").IntVar(&c.Port)
	return c
}
