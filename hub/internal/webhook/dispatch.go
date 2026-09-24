// Package webhook is the hub's webhook delivery engine (spec section 11): the
// dispatcher that fans stored notifications out to deliveries, renders and
// signs their bodies, attempts them on the retry schedule, and watches server
// links for the lifecycle notifications. It also holds the filter matching
// grammar the fan-out applies, which the Admin API reuses to decide coverage.
// The Admin API handlers themselves stay in package hub, beside the principal
// and refusal machinery they depend on.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/That1Drifter/vyshka/hub/internal/id"
	"github.com/That1Drifter/vyshka/hub/store"
)

// Dispatcher pacing. The wake channel makes fresh work prompt; the ticker is
// the backstop that also owns retries and the link monitor's cadence.
const (
	dispatcherTick = time.Second
	// notifyBatch bounds one fan-out read; deliverBatch one delivery pass.
	notifyBatch  = 500
	deliverBatch = 50
	// linkCheckInterval is how often reachability is re-evaluated; linkGrace is
	// the slack added on top of twice the negotiated pollTimeout before a quiet
	// server is declared lost (spec section 11.1).
	linkCheckInterval = 5 * time.Second
	linkGrace         = 10 * time.Second
	// pendingDeliveryBound is the per-webhook pending queue bound of section
	// 11.5: at the bound a new delivery is dead on arrival, visibly.
	pendingDeliveryBound = 1000
)

// Lifecycle notification types (spec section 11.1). The audit notification is
// opt-in: only a filter naming the audit namespace matches it.
const (
	ActionCompleted = "action.completed"
	LinkLost        = "server.link.lost"
	LinkRestored    = "server.link.restored"
	AuditRecorded   = "audit.recorded"
)

// TemplateGenericJSON names the default delivery body, the section 11.3
// generic-json shape.
const TemplateGenericJSON = "generic-json"

// envelopeTimestamp renders a timestamp in the envelope `ts` format of spec
// section 4: RFC 3339, UTC, fixed width. It is the hub's wire format, repeated
// here because every timestamp a delivery body carries must read the same as
// the envelopes and the Admin API.
func envelopeTimestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// webhookPayload is the generic-json delivery body (spec section 11.3). It is
// rendered once per delivery and stored, so every attempt sends the same bytes
// under the same signature. ServerID is omitted only for an audit record that
// names no server, the one notification that can concern none.
type webhookPayload struct {
	DeliveryID string          `json:"deliveryId"`
	WebhookID  string          `json:"webhookId"`
	Type       string          `json:"type"`
	ServerID   string          `json:"serverId,omitempty"`
	EventID    string          `json:"eventId,omitempty"`
	OccurredAt string          `json:"occurredAt"`
	Data       json.RawMessage `json:"data"`
}

// notification is one thing to fan out, whatever its source. LandedAt is when
// it became the hub's to tell: an event's receipt, an action's terminal
// instant, a link transition's detection. It exists for the registration
// boundary, where OccurredAt would be wrong because a plugin's claimed
// timestamp can predate anything.
type notification struct {
	Type       string
	ServerID   string
	EventID    string
	OccurredAt time.Time
	LandedAt   time.Time
	Data       json.RawMessage
}

// fanOut crosses notifications with webhooks: one delivery per match, each
// with its payload rendered for the webhook's template and its id minted,
// because the generic payload embeds it. serverNames maps server ids to
// display names for the templates that name the server; nil is tolerated.
// A webhook observes only what landed at or after its registration (spec
// section 11.2); the notified-flag outbox can legally hand this function older
// rows, a migration backlog above all, and the boundary here is what keeps
// them from becoming a backfill.
func fanOut(webhooks []store.Webhook, notifications []notification, serverNames map[string]string) []store.NewWebhookDelivery {
	var deliveries []store.NewWebhookDelivery
	for _, one := range notifications {
		for _, webhook := range webhooks {
			// Timestamps carry millisecond precision, so a notification and a
			// registration in the same millisecond tie; the tie goes to
			// delivery, because this protocol loses duplicates gracefully
			// (at-least-once everywhere) and loses omissions never.
			if one.LandedAt.Before(webhook.CreatedAt) {
				continue
			}
			if !webhookMatches(webhook, one.Type, one.ServerID) {
				continue
			}
			// Redaction runs before the template, so what a path strips
			// cannot come back as a line of prose (spec section 11.2).
			rendered := one
			rendered.Data = redactData(one.Data, webhook.Redact)
			deliveryID := id.New()
			body, err := renderDeliveryBody(webhook, rendered, deliveryID, serverNames[one.ServerID])
			if err != nil {
				// Strings and raw JSON all the way down; this cannot happen.
				continue
			}
			deliveries = append(deliveries, store.NewWebhookDelivery{
				ID:        deliveryID,
				WebhookID: webhook.ID,
				Type:      one.Type,
				ServerID:  one.ServerID,
				Body:      body,
			})
		}
	}
	return deliveries
}

