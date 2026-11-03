package kongcompat

// version is the kingpin major version whose behavior the package copies:
// 3 for kingpin v3 and its forks, 2 for kingpin v2.
// kingpin2kong sets it when it adds the package to a program.
var version = 3

// legacy is set for kingpin v2 releases before v2.3.0, such as those of
// gopkg.in/alecthomas/kingpin.v2, whose help was closer to kingpin v3's.
var legacy = false

// v2From23 reports whether the program came from kingpin v2.3.0 or later,
// whose help shows --[no-] forms, environment variables and argument
// placeholders, and which names every missing required flag.
func v2From23() bool {
	return version == 2 && !legacy
}

// Action is the type of kingpin's callbacks, for code that kept values of
// it.
type Action func(element *ParseElement, context *ParseContext) error
