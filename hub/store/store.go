// Package store owns the hub's database handle, driver selection, and schema
// migrations. It knows nothing about the protocol; protocol tables arrive with
// the slices that need them.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure Go SQLite driver, keeps the build cgo-free
)

// Store is a database handle plus the metadata needed to describe it in logs
// and health output.
type Store struct {
	db     *sql.DB
	driver string
	dsn    string
}

// Open resolves a DSN to a driver, opens the database, and verifies it answers.
// An empty DSN means the default local SQLite file.
//
// Accepted forms:
//
//	""                     -> sqlite, ./vyshka.db
//	"vyshka.db"            -> sqlite, that path
//	"sqlite://path/to.db"  -> sqlite, that path
//	"postgres://..."       -> not supported yet, reported as such
func Open(ctx context.Context, dsn string) (*Store, error) {
	driver, target, err := resolveDSN(dsn)
	if err != nil {
		return nil, err
	}

	db, err := sql.Open(driver, target)
	if err != nil {
		return nil, fmt.Errorf("open %s database: %w", driver, err)
	}

	// SQLite tolerates exactly one writer. Keeping the pool at one connection
	// trades a little throughput for never seeing SQLITE_BUSY.
	if driver == "sqlite" {
		db.SetMaxOpenConns(1)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping %s database: %w", driver, err)
	}

	if driver == "sqlite" {
		for _, pragma := range []string{
			"PRAGMA journal_mode = WAL",
			"PRAGMA busy_timeout = 5000",
			"PRAGMA foreign_keys = ON",
		} {
			if _, err := db.ExecContext(ctx, pragma); err != nil {
				db.Close()
				return nil, fmt.Errorf("apply %q: %w", pragma, err)
			}
		}
	}

	return &Store{db: db, driver: driver, dsn: target}, nil
}

func resolveDSN(dsn string) (driver, target string, err error) {
	switch {
	case dsn == "":
		return "sqlite", DefaultSQLitePath, nil
	case strings.HasPrefix(dsn, "sqlite://"):
		return "sqlite", strings.TrimPrefix(dsn, "sqlite://"), nil
	case strings.HasPrefix(dsn, "postgres://"), strings.HasPrefix(dsn, "postgresql://"):
		return "", "", fmt.Errorf("postgres support is not implemented yet; unset DATABASE_URL to use SQLite")
	case strings.Contains(dsn, "://"):
		scheme, _, _ := strings.Cut(dsn, "://")
		return "", "", fmt.Errorf("unsupported database scheme %q", scheme)
	default:
		return "sqlite", dsn, nil
	}
}

// DefaultSQLitePath is the database file used when no DSN is configured.
const DefaultSQLitePath = "vyshka.db"

// DB exposes the handle for packages that run queries.
func (s *Store) DB() *sql.DB { return s.db }

// Driver names the backing engine, for logs and health output.
func (s *Store) Driver() string { return s.driver }

// Target is the resolved file path or connection target, for logs.
func (s *Store) Target() string { return s.dsn }

// Ping reports whether the database is still answering.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// Close releases the handle.
func (s *Store) Close() error { return s.db.Close() }
