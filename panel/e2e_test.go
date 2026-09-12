package panel_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"github.com/That1Drifter/vyshka/hub"
	"github.com/That1Drifter/vyshka/panel"
)

// The end-to-end test of issue #13: a real hub with the embedded panel, a
// fake plugin speaking the Plugin API, and a headless browser driving the
// page the way an operator would. It grades the two things a unit test of
// either side cannot: that the form a browser renders is the one the
// manifest's schema describes, and that a click on Dispatch round-trips
// through the hub to the plugin and back onto the page. Issue #47 added the
// event feed to the same run: a batch the plugin publishes is listed in the
// hub's order, filtered by type, followed as new batches land, and paged.
//
// It needs a Chromium-family browser. CI has one and sets VYSHKA_E2E=required
// so a missing browser fails the job instead of skipping the test; anywhere
// else the test skips itself when it finds none. VYSHKA_E2E_BROWSER names the
// executable explicitly.

const e2eAdminToken = "vya_E2ETOKENE2ETOKENE2ETOKENE2"

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
				"item":         map[string]any{"type": "string", "x-vyshka-widget": "itemlist"},
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
	}},
}

var e2ePlayers = map[string]any{
	"players": []map[string]any{{
		"player": map[string]any{"platform": "steam", "id": "76561198000000001"},
		"name":   "Alice",
	}},
}

