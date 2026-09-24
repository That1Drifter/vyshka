package hub_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/That1Drifter/vyshka/hub"
	"github.com/That1Drifter/vyshka/hub/internal/dbtest"
	"github.com/That1Drifter/vyshka/hub/store"
)

// receivedDelivery is one POST a test receiver accepted.
type receivedDelivery struct {
	Body      []byte
	Signature string
	Delivery  string
	Attempt   string
}

// testReceiver is a local webhook target that records everything it is sent
// and answers with whatever status the test configured.
type testReceiver struct {
	mu       sync.Mutex
	status   int
	received []receivedDelivery
	server   *httptest.Server
}

func newTestReceiver(t *testing.T, status int) *testReceiver {
	t.Helper()
	receiver := &testReceiver{status: status}
	receiver.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		receiver.mu.Lock()
		receiver.received = append(receiver.received, receivedDelivery{
			Body:      body,
			Signature: r.Header.Get("X-Vyshka-Signature"),
			Delivery:  r.Header.Get("X-Vyshka-Delivery"),
			Attempt:   r.Header.Get("X-Vyshka-Attempt"),
		})
		status := receiver.status
		receiver.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(receiver.server.Close)
	return receiver
}

// setStatus changes what the receiver answers from the next request on, which
// is how a replay test lets a target that was failing start succeeding.
func (tr *testReceiver) setStatus(status int) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.status = status
}

func (tr *testReceiver) count() int {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return len(tr.received)
}

func (tr *testReceiver) get(index int) receivedDelivery {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.received[index]
}

