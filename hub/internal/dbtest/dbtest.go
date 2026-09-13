// Package dbtest hands tests a throwaway database on whichever backend the
// run selects, so the same test grades SQLite and Postgres.
//
// The backend is chosen by environment, not by test code, so one `go test`
// invocation grades one engine and CI runs the suite once per engine:
//
//	VYSHKA_TEST_BACKEND=sqlite    (default) a fresh SQLite file per test
//	VYSHKA_TEST_BACKEND=postgres  a fresh Postgres database per test, created
//	                              through VYSHKA_TEST_POSTGRES_URL and dropped
//	                              when the test ends
//
// VYSHKA_TEST_POSTGRES_URL is a URL with rights to CREATE DATABASE on the
// server it names; the database in its path is only the one the helper
// connects to for that. Asking for Postgres without it is a failure, not a
// skip: a CI job that meant to grade Postgres must not pass by grading
// nothing.
package dbtest

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // "pgx", for creating and dropping test databases
)

// Environment variables read by URL.
const (
	BackendVar     = "VYSHKA_TEST_BACKEND"
	PostgresURLVar = "VYSHKA_TEST_POSTGRES_URL"
)

// Backend reports which engine this run grades: "sqlite" or "postgres".
func Backend() string {
	switch backend := strings.ToLower(strings.TrimSpace(os.Getenv(BackendVar))); backend {
	case "", "sqlite":
		return "sqlite"
	default:
		return backend
	}
}

// URL returns a DSN for a database that exists for this test alone. On SQLite
// it is a file under t.TempDir(); on Postgres it is a database created now and
// dropped at cleanup. Calling it twice in one test yields two databases.
func URL(t *testing.T) string {
	t.Helper()
	switch backend := Backend(); backend {
	case "sqlite":
		return filepath.Join(t.TempDir(), "test.db")
	case "postgres":
		return postgresURL(t)
	default:
		t.Fatalf("%s=%q: unknown backend, want sqlite or postgres", BackendVar, backend)
		return ""
	}
}

// PostgresAdminURL returns the configured maintenance URL, or "" when the run
// has none. Tests that only make sense on Postgres skip when it is empty and
// the backend is not postgres.
func PostgresAdminURL() string { return strings.TrimSpace(os.Getenv(PostgresURLVar)) }

func postgresURL(t *testing.T) string {
	t.Helper()
	admin := PostgresAdminURL()
	if admin == "" {
		t.Fatalf("%s=postgres needs %s to name a server with CREATE DATABASE rights", BackendVar, PostgresURLVar)
	}
	parsed, err := url.Parse(admin)
	if err != nil {
		t.Fatalf("%s: %v", PostgresURLVar, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	control, err := sql.Open("pgx", admin)
	if err != nil {
		t.Fatalf("open %s: %v", PostgresURLVar, err)
	}
	t.Cleanup(func() { control.Close() })

	name := "vyshka_test_" + randomSuffix()
	// Identifiers cannot be parameters; the name is ours and matches [a-z0-9_].
	if _, err := control.ExecContext(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		t.Fatalf("create test database %s: %v", name, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		// FORCE terminates any connection the store left open; the store is
		// closed by its own cleanup, which runs before this one (LIFO), but a
		// pooled connection mid-close must not leave the database behind.
		if _, err := control.ExecContext(ctx, `DROP DATABASE "`+name+`" WITH (FORCE)`); err != nil {
			t.Errorf("drop test database %s: %v", name, err)
		}
	})

	return perTestURL(parsed, name)
}

// perTestURL points the maintenance URL at one test's database. The
// database is selected by the path, so any query parameter that would
// override it (libpq's dbname, pgx's database) is dropped: left in place,
// every test would connect to the maintenance database, migrate and mutate
// it, and drop an unused database at cleanup.
//
// The other parameters are kept byte for byte and in order. Re-encoding
// them through url.Values would turn a `%20` into a `+`, which the driver
// does not read as a space, and would reorder keys whose precedence depends
// on order.
func perTestURL(admin *url.URL, name string) string {
	test := *admin
	test.Path = "/" + name
	if test.RawQuery != "" {
		kept := make([]string, 0, 4)
		for _, pair := range strings.Split(test.RawQuery, "&") {
			rawKey, _, _ := strings.Cut(pair, "=")
			// Compared as the driver reads it: percent-decoded, with `+`
			// left alone (libpq rules, not form encoding), so an encoded
			// `%64bname` cannot smuggle the override past this filter.
			key, err := url.PathUnescape(rawKey)
			if err != nil {
				key = rawKey
			}
			if key == "dbname" || key == "database" {
				continue
			}
			kept = append(kept, pair)
		}
		test.RawQuery = strings.Join(kept, "&")
	}
	return test.String()
}

func randomSuffix() string {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic(fmt.Sprintf("dbtest: read random bytes: %v", err))
	}
	return hex.EncodeToString(raw[:])
}
