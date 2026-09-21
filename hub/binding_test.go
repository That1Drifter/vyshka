package hub_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/That1Drifter/vyshka/hub"
)

// mintBoundToken mints a token confined to the given servers (spec section
// 10.1) and returns its secret and record.
func mintBoundToken(t *testing.T, server *hub.Server, name string, servers []string, scopes ...string) (string, tokenRecord) {
	t.Helper()

	var response struct {
		Token  tokenRecord `json:"token"`
		Secret string      `json:"secret"`
	}
	status := call(t, server, http.MethodPost, "/api/v1/tokens", testAdminToken,
		map[string]any{"name": name, "scopes": scopes, "servers": servers}, &response)
	if status != http.StatusCreated {
		t.Fatalf("mint bound token %q: status = %d, want 201", name, status)
	}
	return response.Secret, response.Token
}

// The requirement of issue #81: a token bound to server A holds its grants on A
// and nothing on B, on every route that names a server, on the action read
// that reaches a server without naming it, and on the server list, which is
// filtered rather than refused. The key/value store is not narrowed.
func TestBoundTokenIsConfinedToItsServers(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	mine, _ := manifestFirst(t, server, "bound: mine")
	other, _ := manifestFirst(t, server, "bound: other")
	mineID, otherID := mine.Server.ID, other.Server.ID

	secret, minted := mintBoundToken(t, server, "moderator of mine", []string{mineID},
		"servers:read", "events:read", "actions:dispatch:example-mod.*", "kv:rw:example-mod")
	if len(minted.Servers) != 1 || minted.Servers[0] != mineID {
		t.Fatalf("minted record carries servers %v, want [%s]", minted.Servers, mineID)
	}

	// The list is filtered to the binding, never refused.
	var listed struct {
		Servers []struct {
			ID string `json:"id"`
		} `json:"servers"`
	}
	if status := call(t, server, http.MethodGet, "/api/v1/servers", secret, nil, &listed); status != http.StatusOK {
		t.Fatalf("list servers as a bound token: status = %d, want 200", status)
	}
	if len(listed.Servers) != 1 || listed.Servers[0].ID != mineID {
		t.Errorf("bound token lists %d server(s) %+v, want only %s", len(listed.Servers), listed.Servers, mineID)
	}

	// Its own server answers on every route the grants cover.
	for _, path := range []string{
		"/api/v1/servers/" + mineID,
		"/api/v1/servers/" + mineID + "/manifest",
		"/api/v1/servers/" + mineID + "/events",
	} {
		if status := call(t, server, http.MethodGet, path, secret, nil, nil); status != http.StatusOK {
			t.Errorf("GET %s on the bound server: status = %d, want 200", path, status)
		}
	}
	var accepted struct {
		ActionID string `json:"actionId"`
	}
	if status := call(t, server, http.MethodPost, "/api/v1/servers/"+mineID+"/actions", secret,
		map[string]any{"code": "example-mod.heal", "params": map[string]any{"amount": 25}}, &accepted); status != http.StatusAccepted {
		t.Fatalf("dispatch on the bound server: status = %d, want 202", status)
	}
	if status := call(t, server, http.MethodGet, "/api/v1/actions/"+accepted.ActionID, secret, nil, nil); status != http.StatusOK {
		t.Errorf("read back its own action: status = %d, want 200", status)
	}

	// The other server is refused with forbidden on every route that names it,
	// whatever the scope would have allowed.
	for _, one := range []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodGet, "/api/v1/servers/" + otherID, nil},
		{http.MethodGet, "/api/v1/servers/" + otherID + "/manifest", nil},
		{http.MethodGet, "/api/v1/servers/" + otherID + "/events", nil},
		{http.MethodGet, "/api/v1/servers/" + otherID + "/state/players", nil},
		{http.MethodGet, "/api/v1/servers/" + otherID + "/state/players/history", nil},
		{http.MethodPost, "/api/v1/servers/" + otherID + "/actions", map[string]any{"code": "example-mod.heal"}},
	} {
		if got := errorCode(t, server, one.method, one.path, secret, one.body, http.StatusForbidden); got != "forbidden" {
			t.Errorf("%s %s from a token bound elsewhere: error code = %q, want forbidden", one.method, one.path, got)
		}
	}

	// An action on the other server is reached by its own id, not by a path
	// naming the server; it is refused all the same, after the lookup, and
	// before the scope check, so a code the token does not hold is not
	// named in the refusal: it is part of the record.
	var theirs struct {
		ActionID string `json:"actionId"`
	}
	if status := call(t, server, http.MethodPost, "/api/v1/servers/"+otherID+"/actions", testAdminToken,
		map[string]any{"code": "example-mod.heal", "params": map[string]any{"amount": 25}}, &theirs); status != http.StatusAccepted {
		t.Fatalf("admin dispatch on the other server: status = %d, want 202", status)
	}
	if got := errorCode(t, server, http.MethodGet, "/api/v1/actions/"+theirs.ActionID, secret, nil, http.StatusForbidden); got != "forbidden" {
		t.Errorf("reading an action of the other server: error code = %q, want forbidden", got)
	}
	narrowSecret, _ := mintBoundToken(t, server, "reader of another code on mine", []string{mineID}, "actions:read:example-mod.revive")
	var refusal struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if status := call(t, server, http.MethodGet, "/api/v1/actions/"+theirs.ActionID, narrowSecret, nil, &refusal); status != http.StatusForbidden {
		t.Fatalf("reading a foreign action with a code outside the grant: status = %d, want 403", status)
	}
	if refusal.Error.Code != "forbidden" {
		t.Errorf("error code = %q, want forbidden", refusal.Error.Code)
	}
	if strings.Contains(refusal.Error.Message, "example-mod.heal") {
		t.Errorf("the refusal names the foreign action's code: %q", refusal.Error.Message)
	}
	// A made-up id is still not found: the binding check runs after the
	// lookup, and an unguessable id discloses nothing.
	if got := errorCode(t, server, http.MethodGet, "/api/v1/actions/01J00000000000000000000000", secret, nil, http.StatusNotFound); got != "not_found" {
		t.Errorf("reading an unknown action: error code = %q, want not_found", got)
	}

	// The store has no server in a key: the binding does not narrow it.
	if status := call(t, server, http.MethodPut, "/api/v1/kv/example-mod/greeting", secret,
		map[string]any{"value": "hello"}, nil); status != http.StatusOK {
		t.Errorf("key/value write from a bound token: status = %d, want 200", status)
	}

	// The refused dispatch on the other server is in the audit log as a
	// refusal, with no payload digest: nothing was read to digest.
	page := queryAudit(t, server, url.Values{"tokenId": {minted.ID}})
	refusedDispatches := 0
	for _, record := range page.Records {
		if record.Method != http.MethodPost || !strings.Contains(record.Path, otherID) {
			continue
		}
		refusedDispatches++
		if record.Status != http.StatusForbidden {
			t.Errorf("audit record of the refused dispatch has status %d, want 403", record.Status)
		}
		if record.PayloadDigest != "" {
			t.Errorf("audit record of a refusal at the headers carries a payload digest %q, want none", record.PayloadDigest)
		}
	}
	if refusedDispatches != 1 {
		t.Errorf("the log holds %d refused dispatches on the other server, want 1", refusedDispatches)
	}

	// The mint itself recorded the binding beside the scopes.
	minting := queryAudit(t, server, url.Values{})
	found := false
	for _, record := range minting.Records {
		if record.Method != http.MethodPost || record.Path != "/api/v1/tokens" {
			continue
		}
		var detail struct {
			TokenID string   `json:"tokenId"`
			Servers []string `json:"servers"`
		}
		if err := json.Unmarshal(record.Detail, &detail); err != nil {
			t.Fatalf("decode mint audit detail %q: %v", record.Detail, err)
		}
		if detail.TokenID != minted.ID {
			continue
		}
		found = true
		if len(detail.Servers) != 1 || detail.Servers[0] != mineID {
			t.Errorf("mint audit detail carries servers %v, want [%s]", detail.Servers, mineID)
		}
	}
	if !found {
		t.Error("no audit record of the mint names the minted token")
	}
}

