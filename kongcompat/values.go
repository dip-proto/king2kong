package kongcompat

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/alecthomas/kong"
)

// checkValue parses value the way kingpin did, to report a bad one in
// kingpin's words before kong decodes it.
// Other types are left to kong, whose errors valueError cleans up.
func checkValue(v *kong.Value, value string) error {
	if !v.Target.IsValid() || v.IsCounter() {
		return nil
	}
	switch v.Tag.Type {
	case "existingfile", "existingdir":
		return checkPath(value, v.Tag.Type == "existingdir")
	}
	t := v.Target.Type()
	if t.Kind() == reflect.Slice && t.PkgPath() == "" {
		t = t.Elem()
		if t.Kind() == reflect.Bool {
			_, err := strconv.ParseBool(value)
			return err
		}
	}
	switch {
	case t == reflect.TypeOf(net.IP{}):
		if net.ParseIP(value) == nil {
			return fmt.Errorf("'%s' is not an IP address", value)
		}
		return nil
	case t == reflect.TypeOf(&url.URL{}) && version == 2:
		if _, err := url.Parse(value); err != nil {
			return fmt.Errorf("invalid URL: %s", err)
		}
		return nil
	case t == reflect.TypeOf(time.Duration(0)) && !v2From23():
		_, err := time.ParseDuration(value)
		return err
	case t.Kind() == reflect.Map && t.Key().Kind() == reflect.String:
		if !strings.ContainsAny(value, ":=") {
			return fmt.Errorf("expected KEY=VALUE got '%s'", value)
		}
		return nil
	case t.PkgPath() != "":
		// Other named types have mappers of their own.
		return nil
	}
	var err error
	switch t.Kind() {
	case reflect.Int:
		// kingpin parsed an int as a float and truncated it.
		_, err = strconv.ParseFloat(value, 64)
	case reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		_, err = strconv.ParseInt(value, 0, t.Bits())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		_, err = strconv.ParseUint(value, 0, t.Bits())
	case reflect.Float32, reflect.Float64:
		_, err = strconv.ParseFloat(value, t.Bits())
	}
	return err
}

// Values decodes values the way kingpin did where kong differs:
//
//   - an int is parsed as a float and truncated, so 1.5 means 1;
//   - a []bool takes a value each time, true or false;
//   - a map[string]string entry is KEY=VALUE or KEY:VALUE, split at the
//     first separator;
//   - an existingfile or existingdir value is kept as given, not made
//     absolute;
//   - a noflag value is a boolean flag named no-x, which kingpin v3 set to
//     false when it was given.
func Values() kong.Option {
	return kong.OptionFunc(func(k *kong.Kong) error {
		for _, opt := range []kong.Option{
			kong.TypeMapper(reflect.TypeOf(0), kong.MapperFunc(floatInt)),
			kong.TypeMapper(reflect.TypeOf([]bool{}), kong.MapperFunc(boolList)),
			kong.TypeMapper(reflect.TypeOf(map[string]string{}), kong.MapperFunc(stringMap)),
			kong.NamedMapper("existingfile", existingPath(false)),
			kong.NamedMapper("existingdir", existingPath(true)),
			kong.NamedMapper("noflag", noFlag{}),
		} {
			if err := opt.Apply(k); err != nil {
				return err
			}
		}
		return nil
	})
}

func floatInt(ctx *kong.DecodeContext, target reflect.Value) error {
	var s string
	if err := ctx.Scan.PopValueInto("int", &s); err != nil {
		return err
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return err
	}
	target.SetInt(int64(f))
	return nil
}

// valueTexts pops the raw text of a value: a flag's value split at sep, or
// all the remaining arguments for a positional one.
func valueTexts(ctx *kong.DecodeContext, sep rune) ([]string, error) {
	if ctx.Value.Flag == nil {
		var out []string
		for _, t := range ctx.Scan.PopWhile(func(t kong.Token) bool { return t.IsValue() }) {
			out = append(out, fmt.Sprint(t.Value))
		}
		return out, nil
	}
	t := ctx.Scan.Pop()
	if t.IsEOL() {
		return nil, fmt.Errorf("missing value")
	}
	return kong.SplitEscaped(fmt.Sprint(t.Value), sep), nil
}

