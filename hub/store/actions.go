package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Action states (spec section 7). Transitions only ever move forward:
// queued -> delivered -> running -> completed | failed, with expired reachable
// from any non-terminal state once the deadline passes.
const (
	ActionQueued    = "queued"
	ActionDelivered = "delivered"
	ActionRunning   = "running"
	ActionCompleted = "completed"
	ActionFailed    = "failed"
	ActionExpired   = "expired"
)

// Action is one tracked job.
type Action struct {
	ID             string
	ServerID       string
	Code           string
	Context        string
	ReferenceKey   string
	Params         json.RawMessage
	IdempotencyKey string
	EnvelopeID     string
	State          string
	CreatedAt      time.Time
	ExpiresAt      time.Time
	DeliveredAt    *time.Time
	RunningAt      *time.Time
	FinishedAt     *time.Time
	// OK is nil until an action.result landed.
	OK         *bool
	Result     json.RawMessage
	Error      string
	DurationMs *int64
}

// NewAction is the request side of DispatchAction. The caller assigns the id
// and frames the dispatch envelope, because the envelope body embeds the id
// and body shapes are the handler's business, not the store's.
type NewAction struct {
	ID             string
	ServerID       string
	Code           string
	Context        string
	ReferenceKey   string
	Params         json.RawMessage
	IdempotencyKey string
	TTL            time.Duration
	// EnvelopeType and EnvelopeBody frame the action.dispatch envelope queued
	// in the same transaction as the action row.
	EnvelopeType string
	EnvelopeBody json.RawMessage
	// QueueLimit bounds the server's outbound queue (spec section 9.2).
	QueueLimit int
}

// DispatchAction records an action and queues its dispatch envelope in one
// transaction. When request.IdempotencyKey names an action that already
// exists, the original is returned with created false and nothing is queued:
// that is the idempotent-retry contract of spec section 7, enforced by the
// unique index rather than by a read, so two racing retries produce exactly
// one action.
func (s *Store) DispatchAction(ctx context.Context, request NewAction) (Action, bool, error) {
	now := time.Now().UTC()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Action{}, false, fmt.Errorf("begin dispatch action: %w", err)
	}
	defer tx.Rollback()

	var exists string
	switch err := tx.QueryRowContext(ctx, `SELECT id FROM servers WHERE id = ?`, request.ServerID).Scan(&exists); {
	case errors.Is(err, sql.ErrNoRows):
		return Action{}, false, ErrNotFound
	case err != nil:
		return Action{}, false, fmt.Errorf("read server: %w", err)
	}

	// The action row goes in first, so that an idempotent retry is recognized
	// before anything else can fail: a retry against a full outbound queue
	// must still return the original action, not outbound_queue_full.
	envelope := newOutbound(request.EnvelopeType, request.EnvelopeBody)
	var idempotencyKey any
	if request.IdempotencyKey != "" {
		idempotencyKey = request.IdempotencyKey
	}
	result, err := tx.ExecContext(ctx,
		`INSERT INTO actions (id, server_id, code, context, reference_key, params,
		                      idempotency_key, envelope_id, state, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (server_id, idempotency_key) WHERE idempotency_key IS NOT NULL
		 DO NOTHING`,
		request.ID, request.ServerID, request.Code, request.Context, request.ReferenceKey,
		string(request.Params), idempotencyKey, envelope.ID, ActionQueued,
		formatTime(now), formatTime(now.Add(request.TTL)),
	)
	if err != nil {
		return Action{}, false, fmt.Errorf("insert action: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return Action{}, false, fmt.Errorf("insert action: %w", err)
	}

	if inserted == 0 {
		// An idempotent retry: abandon the transaction and hand back the
		// original, queueing nothing.
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			return Action{}, false, fmt.Errorf("roll back idempotent retry: %w", err)
		}
		original, err := s.actionByIdempotencyKey(ctx, request.ServerID, request.IdempotencyKey)
		if err != nil {
			return Action{}, false, err
		}
		return original, false, nil
	}

	if err := insertOutbound(ctx, tx, request.ServerID, envelope, request.QueueLimit); err != nil {
		return Action{}, false, err
	}

	if err := tx.Commit(); err != nil {
		return Action{}, false, fmt.Errorf("commit dispatch action: %w", err)
	}

	action, err := s.ActionByID(ctx, request.ID)
	if err != nil {
		return Action{}, false, err
	}
	return action, true, nil
}

const actionColumns = `id, server_id, code, context, reference_key, params,
	idempotency_key, envelope_id, state, created_at, expires_at,
	delivered_at, running_at, finished_at, ok, result, error, duration_ms`

// ActionByID returns one action, applying lazy expiry first: a read must never
// report a non-terminal state for an action whose deadline has passed, however
// recently the sweeper last ran.
func (s *Store) ActionByID(ctx context.Context, actionID string) (Action, error) {
	if _, err := s.ExpireActions(ctx); err != nil {
		return Action{}, err
	}

	row := s.db.QueryRowContext(ctx,
		`SELECT `+actionColumns+` FROM actions WHERE id = ?`, actionID)
	action, err := scanAction(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Action{}, ErrNotFound
	}
	if err != nil {
		return Action{}, fmt.Errorf("read action: %w", err)
	}
	return action, nil
}

func (s *Store) actionByIdempotencyKey(ctx context.Context, serverID, key string) (Action, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+actionColumns+` FROM actions WHERE server_id = ? AND idempotency_key = ?`,
		serverID, key)
	action, err := scanAction(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Action{}, ErrNotFound
	}
	if err != nil {
		return Action{}, fmt.Errorf("read action by idempotency key: %w", err)
	}
	return action, nil
}

