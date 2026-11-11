package main

// The config file's parsing, mapping, and precedence. These sit in unit tests
// because the refusals and the precedence rungs would each be a separate hub
// boot; scripts/demo-config.sh shows the same behavior on running hubs.

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

// clearEnv blanks every environment variable a serve flag reads, so a test
// sees only what it sets. An empty variable counts as unset.
func clearEnv(t *testing.T) {
	t.Helper()
	flags := flag.NewFlagSet("probe", flag.ContinueOnError)
	if _, err := declareServeFlags(flags); err != nil {
		t.Fatal(err)
	}
	flags.VisitAll(func(f *flag.Flag) { t.Setenv(envName(f.Name), "") })
}

func writeFile(t *testing.T, dir, name, contents string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// resolveServe parses args against a fresh serve flag set and resolves it,
// returning the flag set to read values from.
func resolveServe(t *testing.T, args ...string) (*flag.FlagSet, *settings, error) {
	t.Helper()
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	if _, err := declareServeFlags(flags); err != nil {
		t.Fatal(err)
	}
	if err := flags.Parse(args); err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveSettings(flags, func() (*flag.FlagSet, error) { return flags, nil })
	return flags, resolved, err
}

func value(flags *flag.FlagSet, name string) string { return flags.Lookup(name).Value.String() }

// The env var a flag reads is derived from its name, and its usage text has to
// say so: that is what lets the file layer sit under the environment without
// a hand-kept table, and what keeps a flag added later honest.
func TestEveryServeFlagNamesItsEnvironmentVariable(t *testing.T) {
	flags := flag.NewFlagSet("probe", flag.ContinueOnError)
	if _, err := declareServeFlags(flags); err != nil {
		t.Fatal(err)
	}
	count := 0
	flags.VisitAll(func(f *flag.Flag) {
		count++
		named := regexp.MustCompile(`\(env ` + envName(f.Name) + `[,)]`)
		if !named.MatchString(f.Usage) {
			t.Errorf("flag -%s: usage text does not name (env %s), so the config file layer would put the file above the wrong environment variable:\n%s",
				f.Name, envName(f.Name), f.Usage)
		}
	})
	if count < 10 {
		t.Fatalf("only %d serve flags found", count)
	}
}

// The file layer sits under the environment on the strength of each flag
// reading its own environment variable when it is defined, so a flag whose
// usage names a variable it does not read would let the file win over it.
func TestEveryServeFlagReadsItsEnvironmentVariable(t *testing.T) {
	probe := flag.NewFlagSet("probe", flag.ContinueOnError)
	if _, err := declareServeFlags(probe); err != nil {
		t.Fatal(err)
	}
	clearEnv(t)
	want := map[string]string{}
	probe.VisitAll(func(f *flag.Flag) {
		if f.Name == "config" || secretFlags[f.Name] {
			return
		}
		switch getter, _ := f.Value.(flag.Getter); getter.Get().(type) {
		case bool:
			want[f.Name] = map[bool]string{true: "false", false: "true"}[f.DefValue == "true"]
		case int, int64, uint, uint64:
			want[f.Name] = "41"
		case float64:
			want[f.Name] = "4.5"
		case time.Duration:
			want[f.Name] = "41s"
		default:
			want[f.Name] = "env-" + f.Name
		}
		t.Setenv(envName(f.Name), want[f.Name])
	})
	flags := flag.NewFlagSet("probe", flag.ContinueOnError)
	if _, err := declareServeFlags(flags); err != nil {
		t.Fatal(err)
	}
	for name, text := range want {
		if got := value(flags, name); got != text {
			t.Errorf("-%s is %q with %s=%q: the flag does not read its environment variable", name, got, envName(name), text)
		}
	}
}

// A secret-bearing flag has to be in secretFlags, or it would be printed in
// the effective configuration and skip file: indirection; and a secret flag
// must not carry a printable default.
func TestSecretFlagsAreRegistered(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://u:DEFAULT_SECRET@db/app")
	t.Setenv("VYSHKA_ADMIN_TOKEN", "DEFAULT_SECRET")
	flags := flag.NewFlagSet("probe", flag.ContinueOnError)
	if _, err := declareServeFlags(flags); err != nil {
		t.Fatal(err)
	}
	flags.VisitAll(func(f *flag.Flag) {
		looksSecret := regexp.MustCompile(`token|secret|password|passwd|key|credential|dsn`).MatchString(f.Name)
		if looksSecret && !secretFlags[f.Name] {
			t.Errorf("flag -%s looks secret-bearing but is not in secretFlags", f.Name)
		}
		if secretFlags[f.Name] && f.DefValue != "" {
			t.Errorf("secret flag -%s carries a printable default", f.Name)
		}
	})
	for name := range secretFlags {
		if flags.Lookup(name) == nil {
			t.Errorf("secretFlags names -%s, which is not a serve flag", name)
		}
	}
}

// A serve flag that cannot be set from the file fails here, which is the
// guarantee that a flag added later (rate limits, for one) needs no code of
// its own in the file layer.
func TestEveryServeFlagCanBeSetFromTheFile(t *testing.T) {
	clearEnv(t)
	flags := flag.NewFlagSet("probe", flag.ContinueOnError)
	if _, err := declareServeFlags(flags); err != nil {
		t.Fatal(err)
	}
	var lines []string
	want := map[string]string{}
	flags.VisitAll(func(f *flag.Flag) {
		if f.Name == "config" {
			return
		}
		switch getter, _ := f.Value.(flag.Getter); getter.Get().(type) {
		case bool:
			want[f.Name] = map[bool]string{true: "false", false: "true"}[f.DefValue == "true"]
			lines = append(lines, f.Name+" = "+want[f.Name])
		case int, int64, uint, uint64:
			want[f.Name] = "7"
			if f.DefValue == "7" {
				want[f.Name] = "8"
			}
			lines = append(lines, f.Name+" = "+want[f.Name])
		case float64:
			want[f.Name] = "7.5"
			lines = append(lines, f.Name+" = 7.5")
		case time.Duration:
			want[f.Name] = "7s"
			lines = append(lines, f.Name+` = "7s"`)
		default:
			want[f.Name] = "from-file-" + f.Name
			lines = append(lines, f.Name+` = "`+want[f.Name]+`"`)
		}
	})
	path := writeFile(t, t.TempDir(), "hub.toml", strings.Join(lines, "\n")+"\n", 0o600)

	set, resolved, err := resolveServe(t, "-config", path)
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range want {
		if got := value(set, name); got != text {
			t.Errorf("-%s = %q from the file, want %q", name, got, text)
		}
		if resolved.sources[name] != "file" {
			t.Errorf("-%s source = %q, want file", name, resolved.sources[name])
		}
	}
}

// Each rung of flag > environment > file > default, for a plain setting, an
// integer, and a secret.
func TestPrecedenceLadder(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	path := writeFile(t, dir, "hub.toml", "addr = \"10.0.0.1:1\"\nbackup-keep = 3\nadmin-token = \"vya_from_file\"\n", 0o600)

	cases := []struct {
		name       string
		env        map[string]string
		args       []string
		addr       string
		addrSource string
		keep       string
		token      string
		tokenFrom  string
	}{
		{"default", nil, nil, "127.0.0.1:8080", "default", "7", "", "default"},
		{"file", nil, []string{"-config", path}, "10.0.0.1:1", "file", "3", "vya_from_file", "file"},
		{"env over file",
			map[string]string{"VYSHKA_ADDR": "10.0.0.2:2", "VYSHKA_BACKUP_KEEP": "4", "VYSHKA_ADMIN_TOKEN": "vya_from_env"},
			[]string{"-config", path}, "10.0.0.2:2", "env VYSHKA_ADDR", "4", "vya_from_env", "env VYSHKA_ADMIN_TOKEN"},
		{"flag over env over file",
			map[string]string{"VYSHKA_ADDR": "10.0.0.2:2", "VYSHKA_BACKUP_KEEP": "4", "VYSHKA_ADMIN_TOKEN": "vya_from_env"},
			[]string{"-config", path, "-addr", "10.0.0.3:3", "-backup-keep", "5", "-admin-token", "vya_from_flag"},
			"10.0.0.3:3", "flag", "5", "vya_from_flag", "flag"},
		// An explicitly empty flag is an answer, not an absence.
		{"empty flag over env over file", map[string]string{"VYSHKA_ADMIN_TOKEN": "vya_from_env"},
			[]string{"-config", path, "-admin-token="}, "10.0.0.1:1", "file", "3", "", "flag"},
		// An empty environment variable is no variable at all.
		{"empty env does not mask the file", map[string]string{"VYSHKA_ADDR": ""},
			[]string{"-config", path}, "10.0.0.1:1", "file", "3", "vya_from_file", "file"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			set, resolved, err := resolveServe(t, c.args...)
			if err != nil {
				t.Fatal(err)
			}
			if got := value(set, "addr"); got != c.addr || resolved.sources["addr"] != c.addrSource {
				t.Errorf("addr = %q from %q, want %q from %q", got, resolved.sources["addr"], c.addr, c.addrSource)
			}
			if got := value(set, "backup-keep"); got != c.keep {
				t.Errorf("backup-keep = %q, want %q", got, c.keep)
			}
			if got := value(set, "admin-token"); got != c.token || resolved.sources["admin-token"] != c.tokenFrom {
				t.Errorf("admin-token = %q from %q, want %q from %q", got, resolved.sources["admin-token"], c.token, c.tokenFrom)
			}
		})
	}
}

