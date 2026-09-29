package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/That1Drifter/vyshka/client"
)

// readAction reads an action back through the Admin API, independently of
// the command under test.
func (h *testHub) readAction(id string) map[string]any {
	h.t.Helper()
	var action map[string]any
	h.mustCall(http.MethodGet, "/api/v1/actions/"+id, testToken, nil, &action, http.StatusOK)
	return action
}

func TestRunRefusesBadParamsLocally(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	p := h.plugin("Local")
	p.publishManifest(testManifest(1))

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"above the maximum", []string{"example-mod.heal", "amount=500"}, "amount: 500 is above the maximum 100"},
		{"unknown key", []string{"example-mod.heal", "amt=5"}, `unknown param "amt"; declared params: amount, reason`},
		{"missing required", []string{"example-mod.heal", "reason=x"}, "missing required params: amount"},
		{"bad vector", []string{"example-mod.teleport", "position=1,north"}, `position[1]: "north" is not a finite number`},
		{"unknown code", []string{"example-mod.nope"}, "example-mod.heal, example-mod.ping, example-mod.teleport, example-mod.wipe"},
	}
	for _, c := range cases {
		got := h.run(append([]string{"run", "Local"}, c.args...)...)
		if got.code != ExitUsage || !strings.Contains(got.stderr, c.want) {
			t.Errorf("%s: %v\nwant exit 1 and %q", c.name, got, c.want)
		}
	}
	if sent := h.dispatches.Load(); sent != 0 {
		t.Errorf("%d dispatches reached the hub; a local refusal must send nothing", sent)
	}

	// The same kind of arguments, valid, do reach it: the counter can count.
	if got := h.run("run", "Local", "example-mod.teleport", "position=4501.2,320.1,9800.4", "--target", "76561198000000001"); got.code != ExitOK {
		t.Fatalf("valid run: %v", got)
	}
	if sent := h.dispatches.Load(); sent != 1 {
		t.Errorf("%d dispatches, want 1", sent)
	}
}

func TestRunSendsTypedParamsTargetAndContext(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	p := h.plugin("Typed")
	p.publishManifest(testManifest(1))

	got := h.run("run", "Typed", "example-mod.teleport", "position=4501.2,320.1,9800.4",
		"--target", "76561198000000001", "--ttl", "90s", "--idempotency-key", "tp-1")
	if got.code != ExitOK || !strings.Contains(got.stdout, "state:") {
		t.Fatalf("run: %v", got)
	}
	action := h.readAction(actionIDFrom(t, got.stdout))
	if action["context"] != "player" || action["referenceKey"] != "76561198000000001" ||
		action["idempotencyKey"] != "tp-1" {
		t.Errorf("action = %v", action)
	}
	params, _ := json.Marshal(action["params"])
	if string(params) != `{"position":[4501.2,320.1,9800.4]}` {
		t.Errorf("params = %s", params)
	}
	created, _ := time.Parse(time.RFC3339, action["createdAt"].(string))
	expires, _ := time.Parse(time.RFC3339, action["expiresAt"].(string))
	if lifetime := expires.Sub(created); lifetime < 89*time.Second || lifetime > 91*time.Second {
		t.Errorf("lifetime = %s, want the 90s --ttl", lifetime)
	}

	// The idempotency key answers a retry with the original.
	again := h.run("run", "Typed", "example-mod.teleport", "position=1,2", "--idempotency-key", "tp-1", "--json")
	var dispatched struct {
		ActionID string `json:"actionId"`
	}
	if again.code != ExitOK || json.Unmarshal([]byte(again.stdout), &dispatched) != nil ||
		dispatched.ActionID != action["id"] {
		t.Errorf("idempotent retry: %v", again)
	}

	if got := h.run("run", "Typed", "example-mod.ping", "--ttl", "500ms"); got.code != ExitUsage {
		t.Errorf("a sub-second --ttl was accepted: %v", got)
	}
}

