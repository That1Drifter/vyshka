package hub_test

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/That1Drifter/vyshka/hub"
)

// Listing the key/value store (spec section 12.2): the key page, its cursor,
// and the namespace listing with its scope filter.

// kvKeyRow is the wire shape of one row of a key listing.
type kvKeyRow struct {
	Key       string `json:"key"`
	Revision  int64  `json:"revision"`
	ExpiresAt string `json:"expiresAt"`
}

type kvKeyPage struct {
	Namespace  string     `json:"namespace"`
	Keys       []kvKeyRow `json:"keys"`
	NextCursor string     `json:"nextCursor"`
}

type kvNamespacePage struct {
	Namespaces []struct {
		Namespace string `json:"namespace"`
		Keys      int64  `json:"keys"`
	} `json:"namespaces"`
}

// writeKV puts one key through the Admin API, failing the test if it does not
// land.
func writeKV(t *testing.T, server *hub.Server, bearer, namespace, key string, body map[string]any) {
	t.Helper()
	status := call(t, server, http.MethodPut, "/api/v1/kv/"+namespace+"/"+key, bearer, body, nil)
	if status != http.StatusOK {
		t.Fatalf("write %s/%s: status = %d, want 200", namespace, key, status)
	}
}

// waitForKVAbsence blocks until a key reads as gone, or fails the test. A TTL
// floor of one second (spec section 12.1) is wall clock, so observing the
// expiry boundary through the API means waiting it out.
func waitForKVAbsence(t *testing.T, server *hub.Server, namespace, key string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		status := call(t, server, http.MethodGet, "/api/v1/kv/"+namespace+"/"+key,
			testAdminToken, nil, nil)
		if status == http.StatusNotFound {
			return
		}
		if status != http.StatusOK {
			t.Fatalf("get while waiting for expiry: status = %d", status)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the key %s/%s never expired", namespace, key)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// listKVKeys reads one page of a namespace.
func listKVKeys(t *testing.T, server *hub.Server, bearer, namespace string, parameters url.Values) kvKeyPage {
	t.Helper()
	path := "/api/v1/kv/" + namespace
	if encoded := parameters.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var page kvKeyPage
	if status := call(t, server, http.MethodGet, path, bearer, nil, &page); status != http.StatusOK {
		t.Fatalf("GET %s: status = %d, want 200", path, status)
	}
	return page
}

func TestKVListWalksPagesWithoutGapOrDuplicate(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)

	// More keys than one page holds, so the walk crosses a page boundary
	// several times. The set comparison is the point: a cursor that is
	// inclusive instead of exclusive repeats the boundary key, and one that
	// skips a key leaves a gap, and both show up here.
	const total = 23
	written := make([]string, 0, total)
	for i := range total {
		key := fmt.Sprintf("player.%03d", i)
		written = append(written, key)
		writeKV(t, server, testAdminToken, "page-mod", key, map[string]any{"value": i})
	}

	var (
		walked []string
		cursor string
	)
	for pages := 0; ; pages++ {
		if pages > 50 {
			t.Fatalf("the walk did not terminate after %d pages", pages)
		}
		parameters := url.Values{"limit": {"5"}}
		if cursor != "" {
			parameters.Set("cursor", cursor)
		}
		page := listKVKeys(t, server, testAdminToken, "page-mod", parameters)
		if page.Namespace != "page-mod" {
			t.Errorf("page %d echoed namespace %q, want page-mod", pages, page.Namespace)
		}
		if len(page.Keys) > 5 {
			t.Fatalf("page %d carried %d keys, want at most the limit of 5", pages, len(page.Keys))
		}
		for _, row := range page.Keys {
			if row.Revision != 1 {
				t.Errorf("key %q listed revision %d, want 1", row.Key, row.Revision)
			}
			if row.ExpiresAt != "" {
				t.Errorf("key %q with no TTL listed expiresAt %q", row.Key, row.ExpiresAt)
			}
			walked = append(walked, row.Key)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}

	if !slices.IsSorted(walked) {
		t.Errorf("the walk is not key ascending: %v", walked)
	}
	seen := map[string]int{}
	for _, key := range walked {
		seen[key]++
	}
	for key, count := range seen {
		if count > 1 {
			t.Errorf("key %q came back on %d pages, want 1", key, count)
		}
	}
	for _, key := range written {
		if seen[key] == 0 {
			t.Errorf("the walk skipped %q", key)
		}
	}
	if len(walked) != total {
		t.Errorf("the walk returned %d keys, want exactly the %d written", len(walked), total)
	}
}

func TestKVListOmitsExpiredKeys(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)

	writeKV(t, server, testAdminToken, "ttl-mod", "kept", map[string]any{"value": 1})
	writeKV(t, server, testAdminToken, "ttl-mod", "doomed",
		map[string]any{"value": 1, "ttlSeconds": 1})

	page := listKVKeys(t, server, testAdminToken, "ttl-mod", nil)
	if len(page.Keys) != 2 {
		t.Fatalf("before expiry the listing carries %d keys, want 2", len(page.Keys))
	}
	if page.Keys[0].Key != "doomed" || page.Keys[0].ExpiresAt == "" {
		t.Errorf("keys[0] = %+v, want doomed carrying its expiresAt", page.Keys[0])
	}

	// Rather than sleeping out a TTL, the key is re-set with the shortest TTL
	// the protocol allows and then read once it has passed. A second of wall
	// clock is the floor section 12.1 sets, so waiting it out is the only way
	// to observe the boundary through the API.
	waitForKVAbsence(t, server, "ttl-mod", "doomed")

	page = listKVKeys(t, server, testAdminToken, "ttl-mod", nil)
	if len(page.Keys) != 1 || page.Keys[0].Key != "kept" {
		t.Errorf("after expiry the listing is %+v, want only kept", page.Keys)
	}
	if page.NextCursor != "" {
		t.Errorf("a single-page listing carried nextCursor %q", page.NextCursor)
	}
}

func TestKVListPrefixFilters(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)

	for _, key := range []string{"balance.1", "balance.2", "balances", "bank.1"} {
		writeKV(t, server, testAdminToken, "prefix-mod", key, map[string]any{"value": 1})
	}

	cases := []struct {
		prefix string
		want   []string
	}{
		{"balance.", []string{"balance.1", "balance.2"}},
		{"balance", []string{"balance.1", "balance.2", "balances"}},
		{"bank", []string{"bank.1"}},
		{"", []string{"balance.1", "balance.2", "balances", "bank.1"}},
		// A prefix outside the key alphabet matches nothing rather than
		// failing: it is a literal prefix, not a pattern.
		{"no-such-prefix", nil},
		{"balance/", nil},
	}
	for _, one := range cases {
		page := listKVKeys(t, server, testAdminToken, "prefix-mod",
			url.Values{"prefix": {one.prefix}})
		got := make([]string, 0, len(page.Keys))
		for _, row := range page.Keys {
			got = append(got, row.Key)
		}
		if len(got) == 0 && len(one.want) == 0 {
			continue
		}
		if !slices.Equal(got, one.want) {
			t.Errorf("prefix %q listed %v, want %v", one.prefix, got, one.want)
		}
	}
}

