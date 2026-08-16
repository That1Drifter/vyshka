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
	now := time.Now().UTC()
	envelope := OutboundEnvelope{
		ID:        id.NewAt(now),
		Type:      envelopeType,
		Body:      json.RawMessage(body),
		CreatedAt: now.Truncate(time.Millisecond),
	}

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

	var pending int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM outbound_envelopes WHERE server_id = ? AND acked_at IS NULL`,
		serverID).Scan(&pending); err != nil {
		return OutboundEnvelope{}, fmt.Errorf("count pending envelopes: %w", err)
	}
	if limit > 0 && pending >= limit {
		return OutboundEnvelope{}, ErrOutboundQueueFull
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO outbound_envelopes (id, server_id, type, body, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		envelope.ID, serverID, envelope.Type, string(body), formatTime(envelope.CreatedAt),
	); err != nil {
		return OutboundEnvelope{}, fmt.Errorf("insert envelope: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return OutboundEnvelope{}, fmt.Errorf("commit queue envelope: %w", err)
	}
	return envelope, nil
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
// It returns the batch and the session's sequence high-water mark after
// numbering, so the caller can keep its own copy of the session in step.
func (s *Store) NextOutbound(ctx context.Context, sessionID, serverID string, limit int) ([]OutboundEnvelope, int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("begin next outbound: %w", err)
	}
	defer tx.Rollback()

	var outboundSeq int64
	if err := liveSessionSeq(ctx, tx, sessionID, "outbound_seq", &outboundSeq); err != nil {
		return nil, 0, err
	}

	rows, err := tx.QueryContext(ctx,
		`SELECT id, type, body, created_at, session_id, seq
		   FROM outbound_envelopes
		  WHERE server_id = ? AND acked_at IS NULL
		  ORDER BY created_at, id
		  LIMIT ?`, serverID, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("read outbound envelopes: %w", err)
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
			return nil, 0, fmt.Errorf("scan outbound envelope: %w", err)
		}
		row.envelope.Body = json.RawMessage(body)
		if row.envelope.CreatedAt, err = parseTime(createdAt); err != nil {
			rows.Close()
			return nil, 0, err
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
		return nil, 0, fmt.Errorf("read outbound envelopes: %w", err)
	}
	rows.Close()

	if len(batch) == 0 {
		return nil, outboundSeq, tx.Commit()
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
			return nil, 0, fmt.Errorf("number outbound envelope: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE sessions SET outbound_seq = ? WHERE id = ?`, outboundSeq, sessionID,
	); err != nil {
		return nil, 0, fmt.Errorf("record outbound sequence: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, 0, fmt.Errorf("commit next outbound: %w", err)
	}

	envelopes := make([]OutboundEnvelope, len(batch))
	for i := range batch {
		envelopes[i] = batch[i].envelope
	}
	return envelopes, outboundSeq, nil
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

	if _, err := tx.ExecContext(ctx,
		`UPDATE outbound_envelopes SET acked_at = ?
		  WHERE session_id = ? AND seq <= ? AND acked_at IS NULL`,
		formatTime(time.Now().UTC()), sessionID, ack,
	); err != nil {
		return fmt.Errorf("ack outbound envelopes: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE sessions SET outbound_ack = ? WHERE id = ?`, ack, sessionID,
	); err != nil {
		return fmt.Errorf("record outbound ack: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit ack outbound: %w", err)
	}
	return nil
}

// AdvanceInbound applies a poll's envelopes to the session's inbound ack and
// returns the ack that is now durably committed (spec section 9.3).
//
// classify receives the ack as it stands inside this transaction and returns
// the new ack plus how many envelopes the advance covers. Passing the rule in
// as a callback, rather than passing a precomputed ack in, is what makes the
// result correct when two polls overlap: each one classifies against committed
// state instead of against a snapshot taken back when it authenticated. The
// alternative reports an ack lower than one the hub already gave out, which
// section 9.1 forbids outright, and double-counts duplicates on the way.
//
// The read and the write are one transaction. On SQLite that is enough because
// the pool holds a single connection; a Postgres backend would need the read to
// take a row lock.
func (s *Store) AdvanceInbound(ctx context.Context, sessionID string, classify func(ack int64) (int64, int)) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin advance inbound: %w", err)
	}
	defer tx.Rollback()

	var inboundAck int64
	if err := liveSessionSeq(ctx, tx, sessionID, "inbound_ack", &inboundAck); err != nil {
		return 0, err
	}

	newAck, accepted := classify(inboundAck)
	if newAck < inboundAck {
		// A classifier must never go backwards; treat it as a no-op rather
		// than write a regression into the session.
		newAck = inboundAck
		accepted = 0
	}

	if accepted > 0 && newAck > inboundAck {
		if _, err := tx.ExecContext(ctx,
			`UPDATE sessions SET inbound_ack = ?, inbound_count = inbound_count + ?
			  WHERE id = ?`,
			newAck, accepted, sessionID,
		); err != nil {
			return 0, fmt.Errorf("record inbound ack: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit advance inbound: %w", err)
	}
	return newAck, nil
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