// awaitReceived waits for the receiver to have accepted at least n deliveries.
// The dispatcher runs on a one-second tick with nudges, so a healthy path is
// well under this deadline and a miss is a real failure.
func (tr *testReceiver) awaitReceived(t *testing.T, n int, patience time.Duration) {
	t.Helper()
	deadline := time.Now().Add(patience)
	for time.Now().Before(deadline) {
		if tr.count() >= n {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("receiver saw %d deliveries, want at least %d", tr.count(), n)
}

// registerWebhook registers one webhook and returns its id and secret.
func registerWebhook(t *testing.T, server *hub.Server, body map[string]any) (string, string) {
	t.Helper()
	var response struct {
		Webhook struct {
			ID string `json:"id"`
		} `json:"webhook"`
		Secret string `json:"secret"`
	}
	status := call(t, server, http.MethodPost, "/api/v1/webhooks", testAdminToken, body, &response)
	if status != http.StatusCreated {
		t.Fatalf("register webhook: status = %d, want 201", status)
	}
	if response.Webhook.ID == "" || response.Secret == "" {
		t.Fatalf("register webhook: response missing id or secret: %+v", response)
	}
	return response.Webhook.ID, response.Secret
}

// webhookDeliveries reads a webhook's delivery record.
func webhookDeliveries(t *testing.T, server *hub.Server, webhookID string) []map[string]any {
	t.Helper()
	var response struct {
		Deliveries []map[string]any `json:"deliveries"`
	}
	status := call(t, server, http.MethodGet, "/api/v1/webhooks/"+webhookID+"/deliveries",
		testAdminToken, nil, &response)
	if status != http.StatusOK {
		t.Fatalf("list deliveries: status = %d, want 200", status)
	}
	return response.Deliveries
}

// sendEvent pushes one event through the plugin realm, nudging the hold so the
// poll answers at once.
func sendEvent(t *testing.T, server *hub.Server, serverID, sessionToken, eventType string, seq int64) {
	t.Helper()
	pollNow(t, server, serverID, sessionToken, map[string]any{
		"envelopes": []map[string]any{{
			// seq, not a timestamp: batch ingest dedups on the envelope id per
			// server, and two sends inside one millisecond must stay distinct.
			"v": 1, "id": "evt-env-" + eventType + "-" + strconv.FormatInt(seq, 10),
			"type": "event.batch", "seq": seq, "ts": time.Now().UTC().Format(time.RFC3339),
			"body": map[string]any{"events": []map[string]any{
				{"t": eventType, "data": map[string]any{"probe": true}},
			}},
		}},
	})
}

func TestWebhookRegistrationValidation(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)

	cases := []struct {
		name       string
		body       map[string]any
		wantStatus int
		wantCode   string
	}{
		{"missing url", map[string]any{}, http.StatusBadRequest, "bad_request"},
		{"non-http url", map[string]any{"url": "ftp://example.net/x"}, http.StatusBadRequest, "bad_request"},
		{"pattern outside the grammar", map[string]any{
			"url": "http://127.0.0.1:1/x", "events": []string{"core.*.death"},
		}, http.StatusBadRequest, "bad_request"},
		{"unknown template", map[string]any{
			"url": "http://127.0.0.1:1/x", "template": "slack",
		}, http.StatusBadRequest, "bad_request"},
		{"unknown server id", map[string]any{
			"url": "http://127.0.0.1:1/x", "serverIds": []string{"srv-none"},
		}, http.StatusNotFound, "not_found"},
	}
	for _, tc := range cases {
		if code := errorCode(t, server, http.MethodPost, "/api/v1/webhooks",
			testAdminToken, tc.body, tc.wantStatus); code != tc.wantCode {
			t.Errorf("%s: code = %q, want %q", tc.name, code, tc.wantCode)
		}
	}

	webhookID, secret := registerWebhook(t, server, map[string]any{
		"url": "http://127.0.0.1:1/hook", "events": []string{"core.player.*"},
	})
	if !strings.HasPrefix(secret, "vyw_") {
		t.Errorf("secret %q does not carry the webhook realm prefix", secret)
	}

	// The secret leaves the hub exactly once: the list never carries it.
	var listed struct {
		Webhooks []map[string]any `json:"webhooks"`
	}
	if status := call(t, server, http.MethodGet, "/api/v1/webhooks", testAdminToken, nil, &listed); status != http.StatusOK {
		t.Fatalf("list webhooks: status = %d", status)
	}
	if len(listed.Webhooks) != 1 {
		t.Fatalf("listed %d webhooks, want 1", len(listed.Webhooks))
	}
	if _, leaked := listed.Webhooks[0]["secret"]; leaked {
		t.Error("the webhook list carries the secret")
	}

	if status := call(t, server, http.MethodDelete, "/api/v1/webhooks/"+webhookID, testAdminToken, nil, nil); status != http.StatusNoContent {
		t.Errorf("delete webhook: status = %d, want 204", status)
	}
	if code := errorCode(t, server, http.MethodDelete, "/api/v1/webhooks/"+webhookID,
		testAdminToken, nil, http.StatusNotFound); code != "not_found" {
		t.Errorf("second delete: code = %q, want not_found", code)
	}
}

func TestWebhookRoutesRequireManageScope(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)

	narrow, _ := mintToken(t, server, "narrow", "servers:read")
	if code := errorCode(t, server, http.MethodPost, "/api/v1/webhooks", narrow,
		map[string]any{"url": "http://127.0.0.1:1/x"}, http.StatusForbidden); code != "forbidden" {
		t.Errorf("POST without webhooks:manage: code = %q, want forbidden", code)
	}
	if code := errorCode(t, server, http.MethodGet, "/api/v1/webhooks", narrow,
		nil, http.StatusForbidden); code != "forbidden" {
		t.Errorf("GET without webhooks:manage: code = %q, want forbidden", code)
	}

	// webhooks:manage alone is a delivery capability, not a read grant: a
	// subscription the token's grants do not cover is refused (section 11.2).
	manageOnly, _ := mintToken(t, server, "manage-only", "webhooks:manage")
	if code := errorCode(t, server, http.MethodPost, "/api/v1/webhooks", manageOnly,
		map[string]any{"url": "http://127.0.0.1:1/x", "events": []string{"core.player.*"}},
		http.StatusForbidden); code != "forbidden" {
		t.Errorf("telemetry subscription without events:read: code = %q, want forbidden", code)
	}
	if code := errorCode(t, server, http.MethodPost, "/api/v1/webhooks", manageOnly,
		map[string]any{"url": "http://127.0.0.1:1/x"},
		http.StatusForbidden); code != "forbidden" {
		t.Errorf("catch-all subscription without read grants: code = %q, want forbidden", code)
	}
	if code := errorCode(t, server, http.MethodPost, "/api/v1/webhooks", manageOnly,
		map[string]any{"url": "http://127.0.0.1:1/x", "events": []string{"action.completed"}},
		http.StatusForbidden); code != "forbidden" {
		t.Errorf("action.completed subscription without actions:read: code = %q, want forbidden", code)
	}

	// A narrowed events:read covers a matching pattern and nothing wider.
	scoped, _ := mintToken(t, server, "hooks-scoped",
		"webhooks:manage", "events:read:example-mod.*")
	if code := errorCode(t, server, http.MethodPost, "/api/v1/webhooks", scoped,
		map[string]any{"url": "http://127.0.0.1:1/x", "events": []string{"core.player.*"}},
		http.StatusForbidden); code != "forbidden" {
		t.Errorf("subscription outside the grant: code = %q, want forbidden", code)
	}
	var response struct {
		Webhook struct {
			ID string `json:"id"`
		} `json:"webhook"`
	}
	if status := call(t, server, http.MethodPost, "/api/v1/webhooks", scoped,
		map[string]any{"url": "http://127.0.0.1:1/x", "events": []string{"example-mod.raid.*"}},
		&response); status != http.StatusCreated {
		t.Errorf("subscription inside the grant: status = %d, want 201", status)
	}
}