func TestKVListRefusals(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)

	writeKV(t, server, testAdminToken, "guard-mod", "one", map[string]any{"value": 1})

	// A malformed namespace is bad_request, like every other KV route.
	if code := errorCode(t, server, http.MethodGet, "/api/v1/kv/bad..namespace",
		testAdminToken, nil, http.StatusBadRequest); code != "bad_request" {
		t.Errorf("malformed namespace: code = %q, want bad_request", code)
	}
	// A cursor that is not one this hub issued is refused, never ignored: an
	// ignored cursor answers with page one again, which a walking client reads
	// as a loop of real results.
	for _, cursor := range []string{
		"not base64!!",
		base64.RawURLEncoding.EncodeToString([]byte("bad..key")),
		base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("k", 129))),
	} {
		if code := errorCode(t, server, http.MethodGet,
			"/api/v1/kv/guard-mod?cursor="+url.QueryEscape(cursor),
			testAdminToken, nil, http.StatusBadRequest); code != "bad_request" {
			t.Errorf("cursor %q: code = %q, want bad_request", cursor, code)
		}
	}
	if code := errorCode(t, server, http.MethodGet, "/api/v1/kv/guard-mod?limit=0",
		testAdminToken, nil, http.StatusBadRequest); code != "bad_request" {
		t.Errorf("limit 0: code = %q, want bad_request", code)
	}
	if code := errorCode(t, server, http.MethodGet, "/api/v1/kv/guard-mod?limit=nope",
		testAdminToken, nil, http.StatusBadRequest); code != "bad_request" {
		t.Errorf("non-numeric limit: code = %q, want bad_request", code)
	}

	// An over-large limit is clamped, not refused (spec sections 8.5, 12.2).
	if page := listKVKeys(t, server, testAdminToken, "guard-mod",
		url.Values{"limit": {"100000"}}); len(page.Keys) != 1 {
		t.Errorf("an over-large limit answered %d keys, want the one written", len(page.Keys))
	}

	// The namespace listing needs some kv:rw grant at all.
	unrelated, _ := mintToken(t, server, "no kv", "servers:read")
	if code := errorCode(t, server, http.MethodGet, "/api/v1/kv",
		unrelated, nil, http.StatusForbidden); code != "forbidden" {
		t.Errorf("namespace listing with no kv grant: code = %q, want forbidden", code)
	}
	if code := errorCode(t, server, http.MethodGet, "/api/v1/kv/guard-mod",
		unrelated, nil, http.StatusForbidden); code != "forbidden" {
		t.Errorf("key listing with no kv grant: code = %q, want forbidden", code)
	}

	// A kv:rw grant on one namespace does not reach another's key listing.
	scoped, _ := mintToken(t, server, "one namespace", "kv:rw:guard-mod")
	if status := call(t, server, http.MethodGet, "/api/v1/kv/guard-mod", scoped, nil, nil); status != http.StatusOK {
		t.Errorf("the granted namespace was refused its own listing: status = %d", status)
	}
	if code := errorCode(t, server, http.MethodGet, "/api/v1/kv/other-mod",
		scoped, nil, http.StatusForbidden); code != "forbidden" {
		t.Errorf("another namespace's listing: code = %q, want forbidden", code)
	}
}

