package hub_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/That1Drifter/vyshka/hub"
	"github.com/That1Drifter/vyshka/hub/internal/dbtest"
)

// The hub half of the context enumeration exchange (spec section 6.2): the
// Admin API read that asks the plugin, the cache in front of it, and the
// poll path that matches the reply to the question.

// contextEntriesRecord mirrors the Admin API view on the wire.
type contextEntriesRecord struct {
	Context      string            `json:"context"`
	Entries      []json.RawMessage `json:"entries"`
	Reason       *string           `json:"reason"`
	EnumeratedAt string            `json:"enumeratedAt"`
}

// entriesOutcome is how one background read ended.
type entriesOutcome struct {
	status int
	record contextEntriesRecord
	code   string
	body   string
}

// newEnumerationServer boots a hub with the enumeration bounds a test wants:
// a short hold so the timeout path is testable, and a cache bound of its own.
func newEnumerationServer(t *testing.T, timeout, cacheTTL time.Duration) *hub.Server {
	t.Helper()
	server, err := hub.New(context.Background(), hub.Config{
		DatabaseURL:             dbtest.URL(t),
		AdminToken:              testAdminToken,
		Logger:                  slog.New(slog.NewJSONHandler(io.Discard, nil)),
		ContextEnumerateTimeout: timeout,
		ContextCacheTTL:         cacheTTL,
	})
	if err != nil {
		t.Fatalf("boot hub: %v", err)
	}
	t.Cleanup(func() { server.Close() })
	return server
}

// territoryManifest declares one custom context and one action whose param
// draws on it (section 6.1's `context` annotation).
func territoryManifest(revision int64) map[string]any {
	body := healManifest(revision, map[string]any{
		"code": "example-mod.claim", "name": "Claim territory", "context": "example-mod.territory",
		"namespace": "example-mod", "danger": "none",
		"params": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"neighbour": map[string]any{"type": "string", "context": "example-mod.territory"},
			},
		},
	})
	body["contexts"] = []map[string]any{{
		"id": "example-mod.territory", "name": "Territory", "namespace": "example-mod",
	}}
	return body
}

// enumerateEnvelope finds the context.enumerate in a poll answer and returns
// its requestId, or "" when none arrived. The highest seq delivered is
// returned either way, so the caller can ack everything it saw.
func enumerateEnvelope(t *testing.T, result pollResult, want string) (requestID string, highest int64) {
	t.Helper()
	for _, delivered := range result.Envelopes {
		highest = max(highest, delivered.Seq)
		if delivered.Type != "context.enumerate" {
			continue
		}
		var body struct {
			RequestID string `json:"requestId"`
			Context   string `json:"context"`
		}
		if err := json.Unmarshal(delivered.Body, &body); err != nil {
			t.Fatalf("decode context.enumerate body %s: %v", delivered.Body, err)
		}
		if body.Context != want {
			t.Fatalf("context.enumerate names context %q, want %q", body.Context, want)
		}
		if body.RequestID == "" || len(body.RequestID) > 128 {
			t.Fatalf("context.enumerate requestId %q is not a hub id of at most 128 characters", body.RequestID)
		}
		requestID = body.RequestID
	}
	return requestID, highest
}

// awaitEnumerate polls until a context.enumerate for the context arrives,
// acking as it goes, and returns its requestId and the ack to report next.
func awaitEnumerate(t *testing.T, server *hub.Server, sessionToken string, ack int64, want string) (string, int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		result := poll(t, server, sessionToken, map[string]any{"ack": ack})
		requestID, highest := enumerateEnvelope(t, result, want)
		ack = max(ack, highest)
		if requestID != "" {
			return requestID, ack
		}
	}
	t.Fatalf("no context.enumerate for %q reached the plugin", want)
	return "", ack
}

