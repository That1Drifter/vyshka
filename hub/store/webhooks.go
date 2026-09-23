package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
	// Redact are the member paths stripped from every notification's data
	// before a delivery is rendered (spec section 11.2); empty strips nothing.
	Redact []string
	// AuditGranted records that the webhook's filter was authorized for the
	// opt-in audit notification (spec section 11.1) by a token that could
	// read the audit log, at registration or at its latest edit. A webhook
	// whose filter named the audit namespace before the notification
	// existed was granted telemetry, not the access record, and has it
	// false until such a token saves it again.
	AuditGranted bool
	CreatedAt    time.Time
	// PausedAt is when the webhook was paused, or nil while it is active. A
	// paused webhook keeps queueing deliveries and attempts none of them
	// (spec section 11.2).
	PausedAt *time.Time
}

// WebhookUpdate is the subset of a registration one edit replaces (spec
// section 11.2). A nil member leaves the stored field alone; a non-nil one
// replaces it whole, so an empty Events or ServerIDs means "every type" and
// "every server" exactly as it does at registration.
type WebhookUpdate struct {
	URL       *string
	Template  *string
	Events    *[]string
	ServerIDs *[]string
	Redact    *[]string
	Paused    *bool
	// AuditGranted, when non-nil, decides the webhook's audit grant from the
	// row as locked for the edit, once authorize has passed: an edit is
	// re-authorized as a whole, so its grant is decided afresh every time.
	AuditGranted func(existing Webhook) bool
}

