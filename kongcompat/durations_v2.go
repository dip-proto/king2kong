package kongcompat

import (
	"reflect"
	"time"

	"github.com/alecthomas/kong"
	"github.com/xhit/go-str2duration/v2"
)

// king2kong only copies this file into programs converted from kingpin
// v2.3.0 or later, the only ones that need str2duration.

func init() {
	newOptions = append(newOptions, Durations())
	parseDuration = str2duration.ParseDuration
}

// Durations decodes durations the way kingpin v2.3.0 and later did.
// It also accepts days and weeks, as in 2d or 1w.
func Durations() kong.Option {
	return kong.TypeMapper(reflect.TypeOf(time.Duration(0)), kong.MapperFunc(func(ctx *kong.DecodeContext, target reflect.Value) error {
		var s string
		if err := ctx.Scan.PopValueInto("duration", &s); err != nil {
			return err
		}
		d, err := str2duration.ParseDuration(s)
		if err != nil {
			return err
		}
		target.SetInt(int64(d))
		return nil
	}))
}