// entriesEnvelope frames one context.entries reply. extra overrides or adds
// body members, so a test can send a malformed one.
func entriesEnvelope(seq int64, requestID, contextID string, entries []map[string]any, extra map[string]any) map[string]any {
	body := map[string]any{"requestId": requestID, "context": contextID, "entries": entries}
	if entries == nil {
		body["entries"] = []map[string]any{}
	}
	for key, value := range extra {
		body[key] = value
	}
	return map[string]any{
		"v": 1, "id": "context-entries-" + strconv.FormatInt(seq, 10),
		"type": "context.entries", "seq": seq,
		"ts": time.Now().UTC().Format(time.RFC3339), "body": body,
	}
}

// readEntries starts a GET in the background and hands back its outcome,
// since the hub holds the request until the plugin answers.
func readEntries(server *hub.Server, serverID, contextID, query string) <-chan entriesOutcome {
	done := make(chan entriesOutcome, 1)
	go func() {
		request, _ := http.NewRequest(http.MethodGet,
			"/api/v1/servers/"+serverID+"/contexts/"+contextID+"/entries"+query, nil)
		request.Header.Set("Authorization", "Bearer "+testAdminToken)
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, request)
		outcome := entriesOutcome{status: recorder.Code, body: recorder.Body.String()}
		if recorder.Code == http.StatusOK {
			_ = json.Unmarshal(recorder.Body.Bytes(), &outcome.record)
		} else {
			var failure struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			_ = json.Unmarshal(recorder.Body.Bytes(), &failure)
			outcome.code = failure.Error.Code
		}
		done <- outcome
	}()
	return done
}

func TestContextEnumerationRoundTrip(t *testing.T) {
	t.Parallel()
	// A poll with nothing to deliver holds for the session's 5 s, so the
	// bound is well above one hold: what is graded is the matching, not the
	// clock. Every poll that carries a reply goes through pollNow, which
	// queues a nudge so the hub answers it at once.
	server := newEnumerationServer(t, 30*time.Second, time.Hour)
	created, live := enrolledSession(t, server, "context round trip")
	poll(t, server, live.SessionToken, map[string]any{
		"envelopes": []map[string]any{publishEnvelope(1, territoryManifest(1))},
	})

	// The read holds; the plugin's next poll carries the question.
	pending := readEntries(server, created.Server.ID, "example-mod.territory", "")
	requestID, ack := awaitEnumerate(t, server, live.SessionToken, 0, "example-mod.territory")

	// A reply with a different requestId is unsolicited: acked and ignored,
	// and the read keeps waiting.
	stray := pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{
		"ack":       ack,
		"envelopes": []map[string]any{entriesEnvelope(2, "not-the-question", "example-mod.territory", []map[string]any{{"referenceKey": "stray", "label": "Stray"}}, nil)},
	})
	for _, delivered := range stray.Envelopes {
		ack = max(ack, delivered.Seq)
	}
	if stray.Ack != 2 {
		t.Fatalf("ack = %d after an unsolicited context.entries, want 2: it is acked and ignored (section 6.2)", stray.Ack)
	}
	select {
	case outcome := <-pending:
		t.Fatalf("the read answered %d on an unsolicited reply", outcome.status)
	case <-time.After(200 * time.Millisecond):
	}

	entries := []map[string]any{
		{"referenceKey": "north-ridge", "label": "North Ridge", "position": []float64{4231.5, 300.2, 10620}},
		{"referenceKey": "south-bay", "label": "South Bay", "data": map[string]any{"owner": "clan-a"}},
	}
	answered := pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{
		"ack":       ack,
		"envelopes": []map[string]any{entriesEnvelope(3, requestID, "example-mod.territory", entries, nil)},
	})
	if answered.Ack != 3 {
		t.Fatalf("ack = %d after the reply, want 3", answered.Ack)
	}
	for _, delivered := range answered.Envelopes {
		ack = max(ack, delivered.Seq)
	}
	outcome := <-pending
	if outcome.status != http.StatusOK {
		t.Fatalf("read: status = %d (%s), want 200", outcome.status, outcome.body)
	}
	record := outcome.record
	if record.Context != "example-mod.territory" || len(record.Entries) != 2 || record.Reason != nil {
		t.Fatalf("read = %s, want the two entries and no reason", outcome.body)
	}
	if !strings.Contains(string(record.Entries[1]), `"owner"`) || !strings.Contains(string(record.Entries[1]), `"clan-a"`) {
		t.Errorf("entries[1] = %s, want the plugin's data carried verbatim", record.Entries[1])
	}
	if _, err := time.Parse(time.RFC3339, record.EnumeratedAt); err != nil {
		t.Errorf("enumeratedAt = %q, want RFC 3339", record.EnumeratedAt)
	}

	// A second read is served from the cache: nothing new reaches the plugin.
	cached := <-readEntries(server, created.Server.ID, "example-mod.territory", "")
	if cached.status != http.StatusOK || cached.record.EnumeratedAt != record.EnumeratedAt {
		t.Fatalf("cached read = %+v, want the same answer", cached)
	}
	quiet := pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{"ack": ack})
	second, highest := enumerateEnvelope(t, quiet, "example-mod.territory")
	if second != "" {
		t.Fatalf("a cached read sent a second context.enumerate (%s); a hub SHOULD cache briefly (section 6.2)", second)
	}
	ack = max(ack, highest)

	// refresh=true asks again, whatever the cache holds.
	refreshed := readEntries(server, created.Server.ID, "example-mod.territory", "?refresh=true")
	again, ack := awaitEnumerate(t, server, live.SessionToken, ack, "example-mod.territory")
	if again == requestID {
		t.Fatal("the refresh reused the first requestId; every question carries its own")
	}
	pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{
		"ack":       ack,
		"envelopes": []map[string]any{entriesEnvelope(4, again, "example-mod.territory", []map[string]any{{"referenceKey": "only", "label": "Only"}}, nil)},
	})
	if outcome := <-refreshed; outcome.status != http.StatusOK || len(outcome.record.Entries) != 1 {
		t.Fatalf("refreshed read = %+v, want the fresh single entry", outcome)
	}
}

