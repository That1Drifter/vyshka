package hub_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/That1Drifter/vyshka/hub"
)

// newTestServerWithPanel boots a hub with a stand-in panel: the mounting is
// what these tests grade, not the panel's content, which has its own tests
// beside it.
func newTestServerWithPanel(t *testing.T, panel http.Handler) *hub.Server {
	t.Helper()
	server, err := hub.New(context.Background(), hub.Config{
		DatabaseURL: filepath.Join(t.TempDir(), "test.db"),
		AdminToken:  testAdminToken,
		Logger:      slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Panel:       panel,
	})
	if err != nil {
		t.Fatalf("boot hub: %v", err)
	}
	t.Cleanup(func() { server.Close() })
	return server
}

func get(t *testing.T, server *hub.Server, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(method, path, nil))
	return recorder
}

func protocolErrorCode(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q is not the protocol error shape: %v", recorder.Body.String(), err)
	}
	return body.Error.Code
}

// A mounted panel answers under /panel/ with the prefix stripped, / sends a
// browser there, and nothing else about the hub's routing changes: the API
// realms keep their protocol-shaped refusals and unrouted paths stay 404.
func TestPanelMountedUnderPrefix(t *testing.T) {
	t.Parallel()
	stub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "stub:"+r.URL.Path)
	})
	server := newTestServerWithPanel(t, stub)

	root := get(t, server, http.MethodGet, "/")
	if root.Code != http.StatusFound || root.Header().Get("Location") != "/panel/" {
		t.Fatalf("GET / = %d %q, want 302 to /panel/", root.Code, root.Header().Get("Location"))
	}

	index := get(t, server, http.MethodGet, "/panel/")
	if index.Code != http.StatusOK || index.Body.String() != "stub:/" {
		t.Fatalf("GET /panel/ = %d %q, want the panel's root", index.Code, index.Body.String())
	}
	asset := get(t, server, http.MethodGet, "/panel/app.js")
	if asset.Code != http.StatusOK || asset.Body.String() != "stub:/app.js" {
		t.Fatalf("GET /panel/app.js = %d %q, want the prefix stripped", asset.Code, asset.Body.String())
	}

	// The slashless form reaches the panel too, by the mux's own redirect
	// (whichever 3xx the mux favours; the target is what matters).
	bare := get(t, server, http.MethodGet, "/panel")
	if bare.Code < 300 || bare.Code > 399 || bare.Header().Get("Location") != "/panel/" {
		t.Fatalf("GET /panel = %d %q, want a redirect to /panel/", bare.Code, bare.Header().Get("Location"))
	}

	// Only reads reach a static surface, and the refusal keeps the protocol
	// shape like every other route.
	post := get(t, server, http.MethodPost, "/panel/")
	if post.Code != http.StatusMethodNotAllowed || protocolErrorCode(t, post) != "method_not_allowed" {
		t.Fatalf("POST /panel/ = %d %q, want a protocol-shaped 405", post.Code, post.Body.String())
	}

	// The root is a GET route now, so a write to it is a 405 like any other
	// wrong method, not a 404.
	postRoot := get(t, server, http.MethodPost, "/")
	if postRoot.Code != http.StatusMethodNotAllowed || protocolErrorCode(t, postRoot) != "method_not_allowed" {
		t.Fatalf("POST / = %d %q, want a protocol-shaped 405", postRoot.Code, postRoot.Body.String())
	}

	// The redirect is the exact root only. An unrouted path is still the
	// JSON 404 an API client can parse, not a bounce into the panel.
	missing := get(t, server, http.MethodGet, "/nope")
	if missing.Code != http.StatusNotFound || protocolErrorCode(t, missing) != "not_found" {
		t.Fatalf("GET /nope = %d %q, want a protocol-shaped 404", missing.Code, missing.Body.String())
	}
	if strings.Contains(missing.Body.String(), "stub:") {
		t.Fatalf("GET /nope reached the panel: %q", missing.Body.String())
	}
}

// Without a panel the hub serves none: / and /panel/ are unrouted paths.
func TestNoPanelLeavesRootUnrouted(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)

	for _, path := range []string{"/", "/panel/", "/panel/app.js"} {
		recorder := get(t, server, http.MethodGet, path)
		if recorder.Code != http.StatusNotFound || protocolErrorCode(t, recorder) != "not_found" {
			t.Fatalf("GET %s = %d %q, want a protocol-shaped 404", path, recorder.Code, recorder.Body.String())
		}
	}
}