func boolList(ctx *kong.DecodeContext, target reflect.Value) error {
	texts, err := valueTexts(ctx, ctx.Value.Tag.Sep)
	if err != nil {
		return err
	}
	for _, s := range texts {
		b, err := strconv.ParseBool(s)
		if err != nil {
			return err
		}
		target.Set(reflect.Append(target, reflect.ValueOf(b)))
	}
	return nil
}

func stringMap(ctx *kong.DecodeContext, target reflect.Value) error {
	texts, err := valueTexts(ctx, ctx.Value.Tag.MapSep)
	if err != nil {
		return err
	}
	if target.IsNil() {
		target.Set(reflect.MakeMap(target.Type()))
	}
	for _, s := range texts {
		i := strings.IndexAny(s, ":=")
		if i < 0 {
			return fmt.Errorf("expected KEY=VALUE got '%s'", s)
		}
		target.SetMapIndex(reflect.ValueOf(s[:i]), reflect.ValueOf(s[i+1:]))
	}
	return nil
}

func existingPath(dir bool) kong.MapperFunc {
	return func(ctx *kong.DecodeContext, target reflect.Value) error {
		sep := ctx.Value.Tag.Sep
		if target.Kind() != reflect.Slice {
			sep = -1
		}
		texts, err := valueTexts(ctx, sep)
		if err != nil {
			return err
		}
		for _, s := range texts {
			// kong also decodes the defaults of unselected commands, which
			// kingpin never checked.
			if ctx.Value.Active {
				if err := checkPath(s, dir); err != nil {
					return err
				}
			}
			if target.Kind() == reflect.Slice {
				target.Set(reflect.Append(target, reflect.ValueOf(s)))
			} else {
				target.SetString(s)
			}
		}
		return nil
	}
}

// checkPath checks an existing file or directory as kingpin did.
func checkPath(path string, dir bool) error {
	st, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("path '%s' does not exist", path)
	case err != nil:
		return err
	case !dir && st.IsDir():
		return fmt.Errorf("'%s' is a directory", st.Name())
	case dir && !st.IsDir():
		return fmt.Errorf("'%s' is a file", st.Name())
	}
	return nil
}

// noFlag decodes a kingpin v3 boolean flag named no-x, which is false when
// given without a value.
type noFlag struct{}

func (noFlag) Decode(ctx *kong.DecodeContext, target reflect.Value) error {
	if ctx.Scan.Peek().Type != kong.FlagValueToken {
		target.SetBool(false)
		return nil
	}
	switch v := ctx.Scan.Pop().Value.(type) {
	case bool:
		target.SetBool(v)
	default:
		b, err := strconv.ParseBool(fmt.Sprint(v))
		if err != nil {
			return fmt.Errorf("expected a boolean but got %q", v)
		}
		target.SetBool(b)
	}
	return nil
}

func (noFlag) IsBool() bool { return true }

// valueError strips the flag or argument name kong puts in front of a
// value's own parse error, since kingpin reported that error as is.
func valueError(k *kong.Kong, err error) error {
	names := func(prefix string) bool {
		found := false
		values(k.Model.Node, func(v *kong.Value) { found = found || v.ShortSummary() == prefix })
		return found
	}
	for {
		inner := errors.Unwrap(err)
		if inner == nil {
			return err
		}
		// Only drop the prefix that names a flag or argument.
		// Errors from strconv, net/url or the program wrap their own cause
		// and quote what failed, and kingpin showed them whole.
		msg := err.Error()
		prefix, prefixed := strings.CutSuffix(msg, ": "+inner.Error())
		if msg != inner.Error() && !(prefixed && names(prefix)) {
			return err
		}
		err = inner
	}
}

// newOptions are extra options New gives every parser.
// Files copied only for some kingpin versions, such as durations_v2.go, add
// to it.
var newOptions []kong.Option
