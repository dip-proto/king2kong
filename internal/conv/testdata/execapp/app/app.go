package app

import "github.com/fastly/kingpin"

type Registerer interface {
	Command(name, help string) *kingpin.CmdClause
}

// Base is a framework base embedded in every command; its exported CmdClause is
// a kingpin type kong must be told to ignore.
type Base struct {
	CmdClause *kingpin.CmdClause
}
