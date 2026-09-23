package hub_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/That1Drifter/vyshka/hub"
	"github.com/That1Drifter/vyshka/hub/internal/dbtest"
)

// banRecord mirrors a ban as the Admin API reports it (spec section 13.1).
type banRecord struct {
	ID     string `json:"id"`
	Player struct {
		Platform string `json:"platform"`
		ID       string `json:"id"`
	} `json:"player"`
	Reason    string  `json:"reason"`
	Name      string  `json:"name"`
	ServerID  *string `json:"serverId"`
	CreatedAt string  `json:"createdAt"`
	CreatedBy struct {
		TokenID   string `json:"tokenId"`
		TokenName string `json:"tokenName"`
	} `json:"createdBy"`
	ExpiresAt *string `json:"expiresAt"`
	State     string  `json:"state"`
	LiftedAt  *string `json:"liftedAt"`
	LiftedBy  *struct {
		TokenID   string `json:"tokenId"`
		TokenName string `json:"tokenName"`
	} `json:"liftedBy"`
}

type banChange struct {
	Ban      banRecord `json:"ban"`
	Revision int64     `json:"revision"`
}

type banPage struct {
	Revision   int64       `json:"revision"`
	Bans       []banRecord `json:"bans"`
	NextCursor string      `json:"nextCursor"`
}

// pluginBanPage mirrors one page of the plugin's pull (spec section 13.3).
type pluginBanPage struct {
	Revision int64 `json:"revision"`
	Bans     []struct {
		ID     string `json:"id"`
		Player struct {
			Platform string `json:"platform"`
			ID       string `json:"id"`
		} `json:"player"`
		Reason    string  `json:"reason"`
		Name      string  `json:"name"`
		ExpiresAt *string `json:"expiresAt"`
	} `json:"bans"`
	NextCursor string `json:"nextCursor"`
}

func banBody(playerID, reason string, extra map[string]any) map[string]any {
	body := map[string]any{"player": identity(playerID), "reason": reason}
	for key, value := range extra {
		body[key] = value
	}
	return body
}

func createBan(t *testing.T, server *hub.Server, bearer string, body map[string]any) banChange {
	t.Helper()
	var change banChange
	if status := call(t, server, http.MethodPost, "/api/v1/bans", bearer, body, &change); status != http.StatusCreated {
		t.Fatalf("create ban: status = %d, want 201", status)
	}
	return change
}

func liftBan(t *testing.T, server *hub.Server, bearer, banID string) banChange {
	t.Helper()
	var change banChange
	if status := call(t, server, http.MethodPost, "/api/v1/bans/"+banID+"/lift", bearer, nil, &change); status != http.StatusOK {
		t.Fatalf("lift ban: status = %d, want 200", status)
	}
	return change
}

func listBans(t *testing.T, server *hub.Server, bearer string, parameters url.Values) banPage {
	t.Helper()
	path := "/api/v1/bans"
	if encoded := parameters.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var page banPage
	if status := call(t, server, http.MethodGet, path, bearer, nil, &page); status != http.StatusOK {
		t.Fatalf("GET %s: status = %d, want 200", path, status)
	}
	return page
}

func pullBans(t *testing.T, server *hub.Server, sessionToken, method string, parameters url.Values) pluginBanPage {
	t.Helper()
	path := "/plugin/v1/bans"
	if method == http.MethodPost {
		path += "/get"
	}
	if encoded := parameters.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var page pluginBanPage
	if status := call(t, server, method, path, sessionToken, nil, &page); status != http.StatusOK {
		t.Fatalf("%s %s: status = %d, want 200", method, path, status)
	}
	return page
}

// banManifest is the heal manifest with the bans capability declared, or not.
func banManifest(revision int64, capable bool) map[string]any {
	manifest := healManifest(revision)
	if capable {
		manifest["capabilities"] = []string{"bans", "some-future-capability"}
	}
	return manifest
}

// banServer enrolls a server whose plugin has published a manifest, with the
// bans capability or without, and returns it with its session and the next
// seq its fake plugin would use.
func banServer(t *testing.T, server *hub.Server, name string, capable bool) (createdServer, session) {
	t.Helper()
	created, live := enrolledSession(t, server, name)
	pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{
		"envelopes": []map[string]any{publishEnvelope(1, banManifest(1, capable))},
	})
	return created, live
}

