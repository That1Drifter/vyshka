package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// State snapshots (spec section 8.3): the latest accepted snapshot per
// (server, type), with a bounded history behind it.

// Snapshot is one stored state snapshot.
type Snapshot struct {
	ServerID   string
	Type       string
	CapturedAt time.Time
	ReceivedAt time.Time
	Body       json.RawMessage
}

// NewSnapshot is one snapshot to store, already validated by the caller.
type NewSnapshot struct {
	// EnvelopeID is the id of the state.* envelope that carried this
	// snapshot. It is what deduplicates a retransmission across a session
	// change, where seq is renumbered and only the id survives (spec section
	// 9.1); a snapshot whose id was already stored inserts nothing. The id is
	// remembered in state_snapshot_dedup for the history window, outliving
	// the history row itself, so a replay cannot regress "latest" after its
	// row was pruned.
	EnvelopeID string
	// Type is the list kind: "players", "vehicles", or "entities".
	Type string
	// CapturedAt is when the game says it sampled the state. Nil means no
	// usable timestamp reached the hub and receipt time stands in.
	CapturedAt *time.Time
	Body       json.RawMessage
	// Retention is how long this row is kept for history, counted from
	// receipt. The latest row per (server, type) outlives it: see PruneSnapshots.
	Retention time.Duration
	// HistoryDepth bounds how many rows of this (server, type) survive the
	// insert, newest first. Zero or negative means no depth bound.
	HistoryDepth int
}

// insertSnapshots appends a batch inside an open transaction, so that the ack
// covering the envelopes and the state they carried commit together (spec
// section 9.3). Rows are inserted in slice order, which the AUTOINCREMENT seq
// turns into the acceptance order "latest" is defined by; ULIDs would not do,
// because everything in one poll shares a millisecond.
func insertSnapshots(ctx context.Context, tx *sql.Tx, serverID string, snapshots []NewSnapshot, now time.Time) (int, error) {
	if len(snapshots) == 0 {
		return 0, nil
	}

	// Each envelope id is claimed in the dedup table first, ON CONFLICT DO
	// NOTHING rather than failing: a retransmission renumbered into a new
	// session is accepted by the sequence layer (its seq is fresh) and only
	// the envelope id reveals it was already stored. The claim cannot live on
	// the history row alone, because history is bounded by depth as well as
	// time: a superseded row can be trimmed minutes after it landed while the
	// latest of its type survives every pass, and a replay of the trimmed
	// snapshot would then insert with a fresh acceptance seq and regress
	// "latest" to a state that was already superseded. The marker holds the
	// id for the history window, which is as far as section 8.3 promises
	// deduplication reaches.
	claim, err := tx.PrepareContext(ctx,
		`INSERT INTO state_snapshot_dedup (server_id, envelope_id, expires_at)
		 VALUES (?, ?, ?)
		 ON CONFLICT (server_id, envelope_id) DO NOTHING`)
	if err != nil {
		return 0, fmt.Errorf("prepare snapshot claim: %w", err)
	}
	defer claim.Close()

	// The history insert keeps its own ON CONFLICT DO NOTHING as a backstop
	// for the one row that outlives every marker: the latest snapshot per
	// (server, type) survives retention indefinitely, so a replay of it can
	// arrive after its marker expired, win a fresh claim, and must still not
	// land in history twice.
	insert, err := tx.PrepareContext(ctx,
		`INSERT INTO state_snapshots (server_id, envelope_id, type, captured_at, received_at, expires_at, body)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (server_id, envelope_id) DO NOTHING`)
	if err != nil {
		return 0, fmt.Errorf("prepare snapshot insert: %w", err)
	}
	defer insert.Close()

	receivedAt := formatTime(now)
	stored := 0
	depths := map[string]int{}
	for _, snapshot := range snapshots {
		expiresAt := formatTime(now.Add(snapshot.Retention))
		claimed, err := claim.ExecContext(ctx, serverID, snapshot.EnvelopeID, expiresAt)
		if err != nil {
			return 0, fmt.Errorf("claim snapshot: %w", err)
		}
		fresh, err := claimed.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("claim snapshot: %w", err)
		}
		if fresh == 0 {
			// Already stored once; a replay applies no further.
			continue
		}

		capturedAt := now
		if snapshot.CapturedAt != nil {
			capturedAt = *snapshot.CapturedAt
		}
		result, err := insert.ExecContext(ctx,
			serverID, snapshot.EnvelopeID, snapshot.Type, formatTime(capturedAt), receivedAt,
			expiresAt, string(snapshot.Body),
		)
		if err != nil {
			return 0, fmt.Errorf("insert snapshot: %w", err)
		}
		inserted, err := result.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("insert snapshot: %w", err)
		}
		stored += int(inserted)
		if snapshot.HistoryDepth > depths[snapshot.Type] {
			depths[snapshot.Type] = snapshot.HistoryDepth
		}
	}

	// The depth bound is enforced where rows appear, so a plugin pushing
	// faster than the retention pass runs cannot grow the table between
	// passes. Once per type after the batch, not per row: trimming after each
	// row of the same type would re-run the same delete.
	for stateType, depth := range depths {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM state_snapshots
			  WHERE server_id = ? AND type = ?
			    AND seq NOT IN (SELECT seq FROM state_snapshots
			                     WHERE server_id = ? AND type = ?
			                     ORDER BY seq DESC LIMIT ?)`,
			serverID, stateType, serverID, stateType, depth,
		); err != nil {
			return 0, fmt.Errorf("trim snapshot history: %w", err)
		}
	}
	return stored, nil
}

// LatestSnapshot returns the most recently accepted snapshot of one type, or
// ErrNotFound when the server has never had one accepted.
func (s *Store) LatestSnapshot(ctx context.Context, serverID, stateType string) (Snapshot, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT server_id, type, captured_at, received_at, body
		   FROM state_snapshots
		  WHERE server_id = ? AND type = ?
		  ORDER BY seq DESC LIMIT 1`, serverID, stateType)
	snapshot, err := scanSnapshot(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Snapshot{}, ErrNotFound
	}
	return snapshot, err
}

