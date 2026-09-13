// Package store owns the hub's database handle, driver selection, and schema
// migrations. It knows nothing about the protocol; protocol tables arrive with
// the slices that need them.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // pure Go Postgres driver, registered as "pgx"
	_ "modernc.org/sqlite"             // pure Go SQLite driver, keeps the build cgo-free
)

// Store is a database handle plus the metadata needed to describe it in logs
// and health output.
type Store struct {
	db     *DB
	driver dialect
	// target is what Target reports: a file path for SQLite, and for Postgres
	// a description with the password removed. The raw DSN is not kept.
	target string
}

// Postgres pool bounds. The pool is what makes Postgres worth having over the
// single-connection SQLite default, and also what makes every multi-statement
// write in this package take row locks (see lockServer): with more than one
// connection, two transactions can interleave between a read and the write
// that depends on it. Sixteen is enough to serve many concurrent long-polls
// (a held poll parks no connection) without exhausting a default Postgres
// max_connections of 100 when a few hubs share one server.
const (
	postgresMaxOpenConns    = 16
	postgresMaxIdleConns    = 8
	postgresConnMaxIdleTime = 5 * time.Minute
)

// Open resolves a DSN to a driver, opens the database, and verifies it answers.
// An empty DSN means the default local SQLite file.
//
// Accepted forms:
//
//	""                         -> sqlite, ./vyshka.db
//	"vyshka.db"                -> sqlite, that path
//	"sqlite://path/to.db"      -> sqlite, that path
//	"postgres://user:pw@h/db"  -> postgres, with a connection pool
//	"postgresql://..."         -> the same
//
// Postgres DSNs take the libpq URL form, including its query parameters
// (sslmode, connect_timeout, and the rest).
func Open(ctx context.Context, dsn string) (*Store, error) {
	driver, target, err := resolveDSN(dsn)
	if err != nil {
		return nil, err
	}

	var (
		raw   *sql.DB
		shown string
	)
	switch driver {
	case dialectSQLite:
		raw, err = sql.Open("sqlite", target)
		shown = target
	case dialectPostgres:
		raw, err = sql.Open("pgx", target)
		shown = redactPostgresDSN(target)
	}
	if err != nil {
		return nil, fmt.Errorf("open %s database: %w", driver, err)
	}

	switch driver {
	case dialectSQLite:
		// SQLite tolerates exactly one writer. Keeping the pool at one
		// connection trades a little throughput for never seeing SQLITE_BUSY,
		// and it is also what serializes every multi-statement write in this
		// package: the row locks the Postgres path takes (lockServer,
		// liveSessionSeq) have no SQLite spelling, and the one connection is
		// what stands in for them. Raising this without a locking story
		// reintroduces the concurrent-poll defects those functions document.
		raw.SetMaxOpenConns(1)
	case dialectPostgres:
		raw.SetMaxOpenConns(postgresMaxOpenConns)
		raw.SetMaxIdleConns(postgresMaxIdleConns)
		raw.SetConnMaxIdleTime(postgresConnMaxIdleTime)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := raw.PingContext(pingCtx); err != nil {
		raw.Close()
		return nil, fmt.Errorf("ping %s database: %w", driver, err)
	}

	if driver == dialectSQLite {
		for _, pragma := range []string{
			"PRAGMA journal_mode = WAL",
			"PRAGMA busy_timeout = 5000",
			"PRAGMA foreign_keys = ON",
		} {
			if _, err := raw.ExecContext(ctx, pragma); err != nil {
				raw.Close()
				return nil, fmt.Errorf("apply %q: %w", pragma, err)
			}
		}
	}

	return &Store{db: &DB{raw: raw, d: driver}, driver: driver, target: shown}, nil
}

// resolveDSN maps a configured DSN onto a dialect and the string handed to
// that dialect's driver. Postgres URLs pass through whole: the driver parses
// them, and every libpq query parameter keeps its meaning.
func resolveDSN(dsn string) (driver dialect, target string, err error) {
	switch {
	case dsn == "":
		return dialectSQLite, DefaultSQLitePath, nil
	case strings.HasPrefix(dsn, "sqlite://"):
		return dialectSQLite, strings.TrimPrefix(dsn, "sqlite://"), nil
	case strings.HasPrefix(dsn, "postgres://"), strings.HasPrefix(dsn, "postgresql://"):
		return dialectPostgres, dsn, nil
	case strings.Contains(dsn, "://"):
		scheme, _, _ := strings.Cut(dsn, "://")
		return "", "", fmt.Errorf("unsupported database scheme %q", scheme)
	default:
		return dialectSQLite, dsn, nil
	}
}

// redactPostgresDSN reduces a Postgres URL to what an operator needs to
// recognise the database in a log line: scheme, user, host, and database
// name. The password and every query parameter are dropped, the parameters
// because libpq lets some of them carry secrets too (sslpassword, and a
// password= that overrides the userinfo). A URL that does not parse is
// reported as nothing but its scheme rather than echoed.
func redactPostgresDSN(dsn string) string {
	parsed, err := url.Parse(dsn)
	if err != nil {
		scheme, _, _ := strings.Cut(dsn, "://")
		return scheme + "://<unparseable>"
	}
	redacted := url.URL{Scheme: parsed.Scheme, Host: parsed.Host, Path: parsed.Path}
	if parsed.User != nil && parsed.User.Username() != "" {
		redacted.User = url.User(parsed.User.Username())
	}
	return redacted.String()
}

// DefaultSQLitePath is the database file used when no DSN is configured.
const DefaultSQLitePath = "vyshka.db"

// DB exposes the handle for packages that run queries. Queries are written
// with `?` placeholders whatever the engine; the handle rebinds them.
func (s *Store) DB() *DB { return s.db }

// Driver names the backing engine, for logs and health output.
func (s *Store) Driver() string { return string(s.driver) }

// Target describes the database for logs: the file path for SQLite, and for
// Postgres the URL with its password and query parameters removed. It never
// returns anything a credential could hide in.
func (s *Store) Target() string { return s.target }

// Ping reports whether the database is still answering.
func (s *Store) Ping(ctx context.Context) error { return s.db.raw.PingContext(ctx) }

// Close releases the handle.
func (s *Store) Close() error { return s.db.raw.Close() }
