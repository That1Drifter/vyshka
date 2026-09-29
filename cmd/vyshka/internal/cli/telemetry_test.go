package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func event(eventType string, occurred time.Time, data map[string]any) map[string]any {
	return map[string]any{"t": eventType, "ts": occurred.UTC().Format(time.RFC3339Nano), "data": data}
}

func TestState(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	p := h.plugin("State")

	if missing := h.run("state", "State", "players"); missing.code != ExitRefused || !strings.Contains(missing.stderr, "not_found") {
		t.Errorf("state before any snapshot: %v", missing)
	}

	captured := time.Now().Add(-3 * time.Second)
	p.pushPlayers(captured,
		map[string]any{"player": map[string]any{"platform": "steam", "id": "76561198000000001"},
			"name": "Survivor", "position": []float64{4231.5, 300.2, 10620}},
		map[string]any{"player": map[string]any{"platform": "epic", "id": "abc"}, "name": "Bandit\x1b[31m"},
	)
	p.poll(p.envelope("state.world", map[string]any{"world": map[string]any{
		"time": "2026-09-20T14:32", "data": map[string]any{"rain": 0.25},
	}}))
	p.poll(p.envelope("state.vehicles", map[string]any{"vehicles": []map[string]any{
		{"id": "v1", "kind": "car", "position": []float64{1, 2}},
	}}))

	got := h.run("state", "State", "players")
	if got.code != ExitOK {
		t.Fatalf("state players: %v", got)
	}
	header := strings.SplitN(got.stdout, "\n", 2)[0]
	if !strings.HasPrefix(header, "captured "+captured.UTC().Format(time.RFC3339)) ||
		!strings.Contains(header, "s ago), received ") {
		t.Errorf("header = %q", header)
	}
	for _, want := range []string{"NAME", "PLATFORM", "POSITION", "Survivor", "steam", "76561198000000001", "4231.5,300.2,10620"} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("state players lacks %q:\n%s", want, got.stdout)
		}
	}
	// A control sequence in a player's name never reaches the terminal.
	if strings.Contains(got.stdout, "\x1b") {
		t.Errorf("an escape sequence from the snapshot reached stdout: %q", got.stdout)
	}

	vehicles := h.run("state", "State", "vehicles")
	if vehicles.code != ExitOK || !strings.Contains(vehicles.stdout, "KIND") || !strings.Contains(vehicles.stdout, "car") ||
		!strings.Contains(vehicles.stdout, "1,2") {
		t.Errorf("state vehicles: %v", vehicles)
	}
	world := h.run("state", "State", "world")
	if world.code != ExitOK || !strings.Contains(world.stdout, "time: 2026-09-20T14:32") || !strings.Contains(world.stdout, `"rain": 0.25`) {
		t.Errorf("state world: %v", world)
	}

	p.pushPlayers(time.Now())
	history := h.run("state", "State", "players", "--history", "5")
	if history.code != ExitOK {
		t.Fatalf("state --history: %v", history)
	}
	rows := strings.Split(strings.TrimSpace(history.stdout), "\n")
	if len(rows) != 3 || !strings.Contains(rows[0], "ENTRIES") ||
		!strings.HasSuffix(strings.TrimSpace(rows[1]), " 0") || !strings.HasSuffix(strings.TrimSpace(rows[2]), " 2") {
		t.Errorf("state --history, newest first:\n%s", history.stdout)
	}

	raw := h.run("state", "State", "players", "--json")
	var snapshot map[string]any
	if raw.code != ExitOK || json.Unmarshal([]byte(raw.stdout), &snapshot) != nil || snapshot["type"] != "players" {
		t.Errorf("state --json: %v", raw)
	}
	if bad := h.run("state", "State", "weather"); bad.code != ExitUsage {
		t.Errorf("state weather: %v", bad)
	}
}

