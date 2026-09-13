package store_test

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/That1Drifter/vyshka/hub/internal/dbtest"
	"github.com/That1Drifter/vyshka/hub/store"
)

func openTemp(t *testing.T) *store.Store {
	t.Helper()

	st, err := store.Open(context.Background(), dbtest.URL(t))
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
		"mysql://root@localhost/vyshka",
		"redis://localhost/0",
	} {
		st, err := store.Open(ctx, dsn)
		if err == nil {
			st.Close()
			t.Errorf("Open(%q) succeeded, want an error", dsn)
		}
	}
}

func TestOpenReportsTheSelectedBackend(t *testing.T) {
	st := openTemp(t)
	if st.Driver() != dbtest.Backend() {
		t.Errorf("driver = %q, want the run's backend %q", st.Driver(), dbtest.Backend())
	}
	if st.Driver() == "postgres" {
		// Target is what the startup log prints. The password is the only
		// part of the URL a reader must never see there.
		if parsed, err := url.Parse(dbtest.PostgresAdminURL()); err == nil {
			if password, set := parsed.User.Password(); set && strings.Contains(st.Target(), ":"+password+"@") {
				t.Errorf("Target() = %q carries the password", st.Target())
			}
		}
		if !strings.HasPrefix(st.Target(), "postgres") {
			t.Errorf("Target() = %q, want the redacted URL", st.Target())
		}
	}
}

// Both engines apply the same versions under the same names; only the SQL of
// a version with a dialect-specific file differs.
func TestMigrationsForBothDialectsAgreeOnVersions(t *testing.T) {
	sqlite, err := store.MigrationsFor("sqlite")
	if err != nil {
		t.Fatalf("sqlite migrations: %v", err)
	}
	postgres, err := store.MigrationsFor("postgres")
	if err != nil {
		t.Fatalf("postgres migrations: %v", err)
	}
	if len(sqlite) != len(postgres) {
		t.Fatalf("sqlite has %d migrations, postgres %d", len(sqlite), len(postgres))
	}
	differing := 0
	for i := range sqlite {
		if sqlite[i].Version != postgres[i].Version || sqlite[i].Name != postgres[i].Name {
			t.Errorf("migration %d: sqlite %d %q, postgres %d %q", i,
				sqlite[i].Version, sqlite[i].Name, postgres[i].Version, postgres[i].Name)
		}
		if sqlite[i].SQL != postgres[i].SQL {
			differing++
		}
	}
	// 0010_state is the one file with a Postgres spelling (identity column
	// against AUTOINCREMENT). A new variant is fine, but it should be a
	// decision, so this count is asserted.
	if differing != 1 {
		t.Errorf("%d migrations differ between dialects, want 1 (0010_state)", differing)
	}
	if _, err := store.MigrationsFor("oracle"); err == nil {
		t.Error("MigrationsFor accepted an unknown driver")
	}
}