func TestKVNamespaceListingIsFilteredByScope(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)

	writeKV(t, server, testAdminToken, "mod-a", "one", map[string]any{"value": 1})
	writeKV(t, server, testAdminToken, "mod-a", "two", map[string]any{"value": 1})
	writeKV(t, server, testAdminToken, "mod-b", "one", map[string]any{"value": 1})
	writeKV(t, server, testAdminToken, "family.child", "one", map[string]any{"value": 1})

	names := func(bearer string) []string {
		t.Helper()
		var page kvNamespacePage
		if status := call(t, server, http.MethodGet, "/api/v1/kv", bearer, nil, &page); status != http.StatusOK {
			t.Fatalf("GET /api/v1/kv: status = %d, want 200", status)
		}
		got := make([]string, 0, len(page.Namespaces))
		for _, one := range page.Namespaces {
			got = append(got, one.Namespace)
		}
		return got
	}

	// The bootstrap credential holds admin, so it sees everything, name
	// ascending, with live counts.
	var full kvNamespacePage
	if status := call(t, server, http.MethodGet, "/api/v1/kv", testAdminToken, nil, &full); status != http.StatusOK {
		t.Fatalf("admin listing: status = %d, want 200", status)
	}
	if len(full.Namespaces) != 3 {
		t.Fatalf("admin sees %d namespaces, want 3: %+v", len(full.Namespaces), full.Namespaces)
	}
	if full.Namespaces[0].Namespace != "family.child" || full.Namespaces[0].Keys != 1 {
		t.Errorf("namespaces[0] = %+v, want family.child with 1 key first in name order", full.Namespaces[0])
	}
	if full.Namespaces[1].Namespace != "mod-a" || full.Namespaces[1].Keys != 2 {
		t.Errorf("namespaces[1] = %+v, want mod-a with 2 keys", full.Namespaces[1])
	}

	// A token narrowed to one namespace sees exactly that one: enumeration
	// reveals nothing a key-by-key read could not have found.
	narrow, _ := mintToken(t, server, "mod-a only", "kv:rw:mod-a")
	if got := names(narrow); !slices.Equal(got, []string{"mod-a"}) {
		t.Errorf("a kv:rw:mod-a token sees %v, want only mod-a", got)
	}

	// A prefix grant covers the namespaces under it and nothing else.
	prefixed, _ := mintToken(t, server, "family", "kv:rw:family.*")
	if got := names(prefixed); !slices.Equal(got, []string{"family.child"}) {
		t.Errorf("a kv:rw:family.* token sees %v, want only family.child", got)
	}

	// An unnarrowed kv:rw grant sees the whole store.
	wide, _ := mintToken(t, server, "every namespace", "kv:rw")
	if got := names(wide); !slices.Equal(got, []string{"family.child", "mod-a", "mod-b"}) {
		t.Errorf("an unnarrowed kv:rw token sees %v, want every namespace", got)
	}

	// Two grants union.
	both, _ := mintToken(t, server, "a and b", "kv:rw:mod-a", "kv:rw:mod-b")
	if got := names(both); !slices.Equal(got, []string{"mod-a", "mod-b"}) {
		t.Errorf("a two-grant token sees %v, want mod-a and mod-b", got)
	}
}

