package hub_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/That1Drifter/vyshka/hub"
)

// call sends a JSON request through the hub's handler and decodes the response
// into out when out is non-nil. It returns the status code so tests read as
// "status, then body".
func call(t *testing.T, server *hub.Server, method, path, bearer string, body, out any) int {
	t.Helper()

	var reader *bytes.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode request: %v", err)
		}
		reader = bytes.NewReader(encoded)
	} else {
		reader = bytes.NewReader(nil)
	}

	request := httptest.NewRequest(method, path, reader)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	if out != nil && recorder.Body.Len() > 0 {
		if err := json.Unmarshal(recorder.Body.Bytes(), out); err != nil {
			t.Fatalf("decode %s %s response %q: %v", method, path, recorder.Body.String(), err)
		}
	}
	return recorder.Code
}

// errorCode returns the protocol error code of a failed response.
func errorCode(t *testing.T, server *hub.Server, method, path, bearer string, body any, wantStatus int) string {
	t.Helper()

	var failure struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	status := call(t, server, method, path, bearer, body, &failure)
	if status != wantStatus {
		t.Fatalf("%s %s: status = %d, want %d", method, path, status, wantStatus)
	}
	if failure.Error.Code == "" {
		t.Fatalf("%s %s: response carried no error code", method, path)
	}
	if strings.TrimSpace(failure.Error.Message) == "" {
		t.Errorf("%s %s: error code %q carried no message", method, path, failure.Error.Code)
	}
	return failure.Error.Code
}

type createdServer struct {
	Server struct {
		ID              string `json:"id"`
		Name            string `json:"name"`
		Game            string `json:"game"`
		CredentialState string `json:"credentialState"`
	} `json:"server"`
	Enrollment struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expiresAt"`
	} `json:"enrollment"`
}

type enrolled struct {
	ServerID     string `json:"serverId"`
	ServerSecret string `json:"serverSecret"`
	Server       struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Game string `json:"game"`
	} `json:"server"`
}

type session struct {
	SessionID          string         `json:"sessionId"`
	SessionToken       string         `json:"sessionToken"`
	ExpiresAt          string         `json:"expiresAt"`
	ProtocolVersion    int            `json:"protocolVersion"`
	EnvelopeVersion    int            `json:"envelopeVersion"`
	PollTimeoutSeconds int            `json:"pollTimeoutSeconds"`
	Transports         []string       `json:"transports"`
	Features           map[string]any `json:"features"`
	Server             struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Game string `json:"game"`
	} `json:"server"`
}

// createServer walks the operator half of the flow.
func createServer(t *testing.T, server *hub.Server, name, game string) createdServer {
	t.Helper()

	var created createdServer
	status := call(t, server, http.MethodPost, "/api/v1/servers", testAdminToken,
		map[string]any{"name": name, "game": game}, &created)
	if status != http.StatusCreated {
		t.Fatalf("create server: status = %d, want 201", status)
	}
	return created
}

// enroll walks the plugin half of the flow.
func enroll(t *testing.T, server *hub.Server, enrollmentToken, game string) enrolled {
	t.Helper()

	var result enrolled
	status := call(t, server, http.MethodPost, "/plugin/v1/enroll", "", map[string]any{
		"enrollmentToken": enrollmentToken,
		"game":            game,
		"plugin":          map[string]string{"name": "test-plugin", "version": "0.1.0"},
		"transports":      []string{"poll"},
	}, &result)
	if status != http.StatusCreated {
		t.Fatalf("enroll: status = %d, want 201", status)
	}
	return result
}

func startSession(t *testing.T, server *hub.Server, credentials enrolled, pollTimeoutSeconds int) session {
	t.Helper()

	request := map[string]any{
		"serverId":     credentials.ServerID,
		"serverSecret": credentials.ServerSecret,
		"plugin":       map[string]string{"name": "test-plugin", "version": "0.1.0"},
		"transports":   []string{"poll"},
	}
	if pollTimeoutSeconds != 0 {
		request["pollTimeoutSeconds"] = pollTimeoutSeconds
	}

	var result session
	status := call(t, server, http.MethodPost, "/plugin/v1/session", "", request, &result)
	if status != http.StatusOK {
		t.Fatalf("start session: status = %d, want 200", status)
	}
	return result
}

