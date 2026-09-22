package panel_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
)

// The end-to-end test of issue #13: a real hub with the embedded panel, a
// fake plugin speaking the Plugin API, and a headless browser driving the
// page the way an operator would. It grades the two things a unit test of
// either side cannot: that the form a browser renders is the one the
// manifest's schema describes, and that a click on Dispatch round-trips
// through the hub to the plugin and back onto the page. Issue #47 added the
// event feed to the same run: a batch the plugin publishes is listed in the
// hub's order, filtered by type, followed as new batches land, and paged.
// Issue #46 added the live map: a tileset the test generates is installed
// on the hub, the plugin's snapshot is plotted on it where the world frame
// says, a later snapshot replaces the markers whole, and a click on one
// preselects the player in the action list and its form.
//
// The hub, the browser, the fake plugin, and the step helpers are shared with
// the management test and live in e2e_harness_test.go, along with the
// VYSHKA_E2E contract for a machine with no browser.

// e2eManifest exercises one field of each kind the form builder renders:
// an integer with an exclusive bound (shifted onto the input), a real number
// with an exclusive bound (unrepresentable in HTML, left to the hub, which is
// how the params_invalid path gets exercised), an enum with a default, a
// boolean with a default, a vector-hinted array, and an itemlist-hinted
// string.
var e2eManifest = map[string]any{
	"game":             "e2e",
	"plugin":           map[string]any{"name": "e2e-plugin", "version": "0.1.0"},
	"manifestRevision": 1,
	// One declared custom event (section 6.3), so the feed has a display
	// name to show beside its type; the other custom types the test emits
	// stay undeclared, as the protocol allows.
	"events": []map[string]any{{
		"id": "example-mod.raid.started", "name": "Raid started", "namespace": "example-mod",
	}},
	// Two custom contexts (section 6.2): one an annotated param draws on, one
	// an action is in, so both the param datalist and the target datalist are
	// exercised against the fake plugin's enumeration.
	"contexts": []map[string]any{
		{"id": "example-mod.item", "name": "Item", "namespace": "example-mod"},
		{"id": "example-mod.landmark", "name": "Landmark", "namespace": "example-mod"},
	},
	"actions": []map[string]any{{
		"code": "example-mod.heal", "name": "Heal player", "context": "player",
		"namespace": "example-mod", "danger": "warning",
		"params": map[string]any{
			"type":     "object",
			"required": []string{"amount"},
			"properties": map[string]any{
				"amount":       map[string]any{"type": "integer", "minimum": 1, "exclusiveMaximum": 100},
				"blood":        map[string]any{"type": "number", "exclusiveMaximum": 5000},
				"reason":       map[string]any{"type": "string", "enum": []string{"admin", "event", "test"}, "default": "event"},
				"restoreBlood": map[string]any{"type": "boolean", "default": true},
				"position":     map[string]any{"type": "array", "items": map[string]any{"type": "number"}, "x-vyshka-widget": "vector"},
				"item":         map[string]any{"type": "string", "x-vyshka-widget": "itemlist", "context": "example-mod.item"},
				// A context annotation beside a player widget: the annotation
				// names the data, so its entries are what the field suggests.
				"owner": map[string]any{"type": "string", "x-vyshka-widget": "player", "context": "example-mod.item"},
				// Array items annotated with a context the form does not
				// otherwise draw on: fetched once, offered through a picker
				// beside the one-per-line textarea.
				"parts": map[string]any{"type": "array", "items": map[string]any{"type": "string", "context": "example-mod.landmark"}},
				// A server-side exclusion (section 6.1, not in its one form):
				// the enumerated entry it names is marked blocked in the list
				// and refused at the field, and an excluded enum member is a
				// disabled option.
				"gift":  map[string]any{"type": "string", "context": "example-mod.item", "not": map[string]any{"enum": []string{"Apple"}}},
				"grade": map[string]any{"type": "string", "enum": []string{"a", "b", "c"}, "not": map[string]any{"enum": []string{"c"}}},
				// Fractional bounds on an integer round inward onto the input.
				"ticks": map[string]any{"type": "integer", "exclusiveMinimum": 0.5, "maximum": 9.9},
				// An optional object with a required child: untouched, it is
				// omitted whole and must not block the dispatch.
				"nested": map[string]any{
					"type": "object", "required": []string{"key"},
					"properties": map[string]any{
						"key":   map[string]any{"type": "string"},
						"count": map[string]any{"type": "integer", "minimum": 1},
					},
				},
				// An optional object that does get filled in: once anything
				// inside is entered it is present, its required empty array
				// is sent as [], and invalid input inside it is a fault, not
				// something the omission rule may swallow.
				"extra": map[string]any{
					"type": "object", "required": []string{"key", "items"},
					"properties": map[string]any{
						"key":     map[string]any{"type": "string"},
						"items":   map[string]any{"type": "array", "items": map[string]any{"type": "integer"}},
						"payload": map[string]any{"type": "object"},
					},
				},
			},
		},
	}, {
		// A vehicle-context action: its target field is fed by the vehicles
		// snapshot and preselected from the map, like the player one.
		"code": "example-mod.unstuck", "name": "Unstuck vehicle", "context": "vehicle",
		"namespace": "example-mod", "danger": "warning",
		"params": map[string]any{
			"type":       "object",
			"properties": map[string]any{"lift": map[string]any{"type": "number", "minimum": 0, "default": 1}},
		},
	}, {
		// A custom-context action: its target field is fed by the context's
		// enumeration (section 6.2), the way the player one is fed by the
		// snapshot.
		"code": "example-mod.beacon", "name": "Place a beacon", "context": "example-mod.landmark",
		"namespace": "example-mod", "danger": "none",
		"params": map[string]any{"type": "object", "properties": map[string]any{}},
	}},
}

var e2ePlayers = map[string]any{
	"players": []map[string]any{{
		"player": map[string]any{"platform": "steam", "id": "76561198000000001"},
		"name":   "Alice",
	}},
}

// The synthetic world of the map section: a 1024 m square at one metre per
// raster pixel, tiled at 256 px over three zoom levels, painted in sixteen
// flat colours by raster quadrant so that a canvas pixel under a marker
// says which part of the world was drawn there. Positions are DayZ-shaped,
// [x, y, z] with y the elevation and z the northing, which the manifest's
// default axes read.
const (
	e2eWorld       = "e2e-world"
	e2eWorldSize   = 1024
	e2eTileSize    = 256
	e2eNativeZoom  = 2
	e2eQuadrantPx  = 256
	e2eColourStep  = 45
	e2eColourFloor = 60
)

// e2eQuadrantColour is the colour painted over raster quadrant (qx, qy).
func e2eQuadrantColour(qx, qy int) color.NRGBA {
	return color.NRGBA{R: uint8(e2eColourFloor + e2eColourStep*qx), G: uint8(e2eColourFloor + e2eColourStep*qy), B: 100, A: 255}
}