// TestKVListRoutesDoNotShadow holds the mux arrangement honest: the key
// listing and the per-key operations are different patterns of different
// lengths, and registering the shorter one must not swallow the longer.
func TestKVListRoutesDoNotShadow(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)

	writeKV(t, server, testAdminToken, "shadow-mod", "the-key", map[string]any{"value": 42})

	// The per-key get still answers the entry shape, value and all.
	var entry kvEntry
	if status := call(t, server, http.MethodGet, "/api/v1/kv/shadow-mod/the-key",
		testAdminToken, nil, &entry); status != http.StatusOK {
		t.Fatalf("per-key get: status = %d, want 200", status)
	}
	if entry.Key != "the-key" || string(entry.Value) != "42" {
		t.Errorf("per-key get answered %+v, want the-key with value 42", entry)
	}

	// The namespace listing answers the page shape, not an entry.
	page := listKVKeys(t, server, testAdminToken, "shadow-mod", nil)
	if len(page.Keys) != 1 || page.Keys[0].Key != "the-key" {
		t.Errorf("key listing answered %+v, want one row for the-key", page.Keys)
	}

	// A key whose name collides with nothing must not be read as a namespace,
	// and a namespace must not be read as a key: the two-segment path is not
	// reachable from the one-segment pattern.
	if code := errorCode(t, server, http.MethodGet, "/api/v1/kv/shadow-mod/absent",
		testAdminToken, nil, http.StatusNotFound); code != "not_found" {
		t.Errorf("absent key: code = %q, want not_found from the per-key route", code)
	}

	// Both new routes carry their methodNotAllowed registration.
	for _, one := range []struct {
		path  string
		allow string
	}{
		{"/api/v1/kv", "GET"},
		{"/api/v1/kv/shadow-mod", "GET"},
	} {
		if code := errorCode(t, server, http.MethodPost, one.path, testAdminToken,
			map[string]any{"value": 1}, http.StatusMethodNotAllowed); code != "method_not_allowed" {
			t.Errorf("POST %s: code = %q, want method_not_allowed", one.path, code)
		}
	}
	// The per-key route keeps its own set of methods, which the shorter
	// pattern's GET-only registration must not have narrowed.
	if status := call(t, server, http.MethodDelete, "/api/v1/kv/shadow-mod/the-key",
		testAdminToken, nil, nil); status != http.StatusNoContent {
		t.Errorf("per-key delete: status = %d, want 204", status)
	}
}
