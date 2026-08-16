package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Manifest is a server's stored manifest: the accepted `manifest.publish` body,
// verbatim, plus the metadata the hub keeps beside it (spec section 6).
type Manifest struct {
	ServerID    string
	Revision    int64
	Body        json.RawMessage
	PublishedAt time.Time
}

// Manifest returns a server's stored manifest, or ErrNotFound when the server
// has never had one accepted.
func (s *Store) Manifest(ctx context.Context, serverID string) (Manifest, error) {
	var (
		manifest    Manifest
		body        string
		publishedAt string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT server_id, revision, body, published_at FROM manifests WHERE server_id = ?`,
		serverID,
	).Scan(&manifest.ServerID, &manifest.Revision, &body, &publishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Manifest{}, ErrNotFound
	}
	if err != nil {
		return Manifest{}, fmt.Errorf("read manifest: %w", err)
	}

	manifest.Body = json.RawMessage(body)
	if manifest.PublishedAt, err = parseTime(publishedAt); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

// applyManifest stores a manifest revision inside an open transaction, gated on
// monotonicity: a revision above the stored one replaces it, anything else is
// ignored (spec section 6.1). The gate lives in the statement itself so that
// two concurrent publishes cannot interleave a read and a write.
func applyManifest(ctx context.Context, tx *sql.Tx, serverID string, revision int64, body []byte, now time.Time) (applied bool, err error) {
	result, err := tx.ExecContext(ctx,
		`INSERT INTO manifests (server_id, revision, body, published_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT (server_id) DO UPDATE
		    SET revision = excluded.revision, body = excluded.body,
		        published_at = excluded.published_at
		  WHERE excluded.revision > manifests.revision`,
		serverID, revision, string(body), formatTime(now),
	)
	if err != nil {
		return false, fmt.Errorf("apply manifest: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("apply manifest: %w", err)
	}
	return affected > 0, nil
}