func TestEventsPageAndTypeFilter(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	p := h.plugin("Events")
	now := time.Now()
	p.pushEvents(
		event("core.player.connect", now.Add(-3*time.Second), map[string]any{"player": map[string]any{"platform": "steam", "id": "1"}}),
		event("core.player.disconnect", now.Add(-2*time.Second), map[string]any{"player": map[string]any{"platform": "steam", "id": "1"}}),
		event("example-mod.raid.started", now.Add(-time.Second), map[string]any{"territoryId": "t-19", "attackers": 4}),
	)

	all := h.run("events", "Events")
	if all.code != ExitOK {
		t.Fatalf("events: %v", all)
	}
	lines := strings.Split(strings.TrimSpace(all.stdout), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "OCCURRED") || !strings.Contains(lines[1], "example-mod.raid.started") ||
		!strings.Contains(lines[1], `"attackers":4`) || !strings.Contains(lines[3], "core.player.connect") {
		t.Errorf("events, newest first:\n%s", all.stdout)
	}

	filtered := h.run("events", "Events", "--type", "core.player.*")
	if filtered.code != ExitOK || strings.Count(filtered.stdout, "core.player.") != 2 || strings.Contains(filtered.stdout, "raid") {
		t.Errorf("events --type core.player.*: %v", filtered)
	}
	both := h.run("events", "Events", "--type", "core.player.connect", "--type", "example-mod.*")
	if both.code != ExitOK || strings.Count(both.stdout, "\n") != 3 || strings.Contains(both.stdout, "disconnect") {
		t.Errorf("events with two --type terms: %v", both)
	}

	paged := h.run("events", "Events", "--limit", "2")
	if paged.code != ExitOK || strings.Count(paged.stdout, "\n") != 3 || !strings.Contains(paged.stderr, "--all") {
		t.Errorf("events --limit 2: %v", paged)
	}
	walked := h.run("events", "Events", "--limit", "2", "--all")
	if walked.code != ExitOK || strings.Count(walked.stdout, "\n") != 4 {
		t.Errorf("events --limit 2 --all: %v", walked)
	}

	raw := h.run("events", "Events", "--type", "example-mod.raid.started", "--json")
	var page struct {
		Events []map[string]any `json:"events"`
	}
	if raw.code != ExitOK || strings.Count(raw.stdout, "\n") != 1 || json.Unmarshal([]byte(raw.stdout), &page) != nil ||
		len(page.Events) != 1 {
		t.Errorf("events --json: %v", raw)
	}

	since := h.run("events", "Events", "--since", now.Add(-1500*time.Millisecond).UTC().Format(time.RFC3339Nano))
	if since.code != ExitOK || strings.Count(since.stdout, "\n") != 2 {
		t.Errorf("events --since: %v", since)
	}
	if bad := h.run("events", "Events", "--since", "yesterday"); bad.code != ExitUsage {
		t.Errorf("events --since yesterday: %v", bad)
	}
}

