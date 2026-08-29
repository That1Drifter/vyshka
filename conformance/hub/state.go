package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
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

	// The first snapshot deliberately stresses three tolerance rules at once:
	// a body-level unknown field and an unknown list-shaped field must both
	// survive verbatim (section 2.1), and a player id at the 128-code-point
	// cap must be accepted, pinning the boundary from the accepting side.
	longID := "76561198000000042"
	for len(longID) < 128 {
		longID += "x"
	}
	first := map[string]any{
		"capturedAt": time.Now().UTC().Add(-time.Minute).Format(time.RFC3339),
		"players": []map[string]any{{
			"player":   map[string]any{"platform": "steam", "id": "76561198000000042"},
			"name":     "Conformance Survivor",
			"position": []float64{4231.5, 300.25, 10620},
			"data":     map[string]any{"health": 82},
		}, {
			"player": map[string]any{"platform": "conformance-platform", "id": longID},
		}},
		"x-mod-extra": map[string]any{"weather": "fog"},
		"vehicles":    map[string]any{"formatFromFutureDraft": 2},
	}
	if _, err := plugin.send(ctx, plugin.nextOutbound("state.players", first)); err != nil {
		return err
	}

	record, err := env.latestState(ctx, serverID, "players")
	if err != nil {
		return fmt.Errorf("the snapshot with unknown fields was not accepted: %w", err)
	}
	if record.Type != "players" {
		return fmt.Errorf("latest type = %q, want players", record.Type)
	}
	firstCapturedAt := record.CapturedAt
	for _, stamp := range []struct{ name, value string }{
		{"capturedAt", record.CapturedAt}, {"receivedAt", record.ReceivedAt},
	} {
		if _, err := time.Parse(time.RFC3339, stamp.value); err != nil {
			return fmt.Errorf("%s %q is not RFC 3339: %w", stamp.name, stamp.value, err)
		}
	}
	// Verbatim means verbatim: the stored body must equal what was sent, the
	// unknown fields included, not a rewrite that kept the fields this suite
	// happens to check.
	var sentValue, storedValue any
	sentEncoded, _ := json.Marshal(first)
	if err := json.Unmarshal(sentEncoded, &sentValue); err != nil {
		return err
	}
	if err := json.Unmarshal(record.Snapshot, &storedValue); err != nil {
		return fmt.Errorf("snapshot %q does not decode: %w", truncate(record.Snapshot), err)
	}
	if !reflect.DeepEqual(sentValue, storedValue) {
		return fmt.Errorf("the stored snapshot is not the sent body verbatim: got %s",
			truncate(record.Snapshot))
	}

	// A newer snapshot replaces the older entirely, and "newer" means newer
	// accepted: this one carries an older capturedAt, and a hub ordering by
	// the game's clock instead of acceptance order would keep the wrong one.
	// An empty list is a meaningful answer, not a missing one.
	if _, err := plugin.send(ctx, plugin.nextOutbound("state.players", map[string]any{
		"capturedAt": time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339),
		"players":    []any{},
	})); err != nil {
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
		return fmt.Errorf("after the empty push the latest snapshot is %s, want an empty list; "+
			"latest is the last accepted snapshot, never the one with the freshest capturedAt",
			truncate(record.Snapshot))
	}
	if record.CapturedAt == firstCapturedAt {
		return fmt.Errorf("the latest snapshot still reports the first capturedAt %q", firstCapturedAt)
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
	// The raw envelope queue refuses the whole state family: a forged
	// state.reject must not be queueable as the hub's own word, and a forged
	// snapshot must not route around the validation ingest performs.
	for _, forged := range []string{"state.reject", "state.players", "state.something-new"} {
		if err := env.expectError(ctx, http.MethodPost,
			"/api/v1/servers/"+serverID+"/envelopes", env.AdminToken,
			map[string]any{"type": forged}, http.StatusConflict, "conflict"); err != nil {
			return fmt.Errorf("queueing %s: %w", forged, err)
		}
	}

	// servers:read is the gate, on the latest read and the history read alike.
	reader, err := env.mintToken(ctx, "conformance: state reader", "servers:read")
	if err != nil {
		return err
	}
	if err := env.expectError(ctx, http.MethodGet,
		"/api/v1/servers/"+serverID+"/state/players", reader.Secret,
		nil, http.StatusNotFound, "not_found"); err != nil {
		return fmt.Errorf("servers:read reading empty state: %w", err)
	}
	if err := env.expect(ctx, http.MethodGet,
		"/api/v1/servers/"+serverID+"/state/players/history", reader.Secret,
		nil, http.StatusOK, nil); err != nil {
		return fmt.Errorf("servers:read reading history: %w", err)
	}
	outsider, err := env.mintToken(ctx, "conformance: state outsider", "events:read")
	if err != nil {
		return err
	}
	if err := env.refused(ctx, http.MethodGet,
		"/api/v1/servers/"+serverID+"/state/players", outsider.Secret, nil,
		"a token without servers:read read state"); err != nil {
		return err
	}
	return env.refused(ctx, http.MethodGet,
		"/api/v1/servers/"+serverID+"/state/players/history", outsider.Secret, nil,
		"a token without servers:read read state history")
}