// Everything a binding cannot be is refused at mint, and nothing is stored.
func TestBindingIsValidatedAtMint(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created := createServer(t, server, "binding validation", "dayz")
	serverID := created.Server.ID

	for _, one := range []struct {
		name       string
		body       map[string]any
		wantStatus int
		wantCode   string
	}{
		{"unknown server", map[string]any{"name": "t", "scopes": []string{"servers:read"}, "servers": []string{"01J00000000000000000000000"}}, http.StatusNotFound, "not_found"},
		{"admin on a bound token", map[string]any{"name": "t", "scopes": []string{"admin"}, "servers": []string{serverID}}, http.StatusBadRequest, "bad_request"},
		{"webhooks:manage on a bound token", map[string]any{"name": "t", "scopes": []string{"servers:read", "webhooks:manage"}, "servers": []string{serverID}}, http.StatusBadRequest, "bad_request"},
		{"a pattern where an id belongs", map[string]any{"name": "t", "scopes": []string{"servers:read"}, "servers": []string{"*"}}, http.StatusBadRequest, "bad_request"},
		{"a namespace wildcard where an id belongs", map[string]any{"name": "t", "scopes": []string{"servers:read"}, "servers": []string{"example.*"}}, http.StatusBadRequest, "bad_request"},
		{"an empty id", map[string]any{"name": "t", "scopes": []string{"servers:read"}, "servers": []string{""}}, http.StatusBadRequest, "bad_request"},
		{"too many ids", map[string]any{"name": "t", "scopes": []string{"servers:read"}, "servers": manyIDs(51)}, http.StatusBadRequest, "bad_request"},
	} {
		if got := errorCode(t, server, http.MethodPost, "/api/v1/tokens", testAdminToken, one.body, one.wantStatus); got != one.wantCode {
			t.Errorf("%s: error code = %q, want %s", one.name, got, one.wantCode)
		}
	}

	// Nothing above was minted.
	var listed struct {
		Tokens []tokenRecord `json:"tokens"`
	}
	if status := call(t, server, http.MethodGet, "/api/v1/tokens", testAdminToken, nil, &listed); status != http.StatusOK {
		t.Fatalf("list tokens: status = %d, want 200", status)
	}
	if len(listed.Tokens) != 0 {
		t.Errorf("%d token(s) exist after refused mints, want none", len(listed.Tokens))
	}

	// Duplicates collapse, and an unbound token reports an empty list rather
	// than nothing at all.
	_, bound := mintBoundToken(t, server, "twice", []string{serverID, serverID}, "servers:read")
	if len(bound.Servers) != 1 {
		t.Errorf("a binding naming one server twice is stored as %v, want one entry", bound.Servers)
	}
	_, unbound := mintToken(t, server, "unbound", "servers:read")
	if unbound.Servers == nil || len(unbound.Servers) != 0 {
		t.Errorf("an unbound token reports servers %v, want []", unbound.Servers)
	}
}

func manyIDs(n int) []string {
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		ids = append(ids, "01J0000000000000000000000"+string(rune('A'+i%26)))
	}
	return ids
}
