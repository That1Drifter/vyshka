// Package cli implements the vyshka command, an operator's terminal client
// for a hub's Admin API. It speaks only the public API, through package
// client, never the hub's internals: anything it does, a third-party tool
// can do too.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/That1Drifter/vyshka/client"
)

// Exit codes. Scripts branch on them, so they are part of the command's
// interface: changing one is a breaking change.
const (
	// ExitOK is success; for run --wait and job --wait, the action completed.
	ExitOK = 0
	// ExitUsage is a usage or local error: bad arguments, a config problem, a
	// local schema violation, no or an ambiguous server or player match, or a
	// confirmation declined or impossible to ask.
	ExitUsage = 1
	// ExitActionFailed is an action that reached the failed state.
	ExitActionFailed = 2
	// ExitActionExpired is an action that reached the expired state.
	ExitActionExpired = 3
	// ExitRefused is any refusal from the hub: an answer carrying an error
	// code (unauthorized, forbidden, not_found, params_invalid, ...).
	ExitRefused = 4
	// ExitTransport is a hub that could not be reached, or answered something
	// unreadable.
	ExitTransport = 5
	// ExitWaitTimeout is --wait running out of time with the action still in
	// flight.
	ExitWaitTimeout = 6
)

// IO is everything the command touches outside itself, so tests can run it
// in-process.
type IO struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	Getenv         func(string) string
	// StdinIsTerminal says whether a person could answer a prompt on Stdin.
	// Without one, a destructive action needs --yes.
	StdinIsTerminal bool
	// ConfigDir locates the user's configuration directory; main passes
	// os.UserConfigDir.
	ConfigDir func() (string, error)
	// Context bounds the whole run: main passes one cancelled by SIGINT, which
	// is what ends events --follow. Nil means context.Background().
	Context context.Context
}

// Run executes one command line (without the program name) and returns the
// exit code.
func Run(args []string, stdio IO) int {
	e := newEnv(stdio)
	return e.report(e.dispatch(args))
}

// env is one run's state: its streams, its global flags, and the client,
// built only when a command needs the hub.
type env struct {
	stdio  IO
	ctx    context.Context
	stdout *errWriter
	stderr io.Writer
	g      globals
	hub    *client.Client
}

func newEnv(stdio IO) *env {
	if stdio.Stdin == nil {
		stdio.Stdin = strings.NewReader("")
	}
	if stdio.Stdout == nil {
		stdio.Stdout = io.Discard
	}
	if stdio.Stderr == nil {
		stdio.Stderr = io.Discard
	}
	if stdio.Getenv == nil {
		stdio.Getenv = os.Getenv
	}
	if stdio.ConfigDir == nil {
		stdio.ConfigDir = os.UserConfigDir
	}
	ctx := stdio.Context
	if ctx == nil {
		ctx = context.Background()
	}
	return &env{
		stdio:  stdio,
		ctx:    ctx,
		stdout: &errWriter{w: stdio.Stdout},
		stderr: stdio.Stderr,
		g:      globals{httpTimeout: defaultHTTPTimeout},
	}
}

// commands maps each command name to its implementation. help is handled in
// dispatch, since it needs the table itself.
var commands = map[string]func(e *env, args []string) error{
	"version":  cmdVersion,
	"health":   cmdHealth,
	"servers":  cmdServers,
	"actions":  cmdActions,
	"run":      cmdRun,
	"job":      cmdJob,
	"state":    cmdState,
	"events":   cmdEvents,
	"kv":       cmdKV,
	"player":   cmdPlayer,
	"contexts": cmdContexts,
}

func (e *env) dispatch(args []string) error {
	// Global flags before the command. The standard parse stops at the first
	// non-flag, which is the command name.
	fs := newFlagSet("vyshka")
	apply := e.g.bind(fs)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return &helpRequest{}
		}
		return usagef("%v; run \"vyshka help\" for usage", err)
	}
	apply()

	rest := fs.Args()
	if len(rest) == 0 {
		writeHelp(e.stderr, "")
		return usagef("a command is required")
	}
	name, commandArgs := rest[0], rest[1:]
	if name == "help" {
		if len(commandArgs) > 1 {
			return usagef("help takes at most one command name")
		}
		topic := ""
		if len(commandArgs) == 1 {
			topic = commandArgs[0]
			if _, known := helpTexts[topic]; !known {
				return usagef("no help for %q; run \"vyshka help\" for the commands", topic)
			}
		}
		return &helpRequest{command: topic}
	}
	command, ok := commands[name]
	if !ok {
		return usagef("unknown command %q; run \"vyshka help\" for the commands", name)
	}
	return command(e, commandArgs)
}

