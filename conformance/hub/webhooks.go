package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The webhook checks are the one place the suite stops being a pure client:
// grading push delivery means being a target, so each check binds a local
// listener the hub under test must be able to reach. -webhook-listen moves the
// bind address when the hub is not on this machine's loopback.

// receivedHook is one request the receiver accepted, with enough of the
// request recorded that a check can grade how it was made, not just that it
// was: a hub that GETs the wrong path with no Content-Type must not pass a
// check about POSTing generic-json.
type receivedHook struct {
	Method          string
	Path            string
	ContentType     string
	Body            []byte
	Signature       string
	SignatureValues int
	DeliveryID      string
	Attempt         string
}

// wellFormed reports the first way a request fails to be the delivery section
// 11.3 describes, or "".
func (h receivedHook) wellFormed() string {
	switch {
	case h.Method != http.MethodPost:
		return fmt.Sprintf("delivered with %s, not POST", h.Method)
	case h.Path != "/hook":
		return fmt.Sprintf("delivered to %s, not the registered path", h.Path)
	case h.ContentType != "application/json":
		return fmt.Sprintf("delivered with Content-Type %q, want application/json", h.ContentType)
	case h.SignatureValues != 1:
		return fmt.Sprintf("carried %d X-Vyshka-Signature headers, want exactly 1", h.SignatureValues)
	}
	return ""
}

type hookReceiver struct {
	url      string
	server   *http.Server
	mu       sync.Mutex
	status   int
	received []receivedHook
}

// startHookReceiver binds a listener and answers every request with status,
// recording each one verbatim for the checks to grade.
func (e Env) startHookReceiver(status int) (*hookReceiver, error) {
	listen := e.WebhookListen
	if listen == "" {
		listen = "127.0.0.1:0"
	}
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, fmt.Errorf("bind webhook receiver on %s: %w", listen, err)
	}

	receiver := &hookReceiver{status: status}
	if e.WebhookAdvertise != "" {
		// The hub reaches the suite through a different address than the suite
		// binds (containers, remote hubs); the operator supplies the base URL.
		receiver.url = strings.TrimRight(e.WebhookAdvertise, "/") + "/hook"
	} else {
		receiver.url = "http://" + listener.Addr().String() + "/hook"
	}
	receiver.server = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		contentType := r.Header.Get("Content-Type")
		if mediaType, _, err := mime.ParseMediaType(contentType); err == nil {
			contentType = mediaType
		}
		receiver.mu.Lock()
		receiver.received = append(receiver.received, receivedHook{
			Method:          r.Method,
			Path:            r.URL.Path,
			ContentType:     contentType,
			Body:            body,
			Signature:       r.Header.Get("X-Vyshka-Signature"),
			SignatureValues: len(r.Header.Values("X-Vyshka-Signature")),
			DeliveryID:      r.Header.Get("X-Vyshka-Delivery"),
			Attempt:         r.Header.Get("X-Vyshka-Attempt"),
		})
		status := receiver.status
		receiver.mu.Unlock()
		w.WriteHeader(status)
	})}
	go func() { _ = receiver.server.Serve(listener) }()
	return receiver, nil
}

func (r *hookReceiver) close() { _ = r.server.Close() }

func (r *hookReceiver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.received)
}

func (r *hookReceiver) get(index int) receivedHook {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.received[index]
}