// IsEmpty reports whether an update would change nothing at all.
func (u WebhookUpdate) IsEmpty() bool {
	return u.URL == nil && u.Template == nil && u.Events == nil && u.ServerIDs == nil &&
		u.Redact == nil && u.Paused == nil && u.AuditGranted == nil
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
	redact, err := encodeRedact(webhook.Redact)
	if err != nil {
		return Webhook{}, err
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO webhooks (id, url, secret, template, events, server_ids, redact, audit_granted, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		webhook.ID, webhook.URL, webhook.Secret, webhook.Template,
		string(events), string(serverIDs), redact, boolInt(webhook.AuditGranted), formatTime(now),
	); err != nil {
		return Webhook{}, fmt.Errorf("insert webhook: %w", err)
	}
	webhook.CreatedAt = now
	return webhook, nil
}

const webhookColumns = `id, url, secret, template, events, server_ids, redact, audit_granted, created_at, paused_at`

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// encodeRedact stores a redaction list as a JSON array, [] for none, so the
// column never holds a null a reader would have to special-case.
func encodeRedact(paths []string) (string, error) {
	if paths == nil {
		paths = []string{}
	}
	encoded, err := json.Marshal(paths)
	if err != nil {
		return "", fmt.Errorf("encode webhook redaction: %w", err)
	}
	return string(encoded), nil
}

// Every transaction locking multiple webhooks uses this order, including
// fan-out and delivery retention. Both columns are immutable.
const webhookLockOrder = ` ORDER BY created_at DESC, id DESC`

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

// UpdateWebhook applies one edit and returns the webhook as it now stands, or
// ErrNotFound (spec section 11.2). The secret is never touched here: an edit
// does not rotate it, so a receiver's verification keeps working across one.
//
// authorize, when non-nil, is the caller's coverage decision, and it runs
// inside the transaction against the row as locked there: the webhook as it
// stands at that instant and, when the edit changes the URL, the distinct
// types of its pending deliveries. A decision taken on a snapshot read before
// the transaction could be overtaken by a concurrent edit and authorize a
// subscription the token could never have registered; a decision taken on the
// locked row cannot. An error from authorize aborts the edit and is returned
// as it is, so the caller can tell its own refusal from a store failure.
//
// Pausing is idempotent in SQL rather than in the caller: paused_at is set
// with COALESCE, so pausing an already paused webhook leaves the instant it
// was first paused alone even when two edits race.
func (s *Store) UpdateWebhook(ctx context.Context, webhookID string, update WebhookUpdate,
	authorize func(existing Webhook, pendingTypes []string) error) (Webhook, error) {
	if update.IsEmpty() {
		return s.WebhookByID(ctx, webhookID)
	}

	assignments := make([]string, 0, 5)
	arguments := make([]any, 0, 6)
	if update.URL != nil {
		assignments = append(assignments, "url = ?")
		arguments = append(arguments, *update.URL)
	}
	if update.Template != nil {
		assignments = append(assignments, "template = ?")
		arguments = append(arguments, *update.Template)
	}
	if update.Events != nil {
		events, err := json.Marshal(*update.Events)
		if err != nil {
			return Webhook{}, fmt.Errorf("encode webhook events: %w", err)
		}
		assignments = append(assignments, "events = ?")
		arguments = append(arguments, string(events))
	}
	if update.ServerIDs != nil {
		serverIDs, err := json.Marshal(*update.ServerIDs)
		if err != nil {
			return Webhook{}, fmt.Errorf("encode webhook server ids: %w", err)
		}
		assignments = append(assignments, "server_ids = ?")
		arguments = append(arguments, string(serverIDs))
	}
	if update.Redact != nil {
		redact, err := encodeRedact(*update.Redact)
		if err != nil {
			return Webhook{}, err
		}
		assignments = append(assignments, "redact = ?")
		arguments = append(arguments, redact)
	}
	if update.Paused != nil {
		if *update.Paused {
			assignments = append(assignments, "paused_at = COALESCE(paused_at, ?)")
			arguments = append(arguments, formatTime(time.Now().UTC()))
		} else {
			assignments = append(assignments, "paused_at = NULL")
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Webhook{}, fmt.Errorf("begin webhook update: %w", err)
	}
	defer tx.Rollback()

	// The row is locked before the decision is taken, so nothing can rewrite
	// it between the coverage check and the write. On SQLite the store's one
	// connection serializes the whole transaction and the lock clause is
	// empty; on Postgres it is FOR UPDATE.
	existing, err := scanWebhook(tx.QueryRowContext(ctx,
		`SELECT `+webhookColumns+` FROM webhooks WHERE id = ?`+tx.forUpdate(), webhookID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Webhook{}, ErrNotFound
		}
		return Webhook{}, err
	}
	if authorize != nil {
		// The pending types matter only when the URL actually moves: an
		// edit that repeats the current URL carries nothing anywhere, and
		// the decision is on the locked row's URL, not on a snapshot's.
		var pendingTypes []string
		if update.URL != nil && *update.URL != existing.URL {
			if pendingTypes, err = pendingDeliveryTypes(ctx, tx, webhookID); err != nil {
				return Webhook{}, err
			}
		}
		if err := authorize(existing, pendingTypes); err != nil {
			return Webhook{}, err
		}
	}
	if update.AuditGranted != nil {
		assignments = append(assignments, "audit_granted = ?")
		arguments = append(arguments, boolInt(update.AuditGranted(existing)))
	}

	result, err := tx.ExecContext(ctx,
		`UPDATE webhooks SET `+strings.Join(assignments, ", ")+` WHERE id = ?`,
		append(arguments, webhookID)...)
	if err != nil {
		return Webhook{}, fmt.Errorf("update webhook: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return Webhook{}, fmt.Errorf("update webhook: %w", err)
	}
	if affected == 0 {
		return Webhook{}, ErrNotFound
	}

	// Read back inside the same transaction, so the answer is the row the
	// edit produced rather than one a racing edit rewrote in between.
	webhook, err := scanWebhook(tx.QueryRowContext(ctx,
		`SELECT `+webhookColumns+` FROM webhooks WHERE id = ?`, webhookID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Webhook{}, ErrNotFound
		}
		return Webhook{}, err
	}
	if err := tx.Commit(); err != nil {
		return Webhook{}, fmt.Errorf("commit webhook update: %w", err)
	}
	return webhook, nil
}

// querier is what the pending-type read needs from either a transaction or
// the pool.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// PendingDeliveryTypes returns the distinct notification types of a webhook's
// pending deliveries, in name order. An edit that changes the target URL is
// authorized against these (spec section 11.2): the bodies were rendered
// under the subscription as it stood, and they will follow the URL.
func (s *Store) PendingDeliveryTypes(ctx context.Context, webhookID string) ([]string, error) {
	return pendingDeliveryTypes(ctx, s.db, webhookID)
}

func pendingDeliveryTypes(ctx context.Context, q querier, webhookID string) ([]string, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT DISTINCT type FROM webhook_deliveries
		  WHERE webhook_id = ? AND state = ?
		  ORDER BY type`, webhookID, DeliveryPending)
	if err != nil {
		return nil, fmt.Errorf("read pending delivery types: %w", err)
	}
	defer rows.Close()
	types := []string{}
	for rows.Next() {
		var one string
		if err := rows.Scan(&one); err != nil {
			return nil, fmt.Errorf("scan pending delivery type: %w", err)
		}
		types = append(types, one)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read pending delivery types: %w", err)
	}
	return types, nil
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
		webhook                   Webhook
		events, serverIDs, redact string
		auditGranted              int
		createdAt                 string
		pausedAt                  sql.NullString
	)
	if err := row.Scan(&webhook.ID, &webhook.URL, &webhook.Secret, &webhook.Template,
		&events, &serverIDs, &redact, &auditGranted, &createdAt, &pausedAt); err != nil {
		return Webhook{}, err
	}
	webhook.AuditGranted = auditGranted != 0
	if err := json.Unmarshal([]byte(redact), &webhook.Redact); err != nil {
		return Webhook{}, fmt.Errorf("decode webhook redaction: %w", err)
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
	if webhook.PausedAt, err = scanTime(pausedAt); err != nil {
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
	// Generation counts the replays this delivery has had. An attempt books
	// its outcome against the generation it read, so an outcome arriving
	// after a replay is discarded instead of consuming the replay's attempt.
	Generation int
}

// ErrStaleAttempt is returned when a delivery outcome cannot be booked because
// the delivery was replayed, or is gone, since the attempt was read: the row
// no longer carries the generation the attempt was made under.
var ErrStaleAttempt = errors.New("delivery attempt is stale")

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
	build func([]Event, []Webhook, map[string]string) []NewWebhookDelivery, pendingBound int) (int, error) {
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
	names, err := serverNamesTx(ctx, tx)
	if err != nil {
		return 0, err
	}
	if err := insertDeliveries(ctx, tx, build(events, webhooks, names), pendingBound); err != nil {
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

// serverNamesTx reads every server's display name inside an open
// transaction, for the delivery templates that name the server (section
// 11.3). Read with the webhooks so a rename lands in the same pass as a
// registration would.
func serverNamesTx(ctx context.Context, tx *Tx) (map[string]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, name FROM servers`)
	if err != nil {
		return nil, fmt.Errorf("read server names in transaction: %w", err)
	}
	defer rows.Close()

	names := make(map[string]string, 8)
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, fmt.Errorf("scan server name: %w", err)
		}
		names[id] = name
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read server names in transaction: %w", err)
	}
	return names, nil
}

