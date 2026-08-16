package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/That1Drifter/vyshka/hub/internal/id"
)

// Errors the envelope queries return, mapped onto protocol error codes by the
// handlers (spec/protocol.md sections 3.1.2 and 5.5).
var (
	ErrOutboundQueueFull = errors.New("the server's outbound envelope queue is full")
	ErrAckOutOfRange     = errors.New("ack is above the highest seq sent on this session")
)

// liveSessionSeq reads a session's sequence state, but only while the session
// is still live. Every envelope operation goes through it.
//
// The liveness predicate is the load-bearing part. A poll authenticates, and
// only then reaches the store; in between, its session can be superseded by a
// restarting game server or revoked by an operator. Matching on the row's
// existence alone would let that stale poll renumber envelopes into a dead
// session, stealing them from the live one and silently voiding the ack the
// live session had already sent. Answering `401 session_invalid` is the only
// correct outcome, so a dead session must look like no session here.
func liveSessionSeq(ctx context.Context, tx *sql.Tx, sessionID string, columns string, dest ...any) error {
	err := tx.QueryRowContext(ctx,
		`SELECT `+columns+`
		   FROM sessions
		  WHERE id = ? AND ended_at IS NULL AND expires_at > ?`,
		sessionID, formatTime(time.Now().UTC()),
	).Scan(dest...)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("read live session sequence state: %w", err)
	}
	return nil
}

// OutboundEnvelope is one queued hub -> plugin message. Seq is 0 until the
// envelope has been numbered into a session (spec section 9.2): an envelope
// queued while the game server is down has no sequence number yet, because it
// does not yet belong to a sequence space.
type OutboundEnvelope struct {
	ID        string
	Type      string
	Body      json.RawMessage
	CreatedAt time.Time
	Seq       int64
}

// QueueEnvelope puts one envelope on a server's queue. limit bounds the queue:
// at the bound the hub refuses new work rather than dropping envelopes it has
// already accepted (spec section 9.2).
func (s *Store) QueueEnvelope(ctx context.Context, serverID, envelopeType string, body []byte, limit int) (OutboundEnvelope, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return OutboundEnvelope{}, fmt.Errorf("begin queue envelope: %w", err)
	}
	defer tx.Rollback()

	var exists string
	switch err := tx.QueryRowContext(ctx, `SELECT id FROM servers WHERE id = ?`, serverID).Scan(&exists); {
	case errors.Is(err, sql.ErrNoRows):
		return OutboundEnvelope{}, ErrNotFound
	case err != nil:
		return OutboundEnvelope{}, fmt.Errorf("read server: %w", err)
	}

	envelope, err := queueOutbound(ctx, tx, serverID, envelopeType, body, limit)
	if err != nil {
		return OutboundEnvelope{}, err
	}

	if err := tx.Commit(); err != nil {
		return OutboundEnvelope{}, fmt.Errorf("commit queue envelope: %w", err)
	}
	return envelope, nil
}

// queueOutbound inserts one hub -> plugin envelope inside an open transaction,
// enforcing the per-server queue bound. The caller has already established that
// the server exists.
func queueOutbound(ctx context.Context, tx *sql.Tx, serverID, envelopeType string, body []byte, limit int) (OutboundEnvelope, error) {
	envelope := newOutbound(envelopeType, body)
	if err := insertOutbound(ctx, tx, serverID, envelope, limit); err != nil {
		return OutboundEnvelope{}, err
	}
	return envelope, nil
}

// newOutbound frames a hub -> plugin envelope, assigning its id and ts.
func newOutbound(envelopeType string, body []byte) OutboundEnvelope {
	now := time.Now().UTC()
	return OutboundEnvelope{
		ID:        id.NewAt(now),
		Type:      envelopeType,
		Body:      json.RawMessage(body),
		CreatedAt: now.Truncate(time.Millisecond),
	}
}

