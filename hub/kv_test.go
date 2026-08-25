package hub_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/That1Drifter/vyshka/hub"
)

// kvEntry mirrors the wire shape of one key (spec section 12.2).
type kvEntry struct {
	Namespace string          `json:"namespace"`
	Key       string          `json:"key"`
	Value     json.RawMessage `json:"value"`
	Revision  int64           `json:"revision"`
	ExpiresAt string          `json:"expiresAt"`
}

// kvManifest is a manifest whose only point is its kvNamespaces declaration.
func kvManifest(revision int64, namespaces ...string) map[string]any {
	body := healManifest(revision)
	body["kvNamespaces"] = namespaces
	return body
}

// enrolledKVSession walks a server to a live session with a manifest that
// declares the given KV namespaces.
func enrolledKVSession(t *testing.T, server *hub.Server, name string, namespaces ...string) (createdServer, session) {
	t.Helper()

	created, live := enrolledSession(t, server, name)
	result := pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{
		"envelopes": []map[string]any{publishEnvelope(1, kvManifest(1, namespaces...))},
	})
	if result.Ack != 1 {
		t.Fatalf("ack = %d after manifest.publish, want 1", result.Ack)
	}
	return created, live
}

func TestKVPluginConfinedToDeclaredNamespaces(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)

	// Before any manifest exists, every KV call is refused.
	created, live := enrolledSession(t, server, "kv confinement")
	if code := errorCode(t, server, http.MethodPut, "/plugin/v1/kv/example-mod/some-key",
		live.SessionToken, map[string]any{"value": 1}, http.StatusForbidden); code != "forbidden" {
		t.Errorf("KV with no manifest: code = %q, want forbidden", code)
	}

	// A manifest that declares the namespace opens it, and only it.
	result := pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{
		"envelopes": []map[string]any{publishEnvelope(1, kvManifest(1, "example-mod"))},
	})
	if result.Ack != 1 {
		t.Fatalf("ack = %d after manifest.publish, want 1", result.Ack)
	}

	var written kvEntry
	status := call(t, server, http.MethodPut, "/plugin/v1/kv/example-mod/some-key",
		live.SessionToken, map[string]any{"value": map[string]any{"n": 1}}, &written)
	if status != http.StatusOK {
		t.Fatalf("declared-namespace set: status = %d, want 200", status)
	}
	if written.Revision != 1 {
		t.Errorf("first write revision = %d, want 1", written.Revision)
	}

	if code := errorCode(t, server, http.MethodGet, "/plugin/v1/kv/other-mod/some-key",
		live.SessionToken, nil, http.StatusForbidden); code != "forbidden" {
		t.Errorf("undeclared namespace: code = %q, want forbidden", code)
	}

	// The check runs before the key lookup: an undeclared namespace answers
	// forbidden whether or not the key exists, so 403 vs 404 probes nothing.
	if code := errorCode(t, server, http.MethodGet, "/plugin/v1/kv/other-mod/no-such-key",
		live.SessionToken, nil, http.StatusForbidden); code != "forbidden" {
		t.Errorf("undeclared namespace, missing key: code = %q, want forbidden", code)
	}

	// A session token is required at all.
	if code := errorCode(t, server, http.MethodGet, "/plugin/v1/kv/example-mod/some-key",
		"", nil, http.StatusUnauthorized); code != "session_invalid" {
		t.Errorf("no session token: code = %q, want session_invalid", code)
	}
}

func TestKVAdminScopedByNamespace(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)

	scoped, _ := mintToken(t, server, "kv bot", "kv:rw:mod-a")
	unrelated, _ := mintToken(t, server, "reader", "servers:read")

	var written kvEntry
	status := call(t, server, http.MethodPut, "/api/v1/kv/mod-a/config",
		scoped, map[string]any{"value": "granted"}, &written)
	if status != http.StatusOK {
		t.Fatalf("granted namespace: status = %d, want 200", status)
	}

	if code := errorCode(t, server, http.MethodPut, "/api/v1/kv/mod-b/config",
		scoped, map[string]any{"value": "denied"}, http.StatusForbidden); code != "forbidden" {
		t.Errorf("other namespace: code = %q, want forbidden", code)
	}
	// A prefix grant does not leak sideways: kv:rw:mod-a covers mod-a, not
	// mod-a.anything's parent nor a namespace merely starting with the text.
	if code := errorCode(t, server, http.MethodGet, "/api/v1/kv/mod-alpha/config",
		scoped, nil, http.StatusForbidden); code != "forbidden" {
		t.Errorf("longer namespace: code = %q, want forbidden", code)
	}

	// No kv grant at all fails at the route gate.
	if code := errorCode(t, server, http.MethodGet, "/api/v1/kv/mod-a/config",
		unrelated, nil, http.StatusForbidden); code != "forbidden" {
		t.Errorf("no kv scope: code = %q, want forbidden", code)
	}

	// The bootstrap admin token holds every scope.
	var read kvEntry
	if status := call(t, server, http.MethodGet, "/api/v1/kv/mod-a/config",
		testAdminToken, nil, &read); status != http.StatusOK {
		t.Fatalf("admin read: status = %d, want 200", status)
	}
	if string(read.Value) != `"granted"` {
		t.Errorf("admin read value = %s, want the scoped token's write", read.Value)
	}
}

