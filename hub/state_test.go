package hub_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/That1Drifter/vyshka/hub"
)

// stateView mirrors the Admin API snapshot view on the wire (spec section 8.3).
type stateView struct {
	Type       string          `json:"type"`
	CapturedAt string          `json:"capturedAt"`
	ReceivedAt string          `json:"receivedAt"`
	Snapshot   json.RawMessage `json:"snapshot"`
}

// stateEnvelope frames one state.* envelope.
func stateEnvelope(seq int64, envelopeType string, body map[string]any) map[string]any {
	return map[string]any{
		"v": 1, "id": "state-" + strconv.FormatInt(seq, 10),
		"type": envelopeType, "seq": seq,
		"ts": time.Now().UTC().Format(time.RFC3339), "body": body,
	}
}

func getState(t *testing.T, server *hub.Server, serverID, stateType string) stateView {
	t.Helper()
	var view stateView
	status := call(t, server, http.MethodGet,
		"/api/v1/servers/"+serverID+"/state/"+stateType, testAdminToken, nil, &view)
	if status != http.StatusOK {
		t.Fatalf("get state %s: status = %d, want 200", stateType, status)
	}
	return view
}

func TestStateSnapshotLatestReplacesAndReadsBack(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, live := enrolledSession(t, server, "state latest")

	// No snapshot yet: a real server, an empty answer.
	if code := errorCode(t, server, http.MethodGet,
		"/api/v1/servers/"+created.Server.ID+"/state/players",
		testAdminToken, nil, http.StatusNotFound); code != "not_found" {
		t.Errorf("state before any snapshot: code = %q, want not_found", code)
	}

	first := map[string]any{
		"capturedAt": "2026-08-25T12:00:00Z",
		"players": []map[string]any{{
			"player":   map[string]any{"platform": "steam", "id": "76561198000000001"},
			"name":     "Survivor",
			"position": []float64{4231.5, 300.25, 10620},
			"data":     map[string]any{"health": 82},
		}},
	}
	result := pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{
		"envelopes": []map[string]any{stateEnvelope(1, "state.players", first)},
	})
	if result.Ack != 1 {
		t.Fatalf("ack = %d after state.players, want 1", result.Ack)
	}

	view := getState(t, server, created.Server.ID, "players")
	if view.Type != "players" {
		t.Errorf("type = %q, want players", view.Type)
	}
	if view.CapturedAt != "2026-08-25T12:00:00Z" && view.CapturedAt != "2026-08-25T12:00:00.000Z" {
		t.Errorf("capturedAt = %q, want the body's clock", view.CapturedAt)
	}
	var stored struct {
		Players []struct {
			Player struct {
				Platform string `json:"platform"`
				ID       string `json:"id"`
			} `json:"player"`
			Name string `json:"name"`
		} `json:"players"`
	}
	if err := json.Unmarshal(view.Snapshot, &stored); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if len(stored.Players) != 1 || stored.Players[0].Player.ID != "76561198000000001" {
		t.Fatalf("snapshot = %s, want the published list verbatim", view.Snapshot)
	}

	// A later snapshot replaces the whole list; an empty one means nobody is
	// online, which is a real answer and not a missing one.
	result = pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{
		"envelopes": []map[string]any{stateEnvelope(2, "state.players",
			map[string]any{"players": []map[string]any{}})},
	})
	if result.Ack != 2 {
		t.Fatalf("ack = %d after the empty snapshot, want 2", result.Ack)
	}
	view = getState(t, server, created.Server.ID, "players")
	var emptied struct {
		Players []any `json:"players"`
	}
	if err := json.Unmarshal(view.Snapshot, &emptied); err != nil {
		t.Fatalf("decode empty snapshot: %v", err)
	}
	if emptied.Players == nil || len(emptied.Players) != 0 {
		t.Errorf("snapshot after the empty push = %s, want an empty list", view.Snapshot)
	}

	// History: newest first, both snapshots present.
	var history struct {
		Snapshots []stateView `json:"snapshots"`
	}
	status := call(t, server, http.MethodGet,
		"/api/v1/servers/"+created.Server.ID+"/state/players/history",
		testAdminToken, nil, &history)
	if status != http.StatusOK {
		t.Fatalf("history: status = %d, want 200", status)
	}
	if len(history.Snapshots) != 2 {
		t.Fatalf("history has %d snapshots, want 2", len(history.Snapshots))
	}
	if err := json.Unmarshal(history.Snapshots[0].Snapshot, &emptied); err != nil || len(emptied.Players) != 0 {
		t.Errorf("history[0] = %s, want the newest (empty) snapshot first", history.Snapshots[0].Snapshot)
	}
}

