package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/That1Drifter/vyshka/hub/internal/id"
)

// ErrTokenRevoked separates a credential this hub has retired from one it has
// never heard of. Both are refused, but only the first is worth telling the
// operator apart in a log line, and neither is ever distinguished on the wire.
var ErrTokenRevoked = errors.New("admin token has been revoked or has expired")

// AdminToken is one scoped Admin API credential (spec section 10). The secret
// is not a field: it exists only in the response that mints it.
type AdminToken struct {
	ID        string
	Name      string
	Scopes    []string
	CreatedAt time.Time
	// CreatedBy is the id of the token that minted this one, or "" when the
	// bootstrap credential did. It makes a chain of delegation readable after
	// the fact without a second query per row.
	CreatedBy string
	ExpiresAt *time.Time
	RevokedAt *time.Time
}

// Live reports whether the token may authenticate a request at now.
func (t AdminToken) Live(now time.Time) bool {
	return t.RevokedAt == nil && (t.ExpiresAt == nil || t.ExpiresAt.After(now))
}

// NewAdminToken is the request side of CreateAdminToken.
type NewAdminToken struct {
	Name      string
	TokenHash string
	Scopes    []string
	CreatedBy string
	// TTL is how long the token lives. Zero means it does not expire on its own.
	TTL time.Duration
}

const adminTokenColumns = `id, name, scopes, created_at, created_by, expires_at, revoked_at`

// CreateAdminToken records a minted token and returns the stored row.
func (s *Store) CreateAdminToken(ctx context.Context, request NewAdminToken) (AdminToken, error) {
	now := time.Now().UTC()
	scopes, err := json.Marshal(request.Scopes)
	if err != nil {
		return AdminToken{}, fmt.Errorf("encode scopes: %w", err)
	}

	stored := AdminToken{
		ID:        id.NewAt(now),
		Name:      request.Name,
		Scopes:    request.Scopes,
		CreatedAt: now.Truncate(time.Millisecond),
		CreatedBy: request.CreatedBy,
	}
	var expiresAt any
	if request.TTL > 0 {
		expiry := now.Add(request.TTL).Truncate(time.Millisecond)
		stored.ExpiresAt = &expiry
		expiresAt = formatTime(expiry)
	}

	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO admin_tokens (id, token_hash, name, scopes, created_at, created_by, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		stored.ID, request.TokenHash, stored.Name, string(scopes),
		formatTime(now), stored.CreatedBy, expiresAt,
	); err != nil {
		return AdminToken{}, fmt.Errorf("insert admin token: %w", err)
	}
	return stored, nil
}

// AdminTokenByHash resolves a presented credential's digest to its token.
//
// Lookup is by digest rather than by a scan and compare, so there is nothing to
// compare in constant time: the index either finds the row or it does not, and
// a caller cannot probe a 130-bit secret one digest at a time.
//
// A revoked or expired token answers ErrTokenRevoked rather than ErrNotFound so
// the hub can log which of the two happened; the handler reports the same 401
// either way.
func (s *Store) AdminTokenByHash(ctx context.Context, tokenHash string) (AdminToken, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+adminTokenColumns+` FROM admin_tokens WHERE token_hash = ?`, tokenHash)

	stored, err := scanAdminToken(row)
	if errors.Is(err, sql.ErrNoRows) {
		return AdminToken{}, ErrNotFound
	}
	if err != nil {
		return AdminToken{}, fmt.Errorf("read admin token: %w", err)
	}
	if !stored.Live(time.Now().UTC()) {
		return AdminToken{}, ErrTokenRevoked
	}
	return stored, nil
}

