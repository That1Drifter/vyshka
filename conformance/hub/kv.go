package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
			return assertErrorCode(http.MethodGet, path, body, "not_found")
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
		bad(http.MethodPut, kvPath("/api/v1", namespace, "key"),
			map[string]any{"value": string(oversized)},
			"a value over the 16384 byte cap was accepted"),
		bad(http.MethodPut, kvPath("/api/v1", namespace, "key"),
			map[string]any{"value": 1, "ttlSeconds": 0},
			"ttlSeconds 0 was accepted"),
		bad(http.MethodPut, kvPath("/api/v1", namespace, "key"),
			map[string]any{"value": 1, "ifRevision": -1},
			"a negative ifRevision was accepted"),
	}
	for _, err := range checks {
		if err != nil {
			return err
		}
	}
	return nil
}
