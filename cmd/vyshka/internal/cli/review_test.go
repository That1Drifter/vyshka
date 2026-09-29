package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// These tests each pin a defect the first adversarial review found in the
// command, so a fix cannot quietly come undone.

// A flag the parser cannot read is echoed in its error, token and all: the
// message must reach stderr without the credential.
func TestMalformedTokenFlagIsRedacted(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	const secret = "vya_SLIPPEDTOKENSLIPPEDTOKEN00"
	// A token is opaque: one may hold a backslash, which an error that quotes
	// its argument escapes, or a non-ASCII rune, which a duration's error
	// spells byte by byte. No spelling of any value may come out.
	const slashy = `vya_SLIPPED\TOKEN`
	const accented = "vya_SLIPPEDtökén"
	for _, args := range [][]string{
		{"---token=" + secret, "version"},
		{"version", "---token=" + secret},
		{"kv", "---token=" + secret, "get", "ns", "key"},
		{"run", "Anywhere", "x.y", "---token=" + secret},
		{"version", "--http-timeout", "--token=" + slashy},
		{"---token=" + slashy, "version"},
		{"version", "--http-timeout", "--token=" + accented},
		{"version", "--token", secret, "--http-timeout", accented},
		// Two spellings at once: the shorter must not spoil the longer.
		{"--token=vya_", "---token=" + secret, "version"},
	} {
		got := h.runWith(runOptions{env: map[string]string{"VYSHKA_TOKEN": ""}}, args...)
		if got.code != ExitUsage {
			t.Errorf("%v: %v\nwant exit 1", args, got)
		}
		if strings.Contains(got.stdout+got.stderr, "SLIPPED") || strings.Contains(got.stdout+got.stderr, "TOKEN") ||
			strings.Contains(got.stdout+got.stderr, `\x`) {
			t.Errorf("%v: the token reached the output:\n%v", args, got)
		}
		if !strings.Contains(got.stderr, "[redacted]") || !strings.Contains(got.stderr, "-token") && !strings.Contains(got.stderr, "-http-timeout") {
			t.Errorf("%v: the message does not say which flag failed, without its value:\n%v", args, got)
		}
	}
	// With no token on the command line, the parser's message keeps its
	// value, which is what a typo needs.
	got := h.run("version", "--http-timeout", "soon")
	if got.code != ExitUsage || !strings.Contains(got.stderr, `"soon"`) {
		t.Errorf("a plain flag error lost its value: %v", got)
	}
}

// The global flags are accepted between a command and its subcommand too.
func TestGlobalFlagsBetweenCommandAndSubcommand(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	id, _ := h.createServer("Between")

	got := h.run("servers", "--json", "show", id)
	if got.code != ExitOK {
		t.Fatalf("servers --json show: %v", got)
	}
	var record struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(got.stdout), &record); err != nil || record.ID != id {
		t.Errorf("servers --json show printed %q, want the record of %s", got.stdout, id)
	}

	got = h.run("kv", "--json", "namespaces")
	if got.code != ExitOK || !strings.HasPrefix(got.stdout, `{"namespaces"`) {
		t.Fatalf("kv --json namespaces: %v", got)
	}
	got = h.run("kv", "--http-timeout", "5s", "set", "example-mod", "between", "1")
	if got.code != ExitOK || !strings.Contains(got.stdout, "revision 1") {
		t.Fatalf("kv --http-timeout 5s set: %v", got)
	}
	// -h before a subcommand is the command's help; flags alone with no
	// subcommand is the usage error naming the subcommands.
	got = h.run("kv", "-h")
	if got.code != ExitOK || !strings.Contains(got.stdout, "vyshka kv get NS KEY") {
		t.Fatalf("kv -h: %v", got)
	}
	got = h.run("kv", "--json")
	if got.code != ExitUsage || !strings.Contains(got.stderr, "kv takes get") {
		t.Fatalf("kv --json alone: %v", got)
	}
}

