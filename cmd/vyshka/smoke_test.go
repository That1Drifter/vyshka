package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/That1Drifter/vyshka/cmd/vyshka/internal/testplugin"
	"github.com/That1Drifter/vyshka/hub"
)

// The smoke test grades the shipped binary end to end: the vyshka command as
// built from this package, run as a subprocess against a hub booted in this
// process and a fake plugin that long-polls it the way a real one does. The
// in-process tests under internal/cli grade each command's behaviour; this
// one proves the pieces fit through the real entry point: exit codes reach
// the shell, stdout and stderr are the right streams, a follow keeps printing
// until it is killed, and nothing reads the developer's own configuration.
//
// It is the "CLI against a hub booted in the job" check of issue 138, and
// runs wherever go test ./... runs.

// smokeToken is the bootstrap admin credential the smoke hub boots with.
const smokeToken = "vya_SMOKETOKENSMOKETOKENSMOKE0"

// smokeBinary is the command under test, built once by TestMain.
var smokeBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "vyshka-smoke-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "smoke: temp dir:", err)
		os.Exit(1)
	}
	name := "vyshka-smoke"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	smokeBinary = filepath.Join(dir, name)
	build := exec.Command("go", "build", "-o", smokeBinary, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "smoke: go build:", err)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// smokeRig is a hub served over HTTP and the environment the subprocess runs
// in: this hub's URL and token, and no configuration of the developer's own.
type smokeRig struct {
	t      *testing.T
	hubURL string
	env    []string
}

func newSmokeRig(t *testing.T) *smokeRig {
	t.Helper()
	server, err := hub.New(context.Background(), hub.Config{
		DatabaseURL: filepath.Join(t.TempDir(), "hub.db"),
		AdminToken:  smokeToken,
		Logger:      slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("boot hub: %v", err)
	}
	t.Cleanup(func() { server.Close() })
	listener := httptest.NewServer(server.Handler())
	t.Cleanup(listener.Close)

	// The config file is pointed at a path that does not exist and the
	// profile variable is cleared, so a developer's real profiles cannot
	// leak into the run (os/exec keeps the last of duplicate variables).
	env := append(os.Environ(),
		"VYSHKA_URL="+listener.URL,
		"VYSHKA_TOKEN="+smokeToken,
		"VYSHKA_CONFIG="+filepath.Join(t.TempDir(), "absent.json"),
		"VYSHKA_PROFILE=",
	)
	return &smokeRig{t: t, hubURL: listener.URL, env: env}
}

// outcome is one finished run of the command.
type outcome struct {
	code           int
	stdout, stderr string
}

func (r *smokeRig) run(args ...string) outcome {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, smokeBinary, args...)
	cmd.Env = r.env
	// No Stdin: the subprocess reads the null device, which is not a
	// terminal, so a destructive action must be confirmed with --yes.
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			r.t.Fatalf("vyshka %s: %v", strings.Join(args, " "), err)
		}
		code = exit.ExitCode()
	}
	got := outcome{code: code, stdout: stdout.String(), stderr: stderr.String()}
	r.t.Logf("vyshka %s\n  exit %d\n  stdout: %s\n  stderr: %s",
		strings.Join(args, " "), code, indent(got.stdout), indent(got.stderr))
	return got
}

func indent(s string) string {
	return strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n          ")
}

// expect fails the test unless the run exited with code.
func expect(t *testing.T, got outcome, code int, what string) {
	t.Helper()
	if got.code != code {
		t.Fatalf("%s: exit %d, want %d\nstdout:\n%s\nstderr:\n%s", what, got.code, code, got.stdout, got.stderr)
	}
}

func contains(t *testing.T, text, want, where string) {
	t.Helper()
	if !strings.Contains(text, want) {
		t.Errorf("%s does not contain %q:\n%s", where, want, text)
	}
}

