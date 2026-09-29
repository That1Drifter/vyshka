package cli

import (
	"encoding/json"
	"flag"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/That1Drifter/vyshka/client"
)

func TestVersionNeedsNoHub(t *testing.T) {
	t.Parallel()
	h := &testHub{t: t, configDir: t.TempDir()}

	got := h.runWith(runOptions{env: map[string]string{"VYSHKA_URL": "", "VYSHKA_TOKEN": ""}}, "version")
	if got.code != ExitOK {
		t.Fatalf("version: %v", got)
	}
	want := client.Version + "\nprotocol draft " + client.ProtocolDraft + "\n"
	if got.stdout != want {
		t.Errorf("stdout = %q, want %q", got.stdout, want)
	}

	got = h.runWith(runOptions{env: map[string]string{"VYSHKA_URL": "", "VYSHKA_TOKEN": ""}}, "version", "--json")
	var decoded map[string]string
	if err := json.Unmarshal([]byte(got.stdout), &decoded); err != nil {
		t.Fatalf("version --json: %v\n%v", err, got)
	}
	if decoded["version"] != client.Version || decoded["protocolDraft"] != client.ProtocolDraft {
		t.Errorf("version --json = %v", decoded)
	}
}

func TestHelp(t *testing.T) {
	t.Parallel()
	h := &testHub{t: t, configDir: t.TempDir()}

	top := h.run("help")
	if top.code != ExitOK {
		t.Fatalf("help: %v", top)
	}
	for _, want := range []string{
		"Exit codes", "VYSHKA_URL", "VYSHKA_TOKEN", "VYSHKA_PROFILE", "VYSHKA_CONFIG",
		`"profiles"`, "--http-timeout", "6  --wait ran out of time",
	} {
		if !strings.Contains(top.stdout, want) {
			t.Errorf("help lacks %q", want)
		}
	}
	for name := range commands {
		if !strings.Contains(top.stdout, "\n  "+name+" ") {
			t.Errorf("help does not list the %s command", name)
		}
		if _, ok := helpTexts[name]; !ok {
			t.Errorf("command %s has no help text", name)
		}
	}

	for _, args := range [][]string{{"help", "run"}, {"run", "-h"}, {"run", "SERVER", "--help"}} {
		got := h.run(args...)
		if got.code != ExitOK || !strings.Contains(got.stdout, "--idempotency-key") {
			t.Errorf("%v: %v", args, got)
		}
	}
	if got := h.run("--help"); got.code != ExitOK || !strings.Contains(got.stdout, "Exit codes") {
		t.Errorf("--help: %v", got)
	}
	if got := h.run("help", "nope"); got.code != ExitUsage {
		t.Errorf("help nope: %v", got)
	}
	if got := h.run(); got.code != ExitUsage || !strings.Contains(got.stderr, "a command is required") {
		t.Errorf("no command: %v", got)
	}
	if got := h.run("frobnicate"); got.code != ExitUsage || !strings.Contains(got.stderr, `unknown command "frobnicate"`) {
		t.Errorf("unknown command: %v", got)
	}
}

// The token must never reach the terminal: not through help, which prints
// flag defaults, and not through an error, which a user pastes into an issue.
func TestTokenIsNeverPrinted(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	const secret = "vya_LEAKCHECKSECRETLEAKCHECKSECRET"
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadURL := "http://" + listener.Addr().String()
	listener.Close()

	env := map[string]string{"VYSHKA_TOKEN": secret}
	runs := []struct {
		name string
		env  map[string]string
		args []string
		code int
	}{
		{"help", env, []string{"help"}, ExitOK},
		{"help run", env, []string{"help", "run"}, ExitOK},
		{"-h", env, []string{"-h"}, ExitOK},
		{"flag token then -h", nil, []string{"--token", secret, "servers", "-h"}, ExitOK},
		{"flag token after the command, -h", nil, []string{"run", "--token=" + secret, "-h"}, ExitOK},
		{"bad flag with the token set", env, []string{"servers", "--bogus"}, ExitUsage},
		{"wrong token", env, []string{"servers"}, ExitRefused},
		{"wrong token from a file", map[string]string{"VYSHKA_TOKEN": "file:" + tokenFile}, []string{"servers"}, ExitRefused},
		{"wrong token, JSON mode", env, []string{"servers", "--json"}, ExitRefused},
		{"dead hub", map[string]string{"VYSHKA_TOKEN": secret, "VYSHKA_URL": deadURL},
			[]string{"servers", "--http-timeout", "1s"}, ExitTransport},
		{"bad URL", map[string]string{"VYSHKA_TOKEN": secret, "VYSHKA_URL": "ftp://example"}, []string{"servers"}, ExitUsage},
	}
	for _, run := range runs {
		got := h.runWith(runOptions{env: run.env}, run.args...)
		if got.code != run.code {
			t.Errorf("%s: %v\nwant exit %d", run.name, got, run.code)
		}
		if strings.Contains(got.stdout+got.stderr, secret) || strings.Contains(got.stdout+got.stderr, "LEAKCHECK") {
			t.Errorf("%s: the token reached the output:\n%v", run.name, got)
		}
	}
}

