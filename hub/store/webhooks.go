package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Webhook delivery states (spec section 11.5). Dead is a state, not a
// deletion: the record of what could not be delivered is the point.
const (
	DeliveryPending   = "pending"
	DeliveryDelivered = "delivered"
	DeliveryDead      = "dead"
)

// Server link states (spec section 11.1). Unknown means no session has ever
// been observed, which fires nothing.
const (
	LinkUnknown = "unknown"
	LinkUp      = "up"
	LinkDown    = "down"
)

// Webhook is one registered push target (spec section 11.2).
type Webhook struct {
	ID  string
	URL string
	// Secret is the HMAC signing key, stored as itself: signing needs the key,
	// so there is nothing irreversible to store instead.
	Secret   string
	Template string
	// Events are type patterns in the section 10.1 grammar; empty means every
	// type. ServerIDs are exact ids; empty means every server.
	Events    []string
	ServerIDs []string
	CreatedAt time.Time
}

// CreateWebhook records one webhook. The caller assigns the id and mints the
// secret, because the secret goes back in the response and the store must not
// be the layer deciding what a credential looks like.
func (s *Store) CreateWebhook(ctx context.Context, webhook Webhook) (Webhook, error) {
	now := time.Now().UTC()
	events, err := json.Marshal(webhook.Events)
	if err != nil {
		return Webhook{}, fmt.Errorf("encode webhook events: %w", err)
	}
	serverIDs, err := json.Marshal(webhook.ServerIDs)
	if err != nil {
		return Webhook{}, fmt.Errorf("encode webhook server ids: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO webhooks (id, url, secret, template, events, server_ids, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		webhook.ID, webhook.URL, webhook.Secret, webhook.Template,
		string(events), string(serverIDs), formatTime(now),
	); err != nil {
		return Webhook{}, fmt.Errorf("insert webhook: %w", err)
	}
	webhook.CreatedAt = now
	return webhook, nil
}

const webhookColumns = `id, url, secret, template, events, server_ids, created_at`

// Webhooks returns every registered webhook, newest first. The dispatcher
// reads this on every pass, so the whole table is the working set; a hub with
// enough webhooks for that to matter has outgrown this store.
func (s *Store) Webhooks(ctx context.Context) ([]Webhook, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+webhookColumns+` FROM webhooks ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("read webhooks: %w", err)
	}
	defer rows.Close()

	webhooks := make([]Webhook, 0, 8)
	for rows.Next() {
		webhook, err := scanWebhook(rows)
		if err != nil {
			return nil, err
		}
		webhooks = append(webhooks, webhook)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read webhooks: %w", err)
	}
	return webhooks, nil
}

// WebhookByID returns one webhook, or ErrNotFound.
func (s *Store) WebhookByID(ctx context.Context, webhookID string) (Webhook, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+webhookColumns+` FROM webhooks WHERE id = ?`, webhookID)
	webhook, err := scanWebhook(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Webhook{}, ErrNotFound
	}
	return webhook, err
}

// DeleteWebhook removes a webhook; its deliveries go with it (ON DELETE
// CASCADE), which is the "pending deliveries are abandoned" of section 11.2.
func (s *Store) DeleteWebhook(ctx context.Context, webhookID string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM webhooks WHERE id = ?`, webhookID)
	if err != nil {
		return fmt.Errorf("delete webhook: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete webhook: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

func scanWebhook(row rowScanner) (Webhook, error) {
	var (
		webhook           Webhook
		events, serverIDs string
		createdAt         string
	)
	if err := row.Scan(&webhook.ID, &webhook.URL, &webhook.Secret, &webhook.Template,
		&events, &serverIDs, &createdAt); err != nil {
		return Webhook{}, err
	}
	if err := json.Unmarshal([]byte(events), &webhook.Events); err != nil {
		return Webhook{}, fmt.Errorf("decode webhook events: %w", err)
	}
	if err := json.Unmarshal([]byte(serverIDs), &webhook.ServerIDs); err != nil {
		return Webhook{}, fmt.Errorf("decode webhook server ids: %w", err)
	}
	var err error
	if webhook.CreatedAt, err = parseTime(createdAt); err != nil {
		return Webhook{}, err
	}
	return webhook, nil
}

// NewWebhookDelivery is one delivery to enqueue: one notification, one
// webhook, and the payload rendered once so every attempt sends the same
// bytes (spec section 11.3). The caller assigns the id, because the payload
// embeds it.
type NewWebhookDelivery struct {
	ID        string
	WebhookID string
	Type      string
	ServerID  string
	Body      json.RawMessage
}

// WebhookDelivery is one delivery as the Admin API reports it (section 11.5).
type WebhookDelivery struct {
	ID            string
	WebhookID     string
	Type          string
	ServerID      string
	Body          json.RawMessage
	State         string
	Attempts      int
	NextAttemptAt time.Time
	LastStatus    *int
	LastError     string
	CreatedAt     time.Time
	DeliveredAt   *time.Time
}

// NotifyEvents fans unnotified stored events out to deliveries in one
// transaction: read up to limit flagged rows, let build turn them into
// deliveries against the webhooks as read **inside the same transaction**, and
// clear the flags. The flag and the deliveries commit together, so a crash
// costs neither a lost notification nor a duplicate; the webhooks are read
// in-tx so a registration serializes against the fan-out rather than racing
// it, which with a snapshot taken outside would lose the notifications that
// landed between the snapshot and the pass.
//
// The flag approach is deliberate where a cursor would be lighter: a cursor
// over (received_at, id) can skip a row whose transaction committed out of
// timestamp order, and a skipped notification is exactly the silent loss
// section 11 forbids.
func (s *Store) NotifyEvents(ctx context.Context, limit int,
	build func([]Event, []Webhook) []NewWebhookDelivery, pendingBound int) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin notify events: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx,
		`SELECT id, server_id, type, occurred_at, received_at, data
		   FROM events WHERE notified = 0 ORDER BY id LIMIT ?`, limit)
	if err != nil {
		return 0, fmt.Errorf("read unnotified events: %w", err)
	}
	events := make([]Event, 0, 64)
	ids := make([]any, 0, 64)
	for rows.Next() {
		var (
			event                  Event
			occurredAt, receivedAt string
			data                   string
		)
		if err := rows.Scan(&event.ID, &event.ServerID, &event.Type,
			&occurredAt, &receivedAt, &data); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan unnotified event: %w", err)
		}
		if event.OccurredAt, err = parseTime(occurredAt); err != nil {
			rows.Close()
			return 0, err
		}
		if event.ReceivedAt, err = parseTime(receivedAt); err != nil {
			rows.Close()
			return 0, err
		}
		event.Data = json.RawMessage(data)
		events = append(events, event)
		ids = append(ids, event.ID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("read unnotified events: %w", err)
	}
	rows.Close()
	if len(events) == 0 {
		return 0, tx.Commit()
	}

	webhooks, err := webhooksTx(ctx, tx)
	if err != nil {
		return 0, err
	}
	if err := insertDeliveries(ctx, tx, build(events, webhooks), pendingBound); err != nil {
		return 0, err
	}
	if err := markNotified(ctx, tx, "events", ids); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit notify events: %w", err)
	}
	return len(events), nil
}

