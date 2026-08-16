package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"
)

// Check is one black-box assertion against a hub. Checks only ever speak HTTP
// to the target URL: nothing here may import hub code, or the suite would stop
// grading third-party hubs.
type Check struct {
	ID    string
	Title string
	// Section cites the clause of spec/protocol.md a check enforces, or the
	// slice that introduced it when the behavior is not protocol text.
	Section string
	Run     func(ctx context.Context, env Env) error
}

// Env is what a check gets to work with.
type Env struct {
	BaseURL string
	Client  *http.Client
	// PollClient is Client with a timeout long enough to let a hub hold a poll
	// for the longest interval the protocol allows. A held request is correct
	// behavior, so it must not look like a hang.
	PollClient *http.Client
	// AdminToken is the hub's bootstrap Admin API credential. The suite cannot
	// grade enrollment without one, so the runner refuses to start without it.
	AdminToken string
}

// get issues a GET against the target hub and returns the response body.
func (e Env) get(ctx context.Context, path string) (*http.Response, []byte, error) {
	return e.do(ctx, http.MethodGet, path, "", nil)
}

// do issues one request. body is marshalled as JSON when non-nil; bearer is
// sent as an Authorization header when non-empty.
func (e Env) do(ctx context.Context, method, path, bearer string, body any) (*http.Response, []byte, error) {
	return e.doWith(e.Client, ctx, method, path, bearer, body)
}

func (e Env) doWith(client *http.Client, ctx context.Context, method, path, bearer string, body any) (*http.Response, []byte, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, nil, fmt.Errorf("encode request body: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, e.BaseURL+path, reader)
	if err != nil {
		return nil, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, nil, fmt.Errorf("read body: %w", err)
	}
	return resp, responseBody, nil
}

// expect runs a request and asserts the status, decoding the body into out on
// success. Every check funnels through it so failures name the request.
func (e Env) expect(ctx context.Context, method, path, bearer string, body any, wantStatus int, out any) error {
	resp, responseBody, err := e.do(ctx, method, path, bearer, body)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	if resp.StatusCode != wantStatus {
		return fmt.Errorf("%s %s: want status %d, got %d, body %q",
			method, path, wantStatus, resp.StatusCode, truncate(responseBody))
	}
	if out != nil {
		if err := json.Unmarshal(responseBody, out); err != nil {
			return fmt.Errorf("%s %s: decode body %q: %w", method, path, truncate(responseBody), err)
		}
	}
	return nil
}

// expectPoll issues one long-poll, which needs the patient client.
func (e Env) expectPoll(ctx context.Context, sessionToken string, body any, wantStatus int, out any) error {
	const path = "/plugin/v1/poll"
	resp, responseBody, err := e.doWith(e.PollClient, ctx, http.MethodPost, path, sessionToken, body)
	if err != nil {
		return fmt.Errorf("POST %s: %w", path, err)
	}
	if resp.StatusCode != wantStatus {
		return fmt.Errorf("POST %s: want status %d, got %d, body %q",
			path, wantStatus, resp.StatusCode, truncate(responseBody))
	}
	if out != nil {
		if err := json.Unmarshal(responseBody, out); err != nil {
			return fmt.Errorf("POST %s: decode body %q: %w", path, truncate(responseBody), err)
		}
	}
	return nil
}

// expectPollError asserts a poll fails with a given status and protocol code.
func (e Env) expectPollError(ctx context.Context, sessionToken string, body any, wantStatus int, wantCode string) error {
	const path = "/plugin/v1/poll"
	resp, responseBody, err := e.doWith(e.PollClient, ctx, http.MethodPost, path, sessionToken, body)
	if err != nil {
		return fmt.Errorf("POST %s: %w", path, err)
	}
	if resp.StatusCode != wantStatus {
		return fmt.Errorf("POST %s: want status %d, got %d, body %q",
			path, wantStatus, resp.StatusCode, truncate(responseBody))
	}
	return assertErrorCode(http.MethodPost, path, responseBody, wantCode)
}

// expectError asserts a failed request answers with the protocol error envelope
// and the expected code (spec section 2.2).
func (e Env) expectError(ctx context.Context, method, path, bearer string, body any, wantStatus int, wantCode string) error {
	resp, responseBody, err := e.do(ctx, method, path, bearer, body)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	if resp.StatusCode != wantStatus {
		return fmt.Errorf("%s %s: want status %d, got %d, body %q",
			method, path, wantStatus, resp.StatusCode, truncate(responseBody))
	}
	return assertErrorCode(method, path, responseBody, wantCode)
}

// assertErrorCode checks a failure body against the protocol error envelope.
// wantCode may be empty to accept any code, which is how a check asserts the
// shape without over-fitting to one hub's choice of code.
func assertErrorCode(method, path string, responseBody []byte, wantCode string) error {
	var failure struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(responseBody, &failure); err != nil {
		return fmt.Errorf("%s %s: error body %q is not JSON: %w", method, path, truncate(responseBody), err)
	}
	if failure.Error.Code == "" {
		return fmt.Errorf("%s %s: error body %q carries no error.code", method, path, truncate(responseBody))
	}
	if wantCode != "" && failure.Error.Code != wantCode {
		return fmt.Errorf("%s %s: want error code %q, got %q (body %q)",
			method, path, wantCode, failure.Error.Code, truncate(responseBody))
	}
	return nil
}

// Fixture shapes. They are hand-written rather than shared with the hub so that
// a change in the reference implementation cannot quietly change the contract
// this suite grades.

type serverRecord struct {
	ID                   string  `json:"id"`
	Name                 string  `json:"name"`
	Game                 string  `json:"game"`
	CreatedAt            string  `json:"createdAt"`
	CredentialState      string  `json:"credentialState"`
	LastSeenAt           *string `json:"lastSeenAt"`
	PendingEnvelopeCount int     `json:"pendingEnvelopeCount"`
	Session              *struct {
		ID                 string `json:"id"`
		ExpiresAt          string `json:"expiresAt"`
		PollTimeoutSeconds int    `json:"pollTimeoutSeconds"`
	} `json:"session"`
}

type createdServer struct {
	Server     serverRecord `json:"server"`
	Enrollment struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expiresAt"`
	} `json:"enrollment"`
}

type credentials struct {
	ServerID     string `json:"serverId"`
	ServerSecret string `json:"serverSecret"`
	Server       struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Game string `json:"game"`
	} `json:"server"`
}

