package hub_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/That1Drifter/vyshka/hub"
)

// eventRecord mirrors the Admin API event view on the wire (spec section 8.5).
type eventRecord struct {
	ID         string          `json:"id"`
	ServerID   string          `json:"serverId"`
	Type       string          `json:"type"`
	OccurredAt string          `json:"occurredAt"`
	ReceivedAt string          `json:"receivedAt"`
	Data       json.RawMessage `json:"data"`
}

type eventPage struct {
	Events     []eventRecord `json:"events"`
	NextCursor string        `json:"nextCursor"`
}

// eventBatchEnvelope frames one event.batch envelope.
func eventBatchEnvelope(seq int64, events ...map[string]any) map[string]any {
	if events == nil {
		events = []map[string]any{}
	}
	return map[string]any{
		"v": 1, "id": "event-batch-" + strconv.FormatInt(seq, 10),
		"type": "event.batch", "seq": seq,
		"ts":   time.Now().UTC().Format(time.RFC3339),
		"body": map[string]any{"events": events},
	}
}

func queryEvents(t *testing.T, server *hub.Server, serverID string, parameters url.Values) eventPage {
	t.Helper()

	path := "/api/v1/servers/" + serverID + "/events"
	if encoded := parameters.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var page eventPage
	status := call(t, server, http.MethodGet, path, testAdminToken, nil, &page)
	if status != http.StatusOK {
		t.Fatalf("query events: status = %d, want 200", status)
	}
	return page
}

// The requirement section 8.1 calls load-bearing: a mod's own event is stored
// and queried exactly like a core one, through one channel and one filter.
func TestEventIngestTreatsCoreAndCustomAlike(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, live := enrolledSession(t, server, "event ingest")

	result := pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{
		"envelopes": []map[string]any{eventBatchEnvelope(1,
			map[string]any{
				"t":    "core.player.death",
				"ts":   time.Now().UTC().Add(-time.Minute).Format(time.RFC3339),
				"data": map[string]any{"weapon": "M4A1", "distance": 312.5},
			},
			map[string]any{
				"t":    "example-mod.raid.started",
				"data": map[string]any{"territoryId": "t-19", "attackers": 4},
			},
		)},
	})
	if result.Ack != 1 {
		t.Fatalf("ack = %d after one event.batch, want 1", result.Ack)
	}

	page := queryEvents(t, server, created.Server.ID, nil)
	if len(page.Events) != 2 {
		t.Fatalf("query returned %d events, want both", len(page.Events))
	}
	types := map[string]json.RawMessage{}
	for _, one := range page.Events {
		types[one.Type] = one.Data
		if one.ServerID != created.Server.ID {
			t.Errorf("event %s carries serverId %q, want %q", one.ID, one.ServerID, created.Server.ID)
		}
		if _, err := time.Parse(time.RFC3339, one.OccurredAt); err != nil {
			t.Errorf("occurredAt %q is not RFC 3339", one.OccurredAt)
		}
		if _, err := time.Parse(time.RFC3339, one.ReceivedAt); err != nil {
			t.Errorf("receivedAt %q is not RFC 3339", one.ReceivedAt)
		}
	}
	if _, found := types["core.player.death"]; !found {
		t.Error("the core event is missing from the feed")
	}
	custom, found := types["example-mod.raid.started"]
	if !found {
		t.Fatal("the custom event is missing from the feed")
	}
	var payload struct {
		TerritoryID string `json:"territoryId"`
	}
	if err := json.Unmarshal(custom, &payload); err != nil || payload.TerritoryID != "t-19" {
		t.Errorf("custom event data = %s, want what the plugin sent", custom)
	}

	// The custom event was never declared in a manifest. Section 6.3 requires a
	// hub to store undeclared custom events anyway: pre-declaration would put
	// the restart-to-change problem back.
	if _, undeclared := types["example-mod.raid.started"]; !undeclared {
		t.Error("an undeclared custom event was dropped")
	}
}

