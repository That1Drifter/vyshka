package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The key/value store of spec section 12. Every operation here is atomic on
// its own: get and delete are single statements, and set and incr run their
// read and write inside one transaction, which today the single SQLite
// connection serializes. On a real connection pool (issue #20) the reads in
// kvSetTx and KVIncr MUST take a row lock (SELECT ... FOR UPDATE), or two
// concurrent incrs can read the same value and one of the deltas vanishes,
// which is exactly what section 12.2 forbids.

// maxKVExact is 2^53 - 1, the exactness bound of spec section 12: revisions
// and incr arithmetic must survive a float64 round-trip, so anything beyond it
// is refused rather than computed inexactly.
const maxKVExact = int64(1)<<53 - 1

// KVEntry is one key of the store. Value is nil on results that do not carry
// the value back (a set). ExpiresAt is nil when the key never expires.
type KVEntry struct {
	Namespace string
	Key       string
	Value     json.RawMessage
	Revision  int64
	ExpiresAt *time.Time
}

// KVRevisionMismatchError reports a compare-and-swap that lost. Current is the
// revision the key holds now, 0 when the key does not exist, which is what the
// loser needs to retry without a second round-trip.
type KVRevisionMismatchError struct {
	Current int64
}

func (e *KVRevisionMismatchError) Error() string {
	if e.Current == 0 {
		return "ifRevision does not match: the key does not exist"
	}
	return fmt.Sprintf("ifRevision does not match: the key is at revision %d", e.Current)
}

var (
	// ErrKVNotInteger refuses an incr whose stored value is not a JSON
	// integer inside the exactness bound.
	ErrKVNotInteger = errors.New("the stored value is not an integer within the exactness bound")
	// ErrKVRangeExceeded refuses an incr whose sum would leave the bound.
	ErrKVRangeExceeded = errors.New("the sum would leave the exactness bound")
	// ErrKVRevisionExhausted refuses a write on a key whose revision is at the
	// bound. Unreachable in practice; refused rather than wrapped around,
	// because a wrapped revision would let a stale compare-and-swap win.
	ErrKVRevisionExhausted = errors.New("the key's revision is at the exactness bound")
)

// kvLive reports whether a scanned row is live at now. expiresAt is the raw
// stored column, empty when NULL.
func kvLive(expiresAt string, now time.Time) bool {
	return expiresAt == "" || expiresAt > formatTime(now)
}