// The file is named by -config or VYSHKA_CONFIG and nowhere else: a file
// that merely sits in the working directory is never read.
func TestNoImplicitConfigFile(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	for _, name := range []string{"hub.toml", "vyshka.toml", "config.toml", "vyshka-hub.toml"} {
		writeFile(t, dir, name, "addr = \"10.9.9.9:9\"\n", 0o600)
	}
	t.Chdir(dir)
	set, resolved, err := resolveServe(t)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.configPath != "" || value(set, "addr") != "127.0.0.1:8080" {
		t.Errorf("an unnamed file was read: config %q, addr %q", resolved.configPath, value(set, "addr"))
	}

	t.Setenv("VYSHKA_CONFIG", filepath.Join(dir, "hub.toml"))
	set, resolved, err = resolveServe(t)
	if err != nil {
		t.Fatal(err)
	}
	if value(set, "addr") != "10.9.9.9:9" || resolved.configFrom != "env VYSHKA_CONFIG" {
		t.Errorf("VYSHKA_CONFIG was not honored: addr %q, named by %q", value(set, "addr"), resolved.configFrom)
	}
	// An explicit -config= switches the file off, as an explicit empty flag
	// wins over the environment everywhere else.
	if _, resolved, err = resolveServe(t, "-config="); err != nil || resolved.configPath != "" {
		t.Errorf("-config= did not win over VYSHKA_CONFIG: %q, %v", resolved.configPath, err)
	}
}

