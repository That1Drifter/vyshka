package hub_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/That1Drifter/vyshka/hub"
	"github.com/That1Drifter/vyshka/hub/store"
)

// playerEventRecord mirrors one event of a player profile (spec section 8.6).
type playerEventRecord struct {
	eventRecord
	Roles []string `json:"roles"`
}

type playerEventPage struct {
	Events     []playerEventRecord `json:"events"`
	NextCursor string              `json:"nextCursor"`
}

func identity(id string) map[string]any {
	return map[string]any{"platform": "steam", "id": id}
}

func playerEvents(t *testing.T, server *hub.Server, bearer, platform, playerID string, parameters url.Values) playerEventPage {
	t.Helper()
	path := "/api/v1/players/" + url.PathEscape(platform) + "/" + url.PathEscape(playerID) + "/events"
	if encoded := parameters.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var page playerEventPage
	if status := call(t, server, http.MethodGet, path, bearer, nil, &page); status != http.StatusOK {
		t.Fatalf("GET %s: status = %d, want 200", path, status)
	}
	return page
}

func eventTypesOf(page playerEventPage) []string {
	types := make([]string, 0, len(page.Events))
	for _, event := range page.Events {
		types = append(types, event.Type)
	}
	return types
}

// The profile finds an event by an identity at the top level of its data,
// under whatever member name, on every server, and says which roles it
// holds; nested identities and near-misses are data, not references.
func TestPlayerProfileEventsSpanServers(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	first, firstLive := enrolledSession(t, server, "profile one")
	second, secondLive := enrolledSession(t, server, "profile two")
	base := time.Now().UTC().Add(-time.Hour)
	at := func(minutes int) string { return base.Add(time.Duration(minutes) * time.Minute).Format(time.RFC3339) }

	pollNow(t, server, first.Server.ID, firstLive.SessionToken, map[string]any{
		"envelopes": []map[string]any{eventBatchEnvelope(1,
			map[string]any{"t": "core.player.connect", "ts": at(1), "data": map[string]any{"player": identity("A"), "name": "Anna"}},
			map[string]any{"t": "core.player.death", "ts": at(2), "data": map[string]any{
				"player": identity("B"), "killer": identity("A"), "position": []float64{1, 2, 3},
			}},
			// Nested: a crew list is data like any other (spec section 8.2).
			map[string]any{"t": "core.vehicle.destroy", "ts": at(3), "data": map[string]any{
				"crew": []any{map[string]any{"player": identity("A")}},
			}},
			// Near-misses: a member name in the wrong case, an empty id, a
			// platform over its bound, and a non-string id.
			map[string]any{"t": "example-mod.misses", "ts": at(4), "data": map[string]any{
				"a": map[string]any{"Platform": "steam", "id": "A"},
				"b": map[string]any{"platform": "steam", "id": ""},
				"c": map[string]any{"platform": strings.Repeat("p", 65), "id": "A"},
				"d": map[string]any{"platform": "steam", "id": 7},
			}},
		)},
	})
	pollNow(t, server, second.Server.ID, secondLive.SessionToken, map[string]any{
		"envelopes": []map[string]any{eventBatchEnvelope(1,
			// One identity under two roles is one event with both.
			map[string]any{"t": "core.player.death", "ts": at(5), "data": map[string]any{
				"player": identity("A"), "killer": identity("A"), "cause": "self",
			}},
			map[string]any{"t": "core.player.chat", "ts": at(6), "data": map[string]any{
				"player": map[string]any{"platform": "steam", "id": "A", "extra": true}, "text": "hello",
			}},
		)},
	})

	page := playerEvents(t, server, testAdminToken, "steam", "A", nil)
	want := []string{"core.player.chat", "core.player.death", "core.player.death", "core.player.connect"}
	if got := eventTypesOf(page); !reflect.DeepEqual(got, want) {
		t.Fatalf("profile of A lists %v, want %v newest first", got, want)
	}
	wantRoles := [][]string{{"player"}, {"killer", "player"}, {"killer"}, {"player"}}
	wantServers := []string{second.Server.ID, second.Server.ID, first.Server.ID, first.Server.ID}
	for i, event := range page.Events {
		if !reflect.DeepEqual(event.Roles, wantRoles[i]) {
			t.Errorf("event %d roles = %v, want %v", i, event.Roles, wantRoles[i])
		}
		if event.ServerID != wantServers[i] {
			t.Errorf("event %d serverId = %s, want %s", i, event.ServerID, wantServers[i])
		}
	}
	// A type filter narrows the profile as it narrows the feed.
	if got := eventTypesOf(playerEvents(t, server, testAdminToken, "steam", "A",
		url.Values{"type": {"core.player.death"}})); len(got) != 2 {
		t.Errorf("the deaths of A are %v, want two", got)
	}
	// A walk of one event per page meets all four in order and ends.
	var walked []string
	cursor := ""
	for pages := 0; pages < 10; pages++ {
		parameters := url.Values{"limit": {"1"}}
		if cursor != "" {
			parameters.Set("cursor", cursor)
		}
		one := playerEvents(t, server, testAdminToken, "steam", "A", parameters)
		for _, event := range one.Events {
			walked = append(walked, event.ID)
		}
		if one.NextCursor == "" {
			break
		}
		cursor = one.NextCursor
	}
	if len(walked) != 4 {
		t.Errorf("a walk one event at a time met %d events, want 4", len(walked))
	}

	if b := playerEvents(t, server, testAdminToken, "steam", "B", nil); len(b.Events) != 1 || b.Events[0].Roles[0] != "player" {
		t.Errorf("profile of B = %+v, want the one death as player", b.Events)
	}
	if nobody := playerEvents(t, server, testAdminToken, "steam", "nobody", nil); len(nobody.Events) != 0 {
		t.Errorf("an unknown identity has %d events, want an empty profile", len(nobody.Events))
	}
}

