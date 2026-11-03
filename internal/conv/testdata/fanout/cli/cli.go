package cli

import "github.com/fastly/kingpin"

// Registerer is satisfied by both *kingpin.Application and *kingpin.CmdClause, so
// a command can be registered under either the app or another command.
type Registerer interface {
	Command(name, help string) *kingpin.CmdClause
}
