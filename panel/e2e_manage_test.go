package panel_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// The end-to-end test of issue #64: the management views driven in a real
// browser against a real hub. What a unit test of either side cannot grade is
// exactly what these views are: a one-time secret that must survive a refresh
// tick, a destructive control that must do nothing without its tick, a scope
// bundle expanded from the hub's own stored manifests, a paused webhook that
// must hold its deliveries while the receiver counts, and a scope refusal that
// must be one notice rather than six.
//
// Every claim the design note or panel/README.md makes about a guard gets its
// negative control here: the revoke tick, the delete tick, the two-click token
// revoke, the pause holding deliveries, and the forbidden notice.
//
// The browser, the hub, the fake plugin, and the step helpers come from
// e2e_harness_test.go, which the issue #13 test uses unchanged.

// manageManifest declares two action namespaces and two KV namespaces,
// because the role bundles are expanded from the hub's stored manifests and a
// single namespace would not show that the expansion is a sorted set rather
// than one lucky string.
var manageManifest = map[string]any{
	"game":             "e2e",
	"plugin":           map[string]any{"name": "e2e-plugin", "version": "0.1.0"},
	"manifestRevision": 1,
	"kvNamespaces":     []string{"example-mod", "zeta-store"},
	"actions": []map[string]any{
		{
			"code": "example-mod.heal", "name": "Heal player", "context": "player",
			"namespace": "example-mod", "danger": "warning",
			"params": map[string]any{"type": "object", "properties": map[string]any{
				"amount": map[string]any{"type": "integer", "minimum": 1},
			}},
		},
		{
			"code": "arena.start", "name": "Start the arena", "context": "server",
			"namespace": "arena",
			"params":    map[string]any{"type": "object", "properties": map[string]any{}},
		},
	},
}

// ---------------------------------------------------------------------------
// The webhook receiver: a target that can be told to fail, counts what it was
// sent, and remembers which attempt of which delivery each request was.

type receivedDelivery struct {
	Path       string
	DeliveryID string
	Attempt    int
}

type webhookReceiver struct {
	server *httptest.Server

	mu       sync.Mutex
	status   int
	received []receivedDelivery
}

func newWebhookReceiver(t *testing.T) *webhookReceiver {
	t.Helper()
	receiver := &webhookReceiver{status: http.StatusInternalServerError}
	receiver.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var payload struct {
			DeliveryID string `json:"deliveryId"`
		}
		_ = json.Unmarshal(body, &payload)
		attempt, _ := strconv.Atoi(r.Header.Get("X-Vyshka-Attempt"))
		receiver.mu.Lock()
		receiver.received = append(receiver.received, receivedDelivery{
			Path: r.URL.Path, DeliveryID: payload.DeliveryID, Attempt: attempt,
		})
		status := receiver.status
		receiver.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(receiver.server.Close)
	return receiver
}

func (r *webhookReceiver) answerWith(status int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = status
}

func (r *webhookReceiver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.received)
}

func (r *webhookReceiver) countOn(path string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	total := 0
	for _, one := range r.received {
		if one.Path == path {
			total++
		}
	}
	return total
}

// attemptsOf is every attempt number this receiver was sent for one delivery,
// in the order it received them.
func (r *webhookReceiver) attemptsOf(deliveryID string) []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	var attempts []int
	for _, one := range r.received {
		if one.DeliveryID == deliveryID {
			attempts = append(attempts, one.Attempt)
		}
	}
	return attempts
}

// ---------------------------------------------------------------------------
// The Admin API, as the test's own second opinion on what the page claims.

func adminRequest(t *testing.T, method, url string, body any) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode %s %s: %v", method, url, err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, url, err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+e2eAdminToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		t.Fatalf("read %s %s: %v", method, url, err)
	}
	return response.StatusCode, answer
}

func adminGet(t *testing.T, url string, out any) {
	t.Helper()
	status, body := adminRequest(t, http.MethodGet, url, nil)
	if status != http.StatusOK {
		t.Fatalf("GET %s: status %d body %s", url, status, body)
	}
	if err := json.Unmarshal(body, out); err != nil {
		t.Fatalf("decode GET %s: %v (body %s)", url, err, body)
	}
}

type manageServerRecord struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	CredentialState string `json:"credentialState"`
	Plugin          *struct {
		Name string `json:"name"`
	} `json:"plugin"`
}

type manageTokenRecord struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Scopes    []string   `json:"scopes"`
	ExpiresAt *time.Time `json:"expiresAt"`
	RevokedAt *time.Time `json:"revokedAt"`
}

type manageWebhookRecord struct {
	ID       string     `json:"id"`
	URL      string     `json:"url"`
	Events   []string   `json:"events"`
	Template string     `json:"template"`
	PausedAt *time.Time `json:"pausedAt"`
}

type manageDeliveryRecord struct {
	ID          string     `json:"id"`
	Type        string     `json:"type"`
	State       string     `json:"state"`
	Attempts    int        `json:"attempts"`
	DeliveredAt *time.Time `json:"deliveredAt"`
}

func manageServers(t *testing.T, hubURL string) []manageServerRecord {
	t.Helper()
	var answer struct {
		Servers []manageServerRecord `json:"servers"`
	}
	adminGet(t, hubURL+"/api/v1/servers", &answer)
	return answer.Servers
}

func manageServer(t *testing.T, hubURL, serverID string) manageServerRecord {
	t.Helper()
	var record manageServerRecord
	adminGet(t, hubURL+"/api/v1/servers/"+serverID, &record)
	return record
}

func manageTokens(t *testing.T, hubURL string) []manageTokenRecord {
	t.Helper()
	var answer struct {
		Tokens []manageTokenRecord `json:"tokens"`
	}
	adminGet(t, hubURL+"/api/v1/tokens", &answer)
	return answer.Tokens
}

func manageWebhooks(t *testing.T, hubURL string) []manageWebhookRecord {
	t.Helper()
	var answer struct {
		Webhooks []manageWebhookRecord `json:"webhooks"`
	}
	adminGet(t, hubURL+"/api/v1/webhooks", &answer)
	return answer.Webhooks
}

func manageDeliveries(t *testing.T, hubURL, webhookID string) []manageDeliveryRecord {
	t.Helper()
	var answer struct {
		Deliveries []manageDeliveryRecord `json:"deliveries"`
	}
	adminGet(t, hubURL+"/api/v1/webhooks/"+webhookID+"/deliveries?limit=500", &answer)
	return answer.Deliveries
}