type serverBans struct {
	Bans struct {
		Supported       bool    `json:"supported"`
		AppliedRevision *int64  `json:"appliedRevision"`
		AppliedAt       *string `json:"appliedAt"`
	} `json:"bans"`
}

func getServerBans(t *testing.T, server *hub.Server, serverID string) serverBans {
	t.Helper()
	var view serverBans
	if status := call(t, server, http.MethodGet, "/api/v1/servers/"+serverID, testAdminToken, nil, &view); status != http.StatusOK {
		t.Fatalf("get server: status = %d, want 200", status)
	}
	return view
}

// The whole lifecycle of spec section 13.2: a ban is placed, a second ban of
// the same identity is a conflict naming the first, a lift takes it off the
// list and a second lift changes nothing, and the identity can be banned
// again. Every change moves the revision, and nothing else does.
func TestBanLifecycle(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)

	first := createBan(t, server, testAdminToken, banBody("A", "speed hack", map[string]any{"name": "Anna"}))
	if first.Revision != 1 || first.Ban.State != "active" || first.Ban.ID == "" {
		t.Fatalf("first ban = %+v at revision %d, want an active ban at revision 1", first.Ban, first.Revision)
	}
	if first.Ban.Name != "Anna" || first.Ban.Reason != "speed hack" || first.Ban.ExpiresAt != nil || first.Ban.ServerID != nil {
		t.Errorf("first ban carries %+v", first.Ban)
	}
	if first.Ban.CreatedBy.TokenName != "bootstrap" || first.Ban.CreatedBy.TokenID != "" {
		t.Errorf("createdBy = %+v, want the bootstrap credential with an empty tokenId", first.Ban.CreatedBy)
	}

	var failure struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if status := call(t, server, http.MethodPost, "/api/v1/bans", testAdminToken,
		banBody("A", "again", nil), &failure); status != http.StatusConflict {
		t.Fatalf("second ban of A: status = %d, want 409", status)
	}
	if failure.Error.Code != "conflict" || failure.Error.Details["banId"] != first.Ban.ID {
		t.Errorf("second ban of A answered %+v, want conflict naming %s", failure.Error, first.Ban.ID)
	}

	if page := listBans(t, server, testAdminToken, nil); page.Revision != 1 || len(page.Bans) != 1 {
		t.Fatalf("the active list is %+v at revision %d, want the one ban at revision 1", page.Bans, page.Revision)
	}

	lifted := liftBan(t, server, testAdminToken, first.Ban.ID)
	if lifted.Revision != 2 || lifted.Ban.State != "lifted" || lifted.Ban.LiftedAt == nil || lifted.Ban.LiftedBy == nil {
		t.Fatalf("lift answered %+v at revision %d, want a lifted ban at revision 2", lifted.Ban, lifted.Revision)
	}
	again := liftBan(t, server, testAdminToken, first.Ban.ID)
	if again.Revision != 2 || again.Ban.LiftedAt == nil || *again.Ban.LiftedAt != *lifted.Ban.LiftedAt {
		t.Errorf("a second lift answered %+v at revision %d; it must change nothing, the lift time included",
			again.Ban, again.Revision)
	}

	if page := listBans(t, server, testAdminToken, nil); len(page.Bans) != 0 || page.Revision != 2 {
		t.Errorf("after the lift the active list is %+v at revision %d, want empty at 2", page.Bans, page.Revision)
	}
	if page := listBans(t, server, testAdminToken, url.Values{"state": {"all"}}); len(page.Bans) != 1 || page.Bans[0].State != "lifted" {
		t.Errorf("state=all lists %+v, want the lifted ban", page.Bans)
	}

	second := createBan(t, server, testAdminToken, banBody("A", "again", nil))
	if second.Revision != 3 || second.Ban.ID == first.Ban.ID {
		t.Errorf("a ban after the lift = %+v at revision %d, want a new ban at revision 3", second.Ban, second.Revision)
	}
	createBan(t, server, testAdminToken, banBody("B", "griefing", nil))
	history := listBans(t, server, testAdminToken, url.Values{"state": {"all"}, "platform": {"steam"}, "playerId": {"A"}})
	if len(history.Bans) != 2 || history.Bans[0].ID != second.Ban.ID || history.Bans[1].ID != first.Ban.ID {
		t.Errorf("the history of A lists %+v, want both of its bans newest first", history.Bans)
	}

	var one struct {
		Ban banRecord `json:"ban"`
	}
	if status := call(t, server, http.MethodGet, "/api/v1/bans/"+first.Ban.ID, testAdminToken, nil, &one); status != http.StatusOK || one.Ban.State != "lifted" {
		t.Errorf("GET the first ban: status %d, %+v", status, one.Ban)
	}
	for _, path := range []string{"/api/v1/bans/nope", "/api/v1/bans/nope/lift"} {
		method := http.MethodGet
		if strings.HasSuffix(path, "/lift") {
			method = http.MethodPost
		}
		if code := errorCode(t, server, method, path, testAdminToken, nil, http.StatusNotFound); code != "not_found" {
			t.Errorf("%s %s answered %s, want not_found", method, path, code)
		}
	}

	// A walk one ban at a time meets each once.
	seen := map[string]bool{}
	cursor := ""
	for pages := 0; pages < 10; pages++ {
		parameters := url.Values{"state": {"all"}, "limit": {"1"}}
		if cursor != "" {
			parameters.Set("cursor", cursor)
		}
		page := listBans(t, server, testAdminToken, parameters)
		for _, ban := range page.Bans {
			if seen[ban.ID] {
				t.Fatalf("a one-ban walk met %s twice", ban.ID)
			}
			seen[ban.ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(seen) != 3 {
		t.Errorf("a one-ban walk met %d bans, want 3", len(seen))
	}
}

// What a ban request may carry (spec section 13.2), each refusal changing
// nothing.
func TestBanValidation(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	for _, tc := range []struct {
		name string
		body any
		want int
	}{
		{"no player", map[string]any{"reason": "x"}, http.StatusBadRequest},
		{"empty platform", map[string]any{"player": map[string]any{"platform": "", "id": "A"}, "reason": "x"}, http.StatusBadRequest},
		{"id over its bound", banBody(strings.Repeat("i", 129), "x", nil), http.StatusBadRequest},
		{"id carrying U+0000", banBody("A\u0000B", "x", nil), http.StatusBadRequest},
		{"no reason", map[string]any{"player": identity("A")}, http.StatusBadRequest},
		{"blank reason", banBody("A", "   ", nil), http.StatusBadRequest},
		{"reason over its bound", banBody("A", strings.Repeat("r", 201), nil), http.StatusBadRequest},
		{"name over its bound", banBody("A", "x", map[string]any{"name": strings.Repeat("n", 201)}), http.StatusBadRequest},
		{"zero duration", banBody("A", "x", map[string]any{"durationSeconds": 0}), http.StatusBadRequest},
		{"duration past ten years", banBody("A", "x", map[string]any{"durationSeconds": 315360001}), http.StatusBadRequest},
		{"fractional duration", banBody("A", "x", map[string]any{"durationSeconds": 1.5}), http.StatusBadRequest},
		{"unknown server", banBody("A", "x", map[string]any{"serverId": "no-such-server"}), http.StatusNotFound},
	} {
		code := errorCode(t, server, http.MethodPost, "/api/v1/bans", testAdminToken, tc.body, tc.want)
		want := "bad_request"
		if tc.want == http.StatusNotFound {
			want = "not_found"
		}
		if code != want {
			t.Errorf("%s: answered %s, want %s", tc.name, code, want)
		}
	}
	if page := listBans(t, server, testAdminToken, url.Values{"state": {"all"}}); len(page.Bans) != 0 || page.Revision != 0 {
		t.Errorf("after refusals only, the list holds %d bans at revision %d, want none at 0", len(page.Bans), page.Revision)
	}
	for _, query := range []string{"state=lifted", "platform=steam", "playerId=A", "limit=0", "cursor=%21"} {
		if code := errorCode(t, server, http.MethodGet, "/api/v1/bans?"+query, testAdminToken, nil,
			http.StatusBadRequest); code != "bad_request" {
			t.Errorf("GET /api/v1/bans?%s answered %s, want bad_request", query, code)
		}
	}

	// A ban with a duration expires that far from the hub's clock, and one
	// naming a server records it as provenance and in that server's audit view.
	created := createServer(t, server, "provenance", "test-game")
	before := time.Now().UTC()
	change := createBan(t, server, testAdminToken, banBody("A", "x", map[string]any{
		"durationSeconds": 3600, "serverId": created.Server.ID,
	}))
	if change.Ban.ServerID == nil || *change.Ban.ServerID != created.Server.ID {
		t.Errorf("serverId = %v, want %s", change.Ban.ServerID, created.Server.ID)
	}
	if change.Ban.ExpiresAt == nil {
		t.Fatal("a ban with a duration carries no expiresAt")
	}
	expires, err := time.Parse(time.RFC3339, *change.Ban.ExpiresAt)
	if err != nil || expires.Before(before.Add(time.Hour-time.Second)) || expires.After(time.Now().Add(time.Hour+time.Second)) {
		t.Errorf("expiresAt = %s, want an hour from now", *change.Ban.ExpiresAt)
	}
	audit := queryAudit(t, server, url.Values{"serverId": {created.Server.ID}})
	found := false
	for _, record := range audit.Records {
		if record.Path == "/api/v1/bans" && record.Method == http.MethodPost {
			found = true
		}
	}
	if !found {
		t.Errorf("the ban naming %s is not in that server's audit view", created.Server.ID)
	}
}

// The grants of spec section 10.1: reading and managing are separate,
// managing implies reading, a binding cannot carry the power to ban on every
// server but can carry reading the list, which it does not narrow.
func TestBanScopes(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created := createServer(t, server, "scoped", "test-game")

	reader, _ := mintToken(t, server, "ban reader", "bans:read")
	manager, _ := mintToken(t, server, "ban manager", "bans:manage")
	notes, _ := mintToken(t, server, "notes only", "notes:read")

	if code := errorCode(t, server, http.MethodPost, "/api/v1/bans", reader, banBody("A", "x", nil),
		http.StatusForbidden); code != "forbidden" {
		t.Errorf("a reader banning answered %s, want forbidden", code)
	}
	change := createBan(t, server, manager, banBody("A", "x", nil))
	if change.Ban.CreatedBy.TokenName != "ban manager" || change.Ban.CreatedBy.TokenID == "" {
		t.Errorf("createdBy = %+v, want the manager token", change.Ban.CreatedBy)
	}
	if page := listBans(t, server, manager, nil); len(page.Bans) != 1 {
		t.Errorf("bans:manage reads %d bans, want 1: managing implies reading", len(page.Bans))
	}
	if page := listBans(t, server, reader, nil); len(page.Bans) != 1 {
		t.Errorf("bans:read reads %d bans, want 1", len(page.Bans))
	}
	if code := errorCode(t, server, http.MethodPost, "/api/v1/bans/"+change.Ban.ID+"/lift", reader, nil,
		http.StatusForbidden); code != "forbidden" {
		t.Errorf("a reader lifting answered %s, want forbidden", code)
	}
	if code := errorCode(t, server, http.MethodGet, "/api/v1/bans", notes, nil, http.StatusForbidden); code != "forbidden" {
		t.Errorf("a token without a bans grant answered %s, want forbidden", code)
	}
	if code := errorCode(t, server, http.MethodPost, "/api/v1/tokens", testAdminToken, map[string]any{
		"name": "bound banner", "scopes": []string{"bans:manage"}, "servers": []string{created.Server.ID},
	}, http.StatusBadRequest); code != "bad_request" {
		t.Errorf("minting bans:manage onto a bound token answered %s, want bad_request", code)
	}
	for _, scope := range []string{"bans:read:steam", "bans:manage:*", "bans:write"} {
		if code := errorCode(t, server, http.MethodPost, "/api/v1/tokens", testAdminToken, map[string]any{
			"name": "patterned", "scopes": []string{scope},
		}, http.StatusBadRequest); code != "bad_request" {
			t.Errorf("minting %s answered %s, want bad_request", scope, code)
		}
	}
	bound, _ := mintBoundToken(t, server, "bound reader", []string{created.Server.ID}, "bans:read")
	if page := listBans(t, server, bound, nil); len(page.Bans) != 1 {
		t.Errorf("a bound bans:read token reads %d bans, want the whole list", len(page.Bans))
	}
}

// Spec section 13.3: the session response carries the revision, and a walk
// of the list reads one revision whole, in identity order, whatever changes
// halfway, on both spellings.
func TestBanPullWalksOneRevision(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	for _, playerID := range []string{"C", "A", "B"} {
		createBan(t, server, testAdminToken, banBody(playerID, "reason "+playerID, map[string]any{"name": "name " + playerID}))
	}
	_, live := enrolledSession(t, server, "puller")
	if live.Server.BansRevision == nil || *live.Server.BansRevision != 3 {
		t.Fatalf("session response bansRevision = %v, want 3", live.Server.BansRevision)
	}

	first := pullBans(t, server, live.SessionToken, http.MethodGet, url.Values{"limit": {"1"}})
	if first.Revision != 3 || len(first.Bans) != 1 || first.Bans[0].Player.ID != "A" || first.NextCursor == "" {
		t.Fatalf("first page = %+v, want A at revision 3 with a cursor", first)
	}
	for _, c := range first.NextCursor {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			t.Fatalf("cursor %q carries %q, outside letters, digits, - and _", first.NextCursor, c)
		}
	}
	if first.Bans[0].Reason != "reason A" || first.Bans[0].Name != "name A" || first.Bans[0].ExpiresAt != nil {
		t.Errorf("entry = %+v", first.Bans[0])
	}

	// The list changes under the walk: B is lifted and D banned. The walk
	// still reads revision 3, B included and D not, on either spelling: the
	// pages after the first alternate between them.
	var b string
	for _, ban := range listBans(t, server, testAdminToken, nil).Bans {
		if ban.Player.ID == "B" {
			b = ban.ID
		}
	}
	liftBan(t, server, testAdminToken, b)
	createBan(t, server, testAdminToken, banBody("D", "late", nil))

	cursor := first.NextCursor
	walked := []string{"A"}
	methods := []string{http.MethodPost, http.MethodGet}
	for turn := 0; cursor != ""; turn++ {
		if turn > 5 {
			t.Fatal("the walk did not end")
		}
		page := pullBans(t, server, live.SessionToken, methods[turn%2], url.Values{"limit": {"1"}, "cursor": {cursor}})
		if page.Revision != 3 {
			t.Fatalf("a page of the walk was served at revision %d, want 3", page.Revision)
		}
		for _, ban := range page.Bans {
			walked = append(walked, ban.Player.ID)
		}
		cursor = page.NextCursor
	}
	if strings.Join(walked, "") != "ABC" {
		t.Errorf("the walk of revision 3 met %v, want A B C", walked)
	}

	// A new walk reads the list as it stands now.
	fresh := pullBans(t, server, live.SessionToken, http.MethodPost, nil)
	ids := []string{}
	for _, ban := range fresh.Bans {
		ids = append(ids, ban.Player.ID)
	}
	if fresh.Revision != 5 || strings.Join(ids, "") != "ACD" || fresh.NextCursor != "" {
		t.Errorf("a fresh walk = %v at revision %d, want A C D at 5 in one page", ids, fresh.Revision)
	}

	// Refusals: a cursor this hub never issued, a limit that is not a
	// positive integer, a request with no session, and one on the wrong
	// method.
	for _, query := range []string{"cursor=not-a-cursor", "limit=0", "limit=x"} {
		if code := errorCode(t, server, http.MethodGet, "/plugin/v1/bans?"+query, live.SessionToken, nil,
			http.StatusBadRequest); code != "bad_request" {
			t.Errorf("GET /plugin/v1/bans?%s answered %s, want bad_request", query, code)
		}
	}
	if code := errorCode(t, server, http.MethodGet, "/plugin/v1/bans", "", nil, http.StatusUnauthorized); code != "session_invalid" {
		t.Errorf("a pull with no session answered %s, want session_invalid", code)
	}
	if code := errorCode(t, server, http.MethodPost, "/plugin/v1/bans", live.SessionToken, map[string]any{},
		http.StatusMethodNotAllowed); code != "method_not_allowed" {
		t.Errorf("POST /plugin/v1/bans answered %s, want method_not_allowed", code)
	}
}

// Both spellings walk the same pages (spec section 13.3).
func TestBanPullSpellingsAgree(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	for _, playerID := range []string{"E", "B", "D", "A", "C"} {
		createBan(t, server, testAdminToken, banBody(playerID, "r", nil))
	}
	_, live := enrolledSession(t, server, "spellings")
	walk := func(method string) string {
		var met []string
		cursor := ""
		for pages := 0; pages < 10; pages++ {
			parameters := url.Values{"limit": {"2"}}
			if cursor != "" {
				parameters.Set("cursor", cursor)
			}
			page := pullBans(t, server, live.SessionToken, method, parameters)
			for _, ban := range page.Bans {
				met = append(met, ban.Player.ID)
			}
			if page.NextCursor == "" {
				break
			}
			cursor = page.NextCursor
		}
		return strings.Join(met, "")
	}
	if get, post := walk(http.MethodGet), walk(http.MethodPost); get != "ABCDE" || post != get {
		t.Errorf("GET walked %s and POST walked %s, want ABCDE both", get, post)
	}
}

// Spec section 13.3: a change queues bans.changed for the servers whose
// manifest declares the capability and for no other, a burst of changes costs
// a server one unsent notice, and the raw envelope endpoint cannot forge one.
func TestBanChangedNotifiesCapableServers(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	capable, capableLive := banServer(t, server, "capable", true)
	plain, plainLive := banServer(t, server, "plain", false)

	createBan(t, server, testAdminToken, banBody("A", "x", nil))
	createBan(t, server, testAdminToken, banBody("B", "x", nil))
	third := createBan(t, server, testAdminToken, banBody("C", "x", nil))

	result := poll(t, server, capableLive.SessionToken, map[string]any{"ack": 1})
	var notices []wireEnvelope
	for _, envelope := range result.Envelopes {
		if envelope.Type == "bans.changed" {
			notices = append(notices, envelope)
		}
	}
	if len(notices) != 1 {
		t.Fatalf("three changes queued %d bans.changed for the capable server, want 1 refreshed notice", len(notices))
	}
	var body struct {
		Revision int64 `json:"revision"`
	}
	if err := json.Unmarshal(notices[0].Body, &body); err != nil || body.Revision != third.Revision {
		t.Errorf("the notice carries %s, want revision %d", notices[0].Body, third.Revision)
	}

	// Sent, it keeps its body; the next change queues a second notice rather
	// than rewriting one the plugin may already hold.
	fourth := createBan(t, server, testAdminToken, banBody("D", "x", nil))
	result = poll(t, server, capableLive.SessionToken, map[string]any{"ack": result.Envelopes[len(result.Envelopes)-1].Seq})
	if len(result.Envelopes) != 1 || result.Envelopes[0].Type != "bans.changed" {
		t.Fatalf("after the notice was acked, a fourth change delivered %+v, want one bans.changed", result.Envelopes)
	}
	if err := json.Unmarshal(result.Envelopes[0].Body, &body); err != nil || body.Revision != fourth.Revision {
		t.Errorf("the second notice carries %s, want revision %d", result.Envelopes[0].Body, fourth.Revision)
	}

	// The server without the capability was sent nothing: its queue holds
	// only the nudge pollNow puts there.
	nudged := pollNow(t, server, plain.Server.ID, plainLive.SessionToken, map[string]any{"ack": 1})
	for _, envelope := range nudged.Envelopes {
		if envelope.Type == "bans.changed" {
			t.Errorf("a server whose manifest does not declare bans was sent %s", envelope.Body)
		}
	}
	_ = capable

	if code := errorCode(t, server, http.MethodPost, "/api/v1/servers/"+plain.Server.ID+"/envelopes", testAdminToken,
		map[string]any{"type": "bans.changed", "body": map[string]any{"revision": 99}}, http.StatusConflict); code != "conflict" {
		t.Errorf("queueing a raw bans.changed answered %s, want conflict", code)
	}
}

// Spec section 13.4: a bans.applied report lands on the server record, the
// last of a batch winning, and a body without a usable revision is acked and
// ignored.
func TestBansAppliedIsRecorded(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, live := banServer(t, server, "reporter", true)
	if view := getServerBans(t, server, created.Server.ID); !view.Bans.Supported || view.Bans.AppliedRevision != nil {
		t.Fatalf("before any report the server shows %+v, want supported with no revision", view.Bans)
	}
	report := func(seq int64, id string, body any) map[string]any {
		return map[string]any{"v": 1, "id": id, "type": "bans.applied", "seq": seq,
			"ts": time.Now().UTC().Format(time.RFC3339), "body": body}
	}
	result := pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{"envelopes": []map[string]any{
		report(2, "applied-1", map[string]any{"revision": 4}),
		report(3, "applied-2", map[string]any{"revision": 7}),
		report(4, "applied-bad-1", map[string]any{"revision": 1.5}),
		report(5, "applied-bad-2", map[string]any{"revision": -1}),
		report(6, "applied-bad-3", map[string]any{}),
	}})
	if result.Ack != 6 {
		t.Fatalf("ack = %d after five reports, want 6: an unusable report is acked", result.Ack)
	}
	view := getServerBans(t, server, created.Server.ID)
	if view.Bans.AppliedRevision == nil || *view.Bans.AppliedRevision != 7 || view.Bans.AppliedAt == nil {
		t.Errorf("the server shows %+v, want applied revision 7 with a time", view.Bans)
	}

	plain, plainLive := banServer(t, server, "not supported", false)
	if view := getServerBans(t, server, plain.Server.ID); view.Bans.Supported {
		t.Errorf("a server whose manifest does not declare bans shows supported")
	}
	// A report from it is kept all the same (section 13.4).
	pollNow(t, server, plain.Server.ID, plainLive.SessionToken, map[string]any{"envelopes": []map[string]any{
		report(2, "plain-applied", map[string]any{"revision": 3}),
	}})
	if view := getServerBans(t, server, plain.Server.ID); view.Bans.AppliedRevision == nil || *view.Bans.AppliedRevision != 3 {
		t.Errorf("a report from a server without the capability was not kept: %+v", view.Bans)
	}
}

