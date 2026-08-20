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
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/That1Drifter/vyshka/hub"
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
			"v": 1, "id": "evt-env-" + eventType + "-" + time.Now().Format("150405.000"),
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
			"url": "http://127.0.0.1:1/x", "template": "discord",
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

	// A non-matching type must not fire; a settle window bounds the negative.
	sendEvent(t, server, serverID, session.SessionToken, "example-mod.raid.start", 2)
	time.Sleep(2500 * time.Millisecond)
	if receiver.count() != 1 {
		t.Errorf("receiver saw %d deliveries after a non-matching event, want 1", receiver.count())
	}
}

// A failing target is retried on the configured schedule and then
// dead-lettered with the attempt count exposed (spec section 11.5).
func TestFailingTargetIsRetriedThenDeadLettered(t *testing.T) {
	t.Parallel()
	server, err := hub.New(context.Background(), hub.Config{
		DatabaseURL:        filepath.Join(t.TempDir(), "test.db"),
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
	server := newTestServer(t)
	receiver := newTestReceiver(t, http.StatusNoContent)

	created, session := enrolledSession(t, server, "webhook-boundary")
	serverID := created.Server.ID

	// The event lands first, the webhook second.
	sendEvent(t, server, serverID, session.SessionToken, "core.player.death", 1)
	registerWebhook(t, server, map[string]any{
		"url": receiver.server.URL, "events": []string{"core.player.*"},
	})
	time.Sleep(3 * time.Second)
	if receiver.count() != 0 {
		t.Fatalf("receiver saw %d deliveries of pre-registration history, want 0", receiver.count())
	}

	// Fresh traffic after registration flows normally.
	sendEvent(t, server, serverID, session.SessionToken, "core.player.death", 2)
	receiver.awaitReceived(t, 1, 10*time.Second)
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
