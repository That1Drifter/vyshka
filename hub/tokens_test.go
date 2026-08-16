package hub_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/That1Drifter/vyshka/hub"
)

// tokenRecord mirrors the Admin API token view on the wire (spec section 10).
type tokenRecord struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Scopes    []string `json:"scopes"`
	CreatedAt string   `json:"createdAt"`
	CreatedBy string   `json:"createdBy"`
	ExpiresAt *string  `json:"expiresAt"`
	RevokedAt *string  `json:"revokedAt"`
}

type auditRecord struct {
	ID            string          `json:"id"`
	At            string          `json:"at"`
	TokenID       string          `json:"tokenId"`
	TokenName     string          `json:"tokenName"`
	Method        string          `json:"method"`
	Path          string          `json:"path"`
	Status        int             `json:"status"`
	SourceIP      string          `json:"sourceIp"`
	PayloadDigest string          `json:"payloadDigest"`
	ServerID      string          `json:"serverId"`
	Detail        json.RawMessage `json:"detail"`
}

type auditPage struct {
	Records    []auditRecord `json:"records"`
	NextCursor string        `json:"nextCursor"`
}

// mintToken creates a scoped token and returns its secret alongside its record.
func mintToken(t *testing.T, server *hub.Server, name string, scopes ...string) (string, tokenRecord) {
	t.Helper()

	var response struct {
		Token  tokenRecord `json:"token"`
		Secret string      `json:"secret"`
	}
	status := call(t, server, http.MethodPost, "/api/v1/tokens", testAdminToken,
		map[string]any{"name": name, "scopes": scopes}, &response)
	if status != http.StatusCreated {
		t.Fatalf("mint token %q: status = %d, want 201", name, status)
	}
	if response.Secret == "" {
		t.Fatal("mint token: response carried no secret")
	}
	if response.Token.ID == "" {
		t.Fatal("mint token: response carried no token id")
	}
	return response.Secret, response.Token
}

func queryAudit(t *testing.T, server *hub.Server, parameters url.Values) auditPage {
	t.Helper()

	path := "/api/v1/audit"
	if encoded := parameters.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var page auditPage
	if status := call(t, server, http.MethodGet, path, testAdminToken, nil, &page); status != http.StatusOK {
		t.Fatalf("query audit: status = %d, want 200", status)
	}
	return page
}

