package panel_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// The end-to-end test of issue #79: a player's profile reached from the event
// feed and from the players page, its events across the installation with the
// roles the identity held, the actions against it, notes written and deleted
// behind their tick, a narrowed token seeing the parts its grants cover and a
// notice for the rest, and a webhook's redaction paths set and edited from the
// forms.
func TestPanelPlayerProfileEndToEnd(t *testing.T) {
	h := newE2EHarness(t, "")
	hubURL := h.URL()
	run, setValue, waitJS, text, evalString := h.run, h.setValue, h.waitJS, h.text, h.evalString

	server := createServer(t, hubURL, "Profile server")
	plugin := newFakePlugin(t, hubURL, server.Enrollment.Token)
	anna := map[string]any{"platform": "steam", "id": "76561198000000011"}
	bob := map[string]any{"platform": "steam", "id": "76561198000000012"}
	stamp := func(minutesAgo int) string {
		return time.Now().UTC().Add(-time.Duration(minutesAgo) * time.Minute).Format("2006-01-02T15:04:05.000Z")
	}
	plugin.queue("manifest.publish", manageManifest)
	plugin.queue("event.batch", map[string]any{"events": []map[string]any{
		{"t": "core.player.connect", "ts": stamp(30), "data": map[string]any{"player": anna, "name": "Anna"}},
		{"t": "core.player.death", "ts": stamp(20), "data": map[string]any{
			"player": bob, "name": "Bob", "killer": anna, "killerName": "Anna", "weapon": "M4A1",
		}},
		{"t": "core.player.chat", "ts": stamp(10), "data": map[string]any{"player": anna, "name": "Anna", "text": "<b>hello</b>"}},
	}})
	plugin.queue("state.players", map[string]any{"players": []map[string]any{
		{"player": anna, "name": "Anna", "position": []float64{1, 2, 3}},
	}})
	go plugin.run(h.Ctx)
	if err := plugin.awaitAcked(h.Ctx); err != nil {
		t.Fatalf("the plugin's fixture was never acked: %v", err)
	}
	status, body := adminRequest(t, http.MethodPost, hubURL+"/api/v1/servers/"+server.Server.ID+"/actions", map[string]any{
		"code": "example-mod.heal", "context": "player", "referenceKey": anna["id"], "params": map[string]any{"amount": 50},
	})
	if status != http.StatusAccepted {
		t.Fatalf("dispatch the heal: status %d body %s", status, body)
	}
	var dispatched struct {
		ActionID string `json:"actionId"`
	}
	_ = json.Unmarshal(body, &dispatched)
	h.waitUntil("the heal completed", 30*time.Second, func() bool {
		status, body := adminRequest(t, http.MethodGet, hubURL+"/api/v1/actions/"+dispatched.ActionID, nil)
		return status == http.StatusOK && strings.Contains(string(body), `"completed"`)
	})

	annaKey := "steam:" + anna["id"].(string)
	profileHash := "#/players/steam/" + anna["id"].(string)
	notesURL := hubURL + "/api/v1/players/steam/" + anna["id"].(string) + "/notes"

	// 1. From the event feed: every identity an event names is a link to
	// its profile.
	run("open the panel", chromedp.Navigate(hubURL+"/"), chromedp.WaitVisible("#login", chromedp.ByQuery))
	run("sign in", chromedp.SendKeys("#token", e2eAdminToken, chromedp.ByQuery),
		chromedp.Click("#sign-in", chromedp.ByQuery), chromedp.WaitVisible("#servers", chromedp.ByQuery))
	run("open the event feed", chromedp.Evaluate(`location.hash = `+strconv.Quote("#/servers/"+server.Server.ID+"/events?follow=off"), nil),
		chromedp.WaitVisible(`#events a[data-profile="`+annaKey+`"]`, chromedp.ByQuery))
	if got := evalString(`String(document.querySelectorAll('#events tr[data-event-type="core.player.death"] a[data-profile]').length)`); got != "2" {
		t.Fatalf("the death row links %s profiles, want the victim and the killer", got)
	}
	if got := text(`#events tr[data-event-type="core.player.death"] a[data-profile="` + annaKey + `"]`); got != "killer: Anna" {
		t.Fatalf("the killer's link reads %q, want the role and the name the payload gives", got)
	}
	run("follow the killer's link", chromedp.Click(`#events tr[data-event-type="core.player.death"] a[data-profile="`+annaKey+`"]`, chromedp.ByQuery),
		chromedp.WaitVisible("#player-events", chromedp.ByQuery))

	// 2. The profile: three events, newest first, the roles she held, the
	// name from the newest event that names her as the player, the heal.
	waitJS("the profile's events are drawn", `document.querySelectorAll("#player-events tbody tr").length === 3`)
	if got := evalString(`Array.from(document.querySelectorAll("#player-events tbody tr")).map(r => r.dataset.eventType).join(",")`); got != "core.player.chat,core.player.death,core.player.connect" {
		t.Fatalf("the profile lists %q, want the three events newest first", got)
	}
	if got := text(`#player-events tr[data-event-type="core.player.death"] .badges`); got != "killer" {
		t.Fatalf("the death row's roles read %q, want killer", got)
	}
	if got := text("#player-title"); !strings.HasPrefix(got, "Anna") || !strings.Contains(got, annaKey) {
		t.Fatalf("the profile title is %q, want her name beside the identity", got)
	}
	if got := evalString(`String(document.querySelectorAll("#player-events b").length)`); got != "0" {
		t.Fatal("a chat line's markup reached the page as markup")
	}
	if got := text("#player-window"); !strings.Contains(got, "30 days") {
		t.Fatalf("the profile's window note is %q, want the retention it is a window of", got)
	}
	waitJS("the heal is listed", `document.querySelectorAll("#player-actions tbody tr").length === 1`)
	if got := text(`#player-actions tr[data-action-id="` + dispatched.ActionID + `"]`); !strings.Contains(got, "example-mod.heal") || !strings.Contains(got, "completed") {
		t.Fatalf("the action row reads %q, want the completed heal", got)
	}
	waitJS("the notes say there are none", `!document.querySelector("#notes-empty").hidden`)

	// 3. Notes: written under the signed-in token, deleted only behind the
	// tick.
	run("write a note", setValue("#note-text", "Warned for camping the airfield spawn."),
		chromedp.Click("#note-add", chromedp.ByQuery))
	waitJS("the note is listed", `document.querySelectorAll("#notes li.note").length === 1`)
	notes := func() []map[string]any {
		t.Helper()
		status, body := adminRequest(t, http.MethodGet, notesURL, nil)
		if status != http.StatusOK {
			t.Fatalf("read notes: status %d body %s", status, body)
		}
		var page struct {
			Notes []map[string]any `json:"notes"`
		}
		_ = json.Unmarshal(body, &page)
		return page.Notes
	}
	stored := notes()
	if len(stored) != 1 || stored[0]["text"] != "Warned for camping the airfield spawn." {
		t.Fatalf("the hub holds %+v, want the note as written", stored)
	}
	noteID, _ := stored[0]["id"].(string)
	run("delete without the tick", chromedp.Click(`button[data-note-delete="`+noteID+`"]`, chromedp.ByQuery),
		chromedp.WaitVisible("#notes-error", chromedp.ByQuery))
	if len(notes()) != 1 {
		t.Fatal("an unticked delete reached the hub; the tick is the guard")
	}
	run("delete with the tick", chromedp.Click("#note-confirm-"+noteID, chromedp.ByQuery),
		chromedp.Click(`button[data-note-delete="`+noteID+`"]`, chromedp.ByQuery))
	waitJS("the note is gone from the page", `document.querySelectorAll("#notes li.note").length === 0`)
	if len(notes()) != 0 {
		t.Fatal("the ticked delete did not reach the hub")
	}

	// 4. The players page lists who is online and links their profiles.
	run("open the players page", chromedp.Click("#nav a[data-nav=players]", chromedp.ByQuery),
		chromedp.WaitVisible("#players-online-table", chromedp.ByQuery))
	if got := text(`#players-online-table tr[data-player-id="` + anna["id"].(string) + `"]`); !strings.Contains(got, "Anna") || !strings.Contains(got, "Profile server") {
		t.Fatalf("the online row reads %q, want her name and her server", got)
	}
	run("look an identity up by hand", setValue("#lookup-id", bob["id"].(string)),
		chromedp.Click("#lookup-open", chromedp.ByQuery))
	waitJS("the victim's profile holds his death", `document.querySelectorAll("#player-events tbody tr").length === 1`)
	if got := text(`#player-events tr[data-event-type="core.player.death"] .badges`); got != "player" {
		t.Fatalf("the victim's role reads %q, want player", got)
	}

	// 5. A token that reads events alone sees the events and one notice in
	// each part it cannot read.
	status, body = adminRequest(t, http.MethodPost, hubURL+"/api/v1/tokens", map[string]any{
		"name": "events only", "scopes": []string{"servers:read", "events:read"},
	})
	if status != http.StatusCreated {
		t.Fatalf("mint the narrowed token: status %d body %s", status, body)
	}
	var minted struct {
		Secret string `json:"secret"`
	}
	_ = json.Unmarshal(body, &minted)
	run("sign out", chromedp.Click("#sign-out", chromedp.ByQuery), chromedp.WaitVisible("#login", chromedp.ByQuery))
	run("sign in narrowed", chromedp.SendKeys("#token", minted.Secret, chromedp.ByQuery),
		chromedp.Click("#sign-in", chromedp.ByQuery), chromedp.WaitVisible("#player-events-section", chromedp.ByQuery))
	run("open her profile narrowed", chromedp.Evaluate(`location.hash = `+strconv.Quote(profileHash), nil),
		chromedp.WaitVisible("#notes-forbidden", chromedp.ByQuery), chromedp.WaitVisible("#player-actions-forbidden", chromedp.ByQuery))
	waitJS("the narrowed profile still shows the events", `document.querySelectorAll("#player-events tbody tr").length === 3`)
	if got := text("#notes-forbidden"); !strings.Contains(got, "notes:read") {
		t.Fatalf("the notes notice reads %q, want the grant it needs", got)
	}

	// 6. Redaction paths from the webhook forms (protocol section 11.2).
	run("sign out again", chromedp.Click("#sign-out", chromedp.ByQuery), chromedp.WaitVisible("#login", chromedp.ByQuery))
	run("sign back in", chromedp.SendKeys("#token", e2eAdminToken, chromedp.ByQuery),
		chromedp.Click("#sign-in", chromedp.ByQuery), chromedp.WaitVisible("#player-events-section", chromedp.ByQuery))
	run("open the webhooks view", chromedp.Click("#nav a[data-nav=webhooks]", chromedp.ByQuery),
		chromedp.WaitVisible("#register-webhook", chromedp.ByQuery))
	run("register a redacting webhook",
		setValue("#webhook-url", "http://127.0.0.1:9/kill-feed"),
		setValue("#webhook-events", "core.player.death"),
		setValue("#webhook-redact", "position\nkiller.position\n"),
		chromedp.Click("#register-webhook-submit", chromedp.ByQuery),
		chromedp.WaitVisible("#webhook-secret-value", chromedp.ByQuery))
	redactOf := func() (string, []string) {
		t.Helper()
		status, body := adminRequest(t, http.MethodGet, hubURL+"/api/v1/webhooks", nil)
		if status != http.StatusOK {
			t.Fatalf("list webhooks: status %d", status)
		}
		var list struct {
			Webhooks []struct {
				ID     string   `json:"id"`
				Redact []string `json:"redact"`
			} `json:"webhooks"`
		}
		_ = json.Unmarshal(body, &list)
		if len(list.Webhooks) != 1 {
			t.Fatalf("the hub holds %d webhooks, want the one registered", len(list.Webhooks))
		}
		return list.Webhooks[0].ID, list.Webhooks[0].Redact
	}
	webhookID, redact := redactOf()
	if !reflect.DeepEqual(redact, []string{"position", "killer.position"}) {
		t.Fatalf("the registered webhook redacts %v, want the two lines typed", redact)
	}
	run("open the webhook", chromedp.Evaluate(`location.hash = `+strconv.Quote("#/webhooks/"+url.PathEscape(webhookID)), nil),
		chromedp.WaitVisible("#webhook-redact-summary", chromedp.ByQuery))
	if got := text("#webhook-redact-summary"); !strings.Contains(got, "killer.position") {
		t.Fatalf("the webhook summary's redaction reads %q", got)
	}
	run("edit the redaction", setValue("#edit-webhook-redact", "position"),
		chromedp.Click("#edit-webhook-submit", chromedp.ByQuery))
	waitJS("the summary shows the edit", `!document.querySelector("#webhook-redact-summary").textContent.includes("killer")`)
	if _, redact := redactOf(); !reflect.DeepEqual(redact, []string{"position"}) {
		t.Fatalf("after the edit the webhook redacts %v, want position alone", redact)
	}
}
