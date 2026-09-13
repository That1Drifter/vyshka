package store

import (
	"strings"
	"testing"
)

func TestRebindRewritesPlaceholdersForPostgresOnly(t *testing.T) {
	cases := []struct {
		query    string
		postgres string
	}{
		{`SELECT 1`, `SELECT 1`},
		{`SELECT id FROM servers WHERE id = ?`, `SELECT id FROM servers WHERE id = $1`},
		{`INSERT INTO t (a, b, c) VALUES (?, ?, ?)`, `INSERT INTO t (a, b, c) VALUES ($1, $2, $3)`},
		// A question mark inside a literal is data, not a placeholder.
		{`UPDATE t SET note = 'why?' WHERE id = ?`, `UPDATE t SET note = 'why?' WHERE id = $1`},
		{`SELECT ? WHERE x = '' AND y = ?`, `SELECT $1 WHERE x = '' AND y = $2`},
		{`SELECT 'it''s?' , ?`, `SELECT 'it''s?' , $1`},
		// Comments are copied through, and a quote inside one opens nothing.
		{"SELECT ? -- why? isn't it\nFROM t WHERE a = ?", "SELECT $1 -- why? isn't it\nFROM t WHERE a = $2"},
		{`SELECT /* ? don't */ ?::integer`, `SELECT /* ? don't */ $1::integer`},
		{`SELECT ? -- unterminated?`, `SELECT $1 -- unterminated?`},
	}
	for _, c := range cases {
		if got := dialectPostgres.rebind(c.query); got != c.postgres {
			t.Errorf("postgres rebind(%q) = %q, want %q", c.query, got, c.postgres)
		}
		if got := dialectSQLite.rebind(c.query); got != c.query {
			t.Errorf("sqlite rebind(%q) = %q, want it untouched", c.query, got)
		}
	}
}

func TestForUpdateIsPostgresOnly(t *testing.T) {
	if got := dialectPostgres.forUpdate(); strings.TrimSpace(got) != "FOR UPDATE" {
		t.Errorf("postgres forUpdate = %q", got)
	}
	if got := dialectSQLite.forUpdate(); got != "" {
		t.Errorf("sqlite forUpdate = %q, want empty", got)
	}
}

func TestResolveDSN(t *testing.T) {
	cases := []struct {
		dsn    string
		driver dialect
		target string
		bad    bool
	}{
		{dsn: "", driver: dialectSQLite, target: DefaultSQLitePath},
		{dsn: "vyshka.db", driver: dialectSQLite, target: "vyshka.db"},
		{dsn: "sqlite://data/hub.db", driver: dialectSQLite, target: "data/hub.db"},
		{dsn: "postgres://u:p@db.example:5432/vyshka?sslmode=require", driver: dialectPostgres,
			target: "postgres://u:p@db.example:5432/vyshka?sslmode=require"},
		{dsn: "postgresql://u@db.example/vyshka", driver: dialectPostgres,
			target: "postgresql://u@db.example/vyshka"},
		{dsn: "mysql://root@localhost/vyshka", bad: true},
	}
	for _, c := range cases {
		driver, target, err := resolveDSN(c.dsn)
		if c.bad {
			if err == nil {
				t.Errorf("resolveDSN(%q) accepted, want an error", c.dsn)
			}
			continue
		}
		if err != nil {
			t.Errorf("resolveDSN(%q): %v", c.dsn, err)
			continue
		}
		if driver != c.driver || target != c.target {
			t.Errorf("resolveDSN(%q) = (%s, %q), want (%s, %q)", c.dsn, driver, target, c.driver, c.target)
		}
	}
}

func TestRedactPostgresDSNDropsEverySecretBearingPart(t *testing.T) {
	const password = "hunter2-not-for-logs"
	cases := []struct {
		dsn  string
		want string
	}{
		{"postgres://vyshka:" + password + "@db.example:5432/vyshka?sslmode=require",
			"postgres://vyshka@db.example:5432/vyshka"},
		{"postgresql://vyshka:" + password + "@db.example/vyshka", "postgresql://vyshka@db.example/vyshka"},
		// libpq lets a query parameter carry the password too.
		{"postgres://db.example/vyshka?password=" + password, "postgres://db.example/vyshka"},
		{"postgres://vyshka@db.example/vyshka?sslpassword=" + password, "postgres://vyshka@db.example/vyshka"},
		{"postgres://%zz", "postgres://<unparseable>"},
	}
	for _, c := range cases {
		got := redactPostgresDSN(c.dsn)
		if got != c.want {
			t.Errorf("redact(%q) = %q, want %q", c.dsn, got, c.want)
		}
		if strings.Contains(got, password) {
			t.Errorf("redact(%q) still carries the password", c.dsn)
		}
	}
}
