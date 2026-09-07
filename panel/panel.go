// Package panel is the optional web UI of the reference hub: a handful of
// static files, embedded in the binary, that a browser turns into an operator
// console. It is a thin client over the Admin API and nothing more. The Go
// side of it is only a file server with the response headers a page that
// holds a bearer token deserves; every decision about servers, manifests, and
// actions is made by the JavaScript in static/ through /api/v1, with the
// same bearer tokens and the same scopes as curl.
//
// The hub does not import this package. It takes the handler through
// hub.Config.Panel, so the hub stays free of UI and an embedder may mount a
// different panel or none.
package panel

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed static
var static embed.FS

// Handler serves the panel rooted at "/": index.html at the root, the assets
// beside it. Mount it under a prefix with http.StripPrefix.
//
// Every response carries a Content-Security-Policy that confines the page to
// its own origin with no inline script or style, which is what makes the
// admin token the page holds unreachable from anything but the panel's own
// code: a manifest is plugin-supplied text, and the panel renders labels and
// codes from it, so the policy is the backstop should a rendering path ever
// treat one as markup. The page is also refused to framers, sends no
// referrer, and asks browsers to revalidate rather than cache, so an upgraded
// hub is never driven by a stale panel.
func Handler() http.Handler {
	root, err := fs.Sub(static, "static")
	if err != nil {
		// The directory is embedded at compile time; a missing subtree is a
		// build defect, not a runtime condition.
		panic("panel: embedded static directory missing: " + err.Error())
	}
	files := http.FileServerFS(root)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		header.Set("Content-Security-Policy", contentSecurityPolicy)
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("X-Frame-Options", "DENY")
		header.Set("Referrer-Policy", "no-referrer")
		header.Set("Cache-Control", "no-cache")
		// The panel is flat: its files live at the root and nowhere else.
		// Refusing every deeper path here, before FileServer sees it, is
		// what keeps FileServer's own behaviours off the wire: it would
		// list a directory, and it redirects any path ending in /index.html
		// to its directory before checking that either exists. A missing
		// file is refused in the hub's error shape rather than FileServer's
		// plain text, so every failure from the hub's port parses the same
		// way whichever surface answered it.
		if strings.Count(r.URL.Path, "/") > 1 || !shipped(root, r.URL.Path) {
			notFound(w, r)
			return
		}
		files.ServeHTTP(w, r)
	})
}

// shipped reports whether a request path names a file in the embedded tree;
// "/" is the index and always present.
func shipped(root fs.FS, path string) bool {
	if path == "/" || path == "/index.html" {
		return true
	}
	info, err := fs.Stat(root, strings.TrimPrefix(path, "/"))
	return err == nil && !info.IsDir()
}

// notFound answers in the protocol's error shape (spec section 2.2). The
// panel is not part of either API realm, but a client that reads the hub's
// errors should be able to read this one too.
func notFound(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"code": "not_found", "message": "the panel has no file at " + r.URL.Path},
	})
}

// contentSecurityPolicy is the policy every panel response carries. Scripts,
// styles, and fetches may reach this origin only; images may additionally be
// inline data URLs for the favicon; nothing may be embedded, framed, or used
// as a form action, because the panel submits nothing as a form.
const contentSecurityPolicy = "default-src 'none'; " +
	"script-src 'self'; " +
	"style-src 'self'; " +
	"connect-src 'self'; " +
	"img-src 'self' data:; " +
	"font-src 'self'; " +
	"base-uri 'none'; " +
	"form-action 'none'; " +
	"frame-ancestors 'none'"
