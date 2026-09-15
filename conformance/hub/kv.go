package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Fixtures for the key/value store (spec section 12). The store is
// installation-wide and outlives a suite run, so every check mints a
// namespace no earlier run can have touched.

var kvNamespaceCounter int64

func uniqueKVNamespace() string {
	kvNamespaceCounter++
	return "conformance-kv.run-" + strconv.FormatInt(time.Now().UnixNano(), 36) +
		"-" + strconv.FormatInt(kvNamespaceCounter, 10)
}

// kvEntry mirrors the wire shape of one key (spec section 12.2).
type kvEntry struct {
	Namespace string          `json:"namespace"`
	Key       string          `json:"key"`
	Value     json.RawMessage `json:"value"`
	Revision  int64           `json:"revision"`
	ExpiresAt string          `json:"expiresAt"`
}

// kvManifestBody is the suite manifest with a kvNamespaces declaration on top.
func kvManifestBody(revision int64, namespaces ...string) map[string]any {
	body := manifestBody(revision)
	body["kvNamespaces"] = namespaces
	return body
}

// newKVPlugin walks a fresh server to a live session whose manifest declares
// the given KV namespaces.
func (e Env) newKVPlugin(ctx context.Context, name string, namespaces ...string) (*fakePlugin, error) {
	plugin, err := e.newFakePlugin(ctx, name, 5)
	if err != nil {
		return nil, err
	}
	if _, err := plugin.publishManifest(ctx, kvManifestBody(1, namespaces...)); err != nil {
		return nil, err
	}
	// The publish rode one poll; confirm it was accepted before any check
	// leans on the declaration it carries.
	stored, err := e.storedManifest(ctx, plugin.Creds.ServerID)
	if err != nil {
		return nil, fmt.Errorf("the manifest with kvNamespaces was not stored: %w", err)
	}
	if stored.Revision != 1 {
		return nil, fmt.Errorf("stored manifest revision = %d, want 1", stored.Revision)
	}
	return plugin, nil
}

// kvPath builds the operation path for one realm's KV surface.
func kvPath(realm, namespace, key string) string {
	return realm + "/kv/" + namespace + "/" + key
}