// usageError is a problem with what was asked, answered locally.
type usageError struct{ message string }

func (e *usageError) Error() string { return e.message }

func usagef(format string, args ...any) error {
	return &usageError{message: fmt.Sprintf(format, args...)}
}

// exitError ends a run with a specific code and message: the action outcomes
// and the wait deadline.
type exitError struct {
	code    int
	message string
}

func (e *exitError) Error() string { return e.message }

// helpRequest asks report to print help and succeed.
type helpRequest struct{ command string }

func (h *helpRequest) Error() string { return "help requested" }

// report prints an error the way its kind deserves and picks the exit code.
func (e *env) report(err error) int {
	if err == nil {
		return ExitOK
	}

	var help *helpRequest
	var exit *exitError
	var usage *usageError
	var refusal *client.Error
	var transport *client.TransportError
	switch {
	case errors.As(err, &help):
		writeHelp(e.stdout, help.command)
		return ExitOK
	case errors.As(err, &exit):
		if exit.message != "" {
			fmt.Fprintln(e.stderr, "vyshka: "+exit.message)
		}
		return exit.code
	case errors.As(err, &usage):
		fmt.Fprintln(e.stderr, "vyshka: "+usage.message)
		return ExitUsage
	case errors.As(err, &refusal):
		e.printRefusal(refusal)
		return ExitRefused
	case errors.As(err, &transport):
		fmt.Fprintln(e.stderr, "vyshka: "+transport.Error())
		return ExitTransport
	default:
		fmt.Fprintln(e.stderr, "vyshka: "+err.Error())
		return ExitUsage
	}
}

// printRefusal prints a hub refusal: the code and message, then what the
// details say in terms a person can act on.
func (e *env) printRefusal(refusal *client.Error) {
	if refusal.Code == "" {
		fmt.Fprintln(e.stderr, "vyshka: "+clean(refusal.Error()))
		return
	}
	fmt.Fprintf(e.stderr, "vyshka: %s: %s\n", clean(refusal.Code), clean(refusal.Message))
	if faults, ok := refusal.Details["errors"].([]any); ok {
		for _, fault := range faults {
			entry, _ := fault.(map[string]any)
			path, _ := entry["path"].(string)
			message, _ := entry["message"].(string)
			if path == "" {
				fmt.Fprintf(e.stderr, "  %s\n", clean(message))
			} else {
				fmt.Fprintf(e.stderr, "  %s: %s\n", clean(path), clean(message))
			}
		}
	}
	if refusal.Code == "revision_mismatch" {
		if revision, ok := refusal.Details["revision"].(float64); ok {
			fmt.Fprintf(e.stderr, "  current revision %d\n", int64(revision))
		}
	}
}

// defaultHTTPTimeout bounds each request unless --http-timeout says
// otherwise.
const defaultHTTPTimeout = 30 * time.Second

// client builds the hub client on first use, so commands that never touch the
// hub (version, help) never need a URL or a token.
func (e *env) client() (*client.Client, error) {
	if e.hub != nil {
		return e.hub, nil
	}
	target, err := e.resolveTarget()
	if err != nil {
		return nil, err
	}
	if e.g.httpTimeout <= 0 {
		return nil, usagef("--http-timeout must be positive")
	}
	httpClient := &http.Client{
		Timeout: e.g.httpTimeout,
		// Redirects are reported, not followed: see client.New.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	hub, err := client.New(target.url, target.token,
		client.WithHTTPClient(httpClient),
		client.WithUserAgent("vyshka/"+client.Version))
	if err != nil {
		// client.New never puts the token in an error.
		return nil, usagef("%v", err)
	}
	e.hub = hub
	return hub, nil
}