// A bad file refuses to start, names the file and the key, and never carries
// a value: every file below holds a marker value that must not come back.
func TestBadConfigFilesNameTheKeyAndNoValue(t *testing.T) {
	clearEnv(t)
	cases := []struct {
		name     string
		contents string
		args     []string
		env      map[string]string
		want     []string
	}{
		{"unknown key", "adress = \"MARKER_VALUE\"\n", nil, nil, []string{`unknown key "adress"`}},
		{"several unknown keys", "b = \"MARKER_VALUE\"\na = 1\n", nil, nil, []string{`unknown keys "a", "b"`}},
		{"a table is not a key", "[addr]\nx = \"MARKER_VALUE\"\n", nil, nil, []string{`key "addr" is a table, want a string`}},
		{"string for an integer", "backup-keep = \"MARKER_VALUE\"\n", nil, nil, []string{`key "backup-keep" is a string, want an integer`}},
		{"integer for a string", "addr = 8080\n", nil, nil, []string{`key "addr" is an integer, want a string`}},
		{"string for a boolean", "panel = \"MARKER_VALUE\"\n", nil, nil, []string{`key "panel" is a string, want a boolean`}},
		{"integer for a secret", "admin-token = 12345\n", nil, nil, []string{`key "admin-token" is an integer, want a string`}},
		{"wrong type even when the environment overrides it", "backup-keep = \"MARKER_VALUE\"\n", nil,
			map[string]string{"VYSHKA_BACKUP_KEEP": "4"}, []string{`key "backup-keep" is a string`}},
		{"array for an ordinary string", "addr = [\"MARKER_VALUE\"]\n", nil, nil, []string{`key "addr" is an array, want a string`}},
		{"array of integers", "auto-tls-hosts = [1]\n", nil, nil, []string{`key "auto-tls-hosts" is an array holding an integer, want strings`}},
		{"comma inside an array element", "auto-tls-hosts = [\"a.example.org,MARKER_VALUE\"]\n", nil, nil,
			[]string{`key "auto-tls-hosts" has an array element with a comma`}},
		{"duplicate key", "addr = \"MARKER_VALUE\"\naddr = \"MARKER_VALUE\"\n", nil, nil, []string{`line 2`, `key "addr" is set twice`}},
		{"a secret without quotes", "admin-token = vya_MARKER_VALUE\n", nil, nil, []string{`line 1 is not valid TOML`}},
		{"a file cannot name a file", "config = \"MARKER_VALUE\"\n", nil, nil, []string{`unknown key "config"`}},
		{"an integer too large for TOML", "backup-keep = 99999999999999999999\n", nil, nil, []string{`not valid TOML`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := writeFile(t, t.TempDir(), "hub.toml", c.contents, 0o600)
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			_, _, err := resolveServe(t, append([]string{"-config", path}, c.args...)...)
			if err == nil {
				t.Fatal("the file was accepted")
			}
			msg := err.Error()
			if !strings.Contains(msg, path) {
				t.Errorf("error does not name the file %s: %s", path, msg)
			}
			for _, want := range c.want {
				if !strings.Contains(msg, want) {
					t.Errorf("error does not contain %q: %s", want, msg)
				}
			}
			if rest := strings.ReplaceAll(msg, path, ""); strings.Contains(rest, "MARKER_VALUE") || strings.Contains(rest, "12345") {
				t.Errorf("error carries a value: %s", msg)
			}
		})
	}

	t.Run("a missing file", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "nope.toml")
		if _, _, err := resolveServe(t, "-config", missing); err == nil || !strings.Contains(err.Error(), missing) {
			t.Errorf("want an error naming %s, got %v", missing, err)
		}
	})
}