func TestContextEnumerationRefusals(t *testing.T) {
	t.Parallel()
	server := newEnumerationServer(t, 5*time.Second, time.Hour)
	created := createServer(t, server, "context refusals", "test-game")
	path := "/api/v1/servers/" + created.Server.ID + "/contexts/example-mod.territory/entries"

	// No manifest yet.
	if code := errorCode(t, server, http.MethodGet, path, testAdminToken, nil, http.StatusNotFound); code != "not_found" {
		t.Errorf("before a manifest: code = %q, want not_found", code)
	}
	if code := errorCode(t, server, http.MethodGet, "/api/v1/servers/no-such-server/contexts/x/entries", testAdminToken, nil, http.StatusNotFound); code != "not_found" {
		t.Errorf("unknown server: code = %q, want not_found", code)
	}
	if code := errorCode(t, server, http.MethodGet, path, "", nil, http.StatusUnauthorized); code != "unauthorized" {
		t.Errorf("no token: code = %q, want unauthorized", code)
	}

	credentials := enroll(t, server, created.Enrollment.Token, "test-game")
	live := startSession(t, server, credentials, 5)
	poll(t, server, live.SessionToken, map[string]any{
		"envelopes": []map[string]any{publishEnvelope(1, territoryManifest(1))},
	})

	// A context the manifest does not declare, and the built-in ones, are
	// not enumerated here.
	for _, undeclared := range []string{"example-mod.nothing", "player", "world"} {
		if code := errorCode(t, server, http.MethodGet,
			"/api/v1/servers/"+created.Server.ID+"/contexts/"+undeclared+"/entries",
			testAdminToken, nil, http.StatusNotFound); code != "not_found" {
			t.Errorf("context %q: code = %q, want not_found", undeclared, code)
		}
	}

	// No live session: refused, not queued.
	if err := server.Store().RevokeCredentials(context.Background(), created.Server.ID); err != nil {
		t.Fatal(err)
	}
	if code := errorCode(t, server, http.MethodGet, path, testAdminToken, nil, http.StatusConflict); code != "link_down" {
		t.Errorf("no session: code = %q, want link_down", code)
	}
	var view struct {
		PendingEnvelopeCount int `json:"pendingEnvelopeCount"`
	}
	call(t, server, http.MethodGet, "/api/v1/servers/"+created.Server.ID, testAdminToken, nil, &view)
	if view.PendingEnvelopeCount != 0 {
		t.Errorf("pendingEnvelopeCount = %d after a link_down refusal; nothing may be queued for a plugin that is not there", view.PendingEnvelopeCount)
	}
}