type sessionRecord struct {
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

// The suite's fixture game. It is deliberately not a real game id: a hub must
// not care which game enrolls.
const fixtureGame = "conformance"

// newServer creates a server record and returns it with its enrollment token.
func (e Env) newServer(ctx context.Context, name string) (createdServer, error) {
	var created createdServer
	err := e.expect(ctx, http.MethodPost, "/api/v1/servers", e.AdminToken,
		map[string]any{"name": name, "game": fixtureGame}, http.StatusCreated, &created)
	if err != nil {
		return createdServer{}, err
	}
	if created.Server.ID == "" {
		return createdServer{}, fmt.Errorf("create server: response carried no server.id")
	}
	if created.Enrollment.Token == "" {
		return createdServer{}, fmt.Errorf("create server: response carried no enrollment.token")
	}
	return created, nil
}

// newEnrolled creates a server and enrolls a plugin against it.
func (e Env) newEnrolled(ctx context.Context, name string) (credentials, createdServer, error) {
	created, err := e.newServer(ctx, name)
	if err != nil {
		return credentials{}, createdServer{}, err
	}
	enrolled, err := e.enroll(ctx, created.Enrollment.Token)
	if err != nil {
		return credentials{}, createdServer{}, err
	}
	return enrolled, created, nil
}

func (e Env) enroll(ctx context.Context, enrollmentToken string) (credentials, error) {
	var enrolled credentials
	err := e.expect(ctx, http.MethodPost, "/plugin/v1/enroll", "", map[string]any{
		"enrollmentToken": enrollmentToken,
		"game":            fixtureGame,
		"plugin":          map[string]string{"name": "conformance-plugin", "version": "0.1.0"},
		"transports":      []string{"poll"},
	}, http.StatusCreated, &enrolled)
	if err != nil {
		return credentials{}, err
	}
	if enrolled.ServerID == "" || enrolled.ServerSecret == "" {
		return credentials{}, fmt.Errorf("enroll: response carried no serverId or serverSecret")
	}
	return enrolled, nil
}

func (e Env) startSession(ctx context.Context, enrolled credentials, pollTimeoutSeconds int) (sessionRecord, error) {
	request := map[string]any{
		"serverId":     enrolled.ServerID,
		"serverSecret": enrolled.ServerSecret,
		"plugin":       map[string]string{"name": "conformance-plugin", "version": "0.1.0"},
		"transports":   []string{"poll"},
	}
	if pollTimeoutSeconds != 0 {
		request["pollTimeoutSeconds"] = pollTimeoutSeconds
	}

	var session sessionRecord
	if err := e.expect(ctx, http.MethodPost, "/plugin/v1/session", "", request, http.StatusOK, &session); err != nil {
		return sessionRecord{}, err
	}
	if session.SessionToken == "" || session.SessionID == "" {
		return sessionRecord{}, fmt.Errorf("session: response carried no sessionId or sessionToken")
	}
	return session, nil
}

// checks is the full suite, in execution order.
var checks = []Check{
	{
		ID:      "health.responds",
		Title:   "GET /healthz answers 200 with JSON",
		Section: "prefactor",
		Run: func(ctx context.Context, env Env) error {
			resp, body, err := env.get(ctx, "/healthz")
			if err != nil {
				return err
			}
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("want status 200, got %d, body %q", resp.StatusCode, truncate(body))
			}
			contentType := resp.Header.Get("Content-Type")
			mediaType, _, err := mime.ParseMediaType(contentType)
			if err != nil {
				return fmt.Errorf("parse Content-Type %q: %w", contentType, err)
			}
			if mediaType != "application/json" {
				return fmt.Errorf("want Content-Type application/json, got %q", contentType)
			}
			return nil
		},
	},
	{
		ID:      "health.status",
		Title:   "GET /healthz reports status ok",
		Section: "prefactor",
		Run: func(ctx context.Context, env Env) error {
			_, body, err := env.get(ctx, "/healthz")
			if err != nil {
				return err
			}
			var health struct {
				Status  string `json:"status"`
				Version string `json:"version"`
			}
			if err := json.Unmarshal(body, &health); err != nil {
				return fmt.Errorf("decode body %q: %w", truncate(body), err)
			}
			if health.Status != "ok" {
				return fmt.Errorf(`want status "ok", got %q`, health.Status)
			}
			if strings.TrimSpace(health.Version) == "" {
				return fmt.Errorf("version is empty")
			}
			return nil
		},
	},
	{
		ID:      "errors.shape",
		Title:   "An unrouted path answers 404 in the protocol error shape",
		Section: "2.2",
		Run: func(ctx context.Context, env Env) error {
			return env.expectError(ctx, http.MethodGet, "/api/v1/no-such-endpoint", env.AdminToken,
				nil, http.StatusNotFound, "")
		},
	},
	{
		ID:      "admin.auth.required",
		Title:   "The Admin API rejects a missing or wrong bearer token",
		Section: "2",
		Run: func(ctx context.Context, env Env) error {
			body := map[string]any{"name": "unauthorized attempt", "game": fixtureGame}
			for _, bearer := range []string{"", "vya_definitely-not-the-admin-token"} {
				if err := env.expectError(ctx, http.MethodPost, "/api/v1/servers", bearer,
					body, http.StatusUnauthorized, "unauthorized"); err != nil {
					return err
				}
			}
			return env.expectError(ctx, http.MethodGet, "/api/v1/servers", "",
				nil, http.StatusUnauthorized, "unauthorized")
		},
	},
	{
		ID:      "admin.servers.create",
		Title:   "POST /api/v1/servers records a server and mints one enrollment token",
		Section: "5.1",
		Run: func(ctx context.Context, env Env) error {
			created, err := env.newServer(ctx, "conformance: create")
			if err != nil {
				return err
			}
			if created.Server.CredentialState != "none" {
				return fmt.Errorf("credentialState = %q, want none before enrollment",
					created.Server.CredentialState)
			}
			if created.Server.Session != nil {
				return fmt.Errorf("a server that has never enrolled reported a session")
			}
			expiresAt, err := time.Parse(time.RFC3339, created.Enrollment.ExpiresAt)
			if err != nil {
				return fmt.Errorf("enrollment.expiresAt %q is not RFC 3339: %w",
					created.Enrollment.ExpiresAt, err)
			}
			if !expiresAt.After(time.Now()) {
				return fmt.Errorf("enrollment token expired at %s, before it was issued", expiresAt)
			}

			// The record must be readable back, and must never leak a credential.
			var fetched serverRecord
			if err := env.expect(ctx, http.MethodGet, "/api/v1/servers/"+created.Server.ID,
				env.AdminToken, nil, http.StatusOK, &fetched); err != nil {
				return err
			}
			if fetched.ID != created.Server.ID {
				return fmt.Errorf("fetched server id = %q, want %q", fetched.ID, created.Server.ID)
			}
			_, raw, err := env.do(ctx, http.MethodGet, "/api/v1/servers/"+created.Server.ID, env.AdminToken, nil)
			if err != nil {
				return err
			}
			if bytes.Contains(raw, []byte(created.Enrollment.Token)) {
				return fmt.Errorf("the server record echoed the enrollment token back")
			}
			return nil
		},
	},
	{
		ID:      "admin.servers.validate",
		Title:   "POST /api/v1/servers rejects a record with no name",
		Section: "5.1",
		Run: func(ctx context.Context, env Env) error {
			return env.expectError(ctx, http.MethodPost, "/api/v1/servers", env.AdminToken,
				map[string]any{"game": fixtureGame}, http.StatusBadRequest, "bad_request")
		},
	},
	{
		ID:      "plugin.enroll.exchange",
		Title:   "POST /plugin/v1/enroll exchanges the token for credentials",
		Section: "5.2",
		Run: func(ctx context.Context, env Env) error {
			enrolled, created, err := env.newEnrolled(ctx, "conformance: enroll")
			if err != nil {
				return err
			}
			if enrolled.ServerID != created.Server.ID {
				return fmt.Errorf("serverId = %q, want the created record %q",
					enrolled.ServerID, created.Server.ID)
			}
			if enrolled.ServerSecret == created.Enrollment.Token {
				return fmt.Errorf("the server secret is just the enrollment token again")
			}

			var record serverRecord
			if err := env.expect(ctx, http.MethodGet, "/api/v1/servers/"+created.Server.ID,
				env.AdminToken, nil, http.StatusOK, &record); err != nil {
				return err
			}
			if record.CredentialState != "active" {
				return fmt.Errorf("credentialState = %q after enrollment, want active",
					record.CredentialState)
			}
			return nil
		},
	},
	{
		ID:      "plugin.enroll.singleUse",
		Title:   "An enrollment token is burned by the enrollment that uses it",
		Section: "5.2",
		Run: func(ctx context.Context, env Env) error {
			_, created, err := env.newEnrolled(ctx, "conformance: reuse")
			if err != nil {
				return err
			}
			return env.expectError(ctx, http.MethodPost, "/plugin/v1/enroll", "", map[string]any{
				"enrollmentToken": created.Enrollment.Token,
				"game":            fixtureGame,
			}, http.StatusConflict, "enrollment_token_used")
		},
	},
	{
		ID:      "plugin.enroll.unknownToken",
		Title:   "An unknown enrollment token is rejected",
		Section: "5.2",
		Run: func(ctx context.Context, env Env) error {
			return env.expectError(ctx, http.MethodPost, "/plugin/v1/enroll", "", map[string]any{
				"enrollmentToken": "vye_conformance-unknown-enrollment-token",
				"game":            fixtureGame,
			}, http.StatusUnauthorized, "enrollment_token_invalid")
		},
	},
	{
		ID:      "plugin.session.exchange",
		Title:   "POST /plugin/v1/session issues a session with negotiated terms",
		Section: "5.3",
		Run: func(ctx context.Context, env Env) error {
			enrolled, created, err := env.newEnrolled(ctx, "conformance: session")
			if err != nil {
				return err
			}
			session, err := env.startSession(ctx, enrolled, 0)
			if err != nil {
				return err
			}
			if session.ProtocolVersion < 1 {
				return fmt.Errorf("protocolVersion = %d, want at least 1", session.ProtocolVersion)
			}
			if session.EnvelopeVersion < 1 {
				return fmt.Errorf("envelopeVersion = %d, want at least 1", session.EnvelopeVersion)
			}
			if session.PollTimeoutSeconds != 25 {
				return fmt.Errorf("pollTimeoutSeconds = %d with nothing requested, want the 25 s default",
					session.PollTimeoutSeconds)
			}
			if len(session.Transports) == 0 {
				return fmt.Errorf("the session advertised no transports")
			}
			if session.Features == nil {
				return fmt.Errorf("features must be present, as an object, even when empty")
			}
			expiresAt, err := time.Parse(time.RFC3339, session.ExpiresAt)
			if err != nil {
				return fmt.Errorf("expiresAt %q is not RFC 3339: %w", session.ExpiresAt, err)
			}
			if !expiresAt.After(time.Now()) {
				return fmt.Errorf("the session expired at %s, before it was issued", expiresAt)
			}

			// The session token authenticates a Plugin API call, and the server
			// record mirrors the live session back to the operator.
			var introspected sessionRecord
			if err := env.expect(ctx, http.MethodGet, "/plugin/v1/session", session.SessionToken,
				nil, http.StatusOK, &introspected); err != nil {
				return err
			}
			if introspected.SessionID != session.SessionID {
				return fmt.Errorf("introspected sessionId = %q, want %q",
					introspected.SessionID, session.SessionID)
			}
			var record serverRecord
			if err := env.expect(ctx, http.MethodGet, "/api/v1/servers/"+created.Server.ID,
				env.AdminToken, nil, http.StatusOK, &record); err != nil {
				return err
			}
			if record.Session == nil || record.Session.ID != session.SessionID {
				return fmt.Errorf("the server record does not report the live session")
			}
			if record.LastSeenAt == nil || *record.LastSeenAt == "" {
				return fmt.Errorf("lastSeenAt is still null after a session started")
			}
			return nil
		},
	},
	{
		ID:      "plugin.session.pollTimeout",
		Title:   "A requested pollTimeout is honored in range and clamped outside it",
		Section: "3.1.1",
		Run: func(ctx context.Context, env Env) error {
			enrolled, _, err := env.newEnrolled(ctx, "conformance: poll timeout")
			if err != nil {
				return err
			}

			for _, requested := range []int{5, 12, 60} {
				session, err := env.startSession(ctx, enrolled, requested)
				if err != nil {
					return err
				}
				if session.PollTimeoutSeconds != requested {
					return fmt.Errorf("requested %d s, got %d s; hubs must honor 5 s to 60 s",
						requested, session.PollTimeoutSeconds)
				}
			}

			for _, requested := range []int{1, 600} {
				session, err := env.startSession(ctx, enrolled, requested)
				if err != nil {
					return err
				}
				if session.PollTimeoutSeconds < 5 || session.PollTimeoutSeconds > 60 {
					return fmt.Errorf("requested %d s, got %d s; out-of-range values must be clamped into 5 s to 60 s",
						requested, session.PollTimeoutSeconds)
				}
			}
			return nil
		},
	},
	{
		ID:      "plugin.session.badCredentials",
		Title:   "Session creation rejects a wrong secret and an unknown server",
		Section: "5.3",
		Run: func(ctx context.Context, env Env) error {
			enrolled, _, err := env.newEnrolled(ctx, "conformance: bad credentials")
			if err != nil {
				return err
			}

			if err := env.expectError(ctx, http.MethodPost, "/plugin/v1/session", "", map[string]any{
				"serverId":     enrolled.ServerID,
				"serverSecret": "vys_conformance-wrong-secret",
			}, http.StatusUnauthorized, "credentials_invalid"); err != nil {
				return err
			}
			return env.expectError(ctx, http.MethodPost, "/plugin/v1/session", "", map[string]any{
				"serverId":     "conformance-no-such-server",
				"serverSecret": enrolled.ServerSecret,
			}, http.StatusUnauthorized, "credentials_invalid")
		},
	},
	{
		ID:      "plugin.session.single",
		Title:   "A new session supersedes the server's previous one",
		Section: "5.3",
		Run: func(ctx context.Context, env Env) error {
			enrolled, _, err := env.newEnrolled(ctx, "conformance: one session")
			if err != nil {
				return err
			}
			first, err := env.startSession(ctx, enrolled, 0)
			if err != nil {
				return err
			}
			second, err := env.startSession(ctx, enrolled, 0)
			if err != nil {
				return err
			}
			if first.SessionID == second.SessionID {
				return fmt.Errorf("the second session reused the first session's id %q", first.SessionID)
			}
			if err := env.expectError(ctx, http.MethodGet, "/plugin/v1/session", first.SessionToken,
				nil, http.StatusUnauthorized, "session_invalid"); err != nil {
				return err
			}
			return env.expect(ctx, http.MethodGet, "/plugin/v1/session", second.SessionToken,
				nil, http.StatusOK, nil)
		},
	},
	{
		ID:      "plugin.session.revoked",
		Title:   "Revoking credentials kills the live session at once",
		Section: "5.4",
		Run: func(ctx context.Context, env Env) error {
			enrolled, created, err := env.newEnrolled(ctx, "conformance: revoke")
			if err != nil {
				return err
			}
			session, err := env.startSession(ctx, enrolled, 0)
			if err != nil {
				return err
			}

			if err := env.expect(ctx, http.MethodDelete,
				"/api/v1/servers/"+created.Server.ID+"/credentials", env.AdminToken,
				nil, http.StatusNoContent, nil); err != nil {
				return err
			}

			if err := env.expectError(ctx, http.MethodGet, "/plugin/v1/session", session.SessionToken,
				nil, http.StatusUnauthorized, "session_invalid"); err != nil {
				return err
			}
			if err := env.expectError(ctx, http.MethodPost, "/plugin/v1/session", "", map[string]any{
				"serverId":     enrolled.ServerID,
				"serverSecret": enrolled.ServerSecret,
			}, http.StatusUnauthorized, "credentials_revoked"); err != nil {
				return err
			}

			var record serverRecord
			if err := env.expect(ctx, http.MethodGet, "/api/v1/servers/"+created.Server.ID,
				env.AdminToken, nil, http.StatusOK, &record); err != nil {
				return err
			}
			if record.CredentialState != "revoked" {
				return fmt.Errorf("credentialState = %q after revocation, want revoked", record.CredentialState)
			}
			if record.Session != nil {
				return fmt.Errorf("the server record still reports a live session after revocation")
			}
			return nil
		},
	},
	{
		ID:      "plugin.enroll.recovery",
		Title:   "A reissued enrollment token replaces the credentials of a revoked server",
		Section: "5.2",
		Run: func(ctx context.Context, env Env) error {
			enrolled, created, err := env.newEnrolled(ctx, "conformance: recovery")
			if err != nil {
				return err
			}
			if err := env.expect(ctx, http.MethodDelete,
				"/api/v1/servers/"+created.Server.ID+"/credentials", env.AdminToken,
				nil, http.StatusNoContent, nil); err != nil {
				return err
			}

			var reissued struct {
				Token     string `json:"token"`
				ExpiresAt string `json:"expiresAt"`
			}
			if err := env.expect(ctx, http.MethodPost,
				"/api/v1/servers/"+created.Server.ID+"/enrollment-token", env.AdminToken,
				nil, http.StatusCreated, &reissued); err != nil {
				return err
			}
			if reissued.Token == "" {
				return fmt.Errorf("the reissued enrollment token is empty")
			}
			if reissued.Token == created.Enrollment.Token {
				return fmt.Errorf("the reissued enrollment token repeats the burned one")
			}

			recovered, err := env.enroll(ctx, reissued.Token)
			if err != nil {
				return err
			}
			if recovered.ServerSecret == enrolled.ServerSecret {
				return fmt.Errorf("re-enrollment handed back the revoked secret")
			}
			if _, err := env.startSession(ctx, recovered, 0); err != nil {
				return err
			}
			return env.expectError(ctx, http.MethodPost, "/plugin/v1/session", "", map[string]any{
				"serverId":     enrolled.ServerID,
				"serverSecret": enrolled.ServerSecret,
			}, http.StatusUnauthorized, "credentials_invalid")
		},
	},
	{
		ID:      "realms.separate",
		Title:   "A credential from one realm is worthless in the other",
		Section: "2",
		Run: func(ctx context.Context, env Env) error {
			enrolled, _, err := env.newEnrolled(ctx, "conformance: realms")
			if err != nil {
				return err
			}
			session, err := env.startSession(ctx, enrolled, 0)
			if err != nil {
				return err
			}

			for _, bearer := range []string{session.SessionToken, enrolled.ServerSecret} {
				if err := env.expectError(ctx, http.MethodGet, "/api/v1/servers", bearer,
					nil, http.StatusUnauthorized, "unauthorized"); err != nil {
					return err
				}
			}
			return env.expectError(ctx, http.MethodGet, "/plugin/v1/session", env.AdminToken,
				nil, http.StatusUnauthorized, "session_invalid")
		},
	},
	{
		ID:      "plugin.poll.auth",
		Title:   "POST /plugin/v1/poll requires a live session token",
		Section: "3.1.2",
		Run: func(ctx context.Context, env Env) error {
			for _, bearer := range []string{"", "vyt_conformance-not-a-session-token"} {
				if err := env.expectPollError(ctx, bearer, pollRequest{},
					http.StatusUnauthorized, "session_invalid"); err != nil {
					return err
				}
			}
			return nil
		},
	},
	{
		ID:      "plugin.poll.idleHold",
		Title:   "An idle poll is held to the negotiated timeout and answers an empty batch",
		Section: "3.1.2",
		Run: func(ctx context.Context, env Env) error {
			plugin, err := env.newFakePlugin(ctx, "conformance: idle poll", shortPollTimeoutSeconds)
			if err != nil {
				return err
			}

			started := time.Now()
			response, err := plugin.pollAndAck(ctx)
			if err != nil {
				return err
			}
			held := time.Since(started)

			if len(response.Envelopes) != 0 {
				return fmt.Errorf("an idle poll answered with %d envelopes", len(response.Envelopes))
			}
			if response.PollTimeoutSeconds != shortPollTimeoutSeconds {
				return fmt.Errorf("pollTimeoutSeconds = %d, want the negotiated %d",
					response.PollTimeoutSeconds, shortPollTimeoutSeconds)
			}
			if _, err := time.Parse(time.RFC3339, response.SessionExpiresAt); err != nil {
				return fmt.Errorf("sessionExpiresAt %q is not RFC 3339: %w",
					response.SessionExpiresAt, err)
			}

			negotiated := time.Duration(shortPollTimeoutSeconds) * time.Second
			if held < negotiated/2 {
				return fmt.Errorf("the hub answered an idle poll after %s; it must hold up to %s",
					held, negotiated)
			}
			// "No later than the effective pollTimeout" is a MUST, so the
			// allowance here is for network and scheduling jitter only.
			if held > negotiated+2*time.Second {
				return fmt.Errorf("the hub held for %s, past the %s it negotiated; a response is due no later than the effective pollTimeout",
					held, negotiated)
			}
			return nil
		},
	},
	{
		ID:      "plugin.poll.lastSeen",
		Title:   "Every poll moves the server record's lastSeenAt",
		Section: "3.1.2",
		Run: func(ctx context.Context, env Env) error {
			plugin, err := env.newFakePlugin(ctx, "conformance: last seen", shortPollTimeoutSeconds)
			if err != nil {
				return err
			}
			lastSeen := func() (string, error) {
				var record serverRecord
				if err := env.expect(ctx, http.MethodGet,
					"/api/v1/servers/"+plugin.Server.Server.ID, env.AdminToken,
					nil, http.StatusOK, &record); err != nil {
					return "", err
				}
				if record.LastSeenAt == nil || *record.LastSeenAt == "" {
					return "", fmt.Errorf("lastSeenAt is null after a session started")
				}
				return *record.LastSeenAt, nil
			}

			atSession, err := lastSeen()
			if err != nil {
				return err
			}

			// A poll that is answered at once, so the check does not spend a
			// hold proving a timestamp moved.
			if _, err := env.queueEnvelope(ctx, plugin.Server.Server.ID, unknownType(), nil); err != nil {
				return err
			}
			if _, err := plugin.pollAndAck(ctx); err != nil {
				return err
			}

			afterPoll, err := lastSeen()
			if err != nil {
				return err
			}
			if afterPoll == atSession {
				return fmt.Errorf("lastSeenAt is still %q after a poll; a hub must update it on every poll",
					afterPoll)
			}
			return nil
		},
	},
	{
		ID:      "plugin.poll.inboundTolerance",
		Title:   "An omitted v and an unparseable ts are tolerated, an explicit v of 0 is not",
		Section: "4",
		Run: func(ctx context.Context, env Env) error {
			plugin, err := env.newFakePlugin(ctx, "conformance: inbound tolerance", shortPollTimeoutSeconds)
			if err != nil {
				return err
			}
			// Each poll carries queued work so none of them is held.
			nudge := func() error {
				_, err := env.queueEnvelope(ctx, plugin.Server.Server.ID, unknownType(), nil)
				return err
			}

			// An absent v means the negotiated envelope version.
			noVersion := plugin.nextOutbound(unknownType(), nil)
			noVersion.V = 0 // omitempty drops it from the request body entirely
			if err := nudge(); err != nil {
				return err
			}
			accepted, err := plugin.poll(ctx, pollRequest{Envelopes: []envelope{noVersion}})
			if err != nil {
				return err
			}
			if accepted.Ack != noVersion.Seq {
				return fmt.Errorf("ack = %d after an envelope with no v, want %d; absence means the negotiated version",
					accepted.Ack, noVersion.Seq)
			}

			// A clock a game engine got wrong must not cost the batch.
			badTimestamp := plugin.nextOutbound(unknownType(), nil)
			badTimestamp.TS = "not a timestamp at all"
			if err := nudge(); err != nil {
				return err
			}
			tolerated, err := plugin.poll(ctx, pollRequest{Envelopes: []envelope{badTimestamp}})
			if err != nil {
				return err
			}
			if tolerated.Ack != badTimestamp.Seq {
				return fmt.Errorf("ack = %d after an unparseable ts, want %d; a receiver must not reject an envelope over ts alone",
					tolerated.Ack, badTimestamp.Seq)
			}

			// An explicit zero is not an absence: it names no version at all.
			explicitZero := plugin.nextOutbound(unknownType(), nil)
			return env.expectPollError(ctx, plugin.Session.SessionToken,
				map[string]any{"envelopes": []map[string]any{{
					"v": 0, "id": explicitZero.ID, "type": explicitZero.Type,
					"seq": explicitZero.Seq, "ts": explicitZero.TS, "body": map[string]any{},
				}}}, http.StatusBadRequest, "envelope_invalid")
		},
	},
	{
		ID:      "plugin.poll.batchLimit",
		Title:   "A 200-envelope batch is accepted and an over-cap batch is refused, not truncated",
		Section: "3.1.2",
		Run: func(ctx context.Context, env Env) error {
			plugin, err := env.newFakePlugin(ctx, "conformance: batch limit", shortPollTimeoutSeconds)
			if err != nil {
				return err
			}

			// The floor every hub must accept.
			batch := make([]envelope, 0, 200)
			for range 200 {
				batch = append(batch, plugin.nextOutbound(unknownType(), nil))
			}
			if _, err := env.queueEnvelope(ctx, plugin.Server.Server.ID, unknownType(), nil); err != nil {
				return err
			}
			accepted, err := plugin.poll(ctx, pollRequest{Envelopes: batch})
			if err != nil {
				return err
			}
			if accepted.Ack != 200 {
				return fmt.Errorf("ack = %d after a 200-envelope batch, want 200; a hub must accept at least 200 per poll",
					accepted.Ack)
			}

			// Past whatever cap the hub chose, it must refuse rather than
			// silently keep part of the batch: a plugin that believes an
			// envelope landed will never send it again.
			oversized := make([]envelope, 0, 5001)
			for range 5001 {
				oversized = append(oversized, plugin.nextOutbound(unknownType(), nil))
			}
			resp, body, err := env.doWith(env.PollClient, ctx, http.MethodPost, "/plugin/v1/poll",
				plugin.Session.SessionToken, pollRequest{Envelopes: oversized})
			if err != nil {
				return fmt.Errorf("POST /plugin/v1/poll with 5001 envelopes: %w", err)
			}
			if resp.StatusCode == http.StatusOK {
				return fmt.Errorf("a 5001-envelope batch was answered 200; a hub must refuse a batch over its cap rather than truncate it")
			}
			if resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusRequestEntityTooLarge {
				return fmt.Errorf("a 5001-envelope batch got status %d, want 400 or 413 (body %q)",
					resp.StatusCode, truncate(body))
			}
			return assertErrorCode(http.MethodPost, "/plugin/v1/poll", body, "")
		},
	},
	{
		ID:      "plugin.poll.supersededDuringHold",
		Title:   "A new session answers the previous session's held poll at once",
		Section: "3.1.2",
		Run: func(ctx context.Context, env Env) error {
			plugin, err := env.newFakePlugin(ctx, "conformance: superseded mid-hold", 25)
			if err != nil {
				return err
			}
			stale := plugin.Session.SessionToken

			failed := make(chan error, 1)
			started := time.Now()
			go func() {
				failed <- env.expectPollError(ctx, stale, pollRequest{},
					http.StatusUnauthorized, "session_invalid")
			}()
			time.Sleep(250 * time.Millisecond)

			// The game server restarts and opens a fresh session.
			if err := plugin.reconnect(ctx, shortPollTimeoutSeconds); err != nil {
				return err
			}

			if err := <-failed; err != nil {
				return err
			}
			if elapsed := time.Since(started); elapsed > 5*time.Second {
				return fmt.Errorf("the superseded poll took %s to answer 401; a held poll must not outlive its session",
					elapsed)
			}
			return nil
		},
	},
	{
		ID:      "plugin.poll.deliver",
		Title:   "A queued envelope reaches the plugin, framed per spec section 4",
		Section: "4",
		Run: func(ctx context.Context, env Env) error {
			plugin, err := env.newFakePlugin(ctx, "conformance: delivery", shortPollTimeoutSeconds)
			if err != nil {
				return err
			}

			envelopeType := unknownType()
			queuedID, err := env.queueEnvelope(ctx, plugin.Server.Server.ID, envelopeType,
				map[string]any{"marker": "conformance"})
			if err != nil {
				return err
			}

			started := time.Now()
			response, err := plugin.pollAndAck(ctx)
			if err != nil {
				return err
			}
			// Work was already queued, so there was nothing to hold for.
			if elapsed := time.Since(started); elapsed > 2*time.Second {
				return fmt.Errorf("the hub held for %s with an envelope already queued; it must answer at once when work is waiting",
					elapsed)
			}
			if len(response.Envelopes) != 1 {
				return fmt.Errorf("got %d envelopes, want the one that was queued", len(response.Envelopes))
			}

			delivered := response.Envelopes[0]
			if delivered.ID != queuedID {
				return fmt.Errorf("envelope id = %q, want the queued %q", delivered.ID, queuedID)
			}
			if delivered.Type != envelopeType {
				return fmt.Errorf("type = %q, want %q", delivered.Type, envelopeType)
			}
			if delivered.Seq != 1 {
				return fmt.Errorf("seq = %d on the first envelope of a session, want 1", delivered.Seq)
			}
			if delivered.V < 1 {
				return fmt.Errorf("v = %d, want the envelope version the session negotiated", delivered.V)
			}
			var body struct {
				Marker string `json:"marker"`
			}
			if err := json.Unmarshal(delivered.Body, &body); err != nil {
				return fmt.Errorf("decode envelope body %q: %w", truncate(delivered.Body), err)
			}
			if body.Marker != "conformance" {
				return fmt.Errorf("the envelope body did not survive the round trip: %q",
					truncate(delivered.Body))
			}
			return nil
		},
	},
	{
		ID:      "plugin.poll.earlyFlush",
		Title:   "An envelope queued during a held poll is delivered without waiting the hold out",
		Section: "3.1.2",
		Run: func(ctx context.Context, env Env) error {
			// A long hold, so that answering promptly cannot be confused with
			// the hold simply expiring.
			plugin, err := env.newFakePlugin(ctx, "conformance: early flush", 25)
			if err != nil {
				return err
			}

			held := plugin.pollInBackground(ctx, pollRequest{})
			if _, err := env.queueEnvelope(ctx, plugin.Server.Server.ID, unknownType(), nil); err != nil {
				return err
			}

			result := <-held
			if result.Err != nil {
				return result.Err
			}
			if len(result.Response.Envelopes) != 1 {
				return fmt.Errorf("the held poll answered with %d envelopes, want the one queued mid-hold",
					len(result.Response.Envelopes))
			}
			if result.Elapsed > 5*time.Second {
				return fmt.Errorf("the hub took %s to flush an envelope queued during a 25 s hold; it must respond as soon as messages are queued",
					result.Elapsed)
			}
			return nil
		},
	},
	{
		ID:      "plugin.poll.retransmit",
		Title:   "An unacked envelope is delivered again on the next poll, unchanged",
		Section: "9.1",
		Run: func(ctx context.Context, env Env) error {
			plugin, err := env.newFakePlugin(ctx, "conformance: retransmit", shortPollTimeoutSeconds)
			if err != nil {
				return err
			}
			if _, err := env.queueEnvelope(ctx, plugin.Server.Server.ID, unknownType(),
				map[string]any{"marker": "unchanged across retransmission"}); err != nil {
				return err
			}

			// Deliberately never acking, so the same envelope must come back.
			first, err := plugin.poll(ctx, pollRequest{})
			if err != nil {
				return err
			}
			if len(first.Envelopes) != 1 {
				return fmt.Errorf("the first poll got %d envelopes, want 1", len(first.Envelopes))
			}
			second, err := plugin.poll(ctx, pollRequest{})
			if err != nil {
				return err
			}
			if len(second.Envelopes) != 1 {
				return fmt.Errorf("the second poll got %d envelopes; an unacked envelope must be retransmitted",
					len(second.Envelopes))
			}
			// A retransmission that differs cannot be deduplicated, so the
			// identifying fields must survive it untouched.
			sent, resent := first.Envelopes[0], second.Envelopes[0]
			switch {
			case sent.ID != resent.ID:
				return fmt.Errorf("the retransmission changed id: %q then %q", sent.ID, resent.ID)
			case sent.Seq != resent.Seq:
				return fmt.Errorf("the retransmission changed seq: %d then %d", sent.Seq, resent.Seq)
			case sent.TS != resent.TS:
				return fmt.Errorf("the retransmission changed ts: %q then %q", sent.TS, resent.TS)
			case sent.Type != resent.Type:
				return fmt.Errorf("the retransmission changed type: %q then %q", sent.Type, resent.Type)
			case !bytes.Equal(bytes.TrimSpace(sent.Body), bytes.TrimSpace(resent.Body)):
				return fmt.Errorf("the retransmission changed body: %q then %q",
					truncate(sent.Body), truncate(resent.Body))
			}

			// Acked, so only what was queued afterwards is left to send.
			acked := second.Envelopes[0]
			nextID, err := env.queueEnvelope(ctx, plugin.Server.Server.ID, unknownType(), nil)
			if err != nil {
				return err
			}
			third, err := plugin.poll(ctx, pollRequest{Ack: acked.Seq})
			if err != nil {
				return err
			}
			if len(third.Envelopes) != 1 || third.Envelopes[0].ID != nextID {
				return fmt.Errorf("after acking seq %d, got %+v, want only the newly queued envelope",
					acked.Seq, third.Envelopes)
			}
			return nil
		},
	},
	{
		ID:      "plugin.poll.ackContiguous",
		Title:   "Acks are cumulative and monotonic in the hub -> plugin direction",
		Section: "9.1",
		Run: func(ctx context.Context, env Env) error {
			plugin, err := env.newFakePlugin(ctx, "conformance: cumulative acks", shortPollTimeoutSeconds)
			if err != nil {
				return err
			}
			for range 3 {
				if _, err := env.queueEnvelope(ctx, plugin.Server.Server.ID, unknownType(), nil); err != nil {
					return err
				}
			}

			first, err := plugin.poll(ctx, pollRequest{})
			if err != nil {
				return err
			}
			if len(first.Envelopes) != 3 {
				return fmt.Errorf("got %d envelopes, want the 3 that were queued", len(first.Envelopes))
			}
			for i, delivered := range first.Envelopes {
				if delivered.Seq != int64(i+1) {
					return fmt.Errorf("envelope %d has seq %d, want %d: a session numbers from 1, contiguously",
						i, delivered.Seq, i+1)
				}
			}

			// Acking 2 acks 1 as well, and leaves 3 outstanding.
			second, err := plugin.poll(ctx, pollRequest{Ack: 2})
			if err != nil {
				return err
			}
			if len(second.Envelopes) != 1 || second.Envelopes[0].Seq != 3 {
				return fmt.Errorf("after acking 2, got %+v, want only seq 3: an ack covers everything below it",
					second.Envelopes)
			}

			// A stale ack must not resurrect what a higher one already retired.
			if _, err := env.queueEnvelope(ctx, plugin.Server.Server.ID, unknownType(), nil); err != nil {
				return err
			}
			third, err := plugin.poll(ctx, pollRequest{Ack: 3})
			if err != nil {
				return err
			}
			if len(third.Envelopes) != 1 || third.Envelopes[0].Seq != 4 {
				return fmt.Errorf("after acking 3, got %+v, want only the newly queued seq 4", third.Envelopes)
			}
			fourth, err := plugin.poll(ctx, pollRequest{Ack: 1})
			if err != nil {
				return err
			}
			if len(fourth.Envelopes) != 1 || fourth.Envelopes[0].Seq != 4 {
				return fmt.Errorf("a stale ack of 1 produced %+v, want only the still-unacked seq 4; acks are monotonic",
					fourth.Envelopes)
			}
			return nil
		},
	},
	{
		ID:      "plugin.poll.ackOutOfRange",
		Title:   "An ack above what the hub has sent is refused",
		Section: "3.1.2",
		Run: func(ctx context.Context, env Env) error {
			plugin, err := env.newFakePlugin(ctx, "conformance: ack out of range", shortPollTimeoutSeconds)
			if err != nil {
				return err
			}
			return plugin.pollExpectingError(ctx, pollRequest{Ack: 9999},
				http.StatusBadRequest, "ack_out_of_range")
		},
	},
	{
		ID:      "plugin.poll.inbound",
		Title:   "The hub acks the highest contiguous seq the plugin sends",
		Section: "9.1",
		Run: func(ctx context.Context, env Env) error {
			plugin, err := env.newFakePlugin(ctx, "conformance: inbound acks", shortPollTimeoutSeconds)
			if err != nil {
				return err
			}
			// One envelope queued per poll, so no poll in this check is held:
			// grading the ack should not cost a hold each time.
			nudge := func() error {
				_, err := env.queueEnvelope(ctx, plugin.Server.Server.ID, unknownType(), nil)
				return err
			}

			if err := nudge(); err != nil {
				return err
			}
			first, err := plugin.poll(ctx, pollRequest{Envelopes: []envelope{
				plugin.nextOutbound(unknownType(), nil),
				plugin.nextOutbound(unknownType(), nil),
			}})
			if err != nil {
				return err
			}
			if first.Ack != 2 {
				return fmt.Errorf("ack = %d after two envelopes, want 2", first.Ack)
			}

			// An envelope above a gap must not move the ack.
			gapped := plugin.nextOutbound(unknownType(), nil)
			gapped.Seq = 9
			if err := nudge(); err != nil {
				return err
			}
			afterGap, err := plugin.poll(ctx, pollRequest{Envelopes: []envelope{gapped}})
			if err != nil {
				return err
			}
			if afterGap.Ack != 2 {
				return fmt.Errorf("ack = %d after an envelope above a gap, want it to stay at 2",
					afterGap.Ack)
			}

			// A duplicate is acked again and is never an error.
			duplicate := plugin.nextOutbound(unknownType(), nil)
			duplicate.Seq = 1
			next := plugin.nextOutbound(unknownType(), nil)
			next.Seq = 3
			if err := nudge(); err != nil {
				return err
			}
			afterDuplicate, err := plugin.poll(ctx, pollRequest{Envelopes: []envelope{duplicate, next}})
			if err != nil {
				return err
			}
			if afterDuplicate.Ack != 3 {
				return fmt.Errorf("ack = %d after a duplicate followed by the next envelope, want 3",
					afterDuplicate.Ack)
			}
			return nil
		},
	},
	{
		ID:      "plugin.poll.inboundRenumber",
		Title:   "A plugin's unacked envelope is renumbered into the next session and accepted there",
		Section: "9.1",
		Run: func(ctx context.Context, env Env) error {
			plugin, err := env.newFakePlugin(ctx, "conformance: inbound renumber", shortPollTimeoutSeconds)
			if err != nil {
				return err
			}
			nudge := func() error {
				_, err := env.queueEnvelope(ctx, plugin.Server.Server.ID, unknownType(), nil)
				return err
			}

			if err := nudge(); err != nil {
				return err
			}
			settled, err := plugin.poll(ctx, pollRequest{Envelopes: []envelope{
				plugin.nextOutbound(unknownType(), nil),
				plugin.nextOutbound(unknownType(), nil),
			}})
			if err != nil {
				return err
			}
			if settled.Ack != 2 {
				return fmt.Errorf("ack = %d after two envelopes, want 2", settled.Ack)
			}

			// Framed but never sent: the envelope a real plugin still holds in
			// its ring buffer when the game server goes down.
			strandedInOldSession := plugin.nextOutbound(unknownType(),
				map[string]any{"marker": "survived the session change"})

			if err := plugin.reconnect(ctx, shortPollTimeoutSeconds); err != nil {
				return err
			}

			// Sent unchanged, its old seq is a gap the new session can never
			// close: this is why renumbering is required rather than optional.
			if err := nudge(); err != nil {
				return err
			}
			stranded, err := plugin.poll(ctx, pollRequest{Envelopes: []envelope{strandedInOldSession}})
			if err != nil {
				return err
			}
			if stranded.Ack != 0 {
				return fmt.Errorf("ack = %d for an envelope carrying its previous session's seq %d, want 0: a new session counts from 1, so that seq is above a gap",
					stranded.Ack, strandedInOldSession.Seq)
			}

			// Renumbered into the new session, it lands. Only seq moved, so the
			// receiver can still recognize it as the message it always was.
			renumbered := plugin.renumber(strandedInOldSession)
			if renumbered.Seq != 1 {
				return fmt.Errorf("the suite renumbered to seq %d, want 1", renumbered.Seq)
			}
			if renumbered.ID != strandedInOldSession.ID || renumbered.Type != strandedInOldSession.Type ||
				renumbered.TS != strandedInOldSession.TS {
				return fmt.Errorf("renumbering changed more than seq: %+v became %+v",
					strandedInOldSession, renumbered)
			}
			if err := nudge(); err != nil {
				return err
			}
			recovered, err := plugin.poll(ctx, pollRequest{Envelopes: []envelope{renumbered}})
			if err != nil {
				return err
			}
			if recovered.Ack != 1 {
				return fmt.Errorf("ack = %d after the renumbered envelope, want 1: an envelope unacked when a session ended must be recoverable in the next one",
					recovered.Ack)
			}
			return nil
		},
	},
	{
		ID:      "plugin.poll.envelopeInvalid",
		Title:   "A malformed inbound envelope is refused, naming what was wrong",
		Section: "3.1.2",
		Run: func(ctx context.Context, env Env) error {
			plugin, err := env.newFakePlugin(ctx, "conformance: malformed envelopes", shortPollTimeoutSeconds)
			if err != nil {
				return err
			}

			noType := plugin.nextOutbound("", nil)
			noID := plugin.nextOutbound(unknownType(), nil)
			noID.ID = ""
			badSeq := plugin.nextOutbound(unknownType(), nil)
			badSeq.Seq = 0
			futureVersion := plugin.nextOutbound(unknownType(), nil)
			futureVersion.V = 99

			for _, malformed := range []envelope{noType, noID, badSeq, futureVersion} {
				if err := plugin.pollExpectingError(ctx, pollRequest{Envelopes: []envelope{malformed}},
					http.StatusBadRequest, "envelope_invalid"); err != nil {
					return err
				}
			}

			// The error must name which envelope of the batch was wrong, or a
			// plugin sending 200 at a time cannot act on it.
			good := plugin.nextOutbound(unknownType(), nil)
			_, body, err := env.doWith(env.PollClient, ctx, http.MethodPost, "/plugin/v1/poll",
				plugin.Session.SessionToken,
				pollRequest{Envelopes: []envelope{good, futureVersion}})
			if err != nil {
				return err
			}
			var failure struct {
				Error struct {
					Details map[string]any `json:"details"`
				} `json:"error"`
			}
			if err := json.Unmarshal(body, &failure); err != nil {
				return fmt.Errorf("decode error body %q: %w", truncate(body), err)
			}
			index, ok := failure.Error.Details["index"]
			if !ok {
				return fmt.Errorf("envelope_invalid carried no details.index; it must name the offending envelope (body %q)",
					truncate(body))
			}
			if asFloat, isNumber := index.(float64); !isNumber || int(asFloat) != 1 {
				return fmt.Errorf("details.index = %v, want 1: the second envelope was the malformed one", index)
			}

			// A rejected poll must change nothing, so the valid envelope that
			// travelled beside the malformed one is still unacked.
			if _, err := env.queueEnvelope(ctx, plugin.Server.Server.ID, unknownType(), nil); err != nil {
				return err
			}
			settled, err := plugin.poll(ctx, pollRequest{})
			if err != nil {
				return err
			}
			if settled.Ack != 0 {
				return fmt.Errorf("ack = %d, want 0: a poll rejected for a malformed envelope must apply nothing at all",
					settled.Ack)
			}
			return nil
		},
	},
	{
		// This grades survival across a session change, not across a hub
		// restart. A black-box suite cannot restart the implementation under
		// test, so the durability half of section 9.2 stays ungraded here and
		// the name says so rather than implying more than it proves.
		ID:      "plugin.poll.queueOutlivesSession",
		Title:   "An unacked envelope survives a session change and is renumbered into the next one",
		Section: "9.2",
		Run: func(ctx context.Context, env Env) error {
			enrolled, created, err := env.newEnrolled(ctx, "conformance: queue outlives session")
			if err != nil {
				return err
			}

			// Queued while nothing is connected: durability, not delivery.
			queuedID, err := env.queueEnvelope(ctx, created.Server.ID, unknownType(), nil)
			if err != nil {
				return err
			}
			var record serverRecord
			if err := env.expect(ctx, http.MethodGet, "/api/v1/servers/"+created.Server.ID,
				env.AdminToken, nil, http.StatusOK, &record); err != nil {
				return err
			}
			if record.PendingEnvelopeCount != 1 {
				return fmt.Errorf("pendingEnvelopeCount = %d with one envelope queued, want 1",
					record.PendingEnvelopeCount)
			}

			session, err := env.startSession(ctx, enrolled, shortPollTimeoutSeconds)
			if err != nil {
				return err
			}
			plugin := &fakePlugin{env: env, Creds: enrolled, Server: created, Session: session}

			first, err := plugin.poll(ctx, pollRequest{})
			if err != nil {
				return err
			}
			if len(first.Envelopes) != 1 || first.Envelopes[0].ID != queuedID {
				return fmt.Errorf("the first poll of a new session got %+v, want the envelope queued before it existed",
					first.Envelopes)
			}
			if first.Envelopes[0].Seq != 1 {
				return fmt.Errorf("seq = %d, want 1: each session numbers from the start",
					first.Envelopes[0].Seq)
			}

			// A second session must renumber what the first never acked.
			if err := plugin.reconnect(ctx, shortPollTimeoutSeconds); err != nil {
				return err
			}
			second, err := plugin.poll(ctx, pollRequest{})
			if err != nil {
				return err
			}
			if len(second.Envelopes) != 1 || second.Envelopes[0].ID != queuedID {
				return fmt.Errorf("the second session got %+v, want the still-unacked envelope",
					second.Envelopes)
			}
			if second.Envelopes[0].Seq != 1 {
				return fmt.Errorf("seq = %d in a fresh session, want 1: sequence state is per session",
					second.Envelopes[0].Seq)
			}

			// The counter an operator watches must follow the whole lifecycle,
			// not just the moment of queueing: still 1 while it is outstanding,
			// 0 once acked.
			pending := func() (int, error) {
				var record serverRecord
				if err := env.expect(ctx, http.MethodGet, "/api/v1/servers/"+created.Server.ID,
					env.AdminToken, nil, http.StatusOK, &record); err != nil {
					return 0, err
				}
				return record.PendingEnvelopeCount, nil
			}
			if outstanding, err := pending(); err != nil {
				return err
			} else if outstanding != 1 {
				return fmt.Errorf("pendingEnvelopeCount = %d while an envelope is delivered but unacked, want 1",
					outstanding)
			}

			if _, err := plugin.poll(ctx, pollRequest{Ack: second.Envelopes[0].Seq}); err != nil {
				return err
			}
			if outstanding, err := pending(); err != nil {
				return err
			} else if outstanding != 0 {
				return fmt.Errorf("pendingEnvelopeCount = %d after the ack, want 0", outstanding)
			}
			return nil
		},
	},
	{
		ID:      "plugin.poll.revokedDuringHold",
		Title:   "Revoking credentials answers a held poll at once, not at the end of the hold",
		Section: "5.4",
		Run: func(ctx context.Context, env Env) error {
			plugin, err := env.newFakePlugin(ctx, "conformance: revoked mid-hold", 25)
			if err != nil {
				return err
			}

			failed := make(chan error, 1)
			started := time.Now()
			go func() {
				failed <- env.expectPollError(ctx, plugin.Session.SessionToken, pollRequest{},
					http.StatusUnauthorized, "session_invalid")
			}()
			time.Sleep(250 * time.Millisecond)

			if err := env.expect(ctx, http.MethodDelete,
				"/api/v1/servers/"+plugin.Server.Server.ID+"/credentials", env.AdminToken,
				nil, http.StatusNoContent, nil); err != nil {
				return err
			}

			if err := <-failed; err != nil {
				return err
			}
			if elapsed := time.Since(started); elapsed > 5*time.Second {
				return fmt.Errorf("the held poll took %s to answer 401 after revocation; it must not be left to expire",
					elapsed)
			}
			return nil
		},
	},
	{
		ID:      "admin.envelopes.queue",
		Title:   "POST /api/v1/servers/{serverId}/envelopes validates what it queues",
		Section: "5.5",
		Run: func(ctx context.Context, env Env) error {
			created, err := env.newServer(ctx, "conformance: queue validation")
			if err != nil {
				return err
			}
			path := "/api/v1/servers/" + created.Server.ID + "/envelopes"

			if err := env.expectError(ctx, http.MethodPost, path, env.AdminToken,
				map[string]any{"body": map[string]any{}}, http.StatusBadRequest, "bad_request"); err != nil {
				return err
			}
			// The raw queue must not become a way around the endpoints that
			// validate hub-modelled types, or a way to forge a message the
			// plugin would take as the hub's own word.
			for _, reserved := range []string{"action.dispatch", "manifest.publish", "manifest.reject"} {
				if err := env.expectError(ctx, http.MethodPost, path, env.AdminToken,
					map[string]any{"type": reserved}, http.StatusConflict, "conflict"); err != nil {
					return err
				}
			}
			if err := env.expectError(ctx, http.MethodPost,
				"/api/v1/servers/conformance-no-such-server/envelopes", env.AdminToken,
				map[string]any{"type": unknownType()}, http.StatusNotFound, "not_found"); err != nil {
				return err
			}
			return env.expectError(ctx, http.MethodPost, path, "",
				map[string]any{"type": unknownType()}, http.StatusUnauthorized, "unauthorized")
		},
	},
	{
		ID:      "plugin.manifest.publish",
		Title:   "A published manifest is stored and readable through the Admin API",
		Section: "6",
		Run: func(ctx context.Context, env Env) error {
			plugin, err := env.newFakePlugin(ctx, "conformance: manifest publish", shortPollTimeoutSeconds)
			if err != nil {
				return err
			}

			response, err := plugin.publishManifest(ctx, manifestBody(1))
			if err != nil {
				return err
			}
			if response.Ack < 1 {
				return fmt.Errorf("ack = %d after manifest.publish, want the envelope acked", response.Ack)
			}

			record, err := env.storedManifest(ctx, plugin.Server.Server.ID)
			if err != nil {
				return err
			}
			if record.Revision != 1 {
				return fmt.Errorf("revision = %d, want the published 1", record.Revision)
			}
			if _, err := time.Parse(time.RFC3339, record.PublishedAt); err != nil {
				return fmt.Errorf("publishedAt %q is not RFC 3339: %w", record.PublishedAt, err)
			}
			var stored struct {
				ManifestRevision int64 `json:"manifestRevision"`
				Actions          []struct {
					Code   string `json:"code"`
					Danger string `json:"danger"`
				} `json:"actions"`
			}
			if err := json.Unmarshal(record.Manifest, &stored); err != nil {
				return fmt.Errorf("decode stored manifest %q: %w", truncate(record.Manifest), err)
			}
			if stored.ManifestRevision != 1 {
				return fmt.Errorf("the stored body carries manifestRevision %d, want it returned as published",
					stored.ManifestRevision)
			}
			if len(stored.Actions) != 1 || stored.Actions[0].Code != "example-mod.heal" {
				return fmt.Errorf("stored actions = %+v, want the published heal action", stored.Actions)
			}
			return nil
		},
	},
	{
		ID:      "plugin.manifest.republish",
		Title:   "A runtime republish replaces the manifest; lower and equal revisions are ignored",
		Section: "6.1",
		Run: func(ctx context.Context, env Env) error {
			plugin, err := env.newFakePlugin(ctx, "conformance: manifest republish", shortPollTimeoutSeconds)
			if err != nil {
				return err
			}
			if _, err := plugin.publishManifest(ctx, manifestBody(1)); err != nil {
				return err
			}

			// Same session, no reconnect: changing a mod's actions costs one
			// message, not a server restart.
			revised := manifestBody(2, map[string]any{
				"code": "example-mod.revive", "name": "Revive player",
				"context": "player", "namespace": "example-mod",
			})
			if _, err := plugin.publishManifest(ctx, revised); err != nil {
				return err
			}
			record, err := env.storedManifest(ctx, plugin.Server.Server.ID)
			if err != nil {
				return err
			}
			if record.Revision != 2 {
				return fmt.Errorf("revision = %d after a runtime republish, want 2", record.Revision)
			}

			// A lower revision is ignored, and its envelope still acked.
			lower, err := plugin.publishManifest(ctx, manifestBody(1))
			if err != nil {
				return err
			}
			if lower.Ack < 3 {
				return fmt.Errorf("ack = %d after an ignored revision, want the envelope acked anyway", lower.Ack)
			}
			// So is an equal one, whatever its content says.
			equal := manifestBody(2, map[string]any{"code": "example-mod.other"})
			ignored, err := plugin.publishManifest(ctx, equal)
			if err != nil {
				return err
			}
			// Ignoring a valid stale revision means exactly that: no
			// manifest.reject, which is the answer to an *invalid* manifest.
			for _, response := range []pollResponse{lower, ignored} {
				for _, delivered := range response.Envelopes {
					if delivered.Type == "manifest.reject" {
						return fmt.Errorf("a valid stale revision was answered with manifest.reject; section 6.1 says it is ignored")
					}
				}
			}

			record, err = env.storedManifest(ctx, plugin.Server.Server.ID)
			if err != nil {
				return err
			}
			if record.Revision != 2 {
				return fmt.Errorf("revision = %d after lower and equal republishes, want it held at 2",
					record.Revision)
			}
			var stored struct {
				Actions []struct {
					Code string `json:"code"`
				} `json:"actions"`
			}
			if err := json.Unmarshal(record.Manifest, &stored); err != nil {
				return fmt.Errorf("decode stored manifest: %w", err)
			}
			if len(stored.Actions) != 1 || stored.Actions[0].Code != "example-mod.revive" {
				return fmt.Errorf("stored actions = %+v, want the revision 2 set untouched", stored.Actions)
			}
			return nil
		},
	},
	{
		ID:      "plugin.manifest.invalid",
		Title:   "An invalid params schema is rejected without dropping the session",
		Section: "6.4",
		Run: func(ctx context.Context, env Env) error {
			plugin, err := env.newFakePlugin(ctx, "conformance: manifest invalid", shortPollTimeoutSeconds)
			if err != nil {
				return err
			}
			if _, err := plugin.publishManifest(ctx, manifestBody(1)); err != nil {
				return err
			}

			// `pattern` is a real JSON Schema keyword outside the subset. A hub
			// that accepted it would promise validation it cannot perform.
			invalid := manifestBody(2, map[string]any{
				"code": "example-mod.heal",
				"params": map[string]any{
					"type":       "object",
					"properties": map[string]any{"item": map[string]any{"type": "string", "pattern": "^a"}},
				},
			})
			published := plugin.nextOutbound("manifest.publish", invalid)
			response, err := plugin.pollAndAck(ctx, published)
			if err != nil {
				return fmt.Errorf("the poll carrying an invalid manifest failed; rejection must not fail the batch: %w", err)
			}
			if response.Ack < published.Seq {
				return fmt.Errorf("ack = %d, want %d: a rejected manifest is still durably processed and acked",
					response.Ack, published.Seq)
			}

			// The rejection must arrive as a manifest.reject envelope naming
			// the refused publish. A hub may answer it on the same poll or on
			// a later one; both are compliant.
			var rejects []envelope
			for _, delivered := range response.Envelopes {
				if delivered.Type == "manifest.reject" {
					rejects = append(rejects, delivered)
				}
			}
			if len(rejects) == 0 {
				if rejects, err = plugin.awaitEnvelope(ctx, "manifest.reject", 3); err != nil {
					return err
				}
			}
			if len(rejects) != 1 {
				return fmt.Errorf("got %d manifest.reject envelopes, want exactly 1", len(rejects))
			}
			var reject struct {
				EnvelopeID       string `json:"envelopeId"`
				ManifestRevision *int64 `json:"manifestRevision"`
				Errors           []struct {
					Path    string `json:"path"`
					Message string `json:"message"`
				} `json:"errors"`
			}
			if err := json.Unmarshal(rejects[0].Body, &reject); err != nil {
				return fmt.Errorf("decode manifest.reject body %q: %w", truncate(rejects[0].Body), err)
			}
			if reject.EnvelopeID != published.ID {
				return fmt.Errorf("manifest.reject names envelope %q, want the rejected %q",
					reject.EnvelopeID, published.ID)
			}
			if reject.ManifestRevision == nil || *reject.ManifestRevision != 2 {
				return fmt.Errorf("manifest.reject carries manifestRevision %v, want the readable 2",
					reject.ManifestRevision)
			}
			if len(reject.Errors) == 0 {
				return fmt.Errorf("manifest.reject carried no errors; it must name what was wrong")
			}
			for i, fault := range reject.Errors {
				if fault.Message == "" {
					return fmt.Errorf("manifest.reject error %d carries no message", i)
				}
			}

			// The stored manifest is untouched and the session still works.
			record, err := env.storedManifest(ctx, plugin.Server.Server.ID)
			if err != nil {
				return err
			}
			if record.Revision != 1 {
				return fmt.Errorf("revision = %d after a rejected publish, want the accepted 1 untouched",
					record.Revision)
			}
			if err := env.expect(ctx, http.MethodGet, "/plugin/v1/session", plugin.Session.SessionToken,
				nil, http.StatusOK, nil); err != nil {
				return fmt.Errorf("the session did not survive a rejected manifest: %w", err)
			}

			// The stored revision advances only on acceptance, so the corrected
			// manifest lands at the very revision that was rejected.
			if _, err := plugin.publishManifest(ctx, manifestBody(2)); err != nil {
				return err
			}
			record, err = env.storedManifest(ctx, plugin.Server.Server.ID)
			if err != nil {
				return err
			}
			if record.Revision != 2 {
				return fmt.Errorf("revision = %d after the corrected republish, want 2", record.Revision)
			}
			return nil
		},
	},
	{
		ID:      "admin.manifest.read",
		Title:   "GET /api/v1/servers/{serverId}/manifest answers 404 until a manifest is accepted",
		Section: "6.5",
		Run: func(ctx context.Context, env Env) error {
			created, err := env.newServer(ctx, "conformance: manifest read")
			if err != nil {
				return err
			}
			path := "/api/v1/servers/" + created.Server.ID + "/manifest"

			if err := env.expectError(ctx, http.MethodGet, path, env.AdminToken,
				nil, http.StatusNotFound, "not_found"); err != nil {
				return err
			}
			if err := env.expectError(ctx, http.MethodGet, "/api/v1/servers/conformance-no-such-server/manifest",
				env.AdminToken, nil, http.StatusNotFound, "not_found"); err != nil {
				return err
			}
			return env.expectError(ctx, http.MethodGet, path, "",
				nil, http.StatusUnauthorized, "unauthorized")
		},
	},
	{
		ID:      "action.lifecycle",
		Title:   "An action walks queued -> delivered -> running -> completed, observable at every step",
		Section: "7",
		Run: func(ctx context.Context, env Env) error {
			plugin, err := env.newFakePlugin(ctx, "conformance: action lifecycle", shortPollTimeoutSeconds)
			if err != nil {
				return err
			}
			if _, err := plugin.publishManifest(ctx, manifestBody(1)); err != nil {
				return err
			}

			actionID, state, err := env.dispatchAction(ctx, plugin.Server.Server.ID, map[string]any{
				"code":         "example-mod.heal",
				"context":      "player",
				"referenceKey": "player-1",
				"params":       map[string]any{"amount": 50},
			})
			if err != nil {
				return err
			}
			if state != "queued" {
				return fmt.Errorf("dispatch answered state %q, want queued", state)
			}

			// The dispatch arrives as an action.dispatch envelope carrying what
			// the plugin needs, including the deadline it must discard after.
			response, err := plugin.poll(ctx, pollRequest{Ack: plugin.inboundAck})
			if err != nil {
				return err
			}
			var dispatch *envelope
			for i := range response.Envelopes {
				if response.Envelopes[i].Type == "action.dispatch" {
					dispatch = &response.Envelopes[i]
				}
			}
			if dispatch == nil {
				return fmt.Errorf("no action.dispatch reached the plugin, got %+v", response.Envelopes)
			}
			var body struct {
				ActionID     string          `json:"actionId"`
				Code         string          `json:"code"`
				Context      string          `json:"context"`
				ReferenceKey string          `json:"referenceKey"`
				Params       json.RawMessage `json:"params"`
				ExpiresAt    string          `json:"expiresAt"`
			}
			if err := json.Unmarshal(dispatch.Body, &body); err != nil {
				return fmt.Errorf("decode dispatch body %q: %w", truncate(dispatch.Body), err)
			}
			if body.ActionID != actionID || body.Code != "example-mod.heal" ||
				body.Context != "player" || body.ReferenceKey != "player-1" {
				return fmt.Errorf("dispatch body %+v does not carry the dispatched fields", body)
			}
			if _, err := time.Parse(time.RFC3339, body.ExpiresAt); err != nil {
				return fmt.Errorf("dispatch expiresAt %q is not RFC 3339; a plugin cannot discard late work without a deadline", body.ExpiresAt)
			}
			var params struct {
				Amount int `json:"amount"`
			}
			if err := json.Unmarshal(body.Params, &params); err != nil || params.Amount != 50 {
				return fmt.Errorf("dispatch params = %q, want the dispatched payload", truncate(body.Params))
			}

			// The envelope ack is the delivery receipt.
			plugin.inboundAck = dispatch.Seq
			if _, err := plugin.pollAndAck(ctx); err != nil {
				return err
			}
			record, err := env.action(ctx, actionID)
			if err != nil {
				return err
			}
			if record.State != "delivered" || record.DeliveredAt == nil {
				return fmt.Errorf("state = %q after the envelope ack, want delivered with a timestamp",
					record.State)
			}

			// action.ack -> running.
			ackBody := map[string]any{"actionId": actionID}
			if _, err := plugin.send(ctx, plugin.nextOutbound("action.ack", ackBody)); err != nil {
				return err
			}
			record, err = env.action(ctx, actionID)
			if err != nil {
				return err
			}
			if record.State != "running" || record.RunningAt == nil {
				return fmt.Errorf("state = %q after action.ack, want running", record.State)
			}

			// action.result -> completed, payload readable back.
			resultBody := map[string]any{
				"actionId": actionID, "ok": true,
				"result": map[string]any{"healedTo": 100.0}, "durationMs": 12,
			}
			if _, err := plugin.send(ctx, plugin.nextOutbound("action.result", resultBody)); err != nil {
				return err
			}
			record, err = env.action(ctx, actionID)
			if err != nil {
				return err
			}
			if record.State != "completed" || record.OK == nil || !*record.OK || record.FinishedAt == nil {
				return fmt.Errorf("state = %q ok = %v, want completed true", record.State, record.OK)
			}
			var payload struct {
				HealedTo float64 `json:"healedTo"`
			}
			if err := json.Unmarshal(record.Result, &payload); err != nil || payload.HealedTo != 100 {
				return fmt.Errorf("result = %q, want the payload the plugin reported", truncate(record.Result))
			}

			// The failure path: ok false lands as failed, with the error kept.
			failedID, _, err := env.dispatchAction(ctx, plugin.Server.Server.ID, map[string]any{
				"code": "example-mod.heal", "params": map[string]any{"amount": 10},
			})
			if err != nil {
				return err
			}
			if _, err := plugin.pollAndAck(ctx); err != nil {
				return err
			}
			failure := map[string]any{"actionId": failedID, "ok": false, "error": "player is offline"}
			if _, err := plugin.send(ctx, plugin.nextOutbound("action.result", failure)); err != nil {
				return err
			}
			record, err = env.action(ctx, failedID)
			if err != nil {
				return err
			}
			if record.State != "failed" || record.Error == nil || *record.Error != "player is offline" {
				return fmt.Errorf("state = %q error = %v, want failed with the plugin's message",
					record.State, record.Error)
			}
			return nil
		},
	},
	{
		ID:      "action.dispatch.idempotent",
		Title:   "A dispatch retried with the same idempotencyKey returns the original actionId",
		Section: "7",
		Run: func(ctx context.Context, env Env) error {
			plugin, err := env.newFakePlugin(ctx, "conformance: action idempotency", shortPollTimeoutSeconds)
			if err != nil {
				return err
			}
			if _, err := plugin.publishManifest(ctx, manifestBody(1)); err != nil {
				return err
			}

			body := map[string]any{
				"code": "example-mod.heal", "params": map[string]any{"amount": 10},
				"idempotencyKey": "conformance-heal-once",
			}
			first, _, err := env.dispatchAction(ctx, plugin.Server.Server.ID, body)
			if err != nil {
				return err
			}
			second, _, err := env.dispatchAction(ctx, plugin.Server.Server.ID, body)
			if err != nil {
				return err
			}
			if second != first {
				return fmt.Errorf("the retry returned actionId %q, want the original %q", second, first)
			}

			response, err := plugin.pollAndAck(ctx)
			if err != nil {
				return err
			}
			dispatches := 0
			for _, delivered := range response.Envelopes {
				if delivered.Type == "action.dispatch" {
					dispatches++
				}
			}
			if dispatches != 1 {
				return fmt.Errorf("the plugin received %d action.dispatch envelopes, want exactly 1", dispatches)
			}
			return nil
		},
	},
	{
		ID:      "action.dispatch.invalid",
		Title:   "A schema-invalid or undeclared dispatch is refused and never queued",
		Section: "7",
		Run: func(ctx context.Context, env Env) error {
			plugin, err := env.newFakePlugin(ctx, "conformance: invalid dispatch", shortPollTimeoutSeconds)
			if err != nil {
				return err
			}
			if _, err := plugin.publishManifest(ctx, manifestBody(1)); err != nil {
				return err
			}
			path := "/api/v1/servers/" + plugin.Server.Server.ID + "/actions"

			// amount is bounded [1, 100] by the published schema.
			if err := env.expectError(ctx, http.MethodPost, path, env.AdminToken, map[string]any{
				"code": "example-mod.heal", "params": map[string]any{"amount": 500},
			}, http.StatusBadRequest, "params_invalid"); err != nil {
				return err
			}
			// amount is required.
			if err := env.expectError(ctx, http.MethodPost, path, env.AdminToken, map[string]any{
				"code": "example-mod.heal", "params": map[string]any{},
			}, http.StatusBadRequest, "params_invalid"); err != nil {
				return err
			}
			// A code the manifest does not declare cannot be validated, so it
			// cannot be dispatched.
			if err := env.expectError(ctx, http.MethodPost, path, env.AdminToken, map[string]any{
				"code": "example-mod.undeclared", "params": map[string]any{},
			}, http.StatusConflict, "unknown_action"); err != nil {
				return err
			}
			if err := env.expectError(ctx, http.MethodPost, path, env.AdminToken,
				map[string]any{"params": map[string]any{}}, http.StatusBadRequest, "bad_request"); err != nil {
				return err
			}
			if err := env.expectError(ctx, http.MethodPost,
				"/api/v1/servers/conformance-no-such-server/actions", env.AdminToken,
				map[string]any{"code": "example-mod.heal"}, http.StatusNotFound, "not_found"); err != nil {
				return err
			}

			// None of it reached the queue: a poll comes back with nothing but
			// the nudge it was primed with.
			if _, err := env.queueEnvelope(ctx, plugin.Server.Server.ID, unknownType(), nil); err != nil {
				return err
			}
			response, err := plugin.pollAndAck(ctx)
			if err != nil {
				return err
			}
			for _, delivered := range response.Envelopes {
				if delivered.Type == "action.dispatch" {
					return fmt.Errorf("a refused dispatch was queued anyway; schema-invalid input must never reach the game server")
				}
			}
			return nil
		},
	},
	{
		ID:      "action.expiry",
		Title:   "An undeliverable action expires at its TTL, and a late result cannot resurrect it",
		Section: "7",
		Run: func(ctx context.Context, env Env) error {
			plugin, err := env.newFakePlugin(ctx, "conformance: action expiry", shortPollTimeoutSeconds)
			if err != nil {
				return err
			}
			if _, err := plugin.publishManifest(ctx, manifestBody(1)); err != nil {
				return err
			}

			// Dispatched with the shortest TTL, and never polled for: the game
			// server is down, which is exactly when TTLs matter.
			actionID, _, err := env.dispatchAction(ctx, plugin.Server.Server.ID, map[string]any{
				"code": "example-mod.heal", "params": map[string]any{"amount": 10},
				"ttlSeconds": 1,
			})
			if err != nil {
				return err
			}
			record, err := env.awaitActionState(ctx, actionID, "expired", 5*time.Second)
			if err != nil {
				return err
			}
			if record.FinishedAt == nil {
				return fmt.Errorf("an expired action carries no finishedAt")
			}

			// The plugin comes back and reports a result anyway. The envelope
			// is acked like any other, and the state does not move: the
			// operator already saw expired.
			late := map[string]any{"actionId": actionID, "ok": true, "result": map[string]any{}}
			response, err := plugin.send(ctx, plugin.nextOutbound("action.result", late))
			if err != nil {
				return err
			}
			if response.Ack < 1 {
				return fmt.Errorf("ack = %d after a late result, want the envelope acked", response.Ack)
			}
			record, err = env.action(ctx, actionID)
			if err != nil {
				return err
			}
			if record.State != "expired" {
				return fmt.Errorf("state = %q after a late result, want it held at expired", record.State)
			}
			return nil
		},
	},
	{
		ID:      "compat.unknownFields",
		Title:   "Unknown request fields are tolerated, not rejected",
		Section: "2.1",
		Run: func(ctx context.Context, env Env) error {
			var created createdServer
			if err := env.expect(ctx, http.MethodPost, "/api/v1/servers", env.AdminToken, map[string]any{
				"name":                 "conformance: unknown fields",
				"game":                 fixtureGame,
				"fieldFromALaterDraft": map[string]any{"nested": true},
			}, http.StatusCreated, &created); err != nil {
				return err
			}

			var enrolled credentials
			return env.expect(ctx, http.MethodPost, "/plugin/v1/enroll", "", map[string]any{
				"enrollmentToken":      created.Enrollment.Token,
				"game":                 fixtureGame,
				"fieldFromALaterDraft": []int{1, 2, 3},
			}, http.StatusCreated, &enrolled)
		},
	},
}

func truncate(body []byte) string {
	const limit = 200
	if len(body) <= limit {
		return string(body)
	}
	return string(body[:limit]) + "..."
}