// A retry carrying an idempotency key is answered with the original action
// whatever the manifest says now (spec section 7), so the command sends it
// even when the current manifest no longer declares the code.
func TestRunIdempotentRetrySurvivesAManifestChange(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	p := h.plugin("Retry")
	p.publishManifest(testManifest(1))

	// reason is a string in the schema; as text, 1e400 would not survive a
	// reading as JSON, which the schema-less retry must not attempt.
	args := []string{"run", "Retry", "example-mod.heal", "--target", "76561198000000001", "amount=5", "reason=1e400", "--idempotency-key", "beat-1"}
	first := h.run(args...)
	if first.code != ExitOK {
		t.Fatalf("first dispatch: %v", first)
	}
	original := actionIDFrom(t, first.stdout)

	// A newer manifest without heal.
	changed := testManifest(2)
	var kept []map[string]any
	for _, action := range changed["actions"].([]map[string]any) {
		if action["code"] != "example-mod.heal" {
			kept = append(kept, action)
		}
	}
	changed["actions"] = kept
	p.publishManifest(changed)

	retry := h.run(args...)
	if retry.code != ExitOK {
		t.Fatalf("retry after the manifest change: %v", retry)
	}
	if got := actionIDFrom(t, retry.stdout); got != original {
		t.Errorf("the retry was answered with action %s, want the original %s", got, original)
	}
	if !strings.Contains(retry.stderr, "notice:") || !strings.Contains(retry.stderr, "as written") {
		t.Errorf("the retry printed no notice about the manifest:\n%s", retry.stderr)
	}

	// Without a key the same command is refused locally, as before.
	fresh := h.run("run", "Retry", "example-mod.heal", "--target", "76561198000000001", "amount=5")
	if fresh.code != ExitUsage || !strings.Contains(fresh.stderr, "does not declare") {
		t.Errorf("a fresh dispatch of an undeclared action: %v\nwant exit 1", fresh)
	}
}

// An action already terminal on the first read is reported from that read:
// the wait ends at once, within the timeout, and the state is printed once.
func TestWaitReportsATerminalFirstReadAtOnce(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	p := h.plugin("Done")
	p.publishManifest(testManifest(1))
	got := h.run("run", "Done", "example-mod.ping")
	if got.code != ExitOK {
		t.Fatalf("dispatch: %v", got)
	}
	id := actionIDFrom(t, got.stdout)
	p.finish(p.nextDispatch().ActionID, true, map[string]any{"pong": true}, "")

	reads := h.actionReads.Load()
	got = h.run("job", id, "--wait", "--timeout", "50ms")
	if got.code != ExitOK || !strings.Contains(got.stdout, `"pong": true`) {
		t.Fatalf("job --wait on a completed action: %v", got)
	}
	if strings.TrimSpace(got.stderr) != "completed" {
		t.Errorf("progress = %q, want the one state completed", got.stderr)
	}
	// The terminal record came from the first read; nothing read it again.
	if n := h.actionReads.Load() - reads; n != 1 {
		t.Errorf("a completed action was read %d times, want once", n)
	}
	got = h.run("job", id, "--wait", "--timeout", "50ms", "--json")
	if got.code != ExitOK || strings.Count(strings.TrimSpace(got.stdout), "\n") != 0 {
		t.Fatalf("job --wait --json on a completed action: %v", got)
	}

	// The first read counts against --timeout: a hub too slow to answer it
	// within the deadline ends the wait then, with the action not yet read,
	// rather than after the HTTP timeout with a record read outside the
	// deadline.
	h.actionReadDelay.Store(400)
	defer h.actionReadDelay.Store(0)
	started := time.Now()
	got = h.run("job", id, "--wait", "--timeout", "50ms")
	elapsed := time.Since(started)
	if got.code != ExitWaitTimeout || !strings.Contains(got.stderr, "not yet read") {
		t.Fatalf("job --wait against a slow hub: %v\nwant exit 6 with the action not yet read", got)
	}
	if elapsed > 300*time.Millisecond {
		t.Errorf("the wait took %s against a 50ms deadline: the first read escaped it", elapsed)
	}
}

