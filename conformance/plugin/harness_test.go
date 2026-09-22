package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// These tests point a deliberately broken plugin at the mock hub and assert
// the violation is caught and named. The green path is covered by CI running
// the whole harness against the driver; what needs proving here is that the
// checks can fail, and fail with the message a plugin author needs.

// testPlugin is a hand-driven client for the mock hub's Plugin API.
type testPlugin struct {
	t       *testing.T
	baseURL string
	client  *http.Client

	serverID     string
	serverSecret string
	sessionToken string
}

func newTestPlugin(t *testing.T, h *mockHub) *testPlugin {
	t.Helper()
	p := &testPlugin{t: t, baseURL: h.baseURL, client: &http.Client{Timeout: 10 * time.Second}}

	var enrolled struct {
		ServerID     string `json:"serverId"`
		ServerSecret string `json:"serverSecret"`
	}
	p.post("/plugin/v1/enroll", "", map[string]any{
		"enrollmentToken": h.enrollmentToken, "game": "conformance",
	}, http.StatusCreated, &enrolled)
	p.serverID = enrolled.ServerID
	p.serverSecret = enrolled.ServerSecret
	p.startSession()
	return p
}

func (p *testPlugin) startSession() {
	p.t.Helper()
	var session struct {
		SessionToken string `json:"sessionToken"`
	}
	p.post("/plugin/v1/session", "", map[string]any{
		"serverId": p.serverID, "serverSecret": p.serverSecret,
	}, http.StatusOK, &session)
	p.sessionToken = session.SessionToken
}

func (p *testPlugin) post(path, bearer string, body any, wantStatus int, out any) {
	p.t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		p.t.Fatalf("encode %s body: %v", path, err)
	}
	request, err := http.NewRequest(http.MethodPost, p.baseURL+path, bytes.NewReader(encoded))
	if err != nil {
		p.t.Fatalf("build %s request: %v", path, err)
	}
	request.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	response, err := p.client.Do(request)
	if err != nil {
		p.t.Fatalf("POST %s: %v", path, err)
	}
	defer response.Body.Close()
	if response.StatusCode != wantStatus {
		p.t.Fatalf("POST %s: want status %d, got %d", path, wantStatus, response.StatusCode)
	}
	if out != nil {
		if err := json.NewDecoder(response.Body).Decode(out); err != nil {
			p.t.Fatalf("POST %s: decode response: %v", path, err)
		}
	}
}

// poll sends one poll exactly as given and tolerates any status, because a
// misbehaving plugin is the point of these tests.
func (p *testPlugin) poll(envelopes ...map[string]any) {
	p.t.Helper()
	body := map[string]any{"envelopes": envelopes}
	p.post("/plugin/v1/poll", p.sessionToken, body, http.StatusOK, nil)
}

// testEnvelope frames a well-formed event.batch: an empty one for a nil
// body, otherwise one event carrying the given map as its data, so two
// different maps make two different envelopes while the batch itself stays
// valid under the section 8.1 grading.
func testEnvelope(id string, seq int64, body map[string]any) map[string]any {
	events := []map[string]any{}
	if body != nil {
		events = append(events, map[string]any{"t": "conformance.test", "data": body})
	}
	return typedEnvelope(id, seq, "event.batch", map[string]any{"events": events})
}

func typedEnvelope(id string, seq int64, envelopeType string, body map[string]any) map[string]any {
	return map[string]any{
		"v": 1, "id": id, "type": envelopeType, "seq": seq,
		"ts": "2026-08-20T12:00:00Z", "body": body,
	}
}

// allFaults returns every fault the mock recorded, including the telemetry
// faults it holds for the telemetry stage.
func allFaults(h *mockHub) []fault {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append(append([]fault{}, h.faults...), h.telemetryFaults...)
}

func faultMessages(h *mockHub) string {
	faults := allFaults(h)
	messages := make([]string, 0, len(faults))
	for _, f := range faults {
		messages = append(messages, f.String())
	}
	return strings.Join(messages, "\n")
}

