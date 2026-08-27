package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/That1Drifter/vyshka/hub/internal/id"
)

// Event is one stored telemetry record (spec section 8.1).
type Event struct {
	ID         string
	ServerID   string
	Type       string
	OccurredAt time.Time
	ReceivedAt time.Time
	Data       json.RawMessage
}

// NewEvent is one event to store, already validated by the caller.
type NewEvent struct {
	Type string
	// OccurredAt is when the game server says it happened. Nil means it sent
	// no usable timestamp and receipt time stands in, the same substitution
	// section 4 requires for an envelope's `ts`.
	OccurredAt *time.Time
	Data       json.RawMessage
	// Retention is how long this event is kept, resolved from the hub's
	// configuration against its type. It is counted from receipt rather than
	// from OccurredAt, so that a game server with a wrong clock cannot talk its
	// own telemetry into instant deletion or into immortality.
	Retention time.Duration
}

// NewEventBatch is one accepted event.batch envelope's worth of events.
type NewEventBatch struct {
	// EnvelopeID is the id of the event.batch envelope that carried these
	// events. It is what deduplicates a retransmission across a session
	// change, where seq is renumbered and only the id survives (spec section
	// 9.1); a batch whose id was already ingested stores nothing.
	EnvelopeID string
	Events     []NewEvent
}

// insertEventBatches appends batches inside an open transaction, so that the
// ack covering the envelopes and the events they carried commit together:
// acking an envelope promises its effect is already durable (spec section
// 9.3). It reports how many events were actually stored.
//
// Each batch's envelope id is recorded first, ON CONFLICT DO NOTHING rather
// than failing: a retransmission renumbered into a new session is accepted by
// the sequence layer (its seq is fresh) and only the envelope id reveals its
// events were already stored. Re-storing them would put every one in the feed
// twice with fresh ids and a fresh received_at, and fire webhook fan-out twice
// for each. The dedup row expires with the batch's longest-lived event, so it
// cannot be pruned while a duplicate it guards against would still be visible.
func insertEventBatches(ctx context.Context, tx *sql.Tx, serverID string, batches []NewEventBatch, now time.Time) (int, error) {
	total := 0
	for _, batch := range batches {
		// An empty batch stores nothing, so a replay of it double-stores
		// nothing either: no dedup row to burn an id on.
		if len(batch.Events) == 0 {
			continue
		}

		horizon := time.Duration(0)
		for _, event := range batch.Events {
			horizon = max(horizon, event.Retention)
		}
		claimed, err := tx.ExecContext(ctx,
			`INSERT INTO event_batches (server_id, envelope_id, expires_at)
			 VALUES (?, ?, ?)
			 ON CONFLICT (server_id, envelope_id) DO NOTHING`,
			serverID, batch.EnvelopeID, formatTime(now.Add(horizon)))
		if err != nil {
			return 0, fmt.Errorf("record event batch: %w", err)
		}
		inserted, err := claimed.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("record event batch: %w", err)
		}
		if inserted == 0 {
			continue
		}

		stored, err := insertEvents(ctx, tx, serverID, batch.Events, now)
		if err != nil {
			return 0, err
		}
		total += stored
	}
	return total, nil
}