func TestEnrollmentHappyPath(t *testing.T) {
	server := newTestServer(t)

	created := createServer(t, server, "Chernarus #1", "dayz")
	if created.Server.ID == "" || created.Enrollment.Token == "" {
		t.Fatalf("create server returned %+v", created)
	}
	if created.Server.CredentialState != "none" {
		t.Errorf("credentialState = %q, want none before enrollment", created.Server.CredentialState)
	}

	credentials := enroll(t, server, created.Enrollment.Token, "dayz")
	if credentials.ServerID != created.Server.ID {
		t.Errorf("serverId = %q, want %q", credentials.ServerID, created.Server.ID)
	}
	if credentials.ServerSecret == "" {
		t.Fatal("enroll returned no secret")
	}
	if credentials.Server.Name != "Chernarus #1" {
		t.Errorf("server name = %q", credentials.Server.Name)
	}

	live := startSession(t, server, credentials, 0)
	switch {
	case live.SessionToken == "":
		t.Error("session response carried no token")
	case live.SessionID == "":
		t.Error("session response carried no id")
	case live.ProtocolVersion != hub.ProtocolVersion:
		t.Errorf("protocolVersion = %d, want %d", live.ProtocolVersion, hub.ProtocolVersion)
	case live.EnvelopeVersion != hub.EnvelopeVersion:
		t.Errorf("envelopeVersion = %d, want %d", live.EnvelopeVersion, hub.EnvelopeVersion)
	case live.PollTimeoutSeconds != int(hub.DefaultPollTimeout.Seconds()):
		t.Errorf("pollTimeoutSeconds = %d, want the %v default",
			live.PollTimeoutSeconds, hub.DefaultPollTimeout)
	case len(live.Transports) == 0:
		t.Error("session response advertised no transports")
	case live.Features == nil:
		t.Error("features must be an object, even when empty")
	}

	// The session token authenticates a Plugin API call.
	var introspected session
	if status := call(t, server, http.MethodGet, "/plugin/v1/session", live.SessionToken, nil, &introspected); status != http.StatusOK {
		t.Fatalf("get session: status = %d, want 200", status)
	}
	if introspected.SessionID != live.SessionID {
		t.Errorf("sessionId = %q, want %q", introspected.SessionID, live.SessionID)
	}
	if introspected.SessionToken != "" {
		t.Error("introspection must not echo the session token back")
	}

	// The operator sees the enrolled server, its plugin, and its live session.
	var record struct {
		CredentialState string `json:"credentialState"`
		LastSeenAt      string `json:"lastSeenAt"`
		Plugin          *struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"plugin"`
		Session *struct {
			ID string `json:"id"`
		} `json:"session"`
	}
	if status := call(t, server, http.MethodGet, "/api/v1/servers/"+created.Server.ID,
		testAdminToken, nil, &record); status != http.StatusOK {
		t.Fatalf("get server: status = %d, want 200", status)
	}
	if record.CredentialState != "active" {
		t.Errorf("credentialState = %q, want active", record.CredentialState)
	}
	if record.LastSeenAt == "" {
		t.Error("lastSeenAt was not updated by the session")
	}
	if record.Plugin == nil || record.Plugin.Name != "test-plugin" {
		t.Errorf("plugin = %+v, want the enrolled plugin", record.Plugin)
	}
	if record.Session == nil || record.Session.ID != live.SessionID {
		t.Errorf("session = %+v, want the live session", record.Session)
	}
}

func TestEnrollmentTokenIsSingleUse(t *testing.T) {
	server := newTestServer(t)
	created := createServer(t, server, "Livonia", "dayz")

	enroll(t, server, created.Enrollment.Token, "dayz")

	code := errorCode(t, server, http.MethodPost, "/plugin/v1/enroll", "", map[string]any{
		"enrollmentToken": created.Enrollment.Token,
		"game":            "dayz",
	}, http.StatusConflict)
	if code != "enrollment_token_used" {
		t.Errorf("code = %q, want enrollment_token_used", code)
	}
}

func TestEnrollRejectsUnknownTokenAndGameMismatch(t *testing.T) {
	server := newTestServer(t)
	created := createServer(t, server, "Chernarus", "dayz")

	code := errorCode(t, server, http.MethodPost, "/plugin/v1/enroll", "", map[string]any{
		"enrollmentToken": "vye_NOSUCHTOKENNOSUCHTOKENXX",
		"game":            "dayz",
	}, http.StatusUnauthorized)
	if code != "enrollment_token_invalid" {
		t.Errorf("code = %q, want enrollment_token_invalid", code)
	}

	code = errorCode(t, server, http.MethodPost, "/plugin/v1/enroll", "", map[string]any{
		"enrollmentToken": created.Enrollment.Token,
		"game":            "reforger",
	}, http.StatusConflict)
	if code != "game_mismatch" {
		t.Errorf("code = %q, want game_mismatch", code)
	}

	// A mismatch must not burn the token: the right plugin can still enroll.
	enroll(t, server, created.Enrollment.Token, "dayz")
}

func TestEnrollAcceptsAnyGameWhenTheRecordDeclaresNone(t *testing.T) {
	server := newTestServer(t)
	created := createServer(t, server, "Unclaimed", "")

	credentials := enroll(t, server, created.Enrollment.Token, "Reforger")
	if credentials.Server.Game != "reforger" {
		t.Errorf("game = %q, want the normalized reforger", credentials.Server.Game)
	}
}

func TestSessionRejectsBadCredentials(t *testing.T) {
	server := newTestServer(t)
	created := createServer(t, server, "Chernarus", "dayz")
	credentials := enroll(t, server, created.Enrollment.Token, "dayz")

	for _, testCase := range []struct {
		name     string
		serverID string
		secret   string
	}{
		{"wrong secret", credentials.ServerID, "vys_WRONGWRONGWRONGWRONGWRON"},
		{"unknown server", "01JNOSUCHSERVERNOSUCHSER00", credentials.ServerSecret},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			code := errorCode(t, server, http.MethodPost, "/plugin/v1/session", "", map[string]any{
				"serverId":     testCase.serverID,
				"serverSecret": testCase.secret,
			}, http.StatusUnauthorized)
			if code != "credentials_invalid" {
				t.Errorf("code = %q, want credentials_invalid", code)
			}
		})
	}
}

func TestSessionNegotiatesPollTimeout(t *testing.T) {
	server := newTestServer(t)
	created := createServer(t, server, "Chernarus", "dayz")
	credentials := enroll(t, server, created.Enrollment.Token, "dayz")

	for _, testCase := range []struct{ requested, want int }{
		{requested: 10, want: 10},
		{requested: 1, want: int(hub.MinPollTimeout.Seconds())},
		{requested: 600, want: int(hub.MaxPollTimeout.Seconds())},
	} {
		live := startSession(t, server, credentials, testCase.requested)
		if live.PollTimeoutSeconds != testCase.want {
			t.Errorf("requested %ds: pollTimeoutSeconds = %d, want %d",
				testCase.requested, live.PollTimeoutSeconds, testCase.want)
		}
	}
}

func TestSessionRejectsUnsupportedProtocolVersion(t *testing.T) {
	server := newTestServer(t)
	created := createServer(t, server, "Chernarus", "dayz")
	credentials := enroll(t, server, created.Enrollment.Token, "dayz")

	code := errorCode(t, server, http.MethodPost, "/plugin/v1/session", "", map[string]any{
		"serverId":        credentials.ServerID,
		"serverSecret":    credentials.ServerSecret,
		"protocolVersion": 99,
	}, http.StatusBadRequest)
	if code != "protocol_version_unsupported" {
		t.Errorf("code = %q, want protocol_version_unsupported", code)
	}
}

func TestNewSessionSupersedesTheOldOne(t *testing.T) {
	server := newTestServer(t)
	created := createServer(t, server, "Chernarus", "dayz")
	credentials := enroll(t, server, created.Enrollment.Token, "dayz")

	first := startSession(t, server, credentials, 0)
	second := startSession(t, server, credentials, 0)
	if first.SessionID == second.SessionID {
		t.Fatal("the second session reused the first session's id")
	}

	code := errorCode(t, server, http.MethodGet, "/plugin/v1/session", first.SessionToken,
		nil, http.StatusUnauthorized)
	if code != "session_invalid" {
		t.Errorf("code = %q, want session_invalid", code)
	}
	if status := call(t, server, http.MethodGet, "/plugin/v1/session", second.SessionToken, nil, nil); status != http.StatusOK {
		t.Errorf("newest session: status = %d, want 200", status)
	}
}

func TestRevocationKillsSessionAndCredentials(t *testing.T) {
	server := newTestServer(t)
	created := createServer(t, server, "Chernarus", "dayz")
	credentials := enroll(t, server, created.Enrollment.Token, "dayz")
	live := startSession(t, server, credentials, 0)

	status := call(t, server, http.MethodDelete,
		"/api/v1/servers/"+created.Server.ID+"/credentials", testAdminToken, nil, nil)
	if status != http.StatusNoContent {
		t.Fatalf("revoke: status = %d, want 204", status)
	}

	code := errorCode(t, server, http.MethodGet, "/plugin/v1/session", live.SessionToken,
		nil, http.StatusUnauthorized)
	if code != "session_invalid" {
		t.Errorf("session after revoke: code = %q, want session_invalid", code)
	}

	code = errorCode(t, server, http.MethodPost, "/plugin/v1/session", "", map[string]any{
		"serverId":     credentials.ServerID,
		"serverSecret": credentials.ServerSecret,
	}, http.StatusUnauthorized)
	if code != "credentials_revoked" {
		t.Errorf("re-session after revoke: code = %q, want credentials_revoked", code)
	}

	var record struct {
		CredentialState string `json:"credentialState"`
		Session         *struct {
			ID string `json:"id"`
		} `json:"session"`
	}
	call(t, server, http.MethodGet, "/api/v1/servers/"+created.Server.ID, testAdminToken, nil, &record)
	if record.CredentialState != "revoked" {
		t.Errorf("credentialState = %q, want revoked", record.CredentialState)
	}
	if record.Session != nil {
		t.Errorf("session = %+v, want none after revocation", record.Session)
	}
}

func TestReissuedEnrollmentTokenRecoversARevokedServer(t *testing.T) {
	server := newTestServer(t)
	created := createServer(t, server, "Chernarus", "dayz")
	first := enroll(t, server, created.Enrollment.Token, "dayz")
	startSession(t, server, first, 0)

	if status := call(t, server, http.MethodDelete,
		"/api/v1/servers/"+created.Server.ID+"/credentials", testAdminToken, nil, nil); status != http.StatusNoContent {
		t.Fatalf("revoke: status = %d, want 204", status)
	}

	var reissued struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expiresAt"`
	}
	if status := call(t, server, http.MethodPost,
		"/api/v1/servers/"+created.Server.ID+"/enrollment-token", testAdminToken, nil, &reissued); status != http.StatusCreated {
		t.Fatalf("reissue: status = %d, want 201", status)
	}
	if reissued.Token == "" || reissued.Token == created.Enrollment.Token {
		t.Fatal("reissue did not return a fresh token")
	}

	second := enroll(t, server, reissued.Token, "dayz")
	if second.ServerSecret == first.ServerSecret {
		t.Fatal("re-enrollment reused the revoked secret")
	}
	startSession(t, server, second, 0)

	// The replaced secret is dead even though the record is active again.
	code := errorCode(t, server, http.MethodPost, "/plugin/v1/session", "", map[string]any{
		"serverId":     first.ServerID,
		"serverSecret": first.ServerSecret,
	}, http.StatusUnauthorized)
	if code != "credentials_invalid" {
		t.Errorf("old secret: code = %q, want credentials_invalid", code)
	}
}