// webhooksTx reads every webhook inside an open transaction, for the fan-out
// paths that must see registrations consistently with the rows they flag.
func webhooksTx(ctx context.Context, tx *sql.Tx) ([]Webhook, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT `+webhookColumns+` FROM webhooks ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("read webhooks in transaction: %w", err)
	}
	defer rows.Close()

	webhooks := make([]Webhook, 0, 8)
	for rows.Next() {
		webhook, err := scanWebhook(rows)
		if err != nil {
			return nil, err
		}
		webhooks = append(webhooks, webhook)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read webhooks in transaction: %w", err)
	}
	return webhooks, nil
}

// NotifyFinishedActions is NotifyEvents for the action.completed lifecycle
// notification (section 11.1): terminal actions not yet fanned out.
func (s *Store) NotifyFinishedActions(ctx context.Context, limit int,
	build func([]Action, []Webhook) []NewWebhookDelivery, pendingBound int) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin notify actions: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx,
		`SELECT `+actionColumns+` FROM actions
		  WHERE notified = 0 AND finished_at IS NOT NULL ORDER BY id LIMIT ?`, limit)
	if err != nil {
		return 0, fmt.Errorf("read unnotified actions: %w", err)
	}
	actions := make([]Action, 0, 16)
	ids := make([]any, 0, 16)
	for rows.Next() {
		action, err := scanAction(rows)
		if err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan unnotified action: %w", err)
		}
		actions = append(actions, action)
		ids = append(ids, action.ID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("read unnotified actions: %w", err)
	}
	rows.Close()
	if len(actions) == 0 {
		return 0, tx.Commit()
	}

	webhooks, err := webhooksTx(ctx, tx)
	if err != nil {
		return 0, err
	}
	if err := insertDeliveries(ctx, tx, build(actions, webhooks), pendingBound); err != nil {
		return 0, err
	}
	if err := markNotified(ctx, tx, "actions", ids); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit notify actions: %w", err)
	}
	return len(actions), nil
}