func TestContextEnumerationTimesOutAndLateReplyIsIgnored(t *testing.T) {
	t.Parallel()
	server := newEnumerationServer(t, 300*time.Millisecond, time.Hour)
	created, live := enrolledSession(t, server, "context timeout")
	poll(t, server, live.SessionToken, map[string]any{
		"envelopes": []map[string]any{publishEnvelope(1, territoryManifest(1))},
	})

	pending := readEntries(server, created.Server.ID, "example-mod.territory", "")
	requestID, ack := awaitEnumerate(t, server, live.SessionToken, 0, "example-mod.territory")
	outcome := <-pending
	if outcome.status != http.StatusGatewayTimeout || outcome.code != "enumeration_timeout" {
		t.Fatalf("read after the bound: status %d code %q, want 504 enumeration_timeout", outcome.status, outcome.code)
	}

	// The late reply is acked and ignored; a fresh read asks again rather
	// than being served the answer to a question the hub gave up on.
	late := poll(t, server, live.SessionToken, map[string]any{
		"ack":       ack,
		"envelopes": []map[string]any{entriesEnvelope(2, requestID, "example-mod.territory", []map[string]any{{"referenceKey": "late", "label": "Late"}}, nil)},
	})
	if late.Ack != 2 {
		t.Fatalf("ack = %d after a late reply, want 2", late.Ack)
	}
	fresh := readEntries(server, created.Server.ID, "example-mod.territory", "")
	second, _ := awaitEnumerate(t, server, live.SessionToken, ack, "example-mod.territory")
	if second == requestID {
		t.Fatal("the read after a timeout reused the expired requestId; a question given up on is not asked again under the same id")
	}
	if outcome := <-fresh; outcome.status != http.StatusGatewayTimeout {
		t.Fatalf("second read: status %d, want the timeout again since nobody answered", outcome.status)
	}
}

func TestContextEnumerationInvalidReply(t *testing.T) {
	t.Parallel()
	server := newEnumerationServer(t, 30*time.Second, time.Hour)
	created, live := enrolledSession(t, server, "context invalid reply")
	poll(t, server, live.SessionToken, map[string]any{
		"envelopes": []map[string]any{publishEnvelope(1, territoryManifest(1))},
	})

	cases := []struct {
		name    string
		entries []map[string]any
		extra   map[string]any
	}{
		{"entriesNull", nil, map[string]any{"entries": nil}},
		{"entriesNotArray", nil, map[string]any{"entries": "none"}},
		{"keyMissing", []map[string]any{{"label": "No key"}}, nil},
		{"labelNull", []map[string]any{{"referenceKey": "k", "label": nil}}, nil},
		{"badPosition", []map[string]any{{"referenceKey": "k", "label": "L", "position": []any{1}}}, nil},
		{"badData", []map[string]any{{"referenceKey": "k", "label": "L", "data": "text"}}, nil},
		{"wrongContext", []map[string]any{{"referenceKey": "k", "label": "L"}}, map[string]any{"context": "example-mod.other"}},
		{"contextNotString", []map[string]any{{"referenceKey": "k", "label": "L"}}, map[string]any{"context": 123}},
		{"badReason", []map[string]any{{"referenceKey": "k", "label": "L"}}, map[string]any{"reason": 5}},
	}
	// The plugin's own seq continues after the manifest's 1.
	var ack int64
	outSeq := int64(1)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pending := readEntries(server, created.Server.ID, "example-mod.territory", "?refresh=true")
			requestID, next := awaitEnumerate(t, server, live.SessionToken, ack, "example-mod.territory")
			ack = next
			outSeq++
			acked := pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{
				"ack": ack, "envelopes": []map[string]any{entriesEnvelope(outSeq, requestID, "example-mod.territory", tc.entries, tc.extra)},
			})
			if acked.Ack != outSeq {
				t.Fatalf("ack = %d after an invalid reply, want %d: a refused reply is acked too (section 6.2)", acked.Ack, outSeq)
			}
			for _, delivered := range acked.Envelopes {
				ack = max(ack, delivered.Seq)
			}
			outcome := <-pending
			if outcome.status != http.StatusBadGateway || outcome.code != "enumeration_invalid" {
				t.Fatalf("read: status %d code %q, want 502 enumeration_invalid (%s)", outcome.status, outcome.code, outcome.body)
			}
		})
	}

	// None of the refused replies was cached: a plain read still has to ask,
	// and a good answer to that lands. The cache bound is an hour, so a
	// refused reply that had been cached would have been served here.
	recovered := readEntries(server, created.Server.ID, "example-mod.territory", "")
	requestID, next := awaitEnumerate(t, server, live.SessionToken, ack, "example-mod.territory")
	ack = next
	outSeq++
	pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{
		"ack": ack, "envelopes": []map[string]any{entriesEnvelope(outSeq, requestID, "example-mod.territory", []map[string]any{{"referenceKey": "good", "label": "Good"}}, nil)},
	})
	if outcome := <-recovered; outcome.status != http.StatusOK || len(outcome.record.Entries) != 1 {
		t.Fatalf("read after the refused replies: %+v, want the good answer", outcome)
	}
}

