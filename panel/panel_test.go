package panel_test

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
	"github.com/That1Drifter/vyshka/panel"
)

// Mounted in a real hub, the page the browser fetches carries the policy:
// the hub's routing must not put anything between the request and the
// handler that answers without the headers.
func TestMountedPanelCarriesPolicy(t *testing.T) {
	t.Parallel()
	server, err := hub.New(context.Background(), hub.Config{
		DatabaseURL: filepath.Join(t.TempDir(), "mount.db"),
		AdminToken:  e2eAdminToken,
		Logger:      slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Panel:       panel.Handler(),
	})
	if err != nil {
		t.Fatalf("boot hub: %v", err)
	}
	t.Cleanup(func() { server.Close() })

	for _, path := range []string{"/panel/", "/panel/app.js", "/panel/style.css"} {
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, recorder.Code)
		}
		if csp := recorder.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
			t.Errorf("GET %s through the hub carries CSP %q", path, csp)
		}
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/panel/missing.js", nil))
	if recorder.Code != http.StatusNotFound || !strings.Contains(recorder.Body.String(), `"not_found"`) {
		t.Fatalf("GET /panel/missing.js = %d %q, want a protocol-shaped 404", recorder.Code, recorder.Body.String())
	}
}

func serve(t *testing.T, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	panel.Handler().ServeHTTP(recorder, httptest.NewRequest(method, path, nil))
	return recorder
}

// The three files the panel is made of are served from the root, with the
// headers that confine a page holding a bearer token to its own origin.
func TestHandlerServesEmbeddedFilesWithSecurityHeaders(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		path, contentType, marker string
	}{
		{"/", "text/html", `<script type="module" src="app.js">`},
		{"/app.js", "text/javascript", "const API = '/api/v1'"},
		{"/style.css", "text/css", ":root"},
	} {
		recorder := serve(t, http.MethodGet, tc.path)
		if recorder.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", tc.path, recorder.Code)
		}
		if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, tc.contentType) {
			t.Errorf("GET %s Content-Type = %q, want %s", tc.path, got, tc.contentType)
		}
		if !strings.Contains(recorder.Body.String(), tc.marker) {
			t.Errorf("GET %s body lacks %q", tc.path, tc.marker)
		}
		csp := recorder.Header().Get("Content-Security-Policy")
		for _, directive := range []string{"default-src 'none'", "script-src 'self'", "connect-src 'self'", "frame-ancestors 'none'"} {
			if !strings.Contains(csp, directive) {
				t.Errorf("GET %s CSP %q lacks %q", tc.path, csp, directive)
			}
		}
		if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
			t.Errorf("GET %s CSP %q relaxes inline script or eval", tc.path, csp)
		}
		if got := recorder.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("GET %s X-Content-Type-Options = %q", tc.path, got)
		}
		if got := recorder.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("GET %s Cache-Control = %q, want no-cache", tc.path, got)
		}
	}
}

// The page must not carry inline script or style: the policy would block it,
// and a page that depended on one would be a page that shipped broken.
func TestIndexHasNoInlineScriptOrStyle(t *testing.T) {
	t.Parallel()
	body := serve(t, http.MethodGet, "/").Body.String()
	for _, forbidden := range []string{"<script>", "<style", " onclick=", " onload=", "javascript:"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("index.html contains %q, which the Content-Security-Policy forbids", forbidden)
		}
	}
	// app.js builds every node through createElement and text nodes; a
	// markup sink would let a plugin's manifest label become script.
	script := serve(t, http.MethodGet, "/app.js").Body.String()
	for _, sink := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval("} {
		if strings.Contains(script, sink) {
			t.Errorf("app.js uses %s, a markup sink the panel must not have", sink)
		}
	}
}

// Anything that is not one of the shipped files is a 404 in the protocol's
// error shape: no directory listings, no index fallback for unknown paths, no
// plain text from FileServer.
func TestHandlerRefusesWhatItDoesNotShip(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"/missing.js", "/nested/", "/nested/index.html", "/..%2fpanel.go"} {
		recorder := serve(t, http.MethodGet, path)
		if recorder.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, recorder.Code)
		}
		var body struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || body.Error.Code != "not_found" {
			t.Errorf("GET %s body = %q, want the protocol's not_found shape", path, recorder.Body.String())
		}
	}
	// FileServer answers /index.html with a redirect to the directory. It
	// must be relative, or a prefix the hub mounts the panel under is lost.
	recorder := serve(t, http.MethodGet, "/index.html")
	if recorder.Code != http.StatusMovedPermanently {
		t.Fatalf("GET /index.html = %d, want 301", recorder.Code)
	}
	if location := recorder.Header().Get("Location"); strings.HasPrefix(location, "/") {
		t.Errorf("GET /index.html redirects to absolute %q, which would drop the mount prefix", location)
	}
}