// Types are decided by the flag's own value, so a flag of another ordinary
// type needs no code here. The serve set has none of these types today; a
// private set stands in for what a later slice (rate limits) will add.
func TestValueTypesFollowTheFlag(t *testing.T) {
	clearEnv(t)
	build := func() (*flag.FlagSet, *time.Duration, *float64, *int) {
		flags := flag.NewFlagSet("serve", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		configFlag(flags)
		window := flags.Duration("rate-window", time.Second, "window (env VYSHKA_RATE_WINDOW)")
		share := flags.Float64("rate-share", 0.5, "share (env VYSHKA_RATE_SHARE)")
		burst := flags.Int("rate-burst", 10, "burst (env VYSHKA_RATE_BURST)")
		return flags, window, share, burst
	}
	resolveWith := func(contents string, env map[string]string) (*time.Duration, *float64, *int, error) {
		flags, window, share, burst := build()
		path := writeFile(t, t.TempDir(), "hub.toml", contents, 0o600)
		if err := flags.Parse([]string{"-config", path}); err != nil {
			t.Fatal(err)
		}
		for k, v := range env {
			t.Setenv(k, v)
		}
		_, err := resolveSettings(flags, func() (*flag.FlagSet, error) { return flags, nil })
		return window, share, burst, err
	}

	window, share, burst, err := resolveWith("rate-window = \"30s\"\nrate-share = 2\nrate-burst = 25\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if *window != 30*time.Second || *share != 2 || *burst != 25 {
		t.Errorf("got %v, %v, %v", *window, *share, *burst)
	}
	if _, _, _, err := resolveWith("rate-window = 30\n", nil); err == nil || !strings.Contains(err.Error(), `want a string such as "30s"`) {
		t.Errorf("an integer duration: %v", err)
	}
	if _, _, _, err := resolveWith("rate-window = \"soon\"\n", nil); err == nil ||
		!strings.Contains(err.Error(), "not a valid value") || strings.Contains(err.Error(), "soon") {
		t.Errorf("a bad duration: %v", err)
	}
	if _, _, _, err := resolveWith("rate-burst = 2.5\n", nil); err == nil || !strings.Contains(err.Error(), "is a float, want an integer") {
		t.Errorf("a float for an integer: %v", err)
	}
	if _, _, _, err := resolveWith("rate-share = true\n", nil); err == nil || !strings.Contains(err.Error(), "want a number") {
		t.Errorf("a boolean for a float: %v", err)
	}
}

func TestAutoTLSHostsAcceptsAnArrayAndTheCommaString(t *testing.T) {
	clearEnv(t)
	for contents, want := range map[string]string{
		"auto-tls-hosts = [\"a.example.org\", \"b.example.org\"]\n": "a.example.org,b.example.org",
		"auto-tls-hosts = \"a.example.org,b.example.org\"\n":        "a.example.org,b.example.org",
	} {
		path := writeFile(t, t.TempDir(), "hub.toml", contents, 0o600)
		set, _, err := resolveServe(t, "-config", path)
		if err != nil {
			t.Fatal(err)
		}
		if got := value(set, "auto-tls-hosts"); got != want {
			t.Errorf("%q gave %q, want %q", contents, got, want)
		}
	}
}

// Secrets take file: from every source. From the config file a relative path
// is anchored to the file's directory; from a flag or the environment it
// stays relative to the working directory, as it always was.
func TestSecretFileIndirection(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	elsewhere := t.TempDir()
	writeFile(t, dir, "token.txt", "  vya_from_secret_file \r\n\n", 0o600)
	writeFile(t, dir, "dsn.txt", "postgres://u:pw@db/app\n", 0o600)
	writeFile(t, dir, "empty.txt", " \n", 0o600)
	writeFile(t, elsewhere, "cwd-token.txt", "vya_from_cwd\n", 0o600)

	t.Run("relative path in the file is relative to the file", func(t *testing.T) {
		path := writeFile(t, dir, "hub.toml", "admin-token = \"file:token.txt\"\ndb = \"file:dsn.txt\"\n", 0o600)
		t.Chdir(elsewhere)
		set, resolved, err := resolveServe(t, "-config", path)
		if err != nil {
			t.Fatal(err)
		}
		token, err := resolved.secret("admin-token", value(set, "admin-token"))
		if err != nil || token != "vya_from_secret_file" {
			t.Errorf("token = %q, %v (surrounding whitespace and newlines are trimmed)", token, err)
		}
		dsn, err := resolved.secret("db", value(set, "db"))
		if err != nil || dsn != "postgres://u:pw@db/app" {
			t.Errorf("db = %q, %v", dsn, err)
		}
	})
	t.Run("relative path from the environment or a flag is relative to the working directory", func(t *testing.T) {
		t.Chdir(elsewhere)
		t.Setenv("VYSHKA_ADMIN_TOKEN", "file:cwd-token.txt")
		set, resolved, err := resolveServe(t)
		if err != nil {
			t.Fatal(err)
		}
		if token, err := resolved.secret("admin-token", value(set, "admin-token")); err != nil || token != "vya_from_cwd" {
			t.Errorf("env: token = %q, %v", token, err)
		}
		set, resolved, err = resolveServe(t, "-admin-token", "file:cwd-token.txt")
		if err != nil {
			t.Fatal(err)
		}
		if token, err := resolved.secret("admin-token", value(set, "admin-token")); err != nil || token != "vya_from_cwd" {
			t.Errorf("flag: token = %q, %v", token, err)
		}
	})
	t.Run("an empty or missing secret file is refused, naming the setting, the source, and the path", func(t *testing.T) {
		for name, ref := range map[string]string{"empty": "empty.txt", "missing": "no-such-file.txt"} {
			path := writeFile(t, dir, "bad-"+name+".toml", "admin-token = \"file:"+ref+"\"\n", 0o600)
			set, resolved, err := resolveServe(t, "-config", path)
			if err != nil {
				t.Fatal(err)
			}
			_, err = resolved.secret("admin-token", value(set, "admin-token"))
			if err == nil {
				t.Fatalf("%s: accepted", name)
			}
			for _, want := range []string{"admin-token", "config file " + path, ref} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("%s: error does not contain %q: %v", name, want, err)
				}
			}
		}
	})
}

