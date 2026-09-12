package panel_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
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

// The four files the panel is made of are served from the root, with the
// headers that confine a page holding a bearer token to its own origin.
func TestHandlerServesEmbeddedFilesWithSecurityHeaders(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		path, contentType, marker string
	}{
		{"/", "text/html", `<script type="module" src="app.js">`},
		{"/app.js", "text/javascript", "const API = '/api/v1'"},
		{"/map.js", "text/javascript", "export function createMap"},
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
	// app.js and map.js build every node through createElement and text
	// nodes; a markup sink would let a plugin's manifest label or a player's
	// name become script.
	for _, file := range []string{"/app.js", "/map.js"} {
		script := serve(t, http.MethodGet, file).Body.String()
		for _, sink := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval("} {
			if strings.Contains(script, sink) {
				t.Errorf("%s uses %s, a markup sink the panel must not have", file, sink)
			}
		}
	}
}

// Without a maps directory the tileset surface does not exist: every path
// under /maps/ is the same protocol-shaped 404 as any other unknown path.
func TestMapsRefusedWithoutADirectory(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"/maps/", "/maps/chernarusplus/manifest.json", "/maps/chernarusplus/tiles/0/0/0.webp"} {
		recorder := serve(t, http.MethodGet, path)
		if recorder.Code != http.StatusNotFound || !strings.Contains(recorder.Body.String(), `"not_found"`) {
			t.Errorf("GET %s = %d %q, want a protocol-shaped 404", path, recorder.Code, recorder.Body.String())
		}
	}
}

