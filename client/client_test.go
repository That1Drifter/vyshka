package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/That1Drifter/vyshka/client"
)

const testToken = "vya_CLIENTTESTTOKEN"

// recorded is what the fake hub saw of one request.
type recorded struct {
	method      string
	requestURI  string
	rawPath     string
	query       map[string][]string
	auth        string
	accept      string
	contentType string
	body        string
}

// fakeHub serves canned answers and records every request, so a test can
// assert on the exact wire form the client produced.
type fakeHub struct {
	mu       sync.Mutex
	requests []recorded
	answer   func(w http.ResponseWriter, r *http.Request)
}

func newFakeHub(t *testing.T, answer func(w http.ResponseWriter, r *http.Request)) (*fakeHub, *client.Client) {
	t.Helper()
	hub := &fakeHub{answer: answer}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		hub.mu.Lock()
		hub.requests = append(hub.requests, recorded{
			method:      r.Method,
			requestURI:  r.RequestURI,
			rawPath:     r.URL.RawPath,
			query:       r.URL.Query(),
			auth:        r.Header.Get("Authorization"),
			accept:      r.Header.Get("Accept"),
			contentType: r.Header.Get("Content-Type"),
			body:        string(body),
		})
		hub.mu.Unlock()
		hub.answer(w, r)
	}))
	t.Cleanup(server.Close)

	c, err := client.New(server.URL+"/", testToken)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return hub, c
}

func (h *fakeHub) last(t *testing.T) recorded {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.requests) == 0 {
		t.Fatal("the fake hub saw no request")
	}
	return h.requests[len(h.requests)-1]
}

func (h *fakeHub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.requests)
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func TestNewRejectsUnusableArguments(t *testing.T) {
	cases := []struct {
		name, url, token string
	}{
		{"empty url", "", testToken},
		{"empty token", "http://127.0.0.1:8080", ""},
		{"no scheme", "127.0.0.1:8080", testToken},
		{"wrong scheme", "ftp://127.0.0.1", testToken},
		{"query", "http://127.0.0.1:8080/?x=1", testToken},
		{"token with a newline", "http://127.0.0.1:8080", "vya_abc\n"},
	}
	for _, c := range cases {
		if _, err := client.New(c.url, c.token); err == nil {
			t.Errorf("%s: New accepted %q", c.name, c.url)
		} else if strings.Contains(err.Error(), "vya_abc") {
			t.Errorf("%s: the error echoes the token: %v", c.name, err)
		}
	}
}

func TestRequestsCarryBearerAndJSONHeaders(t *testing.T) {
	hub, c := newFakeHub(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusAccepted, `{"actionId":"A1","state":"queued","extra":"tolerated"}`)
	})

	dispatched, err := c.DispatchAction(context.Background(), "S1", client.DispatchRequest{Code: "example-mod.heal"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if dispatched.ActionID != "A1" || dispatched.State != "queued" {
		t.Errorf("dispatched = %+v", dispatched)
	}
	if !strings.Contains(string(dispatched.Raw), `"extra"`) {
		t.Errorf("Raw lost a member the client does not model: %s", dispatched.Raw)
	}

	got := hub.last(t)
	if got.auth != "Bearer "+testToken {
		t.Errorf("Authorization = %q", got.auth)
	}
	if got.accept != "application/json" {
		t.Errorf("Accept = %q", got.accept)
	}
	if got.contentType != "application/json" {
		t.Errorf("Content-Type = %q", got.contentType)
	}
	// A trailing slash on the base URL must not double up in the path.
	if got.requestURI != "/api/v1/servers/S1/actions" {
		t.Errorf("request URI = %q", got.requestURI)
	}
	// Nil params travel as {}, and empty optional fields are left out.
	var body map[string]any
	if err := json.Unmarshal([]byte(got.body), &body); err != nil {
		t.Fatalf("body %q: %v", got.body, err)
	}
	if params, ok := body["params"].(map[string]any); !ok || len(params) != 0 {
		t.Errorf("params = %#v, want {}", body["params"])
	}
	for _, absent := range []string{"context", "referenceKey", "ttlSeconds", "idempotencyKey"} {
		if _, present := body[absent]; present {
			t.Errorf("empty %s was sent: %s", absent, got.body)
		}
	}
}

func TestGetCarriesNoContentType(t *testing.T) {
	hub, c := newFakeHub(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, `{"servers":[]}`)
	})
	if _, err := c.ListServers(context.Background()); err != nil {
		t.Fatalf("list: %v", err)
	}
	if got := hub.last(t); got.contentType != "" || got.body != "" {
		t.Errorf("a GET carried a body or content type: %+v", got)
	}
}