func TestPanelEndToEnd(t *testing.T) {
	browser := findBrowser(t)

	server, err := hub.New(context.Background(), hub.Config{
		DatabaseURL: filepath.Join(t.TempDir(), "e2e.db"),
		AdminToken:  e2eAdminToken,
		Logger:      slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Panel:       panel.Handler(),
	})
	if err != nil {
		t.Fatalf("boot hub: %v", err)
	}
	t.Cleanup(func() { server.Close() })
	web := httptest.NewServer(server.Handler())
	t.Cleanup(web.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)

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

	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx, append(
		chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath(browser))...)
	t.Cleanup(cancelAlloc)
	page, cancelPage := chromedp.NewContext(allocCtx)
	t.Cleanup(cancelPage)
	// The browser's lifetime is tied to the context of the first Run, so it
	// is started here on the tab context rather than under a step's deadline.
	if err := chromedp.Run(page); err != nil {
		t.Fatalf("start %s: %v", browser, err)
	}

	// Anything the page logs or throws is kept for the failure report, which
	// otherwise says only that a selector never appeared.
	var consoleMu sync.Mutex
	var console []string
	chromedp.ListenTarget(page, func(event any) {
		switch e := event.(type) {
		case *cdpruntime.EventExceptionThrown:
			consoleMu.Lock()
			console = append(console, "exception: "+e.ExceptionDetails.Error())
			consoleMu.Unlock()
		case *cdpruntime.EventConsoleAPICalled:
			var parts []string
			for _, arg := range e.Args {
				parts = append(parts, string(arg.Value))
			}
			consoleMu.Lock()
			console = append(console, string(e.Type)+": "+strings.Join(parts, " "))
			consoleMu.Unlock()
		}
	})
	fail := func(format string, args ...any) {
		t.Helper()
		var bodyText string
		snapshot, cancelSnapshot := context.WithTimeout(page, 5*time.Second)
		_ = chromedp.Run(snapshot, chromedp.Evaluate(`document.body.innerText`, &bodyText))
		cancelSnapshot()
		consoleMu.Lock()
		logged := strings.Join(console, "\n")
		consoleMu.Unlock()
		t.Fatalf(format+"\n--- page text ---\n%s\n--- browser console ---\n%s", append(args, bodyText, logged)...)
	}
	// Every step gets its own deadline, well inside the test's, so a step
	// that hangs still leaves time to read the page for the report.
	run := func(what string, actions ...chromedp.Action) {
		t.Helper()
		step, cancelStep := context.WithTimeout(page, 30*time.Second)
		defer cancelStep()
		if err := chromedp.Run(step, actions...); err != nil {
			fail("%s: %v", what, err)
		}
	}
	// setValue replaces an input's live value through the DOM property.
	// chromedp.Clear writes the value attribute, which a field the test has
	// already typed into ignores, and SetValue refuses an empty string.
	setValue := func(selector, value string) chromedp.Action {
		return chromedp.Evaluate(`(function(){const i=document.querySelector(`+strconv.Quote(selector)+`);i.value=`+
			strconv.Quote(value)+`;i.dispatchEvent(new Event("input",{bubbles:true}));return true})()`, nil)
	}
	// waitJS polls a predicate in the page until it is truthy.
	waitJS := func(what, predicate string) {
		t.Helper()
		var ok bool
		if err := chromedp.Run(page, chromedp.Poll(predicate, &ok,
			chromedp.WithPollingTimeout(30*time.Second), chromedp.WithPollingInterval(100*time.Millisecond))); err != nil {
			fail("%s: %v", what, err)
		}
	}
	text := func(selector string) string {
		t.Helper()
		var value string
		run("read "+selector, chromedp.Text(selector, &value, chromedp.ByQuery))
		return value
	}
	attribute := func(selector, name string) string {
		t.Helper()
		var value string
		var found bool
		run("attribute "+selector, chromedp.AttributeValue(selector, name, &value, &found, chromedp.ByQuery))
		if !found {
			t.Fatalf("%s has no %s attribute", selector, name)
		}
		return value
	}
	evalString := func(expression string) string {
		t.Helper()
		var value string
		run("evaluate "+expression, chromedp.Evaluate(expression, &value))
		return value
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
	// The payload sits inside a closed <details>, so it is read through the
	// DOM rather than as visible text.
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

	// 8a. The type filter is a hub-side query and part of the route.
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
	plugin.queue("event.batch", map[string]any{"events": []map[string]any{
		{"t": "core.player.disconnect", "ts": stamp(4), "data": map[string]any{"player": alice, "name": "Alice"}},
		{"t": "core.player.damage", "ts": stamp(0), "data": map[string]any{"player": alice, "amount": 12}},
		{"t": "example-mod.raid.ended", "ts": stamp(5), "data": map[string]any{"territoryId": "t-19"}},
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

	// 8d. Paging. With the filter cleared the feed holds seven events and
	// no cursor. A batch of 150 older custom events then arrives under
	// follow: the first page fills, the tick adopts the hub's cursor, and
	// Load older walks to the end with no event shown twice.
	run("clear the filter", setValue("#event-filter", ""), chromedp.Click("#event-filter-apply", chromedp.ByQuery))
	waitJS("unfiltered feed shows every event", rowTypes+` === "core.player.kick,example-mod.raid.ended,core.player.disconnect,example-mod.raid.started,core.player.death,core.player.connect,core.player.damage"`)
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
	waitJS("the whole feed is on the page", `document.querySelectorAll("#events tbody tr").length === 157 && document.querySelector("#events-older").hidden`)
	if got := evalString(`String(new Set(Array.from(document.querySelectorAll("#events tbody tr")).map(r => r.dataset.eventId)).size)`); got != "157" {
		t.Fatalf("%s distinct event ids among 157 rows", got)
	}
	if got := evalString(`Array.from(document.querySelectorAll("#events tbody tr")).slice(0, 8).map(r => r.dataset.eventType).join(",")`); got != "core.player.kick,example-mod.raid.ended,core.player.disconnect,example-mod.raid.started,core.player.death,core.player.connect,core.player.damage,example-mod.tick" {
		t.Fatalf("feed head after paging = %q", got)
	}
	if got := evalString(`Array.from(document.querySelectorAll("#events tbody tr")).slice(7).map(r => r.querySelector("summary").textContent).join(",")`); !strings.HasPrefix(got, "n: 149,n: 148,") || !strings.HasSuffix(got, ",n: 1,n: 0") {
		t.Fatalf("older page is out of order: %.60s ... %.20s", got, got[len(got)-20:])
	}

	// 9. Signing out forgets the token: the page is back at the prompt and
	// a reload stays there.
	run("sign out", chromedp.Click("#sign-out", chromedp.ByQuery), chromedp.WaitVisible("#login", chromedp.ByQuery),
		chromedp.Reload(), chromedp.WaitVisible("#login", chromedp.ByQuery))
	if got := evalString(`sessionStorage.getItem("vyshka.adminToken") || "none"`); got != "none" {
		t.Fatalf("token survived sign-out: %q", got)
	}
}

// findBrowser locates a Chromium-family executable or decides between skip
// and fail, per the VYSHKA_E2E contract in the file comment.
func findBrowser(t *testing.T) string {
	t.Helper()
	if path := os.Getenv("VYSHKA_E2E_BROWSER"); path != "" {
		return path
	}
	var candidates []string
	if runtime.GOOS == "windows" {
		for _, root := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("LocalAppData")} {
			if root == "" {
				continue
			}
			candidates = append(candidates,
				filepath.Join(root, "Google", "Chrome", "Application", "chrome.exe"),
				filepath.Join(root, "Chromium", "Application", "chrome.exe"),
				filepath.Join(root, "Microsoft", "Edge", "Application", "msedge.exe"))
		}
		for _, candidate := range candidates {
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
	} else {
		candidates = []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "chrome",
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome", "/Applications/Chromium.app/Contents/MacOS/Chromium"}
		for _, candidate := range candidates {
			if path, err := exec.LookPath(candidate); err == nil {
				return path
			}
			if strings.HasPrefix(candidate, "/") {
				if _, err := os.Stat(candidate); err == nil {
					return candidate
				}
			}
		}
	}
	if os.Getenv("VYSHKA_E2E") == "required" {
		t.Fatalf("VYSHKA_E2E=required but no browser was found (tried %s); set VYSHKA_E2E_BROWSER", strings.Join(candidates, ", "))
	}
	t.Skipf("no Chromium-family browser found (tried %s); set VYSHKA_E2E_BROWSER to run the panel end-to-end test", strings.Join(candidates, ", "))
	return ""
}

// ---------------------------------------------------------------------------
// The Admin API and Plugin API halves, over HTTP like any other client.

type createdServer struct {
	Server struct {
		ID string `json:"id"`
	} `json:"server"`
	Enrollment struct {
		Token string `json:"token"`
	} `json:"enrollment"`
}

func postJSON(client *http.Client, url, bearer string, body any) (int, []byte, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	request, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(encoded))
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	return response.StatusCode, answer, err
}

func createServer(t *testing.T, hubURL, name string) createdServer {
	t.Helper()
	status, body, err := postJSON(http.DefaultClient, hubURL+"/api/v1/servers", e2eAdminToken,
		map[string]any{"name": name, "game": "e2e"})
	if err != nil || status != http.StatusCreated {
		t.Fatalf("create server: status %d err %v body %s", status, err, body)
	}
	var created createdServer
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode created server: %v", err)
	}
	return created
}

type wireEnvelope struct {
	V    int             `json:"v"`
	ID   string          `json:"id"`
	Type string          `json:"type"`
	Seq  int64           `json:"seq"`
	TS   string          `json:"ts"`
	Body json.RawMessage `json:"body"`
}

type dispatchBody struct {
	ActionID     string          `json:"actionId"`
	Code         string          `json:"code"`
	Context      string          `json:"context"`
	ReferenceKey string          `json:"referenceKey"`
	Params       json.RawMessage `json:"params"`
}

// fakePlugin is the smallest plugin that can serve this test: it enrolls,
// keeps one session, long-polls, and answers every action.dispatch with an
// action.ack and an action.result computed from the params. Its outbound
// buffer is acked cumulatively like the protocol says; it never needs to
// survive a session change, so it does no renumbering.
type fakePlugin struct {
	t      *testing.T
	hubURL string
	client *http.Client

	serverID, serverSecret, sessionToken string

	mu        sync.Mutex
	inAck     int64
	outSeq    int64
	buffer    []wireEnvelope
	ids       int64
	received  []dispatchBody
	allAcked  chan struct{}
	ackedOnce sync.Once
}

func newFakePlugin(t *testing.T, hubURL, enrollmentToken string) *fakePlugin {
	t.Helper()
	p := &fakePlugin{t: t, hubURL: hubURL, client: &http.Client{Timeout: 15 * time.Second}, allAcked: make(chan struct{})}
	status, body, err := postJSON(p.client, hubURL+"/plugin/v1/enroll", "", map[string]any{
		"enrollmentToken": enrollmentToken, "game": "e2e",
		"plugin": map[string]any{"name": "e2e-plugin", "version": "0.1.0"}, "transports": []string{"poll"},
	})
	if err != nil || status != http.StatusCreated {
		t.Fatalf("enroll: status %d err %v body %s", status, err, body)
	}
	var enrolled struct {
		ServerID     string `json:"serverId"`
		ServerSecret string `json:"serverSecret"`
	}
	if err := json.Unmarshal(body, &enrolled); err != nil {
		t.Fatalf("decode enrollment: %v", err)
	}
	p.serverID, p.serverSecret = enrolled.ServerID, enrolled.ServerSecret

	status, body, err = postJSON(p.client, hubURL+"/plugin/v1/session", "", map[string]any{
		"serverId": p.serverID, "serverSecret": p.serverSecret, "protocolVersion": 1,
		"pollTimeoutSeconds": 5,
		"plugin":             map[string]any{"name": "e2e-plugin", "version": "0.1.0"}, "transports": []string{"poll"},
	})
	if err != nil || status != http.StatusOK {
		t.Fatalf("session: status %d err %v body %s", status, err, body)
	}
	var session struct {
		SessionToken string `json:"sessionToken"`
	}
	if err := json.Unmarshal(body, &session); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	p.sessionToken = session.SessionToken
	return p
}

func (p *fakePlugin) queue(envelopeType string, body any) {
	encoded, err := json.Marshal(body)
	if err != nil {
		p.t.Fatalf("encode %s: %v", envelopeType, err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.outSeq++
	p.ids++
	p.buffer = append(p.buffer, wireEnvelope{
		V: 1, ID: "e2e-" + strconv.FormatInt(p.ids, 10), Type: envelopeType, Seq: p.outSeq,
		TS: time.Now().UTC().Format(time.RFC3339), Body: encoded,
	})
}

func (p *fakePlugin) run(ctx context.Context) {
	for ctx.Err() == nil {
		p.mu.Lock()
		request := map[string]any{"ack": p.inAck, "envelopes": append([]wireEnvelope(nil), p.buffer...)}
		p.mu.Unlock()
		status, body, err := postJSON(p.client, p.hubURL+"/plugin/v1/poll", p.sessionToken, request)
		if err != nil || status != http.StatusOK {
			if ctx.Err() != nil {
				return
			}
			p.t.Logf("fake plugin poll: status %d err %v body %s", status, err, body)
			time.Sleep(200 * time.Millisecond)
			continue
		}
		var answer struct {
			Envelopes []wireEnvelope `json:"envelopes"`
			Ack       int64          `json:"ack"`
		}
		if err := json.Unmarshal(body, &answer); err != nil {
			p.t.Logf("fake plugin poll: decode: %v", err)
			continue
		}
		p.mu.Lock()
		kept := p.buffer[:0]
		for _, e := range p.buffer {
			if e.Seq > answer.Ack {
				kept = append(kept, e)
			}
		}
		p.buffer = kept
		if len(p.buffer) == 0 && p.outSeq > 0 {
			p.ackedOnce.Do(func() { close(p.allAcked) })
		}
		for _, e := range answer.Envelopes {
			if e.Seq > p.inAck {
				p.inAck = e.Seq
			}
			if e.Type != "action.dispatch" {
				continue
			}
			var dispatch dispatchBody
			if err := json.Unmarshal(e.Body, &dispatch); err != nil {
				continue
			}
			p.received = append(p.received, dispatch)
			p.mu.Unlock()
			p.queue("action.ack", map[string]any{"actionId": dispatch.ActionID})
			var params struct {
				Amount float64 `json:"amount"`
			}
			_ = json.Unmarshal(dispatch.Params, &params)
			p.queue("action.result", map[string]any{
				"actionId": dispatch.ActionID, "ok": true,
				"result": map[string]any{"healedTo": params.Amount, "player": dispatch.ReferenceKey}, "durationMs": 7,
			})
			p.mu.Lock()
		}
		p.mu.Unlock()
	}
}

func (p *fakePlugin) awaitAcked(ctx context.Context) error {
	select {
	case <-p.allAcked:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// awaitDrained waits until the hub has acked everything queued so far, which
// is when everything queued so far is stored.
func (p *fakePlugin) awaitDrained(ctx context.Context) error {
	for ctx.Err() == nil {
		p.mu.Lock()
		pending := len(p.buffer)
		p.mu.Unlock()
		if pending == 0 {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return ctx.Err()
}

func (p *fakePlugin) dispatches() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.received)
}

func (p *fakePlugin) lastDispatch() dispatchBody {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.received) == 0 {
		p.t.Fatal("the plugin received no dispatch")
	}
	return p.received[len(p.received)-1]
}
