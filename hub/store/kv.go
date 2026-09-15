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
// read and write inside one transaction. On SQLite the single connection
// serializes those; on Postgres each write transaction first takes a
// transaction-scoped advisory lock on the key (lockKey), so two concurrent
// incrs read and write one after the other and neither delta vanishes, which
// is what section 12.2 requires. An advisory lock rather than a row lock
// because the row may not exist yet: two writers creating the same key with
// ifRevision 0 have no row to lock, and both would otherwise succeed.

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

// lockKey serializes the writers of one key for the rest of the transaction.
// On Postgres it is a transaction-scoped advisory lock in a keyspace of its
// own (the two-argument form, class 1), released at commit or rollback; a
// hash collision between two keys costs one of them a wait, never a wrong
// answer. On SQLite it is nothing: the single connection is the lock.
func lockKey(ctx context.Context, tx *Tx, namespace, key string) error {
	if tx.d != dialectPostgres {
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(1, hashtext(?))`, namespace+"/"+key); err != nil {
		return fmt.Errorf("lock kv key: %w", err)
	}
	return nil
}

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
	if err := lockKey(ctx, tx, namespace, key); err != nil {
		return KVEntry{}, err
	}
	now := time.Now().UTC()

	current, err := kvCurrentRevision(ctx, tx, namespace, key, now)
	if err != nil {
		return KVEntry{}, err
	}
	runHook(testHooks.afterKVRead)
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
	runHook(testHooks.afterKVWrite)

	if err := tx.Commit(); err != nil {
		return KVEntry{}, fmt.Errorf("commit kv set: %w", err)
	}
	return KVEntry{Namespace: namespace, Key: key, Revision: revision, ExpiresAt: expiry}, nil
}

// KVDelete removes one live key, or answers ErrNotFound when it is absent or
// expired. The statement itself refuses to touch an expired row, so a delete
// racing an expiry cannot report a success for a key that already read as
// gone.
//
// It takes the key lock like the writers do, although it is one statement:
// a delete that slipped between a compare-and-swap's read and its write would
// let the swap succeed against a revision that no longer existed, and an incr
// would resurrect the deleted value plus its delta instead of starting fresh.
// Under the lock the delete lands before the read or after the write, and
// either order is one the caller can reason about.
func (s *Store) KVDelete(ctx context.Context, namespace, key string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin kv delete: %w", err)
	}
	defer tx.Rollback()

	if err := lockKey(ctx, tx, namespace, key); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx,
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
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit kv delete: %w", err)
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

	// Read after the wait for the connection and the lock, as in KVSet:
	// liveness must be judged at the moment this transaction runs, not at the
	// moment it queued.
	if err := lockKey(ctx, tx, namespace, key); err != nil {
		return KVEntry{}, err
	}
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
func kvCurrentRevision(ctx context.Context, tx *Tx, namespace, key string, now time.Time) (int64, error) {
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
func kvUpsert(ctx context.Context, tx *Tx, namespace, key, value string, revision int64, expiry *time.Time, now time.Time) error {
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

// KVKey is one row of a key listing (spec section 12.2). The value is
// deliberately absent: a page of keys is a browse, and a page of 500 values at
// 16 KiB each is not one. ExpiresAt is nil when the key never expires.
type KVKey struct {
	Key       string
	Revision  int64
	ExpiresAt *time.Time
}

// KVListQuery is one page of a namespace's live keys, key ascending in byte
// order.
type KVListQuery struct {
	Namespace string
	// Prefix narrows the page to keys starting with it, as a byte prefix and
	// not a pattern. Empty means every key in the namespace.
	Prefix string
	Limit  int
	// After is the last key of the previous page, exclusive. Empty starts at
	// the beginning. Exclusive is the whole contract: an inclusive comparison
	// would repeat the boundary key on every page.
	After string
}

// KVList answers one page of a namespace's live keys, ordered by key ascending
// in byte order. Expired keys are skipped whether or not the retention pass has
// reached them yet, which is what makes an expiry observable at the moment it
// passes (spec section 12.1).
//
// The ordering is the same byte order every other range in this package
// assumes, so a cursor walk neither repeats nor skips a key: keys are unique
// inside a namespace, so the key alone is a total order and needs no tiebreak.
func (s *Store) KVList(ctx context.Context, query KVListQuery) ([]KVKey, error) {
	if query.Limit <= 0 {
		query.Limit = 100
	}

	conditions := []string{"namespace = ?", "(expires_at IS NULL OR expires_at > ?)"}
	args := []any{query.Namespace, formatTime(time.Now().UTC())}
	if query.After != "" {
		conditions = append(conditions, "key > ?")
		args = append(args, query.After)
	}
	// A prefix is a half-open byte range rather than a LIKE: LIKE would need
	// its own escaping for the wildcards, and its collation is the database's,
	// while a range comparison runs on the primary key's index under the "C"
	// collation migration 0017 pins. A prefix with any byte outside the key
	// alphabet can match no key at all, and is answered here without a query:
	// Postgres refuses a text parameter that is not valid UTF-8, and a key
	// alphabet of ASCII letters, digits, "_", "-", and "." also keeps the
	// successor inside ASCII, so neither bound can ever be a byte sequence
	// the database will not take.
	if query.Prefix != "" {
		if !keyAlphabet(query.Prefix) {
			return []KVKey{}, nil
		}
		conditions = append(conditions, "key >= ?", "key < ?")
		args = append(args, query.Prefix, prefixSuccessor(query.Prefix))
	}
	args = append(args, query.Limit)

	rows, err := s.db.QueryContext(ctx,
		`SELECT key, revision, expires_at
		   FROM kv
		  WHERE `+strings.Join(conditions, " AND ")+`
		  ORDER BY key ASC
		  LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("list kv keys: %w", err)
	}
	defer rows.Close()

	keys := make([]KVKey, 0, min(query.Limit, 128))
	for rows.Next() {
		var (
			entry     KVKey
			expiresAt sql.NullString
		)
		if err := rows.Scan(&entry.Key, &entry.Revision, &expiresAt); err != nil {
			return nil, fmt.Errorf("scan kv key: %w", err)
		}
		if entry.ExpiresAt, err = scanTime(expiresAt); err != nil {
			return nil, err
		}
		keys = append(keys, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list kv keys: %w", err)
	}
	return keys, nil
}

// keyAlphabet reports whether every byte of value is one a key may contain
// (the section 12.1 alphabet: ASCII letters, digits, "_", "-", and the "."
// separator). It checks bytes and not grammar: a prefix may end mid-segment
// or in a dot, and still name a range a real key can fall in.
func keyAlphabet(value string) bool {
	for i := range len(value) {
		c := value[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '_', c == '-', c == '.':
		default:
			return false
		}
	}
	return true
}

// prefixSuccessor returns the smallest string greater than every string
// starting with prefix: the prefix with its last byte incremented. It is the
// exclusive upper bound of the prefix's byte range. The caller has checked
// the prefix against the key alphabet, whose highest byte is "z", so the
// increment never carries and the result stays printable ASCII.
func prefixSuccessor(prefix string) string {
	raw := []byte(prefix)
	raw[len(raw)-1]++
	return string(raw)
}

// KVNamespaceCount is one namespace of the store and how many live keys it
// holds.
type KVNamespaceCount struct {
	Namespace string
	Keys      int64
}

// KVNamespaces lists every namespace holding at least one live key, name
// ascending in byte order, with that count. A namespace whose every key has
// expired does not appear, which is the same "reads as absent from the moment
// its expiry passes" rule the per-key operations follow.
//
// The answer is not paged: a namespace is a mod, an installation has as many as
// it has mods, and a count query over the primary key's index is cheap enough
// that a cursor would buy nothing but a contract to keep.
func (s *Store) KVNamespaces(ctx context.Context) ([]KVNamespaceCount, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT namespace, COUNT(*)
		   FROM kv
		  WHERE expires_at IS NULL OR expires_at > ?
		  GROUP BY namespace
		  ORDER BY namespace ASC`, formatTime(time.Now().UTC()))
	if err != nil {
		return nil, fmt.Errorf("list kv namespaces: %w", err)
	}
	defer rows.Close()

	counts := []KVNamespaceCount{}
	for rows.Next() {
		var one KVNamespaceCount
		if err := rows.Scan(&one.Namespace, &one.Keys); err != nil {
			return nil, fmt.Errorf("scan kv namespace: %w", err)
		}
		counts = append(counts, one)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list kv namespaces: %w", err)
	}
	return counts, nil
}

// PruneKV deletes up to limit keys past their expiry and reports how many
// went. Bounded like every other retention pass: on SQLite the one connection
// it holds is the whole hub's critical section, and on Postgres an unbounded
// delete would hold its row locks against every write for as long as it took.
func (s *Store) PruneKV(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = defaultPruneBatch
	}

	// The expiry test is repeated on the delete target, not only in the
	// subquery that picks the batch. On Postgres a delete that waited on a
	// row lock re-evaluates its own predicate against the committed row, but
	// the subquery keeps the snapshot it was planned with: a key selected
	// as expired, then refreshed by a writer that held the row (a set with
	// ifRevision 0 recreating it without a TTL), would still be a member of
	// the picked set and would be deleted after the writer's commit. The
	// outer test sees the refreshed expires_at and skips it.
	now := formatTime(time.Now().UTC())
	result, err := s.db.ExecContext(ctx,
		`DELETE FROM kv
		  WHERE expires_at IS NOT NULL AND expires_at <= ?
		    AND (namespace, key) IN (
		        SELECT namespace, key FROM kv
		         WHERE expires_at IS NOT NULL AND expires_at <= ?
		         LIMIT ?)`,
		now, now, limit)
	if err != nil {
		return 0, fmt.Errorf("prune kv: %w", err)
	}
	pruned, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune kv: %w", err)
	}
	return int(pruned), nil
}