// webhooksTx reads every webhook inside an open transaction, for the fan-out
// paths that must see registrations consistently with the rows they flag.
// webhooksTx reads every webhook inside a transaction, locked. A pass that
// fans out against the subscriptions it read must not interleave with an
// edit: an edit authorized against "no pending delivery of that type" while
// a fan-out was about to insert exactly one would move the target under a
// body the editor may not read (spec section 11.2). Holding the rows until
// the deliveries commit makes the edit wait for the fan-out, or the fan-out
// read the edited subscription, and nothing in between. Every lock in this
// package is taken webhook first, delivery second, so the order cannot cycle.
func webhooksTx(ctx context.Context, tx *Tx) ([]Webhook, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT `+webhookColumns+` FROM webhooks`+webhookLockOrder+tx.forUpdate())
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
	build func([]Action, []Webhook, map[string]string) []NewWebhookDelivery, pendingBound int) (int, error) {
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
	names, err := serverNamesTx(ctx, tx)
	if err != nil {
		return 0, err
	}
	if err := insertDeliveries(ctx, tx, build(actions, webhooks, names), pendingBound); err != nil {
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
func markNotified(ctx context.Context, tx *Tx, table string, ids []any) error {
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
func insertDeliveries(ctx context.Context, tx *Tx, deliveries []NewWebhookDelivery, pendingBound int) error {
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
// first, up to limit. A paused webhook's deliveries are never due: while a
// webhook is paused the hub attempts nothing for it, retries included, and
// what it owes waits for the resume (spec section 11.2).
//
// There is no claim column: within one hub the dispatcher loop serializes
// passes, and one hub per database is the deployment this store supports on
// both engines. Running more than one hub instance against one Postgres
// database is not supported: the dispatcher, the link monitor, and the
// retention sweeps each assume they are the only pass of their kind, and a
// second instance would attempt the same delivery concurrently. Making that
// safe means a claim here (`SELECT ... FOR UPDATE SKIP LOCKED` or a lease
// column) and the same for the other passes.
func (s *Store) DueWebhookDeliveries(ctx context.Context, now time.Time, limit int) ([]DueDelivery, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT d.id, d.webhook_id, d.type, d.server_id, d.body, d.state, d.attempts,
		        d.next_attempt_at, d.last_status, d.last_error, d.created_at, d.delivered_at,
		        d.generation, w.url, w.secret
		   FROM webhook_deliveries d JOIN webhooks w ON w.id = d.webhook_id
		  WHERE d.state = ? AND d.next_attempt_at <= ? AND w.paused_at IS NULL
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
			&one.Delivery.Generation, &one.URL, &one.Secret); err != nil {
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

// ErrWebhookPaused is returned by BeginDeliveryAttempt when the delivery's
// webhook is paused at the moment the attempt would begin: nothing was
// booked and nothing may be sent.
var ErrWebhookPaused = errors.New("webhook is paused")

// DeliveryAttempt is what one booked attempt sends with: its number, and the
// target and signing key as they stand at the moment it begins.
type DeliveryAttempt struct {
	Attempt int
	URL     string
	Secret  string
}

// BeginDeliveryAttempt books the start of one attempt and answers with what
// the attempt sends: the attempt number and the webhook's URL and secret as
// of that instant. The booking is the attempt's start for every rule that
// cares (spec sections 11.2 and 11.5): the webhook row is locked for it, so
// a pause decides against it or after it and never between; a replay, which
// locks the same row, likewise lands before or after; and the attempt count
// moves here rather than at the outcome, so an attempt that was made is
// counted whether or not its outcome is later booked.
//
// ErrWebhookPaused means the webhook is paused and nothing was booked.
// ErrStaleAttempt means the delivery is no longer the one the batch read: it
// was replayed (the generation moved), finished by another path, or deleted
// with its webhook. In both cases the caller sends nothing.
func (s *Store) BeginDeliveryAttempt(ctx context.Context, deliveryID string, generation int) (DeliveryAttempt, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DeliveryAttempt{}, fmt.Errorf("begin delivery attempt: %w", err)
	}
	defer tx.Rollback()

	var webhookID string
	err = tx.QueryRowContext(ctx,
		`SELECT webhook_id FROM webhook_deliveries WHERE id = ? AND state = ? AND generation = ?`,
		deliveryID, DeliveryPending, generation).Scan(&webhookID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return DeliveryAttempt{}, ErrStaleAttempt
	case err != nil:
		return DeliveryAttempt{}, fmt.Errorf("begin delivery attempt: %w", err)
	}

	var (
		attempt  DeliveryAttempt
		pausedAt sql.NullString
	)
	err = tx.QueryRowContext(ctx,
		`SELECT url, secret, paused_at FROM webhooks WHERE id = ?`+tx.forUpdate(), webhookID).
		Scan(&attempt.URL, &attempt.Secret, &pausedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return DeliveryAttempt{}, ErrStaleAttempt
	case err != nil:
		return DeliveryAttempt{}, fmt.Errorf("begin delivery attempt: %w", err)
	case pausedAt.Valid:
		return DeliveryAttempt{}, ErrWebhookPaused
	}

	// Re-checked under the lock: a replay that committed between the first
	// read and the lock has moved the generation, and this attempt is not
	// the one it promised.
	result, err := tx.ExecContext(ctx,
		`UPDATE webhook_deliveries SET attempts = attempts + 1
		  WHERE id = ? AND state = ? AND generation = ?`,
		deliveryID, DeliveryPending, generation)
	if err != nil {
		return DeliveryAttempt{}, fmt.Errorf("begin delivery attempt: %w", err)
	}
	if err := bookedOrStale(result, "begin delivery attempt"); err != nil {
		return DeliveryAttempt{}, err
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT attempts FROM webhook_deliveries WHERE id = ?`, deliveryID).Scan(&attempt.Attempt); err != nil {
		return DeliveryAttempt{}, fmt.Errorf("begin delivery attempt: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return DeliveryAttempt{}, fmt.Errorf("commit delivery attempt: %w", err)
	}
	return attempt, nil
}