func TestRunDangerLevels(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	p := h.plugin("Danger")
	p.publishManifest(testManifest(1))

	warned := h.run("run", "Danger", "example-mod.heal", "amount=5", "--target", "7")
	if warned.code != ExitOK || !strings.Contains(warned.stderr, "warning: example-mod.heal is marked warning by its plugin") {
		t.Errorf("warning action: %v", warned)
	}
	quiet := h.run("run", "Danger", "example-mod.ping")
	if quiet.code != ExitOK || quiet.stderr != "" {
		t.Errorf("an action with no danger printed something: %v", quiet)
	}
	sent := h.dispatches.Load()

	refused := h.run("run", "Danger", "example-mod.wipe")
	if refused.code != ExitUsage || !strings.Contains(refused.stderr, "--yes") {
		t.Errorf("destructive without a terminal: %v", refused)
	}
	declined := h.runWith(runOptions{terminal: true, stdin: "n\n"}, "run", "Danger", "example-mod.wipe")
	if declined.code != ExitUsage || !strings.Contains(declined.stderr, "run example-mod.wipe on Danger (destructive)? [y/N] ") ||
		!strings.Contains(declined.stderr, "not confirmed") {
		t.Errorf("declined: %v", declined)
	}
	silent := h.runWith(runOptions{terminal: true, stdin: ""}, "run", "Danger", "example-mod.wipe")
	if silent.code != ExitUsage {
		t.Errorf("no answer at all was taken as a yes: %v", silent)
	}
	if got := h.dispatches.Load(); got != sent {
		t.Errorf("%d dispatches went out for refused or declined runs", got-sent)
	}

	for _, answer := range []string{"y\n", "Yes\n"} {
		confirmed := h.runWith(runOptions{terminal: true, stdin: answer}, "run", "Danger", "example-mod.wipe")
		if confirmed.code != ExitOK || !strings.Contains(confirmed.stderr, "[y/N]") {
			t.Errorf("confirmed with %q at the terminal: %v", answer, confirmed)
		}
	}
	forced := h.run("run", "Danger", "example-mod.wipe", "--yes")
	if forced.code != ExitOK || strings.Contains(forced.stderr, "[y/N]") {
		t.Errorf("--yes: %v", forced)
	}
	if got := h.dispatches.Load(); got != sent+3 {
		t.Errorf("%d dispatches after three confirmed runs, want %d", got-sent, 3)
	}
}

func TestRunWaitCompleted(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	p := h.plugin("Wait")
	p.publishManifest(testManifest(1))

	done := h.background(runOptions{}, "run", "Wait", "example-mod.heal", "amount=100", "--target", "7", "--wait")
	dispatch := p.nextDispatch()
	if dispatch.Code != "example-mod.heal" || dispatch.Params["amount"] != float64(100) || dispatch.ReferenceKey != "7" {
		t.Errorf("dispatch = %+v", dispatch)
	}
	p.finish(dispatch.ActionID, true, map[string]any{"healedTo": 100.0, "position": []float64{4501.2, 320.1, 9800.4}}, "")

	got := await(t, done, 10*time.Second)
	if got.code != ExitOK {
		t.Fatalf("run --wait: %v", got)
	}
	if !strings.Contains(got.stdout, `"healedTo": 100`) || !strings.Contains(got.stdout, "4501.2") {
		t.Errorf("stdout lacks the result payload:\n%s", got.stdout)
	}
	if !strings.Contains(got.stderr, "queued -> ") || !strings.Contains(got.stderr, "-> completed") {
		t.Errorf("stderr lacks the progress line:\n%s", got.stderr)
	}
}

func TestRunWaitFailed(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	p := h.plugin("Fail")
	p.publishManifest(testManifest(1))

	done := h.background(runOptions{}, "run", "Fail", "example-mod.ping", "--wait")
	p.finish(p.nextDispatch().ActionID, false, nil, "player not found")
	got := await(t, done, 10*time.Second)
	if got.code != ExitActionFailed || !strings.Contains(got.stderr, "vyshka: action failed: player not found") {
		t.Errorf("run --wait on a failure: %v", got)
	}
}

