package cmd_test

import (
	"strings"
	"testing"

	"github.com/alecthomas/kingpin/v2"

	"applayer/cmd"
)

func TestDefine(t *testing.T) {
	app := kingpin.New("applayer", "Test.")
	commands := cmd.Define(app, &cmd.Config{Greeting: "hi"})
	if _, err := app.Parse([]string{"greet", "bob", "--times", "2"}); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range commands {
		names = append(names, c.Name())
	}
	// kingpin v3 names commands with their arguments: "greet <who>".
	if len(names) != 3 || names[0] != "list" || names[1] != "admin purge" || !strings.HasPrefix(names[2], "greet") {
		t.Fatalf("commands %q", names)
	}
}

func TestConstructor(t *testing.T) {
	app := kingpin.New("applayer", "Test.")
	greet := cmd.NewGreetCommand(app, &cmd.Config{Greeting: "hi"})
	if _, err := app.Parse([]string{"greet", "bob", "--times", "3"}); err != nil {
		t.Fatal(err)
	}
	if err := greet.Run(); err != nil {
		t.Fatal(err)
	}
}
