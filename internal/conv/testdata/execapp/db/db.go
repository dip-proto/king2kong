package db

import "execapp/app"

type Cmd struct {
	app.Base
}

func New(parent app.Registerer) *Cmd {
	c := &Cmd{}
	c.CmdClause = parent.Command("db", "Database commands")
	return c
}
