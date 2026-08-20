package hub

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
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
)

// Lifecycle notification types (spec section 11.1).
const (
	notifyActionCompleted   = "action.completed"
	notifyServerLinkLost    = "server.link.lost"
	notifyServerLinkRestore = "server.link.restored"
)

// webhookPayload is the generic-json delivery body (spec section 11.3). It is
// rendered once per delivery and stored, so every attempt sends the same bytes
// under the same signature.
type webhookPayload struct {
	DeliveryID string          `json:"deliveryId"`
	WebhookID  string          `json:"webhookId"`
	Type       string          `json:"type"`
	ServerID   string          `json:"serverId"`
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
// with its payload rendered and its id minted, because the payload embeds it.
// A webhook observes only what landed at or after its registration (spec
// section 11.2); the notified-flag outbox can legally hand this function older
// rows, a migration backlog above all, and the boundary here is what keeps
// them from becoming a backfill.
func fanOut(webhooks []store.Webhook, notifications []notification) []store.NewWebhookDelivery {
	var deliveries []store.NewWebhookDelivery
	for _, one := range notifications {
		data := one.Data
		if len(data) == 0 {
			data = json.RawMessage(`{}`)
		}
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
			deliveryID := id.New()
			body, err := json.Marshal(webhookPayload{
				DeliveryID: deliveryID,
				WebhookID:  webhook.ID,
				Type:       one.Type,
				ServerID:   one.ServerID,
				EventID:    one.EventID,
				OccurredAt: envelopeTimestamp(one.OccurredAt),
				Data:       data,
			})
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

// nudgeWebhooks wakes the dispatcher without blocking the caller. A full
// buffer means a wake is already owed, which is all a nudge can ask for.
func (s *Server) nudgeWebhooks() {
	select {
	case s.webhookWake <- struct{}{}:
	default:
	}
}

// runWebhookDispatcher owns the webhook pipeline: fanning stored notifications
// out to deliveries, attempting what is due, and watching server links. It is
// a separate goroutine from runMaintenance because a slow webhook target may
// legitimately hold a request open for the delivery timeout, and nothing that
// slow may share a loop with action expiry.
func (s *Server) runWebhookDispatcher() {
	defer close(s.dispatcherDone)

	ticker := time.NewTicker(dispatcherTick)
	defer ticker.Stop()
	var lastLinkCheck time.Time

	for {
		select {
		case <-s.stopSweeper:
			return
		case <-s.webhookWake:
		case <-ticker.C:
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		again := s.dispatchPass(ctx, &lastLinkCheck)
		cancel()
		if again {
			// A full batch means more is waiting; run again now rather than
			// letting a backlog drain at one batch per tick.
			s.nudgeWebhooks()
		}
	}
}

// dispatchPass runs one round of the pipeline and reports whether it drained a
// full batch anywhere, which means another round is owed immediately.
func (s *Server) dispatchPass(ctx context.Context, lastLinkCheck *time.Time) bool {
	// Telemetry fan-out (spec section 11.1). Events are read and flagged even
	// with no webhooks registered, so the outbox never accumulates a backlog;
	// the webhooks the fan-out matches against are read inside the same
	// transaction, so a registration serializes against the pass instead of
	// racing it, and the LandedAt boundary in fanOut keeps whatever backlog
	// does exist from becoming a backfill.
	eventsMarked, err := s.store.NotifyEvents(ctx, notifyBatch, func(events []store.Event, webhooks []store.Webhook) []store.NewWebhookDelivery {
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
		return fanOut(webhooks, notifications)
	}, pendingDeliveryBound)
	if err != nil {
		s.log.Error("webhook pass could not fan out events", "error", err.Error())
		return false
	}

	// action.completed fan-out: every action that reached a terminal state,
	// expiry included (spec section 11.1).
	actionsMarked, err := s.store.NotifyFinishedActions(ctx, notifyBatch, func(actions []store.Action, webhooks []store.Webhook) []store.NewWebhookDelivery {
		notifications := make([]notification, 0, len(actions))
		for _, action := range actions {
			notifications = append(notifications, notification{
				Type:       notifyActionCompleted,
				ServerID:   action.ServerID,
				OccurredAt: finishedAt(action),
				LandedAt:   finishedAt(action),
				Data:       actionNotificationData(action),
			})
		}
		return fanOut(webhooks, notifications)
	}, pendingDeliveryBound)
	if err != nil {
		s.log.Error("webhook pass could not fan out finished actions", "error", err.Error())
		return false
	}

	if time.Since(*lastLinkCheck) >= linkCheckInterval {
		*lastLinkCheck = time.Now()
		s.checkLinks(ctx)
	}

	attempted := s.deliverDue(ctx)
	return eventsMarked == notifyBatch || actionsMarked == notifyBatch || attempted == deliverBatch
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
func (s *Server) checkLinks(ctx context.Context) {
	candidates, err := s.store.LinkCandidates(ctx)
	if err != nil {
		s.log.Error("link check could not read servers", "error", err.Error())
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
		var build func([]store.Webhook) []store.NewWebhookDelivery
		if candidate.LinkState == store.LinkDown && target == store.LinkUp {
			build = func(webhooks []store.Webhook) []store.NewWebhookDelivery {
				return fanOut(webhooks, []notification{
					linkNotification(notifyServerLinkRestore, candidate, now),
				})
			}
		}
		if candidate.LinkState == store.LinkUp && target == store.LinkDown {
			build = func(webhooks []store.Webhook) []store.NewWebhookDelivery {
				return fanOut(webhooks, []notification{
					linkNotification(notifyServerLinkLost, candidate, now),
				})
			}
		}

		applied, err := s.store.ApplyLinkTransition(ctx, candidate.ServerID,
			candidate.LinkState, target, candidate.LastSeenAt, build, pendingDeliveryBound)
		if err != nil {
			s.log.Error("link transition failed", "serverId", candidate.ServerID, "error", err.Error())
			continue
		}
		if !applied {
			continue
		}
		switch {
		case candidate.LinkState == store.LinkUp && target == store.LinkDown:
			s.log.Warn("server link lost", "serverId", candidate.ServerID)
		case candidate.LinkState == store.LinkDown && target == store.LinkUp:
			s.log.Info("server link restored", "serverId", candidate.ServerID)
		default:
			s.log.Info("server link classified", "serverId", candidate.ServerID, "state", target)
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
func (s *Server) deliverDue(ctx context.Context) int {
	due, err := s.store.DueWebhookDeliveries(ctx, time.Now().UTC(), deliverBatch)
	if err != nil {
		s.log.Error("webhook pass could not read due deliveries", "error", err.Error())
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
				s.attemptDelivery(delivery)
			}
		}()
	}
feeding:
	for _, delivery := range due {
		select {
		case work <- delivery:
		case <-s.stopSweeper:
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
// The request context derives from the server's base context, so shutdown
// aborts in-flight attempts instead of waiting out their timeouts; the
// recording context is minted fresh **after** the attempt finishes, from the
// background, because an outcome that happened must be recorded even during a
// slow attempt or a shutdown, and a recording deadline that started ticking
// before the request would expire under a target slower than ten seconds and
// leave a completed attempt looking like it never ran.
func (s *Server) attemptDelivery(due store.DueDelivery) {
	attempt := due.Delivery.Attempts + 1
	body := []byte(due.Delivery.Body)

	recording := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.Background(), 10*time.Second)
	}
	fail := func(status *int, message string) {
		ctx, cancel := recording()
		defer cancel()
		s.recordFailure(ctx, due, attempt, status, message)
	}

	requestCtx, cancel := context.WithTimeout(s.baseCtx, s.cfg.WebhookDeliveryTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, due.URL, bytes.NewReader(body))
	if err != nil {
		fail(nil, "request could not be built: "+err.Error())
		return
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Vyshka-Delivery", due.Delivery.ID)
	request.Header.Set("X-Vyshka-Attempt", strconv.Itoa(attempt))
	request.Header.Set("X-Vyshka-Signature", signWebhookBody(due.Secret, body))

	response, err := s.webhookClient.Do(request)
	if err != nil {
		if s.baseCtx.Err() != nil {
			// Shutdown aborted the attempt, not the target. As far as the
			// schedule is concerned it never happened: the row stays pending
			// and due, and the next boot's dispatcher picks it up with the
			// same attempt number. Booking it would let a few restarts
			// dead-letter a delivery whose target never failed once.
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
		if err := s.store.RecordDeliverySuccess(ctx, due.Delivery.ID, response.StatusCode); err != nil {
			s.log.Error("delivery outcome could not be recorded",
				"deliveryId", due.Delivery.ID, "error", err.Error())
			return
		}
		s.log.Info("webhook delivered",
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

// recordFailure books one failed attempt, scheduling the retry the section
// 11.5 schedule owes it or declaring the delivery dead when none remains.
func (s *Server) recordFailure(ctx context.Context, due store.DueDelivery, attempt int, status *int, message string) {
	delays := s.cfg.WebhookRetryDelays
	var nextAttemptAt *time.Time
	// Attempt N failing consumes delays[N-1] for the next try; past the end,
	// the schedule is exhausted.
	if attempt-1 < len(delays) {
		next := time.Now().UTC().Add(delays[attempt-1])
		nextAttemptAt = &next
	}

	if err := s.store.RecordDeliveryFailure(ctx, due.Delivery.ID, status, message, nextAttemptAt); err != nil {
		s.log.Error("delivery outcome could not be recorded",
			"deliveryId", due.Delivery.ID, "error", err.Error())
		return
	}
	if nextAttemptAt == nil {
		s.log.Warn("webhook delivery dead-lettered",
			"webhookId", due.Delivery.WebhookID, "deliveryId", due.Delivery.ID,
			"type", due.Delivery.Type, "attempts", attempt, "lastError", message)
		return
	}
	s.log.Warn("webhook delivery failed, retry scheduled",
		"webhookId", due.Delivery.WebhookID, "deliveryId", due.Delivery.ID,
		"type", due.Delivery.Type, "attempt", attempt, "lastError", message,
		"nextAttemptAt", envelopeTimestamp(*nextAttemptAt))
}