func TestLooseModes(t *testing.T) {
	for mode, loose := range map[os.FileMode]bool{0o600: false, 0o400: false, 0o640: true, 0o604: true, 0o644: true, 0o660: true, 0o700: false} {
		if looseMode(mode) != loose {
			t.Errorf("looseMode(%04o) = %v", mode, !loose)
		}
	}
}

func TestInlineSecretInALooseFileWarns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no group or other permission bits")
	}
	clearEnv(t)
	dir := t.TempDir()
	loose := writeFile(t, dir, "loose.toml", "admin-token = \"vya_inline\"\ndb = \"file:dsn.txt\"\n", 0o644)
	tight := writeFile(t, dir, "tight.toml", "admin-token = \"vya_inline\"\n", 0o600)
	if err := os.Chmod(loose, 0o644); err != nil {
		t.Fatal(err)
	}
	_, resolved, err := resolveServe(t, "-config", loose)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved.warnings) != 1 || resolved.warnings[0].key != "admin-token" || resolved.warnings[0].file != loose {
		t.Errorf("warnings = %+v, want one for admin-token in %s (db is a file: reference)", resolved.warnings, loose)
	}
	if _, resolved, _ = resolveServe(t, "-config", tight); len(resolved.warnings) != 0 {
		t.Errorf("a 0600 file warned: %+v", resolved.warnings)
	}
}