// A capability list outside the section 6.7 bounds rejects the manifest; an
// unknown entry does not.
func TestManifestCapabilitiesAreBounded(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, live := enrolledSession(t, server, "capabilities")
	many := make([]string, 33)
	for i := range many {
		many[i] = "c" + strings.Repeat("x", i)
	}
	seq := int64(0)
	for _, bad := range []any{many, []string{""}, []string{strings.Repeat("c", 65)}, "bans", []any{1}} {
		seq++
		manifest := healManifest(seq)
		manifest["capabilities"] = bad
		pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{
			"envelopes": []map[string]any{publishEnvelope(seq, manifest)},
		})
		if view := getServerBans(t, server, created.Server.ID); view.Bans.Supported {
			t.Fatalf("capabilities %v were accepted", bad)
		}
	}
	seq++
	manifest := healManifest(seq)
	manifest["capabilities"] = []string{"bans", "bans", "unknown-to-this-hub"}
	pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{
		"envelopes": []map[string]any{publishEnvelope(seq, manifest)},
	})
	if view := getServerBans(t, server, created.Server.ID); !view.Bans.Supported {
		t.Error("a manifest declaring bans beside a duplicate and an unknown entry was not taken as supporting bans")
	}
	if got := getManifest(t, server, created.Server.ID).Revision; got != seq {
		t.Errorf("stored manifest revision = %d, want %d", got, seq)
	}
}

