![King2Kong logo](.media/logo.png)

# King2Kong (kingpin-to-kong)

king2kong moves a Go program from [kingpin](https://github.com/alecthomas/kingpin) to [kong](https://github.com/alecthomas/kong).

It handles kingpin v2 (`github.com/alecthomas/kingpin/v2` and the older `gopkg.in/alecthomas/kingpin.v2`), and kingpin v3 snapshots

Kingpin builds command lines with chains of functions calls like `app.Command("x", "...").Flag("y", "...").StringVar(&dst)`. But Kong reads uses struct with tags instead.

So the tool moves what those calls say onto the fields they write to, turn the command tree into nested structs, and deal with everything around the calls: constructors, helpers, option structs, conditions, actions. Automagically.

The result is meant to be readable any maintainable.

This command, written as Kingpin documents it:

```go
type lsCommand struct {
	all    bool
	sortBy string
}

func (l *lsCommand) run(c *kingpin.ParseContext) error {
	fmt.Println(l.all, l.sortBy)
	return nil
}

func configureLsCommand(app *kingpin.Application) {
	c := &lsCommand{}
	ls := app.Command("ls", "List files.").Action(c.run)
	ls.Flag("all", "List all files.").Short('a').BoolVar(&c.all)
	ls.Flag("sort", "Sort order.").Default("name").EnumVar(&c.sortBy, "name", "size")
}
```

becomes:

```go
type lsCommand struct {
	All    bool   `short:"a" negatable:"" help:"List all files."`
	SortBy string `name:"sort" default:"name" enum:"name,size" help:"Sort order."`
}

func (l *lsCommand) run() error {
	fmt.Println(l.All, l.SortBy)
	return nil
}

// AfterApply runs the command, as kingpin ran its Action after parsing.
func (l *lsCommand) AfterApply() error {
	return l.run()
}
```

and `main` builds the parser with `kong.Must(&cli, ...)` from a generated `CLI` struct that holds `Ls lsCommand` as a `cmd:""` field.

## Usage

```sh
king2kong [-dir DIR] [-write] [-v] [-dump] [packages...]
```

Without `-write`, it only reports what it would do.

With `-write`, it rewrites the files, adds the `kongcompat` package to the module (see below), and runs `go get github.com/alecthomas/kong@v1.17.0`.

`-v` lists every item that needs a person, and `-dump` prints the grammar the tool found.

Run it on a clean working tree, then read the diff, build and run your tests.

A repository holding several kingpin programs is converted one program at a time: name that program's packages, as in `./cmd/server/... ./pkg/...`.

What the tool knows it could not convert is in the report, and flags it could not convert keep a `TODO` with the original kingpin call.

## Output

The tool turns your kingpin registration code into kong struct tags.

The converted output is plain kong, readable, writable, and without the tool's own scaffolding.

It handles:

- Flag definitions (all types kingpin supports)
- Command trees with subcommands
- Default values (including computed ones)
- Short flags, enum flags, boolean flags
- `AfterApply` hooks for action code
- Conditional commands (registered under `if` statements)
- Tests that import kingpin

It also generates a small `internal/kongcompat` package that keeps kingpin's user-visible behaviors: help output, error messages, shell completion, `--version`, `@file` arguments, and other quirks users and scripts rely on.

Delete it once your program moves to kong's own help and errors.