// The issue's demo path: a plugin writes a key, an admin bot's stale CAS is
// rejected with the current revision, and a fresh CAS wins.
func TestKVCompareAndSwapAcrossRealms(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	_, live := enrolledKVSession(t, server, "kv cas", "example-mod")

	var written kvEntry
	status := call(t, server, http.MethodPut, "/plugin/v1/kv/example-mod/territory",
		live.SessionToken, map[string]any{"value": map[string]any{"owner": "clan-a"}}, &written)
	if status != http.StatusOK {
		t.Fatalf("plugin set: status = %d, want 200", status)
	}

	// The plugin writes again, moving the revision past what the bot read.
	if status := call(t, server, http.MethodPut, "/plugin/v1/kv/example-mod/territory",
		live.SessionToken, map[string]any{"value": map[string]any{"owner": "clan-b"}}, &written); status != http.StatusOK {
		t.Fatalf("plugin second set: status = %d, want 200", status)
	}

	var failure struct {
		Error struct {
			Code    string `json:"code"`
			Details struct {
				Revision int64 `json:"revision"`
			} `json:"details"`
		} `json:"error"`
	}
	status = call(t, server, http.MethodPut, "/api/v1/kv/example-mod/territory", testAdminToken,
		map[string]any{"value": map[string]any{"owner": "bot"}, "ifRevision": 1}, &failure)
	if status != http.StatusConflict {
		t.Fatalf("stale CAS: status = %d, want 409", status)
	}
	if failure.Error.Code != "revision_mismatch" {
		t.Errorf("stale CAS code = %q, want revision_mismatch", failure.Error.Code)
	}
	if failure.Error.Details.Revision != 2 {
		t.Errorf("stale CAS details.revision = %d, want the current 2", failure.Error.Details.Revision)
	}

	var swapped kvEntry
	status = call(t, server, http.MethodPut, "/api/v1/kv/example-mod/territory", testAdminToken,
		map[string]any{"value": map[string]any{"owner": "bot"}, "ifRevision": failure.Error.Details.Revision}, &swapped)
	if status != http.StatusOK {
		t.Fatalf("fresh CAS: status = %d, want 200", status)
	}
	if swapped.Revision != 3 {
		t.Errorf("fresh CAS revision = %d, want 3", swapped.Revision)
	}
}

func TestKVIncrOverHTTP(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)

	// An absent body means delta 1 on a fresh key.
	var bumped kvEntry
	status := call(t, server, http.MethodPost, "/api/v1/kv/example-mod/hits/incr",
		testAdminToken, nil, &bumped)
	if status != http.StatusOK {
		t.Fatalf("incr with no body: status = %d, want 200", status)
	}
	if string(bumped.Value) != "1" || bumped.Revision != 1 {
		t.Errorf("incr with no body = (%s, rev %d), want (1, rev 1)", bumped.Value, bumped.Revision)
	}

	status = call(t, server, http.MethodPost, "/api/v1/kv/example-mod/hits/incr",
		testAdminToken, map[string]any{"delta": -3}, &bumped)
	if status != http.StatusOK {
		t.Fatalf("negative delta: status = %d, want 200", status)
	}
	if string(bumped.Value) != "-2" || bumped.Revision != 2 {
		t.Errorf("decrement = (%s, rev %d), want (-2, rev 2)", bumped.Value, bumped.Revision)
	}

	// Incr on a non-integer value is a conflict.
	if status := call(t, server, http.MethodPut, "/api/v1/kv/example-mod/label",
		testAdminToken, map[string]any{"value": "words"}, nil); status != http.StatusOK {
		t.Fatalf("set string: status = %d, want 200", status)
	}
	if code := errorCode(t, server, http.MethodPost, "/api/v1/kv/example-mod/label/incr",
		testAdminToken, nil, http.StatusConflict); code != "conflict" {
		t.Errorf("incr on a string: code = %q, want conflict", code)
	}
}