// The requirement of issue #8, and the one section 10 is written around: a
// token scoped to one action code can dispatch that code and nothing else, and
// the dispatch shows up in the audit log.
func TestNarrowTokenDispatchesOnlyItsOwnCode(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, _ := manifestFirst(t, server, "narrow dispatch")
	serverID := created.Server.ID

	secret, minted := mintToken(t, server, "heal only", "actions:dispatch:example-mod.heal")

	var accepted struct {
		ActionID string `json:"actionId"`
	}
	status := call(t, server, http.MethodPost, "/api/v1/servers/"+serverID+"/actions", secret,
		map[string]any{"code": "example-mod.heal", "params": map[string]any{"amount": 25}}, &accepted)
	if status != http.StatusAccepted {
		t.Fatalf("dispatch of the granted code: status = %d, want 202", status)
	}

	// Any other code is refused, even one the manifest declares nothing about:
	// the scope check runs before the manifest lookup, so a token cannot use
	// the difference between 403 and unknown_action to enumerate a manifest.
	for _, code := range []string{"example-mod.wipe", "core.player.kick", "example-modular.heal"} {
		if got := errorCode(t, server, http.MethodPost, "/api/v1/servers/"+serverID+"/actions",
			secret, map[string]any{"code": code}, http.StatusForbidden); got != "forbidden" {
			t.Errorf("dispatch of %q: error code = %q, want forbidden", code, got)
		}
	}

	// Dispatching implies reading back what was dispatched.
	var record actionRecord
	if status := call(t, server, http.MethodGet, "/api/v1/actions/"+accepted.ActionID,
		secret, nil, &record); status != http.StatusOK {
		t.Fatalf("read back its own action: status = %d, want 200", status)
	}

	// It reaches nothing else on the Admin API.
	for _, one := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v1/servers"},
		{http.MethodGet, "/api/v1/servers/" + serverID},
		{http.MethodGet, "/api/v1/servers/" + serverID + "/events"},
		{http.MethodGet, "/api/v1/servers/" + serverID + "/manifest"},
		{http.MethodGet, "/api/v1/tokens"},
		{http.MethodGet, "/api/v1/audit"},
	} {
		if got := errorCode(t, server, one.method, one.path, secret, nil,
			http.StatusForbidden); got != "forbidden" {
			t.Errorf("%s %s: error code = %q, want forbidden", one.method, one.path, got)
		}
	}
	if got := errorCode(t, server, http.MethodPost, "/api/v1/tokens", secret,
		map[string]any{"name": "escalation", "scopes": []string{"admin"}},
		http.StatusForbidden); got != "forbidden" {
		t.Errorf("minting from a narrow token: error code = %q, want forbidden", got)
	}

	// The successful dispatch, and only the successful one, is in the log with
	// the code it ran and the token that ran it.
	page := queryAudit(t, server, url.Values{"tokenId": {minted.ID}})
	dispatches := 0
	for _, record := range page.Records {
		if record.Status != http.StatusAccepted {
			continue
		}
		dispatches++
		if record.ServerID != serverID {
			t.Errorf("audit record names server %q, want %q", record.ServerID, serverID)
		}
		if record.TokenName != "heal only" {
			t.Errorf("audit record names token %q, want %q", record.TokenName, "heal only")
		}
		if record.PayloadDigest == "" {
			t.Error("audit record of a request with a body carries no payload digest")
		}
		var detail struct {
			Code     string `json:"code"`
			ActionID string `json:"actionId"`
		}
		if err := json.Unmarshal(record.Detail, &detail); err != nil {
			t.Fatalf("decode audit detail %q: %v", record.Detail, err)
		}
		if detail.Code != "example-mod.heal" || detail.ActionID != accepted.ActionID {
			t.Errorf("audit detail = %+v, want the heal dispatch and its action id", detail)
		}
	}
	if dispatches != 1 {
		t.Errorf("the log holds %d accepted dispatches for this token, want 1", dispatches)
	}

	// The refusals are recorded too. An attempt to use a credential outside its
	// grant is exactly what an operator reads this log to find.
	refusals := 0
	for _, record := range page.Records {
		if record.Status == http.StatusForbidden {
			refusals++
		}
	}
	if refusals != 4 {
		t.Errorf("the log holds %d refused mutations, want the three bad dispatches and the mint", refusals)
	}
}