func checkKVPluginWrite(ctx context.Context, env Env) error {
	namespace := uniqueKVNamespace()
	plugin, err := env.newKVPlugin(ctx, "conformance: kv write", namespace)
	if err != nil {
		return err
	}

	// The plugin writes through its own realm.
	var written kvEntry
	if err := env.expect(ctx, http.MethodPut, kvPath("/plugin/v1", namespace, "greeting"),
		plugin.Session.SessionToken, map[string]any{"value": map[string]any{"hello": "world"}},
		http.StatusOK, &written); err != nil {
		return err
	}
	if written.Revision != 1 {
		return fmt.Errorf("first write: revision = %d, want 1", written.Revision)
	}
	if written.Namespace != namespace || written.Key != "greeting" {
		return fmt.Errorf("write echoed %s/%s, want %s/greeting", written.Namespace, written.Key, namespace)
	}

	// The same key reads back on both realms, value and revision intact.
	for _, realm := range []struct{ prefix, bearer string }{
		{"/plugin/v1", plugin.Session.SessionToken},
		{"/api/v1", env.AdminToken},
	} {
		var read kvEntry
		if err := env.expect(ctx, http.MethodGet, kvPath(realm.prefix, namespace, "greeting"),
			realm.bearer, nil, http.StatusOK, &read); err != nil {
			return err
		}
		var value struct {
			Hello string `json:"hello"`
		}
		if err := json.Unmarshal(read.Value, &value); err != nil || value.Hello != "world" {
			return fmt.Errorf("%s read value %s, want the stored object", realm.prefix, read.Value)
		}
		if read.Revision != 1 {
			return fmt.Errorf("%s read revision = %d, want 1", realm.prefix, read.Revision)
		}
		if read.ExpiresAt != "" {
			return fmt.Errorf("%s reported expiresAt %q for a key with no TTL", realm.prefix, read.ExpiresAt)
		}
	}

	// The Plugin API's incr is the same atomic operation as the Admin API's:
	// a bump through each realm lands in the other's read.
	var bumped kvEntry
	if err := env.expect(ctx, http.MethodPost, kvPath("/plugin/v1", namespace, "hits")+"/incr",
		plugin.Session.SessionToken, map[string]any{"delta": 2}, http.StatusOK, &bumped); err != nil {
		return err
	}
	if string(bumped.Value) != "2" || bumped.Revision != 1 {
		return fmt.Errorf("plugin incr = (%s, rev %d), want (2, rev 1)", bumped.Value, bumped.Revision)
	}
	if err := env.expect(ctx, http.MethodPost, kvPath("/api/v1", namespace, "hits")+"/incr",
		env.AdminToken, nil, http.StatusOK, &bumped); err != nil {
		return err
	}
	if string(bumped.Value) != "3" || bumped.Revision != 2 {
		return fmt.Errorf("admin incr after plugin incr = (%s, rev %d), want (3, rev 2)",
			bumped.Value, bumped.Revision)
	}

	// A TTL set through the plugin realm answers with the expiry, and a later
	// set with no TTL clears it: a set defines the key entirely.
	var expiring kvEntry
	if err := env.expect(ctx, http.MethodPut, kvPath("/plugin/v1", namespace, "ephemeral"),
		plugin.Session.SessionToken, map[string]any{"value": 1, "ttlSeconds": 60},
		http.StatusOK, &expiring); err != nil {
		return err
	}
	if expiring.ExpiresAt == "" {
		return fmt.Errorf("a plugin set with ttlSeconds answered no expiresAt")
	}
	// Decoded into a fresh value: a field the response omits must read as
	// absent, not as whatever the previous decode left behind.
	var cleared kvEntry
	if err := env.expect(ctx, http.MethodPut, kvPath("/plugin/v1", namespace, "ephemeral"),
		plugin.Session.SessionToken, map[string]any{"value": 1}, http.StatusOK, &cleared); err != nil {
		return err
	}
	if cleared.ExpiresAt != "" {
		return fmt.Errorf("a set with no ttlSeconds kept the previous expiry %q", cleared.ExpiresAt)
	}

	// Delete, then a get and a retried delete both answer not_found.
	if err := env.expect(ctx, http.MethodDelete, kvPath("/plugin/v1", namespace, "greeting"),
		plugin.Session.SessionToken, nil, http.StatusNoContent, nil); err != nil {
		return err
	}
	if err := env.expectError(ctx, http.MethodGet, kvPath("/api/v1", namespace, "greeting"),
		env.AdminToken, nil, http.StatusNotFound, "not_found"); err != nil {
		return err
	}
	return env.expectError(ctx, http.MethodDelete, kvPath("/plugin/v1", namespace, "greeting"),
		plugin.Session.SessionToken, nil, http.StatusNotFound, "not_found")
}