// Ctrl-C at the confirmation prompt ends the run instead of leaving it
// blocked on a read that cannot see the signal.
func TestConfirmationYieldsToCancellation(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	p := h.plugin("Prompt")
	p.publishManifest(testManifest(1))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// A terminal nobody types at: the pipe is never written.
	reader, writer := io.Pipe()
	defer writer.Close()
	stdout, stderr := &syncBuffer{}, &syncBuffer{}
	done := make(chan int, 1)
	go func() {
		done <- Run([]string{"run", "Prompt", "example-mod.wipe"}, IO{
			Stdin:  reader,
			Stdout: stdout,
			Stderr: stderr,
			Getenv: func(key string) string {
				return map[string]string{"VYSHKA_URL": h.url, "VYSHKA_TOKEN": testToken}[key]
			},
			StdinIsTerminal: true,
			ConfigDir:       func() (string, error) { return h.configDir, nil },
			Context:         ctx,
		})
	}()
	eventually(t, 5*time.Second, "the prompt", func() bool { return strings.Contains(stderr.String(), "[y/N]") })
	cancel()
	select {
	case code := <-done:
		if code != ExitUsage || !strings.Contains(stderr.String(), "interrupted") {
			t.Errorf("exit %d, stderr %q; want exit 1 and an interruption notice", code, stderr.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the run stayed blocked at the prompt after its context was cancelled")
	}
	if sent := h.dispatches.Load(); sent != 0 {
		t.Errorf("%d dispatches reached the hub after an interrupted confirmation", sent)
	}
}

// The lookback of --follow never reaches below a --since the user gave.
func TestFollowKeepsTheUserSinceBound(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	p := h.plugin("Bound")
	now := time.Now()
	p.pushEvents(
		event("test.old", now.Add(-90*time.Second), map[string]any{}),
		event("test.window", now.Add(-50*time.Second), map[string]any{}),
		event("test.recent", now.Add(-10*time.Second), map[string]any{}),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stdout := &syncBuffer{}
	done := h.background(runOptions{ctx: ctx, stdout: stdout}, "events", "Bound", "--follow", "--interval", "30ms",
		"--since", now.Add(-30*time.Second).UTC().Format(time.RFC3339Nano))
	eventually(t, 5*time.Second, "the recent event", func() bool { return strings.Contains(stdout.String(), "test.recent") })
	// Several ticks pass, each asking again over the lookback window, which
	// reaches back past the window event without the bound.
	time.Sleep(300 * time.Millisecond)
	cancel()
	got := await(t, done, 5*time.Second)
	if strings.Contains(got.stdout, "test.window") || strings.Contains(got.stdout, "test.old") {
		t.Errorf("--follow printed events from before --since:\n%s", got.stdout)
	}
}

// The feed needs events:read alone: a token with only that grant reads a
// server's events by id, since the record behind the id is never asked for.
func TestEventsByIDNeedsOnlyEventsRead(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	p := h.plugin("Narrow")
	p.pushEvents(event("test.only", time.Now(), map[string]any{}))
	var minted struct {
		Secret string `json:"secret"`
	}
	h.mustCall(http.MethodPost, "/api/v1/tokens", testToken,
		map[string]any{"name": "feed", "scopes": []string{"events:read"}}, &minted, http.StatusCreated)
	narrow := runOptions{env: map[string]string{"VYSHKA_TOKEN": minted.Secret}}

	got := h.runWith(narrow, "events", p.serverID)
	if got.code != ExitOK || !strings.Contains(got.stdout, "test.only") {
		t.Fatalf("events by id with an events:read token: %v", got)
	}
	// By name the list is needed, which this token may not read: the hub's
	// refusal is reported as such.
	got = h.runWith(narrow, "events", "Narrow")
	if got.code != ExitRefused {
		t.Errorf("events by name with an events:read token: %v\nwant exit 4", got)
	}
}

// Ids are opaque, so a name of any shape still resolves, an id of any shape
// is tried as given, and a not_found that is not about the server stands.
func TestServerArgumentIsTriedAsIDThenAsName(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	const idShaped = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	p := h.plugin(idShaped)
	p.pushEvents(event("test.named", time.Now(), map[string]any{}))
	p.pushPlayers(time.Now(), map[string]any{"player": map[string]any{"platform": "steam", "id": "1"}, "name": "One"})

	for _, args := range [][]string{
		{"events", idShaped},
		{"state", idShaped, "players"},
		{"events", p.serverID},
		{"state", p.serverID, "players"},
		{"servers", "show", idShaped},
		{"servers", "show", p.serverID},
	} {
		if got := h.run(args...); got.code != ExitOK {
			t.Errorf("%v: %v\nwant exit 0", args, got)
		}
	}
	// No vehicles snapshot was ever accepted: that not_found is about the
	// snapshot, and is reported, not turned into a search for a server
	// named like the id.
	got := h.run("state", p.serverID, "vehicles")
	if got.code != ExitRefused || !strings.Contains(got.stderr, "not_found") {
		t.Errorf("state of a snapshot never accepted: %v\nwant exit 4 with not_found", got)
	}
	// An argument that is neither an id nor a name lists the candidates.
	got = h.run("events", "nothing like it")
	if got.code != ExitUsage || !strings.Contains(got.stderr, idShaped) {
		t.Errorf("events of an unknown server: %v\nwant exit 1 naming the servers", got)
	}
}

// A token bound to particular servers is refused for any id outside its
// binding before the id is looked up, a name used as one included: the name
// still resolves through the list of what the token may see.
func TestBoundTokenResolvesAServerName(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	p := h.plugin("Livonia")
	p.pushEvents(event("test.bound", time.Now(), map[string]any{}))
	p.pushPlayers(time.Now(), map[string]any{"player": map[string]any{"platform": "steam", "id": "1"}, "name": "One"})
	other := h.plugin("Chernarus")
	other.pushEvents(event("test.other", time.Now(), map[string]any{}))

	var minted struct {
		Secret string `json:"secret"`
	}
	h.mustCall(http.MethodPost, "/api/v1/tokens", testToken, map[string]any{
		"name": "one server", "scopes": []string{"servers:read", "events:read"}, "servers": []string{p.serverID},
	}, &minted, http.StatusCreated)
	bound := runOptions{env: map[string]string{"VYSHKA_TOKEN": minted.Secret}}

	for _, args := range [][]string{
		{"events", "Livonia"},
		{"events", p.serverID},
		{"state", "Livonia", "players"},
		{"servers", "show", "Livonia"},
	} {
		got := h.runWith(bound, args...)
		if got.code != ExitOK {
			t.Errorf("%v with a bound token: %v\nwant exit 0", args, got)
		}
	}
	// The other server is outside the binding by id and invisible by name.
	got := h.runWith(bound, "events", other.serverID)
	if got.code != ExitUsage && got.code != ExitRefused {
		t.Errorf("events of a server outside the binding: %v\nwant a refusal or no match", got)
	}
	if strings.Contains(got.stdout, "test.other") {
		t.Errorf("a bound token read another server's events:\n%v", got)
	}
}

// When the name search fails below the protocol, that failure is the
// answer, not the id probe's not_found.
func TestNameSearchTransportFailureIsReported(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	h.createServer("Livonia")
	h.listFails.Store(true)
	got := h.run("events", "Livonia")
	if got.code != ExitTransport {
		t.Errorf("events by name with the list unreachable: %v\nwant exit 5", got)
	}
}
