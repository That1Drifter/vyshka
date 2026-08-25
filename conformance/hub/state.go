package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Fixtures for state snapshots (spec section 8.3). Like every other shape in
// this suite they are hand-written rather than shared with the hub.

// stateRecord mirrors the Admin API snapshot view on the wire.
type stateRecord struct {
	Type       string          `json:"type"`
	CapturedAt string          `json:"capturedAt"`
	ReceivedAt string          `json:"receivedAt"`
	Snapshot   json.RawMessage `json:"snapshot"`
}

// latestState reads the latest snapshot of one type through the Admin API.
func (e Env) latestState(ctx context.Context, serverID, stateType string) (stateRecord, error) {
	var record stateRecord
	err := e.expect(ctx, http.MethodGet,
		"/api/v1/servers/"+serverID+"/state/"+stateType, e.AdminToken, nil, http.StatusOK, &record)
	return record, err
}

func checkStateLatestReplaces(ctx context.Context, env Env) error {
	plugin, err := env.newFakePlugin(ctx, "conformance: state latest", 5)
	if err != nil {
		return err
	}
	serverID := plugin.Creds.ServerID

	// Before any snapshot: a real server, an empty answer, and the difference
	// between "no snapshot" and "unknown type" observable.
	if err := env.expectError(ctx, http.MethodGet,
		"/api/v1/servers/"+serverID+"/state/players", env.AdminToken,
		nil, http.StatusNotFound, "not_found"); err != nil {
		return fmt.Errorf("state before any snapshot: %w", err)
	}

	first := map[string]any{
		"capturedAt": time.Now().UTC().Add(-time.Minute).Format(time.RFC3339),
		"players": []map[string]any{{
			"player":   map[string]any{"platform": "steam", "id": "76561198000000042"},
			"name":     "Conformance Survivor",
			"position": []float64{4231.5, 300.25, 10620},
			"data":     map[string]any{"health": 82},
		}},
	}
	if _, err := plugin.send(ctx, plugin.nextOutbound("state.players", first)); err != nil {
		return err
	}

	record, err := env.latestState(ctx, serverID, "players")
	if err != nil {
		return err
	}
	if record.Type != "players" {
		return fmt.Errorf("latest type = %q, want players", record.Type)
	}
	for _, stamp := range []struct{ name, value string }{
		{"capturedAt", record.CapturedAt}, {"receivedAt", record.ReceivedAt},
	} {
		if _, err := time.Parse(time.RFC3339, stamp.value); err != nil {
			return fmt.Errorf("%s %q is not RFC 3339: %w", stamp.name, stamp.value, err)
		}
	}
	var stored struct {
		Players []struct {
			Player struct {
				Platform string `json:"platform"`
				ID       string `json:"id"`
			} `json:"player"`
		} `json:"players"`
	}
	if err := json.Unmarshal(record.Snapshot, &stored); err != nil {
		return fmt.Errorf("snapshot %q does not decode: %w", truncate(record.Snapshot), err)
	}
	if len(stored.Players) != 1 || stored.Players[0].Player.ID != "76561198000000042" {
		return fmt.Errorf("snapshot = %s, want the pushed list verbatim", truncate(record.Snapshot))
	}

	// A newer snapshot replaces the older entirely: an empty list is a
	// meaningful answer, not a missing one.
	if _, err := plugin.send(ctx, plugin.nextOutbound("state.players",
		map[string]any{"players": []any{}})); err != nil {
		return err
	}
	record, err = env.latestState(ctx, serverID, "players")
	if err != nil {
		return err
	}
	var emptied struct {
		Players *[]any `json:"players"`
	}
	if err := json.Unmarshal(record.Snapshot, &emptied); err != nil {
		return fmt.Errorf("empty snapshot does not decode: %w", err)
	}
	if emptied.Players == nil || len(*emptied.Players) != 0 {
		return fmt.Errorf("after the empty push the latest snapshot is %s, want an empty list",
			truncate(record.Snapshot))
	}

	// History: newest first, both snapshots present, latest included first.
	var history struct {
		Snapshots []stateRecord `json:"snapshots"`
	}
	if err := env.expect(ctx, http.MethodGet,
		"/api/v1/servers/"+serverID+"/state/players/history", env.AdminToken,
		nil, http.StatusOK, &history); err != nil {
		return err
	}
	if len(history.Snapshots) != 2 {
		return fmt.Errorf("history has %d snapshots, want 2", len(history.Snapshots))
	}
	if err := json.Unmarshal(history.Snapshots[0].Snapshot, &emptied); err != nil ||
		emptied.Players == nil || len(*emptied.Players) != 0 {
		return fmt.Errorf("history[0] = %s, want the newest (empty) snapshot first",
			truncate(history.Snapshots[0].Snapshot))
	}

	// Types are independent: nothing above touched vehicles.
	return env.expectError(ctx, http.MethodGet,
		"/api/v1/servers/"+serverID+"/state/vehicles", env.AdminToken,
		nil, http.StatusNotFound, "not_found")
}