func TestRunWaitExpired(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	p := h.plugin("Expire")
	p.publishManifest(testManifest(1))

	start := time.Now()
	got := h.run("run", "Expire", "example-mod.ping", "--ttl", "1s", "--wait")
	if got.code != ExitActionExpired || !strings.Contains(got.stderr, "expired") {
		t.Errorf("run --wait with nobody answering: %v", got)
	}
	if took := time.Since(start); took > 4*time.Second {
		t.Errorf("expiry took %s to be reported; the hub flips it within a second of the 1s deadline", took)
	}
}

func TestRunWaitDeadline(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	p := h.plugin("Deadline")
	p.publishManifest(testManifest(1))

	start := time.Now()
	got := h.run("run", "Deadline", "example-mod.ping", "--wait", "--timeout", "300ms")
	if got.code != ExitWaitTimeout {
		t.Fatalf("run --wait --timeout: %v", got)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("a 300ms wait took %s", took)
	}
	// The id is printed so the action can be read later.
	dispatch := p.nextDispatch()
	if !strings.Contains(got.stderr, dispatch.ActionID) || !strings.Contains(got.stderr, "vyshka job "+dispatch.ActionID) {
		t.Errorf("stderr lacks the action id %s:\n%s", dispatch.ActionID, got.stderr)
	}
}

func TestRunWaitJSONPrintsTheFinalRecordOnly(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	p := h.plugin("JSON")
	p.publishManifest(testManifest(1))

	done := h.background(runOptions{}, "--json", "run", "JSON", "example-mod.heal", "amount=5", "--target", "7", "--wait")
	p.finish(p.nextDispatch().ActionID, true, map[string]any{"healedTo": 5}, "")
	got := await(t, done, 10*time.Second)
	if got.code != ExitOK || strings.Count(got.stdout, "\n") != 1 {
		t.Fatalf("run --wait --json: %v", got)
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(got.stdout), &record); err != nil {
		t.Fatalf("stdout is not one JSON object: %v\n%s", err, got.stdout)
	}
	if record["state"] != "completed" || record["ok"] != true {
		t.Errorf("record = %v", record)
	}
}

func TestRunResolvesPlayerFromTheSnapshot(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	p := h.plugin("Players")
	p.publishManifest(testManifest(1))

	noSnapshot := h.run("run", "Players", "example-mod.heal", "amount=5", "--player", "survivor")
	if noSnapshot.code != ExitUsage || !strings.Contains(noSnapshot.stderr, "no players snapshot") {
		t.Errorf("--player without a snapshot: %v", noSnapshot)
	}

	p.pushPlayers(time.Now(),
		map[string]any{"player": map[string]any{"platform": "steam", "id": "76561198000000001"}, "name": "Survivor"},
		map[string]any{"player": map[string]any{"platform": "steam", "id": "76561198000000002"}, "name": "Survivor Two"},
		map[string]any{"player": map[string]any{"platform": "epic", "id": "abc123"}, "name": "Bandit"},
	)

	cases := []struct {
		fragment, id, label string
	}{
		{"survivor", "76561198000000001", "Survivor (steam:76561198000000001)"},              // exact name, any case
		{"76561198000000002", "76561198000000002", "Survivor Two (steam:76561198000000002)"}, // exact id
		{"band", "abc123", "Bandit (epic:abc123)"},                                           // unique part of a name
	}
	for _, c := range cases {
		got := h.run("run", "Players", "example-mod.heal", "amount=5", "--player", c.fragment)
		if got.code != ExitOK {
			t.Errorf("--player %s: %v", c.fragment, got)
			continue
		}
		if !strings.Contains(got.stderr, `resolved "`+c.fragment+`" to `+c.label+" from the players snapshot captured ") ||
			!strings.Contains(got.stderr, " ago)") {
			t.Errorf("--player %s notice:\n%s", c.fragment, got.stderr)
		}
		if action := h.readAction(actionIDFrom(t, got.stdout)); action["referenceKey"] != c.id {
			t.Errorf("--player %s: referenceKey = %v, want %s", c.fragment, action["referenceKey"], c.id)
		}
	}

	sent := h.dispatches.Load()
	ambiguous := h.run("run", "Players", "example-mod.heal", "amount=5", "--player", "surv")
	if ambiguous.code != ExitUsage || !strings.Contains(ambiguous.stderr, "more than one player") ||
		!strings.Contains(ambiguous.stderr, "Survivor (steam:76561198000000001)") ||
		!strings.Contains(ambiguous.stderr, "Survivor Two (steam:76561198000000002)") ||
		!strings.Contains(ambiguous.stderr, "captured ") {
		t.Errorf("ambiguous --player: %v", ambiguous)
	}
	if none := h.run("run", "Players", "example-mod.heal", "amount=5", "--player", "nobody"); none.code != ExitUsage {
		t.Errorf("--player nobody: %v", none)
	}
	both := h.run("run", "Players", "example-mod.heal", "amount=5", "--player", "band", "--target", "x")
	if both.code != ExitUsage || !strings.Contains(both.stderr, "--target and --player") {
		t.Errorf("--player with --target: %v", both)
	}
	if empty := h.run("run", "Players", "example-mod.heal", "amount=5", "--player", ""); empty.code != ExitUsage {
		t.Errorf("--player with nothing to look for: %v", empty)
	}
	if got := h.dispatches.Load(); got != sent {
		t.Errorf("%d dispatches went out for unresolved players", got-sent)
	}
}