// markNotified clears the outbox flag on the rows one pass fanned out. table
// is a compile-time constant at every call site, never caller input.
func markNotified(ctx context.Context, tx *sql.Tx, table string, ids []any) error {
	placeholders := ""
	for i := range ids {
		if i > 0 {
			placeholders += ","
		}
		placeholders += "?"
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE `+table+` SET notified = 1 WHERE id IN (`+placeholders+`)`, ids...); err != nil {
		return fmt.Errorf("mark %s notified: %w", table, err)
	}
	return nil
}

// insertDeliveries enqueues deliveries, honoring the per-webhook pending bound
// of section 11.5: at the bound a delivery is created dead with a lastError
// saying so, because a full queue is exactly the failure webhooks exist to
// surface and discarding the evidence would hide it.
func insertDeliveries(ctx context.Context, tx *sql.Tx, deliveries []NewWebhookDelivery, pendingBound int) error {
	if len(deliveries) == 0 {
		return nil
	}
	now := formatTime(time.Now().UTC())
	pendingByWebhook := map[string]int{}

	for _, delivery := range deliveries {
		pending, counted := pendingByWebhook[delivery.WebhookID]
		if !counted {
			if err := tx.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM webhook_deliveries WHERE webhook_id = ? AND state = ?`,
				delivery.WebhookID, DeliveryPending,
			).Scan(&pending); err != nil {
				return fmt.Errorf("count pending deliveries: %w", err)
			}
		}

		state, lastError, finishedAt := DeliveryPending, any(nil), any(nil)
		if pending >= pendingBound {
			state = DeliveryDead
			lastError = fmt.Sprintf("dead on arrival: this webhook already has %d pending deliveries", pending)
			finishedAt = now
		} else {
			pending++
		}
		pendingByWebhook[delivery.WebhookID] = pending

		if _, err := tx.ExecContext(ctx,
			`INSERT INTO webhook_deliveries
			   (id, webhook_id, type, server_id, body, state, attempts, next_attempt_at, last_error, created_at, finished_at)
			 VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?)`,
			delivery.ID, delivery.WebhookID, delivery.Type, delivery.ServerID,
			string(delivery.Body), state, now, lastError, now, finishedAt,
		); err != nil {
			return fmt.Errorf("insert webhook delivery: %w", err)
		}
	}
	return nil
}

