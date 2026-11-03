package main

import (
	"fmt"
	"os"

	"github.com/fastly/kingpin"

	"fanout/db"
	"fanout/dbmigrate"
	"fanout/serve"
)

func main() {
	app := kingpin.New("app", "Demo app")

	serveC := serve.New(app)
	dbC := db.New(app)
	dbMigrateC := dbmigrate.New(dbC.CmdClause)

	cmd, err := app.Parse(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	switch cmd {
	case "serve":
		err = serveC.Run()
	case "db migrate":
		err = dbMigrateC.Run()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
