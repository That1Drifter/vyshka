package cli

import (
	"errors"
	"flag"
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
	positionals, err := parseInterspersed(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, &helpRequest{command: topic}
		}
		return nil, usagef("%s; run \"vyshka help %s\" for usage", redactSecrets(err.Error(), args), topic)
	}
	apply()
	return positionals, nil
}

// redactSecrets rewrites a flag parser's error message so that no argument
// value appears in it when the command line carries a token. The parser
// echoes the offending argument in some of its errors (`bad flag syntax:
// ---token=...`, `invalid value "..." for flag -x: ...`), in spellings that
// depend on the formatter (quoted, escaped, or byte-escaped), so the values
// are dropped rather than searched for: the flag's name is what the user
// needs to fix the line. A command line with no token on it keeps the
// parser's message whole, values and all, since nothing on it is secret.
func redactSecrets(message string, args []string) string {
	if !argsCarryToken(args) {
		return message
	}
	const (
		badSyntax    = "bad flag syntax: "
		invalidValue = "invalid value "
		invalidBool  = "invalid boolean value "
	)
	switch {
	case strings.HasPrefix(message, badSyntax):
		arg := strings.TrimPrefix(message, badSyntax)
		if name, _, inline := strings.Cut(arg, "="); inline {
			arg = name + "=[redacted]"
		}
		return badSyntax + arg
	case strings.HasPrefix(message, invalidValue) || strings.HasPrefix(message, invalidBool):
		// `invalid value %q for flag -%s: %v` and its boolean twin; the flag
		// name runs to the colon that starts the underlying error.
		for _, marker := range []string{" for flag -", " for -"} {
			if at := strings.LastIndex(message, marker); at >= 0 {
				name, _, _ := strings.Cut(message[at+len(marker):], ":")
				return "invalid value [redacted] for flag -" + name
			}
		}
		return "invalid flag value [redacted]"
	}
	// The other messages name a flag, never a value.
	return message
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

// parseInterspersed lets flags follow positionals (`vyshka run SERVER CODE
// amount=5 --wait`), which the flag package alone does not: it stops at the
// first non-flag. It walks args, hands every flag (with its value, when the
// flag takes one and no = carries it) to fs, and keeps the rest as
// positionals. `--` ends flag handling. A lone `-` and a negative number
// (`kv set ns key -5`) are positionals, since no flag is named like either.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var flags, positionals []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positionals = append(positionals, args[i+1:]...)
			break
		}
		if !isFlag(arg) {
			positionals = append(positionals, arg)
			continue
		}
		flags = append(flags, arg)
		name := strings.TrimLeft(arg, "-")
		if strings.Contains(name, "=") {
			continue
		}
		defined := fs.Lookup(name)
		if defined == nil || isBoolFlag(defined) {
			// An unknown flag is left for fs.Parse to refuse by name.
			continue
		}
		if i+1 < len(args) {
			flags = append(flags, args[i+1])
			i++
		}
	}
	if err := fs.Parse(flags); err != nil {
		return nil, err
	}
	// Everything handed over was a flag or a flag's value, so anything left
	// is a value the flag package did not consume: say so rather than lose it.
	if extra := fs.Args(); len(extra) > 0 {
		return nil, errors.New("unexpected argument " + extra[0])
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