// RecordDeliverySuccess finishes a delivery after a 2xx answer. generation is
// the one the attempt began under; a row that has been replayed since carries
// a later one, and the outcome is then ErrStaleAttempt rather than booked,
// because the replay's attempt is the one that now counts (spec section
// 11.5). The attempt itself was counted when it began.
func (s *Store) RecordDeliverySuccess(ctx context.Context, deliveryID string, generation, status int) error {
	now := formatTime(time.Now().UTC())
	result, err := s.db.ExecContext(ctx,
		`UPDATE webhook_deliveries
		    SET state = ?, last_status = ?, last_error = NULL, delivered_at = ?, finished_at = ?
		  WHERE id = ? AND state = ? AND generation = ?`,
		DeliveryDelivered, status, now, now, deliveryID, DeliveryPending, generation)
	if err != nil {
		return fmt.Errorf("record delivery success: %w", err)
	}
	return bookedOrStale(result, "record delivery success")
}

// RecordDeliveryFailure books one failed attempt. A non-nil nextAttemptAt
// schedules the retry; nil means the schedule is exhausted and the delivery is
// dead (spec section 11.5). status is nil when the failure never produced an
// HTTP response at all. generation is as for RecordDeliverySuccess.
func (s *Store) RecordDeliveryFailure(ctx context.Context, deliveryID string, generation int, status *int, message string, nextAttemptAt *time.Time) error {
	statusValue := any(nil)
	if status != nil {
		statusValue = *status
	}

	if nextAttemptAt != nil {
		result, err := s.db.ExecContext(ctx,
			`UPDATE webhook_deliveries
			    SET last_status = ?, last_error = ?, next_attempt_at = ?
			  WHERE id = ? AND state = ? AND generation = ?`,
			statusValue, message, formatTime(*nextAttemptAt), deliveryID, DeliveryPending, generation)
		if err != nil {
			return fmt.Errorf("record delivery failure: %w", err)
		}
		return bookedOrStale(result, "record delivery failure")
	}

	result, err := s.db.ExecContext(ctx,
		`UPDATE webhook_deliveries
		    SET state = ?, last_status = ?, last_error = ?, finished_at = ?
		  WHERE id = ? AND state = ? AND generation = ?`,
		DeliveryDead, statusValue, message, formatTime(time.Now().UTC()), deliveryID, DeliveryPending, generation)
	if err != nil {
		return fmt.Errorf("record delivery dead: %w", err)
	}
	return bookedOrStale(result, "record delivery dead")
}

