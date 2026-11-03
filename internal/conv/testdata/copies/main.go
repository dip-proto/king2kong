// Command copies builds its flag names from structs that Go copies, by
// assignment, range, parameter or receiver, and registers some flags only
// after a branch that returns.
package main

import (
	"fmt"
	"os"

	"github.com/alecthomas/kingpin/v2"
)

type names struct{ Name string }

type spec struct {
	Names names
	Help  string
}

type options struct {
	region, zone, tier, tierName, regionCopy, apiRegion, token string
	remote                                                     bool
}

func (s spec) prefixed(app *kingpin.Application, prefix string, dst *string) {
	s.Names.Name = prefix + s.Names.Name
	app.Flag(s.Names.Name, s.Help).StringVar(dst)
}

func withSuffix(app *kingpin.Application, s spec, dst *string) {
	s.Names.Name += "-copy"
	app.Flag(s.Names.Name, s.Help).StringVar(dst)
}

func remote(app *kingpin.Application, o *options) {
	if os.Getenv("COPIES_REMOTE") != "" {
		app.Flag("remote", "Remote host.").BoolVar(&o.remote)
	} else {
		return
	}
	app.Flag("token", "Remote token.").StringVar(&o.token)
}

func main() {
	var o options
	app := kingpin.New("copies", "Struct copies.")
	base := spec{Names: names{Name: "region"}, Help: "Region."}
	extra := base
	extra.Names.Name = "zone"
	var other = base
	other.Help = "Other region."
	specs := []spec{{Names: names{Name: "tier"}, Help: "Tier."}}
	for _, s := range specs {
		s.Names.Name += "-name"
		app.Flag(s.Names.Name, s.Help).StringVar(&o.tierName)
	}
	app.Flag(base.Names.Name, base.Help).StringVar(&o.region)
	app.Flag(extra.Names.Name, extra.Help).StringVar(&o.zone)
	app.Flag(specs[0].Names.Name, other.Help).StringVar(&o.tier)
	withSuffix(app, base, &o.regionCopy)
	base.prefixed(app, "api-", &o.apiRegion)
	remote(app, &o)
	if _, err := app.Parse(os.Args[1:]); err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println(o.region, o.zone, o.tier, o.tierName, o.regionCopy, o.apiRegion, o.token, o.remote)
}