// The profile reads through the grants the server feed does (section 10.3)
// and through the binding by leaving other servers out (section 10.2).
func TestPlayerProfileEventsAreNarrowed(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	first, firstLive := enrolledSession(t, server, "narrow one")
	second, secondLive := enrolledSession(t, server, "narrow two")
	pollNow(t, server, first.Server.ID, firstLive.SessionToken, map[string]any{
		"envelopes": []map[string]any{eventBatchEnvelope(1,
			map[string]any{"t": "core.player.connect", "data": map[string]any{"player": identity("A")}})},
	})
	pollNow(t, server, second.Server.ID, secondLive.SessionToken, map[string]any{
		"envelopes": []map[string]any{eventBatchEnvelope(1,
			map[string]any{"t": "core.player.death", "data": map[string]any{"player": identity("A")}})},
	})

	deaths, _ := mintToken(t, server, "deaths only", "events:read:core.player.death")
	if got := eventTypesOf(playerEvents(t, server, deaths, "steam", "A", nil)); !reflect.DeepEqual(got, []string{"core.player.death"}) {
		t.Errorf("a death-only token reads %v, want the death alone", got)
	}
	if code := errorCode(t, server, http.MethodGet, "/api/v1/players/steam/A/events?type=core.player.*",
		deaths, nil, http.StatusForbidden); code != "forbidden" {
		t.Errorf("an uncovered explicit type answered %s, want forbidden", code)
	}

	bound, _ := mintBoundToken(t, server, "first only", []string{first.Server.ID}, "events:read")
	page := playerEvents(t, server, bound, "steam", "A", nil)
	if len(page.Events) != 1 || page.Events[0].ServerID != first.Server.ID {
		t.Errorf("a token bound to the first server reads %+v, want its one event", page.Events)
	}

	notes, _ := mintToken(t, server, "notes only", "notes:read")
	if code := errorCode(t, server, http.MethodGet, "/api/v1/players/steam/A/events",
		notes, nil, http.StatusForbidden); code != "forbidden" {
		t.Errorf("a token without events:read answered %s, want forbidden", code)
	}
}