// tableRow returns a setting's row of the config output with its columns
// separated by single spaces, minus the name.
func tableRow(out, name string) string {
	for _, line := range strings.Split(out, "\n") {
		if fields := strings.Fields(line); len(fields) > 1 && fields[0] == name {
			return strings.Join(fields[1:], " ")
		}
	}
	return ""
}

// capture runs fn with stdout and stderr redirected and returns what was
// written to each.
func capture(t *testing.T, fn func() error) (stdout, stderr string, err error) {
	t.Helper()
	read := func() (*os.File, *os.File, *[]byte, chan struct{}) {
		r, w, perr := os.Pipe()
		if perr != nil {
			t.Fatal(perr)
		}
		data := new([]byte)
		done := make(chan struct{})
		go func() { *data, _ = io.ReadAll(r); close(done) }()
		return r, w, data, done
	}
	_, outW, outData, outDone := read()
	_, errW, errData, errDone := read()
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	err = fn()
	os.Stdout, os.Stderr = oldOut, oldErr
	outW.Close()
	errW.Close()
	<-outDone
	<-errDone
	return string(*outData), string(*errData), err
}

// The effective-configuration output never carries a secret, from any source,
// and the control proves the check can fail: with redaction switched off the
// same run does print them.
func TestConfigCommandRedactsEverySecret(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	path := writeFile(t, dir, "hub.toml", ""+
		"db = \"postgres://vyshka:SECRET_DB_PASSWORD@db.example:5432/vyshka\"\n"+
		"admin-token = \"vya_SECRET_FILE_TOKEN\"\n"+
		"log-level = \"debug\"\n", 0o600)
	t.Setenv("VYSHKA_LOG_LEVEL", "warn")
	t.Setenv("VYSHKA_ADMIN_TOKEN", "vya_SECRET_ENV_TOKEN")
	args := []string{"-config", path, "-addr", "127.0.0.1:9000", "-panel=false"}
	secrets := []string{"SECRET_DB_PASSWORD", "vya_SECRET_FILE_TOKEN", "vya_SECRET_ENV_TOKEN"}

	out, errOut, err := capture(t, func() error { return runConfig(args) })
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range secrets {
		if strings.Contains(out+errOut, secret) {
			t.Errorf("the output carries %q:\n%s%s", secret, out, errOut)
		}
	}
	for row, want := range map[string]string{
		"db":          "<redacted> file",
		"admin-token": "<redacted> env VYSHKA_ADMIN_TOKEN",
		"addr":        "127.0.0.1:9000 flag",
		"log-level":   "warn env VYSHKA_LOG_LEVEL",
		"panel":       "false flag",
		"backup-keep": "7 default",
	} {
		if got := tableRow(out, row); got != want {
			t.Errorf("row %s = %q, want %q\n%s", row, got, want, out)
		}
	}
	for _, want := range []string{
		"serve would listen on 127.0.0.1:9000",
		"config file: " + path + " (from flag)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the output lacks %q:\n%s", want, out)
		}
	}

	// Negative control: the assertion above is not vacuous.
	redactSecrets = false
	defer func() { redactSecrets = true }()
	leaked, _, err := capture(t, func() error { return runConfig(args) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(leaked, "vya_SECRET_ENV_TOKEN") {
		t.Errorf("with redaction off the secret should print, so the redaction check can fail:\n%s", leaked)
	}
}

// A file: reference prints as the reference, and an unreadable one fails the
// check the way it would fail serve.
func TestConfigCommandChecksSecretFiles(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	writeFile(t, dir, "token.txt", "vya_NEVER_PRINTED\n", 0o600)
	good := writeFile(t, dir, "good.toml", "admin-token = \"file:token.txt\"\n", 0o600)
	out, _, err := capture(t, func() error { return runConfig([]string{"-config", good}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "file:"+filepath.Join(dir, "token.txt")) || strings.Contains(out, "vya_NEVER_PRINTED") {
		t.Errorf("the secret reference should print and the secret not:\n%s", out)
	}

	bad := writeFile(t, dir, "bad.toml", "admin-token = \"file:missing.txt\"\n", 0o600)
	if _, _, err := capture(t, func() error { return runConfig([]string{"-config", bad}) }); err == nil ||
		!strings.Contains(err.Error(), "admin-token") || !strings.Contains(err.Error(), "missing.txt") {
		t.Errorf("an unreadable secret file passed the check: %v", err)
	}
	// The check runs serve's validation, including the auto-TLS combinations.
	incoherent := writeFile(t, dir, "tls.toml", "auto-tls-email = \"ops@example.org\"\n", 0o600)
	if _, _, err := capture(t, func() error { return runConfig([]string{"-config", incoherent}) }); err == nil ||
		!strings.Contains(err.Error(), "-auto-tls-email set without -auto-tls") {
		t.Errorf("the auto-TLS check did not run: %v", err)
	}
}

// With a file naming the secrets, usage text still carries none of them.
func TestUsageTextCarriesNoSecretsFromTheFile(t *testing.T) {
	clearEnv(t)
	path := writeFile(t, t.TempDir(), "hub.toml",
		"db = \"postgres://u:FILE_SECRET_PW@db/app\"\nadmin-token = \"vya_FILE_SECRET_TOKEN\"\n", 0o600)
	t.Setenv("VYSHKA_CONFIG", path)
	_, usage, err := capture(t, func() error { return runServe([]string{"-h"}) })
	if err != flag.ErrHelp {
		t.Fatalf("serve -h returned %v", err)
	}
	if !strings.Contains(usage, "-config") || strings.Contains(usage, "FILE_SECRET") {
		t.Errorf("usage text:\n%s", usage)
	}
}

// The storage and vacuum commands take the same file for db, and hold its
// other keys to serve's names and types.
func TestMaintenanceCommandsReadDBFromTheFile(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	path := writeFile(t, dir, "hub.toml", "db = \"from-file.db\"\naddr = \"127.0.0.1:1\"\n", 0o600)
	flags, _ := maintenanceFlags("storage")
	if err := flags.Parse([]string{"-config", path}); err != nil {
		t.Fatal(err)
	}
	if dsn, err := maintenanceDSN(flags); err != nil || dsn != "from-file.db" {
		t.Errorf("db = %q, %v", dsn, err)
	}

	t.Setenv("DATABASE_URL", "from-env.db")
	flags, _ = maintenanceFlags("vacuum")
	_ = flags.Parse([]string{"-config", path})
	if dsn, err := maintenanceDSN(flags); err != nil || dsn != "from-env.db" {
		t.Errorf("env over file: db = %q, %v", dsn, err)
	}
	flags, _ = maintenanceFlags("vacuum")
	_ = flags.Parse([]string{"-config", path, "-db", "from-flag.db"})
	if dsn, err := maintenanceDSN(flags); err != nil || dsn != "from-flag.db" {
		t.Errorf("flag over env over file: db = %q, %v", dsn, err)
	}

	// A typo must not fall back to the default database.
	t.Setenv("DATABASE_URL", "")
	typo := writeFile(t, dir, "typo.toml", "dbb = \"from-file.db\"\n", 0o600)
	flags, _ = maintenanceFlags("storage")
	_ = flags.Parse([]string{"-config", typo})
	if _, err := maintenanceDSN(flags); err == nil || !strings.Contains(err.Error(), `unknown key "dbb"`) {
		t.Errorf("a typo in the shared file: %v", err)
	}
	wrongType := writeFile(t, dir, "type.toml", "db = \"x.db\"\nbackup-keep = \"seven\"\n", 0o600)
	flags, _ = maintenanceFlags("storage")
	_ = flags.Parse([]string{"-config", wrongType})
	if _, err := maintenanceDSN(flags); err == nil || !strings.Contains(err.Error(), `key "backup-keep" is a string`) {
		t.Errorf("a wrong type in a key the command ignores: %v", err)
	}
}

// deploy/hub.toml.example documents every serve setting, and can be loaded
// with every line uncommented: it cannot rot when a flag is added or renamed.
func TestExampleFileCoversEveryServeSetting(t *testing.T) {
	clearEnv(t)
	example, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy", "hub.toml.example"))
	if err != nil {
		t.Fatal(err)
	}
	assignment := regexp.MustCompile(`^#?\s*([a-z][a-z0-9-]*)\s*=`)
	documented := map[string]bool{}
	var uncommented []string
	for _, line := range strings.Split(string(example), "\n") {
		match := assignment.FindStringSubmatch(line)
		if match == nil {
			uncommented = append(uncommented, line)
			continue
		}
		if documented[match[1]] {
			t.Errorf("hub.toml.example assigns %q twice", match[1])
		}
		documented[match[1]] = true
		uncommented = append(uncommented, strings.TrimLeft(strings.TrimPrefix(line, "#"), " "))
	}
	flags := flag.NewFlagSet("probe", flag.ContinueOnError)
	if _, err := declareServeFlags(flags); err != nil {
		t.Fatal(err)
	}
	flags.VisitAll(func(f *flag.Flag) {
		if f.Name != "config" && !documented[f.Name] {
			t.Errorf("deploy/hub.toml.example does not document -%s", f.Name)
		}
	})
	for name := range documented {
		if flags.Lookup(name) == nil {
			t.Errorf("deploy/hub.toml.example documents %q, which is not a serve flag", name)
		}
	}

	path := writeFile(t, t.TempDir(), "all.toml", strings.Join(uncommented, "\n"), 0o600)
	_, resolved, err := resolveServe(t, "-config", path)
	if err != nil {
		t.Fatalf("the example does not load with every setting uncommented: %v", err)
	}
	for name := range documented {
		if resolved.sources[name] != "file" {
			t.Errorf("-%s was not taken from the example", name)
		}
	}
}