func checkStateRejectWhole(ctx context.Context, env Env) error {
	plugin, err := env.newFakePlugin(ctx, "conformance: state reject", 5)
	if err != nil {
		return err
	}
	serverID := plugin.Creds.ServerID

	// One good entry cannot carry an identity-free one: refused whole, acked,
	// and narrated with a state.reject naming the envelope.
	bad := plugin.nextOutbound("state.players", map[string]any{
		"players": []map[string]any{
			{"player": map[string]any{"platform": "steam", "id": "76561198000000042"}},
			{"name": "identity-free"},
		}})
	response, err := plugin.send(ctx, bad)
	if err != nil {
		return err
	}
	if response.Ack != bad.Seq {
		return fmt.Errorf("ack = %d, want %d: a rejection is envelope-level success",
			response.Ack, bad.Seq)
	}
	if err := env.expectError(ctx, http.MethodGet,
		"/api/v1/servers/"+serverID+"/state/players", env.AdminToken,
		nil, http.StatusNotFound, "not_found"); err != nil {
		return fmt.Errorf("a rejected snapshot left state behind: %w", err)
	}

	// The notice may ride the same poll's response, in which case send has
	// already acked it and no retransmission is coming; look there first.
	var rejects []envelope
	for _, delivered := range response.Envelopes {
		if delivered.Type == "state.reject" {
			rejects = append(rejects, delivered)
		}
	}
	if len(rejects) == 0 {
		if rejects, err = plugin.awaitEnvelope(ctx, "state.reject", 4); err != nil {
			return err
		}
	}
	var notice struct {
		EnvelopeID string `json:"envelopeId"`
		Errors     []struct {
			Path    string `json:"path"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(rejects[0].Body, &notice); err != nil {
		return fmt.Errorf("state.reject body %q does not decode: %w", truncate(rejects[0].Body), err)
	}
	if notice.EnvelopeID != bad.ID {
		return fmt.Errorf("state.reject names envelope %q, want %q", notice.EnvelopeID, bad.ID)
	}
	if len(notice.Errors) == 0 {
		return fmt.Errorf("state.reject carries no errors")
	}

	// A valid snapshot on the same session still lands afterwards.
	if _, err := plugin.send(ctx, plugin.nextOutbound("state.players",
		map[string]any{"players": []any{}})); err != nil {
		return err
	}
	if _, err := env.latestState(ctx, serverID, "players"); err != nil {
		return fmt.Errorf("a valid snapshot after a rejection did not land: %w", err)
	}
	return nil
}

func checkStateGuards(ctx context.Context, env Env) error {
	created, err := env.newServer(ctx, "conformance: state guards")
	if err != nil {
		return err
	}
	serverID := created.Server.ID

	// An unknown state type is a bad request, never an empty answer.
	if err := env.expectError(ctx, http.MethodGet,
		"/api/v1/servers/"+serverID+"/state/weather", env.AdminToken,
		nil, http.StatusBadRequest, "bad_request"); err != nil {
		return err
	}
	// The raw envelope queue refuses the state family: a forged state.reject
	// must not be queueable as the hub's own word.
	if err := env.expectError(ctx, http.MethodPost,
		"/api/v1/servers/"+serverID+"/envelopes", env.AdminToken,
		map[string]any{"type": "state.reject"}, http.StatusConflict, "conflict"); err != nil {
		return err
	}

	// servers:read is the gate.
	reader, err := env.mintToken(ctx, "conformance: state reader", "servers:read")
	if err != nil {
		return err
	}
	if err := env.expectError(ctx, http.MethodGet,
		"/api/v1/servers/"+serverID+"/state/players", reader.Secret,
		nil, http.StatusNotFound, "not_found"); err != nil {
		return fmt.Errorf("servers:read reading empty state: %w", err)
	}
	outsider, err := env.mintToken(ctx, "conformance: state outsider", "events:read")
	if err != nil {
		return err
	}
	return env.refused(ctx, http.MethodGet,
		"/api/v1/servers/"+serverID+"/state/players", outsider.Secret, nil,
		"a token without servers:read read state")
}