func TestErrorDecodesCodeMessageDetailsAndStatus(t *testing.T) {
	_, c := newFakeHub(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusBadRequest, `{"error":{"code":"params_invalid","message":"params fail the schema",
			"details":{"errors":[{"path":"amount","message":"500 is above the maximum 100"}]}}}`)
	})

	_, err := c.DispatchAction(context.Background(), "S1", client.DispatchRequest{Code: "x"})
	var refusal *client.Error
	if !errors.As(err, &refusal) {
		t.Fatalf("error %v (%T) is not *client.Error", err, err)
	}
	if refusal.Status != http.StatusBadRequest || refusal.Code != "params_invalid" ||
		refusal.Message != "params fail the schema" {
		t.Errorf("refusal = %+v", refusal)
	}
	faults, _ := refusal.Details["errors"].([]any)
	if len(faults) != 1 {
		t.Fatalf("details = %#v", refusal.Details)
	}
	if got := refusal.Error(); got != "params_invalid (400): params fail the schema" {
		t.Errorf("Error() = %q", got)
	}
}

func TestNonJSONErrorBodyKeepsTheStatus(t *testing.T) {
	_, c := newFakeHub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "<html>proxy error</html>")
	})

	_, err := c.ListServers(context.Background())
	var refusal *client.Error
	if !errors.As(err, &refusal) {
		t.Fatalf("error %v (%T) is not *client.Error", err, err)
	}
	if refusal.Status != http.StatusInternalServerError || refusal.Code != "" ||
		refusal.Message != "Internal Server Error" {
		t.Errorf("refusal = %+v", refusal)
	}
}

func TestUndecodableSuccessIsATransportError(t *testing.T) {
	_, c := newFakeHub(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, `{"servers": not json`)
	})
	_, err := c.ListServers(context.Background())
	var transport *client.TransportError
	if !errors.As(err, &transport) {
		t.Fatalf("error %v (%T) is not *client.TransportError", err, err)
	}
}

func TestDialFailureIsATransportError(t *testing.T) {
	// A listener opened and closed again leaves a port nobody answers on.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	address := listener.Addr().String()
	listener.Close()

	c, err := client.New("http://"+address, testToken,
		client.WithHTTPClient(&http.Client{Timeout: 3 * time.Second}))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	_, err = c.ListServers(context.Background())
	var transport *client.TransportError
	if !errors.As(err, &transport) {
		t.Fatalf("error %v (%T) is not *client.TransportError", err, err)
	}
	var refusal *client.Error
	if errors.As(err, &refusal) {
		t.Errorf("a dial failure also reads as a refusal: %v", err)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Errorf("the transport error carries the token: %v", err)
	}
}

func TestDotSegmentsTravelPercentEncoded(t *testing.T) {
	hub, c := newFakeHub(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, `{"events":[]}`)
	})

	if _, err := c.PlayerEvents(context.Background(), ".", "..", client.EventQuery{}); err != nil {
		t.Fatalf("player events: %v", err)
	}
	got := hub.last(t)
	if !strings.HasPrefix(got.requestURI, "/api/v1/players/%2e/%2e%2e/events") {
		t.Errorf("request URI = %q, want the dot segments as %%2e and %%2e%%2e", got.requestURI)
	}

	// Ordinary escaping still applies to everything else.
	if _, err := c.PlayerEvents(context.Background(), "steam", "a/b c", client.EventQuery{}); err != nil {
		t.Fatalf("player events: %v", err)
	}
	if got := hub.last(t); !strings.HasPrefix(got.requestURI, "/api/v1/players/steam/a%2Fb%20c/events") {
		t.Errorf("request URI = %q", got.requestURI)
	}
}

func TestEmptyPathParameterIsRefusedLocally(t *testing.T) {
	hub, c := newFakeHub(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, `{}`)
	})
	if _, err := c.GetServer(context.Background(), ""); err == nil {
		t.Fatal("an empty server id was accepted")
	}
	if hub.count() != 0 {
		t.Errorf("an empty server id reached the wire")
	}
}

