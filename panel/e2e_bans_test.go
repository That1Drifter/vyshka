package panel_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// The end-to-end test of issue #80's panel half: the installation ban list
// (protocol section 13) placed, refused as a conflict, lifted behind its
// confirmation, and shown in every state and page; a server page saying
// whether its plugin enforces the list and whether it has caught up; the
// Bans section of a player's profile; and tokens without the grants seeing
// the hub's refusals where the other views show theirs.

// panelBan mirrors a ban as the Admin API reports it (section 13.1), the
// members the test checks the page against.
type panelBan struct {
	ID     string `json:"id"`
	Player struct {
		Platform string `json:"platform"`
		ID       string `json:"id"`
	} `json:"player"`
	Reason    string  `json:"reason"`
	Name      string  `json:"name"`
	ServerID  *string `json:"serverId"`
	ExpiresAt *string `json:"expiresAt"`
	State     string  `json:"state"`
}

// bansManifest is the management manifest with the bans capability declared
// (section 6.7), which is what makes a server's plugin one that enforces the
// list.
func bansManifest() map[string]any {
	manifest := map[string]any{}
	for key, value := range manageManifest {
		manifest[key] = value
	}
	manifest["capabilities"] = []string{"bans"}
	return manifest
}

func TestPanelBansEndToEnd(t *testing.T) {
	h := newE2EHarness(t, "")
	hubURL := h.URL()
	run, setValue, selectOption, waitJS, text, evalString := h.run, h.setValue, h.selectOption, h.waitJS, h.text, h.evalString

	capable := createServer(t, hubURL, "Ban server")
	plain := createServer(t, hubURL, "Plain server")
	plugin := newFakePlugin(t, hubURL, capable.Enrollment.Token)
	plugin.queue("manifest.publish", bansManifest())
	go plugin.run(h.Ctx)
	if err := plugin.awaitAcked(h.Ctx); err != nil {
		t.Fatalf("the plugin's manifest was never acked: %v", err)
	}

	const anna, bob, charlie, dave = "76561198000000031", "76561198000000032", "76561198000000033", "76561198000000034"
	bansOf := func(query url.Values) []panelBan {
		t.Helper()
		status, body := adminRequest(t, http.MethodGet, hubURL+"/api/v1/bans?"+query.Encode(), nil)
		if status != http.StatusOK {
			t.Fatalf("list bans: status %d body %s", status, body)
		}
		var page struct {
			Bans []panelBan `json:"bans"`
		}
		_ = json.Unmarshal(body, &page)
		return page.Bans
	}
	activeOf := func(playerID string) []panelBan {
		t.Helper()
		return bansOf(url.Values{"platform": {"steam"}, "playerId": {playerID}})
	}
	expiresIn := func(ban panelBan, want time.Duration) {
		t.Helper()
		if ban.ExpiresAt == nil {
			t.Fatalf("ban %s is permanent, want one ending in %s", ban.ID, want)
		}
		at, err := time.Parse(time.RFC3339Nano, *ban.ExpiresAt)
		if err != nil {
			t.Fatalf("ban %s expiresAt %q: %v", ban.ID, *ban.ExpiresAt, err)
		}
		if left := time.Until(at); left < want-5*time.Minute || left > want+time.Minute {
			t.Fatalf("ban %s ends in %s, want about %s", ban.ID, left, want)
		}
	}
	rows := func(table string) string {
		return evalString(`String(document.querySelectorAll("#` + table + ` tbody tr").length)`)
	}

	// 1. The Bans section of the nav, on an installation with no ban yet.
	run("open the panel", chromedp.Navigate(hubURL+"/"), chromedp.WaitVisible("#login", chromedp.ByQuery))
	run("sign in", chromedp.SendKeys("#token", e2eAdminToken, chromedp.ByQuery),
		chromedp.Click("#sign-in", chromedp.ByQuery), chromedp.WaitVisible("#servers", chromedp.ByQuery))
	run("open the bans view", chromedp.Click("#nav a[data-nav=bans]", chromedp.ByQuery),
		chromedp.WaitVisible("#ban-form", chromedp.ByQuery), chromedp.WaitVisible("#bans-empty", chromedp.ByQuery))
	if got := h.attribute("#nav a[data-nav=bans]", "aria-current"); got != "page" {
		t.Fatalf("the bans link carries aria-current %q, want page", got)
	}
	if got := text("#bans-revision"); got != "List revision 0" {
		t.Fatalf("the revision reads %q before any ban, want List revision 0", got)
	}

	// 2. A ban placed through the form, for seven days, with its provenance,
	// and a reason whose markup must stay text.
	run("ban anna", setValue("#ban-form-player-id", anna),
		setValue("#ban-form-reason", "Speed hack <b>confirmed</b>"), setValue("#ban-form-name", "Anna"),
		selectOption("#ban-form-duration", "604800"), selectOption("#ban-form-server", capable.Server.ID),
		chromedp.Click("#ban-form-submit", chromedp.ByQuery), chromedp.WaitVisible("#ban-form-placed", chromedp.ByQuery))
	waitJS("the ban is listed", `document.querySelectorAll("#bans tbody tr").length === 1`)
	placed := activeOf(anna)
	if len(placed) != 1 || placed[0].Reason != "Speed hack <b>confirmed</b>" || placed[0].Name != "Anna" ||
		placed[0].ServerID == nil || *placed[0].ServerID != capable.Server.ID {
		t.Fatalf("the hub holds %+v for anna, want the ban as typed with its provenance", placed)
	}
	expiresIn(placed[0], 7*24*time.Hour)
	annaBan := placed[0].ID
	row := text(`#bans tr[data-ban-id="` + annaBan + `"]`)
	for _, want := range []string{"steam:" + anna, "Anna", "Speed hack <b>confirmed</b>", "Ban server", "active", "bootstrap"} {
		if !strings.Contains(row, want) {
			t.Fatalf("the ban's row reads %q, want %q in it", row, want)
		}
	}
	if got := evalString(`String(document.querySelectorAll("#bans b").length)`); got != "0" {
		t.Fatal("a ban reason's markup reached the page as markup")
	}
	if got := text("#bans-revision"); got != "List revision 1" {
		t.Fatalf("the revision reads %q after one ban, want List revision 1", got)
	}
	if got := h.attribute(`#bans tr[data-ban-id="`+annaBan+`"] a[data-profile]`, "href"); got != "#/players/steam/"+anna {
		t.Fatalf("the player cell links %q, want the profile", got)
	}

	// 3. A second ban of the same identity is the hub's conflict, named.
	run("ban anna again", setValue("#ban-form-player-id", anna), setValue("#ban-form-reason", "Again"),
		chromedp.Click("#ban-form-submit", chromedp.ByQuery), chromedp.WaitVisible("#ban-form-conflict", chromedp.ByQuery))
	if got := text("#ban-form-conflict"); !strings.Contains(got, "already banned") || !strings.Contains(got, annaBan) {
		t.Fatalf("the conflict reads %q, want it to say anna is banned and name ban %s", got, annaBan)
	}
	waitJS("the conflict says what the standing ban is for", `(document.querySelector("#ban-form-conflict .conflict-detail") || {textContent: ""}).textContent.includes("Speed hack")`)
	if len(activeOf(anna)) != 1 {
		t.Fatal("the conflicting ban reached the list")
	}
	if got := evalString(`document.querySelector("#ban-form-placed").hidden ? "hidden" : "shown"`); got != "hidden" {
		t.Fatal("the refused ban still shows the previous ban's success notice")
	}

	// 4. A permanent ban, and one for a custom number of days.
	run("ban bob permanently", setValue("#ban-form-player-id", bob), setValue("#ban-form-reason", "Duping"),
		selectOption("#ban-form-duration", ""), selectOption("#ban-form-server", ""),
		chromedp.Click("#ban-form-submit", chromedp.ByQuery))
	waitJS("bob is listed", `document.querySelectorAll("#bans tbody tr").length === 2`)
	if got := activeOf(bob); len(got) != 1 || got[0].ExpiresAt != nil || got[0].ServerID != nil {
		t.Fatalf("the hub holds %+v for bob, want one permanent ban with no provenance", got)
	}
	bobBan := activeOf(bob)[0].ID
	if got := text(`#bans tr[data-ban-id="` + bobBan + `"]`); !strings.Contains(got, "permanent") {
		t.Fatalf("bob's row reads %q, want it permanent", got)
	}
	// The custom amount is required once custom is chosen: the negative
	// control for the form's own check.
	run("ban charlie with no amount", setValue("#ban-form-player-id", charlie), setValue("#ban-form-reason", "Griefing"),
		selectOption("#ban-form-duration", "custom"), chromedp.Click("#ban-form-submit", chromedp.ByQuery),
		chromedp.WaitVisible("#ban-form-error", chromedp.ByQuery))
	if len(activeOf(charlie)) != 0 {
		t.Fatal("a custom duration with no amount reached the hub")
	}
	run("ban charlie for two days", setValue("#ban-form-duration-amount", "2"), selectOption("#ban-form-duration-unit", "86400"),
		chromedp.Click("#ban-form-submit", chromedp.ByQuery))
	waitJS("charlie is listed", `document.querySelectorAll("#bans tbody tr").length === 3`)
	if got := activeOf(charlie); len(got) != 1 {
		t.Fatalf("the hub holds %+v for charlie, want one ban", got)
	} else {
		expiresIn(got[0], 48*time.Hour)
	}

	// 5. Lifting asks for a second click, and Cancel takes the ask back.
	run("ask to lift anna's ban", chromedp.Click(`button[data-lift-ban="`+annaBan+`"]`, chromedp.ByQuery),
		chromedp.WaitVisible(`button[data-lift-confirm="`+annaBan+`"]`, chromedp.ByQuery))
	run("cancel the lift", chromedp.Click(`button[data-lift-cancel="`+annaBan+`"]`, chromedp.ByQuery),
		chromedp.WaitVisible(`button[data-lift-ban="`+annaBan+`"]`, chromedp.ByQuery))
	if len(activeOf(anna)) != 1 {
		t.Fatal("a cancelled lift reached the hub")
	}
	run("lift anna's ban", chromedp.Click(`button[data-lift-ban="`+annaBan+`"]`, chromedp.ByQuery),
		chromedp.Click(`button[data-lift-confirm="`+annaBan+`"]`, chromedp.ByQuery))
	waitJS("the lifted ban leaves the active list", `!document.querySelector('#bans tr[data-ban-id="`+annaBan+`"]') && document.querySelectorAll("#bans tbody tr").length === 2`)
	if len(activeOf(anna)) != 0 {
		t.Fatal("the confirmed lift did not reach the hub")
	}
	if got := text("#bans-revision"); got != "List revision 4" {
		t.Fatalf("the revision reads %q after three bans and a lift, want List revision 4", got)
	}

	// 6. Every state, behind the toggle, which lives in the route.
	run("include lifted and expired bans", chromedp.Click("#bans-all", chromedp.ByQuery))
	waitJS("the lifted ban is back in the list", `location.hash.includes("state=all") && document.querySelectorAll("#bans tbody tr").length === 3`)
	if got := h.attribute(`#bans tr[data-ban-id="`+annaBan+`"]`, "data-ban-state"); got != "lifted" {
		t.Fatalf("anna's ban reads state %q, want lifted", got)
	}
	if got := text(`#bans tr[data-ban-id="` + annaBan + `"]`); !strings.Contains(got, "lifted") || !strings.Contains(got, "by bootstrap") {
		t.Fatalf("the lifted row reads %q, want the state and who lifted it", got)
	}
	if got := evalString(`document.querySelector('#bans tr[data-ban-id="` + annaBan + `"]').classList.contains("dim") ? "dim" : "plain"`); got != "dim" {
		t.Fatal("the lifted row is not dimmed")
	}
	if got := evalString(`document.querySelector('#bans tr[data-ban-id="` + annaBan + `"] [data-lift-ban]') ? "lift" : "none"`); got != "none" {
		t.Fatal("a lifted ban offers a Lift")
	}

	// 7. The server page: a server whose plugin enforces the list, before its
	// first report, caught up, and behind; and one that does not enforce it.
	serverHash := "#/servers/" + capable.Server.ID
	run("open the capable server", chromedp.Evaluate(`location.hash = `+strconv.Quote(serverHash), nil),
		chromedp.WaitVisible("#server-bans", chromedp.ByQuery))
	if got := text("#server-bans"); !strings.Contains(got, "no report yet") {
		t.Fatalf("the capable server's bans line reads %q before a report, want no report yet", got)
	}
	waitJS("the unreported server names the list's revision", `(document.querySelector("#server-bans-sync") || {dataset: {}}).dataset.sync === "unreported"`)
	plugin.queue("bans.applied", map[string]any{"revision": 4})
	h.waitUntil("the hub recorded the report", 30*time.Second, func() bool {
		status, body := adminRequest(t, http.MethodGet, hubURL+"/api/v1/servers/"+capable.Server.ID, nil)
		var record struct {
			Bans struct {
				AppliedRevision *int64 `json:"appliedRevision"`
			} `json:"bans"`
		}
		_ = json.Unmarshal(body, &record)
		return status == http.StatusOK && record.Bans.AppliedRevision != nil && *record.Bans.AppliedRevision == 4
	})
	run("reload the server page", chromedp.Reload(), chromedp.WaitVisible("#server-bans-sync", chromedp.ByQuery))
	if got := h.attribute("#server-bans-sync", "data-sync"); got != "current" {
		t.Fatalf("a server enforcing the current revision reads %q, want current", got)
	}
	if got := text("#server-bans"); !strings.Contains(got, "enforcing revision 4") || !strings.Contains(got, "up to date") {
		t.Fatalf("the caught-up bans line reads %q", got)
	}
	if status, body := adminRequest(t, http.MethodPost, hubURL+"/api/v1/bans", map[string]any{
		"player": map[string]any{"platform": "steam", "id": dave}, "reason": "Exploit",
	}); status != http.StatusCreated {
		t.Fatalf("ban dave: status %d body %s", status, body)
	}
	run("reload the server page behind", chromedp.Reload(), chromedp.WaitVisible("#server-bans-sync", chromedp.ByQuery))
	if got := h.attribute("#server-bans-sync", "data-sync"); got != "behind" {
		t.Fatalf("a server a revision behind reads %q, want behind", got)
	}
	if got := text("#server-bans"); !strings.Contains(got, "revision 5") {
		t.Fatalf("the behind bans line reads %q, want the list's revision 5", got)
	}
	run("open the plain server", chromedp.Evaluate(`location.hash = `+strconv.Quote("#/servers/"+plain.Server.ID), nil),
		chromedp.WaitVisible("#no-manifest", chromedp.ByQuery))
	if got := text("#server-bans"); !strings.Contains(got, "not supported by this server") {
		t.Fatalf("the plain server's bans line reads %q, want not supported", got)
	}
	if got := evalString(`document.querySelector("#server-bans-sync") ? "compared" : "none"`); got != "none" {
		t.Fatal("a server that does not enforce the list is compared against it")
	}

	// 8. The profile: every ban of the identity, a ban placed from there, its
	// conflict, and a lift.
	profileHash := "#/players/steam/" + anna
	run("open anna's profile", chromedp.Evaluate(`location.hash = `+strconv.Quote(profileHash), nil),
		chromedp.WaitVisible("#player-ban-form", chromedp.ByQuery))
	waitJS("the profile lists her lifted ban", `document.querySelectorAll("#player-bans tbody tr").length === 1`)
	if got := h.attribute(`#player-bans tr[data-ban-id="`+annaBan+`"]`, "data-ban-state"); got != "lifted" {
		t.Fatalf("the profile shows her ban as %q, want lifted", got)
	}
	if got := evalString(`document.querySelector("#player-ban-form-player-id") ? "asks" : "fixed"`); got != "fixed" {
		t.Fatal("the profile's ban form asks for the identity it is about")
	}
	run("ban her from the profile", setValue("#player-ban-form-reason", "Back again"),
		chromedp.Click("#player-ban-form-submit", chromedp.ByQuery))
	waitJS("the profile lists both bans", `document.querySelectorAll("#player-bans tbody tr").length === 2`)
	again := activeOf(anna)
	if len(again) != 1 || again[0].Reason != "Back again" || again[0].ExpiresAt != nil {
		t.Fatalf("the hub holds %+v for anna, want the permanent ban placed from her profile", again)
	}
	run("ban her twice from the profile", setValue("#player-ban-form-reason", "Once more"),
		chromedp.Click("#player-ban-form-submit", chromedp.ByQuery), chromedp.WaitVisible("#player-ban-form-conflict", chromedp.ByQuery))
	if got := text("#player-ban-form-conflict"); !strings.Contains(got, again[0].ID) {
		t.Fatalf("the profile's conflict reads %q, want it to name ban %s", got, again[0].ID)
	}
	run("lift it from the profile", chromedp.Click(`#player-bans button[data-lift-ban="`+again[0].ID+`"]`, chromedp.ByQuery),
		chromedp.Click(`#player-bans button[data-lift-confirm="`+again[0].ID+`"]`, chromedp.ByQuery))
	waitJS("the profile shows the lift", `(document.querySelector('#player-bans tr[data-ban-id="`+again[0].ID+`"]') || {dataset: {}}).dataset.banState === "lifted"`)
	if len(activeOf(anna)) != 0 {
		t.Fatal("the profile's lift did not reach the hub")
	}

	// 9. Paging: more bans than one page, walked behind "Load older" with no
	// ban shown twice.
	for i := 0; i < 101; i++ {
		if status, body := adminRequest(t, http.MethodPost, hubURL+"/api/v1/bans", map[string]any{
			"player": map[string]any{"platform": "steam", "id": "7656119900000" + strconv.Itoa(1000+i)}, "reason": "Bulk " + strconv.Itoa(i),
		}); status != http.StatusCreated {
			t.Fatalf("bulk ban %d: status %d body %s", i, status, body)
		}
	}
	total := len(bansOf(url.Values{"state": {"all"}, "limit": {"500"}}))
	if total != 106 {
		t.Fatalf("the hub holds %d bans in every state, want 106", total)
	}
	run("open every ban", chromedp.Evaluate(`location.hash = "#/bans?state=all"`, nil),
		chromedp.WaitVisible("#bans-older", chromedp.ByQuery))
	if got := rows("bans"); got != "100" {
		t.Fatalf("the first page holds %s bans, want 100", got)
	}
	run("load older bans", chromedp.Click("#bans-older", chromedp.ByQuery))
	waitJS("the walk reaches every ban", `document.querySelectorAll("#bans tbody tr").length === 106`)
	if got := evalString(`String(new Set(Array.from(document.querySelectorAll("#bans tbody tr")).map(r => r.dataset.banId)).size)`); got != "106" {
		t.Fatalf("the walk shows %s distinct bans, want 106", got)
	}
	if got := evalString(`document.querySelector("#bans-older").hidden ? "hidden" : "shown"`); got != "hidden" {
		t.Fatal("a finished walk still offers older bans")
	}

	// 10. Narrowed tokens: one that reads the list but cannot change it, and
	// one that cannot read it.
	mint := func(name string, scopes ...string) string {
		t.Helper()
		status, body := adminRequest(t, http.MethodPost, hubURL+"/api/v1/tokens", map[string]any{"name": name, "scopes": scopes})
		if status != http.StatusCreated {
			t.Fatalf("mint %s: status %d body %s", name, status, body)
		}
		var minted struct {
			Secret string `json:"secret"`
		}
		_ = json.Unmarshal(body, &minted)
		return minted.Secret
	}
	readOnly := mint("bans reader", "servers:read", "bans:read")
	noBans := mint("no bans", "servers:read", "events:read")

	run("sign out", chromedp.Click("#sign-out", chromedp.ByQuery), chromedp.WaitVisible("#login", chromedp.ByQuery))
	run("sign in as the reader", chromedp.SendKeys("#token", readOnly, chromedp.ByQuery),
		chromedp.Click("#sign-in", chromedp.ByQuery), chromedp.WaitVisible("#ban-form", chromedp.ByQuery))
	waitJS("the reader sees the list", `document.querySelectorAll("#bans tbody tr").length === 100`)
	run("try to ban as the reader", setValue("#ban-form-player-id", "76561198000000099"), setValue("#ban-form-reason", "Not allowed"),
		chromedp.Click("#ban-form-submit", chromedp.ByQuery), chromedp.WaitVisible("#ban-form-error", chromedp.ByQuery))
	if got := text("#ban-form-error"); !strings.Contains(got, "forbidden") {
		t.Fatalf("a refused ban reads %q, want the hub's forbidden", got)
	}
	if len(activeOf("76561198000000099")) != 0 {
		t.Fatal("a token without bans:manage placed a ban")
	}
	victim := evalString(`document.querySelector('#bans tbody tr[data-ban-state="active"]').dataset.banId`)
	victimPlayer := evalString(`document.querySelector('#bans tbody tr[data-ban-state="active"] a[data-profile]').dataset.profile.slice("steam:".length)`)
	run("try to lift as the reader", chromedp.Click(`button[data-lift-ban="`+victim+`"]`, chromedp.ByQuery),
		chromedp.Click(`button[data-lift-confirm="`+victim+`"]`, chromedp.ByQuery), chromedp.WaitVisible("#bans-error", chromedp.ByQuery))
	if got := text("#bans-error"); !strings.Contains(got, "forbidden") {
		t.Fatalf("a refused lift reads %q, want the hub's forbidden", got)
	}
	if got := rows("bans"); got != "100" {
		t.Fatalf("a refused lift left %s rows, want the list untouched", got)
	}
	if got := activeOf(victimPlayer); len(got) != 1 || got[0].ID != victim {
		t.Fatalf("after a refused lift the hub holds %+v for %s, want ban %s still active", got, victimPlayer, victim)
	}

	run("sign out as the reader", chromedp.Click("#sign-out", chromedp.ByQuery), chromedp.WaitVisible("#login", chromedp.ByQuery))
	run("sign in without bans", chromedp.SendKeys("#token", noBans, chromedp.ByQuery),
		chromedp.Click("#sign-in", chromedp.ByQuery), chromedp.WaitVisible("#forbidden", chromedp.ByQuery))
	if got := text("#forbidden"); !strings.Contains(got, "bans:read") {
		t.Fatalf("the refused view reads %q, want the scope it needs", got)
	}
	run("open anna's profile without bans", chromedp.Evaluate(`location.hash = `+strconv.Quote(profileHash), nil),
		chromedp.WaitVisible("#player-bans-forbidden", chromedp.ByQuery))
	if got := text("#player-bans-forbidden"); !strings.Contains(got, "bans:read") {
		t.Fatalf("the profile's bans notice reads %q, want the grant it needs", got)
	}
	if got := evalString(`document.querySelector("#player-ban-form") ? "form" : "none"`); got != "none" {
		t.Fatal("a token that cannot read the list is offered the ban form")
	}
	run("open the capable server without bans", chromedp.Evaluate(`location.hash = `+strconv.Quote(serverHash), nil),
		chromedp.WaitVisible("#server-bans", chromedp.ByQuery))
	if got := text("#server-bans"); !strings.Contains(got, "enforcing revision 4") {
		t.Fatalf("the bans line reads %q without bans:read, want the server's own report", got)
	}
	// The comparison is one read; the refusal must leave it out rather than
	// draw anything. The world card's read goes out beside it, so its answer
	// landing is the sign the page's reads have come back.
	waitJS("the page's reads have come back", `!document.querySelector("#world-body").textContent.includes("Loading")`)
	time.Sleep(500 * time.Millisecond)
	if got := evalString(`document.querySelector("#server-bans-sync") ? "compared" : "none"`); got != "none" {
		t.Fatal("a token without bans:read got a comparison against the list")
	}
}
