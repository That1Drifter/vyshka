// Command vyshka is a terminal client for a Vyshka hub's Admin API: server
// records and enrollment, action dispatch with the outcome waited on, state,
// events, the key/value store, and player profiles.
//
// Usage:
//
//	vyshka [global flags] <command> [arguments] [flags]
//	vyshka help
//
// It speaks only the public Admin API (spec/openapi-admin.yaml), through the
// client package, and imports nothing of the hub itself.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/That1Drifter/vyshka/cmd/vyshka/internal/cli"
)

func main() {
	// Ctrl-C ends a long-running command (events --follow, run --wait)
	// through its context rather than killing it mid-write.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Run(os.Args[1:], cli.IO{
		Stdin:           os.Stdin,
		Stdout:          os.Stdout,
		Stderr:          os.Stderr,
		Getenv:          os.Getenv,
		StdinIsTerminal: stdinIsTerminal(),
		ConfigDir:       os.UserConfigDir,
		Context:         ctx,
	})
	stop()
	os.Exit(code)
}

// stdinIsTerminal (terminal_*.go, one per platform family) reports whether
// stdin is a terminal someone can answer a prompt on, so a destructive
// action's confirmation is only ever put to a person, and a script gets told
// to pass --yes instead.