// bookedOrStale turns an outcome update that matched no row into
// ErrStaleAttempt: the delivery was replayed, finished by another path, or
// deleted with its webhook since the attempt was read.
func bookedOrStale(result sql.Result, what string) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if affected == 0 {
		return ErrStaleAttempt
	}
	return nil
}

const webhookDeliveryColumns = `id, webhook_id, type, server_id, body, state, attempts,
	        next_attempt_at, last_status, last_error, created_at, delivered_at, generation`

func scanWebhookDelivery(row rowScanner) (WebhookDelivery, error) {
	var (
		delivery                 WebhookDelivery
		body                     string
		nextAttemptAt, createdAt string
		lastStatus               sql.NullInt64
		lastError                sql.NullString
		deliveredAt              sql.NullString
	)
	if err := row.Scan(&delivery.ID, &delivery.WebhookID, &delivery.Type,
		&delivery.ServerID, &body, &delivery.State, &delivery.Attempts,
		&nextAttemptAt, &lastStatus, &lastError, &createdAt, &deliveredAt,
		&delivery.Generation); err != nil {
		return WebhookDelivery{}, err
	}
	delivery.Body = json.RawMessage(body)
	delivery.LastError = lastError.String
	if lastStatus.Valid {
		status := int(lastStatus.Int64)
		delivery.LastStatus = &status
	}
	var err error
	if delivery.NextAttemptAt, err = parseTime(nextAttemptAt); err != nil {
		return WebhookDelivery{}, err
	}
	if delivery.CreatedAt, err = parseTime(createdAt); err != nil {
		return WebhookDelivery{}, err
	}
	if delivery.DeliveredAt, err = scanTime(deliveredAt); err != nil {
		return WebhookDelivery{}, err
	}
	return delivery, nil
}