// An idempotency key is client-chosen and keyed only on the server, so the
// action it names may carry a code the presenting token cannot dispatch. The
// scope check on the request's own code does not cover that, and without a
// second check on the returned action's code the retry contract hands a narrow
// token the id and state of an action it could never have dispatched.
func TestIdempotencyKeyCannotLeakAnOutOfScopeAction(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, live := enrolledSession(t, server, "idempotency scope")
	serverID := created.Server.ID
	pollNow(t, server, serverID, live.SessionToken, map[string]any{
		"envelopes": []map[string]any{publishEnvelope(1, healManifest(1,
			map[string]any{"code": "example-mod.heal", "name": "Heal", "namespace": "example-mod"},
			map[string]any{"code": "example-mod.wipe", "name": "Wipe", "namespace": "example-mod"},
		))},
	})

	// The operator binds a key to a code the narrow token may not dispatch.
	privileged, _ := dispatchAction(t, server, serverID, map[string]any{
		"code": "example-mod.wipe", "idempotencyKey": "shared-key",
	})

	secret, _ := mintToken(t, server, "heal only", "actions:dispatch:example-mod.heal")
	path := "/api/v1/servers/" + serverID + "/actions"

	var leaked struct {
		ActionID string `json:"actionId"`
		State    string `json:"state"`
	}
	status := call(t, server, http.MethodPost, path, secret, map[string]any{
		"code": "example-mod.heal", "params": map[string]any{"amount": 1},
		"idempotencyKey": "shared-key",
	}, &leaked)
	if status != http.StatusForbidden {
		t.Fatalf("replaying another token's idempotency key: status = %d, want 403 "+
			"(it returned action %q, state %q)", status, leaked.ActionID, leaked.State)
	}
	if leaked.ActionID == privileged {
		t.Error("the response carried the id of an action outside the token's grant")
	}

	// The same key with a code the token does hold still honors the contract.
	firstID, _ := dispatchAction(t, server, serverID, map[string]any{
		"code": "example-mod.heal", "params": map[string]any{"amount": 5},
		"idempotencyKey": "own-key",
	})
	var replay struct {
		ActionID string `json:"actionId"`
	}
	if status := call(t, server, http.MethodPost, path, secret, map[string]any{
		"code": "example-mod.heal", "params": map[string]any{"amount": 5},
		"idempotencyKey": "own-key",
	}, &replay); status != http.StatusAccepted {
		t.Fatalf("replaying its own key: status = %d, want 202", status)
	}
	if replay.ActionID != firstID {
		t.Errorf("replay returned %q, want the original %q", replay.ActionID, firstID)
	}
}

// A read scope is not a write scope, and the implication in section 10 runs
// only one way.
func TestActionReadScopeDoesNotGrantDispatch(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, _ := manifestFirst(t, server, "read only actions")
	serverID := created.Server.ID

	actionID, _ := dispatchAction(t, server, serverID,
		map[string]any{"code": "example-mod.heal", "params": map[string]any{"amount": 10}})

	secret, _ := mintToken(t, server, "watcher", "actions:read:example-mod.*")

	var record actionRecord
	if status := call(t, server, http.MethodGet, "/api/v1/actions/"+actionID, secret, nil,
		&record); status != http.StatusOK {
		t.Fatalf("read an action within the grant: status = %d, want 200", status)
	}
	if got := errorCode(t, server, http.MethodPost, "/api/v1/servers/"+serverID+"/actions",
		secret, map[string]any{"code": "example-mod.heal", "params": map[string]any{"amount": 1}},
		http.StatusForbidden); got != "forbidden" {
		t.Errorf("dispatch from a read-only token: error code = %q, want forbidden", got)
	}
}

// A namespace-scoped read narrows the feed rather than gating it, and refuses
// an explicit filter it does not cover rather than quietly narrowing that.
func TestEventScopeNarrowsTheFeed(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, live := enrolledSession(t, server, "scoped events")
	serverID := created.Server.ID

	pollNow(t, server, serverID, live.SessionToken, map[string]any{
		"envelopes": []map[string]any{eventBatchEnvelope(1,
			map[string]any{"t": "core.player.death"},
			map[string]any{"t": "core.player.chat"},
			map[string]any{"t": "example-mod.raid.started"},
			map[string]any{"t": "example-mod.raid.ended"},
		)},
	})

	secret, _ := mintToken(t, server, "raid watcher", "events:read:example-mod.*")
	path := "/api/v1/servers/" + serverID + "/events"

	// No filter means "what I may see", not "everything".
	var page eventPage
	if status := call(t, server, http.MethodGet, path, secret, nil, &page); status != http.StatusOK {
		t.Fatalf("unfiltered read: status = %d, want 200", status)
	}
	if len(page.Events) != 2 {
		t.Fatalf("unfiltered read returned %d events, want only the 2 in the granted namespace", len(page.Events))
	}
	for _, event := range page.Events {
		if !strings.HasPrefix(event.Type, "example-mod.") {
			t.Errorf("unfiltered read leaked %q, which is outside the grant", event.Type)
		}
	}

	// A filter inside the grant is answered.
	for _, filter := range []string{"example-mod.*", "example-mod.raid.started", "example-mod.raid.*"} {
		var narrowed eventPage
		if status := call(t, server, http.MethodGet, path+"?type="+url.QueryEscape(filter),
			secret, nil, &narrowed); status != http.StatusOK {
			t.Errorf("filter %q inside the grant: status = %d, want 200", filter, status)
		}
	}

	// A filter outside it is refused, not silently narrowed. That includes a
	// filter that only partly overlaps the grant: answering it with the
	// covered part would look exactly like a complete answer.
	for _, filter := range []string{"*", "core.*", "core.player.death", "example-modular.*"} {
		if got := errorCode(t, server, http.MethodGet, path+"?type="+url.QueryEscape(filter),
			secret, nil, http.StatusForbidden); got != "forbidden" {
			t.Errorf("filter %q outside the grant: error code = %q, want forbidden", filter, got)
		}
	}
	if got := errorCode(t, server, http.MethodGet,
		path+"?type=example-mod.*&type=core.*", secret, nil, http.StatusForbidden); got != "forbidden" {
		t.Errorf("a partly covered filter set: error code = %q, want forbidden", got)
	}

	// A malformed pattern is still the bad request it always was: a caller
	// debugging a typo must not be sent looking at their scopes.
	if got := errorCode(t, server, http.MethodGet, path+"?type=not%20a%20type",
		secret, nil, http.StatusBadRequest); got != "bad_request" {
		t.Errorf("a malformed filter: error code = %q, want bad_request", got)
	}
}

