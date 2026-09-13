package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Migration is one numbered schema step, embedded in the binary so a released
// hub can never drift from the schema it expects.
type Migration struct {
	Version int
	Name    string
	SQL     string
}

// Migrations returns the embedded migrations in version order, as SQLite
// applies them. Postgres applies the same versions; see MigrationsFor.
func Migrations() ([]Migration, error) { return migrationsFor(dialectSQLite) }

// MigrationsFor returns the migrations a named driver ("sqlite" or "postgres")
// applies, in version order. The two sets carry the same versions and names;
// they differ only where a file has a dialect-specific variant.
func MigrationsFor(driver string) ([]Migration, error) {
	switch dialect(driver) {
	case dialectSQLite, dialectPostgres:
		return migrationsFor(dialect(driver))
	default:
		return nil, fmt.Errorf("unknown driver %q", driver)
	}
}

// migrationsFor resolves the embedded files for one dialect. A migration is
// `NNNN_name.sql`, shared by every engine. Where the engines' DDL differs, a
// `NNNN_name.postgres.sql` beside it replaces the shared file for Postgres;
// the shared file must still exist, so the version list never depends on the
// engine. Both files carry the same version and name and land the same
// schema, and a version may not appear twice for one dialect.
func migrationsFor(d dialect) ([]Migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}

	type file struct {
		name    string
		dialect dialect
	}
	byVersion := map[int]map[dialect]file{}
	names := map[int]string{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		version, name, fileDialect, err := parseMigrationName(entry.Name())
		if err != nil {
			return nil, err
		}
		if previous, seen := names[version]; seen && previous != name {
			return nil, fmt.Errorf("migration version %d is named both %q and %q", version, previous, name)
		}
		names[version] = name
		if byVersion[version] == nil {
			byVersion[version] = map[dialect]file{}
		}
		if previous, dup := byVersion[version][fileDialect]; dup {
			return nil, fmt.Errorf("duplicate migration version %d: %q and %q", version, previous.name, entry.Name())
		}
		byVersion[version][fileDialect] = file{name: entry.Name(), dialect: fileDialect}
	}

	migrations := make([]Migration, 0, len(byVersion))
	for version, variants := range byVersion {
		shared, hasShared := variants[""]
		if !hasShared {
			return nil, fmt.Errorf("migration version %d has only dialect-specific files; the shared file is required", version)
		}
		chosen := shared
		if variant, ok := variants[d]; ok {
			chosen = variant
		}
		body, err := fs.ReadFile(migrationFS, "migrations/"+chosen.name)
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", chosen.name, err)
		}
		migrations = append(migrations, Migration{Version: version, Name: names[version], SQL: string(body)})
	}

	sort.Slice(migrations, func(i, j int) bool { return migrations[i].Version < migrations[j].Version })
	return migrations, nil
}

// parseMigrationName splits "0001_meta.sql" into 1, "meta", and no dialect,
// and "0010_state.postgres.sql" into 10, "state", and postgres.
func parseMigrationName(filename string) (int, string, dialect, error) {
	base := strings.TrimSuffix(filename, ".sql")
	prefix, name, found := strings.Cut(base, "_")
	if !found {
		return 0, "", "", fmt.Errorf("migration %q must be named <version>_<name>[.<dialect>].sql", filename)
	}
	version, err := strconv.Atoi(prefix)
	if err != nil {
		return 0, "", "", fmt.Errorf("migration %q has a non-numeric version: %w", filename, err)
	}
	if version <= 0 {
		return 0, "", "", fmt.Errorf("migration %q must have a version above zero", filename)
	}
	var d dialect
	if stem, suffix, dotted := strings.Cut(name, "."); dotted {
		switch dialect(suffix) {
		case dialectSQLite, dialectPostgres:
			name, d = stem, dialect(suffix)
		default:
			return 0, "", "", fmt.Errorf("migration %q names an unknown dialect %q", filename, suffix)
		}
	}
	return version, name, d, nil
}

// migrationLockKey is the Postgres advisory lock every migrator takes for the
// length of its run. Any fixed value works as long as every hub agrees on it;
// this one is "vyshka" in ASCII with a version suffix.
const migrationLockKey int64 = 0x7679_7368_6b61_0001

// Migrate applies every migration the database has not recorded yet, in order,
// each in its own transaction. It is safe to call on every boot: already
// applied versions are skipped, so a hub that restarts unchanged does nothing.
// It returns the versions applied by this call.
//
// On Postgres the whole run holds a session-level advisory lock, so two hubs
// booting against one database migrate one after the other: the second finds
// every version recorded and applies nothing. Without it both could apply the
// same migration, and the loser would fail on an already-existing object or a
// duplicate ledger row after a partial run. SQLite needs no lock: its pool is
// one connection, and a second process contends on the file lock instead.
func (s *Store) Migrate(ctx context.Context) ([]int, error) {
	// One pinned connection for the run, because a session-level advisory
	// lock belongs to the connection that took it. Pinning also keeps the
	// bookkeeping reads on the same connection as the DDL they follow.
	conn, err := s.db.raw.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Close()

	if s.driver == dialectPostgres {
		if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
			return nil, fmt.Errorf("take migration lock: %w", err)
		}
		defer func() {
			// Released explicitly rather than left to the connection's close:
			// a pooled connection outlives this call, and the lock would sit
			// on it until the pool retired it.
			_, _ = conn.ExecContext(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrationLockKey)
		}()
	}

	if _, err := conn.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			name       TEXT NOT NULL,
			applied_at TEXT NOT NULL
		)`); err != nil {
		return nil, fmt.Errorf("create schema_migrations: %w", err)
	}

	applied, err := appliedVersions(ctx, conn)
	if err != nil {
		return nil, err
	}

	migrations, err := migrationsFor(s.driver)
	if err != nil {
		return nil, err
	}

	var ran []int
	for _, migration := range migrations {
		if applied[migration.Version] {
			continue
		}
		if err := s.applyMigration(ctx, conn, migration); err != nil {
			return ran, err
		}
		ran = append(ran, migration.Version)
	}
	return ran, nil
}

func (s *Store) applyMigration(ctx context.Context, conn *sql.Conn, migration Migration) error {
	raw, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration %d: %w", migration.Version, err)
	}
	tx := &Tx{tx: raw, d: s.driver}
	defer tx.Rollback()

	// A migration file is one script of several statements. Both drivers run
	// a multi-statement script when it carries no parameters, which is why
	// migrations never take any.
	if _, err := tx.ExecContext(ctx, migration.SQL); err != nil {
		return fmt.Errorf("apply migration %d (%s): %w", migration.Version, migration.Name, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
		migration.Version, migration.Name, time.Now().UTC().Format(time.RFC3339),
	); err != nil {
		return fmt.Errorf("record migration %d: %w", migration.Version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %d: %w", migration.Version, err)
	}
	return nil
}

func appliedVersions(ctx context.Context, conn *sql.Conn) (map[int]bool, error) {
	rows, err := conn.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()

	applied := map[int]bool{}
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			return nil, fmt.Errorf("scan schema_migrations: %w", err)
		}
		applied[version] = true
	}
	return applied, rows.Err()
}

// SchemaVersion reports the highest applied migration version, or 0 when the
// database is empty.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var version *int
	err := s.db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	if version == nil {
		return 0, nil
	}
	return *version, nil
}
