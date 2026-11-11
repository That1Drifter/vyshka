// Command vyshka-hub is the reference Vyshka hub.
//
// Usage:
//
//	vyshka-hub serve [-config FILE] [-addr host:port] [-db DSN] [-log-level level] [-panel=false] [-maps-dir DIR]
//	                 [-backup-dir DIR] [-backup-keep N]
//	                 [-metrics-addr host:port]
//	                 [-auto-tls -auto-tls-hosts NAMES [-auto-tls-cache DIR] [-auto-tls-email ADDR]
//	                  [-auto-tls-directory URL] [-auto-tls-ca-roots FILE] [-auto-tls-http-addr host:port|off]]
//	vyshka-hub config [the arguments serve would take]
//	vyshka-hub storage [-config FILE] [-db DSN] [-json]
//	vyshka-hub vacuum [-config FILE] [-db DSN]
//	vyshka-hub version
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

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
	case "config":
		return runConfig(args[1:])
	case "storage":
		return runStorage(args[1:])
	case "vacuum":
		return runVacuum(args[1:])
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
  config     print the settings serve would use and where each came from, then exit
  storage    report table sizes and what the retention sweeps would remove now
  vacuum     compact a SQLite database (stop the hub first)
  version    print the build version

Run "vyshka-hub serve -h" for serve flags. A setting comes from, highest first: a flag,
an environment variable, the file named by -config (or VYSHKA_HUB_CONFIG), a built-in default.
`)
}

func runServe(args []string) error { return serve("serve", args, false) }

// runConfig resolves the settings exactly as serve does, validates them, and
// prints them with where each came from; it opens no database and no listener.
func runConfig(args []string) error { return serve("config", args, true) }

// serveValues are the serve flags the rest of serve reads after parsing.
type serveValues struct {
	addr, dsn, adminToken, logLevel, mapsDir, backupDir, metricsAddr *string
	servePanel                                                       *bool
	backupKeep                                                       *int
	tls                                                              *autoTLSFlags
}

// declareServeFlags defines every serve flag. It is separate from serve so
// the maintenance commands can learn which keys a config file may hold. A flag
// added here is settable from the config file with no further code, provided
// its usage text names the environment variable envName gives it.
func declareServeFlags(flags *flag.FlagSet) (*serveValues, error) {
	configFlag(flags)
	addr := flags.String("addr", envOr("VYSHKA_ADDR", "127.0.0.1:8080"), "listen address (env VYSHKA_ADDR)")
	// The two secret-bearing flags do not carry their environment value as
	// the flag default: the flag package prints defaults in its usage text,
	// so `serve -h` or a mistyped flag would print a Postgres password or
	// the admin token to the terminal. The environment is read after parsing
	// instead (see below).
	dsn := flags.String("db", "", "database DSN, or file:/path/to/secret; empty means local SQLite (env DATABASE_URL)")
	adminToken := flags.String("admin-token", "",
		"bootstrap Admin API token, or file:/path/to/secret; empty generates one per boot (env VYSHKA_ADMIN_TOKEN)")
	logLevel := flags.String("log-level", envOr("VYSHKA_LOG_LEVEL", "info"), "debug, info, warn, or error (env VYSHKA_LOG_LEVEL)")
	servePanel := flags.Bool("panel", envOr("VYSHKA_PANEL", "true") != "false",
		"serve the embedded web panel at /panel/ (env VYSHKA_PANEL, \"false\" disables it)")
	mapsDir := flags.String("maps-dir", os.Getenv("VYSHKA_MAPS_DIR"),
		"directory of map tilesets for the panel's live map, one subdirectory per world (env VYSHKA_MAPS_DIR); empty serves none")
	backupDir := flags.String("backup-dir", os.Getenv("VYSHKA_BACKUP_DIR"),
		"directory for POST /api/v1/admin/backup snapshots (env VYSHKA_BACKUP_DIR); empty means a backups directory beside the SQLite file")
	keepDefault, err := envInt("VYSHKA_BACKUP_KEEP", hub.DefaultBackupKeep)
	if err != nil {
		return nil, err
	}
	backupKeep := flags.Int("backup-keep", keepDefault,
		"snapshots the backup endpoint keeps before deleting the oldest (env VYSHKA_BACKUP_KEEP)")
	metricsAddr := flags.String("metrics-addr", os.Getenv("VYSHKA_METRICS_ADDR"),
		"listen address of the Prometheus endpoint, GET /metrics on a listener of its own, unauthenticated: "+
			"keep it on loopback or a private network (env VYSHKA_METRICS_ADDR); empty disables it")
	autoTLSDefault := false
	if raw := os.Getenv("VYSHKA_AUTO_TLS"); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, fmt.Errorf("VYSHKA_AUTO_TLS=%q is not true or false", raw)
		}
		autoTLSDefault = parsed
	}
	var tlsFlags autoTLSFlags
	flags.BoolVar(&tlsFlags.enabled, "auto-tls", autoTLSDefault,
		"obtain and renew a certificate over ACME and serve TLS on -addr (default :443 when on) (env VYSHKA_AUTO_TLS)")
	flags.StringVar(&tlsFlags.hosts, "auto-tls-hosts", os.Getenv("VYSHKA_AUTO_TLS_HOSTS"),
		"comma-separated DNS names to get certificates for, required with -auto-tls; no other name gets one (env VYSHKA_AUTO_TLS_HOSTS)")
	flags.StringVar(&tlsFlags.cacheDir, "auto-tls-cache", os.Getenv("VYSHKA_AUTO_TLS_CACHE"),
		"directory for the ACME account key and certificates, created 0700; empty means "+autoTLSCacheName+" beside the SQLite file (env VYSHKA_AUTO_TLS_CACHE)")
	flags.StringVar(&tlsFlags.email, "auto-tls-email", os.Getenv("VYSHKA_AUTO_TLS_EMAIL"),
		"contact address given to the CA, optional (env VYSHKA_AUTO_TLS_EMAIL)")
	flags.StringVar(&tlsFlags.directory, "auto-tls-directory", os.Getenv("VYSHKA_AUTO_TLS_DIRECTORY"),
		"ACME directory URL; empty means Let's Encrypt production (env VYSHKA_AUTO_TLS_DIRECTORY)")
	flags.StringVar(&tlsFlags.caRoots, "auto-tls-ca-roots", os.Getenv("VYSHKA_AUTO_TLS_CA_ROOTS"),
		"PEM file of extra roots to trust for the ACME directory's own TLS, for a private or test CA (env VYSHKA_AUTO_TLS_CA_ROOTS)")
	flags.StringVar(&tlsFlags.httpAddr, "auto-tls-http-addr", os.Getenv("VYSHKA_AUTO_TLS_HTTP_ADDR"),
		"plain listener for ACME HTTP-01 and the redirect to https, nothing else; empty means :80, off disables it (env VYSHKA_AUTO_TLS_HTTP_ADDR)")
	return &serveValues{
		addr: addr, dsn: dsn, adminToken: adminToken, logLevel: logLevel, mapsDir: mapsDir,
		backupDir: backupDir, metricsAddr: metricsAddr, servePanel: servePanel, backupKeep: backupKeep,
		tls: &tlsFlags,
	}, nil
}

func serve(name string, args []string, describe bool) error {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	values, err := declareServeFlags(flags)
	if err != nil {
		return err
	}
	addr, dsn, adminToken, logLevel, mapsDir := values.addr, values.dsn, values.adminToken, values.logLevel, values.mapsDir
	backupDir, metricsAddr, servePanel, backupKeep, tlsFlags := values.backupDir, values.metricsAddr, values.servePanel, values.backupKeep, values.tls
	if err := flags.Parse(args); err != nil {
		return err
	}
	resolved, err := resolveSettings(flags, func() (*flag.FlagSet, error) { return flags, nil })
	if err != nil {
		return err
	}
	// Both secret-bearing settings take the file: form from every source.
	// The database is resolved before the auto-TLS plan, which looks for the
	// SQLite file beside which the certificate cache goes. The resolved
	// values live in these locals and never in the flag set, which is what
	// the config command prints from.
	resolvedDSN, err := resolved.secret("db", *dsn)
	if err != nil {
		return err
	}
	if *backupKeep < 1 {
		return fmt.Errorf("backup keep: %d is below 1", *backupKeep)
	}
	// An address from any source but the default (flag, environment, config
	// file) keeps auto-TLS from moving the listener to :443.
	addrGiven := resolved.sources["addr"] != "default"
	tlsPlan, listenAddr, err := planAutoTLS(*tlsFlags, *addr, addrGiven, resolvedDSN)
	if err != nil {
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
	resolvedAdminToken, err := resolved.secret("admin-token", *adminToken)
	if err != nil {
		return err
	}
	if describe {
		return resolved.print(os.Stdout, os.Stderr, flags, listenAddr)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	resolved.log(logger)

	// Ctrl-C and SIGTERM both mean "drain and stop".
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var tlsRuntime *autoTLS
	var tlsConfig *tls.Config
	if tlsPlan != nil {
		tlsRuntime, tlsConfig, err = prepareAutoTLS(tlsPlan, logger)
		if err != nil {
			return err
		}
	}

	config := hub.Config{
		Addr:        listenAddr,
		TLSConfig:   tlsConfig,
		DatabaseURL: resolvedDSN,
		AdminToken:  resolvedAdminToken,
		Logger:      logger,
		BackupDir:   *backupDir,
		BackupKeep:  *backupKeep,
		MetricsAddr: *metricsAddr,
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

	if tlsRuntime != nil {
		closePlain, err := tlsRuntime.start(ctx)
		if err != nil {
			return err
		}
		defer closePlain()
	}

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

// maxSecretFileBytes bounds a secret file: a token or a database URL is a
// short line, so a larger file is not one.
const maxSecretFileBytes = 4096

// resolveSecret supports the `file:` indirection every secret-bearing flag
// takes, so an operator can keep credentials out of the process list and out of
// their shell history. The file must be plainly a secret: one line of UTF-8
// text, at most maxSecretFileBytes, without a NUL byte (surrounding whitespace
// and the newline an editor leaves are trimmed). Anything else is refused with
// a message that names the path and says what is wrong, and carries nothing
// from the file: a mistaken reference to a database file or a log must not end
// up in an error message or be used as the secret.
func resolveSecret(value string) (string, error) {
	path, isFile := strings.CutPrefix(value, "file:")
	if !isFile {
		return value, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	contents, err := io.ReadAll(io.LimitReader(file, maxSecretFileBytes+1))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	switch {
	case len(contents) > maxSecretFileBytes:
		return "", fmt.Errorf("%s is larger than %d bytes, which is too large to be a secret", path, maxSecretFileBytes)
	case strings.IndexByte(string(contents), 0) >= 0:
		return "", fmt.Errorf("%s contains a NUL byte, so it is not a text secret", path)
	case !utf8.Valid(contents):
		return "", fmt.Errorf("%s is not valid UTF-8, so it is not a text secret", path)
	}
	secret := strings.TrimSpace(string(contents))
	switch {
	case secret == "":
		return "", fmt.Errorf("%s is empty", path)
	case strings.ContainsAny(secret, "\r\n"):
		return "", fmt.Errorf("%s holds more than one line; a secret file holds the secret and nothing else", path)
	}
	return secret, nil
}

// envInt is envOr for an integer flag's default. A value that does not parse
// is an error rather than a silent fallback.
func envInt(key string, fallback int) (int, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s=%q is not an integer", key, value)
	}
	return parsed, nil
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
