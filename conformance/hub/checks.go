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

	resp, err := e.Client.Do(req)
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

// expectError asserts a failed request answers with the protocol error envelope
// and the expected code (spec section 2.2). wantCode may be empty to accept any
// code, which is how checks assert the shape without over-fitting the code.
func (e Env) expectError(ctx context.Context, method, path, bearer string, body any, wantStatus int, wantCode string) error {
	resp, responseBody, err := e.do(ctx, method, path, bearer, body)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	if resp.StatusCode != wantStatus {
		return fmt.Errorf("%s %s: want status %d, got %d, body %q",
			method, path, wantStatus, resp.StatusCode, truncate(responseBody))
	}

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
	ID              string  `json:"id"`
	Name            string  `json:"name"`
	Game            string  `json:"game"`
	CreatedAt       string  `json:"createdAt"`
	CredentialState string  `json:"credentialState"`
	LastSeenAt      *string `json:"lastSeenAt"`
	Session         *struct {
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