// A maps directory serves exactly three things: the index of worlds that
// hold a manifest, each world's manifest, and its tiles. Everything else
// beside them (a build's intermediates, a directory, a path that climbs)
// is refused, and the responses carry the panel's headers.
func TestMapsServeIndexManifestAndTiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(parts ...string) {
		t.Helper()
		path := filepath.Join(append([]string{dir}, parts...)...)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("content of "+strings.Join(parts, "/")), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("chernarusplus", "manifest.json")
	write("chernarusplus", "tiles", "0", "0", "0.webp")
	write("chernarusplus", "tiles", "1", "0", "1.png")
	write("chernarusplus", "tiles", "0", "0", "index.html") // a tile with FileServer's magic name
	write("chernarusplus", "master.png")                    // a build intermediate beside the tiles
	write("enoch", "manifest.json")
	write("half-built", "tiles", "0", "0", "0.webp") // no manifest: not a world
	write("bad name", "manifest.json")               // outside the world shape
	write("stray.txt")
	if err := os.MkdirAll(filepath.Join(dir, "dir-as-manifest", "manifest.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A dataset built elsewhere and linked in is a world like any other:
	// listed, not only served. Creating the link needs a privilege on some
	// Windows setups; the rest of the test does not depend on it.
	built := filepath.Join(t.TempDir(), "sakhal-build")
	if err := os.MkdirAll(built, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(built, "manifest.json"), []byte("content of sakhal/manifest.json"), 0o644); err != nil {
		t.Fatal(err)
	}
	wantWorlds := "chernarusplus,enoch"
	linked := false
	if err := os.Symlink(built, filepath.Join(dir, "sakhal")); err == nil {
		wantWorlds = "chernarusplus,enoch,sakhal"
		linked = true
		// Links are honoured at the world and at its tiles directory, and
		// nowhere below: a link under tiles that leaves the resolved tiles
		// directory, whether to a build intermediate beside it or to a
		// file outside the maps directory, is not a tile.
		outside := filepath.Join(t.TempDir(), "secret.txt")
		if err := os.WriteFile(outside, []byte("not a tile"), 0o644); err != nil {
			t.Fatal(err)
		}
		tilesElsewhere := filepath.Join(t.TempDir(), "sakhal-tiles")
		if err := os.MkdirAll(filepath.Join(tilesElsewhere, "0", "0"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tilesElsewhere, "0", "0", "0.webp"), []byte("linked tile"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(tilesElsewhere, filepath.Join(built, "tiles")); err != nil {
			t.Fatal(err)
		}
		for name, target := range map[string]string{
			"leak": outside,                                      // a file outside the maps directory
			"back": built,                                        // the world itself, beside its intermediates
			"self": filepath.Join(tilesElsewhere, "0", "0"),      // a link that stays inside the tiles
			"maps": dir,                                          // the whole maps directory
			"up":   filepath.Join(dir, "chernarusplus", "tiles"), // another world's tiles
		} {
			if err := os.Symlink(target, filepath.Join(tilesElsewhere, name)); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(built, "master.png"), []byte("intermediate"), 0o644); err != nil {
			t.Fatal(err)
		}
	} else {
		t.Logf("symlink not available here, the linked cases are not exercised: %v", err)
	}
	handler := panel.NewHandler(panel.Config{MapsDir: dir})
	get := func(path string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		return recorder
	}

	index := get("/maps/")
	if index.Code != http.StatusOK {
		t.Fatalf("GET /maps/ = %d %q", index.Code, index.Body.String())
	}
	var listed struct {
		Worlds []string `json:"worlds"`
	}
	if err := json.Unmarshal(index.Body.Bytes(), &listed); err != nil {
		t.Fatalf("index is not JSON: %v: %q", err, index.Body.String())
	}
	if strings.Join(listed.Worlds, ",") != wantWorlds {
		t.Errorf("index lists %q, want %s", listed.Worlds, wantWorlds)
	}
	if got := index.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("index Cache-Control = %q, want no-cache", got)
	}
	if csp := index.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("index carries CSP %q", csp)
	}

	manifest := get("/maps/chernarusplus/manifest.json")
	if manifest.Code != http.StatusOK || manifest.Body.String() != "content of chernarusplus/manifest.json" {
		t.Fatalf("GET manifest = %d %q", manifest.Code, manifest.Body.String())
	}
	if got := manifest.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("manifest Cache-Control = %q, want no-cache: a regenerated dataset must be picked up", got)
	}
	if got := manifest.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("manifest Content-Type = %q", got)
	}

	served := map[string]string{
		"/maps/chernarusplus/tiles/0/0/0.webp": "image/webp",
		"/maps/chernarusplus/tiles/1/0/1.png":  "image/png",
		// Served as the file it is: FileServer would have redirected it.
		"/maps/chernarusplus/tiles/0/0/index.html": "text/html",
	}
	if linked {
		served["/maps/sakhal/tiles/0/0/0.webp"] = "image/webp"    // through the linked world and its linked tiles
		served["/maps/sakhal/tiles/self/0.webp"] = "image/webp"   // a link that stays inside the tiles
		served["/maps/sakhal/manifest.json"] = "application/json" // the linked world's manifest
	}
	for path, contentType := range served {
		tile := get(path)
		if tile.Code != http.StatusOK {
			t.Fatalf("GET %s = %d %q", path, tile.Code, tile.Body.String())
		}
		if got := tile.Header().Get("Content-Type"); !strings.HasPrefix(got, contentType) {
			t.Errorf("GET %s Content-Type = %q, want %s", path, got, contentType)
		}
		wantCache := "public, max-age=3600"
		if strings.HasSuffix(path, "manifest.json") {
			wantCache = "no-cache"
		}
		if got := tile.Header().Get("Cache-Control"); got != wantCache {
			t.Errorf("GET %s Cache-Control = %q, want %s", path, got, wantCache)
		}
		if got := tile.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("GET %s X-Content-Type-Options = %q", path, got)
		}
	}

	refused := []string{
		"/maps/chernarusplus/master.png",             // beside the tiles, not served
		"/maps/chernarusplus/tiles",                  // a directory
		"/maps/chernarusplus/tiles/",                 // a directory listing
		"/maps/chernarusplus/tiles/0/0/",             // a directory listing
		"/maps/chernarusplus",                        // the world itself
		"/maps/chernarusplus/",                       // the world itself
		"/maps/chernarusplus/tiles/../master.png",    // a climb inside the world
		"/maps/../chernarusplus/manifest.json",       // a climb out of the maps directory
		"/maps/bad%20name/manifest.json",             // outside the world shape
		"/maps/half-built/tiles/0/0/0.webp",          // tiles of a world with no manifest are still tiles; the manifest is what lists it
		"/maps/dir-as-manifest/manifest.json",        // a directory named like the manifest
		"/maps/stray.txt",                            // not a world
		"/maps/chernarusplus/tiles/0/0/missing.webp", // a tile that is not there
	}
	if linked {
		refused = append(refused,
			"/maps/sakhal/tiles/leak",                          // a link out of the maps directory
			"/maps/sakhal/tiles/back/master.png",               // a link back to the world's intermediates
			"/maps/sakhal/tiles/back/manifest.json",            // the manifest through the tiles is not a tile
			"/maps/sakhal/tiles/maps/chernarusplus/master.png", // the whole maps directory through a link
			"/maps/sakhal/tiles/up/0/0/0.webp",                 // another world's tiles through a link
			"/maps/sakhal/master.png",                          // the linked world's own intermediate
		)
	}
	for _, path := range refused {
		recorder := get(path)
		if path == "/maps/half-built/tiles/0/0/0.webp" {
			// Served: a tile path is a tile path. The index is the only
			// place a manifest is required, and this world is not in it.
			if recorder.Code != http.StatusOK {
				t.Errorf("GET %s = %d, want 200", path, recorder.Code)
			}
			continue
		}
		if recorder.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d %q, want 404", path, recorder.Code, recorder.Body.String())
			continue
		}
		if !strings.Contains(recorder.Body.String(), `"not_found"`) {
			t.Errorf("GET %s body = %q, want the protocol's not_found shape", path, recorder.Body.String())
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