func TestEventsFollow(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	p := h.plugin("Follow")
	p.pushEvents(event("test.first", time.Now().Add(-time.Second), map[string]any{"n": 1}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stdout := &syncBuffer{}
	done := h.background(runOptions{ctx: ctx, stdout: stdout}, "events", "Follow", "--follow", "--interval", "50ms")

	eventually(t, 5*time.Second, "the first event", func() bool { return strings.Contains(stdout.String(), "test.first") })
	p.pushEvents(event("test.second", time.Now(), map[string]any{"n": 2}))
	eventually(t, 5*time.Second, "the second event", func() bool { return strings.Contains(stdout.String(), "test.second") })
	// A late arrival stamped before the newest printed event, within the
	// lookback window, is still printed.
	p.pushEvents(event("test.late", time.Now().Add(-10*time.Second), map[string]any{"n": 3}))
	eventually(t, 5*time.Second, "the late event", func() bool { return strings.Contains(stdout.String(), "test.late") })

	cancel()
	got := await(t, done, 5*time.Second)
	if got.code != ExitOK {
		t.Errorf("events --follow after cancel: %v", got)
	}
	for _, eventType := range []string{"test.first", "test.second", "test.late"} {
		if n := strings.Count(got.stdout, eventType); n != 1 {
			t.Errorf("%s printed %d times:\n%s", eventType, n, got.stdout)
		}
	}
}

func TestEventsFollowJSON(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	p := h.plugin("Follow JSON")
	p.pushEvents(event("test.first", time.Now(), map[string]any{"n": 1}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stdout := &syncBuffer{}
	done := h.background(runOptions{ctx: ctx, stdout: stdout}, "events", "Follow JSON", "--follow", "--interval", "50ms", "--json")
	p.pushEvents(event("test.second", time.Now(), map[string]any{"n": 2}))
	eventually(t, 5*time.Second, "both events", func() bool { return strings.Count(stdout.String(), "\n") == 2 })
	cancel()
	await(t, done, 5*time.Second)

	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		var one map[string]any
		if err := json.Unmarshal([]byte(line), &one); err != nil || one["type"] == nil {
			t.Errorf("line %q is not one event object (%v)", line, err)
		}
	}
}

func TestContexts(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	p := h.plugin("Contexts")
	p.publishManifest(testManifest(1))

	done := h.background(runOptions{}, "contexts", "Contexts", "territory")
	// Answer the hub's question as the plugin would.
	deadline := time.Now().Add(5 * time.Second)
	answered := false
	for !answered && time.Now().Before(deadline) {
		for _, envelope := range p.poll() {
			if envelope.Type != "context.enumerate" {
				continue
			}
			var question struct {
				RequestID string `json:"requestId"`
			}
			if err := json.Unmarshal(envelope.Body, &question); err != nil {
				t.Fatal(err)
			}
			p.poll(p.envelope("context.entries", map[string]any{
				"requestId": question.RequestID, "context": "territory",
				"entries": []map[string]any{
					{"referenceKey": "north-ridge", "label": "North Ridge", "position": []float64{4231.5, 300.2, 10620}},
					{"referenceKey": "south-bay", "label": "South Bay"},
				},
			}))
			answered = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !answered {
		t.Fatal("no context.enumerate arrived")
	}
	got := await(t, done, 10*time.Second)
	if got.code != ExitOK {
		t.Fatalf("contexts: %v", got)
	}
	for _, want := range []string{"enumerated ", " ago)", "KEY", "LABEL", "north-ridge", "North Ridge", "4231.5,300.2,10620", "south-bay"} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("contexts lacks %q:\n%s", want, got.stdout)
		}
	}

	// Within the hub's cache window the same read needs no plugin.
	cached := h.run("contexts", "Contexts", "territory", "--json")
	var entries struct {
		Entries []map[string]any `json:"entries"`
	}
	if cached.code != ExitOK || json.Unmarshal([]byte(cached.stdout), &entries) != nil || len(entries.Entries) != 2 {
		t.Errorf("contexts --json from the cache: %v", cached)
	}
	if unknown := h.run("contexts", "Contexts", "nope"); unknown.code != ExitRefused {
		t.Errorf("contexts on an undeclared context: %v", unknown)
	}
}

func TestPlayerProfile(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	p := h.plugin("Profile")
	p.publishManifest(testManifest(1))
	p.pushEvents(event("core.player.connect", time.Now(), map[string]any{
		"player": map[string]any{"platform": "steam", "id": "76561198000000001"},
	}))
	dispatched := h.run("run", "Profile", "example-mod.heal", "amount=5", "--target", "76561198000000001")
	actionID := actionIDFrom(t, dispatched.stdout)

	overview := h.run("player", "steam", "76561198000000001")
	if overview.code != ExitOK {
		t.Fatalf("player overview: %v", overview)
	}
	for _, want := range []string{"Events", "Actions", "Notes", "core.player.connect", "player", p.serverID,
		"example-mod.heal", actionID, "CREATED", "BY", "TEXT"} {
		if !strings.Contains(overview.stdout, want) {
			t.Errorf("player overview lacks %q:\n%s", want, overview.stdout)
		}
	}

	events := h.run("player", "steam", "76561198000000001", "events", "--type", "core.player.*")
	if events.code != ExitOK || !strings.Contains(events.stdout, "ROLES") || strings.Contains(events.stdout, "Actions") {
		t.Errorf("player events: %v", events)
	}
	actions := h.run("player", "steam", "76561198000000001", "actions", "--json")
	var page struct {
		Actions []map[string]any `json:"actions"`
	}
	if actions.code != ExitOK || json.Unmarshal([]byte(actions.stdout), &page) != nil || len(page.Actions) != 1 {
		t.Errorf("player actions --json: %v", actions)
	}
	if bad := h.run("player", "steam", "1", "bans"); bad.code != ExitUsage {
		t.Errorf("player section bans: %v", bad)
	}
}
