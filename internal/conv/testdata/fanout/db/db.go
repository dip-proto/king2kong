package db

import (
	"github.com/fastly/kingpin"

	"fanout/cli"
)

type Cmd struct {
	CmdClause *kingpin.CmdClause
}

func New(parent cli.Registerer) *Cmd {
	c := &Cmd{}
	c.CmdClause = parent.Command("db", "Database commands")
	return c
}
