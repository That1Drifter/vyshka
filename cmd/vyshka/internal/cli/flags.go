package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"
)

// globals are the flags every command accepts, before the command name or
// after it.
type globals struct {
	url, token, profile, config          string
	urlSet, tokenSet, profileSet, cfgSet bool
	json                                 bool
	httpTimeout                          time.Duration
}

// bind registers the global flags on fs and returns the function that folds
// the ones given on this command line into g. Each flag set gets fresh
// variables with neutral defaults rather than pointers into g: the flag
// package prints a flag's default in its usage text, so a default carrying
// the current token would print it, and registering g's own fields would
// reset them to the default on every new flag set.
func (g *globals) bind(fs *flag.FlagSet) func() {
	url := fs.String("url", "", "hub base URL (env VYSHKA_URL)")
	token := fs.String("token", "", "Admin API token, or file:/path (env VYSHKA_TOKEN)")
	profile := fs.String("profile", "", "config file profile (env VYSHKA_PROFILE)")
	config := fs.String("config", "", "config file path (env VYSHKA_CONFIG)")
	jsonOut := fs.Bool("json", false, "print the hub's JSON, one object per line")
	timeout := fs.Duration("http-timeout", defaultHTTPTimeout, "timeout for each HTTP request")
	return func() {
		fs.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "url":
				g.url, g.urlSet = *url, true
			case "token":
				g.token, g.tokenSet = *token, true
			case "profile":
				g.profile, g.profileSet = *profile, true
			case "config":
				g.config, g.cfgSet = *config, true
			case "json":
				g.json = *jsonOut
			case "http-timeout":
				g.httpTimeout = *timeout
			}
		})
	}
}

// newFlagSet returns a flag set that reports nothing itself: parse errors
// come back to the caller, which prints them once in the command's own
// voice, and -h prints the command's help on stdout.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	return fs
}

// flagSet returns a command's flag set with the global flags bound.
func (e *env) flagSet(name string) (*flag.FlagSet, func()) {
	fs := newFlagSet(name)
	return fs, e.g.bind(fs)
}

// parse parses a command's arguments, flags anywhere among the positionals,
// and folds the global flags in. topic names the help page -h shows.
func (e *env) parse(fs *flag.FlagSet, apply func(), args []string, topic string) ([]string, error) {
	positionals, err := parseFlags(fs, args, true)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, &helpRequest{command: topic}
		}
		return nil, usagef("%s; run \"vyshka help %s\" for usage", e.flagMessage(err), topic)
	}
	apply()
	return positionals, nil
}

// flagMessage words a flag error for stderr. With a token anywhere on the
// command line it carries no value at all, since the value that failed could
// be the token itself (`--http-timeout --token=...`) and no formatting of it
// is safe to echo; otherwise it reads as the flag package's would, value and
// all, which is what a typo needs.
func (e *env) flagMessage(err error) string {
	var failed *flagError
	if e.secret && errors.As(err, &failed) {
		return failed.redacted()
	}
	return err.Error()
}

// flagError is what went wrong with one flag: which flag, what kind of
// problem, and for a value the flag refused, the value and the flag's own
// complaint. The parsing is done by hand (parseFlags) rather than by the
// flag package precisely so that this is known as data, not recovered from
// prose that echoes the values.
type flagError struct {
	kind    flagFault
	arg     string // the argument as typed, for a syntax fault
	name    string // the flag's name, for the other faults
	boolean bool
	value   string
	err     error
}

type flagFault int

const (
	faultSyntax  flagFault = iota // dashes with no name, or a name starting with - or =
	faultUnknown                  // no such flag
	faultMissing                  // a flag that takes a value, given last
	faultValue                    // the flag refused its value
)

// Error reads as the flag package's messages do, values included.
func (e *flagError) Error() string {
	switch e.kind {
	case faultSyntax:
		return "bad flag syntax: " + e.arg
	case faultUnknown:
		return "flag provided but not defined: -" + e.name
	case faultMissing:
		return "flag needs an argument: -" + e.name
	case faultValue:
		if e.boolean {
			return fmt.Sprintf("invalid boolean value %q for -%s: %v", e.value, e.name, e.err)
		}
	}
	return fmt.Sprintf("invalid value %q for flag -%s: %v", e.value, e.name, e.err)
}