// insertOutbound writes a framed envelope inside an open transaction,
// enforcing the per-server queue bound.
func insertOutbound(ctx context.Context, tx *sql.Tx, serverID string, envelope OutboundEnvelope, limit int) error {
	var pending int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM outbound_envelopes WHERE server_id = ? AND acked_at IS NULL`,
		serverID).Scan(&pending); err != nil {
		return fmt.Errorf("count pending envelopes: %w", err)
	}
	if limit > 0 && pending >= limit {
		return ErrOutboundQueueFull
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO outbound_envelopes (id, server_id, type, body, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		envelope.ID, serverID, envelope.Type, string(envelope.Body), formatTime(envelope.CreatedAt),
	); err != nil {
		return fmt.Errorf("insert envelope: %w", err)
	}
	return nil
}

// PendingEnvelopeCount reports how many envelopes are queued for a server and
// not yet acked (spec section 9.4).
func (s *Store) PendingEnvelopeCount(ctx context.Context, serverID string) (int, error) {
	var pending int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM outbound_envelopes WHERE server_id = ? AND acked_at IS NULL`,
		serverID).Scan(&pending); err != nil {
		return 0, fmt.Errorf("count pending envelopes: %w", err)
	}
	return pending, nil
}

// NextOutbound returns every unacked envelope for the session's server, oldest
// first, numbering anything that does not yet carry a sequence number in this
// session. Envelopes already sent and not yet acked come back again, unchanged:
// that is the retransmission rule of spec section 9.1, and it is what makes an
// interrupted poll cost nothing.
//
// It returns the batch, the session's outbound sequence high-water mark after
// numbering, and the committed inbound ack. The inbound ack rides along because
// a poll that held for seconds must report the ack as committed *now*, not the
// value it ingested before the hold: another poll may have advanced it in
// between, and an ack the hub has already reported must never be lowered
// (section 9.1).
func (s *Store) NextOutbound(ctx context.Context, sessionID, serverID string, limit int) ([]OutboundEnvelope, int64, int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("begin next outbound: %w", err)
	}
	defer tx.Rollback()

	var outboundSeq, inboundAck int64
	if err := liveSessionSeq(ctx, tx, sessionID, "outbound_seq, inbound_ack",
		&outboundSeq, &inboundAck); err != nil {
		return nil, 0, 0, err
	}

	rows, err := tx.QueryContext(ctx,
		`SELECT id, type, body, created_at, session_id, seq
		   FROM outbound_envelopes
		  WHERE server_id = ? AND acked_at IS NULL
		  ORDER BY created_at, id
		  LIMIT ?`, serverID, limit)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("read outbound envelopes: %w", err)
	}

	type pending struct {
		envelope OutboundEnvelope
		numbered bool
	}
	var batch []pending
	for rows.Next() {
		var (
			row       pending
			body      string
			createdAt string
			sessionOf sql.NullString
			seq       sql.NullInt64
		)
		if err := rows.Scan(&row.envelope.ID, &row.envelope.Type, &body, &createdAt,
			&sessionOf, &seq); err != nil {
			rows.Close()
			return nil, 0, 0, fmt.Errorf("scan outbound envelope: %w", err)
		}
		row.envelope.Body = json.RawMessage(body)
		if row.envelope.CreatedAt, err = parseTime(createdAt); err != nil {
			rows.Close()
			return nil, 0, 0, err
		}
		// A seq from an earlier session means nothing here: sequence spaces are
		// per session, so the envelope is renumbered into this one.
		if sessionOf.Valid && sessionOf.String == sessionID && seq.Valid {
			row.envelope.Seq = seq.Int64
			row.numbered = true
		}
		batch = append(batch, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, 0, 0, fmt.Errorf("read outbound envelopes: %w", err)
	}
	rows.Close()

	if len(batch) == 0 {
		return nil, outboundSeq, inboundAck, tx.Commit()
	}

	sentAt := formatTime(time.Now().UTC())
	for i := range batch {
		if !batch[i].numbered {
			outboundSeq++
			batch[i].envelope.Seq = outboundSeq
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE outbound_envelopes SET session_id = ?, seq = ?, sent_at = ? WHERE id = ?`,
			sessionID, batch[i].envelope.Seq, sentAt, batch[i].envelope.ID,
		); err != nil {
			return nil, 0, 0, fmt.Errorf("number outbound envelope: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE sessions SET outbound_seq = ? WHERE id = ?`, outboundSeq, sessionID,
	); err != nil {
		return nil, 0, 0, fmt.Errorf("record outbound sequence: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, 0, 0, fmt.Errorf("commit next outbound: %w", err)
	}

	envelopes := make([]OutboundEnvelope, len(batch))
	for i := range batch {
		envelopes[i] = batch[i].envelope
	}
	return envelopes, outboundSeq, inboundAck, nil
}

// AckOutbound applies the plugin's ack: everything at or below it is delivered
// and done. Acks are cumulative and monotonic, so a repeat or a stale ack is a
// no-op rather than a resurrection (spec section 9.1). An ack above what the hub
// has actually sent is ErrAckOutOfRange: the plugin has lost track of the
// session, and pretending otherwise would drop real work.
func (s *Store) AckOutbound(ctx context.Context, sessionID string, ack int64) error {
	if ack <= 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin ack outbound: %w", err)
	}
	defer tx.Rollback()

	var outboundSeq, outboundAck int64
	if err := liveSessionSeq(ctx, tx, sessionID, "outbound_seq, outbound_ack",
		&outboundSeq, &outboundAck); err != nil {
		return err
	}

	if ack > outboundSeq {
		return ErrAckOutOfRange
	}
	if ack <= outboundAck {
		return nil
	}

	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx,
		`UPDATE outbound_envelopes SET acked_at = ?
		  WHERE session_id = ? AND seq <= ? AND acked_at IS NULL`,
		formatTime(now), sessionID, ack,
	); err != nil {
		return fmt.Errorf("ack outbound envelopes: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE sessions SET outbound_ack = ? WHERE id = ?`, ack, sessionID,
	); err != nil {
		return fmt.Errorf("record outbound ack: %w", err)
	}
	// The dispatch envelope carries the action, so this ack is the delivery
	// receipt for any action it carried (spec section 7).
	if err := markDelivered(ctx, tx, sessionID, ack, now); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit ack outbound: %w", err)
	}
	return nil
}

