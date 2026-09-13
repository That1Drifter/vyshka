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
		if got := perTestURL(admin, "vyshka_test_x"); got != c.want {
			t.Errorf("perTestURL(%q) = %q, want %q", c.admin, got, c.want)
		}
	}
}
