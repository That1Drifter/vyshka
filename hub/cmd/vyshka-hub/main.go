// Command vyshka-hub is the reference Vyshka hub.
//
// Usage:
//
//	vyshka-hub serve [-addr host:port] [-db DSN] [-log-level level] [-panel=false] [-maps-dir DIR]
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
	"github.com/That1Drifter/vyshka/panel"
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
	adminToken := flags.String("admin-token", os.Getenv("VYSHKA_ADMIN_TOKEN"),
		"bootstrap Admin API token, or file:/path/to/secret; empty generates one per boot (env VYSHKA_ADMIN_TOKEN)")
	logLevel := flags.String("log-level", envOr("VYSHKA_LOG_LEVEL", "info"), "debug, info, warn, or error (env VYSHKA_LOG_LEVEL)")
	servePanel := flags.Bool("panel", envOr("VYSHKA_PANEL", "true") != "false",
		"serve the embedded web panel at /panel/ (env VYSHKA_PANEL, \"false\" disables it)")
	mapsDir := flags.String("maps-dir", os.Getenv("VYSHKA_MAPS_DIR"),
		"directory of map tilesets for the panel's live map, one subdirectory per world (env VYSHKA_MAPS_DIR); empty serves none")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *mapsDir != "" {
		info, err := os.Stat(*mapsDir)
		if err != nil {
			return fmt.Errorf("maps dir: %w", err)
		}
		if !info.IsDir() {
			return fmt.Errorf("maps dir: %s is not a directory", *mapsDir)
		}
	}

	level, err := parseLevel(*logLevel)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	resolvedAdminToken, err := resolveSecret(*adminToken)
	if err != nil {
		return fmt.Errorf("admin token: %w", err)
	}

	// Ctrl-C and SIGTERM both mean "drain and stop".
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	config := hub.Config{
		Addr:        *addr,
		DatabaseURL: *dsn,
		AdminToken:  resolvedAdminToken,
		Logger:      logger,
	}
	if *servePanel {
		config.Panel = panel.NewHandler(panel.Config{MapsDir: *mapsDir})
		if *mapsDir != "" {
			logger.Info("panel maps enabled", "dir", *mapsDir)
		}
	}
	server, err := hub.New(ctx, config)
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

// resolveSecret supports the `file:` indirection every secret-bearing flag
// takes, so an operator can keep credentials out of the process list and out of
// their shell history.
func resolveSecret(value string) (string, error) {
	path, isFile := strings.CutPrefix(value, "file:")
	if !isFile {
		return value, nil
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	secret := strings.TrimSpace(string(contents))
	if secret == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return secret, nil
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