func TestContextEnumerationCoalescesAndInvalidatesOnRepublish(t *testing.T) {
	t.Parallel()
	server := newEnumerationServer(t, 30*time.Second, time.Hour)
	created, live := enrolledSession(t, server, "context coalesce")
	poll(t, server, live.SessionToken, map[string]any{
		"envelopes": []map[string]any{publishEnvelope(1, territoryManifest(1))},
	})

	// Three concurrent reads, one question.
	var readers []<-chan entriesOutcome
	for range 3 {
		readers = append(readers, readEntries(server, created.Server.ID, "example-mod.territory", ""))
	}
	requestID, ack := awaitEnumerate(t, server, live.SessionToken, 0, "example-mod.territory")
	// Give any second question a chance to show up before answering.
	time.Sleep(100 * time.Millisecond)
	settle := pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{"ack": ack})
	if extra, _ := enumerateEnvelope(t, settle, "example-mod.territory"); extra != "" {
		t.Fatalf("a second context.enumerate (%s) for three concurrent reads; concurrent reads share one question (section 6.2)", extra)
	}
	for _, delivered := range settle.Envelopes {
		ack = max(ack, delivered.Seq)
	}
	shared := pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{
		"ack":       ack,
		"envelopes": []map[string]any{entriesEnvelope(2, requestID, "example-mod.territory", []map[string]any{{"referenceKey": "one", "label": "One"}}, nil)},
	})
	for _, delivered := range shared.Envelopes {
		ack = max(ack, delivered.Seq)
	}
	var wg sync.WaitGroup
	for i, reader := range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if outcome := <-reader; outcome.status != http.StatusOK || len(outcome.record.Entries) != 1 {
				t.Errorf("reader %d: %+v, want the shared answer", i, outcome)
			}
		}()
	}
	wg.Wait()

	// A republished manifest invalidates the cached answer, however young.
	republished := pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{
		"ack":       ack,
		"envelopes": []map[string]any{publishEnvelope(3, territoryManifest(2))},
	})
	for _, delivered := range republished.Envelopes {
		ack = max(ack, delivered.Seq)
	}
	pending := readEntries(server, created.Server.ID, "example-mod.territory", "")
	again, ack := awaitEnumerate(t, server, live.SessionToken, ack, "example-mod.territory")
	pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{
		"ack":       ack,
		"envelopes": []map[string]any{entriesEnvelope(4, again, "example-mod.territory", nil, map[string]any{"reason": "nothing claimed yet"})},
	})
	outcome := <-pending
	if outcome.status != http.StatusOK || len(outcome.record.Entries) != 0 || outcome.record.Reason == nil || *outcome.record.Reason != "nothing claimed yet" {
		t.Fatalf("read after republish = %+v, want the empty answer with its reason", outcome)
	}
}