// An oversized batch is refused whole, the poll still succeeds, the envelope is
// still acked, and the plugin is told why through an event.reject notice.
func TestEventBatchOverTheLimitIsRejectedNotFatal(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, live := enrolledSession(t, server, "event batch limit")

	oversized := make([]map[string]any, 0, 201)
	for range 201 {
		oversized = append(oversized, map[string]any{"t": "core.player.damage"})
	}

	result := pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{
		"envelopes": []map[string]any{eventBatchEnvelope(1, oversized...)},
	})
	if result.Ack != 1 {
		t.Fatalf("ack = %d; a rejected batch is still acked, its durable effect is nothing", result.Ack)
	}
	if page := queryEvents(t, server, created.Server.ID, nil); len(page.Events) != 0 {
		t.Fatalf("the feed holds %d events after a rejected batch, want none", len(page.Events))
	}

	// The rejection is not silent: it comes back as a queued envelope.
	var notice *wireEnvelope
	for attempt := range 3 {
		next := poll(t, server, live.SessionToken, map[string]any{"ack": int64(attempt)})
		for i := range next.Envelopes {
			if next.Envelopes[i].Type == "event.reject" {
				notice = &next.Envelopes[i]
			}
		}
		if notice != nil {
			break
		}
	}
	if notice == nil {
		t.Fatal("no event.reject reached the plugin")
	}
	var body struct {
		EnvelopeID string `json:"envelopeId"`
		Errors     []struct {
			Path    string `json:"path"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(notice.Body, &body); err != nil {
		t.Fatalf("decode event.reject body %s: %v", notice.Body, err)
	}
	if body.EnvelopeID != "event-batch-1" {
		t.Errorf("event.reject names envelope %q, want the batch it refused", body.EnvelopeID)
	}
	if len(body.Errors) == 0 || body.Errors[0].Message == "" {
		t.Errorf("event.reject carried no usable faults: %+v", body.Errors)
	}
}

// A malformed event rejects its whole batch rather than landing partially: the
// plugin cannot tell which of its events survived a partial accept, because the
// ack is per envelope and there is no per-event receipt.
func TestEventBatchRejectionIsWholeBatch(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, live := enrolledSession(t, server, "event batch atomic")

	result := pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{
		"envelopes": []map[string]any{eventBatchEnvelope(1,
			map[string]any{"t": "core.player.death"},
			map[string]any{"t": "no-namespace"},
		)},
	})
	if result.Ack != 1 {
		t.Fatalf("ack = %d, want the rejected envelope acked", result.Ack)
	}
	if page := queryEvents(t, server, created.Server.ID, nil); len(page.Events) != 0 {
		t.Errorf("the feed holds %d events; a batch with a bad event stores none", len(page.Events))
	}
}

// Retransmission is deduplicated by the sequence machinery of section 9.1, so a
// plugin that never sees an ack does not double-store its telemetry.
func TestEventBatchRetransmissionDoesNotDoubleStore(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, live := enrolledSession(t, server, "event dedup")

	batch := eventBatchEnvelope(1, map[string]any{"t": "core.player.connect"})
	for range 3 {
		pollNow(t, server, created.Server.ID, live.SessionToken,
			map[string]any{"envelopes": []map[string]any{batch}})
	}

	page := queryEvents(t, server, created.Server.ID, nil)
	if len(page.Events) != 1 {
		t.Fatalf("the feed holds %d copies of one retransmitted event, want 1", len(page.Events))
	}
}

// The query narrows by type pattern and paginates with an opaque cursor.
func TestEventQueryFiltersAndPaginates(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, live := enrolledSession(t, server, "event query")

	base := time.Now().UTC().Add(-time.Hour)
	events := make([]map[string]any, 0, 8)
	for i := range 5 {
		events = append(events, map[string]any{
			"t":  "core.player.death",
			"ts": base.Add(time.Duration(i) * time.Second).Format(time.RFC3339),
		})
	}
	for i := range 3 {
		events = append(events, map[string]any{
			"t":  "example-mod.raid.started",
			"ts": base.Add(time.Duration(10+i) * time.Second).Format(time.RFC3339),
		})
	}
	pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{
		"envelopes": []map[string]any{eventBatchEnvelope(1, events...)},
	})

	// A namespace pattern.
	page := queryEvents(t, server, created.Server.ID, url.Values{"type": {"example-mod.*"}})
	if len(page.Events) != 3 {
		t.Errorf("example-mod.* matched %d events, want 3", len(page.Events))
	}
	// An exact type.
	page = queryEvents(t, server, created.Server.ID, url.Values{"type": {"core.player.death"}})
	if len(page.Events) != 5 {
		t.Errorf("core.player.death matched %d events, want 5", len(page.Events))
	}
	// Repeated terms are ORed, which is what makes one query span core and
	// custom events without privileging either.
	page = queryEvents(t, server, created.Server.ID,
		url.Values{"type": {"core.player.*", "example-mod.*"}})
	if len(page.Events) != 8 {
		t.Errorf("the ORed filter matched %d events, want all 8", len(page.Events))
	}

	// Pagination walks the feed newest first, exactly once.
	seen := map[string]int{}
	parameters := url.Values{"limit": {"3"}}
	for range 5 {
		page = queryEvents(t, server, created.Server.ID, parameters)
		for _, one := range page.Events {
			seen[one.ID]++
		}
		if page.NextCursor == "" {
			break
		}
		parameters.Set("cursor", page.NextCursor)
	}
	if page.NextCursor != "" {
		t.Error("the feed never ran out of pages")
	}
	if len(seen) != 8 {
		t.Fatalf("pagination saw %d distinct events, want 8", len(seen))
	}
	for eventID, count := range seen {
		if count != 1 {
			t.Errorf("event %s appeared on %d pages", eventID, count)
		}
	}

	// A time window bounds the feed on occurredAt, since inclusive and until
	// exclusive.
	page = queryEvents(t, server, created.Server.ID, url.Values{
		"since": {base.Format(time.RFC3339)},
		"until": {base.Add(3 * time.Second).Format(time.RFC3339)},
	})
	if len(page.Events) != 3 {
		t.Errorf("the window matched %d events, want the 3 inside [since, until)", len(page.Events))
	}
}

// The per-poll event budget is charged against what is actually accepted.
// Retransmitted batches store nothing, so they must not push a new batch over
// the line: doing so would lose real events to duplicates.
func TestEventPollBudgetIgnoresDuplicates(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, live := enrolledSession(t, server, "event budget")

	// Five full batches saturate the budget exactly, so a sixth would be
	// refused. Send the first five, let them be acked, then resend all five
	// alongside a sixth: the five are duplicates and the sixth must land.
	full := make([]map[string]any, 0, 200)
	for range 200 {
		full = append(full, map[string]any{"t": "core.player.damage"})
	}
	saturating := make([]map[string]any, 0, 5)
	for seq := range 5 {
		saturating = append(saturating, eventBatchEnvelope(int64(seq+1), full...))
	}
	result := pollNow(t, server, created.Server.ID, live.SessionToken,
		map[string]any{"envelopes": saturating})
	if result.Ack != 5 {
		t.Fatalf("ack = %d after five batches, want 5", result.Ack)
	}

	withNewTail := append(append([]map[string]any{}, saturating...),
		eventBatchEnvelope(6, map[string]any{"t": "example-mod.raid.started"}))
	result = pollNow(t, server, created.Server.ID, live.SessionToken,
		map[string]any{"ack": 0, "envelopes": withNewTail})
	if result.Ack != 6 {
		t.Fatalf("ack = %d, want 6", result.Ack)
	}

	page := queryEvents(t, server, created.Server.ID,
		url.Values{"type": {"example-mod.raid.started"}})
	if len(page.Events) != 1 {
		t.Fatalf("the new batch behind five retransmitted ones stored %d events, want 1: "+
			"duplicates store nothing and must not spend the poll's budget", len(page.Events))
	}
}

// Past the per-poll budget the tail is refused, with a notice, and everything
// that fit is stored: an over-budget poll loses its end rather than an
// arbitrary subset.
func TestEventPollBudgetRefusesTheTail(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, live := enrolledSession(t, server, "event budget tail")

	full := make([]map[string]any, 0, 200)
	for range 200 {
		full = append(full, map[string]any{"t": "core.player.damage"})
	}
	// Five full batches saturate the 1 000 event budget; the sixth is over it.
	envelopes := make([]map[string]any, 0, 6)
	for seq := range 5 {
		envelopes = append(envelopes, eventBatchEnvelope(int64(seq+1), full...))
	}
	envelopes = append(envelopes, eventBatchEnvelope(6, map[string]any{"t": "example-mod.raid.started"}))

	result := pollNow(t, server, created.Server.ID, live.SessionToken,
		map[string]any{"envelopes": envelopes})
	if result.Ack != 6 {
		t.Fatalf("ack = %d, want 6: a refused batch is still acked", result.Ack)
	}

	// Everything inside the budget landed.
	page := queryEvents(t, server, created.Server.ID,
		url.Values{"type": {"core.player.damage"}, "limit": {"500"}})
	if len(page.Events) != 500 {
		t.Errorf("the first page holds %d of the 1000 events inside the budget", len(page.Events))
	}
	// The batch past it did not.
	page = queryEvents(t, server, created.Server.ID,
		url.Values{"type": {"example-mod.raid.started"}})
	if len(page.Events) != 0 {
		t.Errorf("the over-budget batch stored %d events, want none", len(page.Events))
	}

	// And the plugin was told, rather than left to wonder.
	var notice *wireEnvelope
	for attempt := range 3 {
		next := poll(t, server, live.SessionToken, map[string]any{"ack": int64(attempt)})
		for i := range next.Envelopes {
			if next.Envelopes[i].Type == "event.reject" {
				notice = &next.Envelopes[i]
			}
		}
		if notice != nil {
			break
		}
	}
	if notice == nil {
		t.Fatal("no event.reject named the batch refused for budget")
	}
	var body struct {
		EnvelopeID string `json:"envelopeId"`
	}
	if err := json.Unmarshal(notice.Body, &body); err != nil {
		t.Fatalf("decode event.reject body %s: %v", notice.Body, err)
	}
	if body.EnvelopeID != "event-batch-6" {
		t.Errorf("event.reject names %q, want the over-budget event-batch-6", body.EnvelopeID)
	}
}

func TestEventQueryRejectsUnusableParameters(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, _ := enrolledSession(t, server, "event query validation")
	path := "/api/v1/servers/" + created.Server.ID + "/events"

	for name, query := range map[string]string{
		"badType":   "?type=not%20a%20type",
		"badSince":  "?since=yesterday",
		"badUntil":  "?until=2026",
		"zeroLimit": "?limit=0",
		"wordLimit": "?limit=lots",
		"badCursor": "?cursor=not-a-cursor",
		// A bad term beside the catch-all is still a bad term: answering with
		// the whole feed would look like a real, narrower answer.
		"badTypeBesideCatchAll": "?type=*&type=not%20a%20type",
		// So is a filter the caller meant to set and left empty.
		"emptyType": "?type=",
	} {
		t.Run(name, func(t *testing.T) {
			code := errorCode(t, server, http.MethodGet, path+query, testAdminToken,
				nil, http.StatusBadRequest)
			if code != "bad_request" {
				t.Errorf("code = %q, want bad_request", code)
			}
		})
	}

	// An unknown server is 404 rather than an empty feed: an operator asking
	// about a server that does not exist has a different problem from one whose
	// server is quiet.
	if code := errorCode(t, server, http.MethodGet, "/api/v1/servers/nope/events",
		testAdminToken, nil, http.StatusNotFound); code != "not_found" {
		t.Errorf("unknown server: code = %q, want not_found", code)
	}
}

// One server's telemetry is not another's to read.
func TestEventQueryIsScopedToItsServer(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	mine, mineSession := enrolledSession(t, server, "events mine")
	theirs, theirsSession := enrolledSession(t, server, "events theirs")

	pollNow(t, server, mine.Server.ID, mineSession.SessionToken, map[string]any{
		"envelopes": []map[string]any{eventBatchEnvelope(1,
			map[string]any{"t": "core.player.death", "data": map[string]any{"who": "mine"}})},
	})
	pollNow(t, server, theirs.Server.ID, theirsSession.SessionToken, map[string]any{
		"envelopes": []map[string]any{eventBatchEnvelope(1,
			map[string]any{"t": "core.player.death", "data": map[string]any{"who": "theirs"}})},
	})

	page := queryEvents(t, server, mine.Server.ID, nil)
	if len(page.Events) != 1 {
		t.Fatalf("server %s sees %d events, want only its own", mine.Server.ID, len(page.Events))
	}
	var payload struct {
		Who string `json:"who"`
	}
	if err := json.Unmarshal(page.Events[0].Data, &payload); err != nil || payload.Who != "mine" {
		t.Errorf("the feed returned %s, want only this server's telemetry", page.Events[0].Data)
	}
}

// event.reject is the hub's own word to the plugin. The raw queue endpoint must
// not be usable to forge one, for the same reason it refuses manifest.* and
// action.*.
func TestQueueEndpointRefusesEventTypes(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, _ := enrolledSession(t, server, "event forgery")

	code := errorCode(t, server, http.MethodPost,
		"/api/v1/servers/"+created.Server.ID+"/envelopes", testAdminToken,
		map[string]any{"type": "event.reject", "body": map[string]any{}}, http.StatusConflict)
	if code != "conflict" {
		t.Errorf("code = %q, want conflict", code)
	}
}
