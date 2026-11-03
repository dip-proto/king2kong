package main

import (
	"fmt"
	"os"

	"github.com/alecthomas/kong"
	"github.com/fastly/kingpin"
)

// optStr is the optional-wrapper idiom: it records whether the flag was set. It
// implements kingpin's Action (Set) for the pre-migration program and kong's
// MapperValue (Decode) for the migrated one, so the same source migrates cleanly.
type optStr struct {
	WasSet bool
	Value  string
}

func (o *optStr) Set(_ *kingpin.ParseElement, _ *kingpin.ParseContext) error {
	o.WasSet = true
	return nil
}

func (o *optStr) Decode(ctx *kong.DecodeContext) error {
	if err := ctx.Scan.PopValueInto("string", &o.Value); err != nil {
		return err
	}
	o.WasSet = true
	return nil
}

// marker records whether a flag was given, for the numeric wrappers below.
type marker struct {
	WasSet bool
}

func (m *marker) Set(_ *kingpin.ParseElement, _ *kingpin.ParseContext) error {
	m.WasSet = true
	return nil
}

type optInt struct {
	marker
	Value int
}

type optInt8 struct {
	marker
	Value int8
}

type optUint64 struct {
	marker
	Value uint64
}

type optFloat32 struct {
	marker
	Value float32
}

type serveCmd struct {
	Port    int
	Host    string
	Label   optStr
	Workers optInt
	Retries optInt8
	Max     optUint64
	Ratio   optFloat32
}

func (c *serveCmd) Run() error {
	label := "<none>"
	if c.Label.WasSet {
		label = c.Label.Value
	}
	fmt.Printf("serve host=%s port=%d label=%s\n", c.Host, c.Port, label)
	fmt.Println(c.Workers, c.Retries, c.Max, c.Ratio)
	return nil
}

type dbMigrateCmd struct {
	Steps int
}

func (c *dbMigrateCmd) Run() error {
	fmt.Printf("db migrate steps=%d\n", c.Steps)
	return nil
}

type greetCmd struct {
	Name string
}

func (c *greetCmd) Run() error {
	fmt.Printf("hello %s\n", c.Name)
	return nil
}

func main() {
	app := kingpin.New("app", "Demo app")

	var serve serveCmd
	serveClause := app.Command("serve", "Run the server")
	serveClause.Flag("port", "Port to listen on").Default("8080").IntVar(&serve.Port)
	serveClause.Flag("host", "Host to bind").Default("localhost").StringVar(&serve.Host)
	serveClause.Flag("label", "Optional label").Action(serve.Label.Set).StringVar(&serve.Label.Value)
	serveClause.Flag("workers", "Workers").Action(serve.Workers.Set).IntVar(&serve.Workers.Value)
	serveClause.Flag("retries", "Retries").Action(serve.Retries.Set).Int8Var(&serve.Retries.Value)
	serveClause.Flag("max", "Maximum").Action(serve.Max.Set).Uint64Var(&serve.Max.Value)
	serveClause.Flag("ratio", "Ratio").Action(serve.Ratio.Set).Float32Var(&serve.Ratio.Value)

	db := app.Command("db", "Database commands")
	var dbMigrate dbMigrateCmd
	dbMigrateClause := db.Command("migrate", "Run migrations")
	dbMigrateClause.Flag("steps", "How many steps").Default("1").IntVar(&dbMigrate.Steps)

	var greet greetCmd
	greetClause := app.Command("greet", "Greet someone")
	greetClause.Arg("name", "Who to greet").Required().StringVar(&greet.Name)

	cmd, err := app.Parse(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_ = serveClause
	_ = dbMigrateClause
	_ = greetClause
	switch cmd {
	case "serve":
		err = serve.Run()
	case "db migrate":
		err = dbMigrate.Run()
	case "greet":
		err = greet.Run()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