// InboundApplication is what one poll's batch should do to the session,
// derived by the classify callback from the committed ack.
type InboundApplication struct {
	// Ack is the highest contiguous inbound seq after applying the batch, and
	// Accepted how many envelopes the advance covers.
	Ack      int64
	Accepted int
	// Manifests are validated manifest.publish bodies among the newly accepted
	// envelopes, in arrival order. They are applied in the same transaction as
	// the ack that covers them, because acking an envelope promises its effect
	// is already durable (spec section 9.3).
	Manifests []ManifestPublish
	// Notices are hub -> plugin envelopes to queue in the same transaction,
	// such as a manifest rejection. A notice that would overflow the outbound
	// queue is dropped rather than failing the poll.
	Notices []Notice
	// ActionAcks are action ids the plugin reported receipt of (-> running),
	// and ActionResults the outcomes it reported (-> completed/failed), both
	// applied in this transaction (spec section 7). Unknown ids, terminal
	// states, and actions past their deadline are no-ops, never errors.
	ActionAcks    []string
	ActionResults []ActionResult
}

// ManifestPublish is one validated manifest to store, revision-gated.
type ManifestPublish struct {
	Revision int64
	Body     json.RawMessage
}

// Notice is one hub -> plugin envelope queued as a side effect of ingest.
type Notice struct {
	Type string
	Body json.RawMessage
}

// InboundApplied reports what an ApplyInbound call committed.
type InboundApplied struct {
	// Ack is the inbound ack that is now durable.
	Ack int64
	// ManifestsApplied counts manifests that replaced the stored one; the rest
	// carried an equal or lower revision and were ignored (spec section 6.1).
	ManifestsApplied int
	// NoticesQueued counts notices that made it onto the queue.
	NoticesQueued int
	// ActionsStarted counts acks that moved an action to running, and
	// ActionsFinished results that reached a terminal state; the difference
	// against what was sent is late or duplicate traffic, which is normal.
	ActionsStarted  int
	ActionsFinished int
}

