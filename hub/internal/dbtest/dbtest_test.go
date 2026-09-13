package dbtest

import (
	"net/url"
	"testing"
)

func TestPerTestURLSelectsTheDatabaseByPathAlone(t *testing.T) {
	cases := []struct{ admin, want string }{
		{"postgres://u:p@localhost:5432/postgres", "postgres://u:p@localhost:5432/vyshka_test_x"},
		{"postgres://u:p@localhost/postgres?sslmode=disable", "postgres://u:p@localhost/vyshka_test_x?sslmode=disable"},
		// A database named in the query would override the path.
		{"postgres://localhost/postgres?dbname=postgres&sslmode=disable", "postgres://localhost/vyshka_test_x?sslmode=disable"},
		{"postgres://localhost/postgres?database=postgres", "postgres://localhost/vyshka_test_x"},
	}
	for _, c := range cases {
		admin, err := url.Parse(c.admin)
		if err != nil {
			t.Fatalf("parse %q: %v", c.admin, err)
		}
		if got := perTestURL(admin, "vyshka_test_x"); got != c.want {
			t.Errorf("perTestURL(%q) = %q, want %q", c.admin, got, c.want)
		}
	}
}