// ListAdminTokens returns every token record, newest first, including revoked
// and expired ones: an operator auditing access needs to see what used to
// exist, not only what still works.
func (s *Store) ListAdminTokens(ctx context.Context) ([]AdminToken, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+adminTokenColumns+` FROM admin_tokens ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("list admin tokens: %w", err)
	}
	defer rows.Close()

	tokens := []AdminToken{}
	for rows.Next() {
		stored, err := scanAdminToken(rows)
		if err != nil {
			return nil, fmt.Errorf("scan admin token: %w", err)
		}
		tokens = append(tokens, stored)
	}
	return tokens, rows.Err()
}

// AdminToken returns one token record by id, revoked or not.
func (s *Store) AdminToken(ctx context.Context, tokenID string) (AdminToken, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+adminTokenColumns+` FROM admin_tokens WHERE id = ?`, tokenID)

	stored, err := scanAdminToken(row)
	if errors.Is(err, sql.ErrNoRows) {
		return AdminToken{}, ErrNotFound
	}
	if err != nil {
		return AdminToken{}, fmt.Errorf("read admin token: %w", err)
	}
	return stored, nil
}

// RevokeAdminToken retires a token. It is idempotent: revoking an already
// revoked token leaves the first revocation's timestamp in place, because that
// is the moment the operator will be looking for later.
func (s *Store) RevokeAdminToken(ctx context.Context, tokenID string) error {
	result, err := s.db.ExecContext(ctx,
		`UPDATE admin_tokens SET revoked_at = COALESCE(revoked_at, ?) WHERE id = ?`,
		formatTime(time.Now().UTC()), tokenID)
	if err != nil {
		return fmt.Errorf("revoke admin token: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("revoke admin token: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// LiveAdminTokens returns the tokens that could authenticate right now.
//
// Boot uses it to decide whether minting an ephemeral bootstrap credential is
// still first-run behavior (spec section 10.6). It deliberately returns the
// records rather than a count: what boot needs to know is whether any of them
// can still *manage tokens*, and that means reading their scopes, which is the
// hub package's grammar to interpret rather than this one's.
func (s *Store) LiveAdminTokens(ctx context.Context) ([]AdminToken, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+adminTokenColumns+` FROM admin_tokens
		  WHERE revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?)
		  ORDER BY created_at DESC, id DESC`,
		formatTime(time.Now().UTC()))
	if err != nil {
		return nil, fmt.Errorf("list live admin tokens: %w", err)
	}
	defer rows.Close()

	tokens := []AdminToken{}
	for rows.Next() {
		stored, err := scanAdminToken(rows)
		if err != nil {
			return nil, fmt.Errorf("scan admin token: %w", err)
		}
		tokens = append(tokens, stored)
	}
	return tokens, rows.Err()
}

func scanAdminToken(row rowScanner) (AdminToken, error) {
	var (
		stored               AdminToken
		scopes, createdAt    string
		expiresAt, revokedAt sql.NullString
	)
	if err := row.Scan(&stored.ID, &stored.Name, &scopes, &createdAt,
		&stored.CreatedBy, &expiresAt, &revokedAt); err != nil {
		return AdminToken{}, err
	}

	var err error
	if stored.CreatedAt, err = parseTime(createdAt); err != nil {
		return AdminToken{}, err
	}
	if stored.ExpiresAt, err = scanTime(expiresAt); err != nil {
		return AdminToken{}, err
	}
	if stored.RevokedAt, err = scanTime(revokedAt); err != nil {
		return AdminToken{}, err
	}
	if err := json.Unmarshal([]byte(scopes), &stored.Scopes); err != nil {
		return AdminToken{}, fmt.Errorf("decode scopes of token %s: %w", stored.ID, err)
	}
	return stored, nil
}

// AuditRecord is one entry of the audit log (spec section 10).
type AuditRecord struct {
	ID   string
	At   time.Time
	Name string
	// TokenID is "" for the bootstrap credential, which has no token row.
	TokenID       string
	Method        string
	Path          string
	Status        int
	SourceIP      string
	PayloadDigest string
	ServerID      string
	Detail        json.RawMessage
}

// NewAuditRecord is the request side of RecordAudit.
type NewAuditRecord struct {
	TokenID       string
	TokenName     string
	Method        string
	Path          string
	Status        int
	SourceIP      string
	PayloadDigest string
	ServerID      string
	Detail        json.RawMessage
	Retention     time.Duration
}

// RecordAudit appends one entry.
//
// The write follows the mutation's own commit rather than sharing its
// transaction, which is an honest limitation: a crash in that window loses the
// record of a change that did happen. Making the two atomic means threading a
// transaction through every Admin API handler, and the failure it would close
// is narrower than the one it would open, because a handler that had to hold a
// transaction across its whole body would serialize the Admin API on SQLite's
// single connection.
func (s *Store) RecordAudit(ctx context.Context, request NewAuditRecord) (AuditRecord, error) {
	now := time.Now().UTC()
	detail := request.Detail
	if len(detail) == 0 {
		detail = json.RawMessage(`{}`)
	}
	retention := request.Retention
	if retention <= 0 {
		retention = 365 * 24 * time.Hour
	}

	record := AuditRecord{
		ID:            id.NewAt(now),
		At:            now.Truncate(time.Millisecond),
		TokenID:       request.TokenID,
		Name:          request.TokenName,
		Method:        request.Method,
		Path:          request.Path,
		Status:        request.Status,
		SourceIP:      request.SourceIP,
		PayloadDigest: request.PayloadDigest,
		ServerID:      request.ServerID,
		Detail:        detail,
	}

	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_records (id, at, token_id, token_name, method, path, status,
		                            source_ip, payload_digest, server_id, detail, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.ID, formatTime(now), record.TokenID, record.Name, record.Method,
		record.Path, record.Status, record.SourceIP, record.PayloadDigest,
		record.ServerID, string(detail), formatTime(now.Add(retention)),
	); err != nil {
		return AuditRecord{}, fmt.Errorf("insert audit record: %w", err)
	}
	return record, nil
}

// AuditQuery is one page of the audit feed, newest first.
type AuditQuery struct {
	// TokenID and ServerID narrow the feed; empty means no narrowing.
	TokenID  string
	ServerID string
	// Since is inclusive and Until exclusive, both on At.
	Since time.Time
	Until time.Time
	Limit int
	After AuditCursor
}

// AuditCursor is a position in the feed, the same coordinate-rather-than-row
// shape the event feed uses so that a cursor stays valid across a prune.
type AuditCursor struct {
	At time.Time
	ID string
}

// Set reports whether the cursor names a position at all.
func (c AuditCursor) Set() bool { return c.ID != "" }

// AuditRecords answers one page of the audit log, newest first, ordered by
// (at, id) descending so that pagination can neither skip nor repeat rows that
// share a millisecond.
func (s *Store) AuditRecords(ctx context.Context, query AuditQuery) ([]AuditRecord, error) {
	conditions := []string{"1 = 1"}
	args := []any{}

	if query.TokenID != "" {
		conditions = append(conditions, "token_id = ?")
		args = append(args, query.TokenID)
	}
	if query.ServerID != "" {
		conditions = append(conditions, "server_id = ?")
		args = append(args, query.ServerID)
	}
	// Rounded up to the stored resolution for the same reason the event query
	// rounds: truncating a bound below a millisecond would widen an inclusive
	// `since` and narrow an exclusive `until`.
	if !query.Since.IsZero() {
		conditions = append(conditions, "at >= ?")
		args = append(args, formatTime(ceilMillisecond(query.Since)))
	}
	if !query.Until.IsZero() {
		conditions = append(conditions, "at < ?")
		args = append(args, formatTime(ceilMillisecond(query.Until)))
	}
	if query.After.Set() {
		conditions = append(conditions, "(at < ? OR (at = ? AND id < ?))")
		after := formatTime(query.After.At)
		args = append(args, after, after, query.After.ID)
	}

	args = append(args, query.Limit)
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, at, token_id, token_name, method, path, status, source_ip,
		        payload_digest, server_id, detail
		   FROM audit_records
		  WHERE `+strings.Join(conditions, " AND ")+`
		  ORDER BY at DESC, id DESC
		  LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("read audit records: %w", err)
	}
	defer rows.Close()

	records := make([]AuditRecord, 0, min(query.Limit, 128))
	for rows.Next() {
		var (
			record     AuditRecord
			at, detail string
		)
		if err := rows.Scan(&record.ID, &at, &record.TokenID, &record.Name,
			&record.Method, &record.Path, &record.Status, &record.SourceIP,
			&record.PayloadDigest, &record.ServerID, &detail); err != nil {
			return nil, fmt.Errorf("scan audit record: %w", err)
		}
		if record.At, err = parseTime(at); err != nil {
			return nil, err
		}
		record.Detail = json.RawMessage(detail)
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read audit records: %w", err)
	}
	return records, nil
}

// PruneAudit deletes up to limit entries past their retention and reports how
// many went, bounded for the same reason the event prune is: on SQLite the one
// connection it holds is the whole hub's critical section.
func (s *Store) PruneAudit(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = defaultPruneBatch
	}

	result, err := s.db.ExecContext(ctx,
		`DELETE FROM audit_records
		  WHERE id IN (SELECT id FROM audit_records WHERE expires_at <= ? LIMIT ?)`,
		formatTime(time.Now().UTC()), limit)
	if err != nil {
		return 0, fmt.Errorf("prune audit records: %w", err)
	}
	pruned, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune audit records: %w", err)
	}
	return int(pruned), nil
}