func TestListEventsEncodesRepeatedTypesAndUTCTimes(t *testing.T) {
	hub, c := newFakeHub(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, `{"events":[{"id":"E1","serverId":"S1","type":"core.player.connect",
			"occurredAt":"2026-09-01T11:00:00.000Z","receivedAt":"2026-09-01T11:00:01.000Z","data":{}}],
			"nextCursor":"C2"}`)
	})

	offset := time.FixedZone("plus-one", 3600)
	since := time.Date(2026, 9, 1, 12, 0, 0, 0, offset)
	until := time.Date(2026, 9, 2, 12, 30, 0, 0, offset)
	page, err := c.ListEvents(context.Background(), "S1", client.EventQuery{
		Types: []string{"core.player.*", "example-mod.raid.started"},
		Since: &since, Until: &until, Limit: 50, Cursor: "C1",
	})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(page.Events) != 1 || page.NextCursor != "C2" {
		t.Errorf("page = %+v", page)
	}

	got := hub.last(t)
	if types := got.query["type"]; len(types) != 2 || types[0] != "core.player.*" || types[1] != "example-mod.raid.started" {
		t.Errorf("type = %v, want both terms, repeated", types)
	}
	if got.query["since"][0] != "2026-09-01T11:00:00Z" {
		t.Errorf("since = %q, want RFC 3339 in UTC", got.query["since"][0])
	}
	if got.query["until"][0] != "2026-09-02T11:30:00Z" {
		t.Errorf("until = %q, want RFC 3339 in UTC", got.query["until"][0])
	}
	if got.query["limit"][0] != "50" || got.query["cursor"][0] != "C1" {
		t.Errorf("query = %v", got.query)
	}
}

// actionSequence answers GET /api/v1/actions/{id} with each state in turn,
// repeating the last one.
func actionSequence(states ...string) func(w http.ResponseWriter, r *http.Request) {
	var mu sync.Mutex
	next := 0
	return func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		state := states[min(next, len(states)-1)]
		next++
		mu.Unlock()
		writeJSON(w, http.StatusOK, `{"id":"A1","serverId":"S1","code":"x","params":{},"state":"`+state+
			`","createdAt":"2026-09-01T12:00:00Z","expiresAt":"2026-09-01T12:02:00Z","result":{"healedTo":100}}`)
	}
}

func TestWaitActionReturnsOnTerminalAndReportsEachChange(t *testing.T) {
	_, c := newFakeHub(t, actionSequence("queued", "queued", "delivered", "running", "running", "completed"))

	var changes []string
	action, err := c.WaitAction(context.Background(), "A1", client.WaitOptions{
		Interval: time.Millisecond,
		OnChange: func(a client.Action) { changes = append(changes, a.State) },
	})
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if action.State != "completed" || !action.Terminal() || !action.HasResult() {
		t.Errorf("action = %+v", action)
	}
	if got := strings.Join(changes, " "); got != "queued delivered running completed" {
		t.Errorf("changes = %q, want one call per state change including the first read", got)
	}
}

func TestWaitActionReturnsTheLastRecordAndTheContextErrorOnDeadline(t *testing.T) {
	_, c := newFakeHub(t, actionSequence("queued", "running"))

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	action, err := c.WaitAction(ctx, "A1", client.WaitOptions{Interval: 5 * time.Millisecond})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want one wrapping context.DeadlineExceeded", err)
	}
	var refusal *client.Error
	if errors.As(err, &refusal) {
		t.Errorf("a deadline reads as a refusal: %v", err)
	}
	if action.State != "running" || action.ID != "A1" {
		t.Errorf("action = %+v, want the last record read", action)
	}
}

func TestWaitActionPassesARefusalThrough(t *testing.T) {
	_, c := newFakeHub(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusForbidden, `{"error":{"code":"forbidden","message":"no"}}`)
	})
	_, err := c.WaitAction(context.Background(), "A1", client.WaitOptions{Interval: time.Millisecond})
	var refusal *client.Error
	if !errors.As(err, &refusal) || refusal.Code != "forbidden" {
		t.Fatalf("error = %v, want the forbidden refusal", err)
	}
}