// writeE2ETileset writes the world's manifest and tile pyramid under dir.
func writeE2ETileset(t *testing.T, dir string) {
	t.Helper()
	world := filepath.Join(dir, e2eWorld)
	manifest := map[string]any{
		"schemaVersion": 1,
		"world":         e2eWorld,
		"name":          "E2E world",
		"bounds":        map[string]any{"xmin": 0, "xmax": e2eWorldSize, "zmin": 0, "zmax": e2eWorldSize},
		"raster":        map[string]any{"width": e2eWorldSize, "height": e2eWorldSize, "metresPerPixel": 1},
		"tiles": map[string]any{
			"size": e2eTileSize, "minZoom": 0, "maxZoom": e2eNativeZoom,
			"urlTemplate": "tiles/{z}/{x}/{y}.png", "format": "png", "scheme": "xyz",
		},
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(world, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(world, "manifest.json"), encoded, 0o644); err != nil {
		t.Fatal(err)
	}
	for zoom := 0; zoom <= e2eNativeZoom; zoom++ {
		levelSize := e2eWorldSize >> (e2eNativeZoom - zoom)
		rasterPerLevelPx := 1 << (e2eNativeZoom - zoom)
		tiles := (levelSize + e2eTileSize - 1) / e2eTileSize
		for x := 0; x < tiles; x++ {
			for y := 0; y < tiles; y++ {
				img := image.NewNRGBA(image.Rect(0, 0, e2eTileSize, e2eTileSize))
				for i := 0; i < e2eTileSize; i++ {
					for j := 0; j < e2eTileSize; j++ {
						u := (x*e2eTileSize + i) * rasterPerLevelPx
						v := (y*e2eTileSize + j) * rasterPerLevelPx
						img.SetNRGBA(i, j, e2eQuadrantColour(u/e2eQuadrantPx, v/e2eQuadrantPx))
					}
				}
				path := filepath.Join(world, "tiles", strconv.Itoa(zoom), strconv.Itoa(x), strconv.Itoa(y)+".png")
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				var buf bytes.Buffer
				if err := png.Encode(&buf, img); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}

func TestPanelEndToEnd(t *testing.T) {
	mapsDir := t.TempDir()
	writeE2ETileset(t, mapsDir)
	// The hub, the browser, the console capture, and the step helpers are
	// e2e_harness_test.go's; the steps below are this test's.
	h := newE2EHarness(t, mapsDir)
	web, ctx := h.web, h.Ctx
	run, setValue, waitJS := h.run, h.setValue, h.waitJS
	text, attribute, evalString := h.text, h.attribute, h.evalString

	// A server record and a plugin attached to it, before the browser looks.
	created := createServer(t, web.URL, "E2E server")
	plugin := newFakePlugin(t, web.URL, created.Enrollment.Token)
	plugin.queue("manifest.publish", e2eManifest)
	plugin.queue("state.players", e2ePlayers)
	// Event timestamps are minutes from a base ten minutes ago: in the past
	// so the hub keeps them as sent (it substitutes receipt time only for a
	// ts ahead of its clock), and distinct so the order the feed shows is
	// the order occurredAt dictates. The batch lists them out of order on
	// purpose: the feed's order must come from the hub, not from the wire.
	feedBase := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)
	stamp := func(minutes int) string {
		return feedBase.Add(time.Duration(minutes) * time.Minute).Format("2006-01-02T15:04:05.000Z")
	}
	alice := map[string]any{"platform": "steam", "id": "76561198000000001"}
	bob := map[string]any{"platform": "steam", "id": "76561198000000002"}
	plugin.queue("event.batch", map[string]any{"events": []map[string]any{
		{"t": "core.player.death", "ts": stamp(2), "data": map[string]any{
			"player": alice, "name": "Alice", "cause": "player", "killer": bob, "killerName": "Bob",
			"weapon": "M4A1", "distance": 312.5, "position": []float64{4501.2, 320.1, 9800.4},
		}},
		{"t": "core.player.connect", "ts": stamp(1), "data": map[string]any{"player": alice, "name": "Alice"}},
		// A custom event whose data carries markup-shaped text: it must be
		// shown as the text it is.
		{"t": "example-mod.raid.started", "ts": stamp(3), "data": map[string]any{
			"territoryId": "t-19", "attackers": 4, "note": "<b>not markup</b>",
		}},
	}})
	go plugin.run(ctx)
	if err := plugin.awaitAcked(ctx); err != nil {
		t.Fatalf("plugin never got its manifest acked: %v", err)
	}

	// 1. / lands on the panel, which asks for a token.
	var location string
	run("open the panel", chromedp.Navigate(web.URL+"/"), chromedp.WaitVisible("#login", chromedp.ByQuery),
		chromedp.Location(&location))
	if !strings.HasSuffix(location, "/panel/") {
		t.Fatalf("landed on %s, want /panel/", location)
	}

	// 2. A bad token is refused on the page, with the hub's code.
	run("sign in with a bad token", chromedp.SendKeys("#token", "vya_not_a_token", chromedp.ByQuery),
		chromedp.Click("#sign-in", chromedp.ByQuery), chromedp.WaitVisible("#login-error", chromedp.ByQuery))
	if got := text("#login-error"); !strings.Contains(got, "unauthorized") {
		t.Fatalf("login error = %q, want the hub's unauthorized code", got)
	}

	// 3. The right token shows the server list with the enrolled plugin.
	run("sign in", setValue("#token", ""),
		chromedp.SendKeys("#token", e2eAdminToken, chromedp.ByQuery),
		chromedp.Click("#sign-in", chromedp.ByQuery), chromedp.WaitVisible("#servers", chromedp.ByQuery))
	row := `tr[data-server-id="` + created.Server.ID + `"]`
	if got := text(row); !strings.Contains(got, "E2E server") || !strings.Contains(got, "e2e-plugin") {
		t.Fatalf("server row = %q, want the name and the plugin", got)
	}

	// 4. The server page lists the manifest's actions.
	run("open the server", chromedp.Click(row+" a", chromedp.ByQuery),
		chromedp.WaitVisible(`a[data-action-code="example-mod.heal"]`, chromedp.ByQuery))
	if got := text(`a[data-action-code="example-mod.heal"]`); !strings.Contains(got, "Heal player") || !strings.Contains(got, "warning") {
		t.Fatalf("action item = %q, want its label and danger", got)
	}

	// 5. The form is the schema, rendered.
	run("open the action", chromedp.Click(`a[data-action-code="example-mod.heal"]`, chromedp.ByQuery),
		chromedp.WaitVisible("#action-form", chromedp.ByQuery))
	amount := `input[name="params.amount"]`
	if got := attribute(amount, "min"); got != "1" {
		t.Errorf("amount min = %q, want 1", got)
	}
	if got := attribute(amount, "max"); got != "99" {
		t.Errorf("amount max = %q, want 99 (exclusiveMaximum 100 shifted for an integer)", got)
	}
	if got := attribute(amount, "step"); got != "1" {
		t.Errorf("amount step = %q, want 1", got)
	}
	if evalString(`document.querySelector('input[name="params.amount"]').required ? "yes" : "no"`) != "yes" {
		t.Errorf("amount is not marked required")
	}
	if got := evalString(`document.querySelector('input[name="params.blood"]').getAttribute("max") || "none"`); got != "none" {
		t.Errorf("blood max = %q, want none: a real number's exclusive bound cannot be an HTML max", got)
	}
	if got := evalString(`document.querySelector('select[name="params.reason"]').selectedOptions[0].textContent`); got != "event" {
		t.Errorf("reason default = %q, want event", got)
	}
	if got := evalString(`Array.from(document.querySelectorAll('select[name="params.reason"] option')).map(o => o.textContent).join(",")`); got != "(not set),admin,event,test" {
		t.Errorf("reason options = %q", got)
	}
	if evalString(`document.querySelector('input[name="params.restoreBlood"]').checked ? "yes" : "no"`) != "yes" {
		t.Errorf("restoreBlood default true is not checked")
	}
	for _, axis := range []string{"x", "y", "z"} {
		if got := attribute(`input[name="params.position.`+axis+`"]`, "type"); got != "number" {
			t.Errorf("position.%s type = %q, want number (vector widget)", axis, got)
		}
	}
	if got := attribute(`input[name="params.item"]`, "placeholder"); got != "item class name" {
		t.Errorf("item placeholder = %q, want the itemlist hint", got)
	}
	// The item param is annotated with a custom context (section 6.1): the
	// hub asked the plugin for its entries (section 6.2) and the field
	// suggests them, the referenceKey as the value and the label as the text.
	if got := evalString(`(function(){const i=document.querySelector('input[name="params.item"]');const l=document.getElementById(i.getAttribute("list"));return l?Array.from(l.options).map(o=>o.value+"="+o.textContent).join(","):"no datalist"})()`); got != "AKM=AKM,Apple=Apple" {
		t.Errorf("item datalist = %q, want the context's entries", got)
	}
	if got := plugin.enumerated(); got != 2 {
		t.Errorf("the plugin answered %d context.enumerate questions for one form, want 2: one per context the form draws on (the item and owner fields share one, the array items name the other)", got)
	}
	if got := evalString(`(function(){const i=document.querySelector('input[name="params.owner"]');const l=document.getElementById(i.getAttribute("list"));return l?Array.from(l.options).map(o=>o.value).join(","):"no datalist"})()`); got != "AKM,Apple" {
		t.Errorf("owner datalist = %q, want the context's entries over the player widget's", got)
	}
	// The array's picker suggests the item context's entries and appends
	// the chosen one as a line of the textarea.
	if got := evalString(`(function(){const p=document.querySelector('input[data-picker-for="params.parts"]');const l=document.getElementById(p.getAttribute("list"));return l?Array.from(l.options).map(o=>o.value+"="+o.textContent).join(","):"no datalist"})()`); got != "green-mountain=Green Mountain" {
		t.Errorf("parts picker datalist = %q, want the landmark entries", got)
	}
	run("pick a part", chromedp.SendKeys(`input[data-picker-for="params.parts"]`, "green-mountain\t", chromedp.ByQuery))
	if got := evalString(`document.querySelector('textarea[name="params.parts"]').value`); got != "green-mountain" {
		t.Errorf("parts textarea = %q after the pick, want the picked value as its line", got)
	}
	if got := evalString(`document.querySelector('textarea[name="params.parts"]').closest("label").querySelector(".hint").textContent`); !strings.Contains(got, "Landmark (1 entry)") {
		t.Errorf("parts hint = %q, want the landmark context named with its count", got)
	}
	run("clear the part", setValue(`textarea[name="params.parts"]`, ""))
	// The excluded entry stays in the suggestions, marked, and naming it is
	// an error on the field as it is typed and again when the form is read.
	if got := evalString(`(function(){const i=document.querySelector('input[name="params.gift"]');const l=document.getElementById(i.getAttribute("list"));return l?Array.from(l.options).map(o=>o.value+"="+o.textContent+(o.dataset.excluded?"!":"")).join(","):"no datalist"})()`); got != "AKM=AKM,Apple=Apple (blocked on this server)!" {
		t.Errorf("gift datalist = %q, want Apple marked as blocked", got)
	}
	if got := evalString(`document.querySelector('input[name="params.gift"]').closest("label").querySelector(".hint").textContent`); !strings.Contains(got, "1 value is blocked on this server") {
		t.Errorf("gift hint = %q, want the blocked count", got)
	}
	run("type an excluded gift", chromedp.SendKeys(`input[name="params.gift"]`, "Apple", chromedp.ByQuery))
	waitJS("excluded value named on the field as typed",
		`(function(){const e=document.querySelector('label[data-path="gift"] .field-error');return e && !e.hidden && e.textContent.includes("excluded")})()`)
	run("dispatch with the excluded gift", chromedp.Click("#dispatch", chromedp.ByQuery))
	waitJS("excluded value refused when the form is read",
		`(function(){const e=document.querySelector('label[data-path="gift"] .field-error');return e && !e.hidden && e.textContent.startsWith("gift is excluded")})()`)
	run("clear the gift", setValue(`input[name="params.gift"]`, ""))
	if got := evalString(`Array.from(document.querySelectorAll('select[name="params.grade"] option')).map(o => o.textContent+(o.disabled?"!":"")).join(",")`); got != "(not set),a,b,c (excluded)!" {
		t.Errorf("grade options = %q, want c disabled and marked", got)
	}
	if got := attribute(`input[name="params.ticks"]`, "min") + ".." + attribute(`input[name="params.ticks"]`, "max"); got != "1..9" {
		t.Errorf("ticks bounds = %q, want 1..9 (fractional bounds rounded inward for an integer)", got)
	}
	if evalString(`document.querySelector('input[name="params.nested.key"]').required ? "yes" : "no"`) != "no" {
		t.Errorf("a required child of an optional object must not carry the HTML required flag")
	}
	// The player context becomes a target field fed by the players snapshot.
	if got := evalString(`(function(){const i=document.querySelector('input[name="referenceKey"]');const l=document.getElementById(i.getAttribute("list"));return Array.from(l.options).map(o=>o.value+"="+o.textContent).join(",")})()`); got != "76561198000000001=Alice (steam)" {
		t.Errorf("player datalist = %q", got)
	}
	if evalString(`document.querySelector('input[name="referenceKey"]').required ? "yes" : "no"`) != "yes" {
		t.Errorf("player target is not marked required")
	}

	// 5a. A custom-context action's target is fed by that context's
	// enumeration, the way the player target is fed by the snapshot; the
	// heal form is then reopened for the steps below, from the hub's cache
	// (the plugin is not asked again within the cache bound).
	run("open the beacon action", chromedp.Navigate(web.URL+"/panel/#/servers/"+created.Server.ID+"/actions/example-mod.beacon"),
		chromedp.WaitVisible("#action-form", chromedp.ByQuery))
	if got := evalString(`(function(){const i=document.querySelector('input[name="referenceKey"]');const l=document.getElementById(i.getAttribute("list"));return l?Array.from(l.options).map(o=>o.value+"="+o.textContent).join(","):"no datalist"})()`); got != "green-mountain=Green Mountain" {
		t.Errorf("landmark datalist = %q, want the context's entries", got)
	}
	if evalString(`document.querySelector('input[name="referenceKey"]').required ? "yes" : "no"`) != "no" {
		t.Errorf("a custom-context target is marked required; the protocol leaves the reference optional")
	}
	if got := plugin.enumerated(); got != 2 {
		t.Errorf("the plugin answered %d context.enumerate questions after two forms, want 2: the beacon form's landmark context was read within the cache bound", got)
	}
	run("reopen the heal action", chromedp.Navigate(web.URL+"/panel/#/servers/"+created.Server.ID+"/actions/example-mod.heal"),
		chromedp.WaitVisible("#action-form", chromedp.ByQuery))
	if got := evalString(`(function(){const i=document.querySelector('input[name="params.item"]');const l=document.getElementById(i.getAttribute("list"));return l?String(l.options.length):"no datalist"})()`); got != "2" {
		t.Errorf("item datalist on reopen has %s options, want 2", got)
	}
	if got := plugin.enumerated(); got != 2 {
		t.Errorf("the plugin answered %d questions after the reopen, want the cached 2 (section 6.2: a hub caches briefly)", got)
	}

	// 6. The danger confirmation is the form's own check now that native
	// validation is off: an unticked box is reported, nothing is sent.
	run("dispatch without confirming",
		chromedp.SendKeys(`input[name="referenceKey"]`, "76561198000000001", chromedp.ByQuery),
		chromedp.SendKeys(amount, "50", chromedp.ByQuery),
		chromedp.Click("#dispatch", chromedp.ByQuery))
	waitJS("confirmation fault shown", `(function(){const e=document.querySelector("#form-errors");return e && !e.hidden && e.textContent.includes("confirmation")})()`)

	// 6a. A half-filled vector is refused on the page, coordinate by
	// coordinate, before anything is sent: a blank axis is never a zero.
	run("fill in a vector with a blank y",
		chromedp.SendKeys(`input[name="params.position.x"]`, "1.5", chromedp.ByQuery),
		chromedp.SendKeys(`input[name="params.position.z"]`, "3", chromedp.ByQuery),
		chromedp.Click("#confirm-danger", chromedp.ByQuery),
		chromedp.Click("#dispatch", chromedp.ByQuery))
	waitJS("blank coordinate fault shown on the vector",
		`(function(){const e=document.querySelector('label[data-path="position"] .field-error');return e && !e.hidden && e.textContent.includes("coordinate y")})()`)

	// 6b. Invalid input inside an otherwise empty optional object is a fault
	// on its field, never dropped by omitting the object.
	run("type broken JSON into the optional object",
		chromedp.SendKeys(`input[name="params.position.y"]`, "2", chromedp.ByQuery),
		chromedp.SendKeys(`textarea[name="params.extra.payload"]`, "{", chromedp.ByQuery),
		chromedp.Click("#dispatch", chromedp.ByQuery))
	waitJS("invalid JSON fault shown inside the optional object",
		`(function(){const e=document.querySelector('label[data-path="extra.payload"] .field-error');return e && !e.hidden && e.textContent.includes("JSON")})()`)
	if plugin.dispatches() != 0 {
		t.Fatalf("the plugin received %d dispatches; a form with a fault must not submit", plugin.dispatches())
	}

	// 6c. Including an optional object by hand enforces its children even
	// with nothing typed inside: the untouched nested.key is now a fault.
	// Typing into extra ticked its box on its own; nested's is unticked.
	nestedInclude := `input[name="params.nested.__include"]`
	if evalString(`document.querySelector('input[name="params.extra.__include"]').checked ? "yes" : "no"`) != "yes" {
		t.Fatalf("typing inside the optional object extra did not include it")
	}
	run("include the untouched optional object by hand",
		setValue(`textarea[name="params.extra.payload"]`, ""),
		chromedp.Click(nestedInclude, chromedp.ByQuery),
		chromedp.Click("#dispatch", chromedp.ByQuery))
	waitJS("required fault shown inside the hand-included object",
		`(function(){const e=document.querySelector('label[data-path="nested.key"] .field-error');return e && !e.hidden && e.textContent.includes("required")})()`)

	// 6d. A value the browser cannot refuse is refused by the hub, and the
	// fault lands on its field. The optional object extra has its key
	// entered and its required array left empty, which must travel as [].
	// nested gets a count below its minimum typed in and is then unticked:
	// excluded, so neither the browser's own constraint check nor the form
	// may hold the dispatch over a value that is not being sent.
	run("fill in an over-bound blood value",
		chromedp.SendKeys(`input[name="params.nested.count"]`, "0", chromedp.ByQuery),
		chromedp.Click(nestedInclude, chromedp.ByQuery),
		chromedp.SendKeys(`input[name="params.extra.key"]`, "yes", chromedp.ByQuery),
		chromedp.SendKeys(`input[name="params.blood"]`, "5000", chromedp.ByQuery),
		chromedp.Click("#dispatch", chromedp.ByQuery))
	waitJS("params_invalid fault shown on the blood field",
		`(function(){const e=document.querySelector('label[data-path="blood"] .field-error');return e && !e.hidden && e.textContent.includes("blood")})()`)
	if got := text("#form-errors"); !strings.Contains(got, "params_invalid") {
		t.Fatalf("form errors = %q, want the params_invalid heading", got)
	}
	if plugin.dispatches() != 0 {
		t.Fatalf("the plugin received %d dispatches; schema-invalid input must never reach it", plugin.dispatches())
	}

	// 7. Corrected, the dispatch round-trips: queued on the page, executed
	// by the plugin, completed with its result on the page.
	run("correct the value and dispatch",
		setValue(`input[name="params.blood"]`, ""),
		chromedp.SendKeys(`input[name="params.blood"]`, "4999.5", chromedp.ByQuery),
		chromedp.Click("#dispatch", chromedp.ByQuery),
		chromedp.WaitVisible("#result-card", chromedp.ByQuery))
	waitJS("action completed on the page",
		`document.querySelector("#result-state") && document.querySelector("#result-state").textContent === "completed"`)
	if got := text("#result-payload"); !strings.Contains(got, `"healedTo": 50`) {
		t.Fatalf("result payload = %q, want the plugin's healedTo", got)
	}
	if got := text("#timeline li.reached"); got != "queued" {
		t.Fatalf("first timeline step = %q", got)
	}
	if got := evalString(`Array.from(document.querySelectorAll("#timeline li.reached")).map(l => l.textContent).join(",")`); got != "queued,delivered,running,completed" {
		t.Fatalf("timeline = %q, want every state reached", got)
	}
	// VYSHKA_E2E_SCREENSHOT names a PNG to write of the completed dispatch,
	// for demos and issue evidence; the test itself never needs it.
	if path := os.Getenv("VYSHKA_E2E_SCREENSHOT"); path != "" {
		var png []byte
		run("screenshot", chromedp.FullScreenshot(&png, 90))
		if err := os.WriteFile(path, png, 0o644); err != nil {
			t.Fatalf("write screenshot: %v", err)
		}
	}

	// The completed round trip is the barrier that makes the earlier
	// refusal's "never reached the plugin" claim sound: the hub delivers in
	// order, so had it queued the refused dispatch after all, the plugin
	// would have executed it before this one, and the count would be two.
	if plugin.dispatches() != 1 {
		t.Fatalf("the plugin received %d dispatches in total, want exactly the corrected one", plugin.dispatches())
	}

	// What the plugin executed is what the form built: the enum default,
	// the boolean default, the vector, and the corrected number; the empty
	// itemlist string omitted rather than sent as "".
	got := plugin.lastDispatch()
	want := map[string]any{
		"amount": float64(50), "blood": 4999.5, "reason": "event", "restoreBlood": true,
		"position": []any{1.5, float64(2), float64(3)},
		"extra":    map[string]any{"key": "yes", "items": []any{}},
	}
	if got.ReferenceKey != "76561198000000001" || got.Context != "player" {
		t.Fatalf("dispatch target = %s/%s", got.Context, got.ReferenceKey)
	}
	var params map[string]any
	if err := json.Unmarshal(got.Params, &params); err != nil {
		t.Fatalf("decode params: %v", err)
	}
	if fmt.Sprint(params) != fmt.Sprint(want) {
		t.Fatalf("plugin received params %v, want %v", params, want)
	}

	// 8. The event feed (issue #47). The batch the plugin published before
	// the browser opened is listed newest first by occurredAt, whatever
	// order the batch carried them in.
	rowTypes := `Array.from(document.querySelectorAll("#events tbody tr")).map(r => r.dataset.eventType).join(",")`
	run("open the server page", chromedp.Evaluate(`location.hash = `+strconv.Quote("#/servers/"+created.Server.ID), nil),
		chromedp.WaitVisible("#events-link", chromedp.ByQuery))
	run("open the event feed", chromedp.Click("#events-link", chromedp.ByQuery),
		chromedp.WaitVisible("#events", chromedp.ByQuery))
	waitJS("feed lists the batch newest first", rowTypes+` === "example-mod.raid.started,core.player.death,core.player.connect"`)
	deathRow := `#events tr[data-event-type="core.player.death"]`
	if got := text(deathRow); !strings.Contains(got, "M4A1") || !strings.Contains(got, "steam:76561198000000002") || !strings.Contains(got, "Alice") {
		t.Fatalf("death row = %q, want the weapon, the killer identity, and the victim's name", got)
	}
	// The payload is serialized when its <details> is first opened, and is
	// read through the DOM rather than as visible text.
	if got := evalString(`String(document.querySelector(` + strconv.Quote(deathRow+" details pre") + `).textContent.length)`); got != "0" {
		t.Fatalf("death payload was serialized before its disclosure was opened (%s characters)", got)
	}
	run("open the death payload", chromedp.Evaluate(`document.querySelector(`+strconv.Quote(deathRow+" details")+`).open = true`, nil))
	waitJS("death payload rendered", `document.querySelector(`+strconv.Quote(deathRow+" details pre")+`).textContent.length > 0`)
	if got := evalString(`document.querySelector(` + strconv.Quote(deathRow+" details pre") + `).textContent`); !strings.Contains(got, `"weapon": "M4A1"`) {
		t.Fatalf("death row payload = %q, want the full data as JSON", got)
	}
	raidRow := `#events tr[data-event-type="example-mod.raid.started"]`
	if got := text(raidRow); !strings.Contains(got, "Raid started") || !strings.Contains(got, "<b>not markup</b>") {
		t.Fatalf("raid row = %q, want the declared name and the note as text", got)
	}
	if evalString(`document.querySelector("#events b") ? "element" : "none"`) != "none" {
		t.Fatalf("event data was rendered as markup")
	}
	if evalString(`document.querySelector(`+strconv.Quote(raidRow+" .badge")+`).textContent + "/" + document.querySelector(`+strconv.Quote(deathRow+" .badge")+`).textContent`) != "custom/core" {
		t.Fatalf("event kind badges are wrong")
	}
	if got := text("#event-status"); !strings.Contains(got, "3 events shown") || !strings.Contains(got, "following") {
		t.Fatalf("feed status = %q", got)
	}

	// 8a. The type filter is a hub-side query and part of the route. A term
	// the hub refuses is shown with its code: an explicit empty term in a
	// shared link travels as it is rather than quietly widening the feed.
	run("open a link with an empty type term",
		chromedp.Evaluate(`location.hash = `+strconv.Quote("#/servers/"+created.Server.ID+"/events?type="), nil),
		chromedp.WaitVisible("#event-error", chromedp.ByQuery))
	if got := text("#event-error"); !strings.Contains(got, "bad_request") {
		t.Fatalf("empty type term: error = %q, want the hub's bad_request", got)
	}
	run("filter to core player events", setValue("#event-filter", "core.player.*"),
		chromedp.Click("#event-filter-apply", chromedp.ByQuery))
	waitJS("filtered feed shows only core player events", rowTypes+` === "core.player.death,core.player.connect"`)
	if got := evalString(`location.hash`); !strings.Contains(got, "type=core.player.*") {
		t.Fatalf("filter is not in the route: %q", got)
	}

	// 8b. Follow mode: a batch published now appears without a click, in
	// the hub's order, through the same filter. The damage event is older
	// than everything shown, which a since= query keyed on the newest
	// event would never see; the raid.ended event is filtered out.
	// The deep event is 2500 nested objects in 15 KB, inside the hub's
	// 16 KiB data cap: a payload the feed must show bounded rather than
	// indent into megabytes or overflow the stack on.
	deep := strings.Repeat(`{"a":`, 2500) + "1" + strings.Repeat("}", 2500)
	plugin.queue("event.batch", map[string]any{"events": []map[string]any{
		{"t": "core.player.disconnect", "ts": stamp(4), "data": map[string]any{"player": alice, "name": "Alice"}},
		{"t": "core.player.damage", "ts": stamp(0), "data": map[string]any{"player": alice, "amount": 12}},
		{"t": "example-mod.raid.ended", "ts": stamp(5), "data": map[string]any{"territoryId": "t-19"}},
		{"t": "example-mod.deep", "ts": stamp(7), "data": json.RawMessage(deep)},
	}})
	waitJS("follow mode merged the new events in order", rowTypes+` === "core.player.disconnect,core.player.death,core.player.connect,core.player.damage"`)
	if got := evalString(`String(document.querySelectorAll("#events tr.new").length)`); got != "2" {
		t.Fatalf("%s rows marked new, want 2", got)
	}

	// 8c. Not following: an event the hub has stored stays off the page
	// until the operator asks again. The plugin's buffer draining is the
	// proof the hub holds it; the wait is longer than one follow tick.
	run("stop following", chromedp.Click("#event-follow", chromedp.ByQuery))
	waitJS("route says follow=off", `location.hash.includes("follow=off") && document.querySelector("#event-status").textContent.includes("not following")`)
	plugin.queue("event.batch", map[string]any{"events": []map[string]any{
		{"t": "core.player.kick", "ts": stamp(6), "data": map[string]any{"player": alice, "reason": "afk"}},
	}})
	if err := plugin.awaitDrained(ctx); err != nil {
		t.Fatalf("plugin never got its kick event acked: %v", err)
	}
	time.Sleep(6 * time.Second)
	if got := evalString(rowTypes); got != "core.player.disconnect,core.player.death,core.player.connect,core.player.damage" {
		t.Fatalf("feed moved while not following: %q", got)
	}
	run("follow again", chromedp.Click("#event-follow", chromedp.ByQuery))
	waitJS("the kick is on the page once following resumes", rowTypes+` === "core.player.kick,core.player.disconnect,core.player.death,core.player.connect,core.player.damage"`)

	// 8d. The deep payload: its row is there, the feed is intact, and the
	// disclosure shows it bounded (compact JSON, not an indentation that
	// grows with the square of the depth).
	run("clear the filter", setValue("#event-filter", ""), chromedp.Click("#event-filter-apply", chromedp.ByQuery))
	waitJS("unfiltered feed shows every event", rowTypes+` === "example-mod.deep,core.player.kick,example-mod.raid.ended,core.player.disconnect,example-mod.raid.started,core.player.death,core.player.connect,core.player.damage"`)
	if got := evalString(`document.querySelector("#event-error").hidden ? "hidden" : "shown"`); got != "hidden" {
		t.Fatalf("the feed reports an error with the deep payload on the page: %s", text("#event-error"))
	}
	deepRow := `#events tr[data-event-type="example-mod.deep"]`
	run("open the deep payload", chromedp.Evaluate(`document.querySelector(`+strconv.Quote(deepRow+" details")+`).open = true`, nil))
	waitJS("deep payload rendered", `document.querySelector(`+strconv.Quote(deepRow+" details pre")+`).textContent.length > 0`)
	deepLength, err := strconv.Atoi(evalString(`String(document.querySelector(` + strconv.Quote(deepRow+" details pre") + `).textContent.length)`))
	if err != nil || deepLength < len(deep) || deepLength > 2*len(deep) {
		t.Fatalf("deep payload rendered as %d characters (%v), want about the compact %d", deepLength, err, len(deep))
	}

	// 8e. Paging. With the filter cleared the feed holds eight events and
	// no cursor. A batch of 150 older custom events then arrives under
	// follow: the first page fills, the tick adopts the hub's cursor, and
	// Load older walks to the end with no event shown twice.
	if got := evalString(`document.querySelector("#events-older").hidden ? "hidden" : "shown"`); got != "hidden" {
		t.Fatalf("Load older is %s on a feed with no further page", got)
	}
	ticks := make([]map[string]any, 0, 150)
	for i := 0; i < 150; i++ {
		ticks = append(ticks, map[string]any{
			"t": "example-mod.tick", "ts": feedBase.Add(-time.Hour + time.Duration(i)*time.Second).Format("2006-01-02T15:04:05.000Z"),
			"data": map[string]any{"n": i},
		})
	}
	plugin.queue("event.batch", map[string]any{"events": ticks})
	waitJS("follow filled the first page and offered older events",
		`document.querySelectorAll("#events tbody tr").length === 100 && !document.querySelector("#events-older").hidden`)
	run("load older", chromedp.Click("#events-older", chromedp.ByQuery))
	waitJS("the whole feed is on the page", `document.querySelectorAll("#events tbody tr").length === 158 && document.querySelector("#events-older").hidden`)
	if got := evalString(`String(new Set(Array.from(document.querySelectorAll("#events tbody tr")).map(r => r.dataset.eventId)).size)`); got != "158" {
		t.Fatalf("%s distinct event ids among 158 rows", got)
	}
	if got := evalString(`Array.from(document.querySelectorAll("#events tbody tr")).slice(0, 9).map(r => r.dataset.eventType).join(",")`); got != "example-mod.deep,core.player.kick,example-mod.raid.ended,core.player.disconnect,example-mod.raid.started,core.player.death,core.player.connect,core.player.damage,example-mod.tick" {
		t.Fatalf("feed head after paging = %q", got)
	}
	if got := evalString(`Array.from(document.querySelectorAll("#events tbody tr")).slice(8).map(r => r.querySelector("summary").textContent).join(",")`); !strings.HasPrefix(got, "n: 149,n: 148,") || !strings.HasSuffix(got, ",n: 1,n: 0") {
		t.Fatalf("older page is out of order: %.60s ... %.20s", got, got[len(got)-20:])
	}

	// 8f. With the walk exhausted, a new event at the top is followed onto
	// the page, and the first page's cursor, which now points into history
	// the feed has already walked, must not bring Load older back.
	plugin.queue("event.batch", map[string]any{"events": []map[string]any{
		{"t": "example-mod.late", "ts": stamp(8), "data": map[string]any{"why": "the walk is done"}},
	}})
	waitJS("follow shows the late event at the top",
		`document.querySelectorAll("#events tbody tr").length === 159 && document.querySelector("#events tbody tr").dataset.eventType === "example-mod.late"`)
	if got := evalString(`document.querySelector("#events-older").hidden ? "hidden" : "shown"`); got != "hidden" {
		t.Fatalf("Load older is %s after the walk was exhausted and one new event arrived", got)
	}

	// 8g. The other way the hub can hold more than the feed has walked: a
	// feed of exactly one page, no cursor, and then one late event older
	// than everything on it. The page does not change, but it now carries
	// a cursor where it carried none, and that alone must offer the walk.
	// Under the core.player.* filter the feed holds five events; 95 older
	// chat events fill the page exactly.
	chats := make([]map[string]any, 0, 95)
	for i := 0; i < 95; i++ {
		chats = append(chats, map[string]any{
			"t": "core.player.chat", "ts": feedBase.Add(-2*time.Hour + time.Duration(i)*time.Second).Format("2006-01-02T15:04:05.000Z"),
			"data": map[string]any{"player": alice, "text": "hello " + strconv.Itoa(i)},
		})
	}
	plugin.queue("event.batch", map[string]any{"events": chats})
	if err := plugin.awaitDrained(ctx); err != nil {
		t.Fatalf("plugin never got its chat events acked: %v", err)
	}
	run("filter to core player events again", setValue("#event-filter", "core.player.*"),
		chromedp.Click("#event-filter-apply", chromedp.ByQuery))
	waitJS("the filtered feed is exactly one page with no cursor",
		`document.querySelectorAll("#events tbody tr").length === 100 && document.querySelector("#events-older").hidden && document.querySelector("#event-status").textContent.includes("following")`)
	plugin.queue("event.batch", map[string]any{"events": []map[string]any{
		{"t": "core.player.chat", "ts": feedBase.Add(-3 * time.Hour).Format("2006-01-02T15:04:05.000Z"), "data": map[string]any{"player": alice, "text": "late"}},
	}})
	waitJS("the unchanged page's new cursor offers the walk", `!document.querySelector("#events-older").hidden`)
	if got := evalString(`String(document.querySelectorAll("#events tbody tr").length)`); got != "100" {
		t.Fatalf("%s rows before loading older, want the unchanged page of 100", got)
	}
	run("load the late event", chromedp.Click("#events-older", chromedp.ByQuery))
	waitJS("the late event is at the bottom and the walk is done",
		`document.querySelectorAll("#events tbody tr").length === 101 && document.querySelector("#events-older").hidden && document.querySelector("#events tbody tr:last-child summary").textContent.includes("late")`)

	// 8h. The backlog case on a feed already past the boundary: 150 new
	// events land on top of the 101 walked. The first page is all new and
	// ends in an unseen event, which is the sign that more lie below it;
	// the walk from there reaches the end in two pages with every event
	// shown once.
	burst := make([]map[string]any, 0, 150)
	for i := 0; i < 150; i++ {
		burst = append(burst, map[string]any{
			"t": "core.player.chat", "ts": feedBase.Add(9*time.Minute + time.Duration(i)*time.Second).Format("2006-01-02T15:04:05.000Z"),
			"data": map[string]any{"player": alice, "text": "burst " + strconv.Itoa(i)},
		})
	}
	plugin.queue("event.batch", map[string]any{"events": burst})
	waitJS("the burst's first page is on top and the walk below it is offered",
		`document.querySelectorAll("#events tbody tr").length === 201 && !document.querySelector("#events-older").hidden && document.querySelector("#events tbody tr summary").textContent === "player: steam:76561198000000001, text: burst 149"`)
	run("load the rest of the burst", chromedp.Click("#events-older", chromedp.ByQuery))
	waitJS("the second page joins the burst to the walked history",
		`document.querySelectorAll("#events tbody tr").length === 251 && !document.querySelector("#events-older").hidden`)
	run("load the last page", chromedp.Click("#events-older", chromedp.ByQuery))
	waitJS("the walk reaches the end with nothing new", `document.querySelectorAll("#events tbody tr").length === 251 && document.querySelector("#events-older").hidden`)
	if got := evalString(`String(new Set(Array.from(document.querySelectorAll("#events tbody tr")).map(r => r.dataset.eventId)).size)`); got != "251" {
		t.Fatalf("%s distinct event ids among 251 rows", got)
	}

	// 9. The live map (issue #46). The plugin names its world in a start
	// event and publishes a snapshot with positions: Alice and Bob at the
	// centres of two raster quadrants, Dave with the flat two-number form,
	// Carol with no position at all. The page plots the three it can where
	// the world frame puts them, over the tiles the hub serves.
	carol := map[string]any{"platform": "steam", "id": "76561198000000003"}
	dave := map[string]any{"platform": "steam", "id": "76561198000000004"}
	// Two identities whose platform and id joined with a colon would read
	// the same: the map must keep them apart and drop both when they go.
	colon1 := map[string]any{"platform": "a:b", "id": "c"}
	colon2 := map[string]any{"platform": "a", "id": "b:c"}
	plugin.queue("event.batch", map[string]any{"events": []map[string]any{
		{"t": "core.server.start", "ts": stamp(9), "data": map[string]any{"game": "e2e", "world": e2eWorld}},
	}})
	plugin.queue("state.players", map[string]any{
		"capturedAt": time.Now().UTC().Add(-20 * time.Second).Format("2006-01-02T15:04:05Z"),
		"players": []map[string]any{
			{"player": alice, "name": "Alice", "position": []float64{384, 12.5, 640}, "data": map[string]any{"alive": true, "health": 82, "flags": map[string]any{"god": true, "freeze": true, "unlimitedAmmo": false}}},
			{"player": bob, "name": "Bob", "position": []float64{896, 3, 128}},
			{"player": carol, "name": "Carol"},
			{"player": dave, "name": "Dave", "position": []float64{128, 896}},
			{"player": colon1, "name": "Colon one", "position": []float64{600, 0, 200}},
			{"player": colon2, "name": "Colon two", "position": []float64{700, 0, 200}},
		},
	})
	// The vehicles snapshot (issue #67) rides beside the players: one car
	// at the centre of a raster quadrant, one with no position at all. Its
	// markers are drawn apart from the players' and its rows listed apart.
	plugin.queue("state.vehicles", map[string]any{
		"capturedAt": time.Now().UTC().Add(-15 * time.Second).Format("2006-01-02T15:04:05Z"),
		"vehicles": []map[string]any{
			{"id": "0-4242", "kind": "car", "position": []float64{256, 5, 768}, "data": map[string]any{"type": "OffroadHatchback", "displayName": "ADA 4x4", "seats": 4}},
			{"id": "0-4243", "kind": "boat", "data": map[string]any{"type": "Boat_01"}},
		},
	})
	marker := func(platform, id string) string {
		key, _ := json.Marshal([]string{platform, id})
		return `.map-marker[data-marker-key='` + string(key) + `']`
	}
	vehicleMarker := func(id string) string { return `.map-marker-vehicle[data-marker-key='vehicle:` + id + `']` }
	if err := plugin.awaitDrained(ctx); err != nil {
		t.Fatalf("plugin never got its map snapshot acked: %v", err)
	}
	run("open the live map", chromedp.Evaluate(`location.hash = `+strconv.Quote("#/servers/"+created.Server.ID+"/map"), nil),
		chromedp.WaitVisible("#map", chromedp.ByQuery), chromedp.WaitVisible("#players", chromedp.ByQuery))
	if got := attribute("#map", "data-world"); got != e2eWorld {
		t.Fatalf("map world = %q, want the one the start event reported", got)
	}
	if got := evalString(`document.querySelector("#map-world").value + "|" + document.querySelector("#map-world option").textContent`); got != "|as reported by the server ("+e2eWorld+")" {
		t.Fatalf("world selector = %q", got)
	}
	if got := text("#map-status"); !strings.Contains(got, "6 players") || !strings.Contains(got, "captured") || !strings.Contains(got, "1 without a position") ||
		!strings.Contains(got, "2 vehicles") {
		t.Fatalf("map status = %q", got)
	}
	vehicleRow := func(id string) string { return `#vehicles tr[data-vehicle-id="` + id + `"]` }
	if got := text(vehicleRow("0-4242")); !strings.Contains(got, "ADA 4x4") || !strings.Contains(got, "car") {
		t.Fatalf("the car's row = %q, want its display name and kind", got)
	}
	if got := text(vehicleRow("0-4242") + " td.position"); got != "x 256, z 768" {
		t.Fatalf("the car's position cell = %q", got)
	}
	if got := text(vehicleRow("0-4243") + " td.position"); got != "no position" {
		t.Fatalf("the boat's position cell = %q", got)
	}
	playerRow := func(id string) string { return `#players tr[data-player-id="` + id + `"]` }
	if got := text(playerRow("76561198000000001") + " td.position"); got != "x 384, z 640" {
		t.Fatalf("Alice's position cell = %q", got)
	}
	if got := text(playerRow("76561198000000003") + " td.position"); got != "no position" {
		t.Fatalf("Carol's position cell = %q", got)
	}
	if got := text(playerRow("76561198000000004") + " td.position"); got != "x 128, z 896" {
		t.Fatalf("Dave's flat position cell = %q", got)
	}
	if got := text(playerRow("76561198000000001") + " td.data"); got != "alive: true, health: 82" {
		t.Fatalf("Alice's data cell = %q (the flags belong beside the name, not in the summary)", got)
	}
	// The admin flags set on Alice are badges beside her name (issue #71),
	// the set ones only, in the order the snapshot lists them (a Go map
	// encodes its keys sorted, hence freeze before god here); Bob, with no
	// flags, has none.
	if got := evalString(`Array.from(document.querySelectorAll(` + strconv.Quote(playerRow("76561198000000001")+" .badge.flag") + `)).map((b) => b.textContent).join(",")`); got != "freeze,god" {
		t.Fatalf("Alice's flag badges = %q, want freeze,god", got)
	}
	if got := evalString(`String(document.querySelectorAll(` + strconv.Quote(playerRow("76561198000000002")+" .badge.flag") + `).length)`); got != "0" {
		t.Fatalf("Bob has %s flag badges, want none", got)
	}
	waitJS("every visible tile loaded", `(function(){const s=document.querySelector("#map .map-tiles");return s && s.dataset.total !== "0" && s.dataset.loaded === s.dataset.total && s.dataset.failed === "0"})()`)
	if got := evalString(`String(document.querySelectorAll("#map .map-marker:not(.map-marker-vehicle)").length)`); got != "5" {
		t.Fatalf("%s player markers plotted, want Alice, Bob, Dave, and the two colon identities", got)
	}
	if got := evalString(`String(document.querySelectorAll("#map .map-marker-vehicle").length) + " " + document.querySelector(` + strconv.Quote(vehicleMarker("0-4242")) + `).title`); got != "1 ADA 4x4 (x 256, z 768)" {
		t.Fatalf("vehicle markers = %q, want the one car with its label", got)
	}
	if got := evalString(`document.querySelector(` + strconv.Quote(marker("a:b", "c")) + `).title + " | " + document.querySelector(` + strconv.Quote(marker("a", "b:c")) + `).title`); got != "Colon one (x 600, z 200) | Colon two (x 700, z 200)" {
		t.Fatalf("colon identities are not two distinct markers: %q", got)
	}
	// Where a marker sits on the page is checked against the frame the map
	// was fitted with: the raster centred in the stage at the scale that
	// fits it, u = x and v = 1024 - z, north up. The canvas pixel under
	// each marker's centre is the colour painted over that quadrant, which
	// is what says the tiles are drawn in the same frame as the markers:
	// a flipped axis puts Alice's marker over quadrant (1, 2), a swapped
	// one over (2, 1).
	geometry := `(function(){
		const stage = document.querySelector("#map .map-stage");
		const rect = stage.getBoundingClientRect();
		const width = Math.round(rect.width), height = Math.round(rect.height);
		const scale = Math.min(width / 1024, height / 1024);
		const ox = width / 2 - 512 * scale, oy = height / 2 - 512 * scale;
		const canvas = document.querySelector("#map .map-canvas");
		const ratio = window.devicePixelRatio || 1;
		const ctx = canvas.getContext("2d");
		const out = [];
		for (const [id, x, z] of [["76561198000000001", 384, 640], ["76561198000000002", 896, 128], ["76561198000000004", 128, 896]]) {
			const key = JSON.stringify(["steam", id]);
			const dot = document.querySelector(".map-marker[data-marker-key='" + key + "'] .map-marker-dot").getBoundingClientRect();
			const got = { x: dot.left + dot.width / 2 - rect.left, y: dot.top + dot.height / 2 - rect.top };
			const want = { x: ox + x * scale, y: oy + (1024 - z) * scale };
			const px = ctx.getImageData(Math.round(want.x * ratio), Math.round(want.y * ratio), 1, 1).data;
			out.push([Math.round((got.x - want.x) * 10) / 10, Math.round((got.y - want.y) * 10) / 10, px[0], px[1], px[2]].join(" "));
		}
		return out.join(";");
	})()`
	for i, entry := range strings.Split(evalString(geometry), ";") {
		var dx, dy float64
		var r, g, b int
		if _, err := fmt.Sscan(entry, &dx, &dy, &r, &g, &b); err != nil {
			t.Fatalf("marker geometry %q: %v", entry, err)
		}
		if dx < -1.5 || dx > 1.5 || dy < -1.5 || dy > 1.5 {
			t.Errorf("marker %d is off by (%.1f, %.1f) px from where the world frame puts it", i, dx, dy)
		}
		want := []color.NRGBA{e2eQuadrantColour(1, 1), e2eQuadrantColour(3, 3), e2eQuadrantColour(0, 0)}[i]
		if abs(r-int(want.R)) > 4 || abs(g-int(want.G)) > 4 || abs(b-int(want.B)) > 4 {
			t.Errorf("tile pixel under marker %d = (%d, %d, %d), want quadrant colour (%d, %d, %d)", i, r, g, b, want.R, want.G, want.B)
		}
	}

	// 9a. A snapshot is whole: the next one moves Alice, drops Bob, and the
	// page follows without a click, marker for marker.
	plugin.queue("state.players", map[string]any{
		"players": []map[string]any{
			{"player": alice, "name": "Alice", "position": []float64{640, 12.5, 640}},
			{"player": carol, "name": "Carol"},
			{"player": dave, "name": "Dave", "position": []float64{128, 896}},
		},
	})
	waitJS("the new snapshot replaced the markers",
		`document.querySelectorAll("#map .map-marker:not(.map-marker-vehicle)").length === 2 && document.querySelector(`+strconv.Quote(marker("steam", "76561198000000001"))+`).dataset.x === "640" && !document.querySelector('#players tr[data-player-id="76561198000000002"]')`)
	// The vehicles are their own snapshot: the players' replacement left
	// the car where it was.
	if got := evalString(`String(document.querySelectorAll("#map .map-marker-vehicle").length)`); got != "1" {
		t.Fatalf("%s vehicle markers after the players snapshot, want the car still there", got)
	}
	if got := text("#map-status"); !strings.Contains(got, "3 players") {
		t.Fatalf("map status after the second snapshot = %q", got)
	}

	// 9b. A pan is a pan and a key is a click. Zoomed in on Alice (a fitted
	// map is smaller than the viewport and stays centred, so it cannot pan),
	// the stage is dragged 40 px with the mouse: every marker moves with it,
	// and nothing is selected. Then a marker is activated from the keyboard,
	// which must still open the action list: a finished drag may not leave
	// marker activation disabled behind it.
	aliceMarker := marker("steam", "76561198000000001")
	markerX := func() string {
		return `Math.round(document.querySelector(` + strconv.Quote(aliceMarker) + `).getBoundingClientRect().left)`
	}
	run("zoom in on Alice", chromedp.Click(playerRow("76561198000000001")+" button[data-show]", chromedp.ByQuery))
	// At one metre per pixel the scale bar's nicest length near 90 px is 50 m.
	waitJS("the view is at native zoom on Alice", `document.querySelector("#map .map-scale-label").textContent === "50 m"`)
	// The Show click scrolled Alice's row into view, which with the vehicle
	// list below the players can leave the stage above the viewport, where
	// a synthetic mouse event lands on nothing. Bring it back first.
	var stageBox []float64
	run("measure the stage", chromedp.ScrollIntoView("#map .map-stage", chromedp.ByQuery),
		chromedp.Evaluate(`(function(){const r=document.querySelector("#map .map-stage").getBoundingClientRect();return [r.left, r.top, r.width, r.height]})()`, &stageBox))
	beforeDrag := evalString(`String(` + markerX() + `)`)
	fromX, fromY := stageBox[0]+stageBox[2]/2, stageBox[1]+stageBox[3]/2
	run("drag the map 40 px to the right",
		input.DispatchMouseEvent(input.MousePressed, fromX, fromY).WithButton(input.Left).WithButtons(1).WithClickCount(1),
		input.DispatchMouseEvent(input.MouseMoved, fromX+10, fromY).WithButton(input.Left).WithButtons(1),
		input.DispatchMouseEvent(input.MouseMoved, fromX+40, fromY).WithButton(input.Left).WithButtons(1),
		input.DispatchMouseEvent(input.MouseReleased, fromX+40, fromY).WithButton(input.Left).WithClickCount(1))
	waitJS("the markers moved with the drag", markerX()+` === `+beforeDrag+` + 40`)
	if got := evalString(`location.hash`); !strings.HasSuffix(got, "/map") {
		t.Fatalf("a drag navigated away: %q", got)
	}
	run("activate Alice's marker from the keyboard",
		chromedp.Focus(aliceMarker, chromedp.ByQuery), chromedp.KeyEvent("\r"),
		chromedp.WaitVisible("#target-player", chromedp.ByQuery))
	if got := evalString(`location.hash`); got != "#/servers/"+created.Server.ID+"?player=76561198000000001" {
		t.Fatalf("keyboard activation after a drag landed on %q", got)
	}

	// 9c. A click on a marker opens the action list with that player as
	// the target, and the player action's form arrives filled in.
	run("back to the map", chromedp.Evaluate(`location.hash = `+strconv.Quote("#/servers/"+created.Server.ID+"/map"), nil),
		chromedp.WaitVisible(aliceMarker, chromedp.ByQuery))
	run("click Alice's marker", chromedp.Click(aliceMarker, chromedp.ByQuery),
		chromedp.WaitVisible("#target-player", chromedp.ByQuery))
	if got := evalString(`location.hash`); got != "#/servers/"+created.Server.ID+"?player=76561198000000001" {
		t.Fatalf("marker click landed on %q", got)
	}
	if got := text("#target-player"); !strings.Contains(got, "Alice") || !strings.Contains(got, "steam:76561198000000001") {
		t.Fatalf("target banner = %q", got)
	}
	if got := attribute(`a[data-action-code="example-mod.heal"]`, "href"); !strings.HasSuffix(got, "?player=76561198000000001") {
		t.Fatalf("player action href = %q, want the preselected player", got)
	}
	run("open the action with the target preselected", chromedp.Click(`a[data-action-code="example-mod.heal"]`, chromedp.ByQuery),
		chromedp.WaitVisible("#action-form", chromedp.ByQuery))
	if got := evalString(`document.querySelector('input[name="referenceKey"]').value`); got != "76561198000000001" {
		t.Fatalf("referenceKey = %q, want the preselected player", got)
	}

	// 9c'. The same for a vehicle: its marker lands on the action list with
	// the vehicle preselected, the vehicle action's form opens with the id
	// filled in, and the vehicles snapshot feeds its suggestions.
	//
	// The map is opened with the page's animation frames held back, and
	// the marker must already be at its position when it is first visible.
	// A marker placed only by the next frame sat at the stage's corner
	// while a click measured it and had moved by the time the click landed,
	// which one CI run in six lost the step to (#102). The frames are
	// released, in order, before the click.
	run("hold the page's animation frames", chromedp.Evaluate(
		`(function(){const held=[];const real=window.requestAnimationFrame;`+
			`window.requestAnimationFrame=(cb)=>held.push(cb);`+
			`window.__e2eReleaseFrames=()=>{window.requestAnimationFrame=real;const run=held.splice(0);for(const cb of run)cb(performance.now());return run.length};`+
			`return true})()`, nil))
	run("back to the map for the car", chromedp.Evaluate(`location.hash = `+strconv.Quote("#/servers/"+created.Server.ID+"/map"), nil),
		chromedp.WaitVisible(vehicleMarker("0-4242"), chromedp.ByQuery))
	if got := evalString(`document.querySelector(` + strconv.Quote(vehicleMarker("0-4242")) + `).style.transform`); got == "" {
		t.Fatalf("the car's marker was visible before the widget placed it")
	}
	if got := evalString(`String(window.__e2eReleaseFrames())`); got == "0" {
		t.Fatalf("no animation frame was held back, so the placement check proved nothing")
	}
	run("click the car's marker", chromedp.Click(vehicleMarker("0-4242"), chromedp.ByQuery),
		chromedp.WaitVisible("#target-vehicle", chromedp.ByQuery))
	if got := evalString(`location.hash`); got != "#/servers/"+created.Server.ID+"?vehicle=0-4242" {
		t.Fatalf("vehicle marker click landed on %q", got)
	}
	if got := text("#target-vehicle"); !strings.Contains(got, "ADA 4x4") || !strings.Contains(got, "0-4242") {
		t.Fatalf("vehicle target banner = %q", got)
	}
	if got := attribute(`a[data-action-code="example-mod.unstuck"]`, "href"); !strings.HasSuffix(got, "?vehicle=0-4242") {
		t.Fatalf("vehicle action href = %q, want the preselected vehicle", got)
	}
	if got := attribute(`a[data-action-code="example-mod.heal"]`, "href"); strings.Contains(got, "vehicle=") {
		t.Fatalf("player action href = %q carries a vehicle preselection", got)
	}
	run("open the vehicle action with the target preselected", chromedp.Click(`a[data-action-code="example-mod.unstuck"]`, chromedp.ByQuery),
		chromedp.WaitVisible("#action-form", chromedp.ByQuery))
	if got := evalString(`document.querySelector('input[name="referenceKey"]').value`); got != "0-4242" {
		t.Fatalf("referenceKey = %q, want the preselected vehicle", got)
	}
	if got := evalString(`(function(){const i=document.querySelector('input[name="referenceKey"]');const l=document.getElementById(i.getAttribute("list"));return Array.from(l.options).map(o=>o.value+"="+o.textContent).join(",")})()`); got != "0-4242=ADA 4x4 (car),0-4243=Boat_01 (boat)" {
		t.Errorf("vehicle datalist = %q", got)
	}

	// 9d. The entities snapshot (issue #72) is the third whole list: what a
	// mod put on the map that is neither a player nor a vehicle. One beacon
	// sits at the centre of a raster quadrant and is plotted apart from the
	// other two marker sets; the second has no position at all, so it is
	// listed and counted but never plotted.
	//
	// It is queued here rather than beside the vehicles because the earlier
	// steps count the player markers as every marker that is not a vehicle,
	// which an entity marker would join.
	plugin.queue("state.entities", map[string]any{
		"capturedAt": time.Now().UTC().Add(-10 * time.Second).Format("2006-01-02T15:04:05Z"),
		"entities": []map[string]any{
			{"id": "sample:beacon:nwaf", "kind": "beacon", "position": []float64{896, 20, 384},
				"data": map[string]any{"label": "Northwest Airfield", "landmark": "nwaf"}},
			{"id": "sample:beacon:none", "kind": "beacon", "data": map[string]any{"label": "Nowhere"}},
		},
	})
	entityMarker := func(id string) string { return `.map-marker-entity[data-marker-key='entity:` + id + `']` }
	entityRow := func(id string) string { return `#entities tr[data-entity-id="` + id + `"]` }
	run("back to the map for the entities", chromedp.Evaluate(`location.hash = `+strconv.Quote("#/servers/"+created.Server.ID+"/map"), nil),
		chromedp.WaitVisible("#map", chromedp.ByQuery), chromedp.WaitVisible("#entities", chromedp.ByQuery))
	waitJS("the entities snapshot reached the page",
		`document.querySelector("#map-status").dataset.entities === "2" && document.querySelector("#map-status").dataset.entitiesPlotted === "1"`)
	if got := evalString(`String(document.querySelectorAll("#map .map-marker-entity").length) + " " + document.querySelector(` + strconv.Quote(entityMarker("sample:beacon:nwaf")) + `).title`); got != "1 Northwest Airfield (x 896, z 384)" {
		t.Fatalf("entity markers = %q, want the one beacon with its label", got)
	}
	if got := evalString(`document.querySelector(` + strconv.Quote(entityMarker("sample:beacon:none")) + `) ? "plotted" : "none"`); got != "none" {
		t.Fatalf("the beacon with no position was plotted")
	}
	if got := text(entityRow("sample:beacon:nwaf")); !strings.Contains(got, "Northwest Airfield") || !strings.Contains(got, "beacon") {
		t.Fatalf("the plotted beacon's row = %q, want its label and kind", got)
	}
	if got := text(entityRow("sample:beacon:none")); !strings.Contains(got, "Nowhere") || !strings.Contains(got, "beacon") {
		t.Fatalf("the unplotted beacon's row = %q, want its label and kind", got)
	}
	if got := text(entityRow("sample:beacon:nwaf") + " td.position"); got != "x 896, z 384" {
		t.Fatalf("the plotted beacon's position cell = %q", got)
	}
	if got := text(entityRow("sample:beacon:none") + " td.position"); got != "no position" {
		t.Fatalf("the unplotted beacon's position cell = %q", got)
	}
	// The label is the row's first cell, so the data summary beside it
	// carries what else the snapshot said and not the label again.
	if got := text(entityRow("sample:beacon:nwaf") + " td.data"); got != "landmark: nwaf" {
		t.Fatalf("the plotted beacon's data cell = %q", got)
	}
	// The entities are counted last in the status line, so the clause about
	// positions the map cannot plot is the entities' own.
	if got := text("#map-status"); !strings.Contains(got, "2 entities, captured") ||
		!strings.Contains(got, "1 without a position the map can plot. Re-read") {
		t.Fatalf("map status with the entities = %q", got)
	}

	// 9e. A world with no tileset still lists the players, with their
	// positions as numbers and a notice in place of the map.
	run("open the map for a world with no tileset",
		chromedp.Evaluate(`location.hash = `+strconv.Quote("#/servers/"+created.Server.ID+"/map?world=nowhere"), nil),
		chromedp.WaitVisible("#map-missing", chromedp.ByQuery), chromedp.WaitVisible("#players", chromedp.ByQuery))
	if evalString(`document.querySelector("#map") ? "map" : "none"`) != "none" {
		t.Fatalf("a map was drawn for a world with no tileset")
	}
	if got := text("#map-missing"); !strings.Contains(got, "nowhere") {
		t.Fatalf("missing-map notice = %q", got)
	}
	if got := text(playerRow("76561198000000001") + " td.position"); got != "640, 13, 640" {
		t.Fatalf("Alice's position without a map = %q, want the raw numbers", got)
	}
	if got := text(vehicleRow("0-4242") + " td.position"); got != "256, 5, 768" {
		t.Fatalf("the car's position without a map = %q, want the raw numbers", got)
	}

	// 10. Signing out forgets the token: the page is back at the prompt and
	// a reload stays there.
	run("sign out", chromedp.Click("#sign-out", chromedp.ByQuery), chromedp.WaitVisible("#login", chromedp.ByQuery),
		chromedp.Reload(), chromedp.WaitVisible("#login", chromedp.ByQuery))
	if got := evalString(`sessionStorage.getItem("vyshka.adminToken") || "none"`); got != "none" {
		t.Fatalf("token survived sign-out: %q", got)
	}
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