func TestPanelManagementEndToEnd(t *testing.T) {
	h := newE2EHarness(t, "")
	hubURL := h.URL()
	run, setValue, selectOption, waitJS := h.run, h.setValue, h.selectOption, h.waitJS
	text, attribute, evalString := h.text, h.attribute, h.evalString

	receiver := newWebhookReceiver(t)

	// A server with a plugin and a manifest (the bundles and the pins read
	// it), and a second server nobody has enrolled, which is the case the
	// bundle expansion must tolerate: GET manifest answers 404 and narrows
	// nothing.
	managed := createServer(t, hubURL, "Managed server")
	plugin := newFakePlugin(t, hubURL, managed.Enrollment.Token)
	plugin.queue("manifest.publish", manageManifest)
	go plugin.run(h.Ctx)
	if err := plugin.awaitAcked(h.Ctx); err != nil {
		t.Fatalf("plugin never got its manifest acked: %v", err)
	}
	createServer(t, hubURL, "Unenrolled server")

	// The key/value fixture, written over the Admin API like any other
	// client. Four live keys in one namespace, three of them sharing a
	// prefix; one key in a second namespace with the shortest TTL the hub
	// accepts, so that by the time the view is opened, minutes from now, the
	// namespace holds nothing live and must not be listed at all.
	for key, value := range map[string]any{
		"balance.alpha": map[string]any{"coins": 42},
		"balance.beta":  map[string]any{"coins": 7},
		"balance.gamma": map[string]any{"coins": 0},
		"profile.alice": map[string]any{"name": "Alice"},
	} {
		if status, body := adminRequest(t, http.MethodPut,
			hubURL+"/api/v1/kv/example-mod/"+key, map[string]any{"value": value}); status != http.StatusOK {
			t.Fatalf("write kv key %s: status %d body %s", key, status, body)
		}
	}
	if status, body := adminRequest(t, http.MethodPut, hubURL+"/api/v1/kv/zeta-store/scratch.temp",
		map[string]any{"value": map[string]any{"gone": true}, "ttlSeconds": 1}); status != http.StatusOK {
		t.Fatalf("write the expiring kv key: status %d body %s", status, body)
	}

	// 1. Sign in. The nav is the shell's, hidden until a token is held, and
	// the link for the view being shown says so.
	run("open the panel", chromedp.Navigate(hubURL+"/"), chromedp.WaitVisible("#login", chromedp.ByQuery))
	if got := evalString(`document.querySelector("#nav").hidden ? "hidden" : "shown"`); got != "hidden" {
		t.Fatalf("the nav is %s on the sign-in page, want hidden: every link behind it needs a token", got)
	}
	run("sign in", chromedp.SendKeys("#token", e2eAdminToken, chromedp.ByQuery),
		chromedp.Click("#sign-in", chromedp.ByQuery), chromedp.WaitVisible("#servers", chromedp.ByQuery))
	if got := evalString(`Array.from(document.querySelectorAll("#nav a[data-nav]")).map(a => a.dataset.nav).join(",")`); got != "servers,players,tokens,webhooks,audit,kv" {
		t.Fatalf("nav links = %q, want the six sections", got)
	}
	if got := attribute("#nav a[data-nav=servers]", "aria-current"); got != "page" {
		t.Fatalf("the servers link carries aria-current %q, want page", got)
	}
	if got := evalString(`String(document.querySelectorAll("#nav a[aria-current]").length)`); got != "1" {
		t.Fatalf("%s nav links carry aria-current, want exactly the current one", got)
	}

	// 2. Registering a server hands back a one-time enrollment token. The
	// list refreshes every five seconds and the form is not redrawn with it,
	// because that value exists nowhere else: the wait here crosses a tick.
	run("register a server",
		chromedp.SendKeys("#server-name", "Browser server", chromedp.ByQuery),
		chromedp.SendKeys("#server-game", "e2e", chromedp.ByQuery),
		chromedp.Click("#register-server-submit", chromedp.ByQuery),
		chromedp.WaitVisible("#enrollment-token-value", chromedp.ByQuery))
	enrollmentToken := text("#enrollment-token-value")
	if !strings.HasPrefix(enrollmentToken, "vye_") {
		t.Fatalf("enrollment token = %q, want the vye_ prefix of protocol section 5.1", enrollmentToken)
	}
	if got := text("#enrollment-token"); !strings.Contains(got, "Expires") {
		t.Fatalf("the enrollment token card = %q, want its expiry", got)
	}
	var registered manageServerRecord
	for _, record := range manageServers(t, hubURL) {
		if record.Name == "Browser server" {
			registered = record
		}
	}
	if registered.ID == "" {
		t.Fatal("the registered server is not in the hub's server list")
	}
	registeredRow := `tr[data-server-id="` + registered.ID + `"]`
	time.Sleep(6 * time.Second)
	if got := text("#enrollment-token-value"); got != enrollmentToken {
		t.Fatalf("the one-time token became %q across a refresh tick; a redraw took the only copy of it", got)
	}
	if got := evalString(`document.querySelector(` + strconv.Quote(registeredRow) + `) ? "row" : "none"`); got != "row" {
		t.Fatalf("the registered server has no row after a refresh tick (%s)", got)
	}

	// The token is what the plugin enrolls with, and the row says so once
	// the list next refreshes.
	enrolled := newFakePlugin(t, hubURL, enrollmentToken)
	go enrolled.run(h.Ctx)
	if enrolled.serverID != registered.ID {
		t.Fatalf("the plugin enrolled onto %s, want the server just registered (%s)", enrolled.serverID, registered.ID)
	}
	waitJS("the registered server's row shows its plugin",
		`(function(){const r=document.querySelector(`+strconv.Quote(registeredRow)+`);`+
			`return r && r.textContent.includes("e2e-plugin") && r.textContent.includes("active")})()`)

	// 3. The server page: a fresh enrollment token, then revocation, which
	// is the destructive control the README says is behind an explicit tick.
	run("open the registered server",
		chromedp.Evaluate(`location.hash = `+strconv.Quote("#/servers/"+registered.ID), nil),
		chromedp.WaitVisible("#credentials", chromedp.ByQuery),
		chromedp.Click("#new-enrollment-token", chromedp.ByQuery),
		chromedp.WaitVisible("#enrollment-token-value", chromedp.ByQuery))
	waitJS("a fresh enrollment token replaced the one on the page",
		`document.querySelector("#enrollment-token-value").textContent !== `+strconv.Quote(enrollmentToken))
	if got := text("#enrollment-token-value"); !strings.HasPrefix(got, "vye_") {
		t.Fatalf("fresh enrollment token = %q, want the vye_ prefix", got)
	}

	// Negative control: the same click, without the tick, must reach the hub
	// with nothing at all.
	run("revoke without confirming", chromedp.Click("#revoke-credentials", chromedp.ByQuery),
		chromedp.WaitVisible("#credentials-error", chromedp.ByQuery))
	if got := text("#credentials-error"); !strings.Contains(got, "confirm") {
		t.Fatalf("unconfirmed revoke said %q, want the confirmation refusal", got)
	}
	if got := manageServer(t, hubURL, registered.ID).CredentialState; got != "active" {
		t.Fatalf("credentialState = %q after an unconfirmed revoke, want active: the tick is the guard", got)
	}
	run("revoke with the tick", chromedp.Click("#revoke-credentials-confirm", chromedp.ByQuery),
		chromedp.Click("#revoke-credentials", chromedp.ByQuery))
	waitJS("the credential summary says revoked",
		`(function(){const c=document.querySelector("#credentials");return c && c.textContent.includes("revoked")})()`)
	if got := manageServer(t, hubURL, registered.ID).CredentialState; got != "revoked" {
		t.Fatalf("credentialState = %q after a confirmed revoke, want revoked", got)
	}
	// What revocation means on the other side of the wire: the plugin's next
	// poll is refused, which is the whole point of the control.
	h.waitUntil("the revoked plugin's poll is refused", 30*time.Second, func() bool {
		return enrolled.unauthorizedPolls() > 0
	})

	// 4. Pins, on the server that has a manifest. They are this browser's,
	// not the hub's, so the proof they stick is a reload.
	listPin := `div[data-action-row="example-mod.heal"] button[data-pin]`
	pinnedPin := `div[data-pinned-row="example-mod.heal"] button[data-pin]`
	run("open the managed server",
		chromedp.Evaluate(`location.hash = `+strconv.Quote("#/servers/"+managed.Server.ID), nil),
		chromedp.WaitVisible(listPin, chromedp.ByQuery))
	if got := evalString(`document.querySelector("#pinned").hidden ? "hidden" : "shown"`); got != "hidden" {
		t.Fatalf("the pinned section is %s with no pins", got)
	}
	run("pin the heal action", chromedp.Click(listPin, chromedp.ByQuery),
		chromedp.WaitVisible(pinnedPin, chromedp.ByQuery))
	if got := attribute(listPin, "aria-pressed"); got != "true" {
		t.Fatalf("the action list's pin toggle reads aria-pressed %q after pinning", got)
	}
	if got := evalString(`document.querySelector('div[data-pinned-row="example-mod.heal"] a[data-action-code]').dataset.pinned`); got != "true" {
		t.Fatalf("the pinned copy of the action is not marked as one (%q)", got)
	}
	run("reload the server page", chromedp.Reload(), chromedp.WaitVisible(pinnedPin, chromedp.ByQuery))
	run("unpin from the pinned row", chromedp.Click(pinnedPin, chromedp.ByQuery))
	waitJS("the pinned section is gone with the last pin",
		`document.querySelector("#pinned").hidden && !document.querySelector('div[data-pinned-row="example-mod.heal"]')`)

	// A pin the manifest no longer declares: written by hand, because that
	// is exactly how it arises, a manifest revision dropping a code a
	// browser still holds.
	run("write a stale pin into localStorage",
		chromedp.Evaluate(`localStorage.setItem("vyshka.pins."+`+strconv.Quote(managed.Server.ID)+
			`, JSON.stringify(["example-mod.gone"]))`, nil),
		chromedp.Reload(),
		chromedp.WaitVisible(`div[data-pin-missing="example-mod.gone"]`, chromedp.ByQuery))
	if got := text(`div[data-pin-missing="example-mod.gone"]`); !strings.Contains(got, "not in manifest revision 1") {
		t.Fatalf("stale pin row = %q, want the manifest revision that dropped it", got)
	}
	run("unpin the stale code", chromedp.Click(`div[data-pin-missing="example-mod.gone"] button[data-pin]`, chromedp.ByQuery))
	waitJS("the stale pin is gone", `document.querySelector("#pinned").hidden`)

	// 5. Tokens. The bundles are expanded from the hub's stored manifests,
	// which is the claim worth grading: a bundle that quietly grants an
	// unnarrowed dispatch, or forgets a namespace, is the failure mode.
	run("open the tokens view", chromedp.Click("#nav a[data-nav=tokens]", chromedp.ByQuery),
		chromedp.WaitVisible("#mint-token", chromedp.ByQuery))
	if got := attribute("#nav a[data-nav=tokens]", "aria-current"); got != "page" {
		t.Fatalf("the tokens link carries aria-current %q, want page", got)
	}
	// Both bundles share the read and dispatch grants; the moderator adds the
	// player notes (protocol section 8.6), the event host the stores.
	sharedScopes := "servers:read\nevents:read\nactions:dispatch:arena.*\nactions:dispatch:example-mod.*"
	moderatorScopes := sharedScopes + "\nnotes:read\nnotes:write"
	eventHostScopes := sharedScopes + "\nkv:rw:example-mod\nkv:rw:zeta-store"
	run("choose the moderator bundle", selectOption("#token-bundle", "moderator"))
	waitJS("the moderator bundle narrowed itself to the declared action namespaces",
		`document.querySelector("#token-scopes").value === `+strconv.Quote(moderatorScopes))
	if got := evalString(`document.querySelector("#dispatch-warning").hidden ? "hidden" : "shown"`); got != "hidden" {
		t.Fatalf("the unnarrowed-dispatch warning is %s on a narrowed bundle", got)
	}
	run("choose the event host bundle", selectOption("#token-bundle", "event-host"))
	waitJS("the event host bundle adds the declared key/value namespaces",
		`document.querySelector("#token-scopes").value === `+strconv.Quote(eventHostScopes))
	run("choose the owner bundle", selectOption("#token-bundle", "owner"))
	waitJS("the owner bundle is the admin scope", `document.querySelector("#token-scopes").value === "admin"`)
	run("choose the moderator bundle again", selectOption("#token-bundle", "moderator"))
	waitJS("the moderator bundle is back",
		`document.querySelector("#token-scopes").value === `+strconv.Quote(moderatorScopes))

	run("mint a moderator token",
		chromedp.SendKeys("#token-name", "moderator", chromedp.ByQuery),
		selectOption("#token-expiry", "2592000"),
		chromedp.Click("#mint-token-submit", chromedp.ByQuery),
		chromedp.WaitVisible("#token-secret-value", chromedp.ByQuery))
	moderatorSecret := text("#token-secret-value")
	if !strings.HasPrefix(moderatorSecret, "vya_") {
		t.Fatalf("minted secret = %q, want the vya_ prefix of protocol section 10.4", moderatorSecret)
	}
	waitJS("the minted token is in the table", `document.querySelectorAll("#tokens tbody tr[data-token-id]").length === 1`)
	moderatorID := evalString(`document.querySelector("#tokens tbody tr[data-token-id]").dataset.tokenId`)
	moderatorRow := `tr[data-token-id="` + moderatorID + `"]`
	if got := attribute(moderatorRow, "data-token-state"); got != "live" {
		t.Fatalf("the minted token's row is %q, want live", got)
	}
	if got := evalString(`Array.from(document.querySelectorAll(` + strconv.Quote(moderatorRow+" .badges .badge") + `)).map(b => b.textContent).join(",")`); got != strings.ReplaceAll(moderatorScopes, "\n", ",") {
		t.Fatalf("the minted token's scope badges = %q, want one per scope minted", got)
	}
	var minted manageTokenRecord
	for _, record := range manageTokens(t, hubURL) {
		if record.ID == moderatorID {
			minted = record
		}
	}
	if minted.ExpiresAt == nil {
		t.Fatal("the minted token never expires, want the 30 day lifetime the form offered")
	}
	if lifetime := time.Until(*minted.ExpiresAt); lifetime < 29*24*time.Hour || lifetime > 31*24*time.Hour {
		t.Fatalf("the minted token expires in %s, want about 30 days", lifetime)
	}

	// A narrowed token sees the views its scopes cover and one notice on the
	// one they do not. Six failed calls would mean six notices; the README
	// promises one.
	run("sign out", chromedp.Click("#sign-out", chromedp.ByQuery), chromedp.WaitVisible("#login", chromedp.ByQuery))
	// The route is still the tokens view, so signing in with a token that
	// cannot read it is exactly the case the notice exists for.
	run("sign in with the minted token",
		chromedp.SendKeys("#token", moderatorSecret, chromedp.ByQuery),
		chromedp.Click("#sign-in", chromedp.ByQuery), chromedp.WaitVisible("#forbidden", chromedp.ByQuery))
	if got := evalString(`String(document.querySelectorAll("#forbidden").length)`); got != "1" {
		t.Fatalf("%s forbidden notices on the tokens view, want exactly one", got)
	}
	if got := text("#forbidden"); !strings.Contains(got, "admin") {
		t.Fatalf("the forbidden notice = %q, want it to name the admin scope", got)
	}
	run("back to the server list on the narrowed token",
		chromedp.Click("#nav a[data-nav=servers]", chromedp.ByQuery),
		chromedp.WaitVisible("#servers", chromedp.ByQuery))
	if got := text(`tr[data-server-id="` + managed.Server.ID + `"]`); !strings.Contains(got, "Managed server") {
		t.Fatalf("the server list on a servers:read token = %q", got)
	}

	run("sign out again", chromedp.Click("#sign-out", chromedp.ByQuery), chromedp.WaitVisible("#login", chromedp.ByQuery))
	run("sign back in with the bootstrap token",
		chromedp.SendKeys("#token", e2eAdminToken, chromedp.ByQuery),
		chromedp.Click("#sign-in", chromedp.ByQuery), chromedp.WaitVisible("#servers", chromedp.ByQuery))
	run("open the tokens view again", chromedp.Click("#nav a[data-nav=tokens]", chromedp.ByQuery),
		chromedp.WaitVisible(moderatorRow, chromedp.ByQuery))

	// Negative control on the two-click revoke: asking and then cancelling
	// leaves the token live on the hub.
	run("ask to revoke and cancel",
		chromedp.Click(`button[data-revoke-token="`+moderatorID+`"]`, chromedp.ByQuery),
		chromedp.WaitVisible(`button[data-revoke-confirm="`+moderatorID+`"]`, chromedp.ByQuery),
		chromedp.Click(`button[data-revoke-cancel="`+moderatorID+`"]`, chromedp.ByQuery),
		chromedp.WaitVisible(`button[data-revoke-token="`+moderatorID+`"]`, chromedp.ByQuery))
	for _, record := range manageTokens(t, hubURL) {
		if record.ID == moderatorID && record.RevokedAt != nil {
			t.Fatal("a cancelled revoke reached the hub; the second click is the guard")
		}
	}
	run("revoke the minted token",
		chromedp.Click(`button[data-revoke-token="`+moderatorID+`"]`, chromedp.ByQuery),
		chromedp.WaitVisible(`button[data-revoke-confirm="`+moderatorID+`"]`, chromedp.ByQuery),
		chromedp.Click(`button[data-revoke-confirm="`+moderatorID+`"]`, chromedp.ByQuery))
	waitJS("the revoked token's row says so",
		`(function(){const r=document.querySelector(`+strconv.Quote(moderatorRow)+`);`+
			`return r && r.dataset.tokenState === "revoked" && r.classList.contains("dim")})()`)
	run("sign out with a revoked token in hand",
		chromedp.Click("#sign-out", chromedp.ByQuery), chromedp.WaitVisible("#login", chromedp.ByQuery))
	run("try the revoked secret",
		chromedp.SendKeys("#token", moderatorSecret, chromedp.ByQuery),
		chromedp.Click("#sign-in", chromedp.ByQuery), chromedp.WaitVisible("#login-error", chromedp.ByQuery))
	if got := text("#login-error"); !strings.Contains(got, "unauthorized") {
		t.Fatalf("the revoked secret was answered %q, want the hub's unauthorized code", got)
	}
	run("sign in with the bootstrap token once more",
		setValue("#token", ""), chromedp.SendKeys("#token", e2eAdminToken, chromedp.ByQuery),
		chromedp.Click("#sign-in", chromedp.ByQuery), chromedp.WaitVisible("#mint-token", chromedp.ByQuery))

	// 6. Webhooks. The receiver refuses everything until told otherwise, so
	// the first delivery is a failure with a retry booked, which is what the
	// pause must then hold.
	hookOne, hookTwo := receiver.server.URL+"/hook-one", receiver.server.URL+"/hook-two"
	run("open the webhooks view", chromedp.Click("#nav a[data-nav=webhooks]", chromedp.ByQuery),
		chromedp.WaitVisible("#register-webhook", chromedp.ByQuery))
	run("register a webhook",
		setValue("#webhook-url", hookOne),
		setValue("#webhook-events", "core.player.*"),
		chromedp.Click("#register-webhook-submit", chromedp.ByQuery),
		chromedp.WaitVisible("#webhook-secret-value", chromedp.ByQuery))
	if got := text("#webhook-secret-value"); len(got) < 16 {
		t.Fatalf("the webhook signing secret is %q, want the value the hub signs with, shown once", got)
	}
	hooks := manageWebhooks(t, hubURL)
	if len(hooks) != 1 || hooks[0].URL != hookOne || hooks[0].Template != "generic-json" {
		t.Fatalf("the hub holds %+v, want one generic-json webhook aimed at the receiver", hooks)
	}
	webhookID := hooks[0].ID
	run("open the webhook page",
		chromedp.Click(`tr[data-webhook-id="`+webhookID+`"] a`, chromedp.ByQuery),
		chromedp.WaitVisible("#webhook-summary", chromedp.ByQuery))

	publish := func(what string, events ...map[string]any) {
		t.Helper()
		plugin.queue("event.batch", map[string]any{"events": events})
		if err := plugin.awaitDrained(h.Ctx); err != nil {
			t.Fatalf("the hub never acked %s: %v", what, err)
		}
	}
	alice := map[string]any{"platform": "steam", "id": "76561198000000001"}
	stamp := func() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }
	publish("the connect event", map[string]any{
		"t": "core.player.connect", "ts": stamp(), "data": map[string]any{"player": alice, "name": "Alice"},
	})
	waitJS("the failed delivery is on the page",
		`(function(){const r=document.querySelector("#deliveries tbody tr[data-delivery-id]");`+
			`return r && (r.dataset.deliveryState === "pending" || r.dataset.deliveryState === "dead")})()`)
	h.waitUntil("the hub booked a failed attempt", 30*time.Second, func() bool {
		for _, one := range manageDeliveries(t, hubURL, webhookID) {
			if one.Type == "core.player.connect" && one.Attempts >= 1 {
				return true
			}
		}
		return false
	})
	if receiver.countOn("/hook-one") < 1 {
		t.Fatal("the receiver was never called, so nothing was delivered to fail")
	}

	// Pause. The hub must stop talking to the target without forgetting what
	// it owes: deliveries keep arriving, and none of them go out.
	run("pause the webhook", chromedp.Click("#toggle-pause", chromedp.ByQuery))
	waitJS("the pause toggle is pressed", `document.querySelector("#toggle-pause").getAttribute("aria-pressed") === "true"`)
	h.waitUntil("the hub recorded the pause", 15*time.Second, func() bool {
		hooks := manageWebhooks(t, hubURL)
		return len(hooks) == 1 && hooks[0].PausedAt != nil
	})
	if got := text("#webhook-state"); !strings.Contains(got, "paused") {
		t.Fatalf("the webhook summary says %q while paused", got)
	}
	pausedAt := receiver.count()
	publish("an event while paused", map[string]any{
		"t": "core.player.disconnect", "ts": stamp(), "data": map[string]any{"player": alice, "name": "Alice"},
	})
	// Longer than the dispatcher's one-second cadence, and long enough for
	// the first delivery's ten-second retry to have been due had the pause
	// not held it.
	time.Sleep(3 * time.Second)
	if got := receiver.count(); got != pausedAt {
		t.Fatalf("the receiver was called %d times while the webhook was paused, want the %d it had", got, pausedAt)
	}
	pending := 0
	for _, one := range manageDeliveries(t, hubURL, webhookID) {
		if one.State == "pending" {
			pending++
		}
	}
	if pending < 2 {
		t.Fatalf("%d pending deliveries while paused, want the held ones: a pause may not drop what it owes", pending)
	}

	// Resume, and the same receiver starts hearing from the hub again. This
	// is the positive control that makes the silence above mean something.
	run("resume the webhook", chromedp.Click("#toggle-pause", chromedp.ByQuery))
	waitJS("the pause toggle is released", `document.querySelector("#toggle-pause").getAttribute("aria-pressed") === "false"`)
	h.waitUntil("deliveries flow again after the resume", 30*time.Second, func() bool {
		return receiver.count() > pausedAt
	})

	// An edit replaces the fields named and nothing else, and a pending
	// delivery goes to the new URL on its next attempt.
	run("edit the webhook's url",
		setValue("#edit-webhook-url", hookTwo),
		chromedp.Click("#edit-webhook-submit", chromedp.ByQuery))
	h.waitUntil("the hub stored the new url", 15*time.Second, func() bool {
		hooks := manageWebhooks(t, hubURL)
		return len(hooks) == 1 && hooks[0].URL == hookTwo
	})
	waitJS("the summary shows the new url",
		`document.querySelector("#webhook-summary").textContent.includes("/hook-two")`)
	if hooks := manageWebhooks(t, hubURL); len(hooks) != 1 || len(hooks[0].Events) != 1 || hooks[0].Events[0] != "core.player.*" {
		t.Fatalf("the edit changed the filter too: %+v", hooks)
	}

	// Replay. The reference retry schedule takes a quarter of an hour to
	// dead-letter anything, so the delivered path is the one that can be
	// graded here: a target that starts answering, a delivery that landed,
	// and the operator asking for it again. The webhook is paused across the
	// replay so that "the hub re-armed it" is observable rather than a race
	// with the dispatcher, which is the section 11.5 rule that a replay on a
	// paused webhook succeeds and waits for the resume.
	receiver.answerWith(http.StatusOK)
	publish("an event the receiver accepts", map[string]any{
		"t": "core.player.chat", "ts": stamp(), "data": map[string]any{"player": alice, "text": "hello"},
	})
	waitJS("a delivery reached delivered", `!!document.querySelector('#deliveries tbody tr[data-delivery-state="delivered"]')`)
	deliveredID := evalString(`document.querySelector('#deliveries tbody tr[data-delivery-state="delivered"]').dataset.deliveryId`)
	before := receiver.attemptsOf(deliveredID)
	if len(before) != 1 {
		t.Fatalf("the receiver saw %v attempts of the delivered delivery, want exactly one", before)
	}
	run("pause before the replay", chromedp.Click("#toggle-pause", chromedp.ByQuery))
	waitJS("paused for the replay", `document.querySelector("#toggle-pause").getAttribute("aria-pressed") === "true"`)
	h.waitUntil("the hub recorded the pause", 15*time.Second, func() bool {
		hooks := manageWebhooks(t, hubURL)
		return len(hooks) == 1 && hooks[0].PausedAt != nil
	})
	run("replay the delivered delivery", chromedp.Click(`button[data-replay="`+deliveredID+`"]`, chromedp.ByQuery))
	h.waitUntil("the replayed delivery is pending again", 15*time.Second, func() bool {
		for _, one := range manageDeliveries(t, hubURL, webhookID) {
			if one.ID == deliveredID {
				return one.State == "pending" && one.DeliveredAt == nil
			}
		}
		return false
	})
	run("resume after the replay", chromedp.Click("#toggle-pause", chromedp.ByQuery))
	waitJS("resumed after the replay", `document.querySelector("#toggle-pause").getAttribute("aria-pressed") === "false"`)
	h.waitUntil("the replay reached the receiver", 30*time.Second, func() bool {
		return len(receiver.attemptsOf(deliveredID)) > 1
	})
	after := receiver.attemptsOf(deliveredID)
	if len(after) != 2 || after[1] != before[0]+1 {
		t.Fatalf("the receiver saw attempts %v for delivery %s, want the same delivery once more at attempt %d",
			after, deliveredID, before[0]+1)
	}

	// The dead-letter filter. Nothing is dead on the reference schedule this
	// early, so what it must show is nothing at all, and unticking it must
	// bring the deliveries back: a filter that never filtered would pass the
	// first half of that and fail the second.
	for _, one := range manageDeliveries(t, hubURL, webhookID) {
		if one.State == "dead" {
			t.Fatalf("delivery %s is dead already; the retry schedule was not the reference one", one.ID)
		}
	}
	run("show dead letters only", chromedp.Click("#dead-only", chromedp.ByQuery))
	waitJS("no row survives the dead-letter filter",
		`document.querySelectorAll("#deliveries tbody tr").length === 0 && !document.querySelector("#deliveries-empty").hidden`)
	run("show every delivery again", chromedp.Click("#dead-only", chromedp.ByQuery))
	waitJS("the deliveries are back", `document.querySelectorAll("#deliveries tbody tr").length > 0`)

	// Negative control on the delete tick, then the delete.
	run("delete without confirming", chromedp.Click("#delete-webhook", chromedp.ByQuery),
		chromedp.WaitVisible("#webhook-error", chromedp.ByQuery))
	if got := text("#webhook-error"); !strings.Contains(got, "confirm") {
		t.Fatalf("unconfirmed delete said %q, want the confirmation refusal", got)
	}
	if len(manageWebhooks(t, hubURL)) != 1 {
		t.Fatal("an unconfirmed delete reached the hub; the tick is the guard")
	}
	run("delete with the tick",
		chromedp.Click("#delete-webhook-confirm", chromedp.ByQuery),
		chromedp.Click("#delete-webhook", chromedp.ByQuery),
		chromedp.WaitVisible("#webhooks-empty", chromedp.ByQuery))
	if got := evalString(`document.querySelector(` + strconv.Quote(`tr[data-webhook-id="`+webhookID+`"]`) + `) ? "row" : "none"`); got != "none" {
		t.Fatalf("the deleted webhook still has a row (%s)", got)
	}
	if hooks := manageWebhooks(t, hubURL); len(hooks) != 0 {
		t.Fatalf("the hub still holds %+v after the delete", hooks)
	}

	// 7. The audit log holds every mutation the browser has made, newest
	// first, and nothing it refused to send.
	run("open the audit view", chromedp.Click("#nav a[data-nav=audit]", chromedp.ByQuery),
		chromedp.WaitVisible("#audit", chromedp.ByQuery))
	requests := strings.Split(evalString(
		`Array.from(document.querySelectorAll("#audit tbody tr")).map(r => r.children[2].textContent).join("|")`), "|")
	// The pause, resume, and edit are all PATCHes of the same path, so the
	// ordering chain is taken over the mutations that appear once; the
	// PATCHes are counted instead.
	rowAt := func(entry string) int {
		for i, request := range requests {
			if request == entry {
				return i
			}
		}
		t.Fatalf("the audit log does not list %q (it lists %q)", entry, strings.Join(requests, "|"))
		return -1
	}
	order := []string{
		"DELETE /api/v1/webhooks/" + webhookID,
		"POST /api/v1/webhooks/" + webhookID + "/deliveries/" + deliveredID + "/replay",
		"POST /api/v1/webhooks",
		"DELETE /api/v1/tokens/" + moderatorID,
		"POST /api/v1/tokens",
		"DELETE /api/v1/servers/" + registered.ID + "/credentials",
		"POST /api/v1/servers/" + registered.ID + "/enrollment-token",
		"POST /api/v1/servers",
	}
	previous := -1
	for _, entry := range order {
		at := rowAt(entry)
		if at <= previous {
			t.Fatalf("the audit log lists %q out of order; newest first is the contract (%q)",
				entry, strings.Join(requests, "|"))
		}
		previous = at
	}
	patches := 0
	for i, request := range requests {
		if request != "PATCH /api/v1/webhooks/"+webhookID {
			continue
		}
		patches++
		if i < rowAt("DELETE /api/v1/webhooks/"+webhookID) || i > rowAt("POST /api/v1/webhooks") {
			t.Fatalf("an edit or pause is listed outside the webhook's life (%q)", strings.Join(requests, "|"))
		}
	}
	// Two pauses, two resumes, and the edit.
	if patches != 5 {
		t.Fatalf("%d webhook PATCH records, want the two pauses, two resumes, and the edit (%q)",
			patches, strings.Join(requests, "|"))
	}
	if got := evalString(`Array.from(document.querySelectorAll("#audit tbody tr")).every(r => /^[1-5][0-9][0-9]$/.test(r.dataset.auditStatus)) ? "yes" : "no"`); got != "yes" {
		t.Fatalf("not every audit row carries a status (%s)", got)
	}
	if got := evalString(`String(document.querySelectorAll('#audit tbody tr[data-audit-status="201"]').length > 0)`); got != "true" {
		t.Fatal("no audit row carries a 201, so the status badges are not the hub's")
	}

	run("filter the audit log to the registered server",
		selectOption("#audit-server", registered.ID),
		chromedp.Click("#audit-apply", chromedp.ByQuery))
	waitJS("the server filter is in the route", `location.hash.includes("serverId=`+registered.ID+`")`)
	waitJS("the audit log narrowed to that server's mutations",
		`Array.from(document.querySelectorAll("#audit tbody tr")).map(r => r.children[2].textContent).join("|") === `+
			strconv.Quote("DELETE /api/v1/servers/"+registered.ID+"/credentials|"+
				"POST /api/v1/servers/"+registered.ID+"/enrollment-token|POST /api/v1/servers"))

	// A window with nothing in it is an empty table, not an error: the
	// difference matters, because a filter the hub refuses looks different.
	future := time.Now().Add(48 * time.Hour).Format("2006-01-02T15:04")
	run("ask for records from the future",
		setValue("#audit-since", future),
		chromedp.Click("#audit-apply", chromedp.ByQuery))
	waitJS("the empty window is empty and not an error",
		`document.querySelector("#audit").hidden && !document.querySelector("#audit-empty").hidden && document.querySelector("#audit-error").hidden`)
	run("clear the filters", chromedp.Click("#audit-clear", chromedp.ByQuery))
	waitJS("the unfiltered log is back",
		`location.hash === "#/audit" && document.querySelectorAll("#audit tbody tr").length > 3`)

	// A link can name a server this hub's list does not hold: one deleted, or
	// one this token cannot read. The select has no option for it, so without
	// one of its own it would sit empty and Apply would widen the filter from
	// that server back to every server, silently, on a page the operator only
	// meant to re-apply. The probe attribute is what makes the assertion mean
	// something: it is gone once the view has been redrawn, so the hash is
	// read after the Apply rather than before it.
	const unlisted = "01ZZDELETEDSERVER0000000000"
	run("open the audit log filtered to a server the list does not hold",
		chromedp.Evaluate(`location.hash = "#/audit?serverId=`+unlisted+`"`, nil))
	waitJS("the unlisted id is what the select holds",
		`document.querySelector("#audit-server").value === "`+unlisted+`"`)
	run("apply the filter unchanged",
		chromedp.Evaluate(`document.querySelector("#audit-server").dataset.probe = "before"`, nil),
		chromedp.Click("#audit-apply", chromedp.ByQuery))
	waitJS("the unlisted id round-trips through Apply rather than widening the filter",
		`!document.querySelector("#audit-server").dataset.probe && location.hash === "#/audit?serverId=`+unlisted+`"`)

	// 8. Key/value: the listings, then the editor. The expired key's
	// namespace holds nothing live, so it is not listed at all.
	run("open the key/value view", chromedp.Click("#nav a[data-nav=kv]", chromedp.ByQuery),
		chromedp.WaitVisible("#kv-namespaces", chromedp.ByQuery))
	if got := evalString(`document.querySelector('tr[data-namespace="example-mod"] td:nth-child(2)').textContent`); got != "4" {
		t.Fatalf("example-mod lists %q live keys, want 4", got)
	}
	if got := evalString(`document.querySelector('tr[data-namespace="zeta-store"]') ? "listed" : "absent"`); got != "absent" {
		t.Fatalf("the namespace whose only key has expired is %s, want absent", got)
	}
	run("open the namespace", chromedp.Click(`tr[data-namespace="example-mod"] a`, chromedp.ByQuery),
		chromedp.WaitVisible("#kv-keys", chromedp.ByQuery))
	waitJS("every live key is listed", `document.querySelectorAll("#kv-keys tbody tr[data-key]").length === 4`)
	run("filter by prefix", setValue("#kv-prefix", "balance."), chromedp.Click("#kv-prefix-apply", chromedp.ByQuery))
	waitJS("the prefix filter is in the route and applied",
		`location.hash.includes("prefix=balance.") && `+
			`Array.from(document.querySelectorAll("#kv-keys tbody tr[data-key]")).map(r => r.dataset.key).join(",") === "balance.alpha,balance.beta,balance.gamma"`)
	run("open a key", chromedp.Click(`button[data-open-key="balance.alpha"]`, chromedp.ByQuery),
		chromedp.WaitVisible("#kv-value-card", chromedp.ByQuery))
	if got := evalString(`document.querySelector("#kv-edit-value").value`); !strings.Contains(got, `"coins": 42`) {
		t.Fatalf("the opened value = %q, want the stored JSON", got)
	}
	if got := text("#kv-value-revision"); got != "1" {
		t.Fatalf("the opened key's revision = %q, want 1", got)
	}
	kvValue := func(key string) (int, map[string]any) {
		var record struct {
			Value    map[string]any `json:"value"`
			Revision int            `json:"revision"`
		}
		status, body := adminRequest(t, http.MethodGet, hubURL+"/api/v1/kv/example-mod/"+key, nil)
		if status == http.StatusNotFound {
			return 0, nil
		}
		if status != http.StatusOK {
			t.Fatalf("read kv key %s: status %d body %s", key, status, body)
		}
		if err := json.Unmarshal(body, &record); err != nil {
			t.Fatalf("decode kv key %s: %v", key, err)
		}
		return record.Revision, record.Value
	}

	// Save: the edit lands as revision 2, guarded by the revision opened.
	run("edit and save the key", setValue("#kv-edit-value", `{"coins": 43}`),
		chromedp.Click("#kv-save", chromedp.ByQuery))
	waitJS("the save reports the new revision",
		`document.querySelector("#kv-value-revision").textContent === "2" && `+
			`document.querySelector('tr[data-key="balance.alpha"] td[data-revision]').textContent === "2"`)
	if revision, value := kvValue("balance.alpha"); revision != 2 || value["coins"] != float64(43) {
		t.Fatalf("after the save the store holds revision %d value %v, want 2 and 43 coins", revision, value)
	}

	// A write from elsewhere between the open and the save: the save is
	// refused with revision_mismatch and the other writer's value stands.
	if status, body := adminRequest(t, http.MethodPut, hubURL+"/api/v1/kv/example-mod/balance.alpha",
		map[string]any{"value": map[string]any{"coins": 99}}); status != http.StatusOK {
		t.Fatalf("the concurrent write: status %d body %s", status, body)
	}
	run("save over the concurrent write", setValue("#kv-edit-value", `{"coins": 44}`),
		chromedp.Click("#kv-save", chromedp.ByQuery))
	waitJS("the stale save is refused and says why",
		`!document.querySelector("#kv-keys-error").hidden && document.querySelector("#kv-keys-error").textContent.includes("revision_mismatch")`)
	if got := text("#kv-keys-error"); !strings.Contains(got, "revision 3 now") {
		t.Fatalf("the mismatch notice = %q, want the current revision named", got)
	}
	if revision, value := kvValue("balance.alpha"); revision != 3 || value["coins"] != float64(99) {
		t.Fatalf("after the refused save the store holds revision %d value %v, want the concurrent writer's 3 and 99", revision, value)
	}
	run("reload the key", chromedp.Click("#kv-reload", chromedp.ByQuery))
	waitJS("the reload shows the stored value",
		`document.querySelector("#kv-value-revision").textContent === "3" && document.querySelector("#kv-edit-value").value.includes("99")`)

	// A value that is not JSON, or is null, is refused before any request.
	run("save a value that is not JSON", setValue("#kv-edit-value", `{"coins": `),
		chromedp.Click("#kv-save", chromedp.ByQuery))
	waitJS("the malformed value is refused in the browser",
		`document.querySelector("#kv-keys-error").textContent.includes("bad_json")`)
	if revision, _ := kvValue("balance.alpha"); revision != 3 {
		t.Fatalf("a malformed save reached the store: revision %d, want 3", revision)
	}

	// Create: a new key lands in key order; a name already taken is refused,
	// not replaced (ifRevision 0).
	run("create a key", setValue("#kv-new-key", "balance.delta"), setValue("#kv-new-value", `{"coins": 5}`),
		chromedp.Click("#kv-create", chromedp.ByQuery))
	waitJS("the created key is listed in order and opened",
		`Array.from(document.querySelectorAll("#kv-keys tbody tr[data-key]")).map(r => r.dataset.key).join(",") === "balance.alpha,balance.beta,balance.delta,balance.gamma" && `+
			`document.querySelector("#kv-value-card h2").textContent.includes("balance.delta")`)
	if revision, value := kvValue("balance.delta"); revision != 1 || value["coins"] != float64(5) {
		t.Fatalf("the created key holds revision %d value %v, want 1 and 5 coins", revision, value)
	}
	run("create the same key again", setValue("#kv-new-key", "balance.beta"), setValue("#kv-new-value", `{"coins": 1000}`),
		chromedp.Click("#kv-create", chromedp.ByQuery))
	waitJS("a taken name is refused",
		`document.querySelector("#kv-keys-error").textContent.includes("exists already")`)
	if _, value := kvValue("balance.beta"); value["coins"] != float64(7) {
		t.Fatalf("creating over an existing key replaced it: %v", value)
	}

	// Delete: refused until confirmed, then gone from the list and the store.
	run("delete without confirming", chromedp.Click("#kv-delete", chromedp.ByQuery))
	waitJS("the delete asks for the confirmation",
		`document.querySelector("#kv-keys-error").textContent.includes("confirmation")`)
	if revision, _ := kvValue("balance.delta"); revision != 1 {
		t.Fatalf("an unconfirmed delete reached the store")
	}
	run("delete the key", chromedp.Click("#kv-delete-confirm", chromedp.ByQuery), chromedp.Click("#kv-delete", chromedp.ByQuery))
	waitJS("the deleted key leaves the list and the editor",
		`!document.querySelector('tr[data-key="balance.delta"]') && document.querySelector("#kv-value").hidden`)
	if revision, _ := kvValue("balance.delta"); revision != 0 {
		t.Fatalf("the deleted key still reads at revision %d", revision)
	}

	// A stored integer beyond 2^53 has been rounded by the browser's parser
	// before the editor sees it, so the editor refuses to save it back.
	if status, body := adminRequest(t, http.MethodPut, hubURL+"/api/v1/kv/example-mod/balance.huge",
		json.RawMessage(`{"value": {"id": 9007199254740993}}`)); status != http.StatusOK {
		t.Fatalf("write the huge-number key: status %d body %s", status, body)
	}
	run("open the huge-number key", chromedp.Navigate(hubURL+"/#/kv/example-mod?prefix=balance.huge"),
		chromedp.WaitVisible(`button[data-open-key="balance.huge"]`, chromedp.ByQuery),
		chromedp.Click(`button[data-open-key="balance.huge"]`, chromedp.ByQuery),
		chromedp.WaitVisible("#kv-edit-unsafe", chromedp.ByQuery))
	if got := evalString(`document.querySelector("#kv-save").disabled ? "disabled" : "enabled"`); got != "disabled" {
		t.Fatalf("the save of a value holding an unsafe integer is %s, want disabled", got)
	}
	// The same number 70 arrays deep, and one past the range of a double,
	// which the browser parses as Infinity and would write back as null.
	deep := strings.Repeat("[", 70) + "9007199254740993" + strings.Repeat("]", 70)
	for key, raw := range map[string]string{"balance.hugedeep": deep, "balance.infinite": "1e400"} {
		if status, body := adminRequest(t, http.MethodPut, hubURL+"/api/v1/kv/example-mod/"+key,
			json.RawMessage(`{"value": `+raw+`}`)); status != http.StatusOK {
			t.Fatalf("write %s: status %d body %s", key, status, body)
		}
		run("open "+key, chromedp.Navigate(hubURL+"/#/kv/example-mod?prefix="+key),
			chromedp.WaitVisible(`button[data-open-key="`+key+`"]`, chromedp.ByQuery),
			chromedp.Click(`button[data-open-key="`+key+`"]`, chromedp.ByQuery),
			chromedp.WaitVisible("#kv-edit-unsafe", chromedp.ByQuery))
		if got := evalString(`document.querySelector("#kv-save").disabled ? "disabled" : "enabled"`); got != "disabled" {
			t.Fatalf("the save of %s is %s, want disabled", key, got)
		}
	}

	// A key created inside the walked part of a paged namespace does not
	// move the walk's boundary: a second one that sorts after the first but
	// inside the page is listed too (the hub's page is 100 keys, so 101 make
	// a cursor).
	for i := 0; i <= 100; i++ {
		key := fmt.Sprintf("k%03d", i)
		if status, body := adminRequest(t, http.MethodPut, hubURL+"/api/v1/kv/paged/"+key,
			map[string]any{"value": i}); status != http.StatusOK {
			t.Fatalf("write paged/%s: status %d body %s", key, status, body)
		}
	}
	run("open the paged namespace", chromedp.Navigate(hubURL+"/#/kv/paged"),
		chromedp.WaitVisible("#kv-keys", chromedp.ByQuery))
	waitJS("the first page is listed with more to come",
		`document.querySelectorAll("#kv-keys tbody tr[data-key]").length === 100 && !document.querySelector("#kv-more").hidden`)
	for _, key := range []string{"k005a", "k006a"} {
		run("create "+key, setValue("#kv-new-key", key), setValue("#kv-new-value", `1`),
			chromedp.Click("#kv-create", chromedp.ByQuery))
		waitJS(key+" is listed", `!!document.querySelector('tr[data-key="`+key+`"]')`)
	}

	// 9. Signing out forgets the token, and the nav goes with it.
	run("sign out at the end", chromedp.Click("#sign-out", chromedp.ByQuery),
		chromedp.WaitVisible("#login", chromedp.ByQuery))
	if got := evalString(`sessionStorage.getItem("vyshka.adminToken") || "none"`); got != "none" {
		t.Fatalf("token survived sign-out: %q", got)
	}
	if got := evalString(`document.querySelector("#nav").hidden ? "hidden" : "shown"`); got != "hidden" {
		t.Fatalf("the nav is %s after signing out", got)
	}
}