// The identity in the path is bounded like the one in a snapshot, and a
// platform carrying a slash is one percent-encoded segment.
func TestPlayerProfileIdentityBounds(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, live := enrolledSession(t, server, "bounds")
	pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{
		"envelopes": []map[string]any{eventBatchEnvelope(1,
			map[string]any{"t": "core.player.connect", "data": map[string]any{
				"player": map[string]any{"platform": "odd/platform", "id": "x y"},
			}})},
	})
	if page := playerEvents(t, server, testAdminToken, "odd/platform", "x y", nil); len(page.Events) != 1 {
		t.Errorf("an encoded identity reads %d events, want 1", len(page.Events))
	}
	for _, path := range []string{
		"/api/v1/players/" + strings.Repeat("p", 65) + "/A/events",
		"/api/v1/players/steam/" + strings.Repeat("i", 129) + "/events",
		"/api/v1/players/steam/%FF/events",
	} {
		if code := errorCode(t, server, http.MethodGet, path, testAdminToken, nil, http.StatusBadRequest); code != "bad_request" {
			t.Errorf("GET %s answered %s, want bad_request", path, code)
		}
	}
}

// The action list is the player-context actions whose referenceKey is the
// id, narrowed to the codes and servers the token may read.
func TestPlayerProfileActions(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	first, _ := manifestFirst(t, server, "actions one")
	second, _ := manifestFirst(t, server, "actions two")
	heal := func(serverID, player string) string {
		id, _ := dispatchAction(t, server, serverID, map[string]any{
			"code": "example-mod.heal", "context": "player", "referenceKey": player,
			"params": map[string]any{"amount": 10},
		})
		return id
	}
	older := heal(first.Server.ID, "A")
	newer := heal(second.Server.ID, "A")
	heal(first.Server.ID, "B")

	type actionsPage struct {
		Actions    []actionRecord `json:"actions"`
		NextCursor string         `json:"nextCursor"`
	}
	read := func(bearer, query string) actionsPage {
		t.Helper()
		var page actionsPage
		if status := call(t, server, http.MethodGet, "/api/v1/players/steam/A/actions"+query, bearer, nil, &page); status != http.StatusOK {
			t.Fatalf("read actions: status = %d, want 200", status)
		}
		return page
	}
	page := read(testAdminToken, "")
	if len(page.Actions) != 2 || page.Actions[0].ID != newer || page.Actions[1].ID != older {
		t.Fatalf("actions of A = %+v, want the two heals newest first", page.Actions)
	}
	first1 := read(testAdminToken, "?limit=1")
	if len(first1.Actions) != 1 || first1.NextCursor == "" {
		t.Fatalf("a one-action page = %+v, want one action and a cursor", first1)
	}
	rest := read(testAdminToken, "?limit=1&cursor="+url.QueryEscape(first1.NextCursor))
	if len(rest.Actions) != 1 || rest.Actions[0].ID != older || rest.NextCursor != "" {
		t.Errorf("the second page = %+v, want the older heal and no cursor", rest)
	}

	other, _ := mintToken(t, server, "other codes", "actions:read:other-mod.*")
	if page := read(other, ""); len(page.Actions) != 0 {
		t.Errorf("a token for other codes reads %d actions, want none", len(page.Actions))
	}
	// A namespace grant narrows by prefix, which on Postgres is a range scan
	// that only byte order gets right (migration 0015).
	namespace, _ := mintToken(t, server, "example-mod reader", "actions:read:example-mod.*")
	if page := read(namespace, ""); len(page.Actions) != 2 {
		t.Errorf("a token reading example-mod.* sees %d actions, want both heals", len(page.Actions))
	}
	dispatcher, _ := mintToken(t, server, "healer", "actions:dispatch:example-mod.heal")
	if page := read(dispatcher, ""); len(page.Actions) != 2 {
		t.Errorf("a heal dispatcher reads %d actions, want both: dispatch implies read", len(page.Actions))
	}
	bound, _ := mintBoundToken(t, server, "second only", []string{second.Server.ID}, "actions:read")
	if page := read(bound, ""); len(page.Actions) != 1 || page.Actions[0].ID != newer {
		t.Errorf("a token bound to the second server reads %+v, want its one heal", page.Actions)
	}
	if code := errorCode(t, server, http.MethodGet, "/api/v1/players/steam/A/actions?cursor=nonsense",
		testAdminToken, nil, http.StatusBadRequest); code != "bad_request" {
		t.Errorf("a cursor the hub never issued answered %s, want bad_request", code)
	}
}