func TestTokenLifecycleAndRevocation(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)

	secret, minted := mintToken(t, server, "panel", "servers:read", "servers:read", "events:read")
	if len(minted.Scopes) != 2 {
		t.Errorf("scopes = %v, want the duplicate collapsed", minted.Scopes)
	}
	if minted.RevokedAt != nil || minted.ExpiresAt != nil {
		t.Errorf("a fresh token reports revokedAt %v and expiresAt %v", minted.RevokedAt, minted.ExpiresAt)
	}

	var listed struct {
		Tokens []tokenRecord `json:"tokens"`
	}
	if status := call(t, server, http.MethodGet, "/api/v1/tokens", testAdminToken, nil,
		&listed); status != http.StatusOK {
		t.Fatalf("list tokens: status = %d, want 200", status)
	}
	if len(listed.Tokens) != 1 || listed.Tokens[0].ID != minted.ID {
		t.Fatalf("list tokens = %+v, want the one minted token", listed.Tokens)
	}
	// The secret is returned once, at mint, and never again.
	if strings.Contains(strings.ToLower(mustJSON(t, listed)), strings.ToLower(secret)) {
		t.Error("the token list leaked a secret")
	}

	if status := call(t, server, http.MethodGet, "/api/v1/servers", secret, nil, nil); status != http.StatusOK {
		t.Fatalf("a live token: status = %d, want 200", status)
	}

	if status := call(t, server, http.MethodDelete, "/api/v1/tokens/"+minted.ID,
		testAdminToken, nil, nil); status != http.StatusNoContent {
		t.Fatalf("revoke: status = %d, want 204", status)
	}
	if got := errorCode(t, server, http.MethodGet, "/api/v1/servers", secret, nil,
		http.StatusUnauthorized); got != "unauthorized" {
		t.Errorf("a revoked token: error code = %q, want unauthorized", got)
	}

	// The record survives revocation, so the audit log's references to it keep
	// resolving.
	listed.Tokens = nil
	call(t, server, http.MethodGet, "/api/v1/tokens", testAdminToken, nil, &listed)
	if len(listed.Tokens) != 1 || listed.Tokens[0].RevokedAt == nil {
		t.Errorf("after revocation the list is %+v, want the record kept and stamped", listed.Tokens)
	}

	// Revoking again is idempotent; revoking nothing is a 404.
	if status := call(t, server, http.MethodDelete, "/api/v1/tokens/"+minted.ID,
		testAdminToken, nil, nil); status != http.StatusNoContent {
		t.Errorf("second revoke: status = %d, want 204", status)
	}
	if got := errorCode(t, server, http.MethodDelete, "/api/v1/tokens/does-not-exist",
		testAdminToken, nil, http.StatusNotFound); got != "not_found" {
		t.Errorf("revoking an unknown token: error code = %q, want not_found", got)
	}
}

