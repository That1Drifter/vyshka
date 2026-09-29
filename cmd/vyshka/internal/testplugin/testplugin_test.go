package testplugin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/That1Drifter/vyshka/cmd/vyshka/internal/testplugin"
	"github.com/That1Drifter/vyshka/hub"
)

// adminToken is the bootstrap Admin API credential the test hub boots with, so
// no test has to scrape it out of a log.
const adminToken = "vya_TESTTOKENTESTTOKENTESTTO"

// rig is a hub served over real HTTP, which is what the plugin under test
// speaks.
type rig struct {
	t   *testing.T
	url string
	api *http.Client
}

func newRig(t *testing.T) *rig {
	t.Helper()
	server, err := hub.New(context.Background(), hub.Config{
		DatabaseURL: filepath.Join(t.TempDir(), "test.db"),
		AdminToken:  adminToken,
		Logger:      slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("boot hub: %v", err)
	}
	// Cleanups run last in, first out: plugins registered later close before the
	// listener and the database go away.
	t.Cleanup(func() { server.Close() })
	listener := httptest.NewServer(server.Handler())
	t.Cleanup(listener.Close)
	return &rig{t: t, url: listener.URL, api: &http.Client{Timeout: 15 * time.Second}}
}

// call sends an Admin API request and decodes the answer into out when it is
// non-nil, returning the status.
func (r *rig) call(method, path string, body, out any) int {
	r.t.Helper()
	var reader io.Reader = http.NoBody
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			r.t.Fatalf("encode %s %s: %v", method, path, err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, r.url+path, reader)
	if err != nil {
		r.t.Fatalf("build %s %s: %v", method, path, err)
	}
	request.Header.Set("Authorization", "Bearer "+adminToken)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := r.api.Do(request)
	if err != nil {
		r.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		r.t.Fatalf("read %s %s: %v", method, path, err)
	}
	if out != nil && len(raw) > 0 && response.StatusCode/100 == 2 {
		if err := json.Unmarshal(raw, out); err != nil {
			r.t.Fatalf("decode %s %s answer %q: %v", method, path, raw, err)
		}
	}
	return response.StatusCode
}

// start creates a server through the Admin API and starts a plugin against it.
// The plugin is closed at cleanup, and a fatal error it reports fails the test.
func (r *rig) start(mutate func(*testplugin.Options)) *testplugin.Plugin {
	r.t.Helper()
	var created struct {
		Enrollment struct {
			Token string `json:"token"`
		} `json:"enrollment"`
	}
	if status := r.call(http.MethodPost, "/api/v1/servers", map[string]any{"name": "plugin under test", "game": "cli-test"}, &created); status != http.StatusCreated {
		r.t.Fatalf("create server: status %d, want 201", status)
	}
	options := testplugin.Options{
		HubURL:          r.url,
		EnrollmentToken: created.Enrollment.Token,
		Logf:            r.t.Logf,
	}
	if mutate != nil {
		mutate(&options)
	}
	plugin, err := testplugin.Start(context.Background(), options)
	if err != nil {
		r.t.Fatalf("start plugin: %v", err)
	}
	r.t.Cleanup(func() {
		plugin.Close()
		if err := plugin.Err(); err != nil {
			r.t.Errorf("plugin reported a fatal error: %v", err)
		}
	})
	return plugin
}

// waitFor polls a condition every 50 ms until it holds or the deadline passes.
func waitFor(t *testing.T, within time.Duration, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("gave up after %s waiting for %s", within, what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// awaitManifest waits until the hub has accepted the plugin's manifest, which
// is what a dispatch needs before it can validate anything.
func (r *rig) awaitManifest(serverID string) {
	r.t.Helper()
	waitFor(r.t, 5*time.Second, "the manifest to be accepted", func() bool {
		return r.call(http.MethodGet, "/api/v1/servers/"+serverID+"/manifest", nil, nil) == http.StatusOK
	})
}

type actionRecord struct {
	ID         string          `json:"id"`
	State      string          `json:"state"`
	CreatedAt  time.Time       `json:"createdAt"`
	FinishedAt *time.Time      `json:"finishedAt"`
	OK         *bool           `json:"ok"`
	Result     json.RawMessage `json:"result"`
	Error      *string         `json:"error"`
}

func (r *rig) dispatch(serverID string, body map[string]any) string {
	r.t.Helper()
	var accepted struct {
		ActionID string `json:"actionId"`
	}
	if status := r.call(http.MethodPost, "/api/v1/servers/"+serverID+"/actions", body, &accepted); status != http.StatusAccepted {
		r.t.Fatalf("dispatch %v: status %d, want 202", body["code"], status)
	}
	return accepted.ActionID
}

// awaitState waits for an action to reach one of the states and returns its record.
func (r *rig) awaitState(actionID string, within time.Duration, states ...string) actionRecord {
	r.t.Helper()
	var record actionRecord
	waitFor(r.t, within, fmt.Sprintf("action %s to reach %v", actionID, states), func() bool {
		record = actionRecord{}
		if status := r.call(http.MethodGet, "/api/v1/actions/"+actionID, nil, &record); status != http.StatusOK {
			r.t.Fatalf("read action: status %d", status)
		}
		for _, state := range states {
			if record.State == state {
				return true
			}
		}
		return false
	})
	return record
}

func TestDefaultHandlerAnswers(t *testing.T) {
	cases := []struct {
		name   string
		code   string
		params map[string]any
		key    string
		result string // the result as JSON; empty when the handler must stay silent
		errMsg string
		delay  time.Duration
	}{
		{name: "heal", code: "cli-test.heal", params: map[string]any{"amount": 5.0}, result: `{"amount":5,"healedTo":100}`},
		{name: "teleport", code: "cli-test.teleport", params: map[string]any{"position": []any{1.0, 2.0, 3.0}}, result: `{"position":[1,2,3]}`},
		{name: "wipe", code: "cli-test.wipe", params: map[string]any{"reason": "test"}, result: `{"wiped":true}`},
		{name: "echo", code: "cli-test.echo", params: map[string]any{"a": "b"}, result: `{"params":{"a":"b"}}`},
		{name: "echo without params", code: "cli-test.echo", result: `{"params":{}}`},
		{name: "slow", code: "cli-test.slow", params: map[string]any{"delayMs": 250.0}, result: `{"slept":250}`, delay: 250 * time.Millisecond},
		{name: "fail", code: "cli-test.fail", errMsg: "failed as asked"},
		{name: "never", code: "cli-test.never"},
		{name: "pick", code: "cli-test.pick", key: "zone-a", params: map[string]any{"mode": "hard"}, result: `{"mode":"hard","referenceKey":"zone-a"}`},
		{name: "unknown", code: "cli-test.nope", errMsg: "unknown action"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			outcome := testplugin.DefaultHandler(testplugin.Dispatch{Code: tc.code, Params: tc.params, ReferenceKey: tc.key})
			if !outcome.Ack {
				t.Error("every dispatch is acked")
			}
			if outcome.Delay != tc.delay {
				t.Errorf("delay = %s, want %s", outcome.Delay, tc.delay)
			}
			switch {
			case tc.result == "" && tc.errMsg == "":
				if outcome.Result != nil {
					t.Errorf("result = %+v, want none (the hub expires it)", outcome.Result)
				}
			case tc.errMsg != "":
				if outcome.Result == nil || outcome.Result.OK || outcome.Result.Error != tc.errMsg {
					t.Errorf("result = %+v, want a failure saying %q", outcome.Result, tc.errMsg)
				}
			default:
				if outcome.Result == nil || !outcome.Result.OK {
					t.Fatalf("result = %+v, want success", outcome.Result)
				}
				got, err := json.Marshal(outcome.Result.Result)
				if err != nil || string(got) != tc.result {
					t.Errorf("result body = %s (%v), want %s", got, err, tc.result)
				}
			}
		})
	}
}

// A caller edits the default to make its own (a higher revision, one action
// fewer); that must not leak into the next caller's copy.
func TestDefaultManifestIsFreshEachCall(t *testing.T) {
	first := testplugin.DefaultManifest()
	first["manifestRevision"] = 99
	first["actions"].([]map[string]any)[0]["code"] = "mutated"
	first["kvNamespaces"].([]string)[0] = "mutated"

	second := testplugin.DefaultManifest()
	if second["manifestRevision"] != 1 || second["actions"].([]map[string]any)[0]["code"] != "cli-test.heal" || second["kvNamespaces"].([]string)[0] != "cli-test" {
		t.Errorf("a later DefaultManifest shows the earlier caller's edits: %v", second["manifestRevision"])
	}
}

func TestDefaultManifestIsAccepted(t *testing.T) {
	r := newRig(t)
	plugin := r.start(nil)

	var view struct {
		Revision int            `json:"revision"`
		Manifest map[string]any `json:"manifest"`
	}
	waitFor(t, 5*time.Second, "the manifest to be accepted", func() bool {
		return r.call(http.MethodGet, "/api/v1/servers/"+plugin.ServerID()+"/manifest", nil, &view) == http.StatusOK
	})
	if view.Revision != 1 {
		t.Errorf("revision = %d, want 1", view.Revision)
	}
	actions, _ := view.Manifest["actions"].([]any)
	if len(actions) != 8 {
		t.Errorf("stored manifest has %d actions, want the 8 the default declares", len(actions))
	}
	if rejections := plugin.Rejections(); len(rejections) != 0 {
		t.Errorf("hub rejected the default manifest: %v", rejections)
	}
}

// A rejection has to be visible, or the acceptance test above could pass
// against a plugin that never listens for one.
func TestRejectedManifestIsSurfaced(t *testing.T) {
	r := newRig(t)
	manifest := testplugin.DefaultManifest()
	manifest["actions"] = []map[string]any{{
		"code":   "cli-test.bad",
		"params": map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string", "pattern": "^a"}}},
	}}
	plugin := r.start(func(o *testplugin.Options) { o.Manifest = manifest })

	waitFor(t, 5*time.Second, "a manifest.reject", func() bool { return len(plugin.Rejections()) > 0 })
	rejection := plugin.Rejections()[0]
	if faults, _ := rejection["errors"].([]any); len(faults) == 0 {
		t.Errorf("rejection carried no errors: %v", rejection)
	}
	if status := r.call(http.MethodGet, "/api/v1/servers/"+plugin.ServerID()+"/manifest", nil, nil); status != http.StatusNotFound {
		t.Errorf("manifest read after a rejection = %d, want 404", status)
	}
}

// The result has to reach the hub as soon as it is produced. A plugin that only
// sent it with its next held poll would take the whole 5 s hold, so the bound
// here is what tells the two apart.
func TestHealCompletesWithoutWaitingForTheHold(t *testing.T) {
	r := newRig(t)
	plugin := r.start(nil)
	r.awaitManifest(plugin.ServerID())

	started := time.Now()
	id := r.dispatch(plugin.ServerID(), map[string]any{
		"code": "cli-test.heal", "context": "player", "referenceKey": "player-1",
		"params": map[string]any{"amount": 5},
	})
	record := r.awaitState(id, 5*time.Second, "completed", "failed", "expired")
	elapsed := time.Since(started)

	if record.State != "completed" || record.OK == nil || !*record.OK {
		t.Fatalf("state = %q ok = %v, want completed true", record.State, record.OK)
	}
	if elapsed >= 3*time.Second {
		t.Errorf("heal took %s to complete, want under 3s (the result waited for a poll hold)", elapsed)
	}
	var result map[string]any
	if err := json.Unmarshal(record.Result, &result); err != nil {
		t.Fatalf("decode result %s: %v", record.Result, err)
	}
	if want := (map[string]any{"healedTo": 100.0, "amount": 5.0}); !reflect.DeepEqual(result, want) {
		t.Errorf("result = %v, want %v", result, want)
	}

	dispatches := plugin.Dispatches()
	if len(dispatches) != 1 {
		t.Fatalf("plugin saw %d dispatches, want 1", len(dispatches))
	}
	got := dispatches[0]
	if got.ActionID != id || got.Code != "cli-test.heal" || got.Context != "player" || got.ReferenceKey != "player-1" || got.Params["amount"] != 5.0 {
		t.Errorf("dispatch = %+v, want the heal as sent", got)
	}
	if got.ExpiresAt.IsZero() {
		t.Error("dispatch carried no deadline")
	}
}

// The rest of the default actions, each through the hub's own validation of
// its schema, so a manifest constant the hub would refuse at dispatch shows up
// here rather than in a client test.
func TestOtherDefaultActionsRoundTrip(t *testing.T) {
	r := newRig(t)
	plugin := r.start(nil)
	server := plugin.ServerID()
	r.awaitManifest(server)

	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{"teleport", map[string]any{"code": "cli-test.teleport", "context": "player", "referenceKey": "p-1", "params": map[string]any{"position": []float64{1, 2.5, 3}}}, `{"position":[1,2.5,3]}`},
		{"wipe", map[string]any{"code": "cli-test.wipe", "context": "world", "params": map[string]any{"reason": "cleanup"}}, `{"wiped":true}`},
		{"echo", map[string]any{"code": "cli-test.echo", "params": map[string]any{"any": []int{1, 2}}}, `{"params":{"any":[1,2]}}`},
		{"pick", map[string]any{"code": "cli-test.pick", "context": "cli-test.zone", "referenceKey": "zone-b", "params": map[string]any{"mode": "soft"}}, `{"mode":"soft","referenceKey":"zone-b"}`},
	}
	for _, tc := range cases {
		id := r.dispatch(server, tc.body)
		record := r.awaitState(id, 3*time.Second, "completed", "failed", "expired")
		if record.State != "completed" {
			t.Errorf("%s: state = %q, want completed", tc.name, record.State)
			continue
		}
		// Compared as values: the hub is free to reformat what it stores.
		var got, want any
		if err := json.Unmarshal(record.Result, &got); err != nil {
			t.Errorf("%s: result %s is not JSON: %v", tc.name, record.Result, err)
			continue
		}
		if err := json.Unmarshal([]byte(tc.want), &want); err != nil {
			t.Fatalf("%s: bad expectation %s: %v", tc.name, tc.want, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: result = %s, want %s", tc.name, record.Result, tc.want)
		}
	}
}

func TestFailedActionCarriesItsError(t *testing.T) {
	r := newRig(t)
	plugin := r.start(nil)
	r.awaitManifest(plugin.ServerID())

	id := r.dispatch(plugin.ServerID(), map[string]any{"code": "cli-test.fail", "context": "world", "params": map[string]any{}})
	record := r.awaitState(id, 3*time.Second, "completed", "failed", "expired")
	if record.State != "failed" || record.OK == nil || *record.OK {
		t.Fatalf("state = %q ok = %v, want failed false", record.State, record.OK)
	}
	if record.Error == nil || *record.Error != "failed as asked" {
		t.Errorf("error = %v, want the text the handler gave", record.Error)
	}
}

// An action the plugin acks and never answers is the hub's to expire, at its
// own deadline and not before.
func TestUnansweredActionExpires(t *testing.T) {
	r := newRig(t)
	plugin := r.start(nil)
	r.awaitManifest(plugin.ServerID())

	started := time.Now()
	id := r.dispatch(plugin.ServerID(), map[string]any{"code": "cli-test.never", "context": "world", "params": map[string]any{}, "ttlSeconds": 1})
	record := r.awaitState(id, 4*time.Second, "completed", "failed", "expired")
	elapsed := time.Since(started)
	if record.State != "expired" {
		t.Fatalf("state = %q, want expired (the plugin must never answer)", record.State)
	}
	if elapsed < time.Second || elapsed > 2500*time.Millisecond {
		t.Errorf("expired after %s, want about the 1s deadline", elapsed)
	}
}

func TestSlowActionWaitsForItsDelay(t *testing.T) {
	r := newRig(t)
	plugin := r.start(nil)
	r.awaitManifest(plugin.ServerID())

	started := time.Now()
	id := r.dispatch(plugin.ServerID(), map[string]any{"code": "cli-test.slow", "context": "world", "params": map[string]any{"delayMs": 800}})
	record := r.awaitState(id, 4*time.Second, "completed", "failed", "expired")
	elapsed := time.Since(started)
	if record.State != "completed" {
		t.Fatalf("state = %q, want completed", record.State)
	}
	if elapsed < 800*time.Millisecond {
		t.Errorf("completed after %s, want at least the 800ms delay", elapsed)
	}
	if elapsed > 3*time.Second {
		t.Errorf("completed after %s, want the delay plus a moment, not a poll hold", elapsed)
	}
}

func TestEventsAndSnapshotsReachTheHub(t *testing.T) {
	r := newRig(t)
	plugin := r.start(nil)
	server := plugin.ServerID()
	r.awaitManifest(server)

	at := time.Now().UTC().Truncate(time.Millisecond)
	plugin.Emit(
		testplugin.Event{Type: "cli-test.beat", At: at, Data: map[string]any{"n": 7}},
		testplugin.Event{Type: "core.player.chat", Data: map[string]any{
			"player":  map[string]any{"platform": "cli-test", "id": "p-1"},
			"message": "hello",
		}},
	)
	plugin.Snapshot("players", map[string]any{
		"capturedAt": at.Format(time.RFC3339Nano),
		"players": []map[string]any{{
			"player":   map[string]any{"platform": "cli-test", "id": "p-1"},
			"name":     "Survivor",
			"position": []float64{1, 2, 3},
		}},
	})

	var events struct {
		Events []struct {
			Type string         `json:"type"`
			Data map[string]any `json:"data"`
		} `json:"events"`
	}
	waitFor(t, 3*time.Second, "both events in the feed", func() bool {
		events.Events = nil
		r.call(http.MethodGet, "/api/v1/servers/"+server+"/events", nil, &events)
		return len(events.Events) >= 2
	})
	seen := map[string]map[string]any{}
	for _, event := range events.Events {
		seen[event.Type] = event.Data
	}
	if seen["cli-test.beat"]["n"] != 7.0 {
		t.Errorf("cli-test.beat data = %v, want n=7", seen["cli-test.beat"])
	}
	if seen["core.player.chat"]["message"] != "hello" {
		t.Errorf("core.player.chat data = %v, want the message", seen["core.player.chat"])
	}

	var state struct {
		Snapshot struct {
			Players []struct {
				Name string `json:"name"`
			} `json:"players"`
		} `json:"snapshot"`
	}
	waitFor(t, 3*time.Second, "the players snapshot", func() bool {
		return r.call(http.MethodGet, "/api/v1/servers/"+server+"/state/players", nil, &state) == http.StatusOK
	})
	if len(state.Snapshot.Players) != 1 || state.Snapshot.Players[0].Name != "Survivor" {
		t.Errorf("players snapshot = %+v, want the one survivor", state.Snapshot.Players)
	}
}

// A hub refuses a batch over 200 events whole, so Emit has to split.
func TestEmitSplitsOversizedBatches(t *testing.T) {
	r := newRig(t)
	plugin := r.start(nil)
	server := plugin.ServerID()
	r.awaitManifest(server)

	events := make([]testplugin.Event, 250)
	for i := range events {
		events[i] = testplugin.Event{Type: "cli-test.beat", Data: map[string]any{"i": i}}
	}
	plugin.Emit(events...)

	var feed struct {
		Events []json.RawMessage `json:"events"`
	}
	waitFor(t, 5*time.Second, "all 250 events", func() bool {
		feed.Events = nil
		r.call(http.MethodGet, "/api/v1/servers/"+server+"/events?limit=500", nil, &feed)
		return len(feed.Events) == 250
	})
}

func TestContextEntriesAreEnumerated(t *testing.T) {
	r := newRig(t)
	plugin := r.start(nil)
	server := plugin.ServerID()
	r.awaitManifest(server)

	var view struct {
		Context string `json:"context"`
		Entries []struct {
			ReferenceKey string    `json:"referenceKey"`
			Label        string    `json:"label"`
			Position     []float64 `json:"position"`
		} `json:"entries"`
	}
	if status := r.call(http.MethodGet, "/api/v1/servers/"+server+"/contexts/cli-test.zone/entries", nil, &view); status != http.StatusOK {
		t.Fatalf("read entries: status %d, want 200", status)
	}
	if len(view.Entries) != 2 {
		t.Fatalf("entries = %+v, want the two default zones", view.Entries)
	}
	alpha, bravo := view.Entries[0], view.Entries[1]
	if alpha.ReferenceKey != "zone-a" || alpha.Label != "Alpha" || !reflect.DeepEqual(alpha.Position, []float64{100, 0, 200}) {
		t.Errorf("first entry = %+v, want zone-a Alpha at 100,0,200", alpha)
	}
	if bravo.ReferenceKey != "zone-b" || bravo.Label != "Bravo" || len(bravo.Position) != 0 {
		t.Errorf("second entry = %+v, want zone-b Bravo without a position", bravo)
	}
}

func TestOptionsOverrideHandlerAndContexts(t *testing.T) {
	r := newRig(t)
	plugin := r.start(func(o *testplugin.Options) {
		o.Handler = func(d testplugin.Dispatch) testplugin.Outcome {
			return testplugin.Outcome{Ack: true, Result: &testplugin.Result{
				OK: true, Result: map[string]any{"handledBy": "custom", "code": d.Code}, DurationMs: 42,
			}}
		}
		o.Contexts = map[string][]testplugin.ContextEntry{
			"cli-test.zone": {{ReferenceKey: "only", Label: "Only", Data: map[string]any{"k": "v"}}},
		}
	})
	server := plugin.ServerID()
	r.awaitManifest(server)

	id := r.dispatch(server, map[string]any{"code": "cli-test.echo", "params": map[string]any{"a": 1}})
	record := r.awaitState(id, 3*time.Second, "completed", "failed", "expired")
	var answer map[string]any
	if err := json.Unmarshal(record.Result, &answer); err != nil {
		t.Fatalf("decode result %s: %v", record.Result, err)
	}
	if record.State != "completed" || answer["handledBy"] != "custom" || answer["code"] != "cli-test.echo" {
		t.Errorf("state = %q result = %v, want the custom handler's answer", record.State, answer)
	}

	var view struct {
		Entries []struct {
			ReferenceKey string `json:"referenceKey"`
		} `json:"entries"`
	}
	if status := r.call(http.MethodGet, "/api/v1/servers/"+server+"/contexts/cli-test.zone/entries?refresh=true", nil, &view); status != http.StatusOK {
		t.Fatalf("read entries: status %d, want 200", status)
	}
	if len(view.Entries) != 1 || view.Entries[0].ReferenceKey != "only" {
		t.Errorf("entries = %+v, want the one the option supplied", view.Entries)
	}
}

// pluginGoroutinesRunning reports whether any goroutine is inside a method of
// the plugin, by scanning every stack for its frames.
func pluginGoroutinesRunning() bool {
	stacks := make([]byte, 1<<20)
	stacks = stacks[:runtime.Stack(stacks, true)]
	return strings.Contains(string(stacks), "testplugin.(*Plugin)")
}

// Close has to cut a held poll and a handler sitting in its delay, and leave
// nothing of the plugin running.
func TestCloseIsPromptAndLeavesNothingRunning(t *testing.T) {
	r := newRig(t)
	plugin := r.start(nil)
	server := plugin.ServerID()
	r.awaitManifest(server)

	// A handler parked in a minute-long delay, alongside the held poll.
	r.dispatch(server, map[string]any{"code": "cli-test.slow", "context": "world", "params": map[string]any{"delayMs": 60000}})
	waitFor(t, 3*time.Second, "the dispatch to arrive", func() bool { return len(plugin.Dispatches()) == 1 })

	// The detector below has to be able to see a running plugin, or "nothing
	// left running" would pass vacuously.
	if !pluginGoroutinesRunning() {
		t.Fatal("no goroutine of a running plugin found; the leak check cannot see anything")
	}

	// Closed from a goroutine, so a Close that blocks on the parked handler
	// fails this test instead of hanging the whole run.
	started := time.Now()
	closed := make(chan error, 1)
	go func() { closed <- plugin.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Errorf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close still blocked after 5s")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("Close took %s, want under 1s", elapsed)
	}

	started = time.Now()
	if err := plugin.Close(); err != nil || time.Since(started) > 100*time.Millisecond {
		t.Errorf("second Close = %v after %s, want nil and immediate", err, time.Since(started))
	}

	if pluginGoroutinesRunning() {
		t.Error("goroutines of the plugin are still running after Close")
	}
	if err := plugin.Err(); err != nil {
		t.Errorf("Err after a clean Close = %v, want nil", err)
	}
}
