package cmd

import (
	"reflect"
	"sort"
	"testing"

	"github.com/alecthomas/kingpin/v2"
)

// TestGreetFlags reads the flags kingpin registered by reflection, as some
// programs check that composite commands kept the flags of their parts.
func TestGreetFlags(t *testing.T) {
	app := kingpin.New("applayer", "Test.")
	greet := NewGreetCommand(app, &Config{})
	long := reflect.ValueOf(greet.clause).Elem().FieldByName("cmdMixin").FieldByName("flagGroup").Elem().FieldByName("long")
	var names []string
	iter := long.MapRange()
	for iter.Next() {
		names = append(names, iter.Key().String())
	}
	sort.Strings(names)
	if len(names) != 2 || names[0] != "separator" || names[1] != "times" {
		t.Fatalf("flags %q", names)
	}
}