// checkStateReplayAfterPrune drives the harder replay: a superseded snapshot
// whose history row is already gone. History is bounded by depth as well as
// time (section 8.3), so pushing one snapshot more than the configured depth
// forces the oldest out while the latest survives; the dedup obligation must
// outlive the row, or the replay inserts with a fresh acceptance seq and
// "latest" regresses to a state that was already superseded. The check needs
// the hub's configured depth to force the trim, so it takes it from the
// runner (-state-history-depth, reference 500); against a hub configured
// deeper, the trim never happens and the check degrades to the plain
// cross-session dedup that state.retransmitDedup already grades.
func checkStateReplayAfterPrune(ctx context.Context, env Env) error {
	depth := env.StateHistoryDepth
	if depth <= 0 {
		depth = 500
	}
	plugin, err := env.newFakePlugin(ctx, "conformance: state replay after prune", shortPollTimeoutSeconds)
	if err != nil {
		return err
	}
	serverID := plugin.Creds.ServerID

	// The victim first, then depth fillers, so the victim is the one row the
	// trim removes. The last filler is marked: it is what latest must still
	// answer after the replay.
	victim := plugin.nextOutbound("state.players", map[string]any{
		"players": []map[string]any{{
			"player": map[string]any{"platform": "conformance-platform", "id": "victim"},
			"name":   "replay-victim",
		}}})
	batch := []envelope{victim}
	for i := 1; i <= depth; i++ {
		body := map[string]any{"players": []any{}}
		if i == depth {
			body = map[string]any{
				"players": []map[string]any{{
					"player": map[string]any{"platform": "conformance-platform", "id": "final"},
					"name":   "final-state",
				}}}
		}
		batch = append(batch, plugin.nextOutbound("state.players", body))
	}

	// Chunks of the 200-envelope floor every hub must accept (section 3.1.2),
	// each poll nudged with queued work so none of them is held.
	for start := 0; start < len(batch); start += 200 {
		if _, err := env.queueEnvelope(ctx, serverID, unknownType(), nil); err != nil {
			return err
		}
		response, err := plugin.pollAndAck(ctx, batch[start:min(start+200, len(batch))]...)
		if err != nil {
			return err
		}
		if want := batch[min(start+200, len(batch))-1].Seq; response.Ack != want {
			return fmt.Errorf("ack = %d after a filler chunk, want %d", response.Ack, want)
		}
	}
	record, err := env.latestState(ctx, serverID, "players")
	if err != nil {
		return err
	}
	if !bytes.Contains(record.Snapshot, []byte("final-state")) {
		return fmt.Errorf("latest = %s before the replay, want the final filler", truncate(record.Snapshot))
	}

	// The game server restarts before the victim's ack reached the plugin (it
	// did, but a lost response is indistinguishable to the plugin): the buffer
	// is replayed on the next session, renumbered, everything else unchanged.
	if err := plugin.reconnect(ctx, shortPollTimeoutSeconds); err != nil {
		return err
	}
	if _, err := env.queueEnvelope(ctx, serverID, unknownType(), nil); err != nil {
		return err
	}
	replayed := plugin.renumber(victim)
	response, err := plugin.send(ctx, replayed)
	if err != nil {
		return err
	}
	if response.Ack != replayed.Seq {
		return fmt.Errorf("ack = %d after the replay, want %d: a duplicate is acked, applied no further",
			response.Ack, replayed.Seq)
	}

	record, err = env.latestState(ctx, serverID, "players")
	if err != nil {
		return err
	}
	if bytes.Contains(record.Snapshot, []byte("replay-victim")) {
		return fmt.Errorf("latest regressed to the replayed snapshot %s; the dedup obligation must outlive the pruned history row",
			truncate(record.Snapshot))
	}
	if !bytes.Contains(record.Snapshot, []byte("final-state")) {
		return fmt.Errorf("latest = %s after the replay, want the final filler still standing",
			truncate(record.Snapshot))
	}
	return nil
}

// checkStateRetransmitDedup drives the one case section 14 singles out: a
// session change with a snapshot the plugin believes unacked. The replay is
// renumbered into the new session (section 9.1), so only the envelope id can
// reveal it was already stored; a hub that stores it again puts the same
// snapshot in history twice with a fresh receipt time.
func checkStateRetransmitDedup(ctx context.Context, env Env) error {
	plugin, err := env.newFakePlugin(ctx, "conformance: state retransmit", 5)
	if err != nil {
		return err
	}
	serverID := plugin.Creds.ServerID

	sent := plugin.nextOutbound("state.players", map[string]any{
		"players": []map[string]any{{
			"player": map[string]any{"platform": "steam", "id": "76561198000000042"},
		}}})
	if _, err := plugin.send(ctx, sent); err != nil {
		return err
	}

	// The game server restarts before the ack reaches the plugin: the buffer
	// is replayed on the next session, renumbered, everything else unchanged.
	if err := plugin.reconnect(ctx, 5); err != nil {
		return err
	}
	if _, err := plugin.send(ctx, plugin.renumber(sent)); err != nil {
		return err
	}

	var history struct {
		Snapshots []stateRecord `json:"snapshots"`
	}
	if err := env.expect(ctx, http.MethodGet,
		"/api/v1/servers/"+serverID+"/state/players/history", env.AdminToken,
		nil, http.StatusOK, &history); err != nil {
		return err
	}
	if len(history.Snapshots) != 1 {
		return fmt.Errorf("history has %d snapshots after a cross-session replay, want the one original",
			len(history.Snapshots))
	}
	return nil
}