type noteRecord struct {
	ID     string `json:"id"`
	Player struct {
		Platform string `json:"platform"`
		ID       string `json:"id"`
	} `json:"player"`
	Text      string `json:"text"`
	CreatedAt string `json:"createdAt"`
	CreatedBy struct {
		TokenID   string `json:"tokenId"`
		TokenName string `json:"tokenName"`
	} `json:"createdBy"`
}

// Notes are written under notes:write, read under notes:read, credited to
// the token that wrote them, and deleted only from their own identity.
func TestPlayerNotes(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	writer, writerRecord := mintToken(t, server, "moderator-anna", "notes:write")
	reader, _ := mintToken(t, server, "reader", "notes:read")

	var created struct {
		Note noteRecord `json:"note"`
	}
	if status := call(t, server, http.MethodPost, "/api/v1/players/steam/A/notes", writer,
		map[string]any{"text": "Warned for camping the airfield."}, &created); status != http.StatusCreated {
		t.Fatalf("write note: status = %d, want 201", status)
	}
	if created.Note.CreatedBy.TokenName != "moderator-anna" || created.Note.CreatedBy.TokenID != writerRecord.ID ||
		created.Note.Player.ID != "A" || created.Note.Text != "Warned for camping the airfield." {
		t.Errorf("created note = %+v, want the text credited to moderator-anna", created.Note)
	}
	// Notes in one millisecond tie on id, which is not creation order; the
	// pause makes "newest first" observable.
	time.Sleep(5 * time.Millisecond)
	var second struct {
		Note noteRecord `json:"note"`
	}
	call(t, server, http.MethodPost, "/api/v1/players/steam/A/notes", testAdminToken,
		map[string]any{"text": "Second."}, &second)

	var listed struct {
		Notes      []noteRecord `json:"notes"`
		NextCursor string       `json:"nextCursor"`
	}
	if status := call(t, server, http.MethodGet, "/api/v1/players/steam/A/notes", reader, nil, &listed); status != http.StatusOK {
		t.Fatalf("read notes: status = %d, want 200", status)
	}
	if len(listed.Notes) != 2 || listed.Notes[0].ID != second.Note.ID {
		t.Fatalf("notes = %+v, want both, newest first", listed.Notes)
	}
	if listed.Notes[0].CreatedBy.TokenID != "" || listed.Notes[0].CreatedBy.TokenName != "bootstrap" {
		t.Errorf("a bootstrap note is credited to %+v, want an empty id and the bootstrap name", listed.Notes[0].CreatedBy)
	}

	for _, refusal := range []struct {
		method, path, bearer string
		body                 any
	}{
		{http.MethodPost, "/api/v1/players/steam/A/notes", reader, map[string]any{"text": "nope"}},
		{http.MethodGet, "/api/v1/players/steam/A/notes", writer, nil},
		{http.MethodDelete, "/api/v1/players/steam/A/notes/" + created.Note.ID, reader, nil},
	} {
		if code := errorCode(t, server, refusal.method, refusal.path, refusal.bearer, refusal.body, http.StatusForbidden); code != "forbidden" {
			t.Errorf("%s %s answered %s, want forbidden", refusal.method, refusal.path, code)
		}
	}
	for _, body := range []any{
		map[string]any{},
		map[string]any{"text": "   \n\t"},
		map[string]any{"text": strings.Repeat("é", 4001)},
		map[string]any{"text": 7},
	} {
		if code := errorCode(t, server, http.MethodPost, "/api/v1/players/steam/A/notes", writer, body, http.StatusBadRequest); code != "bad_request" {
			t.Errorf("note body %v answered %s, want bad_request", body, code)
		}
	}
	var longest struct {
		Note noteRecord `json:"note"`
	}
	if status := call(t, server, http.MethodPost, "/api/v1/players/steam/A/notes", writer,
		map[string]any{"text": strings.Repeat("é", 4000)}, &longest); status != http.StatusCreated {
		t.Errorf("a note of exactly 4000 characters: status = %d, want 201", status)
	}

	if code := errorCode(t, server, http.MethodDelete, "/api/v1/players/steam/B/notes/"+created.Note.ID,
		writer, nil, http.StatusNotFound); code != "not_found" {
		t.Errorf("deleting A's note through B answered %s, want not_found", code)
	}
	if status := call(t, server, http.MethodDelete, "/api/v1/players/steam/A/notes/"+created.Note.ID, writer, nil, nil); status != http.StatusNoContent {
		t.Errorf("delete note: status = %d, want 204", status)
	}
	if code := errorCode(t, server, http.MethodDelete, "/api/v1/players/steam/A/notes/"+created.Note.ID,
		writer, nil, http.StatusNotFound); code != "not_found" {
		t.Errorf("deleting a deleted note answered %s, want not_found", code)
	}

	// The audit record names the note and the identity, never the text.
	audit := queryAudit(t, server, url.Values{"tokenId": {writerRecord.ID}})
	found := false
	for _, record := range audit.Records {
		if strings.Contains(string(record.Detail), "camping") {
			t.Errorf("the audit detail %s carries the note's text", record.Detail)
		}
		if record.Method == http.MethodPost && strings.Contains(string(record.Detail), created.Note.ID) {
			found = true
		}
	}
	if !found {
		t.Error("no audit record names the note's creation")
	}
}

