package hub_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/That1Drifter/vyshka/hub"
)

// actionRecord mirrors the Admin API action view on the wire.
type actionRecord struct {
	ID          string          `json:"id"`
	ServerID    string          `json:"serverId"`
	Code        string          `json:"code"`
	State       string          `json:"state"`
	CreatedAt   string          `json:"createdAt"`
	ExpiresAt   string          `json:"expiresAt"`
	DeliveredAt *string         `json:"deliveredAt"`
	RunningAt   *string         `json:"runningAt"`
	FinishedAt  *string         `json:"finishedAt"`
	OK          *bool           `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Error       *string         `json:"error"`
	DurationMs  *int64          `json:"durationMs"`
}

// manifestFirst publishes the heal manifest so dispatches have something to
// validate against, returning the enrolled session.
func manifestFirst(t *testing.T, server *hub.Server, name string) (createdServer, session) {
	t.Helper()
	created, live := enrolledSession(t, server, name)
	pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{
		"envelopes": []map[string]any{publishEnvelope(1, healManifest(1))},
	})
	return created, live
}

func dispatchAction(t *testing.T, server *hub.Server, serverID string, body map[string]any) (string, string) {
	t.Helper()
	var response struct {
		ActionID string `json:"actionId"`
		State    string `json:"state"`
	}
	status := call(t, server, http.MethodPost, "/api/v1/servers/"+serverID+"/actions",
		testAdminToken, body, &response)
	if status != http.StatusAccepted {
		t.Fatalf("dispatch: status = %d, want 202", status)
	}
	if response.ActionID == "" {
		t.Fatal("dispatch: response carried no actionId")
	}
	return response.ActionID, response.State
}

func getAction(t *testing.T, server *hub.Server, actionID string) actionRecord {
	t.Helper()
	var record actionRecord
	status := call(t, server, http.MethodGet, "/api/v1/actions/"+actionID, testAdminToken, nil, &record)
	if status != http.StatusOK {
		t.Fatalf("get action: status = %d, want 200", status)
	}
	return record
}

// The tracer bullet of spec section 7: dispatch, deliver, ack, result, and
// every state observable along the way.
func TestActionLifecycleEndToEnd(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, live := manifestFirst(t, server, "action walk")

	actionID, state := dispatchAction(t, server, created.Server.ID, map[string]any{
		"code":         "example-mod.heal",
		"context":      "player",
		"referenceKey": "player-1",
		"params":       map[string]any{"amount": 50},
	})
	if state != "queued" {
		t.Fatalf("dispatch answered state %q, want queued", state)
	}

	// The dispatch arrives as an action.dispatch envelope carrying everything
	// the plugin needs, including the deadline.
	delivery := poll(t, server, live.SessionToken, map[string]any{"ack": 1})
	if len(delivery.Envelopes) != 1 || delivery.Envelopes[0].Type != "action.dispatch" {
		t.Fatalf("got %+v, want one action.dispatch envelope", delivery.Envelopes)
	}
	var dispatched struct {
		ActionID     string          `json:"actionId"`
		Code         string          `json:"code"`
		Context      string          `json:"context"`
		ReferenceKey string          `json:"referenceKey"`
		Params       json.RawMessage `json:"params"`
		ExpiresAt    string          `json:"expiresAt"`
	}
	if err := json.Unmarshal(delivery.Envelopes[0].Body, &dispatched); err != nil {
		t.Fatalf("decode dispatch body: %v", err)
	}
	if dispatched.ActionID != actionID || dispatched.Code != "example-mod.heal" ||
		dispatched.Context != "player" || dispatched.ReferenceKey != "player-1" {
		t.Fatalf("dispatch body = %+v, want the dispatched fields", dispatched)
	}
	if _, err := time.Parse(time.RFC3339, dispatched.ExpiresAt); err != nil {
		t.Fatalf("expiresAt %q is not RFC 3339: %v", dispatched.ExpiresAt, err)
	}

	// The envelope ack is the delivery receipt.
	envelopeSeq := delivery.Envelopes[0].Seq
	pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{"ack": envelopeSeq})
	if record := getAction(t, server, actionID); record.State != "delivered" || record.DeliveredAt == nil {
		t.Fatalf("state = %q after the envelope ack, want delivered", record.State)
	}

	// action.ack -> running.
	pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{
		"ack": envelopeSeq,
		"envelopes": []map[string]any{{
			"v": 1, "id": "walk-ack", "type": "action.ack", "seq": 2,
			"ts":   time.Now().UTC().Format(time.RFC3339),
			"body": map[string]any{"actionId": actionID},
		}},
	})
	if record := getAction(t, server, actionID); record.State != "running" || record.RunningAt == nil {
		t.Fatalf("state = %q after action.ack, want running", record.State)
	}

	// action.result -> completed, with the payload readable back.
	pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{
		"ack": envelopeSeq,
		"envelopes": []map[string]any{{
			"v": 1, "id": "walk-result", "type": "action.result", "seq": 3,
			"ts": time.Now().UTC().Format(time.RFC3339),
			"body": map[string]any{
				"actionId": actionID, "ok": true,
				"result": map[string]any{"healedTo": 100}, "durationMs": 12,
			},
		}},
	})
	record := getAction(t, server, actionID)
	if record.State != "completed" || record.OK == nil || !*record.OK || record.FinishedAt == nil {
		t.Fatalf("state = %q ok = %v, want completed true", record.State, record.OK)
	}
	var payload struct {
		HealedTo float64 `json:"healedTo"`
	}
	if err := json.Unmarshal(record.Result, &payload); err != nil || payload.HealedTo != 100 {
		t.Errorf("result = %s, want the reported payload", record.Result)
	}
	if record.DurationMs == nil || *record.DurationMs != 12 {
		t.Errorf("durationMs = %v, want 12", record.DurationMs)
	}
}

func TestActionDispatchValidation(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, _ := manifestFirst(t, server, "dispatch validation")
	path := "/api/v1/servers/" + created.Server.ID + "/actions"

	pending := func() int {
		var record struct {
			PendingEnvelopeCount int `json:"pendingEnvelopeCount"`
		}
		call(t, server, http.MethodGet, "/api/v1/servers/"+created.Server.ID, testAdminToken, nil, &record)
		return record.PendingEnvelopeCount
	}
	baseline := pending()

	// Schema-invalid params are refused with the faults named, and nothing is
	// queued: schema-invalid input never reaches the game server.
	var failure struct {
		Error struct {
			Code    string `json:"code"`
			Details struct {
				Errors []struct {
					Path    string `json:"path"`
					Message string `json:"message"`
				} `json:"errors"`
			} `json:"details"`
		} `json:"error"`
	}
	status := call(t, server, http.MethodPost, path, testAdminToken, map[string]any{
		"code": "example-mod.heal", "params": map[string]any{"amount": 500},
	}, &failure)
	if status != http.StatusBadRequest || failure.Error.Code != "params_invalid" {
		t.Fatalf("status = %d code = %q, want 400 params_invalid", status, failure.Error.Code)
	}
	if len(failure.Error.Details.Errors) == 0 ||
		!strings.Contains(failure.Error.Details.Errors[0].Path+failure.Error.Details.Errors[0].Message, "maximum") {
		t.Errorf("details = %+v, want a fault naming the violated maximum", failure.Error.Details.Errors)
	}
	// Required-property enforcement, same path.
	if code := errorCode(t, server, http.MethodPost, path, testAdminToken, map[string]any{
		"code": "example-mod.heal", "params": map[string]any{},
	}, http.StatusBadRequest); code != "params_invalid" {
		t.Errorf("missing required param: code = %q, want params_invalid", code)
	}

	if code := errorCode(t, server, http.MethodPost, path, testAdminToken, map[string]any{
		"code": "example-mod.undeclared", "params": map[string]any{},
	}, http.StatusConflict); code != "unknown_action" {
		t.Errorf("undeclared code: error code = %q, want unknown_action", code)
	}
	if code := errorCode(t, server, http.MethodPost, path, testAdminToken,
		map[string]any{"params": map[string]any{}}, http.StatusBadRequest); code != "bad_request" {
		t.Errorf("missing code: error code = %q, want bad_request", code)
	}
	if code := errorCode(t, server, http.MethodPost, "/api/v1/servers/nope/actions", testAdminToken,
		map[string]any{"code": "example-mod.heal"}, http.StatusNotFound); code != "not_found" {
		t.Errorf("unknown server: error code = %q, want not_found", code)
	}

	// Nothing above may have queued anything.
	if after := pending(); after != baseline {
		t.Errorf("pendingEnvelopeCount moved from %d to %d across refused dispatches", baseline, after)
	}

	// A server with no manifest cannot validate anything, so it dispatches
	// nothing.
	bare := createServer(t, server, "no manifest", "test-game")
	if code := errorCode(t, server, http.MethodPost, "/api/v1/servers/"+bare.Server.ID+"/actions",
		testAdminToken, map[string]any{"code": "example-mod.heal"}, http.StatusConflict); code != "unknown_action" {
		t.Errorf("no manifest: error code = %q, want unknown_action", code)
	}
}

func TestActionDispatchIdempotency(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, live := manifestFirst(t, server, "dispatch idempotency")

	body := map[string]any{
		"code": "example-mod.heal", "params": map[string]any{"amount": 10},
		"idempotencyKey": "heal-once",
	}
	first, _ := dispatchAction(t, server, created.Server.ID, body)
	second, _ := dispatchAction(t, server, created.Server.ID, body)
	if second != first {
		t.Fatalf("retry returned %q, want the original %q", second, first)
	}

	// Exactly one dispatch envelope reached the plugin.
	delivery := poll(t, server, live.SessionToken, map[string]any{"ack": 1})
	dispatches := 0
	for _, delivered := range delivery.Envelopes {
		if delivered.Type == "action.dispatch" {
			dispatches++
		}
	}
	if dispatches != 1 {
		t.Errorf("the plugin received %d action.dispatch envelopes, want 1", dispatches)
	}
}

func TestActionExpiryAndLateResult(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, live := manifestFirst(t, server, "action expiry")

	actionID, _ := dispatchAction(t, server, created.Server.ID, map[string]any{
		"code": "example-mod.heal", "params": map[string]any{"amount": 10},
		"ttlSeconds": 1,
	})

	deadline := time.Now().Add(5 * time.Second)
	for {
		if record := getAction(t, server, actionID); record.State == "expired" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the action never expired")
		}
		time.Sleep(100 * time.Millisecond)
	}

	// A late result is acked and ignored: the operator already saw expired.
	response := pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{
		"envelopes": []map[string]any{{
			"v": 1, "id": "late-result", "type": "action.result", "seq": 2,
			"ts":   time.Now().UTC().Format(time.RFC3339),
			"body": map[string]any{"actionId": actionID, "ok": true, "result": map[string]any{}},
		}},
	})
	if response.Ack != 2 {
		t.Fatalf("ack = %d, want 2: a late result is still acked", response.Ack)
	}
	if record := getAction(t, server, actionID); record.State != "expired" {
		t.Errorf("state = %q after a late result, want it held at expired", record.State)
	}
}

func TestActionFailureAndOversizedResult(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, live := manifestFirst(t, server, "action failure")

	failedID, _ := dispatchAction(t, server, created.Server.ID, map[string]any{
		"code": "example-mod.heal", "params": map[string]any{"amount": 10},
	})
	oversizedID, _ := dispatchAction(t, server, created.Server.ID, map[string]any{
		"code": "example-mod.heal", "params": map[string]any{"amount": 20},
	})

	pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{
		"ack": 1,
		"envelopes": []map[string]any{
			{
				"v": 1, "id": "fail-result", "type": "action.result", "seq": 2,
				"ts": time.Now().UTC().Format(time.RFC3339),
				"body": map[string]any{
					"actionId": failedID, "ok": false, "error": "player is offline",
				},
			},
			{
				"v": 1, "id": "big-result", "type": "action.result", "seq": 3,
				"ts": time.Now().UTC().Format(time.RFC3339),
				"body": map[string]any{
					"actionId": oversizedID, "ok": true,
					"result": map[string]any{"blob": strings.Repeat("x", 70_000)},
				},
			},
		},
	})

	failed := getAction(t, server, failedID)
	if failed.State != "failed" || failed.OK == nil || *failed.OK {
		t.Errorf("state = %q ok = %v, want failed false", failed.State, failed.OK)
	}
	if failed.Error == nil || *failed.Error != "player is offline" {
		t.Errorf("error = %v, want the plugin's message", failed.Error)
	}

	// An oversized result finishes the action but drops the payload, saying so.
	oversized := getAction(t, server, oversizedID)
	if oversized.State != "completed" {
		t.Errorf("state = %q for an oversized result, want completed", oversized.State)
	}
	if len(oversized.Result) != 0 && string(oversized.Result) != "null" {
		t.Errorf("result = %d bytes survived the 64 KiB cap", len(oversized.Result))
	}
	if oversized.Error == nil || !strings.Contains(*oversized.Error, "64 KiB") {
		t.Errorf("error = %v, want a note that the payload was dropped", oversized.Error)
	}
}

// An actionId is not a credential: another server's plugin must not be able
// to move it, however it learned the id.
func TestActionEffectsAreScopedToTheirServer(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	victim, _ := manifestFirst(t, server, "forgery victim")
	attacker, attackerLive := manifestFirst(t, server, "forgery attacker")

	actionID, _ := dispatchAction(t, server, victim.Server.ID, map[string]any{
		"code": "example-mod.heal", "params": map[string]any{"amount": 10},
	})

	// The attacker's plugin sends an ack and a forged failure for the victim's
	// action. Both envelopes are acked (their durable effect is nothing), and
	// the victim's action does not move.
	response := pollNow(t, server, attacker.Server.ID, attackerLive.SessionToken, map[string]any{
		"envelopes": []map[string]any{
			{
				"v": 1, "id": "forged-ack", "type": "action.ack", "seq": 2,
				"ts":   time.Now().UTC().Format(time.RFC3339),
				"body": map[string]any{"actionId": actionID},
			},
			{
				"v": 1, "id": "forged-result", "type": "action.result", "seq": 3,
				"ts": time.Now().UTC().Format(time.RFC3339),
				"body": map[string]any{
					"actionId": actionID, "ok": false, "error": "forged",
				},
			},
		},
	})
	if response.Ack != 3 {
		t.Fatalf("ack = %d, want 3: the envelopes themselves are ordinary traffic", response.Ack)
	}

	record := getAction(t, server, actionID)
	if record.State != "queued" {
		t.Fatalf("state = %q after another server's forged result, want queued", record.State)
	}
	if record.Error != nil {
		t.Errorf("error = %q was written by another server's plugin", *record.Error)
	}
}

// The idempotency contract survives a manifest change: a retry of an accepted
// action returns the original even when a fresh dispatch of the same body
// would now be refused.
func TestActionRetrySurvivesAManifestChange(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, live := manifestFirst(t, server, "retry survives republish")

	body := map[string]any{
		"code": "example-mod.heal", "params": map[string]any{"amount": 10},
		"idempotencyKey": "heal-before-republish",
	}
	original, _ := dispatchAction(t, server, created.Server.ID, body)

	// Revision 2 drops the heal action entirely; a fresh dispatch would be
	// unknown_action now.
	replacement := healManifest(2, map[string]any{"code": "example-mod.revive"})
	pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{
		"envelopes": []map[string]any{publishEnvelope(2, replacement)},
	})
	if code := errorCode(t, server, http.MethodPost, "/api/v1/servers/"+created.Server.ID+"/actions",
		testAdminToken, map[string]any{"code": "example-mod.heal", "params": map[string]any{"amount": 10}},
		http.StatusConflict); code != "unknown_action" {
		t.Fatalf("fresh dispatch after the republish: code = %q, want unknown_action", code)
	}

	retry, _ := dispatchAction(t, server, created.Server.ID, body)
	if retry != original {
		t.Errorf("retry returned %q, want the original %q despite the republish", retry, original)
	}
}

// An expired, never-delivered dispatch stops counting against the queue bound.
func TestExpiredDispatchesLeaveTheQueue(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, _ := manifestFirst(t, server, "expired queue")

	pending := func() int {
		var record struct {
			PendingEnvelopeCount int `json:"pendingEnvelopeCount"`
		}
		call(t, server, http.MethodGet, "/api/v1/servers/"+created.Server.ID, testAdminToken, nil, &record)
		return record.PendingEnvelopeCount
	}
	baseline := pending()

	actionID, _ := dispatchAction(t, server, created.Server.ID, map[string]any{
		"code": "example-mod.heal", "params": map[string]any{"amount": 10},
		"ttlSeconds": 1,
	})
	if pending() != baseline+1 {
		t.Fatalf("the dispatch envelope was not queued")
	}

	deadline := time.Now().Add(5 * time.Second)
	for getAction(t, server, actionID).State != "expired" {
		if time.Now().After(deadline) {
			t.Fatal("the action never expired")
		}
		time.Sleep(100 * time.Millisecond)
	}
	deadline = time.Now().Add(3 * time.Second)
	for pending() != baseline {
		if time.Now().After(deadline) {
			t.Fatalf("pendingEnvelopeCount = %d long after expiry, want the baseline %d",
				pending(), baseline)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// A TTL large enough to overflow time.Duration arithmetic must clamp to the
// maximum, not wrap under the minimum.
func TestActionTTLClampsWithoutOverflow(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, _ := manifestFirst(t, server, "ttl overflow")

	actionID, _ := dispatchAction(t, server, created.Server.ID, map[string]any{
		"code": "example-mod.heal", "params": map[string]any{"amount": 10},
		"ttlSeconds": int64(9_223_372_037),
	})
	record := getAction(t, server, actionID)
	expiresAt, err := time.Parse(time.RFC3339, record.ExpiresAt)
	if err != nil {
		t.Fatalf("expiresAt %q: %v", record.ExpiresAt, err)
	}
	lifetime := time.Until(expiresAt)
	if lifetime < 23*time.Hour || lifetime > 25*time.Hour {
		t.Errorf("an overflowing ttlSeconds produced a %s lifetime, want the 24 h cap", lifetime)
	}
	if record.State == "expired" {
		t.Error("an overflowing ttlSeconds expired the action immediately")
	}
}

// A request body must be one JSON value: trailing content is rejected, not
// silently swallowed past the size cap.
func TestRequestBodiesRejectTrailingContent(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, _ := manifestFirst(t, server, "trailing content")

	raw := `{"code": "example-mod.heal", "params": {"amount": 10}}{"smuggled": true}`
	request := httptest.NewRequest(http.MethodPost,
		"/api/v1/servers/"+created.Server.ID+"/actions", strings.NewReader(raw))
	request.Header.Set("Authorization", "Bearer "+testAdminToken)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d for a body with trailing JSON, want 400 (body %s)",
			recorder.Code, recorder.Body.String())
	}
}

func TestActionReadErrors(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)

	if code := errorCode(t, server, http.MethodGet, "/api/v1/actions/no-such-action",
		testAdminToken, nil, http.StatusNotFound); code != "not_found" {
		t.Errorf("unknown action: error code = %q, want not_found", code)
	}
	if code := errorCode(t, server, http.MethodGet, "/api/v1/actions/no-such-action",
		"", nil, http.StatusUnauthorized); code != "unauthorized" {
		t.Errorf("no admin token: error code = %q, want unauthorized", code)
	}
}
