package main

import (
	"fmt"
	"os"

	"github.com/fastly/kingpin"

	"execapp/db"
	"execapp/dbmigrate"
	"execapp/serve"
)

func main() {
	a := kingpin.New("app", "Demo app")

	serveC := serve.New(a)
	dbC := db.New(a)
	dbMigrateC := dbmigrate.New(dbC.CmdClause)

	cmd, err := a.Parse(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	switch cmd {
	case "serve":
		err = serveC.Exec(os.Stdin, os.Stdout)
	case "db migrate":
		err = dbMigrateC.Exec(os.Stdin, os.Stdout)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
