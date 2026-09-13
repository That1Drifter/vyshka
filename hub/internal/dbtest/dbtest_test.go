package dbtest

import (
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestPerTestURLSelectsTheDatabaseByPathAlone(t *testing.T) {
	cases := []struct{ admin, want string }{
		{"postgres://u:p@localhost:5432/postgres", "postgres://u:p@localhost:5432/vyshka_test_x"},
		{"postgres://u:p@localhost/postgres?sslmode=disable", "postgres://u:p@localhost/vyshka_test_x?sslmode=disable"},
		// A database named in the query would override the path.
		{"postgres://localhost/postgres?dbname=postgres&sslmode=disable", "postgres://localhost/vyshka_test_x?sslmode=disable"},
		{"postgres://localhost/postgres?database=postgres", "postgres://localhost/vyshka_test_x"},
		// So would one whose key is percent-encoded: the driver decodes it.
		{"postgres://localhost/postgres?%64bname=postgres", "postgres://localhost/vyshka_test_x"},
		{"postgres://localhost/postgres?sslmode=disable&%64atabase=postgres", "postgres://localhost/vyshka_test_x?sslmode=disable"},
		// The driver also drops spaces around a key.
		{"postgres://localhost/postgres?%64bname =postgres", "postgres://localhost/vyshka_test_x"},
		{"postgres://localhost/postgres? database=postgres&sslmode=disable", "postgres://localhost/vyshka_test_x?sslmode=disable"},
		// Other parameters keep their bytes and their order: the driver reads
		// %20 as a space but not +, and some keys' precedence is positional.
		{"postgres://localhost/postgres?password=alpha%20beta&dbname=postgres&sslmode=disable&ssl=true",
			"postgres://localhost/vyshka_test_x?password=alpha%20beta&sslmode=disable&ssl=true"},
	}
	for _, c := range cases {
		admin, err := url.Parse(c.admin)
		if err != nil {
			t.Fatalf("parse %q: %v", c.admin, err)
		}
		got := perTestURL(admin, "vyshka_test_x")
		if got != c.want {
			t.Errorf("perTestURL(%q) = %q, want %q", c.admin, got, c.want)
		}
		// The string is one thing; what the driver would connect to is the
		// claim. Parsed the driver's way, the database must be the test's.
		config, err := pgconn.ParseConfig(got)
		if err != nil {
			t.Errorf("the driver rejects %q: %v", got, err)
			continue
		}
		if config.Database != "vyshka_test_x" {
			t.Errorf("the driver would connect %q to database %q, want vyshka_test_x", got, config.Database)
		}
		if err := selectsDatabase(got, "vyshka_test_x"); err != nil {
			t.Errorf("selectsDatabase(%q) = %v, want nil", got, err)
		}
	}
}

// The guard behind the filter: whatever spelling of an override the filter
// misses, the driver's own reading of the URL decides, and a wrong database
// is refused before any test database is created.
func TestSelectsDatabaseRefusesAnOverrideTheFilterMissed(t *testing.T) {
	for _, testURL := range []string{
		"postgres://localhost/vyshka_test_x?dbname=postgres",
		"postgres://localhost/vyshka_test_x?sslmode=disable&database=postgres",
	} {
		if err := selectsDatabase(testURL, "vyshka_test_x"); err == nil {
			t.Errorf("selectsDatabase(%q) accepted a URL the driver would point at postgres", testURL)
		}
	}
	if err := selectsDatabase("postgres://localhost/vyshka_test_x?sslmode=disable", "vyshka_test_x"); err != nil {
		t.Errorf("selectsDatabase refused a clean URL: %v", err)
	}
}