func TestTokenMintValidation(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)

	for _, one := range []struct {
		why  string
		body map[string]any
	}{
		{"no name", map[string]any{"scopes": []string{"servers:read"}}},
		{"blank name", map[string]any{"name": "   ", "scopes": []string{"servers:read"}}},
		{"no scopes", map[string]any{"name": "empty"}},
		{"empty scope list", map[string]any{"name": "empty", "scopes": []string{}}},
		{"an undefined scope", map[string]any{"name": "typo", "scopes": []string{"event:read"}}},
		{"a pattern on a pair that takes none",
			map[string]any{"name": "typo", "scopes": []string{"servers:read:s-1"}}},
		{"a negative expiry",
			map[string]any{"name": "past", "scopes": []string{"servers:read"}, "expiresInSeconds": -1}},
		{"too many scopes", map[string]any{"name": "greedy", "scopes": manyScopes(33)}},
	} {
		if got := errorCode(t, server, http.MethodPost, "/api/v1/tokens", testAdminToken,
			one.body, http.StatusBadRequest); got != "bad_request" {
			t.Errorf("%s: error code = %q, want bad_request", one.why, got)
		}
	}
}

// An expiring token stops working on its own, without anyone revoking it.
func TestTokenExpiryIsEnforced(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)

	var response struct {
		Token  tokenRecord `json:"token"`
		Secret string      `json:"secret"`
	}
	// The floor is a minute, so this token is alive for the length of the test
	// and its expiry is asserted on the record rather than waited out.
	status := call(t, server, http.MethodPost, "/api/v1/tokens", testAdminToken, map[string]any{
		"name": "short lived", "scopes": []string{"servers:read"}, "expiresInSeconds": 1,
	}, &response)
	if status != http.StatusCreated {
		t.Fatalf("mint: status = %d, want 201", status)
	}
	if response.Token.ExpiresAt == nil {
		t.Fatal("an expiring token reports no expiresAt")
	}
	expiresAt, err := time.Parse(time.RFC3339, *response.Token.ExpiresAt)
	if err != nil {
		t.Fatalf("parse expiresAt %q: %v", *response.Token.ExpiresAt, err)
	}
	if until := time.Until(expiresAt); until < 30*time.Second {
		t.Errorf("expiresAt is %s away; a sub-minute request must be clamped to the floor", until)
	}
	if status := call(t, server, http.MethodGet, "/api/v1/servers", response.Secret, nil,
		nil); status != http.StatusOK {
		t.Errorf("an unexpired token: status = %d, want 200", status)
	}
}

// Reads are not audited, and neither is a request that never authenticated:
// an unauthenticated caller could otherwise fill the log an operator reads.
func TestAuditRecordsMutationsOnly(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)

	created := createServer(t, server, "audited", "test-game")
	call(t, server, http.MethodGet, "/api/v1/servers", testAdminToken, nil, nil)
	call(t, server, http.MethodGet, "/api/v1/servers/"+created.Server.ID, testAdminToken, nil, nil)
	for range 3 {
		call(t, server, http.MethodPost, "/api/v1/servers", "vya_wrong_token_entirely00", nil, nil)
	}

	page := queryAudit(t, server, nil)
	if len(page.Records) != 1 {
		t.Fatalf("the log holds %d records, want only the one server creation: %+v",
			len(page.Records), page.Records)
	}

	record := page.Records[0]
	if record.Method != http.MethodPost || record.Path != "/api/v1/servers" {
		t.Errorf("record = %s %s, want POST /api/v1/servers", record.Method, record.Path)
	}
	if record.Status != http.StatusCreated {
		t.Errorf("status = %d, want 201", record.Status)
	}
	if record.TokenName != "bootstrap" || record.TokenID != "" {
		t.Errorf("token = %q/%q, want the bootstrap credential, which has no id",
			record.TokenID, record.TokenName)
	}
	if record.SourceIP == "" {
		t.Error("record carries no source IP")
	}
	if len(record.PayloadDigest) != 64 {
		t.Errorf("payloadDigest = %q, want a 64-character SHA-256 digest", record.PayloadDigest)
	}

	// The route names no {serverId}, so the handler has to say which server it
	// created for the record to be worth anything.
	var detail struct {
		ServerID string `json:"serverId"`
		Name     string `json:"name"`
	}
	if err := json.Unmarshal(record.Detail, &detail); err != nil {
		t.Fatalf("decode detail %q: %v", record.Detail, err)
	}
	if detail.ServerID != created.Server.ID || detail.Name != "audited" {
		t.Errorf("detail = %+v, want the created server", detail)
	}
}