func TestManifestContextAnnotationMustBeDeclared(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, live := enrolledSession(t, server, "context annotation")

	// Declared: accepted, and the annotation is stored verbatim for UIs.
	poll(t, server, live.SessionToken, map[string]any{
		"envelopes": []map[string]any{publishEnvelope(1, territoryManifest(1))},
	})
	record := getManifest(t, server, created.Server.ID)
	if record.Revision != 1 {
		t.Fatalf("revision = %d, want 1: a manifest whose annotation names a declared context is accepted", record.Revision)
	}

	// Undeclared: rejected at the annotation's path, the stored manifest untouched.
	undeclared := territoryManifest(2)
	undeclared["contexts"] = []any{}
	result := poll(t, server, live.SessionToken, map[string]any{
		"envelopes": []map[string]any{publishEnvelope(2, undeclared)},
	})
	if result.Ack != 2 {
		t.Fatalf("ack = %d, want 2: a rejected manifest is still acked", result.Ack)
	}
	var reject json.RawMessage
	for _, delivered := range result.Envelopes {
		if delivered.Type == "manifest.reject" {
			reject = delivered.Body
		}
	}
	if reject == nil {
		t.Fatalf("no manifest.reject in %+v", result.Envelopes)
	}
	if !strings.Contains(string(reject), "actions[0].params.properties.neighbour.context") || !strings.Contains(string(reject), "not declared") {
		t.Fatalf("manifest.reject = %s, want a fault at the annotation's path naming the undeclared context", reject)
	}
	if getManifest(t, server, created.Server.ID).Revision != 1 {
		t.Fatal("the rejected manifest replaced the stored one")
	}

	// A manifest declaring a built-in context as its own is rejected at the
	// declaration, so the enumeration read can never be talked into asking
	// for one (section 6.2).
	builtin := territoryManifest(3)
	builtin["contexts"] = []map[string]any{
		{"id": "example-mod.territory", "name": "Territory", "namespace": "example-mod"},
		{"id": "player", "name": "Players", "namespace": "example-mod"},
	}
	result = poll(t, server, live.SessionToken, map[string]any{
		"envelopes": []map[string]any{publishEnvelope(3, builtin)},
	})
	reject = nil
	for _, delivered := range result.Envelopes {
		if delivered.Type == "manifest.reject" {
			reject = delivered.Body
		}
	}
	if reject == nil || !strings.Contains(string(reject), "contexts[1].id") || !strings.Contains(string(reject), "built-in") {
		t.Fatalf("manifest.reject = %s, want a fault at contexts[1].id naming the built-in context", reject)
	}
	if getManifest(t, server, created.Server.ID).Revision != 1 {
		t.Fatal("the manifest declaring a built-in context replaced the stored one")
	}

	// A null annotation reads as no annotation (section 6.4).
	nulled := territoryManifest(4)
	nulled["actions"].([]map[string]any)[0]["params"] = map[string]any{
		"type":       "object",
		"properties": map[string]any{"neighbour": map[string]any{"type": "integer", "context": nil}},
	}
	poll(t, server, live.SessionToken, map[string]any{
		"envelopes": []map[string]any{publishEnvelope(4, nulled)},
	})
	if getManifest(t, server, created.Server.ID).Revision != 4 {
		t.Fatal("a null context annotation was refused; null reads as absent")
	}
	// Back to the annotated manifest for the dispatch below.
	poll(t, server, live.SessionToken, map[string]any{
		"envelopes": []map[string]any{publishEnvelope(5, territoryManifest(5))},
	})

	// The annotation never constrains a dispatch.
	var accepted struct {
		ActionID string `json:"actionId"`
	}
	status := call(t, server, http.MethodPost, "/api/v1/servers/"+created.Server.ID+"/actions", testAdminToken,
		map[string]any{"code": "example-mod.claim", "context": "example-mod.territory", "referenceKey": "anything",
			"params": map[string]any{"neighbour": "not-enumerated"}}, &accepted)
	if status != http.StatusAccepted {
		t.Fatalf("dispatch with a value outside any enumeration: status = %d, want 202 (section 6.1: an annotation, not a constraint)", status)
	}
}