func TestConfigPrecedence(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	h.createServer("Precedence")

	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("  "+testToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := map[string]any{
		"default": "home",
		"profiles": map[string]any{
			"home":  map[string]string{"url": h.url, "token": "file:" + tokenFile},
			"wrong": map[string]string{"url": h.url, "token": "vya_WRONG"},
		},
	}
	encoded, _ := json.Marshal(config)
	if err := os.MkdirAll(filepath.Join(h.configDir, "vyshka"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.configDir, "vyshka", "config.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	noEnv := map[string]string{"VYSHKA_URL": "", "VYSHKA_TOKEN": ""}

	// The default profile, with its file: token, is enough on its own.
	if got := h.runWith(runOptions{env: noEnv}, "servers"); got.code != ExitOK || !strings.Contains(got.stdout, "Precedence") {
		t.Errorf("profile alone: %v", got)
	}
	// --profile picks another one.
	if got := h.runWith(runOptions{env: noEnv}, "--profile", "wrong", "servers"); got.code != ExitRefused {
		t.Errorf("--profile wrong: %v", got)
	}
	// VYSHKA_PROFILE does too, and the flag beats it.
	if got := h.runWith(runOptions{env: map[string]string{"VYSHKA_URL": "", "VYSHKA_TOKEN": "", "VYSHKA_PROFILE": "wrong"}},
		"servers", "--profile", "home"); got.code != ExitOK {
		t.Errorf("--profile over VYSHKA_PROFILE: %v", got)
	}
	// The environment beats the profile.
	if got := h.runWith(runOptions{env: map[string]string{"VYSHKA_URL": "", "VYSHKA_TOKEN": "vya_WRONG"}}, "servers"); got.code != ExitRefused {
		t.Errorf("env over profile: %v", got)
	}
	// The flag beats the environment.
	if got := h.runWith(runOptions{env: map[string]string{"VYSHKA_URL": "", "VYSHKA_TOKEN": "vya_WRONG"}},
		"servers", "--token", testToken); got.code != ExitOK {
		t.Errorf("flag over env: %v", got)
	}
	// An unknown profile is an error that names the ones there are.
	got := h.runWith(runOptions{env: noEnv}, "servers", "--profile", "nope")
	if got.code != ExitUsage || !strings.Contains(got.stderr, "home, wrong") {
		t.Errorf("unknown profile: %v", got)
	}
	// --config names another file; a malformed one is a usage error.
	broken := filepath.Join(t.TempDir(), "broken.json")
	if err := os.WriteFile(broken, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := h.runWith(runOptions{env: noEnv}, "servers", "--config", broken); got.code != ExitUsage ||
		!strings.Contains(got.stderr, "is not valid") {
		t.Errorf("malformed config: %v", got)
	}
	// VYSHKA_CONFIG naming a missing file is no file at all.
	if got := h.runWith(runOptions{env: map[string]string{"VYSHKA_URL": "", "VYSHKA_TOKEN": "",
		"VYSHKA_CONFIG": filepath.Join(t.TempDir(), "absent.json")}}, "servers"); got.code != ExitUsage ||
		!strings.Contains(got.stderr, "no hub URL") {
		t.Errorf("missing config file: %v", got)
	}
}

func TestOnlyProfileIsUsedWithoutADefault(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	path := filepath.Join(t.TempDir(), "config.json")
	encoded, _ := json.Marshal(map[string]any{"profiles": map[string]any{
		"solo": map[string]string{"url": h.url, "token": testToken},
	}})
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	got := h.runWith(runOptions{env: map[string]string{"VYSHKA_URL": "", "VYSHKA_TOKEN": "", "VYSHKA_CONFIG": path}}, "servers")
	if got.code != ExitOK {
		t.Errorf("the only profile was not used: %v", got)
	}
}

func TestMissingURLOrTokenNamesWhereToSetThem(t *testing.T) {
	t.Parallel()
	h := &testHub{t: t, configDir: t.TempDir()}

	got := h.runWith(runOptions{env: map[string]string{"VYSHKA_URL": "", "VYSHKA_TOKEN": "vya_x"}}, "servers")
	if got.code != ExitUsage || !strings.Contains(got.stderr, "--url") || !strings.Contains(got.stderr, "VYSHKA_URL") {
		t.Errorf("missing URL: %v", got)
	}
	got = h.runWith(runOptions{env: map[string]string{"VYSHKA_URL": "http://127.0.0.1:1", "VYSHKA_TOKEN": ""}}, "servers")
	if got.code != ExitUsage || !strings.Contains(got.stderr, "--token") || !strings.Contains(got.stderr, "VYSHKA_TOKEN") {
		t.Errorf("missing token: %v", got)
	}
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got = h.runWith(runOptions{env: map[string]string{"VYSHKA_URL": "http://127.0.0.1:1", "VYSHKA_TOKEN": "file:" + empty}}, "servers")
	if got.code != ExitUsage || !strings.Contains(got.stderr, "is empty") {
		t.Errorf("empty token file: %v", got)
	}
}

func TestExitCodesForRefusalAndTransport(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)

	got := h.runWith(runOptions{env: map[string]string{"VYSHKA_TOKEN": "vya_NOTTHETOKEN"}}, "servers")
	if got.code != ExitRefused || !strings.HasPrefix(got.stderr, "vyshka: unauthorized: ") {
		t.Errorf("wrong token: %v", got)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadURL := "http://" + listener.Addr().String()
	listener.Close()
	got = h.runWith(runOptions{env: map[string]string{"VYSHKA_URL": deadURL}}, "servers", "--http-timeout", "1s")
	if got.code != ExitTransport || !strings.HasPrefix(got.stderr, "vyshka: GET ") {
		t.Errorf("nobody listening: %v", got)
	}
}

func TestParseInterspersed(t *testing.T) {
	cases := []struct {
		args        []string
		positionals []string
		wait, json  bool
		ttl         string
		delta       int64
		err         string
	}{
		{args: []string{"S", "CODE", "amount=5", "--wait"}, positionals: []string{"S", "CODE", "amount=5"}, wait: true},
		{args: []string{"--wait", "S", "--ttl", "5s", "CODE"}, positionals: []string{"S", "CODE"}, wait: true, ttl: "5s"},
		{args: []string{"S", "--ttl=7s", "-json"}, positionals: []string{"S"}, ttl: "7s", json: true},
		{args: []string{"S", "--", "--wait", "-x"}, positionals: []string{"S", "--wait", "-x"}},
		{args: []string{"ns", "key", "-5"}, positionals: []string{"ns", "key", "-5"}},
		{args: []string{"--delta", "-5", "ns"}, positionals: []string{"ns"}, delta: -5},
		{args: []string{"-", "x=-1"}, positionals: []string{"-", "x=-1"}},
		{args: []string{"S", "--bogus"}, err: "flag provided but not defined: -bogus"},
		{args: []string{"S", "--ttl"}, err: "flag needs an argument"},
		{args: []string{"--wait=maybe"}, err: "invalid boolean value"},
	}
	for _, c := range cases {
		fs := newFlagSet("test")
		wait := fs.Bool("wait", false, "")
		jsonOut := fs.Bool("json", false, "")
		ttl := fs.String("ttl", "", "")
		delta := fs.Int64("delta", 0, "")
		positionals, err := parseInterspersed(fs, c.args)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%v: error %v, want %q", c.args, err, c.err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%v: %v", c.args, err)
			continue
		}
		if !reflect.DeepEqual(positionals, c.positionals) && !(len(positionals) == 0 && len(c.positionals) == 0) {
			t.Errorf("%v: positionals %q, want %q", c.args, positionals, c.positionals)
		}
		if *wait != c.wait || *jsonOut != c.json || *ttl != c.ttl || *delta != c.delta {
			t.Errorf("%v: wait=%v json=%v ttl=%q delta=%d", c.args, *wait, *jsonOut, *ttl, *delta)
		}
	}

	fs := newFlagSet("test")
	if _, err := parseInterspersed(fs, []string{"-h"}); err != flag.ErrHelp {
		t.Errorf("-h: %v, want flag.ErrHelp", err)
	}
}

// Global flags given before the command and after it both land, the later
// one winning.
func TestGlobalFlagsBeforeAndAfterTheCommand(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	h.createServer("Globals")
	noEnv := map[string]string{"VYSHKA_URL": "", "VYSHKA_TOKEN": ""}

	got := h.runWith(runOptions{env: noEnv}, "--url", h.url, "servers", "--token", testToken, "--json")
	if got.code != ExitOK {
		t.Fatalf("globals split around the command: %v", got)
	}
	var decoded struct {
		Servers []json.RawMessage `json:"servers"`
	}
	if err := json.Unmarshal([]byte(got.stdout), &decoded); err != nil || len(decoded.Servers) != 1 {
		t.Errorf("servers --json = %q (%v)", got.stdout, err)
	}
	got = h.runWith(runOptions{env: noEnv}, "--token", "vya_WRONG", "--url", h.url, "servers", "--token", testToken)
	if got.code != ExitOK {
		t.Errorf("the later --token did not win: %v", got)
	}
}
