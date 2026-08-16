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

// ErrManifestChanged reports that the manifest a dispatch was validated
// against was replaced before the dispatch could commit. The caller
// revalidates against the new manifest and retries.
var ErrManifestChanged = errors.New("the manifest changed since the dispatch was validated")

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
	// ExpiresAt is the action's one deadline: the same instant the dispatch
	// envelope advertises to the plugin. Computed once by the caller, so the
	// database can never hold a later deadline than the wire promised.
	ExpiresAt time.Time
	// ManifestRevision is the revision the dispatch was validated against.
	// The insert is gated on it still being the stored revision, closing the
	// window in which a republish could invalidate the validation that
	// already happened (ErrManifestChanged when it lost the race).
	ManifestRevision int64
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

	// The validation the caller performed is only as good as the manifest it
	// read, and that read happened outside this transaction. Re-checking the
	// revision here closes the window: a dispatch that lost the race to a
	// republish is refused and revalidated rather than queued with params the
	// plugin's current manifest never approved.
	var storedRevision int64
	switch err := tx.QueryRowContext(ctx,
		`SELECT revision FROM manifests WHERE server_id = ?`, request.ServerID,
	).Scan(&storedRevision); {
	case errors.Is(err, sql.ErrNoRows):
		return Action{}, false, ErrManifestChanged
	case err != nil:
		return Action{}, false, fmt.Errorf("read manifest revision: %w", err)
	case storedRevision != request.ManifestRevision:
		return Action{}, false, ErrManifestChanged
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
		formatTime(now), formatTime(request.ExpiresAt),
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
		original, err := s.ActionByIdempotencyKey(ctx, request.ServerID, request.IdempotencyKey)
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

// ActionByID returns one action, applying lazy expiry to that action first: a
// read must never report a non-terminal state for an action whose deadline has
// passed, however recently the sweeper last ran. Expiry here is per row rather
// than the sweeper's global pass, so a hot read path never pays for, or
// contends on, everyone else's deadlines.
func (s *Store) ActionByID(ctx context.Context, actionID string) (Action, error) {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE actions SET state = ?, finished_at = ?
		  WHERE id = ? AND state IN (?, ?, ?) AND expires_at <= ?`,
		ActionExpired, formatTime(time.Now().UTC()), actionID,
		ActionQueued, ActionDelivered, ActionRunning, formatTime(time.Now().UTC()),
	); err != nil {
		return Action{}, fmt.Errorf("expire action on read: %w", err)
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

// ActionCode returns just an action's code, without the lazy expiry ActionByID
// applies. It exists for authorization: the scope a read needs depends on the
// code (spec section 10.2), so the code has to be known before the caller has
// been allowed to touch the row at all. Reading through ActionByID instead
// would let a token with no grant over this action drive a state change, which
// is a write, on a route that is not even audited.
func (s *Store) ActionCode(ctx context.Context, actionID string) (string, error) {
	var code string
	switch err := s.db.QueryRowContext(ctx,
		`SELECT code FROM actions WHERE id = ?`, actionID).Scan(&code); {
	case errors.Is(err, sql.ErrNoRows):
		return "", ErrNotFound
	case err != nil:
		return "", fmt.Errorf("read action code: %w", err)
	}
	return code, nil
}

// ActionByIdempotencyKey returns the action a client-chosen key names, or
// ErrNotFound. Callers use it to honor the retry contract before anything
// else, validation included, can refuse the request (spec section 7).
//
// The action it returns may carry a different code than the request that
// presented the key: the key is client-chosen and keyed only on the server. A
// caller MUST therefore authorize against the returned action's own code as
// well as the one it asked for.
func (s *Store) ActionByIdempotencyKey(ctx context.Context, serverID, key string) (Action, error) {
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
// (spec section 7), and retires the dispatch envelope of any expiring action
// that was never numbered into a session: undelivered work whose deadline
// passed would otherwise sit on the queue forever, counting against the
// section 9.2 bound until an offline server could refuse all fresh work.
//
// An envelope already numbered into a session is deliberately left alone.
// Deleting it would open a gap in a live sequence space that the plugin's
// contiguous ack could never cross; retransmission delivers it, and the
// deadline in its body tells the plugin to discard it (section 7).
func (s *Store) ExpireActions(ctx context.Context) (int, error) {
	now := formatTime(time.Now().UTC())

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin expire actions: %w", err)
	}
	defer tx.Rollback()

	// Expired actions are included alongside queued ones because the lazy
	// per-row expiry in ActionByID can flip an action first; the sweep still
	// owes its envelope a retirement on the next pass.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM outbound_envelopes
		  WHERE acked_at IS NULL AND session_id IS NULL
		    AND id IN (SELECT envelope_id FROM actions
		                WHERE state IN (?, ?) AND expires_at <= ?)`,
		ActionQueued, ActionExpired, now,
	); err != nil {
		return 0, fmt.Errorf("retire expired dispatch envelopes: %w", err)
	}

	result, err := tx.ExecContext(ctx,
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

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit expire actions: %w", err)
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
//
// serverID scopes the update to the session that sent the ack. Without it, any
// enrolled plugin could finish any other server's action by guessing or
// leaking an actionId; ids are not credentials, and the session's server is.
// An ack implies receipt, so a missing delivered_at is filled in on the way:
// the plugin can only be acking a dispatch it received.
func applyActionAck(ctx context.Context, tx *sql.Tx, serverID, actionID string, now time.Time) (bool, error) {
	result, err := tx.ExecContext(ctx,
		`UPDATE actions
		    SET state = ?, running_at = ?, delivered_at = COALESCE(delivered_at, ?)
		  WHERE id = ? AND server_id = ? AND state IN (?, ?) AND expires_at > ?`,
		ActionRunning, formatTime(now), formatTime(now),
		actionID, serverID, ActionQueued, ActionDelivered, formatTime(now))
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
// would be worse than a lost result. serverID scopes the update for the same
// reason as in applyActionAck: an actionId is not a credential.
func applyActionResult(ctx context.Context, tx *sql.Tx, serverID string, result ActionResult, now time.Time) (bool, error) {
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
		        delivered_at = COALESCE(delivered_at, ?),
		        ok = ?, result = ?, error = ?, duration_ms = ?
		  WHERE id = ? AND server_id = ? AND state IN (?, ?, ?) AND expires_at > ?`,
		state, formatTime(now), formatTime(now), formatTime(now),
		result.OK, resultJSON, errorText, result.DurationMs,
		result.ActionID, serverID, ActionQueued, ActionDelivered, ActionRunning, formatTime(now))
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