func TestKVSetSendsIfRevisionZeroAndOmitsNil(t *testing.T) {
	hub, c := newFakeHub(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, `{"namespace":"ns","key":"k","revision":1}`)
	})

	zero := int64(0)
	if _, err := c.KVSet(context.Background(), "ns", "k", client.KVSetRequest{Value: 5, IfRevision: &zero}); err != nil {
		t.Fatalf("set: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(hub.last(t).body), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if revision, present := body["ifRevision"]; !present || revision != float64(0) {
		t.Errorf("ifRevision = %#v (present %v), want 0 sent", revision, present)
	}
	if _, present := body["ttlSeconds"]; present {
		t.Errorf("a zero ttlSeconds was sent: %v", body)
	}

	if _, err := c.KVSet(context.Background(), "ns", "k", client.KVSetRequest{Value: "x"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got := hub.last(t).body; strings.Contains(got, "ifRevision") {
		t.Errorf("a nil ifRevision was sent: %s", got)
	}
	if got := hub.last(t); got.method != http.MethodPut || got.requestURI != "/api/v1/kv/ns/k" {
		t.Errorf("request = %s %s", got.method, got.requestURI)
	}

	// A raw value travels byte for byte, so a number beyond float64's exact
	// range is not rounded on the way.
	if _, err := c.KVSet(context.Background(), "ns", "k",
		client.KVSetRequest{Value: json.RawMessage(`9007199254740993`)}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got := hub.last(t).body; !strings.Contains(got, "9007199254740993") {
		t.Errorf("raw value was rewritten: %s", got)
	}
}

func TestKVIncrWithNilDeltaSendsNoBody(t *testing.T) {
	hub, c := newFakeHub(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, `{"namespace":"ns","key":"k","value":1,"revision":1}`)
	})

	if _, err := c.KVIncr(context.Background(), "ns", "k", nil); err != nil {
		t.Fatalf("incr: %v", err)
	}
	got := hub.last(t)
	if got.body != "" || got.contentType != "" {
		t.Errorf("nil delta sent body %q with content type %q", got.body, got.contentType)
	}
	if got.method != http.MethodPost || got.requestURI != "/api/v1/kv/ns/k/incr" {
		t.Errorf("request = %s %s", got.method, got.requestURI)
	}

	delta := int64(-5)
	entry, err := c.KVIncr(context.Background(), "ns", "k", &delta)
	if err != nil {
		t.Fatalf("incr: %v", err)
	}
	if got := hub.last(t).body; got != `{"delta":-5}` {
		t.Errorf("body = %q", got)
	}
	if string(entry.Value) != "1" || entry.Revision != 1 {
		t.Errorf("entry = %+v", entry)
	}
}

func TestHealthReturnsTheBodyBesideADegradedError(t *testing.T) {
	_, c := newFakeHub(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusServiceUnavailable, `{"status":"degraded","version":"v1","uptimeSeconds":5,
			"database":{"driver":"sqlite","ok":false,"schemaVersion":0,"error":"disk full"}}`)
	})
	health, err := c.Health(context.Background())
	var refusal *client.Error
	if !errors.As(err, &refusal) || refusal.Code != "degraded" || refusal.Status != http.StatusServiceUnavailable {
		t.Fatalf("error = %v, want a degraded refusal", err)
	}
	if health.Status != "degraded" || health.Database.Error != "disk full" {
		t.Errorf("health = %+v, want the body beside the error", health)
	}
}

func TestUnknownStatesAndMembersAreTolerated(t *testing.T) {
	_, c := newFakeHub(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, `{"id":"A1","serverId":"S1","code":"x","params":{},"state":"paused",
			"createdAt":"2026-09-01T12:00:00Z","expiresAt":"2026-09-01T12:02:00Z","result":null,
			"futureMember":{"nested":[1,2,3]}}`)
	})
	action, err := c.GetAction(context.Background(), "A1")
	if err != nil {
		t.Fatalf("get action: %v", err)
	}
	if action.State != "paused" || action.Terminal() || action.HasResult() {
		t.Errorf("action = %+v", action)
	}
}

func TestStateSnapshotDecoders(t *testing.T) {
	snapshot := client.StateSnapshot{Snapshot: json.RawMessage(`{"players":[{"player":{"platform":"steam","id":"7"},
		"name":"Survivor","position":[1,2,3],"data":{"health":82},"extra":true}],
		"vehicles":[{"id":"v1","kind":"car"}],"entities":[{"id":"e1"}],
		"world":{"time":"2026-09-20T14:32","data":{"rain":0}}}`)}

	players, err := snapshot.Players()
	if err != nil || len(players) != 1 || players[0].Player.ID != "7" || players[0].Name != "Survivor" ||
		len(players[0].Position) != 3 {
		t.Errorf("players = %+v, %v", players, err)
	}
	vehicles, err := snapshot.Vehicles()
	if err != nil || len(vehicles) != 1 || vehicles[0].Kind != "car" {
		t.Errorf("vehicles = %+v, %v", vehicles, err)
	}
	entities, err := snapshot.Entities()
	if err != nil || len(entities) != 1 || entities[0].ID != "e1" {
		t.Errorf("entities = %+v, %v", entities, err)
	}
	world, err := snapshot.World()
	if err != nil || world.Time != "2026-09-20T14:32" {
		t.Errorf("world = %+v, %v", world, err)
	}
}