// KVGet returns one live key, or ErrNotFound when it is absent or expired. An
// expired row is left for the retention pass rather than deleted here: a read
// that writes would turn the store's cheapest operation into a lock.
func (s *Store) KVGet(ctx context.Context, namespace, key string) (KVEntry, error) {
	var (
		value     string
		revision  int64
		expiresAt sql.NullString
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT value, revision, expires_at FROM kv WHERE namespace = ? AND key = ?`,
		namespace, key,
	).Scan(&value, &revision, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return KVEntry{}, ErrNotFound
	}
	if err != nil {
		return KVEntry{}, fmt.Errorf("read kv key: %w", err)
	}
	if !kvLive(expiresAt.String, time.Now().UTC()) {
		return KVEntry{}, ErrNotFound
	}

	expiry, err := scanTime(expiresAt)
	if err != nil {
		return KVEntry{}, err
	}
	return KVEntry{
		Namespace: namespace,
		Key:       key,
		Value:     json.RawMessage(value),
		Revision:  revision,
		ExpiresAt: expiry,
	}, nil
}

// KVSet writes one key, optionally as a compare-and-swap. ifRevision nil means
// unconditional; 0 means "only if the key does not exist"; n >= 1 means "only
// if the current revision is exactly n". ttl nil means the key does not
// expire, whatever TTL it carried before: a set defines the key entirely
// (spec section 12.2). The returned entry carries the new revision and expiry
// and no value.
func (s *Store) KVSet(ctx context.Context, namespace, key string, value []byte, ifRevision *int64, ttl *time.Duration) (KVEntry, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return KVEntry{}, fmt.Errorf("begin kv set: %w", err)
	}
	defer tx.Rollback()

	// The clock is read after the wait for the connection, not before: a TTL
	// measured from a timestamp that aged in the queue would sell the caller
	// an expiry already partly (or wholly) spent, and liveness decisions made
	// from it would misread a key that expired while this write waited.
	now := time.Now().UTC()

	current, err := kvCurrentRevision(ctx, tx, namespace, key, now)
	if err != nil {
		return KVEntry{}, err
	}
	if ifRevision != nil && *ifRevision != current {
		return KVEntry{}, &KVRevisionMismatchError{Current: current}
	}
	if current >= maxKVExact {
		return KVEntry{}, ErrKVRevisionExhausted
	}

	var expiry *time.Time
	if ttl != nil {
		at := now.Add(*ttl).Truncate(time.Millisecond)
		expiry = &at
	}
	revision := current + 1
	if err := kvUpsert(ctx, tx, namespace, key, string(value), revision, expiry, now); err != nil {
		return KVEntry{}, err
	}

	if err := tx.Commit(); err != nil {
		return KVEntry{}, fmt.Errorf("commit kv set: %w", err)
	}
	return KVEntry{Namespace: namespace, Key: key, Revision: revision, ExpiresAt: expiry}, nil
}

// KVDelete removes one live key, or answers ErrNotFound when it is absent or
// expired. The statement itself refuses to touch an expired row, so a delete
// racing an expiry cannot report a success for a key that already read as
// gone.
func (s *Store) KVDelete(ctx context.Context, namespace, key string) error {
	result, err := s.db.ExecContext(ctx,
		`DELETE FROM kv
		  WHERE namespace = ? AND key = ?
		    AND (expires_at IS NULL OR expires_at > ?)`,
		namespace, key, formatTime(time.Now().UTC()))
	if err != nil {
		return fmt.Errorf("delete kv key: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete kv key: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// KVIncr atomically adds delta to one key, creating it at delta (revision 1,
// no TTL) when it is absent or expired. An existing value must be a JSON
// integer inside the exactness bound, and the sum must stay inside it;
// otherwise the matching error is returned and nothing changes. A successful
// incr preserves the key's TTL (spec section 12.2).
func (s *Store) KVIncr(ctx context.Context, namespace, key string, delta int64) (KVEntry, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return KVEntry{}, fmt.Errorf("begin kv incr: %w", err)
	}
	defer tx.Rollback()

	// Read after the wait for the connection, as in KVSet: liveness must be
	// judged at the moment this transaction runs, not at the moment it queued.
	now := time.Now().UTC()

	var (
		value     string
		revision  int64
		expiresAt sql.NullString
	)
	scanErr := tx.QueryRowContext(ctx,
		`SELECT value, revision, expires_at FROM kv WHERE namespace = ? AND key = ?`,
		namespace, key,
	).Scan(&value, &revision, &expiresAt)

	entry := KVEntry{Namespace: namespace, Key: key}
	switch {
	case errors.Is(scanErr, sql.ErrNoRows) || (scanErr == nil && !kvLive(expiresAt.String, now)):
		// Absent and expired alike: created fresh, with no TTL.
		entry.Revision = 1
		entry.Value = json.RawMessage(strconv.FormatInt(delta, 10))
		if err := kvUpsert(ctx, tx, namespace, key, string(entry.Value), 1, nil, now); err != nil {
			return KVEntry{}, err
		}
	case scanErr != nil:
		return KVEntry{}, fmt.Errorf("read kv key: %w", scanErr)
	default:
		current, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil || current > maxKVExact || current < -maxKVExact {
			return KVEntry{}, ErrKVNotInteger
		}
		// Both operands are inside ±(2^53 - 1), so the sum cannot overflow
		// int64; only the exactness bound can be left.
		sum := current + delta
		if sum > maxKVExact || sum < -maxKVExact {
			return KVEntry{}, ErrKVRangeExceeded
		}
		if revision >= maxKVExact {
			return KVEntry{}, ErrKVRevisionExhausted
		}

		entry.Revision = revision + 1
		entry.Value = json.RawMessage(strconv.FormatInt(sum, 10))
		if entry.ExpiresAt, err = scanTime(expiresAt); err != nil {
			return KVEntry{}, err
		}
		if err := kvUpsert(ctx, tx, namespace, key, string(entry.Value), entry.Revision, entry.ExpiresAt, now); err != nil {
			return KVEntry{}, err
		}
	}

	if err := tx.Commit(); err != nil {
		return KVEntry{}, fmt.Errorf("commit kv incr: %w", err)
	}
	return entry, nil
}

// kvCurrentRevision reads a key's revision inside an open transaction: 0 when
// the row is absent or expired, which is the same "does not exist" a
// compare-and-swap must see in both cases.
func kvCurrentRevision(ctx context.Context, tx *sql.Tx, namespace, key string, now time.Time) (int64, error) {
	var (
		revision  int64
		expiresAt sql.NullString
	)
	err := tx.QueryRowContext(ctx,
		`SELECT revision, expires_at FROM kv WHERE namespace = ? AND key = ?`,
		namespace, key,
	).Scan(&revision, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read kv key: %w", err)
	}
	if !kvLive(expiresAt.String, now) {
		return 0, nil
	}
	return revision, nil
}

// kvUpsert writes one row inside an open transaction, replacing whatever the
// primary key held, including an expired row being recreated.
func kvUpsert(ctx context.Context, tx *sql.Tx, namespace, key, value string, revision int64, expiry *time.Time, now time.Time) error {
	var expiresAt any
	if expiry != nil {
		expiresAt = formatTime(*expiry)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO kv (namespace, key, value, revision, created_at, updated_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (namespace, key) DO UPDATE
		    SET value = excluded.value, revision = excluded.revision,
		        updated_at = excluded.updated_at, expires_at = excluded.expires_at`,
		namespace, key, value, revision, formatTime(now), formatTime(now), expiresAt,
	); err != nil {
		return fmt.Errorf("write kv key: %w", err)
	}
	return nil
}

// PruneKV deletes up to limit keys past their expiry and reports how many
// went. Bounded like every other retention pass: the SQLite pool is one
// connection, and an unbounded delete would hold it, and therefore the whole
// hub, for as long as it took.
func (s *Store) PruneKV(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = defaultPruneBatch
	}

	result, err := s.db.ExecContext(ctx,
		`DELETE FROM kv
		  WHERE (namespace, key) IN (
		        SELECT namespace, key FROM kv
		         WHERE expires_at IS NOT NULL AND expires_at <= ?
		         LIMIT ?)`,
		formatTime(time.Now().UTC()), limit)
	if err != nil {
		return 0, fmt.Errorf("prune kv: %w", err)
	}
	pruned, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune kv: %w", err)
	}
	return int(pruned), nil
}