func TestReissuingInvalidatesTheUnusedToken(t *testing.T) {
	server := newTestServer(t)
	created := createServer(t, server, "Chernarus", "dayz")

	var reissued struct {
		Token string `json:"token"`
	}
	call(t, server, http.MethodPost, "/api/v1/servers/"+created.Server.ID+"/enrollment-token",
		testAdminToken, nil, &reissued)

	code := errorCode(t, server, http.MethodPost, "/plugin/v1/enroll", "", map[string]any{
		"enrollmentToken": created.Enrollment.Token,
		"game":            "dayz",
	}, http.StatusUnauthorized)
	if code != "enrollment_token_invalid" {
		t.Errorf("superseded token: code = %q, want enrollment_token_invalid", code)
	}
	enroll(t, server, reissued.Token, "dayz")
}

func TestAdminRealmIsSeparateFromPluginRealm(t *testing.T) {
	server := newTestServer(t)
	created := createServer(t, server, "Chernarus", "dayz")
	credentials := enroll(t, server, created.Enrollment.Token, "dayz")
	live := startSession(t, server, credentials, 0)

	// No credential, wrong credential, and a Plugin API credential all fail.
	for _, bearer := range []string{"", "vya_WRONGWRONGWRONGWRONGWRON", live.SessionToken, credentials.ServerSecret} {
		if code := errorCode(t, server, http.MethodGet, "/api/v1/servers", bearer, nil,
			http.StatusUnauthorized); code != "unauthorized" {
			t.Errorf("bearer %q: code = %q, want unauthorized", bearer, code)
		}
	}

	// And the admin token buys nothing in the Plugin API.
	if code := errorCode(t, server, http.MethodGet, "/plugin/v1/session", testAdminToken, nil,
		http.StatusUnauthorized); code != "session_invalid" {
		t.Errorf("admin token on plugin realm: code = %q, want session_invalid", code)
	}
}

