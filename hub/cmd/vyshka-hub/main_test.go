package main

import (
	"flag"
	"io"
	"os"
	"strings"
	"testing"
)

func TestEnvFallbackYieldsToAnExplicitFlag(t *testing.T) {
	t.Setenv("VYSHKA_TEST_FALLBACK", "from-env")

	cases := []struct {
		args []string
		want string
	}{
		{nil, "from-env"},
		{[]string{"-x=explicit"}, "explicit"},
		// An explicitly empty flag is an answer, not an absence.
		{[]string{"-x="}, ""},
	}
	for _, c := range cases {
		flags := flag.NewFlagSet("test", flag.ContinueOnError)
		value := flags.String("x", "", "test flag (env VYSHKA_TEST_FALLBACK)")
		if err := flags.Parse(c.args); err != nil {
			t.Fatalf("parse %v: %v", c.args, err)
		}
		envFallback(flags, "x", "VYSHKA_TEST_FALLBACK", value)
		if *value != c.want {
			t.Errorf("args %v: value = %q, want %q", c.args, *value, c.want)
		}
	}
}

// The usage text prints flag defaults, so the secret-bearing flags must not
// carry the environment as their default. The flag set writes to the
// process's stderr, so that is what is captured.
func TestUsageTextCarriesNoSecrets(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://u:USAGE_SECRET_PW@db/app")
	t.Setenv("VYSHKA_ADMIN_TOKEN", "vya_USAGE_SECRET_TOKEN")

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	stderr := os.Stderr
	os.Stderr = writer
	runErr := runServe([]string{"-h"})
	os.Stderr = stderr
	writer.Close()
	usage, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read usage: %v", err)
	}

	if runErr != flag.ErrHelp {
		t.Fatalf("serve -h returned %v, want flag.ErrHelp", runErr)
	}
	if !strings.Contains(string(usage), "-db") {
		t.Fatalf("usage text did not reach stderr:\n%s", usage)
	}
	if strings.Contains(string(usage), "USAGE_SECRET") {
		t.Errorf("usage text carries a secret from the environment:\n%s", usage)
	}
}