func TestStateSnapshotTypesAreIndependent(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, live := enrolledSession(t, server, "state types")

	result := pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{
		"envelopes": []map[string]any{stateEnvelope(1, "state.vehicles",
			map[string]any{"vehicles": []map[string]any{{
				"id": "v-101", "kind": "helicopter", "position": []float64{100, 200},
			}}})},
	})
	if result.Ack != 1 {
		t.Fatalf("ack = %d after state.vehicles, want 1", result.Ack)
	}

	view := getState(t, server, created.Server.ID, "vehicles")
	var stored struct {
		Vehicles []struct {
			ID   string `json:"id"`
			Kind string `json:"kind"`
		} `json:"vehicles"`
	}
	if err := json.Unmarshal(view.Snapshot, &stored); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(stored.Vehicles) != 1 || stored.Vehicles[0].ID != "v-101" {
		t.Fatalf("vehicles snapshot = %s, want the published one", view.Snapshot)
	}

	// The vehicles push says nothing about players.
	if code := errorCode(t, server, http.MethodGet,
		"/api/v1/servers/"+created.Server.ID+"/state/players",
		testAdminToken, nil, http.StatusNotFound); code != "not_found" {
		t.Errorf("players after a vehicles push: code = %q, want not_found", code)
	}
}

func TestStateSnapshotRejectedWholeWithNotice(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, live := enrolledSession(t, server, "state reject")

	// One good entry cannot carry a bad one: the snapshot is refused whole.
	bad := map[string]any{"players": []map[string]any{
		{"player": map[string]any{"platform": "steam", "id": "76561198000000001"}},
		{"name": "identity-free"},
	}}
	result := pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{
		"envelopes": []map[string]any{stateEnvelope(1, "state.players", bad)},
	})
	if result.Ack != 1 {
		t.Fatalf("ack = %d, want the refused envelope acked (rejection is envelope-level success)", result.Ack)
	}

	// Nothing was stored...
	if code := errorCode(t, server, http.MethodGet,
		"/api/v1/servers/"+created.Server.ID+"/state/players",
		testAdminToken, nil, http.StatusNotFound); code != "not_found" {
		t.Errorf("state after a rejected snapshot: code = %q, want not_found", code)
	}

	// ...and the plugin was told, with the fault pointing at the entry.
	var reject *wireEnvelope
	for _, delivered := range result.Envelopes {
		if delivered.Type == "state.reject" {
			reject = &delivered
			break
		}
	}
	if reject == nil {
		// Ack what the first poll delivered, in the hub -> plugin space, and
		// look again: the notice may have been queued after the first read.
		var deliveredThrough int64
		for _, delivered := range result.Envelopes {
			deliveredThrough = max(deliveredThrough, delivered.Seq)
		}
		second := poll(t, server, live.SessionToken, map[string]any{"ack": deliveredThrough})
		for _, delivered := range second.Envelopes {
			if delivered.Type == "state.reject" {
				reject = &delivered
				break
			}
		}
	}
	if reject == nil {
		t.Fatal("no state.reject envelope was delivered")
	}
	var notice struct {
		EnvelopeID string `json:"envelopeId"`
		Errors     []struct {
			Path    string `json:"path"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(reject.Body, &notice); err != nil {
		t.Fatalf("decode state.reject: %v", err)
	}
	if notice.EnvelopeID != "state-1" {
		t.Errorf("state.reject names envelope %q, want state-1", notice.EnvelopeID)
	}
	if len(notice.Errors) == 0 || notice.Errors[0].Path != "players[1].player" {
		t.Errorf("state.reject errors = %+v, want the identity fault at players[1].player", notice.Errors)
	}
}

func TestStateValidationEdges(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, live := enrolledSession(t, server, "state validation")

	cases := []struct {
		name string
		t    string
		body map[string]any
	}{
		{"missing list", "state.players", map[string]any{"capturedAt": "2026-08-25T12:00:00Z"}},
		{"wrong list for the type", "state.vehicles", map[string]any{"players": []any{}}},
		{"vehicle without id", "state.vehicles",
			map[string]any{"vehicles": []map[string]any{{"kind": "car"}}}},
		{"one-coordinate position", "state.entities",
			map[string]any{"entities": []map[string]any{{"id": "e-1", "position": []float64{1}}}}},
		{"null coordinate", "state.entities",
			map[string]any{"entities": []map[string]any{{"id": "e-1", "position": []any{nil, 2}}}}},
		{"data not an object", "state.players",
			map[string]any{"players": []map[string]any{{
				"player": map[string]any{"platform": "steam", "id": "1"}, "data": "words"}}}},
	}
	for i, one := range cases {
		seq := int64(i + 1)
		result := pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{
			"envelopes": []map[string]any{stateEnvelope(seq, one.t, one.body)},
		})
		if result.Ack != seq {
			t.Fatalf("%s: ack = %d, want %d", one.name, result.Ack, seq)
		}
	}
	for _, stateType := range []string{"players", "vehicles", "entities"} {
		if code := errorCode(t, server, http.MethodGet,
			"/api/v1/servers/"+created.Server.ID+"/state/"+stateType,
			testAdminToken, nil, http.StatusNotFound); code != "not_found" {
			t.Errorf("%s stored something from an invalid snapshot (code %q)", stateType, code)
		}
	}
}

// Section 2.1's unknown-field rule applies inside a snapshot body too: a
// state.players body carrying a `vehicles` field of any shape at all is a
// body with an unknown field, not a malformed snapshot.
func TestStateSnapshotToleratesUnknownFields(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, live := enrolledSession(t, server, "state tolerance")

	body := map[string]any{
		"players":     []map[string]any{{"player": map[string]any{"platform": "steam", "id": "1"}}},
		"vehicles":    map[string]any{"formatFromFutureDraft": 2},
		"x-mod-extra": "kept verbatim",
	}
	result := pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{
		"envelopes": []map[string]any{stateEnvelope(1, "state.players", body)},
	})
	if result.Ack != 1 {
		t.Fatalf("ack = %d, want 1", result.Ack)
	}

	view := getState(t, server, created.Server.ID, "players")
	var stored map[string]json.RawMessage
	if err := json.Unmarshal(view.Snapshot, &stored); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if string(stored["x-mod-extra"]) != `"kept verbatim"` {
		t.Errorf("unknown field was not stored verbatim: %s", stored["x-mod-extra"])
	}
	if _, present := stored["vehicles"]; !present {
		t.Error("the unknown-shaped vehicles field was dropped rather than kept verbatim")
	}
}

func TestStateEndpointGuards(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, _ := enrolledSession(t, server, "state guards")

	// An unknown state type is a bad request, never an empty answer.
	if code := errorCode(t, server, http.MethodGet,
		"/api/v1/servers/"+created.Server.ID+"/state/weather",
		testAdminToken, nil, http.StatusBadRequest); code != "bad_request" {
		t.Errorf("unknown state type: code = %q, want bad_request", code)
	}
	// An unknown server is not found.
	if code := errorCode(t, server, http.MethodGet,
		"/api/v1/servers/no-such-server/state/players",
		testAdminToken, nil, http.StatusNotFound); code != "not_found" {
		t.Errorf("unknown server: code = %q, want not_found", code)
	}
	// The raw envelope queue refuses the state family: a forged state.reject
	// must not be queueable as the hub's own word.
	if code := errorCode(t, server, http.MethodPost,
		"/api/v1/servers/"+created.Server.ID+"/envelopes", testAdminToken,
		map[string]any{"type": "state.reject"}, http.StatusConflict); code != "conflict" {
		t.Errorf("queueing state.reject: code = %q, want conflict", code)
	}

	// servers:read is the gate; a token without it is refused.
	reader, _ := mintToken(t, server, "state reader", "servers:read")
	unrelated, _ := mintToken(t, server, "state outsider", "events:read")
	if code := errorCode(t, server, http.MethodGet,
		"/api/v1/servers/"+created.Server.ID+"/state/players",
		reader, nil, http.StatusNotFound); code != "not_found" {
		t.Errorf("servers:read reading empty state: code = %q, want not_found", code)
	}
	if code := errorCode(t, server, http.MethodGet,
		"/api/v1/servers/"+created.Server.ID+"/state/players",
		unrelated, nil, http.StatusForbidden); code != "forbidden" {
		t.Errorf("events:read reading state: code = %q, want forbidden", code)
	}
}

func TestStateCapturedAtFallsBackToEnvelopeTS(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, live := enrolledSession(t, server, "state clock")

	sent := map[string]any{
		"v": 1, "id": "state-clock-1", "type": "state.players", "seq": 1,
		"ts":   "2026-08-25T11:30:00Z",
		"body": map[string]any{"players": []any{}},
	}
	result := pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{
		"envelopes": []map[string]any{sent},
	})
	if result.Ack != 1 {
		t.Fatalf("ack = %d, want 1", result.Ack)
	}

	view := getState(t, server, created.Server.ID, "players")
	parsed, err := time.Parse(time.RFC3339, view.CapturedAt)
	if err != nil {
		t.Fatalf("capturedAt %q is not RFC 3339: %v", view.CapturedAt, err)
	}
	want := time.Date(2026, 8, 25, 11, 30, 0, 0, time.UTC)
	if !parsed.Equal(want) {
		t.Errorf("capturedAt = %v, want the envelope ts %v", parsed, want)
	}
}