// WebhookDeliveries lists one webhook's deliveries, newest first, up to limit.
func (s *Store) WebhookDeliveries(ctx context.Context, webhookID string, limit int) ([]WebhookDelivery, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+webhookDeliveryColumns+`
		   FROM webhook_deliveries WHERE webhook_id = ?
		  ORDER BY created_at DESC, id DESC LIMIT ?`, webhookID, limit)
	if err != nil {
		return nil, fmt.Errorf("read webhook deliveries: %w", err)
	}
	defer rows.Close()

	deliveries := make([]WebhookDelivery, 0, 16)
	for rows.Next() {
		delivery, err := scanWebhookDelivery(rows)
		if err != nil {
			return nil, fmt.Errorf("scan webhook delivery: %w", err)
		}
		deliveries = append(deliveries, delivery)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read webhook deliveries: %w", err)
	}
	return deliveries, nil
}

// WebhookDelivery reads one delivery of one webhook, or ErrNotFound when the
// delivery does not exist or belongs to another webhook.
func (s *Store) WebhookDelivery(ctx context.Context, webhookID, deliveryID string) (WebhookDelivery, error) {
	delivery, err := scanWebhookDelivery(s.db.QueryRowContext(ctx,
		`SELECT `+webhookDeliveryColumns+` FROM webhook_deliveries WHERE id = ? AND webhook_id = ?`,
		deliveryID, webhookID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return WebhookDelivery{}, ErrNotFound
		}
		return WebhookDelivery{}, fmt.Errorf("read webhook delivery: %w", err)
	}
	return delivery, nil
}

