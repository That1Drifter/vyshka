package hub_test

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/That1Drifter/vyshka/hub/internal/dbtest"
)

// The startup log names the database so an operator can tell which one a hub
// booted against, and must do so without the credential in the URL. Graded
// on the Postgres backend, where the URL carries one.
func TestBootLogDescribesPostgresWithoutItsPassword(t *testing.T) {
	if dbtest.Backend() != "postgres" {
		t.Skip("grades the Postgres startup log; run with VYSHKA_TEST_BACKEND=postgres")
	}
	databaseURL := dbtest.URL(t)
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatalf("parse test url: %v", err)
	}
	password, hasPassword := parsed.User.Password()
	if !hasPassword {
		t.Skipf("%s carries no password to redact", dbtest.PostgresURLVar)
	}

	logged := bootLogged(t, databaseURL, testAdminToken)
	if strings.Contains(logged, databaseURL) {
		t.Fatalf("the raw DSN is in the boot log:\n%s", logged)
	}

	var target, driver string
	for _, line := range strings.Split(strings.TrimSpace(logged), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		if record["msg"] == "database ready" {
			target, _ = record["target"].(string)
			driver, _ = record["driver"].(string)
		}
	}
	if driver != "postgres" {
		t.Errorf("logged driver %q, want postgres", driver)
	}
	// The expected description: the same URL with the password and the query
	// gone. The user is compared as a whole, not as a substring, because a
	// user named like its password would otherwise pass a wrong redaction.
	expected := url.URL{Scheme: parsed.Scheme, Host: parsed.Host, Path: parsed.Path, User: url.User(parsed.User.Username())}
	if target != expected.String() {
		t.Errorf("logged target %q, want %q", target, expected.String())
	}
	if strings.Contains(target, ":"+password+"@") {
		t.Errorf("logged target %q carries the password", target)
	}
}
