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
	switch err := tx.QueryRowContext(ctx,
		`SELECT outbound_seq FROM sessions WHERE id = ?`, sessionID).Scan(&outboundSeq); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, 0, ErrNotFound
	case err != nil:
		return nil, 0, fmt.Errorf("read session sequence: %w", err)
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
	switch err := tx.QueryRowContext(ctx,
		`SELECT outbound_seq, outbound_ack FROM sessions WHERE id = ?`, sessionID,
	).Scan(&outboundSeq, &outboundAck); {
	case errors.Is(err, sql.ErrNoRows):
		return ErrNotFound
	case err != nil:
		return fmt.Errorf("read session sequence: %w", err)
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

// RecordInbound durably advances the session's inbound ack. It is the write that
// makes the ack honest: the hub reports the number only after this commits (spec
// section 9.3). accepted counts the envelopes the advance covers.
func (s *Store) RecordInbound(ctx context.Context, sessionID string, ack int64, accepted int) error {
	if accepted == 0 {
		return nil
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET inbound_ack = ?, inbound_count = inbound_count + ?
		  WHERE id = ? AND inbound_ack < ?`,
		ack, accepted, sessionID, ack,
	); err != nil {
		return fmt.Errorf("record inbound ack: %w", err)
	}
	return nil
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