// The digest is over the request body, so two identical requests hash alike and
// a changed one does not. That is the whole point of recording it: an operator
// comparing two entries can tell whether the same change was made twice.
func TestAuditPayloadDigestTracksTheBody(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)

	body := map[string]any{"name": "digest", "game": "test-game"}
	call(t, server, http.MethodPost, "/api/v1/servers", testAdminToken, body, nil)
	call(t, server, http.MethodPost, "/api/v1/servers", testAdminToken, body, nil)
	call(t, server, http.MethodPost, "/api/v1/servers", testAdminToken,
		map[string]any{"name": "different", "game": "test-game"}, nil)

	page := queryAudit(t, server, nil)
	if len(page.Records) != 3 {
		t.Fatalf("the log holds %d records, want 3", len(page.Records))
	}
	digests := map[string]int{}
	for _, record := range page.Records {
		digests[record.PayloadDigest]++
	}
	if len(digests) != 2 {
		t.Errorf("three requests produced %d distinct digests, want 2", len(digests))
	}
	if digests[page.Records[1].PayloadDigest] != 2 && digests[page.Records[0].PayloadDigest] != 2 {
		t.Error("the two identical bodies did not hash alike")
	}
}

func TestAuditPaginatesWithoutSkippingOrRepeating(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)

	// Eight mutations in a tight loop, so several land in the same millisecond:
	// a hub ordering on the timestamp alone fails here, which is the point.
	for i := range 8 {
		createServer(t, server, "audit page "+string(rune('a'+i)), "test-game")
	}

	seen := map[string]int{}
	parameters := url.Values{"limit": {"3"}}
	pages := 0
	for range 10 {
		page := queryAudit(t, server, parameters)
		pages++
		for _, record := range page.Records {
			seen[record.ID]++
		}
		if page.NextCursor == "" {
			break
		}
		if len(page.Records) == 0 {
			t.Fatalf("page %d was empty but carried a nextCursor", pages)
		}
		parameters.Set("cursor", page.NextCursor)
	}
	if len(seen) != 8 {
		t.Errorf("pagination saw %d distinct records over %d pages, want 8", len(seen), pages)
	}
	for id, count := range seen {
		if count != 1 {
			t.Errorf("record %s came back on %d pages", id, count)
		}
	}

	if got := errorCode(t, server, http.MethodGet, "/api/v1/audit?cursor=not-a-cursor",
		testAdminToken, nil, http.StatusBadRequest); got != "bad_request" {
		t.Errorf("an unparseable cursor: error code = %q, want bad_request", got)
	}
	if got := errorCode(t, server, http.MethodGet, "/api/v1/audit?since=yesterday",
		testAdminToken, nil, http.StatusBadRequest); got != "bad_request" {
		t.Errorf("an unparseable since: error code = %q, want bad_request", got)
	}
}