// ApplyInbound applies a poll's envelopes to the session's inbound ack, plus
// whatever effects the accepted envelopes carry, in one transaction (spec
// sections 9.1 and 9.3).
//
// classify receives the ack as it stands inside this transaction and returns
// what to apply. Passing the rule in as a callback, rather than passing a
// precomputed result in, is what makes the outcome correct when two polls
// overlap: each one classifies against committed state instead of against a
// snapshot taken back when it authenticated. The alternative reports an ack
// lower than one the hub already gave out, which section 9.1 forbids outright,
// and double-counts duplicates on the way.
//
// The read and the writes are one transaction. On SQLite that is enough because
// the pool holds a single connection; a Postgres backend would need the read in
// liveSessionSeq to take a row lock. See the Postgres note in resolveDSN.
func (s *Store) ApplyInbound(ctx context.Context, sessionID string, classify func(ack int64) InboundApplication, noticeQueueLimit int) (InboundApplied, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return InboundApplied{}, fmt.Errorf("begin apply inbound: %w", err)
	}
	defer tx.Rollback()

	var (
		inboundAck int64
		serverID   string
	)
	if err := liveSessionSeq(ctx, tx, sessionID, "inbound_ack, server_id",
		&inboundAck, &serverID); err != nil {
		return InboundApplied{}, err
	}

	application := classify(inboundAck)
	if application.Ack < inboundAck {
		// A classifier must never go backwards. Distrust everything it derived
		// and treat the batch as a no-op rather than write a regression into
		// the session.
		application = InboundApplication{Ack: inboundAck}
	}

	if application.Accepted > 0 && application.Ack > inboundAck {
		if _, err := tx.ExecContext(ctx,
			`UPDATE sessions SET inbound_ack = ?, inbound_count = inbound_count + ?
			  WHERE id = ?`,
			application.Ack, application.Accepted, sessionID,
		); err != nil {
			return InboundApplied{}, fmt.Errorf("record inbound ack: %w", err)
		}
	}

	applied := InboundApplied{Ack: application.Ack}
	now := time.Now().UTC()
	for _, manifest := range application.Manifests {
		replaced, err := applyManifest(ctx, tx, serverID, manifest.Revision, manifest.Body, now)
		if err != nil {
			return InboundApplied{}, err
		}
		if replaced {
			applied.ManifestsApplied++
		}
	}
	for _, notice := range application.Notices {
		switch _, err := queueOutbound(ctx, tx, serverID, notice.Type, notice.Body, noticeQueueLimit); {
		case errors.Is(err, ErrOutboundQueueFull):
			// Dropped. The queue bound protects the plugin's own traffic, and a
			// notice must never turn a valid poll into a failure.
		case err != nil:
			return InboundApplied{}, err
		default:
			applied.NoticesQueued++
		}
	}
	for _, actionID := range application.ActionAcks {
		started, err := applyActionAck(ctx, tx, serverID, actionID, now)
		if err != nil {
			return InboundApplied{}, err
		}
		if started {
			applied.ActionsStarted++
		}
	}
	for _, result := range application.ActionResults {
		finished, err := applyActionResult(ctx, tx, serverID, result, now)
		if err != nil {
			return InboundApplied{}, err
		}
		if finished {
			applied.ActionsFinished++
		}
	}

	if err := tx.Commit(); err != nil {
		return InboundApplied{}, fmt.Errorf("commit apply inbound: %w", err)
	}
	return applied, nil
}

// TouchServer records that the plugin was heard from, which is the operator's
// only view of link liveness between sessions (spec section 9.4).
func (s *Store) TouchServer(ctx context.Context, serverID string) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE servers SET last_seen_at = ? WHERE id = ?`,
		formatTime(time.Now().UTC()), serverID,
	); err != nil {
		return fmt.Errorf("touch server: %w", err)
	}
	return nil
}