func checkKVCompareAndSwap(ctx context.Context, env Env) error {
	namespace := uniqueKVNamespace()
	plugin, err := env.newKVPlugin(ctx, "conformance: kv cas", namespace)
	if err != nil {
		return err
	}

	// The issue's demo path: the plugin writes, writes again, and an admin
	// bot holding the first revision loses its CAS.
	for i := 1; i <= 2; i++ {
		var written kvEntry
		if err := env.expect(ctx, http.MethodPut, kvPath("/plugin/v1", namespace, "territory"),
			plugin.Session.SessionToken, map[string]any{"value": i}, http.StatusOK, &written); err != nil {
			return err
		}
		if written.Revision != int64(i) {
			return fmt.Errorf("write %d: revision = %d, want %d", i, written.Revision, i)
		}
	}

	stalePath := kvPath("/api/v1", namespace, "territory")
	resp, body, err := env.do(ctx, http.MethodPut, stalePath, env.AdminToken,
		map[string]any{"value": 99, "ifRevision": 1})
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusConflict {
		return fmt.Errorf("stale CAS: status = %d, want 409, body %q", resp.StatusCode, truncate(body))
	}
	var failure struct {
		Error struct {
			Code    string `json:"code"`
			Details struct {
				Revision int64 `json:"revision"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &failure); err != nil {
		return fmt.Errorf("stale CAS: error body %q is not JSON: %w", truncate(body), err)
	}
	if failure.Error.Code != "revision_mismatch" {
		return fmt.Errorf("stale CAS: code = %q, want revision_mismatch", failure.Error.Code)
	}
	if failure.Error.Details.Revision != 2 {
		return fmt.Errorf("stale CAS: details.revision = %d, want the current 2", failure.Error.Details.Revision)
	}

	// Losing must change nothing.
	var read kvEntry
	if err := env.expect(ctx, http.MethodGet, stalePath, env.AdminToken, nil, http.StatusOK, &read); err != nil {
		return err
	}
	if string(read.Value) != "2" || read.Revision != 2 {
		return fmt.Errorf("after a lost CAS the key is (%s, rev %d), want (2, rev 2) untouched",
			read.Value, read.Revision)
	}

	// The fresh revision wins.
	var swapped kvEntry
	if err := env.expect(ctx, http.MethodPut, stalePath, env.AdminToken,
		map[string]any{"value": 99, "ifRevision": 2}, http.StatusOK, &swapped); err != nil {
		return err
	}
	if swapped.Revision != 3 {
		return fmt.Errorf("fresh CAS: revision = %d, want 3", swapped.Revision)
	}

	// ifRevision 0 is create-only, so it must lose against an existing key.
	if err := env.expectError(ctx, http.MethodPut, stalePath, env.AdminToken,
		map[string]any{"value": 1, "ifRevision": 0}, http.StatusConflict, "revision_mismatch"); err != nil {
		return err
	}
	return nil
}

func checkKVIncrAtomic(ctx context.Context, env Env) error {
	namespace := uniqueKVNamespace()
	path := kvPath("/api/v1", namespace, "counter")

	const workers = 4
	const perWorker = 10

	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perWorker {
				var bumped kvEntry
				if err := env.expect(ctx, http.MethodPost, path+"/incr", env.AdminToken,
					map[string]any{"delta": 1}, http.StatusOK, &bumped); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		return err
	}

	var read kvEntry
	if err := env.expect(ctx, http.MethodGet, path, env.AdminToken, nil, http.StatusOK, &read); err != nil {
		return err
	}
	if string(read.Value) != strconv.Itoa(workers*perWorker) {
		return fmt.Errorf("after %d concurrent incrs the value is %s, want %d: a delta vanished",
			workers*perWorker, read.Value, workers*perWorker)
	}

	// The decrement is a negative delta, and an empty body means delta 1.
	var bumped kvEntry
	if err := env.expect(ctx, http.MethodPost, path+"/incr", env.AdminToken,
		map[string]any{"delta": -5}, http.StatusOK, &bumped); err != nil {
		return err
	}
	if string(bumped.Value) != strconv.Itoa(workers*perWorker-5) {
		return fmt.Errorf("decrement answered %s, want %d", bumped.Value, workers*perWorker-5)
	}
	if err := env.expect(ctx, http.MethodPost, path+"/incr", env.AdminToken,
		nil, http.StatusOK, &bumped); err != nil {
		return err
	}
	if string(bumped.Value) != strconv.Itoa(workers*perWorker-4) {
		return fmt.Errorf("bodyless incr answered %s, want %d", bumped.Value, workers*perWorker-4)
	}

	// Incr on a non-integer value refuses with conflict and changes nothing.
	if err := env.expect(ctx, http.MethodPut, kvPath("/api/v1", namespace, "label"), env.AdminToken,
		map[string]any{"value": "words"}, http.StatusOK, nil); err != nil {
		return err
	}
	if err := env.expectError(ctx, http.MethodPost, kvPath("/api/v1", namespace, "label")+"/incr",
		env.AdminToken, nil, http.StatusConflict, "conflict"); err != nil {
		return err
	}
	if err := env.expect(ctx, http.MethodGet, kvPath("/api/v1", namespace, "label"),
		env.AdminToken, nil, http.StatusOK, &read); err != nil {
		return err
	}
	if string(read.Value) != `"words"` || read.Revision != 1 {
		return fmt.Errorf("a refused incr changed the key to (%s, rev %d)", read.Value, read.Revision)
	}
	return nil
}

func checkKVTTL(ctx context.Context, env Env) error {
	namespace := uniqueKVNamespace()
	path := kvPath("/api/v1", namespace, "ephemeral")

	var written kvEntry
	if err := env.expect(ctx, http.MethodPut, path, env.AdminToken,
		map[string]any{"value": 1, "ttlSeconds": 1}, http.StatusOK, &written); err != nil {
		return err
	}
	if written.ExpiresAt == "" {
		return fmt.Errorf("a set with ttlSeconds answered no expiresAt")
	}
	if _, err := time.Parse(time.RFC3339, written.ExpiresAt); err != nil {
		return fmt.Errorf("expiresAt %q is not RFC 3339: %w", written.ExpiresAt, err)
	}

	// From the moment expiry passes the key must read as absent, whether or
	// not the hub has physically deleted it yet.
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, body, err := env.do(ctx, http.MethodGet, path, env.AdminToken, nil)
		if err != nil {
			return err
		}
		if resp.StatusCode == http.StatusNotFound {
			if err := assertErrorCode(http.MethodGet, path, body, "not_found"); err != nil {
				return err
			}
			break
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("waiting for expiry: status = %d, body %q", resp.StatusCode, truncate(body))
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the key still answers 200 well past its one-second TTL")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}

	// An expired key counts as absent to a compare-and-swap: the create-only
	// guard wins, and the recreated key starts over at revision 1.
	var recreated kvEntry
	if err := env.expect(ctx, http.MethodPut, path, env.AdminToken,
		map[string]any{"value": 2, "ifRevision": 0}, http.StatusOK, &recreated); err != nil {
		return fmt.Errorf("create-only set over an expired key: %w", err)
	}
	if recreated.Revision != 1 {
		return fmt.Errorf("recreated key: revision = %d, want a fresh 1", recreated.Revision)
	}
	if recreated.ExpiresAt != "" {
		return fmt.Errorf("recreated key kept the expired TTL, expiresAt %q", recreated.ExpiresAt)
	}
	return nil
}

func checkKVConfinement(ctx context.Context, env Env) error {
	declared := uniqueKVNamespace()
	undeclared := uniqueKVNamespace()

	// Plugin realm: a manifest-declared namespace answers, any other is
	// forbidden, and so is everything while no manifest is stored.
	bare, err := env.newFakePlugin(ctx, "conformance: kv no manifest", 5)
	if err != nil {
		return err
	}
	if err := env.expectError(ctx, http.MethodPut, kvPath("/plugin/v1", declared, "key"),
		bare.Session.SessionToken, map[string]any{"value": 1},
		http.StatusForbidden, "forbidden"); err != nil {
		return fmt.Errorf("KV with no stored manifest: %w", err)
	}

	plugin, err := env.newKVPlugin(ctx, "conformance: kv confinement", declared)
	if err != nil {
		return err
	}
	if err := env.expect(ctx, http.MethodPut, kvPath("/plugin/v1", declared, "key"),
		plugin.Session.SessionToken, map[string]any{"value": 1}, http.StatusOK, nil); err != nil {
		return err
	}
	for _, op := range []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodGet, kvPath("/plugin/v1", undeclared, "key"), nil},
		{http.MethodPut, kvPath("/plugin/v1", undeclared, "key"), map[string]any{"value": 1}},
		{http.MethodDelete, kvPath("/plugin/v1", undeclared, "key"), nil},
		{http.MethodPost, kvPath("/plugin/v1", undeclared, "key") + "/incr", nil},
	} {
		if err := env.expectError(ctx, op.method, op.path, plugin.Session.SessionToken,
			op.body, http.StatusForbidden, "forbidden"); err != nil {
			return fmt.Errorf("undeclared namespace over the Plugin API: %w", err)
		}
	}

	// Admin realm: kv:rw:{namespace} gates by the namespace in the path.
	scoped, err := env.mintToken(ctx, "conformance: kv scoped", "kv:rw:"+declared)
	if err != nil {
		return err
	}
	if err := env.expect(ctx, http.MethodGet, kvPath("/api/v1", declared, "key"),
		scoped.Secret, nil, http.StatusOK, nil); err != nil {
		return fmt.Errorf("the granted namespace was refused: %w", err)
	}
	for _, op := range []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodGet, kvPath("/api/v1", undeclared, "key"), nil},
		{http.MethodPut, kvPath("/api/v1", undeclared, "key"), map[string]any{"value": 1}},
		{http.MethodDelete, kvPath("/api/v1", undeclared, "key"), nil},
		{http.MethodPost, kvPath("/api/v1", undeclared, "key") + "/incr", nil},
	} {
		if err := env.refused(ctx, op.method, op.path, scoped.Secret, op.body,
			"a kv:rw grant on one namespace reached another"); err != nil {
			return err
		}
	}

	// A token with no kv grant at all is refused before any namespace math.
	unrelated, err := env.mintToken(ctx, "conformance: kv unrelated", "servers:read")
	if err != nil {
		return err
	}
	return env.refused(ctx, http.MethodGet, kvPath("/api/v1", declared, "key"),
		unrelated.Secret, nil, "a token with no kv scope read the store")
}

// kvKeyRow is one row of a key listing (spec section 12.2). Value is declared
// so that a hub echoing one can be caught: a listing is a browse, not a bulk
// read.
type kvKeyRow struct {
	Key       string          `json:"key"`
	Revision  int64           `json:"revision"`
	ExpiresAt string          `json:"expiresAt"`
	Value     json.RawMessage `json:"value"`
}

type kvKeyPage struct {
	Namespace  string     `json:"namespace"`
	Keys       []kvKeyRow `json:"keys"`
	NextCursor string     `json:"nextCursor"`
}

type kvNamespaceListing struct {
	Namespaces []struct {
		Namespace string `json:"namespace"`
		Keys      int64  `json:"keys"`
	} `json:"namespaces"`
}

// kvListPath builds one page request for a namespace's keys.
func kvListPath(namespace string, parameters url.Values) string {
	path := "/api/v1/kv/" + namespace
	if encoded := parameters.Encode(); encoded != "" {
		path += "?" + encoded
	}
	return path
}

// walkKVKeys pages a namespace to exhaustion the way a client does and returns
// the keys in the order they arrived.
func (e Env) walkKVKeys(ctx context.Context, bearer, namespace, prefix string, limit int) ([]string, error) {
	var (
		walked []string
		cursor string
	)
	for pages := 0; ; pages++ {
		if pages > 50 {
			return nil, fmt.Errorf("the key listing walk did not terminate after %d pages", pages)
		}
		parameters := url.Values{"limit": {strconv.Itoa(limit)}}
		if prefix != "" {
			parameters.Set("prefix", prefix)
		}
		if cursor != "" {
			parameters.Set("cursor", cursor)
		}
		path := kvListPath(namespace, parameters)

		var page kvKeyPage
		if err := e.expect(ctx, http.MethodGet, path, bearer, nil, http.StatusOK, &page); err != nil {
			return nil, err
		}
		if page.Namespace != namespace {
			return nil, fmt.Errorf("%s echoed namespace %q, want %q", path, page.Namespace, namespace)
		}
		if len(page.Keys) > limit {
			return nil, fmt.Errorf("%s answered %d keys past the limit of %d", path, len(page.Keys), limit)
		}
		for _, row := range page.Keys {
			if len(row.Value) != 0 {
				return nil, fmt.Errorf("%s carried a value for key %q; a listing does not include values",
					path, row.Key)
			}
			walked = append(walked, row.Key)
		}
		if page.NextCursor == "" {
			return walked, nil
		}
		if page.NextCursor == cursor {
			return nil, fmt.Errorf("%s answered the cursor it was given, which loops forever", path)
		}
		cursor = page.NextCursor
	}
}

func checkKVListPaging(ctx context.Context, env Env) error {
	namespace := uniqueKVNamespace()

	// More keys than one page holds, so the walk crosses a page boundary
	// several times rather than once.
	const total = 13
	const pageSize = 5
	written := map[string]bool{}
	for i := range total {
		key := fmt.Sprintf("player.%03d", i)
		written[key] = true
		if err := env.expect(ctx, http.MethodPut, kvPath("/api/v1", namespace, key),
			env.AdminToken, map[string]any{"value": i}, http.StatusOK, nil); err != nil {
			return err
		}
	}

	walked, err := env.walkKVKeys(ctx, env.AdminToken, namespace, "", pageSize)
	if err != nil {
		return err
	}
	if !sort.StringsAreSorted(walked) {
		return fmt.Errorf("the walk is not key ascending: %v", walked)
	}
	seen := map[string]int{}
	for _, key := range walked {
		seen[key]++
		if seen[key] > 1 {
			return fmt.Errorf("key %q came back on two pages of one walk", key)
		}
		if !written[key] {
			return fmt.Errorf("the walk returned %q, which was never written", key)
		}
	}
	for key := range written {
		if seen[key] == 0 {
			return fmt.Errorf("the walk skipped %q, which was live for the whole walk", key)
		}
	}

	// An over-large limit is clamped, not refused; a limit that is not a page
	// size at all, and a cursor this hub could not have issued, are refused
	// rather than ignored.
	var clamped kvKeyPage
	if err := env.expect(ctx, http.MethodGet,
		kvListPath(namespace, url.Values{"limit": {"100000"}}), env.AdminToken,
		nil, http.StatusOK, &clamped); err != nil {
		return fmt.Errorf("an over-large limit was refused instead of clamped: %w", err)
	}
	if len(clamped.Keys) != total {
		return fmt.Errorf("a clamped limit answered %d keys, want all %d", len(clamped.Keys), total)
	}
	if clamped.NextCursor != "" {
		return fmt.Errorf("the last page carried nextCursor %q; it must be absent", clamped.NextCursor)
	}
	for _, bad := range []struct{ parameter, value string }{
		{"limit", "0"},
		{"limit", "not-a-number"},
		{"cursor", "!not base64!"},
		{"cursor", base64.RawURLEncoding.EncodeToString([]byte("bad..key"))},
	} {
		path := kvListPath(namespace, url.Values{bad.parameter: {bad.value}})
		if err := env.expectError(ctx, http.MethodGet, path, env.AdminToken, nil,
			http.StatusBadRequest, "bad_request"); err != nil {
			return fmt.Errorf("%s=%q was not refused: %w", bad.parameter, bad.value, err)
		}
	}
	return nil
}

func checkKVListPrefixAndExpiry(ctx context.Context, env Env) error {
	namespace := uniqueKVNamespace()

	for _, key := range []string{"balance.1", "balance.2", "balances", "bank.1"} {
		if err := env.expect(ctx, http.MethodPut, kvPath("/api/v1", namespace, key),
			env.AdminToken, map[string]any{"value": 1}, http.StatusOK, nil); err != nil {
			return err
		}
	}

	// prefix is a literal prefix of the key, not a pattern: "balance." takes
	// the two dotted keys, "balance" takes "balances" as well, and a prefix
	// outside the key alphabet matches nothing rather than failing.
	for _, one := range []struct {
		prefix string
		want   []string
	}{
		{"balance.", []string{"balance.1", "balance.2"}},
		{"balance", []string{"balance.1", "balance.2", "balances"}},
		{"bank", []string{"bank.1"}},
		{"no-such-prefix", nil},
		{"balance/", nil},
	} {
		got, err := env.walkKVKeys(ctx, env.AdminToken, namespace, one.prefix, 2)
		if err != nil {
			return err
		}
		if len(got) != len(one.want) {
			return fmt.Errorf("prefix %q listed %v, want %v", one.prefix, got, one.want)
		}
		for i := range got {
			if got[i] != one.want[i] {
				return fmt.Errorf("prefix %q listed %v, want %v", one.prefix, got, one.want)
			}
		}
	}

	// A key past its expiry must not appear, whether or not the hub has
	// physically deleted it yet (spec section 12.1).
	expiring := uniqueKVNamespace()
	if err := env.expect(ctx, http.MethodPut, kvPath("/api/v1", expiring, "kept"),
		env.AdminToken, map[string]any{"value": 1}, http.StatusOK, nil); err != nil {
		return err
	}
	var written kvEntry
	if err := env.expect(ctx, http.MethodPut, kvPath("/api/v1", expiring, "doomed"),
		env.AdminToken, map[string]any{"value": 1, "ttlSeconds": 1}, http.StatusOK, &written); err != nil {
		return err
	}
	if written.ExpiresAt == "" {
		return fmt.Errorf("a set with ttlSeconds answered no expiresAt")
	}

	var page kvKeyPage
	if err := env.expect(ctx, http.MethodGet, kvListPath(expiring, nil), env.AdminToken,
		nil, http.StatusOK, &page); err != nil {
		return err
	}
	if len(page.Keys) != 2 {
		return fmt.Errorf("before expiry the listing carries %d keys, want 2", len(page.Keys))
	}
	for _, row := range page.Keys {
		if row.Key == "doomed" && row.ExpiresAt == "" {
			return fmt.Errorf("a listed key with a TTL carried no expiresAt")
		}
		if row.Key == "kept" && row.ExpiresAt != "" {
			return fmt.Errorf("a listed key with no TTL carried expiresAt %q", row.ExpiresAt)
		}
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := env.expect(ctx, http.MethodGet, kvListPath(expiring, nil), env.AdminToken,
			nil, http.StatusOK, &page); err != nil {
			return err
		}
		if len(page.Keys) == 1 && page.Keys[0].Key == "kept" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the listing still carries %d keys well past a one-second TTL", len(page.Keys))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func checkKVNamespaceListing(ctx context.Context, env Env) error {
	granted := uniqueKVNamespace()
	withheld := uniqueKVNamespace()

	for _, one := range []struct{ namespace, key string }{
		{granted, "one"},
		{granted, "two"},
		{withheld, "one"},
	} {
		if err := env.expect(ctx, http.MethodPut, kvPath("/api/v1", one.namespace, one.key),
			env.AdminToken, map[string]any{"value": 1}, http.StatusOK, nil); err != nil {
			return err
		}
	}

	listing := func(bearer string) (map[string]int64, error) {
		var page kvNamespaceListing
		if err := env.expect(ctx, http.MethodGet, "/api/v1/kv", bearer, nil,
			http.StatusOK, &page); err != nil {
			return nil, err
		}
		counts := map[string]int64{}
		names := make([]string, 0, len(page.Namespaces))
		for _, one := range page.Namespaces {
			counts[one.Namespace] = one.Keys
			names = append(names, one.Namespace)
		}
		if !sort.StringsAreSorted(names) {
			return nil, fmt.Errorf("the namespace listing is not name ascending: %v", names)
		}
		return counts, nil
	}

	// The suite's own credential sees both namespaces with their live counts.
	all, err := listing(env.AdminToken)
	if err != nil {
		return err
	}
	if all[granted] != 2 {
		return fmt.Errorf("namespace %s listed %d live keys, want 2", granted, all[granted])
	}
	if all[withheld] != 1 {
		return fmt.Errorf("namespace %s listed %d live keys, want 1", withheld, all[withheld])
	}

	// A token narrowed to one namespace sees that one and not the other:
	// enumeration reveals nothing a key-by-key read could not have found.
	narrow, err := env.mintToken(ctx, "conformance: kv listing narrow", "kv:rw:"+granted)
	if err != nil {
		return err
	}
	scoped, err := listing(narrow.Secret)
	if err != nil {
		return err
	}
	if scoped[granted] != 2 {
		return fmt.Errorf("a kv:rw:%s token does not see its own namespace in the listing", granted)
	}
	if _, leaked := scoped[withheld]; leaked {
		return fmt.Errorf("a kv:rw:%s token saw namespace %s, which it may not read", granted, withheld)
	}

	// The same token may list that namespace's keys and no other's.
	if err := env.expect(ctx, http.MethodGet, kvListPath(granted, nil), narrow.Secret,
		nil, http.StatusOK, nil); err != nil {
		return fmt.Errorf("the granted namespace's key listing was refused: %w", err)
	}
	if err := env.refused(ctx, http.MethodGet, kvListPath(withheld, nil), narrow.Secret, nil,
		"a kv:rw grant on one namespace listed another's keys"); err != nil {
		return err
	}

	// A token with no kv grant at all is refused both listings outright.
	unrelated, err := env.mintToken(ctx, "conformance: kv listing unrelated", "servers:read")
	if err != nil {
		return err
	}
	if err := env.refused(ctx, http.MethodGet, "/api/v1/kv", unrelated.Secret, nil,
		"a token with no kv grant enumerated the store's namespaces"); err != nil {
		return err
	}
	return env.refused(ctx, http.MethodGet, kvListPath(granted, nil), unrelated.Secret, nil,
		"a token with no kv grant listed a namespace's keys")
}

func checkKVValidation(ctx context.Context, env Env) error {
	namespace := uniqueKVNamespace()

	bad := func(method, path string, body any, why string) error {
		if err := env.expectError(ctx, method, path, env.AdminToken, body,
			http.StatusBadRequest, "bad_request"); err != nil {
			return fmt.Errorf("%s: %w", why, err)
		}
		return nil
	}

	oversized := make([]byte, 17000)
	for i := range oversized {
		oversized[i] = 'x'
	}
	checks := []error{
		bad(http.MethodGet, kvPath("/api/v1", "bad..namespace", "key"), nil,
			"a namespace with an empty segment was accepted"),
		bad(http.MethodGet, kvPath("/api/v1", namespace, "bad..key"), nil,
			"a key with an empty segment was accepted"),
		bad(http.MethodPut, kvPath("/api/v1", namespace, "key"), map[string]any{},
			"a set with no value was accepted"),
		bad(http.MethodPut, kvPath("/api/v1", namespace, "key"), map[string]any{"value": nil},
			"a null value was accepted; null must read as the field being absent"),
		bad(http.MethodPut, kvPath("/api/v1", namespace, "key"),
			map[string]any{"value": string(oversized)},
			"a value over the 16384 byte cap was accepted"),
		bad(http.MethodPut, kvPath("/api/v1", namespace, "key"),
			map[string]any{"value": 1, "ttlSeconds": 0},
			"ttlSeconds 0 was accepted"),
		bad(http.MethodPut, kvPath("/api/v1", namespace, "key"),
			map[string]any{"value": 1, "ifRevision": -1},
			"a negative ifRevision was accepted"),
		bad(http.MethodPost, kvPath("/api/v1", namespace, "key")+"/incr",
			map[string]any{"delta": int64(1) << 53},
			"a delta at the 2^53 exactness bound was accepted"),
	}
	for _, err := range checks {
		if err != nil {
			return err
		}
	}
	return nil
}