func TestJob(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	p := h.plugin("Job")
	p.publishManifest(testManifest(1))

	dispatched := h.run("run", "Job", "example-mod.heal", "amount=42", "--target", "7")
	id := actionIDFrom(t, dispatched.stdout)

	queued := h.run("job", id)
	if queued.code != ExitOK || !strings.Contains(queued.stdout, "queued") ||
		!strings.Contains(queued.stdout, `{"amount":42}`) || !strings.Contains(queued.stdout, "result: none") {
		t.Errorf("job on a queued action: %v", queued)
	}

	done := h.background(runOptions{}, "job", id, "--wait")
	p.finish(p.nextDispatch().ActionID, true, map[string]any{"healedTo": 42}, "")
	waited := await(t, done, 10*time.Second)
	if waited.code != ExitOK || !strings.Contains(waited.stdout, `"healedTo": 42`) {
		t.Errorf("job --wait: %v", waited)
	}

	finished := h.run("job", id)
	if finished.code != ExitOK || !strings.Contains(finished.stdout, "completed") ||
		!strings.Contains(finished.stdout, "7ms") || !strings.Contains(finished.stdout, `"healedTo": 42`) {
		t.Errorf("job on a completed action: %v", finished)
	}
	if missing := h.run("job", "01JNOSUCHACTION0000000000"); missing.code != ExitRefused ||
		!strings.Contains(missing.stderr, "not_found") {
		t.Errorf("job on an unknown id: %v", missing)
	}
}

func TestRefusalPrintsDetails(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	p := h.plugin("Details")
	p.publishManifest(testManifest(1))

	// The local check stops what the hub would refuse with params_invalid,
	// so the refusal comes from the hub through the client directly, and the
	// command's printer is checked against what the hub really sends.
	hub, err := client.New(h.url, testToken)
	if err != nil {
		t.Fatal(err)
	}
	_, err = hub.DispatchAction(context.Background(), p.serverID, client.DispatchRequest{
		Code: "example-mod.heal", Params: map[string]any{"amount": 500},
	})
	var refusal *client.Error
	if !errors.As(err, &refusal) || refusal.Code != "params_invalid" {
		t.Fatalf("the hub answered %v", err)
	}

	var stderr syncBuffer
	e := newEnv(IO{Stderr: &stderr})
	if code := e.report(err); code != ExitRefused {
		t.Errorf("exit %d, want %d", code, ExitRefused)
	}
	if !strings.HasPrefix(stderr.String(), "vyshka: params_invalid: ") ||
		!strings.Contains(stderr.String(), "\n  amount: 500 is above the maximum 100\n") {
		t.Errorf("the refusal printed as:\n%s", stderr.String())
	}
}
