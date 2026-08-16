package store_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/That1Drifter/vyshka/hub/store"
)

func openTemp(t *testing.T) *store.Store {
	t.Helper()

	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestMigrateAppliesThenIsIdempotent(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	applied, err := st.Migrate(ctx)
	if err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	if len(applied) == 0 {
		t.Fatal("first migrate applied nothing")
	}

	version, err := st.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}
	if version != applied[len(applied)-1] {
		t.Errorf("schema version = %d, want %d", version, applied[len(applied)-1])
	}

	// A second boot against the same database must be a no-op.
	again, err := st.Migrate(ctx)
	if err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("second migrate applied %v, want nothing", again)
	}
}

func TestMigrationsAreOrderedAndUnique(t *testing.T) {
	migrations, err := store.Migrations()
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	if len(migrations) == 0 {
		t.Fatal("no embedded migrations")
	}

	previous := 0
	for _, migration := range migrations {
		if migration.Version <= previous {
			t.Fatalf("version %d follows %d, migrations must ascend", migration.Version, previous)
		}
		if strings.TrimSpace(migration.SQL) == "" {
			t.Errorf("migration %d (%s) is empty", migration.Version, migration.Name)
		}
		previous = migration.Version
	}
}

func TestSchemaVersionIsZeroBeforeMigrating(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	// SchemaVersion needs the bookkeeping table, which Migrate creates. Before
	// any migration runs there is nothing to report, and that must not be an
	// error a caller has to special-case.
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx, "DELETE FROM schema_migrations"); err != nil {
		t.Fatalf("clear schema_migrations: %v", err)
	}

	version, err := st.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}
	if version != 0 {
		t.Errorf("schema version = %d, want 0", version)
	}
}

func TestOpenRejectsUnsupportedDSN(t *testing.T) {
	ctx := context.Background()

	for _, dsn := range []string{
		"postgres://user:pass@localhost/vyshka",
		"mysql://root@localhost/vyshka",
	} {
		st, err := store.Open(ctx, dsn)
		if err == nil {
			st.Close()
			t.Errorf("Open(%q) succeeded, want an error", dsn)
		}
	}
}

func TestOpenDefaultsToSQLite(t *testing.T) {
	st := openTemp(t)
	if st.Driver() != "sqlite" {
		t.Errorf("driver = %q, want sqlite", st.Driver())
	}
}