// redacted is the same message with every value left out, the flag's own
// complaint included, since that quotes the value in a spelling of its own.
func (e *flagError) redacted() string {
	switch e.kind {
	case faultSyntax:
		arg := e.arg
		if name, _, inline := strings.Cut(arg, "="); inline {
			arg = name + "=[redacted]"
		}
		return "bad flag syntax: " + arg
	case faultUnknown, faultMissing:
		return e.Error()
	case faultValue:
		if e.boolean {
			return "invalid boolean value [redacted] for -" + e.name
		}
	}
	return "invalid value [redacted] for flag -" + e.name
}

// argsCarryToken reports whether a token was given on the command line, in
// any spelling of the flag (`--token X`, `--token=X`, or one with a slip
// like `---token=X`).
func argsCarryToken(args []string) bool {
	for _, arg := range args {
		if !isFlag(arg) {
			continue
		}
		name, _, _ := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if name == "token" {
			return true
		}
	}
	return false
}

// leadingGlobals splits the global flags that precede a subcommand name
// (`vyshka kv --json get ns key`) from the rest, so they can be handed to the
// subcommand's own parse and read there: the global flags are accepted
// before the command and after it, and between a command and its subcommand
// is after it. rest starts at the subcommand name, or is nil when only flags
// were given.
func leadingGlobals(args []string) (flags, rest []string) {
	probe := newFlagSet("probe")
	(&globals{}).bind(probe)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !isFlag(arg) || arg == "--" {
			return flags, args[i:]
		}
		flags = append(flags, arg)
		name := strings.TrimLeft(arg, "-")
		if strings.Contains(name, "=") {
			continue
		}
		if defined := probe.Lookup(name); defined != nil && !isBoolFlag(defined) && i+1 < len(args) {
			flags = append(flags, args[i+1])
			i++
		}
	}
	return flags, nil
}

// parseInterspersed reads a command's arguments with flags anywhere among
// the positionals (`vyshka run SERVER CODE amount=5 --wait`).
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	return parseFlags(fs, args, true)
}

// parseFlags reads args into fs by hand, following the flag package's own
// rules (one or two dashes, `--name=value` or a value in the next argument,
// a boolean flag standing alone, `-h` or `-help` when no such flag exists
// asking for help) so that what failed is known exactly: the flag package's
// errors are prose that echoes values, and a value can be a token. With
// interspersed, flags may sit anywhere among the positionals; otherwise the
// first positional ends the flags and is returned with everything after it.
// `--` ends flag handling either way. A lone `-` and a negative number
// (`kv set ns key -5`) are positionals, since no flag is named like either.
func parseFlags(fs *flag.FlagSet, args []string, interspersed bool) ([]string, error) {
	var positionals []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return append(positionals, args[i+1:]...), nil
		}
		if !isFlag(arg) {
			if !interspersed {
				return append(positionals, args[i:]...), nil
			}
			positionals = append(positionals, arg)
			continue
		}
		spec := arg[1:]
		if spec[0] == '-' {
			spec = spec[1:]
		}
		if spec == "" || spec[0] == '-' || spec[0] == '=' {
			return nil, &flagError{kind: faultSyntax, arg: arg}
		}
		name, value, inline := strings.Cut(spec, "=")
		defined := fs.Lookup(name)
		if defined == nil {
			if name == "h" || name == "help" {
				return nil, flag.ErrHelp
			}
			return nil, &flagError{kind: faultUnknown, name: name}
		}
		boolean := isBoolFlag(defined)
		switch {
		case boolean && !inline:
			value = "true"
		case !boolean && !inline:
			if i+1 >= len(args) {
				return nil, &flagError{kind: faultMissing, name: name}
			}
			value = args[i+1]
			i++
		}
		if err := fs.Set(name, value); err != nil {
			return nil, &flagError{kind: faultValue, name: name, boolean: boolean, value: value, err: err}
		}
	}
	return positionals, nil
}

func isFlag(arg string) bool {
	if len(arg) < 2 || arg[0] != '-' {
		return false
	}
	// A negative number is a value, never a flag.
	if c := arg[1]; c >= '0' && c <= '9' || c == '.' {
		return false
	}
	return true
}

func isBoolFlag(f *flag.Flag) bool {
	boolean, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && boolean.IsBoolFlag()
}

// stringList is a repeatable string flag (--type a --type b).
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(value string) error {
	*l = append(*l, value)
	return nil
}