// signWebhookBody computes the section 11.4 signature header value.
func signWebhookBody(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// Config is everything a dispatcher needs. The hub sanitizes RetryDelays and
// DeliveryTimeout before handing them over (spec section 11.5 makes promises
// no configuration may opt out of); the dispatcher uses them as given.
type Config struct {
	// Store holds the webhooks, the notification outboxes, and the delivery
	// record. The dispatcher does not own it: whoever built the dispatcher
	// closes the store, after Close has returned.
	Store *store.Store
	// Log receives structured logs. Nil means slog.Default().
	Log *slog.Logger
	// RetryDelays are the waits between a failed attempt and the next one;
	// total attempts are one more than the number of delays.
	RetryDelays []time.Duration
	// DeliveryTimeout bounds one delivery attempt end to end.
	DeliveryTimeout time.Duration
	// AuditData renders an audit record exactly as GET /api/v1/audit answers
	// it, which is the data of its audit.recorded notification (spec section
	// 11.1). The view is the Admin API's to define, so it is handed in rather
	// than rebuilt here. Nil renders an empty object.
	AuditData func(store.AuditRecord) json.RawMessage
}

// Dispatcher is the webhook delivery engine: it fans stored notifications out
// to deliveries, attempts what is due, and watches server links (spec section
// 11). It owns its loop, its HTTP client, and the base context every in-flight
// attempt derives from.
type Dispatcher struct {
	store           *store.Store
	log             *slog.Logger
	retryDelays     []time.Duration
	deliveryTimeout time.Duration
	auditData       func(store.AuditRecord) json.RawMessage
	// client makes the delivery attempts.
	client *http.Client
	// wake nudges the loop when fresh work landed; stop ends it and done
	// confirms it ended, so Close never races the loop against a store its
	// owner is about to close.
	wake chan struct{}
	stop chan struct{}
	done chan struct{}
	// baseCtx parents every in-flight delivery attempt; Close cancels it so
	// shutdown never waits out a slow webhook target's timeout.
	baseCtx    context.Context
	baseCancel context.CancelFunc

	mu        sync.Mutex
	started   bool
	stopped   bool
	closeOnce sync.Once
}

// NewDispatcher builds a dispatcher over cfg. Nothing runs until Start.
func NewDispatcher(cfg Config) *Dispatcher {
	d := &Dispatcher{
		store:           cfg.Store,
		log:             cfg.Log,
		retryDelays:     cfg.RetryDelays,
		deliveryTimeout: cfg.DeliveryTimeout,
		auditData:       cfg.AuditData,
		client: &http.Client{
			Timeout: cfg.DeliveryTimeout,
			// A 3xx is handed back as the final answer, never followed: a
			// redirect would resend the signed body to an address nobody
			// registered and no audit names (spec section 11.3).
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		wake: make(chan struct{}, 1),
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	if d.log == nil {
		d.log = slog.Default()
	}
	if d.auditData == nil {
		d.auditData = func(store.AuditRecord) json.RawMessage { return json.RawMessage(`{}`) }
	}
	d.baseCtx, d.baseCancel = context.WithCancel(context.Background())
	return d
}

// Start launches the dispatcher loop. It is a no-op on a dispatcher already
// started or already closed.
func (d *Dispatcher) Start() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.started || d.stopped {
		return
	}
	d.started = true
	go d.run()
}

// Nudge wakes the dispatcher without blocking the caller. A full buffer means
// a wake is already owed, which is all a nudge can ask for.
func (d *Dispatcher) Nudge() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Close stops the loop, cancels in-flight delivery attempts, and waits for the
// loop to end, so the store can be closed once it returns. It is idempotent,
// and safe on a dispatcher that was never started.
func (d *Dispatcher) Close() {
	d.closeOnce.Do(func() {
		d.mu.Lock()
		d.stopped = true
		started := d.started
		d.mu.Unlock()

		close(d.stop)
		d.baseCancel()
		if started {
			<-d.done
		}
	})
}

// run owns the webhook pipeline: fanning stored notifications out to
// deliveries, attempting what is due, and watching server links. It is a
// separate goroutine from the hub's maintenance loop because a slow webhook
// target may legitimately hold a request open for the delivery timeout, and
// nothing that slow may share a loop with action expiry.
func (d *Dispatcher) run() {
	defer close(d.done)

	ticker := time.NewTicker(dispatcherTick)
	defer ticker.Stop()
	var lastLinkCheck time.Time

	for {
		select {
		case <-d.stop:
			return
		case <-d.wake:
		case <-ticker.C:
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		again := d.dispatchPass(ctx, &lastLinkCheck)
		cancel()
		if again {
			// A full batch means more is waiting; run again now rather than
			// letting a backlog drain at one batch per tick.
			d.Nudge()
		}
	}
}

// dispatchPass runs one round of the pipeline and reports whether it drained a
// full batch anywhere, which means another round is owed immediately.
func (d *Dispatcher) dispatchPass(ctx context.Context, lastLinkCheck *time.Time) bool {
	// Telemetry fan-out (spec section 11.1). Events are read and flagged even
	// with no webhooks registered, so the outbox never accumulates a backlog;
	// the webhooks the fan-out matches against are read inside the same
	// transaction, so a registration serializes against the pass instead of
	// racing it, and the LandedAt boundary in fanOut keeps whatever backlog
	// does exist from becoming a backfill.
	eventsMarked, err := d.store.NotifyEvents(ctx, notifyBatch, func(events []store.Event, webhooks []store.Webhook, names map[string]string) []store.NewWebhookDelivery {
		notifications := make([]notification, 0, len(events))
		for _, event := range events {
			notifications = append(notifications, notification{
				Type:       event.Type,
				ServerID:   event.ServerID,
				EventID:    event.ID,
				OccurredAt: event.OccurredAt,
				LandedAt:   event.ReceivedAt,
				Data:       event.Data,
			})
		}
		return fanOut(webhooks, notifications, names)
	}, pendingDeliveryBound)
	if err != nil {
		d.log.Error("webhook pass could not fan out events", "error", err.Error())
		return false
	}

	// action.completed fan-out: every action that reached a terminal state,
	// expiry included (spec section 11.1).
	actionsMarked, err := d.store.NotifyFinishedActions(ctx, notifyBatch, func(actions []store.Action, webhooks []store.Webhook, names map[string]string) []store.NewWebhookDelivery {
		notifications := make([]notification, 0, len(actions))
		for _, action := range actions {
			notifications = append(notifications, notification{
				Type:       ActionCompleted,
				ServerID:   action.ServerID,
				OccurredAt: finishedAt(action),
				LandedAt:   finishedAt(action),
				Data:       actionNotificationData(action),
			})
		}
		return fanOut(webhooks, notifications, names)
	}, pendingDeliveryBound)
	if err != nil {
		d.log.Error("webhook pass could not fan out finished actions", "error", err.Error())
		return false
	}

	// audit.recorded fan-out: every audit record written, from the outbox
	// its insert filled in the same transaction (spec section 11.1).
	auditMarked, err := d.store.NotifyAudit(ctx, notifyBatch, func(records []store.AuditRecord, webhooks []store.Webhook, names map[string]string) []store.NewWebhookDelivery {
		notifications := make([]notification, 0, len(records))
		for _, record := range records {
			notifications = append(notifications, notification{
				Type:       AuditRecorded,
				ServerID:   record.ServerID,
				OccurredAt: record.At,
				LandedAt:   record.At,
				Data:       d.auditData(record),
			})
		}
		return fanOut(webhooks, notifications, names)
	}, pendingDeliveryBound)
	if err != nil {
		d.log.Error("webhook pass could not fan out audit records", "error", err.Error())
		return false
	}

	if time.Since(*lastLinkCheck) >= linkCheckInterval {
		*lastLinkCheck = time.Now()
		d.checkLinks(ctx)
	}

	attempted := d.deliverDue(ctx)
	return eventsMarked == notifyBatch || actionsMarked == notifyBatch || auditMarked == notifyBatch ||
		attempted == deliverBatch
}

// finishedAt is the action's terminal instant, with its deadline standing in
// for the rare row expired by the lazy read path before finished_at was set.
func finishedAt(action store.Action) time.Time {
	if action.FinishedAt != nil {
		return *action.FinishedAt
	}
	return action.ExpiresAt
}

// actionNotificationData is the `data` of an action.completed notification:
// the record an operator would otherwise fetch by id (spec section 11.1).
func actionNotificationData(action store.Action) json.RawMessage {
	payload := map[string]any{
		"actionId":   action.ID,
		"code":       action.Code,
		"state":      action.State,
		"ok":         action.OK,
		"createdAt":  envelopeTimestamp(action.CreatedAt),
		"finishedAt": envelopeTimestamp(finishedAt(action)),
	}
	if action.Error != "" {
		payload["error"] = action.Error
	}
	if action.DurationMs != nil {
		payload["durationMs"] = *action.DurationMs
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return encoded
}

// checkLinks evaluates every server's reachability and fires the section 11.1
// link transitions. Reachable means a live session whose traffic is fresher
// than twice its negotiated pollTimeout plus grace: the longest silence a
// healthy plugin can produce, with slack for scheduling and clock coarseness.
//
// A server's first classification out of unknown is silent in both directions:
// a hub coming up over an upgraded or long-idle database must not announce a
// fleet of losses for servers that merely predate its bookkeeping, and a first
// sighting is not a restoration (spec section 11.1). Transitions fire only on
// the up <-> down edges, and every transition is guarded on the exact
// last_seen_at it was computed from, so a poll racing the monitor voids the
// decision instead of turning into a false lost/restored pair.
func (d *Dispatcher) checkLinks(ctx context.Context) {
	candidates, err := d.store.LinkCandidates(ctx)
	if err != nil {
		d.log.Error("link check could not read servers", "error", err.Error())
		return
	}
	now := time.Now().UTC()

	for _, candidate := range candidates {
		if candidate.LinkState == store.LinkUnknown && candidate.LastSeenAt == nil {
			// Never heard from: no link to gain or lose.
			continue
		}
		reachable := false
		if candidate.PollTimeoutSeconds != nil && candidate.LastSeenAt != nil {
			threshold := 2*time.Duration(*candidate.PollTimeoutSeconds)*time.Second + linkGrace
			reachable = now.Sub(*candidate.LastSeenAt) <= threshold
		}
		target := store.LinkDown
		if reachable {
			target = store.LinkUp
		}
		if target == candidate.LinkState {
			continue
		}

		// The deliveries are built inside the transition's transaction against
		// the webhooks as stored there, so a registration racing the monitor
		// serializes instead of losing its first link notification.
		var build func([]store.Webhook, map[string]string) []store.NewWebhookDelivery
		if candidate.LinkState == store.LinkDown && target == store.LinkUp {
			build = func(webhooks []store.Webhook, names map[string]string) []store.NewWebhookDelivery {
				return fanOut(webhooks, []notification{
					linkNotification(LinkRestored, candidate, now),
				}, names)
			}
		}
		if candidate.LinkState == store.LinkUp && target == store.LinkDown {
			build = func(webhooks []store.Webhook, names map[string]string) []store.NewWebhookDelivery {
				return fanOut(webhooks, []notification{
					linkNotification(LinkLost, candidate, now),
				}, names)
			}
		}

		applied, err := d.store.ApplyLinkTransition(ctx, candidate.ServerID,
			candidate.LinkState, target, candidate.LastSeenAt, build, pendingDeliveryBound)
		if err != nil {
			d.log.Error("link transition failed", "serverId", candidate.ServerID, "error", err.Error())
			continue
		}
		if !applied {
			continue
		}
		switch {
		case candidate.LinkState == store.LinkUp && target == store.LinkDown:
			d.log.Warn("server link lost", "serverId", candidate.ServerID)
		case candidate.LinkState == store.LinkDown && target == store.LinkUp:
			d.log.Info("server link restored", "serverId", candidate.ServerID)
		default:
			d.log.Info("server link classified", "serverId", candidate.ServerID, "state", target)
		}
	}
}

func linkNotification(notificationType string, candidate store.LinkCandidate, now time.Time) notification {
	data := map[string]any{"lastSeenAt": nil}
	if candidate.LastSeenAt != nil {
		data["lastSeenAt"] = envelopeTimestamp(*candidate.LastSeenAt)
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		encoded = []byte(`{}`)
	}
	return notification{
		Type:       notificationType,
		ServerID:   candidate.ServerID,
		OccurredAt: now,
		LandedAt:   now,
		Data:       encoded,
	}
}

// deliveryWorkers bounds how many delivery attempts run at once. One slow
// target must not be able to hold every other webhook's retry hostage for its
// timeout, and an unbounded fan-out must not exist at all.
const deliveryWorkers = 8

// deliverDue attempts every pending delivery whose time has come, across a
// bounded pool. Passes are serialized by the dispatcher loop, so no delivery
// is ever attempted twice concurrently; the pool only overlaps distinct
// deliveries within one pass.
func (d *Dispatcher) deliverDue(ctx context.Context) int {
	due, err := d.store.DueWebhookDeliveries(ctx, time.Now().UTC(), deliverBatch)
	if err != nil {
		d.log.Error("webhook pass could not read due deliveries", "error", err.Error())
		return 0
	}
	if len(due) == 0 {
		return 0
	}

	work := make(chan store.DueDelivery)
	var wg sync.WaitGroup
	for range min(deliveryWorkers, len(due)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for delivery := range work {
				d.attemptDelivery(delivery)
			}
		}()
	}
feeding:
	for _, delivery := range due {
		select {
		case work <- delivery:
		case <-d.stop:
			// Shutdown mid-pass: what was not fed stays pending and due, and
			// the next boot's dispatcher picks it up.
			break feeding
		}
	}
	close(work)
	wg.Wait()
	return len(due)
}

// attemptDelivery makes one attempt and books the outcome: delivered on 2xx,
// retried on failure while the schedule lasts, dead after (spec section 11.5).
//
// The request context derives from the dispatcher's base context, so shutdown
// aborts in-flight attempts instead of waiting out their timeouts; the
// recording context is minted fresh **after** the attempt finishes, from the
// background, because an outcome that happened must be recorded even during a
// slow attempt or a shutdown, and a recording deadline that started ticking
// before the request would expire under a target slower than ten seconds and
// leave a completed attempt looking like it never ran.
func (d *Dispatcher) attemptDelivery(due store.DueDelivery) {
	body := []byte(due.Delivery.Body)

	// The attempt begins by being booked, not by being selected into the
	// batch: the store locks the webhook row, refuses if it is paused, refuses
	// if the delivery was replayed or removed since the batch was read, and
	// counts the attempt, answering with the URL and secret as of that instant.
	// That booking is the boundary the protocol draws (spec section 11.2): a
	// pause or a URL edit that commits before it holds, and one that commits
	// after it finds this attempt already begun.
	beginCtx, cancelBegin := context.WithTimeout(d.baseCtx, 10*time.Second)
	begun, err := d.store.BeginDeliveryAttempt(beginCtx, due.Delivery.ID, due.Delivery.Generation)
	cancelBegin()
	switch {
	case errors.Is(err, store.ErrWebhookPaused):
		// Left pending and due: the resume's pass picks it up.
		return
	case errors.Is(err, store.ErrStaleAttempt):
		// Replayed, finished, or deleted since the batch was read; whatever
		// it became, it is not the attempt this batch owed.
		return
	case err != nil:
		d.log.Error("delivery attempt could not be booked",
			"webhookId", due.Delivery.WebhookID, "deliveryId", due.Delivery.ID, "error", err.Error())
		return
	}
	attempt := begun.Attempt

	recording := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.Background(), 10*time.Second)
	}
	fail := func(status *int, message string) {
		ctx, cancel := recording()
		defer cancel()
		d.recordFailure(ctx, due, attempt, status, message)
	}

	requestCtx, cancel := context.WithTimeout(d.baseCtx, d.deliveryTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, begun.URL, bytes.NewReader(body))
	if err != nil {
		fail(nil, "request could not be built: "+err.Error())
		return
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Vyshka-Delivery", due.Delivery.ID)
	request.Header.Set("X-Vyshka-Attempt", strconv.Itoa(attempt))
	request.Header.Set("X-Vyshka-Signature", signWebhookBody(begun.Secret, body))

	response, err := d.client.Do(request)
	if err != nil {
		if d.baseCtx.Err() != nil {
			// Shutdown aborted the wait, not the target: the request may
			// well have reached the receiver, which is why the booking
			// stands and the count is not given back (an attempt that may
			// have been seen was made). No outcome is booked and the
			// schedule does not advance: the row stays pending and due, and
			// the next boot's dispatcher tries it again. A restart therefore
			// costs one slot of the retry schedule, never a dead letter on
			// its own.
			return
		}
		fail(nil, "delivery failed: "+err.Error())
		return
	}
	// The response body is drained and discarded: webhooks are push-only, and
	// reading keeps the connection reusable.
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<16))
	response.Body.Close()

	if response.StatusCode >= 200 && response.StatusCode < 300 {
		ctx, cancelRecord := recording()
		defer cancelRecord()
		if err := d.store.RecordDeliverySuccess(ctx, due.Delivery.ID, due.Delivery.Generation, response.StatusCode); err != nil {
			d.logOutcomeNotBooked(due, err)
			return
		}
		d.log.Info("webhook delivered",
			"webhookId", due.Delivery.WebhookID, "deliveryId", due.Delivery.ID,
			"type", due.Delivery.Type, "attempt", attempt, "status", response.StatusCode)
		return
	}

	status := response.StatusCode
	message := "status " + strconv.Itoa(status)
	if status >= 300 && status < 400 {
		// Redirects are never followed (spec section 11.3): the client is
		// configured to hand back the 3xx itself, and it books as a failure.
		message += " (redirects are not followed)"
	}
	fail(&status, message)
}