// DueDelivery is one pending delivery whose time has come, joined with what
// the dispatcher needs to send it.
type DueDelivery struct {
	Delivery WebhookDelivery
	URL      string
	Secret   string
}

// DueWebhookDeliveries returns pending deliveries due at or before now, oldest
// first, up to limit.
//
// There is no claim column: within one hub the dispatcher loop serializes
// passes, and SQLite's single connection is the only deployment this store
// supports. A Postgres backend running more than one hub instance MUST add a
// claim (`SELECT ... FOR UPDATE SKIP LOCKED` or a lease column), or two
// dispatchers will attempt the same delivery concurrently. See the Postgres
// note in resolveDSN and issue #20.
func (s *Store) DueWebhookDeliveries(ctx context.Context, now time.Time, limit int) ([]DueDelivery, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT d.id, d.webhook_id, d.type, d.server_id, d.body, d.state, d.attempts,
		        d.next_attempt_at, d.last_status, d.last_error, d.created_at, d.delivered_at,
		        w.url, w.secret
		   FROM webhook_deliveries d JOIN webhooks w ON w.id = d.webhook_id
		  WHERE d.state = ? AND d.next_attempt_at <= ?
		  ORDER BY d.next_attempt_at, d.id
		  LIMIT ?`, DeliveryPending, formatTime(now), limit)
	if err != nil {
		return nil, fmt.Errorf("read due deliveries: %w", err)
	}
	defer rows.Close()

	due := make([]DueDelivery, 0, 16)
	for rows.Next() {
		var (
			one                      DueDelivery
			body                     string
			nextAttemptAt, createdAt string
			lastStatus               sql.NullInt64
			lastError                sql.NullString
			deliveredAt              sql.NullString
		)
		if err := rows.Scan(&one.Delivery.ID, &one.Delivery.WebhookID, &one.Delivery.Type,
			&one.Delivery.ServerID, &body, &one.Delivery.State, &one.Delivery.Attempts,
			&nextAttemptAt, &lastStatus, &lastError, &createdAt, &deliveredAt,
			&one.URL, &one.Secret); err != nil {
			return nil, fmt.Errorf("scan due delivery: %w", err)
		}
		one.Delivery.Body = json.RawMessage(body)
		one.Delivery.LastError = lastError.String
		if lastStatus.Valid {
			status := int(lastStatus.Int64)
			one.Delivery.LastStatus = &status
		}
		if one.Delivery.NextAttemptAt, err = parseTime(nextAttemptAt); err != nil {
			return nil, err
		}
		if one.Delivery.CreatedAt, err = parseTime(createdAt); err != nil {
			return nil, err
		}
		due = append(due, one)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read due deliveries: %w", err)
	}
	return due, nil
}

// RecordDeliverySuccess finishes a delivery after a 2xx answer.
func (s *Store) RecordDeliverySuccess(ctx context.Context, deliveryID string, status int) error {
	now := formatTime(time.Now().UTC())
	if _, err := s.db.ExecContext(ctx,
		`UPDATE webhook_deliveries
		    SET state = ?, attempts = attempts + 1, last_status = ?, last_error = NULL,
		        delivered_at = ?, finished_at = ?
		  WHERE id = ? AND state = ?`,
		DeliveryDelivered, status, now, now, deliveryID, DeliveryPending,
	); err != nil {
		return fmt.Errorf("record delivery success: %w", err)
	}
	return nil
}

// RecordDeliveryFailure books one failed attempt. A non-nil nextAttemptAt
// schedules the retry; nil means the schedule is exhausted and the delivery is
// dead (spec section 11.5). status is nil when the failure never produced an
// HTTP response at all.
func (s *Store) RecordDeliveryFailure(ctx context.Context, deliveryID string, status *int, message string, nextAttemptAt *time.Time) error {
	statusValue := any(nil)
	if status != nil {
		statusValue = *status
	}

	if nextAttemptAt != nil {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE webhook_deliveries
			    SET attempts = attempts + 1, last_status = ?, last_error = ?, next_attempt_at = ?
			  WHERE id = ? AND state = ?`,
			statusValue, message, formatTime(*nextAttemptAt), deliveryID, DeliveryPending,
		); err != nil {
			return fmt.Errorf("record delivery failure: %w", err)
		}
		return nil
	}

	if _, err := s.db.ExecContext(ctx,
		`UPDATE webhook_deliveries
		    SET state = ?, attempts = attempts + 1, last_status = ?, last_error = ?, finished_at = ?
		  WHERE id = ? AND state = ?`,
		DeliveryDead, statusValue, message, formatTime(time.Now().UTC()), deliveryID, DeliveryPending,
	); err != nil {
		return fmt.Errorf("record delivery dead: %w", err)
	}
	return nil
}