func TestKVValidation(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)

	badRequest := func(method, path string, body any, why string) {
		t.Helper()
		if code := errorCode(t, server, method, path, testAdminToken, body, http.StatusBadRequest); code != "bad_request" {
			t.Errorf("%s: code = %q, want bad_request", why, code)
		}
	}

	badRequest(http.MethodGet, "/api/v1/kv/bad..namespace/key", nil, "empty namespace segment")
	badRequest(http.MethodGet, "/api/v1/kv/spaced%20out/key", nil, "namespace outside the alphabet")
	badRequest(http.MethodGet, "/api/v1/kv/example-mod/bad..key", nil, "empty key segment")
	badRequest(http.MethodGet, "/api/v1/kv/example-mod/"+strings.Repeat("k", 129), nil, "key over 128 characters")
	badRequest(http.MethodGet, "/api/v1/kv/"+strings.Repeat("n", 65)+"/key", nil, "namespace over 64 characters")

	badRequest(http.MethodPut, "/api/v1/kv/example-mod/key", map[string]any{}, "value missing")
	badRequest(http.MethodPut, "/api/v1/kv/example-mod/key", map[string]any{"value": nil}, "value null")
	badRequest(http.MethodPut, "/api/v1/kv/example-mod/key",
		map[string]any{"value": strings.Repeat("x", 16400)}, "value over the 16 KiB cap")
	badRequest(http.MethodPut, "/api/v1/kv/example-mod/key",
		map[string]any{"value": 1, "ifRevision": -1}, "negative ifRevision")
	badRequest(http.MethodPut, "/api/v1/kv/example-mod/key",
		map[string]any{"value": 1, "ttlSeconds": 0}, "zero ttlSeconds")
	badRequest(http.MethodPost, "/api/v1/kv/example-mod/key/incr",
		map[string]any{"delta": int64(1) << 53}, "delta at the exactness bound")

	// A delete of a key that never existed answers not_found, which a retry
	// reads as success.
	if code := errorCode(t, server, http.MethodDelete, "/api/v1/kv/example-mod/never-was",
		testAdminToken, nil, http.StatusNotFound); code != "not_found" {
		t.Errorf("delete of a missing key: code = %q, want not_found", code)
	}
}

func TestKVTTLOverHTTP(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)

	var written kvEntry
	status := call(t, server, http.MethodPut, "/api/v1/kv/example-mod/ephemeral",
		testAdminToken, map[string]any{"value": 1, "ttlSeconds": 1}, &written)
	if status != http.StatusOK {
		t.Fatalf("set with TTL: status = %d, want 200", status)
	}
	expiry, err := time.Parse(time.RFC3339, written.ExpiresAt)
	if err != nil {
		t.Fatalf("expiresAt %q is not RFC 3339: %v", written.ExpiresAt, err)
	}
	if until := time.Until(expiry); until <= 0 || until > 2*time.Second {
		t.Errorf("expiresAt is %v away, want about one second", until)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		status := call(t, server, http.MethodGet, "/api/v1/kv/example-mod/ephemeral",
			testAdminToken, nil, nil)
		if status == http.StatusNotFound {
			break
		}
		if status != http.StatusOK {
			t.Fatalf("get while waiting for expiry: status = %d", status)
		}
		if time.Now().After(deadline) {
			t.Fatal("the key never expired")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// A manifest whose kvNamespaces breaks the grammar is rejected whole (spec
// section 6.6): a malformed grant fails at publish time.
func TestKVManifestDeclarationValidated(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, live := enrolledSession(t, server, "kv manifest validation")

	cases := []struct {
		name       string
		namespaces []string
	}{
		{"outside the alphabet", []string{"bad namespace"}},
		{"empty segment", []string{"trailing."}},
		{"duplicate", []string{"example-mod", "example-mod"}},
	}
	for i, one := range cases {
		seq := int64(i + 1)
		result := pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{
			"envelopes": []map[string]any{publishEnvelope(seq, kvManifest(seq, one.namespaces...))},
		})
		if result.Ack != seq {
			t.Fatalf("%s: ack = %d, want %d (a rejection is envelope-level success)", one.name, result.Ack, seq)
		}
		if status := call(t, server, http.MethodGet, "/api/v1/servers/"+created.Server.ID+"/manifest",
			testAdminToken, nil, nil); status != http.StatusNotFound {
			t.Errorf("%s: a manifest with invalid kvNamespaces was stored (status %d)", one.name, status)
		}
	}
}