// The demo of the slice, as a test: a local receiver gets a signed
// core.player.death delivery, and the signature verifies against the secret
// the registration returned.
func TestSignedDeliveryEndToEnd(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	receiver := newTestReceiver(t, http.StatusNoContent)

	created, session := enrolledSession(t, server, "webhook-e2e")
	serverID := created.Server.ID

	webhookID, secret := registerWebhook(t, server, map[string]any{
		"url":       receiver.server.URL,
		"events":    []string{"core.player.*"},
		"serverIds": []string{serverID},
	})

	sendEvent(t, server, serverID, session.SessionToken, "core.player.death", 1)
	receiver.awaitReceived(t, 1, 10*time.Second)

	delivery := receiver.get(0)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(delivery.Body)
	if want := "sha256=" + hex.EncodeToString(mac.Sum(nil)); delivery.Signature != want {
		t.Errorf("signature = %q, want %q", delivery.Signature, want)
	}
	if delivery.Attempt != "1" {
		t.Errorf("attempt = %q, want 1", delivery.Attempt)
	}

	var payload struct {
		DeliveryID string          `json:"deliveryId"`
		WebhookID  string          `json:"webhookId"`
		Type       string          `json:"type"`
		ServerID   string          `json:"serverId"`
		EventID    string          `json:"eventId"`
		OccurredAt string          `json:"occurredAt"`
		Data       json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(delivery.Body, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.Type != "core.player.death" || payload.ServerID != serverID {
		t.Errorf("payload names %q on %q", payload.Type, payload.ServerID)
	}
	if payload.WebhookID != webhookID || payload.DeliveryID != delivery.Delivery {
		t.Errorf("payload ids %q/%q disagree with headers", payload.WebhookID, payload.DeliveryID)
	}
	if payload.EventID == "" || payload.OccurredAt == "" {
		t.Errorf("payload is missing eventId or occurredAt: %s", delivery.Body)
	}

	// The delivery record agrees.
	deadline := time.Now().Add(5 * time.Second)
	for {
		deliveries := webhookDeliveries(t, server, webhookID)
		if len(deliveries) == 1 && deliveries[0]["state"] == "delivered" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("delivery record never reached delivered: %+v", deliveries)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// A non-matching type must not fire. The proof is by ordering, not by a
	// settle window: the non-matching event is fanned out no later than the
	// pass that fans out the matching one sent after it, so once that one's
	// delivery arrives, the record already holds whatever the first became.
	sendEvent(t, server, serverID, session.SessionToken, "example-mod.raid.start", 2)
	sendEvent(t, server, serverID, session.SessionToken, "core.player.connect", 3)
	receiver.awaitReceived(t, 2, 10*time.Second)
	deliveries := webhookDeliveries(t, server, webhookID)
	if len(deliveries) != 2 {
		t.Fatalf("webhook holds %d deliveries after a non-matching and a matching event, want 2: %+v", len(deliveries), deliveries)
	}
	for _, delivery := range deliveries {
		if delivery["type"] == "example-mod.raid.start" {
			t.Errorf("a non-matching event became a delivery: %+v", delivery)
		}
	}
}

// A webhook registered with the discord template receives a Discord embed
// body, still signed, with mentions disabled (spec section 11.3).
func TestDiscordTemplateDelivery(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	receiver := newTestReceiver(t, http.StatusNoContent)

	created, session := enrolledSession(t, server, "webhook-discord")
	serverID := created.Server.ID

	var refused map[string]any
	if status := call(t, server, http.MethodPost, "/api/v1/webhooks", testAdminToken, map[string]any{
		"url": receiver.server.URL, "template": "slack",
	}, &refused); status != http.StatusBadRequest {
		t.Fatalf("an unknown template registered with status %d, want 400", status)
	}

	webhookID, secret := registerWebhook(t, server, map[string]any{
		"url":       receiver.server.URL,
		"events":    []string{"core.player.*"},
		"serverIds": []string{serverID},
		"template":  "discord",
	})
	var listed struct {
		Webhooks []struct {
			ID       string `json:"id"`
			Template string `json:"template"`
		} `json:"webhooks"`
	}
	call(t, server, http.MethodGet, "/api/v1/webhooks", testAdminToken, nil, &listed)
	if len(listed.Webhooks) != 1 || listed.Webhooks[0].ID != webhookID || listed.Webhooks[0].Template != "discord" {
		t.Fatalf("the webhook view does not echo the discord template: %+v", listed.Webhooks)
	}

	sendEvent(t, server, serverID, session.SessionToken, "core.player.connect", 1)
	receiver.awaitReceived(t, 1, 10*time.Second)

	delivery := receiver.get(0)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(delivery.Body)
	if want := "sha256=" + hex.EncodeToString(mac.Sum(nil)); delivery.Signature != want {
		t.Errorf("a discord delivery is still signed; signature = %q, want %q", delivery.Signature, want)
	}

	var body struct {
		Embeds []struct {
			Title       string `json:"title"`
			Description string `json:"description"`
			Timestamp   string `json:"timestamp"`
			Footer      struct {
				Text string `json:"text"`
			} `json:"footer"`
		} `json:"embeds"`
		AllowedMentions struct {
			Parse []string `json:"parse"`
		} `json:"allowed_mentions"`
		DeliveryID string `json:"deliveryId"`
	}
	if err := json.Unmarshal(delivery.Body, &body); err != nil {
		t.Fatalf("decode discord body: %v\n%s", err, delivery.Body)
	}
	if len(body.Embeds) != 1 {
		t.Fatalf("discord body carries %d embeds, want 1: %s", len(body.Embeds), delivery.Body)
	}
	if body.Embeds[0].Title != "Player connected" {
		t.Errorf("embed title = %q", body.Embeds[0].Title)
	}
	if _, err := time.Parse(time.RFC3339, body.Embeds[0].Timestamp); err != nil {
		t.Errorf("embed timestamp %q is not RFC 3339", body.Embeds[0].Timestamp)
	}
	if !strings.HasPrefix(body.Embeds[0].Footer.Text, "webhook-discord · ") {
		t.Errorf("footer = %q, want the server's name", body.Embeds[0].Footer.Text)
	}
	if body.AllowedMentions.Parse == nil || len(body.AllowedMentions.Parse) != 0 {
		t.Errorf("allowed_mentions.parse = %v, want an empty list", body.AllowedMentions.Parse)
	}
	if body.DeliveryID != "" {
		t.Errorf("a discord body is the Discord shape, not generic-json with extras: %s", delivery.Body)
	}
}

// A failing target is retried on the configured schedule and then
// dead-lettered with the attempt count exposed (spec section 11.5).
func TestFailingTargetIsRetriedThenDeadLettered(t *testing.T) {
	t.Parallel()
	server, err := hub.New(context.Background(), hub.Config{
		DatabaseURL:        dbtest.URL(t),
		AdminToken:         testAdminToken,
		Logger:             slog.New(slog.NewJSONHandler(io.Discard, nil)),
		WebhookRetryDelays: []time.Duration{100 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("boot hub: %v", err)
	}
	t.Cleanup(func() { server.Close() })

	receiver := newTestReceiver(t, http.StatusInternalServerError)
	created, session := enrolledSession(t, server, "webhook-retry")
	serverID := created.Server.ID
	webhookID, _ := registerWebhook(t, server, map[string]any{"url": receiver.server.URL})

	sendEvent(t, server, serverID, session.SessionToken, "core.player.death", 1)
	receiver.awaitReceived(t, 2, 15*time.Second)

	if first, second := receiver.get(0), receiver.get(1); string(first.Body) != string(second.Body) {
		t.Error("the retry changed the body; every attempt sends the same bytes")
	} else if first.Attempt != "1" || second.Attempt != "2" {
		t.Errorf("attempts = %q, %q, want 1 and 2", first.Attempt, second.Attempt)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		deliveries := webhookDeliveries(t, server, webhookID)
		if len(deliveries) == 1 && deliveries[0]["state"] == "dead" {
			if attempts := deliveries[0]["attempts"].(float64); attempts != 2 {
				t.Errorf("dead delivery attempts = %v, want 2", attempts)
			}
			if status := deliveries[0]["lastStatus"].(float64); status != 500 {
				t.Errorf("dead delivery lastStatus = %v, want 500", status)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("delivery never dead-lettered: %+v", deliveries)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// A webhook observes only what lands after it registers: stored history is
// never replayed into a new webhook (spec section 11.2).
func TestRegistrationIsNotABackfill(t *testing.T) {
	t.Parallel()
	databaseURL := dbtest.URL(t)
	server := newTestServerAt(t, databaseURL)
	receiver := newTestReceiver(t, http.StatusNoContent)

	created, session := enrolledSession(t, server, "webhook-boundary")
	serverID := created.Server.ID

	// The event lands first, the webhook second.
	sendEvent(t, server, serverID, session.SessionToken, "core.player.death", 1)
	webhookID, _ := registerWebhook(t, server, map[string]any{
		"url": receiver.server.URL, "events": []string{"core.player.*"},
	})

	// Left alone, the dispatcher fans the event out before the webhook exists,
	// and the boundary is never asked anything. Handing the event back to the
	// outbox is the backlog the boundary is for: a migration, or a pass that
	// had not reached it yet, leaves history unnotified at registration.
	side, err := store.Open(context.Background(), databaseURL)
	if err != nil {
		t.Fatalf("open a second handle on the hub's database: %v", err)
	}
	defer side.Close()
	rearmed, err := side.DB().Exec(`UPDATE events SET notified = 0 WHERE server_id = ?`, serverID)
	if err != nil {
		t.Fatalf("hand the event back to the outbox: %v", err)
	}
	if n, _ := rearmed.RowsAffected(); n != 1 {
		t.Fatalf("handed %d events back to the outbox, want 1", n)
	}

	// Fresh traffic after registration flows normally, and is also the
	// barrier for the negative: the history event is fanned out no later than
	// the pass that fans out this one, so once this delivery arrives the
	// record is final for both.
	sendEvent(t, server, serverID, session.SessionToken, "core.player.death", 2)
	receiver.awaitReceived(t, 1, 10*time.Second)
	if deliveries := webhookDeliveries(t, server, webhookID); len(deliveries) != 1 {
		t.Fatalf("webhook holds %d deliveries, want 1: pre-registration history was delivered: %+v", len(deliveries), deliveries)
	}
}

// The action and server namespaces are reserved at ingest (spec section 8.1),
// so a plugin's telemetry can never impersonate the hub's own lifecycle
// notifications to a webhook receiver.
func TestReservedNamespacesRefusedAtIngest(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, session := enrolledSession(t, server, "webhook-reserved")
	serverID := created.Server.ID

	sendEvent(t, server, serverID, session.SessionToken, "server.link.lost", 1)
	sendEvent(t, server, serverID, session.SessionToken, "action.completed", 2)

	var page struct {
		Events []map[string]any `json:"events"`
	}
	if status := call(t, server, http.MethodGet, "/api/v1/servers/"+serverID+"/events",
		testAdminToken, nil, &page); status != http.StatusOK {
		t.Fatalf("list events: status = %d", status)
	}
	if len(page.Events) != 0 {
		t.Fatalf("reserved-namespace events were stored: %+v", page.Events)
	}
}

// A 3xx answer is a failure, never followed: the redirect target was never
// registered and no audit names it (spec section 11.3).
func TestRedirectsAreNotFollowed(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)

	// The redirect target that must never be hit.
	var hijacked bool
	var mu sync.Mutex
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hijacked = true
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirector.Close)

	created, session := enrolledSession(t, server, "webhook-redirect")
	serverID := created.Server.ID
	webhookID, _ := registerWebhook(t, server, map[string]any{
		"url": redirector.URL, "events": []string{"core.player.*"},
	})

	sendEvent(t, server, serverID, session.SessionToken, "core.player.death", 1)

	deadline := time.Now().Add(10 * time.Second)
	for {
		deliveries := webhookDeliveries(t, server, webhookID)
		if len(deliveries) == 1 && deliveries[0]["attempts"].(float64) >= 1 {
			if state := deliveries[0]["state"]; state == "delivered" {
				t.Fatalf("a 307 answer was recorded as delivered")
			}
			if status := deliveries[0]["lastStatus"].(float64); status != 307 {
				t.Fatalf("lastStatus = %v, want the redirect's 307", status)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no attempt was recorded: %+v", deliveries)
		}
		time.Sleep(100 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if hijacked {
		t.Fatal("the hub followed the redirect and delivered the signed body to an unregistered address")
	}
}

// Expiry is a terminal state, so an action nobody ever delivered still fires
// action.completed (spec section 11.1).
func TestActionExpiryFiresActionCompleted(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	receiver := newTestReceiver(t, http.StatusOK)

	created, session := manifestFirst(t, server, "webhook-action")
	serverID := created.Server.ID
	_ = session

	registerWebhook(t, server, map[string]any{
		"url": receiver.server.URL, "events": []string{"action.completed"},
	})

	actionID, _ := dispatchAction(t, server, serverID, map[string]any{
		"code": "example-mod.heal", "context": "player",
		"referenceKey": "target-1", "params": map[string]any{"amount": 10},
		"ttlSeconds": 1,
	})

	receiver.awaitReceived(t, 1, 15*time.Second)
	var payload struct {
		Type string `json:"type"`
		Data struct {
			ActionID string `json:"actionId"`
			State    string `json:"state"`
		} `json:"data"`
	}
	if err := json.Unmarshal(receiver.get(0).Body, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.Type != "action.completed" || payload.Data.ActionID != actionID {
		t.Errorf("payload = %s, want action.completed for %s", receiver.get(0).Body, actionID)
	}
	if payload.Data.State != "expired" {
		t.Errorf("state = %q, want expired", payload.Data.State)
	}
}

// PATCH validates what registration validates, refuses an edit that widens a
// subscription past the editing token's grants, and replaces only the members
// it names (spec section 11.2).
func TestWebhookEditValidation(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, _ := enrolledSession(t, server, "webhook-edit")
	serverID := created.Server.ID

	webhookID, _ := registerWebhook(t, server, map[string]any{
		"url": "http://127.0.0.1:1/hook", "events": []string{"core.player.*"},
	})
	path := "/api/v1/webhooks/" + webhookID

	cases := []struct {
		name       string
		body       map[string]any
		wantStatus int
		wantCode   string
	}{
		{"no recognised member", map[string]any{}, http.StatusBadRequest, "bad_request"},
		{"nothing this draft knows", map[string]any{"colour": "red"}, http.StatusBadRequest, "bad_request"},
		{"non-http url", map[string]any{"url": "ftp://example.net/x"}, http.StatusBadRequest, "bad_request"},
		{"pattern outside the grammar", map[string]any{"events": []string{"core.*.death"}}, http.StatusBadRequest, "bad_request"},
		{"unknown template", map[string]any{"template": "slack"}, http.StatusBadRequest, "bad_request"},
		{"unknown server id", map[string]any{"serverIds": []string{"srv-none"}}, http.StatusNotFound, "not_found"},
	}
	for _, tc := range cases {
		if code := errorCode(t, server, http.MethodPatch, path,
			testAdminToken, tc.body, tc.wantStatus); code != tc.wantCode {
			t.Errorf("%s: code = %q, want %q", tc.name, code, tc.wantCode)
		}
	}
	// An empty body carries no edit at all.
	if code := errorCode(t, server, http.MethodPatch, path, testAdminToken, nil,
		http.StatusBadRequest); code != "bad_request" {
		t.Errorf("empty body: code = %q, want bad_request", code)
	}
	if code := errorCode(t, server, http.MethodPatch, "/api/v1/webhooks/wh-none",
		testAdminToken, map[string]any{"paused": true}, http.StatusNotFound); code != "not_found" {
		t.Errorf("unknown webhook: code = %q, want not_found", code)
	}

	// The edit replaces the members it names and leaves the rest alone, and
	// the answer never carries the secret.
	var edited struct {
		Webhook  map[string]any `json:"webhook"`
		Secret   string         `json:"secret"`
		PausedAt any            `json:"pausedAt"`
	}
	if status := call(t, server, http.MethodPatch, path, testAdminToken, map[string]any{
		"events": []string{}, "serverIds": []string{serverID}, "template": "discord",
	}, &edited); status != http.StatusOK {
		t.Fatalf("edit: status = %d, want 200", status)
	}
	if edited.Secret != "" {
		t.Error("an edit returned the signing secret; it leaves the hub once, at registration")
	}
	if events, _ := edited.Webhook["events"].([]any); len(events) != 0 {
		t.Errorf("events = %v, want the empty filter the edit named", edited.Webhook["events"])
	}
	if edited.Webhook["template"] != "discord" {
		t.Errorf("template = %v, want discord", edited.Webhook["template"])
	}
	if pausedAt, present := edited.Webhook["pausedAt"]; !present || pausedAt != nil {
		t.Errorf("pausedAt = %v (present %v), want an explicit null on an active webhook", pausedAt, present)
	}

	// The coverage rule is re-applied to the merged subscription, so a
	// narrowly granted token cannot widen a webhook it may edit.
	scoped, _ := mintToken(t, server, "edit-scoped", "webhooks:manage", "events:read:example-mod.*")
	narrowID, _ := registerWebhook(t, server, map[string]any{
		"url": "http://127.0.0.1:1/hook", "events": []string{"example-mod.raid.*"},
	})
	if code := errorCode(t, server, http.MethodPatch, "/api/v1/webhooks/"+narrowID, scoped,
		map[string]any{"events": []string{"core.player.*"}}, http.StatusForbidden); code != "forbidden" {
		t.Errorf("widening edit: code = %q, want forbidden", code)
	}
	// An edit that only pauses still re-checks the stored filter, which this
	// token does cover.
	if status := call(t, server, http.MethodPatch, "/api/v1/webhooks/"+narrowID, scoped,
		map[string]any{"paused": true}, nil); status != http.StatusOK {
		t.Errorf("pausing a webhook the token's grants cover: status = %d, want 200", status)
	}
	// Naming serverIds needs servers:read even when the events filter is
	// already covered, the same rule registration applies.
	if code := errorCode(t, server, http.MethodPatch, "/api/v1/webhooks/"+narrowID, scoped,
		map[string]any{"serverIds": []string{serverID}}, http.StatusForbidden); code != "forbidden" {
		t.Errorf("edit naming serverIds without servers:read: code = %q, want forbidden", code)
	}
}

// A paused webhook is one the hub does not talk to: the delivery is created
// and held, and the resume sends it (spec section 11.2).
func TestPausedWebhookIsNotDelivered(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	receiver := newTestReceiver(t, http.StatusNoContent)

	created, session := enrolledSession(t, server, "webhook-pause")
	serverID := created.Server.ID
	webhookID, _ := registerWebhook(t, server, map[string]any{
		"url": receiver.server.URL, "events": []string{"core.player.*"},
		"serverIds": []string{serverID},
	})
	// An active twin with the same subscription is the barrier for the
	// negative below: its deliveries show the dispatcher's passes going by.
	twin := newTestReceiver(t, http.StatusNoContent)
	registerWebhook(t, server, map[string]any{
		"url": twin.server.URL, "events": []string{"core.player.*"},
		"serverIds": []string{serverID},
	})

	var paused struct {
		Webhook struct {
			PausedAt *string `json:"pausedAt"`
		} `json:"webhook"`
	}
	if status := call(t, server, http.MethodPatch, "/api/v1/webhooks/"+webhookID,
		testAdminToken, map[string]any{"paused": true}, &paused); status != http.StatusOK {
		t.Fatalf("pause: status = %d, want 200", status)
	}
	if paused.Webhook.PausedAt == nil {
		t.Fatal("pausing left pausedAt null")
	}
	if _, err := time.Parse(time.RFC3339, *paused.Webhook.PausedAt); err != nil {
		t.Errorf("pausedAt %q is not RFC 3339", *paused.Webhook.PausedAt)
	}

	sendEvent(t, server, serverID, session.SessionToken, "core.player.death", 1)

	// The delivery is queued: pause holds what the hub owes, it does not drop
	// it.
	deadline := time.Now().Add(10 * time.Second)
	for {
		deliveries := webhookDeliveries(t, server, webhookID)
		if len(deliveries) == 1 {
			if state := deliveries[0]["state"]; state != "pending" {
				t.Fatalf("a paused webhook's delivery is %v, want pending", state)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a paused webhook queued no delivery: %+v", deliveries)
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Both deliveries of the first event were created together and fall due
	// together, so a hub that ignored the pause would attempt the paused one
	// in the pass that sends the twin its copy. Passes run one at a time, and
	// the second event's copy can only go out in a later pass, so once the
	// twin has it, that pass has finished with the paused delivery.
	twin.awaitReceived(t, 1, 10*time.Second)
	sendEvent(t, server, serverID, session.SessionToken, "core.player.death", 2)
	twin.awaitReceived(t, 2, 10*time.Second)
	if receiver.count() != 0 {
		t.Fatalf("a paused webhook was delivered to %d times", receiver.count())
	}
	for _, delivery := range webhookDeliveries(t, server, webhookID) {
		if attempts := delivery["attempts"].(float64); attempts != 0 {
			t.Errorf("a paused webhook's delivery was attempted %v times", attempts)
		}
	}

	// Pausing twice does not move the instant.
	var again struct {
		Webhook struct {
			PausedAt *string `json:"pausedAt"`
		} `json:"webhook"`
	}
	call(t, server, http.MethodPatch, "/api/v1/webhooks/"+webhookID,
		testAdminToken, map[string]any{"paused": true}, &again)
	if again.Webhook.PausedAt == nil || *again.Webhook.PausedAt != *paused.Webhook.PausedAt {
		t.Errorf("pausing twice moved pausedAt from %v to %v", paused.Webhook.PausedAt, again.Webhook.PausedAt)
	}

	// The resume sends what the pause held.
	var resumed struct {
		Webhook struct {
			PausedAt *string `json:"pausedAt"`
		} `json:"webhook"`
	}
	if status := call(t, server, http.MethodPatch, "/api/v1/webhooks/"+webhookID,
		testAdminToken, map[string]any{"paused": false}, &resumed); status != http.StatusOK {
		t.Fatalf("resume: status = %d, want 200", status)
	}
	if resumed.Webhook.PausedAt != nil {
		t.Errorf("resuming left pausedAt at %v", *resumed.Webhook.PausedAt)
	}
	receiver.awaitReceived(t, 2, 10*time.Second)
}

// A dead delivery replays with the same id and the same bytes, one further
// attempt, and the attempt counter carrying on (spec section 11.5).
func TestReplayReArmsADeadDelivery(t *testing.T) {
	t.Parallel()
	server, err := hub.New(context.Background(), hub.Config{
		DatabaseURL:        dbtest.URL(t),
		AdminToken:         testAdminToken,
		Logger:             slog.New(slog.NewJSONHandler(io.Discard, nil)),
		WebhookRetryDelays: []time.Duration{100 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("boot hub: %v", err)
	}
	t.Cleanup(func() { server.Close() })

	receiver := newTestReceiver(t, http.StatusInternalServerError)
	created, session := enrolledSession(t, server, "webhook-replay")
	serverID := created.Server.ID
	webhookID, secret := registerWebhook(t, server, map[string]any{
		"url": receiver.server.URL, "events": []string{"core.player.*"},
		"serverIds": []string{serverID},
	})

	sendEvent(t, server, serverID, session.SessionToken, "core.player.death", 1)
	receiver.awaitReceived(t, 2, 15*time.Second)

	var dead map[string]any
	deadline := time.Now().Add(10 * time.Second)
	for {
		deliveries := webhookDeliveries(t, server, webhookID)
		if len(deliveries) == 1 && deliveries[0]["state"] == "dead" {
			dead = deliveries[0]
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("delivery never dead-lettered: %+v", deliveries)
		}
		time.Sleep(100 * time.Millisecond)
	}
	deliveryID := dead["id"].(string)

	// Refusals first: a delivery is replayable only through the webhook that
	// owns it.
	replayPath := "/api/v1/webhooks/" + webhookID + "/deliveries/" + deliveryID + "/replay"
	if code := errorCode(t, server, http.MethodPost,
		"/api/v1/webhooks/wh-none/deliveries/"+deliveryID+"/replay",
		testAdminToken, nil, http.StatusNotFound); code != "not_found" {
		t.Errorf("replay on an unknown webhook: code = %q, want not_found", code)
	}
	if code := errorCode(t, server, http.MethodPost,
		"/api/v1/webhooks/"+webhookID+"/deliveries/dlv-none/replay",
		testAdminToken, nil, http.StatusNotFound); code != "not_found" {
		t.Errorf("replay of an unknown delivery: code = %q, want not_found", code)
	}
	other, _ := registerWebhook(t, server, map[string]any{"url": "http://127.0.0.1:1/hook"})
	if code := errorCode(t, server, http.MethodPost,
		"/api/v1/webhooks/"+other+"/deliveries/"+deliveryID+"/replay",
		testAdminToken, nil, http.StatusNotFound); code != "not_found" {
		t.Errorf("replay across webhooks: code = %q, want not_found", code)
	}
	narrow, _ := mintToken(t, server, "replay-narrow", "servers:read")
	if code := errorCode(t, server, http.MethodPost, replayPath, narrow, nil,
		http.StatusForbidden); code != "forbidden" {
		t.Errorf("replay without webhooks:manage: code = %q, want forbidden", code)
	}

	receiver.setStatus(http.StatusNoContent)
	var replayed struct {
		Delivery struct {
			ID          string  `json:"id"`
			State       string  `json:"state"`
			Attempts    int     `json:"attempts"`
			LastStatus  *int    `json:"lastStatus"`
			LastError   string  `json:"lastError"`
			DeliveredAt *string `json:"deliveredAt"`
		} `json:"delivery"`
	}
	if status := call(t, server, http.MethodPost, replayPath, testAdminToken, nil, &replayed); status != http.StatusAccepted {
		t.Fatalf("replay: status = %d, want 202", status)
	}
	if replayed.Delivery.ID != deliveryID || replayed.Delivery.State != "pending" {
		t.Errorf("replay answered %+v, want the same id back as pending", replayed.Delivery)
	}
	if replayed.Delivery.Attempts != 2 {
		t.Errorf("attempts = %d, want the 2 already made kept", replayed.Delivery.Attempts)
	}
	if replayed.Delivery.LastStatus == nil || *replayed.Delivery.LastStatus != 500 {
		t.Errorf("the last failure was erased by the replay: %+v", replayed.Delivery)
	}

	receiver.awaitReceived(t, 3, 15*time.Second)
	first, third := receiver.get(0), receiver.get(2)
	if string(first.Body) != string(third.Body) {
		t.Error("the replay changed the body; a replay sends the delivery that already existed")
	}
	if first.Signature != third.Signature {
		t.Error("the replay changed the signature")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(third.Body)
	if want := "sha256=" + hex.EncodeToString(mac.Sum(nil)); third.Signature != want {
		t.Errorf("replayed signature = %q, want %q", third.Signature, want)
	}
	if third.Delivery != deliveryID {
		t.Errorf("X-Vyshka-Delivery = %q, want the replayed delivery's own id %q", third.Delivery, deliveryID)
	}
	if third.Attempt != "3" {
		t.Errorf("X-Vyshka-Attempt = %q, want 3: the counter carries on across a replay", third.Attempt)
	}

	deadline = time.Now().Add(10 * time.Second)
	for {
		deliveries := webhookDeliveries(t, server, webhookID)
		if len(deliveries) == 1 && deliveries[0]["state"] == "delivered" {
			if attempts := deliveries[0]["attempts"].(float64); attempts != 3 {
				t.Errorf("delivered after a replay with attempts = %v, want 3", attempts)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the replayed delivery never reached delivered: %+v", deliveries)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
