// Command pkgstate keeps its application and command structs in
// package-level variables, some with initial values.
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/alecthomas/kingpin/v2"
)

type dbCmd struct {
	dsn string
}

type migrateCmd struct {
	label string
	steps int
}

type listCmd struct {
	sep string
	all bool
}

var (
	app     = kingpin.New("pkgstate", "Package-level commands.")
	db      = &dbCmd{dsn: "local"}
	migrate = &migrateCmd{label: strings.ToUpper("migrating")}
	list    = new(listCmd)
)

func main() {
	list.sep = ", "
	d := app.Command("db", "Database.")
	d.Flag("dsn", "DSN.").StringVar(&db.dsn)
	m := d.Command("migrate", "Migrate.")
	m.Flag("steps", "Steps.").IntVar(&migrate.steps)
	l := app.Command("list", "List.")
	l.Flag("all", "All.").BoolVar(&list.all)
	switch kingpin.MustParse(app.Parse(os.Args[1:])) {
	case m.FullCommand():
		fmt.Println(migrate.label, migrate.steps, db.dsn)
	case l.FullCommand():
		fmt.Println(strings.Join([]string{"a", "b"}, list.sep), list.all, db.dsn)
	}
}