// The generated bootstrap credential is first-run behavior, not a standing back
// door. Once the hub holds a token that can manage tokens, a restart with no
// configured credential must not print a fresh superuser token to the log.
func TestGeneratedBootstrapTokenIsConfinedToFirstRun(t *testing.T) {
	t.Parallel()
	databaseURL := filepath.Join(t.TempDir(), "bootstrap.db")

	firstRun := bootLogged(t, databaseURL, "")
	if !strings.Contains(firstRun, "generated an ephemeral one") {
		t.Fatalf("first run did not mint a bootstrap token; log was:\n%s", firstRun)
	}

	// Still nothing minted, so a second run is still a first run.
	secondRun := bootLogged(t, databaseURL, "")
	if !strings.Contains(secondRun, "generated an ephemeral one") {
		t.Fatalf("a hub with no tokens stopped minting a bootstrap credential; log was:\n%s", secondRun)
	}

	// A token that cannot manage tokens must NOT suppress the bootstrap. The
	// test that matters for lockout: an operator who minted only a read-only
	// panel credential and then restarted would otherwise be shut out of their
	// own Admin API with no way back that does not involve the host.
	configured := newTestServerAt(t, databaseURL)
	readerSecret, _ := mintToken(t, configured, "panel", "servers:read")
	configured.Close()

	afterReader := bootLogged(t, databaseURL, "")
	if !strings.Contains(afterReader, "generated an ephemeral one") {
		t.Errorf("a read-only token suppressed the bootstrap credential and locked the "+
			"operator out; log was:\n%s", afterReader)
	}

	// A token that can manage tokens is the condition that does suppress it.
	configured = newTestServerAt(t, databaseURL)
	adminSecret, _ := mintToken(t, configured, "the real one", "admin")
	configured.Close()

	afterAdmin := bootLogged(t, databaseURL, "")
	if strings.Contains(afterAdmin, "generated an ephemeral one") {
		t.Errorf("a hub holding an admin-scoped token still minted a bootstrap credential; "+
			"log was:\n%s", afterAdmin)
	}
	if !strings.Contains(afterAdmin, "scoped tokens only") {
		t.Errorf("the hub did not say it has no bootstrap credential; log was:\n%s", afterAdmin)
	}

	// The minted tokens are what still work, and the old bootstrap does not.
	restarted := newTestServerNoBootstrap(t, databaseURL)
	for _, one := range []struct {
		secret string
		name   string
	}{{adminSecret, "the admin token"}, {readerSecret, "the reader token"}} {
		if status := call(t, restarted, http.MethodGet, "/api/v1/servers", one.secret, nil,
			nil); status != http.StatusOK {
			t.Errorf("%s after restart: status = %d, want 200", one.name, status)
		}
	}
	if status := call(t, restarted, http.MethodGet, "/api/v1/servers", testAdminToken, nil, nil); status != http.StatusUnauthorized {
		t.Errorf("the old bootstrap token after restart: status = %d, want 401", status)
	}
}

// bootLogged starts and stops a hub, returning what it logged on the way up.
func bootLogged(t *testing.T, databaseURL, adminToken string) string {
	t.Helper()

	var logged bytes.Buffer
	server, err := hub.New(context.Background(), hub.Config{
		DatabaseURL: databaseURL,
		AdminToken:  adminToken,
		Logger:      slog.New(slog.NewJSONHandler(&logged, nil)),
	})
	if err != nil {
		t.Fatalf("boot hub: %v", err)
	}
	server.Close()
	return logged.String()
}

// newTestServerNoBootstrap boots a hub with no configured bootstrap credential,
// which on a database that already holds a token means no bootstrap at all.
func newTestServerNoBootstrap(t *testing.T, databaseURL string) *hub.Server {
	t.Helper()

	server, err := hub.New(context.Background(), hub.Config{
		DatabaseURL: databaseURL,
		Logger:      slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)),
	})
	if err != nil {
		t.Fatalf("boot hub: %v", err)
	}
	t.Cleanup(func() { server.Close() })
	return server
}

func manyScopes(count int) []string {
	scopes := make([]string, 0, count)
	for i := range count {
		scopes = append(scopes, "events:read:namespace"+string(rune('a'+i%26))+string(rune('a'+i/26))+".*")
	}
	return scopes
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return string(encoded)
}