// WebhookDeliveries lists one webhook's deliveries, newest first, up to limit.
func (s *Store) WebhookDeliveries(ctx context.Context, webhookID string, limit int) ([]WebhookDelivery, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, webhook_id, type, server_id, body, state, attempts,
		        next_attempt_at, last_status, last_error, created_at, delivered_at
		   FROM webhook_deliveries WHERE webhook_id = ?
		  ORDER BY created_at DESC, id DESC LIMIT ?`, webhookID, limit)
	if err != nil {
		return nil, fmt.Errorf("read webhook deliveries: %w", err)
	}
	defer rows.Close()

	deliveries := make([]WebhookDelivery, 0, 16)
	for rows.Next() {
		var (
			delivery                 WebhookDelivery
			body                     string
			nextAttemptAt, createdAt string
			lastStatus               sql.NullInt64
			lastError                sql.NullString
			deliveredAt              sql.NullString
		)
		if err := rows.Scan(&delivery.ID, &delivery.WebhookID, &delivery.Type,
			&delivery.ServerID, &body, &delivery.State, &delivery.Attempts,
			&nextAttemptAt, &lastStatus, &lastError, &createdAt, &deliveredAt); err != nil {
			return nil, fmt.Errorf("scan webhook delivery: %w", err)
		}
		delivery.Body = json.RawMessage(body)
		delivery.LastError = lastError.String
		if lastStatus.Valid {
			status := int(lastStatus.Int64)
			delivery.LastStatus = &status
		}
		if delivery.NextAttemptAt, err = parseTime(nextAttemptAt); err != nil {
			return nil, err
		}
		if delivery.CreatedAt, err = parseTime(createdAt); err != nil {
			return nil, err
		}
		if delivery.DeliveredAt, err = scanTime(deliveredAt); err != nil {
			return nil, err
		}
		deliveries = append(deliveries, delivery)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read webhook deliveries: %w", err)
	}
	return deliveries, nil
}

// PruneWebhookDeliveries deletes up to limit finished (delivered or dead)
// deliveries whose terminal instant is before cutoff. Retention counts from
// finished_at, not created_at, so a delivery that spent its whole schedule
// pending stays readable for the same window as one that died at once (spec
// section 11.5). Pending deliveries are never pruned: a pending row is a
// promise, and the schedule decides its fate, not retention.
func (s *Store) PruneWebhookDeliveries(ctx context.Context, cutoff time.Time, limit int) (int, error) {
	if limit <= 0 {
		limit = defaultPruneBatch
	}
	result, err := s.db.ExecContext(ctx,
		`DELETE FROM webhook_deliveries
		  WHERE id IN (SELECT id FROM webhook_deliveries
		                WHERE state <> ? AND finished_at IS NOT NULL AND finished_at <= ? LIMIT ?)`,
		DeliveryPending, formatTime(cutoff), limit)
	if err != nil {
		return 0, fmt.Errorf("prune webhook deliveries: %w", err)
	}
	pruned, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune webhook deliveries: %w", err)
	}
	return int(pruned), nil
}

// LinkCandidate is what the link monitor needs to know about one server
// (spec section 11.1).
type LinkCandidate struct {
	ServerID  string
	LinkState string
	// LastSeenAt is nil for a server that has never been heard from.
	LastSeenAt *time.Time
	// PollTimeoutSeconds is the live session's negotiated hold; nil when no
	// live session exists right now.
	PollTimeoutSeconds *int
}

// LinkCandidates returns every server's link bookkeeping, joined with its live
// session when one exists.
func (s *Store) LinkCandidates(ctx context.Context) ([]LinkCandidate, error) {
	now := formatTime(time.Now().UTC())
	rows, err := s.db.QueryContext(ctx,
		`SELECT srv.id, srv.link_state, srv.last_seen_at, sess.poll_timeout_seconds
		   FROM servers srv
		   LEFT JOIN sessions sess
		     ON sess.server_id = srv.id AND sess.ended_at IS NULL AND sess.expires_at > ?`,
		now)
	if err != nil {
		return nil, fmt.Errorf("read link candidates: %w", err)
	}
	defer rows.Close()

	candidates := make([]LinkCandidate, 0, 16)
	for rows.Next() {
		var (
			candidate   LinkCandidate
			lastSeenAt  sql.NullString
			pollTimeout sql.NullInt64
		)
		if err := rows.Scan(&candidate.ServerID, &candidate.LinkState,
			&lastSeenAt, &pollTimeout); err != nil {
			return nil, fmt.Errorf("scan link candidate: %w", err)
		}
		if candidate.LastSeenAt, err = scanTime(lastSeenAt); err != nil {
			return nil, err
		}
		if pollTimeout.Valid {
			seconds := int(pollTimeout.Int64)
			candidate.PollTimeoutSeconds = &seconds
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read link candidates: %w", err)
	}
	return candidates, nil
}

// ApplyLinkTransition moves one server's link state and enqueues the
// notification deliveries in the same transaction. Three guards keep a stale
// decision from committing:
//
//   - the state the monitor read, so a racing pass fires a transition once;
//   - the very last_seen_at the decision was computed from, so a poll landing
//     between the monitor's read and this write voids the decision instead of
//     becoming a false loss the next pass has to "restore";
//   - for a transition to up, a live session existing right now, so a
//     revocation landing in the same window cannot buy a restored
//     notification for a server that can no longer speak.
//
// The deliveries come from build, called against the webhooks as read inside
// this transaction, for the same reason NotifyEvents reads them in-tx: a
// registration serializes against the transition instead of racing it.
func (s *Store) ApplyLinkTransition(ctx context.Context, serverID, from, to string, observedLastSeen *time.Time,
	build func([]Webhook) []NewWebhookDelivery, pendingBound int) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin link transition: %w", err)
	}
	defer tx.Rollback()

	observed := any(nil)
	if observedLastSeen != nil {
		observed = formatTime(*observedLastSeen)
	}
	liveSessionGuard := ""
	arguments := []any{to, serverID, from, observed}
	if to == LinkUp {
		liveSessionGuard = ` AND EXISTS (SELECT 1 FROM sessions
			WHERE server_id = servers.id AND ended_at IS NULL AND expires_at > ?)`
		arguments = append(arguments, formatTime(time.Now().UTC()))
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE servers SET link_state = ?
		  WHERE id = ? AND link_state = ? AND COALESCE(last_seen_at, '') = COALESCE(?, '')`+liveSessionGuard,
		arguments...)
	if err != nil {
		return false, fmt.Errorf("update link state: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("update link state: %w", err)
	}
	if affected == 0 {
		return false, nil
	}

	if build != nil {
		webhooks, err := webhooksTx(ctx, tx)
		if err != nil {
			return false, err
		}
		if err := insertDeliveries(ctx, tx, build(webhooks), pendingBound); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit link transition: %w", err)
	}
	return true, nil
}
