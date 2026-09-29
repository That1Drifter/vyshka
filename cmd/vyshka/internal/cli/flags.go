package cli

import (
	"errors"
	"flag"
	"io"
	"strconv"
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

// redactSecrets removes token values from a flag parser's error message. The
// parser echoes the offending argument in some of its errors (`bad flag
// syntax: ---token=...`), and a token typed with a slip of the hand must not
// reach stderr that way: neutral flag defaults keep it out of help, this
// keeps it out of the errors.
func redactSecrets(message string, args []string) string {
	for i, arg := range args {
		if !isFlag(arg) {
			continue
		}
		name, value, inline := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if name != "token" {
			continue
		}
		if !inline && i+1 < len(args) {
			value = args[i+1]
		}
		if value == "" {
			continue
		}
		message = strings.ReplaceAll(message, value, "[redacted]")
		// Some errors quote the argument (`invalid value %q`), which
		// escapes a backslash or a quote in it: that spelling goes too.
		if quoted := strconv.Quote(value); quoted != `"`+value+`"` {
			message = strings.ReplaceAll(message, quoted[1:len(quoted)-1], "[redacted]")
		}
	}
	return message
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