// await waits until the receiver has accepted at least n deliveries.
func (r *hookReceiver) await(ctx context.Context, n int, patience time.Duration, what string) error {
	deadline := time.Now().Add(patience)
	for time.Now().Before(deadline) {
		if r.count() >= n {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("the receiver saw %d deliveries after %s waiting for %s", r.count(), patience, what)
}

// hookPayload is the generic-json delivery body (spec section 11.3),
// hand-written like every wire shape in this suite.
type hookPayload struct {
	DeliveryID string          `json:"deliveryId"`
	WebhookID  string          `json:"webhookId"`
	Type       string          `json:"type"`
	ServerID   string          `json:"serverId"`
	EventID    string          `json:"eventId"`
	OccurredAt string          `json:"occurredAt"`
	Data       json.RawMessage `json:"data"`
}

// firstOfType returns the first received delivery whose payload carries the
// wanted type, so link checks are immune to stray traffic.
func (r *hookReceiver) firstOfType(wanted string) (receivedHook, hookPayload, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, hook := range r.received {
		var payload hookPayload
		if json.Unmarshal(hook.Body, &payload) == nil && payload.Type == wanted {
			return hook, payload, true
		}
	}
	return receivedHook{}, hookPayload{}, false
}

func (r *hookReceiver) countOfType(wanted string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, hook := range r.received {
		var payload hookPayload
		if json.Unmarshal(hook.Body, &payload) == nil && payload.Type == wanted {
			count++
		}
	}
	return count
}

// awaitType waits for a delivery of one payload type.
func (r *hookReceiver) awaitType(ctx context.Context, wanted string, patience time.Duration) (receivedHook, hookPayload, error) {
	deadline := time.Now().Add(patience)
	for time.Now().Before(deadline) {
		if hook, payload, found := r.firstOfType(wanted); found {
			return hook, payload, nil
		}
		select {
		case <-ctx.Done():
			return receivedHook{}, hookPayload{}, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return receivedHook{}, hookPayload{}, fmt.Errorf("no %s delivery arrived within %s", wanted, patience)
}

// verifySignature checks the section 11.4 signature against the shared secret.
func verifySignature(hook receivedHook, secret string) error {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(hook.Body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if hook.Signature != want {
		return fmt.Errorf("X-Vyshka-Signature is %q; HMAC-SHA256 of the body under the registration secret is %q (section 11.4)",
			hook.Signature, want)
	}
	return nil
}

// Admin API shapes and helpers for the webhook routes (spec section 11.2).

type webhookRecord struct {
	ID        string   `json:"id"`
	URL       string   `json:"url"`
	Events    []string `json:"events"`
	ServerIDs []string `json:"serverIds"`
	Template  string   `json:"template"`
	CreatedAt string   `json:"createdAt"`
}

type registeredWebhook struct {
	Webhook webhookRecord `json:"webhook"`
	Secret  string        `json:"secret"`
}

func (e Env) registerWebhook(ctx context.Context, body map[string]any) (registeredWebhook, error) {
	var registered registeredWebhook
	if err := e.expect(ctx, http.MethodPost, "/api/v1/webhooks", e.AdminToken,
		body, http.StatusCreated, &registered); err != nil {
		return registeredWebhook{}, err
	}
	if registered.Webhook.ID == "" {
		return registeredWebhook{}, fmt.Errorf("register webhook: response carried no webhook.id")
	}
	if registered.Secret == "" {
		return registeredWebhook{}, fmt.Errorf("register webhook: response carried no secret; the signing secret is returned exactly once, here (section 11.2)")
	}
	return registered, nil
}

func (e Env) deleteWebhook(ctx context.Context, webhookID string) error {
	return e.expect(ctx, http.MethodDelete, "/api/v1/webhooks/"+webhookID,
		e.AdminToken, nil, http.StatusNoContent, nil)
}

type deliveryRecord struct {
	ID            string  `json:"id"`
	Type          string  `json:"type"`
	ServerID      string  `json:"serverId"`
	State         string  `json:"state"`
	Attempts      int     `json:"attempts"`
	LastStatus    *int    `json:"lastStatus"`
	LastError     string  `json:"lastError"`
	CreatedAt     string  `json:"createdAt"`
	NextAttemptAt *string `json:"nextAttemptAt"`
	DeliveredAt   *string `json:"deliveredAt"`
}

func (e Env) webhookDeliveries(ctx context.Context, webhookID string) ([]deliveryRecord, error) {
	var response struct {
		Deliveries []deliveryRecord `json:"deliveries"`
	}
	err := e.expect(ctx, http.MethodGet, "/api/v1/webhooks/"+webhookID+"/deliveries",
		e.AdminToken, nil, http.StatusOK, &response)
	return response.Deliveries, err
}

// awaitDelivery waits for the record of a delivery matching pred.
func (e Env) awaitDelivery(ctx context.Context, webhookID string, patience time.Duration,
	what string, pred func(deliveryRecord) bool) (deliveryRecord, error) {
	deadline := time.Now().Add(patience)
	for {
		deliveries, err := e.webhookDeliveries(ctx, webhookID)
		if err != nil {
			return deliveryRecord{}, err
		}
		for _, delivery := range deliveries {
			if pred(delivery) {
				return delivery, nil
			}
		}
		if time.Now().After(deadline) {
			return deliveryRecord{}, fmt.Errorf("no delivery record matching %s within %s (records: %+v)",
				what, patience, deliveries)
		}
		select {
		case <-ctx.Done():
			return deliveryRecord{}, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// uniqueWebhookEventType mints an event type no other check's webhook matches.
func uniqueWebhookEventType() string {
	return "conformance-webhook.probe-" + strconv.FormatInt(unknownTypeCounter.Add(1), 10)
}

// ---- the checks ----

func checkWebhookRegister(ctx context.Context, env Env) error {
	// Refusals first: each one must be a named refusal, never a webhook that
	// silently matches nothing (section 11.2).
	if err := env.expectError(ctx, http.MethodPost, "/api/v1/webhooks", env.AdminToken,
		map[string]any{}, http.StatusBadRequest, "bad_request"); err != nil {
		return fmt.Errorf("a registration without url must be refused: %w", err)
	}
	if err := env.expectError(ctx, http.MethodPost, "/api/v1/webhooks", env.AdminToken,
		map[string]any{"url": "ftp://example.net/hook"}, http.StatusBadRequest, "bad_request"); err != nil {
		return fmt.Errorf("a non-http url must be refused: %w", err)
	}
	if err := env.expectError(ctx, http.MethodPost, "/api/v1/webhooks", env.AdminToken,
		map[string]any{"url": "http://127.0.0.1:9/hook", "events": []string{"core.*.death"}},
		http.StatusBadRequest, "bad_request"); err != nil {
		return fmt.Errorf("a pattern outside the section 10.1 grammar must be refused: %w", err)
	}
	if err := env.expectError(ctx, http.MethodPost, "/api/v1/webhooks", env.AdminToken,
		map[string]any{"url": "http://127.0.0.1:9/hook", "serverIds": []string{"conformance-no-such-server"}},
		http.StatusNotFound, "not_found"); err != nil {
		return fmt.Errorf("a serverIds entry naming no server must be refused: %w", err)
	}

	registered, err := env.registerWebhook(ctx, map[string]any{
		"url": "http://127.0.0.1:9/hook", "events": []string{"conformance-webhook.*"},
	})
	if err != nil {
		return err
	}
	defer env.deleteWebhook(context.WithoutCancel(ctx), registered.Webhook.ID)
	if registered.Webhook.Template == "" {
		return fmt.Errorf("the webhook view carries no template; generic-json is the default (section 11.2)")
	}

	// The secret is returned once. Rather than guessing which field name a
	// leak would hide under, the raw bytes of every later read are searched
	// for the secret's value itself.
	for _, path := range []string{
		"/api/v1/webhooks",
		"/api/v1/webhooks/" + registered.Webhook.ID + "/deliveries",
	} {
		response, body, err := env.do(ctx, http.MethodGet, path, env.AdminToken, nil)
		if err != nil {
			return fmt.Errorf("GET %s: %w", path, err)
		}
		if response.StatusCode != http.StatusOK {
			return fmt.Errorf("GET %s: status %d, want 200", path, response.StatusCode)
		}
		if strings.Contains(string(body), registered.Secret) {
			return fmt.Errorf("GET %s carries the signing secret; it is returned only at registration (section 11.2)", path)
		}
	}
	var listed struct {
		Webhooks []webhookRecord `json:"webhooks"`
	}
	if err := env.expect(ctx, http.MethodGet, "/api/v1/webhooks", env.AdminToken,
		nil, http.StatusOK, &listed); err != nil {
		return err
	}
	found := false
	for _, webhook := range listed.Webhooks {
		if webhook.ID == registered.Webhook.ID {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("GET /api/v1/webhooks does not list the webhook just registered")
	}

	if err := env.deleteWebhook(ctx, registered.Webhook.ID); err != nil {
		return err
	}
	if err := env.expectError(ctx, http.MethodDelete, "/api/v1/webhooks/"+registered.Webhook.ID,
		env.AdminToken, nil, http.StatusNotFound, "not_found"); err != nil {
		return fmt.Errorf("deleting a deleted webhook must answer not_found: %w", err)
	}
	return nil
}

func checkWebhookScope(ctx context.Context, env Env) error {
	// Every webhook route refuses a token without webhooks:manage, not just
	// one of them.
	narrow, err := env.mintToken(ctx, "conformance: no webhook scope", "servers:read")
	if err != nil {
		return err
	}
	refusals := []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/api/v1/webhooks", nil},
		{http.MethodPost, "/api/v1/webhooks", map[string]any{"url": "http://127.0.0.1:9/hook"}},
		{http.MethodDelete, "/api/v1/webhooks/conformance-no-such-webhook", nil},
		{http.MethodGet, "/api/v1/webhooks/conformance-no-such-webhook/deliveries", nil},
	}
	for _, refusal := range refusals {
		if err := env.expectError(ctx, refusal.method, refusal.path, narrow.Secret,
			refusal.body, http.StatusForbidden, "forbidden"); err != nil {
			return fmt.Errorf("%s %s without webhooks:manage was not refused: %w",
				refusal.method, refusal.path, err)
		}
	}

	// webhooks:manage alone is a delivery capability, not a read grant: a
	// subscription the token's grants do not cover is refused, because a
	// webhook is a standing export of what it matches (section 11.2).
	manageOnly, err := env.mintToken(ctx, "conformance: manage only", "webhooks:manage")
	if err != nil {
		return err
	}
	if err := env.expectError(ctx, http.MethodPost, "/api/v1/webhooks", manageOnly.Secret,
		map[string]any{"url": "http://127.0.0.1:9/hook", "events": []string{"conformance-webhook.scope-probe"}},
		http.StatusForbidden, "forbidden"); err != nil {
		return fmt.Errorf("webhooks:manage alone subscribed to telemetry it cannot read: %w", err)
	}

	manager, err := env.mintToken(ctx, "conformance: webhook manager",
		"webhooks:manage", "events:read:conformance-webhook.*")
	if err != nil {
		return err
	}
	var response struct {
		Webhook webhookRecord `json:"webhook"`
	}
	if err := env.expect(ctx, http.MethodPost, "/api/v1/webhooks", manager.Secret,
		map[string]any{"url": "http://127.0.0.1:9/hook", "events": []string{"conformance-webhook.scope-probe"}},
		http.StatusCreated, &response); err != nil {
		return fmt.Errorf("a token whose grants cover its subscription could not register: %w", err)
	}
	return env.deleteWebhook(ctx, response.Webhook.ID)
}

func checkWebhookSignedDelivery(ctx context.Context, env Env) error {
	receiver, err := env.startHookReceiver(http.StatusNoContent)
	if err != nil {
		return err
	}
	defer receiver.close()

	plugin, err := env.newFakePlugin(ctx, "conformance:webhook-delivery", shortPollTimeoutSeconds)
	if err != nil {
		return err
	}
	serverID := plugin.Server.Server.ID
	eventType := uniqueWebhookEventType()

	registered, err := env.registerWebhook(ctx, map[string]any{
		"url": receiver.url, "events": []string{eventType}, "serverIds": []string{serverID},
	})
	if err != nil {
		return err
	}
	defer env.deleteWebhook(context.WithoutCancel(ctx), registered.Webhook.ID)

	if _, err := plugin.sendEvents(ctx, map[string]any{
		"t": eventType, "data": map[string]any{"probe": true},
	}); err != nil {
		return err
	}
	if err := receiver.await(ctx, 1, 30*time.Second, "the signed delivery"); err != nil {
		return fmt.Errorf("%w; a stored event matching a webhook owes it a delivery (section 11.3)", err)
	}

	hook := receiver.get(0)
	if fault := hook.wellFormed(); fault != "" {
		return fmt.Errorf("the delivery was %s (section 11.3)", fault)
	}
	if err := verifySignature(hook, registered.Secret); err != nil {
		return err
	}
	var payload hookPayload
	if err := json.Unmarshal(hook.Body, &payload); err != nil {
		return fmt.Errorf("the delivery body is not the generic-json shape: %w", err)
	}
	if payload.Type != eventType || payload.ServerID != serverID {
		return fmt.Errorf("the payload names %q on %q; want %q on %q (section 11.3)",
			payload.Type, payload.ServerID, eventType, serverID)
	}
	if payload.WebhookID != registered.Webhook.ID {
		return fmt.Errorf("the payload names webhook %q, want %q", payload.WebhookID, registered.Webhook.ID)
	}
	if payload.DeliveryID == "" || payload.DeliveryID != hook.DeliveryID {
		return fmt.Errorf("deliveryId %q and X-Vyshka-Delivery %q must agree (section 11.3)",
			payload.DeliveryID, hook.DeliveryID)
	}
	if payload.EventID == "" {
		return fmt.Errorf("a telemetry delivery carries the stored event's id (section 11.3)")
	}
	if _, err := time.Parse(time.RFC3339, payload.OccurredAt); err != nil {
		return fmt.Errorf("occurredAt %q is not RFC 3339", payload.OccurredAt)
	}
	if hook.Attempt != "1" {
		return fmt.Errorf("X-Vyshka-Attempt is %q on a first delivery, want 1", hook.Attempt)
	}

	// The delivery record agrees (section 11.5).
	if _, err := env.awaitDelivery(ctx, registered.Webhook.ID, 15*time.Second, "state delivered",
		func(delivery deliveryRecord) bool { return delivery.State == "delivered" }); err != nil {
		return err
	}

	// A non-matching event must not fire. The settle window bounds the
	// negative: a delivery that has not arrived seconds after a matching one
	// did is a delivery that was correctly never made.
	if _, err := plugin.sendEvents(ctx, map[string]any{
		"t": uniqueWebhookEventType(), "data": map[string]any{}}); err != nil {
		return err
	}
	time.Sleep(3 * time.Second)
	if receiver.count() != 1 {
		return fmt.Errorf("the receiver saw %d deliveries after an event outside the filter; filters narrow, or they are decoration (section 11.2)",
			receiver.count())
	}
	return nil
}

func checkWebhookRetryVisible(ctx context.Context, env Env) error {
	receiver, err := env.startHookReceiver(http.StatusInternalServerError)
	if err != nil {
		return err
	}
	defer receiver.close()

	plugin, err := env.newFakePlugin(ctx, "conformance:webhook-retry", shortPollTimeoutSeconds)
	if err != nil {
		return err
	}
	eventType := uniqueWebhookEventType()
	registered, err := env.registerWebhook(ctx, map[string]any{
		"url": receiver.url, "events": []string{eventType},
		"serverIds": []string{plugin.Server.Server.ID},
	})
	if err != nil {
		return err
	}
	// Deleting the webhook abandons its pending deliveries, so a hub graded
	// here does not keep retrying against a closed listener.
	defer env.deleteWebhook(context.WithoutCancel(ctx), registered.Webhook.ID)

	sentAt := time.Now()
	if _, err := plugin.sendEvents(ctx, map[string]any{"t": eventType, "data": map[string]any{}}); err != nil {
		return err
	}

	// The schedule bound of section 11.5, graded through the record it makes
	// visible: the first failure cannot predate the event that caused it, so a
	// nextAttemptAt more than 60 s past the send, plus slack for the enqueue
	// and the attempt itself, is a schedule outside the bound.
	failed, err := env.awaitDelivery(ctx, registered.Webhook.ID, 30*time.Second, "the first failed attempt",
		func(delivery deliveryRecord) bool { return delivery.Attempts >= 1 })
	if err != nil {
		return err
	}
	if failed.State == "pending" {
		if failed.NextAttemptAt == nil {
			return fmt.Errorf("a pending delivery reports no nextAttemptAt (section 11.5)")
		}
		scheduled, err := time.Parse(time.RFC3339, *failed.NextAttemptAt)
		if err != nil {
			return fmt.Errorf("nextAttemptAt %q is not RFC 3339", *failed.NextAttemptAt)
		}
		if delay := scheduled.Sub(sentAt); delay > 70*time.Second {
			return fmt.Errorf("the first retry is scheduled %s after the event that failed; section 11.5 requires it scheduled within 60 s of the failure", delay.Round(time.Second))
		}
	}

	if err := receiver.await(ctx, 2, 75*time.Second, "the first retry"); err != nil {
		return fmt.Errorf("%w; a failed delivery is retried (section 11.5)", err)
	}

	first, second := receiver.get(0), receiver.get(1)
	for attempt, hook := range []receivedHook{first, second} {
		if fault := hook.wellFormed(); fault != "" {
			return fmt.Errorf("attempt %d was %s (section 11.3)", attempt+1, fault)
		}
	}
	if string(first.Body) != string(second.Body) {
		return fmt.Errorf("the retry changed the body; every attempt of one delivery sends the same bytes (section 11.3)")
	}
	if first.Signature != second.Signature {
		return fmt.Errorf("the retry changed the signature of an unchanged body (section 11.4)")
	}
	if first.Attempt != "1" || second.Attempt != "2" {
		return fmt.Errorf("X-Vyshka-Attempt ran %q, %q; want 1 then 2 (section 11.3)", first.Attempt, second.Attempt)
	}

	// The failure is visible: attempts counted, last status recorded, state
	// honest (section 11.5). Dead is not required yet; two attempts in, the
	// delivery may legally still be pending its later retries.
	delivery, err := env.awaitDelivery(ctx, registered.Webhook.ID, 10*time.Second, "a visible failure count",
		func(delivery deliveryRecord) bool { return delivery.Attempts >= 2 })
	if err != nil {
		return fmt.Errorf("%w; nothing is dropped without a visible counter (section 11.5)", err)
	}
	if delivery.State != "pending" && delivery.State != "dead" {
		return fmt.Errorf("delivery state %q is not pending or dead", delivery.State)
	}
	if delivery.LastStatus == nil || *delivery.LastStatus != http.StatusInternalServerError {
		return fmt.Errorf("lastStatus does not record the target's 500 (section 11.5): %+v", delivery)
	}
	return nil
}

func checkWebhookLinkTransitions(ctx context.Context, env Env) error {
	receiver, err := env.startHookReceiver(http.StatusOK)
	if err != nil {
		return err
	}
	defer receiver.close()

	// The shortest negotiable pollTimeout keeps the loss threshold, which a hub
	// derives from it, as small as the protocol allows.
	plugin, err := env.newFakePlugin(ctx, "conformance:webhook-link", shortPollTimeoutSeconds)
	if err != nil {
		return err
	}
	serverID := plugin.Server.Server.ID
	if _, err := plugin.pollAndAck(ctx); err != nil {
		return err
	}

	registered, err := env.registerWebhook(ctx, map[string]any{
		"url": receiver.url, "events": []string{"server.link.*"}, "serverIds": []string{serverID},
	})
	if err != nil {
		return err
	}
	defer env.deleteWebhook(context.WithoutCancel(ctx), registered.Webhook.ID)

	// Warm up until the hub has classified the server up, polling as a live
	// plugin would. Going silent before that would let the hub's first
	// classification be down, which section 11.1 makes deliberately silent,
	// and no lost notification would ever be owed. Section 11.1 bounds the
	// classification lag by the same ceiling as the loss itself.
	warmupDeadline := time.Now().Add(30 * time.Second)
	for {
		var record serverRecord
		if err := env.expect(ctx, http.MethodGet, "/api/v1/servers/"+serverID,
			env.AdminToken, nil, http.StatusOK, &record); err != nil {
			return err
		}
		if record.LinkState == "up" {
			break
		}
		if time.Now().After(warmupDeadline) {
			return fmt.Errorf("linkState is %q after 30 s of live polling; a reachable server is classified up within the section 11.1 ceiling, and the state is exposed on the server record", record.LinkState)
		}
		if _, err := plugin.pollAndAck(ctx); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}

	// Fall silent. The plugin stops polling entirely, and the hub owes the
	// webhook a server.link.lost. The wait covers the normative ceiling of
	// section 11.1 (four times the negotiated pollTimeout plus 30 s: 50 s at
	// the 5 s floor) with slack for the hub's own monitoring cadence.
	lostHook, lost, err := receiver.awaitType(ctx, "server.link.lost", 70*time.Second)
	if err != nil {
		return fmt.Errorf("%w; a server silent past the ceiling of four pollTimeouts plus 30 s must have been declared lost (section 11.1)", err)
	}
	if lost.ServerID != serverID {
		return fmt.Errorf("server.link.lost names %q, want %q", lost.ServerID, serverID)
	}
	// Lifecycle deliveries are signed exactly like telemetry deliveries.
	if fault := lostHook.wellFormed(); fault != "" {
		return fmt.Errorf("the server.link.lost delivery was %s (section 11.3)", fault)
	}
	if err := verifySignature(lostHook, registered.Secret); err != nil {
		return err
	}

	// Speak again: one poll must restore the link, and the restoration is a
	// delivery like any other: well-formed and signed.
	if _, err := plugin.pollAndAck(ctx); err != nil {
		return err
	}
	restoredHook, _, err := receiver.awaitType(ctx, "server.link.restored", 15*time.Second)
	if err != nil {
		return fmt.Errorf("%w; a lost server that polls again is restored (section 11.1)", err)
	}
	if fault := restoredHook.wellFormed(); fault != "" {
		return fmt.Errorf("the server.link.restored delivery was %s (section 11.3)", fault)
	}
	if err := verifySignature(restoredHook, registered.Secret); err != nil {
		return err
	}

	if count := receiver.countOfType("server.link.lost"); count != 1 {
		return fmt.Errorf("server.link.lost fired %d times for one outage; transitions fire once per edge (section 11.1)", count)
	}
	return nil
}