func TestVerbatimReplayAfterASessionChangeIsNamedPrecisely(t *testing.T) {
	h, err := startMockHub("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	p := newTestPlugin(t, h)

	// First envelope acked, second deliberately left unacked, exactly how the
	// renumber stage sets a session change up.
	p.poll(testEnvelope("evt-1", 1, nil))
	h.freezeAck()
	p.poll(testEnvelope("evt-2", 2, nil))

	if stranded := h.killSession(); stranded != 1 {
		t.Fatalf("killSession stranded %d envelopes, want 1", stranded)
	}

	// The broken plugin replays its buffer verbatim in the new session: same
	// envelope, old seq.
	p.startSession()
	p.poll(testEnvelope("evt-2", 2, nil))

	faults := faultMessages(h)
	if !strings.Contains(faults, "replayed with its old seq") {
		t.Fatalf("verbatim replay was not named; recorded faults:\n%s", faults)
	}
	if !strings.Contains(faults, "section 9.1") {
		t.Fatalf("the replay fault does not cite section 9.1; recorded faults:\n%s", faults)
	}
}

func TestRenumberedReplayAfterASessionChangeIsAccepted(t *testing.T) {
	h, err := startMockHub("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	p := newTestPlugin(t, h)
	p.poll(testEnvelope("evt-1", 1, nil))
	h.freezeAck()
	p.poll(testEnvelope("evt-2", 2, map[string]any{"marker": "kept"}))
	h.killSession()

	// The correct plugin renumbers: seq restarts at 1, everything else stays.
	p.startSession()
	p.poll(testEnvelope("evt-2", 1, map[string]any{"marker": "kept"}))

	h.mu.Lock()
	expectation := h.expectedRenumber["evt-2"]
	h.mu.Unlock()
	if expectation == nil || !expectation.arrived {
		t.Fatalf("the renumbered envelope was not marked off; recorded faults:\n%s", faultMessages(h))
	}
	if faults := faultMessages(h); faults != "" {
		t.Fatalf("a correct renumbering was faulted:\n%s", faults)
	}
}

// An id recycled for a different message in a later session is faulted: a
// hub's cross-session dedup treats equal ids as the same message (section 4),
// so it would silently drop the fresh one.
func TestCrossSessionIDReuseForADifferentMessageIsFaulted(t *testing.T) {
	h, err := startMockHub("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	p := newTestPlugin(t, h)
	p.poll(testEnvelope("evt-1", 1, map[string]any{"marker": "first"}))
	h.killSession()

	// An id generator that reset with the session hands out evt-1 again, this
	// time naming a different message.
	p.startSession()
	p.poll(testEnvelope("evt-1", 1, map[string]any{"marker": "second"}))

	faults := faultMessages(h)
	if !strings.Contains(faults, "reused in session") {
		t.Fatalf("cross-session id reuse was not faulted; recorded faults:\n%s", faults)
	}
	if !strings.Contains(faults, "section 4") {
		t.Fatalf("the reuse fault does not cite section 4; recorded faults:\n%s", faults)
	}
}

func TestAChangedRetransmissionIsFaulted(t *testing.T) {
	h, err := startMockHub("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	p := newTestPlugin(t, h)
	p.poll(testEnvelope("evt-1", 1, map[string]any{"count": 1}))
	// The retransmission arrives with the same seq but a different body.
	p.poll(testEnvelope("evt-1", 1, map[string]any{"count": 2}))

	faults := faultMessages(h)
	if !strings.Contains(faults, "retransmission") || !strings.Contains(faults, "section 9.1") {
		t.Fatalf("a changed retransmission was not faulted; recorded faults:\n%s", faults)
	}
}

func TestAFaithfulRetransmissionIsRecordedNotFaulted(t *testing.T) {
	h, err := startMockHub("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	p := newTestPlugin(t, h)
	envelope := testEnvelope("evt-1", 1, map[string]any{"count": 1})
	p.poll(envelope)
	p.poll(envelope)

	h.mu.Lock()
	retransmissions := len(h.retransmissions)
	h.mu.Unlock()
	if retransmissions != 1 {
		t.Fatalf("recorded %d retransmissions, want 1; faults:\n%s", retransmissions, faultMessages(h))
	}
	if faults := faultMessages(h); faults != "" {
		t.Fatalf("a faithful retransmission was faulted:\n%s", faults)
	}
}

func TestReEnrollingAfterSessionInvalidIsFaulted(t *testing.T) {
	h, err := startMockHub("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	p := newTestPlugin(t, h)
	h.killSession()

	// The broken plugin answers session_invalid by re-enrolling with the
	// burned token instead of re-sessioning.
	p.post("/plugin/v1/enroll", "", map[string]any{
		"enrollmentToken": h.enrollmentToken, "game": "conformance",
	}, http.StatusConflict, nil)

	faults := faultMessages(h)
	if !strings.Contains(faults, "re-session") && !strings.Contains(faults, "re-enrolling") {
		t.Fatalf("re-enrollment was not faulted; recorded faults:\n%s", faults)
	}
}

// A compile-time style guard: the harness's fault sections must all cite a
// real-looking spec clause, because the report promises actionable messages.
func TestFaultSectionsLookLikeSpecClauses(t *testing.T) {
	h, err := startMockHub("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	p := newTestPlugin(t, h)
	// Provoke a few different faults.
	p.poll(map[string]any{"v": 1, "type": "event.batch", "seq": 1, "ts": "x", "body": map[string]any{}})

	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.faults) == 0 {
		t.Fatal("an envelope with no id produced no fault")
	}
	for _, f := range h.faults {
		if f.Section == "" {
			t.Fatalf("fault %q cites no spec section", f.Message)
		}
		if !strings.ContainsAny(f.Section, "0123456789") {
			t.Fatalf("fault section %q does not look like a spec clause", f.Section)
		}
	}
}

func TestWellFormedTelemetryIsNotFaulted(t *testing.T) {
	h, err := startMockHub("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	p := newTestPlugin(t, h)
	p.poll(
		typedEnvelope("tel-1", 1, "event.batch", map[string]any{"events": []map[string]any{
			{"t": "core.player.connect", "ts": "2026-09-11T10:00:00+02:00", "data": map[string]any{"player": map[string]any{"platform": "steam", "id": "1"}}},
			{"t": "my-mod.raid.started"},
			{"t": "my-mod.raid.ended", "data": nil},
		}}),
		typedEnvelope("tel-2", 2, "state.players", map[string]any{"capturedAt": "2026-09-11T10:00:00Z", "players": []map[string]any{
			{"player": map[string]any{"platform": "steam", "id": "1"}, "name": "Survivor", "position": []float64{1, 2, 3}, "data": map[string]any{"health": 100}},
			{"player": map[string]any{"platform": "steam", "id": "2"}, "position": []float64{1, 2}},
		}}),
		typedEnvelope("tel-3", 3, "state.vehicles", map[string]any{"vehicles": []map[string]any{{"id": "v-1", "kind": "car"}}}),
		typedEnvelope("tel-4", 4, "state.entities", map[string]any{"entities": []any{}}),
		// Optional fields set to null read as absent (section 2.1): a hub
		// fills the timestamps with receipt time and never refuses over them.
		typedEnvelope("tel-5", 5, "event.batch", map[string]any{"events": []map[string]any{{"t": "core.server.fps", "ts": nil}}}),
		typedEnvelope("tel-6", 6, "state.players", map[string]any{"capturedAt": nil, "players": []map[string]any{
			{"player": map[string]any{"platform": "steam", "id": "1"}, "position": nil},
		}}),
	)

	if faults := faultMessages(h); faults != "" {
		t.Fatalf("well-formed telemetry was faulted:\n%s", faults)
	}
	h.mu.Lock()
	stats := h.telemetry
	h.mu.Unlock()
	if stats.batches != 2 || stats.events != 4 || stats.snapshots != 4 {
		t.Fatalf("telemetry counted as %+v, want 2 batches, 4 events, 4 snapshots", stats)
	}
}

func TestHeldTelemetryFaultsSurviveAFatalStage(t *testing.T) {
	h, err := startMockHub("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	p := newTestPlugin(t, h)
	p.poll(typedEnvelope("held-1", 1, "event.batch", map[string]any{}))

	failing := Stage{ID: "test.fatal", Title: "fails first", Section: "0", Fatal: true,
		Run: func(*harness) error { return errors.New("boom") }}
	results := runStages(&harness{hub: h, checkTimeout: time.Second}, []Stage{failing, telemetryStage})
	if len(results) != 2 || results[1].ID != telemetryStage.ID || results[1].Passed {
		t.Fatalf("unexpected results: %+v", results)
	}
	if !strings.Contains(results[1].Error, "prerequisite failed") || !strings.Contains(results[1].Error, "held-1 carries no events array") {
		t.Fatalf("the skipped telemetry stage did not report the held fault: %q", results[1].Error)
	}
}

// ---- context enumeration (spec section 6.2) ----

// enumerateResponder polls the mock hub in the background and answers every
// context.enumerate it is handed with whatever reply returns, so a test can
// run the enumerate stage against a plugin that behaves in one named way.
// A nil reply sends nothing, which is how a plugin that never answers is
// played.
type enumerateResponder struct {
	done     chan struct{}
	finished chan struct{}
	err      error
}

// startEnumerateResponder polls and answers every context.enumerate with
// what reply returns, nothing for nil.
func startEnumerateResponder(p *testPlugin, nextSeq int64, reply func(requestID, contextID string) map[string]any) *enumerateResponder {
	return startResponder(p, nextSeq, func(delivered deliverable) []map[string]any {
		if delivered.Type != "context.enumerate" {
			return nil
		}
		var asked struct {
			RequestID string `json:"requestId"`
			Context   string `json:"context"`
		}
		_ = json.Unmarshal(delivered.Body, &asked)
		answer := reply(asked.RequestID, asked.Context)
		if answer == nil {
			return nil
		}
		return []map[string]any{{"type": "context.entries", "body": answer}}
	})
}

// startDispatchResponder polls and answers every action.dispatch with the
// envelopes reply returns, each a map with type and body; nothing for nil.
func startDispatchResponder(p *testPlugin, nextSeq int64, reply func(actionID string, params json.RawMessage) []map[string]any) *enumerateResponder {
	return startResponder(p, nextSeq, func(delivered deliverable) []map[string]any {
		if delivered.Type != "action.dispatch" {
			return nil
		}
		var body struct {
			ActionID string          `json:"actionId"`
			Params   json.RawMessage `json:"params"`
		}
		_ = json.Unmarshal(delivered.Body, &body)
		return reply(body.ActionID, body.Params)
	})
}

// startResponder is a hand-driven plugin loop: it polls, acks every
// delivered envelope in order, keeps what it sends until the hub's ack
// covers it, and buffers whatever handle returns for a delivery, each
// entry a map with the envelope type and body.
func startResponder(p *testPlugin, nextSeq int64, handle func(delivered deliverable) []map[string]any) *enumerateResponder {
	r := &enumerateResponder{done: make(chan struct{}), finished: make(chan struct{})}
	go func() {
		defer close(r.finished)
		client := &http.Client{Timeout: 10 * time.Second}
		var ack int64
		var buffer []map[string]any
		for {
			select {
			case <-r.done:
				return
			default:
			}
			request := map[string]any{"ack": ack, "envelopes": buffer}
			encoded, err := json.Marshal(request)
			if err != nil {
				r.err = err
				return
			}
			httpRequest, err := http.NewRequest(http.MethodPost, p.baseURL+"/plugin/v1/poll", bytes.NewReader(encoded))
			if err != nil {
				r.err = err
				return
			}
			httpRequest.Header.Set("Content-Type", "application/json")
			httpRequest.Header.Set("Authorization", "Bearer "+p.sessionToken)
			response, err := client.Do(httpRequest)
			if err != nil {
				r.err = err
				return
			}
			var decoded struct {
				Envelopes []deliverable `json:"envelopes"`
				Ack       int64         `json:"ack"`
			}
			decodeErr := json.NewDecoder(response.Body).Decode(&decoded)
			response.Body.Close()
			if response.StatusCode != http.StatusOK {
				r.err = fmt.Errorf("poll: status %d", response.StatusCode)
				return
			}
			if decodeErr != nil {
				r.err = decodeErr
				return
			}
			// Everything the hub has taken leaves the buffer; the rest is
			// sent again, as a plugin's outbox does.
			kept := buffer[:0]
			for _, unacked := range buffer {
				if seq, ok := unacked["seq"].(int64); ok && seq > decoded.Ack {
					kept = append(kept, unacked)
				}
			}
			buffer = kept
			for _, delivered := range decoded.Envelopes {
				if delivered.Seq != ack+1 {
					continue
				}
				ack = delivered.Seq
				for _, reply := range handle(delivered) {
					envelopeType, _ := reply["type"].(string)
					body, _ := reply["body"].(map[string]any)
					buffer = append(buffer, typedEnvelope(fmt.Sprintf("reply-%d", nextSeq), nextSeq, envelopeType, body))
					nextSeq++
				}
			}
		}
	}()
	return r
}

func (r *enumerateResponder) stop(t *testing.T) {
	t.Helper()
	close(r.done)
	<-r.finished
	if r.err != nil {
		t.Fatalf("the responder failed: %v", r.err)
	}
}

// enumerateHub starts a hub whose manifest declares one custom context, the
// state the enumerate stage begins from, and returns the plugin that
// published it.
func enumerateHub(t *testing.T) (*mockHub, *testPlugin) {
	t.Helper()
	return enumerateHubDeclaring(t, []map[string]any{{"id": "test.zone", "name": "Zone", "namespace": "test"}})
}

// enumerateHubDeclaring is enumerateHub with the declared contexts chosen, for
// the tests that care which ids a manifest claims.
func enumerateHubDeclaring(t *testing.T, contexts []map[string]any) (*mockHub, *testPlugin) {
	t.Helper()
	h, err := startMockHub("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	p := newTestPlugin(t, h)
	p.poll(typedEnvelope("manifest-1", 1, "manifest.publish", map[string]any{
		"game":             "conformance",
		"manifestRevision": 1,
		"actions":          []map[string]any{{"code": "test.echo", "context": "world"}},
		"contexts":         contexts,
	}))
	return h, p
}

func runEnumerateStage(t *testing.T, h *mockHub, timeout time.Duration) Result {
	t.Helper()
	results := runStages(&harness{hub: h, checkTimeout: timeout}, []Stage{contextEnumerateStage})
	if len(results) != 1 {
		t.Fatalf("unexpected results: %+v", results)
	}
	return results[0]
}

// goodEntries is a reply a conformant plugin sends: the declared context
// enumerated, an undeclared one answered with an empty list and a reason.
func goodEntries(requestID, contextID string) map[string]any {
	if contextID != "test.zone" {
		return map[string]any{
			"requestId": requestID, "context": contextID,
			"entries": []map[string]any{},
			"reason":  "this plugin declares no context named " + contextID,
		}
	}
	return map[string]any{
		"requestId": requestID, "context": contextID,
		"entries": []map[string]any{
			{"referenceKey": "north-ridge", "label": "North Ridge", "position": []float64{4231.5, 300.25, 10620}},
			{"referenceKey": "south-hollow", "label": "South Hollow", "data": map[string]any{"guarded": true}},
		},
	}
}

func TestEnumerateStagePassesAgainstAConformantReply(t *testing.T) {
	h, p := enumerateHub(t)
	responder := startEnumerateResponder(p, 2, goodEntries)
	defer responder.stop(t)

	result := runEnumerateStage(t, h, 10*time.Second)
	if !result.Passed || result.Note != "" {
		t.Fatalf("a conformant enumeration did not pass: %+v; faults:\n%s", result, faultMessages(h))
	}
	h.mu.Lock()
	replies := h.contextReplies
	h.mu.Unlock()
	if replies != 2 {
		t.Fatalf("the stage collected %d context.entries replies, want 2 (the declared context and the undeclared one)", replies)
	}
}

func TestEnumerateStageFailsWhenNoReplyArrives(t *testing.T) {
	h, p := enumerateHub(t)
	// The plugin polls and acks the request, and answers nothing.
	responder := startEnumerateResponder(p, 2, func(string, string) map[string]any { return nil })
	defer responder.stop(t)

	result := runEnumerateStage(t, h, 500*time.Millisecond)
	if result.Passed {
		t.Fatal("a plugin that never answered a context.enumerate passed")
	}
	if !strings.Contains(result.Error, "a context.entries reply for context \"test.zone\"") ||
		!strings.Contains(result.Error, "section 6.2") {
		t.Fatalf("the missing reply was not named: %q", result.Error)
	}
}

func TestEnumerateStageFailsOnAnEntryOutsideItsBounds(t *testing.T) {
	h, p := enumerateHub(t)
	responder := startEnumerateResponder(p, 2, func(requestID, contextID string) map[string]any {
		if contextID != "test.zone" {
			return goodEntries(requestID, contextID)
		}
		return map[string]any{
			"requestId": requestID, "context": contextID,
			"entries": []map[string]any{
				{"referenceKey": "north-ridge", "label": "North Ridge"},
				{"referenceKey": "", "label": "Nameless"},
			},
		}
	})
	defer responder.stop(t)

	result := runEnumerateStage(t, h, 10*time.Second)
	if result.Passed {
		t.Fatal("an entry with an empty referenceKey passed")
	}
	if !strings.Contains(result.Error, "entries[1].referenceKey must be a non-empty string") {
		t.Fatalf("the bad entry was not named: %q", result.Error)
	}
}

func TestEnumerateStageFailsWhenAnUndeclaredContextCarriesNoReason(t *testing.T) {
	h, p := enumerateHub(t)
	responder := startEnumerateResponder(p, 2, func(requestID, contextID string) map[string]any {
		if contextID == "test.zone" {
			return goodEntries(requestID, contextID)
		}
		// Empty, but silent about why.
		return map[string]any{
			"requestId": requestID, "context": contextID,
			"entries": []map[string]any{},
		}
	})
	defer responder.stop(t)

	result := runEnumerateStage(t, h, 10*time.Second)
	if result.Passed {
		t.Fatal("an undeclared context answered without a reason passed")
	}
	if !strings.Contains(result.Error, absentContext) || !strings.Contains(result.Error, "no reason string") {
		t.Fatalf("the missing reason was not named: %q", result.Error)
	}
}

// Nothing stops a manifest from declaring the id the stage starts its probe
// from. A plugin that enumerates it properly is conformant, so the probe has
// to move rather than read a correct answer as a fault.
func TestEnumerateStagePassesWhenTheManifestDeclaresTheProbeId(t *testing.T) {
	h, p := enumerateHubDeclaring(t, []map[string]any{
		{"id": "test.zone", "name": "Zone", "namespace": "test"},
		{"id": absentContext, "name": "Absent in name only", "namespace": "conformance"},
	})
	responder := startEnumerateResponder(p, 2, func(requestID, contextID string) map[string]any {
		// Both declared contexts have members. Anything else is the probe,
		// and gets the empty answer with a reason.
		if contextID == "test.zone" || contextID == absentContext {
			return map[string]any{
				"requestId": requestID, "context": contextID,
				"entries": []map[string]any{{"referenceKey": "north-ridge", "label": "North Ridge"}},
			}
		}
		return map[string]any{
			"requestId": requestID, "context": contextID,
			"entries": []map[string]any{},
			"reason":  "this plugin declares no context named " + contextID,
		}
	})
	defer responder.stop(t)

	result := runEnumerateStage(t, h, 10*time.Second)
	if !result.Passed || result.Note != "" {
		t.Fatalf("a plugin that declares %q as a real context did not pass: %+v; faults: %s",
			absentContext, result, faultMessages(h))
	}
}

// A JSON null decodes into a Go string without complaint, so a label of null
// has to be refused on its own rather than through the length bound.
func TestEnumerateStageFailsOnANullLabel(t *testing.T) {
	h, p := enumerateHub(t)
	responder := startEnumerateResponder(p, 2, func(requestID, contextID string) map[string]any {
		if contextID != "test.zone" {
			return goodEntries(requestID, contextID)
		}
		return map[string]any{
			"requestId": requestID, "context": contextID,
			"entries": []map[string]any{{"referenceKey": "north-ridge", "label": nil}},
		}
	})
	defer responder.stop(t)

	result := runEnumerateStage(t, h, 10*time.Second)
	if result.Passed {
		t.Fatal("an entry whose label is JSON null passed")
	}
	if !strings.Contains(result.Error, "entries[0].label must be a display string") {
		t.Fatalf("the null label was not named: %q", result.Error)
	}
}

// A manifest declaring no custom context leaves the stage nothing to grade,
// which is a PART with a note rather than a pass or a failure.
func TestEnumerateStageIsUngradedWithoutADeclaredContext(t *testing.T) {
	h, err := startMockHub("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	p := newTestPlugin(t, h)
	p.poll(typedEnvelope("manifest-1", 1, "manifest.publish", map[string]any{
		"game":             "conformance",
		"manifestRevision": 1,
		"actions":          []map[string]any{{"code": "test.echo", "context": "world"}},
		"contexts":         []any{},
	}))

	result := runEnumerateStage(t, h, 500*time.Millisecond)
	if !result.Passed || result.Note == "" {
		t.Fatalf("want a pass with a note, got %+v", result)
	}
	if !strings.Contains(result.Note, "declares no custom context") {
		t.Fatalf("unexpected note: %q", result.Note)
	}
}

func TestMalformedEventBatchesAreFaulted(t *testing.T) {
	h, err := startMockHub("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	tooMany := make([]map[string]any, 201)
	for i := range tooMany {
		tooMany[i] = map[string]any{"t": "core.server.fps"}
	}
	p := newTestPlugin(t, h)
	p.poll(
		typedEnvelope("bad-1", 1, "event.batch", map[string]any{}),
		typedEnvelope("bad-2", 2, "event.batch", map[string]any{"events": []map[string]any{{"t": "noNamespace"}}}),
		typedEnvelope("bad-3", 3, "event.batch", map[string]any{"events": []map[string]any{{"t": "server.start"}}}),
		typedEnvelope("bad-4", 4, "event.batch", map[string]any{"events": []map[string]any{{"t": "core.player.chat", "data": "hello"}}}),
		typedEnvelope("bad-5", 5, "event.batch", map[string]any{"events": []map[string]any{{"t": "core.player.chat", "ts": "yesterday"}}}),
		typedEnvelope("bad-6", 6, "event.batch", map[string]any{"events": tooMany}),
		typedEnvelope("bad-7", 7, "event.batch", map[string]any{"events": []map[string]any{{"data": map[string]any{}}}}),
	)

	faults := faultMessages(h)
	for _, want := range []string{
		"bad-1 carries no events array",
		`bad-2 events[0].t "noNamespace" is outside`,
		`bad-3 events[0].t "server.start" begins with a namespace reserved`,
		"bad-4 events[0].data is not an object",
		"bad-5 events[0].ts",
		"bad-6 carries 201 events",
		"bad-7 events[0] carries no t",
	} {
		if !strings.Contains(faults, want) {
			t.Errorf("expected a fault containing %q; recorded faults:\n%s", want, faults)
		}
	}
	for _, f := range allFaults(h) {
		if f.Section != "8.1" {
			t.Errorf("fault %q cites section %s, want 8.1", f.Message, f.Section)
		}
	}
}

func TestMalformedSnapshotsAreFaulted(t *testing.T) {
	h, err := startMockHub("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	p := newTestPlugin(t, h)
	p.poll(
		typedEnvelope("snap-1", 1, "state.players", map[string]any{"capturedAt": "2026-09-11T10:00:00Z"}),
		typedEnvelope("snap-2", 2, "state.players", map[string]any{"players": []map[string]any{{"name": "Nobody"}}}),
		typedEnvelope("snap-3", 3, "state.players", map[string]any{"players": []map[string]any{{"player": map[string]any{"platform": "steam", "id": ""}}}}),
		typedEnvelope("snap-4", 4, "state.players", map[string]any{"players": []map[string]any{{"player": map[string]any{"platform": "steam", "id": "1"}, "position": []any{1}}}}),
		typedEnvelope("snap-5", 5, "state.vehicles", map[string]any{"vehicles": []map[string]any{{"kind": "car"}}}),
		typedEnvelope("snap-6", 6, "state.entities", map[string]any{"entities": nil}),
		typedEnvelope("snap-7", 7, "state.players", map[string]any{"capturedAt": 12345, "players": []any{}}),
		typedEnvelope("snap-8", 8, "state.players", map[string]any{"players": []map[string]any{{"player": map[string]any{"platform": "steam", "id": "1"}, "data": []any{}}}}),
		typedEnvelope("snap-9", 9, "state.players", map[string]any{"players": []map[string]any{{"player": map[string]any{"platform": "steam", "id": "1"}, "position": []any{nil, 2}}}}),
	)

	faults := faultMessages(h)
	for _, want := range []string{
		"snap-1 carries no players list",
		"snap-2 players[0] carries no player",
		"snap-3 players[0].player.id must be a non-empty string",
		"snap-4 players[0].position must be an array of two or three numbers",
		"snap-5 vehicles[0].id must be a non-empty string",
		"snap-6: entities is not an array",
		"snap-7: capturedAt 12345 is not an RFC 3339 timestamp",
		"snap-8 players[0].data is not an object",
		"snap-9 players[0].position[0] is not a number",
	} {
		if !strings.Contains(faults, want) {
			t.Errorf("expected a fault containing %q; recorded faults:\n%s", want, faults)
		}
	}
	for _, f := range allFaults(h) {
		if f.Section != "8.3" {
			t.Errorf("fault %q cites section %s, want 8.3", f.Message, f.Section)
		}
	}
}

// ---- dispatch.largeParams ----

// largeParamsHub is a hub past the manifest stage, with the one action the
// stage will dispatch.
func largeParamsHub(t *testing.T) (*mockHub, *testPlugin, *harness) {
	t.Helper()
	h, p := enumerateHubDeclaring(t, nil)
	stageHarness := &harness{hub: h, checkTimeout: 10 * time.Second, action: manifestAction{Code: "test.echo", Context: "world"}}
	return h, p, stageHarness
}

func runLargeParamsStage(t *testing.T, stageHarness *harness) Result {
	t.Helper()
	results := runStages(stageHarness, []Stage{largeParamsStage})
	if len(results) != 1 {
		t.Fatalf("unexpected results: %+v", results)
	}
	return results[0]
}

func TestLargeParamsStagePassesWhenTheDispatchIsAckedAndAnswered(t *testing.T) {
	h, p, stageHarness := largeParamsHub(t)
	var seen int
	responder := startDispatchResponder(p, 2, func(actionID string, params json.RawMessage) []map[string]any {
		seen = len(params)
		return []map[string]any{
			{"type": "action.ack", "body": map[string]any{"actionId": actionID}},
			{"type": "action.result", "body": map[string]any{"actionId": actionID, "ok": false, "error": "the test declines"}},
		}
	})
	defer responder.stop(t)

	result := runLargeParamsStage(t, stageHarness)
	if !result.Passed {
		t.Fatalf("an acked and answered large dispatch did not pass: %+v; faults:\n%s", result, faultMessages(h))
	}
	if seen < largeParamsBytes {
		t.Fatalf("the dispatch carried %d bytes of params, want at least %d", seen, largeParamsBytes)
	}
}

func TestLargeParamsStageKeepsADeclaredPaddingProperty(t *testing.T) {
	h, p, stageHarness := largeParamsHub(t)
	// An action that happens to declare the padding's name as a required
	// integer: the padding has to go under another name, and the declared
	// property has to keep its synthesized value.
	stageHarness.action.Params = map[string]any{
		"type":     "object",
		"required": []any{largeParamsKey},
		"properties": map[string]any{
			largeParamsKey: map[string]any{"type": "integer", "minimum": 7, "maximum": 7},
		},
	}
	var got map[string]any
	responder := startDispatchResponder(p, 2, func(actionID string, params json.RawMessage) []map[string]any {
		_ = json.Unmarshal(params, &got)
		return []map[string]any{
			{"type": "action.ack", "body": map[string]any{"actionId": actionID}},
			{"type": "action.result", "body": map[string]any{"actionId": actionID, "ok": true}},
		}
	})
	defer responder.stop(t)

	result := runLargeParamsStage(t, stageHarness)
	if !result.Passed {
		t.Fatalf("the stage did not pass: %+v; faults:\n%s", result, faultMessages(h))
	}
	if _, ok := got[largeParamsKey].(float64); !ok {
		t.Fatalf("the declared property was displaced: %v", got[largeParamsKey])
	}
	padding, ok := got[largeParamsKey+"2"].(string)
	if !ok || len(padding) < largeParamsBytes {
		t.Fatalf("the padding did not go under the next free name: %d bytes under %q", len(padding), largeParamsKey+"2")
	}
}

func TestLargeParamsStageIsUngradedWhenTheSchemaAdmitsNoMember(t *testing.T) {
	for _, schema := range []map[string]any{
		{"type": "object", "enum": []any{map[string]any{largeParamsKey: 7}}},
		{"type": "object", "properties": map[string]any{"amount": map[string]any{"type": "integer"}}, "additionalProperties": false},
	} {
		h, _, stageHarness := largeParamsHub(t)
		stageHarness.action.Params = schema
		stageHarness.checkTimeout = 500 * time.Millisecond
		result := runLargeParamsStage(t, stageHarness)
		if !result.Passed || result.Note == "" || !strings.Contains(result.Note, "no member can be added") {
			t.Fatalf("a schema that admits no member was not reported ungraded: %+v; faults:\n%s", result, faultMessages(h))
		}
		if len(h.outbound) != 0 {
			t.Fatalf("a dispatch was queued although nothing could be graded")
		}
		h.Close()
	}
}

func TestLargeParamsStageFailsWhenTheDispatchIsAckedButNeverAnswered(t *testing.T) {
	_, p, stageHarness := largeParamsHub(t)
	stageHarness.checkTimeout = 500 * time.Millisecond
	// The poll ack covers the envelope; nothing else is ever sent, which is
	// what a plugin that parsed the body past expiresAt does.
	responder := startDispatchResponder(p, 2, func(string, json.RawMessage) []map[string]any { return nil })
	defer responder.stop(t)

	result := runLargeParamsStage(t, stageHarness)
	if result.Passed {
		t.Fatal("a large dispatch that was never answered passed")
	}
	if !strings.Contains(result.Error, "an action.result for the large dispatch") || !strings.Contains(result.Error, "expiresAt") {
		t.Fatalf("the missing result was not named: %q", result.Error)
	}
}

func TestLargeParamsStageFailsWhenTheDispatchIsNeverAcked(t *testing.T) {
	_, _, stageHarness := largeParamsHub(t)
	stageHarness.checkTimeout = 500 * time.Millisecond
	// No responder at all: the plugin never polls the dispatch away.
	result := runLargeParamsStage(t, stageHarness)
	if result.Passed {
		t.Fatal("a large dispatch nobody acked passed")
	}
	if !strings.Contains(result.Error, "the plugin to ack the large dispatch") || !strings.Contains(result.Error, "issue #108") {
		t.Fatalf("the missing ack was not named: %q", result.Error)
	}
}