// ReplayWebhookDelivery re-arms one delivery for a further attempt (spec
// section 11.5): pending again, due now, its terminal timestamps cleared. The
// id, the stored body, and the attempt count survive, so the receiver sees the
// same signed bytes under the same deliveryId and X-Vyshka-Attempt keeps
// counting; that is what makes a replay a replay rather than a second
// delivery of the same notification.
//
// It is allowed in every state, a pending delivery simply being brought
// forward, and it is not subject to the per-webhook pending bound: one
// operator-driven attempt is not fan-out. The delivery must belong to the
// named webhook, or the answer is ErrNotFound.
func (s *Store) ReplayWebhookDelivery(ctx context.Context, webhookID, deliveryID string) (WebhookDelivery, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WebhookDelivery{}, fmt.Errorf("begin delivery replay: %w", err)
	}
	defer tx.Rollback()

	// The webhook row is locked first, so a replay serializes with an edit
	// deciding on the webhook's pending deliveries (spec section 11.2) and
	// with an attempt beginning against this delivery: a re-armed row is
	// either seen by the edit's coverage check or written after it commits.
	var owner string
	err = tx.QueryRowContext(ctx, `SELECT id FROM webhooks WHERE id = ?`+tx.forUpdate(), webhookID).Scan(&owner)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return WebhookDelivery{}, ErrNotFound
	case err != nil:
		return WebhookDelivery{}, fmt.Errorf("replay webhook delivery: %w", err)
	}

	// The generation moves with every replay, so an attempt that was already
	// in flight books nothing against the re-armed row (see
	// RecordDeliverySuccess), and the further attempt the replay promised is
	// the one that gets made.
	result, err := tx.ExecContext(ctx,
		`UPDATE webhook_deliveries
		    SET state = ?, next_attempt_at = ?, finished_at = NULL, delivered_at = NULL,
		        generation = generation + 1
		  WHERE id = ? AND webhook_id = ?`,
		DeliveryPending, formatTime(time.Now().UTC()), deliveryID, webhookID)
	if err != nil {
		return WebhookDelivery{}, fmt.Errorf("replay webhook delivery: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return WebhookDelivery{}, fmt.Errorf("replay webhook delivery: %w", err)
	}
	if affected == 0 {
		return WebhookDelivery{}, ErrNotFound
	}

	delivery, err := scanWebhookDelivery(tx.QueryRowContext(ctx,
		`SELECT `+webhookDeliveryColumns+` FROM webhook_deliveries WHERE id = ?`, deliveryID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return WebhookDelivery{}, ErrNotFound
		}
		return WebhookDelivery{}, fmt.Errorf("scan webhook delivery: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return WebhookDelivery{}, fmt.Errorf("commit delivery replay: %w", err)
	}
	return delivery, nil
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
	// The DELETE binds each candidate id plus two eligibility parameters.
	// Stay within SQLite's 32766-variable limit (Postgres allows 65535).
	limit = min(limit, 32764)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin prune webhook deliveries: %w", err)
	}
	defer tx.Rollback()

	// Choose a bounded batch without locking deliveries: DeleteWebhook's
	// cascade, replay, and fan-out all take the webhook lock first. Taking a
	// delivery lock before its parent could deadlock with those operations.
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM webhook_deliveries
		  WHERE state <> ? AND finished_at IS NOT NULL AND finished_at <= ?
		  ORDER BY finished_at, id LIMIT ?`, DeliveryPending, formatTime(cutoff), limit)
	if err != nil {
		return 0, fmt.Errorf("read delivery prune batch: %w", err)
	}
	ids := make([]any, 0)
	for rows.Next() {
		var deliveryID string
		if err := rows.Scan(&deliveryID); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan delivery prune batch: %w", err)
		}
		ids = append(ids, deliveryID)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, fmt.Errorf("read delivery prune batch: %w", err)
	}
	if len(ids) == 0 {
		return 0, tx.Commit()
	}

	// Lock only this batch's parents, in the same order as webhooksTx, and
	// consume every row before deleting any delivery. Keep the original batch:
	// selecting fresh candidates afterwards could touch an unlocked parent.
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	rows, err = tx.QueryContext(ctx,
		`SELECT id FROM webhooks WHERE id IN
		 (SELECT webhook_id FROM webhook_deliveries WHERE id IN (`+placeholders+`))`+
			webhookLockOrder+tx.forUpdate(), ids...)
	if err != nil {
		return 0, fmt.Errorf("lock delivery prune webhooks: %w", err)
	}
	for rows.Next() {
		var webhookID string
		if err := rows.Scan(&webhookID); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan delivery prune webhook: %w", err)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, fmt.Errorf("lock delivery prune webhooks: %w", err)
	}

	// Recheck eligibility after acquiring the parents: a replay may have
	// re-armed a candidate while we waited. A concurrent cascade may also have
	// removed a parent and its candidates, in which case they delete nothing.
	result, err := tx.ExecContext(ctx,
		`DELETE FROM webhook_deliveries
		  WHERE id IN (`+placeholders+`)
		    AND state <> ? AND finished_at IS NOT NULL AND finished_at <= ?`,
		append(ids, DeliveryPending, formatTime(cutoff))...)
	if err != nil {
		return 0, fmt.Errorf("prune webhook deliveries: %w", err)
	}
	pruned, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune webhook deliveries: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit prune webhook deliveries: %w", err)
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
	build func([]Webhook, map[string]string) []NewWebhookDelivery, pendingBound int) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin link transition: %w", err)
	}
	defer tx.Rollback()

	// The server row is locked in a statement of its own before the guarded
	// update runs. The update's own wait on that row would re-evaluate its
	// predicate against the committed server row, but the live-session
	// subquery inside it would keep the snapshot the statement started with:
	// a revocation committing during the wait ends the session, and the
	// guard would still see it live and fire a restored notification for a
	// server that can no longer speak. Taken first, the lock makes the
	// update's snapshot postdate the revocation.
	switch err := lockServer(ctx, tx, serverID); {
	case errors.Is(err, ErrNotFound):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("read server: %w", err)
	}

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
		names, err := serverNamesTx(ctx, tx)
		if err != nil {
			return false, err
		}
		if err := insertDeliveries(ctx, tx, build(webhooks, names), pendingBound); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit link transition: %w", err)
	}
	return true, nil
}
