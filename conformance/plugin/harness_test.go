package main

import (
	"bytes"
	"encoding/json"
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

func testEnvelope(id string, seq int64, body map[string]any) map[string]any {
	if body == nil {
		body = map[string]any{}
	}
	return map[string]any{
		"v": 1, "id": id, "type": "event.batch", "seq": seq,
		"ts": "2026-08-20T12:00:00Z", "body": body,
	}
}

func faultMessages(h *mockHub) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	messages := make([]string, 0, len(h.faults))
	for _, f := range h.faults {
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