// insertEvents appends one batch's rows inside an open transaction. The caller
// has already claimed the batch's envelope id.
func insertEvents(ctx context.Context, tx *sql.Tx, serverID string, events []NewEvent, now time.Time) (int, error) {
	statement, err := tx.PrepareContext(ctx,
		`INSERT INTO events (id, server_id, type, occurred_at, received_at, expires_at, data)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, fmt.Errorf("prepare event insert: %w", err)
	}
	defer statement.Close()

	receivedAt := formatTime(now)
	for _, event := range events {
		occurredAt := now
		if event.OccurredAt != nil {
			occurredAt = *event.OccurredAt
		}
		data := event.Data
		if len(data) == 0 {
			data = json.RawMessage(`{}`)
		}
		if _, err := statement.ExecContext(ctx,
			id.NewAt(now), serverID, event.Type, formatTime(occurredAt), receivedAt,
			formatTime(now.Add(event.Retention)), string(data),
		); err != nil {
			return 0, fmt.Errorf("insert event: %w", err)
		}
	}
	return len(events), nil
}

// EventTypeFilter is one `type` term of an event query. Exactly one field is
// set: Exact matches a whole type, Prefix a namespace and everything under it.
// Terms within a query are ORed.
type EventTypeFilter struct {
	Exact string
	// Prefix ends with the separating ".", so "core.player." selects
	// core.player.death but not a type merely starting with "core.player".
	Prefix string
}

// EventCursor is a position in the feed. It is a coordinate rather than a row
// reference, so a cursor whose event has since been pruned still names the
// right place to resume.
type EventCursor struct {
	OccurredAt time.Time
	ID         string
}

// Set reports whether the cursor names a position at all.
func (c EventCursor) Set() bool { return c.ID != "" }

// EventQuery is one page of one server's feed, newest first.
type EventQuery struct {
	ServerID string
	// Types narrow the feed; empty means every type.
	Types []EventTypeFilter
	// Since is inclusive and Until exclusive, both on OccurredAt. A zero time
	// means unbounded on that side.
	Since time.Time
	Until time.Time
	// Limit bounds the page. The caller asks for one row more than it intends
	// to return when it wants to know whether a next page exists.
	Limit int
	// After resumes a previous page; the zero value starts at the newest event.
	After EventCursor
}

// Events answers one page of the feed, newest first, ordered by
// (occurred_at, id) descending. That pair is a strict total order because ids
// are unique, which is what keeps pagination from skipping or repeating rows
// that landed in the same millisecond.
func (s *Store) Events(ctx context.Context, query EventQuery) ([]Event, error) {
	conditions := []string{"server_id = ?"}
	args := []any{query.ServerID}

	if len(query.Types) > 0 {
		terms := make([]string, 0, len(query.Types))
		for _, filter := range query.Types {
			if filter.Prefix == "" {
				terms = append(terms, "type = ?")
				args = append(args, filter.Exact)
				continue
			}
			// A half-open range over the index rather than LIKE: `_` is a LIKE
			// wildcard and a legal namespace character, so `example_mod.%` would
			// quietly match `exampleXmod.raid` too. The upper bound raises the
			// prefix's trailing "." to "/", the next byte in ASCII, which the
			// event type grammar of section 8.1 cannot produce.
			terms = append(terms, "(type >= ? AND type < ?)")
			args = append(args, filter.Prefix, prefixUpperBound(filter.Prefix))
		}
		conditions = append(conditions, "("+strings.Join(terms, " OR ")+")")
	}
	// Both bounds are rounded up to the stored resolution rather than formatted
	// straight in. Timestamps are stored to the millisecond, so truncating a
	// bound of 12:00:00.0005 down to 12:00:00.000 would widen an inclusive
	// `since` to admit an event below it and narrow an exclusive `until` to
	// exclude one it covers. Rounding up keeps the window half-open either way.
	if !query.Since.IsZero() {
		conditions = append(conditions, "occurred_at >= ?")
		args = append(args, formatTime(ceilMillisecond(query.Since)))
	}
	if !query.Until.IsZero() {
		conditions = append(conditions, "occurred_at < ?")
		args = append(args, formatTime(ceilMillisecond(query.Until)))
	}
	if query.After.Set() {
		// Written out rather than as a row-value comparison, which not every
		// backend this store will grow into supports.
		conditions = append(conditions, "(occurred_at < ? OR (occurred_at = ? AND id < ?))")
		after := formatTime(query.After.OccurredAt)
		args = append(args, after, after, query.After.ID)
	}

	args = append(args, query.Limit)
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, server_id, type, occurred_at, received_at, data
		   FROM events
		  WHERE `+strings.Join(conditions, " AND ")+`
		  ORDER BY occurred_at DESC, id DESC
		  LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("read events: %w", err)
	}
	defer rows.Close()

	events := make([]Event, 0, min(query.Limit, 128))
	for rows.Next() {
		var (
			event                  Event
			occurredAt, receivedAt string
			data                   string
		)
		if err := rows.Scan(&event.ID, &event.ServerID, &event.Type,
			&occurredAt, &receivedAt, &data); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		if event.OccurredAt, err = parseTime(occurredAt); err != nil {
			return nil, err
		}
		if event.ReceivedAt, err = parseTime(receivedAt); err != nil {
			return nil, err
		}
		event.Data = json.RawMessage(data)
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read events: %w", err)
	}
	return events, nil
}

// ceilMillisecond rounds a time up to the resolution timestamps are stored at,
// leaving one that already sits on a millisecond alone.
func ceilMillisecond(t time.Time) time.Time {
	truncated := t.Truncate(time.Millisecond)
	if truncated.Equal(t) {
		return t
	}
	return truncated.Add(time.Millisecond)
}

// prefixUpperBound turns a namespace prefix into the exclusive upper bound of
// its range scan by raising the trailing "." to "/". Callers only ever pass a
// prefix that ends in ".", but a defensive fallback beats a silently wrong
// range if that ever stops being true.
func prefixUpperBound(prefix string) string {
	if strings.HasSuffix(prefix, ".") {
		return strings.TrimSuffix(prefix, ".") + "/"
	}
	return prefix + "￿"
}

// defaultPruneBatch bounds a prune whose caller asked for no usable bound.
const defaultPruneBatch = 1000

// PruneEvents deletes up to limit events past their retention (spec section
// 8.4) and reports how many went. It is bounded because the SQLite pool is one
// connection: an unbounded delete over a long-neglected database would hold
// that connection, and therefore the whole hub, for as long as it took.
//
// Retention is the deadline each event was stamped with at ingest, not a rule
// re-derived on every pass. Re-deriving would make a configuration typo destroy
// history the moment it was saved, and would cost a full scan where this costs
// an index range; the price is that re-configuring retention governs what
// arrives next rather than what is already stored.
func (s *Store) PruneEvents(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		// SQLite reads a negative LIMIT as unbounded, which would turn the one
		// call that promises to be bounded into the unbounded delete this
		// method exists to avoid.
		limit = defaultPruneBatch
	}

	now := formatTime(time.Now().UTC())
	result, err := s.db.ExecContext(ctx,
		`DELETE FROM events
		  WHERE id IN (SELECT id FROM events WHERE expires_at <= ? LIMIT ?)`,
		now, limit)
	if err != nil {
		return 0, fmt.Errorf("prune events: %w", err)
	}
	pruned, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune events: %w", err)
	}

	// The batch dedup rows expire on their own column and ride the same pass,
	// bounded the same way. They are not counted in the return: the count is
	// what the caller's loop paces event deletion by, and there is at most one
	// of these per batch of up to 200 events, so any straggler waits one tick.
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM event_batches
		  WHERE (server_id, envelope_id) IN
		        (SELECT server_id, envelope_id FROM event_batches
		          WHERE expires_at <= ? LIMIT ?)`,
		now, limit); err != nil {
		return 0, fmt.Errorf("prune event batches: %w", err)
	}
	return int(pruned), nil
}