// ExpireActions flips every non-terminal action past its deadline to expired
// (spec section 7). It runs on a timer and lazily before reads; both callers
// tolerate racing each other because the WHERE clause makes the flip
// idempotent.
func (s *Store) ExpireActions(ctx context.Context) (int, error) {
	now := formatTime(time.Now().UTC())
	result, err := s.db.ExecContext(ctx,
		`UPDATE actions SET state = ?, finished_at = ?
		  WHERE state IN (?, ?, ?) AND expires_at <= ?`,
		ActionExpired, now, ActionQueued, ActionDelivered, ActionRunning, now)
	if err != nil {
		return 0, fmt.Errorf("expire actions: %w", err)
	}
	expired, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("expire actions: %w", err)
	}
	return int(expired), nil
}

// markDelivered flips queued actions whose dispatch envelopes the plugin just
// acked, inside the ack's transaction. The envelope carries the action, so its
// ack is the delivery receipt (spec section 7). Expiry is not checked here:
// the flip to expired is the sweeper's job, and delivered is not a terminal
// state, so a late receipt loses nothing.
func markDelivered(ctx context.Context, tx *sql.Tx, sessionID string, ack int64, now time.Time) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE actions SET state = ?, delivered_at = ?
		  WHERE state = ? AND envelope_id IN
		        (SELECT id FROM outbound_envelopes WHERE session_id = ? AND seq <= ?)`,
		ActionDelivered, formatTime(now), ActionQueued, sessionID, ack,
	); err != nil {
		return fmt.Errorf("mark actions delivered: %w", err)
	}
	return nil
}

// applyActionAck moves an action to running (spec section 7). It is a no-op
// for an unknown id, a terminal state, or an action past its deadline: the ack
// beat the sweeper, but expiry still wins, and at-least-once delivery makes a
// repeat ack ordinary rather than an error.
func applyActionAck(ctx context.Context, tx *sql.Tx, actionID string, now time.Time) (bool, error) {
	result, err := tx.ExecContext(ctx,
		`UPDATE actions SET state = ?, running_at = ?
		  WHERE id = ? AND state IN (?, ?) AND expires_at > ?`,
		ActionRunning, formatTime(now), actionID, ActionQueued, ActionDelivered, formatTime(now))
	if err != nil {
		return false, fmt.Errorf("apply action ack: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("apply action ack: %w", err)
	}
	return affected > 0, nil
}

// ActionResult is one action.result to apply.
type ActionResult struct {
	ActionID   string
	OK         bool
	Result     json.RawMessage
	Error      string
	DurationMs *int64
}

// applyActionResult finishes an action (spec section 7). Like the ack it is a
// no-op for an unknown id, a terminal state, or an action past its deadline: a
// result that arrives after expiry changes nothing, because the operator has
// already been told the action expired and a state that flip-flops afterwards
// would be worse than a lost result.
func applyActionResult(ctx context.Context, tx *sql.Tx, result ActionResult, now time.Time) (bool, error) {
	state := ActionCompleted
	if !result.OK {
		state = ActionFailed
	}
	resultJSON := any(nil)
	if len(result.Result) > 0 {
		resultJSON = string(result.Result)
	}
	errorText := any(nil)
	if result.Error != "" {
		errorText = result.Error
	}

	outcome, err := tx.ExecContext(ctx,
		`UPDATE actions
		    SET state = ?, finished_at = ?, running_at = COALESCE(running_at, ?),
		        ok = ?, result = ?, error = ?, duration_ms = ?
		  WHERE id = ? AND state IN (?, ?, ?) AND expires_at > ?`,
		state, formatTime(now), formatTime(now),
		result.OK, resultJSON, errorText, result.DurationMs,
		result.ActionID, ActionQueued, ActionDelivered, ActionRunning, formatTime(now))
	if err != nil {
		return false, fmt.Errorf("apply action result: %w", err)
	}
	affected, err := outcome.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("apply action result: %w", err)
	}
	return affected > 0, nil
}

func scanAction(row rowScanner) (Action, error) {
	var (
		action                           Action
		params                           string
		idempotencyKey                   sql.NullString
		createdAt, expiresAt             string
		deliveredAt, runningAt, finished sql.NullString
		ok                               sql.NullBool
		result, errorText                sql.NullString
		durationMs                       sql.NullInt64
	)
	if err := row.Scan(&action.ID, &action.ServerID, &action.Code, &action.Context,
		&action.ReferenceKey, &params, &idempotencyKey, &action.EnvelopeID, &action.State,
		&createdAt, &expiresAt, &deliveredAt, &runningAt, &finished,
		&ok, &result, &errorText, &durationMs,
	); err != nil {
		return Action{}, err
	}

	action.Params = json.RawMessage(params)
	action.IdempotencyKey = idempotencyKey.String

	var err error
	if action.CreatedAt, err = parseTime(createdAt); err != nil {
		return Action{}, err
	}
	if action.ExpiresAt, err = parseTime(expiresAt); err != nil {
		return Action{}, err
	}
	if action.DeliveredAt, err = scanTime(deliveredAt); err != nil {
		return Action{}, err
	}
	if action.RunningAt, err = scanTime(runningAt); err != nil {
		return Action{}, err
	}
	if action.FinishedAt, err = scanTime(finished); err != nil {
		return Action{}, err
	}
	if ok.Valid {
		action.OK = &ok.Bool
	}
	if result.Valid && result.String != "" {
		action.Result = json.RawMessage(result.String)
	}
	action.Error = errorText.String
	if durationMs.Valid {
		action.DurationMs = &durationMs.Int64
	}
	return action, nil
}
