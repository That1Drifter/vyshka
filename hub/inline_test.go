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

// Inline errors, spec section 2.3: a Plugin API request carrying ?errors=inline
// gets its refusals as a 200 with error.status inside.

type inlineFailure struct {
	Error struct {
		Code    string         `json:"code"`
		Status  int            `json:"status"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

// rawCall is call without the decoding, for tests that inspect the response
// as sent: status, headers, and the exact bytes.
func rawCall(t *testing.T, server *hub.Server, method, path, bearer string, body any) *httptest.ResponseRecorder {
	t.Helper()

	var payload string
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode request: %v", err)
		}
		payload = string(encoded)
	}
	request := httptest.NewRequest(method, path, strings.NewReader(payload))
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	return recorder
}

// inlineError asserts an inline refusal: 200, JSON, an error with the expected
// code and the status the hub would otherwise have sent.
func inlineError(t *testing.T, recorder *httptest.ResponseRecorder, wantCode string, wantStatus int) inlineFailure {
	t.Helper()

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for an inline error (body %s)", recorder.Code, recorder.Body.String())
	}
	if contentType := recorder.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", contentType)
	}
	var failure inlineFailure
	if err := json.Unmarshal(recorder.Body.Bytes(), &failure); err != nil {
		t.Fatalf("inline error body %q is not JSON: %v", recorder.Body.String(), err)
	}
	if failure.Error.Code != wantCode {
		t.Errorf("error.code = %q, want %q (body %s)", failure.Error.Code, wantCode, recorder.Body.String())
	}
	if failure.Error.Status != wantStatus {
		t.Errorf("error.status = %d, want %d", failure.Error.Status, wantStatus)
	}
	if strings.TrimSpace(failure.Error.Message) == "" {
		t.Error("inline error carries no message")
	}
	return failure
}

func TestInlineErrorsCoverEnrollmentSessionAndPoll(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)

	// Enrollment: the request no credential can cover.
	recorder := rawCall(t, server, http.MethodPost, "/plugin/v1/enroll?errors=inline", "",
		map[string]any{"enrollmentToken": "vye_nope", "game": "dayz"})
	inlineError(t, recorder, "enrollment_token_invalid", http.StatusUnauthorized)

	// A session request that fails.
	recorder = rawCall(t, server, http.MethodPost, "/plugin/v1/session?errors=inline", "",
		map[string]any{"serverId": "01NOPE", "serverSecret": "vys_nope"})
	inlineError(t, recorder, "credentials_invalid", http.StatusUnauthorized)

	// A session token the hub does not know. The hang-up headers of an
	// ordinary refusal still go out: only the status line changes.
	recorder = rawCall(t, server, http.MethodPost, "/plugin/v1/poll?errors=inline", "vyt_unknown",
		map[string]any{})
	inlineError(t, recorder, "session_invalid", http.StatusUnauthorized)
	if recorder.Header().Get("Connection") != "close" {
		t.Errorf("inline session refusal lost the Connection: close header")
	}
	if !strings.HasPrefix(recorder.Header().Get("WWW-Authenticate"), "Bearer") {
		t.Errorf("inline session refusal lost WWW-Authenticate")
	}

	// A refusal with details: the offending envelope is still named.
	_, live := enrolledSession(t, server, "inline details")
	recorder = rawCall(t, server, http.MethodPost, "/plugin/v1/poll?errors=inline", live.SessionToken,
		map[string]any{"envelopes": []map[string]any{
			{"v": 1, "id": "01OK", "type": "event.batch", "seq": 1, "ts": "2026-09-10T00:00:00Z", "body": map[string]any{}},
			{"v": 1, "type": "event.batch", "seq": 2, "ts": "2026-09-10T00:00:00Z", "body": map[string]any{}},
		}})
	failure := inlineError(t, recorder, "envelope_invalid", http.StatusBadRequest)
	if index, ok := failure.Error.Details["index"].(float64); !ok || index != 1 {
		t.Errorf("details.index = %v, want 1", failure.Error.Details["index"])
	}

	// A refusal from the KV realm: no manifest declares a namespace, so the
	// read is forbidden.
	recorder = rawCall(t, server, http.MethodGet, "/plugin/v1/kv/some-mod/key?errors=inline", live.SessionToken, nil)
	inlineError(t, recorder, "forbidden", http.StatusForbidden)

	// The generic refusals of section 2.2 too: a body that is not JSON.
	request := httptest.NewRequest(http.MethodPost, "/plugin/v1/session?errors=inline", strings.NewReader("<xml/>"))
	request.Header.Set("Content-Type", "text/xml")
	recorder = httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	inlineError(t, recorder, "unsupported_media_type", http.StatusUnsupportedMediaType)

	// And a path under /plugin/ that does not exist.
	recorder = rawCall(t, server, http.MethodGet, "/plugin/v1/nothing?errors=inline", "", nil)
	inlineError(t, recorder, "not_found", http.StatusNotFound)
	recorder = rawCall(t, server, http.MethodGet, "/plugin/v1/poll?errors=inline", "", nil)
	inlineError(t, recorder, "method_not_allowed", http.StatusMethodNotAllowed)
}

func TestInlineErrorsLeaveSuccessesAlone(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created := createServer(t, server, "inline success", "test-game")

	recorder := rawCall(t, server, http.MethodPost, "/plugin/v1/enroll?errors=inline", "",
		map[string]any{"enrollmentToken": created.Enrollment.Token, "game": "test-game"})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("enroll with inline errors: status = %d, want 201 (body %s)", recorder.Code, recorder.Body.String())
	}
	var credentials enrolled
	if err := json.Unmarshal(recorder.Body.Bytes(), &credentials); err != nil || credentials.ServerSecret == "" {
		t.Fatalf("enroll body %q did not carry credentials (%v)", recorder.Body.String(), err)
	}
	assertNoErrorMember(t, recorder.Body.Bytes())

	recorder = rawCall(t, server, http.MethodPost, "/plugin/v1/session?errors=inline", "",
		map[string]any{"serverId": credentials.ServerID, "serverSecret": credentials.ServerSecret, "pollTimeoutSeconds": 5})
	if recorder.Code != http.StatusOK {
		t.Fatalf("session with inline errors: status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	var live session
	if err := json.Unmarshal(recorder.Body.Bytes(), &live); err != nil || live.SessionToken == "" {
		t.Fatalf("session body %q did not carry a token (%v)", recorder.Body.String(), err)
	}
	assertNoErrorMember(t, recorder.Body.Bytes())
	if flag, _ := live.Features["inlineErrors"].(bool); !flag {
		t.Errorf("features = %v, want inlineErrors: true", live.Features)
	}

	queueEnvelope(t, server, created.Server.ID, "test.nudge", nil)
	recorder = rawCall(t, server, http.MethodPost, "/plugin/v1/poll?errors=inline", live.SessionToken, map[string]any{})
	if recorder.Code != http.StatusOK {
		t.Fatalf("poll with inline errors: status = %d", recorder.Code)
	}
	var result pollResult
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil || len(result.Envelopes) != 1 {
		t.Fatalf("poll body %q did not carry the queued envelope (%v)", recorder.Body.String(), err)
	}
	assertNoErrorMember(t, recorder.Body.Bytes())
}

func TestOrdinaryErrorsDoNotCarryAStatusMember(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)

	recorder := rawCall(t, server, http.MethodPost, "/plugin/v1/poll", "vyt_unknown", map[string]any{})
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 without the opt-in", recorder.Code)
	}
	var raw map[string]map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, present := raw["error"]["status"]; present {
		t.Errorf("an ordinary error carried error.status: %s", recorder.Body.String())
	}
}

func TestInlineErrorsRefuseUnknownModesAndIgnoreTheAdminAPI(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)

	// A malformed query string is refused too, whether or not it also carries
	// a well-formed opt-in: r.URL.Query() would have dropped the bad pair and
	// let the rest through.
	for _, query := range []string{"errors=loud", "errors=", "errors=inline&errors=inline", "errors=inline&errors=loud", "errors=%ZZ", "errors=inline&errors=%ZZ"} {
		recorder := rawCall(t, server, http.MethodPost, "/plugin/v1/poll?"+query, "vyt_unknown", map[string]any{})
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("?%s: status = %d, want an ordinary 400", query, recorder.Code)
			continue
		}
		var failure inlineFailure
		if err := json.Unmarshal(recorder.Body.Bytes(), &failure); err != nil || failure.Error.Code != "bad_request" {
			t.Errorf("?%s: body %q, want bad_request", query, recorder.Body.String())
		}
	}

	// The Admin API always answers with ordinary statuses.
	recorder := rawCall(t, server, http.MethodGet, "/api/v1/servers?errors=inline", "vya_unknown", nil)
	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("admin refusal with ?errors=inline: status = %d, want 401", recorder.Code)
	}
	recorder = rawCall(t, server, http.MethodGet, "/api/v1/servers?errors=inline", testAdminToken, nil)
	if recorder.Code != http.StatusOK {
		t.Errorf("admin success with ?errors=inline: status = %d, want 200", recorder.Code)
	}
}

func TestInlineErrorsAnswerASupersededHeldPoll(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created := createServer(t, server, "inline supersede", "test-game")
	credentials := enroll(t, server, created.Enrollment.Token, "test-game")
	stale := startSession(t, server, credentials, 25)

	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		answered <- rawCall(t, server, http.MethodPost, "/plugin/v1/poll?errors=inline", stale.SessionToken, map[string]any{})
	}()
	time.Sleep(300 * time.Millisecond)
	started := time.Now()
	startSession(t, server, credentials, 5)

	select {
	case recorder := <-answered:
		inlineError(t, recorder, "session_invalid", http.StatusUnauthorized)
		if elapsed := time.Since(started); elapsed > 5*time.Second {
			t.Errorf("the superseded poll took %s to answer; it must not run to term", elapsed)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the superseded held poll was never answered")
	}
}

func assertNoErrorMember(t *testing.T, body []byte) {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decode success body: %v", err)
	}
	if _, present := raw["error"]; present {
		t.Errorf("a success body carried a top-level error member: %s", body)
	}
}