// A webhook's redact paths strip members from what leaves the hub, top-level,
// nested, and through arrays, and leave every other byte as the plugin wrote
// it; the stored event is untouched.
func TestWebhookRedaction(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, live := enrolledSession(t, server, "redaction")
	public := newTestReceiver(t, http.StatusOK)
	admin := newTestReceiver(t, http.StatusOK)
	registerWebhook(t, server, map[string]any{
		"url": public.server.URL, "events": []string{"core.player.death"},
		"redact": []string{"position", "killer.position", "crew.position", "absent.path"},
	})
	registerWebhook(t, server, map[string]any{"url": admin.server.URL, "events": []string{"core.player.death"}})

	var raw json.RawMessage = json.RawMessage(`{"player":{"platform":"steam","id":"A"},"position":[1,2,3],` +
		`"killer":{"platform":"steam","id":"B","position":[4,5,6]},"crew":[{"seat":0,"position":[7]},"x",{"seat":1}],` +
		`"big":12345678901234567890,"text":"<b>&</b>"}`)
	pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{
		"envelopes": []map[string]any{eventBatchEnvelope(1, map[string]any{"t": "core.player.death", "data": raw})},
	})
	public.awaitReceived(t, 1, 10*time.Second)
	admin.awaitReceived(t, 1, 10*time.Second)

	var delivered struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(public.get(0).Body, &delivered); err != nil {
		t.Fatalf("decode delivery: %v", err)
	}
	body := string(delivered.Data)
	for _, gone := range []string{`"position"`, `[1,2,3]`, `[4,5,6]`, `[7]`} {
		if strings.Contains(body, gone) {
			t.Errorf("redacted delivery %s still carries %s", body, gone)
		}
	}
	// The test's own encoder escaped the markup on the way in, so the plugin's
	// bytes are the escaped form, and those are what must survive.
	escapedMarkup, _ := json.Marshal("<b>&</b>")
	for _, kept := range []string{`"seat":0`, `"x"`, `"seat":1`, `12345678901234567890`, string(escapedMarkup), `"id":"B"`} {
		if !strings.Contains(body, kept) {
			t.Errorf("redacted delivery %s lost %s", body, kept)
		}
	}
	if !strings.Contains(string(admin.get(0).Body), "[1,2,3]") {
		t.Error("the webhook without redaction lost the position")
	}
	stored := queryEvents(t, server, created.Server.ID, nil)
	var compact bytes.Buffer
	if len(stored.Events) != 1 || json.Compact(&compact, stored.Events[0].Data) != nil ||
		!strings.Contains(compact.String(), "[1,2,3]") {
		t.Errorf("redaction reached the stored event: %s", compact.String())
	}

	for _, bad := range [][]string{{""}, {"a..b"}, {"a.*"}, {"a b"}, {strings.Repeat("a", 129)}, make([]string, 21)} {
		if code := errorCode(t, server, http.MethodPost, "/api/v1/webhooks", testAdminToken,
			map[string]any{"url": public.server.URL, "redact": bad}, http.StatusBadRequest); code != "bad_request" {
			t.Errorf("redact %q answered %s, want bad_request", bad, code)
		}
	}
}

