// Command vyshka-hub is the reference Vyshka hub.
//
// Usage:
//
//	vyshka-hub serve [-addr host:port] [-db DSN] [-log-level level]
//	vyshka-hub version
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/That1Drifter/vyshka/hub"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "vyshka-hub:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return fmt.Errorf("a command is required")
	}

	switch args[0] {
	case "serve":
		return runServe(args[1:])
	case "version":
		fmt.Println(hub.Version)
		return nil
	case "-h", "--help", "help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `vyshka-hub, the reference Vyshka hub.

Commands:
  serve      run the hub
  version    print the build version

Run "vyshka-hub serve -h" for serve flags.
`)
}

func runServe(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := flags.String("addr", envOr("VYSHKA_ADDR", "127.0.0.1:8080"), "listen address (env VYSHKA_ADDR)")
	dsn := flags.String("db", os.Getenv("DATABASE_URL"), "database DSN, empty means local SQLite (env DATABASE_URL)")
	logLevel := flags.String("log-level", envOr("VYSHKA_LOG_LEVEL", "info"), "debug, info, warn, or error (env VYSHKA_LOG_LEVEL)")
	if err := flags.Parse(args); err != nil {
		return err
	}

	level, err := parseLevel(*logLevel)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	// Ctrl-C and SIGTERM both mean "drain and stop".
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	server, err := hub.New(ctx, hub.Config{
		Addr:        *addr,
		DatabaseURL: *dsn,
		Logger:      logger,
	})
	if err != nil {
		return err
	}
	defer server.Close()

	return server.Serve(ctx)
}

func parseLevel(name string) (slog.Level, error) {
	switch strings.ToLower(name) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unknown log level %q", name)
	}
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
