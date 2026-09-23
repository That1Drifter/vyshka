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

// EventIdentity is one player identity an event refers to (spec section 8.2):
// the { platform, id } pair and the names of the top-level members of the
// event's data that held it, in name order.
type EventIdentity struct {
	Platform string
	ID       string
	Roles    []string
}

// inList renders n placeholders for an IN (...) clause. n must be positive.
func inList(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// insertEventIdentities indexes one event's identities inside the ingest
// transaction. The conflict clause is for the background backfill, which can
// meet an event the ingest path already indexed.
func insertEventIdentities(ctx context.Context, tx *Tx, eventID, serverID, eventType, occurredAt string, identities []EventIdentity) error {
	for _, identity := range identities {
		roles, err := json.Marshal(identity.Roles)
		if err != nil {
			return fmt.Errorf("encode identity roles: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO event_identities (event_id, platform, player_id, roles, server_id, type, occurred_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT (event_id, platform, player_id) DO NOTHING`,
			eventID, identity.Platform, identity.ID, string(roles), serverID, eventType, occurredAt,
		); err != nil {
			return fmt.Errorf("index event identity: %w", err)
		}
	}
	return nil
}

// BackfillEventIdentities indexes up to limit of the events that were stored
// before the identity index existed (migration 0020), in id order, and
// reports how many it walked and whether the walk is over. extract is the
// caller's reading of section 8.2, applied to each event's data exactly as the
// ingest path applies it.
//
// The walk's progress is one row, advanced in the same transaction as the
// rows it indexed, so a crash repeats at most one batch, which the conflict
// clause absorbs. It stops at the newest event id that existed when the
// migration ran: every later event was indexed at ingest.
func (s *Store) BackfillEventIdentities(ctx context.Context, limit int, extract func(json.RawMessage) []EventIdentity) (int, bool, error) {
	if limit <= 0 {
		limit = defaultPruneBatch
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, fmt.Errorf("begin identity backfill: %w", err)
	}
	defer tx.Rollback()

	var after, through string
	err = tx.QueryRowContext(ctx,
		`SELECT after_id, through_id FROM event_identity_backfill WHERE id = 1`+tx.forUpdate()).Scan(&after, &through)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, true, tx.Commit()
	case err != nil:
		return 0, false, fmt.Errorf("read identity backfill: %w", err)
	}

	rows, err := tx.QueryContext(ctx,
		`SELECT id, server_id, type, occurred_at, data FROM events
		  WHERE id > ? AND id <= ? ORDER BY id LIMIT ?`, after, through, limit)
	if err != nil {
		return 0, false, fmt.Errorf("read events to index: %w", err)
	}
	type pending struct {
		id, serverID, eventType, occurredAt string
		identities                          []EventIdentity
	}
	batch := make([]pending, 0, limit)
	for rows.Next() {
		var (
			one  pending
			data string
		)
		if err := rows.Scan(&one.id, &one.serverID, &one.eventType, &one.occurredAt, &data); err != nil {
			rows.Close()
			return 0, false, fmt.Errorf("scan event to index: %w", err)
		}
		one.identities = extract(json.RawMessage(data))
		batch = append(batch, one)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, false, fmt.Errorf("read events to index: %w", err)
	}

	for _, one := range batch {
		if err := insertEventIdentities(ctx, tx, one.id, one.serverID, one.eventType, one.occurredAt, one.identities); err != nil {
			return 0, false, err
		}
	}

	done := len(batch) < limit
	if done {
		_, err = tx.ExecContext(ctx, `DELETE FROM event_identity_backfill WHERE id = 1`)
	} else {
		_, err = tx.ExecContext(ctx,
			`UPDATE event_identity_backfill SET after_id = ? WHERE id = 1`, batch[len(batch)-1].id)
	}
	if err != nil {
		return 0, false, fmt.Errorf("advance identity backfill: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("commit identity backfill: %w", err)
	}
	return len(batch), done, nil
}

// PlayerEvent is one event of a player's profile: the stored event and the
// roles the identity holds in it.
type PlayerEvent struct {
	Event
	Roles []string
}

// PlayerEventQuery is one page of one identity's events across servers,
// newest first (spec section 8.6).
type PlayerEventQuery struct {
	Platform string
	PlayerID string
	// Servers confines the answer to these servers, for a bound token; nil
	// means every server.
	Servers []string
	// Types narrow the feed as in EventQuery; empty means every type.
	Types []EventTypeFilter
	Since time.Time
	Until time.Time
	Limit int
	After EventCursor
}

// PlayerEvents answers one page of an identity's events, ordered by
// (occurred_at, id) descending, the total order the server feed uses. The
// filters and the order run on the identity index; the events themselves are
// joined for the rows of the page.
func (s *Store) PlayerEvents(ctx context.Context, query PlayerEventQuery) ([]PlayerEvent, error) {
	conditions := []string{"i.platform = ?", "i.player_id = ?"}
	args := []any{query.Platform, query.PlayerID}

	if query.Servers != nil {
		if len(query.Servers) == 0 {
			return []PlayerEvent{}, nil
		}
		conditions = append(conditions, "i.server_id IN ("+inList(len(query.Servers))+")")
		for _, serverID := range query.Servers {
			args = append(args, serverID)
		}
	}
	if len(query.Types) > 0 {
		condition, terms := typeCondition("i.type", query.Types)
		conditions = append(conditions, condition)
		args = append(args, terms...)
	}
	if !query.Since.IsZero() {
		conditions = append(conditions, "i.occurred_at >= ?")
		args = append(args, formatTime(ceilMillisecond(query.Since)))
	}
	if !query.Until.IsZero() {
		conditions = append(conditions, "i.occurred_at < ?")
		args = append(args, formatTime(ceilMillisecond(query.Until)))
	}
	if query.After.Set() {
		conditions = append(conditions, "(i.occurred_at < ? OR (i.occurred_at = ? AND i.event_id < ?))")
		after := formatTime(query.After.OccurredAt)
		args = append(args, after, after, query.After.ID)
	}

	args = append(args, query.Limit)
	rows, err := s.db.QueryContext(ctx,
		`SELECT e.id, e.server_id, e.type, e.occurred_at, e.received_at, e.data, i.roles
		   FROM event_identities i JOIN events e ON e.id = i.event_id
		  WHERE `+strings.Join(conditions, " AND ")+`
		  ORDER BY i.occurred_at DESC, i.event_id DESC
		  LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("read player events: %w", err)
	}
	defer rows.Close()

	events := make([]PlayerEvent, 0, min(query.Limit, 128))
	for rows.Next() {
		var (
			event                  PlayerEvent
			occurredAt, receivedAt string
			data, roles            string
		)
		if err := rows.Scan(&event.ID, &event.ServerID, &event.Type,
			&occurredAt, &receivedAt, &data, &roles); err != nil {
			return nil, fmt.Errorf("scan player event: %w", err)
		}
		if event.OccurredAt, err = parseTime(occurredAt); err != nil {
			return nil, err
		}
		if event.ReceivedAt, err = parseTime(receivedAt); err != nil {
			return nil, err
		}
		event.Data = json.RawMessage(data)
		if err := json.Unmarshal([]byte(roles), &event.Roles); err != nil {
			return nil, fmt.Errorf("decode player event roles: %w", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read player events: %w", err)
	}
	return events, nil
}

// ActionCursor is a position in an action list ordered by creation, newest
// first: a coordinate rather than a row, like EventCursor.
type ActionCursor struct {
	CreatedAt time.Time
	ID        string
}

// Set reports whether the cursor names a position at all.
func (c ActionCursor) Set() bool { return c.ID != "" }

// PlayerActionQuery is one page of the actions dispatched against one
// identity (spec section 8.6).
type PlayerActionQuery struct {
	// PlayerID is matched against a player-context action's referenceKey,
	// which carries the platform id alone.
	PlayerID string
	// Servers confines the answer, as in PlayerEventQuery; nil means every
	// server.
	Servers []string
	// Codes narrows the answer to the codes the caller may read, with the
	// matching rules of an event type filter; nil means every code, and a
	// non-nil empty slice means none.
	Codes []EventTypeFilter
	Limit int
	After ActionCursor
}

// PlayerActions answers one page of the player-context actions whose
// referenceKey is the identity's id, newest first by (created_at, id).
func (s *Store) PlayerActions(ctx context.Context, query PlayerActionQuery) ([]Action, error) {
	conditions := []string{"context = ?", "reference_key = ?"}
	args := []any{"player", query.PlayerID}

	if query.Servers != nil {
		if len(query.Servers) == 0 {
			return []Action{}, nil
		}
		conditions = append(conditions, "server_id IN ("+inList(len(query.Servers))+")")
		for _, serverID := range query.Servers {
			args = append(args, serverID)
		}
	}
	if query.Codes != nil {
		if len(query.Codes) == 0 {
			return []Action{}, nil
		}
		condition, terms := typeCondition("code", query.Codes)
		conditions = append(conditions, condition)
		args = append(args, terms...)
	}
	if query.After.Set() {
		conditions = append(conditions, "(created_at < ? OR (created_at = ? AND id < ?))")
		after := formatTime(query.After.CreatedAt)
		args = append(args, after, after, query.After.ID)
	}

	args = append(args, query.Limit)
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+actionColumns+` FROM actions
		  WHERE `+strings.Join(conditions, " AND ")+`
		  ORDER BY created_at DESC, id DESC
		  LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("read player actions: %w", err)
	}
	defer rows.Close()

	actions := make([]Action, 0, min(query.Limit, 128))
	for rows.Next() {
		action, err := scanAction(rows)
		if err != nil {
			return nil, fmt.Errorf("scan player action: %w", err)
		}
		actions = append(actions, action)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read player actions: %w", err)
	}
	return actions, nil
}

// PlayerNote is one operator note on an identity (spec section 8.6).
type PlayerNote struct {
	ID        string
	Platform  string
	PlayerID  string
	Text      string
	CreatedAt time.Time
	// TokenID is "" for the bootstrap credential; TokenName is the writer's
	// name as it was when the note was written.
	TokenID   string
	TokenName string
}

// ErrNoteLimit is returned when an identity already carries as many notes as
// the hub allows.
var ErrNoteLimit = errors.New("the identity carries as many notes as the hub allows")

// CreatePlayerNote stores one note, refusing with ErrNoteLimit when the
// identity already carries bound of them. The count and the insert are one
// statement, so the bound holds on SQLite's single connection; on Postgres two
// writers racing the last slot can both land, which overshoots a MAY bound by
// the width of the race and is accepted rather than locked against.
func (s *Store) CreatePlayerNote(ctx context.Context, note PlayerNote, bound int) (PlayerNote, error) {
	now := time.Now().UTC()
	note.CreatedAt = now.Truncate(time.Millisecond)
	result, err := s.db.ExecContext(ctx,
		`INSERT INTO player_notes (id, platform, player_id, text, created_at, token_id, token_name)
		 SELECT ?, ?, ?, ?, ?, ?, ?
		  WHERE (SELECT COUNT(*) FROM player_notes WHERE platform = ? AND player_id = ?) < ?`,
		note.ID, note.Platform, note.PlayerID, note.Text, formatTime(now), note.TokenID, note.TokenName,
		note.Platform, note.PlayerID, bound)
	if err != nil {
		return PlayerNote{}, fmt.Errorf("insert player note: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return PlayerNote{}, fmt.Errorf("insert player note: %w", err)
	}
	if inserted == 0 {
		return PlayerNote{}, ErrNoteLimit
	}
	return note, nil
}

// NoteCursor is a position in an identity's notes, newest first.
type NoteCursor struct {
	CreatedAt time.Time
	ID        string
}

// Set reports whether the cursor names a position at all.
func (c NoteCursor) Set() bool { return c.ID != "" }

// PlayerNotes answers one page of an identity's notes, newest first by
// (created_at, id).
func (s *Store) PlayerNotes(ctx context.Context, platform, playerID string, limit int, after NoteCursor) ([]PlayerNote, error) {
	conditions := []string{"platform = ?", "player_id = ?"}
	args := []any{platform, playerID}
	if after.Set() {
		conditions = append(conditions, "(created_at < ? OR (created_at = ? AND id < ?))")
		at := formatTime(after.CreatedAt)
		args = append(args, at, at, after.ID)
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, platform, player_id, text, created_at, token_id, token_name
		   FROM player_notes
		  WHERE `+strings.Join(conditions, " AND ")+`
		  ORDER BY created_at DESC, id DESC
		  LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("read player notes: %w", err)
	}
	defer rows.Close()

	notes := make([]PlayerNote, 0, min(limit, 64))
	for rows.Next() {
		var (
			note      PlayerNote
			createdAt string
		)
		if err := rows.Scan(&note.ID, &note.Platform, &note.PlayerID, &note.Text,
			&createdAt, &note.TokenID, &note.TokenName); err != nil {
			return nil, fmt.Errorf("scan player note: %w", err)
		}
		if note.CreatedAt, err = parseTime(createdAt); err != nil {
			return nil, err
		}
		notes = append(notes, note)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read player notes: %w", err)
	}
	return notes, nil
}

// DeletePlayerNote removes one note of one identity, or answers ErrNotFound
// when no such note exists on that identity.
func (s *Store) DeletePlayerNote(ctx context.Context, platform, playerID, noteID string) error {
	result, err := s.db.ExecContext(ctx,
		`DELETE FROM player_notes WHERE id = ? AND platform = ? AND player_id = ?`,
		noteID, platform, playerID)
	if err != nil {
		return fmt.Errorf("delete player note: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete player note: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}
