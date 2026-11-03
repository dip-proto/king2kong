package kongcompat

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/alecthomas/kong"
)

// ErrorReporter prints errors and exits the way kingpin's Application did.
// kong's methods of the same names differ a little: they indent multi-line
// messages and return the parser, and FatalIfErrorf takes the exit status
// from the error and may print the usage.
type ErrorReporter struct {
	k *kong.Kong
}

// Errors returns the error reporter of k.
func Errors(k *kong.Kong) ErrorReporter {
	return ErrorReporter{k}
}

// kingpin's default application took its name and error writer when the
// program started.
var (
	defaultName             = filepath.Base(os.Args[0])
	defaultStderr io.Writer = os.Stderr
)

// Errorf prints "<name>: error: <message>" on the error writer.
func (r ErrorReporter) Errorf(format string, args ...any) {
	name, w := defaultName, defaultStderr
	if r.k != nil {
		name, w = r.k.Model.Name, r.k.Stderr
	}
	fmt.Fprintf(w, name+": error: "+format+"\n", args...)
}

// Fatalf prints an error as Errorf does and exits with status 1.
func (r ErrorReporter) Fatalf(format string, args ...any) {
	r.Errorf(format, args...)
	r.exit(1)
}

// FatalIfError prints err, after the formatted prefix if there is one, and
// exits with status 1, when err is not nil.
func (r ErrorReporter) FatalIfError(err error, format string, args ...any) {
	if err == nil {
		return
	}
	prefix := ""
	if format != "" {
		prefix = fmt.Sprintf(format, args...) + ": "
	}
	r.Errorf(prefix+"%s", err)
	r.exit(1)
}

func (r ErrorReporter) exit(code int) {
	if r.k != nil {
		r.k.Exit(code)
		return
	}
	os.Exit(code)
}

// Errorf is kingpin's package-level Errorf, which printed through its
// default application, named after the program.
func Errorf(format string, args ...any) {
	ErrorReporter{}.Errorf(format, args...)
}

// Fatalf is kingpin's package-level Fatalf.
func Fatalf(format string, args ...any) {
	ErrorReporter{}.Fatalf(format, args...)
}

// FatalIfError is kingpin's package-level FatalIfError.
func FatalIfError(err error, format string, args ...any) {
	ErrorReporter{}.FatalIfError(err, format, args...)
}
