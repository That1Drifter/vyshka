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

	insert, err := tx.PrepareContext(ctx,
		`INSERT INTO state_snapshots (server_id, type, captured_at, received_at, expires_at, body)
		 VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, fmt.Errorf("prepare snapshot insert: %w", err)
	}
	defer insert.Close()

	receivedAt := formatTime(now)
	depths := map[string]int{}
	for _, snapshot := range snapshots {
		capturedAt := now
		if snapshot.CapturedAt != nil {
			capturedAt = *snapshot.CapturedAt
		}
		if _, err := insert.ExecContext(ctx,
			serverID, snapshot.Type, formatTime(capturedAt), receivedAt,
			formatTime(now.Add(snapshot.Retention)), string(snapshot.Body),
		); err != nil {
			return 0, fmt.Errorf("insert snapshot: %w", err)
		}
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
	return len(snapshots), nil
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
// SQLite pool is one connection.
func (s *Store) PruneSnapshots(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = defaultPruneBatch
	}

	result, err := s.db.ExecContext(ctx,
		`DELETE FROM state_snapshots WHERE seq IN (
		    SELECT seq FROM state_snapshots AS stale
		     WHERE stale.expires_at <= ?
		       AND stale.seq < (SELECT MAX(newest.seq) FROM state_snapshots AS newest
		                         WHERE newest.server_id = stale.server_id
		                           AND newest.type = stale.type)
		     LIMIT ?)`,
		formatTime(time.Now().UTC()), limit)
	if err != nil {
		return 0, fmt.Errorf("prune snapshots: %w", err)
	}
	pruned, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune snapshots: %w", err)
	}
	return int(pruned), nil
}
