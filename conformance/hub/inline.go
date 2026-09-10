package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Inline errors (spec section 2.3): a Plugin API request carrying
// ?errors=inline gets its refusals as a 200 whose body is the section 2.2
// error with error.status added, because some engine HTTP clients hand script
// nothing but an opaque code for a non-2xx response.

const inlineQuery = "?errors=inline"

type inlineFailure struct {
	Error struct {
		Code    string         `json:"code"`
		Status  int            `json:"status"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

// expectInline issues a request that asked for inline errors and asserts the
// refusal came back as a 200 carrying the expected code and the status the
// hub would otherwise have used.
func (e Env) expectInline(ctx context.Context, client *http.Client, method, path, bearer string, body any, wantCode string, wantStatus int) (inlineFailure, error) {
	var failure inlineFailure
	resp, responseBody, err := e.doWith(client, ctx, method, path, bearer, body)
	if err != nil {
		return failure, fmt.Errorf("%s %s: %w", method, path, err)
	}
	if resp.StatusCode != http.StatusOK {
		return failure, fmt.Errorf("%s %s: want status 200 for an inline error, got %d, body %q",
			method, path, resp.StatusCode, truncate(responseBody))
	}
	if contentType := resp.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		return failure, fmt.Errorf("%s %s: inline error Content-Type = %q, want application/json", method, path, contentType)
	}
	if err := json.Unmarshal(responseBody, &failure); err != nil {
		return failure, fmt.Errorf("%s %s: inline error body %q is not JSON: %w", method, path, truncate(responseBody), err)
	}
	if failure.Error.Code != wantCode {
		return failure, fmt.Errorf("%s %s: want error.code %q, got %q (body %q)",
			method, path, wantCode, failure.Error.Code, truncate(responseBody))
	}
	if failure.Error.Status != wantStatus {
		return failure, fmt.Errorf("%s %s: want error.status %d, got %d; the status the hub would have sent is required inline (body %q)",
			method, path, wantStatus, failure.Error.Status, truncate(responseBody))
	}
	if strings.TrimSpace(failure.Error.Message) == "" {
		return failure, fmt.Errorf("%s %s: inline error carries no message", method, path)
	}
	return failure, nil
}

// expectNoErrorMember asserts a success body has no top-level error member,
// which is what lets an opted-in plugin tell a success from a refusal.
func expectNoErrorMember(method, path string, responseBody []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(responseBody, &raw); err != nil {
		return fmt.Errorf("%s %s: success body %q is not a JSON object: %w", method, path, truncate(responseBody), err)
	}
	if _, present := raw["error"]; present {
		return fmt.Errorf("%s %s: a success body carried a top-level error member: %q", method, path, truncate(responseBody))
	}
	return nil
}

func init() {
	checks = append(checks, inlineChecks...)
}

var inlineChecks = []Check{
	{
		ID:      "plugin.errors.inlineRefusals",
		Title:   "Every Plugin API refusal arrives as a 200 with error.status when asked for",
		Section: "2.3",
		Run: func(ctx context.Context, env Env) error {
			// Enrollment: the request no credential can cover.
			if _, err := env.expectInline(ctx, env.Client, http.MethodPost, "/plugin/v1/enroll"+inlineQuery, "",
				map[string]any{"enrollmentToken": "conformance-not-a-token", "game": fixtureGame},
				"enrollment_token_invalid", http.StatusUnauthorized); err != nil {
				return err
			}
			// A session request that fails.
			if _, err := env.expectInline(ctx, env.Client, http.MethodPost, "/plugin/v1/session"+inlineQuery, "",
				map[string]any{"serverId": "conformance-no-such-server", "serverSecret": "conformance-no-such-secret"},
				"credentials_invalid", http.StatusUnauthorized); err != nil {
				return err
			}
			// A session token the hub does not know.
			if _, err := env.expectInline(ctx, env.PollClient, http.MethodPost, "/plugin/v1/poll"+inlineQuery, "conformance-no-such-session",
				map[string]any{}, "session_invalid", http.StatusUnauthorized); err != nil {
				return err
			}

			// A refusal with details: the offending envelope is still named.
			plugin, err := env.newFakePlugin(ctx, "conformance: inline refusals", shortPollTimeoutSeconds)
			if err != nil {
				return err
			}
			failure, err := env.expectInline(ctx, env.PollClient, http.MethodPost, "/plugin/v1/poll"+inlineQuery, plugin.Session.SessionToken,
				map[string]any{"envelopes": []map[string]any{
					{"v": 1, "id": "conformance-inline-ok", "type": "event.batch", "seq": 1, "ts": "2026-09-10T00:00:00Z", "body": map[string]any{}},
					{"v": 1, "type": "event.batch", "seq": 2, "ts": "2026-09-10T00:00:00Z", "body": map[string]any{}},
				}}, "envelope_invalid", http.StatusBadRequest)
			if err != nil {
				return err
			}
			if index, ok := failure.Error.Details["index"].(float64); !ok || index != 1 {
				return fmt.Errorf("inline envelope_invalid carried details.index %v, want 1; details travel unchanged inline", failure.Error.Details["index"])
			}
			if _, err := env.expectInline(ctx, env.PollClient, http.MethodPost, "/plugin/v1/poll"+inlineQuery, plugin.Session.SessionToken,
				map[string]any{"ack": 50}, "ack_out_of_range", http.StatusBadRequest); err != nil {
				return err
			}

			// Revoked credentials, on the session request the plugin retries.
			if err := env.expect(ctx, http.MethodDelete,
				"/api/v1/servers/"+plugin.Server.Server.ID+"/credentials", env.AdminToken,
				nil, http.StatusNoContent, nil); err != nil {
				return err
			}
			_, err = env.expectInline(ctx, env.Client, http.MethodPost, "/plugin/v1/session"+inlineQuery, "",
				map[string]any{"serverId": plugin.Creds.ServerID, "serverSecret": plugin.Creds.ServerSecret},
				"credentials_revoked", http.StatusUnauthorized)
			return err
		},
	},
	{
		ID:      "plugin.errors.inlineSuccess",
		Title:   "Opting in leaves successes untouched and is advertised in features",
		Section: "2.3",
		Run: func(ctx context.Context, env Env) error {
			created, err := env.newServer(ctx, "conformance: inline success")
			if err != nil {
				return err
			}
			resp, body, err := env.do(ctx, http.MethodPost, "/plugin/v1/enroll"+inlineQuery, "", map[string]any{
				"enrollmentToken": created.Enrollment.Token,
				"game":            fixtureGame,
			})
			if err != nil {
				return fmt.Errorf("POST /plugin/v1/enroll: %w", err)
			}
			if resp.StatusCode != http.StatusCreated {
				return fmt.Errorf("enroll with ?errors=inline: want 201, got %d, body %q; only refusals change status", resp.StatusCode, truncate(body))
			}
			if err := expectNoErrorMember(http.MethodPost, "/plugin/v1/enroll", body); err != nil {
				return err
			}
			var creds credentials
			if err := json.Unmarshal(body, &creds); err != nil || creds.ServerSecret == "" {
				return fmt.Errorf("enroll with ?errors=inline: body %q carried no credentials", truncate(body))
			}

			resp, body, err = env.do(ctx, http.MethodPost, "/plugin/v1/session"+inlineQuery, "", map[string]any{
				"serverId":           creds.ServerID,
				"serverSecret":       creds.ServerSecret,
				"pollTimeoutSeconds": shortPollTimeoutSeconds,
			})
			if err != nil {
				return fmt.Errorf("POST /plugin/v1/session: %w", err)
			}
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("session with ?errors=inline: want 200, got %d, body %q", resp.StatusCode, truncate(body))
			}
			if err := expectNoErrorMember(http.MethodPost, "/plugin/v1/session", body); err != nil {
				return err
			}
			var session sessionRecord
			if err := json.Unmarshal(body, &session); err != nil || session.SessionToken == "" {
				return fmt.Errorf("session with ?errors=inline: body %q carried no token", truncate(body))
			}
			if flag, _ := session.Features["inlineErrors"].(bool); !flag {
				return fmt.Errorf("features = %v; a hub implementing inline errors reports inlineErrors: true", session.Features)
			}

			resp, body, err = env.doWith(env.PollClient, ctx, http.MethodPost, "/plugin/v1/poll"+inlineQuery, session.SessionToken, map[string]any{})
			if err != nil {
				return fmt.Errorf("POST /plugin/v1/poll: %w", err)
			}
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("idle poll with ?errors=inline: want 200, got %d, body %q", resp.StatusCode, truncate(body))
			}
			if err := expectNoErrorMember(http.MethodPost, "/plugin/v1/poll", body); err != nil {
				return err
			}
			var response pollResponse
			if err := json.Unmarshal(body, &response); err != nil || response.Envelopes == nil {
				return fmt.Errorf("idle poll with ?errors=inline: body %q is not a poll response", truncate(body))
			}
			return nil
		},
	},
	{
		ID:      "plugin.errors.unknownMode",
		Title:   "An unknown errors mode is refused in ordinary form, and the Admin API ignores the parameter",
		Section: "2.3",
		Run: func(ctx context.Context, env Env) error {
			for _, query := range []string{"?errors=loud", "?errors=inline&errors=inline"} {
				if err := env.expectError(ctx, http.MethodPost, "/plugin/v1/poll"+query, "conformance-no-such-session",
					map[string]any{}, http.StatusBadRequest, "bad_request"); err != nil {
					return fmt.Errorf("%w; a mode the hub does not offer is refused with an ordinary 400 bad_request", err)
				}
			}
			if err := env.expectError(ctx, http.MethodGet, "/api/v1/servers"+inlineQuery, "conformance-not-an-admin-token",
				nil, http.StatusUnauthorized, ""); err != nil {
				return fmt.Errorf("%w; the Admin API always answers with ordinary statuses", err)
			}
			return env.expect(ctx, http.MethodGet, "/api/v1/servers"+inlineQuery, env.AdminToken, nil, http.StatusOK, nil)
		},
	},
	{
		ID:      "plugin.errors.inlineSupersededHold",
		Title:   "A held poll that opted in is answered inline the moment its session is superseded",
		Section: "2.3",
		Run: func(ctx context.Context, env Env) error {
			plugin, err := env.newFakePlugin(ctx, "conformance: inline superseded mid-hold", 25)
			if err != nil {
				return err
			}
			stale := plugin.Session.SessionToken

			failed := make(chan error, 1)
			started := time.Now()
			go func() {
				_, err := env.expectInline(ctx, env.PollClient, http.MethodPost, "/plugin/v1/poll"+inlineQuery, stale,
					map[string]any{}, "session_invalid", http.StatusUnauthorized)
				failed <- err
			}()
			time.Sleep(250 * time.Millisecond)

			if err := plugin.reconnect(ctx, shortPollTimeoutSeconds); err != nil {
				return err
			}
			if err := <-failed; err != nil {
				return err
			}
			if elapsed := time.Since(started); elapsed > 5*time.Second {
				return fmt.Errorf("the superseded poll took %s to answer inline; a held poll must not outlive its session however its refusal travels", elapsed)
			}
			return nil
		},
	},
}