// SnapshotHistory returns up to limit snapshots of one type, newest first in
// acceptance order. An empty answer for a server that exists is a valid
// answer: no snapshot of this type has been accepted, or history was trimmed.
func (s *Store) SnapshotHistory(ctx context.Context, serverID, stateType string, limit int) ([]Snapshot, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT server_id, type, captured_at, received_at, body
		   FROM state_snapshots
		  WHERE server_id = ? AND type = ?
		  ORDER BY seq DESC LIMIT ?`, serverID, stateType, limit)
	if err != nil {
		return nil, fmt.Errorf("read snapshot history: %w", err)
	}
	defer rows.Close()

	snapshots := make([]Snapshot, 0, min(limit, 32))
	for rows.Next() {
		snapshot, err := scanSnapshot(rows.Scan)
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, snapshot)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read snapshot history: %w", err)
	}
	return snapshots, nil
}

func scanSnapshot(scan func(...any) error) (Snapshot, error) {
	var (
		snapshot               Snapshot
		capturedAt, receivedAt string
		body                   string
	)
	if err := scan(&snapshot.ServerID, &snapshot.Type, &capturedAt, &receivedAt, &body); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Snapshot{}, err
		}
		return Snapshot{}, fmt.Errorf("scan snapshot: %w", err)
	}
	var err error
	if snapshot.CapturedAt, err = parseTime(capturedAt); err != nil {
		return Snapshot{}, err
	}
	if snapshot.ReceivedAt, err = parseTime(receivedAt); err != nil {
		return Snapshot{}, err
	}
	snapshot.Body = json.RawMessage(body)
	return snapshot, nil
}

// PruneSnapshots deletes up to limit snapshots past their retention, always
// leaving the newest row per (server, type) standing whatever its age: a
// server's last known state stays readable, and its capturedAt says how stale
// it is (spec section 8.3). Bounded like every retention pass, because the
// SQLite pool is one connection. The returned count covers history rows and
// expired dedup markers together, so the caller's pacing loop drains both.
func (s *Store) PruneSnapshots(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = defaultPruneBatch
	}

	now := formatTime(time.Now().UTC())
	result, err := s.db.ExecContext(ctx,
		`DELETE FROM state_snapshots WHERE seq IN (
		    SELECT seq FROM state_snapshots AS stale
		     WHERE stale.expires_at <= ?
		       AND stale.seq < (SELECT MAX(newest.seq) FROM state_snapshots AS newest
		                         WHERE newest.server_id = stale.server_id
		                           AND newest.type = stale.type)
		     LIMIT ?)`,
		now, limit)
	if err != nil {
		return 0, fmt.Errorf("prune snapshots: %w", err)
	}
	pruned, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune snapshots: %w", err)
	}

	// The dedup markers expire on their own column and ride the same pass,
	// within the pass's remaining budget. Sweeping a marker early is safe,
	// unlike the event-batch sweep: any history row it still guards carries
	// its own unique (server_id, envelope_id) index and blocks the replay
	// itself. The markers ARE counted in the return, also unlike the
	// event-batch sweep (at most one marker per batch of up to 200 events):
	// the depth trim deletes rows long before their markers expire, so
	// markers can outnumber expired rows without bound, and a return that
	// ignored them would let the caller's loop stop while an expired-marker
	// backlog kept growing.
	total := int(pruned)
	if total < limit {
		swept, err := s.db.ExecContext(ctx,
			`DELETE FROM state_snapshot_dedup
			  WHERE (server_id, envelope_id) IN
			        (SELECT server_id, envelope_id FROM state_snapshot_dedup
			          WHERE expires_at <= ? LIMIT ?)`,
			now, limit-total)
		if err != nil {
			return 0, fmt.Errorf("prune snapshot dedup markers: %w", err)
		}
		markers, err := swept.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("prune snapshot dedup markers: %w", err)
		}
		total += int(markers)
	}
	return total, nil
}