// Spec section 13.1: an expired ban reads as expired at once, leaves the
// served list within the sweep's bound with the revision moving, and a
// capable server hears about it.
func TestBanExpiry(t *testing.T) {
	t.Parallel()
	server, err := hub.New(context.Background(), hub.Config{
		DatabaseURL:      dbtest.URL(t),
		AdminToken:       testAdminToken,
		Logger:           slog.New(slog.NewJSONHandler(io.Discard, nil)),
		BanSweepInterval: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("boot hub: %v", err)
	}
	t.Cleanup(func() { server.Close() })
	_, live := banServer(t, server, "expiry", true)

	change := createBan(t, server, testAdminToken, banBody("A", "brief", map[string]any{"durationSeconds": 1}))
	deadline := time.Now().Add(10 * time.Second)
	for {
		page := pullBans(t, server, live.SessionToken, http.MethodGet, nil)
		if len(page.Bans) == 0 {
			if page.Revision <= change.Revision {
				t.Errorf("the expired ban left the list at revision %d, not above %d", page.Revision, change.Revision)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the expired ban was still on the served list 10 s after its expiry")
		}
		time.Sleep(50 * time.Millisecond)
	}
	var one struct {
		Ban banRecord `json:"ban"`
	}
	call(t, server, http.MethodGet, "/api/v1/bans/"+change.Ban.ID, testAdminToken, nil, &one)
	if one.Ban.State != "expired" || one.Ban.LiftedAt != nil {
		t.Errorf("the expired ban reads %+v, want expired and never lifted", one.Ban)
	}
	// Lifting it changes nothing: it is no longer active.
	if again := liftBan(t, server, testAdminToken, change.Ban.ID); again.Ban.State != "expired" || again.Ban.LiftedAt != nil {
		t.Errorf("lifting an expired ban answered %+v, want it unchanged", again.Ban)
	}
	// The identity can be banned again.
	createBan(t, server, testAdminToken, banBody("A", "again", nil))
}