// The discord template renders from the redacted data, so a stripped member
// cannot come back as prose.
func TestWebhookRedactionPrecedesTheDiscordTemplate(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, live := enrolledSession(t, server, "redaction discord")
	receiver := newTestReceiver(t, http.StatusNoContent)
	registerWebhook(t, server, map[string]any{
		"url": receiver.server.URL, "events": []string{"example-mod.secret"}, "template": "discord",
		"redact": []string{"hideout"},
	})
	pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{
		"envelopes": []map[string]any{eventBatchEnvelope(1, map[string]any{
			"t": "example-mod.secret", "data": map[string]any{"hideout": "under-the-bridge", "visible": "shown"},
		})},
	})
	receiver.awaitReceived(t, 1, 10*time.Second)
	body := string(receiver.get(0).Body)
	if strings.Contains(body, "under-the-bridge") || strings.Contains(body, "hideout") {
		t.Errorf("the discord body %s carries a redacted member", body)
	}
	if !strings.Contains(body, "shown") {
		t.Errorf("the discord body %s lost the unredacted member", body)
	}
}

// Audit records are webhook material for a filter that names them, never for
// the catch-all, and subscribing needs admin (spec sections 11.1 and 11.2).
func TestAuditRecordsAreOptInWebhookMaterial(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	audit := newTestReceiver(t, http.StatusOK)
	everything := newTestReceiver(t, http.StatusOK)
	auditID, _ := registerWebhook(t, server, map[string]any{"url": audit.server.URL, "events": []string{"audit.*"}})
	registerWebhook(t, server, map[string]any{"url": everything.server.URL})

	// A mutation: the note write is audited, and the audit webhook hears it.
	call(t, server, http.MethodPost, "/api/v1/players/steam/A/notes", testAdminToken, map[string]any{"text": "hello"}, nil)
	deadline := time.Now().Add(10 * time.Second)
	var payload struct {
		Type     string `json:"type"`
		ServerID *string
		Data     struct {
			Method    string          `json:"method"`
			Path      string          `json:"path"`
			Status    int             `json:"status"`
			TokenName string          `json:"tokenName"`
			Detail    json.RawMessage `json:"detail"`
		} `json:"data"`
	}
	for found := false; !found; {
		if time.Now().After(deadline) {
			t.Fatal("the audit webhook never heard the note's creation")
		}
		for i := 0; i < audit.count(); i++ {
			var one map[string]json.RawMessage
			_ = json.Unmarshal(audit.get(i).Body, &one)
			if strings.Contains(string(one["data"]), "/notes") {
				_ = json.Unmarshal(audit.get(i).Body, &payload)
				if _, present := one["serverId"]; present {
					t.Errorf("an audit delivery naming no server carries serverId %s", one["serverId"])
				}
				found = true
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if payload.Type != "audit.recorded" || payload.Data.Method != http.MethodPost ||
		payload.Data.Status != http.StatusCreated || payload.Data.TokenName != "bootstrap" {
		t.Errorf("audit delivery = %+v, want the POST credited to bootstrap", payload)
	}

	// The catch-all heard the registrations as nothing: it was registered
	// after the first and hears only what matches it, which audit never does.
	time.Sleep(1500 * time.Millisecond)
	for i := 0; i < everything.count(); i++ {
		if strings.Contains(string(everything.get(i).Body), "audit.recorded") {
			t.Fatalf("the catch-all webhook received an audit record: %s", everything.get(i).Body)
		}
	}

	// Coverage: everything but admin cannot subscribe to the audit log, by
	// name or by namespace, and can still subscribe to everything else.
	manager, _ := mintToken(t, server, "manager", "webhooks:manage", "events:read", "actions:read", "servers:read")
	for _, filter := range [][]string{{"audit.*"}, {"audit.recorded"}, {"core.*", "audit.recorded"}} {
		if code := errorCode(t, server, http.MethodPost, "/api/v1/webhooks", manager,
			map[string]any{"url": everything.server.URL, "events": filter}, http.StatusForbidden); code != "forbidden" {
			t.Errorf("a non-admin registering %v answered %s, want forbidden", filter, code)
		}
	}
	if status := call(t, server, http.MethodPost, "/api/v1/webhooks", manager,
		map[string]any{"url": everything.server.URL}, nil); status != http.StatusCreated {
		t.Errorf("a non-admin registering the catch-all: status = %d, want 201", status)
	}
	// Nor can it send an audit body again: a replay is an export of the type
	// it carries (spec section 11.5).
	deliveries := webhookDeliveries(t, server, auditID)
	if len(deliveries) == 0 {
		t.Fatal("the audit webhook has no deliveries to replay")
	}
	deliveryID, _ := deliveries[0]["id"].(string)
	if code := errorCode(t, server, http.MethodPost, "/api/v1/webhooks/"+auditID+"/deliveries/"+deliveryID+"/replay",
		manager, nil, http.StatusForbidden); code != "forbidden" {
		t.Errorf("a non-admin replaying an audit delivery answered %s, want forbidden", code)
	}
}

// Telemetry may not speak in the audit namespace any more than in the action
// and server ones (spec section 8.1).
func TestAuditNamespaceIsReserved(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, live := enrolledSession(t, server, "reserved audit")
	result := pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{
		"envelopes": []map[string]any{eventBatchEnvelope(1, map[string]any{"t": "audit.recorded"})},
	})
	if result.Ack != 1 {
		t.Fatalf("ack = %d, want the refused batch acked", result.Ack)
	}
	if page := queryEvents(t, server, created.Server.ID, nil); len(page.Events) != 0 {
		t.Errorf("an audit.* event was stored: %+v", page.Events)
	}
}

// A filter naming the audit namespace that predates the notification was a
// telemetry grant, not a request for the access record: a row as migration
// 0020 leaves it hears no audit record until a token that reads the audit log
// saves the webhook again (spec section 11.1).
func TestLegacyAuditFilterIsNotAGrant(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	receiver := newTestReceiver(t, http.StatusOK)
	legacy, err := server.Store().CreateWebhook(context.Background(), store.Webhook{
		ID: "01LEGACYAUDITWEBHOOK000000", URL: receiver.server.URL, Secret: "legacy-secret",
		Template: "generic-json", Events: []string{"audit.*"},
	})
	if err != nil {
		t.Fatalf("seed the legacy webhook: %v", err)
	}
	createServer(t, server, "a mutation to audit", "test-game")
	time.Sleep(2 * time.Second)
	for i := 0; i < receiver.count(); i++ {
		if strings.Contains(string(receiver.get(i).Body), "audit.recorded") {
			t.Fatalf("a legacy audit.* filter received an audit record: %s", receiver.get(i).Body)
		}
	}

	// An admin edit re-authorizes it, and from then on it hears the log.
	if status := call(t, server, http.MethodPatch, "/api/v1/webhooks/"+legacy.ID, testAdminToken,
		map[string]any{"paused": false}, nil); status != http.StatusOK {
		t.Fatalf("admin edit: status = %d, want 200", status)
	}
	createServer(t, server, "a mutation after the grant", "test-game")
	receiver.awaitReceived(t, 1, 10*time.Second)
	if !strings.Contains(string(receiver.get(0).Body), "audit.recorded") {
		t.Errorf("after the admin edit the webhook received %s, want an audit record", receiver.get(0).Body)
	}
}

// U+0000 is legal in a JSON string and illegal in Postgres text. An event
// whose identity carries one is stored like any other, with that member read
// as data rather than an identity; a path or a note carrying one is refused.
func TestNulIsNeverAnIdentity(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, live := enrolledSession(t, server, "nul identity")
	nul := string(rune(0))
	result := pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{
		"envelopes": []map[string]any{eventBatchEnvelope(1, map[string]any{
			"t": "core.player.death", "data": map[string]any{
				"player": identity("A" + nul + "B"), "killer": identity("A"),
			},
		})},
	})
	if result.Ack != 1 {
		t.Fatalf("ack = %d, want the batch acked", result.Ack)
	}
	if page := queryEvents(t, server, created.Server.ID, nil); len(page.Events) != 1 {
		t.Fatalf("the feed holds %d events, want the one sent", len(page.Events))
	}
	if page := playerEvents(t, server, testAdminToken, "steam", "A", nil); len(page.Events) != 1 ||
		!reflect.DeepEqual(page.Events[0].Roles, []string{"killer"}) {
		t.Errorf("the killer's profile is %+v, want the death with the killer role alone", page.Events)
	}
	for _, path := range []string{"/api/v1/players/steam/A%00B/events", "/api/v1/players/st%00eam/A/notes"} {
		if code := errorCode(t, server, http.MethodGet, path, testAdminToken, nil, http.StatusBadRequest); code != "bad_request" {
			t.Errorf("GET %s answered %s, want bad_request", path, code)
		}
	}
	if code := errorCode(t, server, http.MethodPost, "/api/v1/players/steam/A/notes", testAdminToken,
		map[string]any{"text": "a" + nul + "b"}, http.StatusBadRequest); code != "bad_request" {
		t.Errorf("a note carrying U+0000 answered %s, want bad_request", code)
	}
}

// A member that is a dot segment is reachable by a client that sends the
// request target as written, the dots percent-encoded; it is browsers and
// most URL libraries that remove it (spec section 8.6).
func TestDotSegmentIdentityIsReachableEncoded(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, live := enrolledSession(t, server, "dot identity")
	pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{
		"envelopes": []map[string]any{eventBatchEnvelope(1, map[string]any{
			"t": "core.player.connect", "data": map[string]any{"player": identity("..")},
		})},
	})
	var page playerEventPage
	if status := call(t, server, http.MethodGet, "/api/v1/players/steam/%2e%2e/events", testAdminToken, nil, &page); status != http.StatusOK {
		t.Fatalf("GET the encoded dot identity: status = %d, want 200", status)
	}
	if len(page.Events) != 1 {
		t.Errorf("the dot identity's profile holds %d events, want 1", len(page.Events))
	}
}