func TestCreateServerValidatesInput(t *testing.T) {
	server := newTestServer(t)

	if code := errorCode(t, server, http.MethodPost, "/api/v1/servers", testAdminToken,
		map[string]any{"game": "dayz"}, http.StatusBadRequest); code != "bad_request" {
		t.Errorf("missing name: code = %q, want bad_request", code)
	}
	if code := errorCode(t, server, http.MethodPost, "/api/v1/servers", testAdminToken,
		map[string]any{"name": "   "}, http.StatusBadRequest); code != "bad_request" {
		t.Errorf("blank name: code = %q, want bad_request", code)
	}
}

func TestUnknownFieldsAreTolerated(t *testing.T) {
	server := newTestServer(t)

	// Forward compatibility: a newer client sends a field this hub has never
	// heard of, and the request still works (spec section 2.1).
	var created createdServer
	status := call(t, server, http.MethodPost, "/api/v1/servers", testAdminToken, map[string]any{
		"name":            "Chernarus",
		"game":            "dayz",
		"somethingNewer":  map[string]any{"nested": true},
		"anotherNewField": 42,
	}, &created)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201", status)
	}
	enroll(t, server, created.Enrollment.Token, "dayz")
}

func TestNonJSONBodyIsRejected(t *testing.T) {
	server := newTestServer(t)

	request := httptest.NewRequest(http.MethodPost, "/plugin/v1/enroll", strings.NewReader("name=x"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want 415", recorder.Code)
	}
}

func TestMethodMismatchAnswersInTheErrorShape(t *testing.T) {
	server := newTestServer(t)

	for _, target := range []struct {
		method, path, allow string
	}{
		{http.MethodDelete, "/plugin/v1/enroll", "POST"},
		{http.MethodPut, "/api/v1/servers", "GET, POST"},
	} {
		request := httptest.NewRequest(target.method, target.path, nil)
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, request)

		if recorder.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: status = %d, want 405", target.method, target.path, recorder.Code)
		}
		if got := recorder.Header().Get("Allow"); got != target.allow {
			t.Errorf("%s %s: Allow = %q, want %q", target.method, target.path, got, target.allow)
		}

		var failure struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &failure); err != nil {
			t.Fatalf("decode body %q: %v", recorder.Body.String(), err)
		}
		if failure.Error.Code != "method_not_allowed" {
			t.Errorf("%s %s: code = %q", target.method, target.path, failure.Error.Code)
		}
	}
}