// logOutcomeNotBooked reports an outcome the store did not book. A stale
// attempt is the expected case: the delivery was replayed while the attempt
// was in flight, and the replay's attempt is the one that counts (spec
// section 11.5), so the outcome is discarded on purpose. Anything else is a
// store failure.
func (d *Dispatcher) logOutcomeNotBooked(due store.DueDelivery, err error) {
	if errors.Is(err, store.ErrStaleAttempt) {
		d.log.Info("delivery outcome discarded: the delivery was replayed or removed during the attempt",
			"webhookId", due.Delivery.WebhookID, "deliveryId", due.Delivery.ID,
			"generation", due.Delivery.Generation)
		return
	}
	d.log.Error("delivery outcome could not be recorded",
		"deliveryId", due.Delivery.ID, "error", err.Error())
}

// recordFailure books one failed attempt, scheduling the retry the section
// 11.5 schedule owes it or declaring the delivery dead when none remains.
func (d *Dispatcher) recordFailure(ctx context.Context, due store.DueDelivery, attempt int, status *int, message string) {
	delays := d.retryDelays
	var nextAttemptAt *time.Time
	// Attempt N failing consumes delays[N-1] for the next try; past the end,
	// the schedule is exhausted.
	if attempt-1 < len(delays) {
		next := time.Now().UTC().Add(delays[attempt-1])
		nextAttemptAt = &next
	}

	if err := d.store.RecordDeliveryFailure(ctx, due.Delivery.ID, due.Delivery.Generation, status, message, nextAttemptAt); err != nil {
		d.logOutcomeNotBooked(due, err)
		return
	}
	if nextAttemptAt == nil {
		d.log.Warn("webhook delivery dead-lettered",
			"webhookId", due.Delivery.WebhookID, "deliveryId", due.Delivery.ID,
			"type", due.Delivery.Type, "attempts", attempt, "lastError", message)
		return
	}
	d.log.Warn("webhook delivery failed, retry scheduled",
		"webhookId", due.Delivery.WebhookID, "deliveryId", due.Delivery.ID,
		"type", due.Delivery.Type, "attempt", attempt, "lastError", message,
		"nextAttemptAt", envelopeTimestamp(*nextAttemptAt))
}