// waitFor polls a condition every 100 ms until it holds or the deadline
// passes: the plugin's answers travel through real polls, so they arrive
// soon but not at once.
func waitFor(t *testing.T, within time.Duration, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("gave up after %s waiting for %s", within, what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// syncBuffer collects a running subprocess's output while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

const (
	playerOne = "76561198000000001"
	playerTwo = "76561198000000002"
)

func TestSmoke(t *testing.T) {
	rig := newSmokeRig(t)

	// version needs no hub; its first line is what the release workflow
	// compares with the tag.
	got := rig.run("version")
	expect(t, got, 0, "version")
	lines := strings.Split(strings.TrimRight(got.stdout, "\n"), "\n")
	if len(lines) != 2 || lines[0] != "dev" || !strings.HasPrefix(lines[1], "protocol draft ") {
		t.Fatalf("version printed %q, want the version then \"protocol draft N\"", got.stdout)
	}

	// The server record and its enrollment token come from the command
	// itself, as an operator's would.
	got = rig.run("servers", "create", "Smoke Server", "--game", "cli-test", "--json")
	expect(t, got, 0, "servers create")
	if strings.Count(strings.TrimRight(got.stdout, "\n"), "\n") != 0 {
		t.Fatalf("--json printed more than one line:\n%s", got.stdout)
	}
	var created struct {
		Server struct {
			ID string `json:"id"`
		} `json:"server"`
		Enrollment struct {
			Token string `json:"token"`
		} `json:"enrollment"`
	}
	if err := json.Unmarshal([]byte(got.stdout), &created); err != nil {
		t.Fatalf("servers create --json is not JSON: %v\n%s", err, got.stdout)
	}
	serverID := created.Server.ID
	if serverID == "" || created.Enrollment.Token == "" {
		t.Fatalf("servers create --json carried no id or token: %s", got.stdout)
	}

	plugin, err := testplugin.Start(context.Background(), testplugin.Options{
		HubURL:          rig.hubURL,
		EnrollmentToken: created.Enrollment.Token,
		Logf:            t.Logf,
	})
	if err != nil {
		t.Fatalf("start plugin: %v", err)
	}
	t.Cleanup(func() {
		plugin.Close()
		if err := plugin.Err(); err != nil {
			t.Errorf("the plugin reported a fatal error: %v", err)
		}
	})
	// The manifest rides the plugin's first poll; until it lands, actions
	// says the server has none (exit 1).
	waitFor(t, 10*time.Second, "the manifest to be accepted", func() bool {
		return rig.run("actions", serverID).code == 0
	})

	t.Run("servers", func(t *testing.T) {
		got := rig.run("servers")
		expect(t, got, 0, "servers")
		contains(t, got.stdout, "Smoke Server", "servers")
		contains(t, got.stdout, serverID, "servers")
		contains(t, got.stdout, "vyshka-testplugin 0.1.0", "servers PLUGIN column")

		// A unique part of the name resolves the server.
		got = rig.run("servers", "show", "smoke")
		expect(t, got, 0, "servers show by name")
		contains(t, got.stdout, serverID, "servers show")
		contains(t, got.stdout, "cli-test", "servers show game")

		got = rig.run("servers", "show", "no such server")
		expect(t, got, 1, "servers show of an unknown name")
		contains(t, got.stderr, "no server", "servers show stderr")
	})

	t.Run("actions", func(t *testing.T) {
		got := rig.run("actions", serverID)
		expect(t, got, 0, "actions")
		for _, want := range []string{"cli-test.heal", "amount:integer*", "position:number[]*", "destructive", "cli-test.zone"} {
			contains(t, got.stdout, want, "actions table")
		}
		got = rig.run("actions", serverID, "--code", "cli-test.heal")
		expect(t, got, 0, "actions --code")
		contains(t, got.stdout, `"maximum": 100`, "actions --code schema")
	})

	t.Run("run with a player resolved from the snapshot", func(t *testing.T) {
		plugin.Snapshot("players", map[string]any{
			"capturedAt": time.Now().UTC().Format(time.RFC3339),
			"players": []map[string]any{
				{"player": map[string]any{"platform": "steam", "id": playerOne}, "name": "Survivor One", "position": []float64{4501.2, 320.1, 9800.4}},
				{"player": map[string]any{"platform": "steam", "id": playerTwo}, "name": "Second Survivor"},
			},
		})
		waitFor(t, 10*time.Second, "the players snapshot", func() bool {
			return rig.run("state", serverID, "players").code == 0
		})
		got := rig.run("state", serverID, "players")
		expect(t, got, 0, "state players")
		contains(t, got.stdout, "captured ", "state players header")
		contains(t, got.stdout, "Survivor One", "state players")
		contains(t, got.stdout, "4501.2,320.1,9800.4", "state players position")

		before := len(plugin.Dispatches())
		got = rig.run("run", serverID, "cli-test.heal", "--player", "one", "amount=5", "--wait")
		expect(t, got, 0, "run heal --player --wait")
		contains(t, got.stderr, `resolved "one" to Survivor One (steam:`+playerOne+")", "run stderr")
		contains(t, got.stderr, "captured ", "run stderr snapshot age")
		contains(t, got.stderr, "completed", "run progress")
		contains(t, got.stdout, `"healedTo": 100`, "run result")
		contains(t, got.stdout, `"amount": 5`, "run result echoes the typed param")
		dispatches := plugin.Dispatches()
		if len(dispatches) != before+1 {
			t.Fatalf("the plugin saw %d dispatches, want %d", len(dispatches), before+1)
		}
		last := dispatches[len(dispatches)-1]
		if last.Context != "player" || last.ReferenceKey != playerOne {
			t.Errorf("dispatch carried context %q and referenceKey %q, want player and %s", last.Context, last.ReferenceKey, playerOne)
		}
		if amount, ok := last.Params["amount"].(float64); !ok || amount != 5 {
			t.Errorf("dispatch carried amount %v (%T), want the number 5", last.Params["amount"], last.Params["amount"])
		}

		// Two players match "survivor": the command refuses to guess.
		got = rig.run("run", serverID, "cli-test.heal", "--player", "survivor", "amount=5")
		expect(t, got, 1, "run with an ambiguous --player")
		contains(t, got.stderr, "more than one player", "ambiguous player stderr")

		// A schema violation is caught before any request leaves.
		before = len(plugin.Dispatches())
		got = rig.run("run", serverID, "cli-test.heal", "--player", "one", "amount=500")
		expect(t, got, 1, "run with an out-of-range param")
		contains(t, got.stderr, "amount", "local schema error")
		got = rig.run("run", serverID, "cli-test.heal", "--target", playerOne, "amt=5")
		expect(t, got, 1, "run with an unknown param")
		contains(t, got.stderr, `unknown param "amt"`, "unknown param error")
		if got := len(plugin.Dispatches()); got != before {
			t.Fatalf("locally refused runs reached the plugin: %d dispatches, want %d", got, before)
		}
	})

	t.Run("danger levels", func(t *testing.T) {
		got := rig.run("run", serverID, "cli-test.teleport", "--target", playerOne, "position=1,2,3", "--wait")
		expect(t, got, 0, "run a warning action")
		contains(t, got.stderr, "warning: cli-test.teleport is marked warning", "warning notice")
		contains(t, got.stdout, "1,", "teleport result carries the vector")
		dispatches := plugin.Dispatches()
		position, _ := dispatches[len(dispatches)-1].Params["position"].([]any)
		if len(position) != 3 {
			t.Errorf("the vector reached the plugin as %v, want three numbers", dispatches[len(dispatches)-1].Params["position"])
		}

		// Without a terminal, a destructive action needs --yes.
		before := len(plugin.Dispatches())
		got = rig.run("run", serverID, "cli-test.wipe", "reason=smoke")
		expect(t, got, 1, "run a destructive action without --yes")
		contains(t, got.stderr, "--yes", "destructive refusal")
		if len(plugin.Dispatches()) != before {
			t.Fatal("a refused destructive action reached the plugin")
		}
		got = rig.run("run", serverID, "cli-test.wipe", "reason=smoke", "--yes", "--wait")
		expect(t, got, 0, "run a destructive action with --yes")
		contains(t, got.stdout, `"wiped": true`, "wipe result")
	})

	var slowActionID string
	t.Run("outcomes and exit codes", func(t *testing.T) {
		got := rig.run("run", serverID, "cli-test.echo", "note=hello")
		expect(t, got, 0, "run without --wait")
		contains(t, got.stdout, "actionId", "run output")
		contains(t, got.stdout, "queued", "run state")

		got = rig.run("run", serverID, "cli-test.fail", "--wait")
		expect(t, got, 2, "run a failing action")
		contains(t, got.stderr, "failed as asked", "failure message")

		got = rig.run("run", serverID, "cli-test.never", "--ttl", "1s", "--wait")
		expect(t, got, 3, "run an action nobody finishes")
		contains(t, got.stderr, "expired", "expiry message")

		got = rig.run("run", serverID, "cli-test.slow", "delayMs=8000", "--wait", "--timeout", "700ms")
		expect(t, got, 6, "run --wait with a short --timeout")
		match := regexp.MustCompile(`vyshka job (\S+)"`).FindStringSubmatch(got.stderr)
		if match == nil {
			t.Fatalf("the timeout message names no action to read later:\n%s", got.stderr)
		}
		slowActionID = match[1]

		got = rig.run("job", slowActionID)
		expect(t, got, 0, "job")
		contains(t, got.stdout, "cli-test.slow", "job output")
		contains(t, got.stdout, "state:", "job output")

		got = rig.run("run", serverID, "cli-test.heal", "--target", playerOne, "amount=1", "--wait", "--json")
		expect(t, got, 0, "run --wait --json")
		if strings.Count(strings.TrimRight(got.stdout, "\n"), "\n") != 0 {
			t.Fatalf("--json printed more than one line:\n%s", got.stdout)
		}
		var record struct {
			State  string `json:"state"`
			Result struct {
				HealedTo float64 `json:"healedTo"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(got.stdout), &record); err != nil {
			t.Fatalf("run --json is not JSON: %v\n%s", err, got.stdout)
		}
		if record.State != "completed" || record.Result.HealedTo != 100 {
			t.Errorf("run --json printed state %q and healedTo %v", record.State, record.Result.HealedTo)
		}
	})

	t.Run("contexts", func(t *testing.T) {
		got := rig.run("contexts", serverID, "cli-test.zone")
		expect(t, got, 0, "contexts")
		contains(t, got.stdout, "enumerated ", "contexts header")
		contains(t, got.stdout, "zone-a", "contexts")
		contains(t, got.stdout, "Alpha", "contexts")
		contains(t, got.stdout, "100,0,200", "contexts position")

		got = rig.run("run", serverID, "cli-test.pick", "--target", "zone-a", "mode=hard", "--wait")
		expect(t, got, 0, "run in a custom context")
		contains(t, got.stdout, `"referenceKey": "zone-a"`, "pick result")
		contains(t, got.stdout, `"mode": "hard"`, "pick result")

		got = rig.run("run", serverID, "cli-test.pick", "--target", "zone-a", "mode=medium")
		expect(t, got, 1, "run with a value outside the enum")
		contains(t, got.stderr, "allowed values", "enum error")
	})

	t.Run("events", func(t *testing.T) {
		plugin.Emit(
			testplugin.Event{Type: "cli-test.beat", Data: map[string]any{"n": 1}},
			testplugin.Event{Type: "core.player.chat", Data: map[string]any{
				"player": map[string]any{"platform": "steam", "id": playerOne}, "message": "hello"}},
		)
		waitFor(t, 10*time.Second, "the events to be stored", func() bool {
			return strings.Contains(rig.run("events", serverID).stdout, "core.player.chat")
		})
		got := rig.run("events", serverID, "--type", "cli-test.*")
		expect(t, got, 0, "events --type")
		contains(t, got.stdout, "cli-test.beat", "filtered events")
		if strings.Contains(got.stdout, "core.player.chat") {
			t.Errorf("--type cli-test.* still listed core.player.chat:\n%s", got.stdout)
		}

		// The profile finds the chat event by its top-level identity.
		got = rig.run("player", "steam", playerOne, "events")
		expect(t, got, 0, "player events")
		contains(t, got.stdout, "core.player.chat", "player events")
		contains(t, got.stdout, "player", "player events roles")

		// --follow prints what arrives until it is killed, and prints an
		// event once even though every tick re-reads the lookback window.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		follow := exec.CommandContext(ctx, smokeBinary, "events", serverID, "--follow", "--json", "--interval", "200ms")
		follow.Env = rig.env
		var out syncBuffer
		var errOut bytes.Buffer
		follow.Stdout, follow.Stderr = &out, &errOut
		if err := follow.Start(); err != nil {
			t.Fatalf("start events --follow: %v", err)
		}
		waitFor(t, 10*time.Second, "the initial page of the follow", func() bool {
			return strings.Contains(out.String(), "cli-test.beat")
		})
		plugin.Emit(testplugin.Event{Type: "cli-test.later", Data: map[string]any{"n": 2}})
		waitFor(t, 10*time.Second, "the followed event", func() bool {
			return strings.Contains(out.String(), "cli-test.later")
		})
		// Two more ticks pass with nothing new, which is when a follow
		// without deduplication would print the window again.
		time.Sleep(500 * time.Millisecond)
		cancel()
		_ = follow.Wait()

		seen := map[string]int{}
		for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
			var event struct {
				ID   string `json:"id"`
				Type string `json:"type"`
			}
			if err := json.Unmarshal([]byte(line), &event); err != nil {
				t.Fatalf("follow --json printed a line that is not JSON: %q (%v)", line, err)
			}
			seen[event.ID]++
			if seen[event.ID] > 1 {
				t.Errorf("follow printed event %s (%s) %d times", event.ID, event.Type, seen[event.ID])
			}
		}
		if errOut.Len() > 0 {
			t.Errorf("follow wrote to stderr:\n%s", errOut.String())
		}
	})

	t.Run("kv", func(t *testing.T) {
		got := rig.run("kv", "set", "cli-test", "smoke.flag", "1")
		expect(t, got, 0, "kv set")
		contains(t, got.stdout, "revision 1", "kv set")

		got = rig.run("kv", "get", "cli-test", "smoke.flag")
		expect(t, got, 0, "kv get")
		if got.stdout != "1\n" {
			t.Errorf("kv get stdout = %q, want the value alone", got.stdout)
		}
		contains(t, got.stderr, "revision 1", "kv get stderr")

		got = rig.run("kv", "incr", "cli-test", "smoke.flag", "--delta", "4")
		expect(t, got, 0, "kv incr")
		if got.stdout != "5\n" {
			t.Errorf("kv incr stdout = %q, want 5", got.stdout)
		}

		got = rig.run("kv", "set", "cli-test", "smoke.flag", "9", "--if-revision", "1")
		expect(t, got, 4, "kv set with a stale --if-revision")
		contains(t, got.stderr, "revision_mismatch", "kv set stderr")
		contains(t, got.stderr, "current revision 2", "kv set stderr")

		got = rig.run("kv", "set", "cli-test", "smoke.flag", `{"owner":"clan-a"}`, "--if-revision", "2")
		expect(t, got, 0, "kv set with the right --if-revision")
		got = rig.run("kv", "get", "cli-test", "smoke.flag")
		expect(t, got, 0, "kv get of an object")
		if got.stdout != `{"owner":"clan-a"}`+"\n" {
			t.Errorf("kv get stdout = %q, want the object on one line", got.stdout)
		}

		got = rig.run("kv", "set", "cli-test", "smoke.text", "12", "--string")
		expect(t, got, 0, "kv set --string")
		got = rig.run("kv", "get", "cli-test", "smoke.text")
		expect(t, got, 0, "kv get of a string")
		if got.stdout != `"12"`+"\n" {
			t.Errorf("kv get stdout = %q, want the string \"12\"", got.stdout)
		}

		got = rig.run("kv", "list", "cli-test")
		expect(t, got, 0, "kv list")
		contains(t, got.stdout, "smoke.flag", "kv list")
		contains(t, got.stdout, "smoke.text", "kv list")
		got = rig.run("kv", "namespaces")
		expect(t, got, 0, "kv namespaces")
		contains(t, got.stdout, "cli-test", "kv namespaces")

		got = rig.run("kv", "delete", "cli-test", "smoke.flag")
		expect(t, got, 0, "kv delete")
		contains(t, got.stdout, "deleted", "kv delete")
		got = rig.run("kv", "delete", "cli-test", "smoke.flag")
		expect(t, got, 0, "kv delete of an absent key")
		contains(t, got.stdout, "already absent", "kv delete")
		got = rig.run("kv", "get", "cli-test", "smoke.flag")
		expect(t, got, 4, "kv get of a deleted key")
		contains(t, got.stderr, "not_found", "kv get stderr")
	})

	t.Run("refusal and transport exit codes", func(t *testing.T) {
		got := rig.run("servers", "--token", "vya_not_the_token")
		expect(t, got, 4, "a wrong token")
		contains(t, got.stderr, "unauthorized", "wrong token stderr")
		if strings.Contains(got.stderr, "vya_not_the_token") {
			t.Errorf("the refusal echoed the token:\n%s", got.stderr)
		}

		got = rig.run("servers", "--url", "http://127.0.0.1:1", "--http-timeout", "2s")
		expect(t, got, 5, "a URL nobody listens on")

		got = rig.run("health")
		expect(t, got, 0, "health")
		contains(t, got.stdout, "status:", "health")
		contains(t, got.stdout, "ok", "health")
	})
}
