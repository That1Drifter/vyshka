package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
)

// dialect names the SQL flavor a store speaks. The two differ in three places
// this package has to care about: placeholder syntax (`?` against `$1`), row
// locking (`SELECT ... FOR UPDATE`, which SQLite does not parse), and a
// handful of DDL spellings that live in per-dialect migration files.
type dialect string

const (
	dialectSQLite   dialect = "sqlite"
	dialectPostgres dialect = "postgres"
)

// rebind rewrites `?` placeholders into the dialect's own. Every query in this
// package is written with `?`, and every statement goes through a DB or Tx
// handle that calls this, so no query has to know which engine it runs on.
// Question marks inside single-quoted literals, `--` line comments, and
// `/* */` block comments (nested, as Postgres nests them) are left alone,
// and a quote inside a comment does not open a literal. Dollar-quoted
// strings are not recognised: nothing in this package writes one, and a
// query that did would have its `$` sequences misread, so do not.
func (d dialect) rebind(query string) string {
	if d != dialectPostgres || !strings.Contains(query, "?") {
		return query
	}
	var out strings.Builder
	out.Grow(len(query) + 16)
	n := 0
	for i := 0; i < len(query); i++ {
		c := query[i]
		switch {
		case c == '\'':
			// Copy the literal through its closing quote. A doubled quote
			// inside it closes and reopens, which lands on the same byte.
			end := strings.IndexByte(query[i+1:], '\'')
			if end < 0 {
				out.WriteString(query[i:])
				return out.String()
			}
			out.WriteString(query[i : i+end+2])
			i += end + 1
		case c == '-' && i+1 < len(query) && query[i+1] == '-':
			end := strings.IndexByte(query[i:], '\n')
			if end < 0 {
				out.WriteString(query[i:])
				return out.String()
			}
			out.WriteString(query[i : i+end])
			i += end - 1
		case c == '/' && i+1 < len(query) && query[i+1] == '*':
			end := blockCommentEnd(query, i)
			if end < 0 {
				out.WriteString(query[i:])
				return out.String()
			}
			out.WriteString(query[i:end])
			i = end - 1
		case c == '?':
			n++
			out.WriteByte('$')
			out.WriteString(strconv.Itoa(n))
		default:
			out.WriteByte(c)
		}
	}
	return out.String()
}

// blockCommentEnd returns the index just past the block comment opening at
// start, counting nested openings as Postgres does, or -1 when it never
// closes.
func blockCommentEnd(query string, start int) int {
	depth := 0
	for i := start; i+1 < len(query); i++ {
		switch {
		case query[i] == '/' && query[i+1] == '*':
			depth++
			i++
		case query[i] == '*' && query[i+1] == '/':
			depth--
			i++
			if depth == 0 {
				return i + 1
			}
		}
	}
	return -1
}

// forUpdate is the row-lock suffix for a SELECT that a transaction will later
// write against. Postgres needs it because a connection pool lets two
// transactions interleave between a read and the write that depends on it.
// SQLite does not parse it and does not need it: its pool holds one
// connection (see Open), so a transaction there is the whole database's
// critical section. Callers append it to a plain-row SELECT; it is not valid
// after an aggregate.
func (d dialect) forUpdate() string {
	if d == dialectPostgres {
		return " FOR UPDATE"
	}
	return ""
}

// DB is the store's dialect-aware database handle. It exposes the subset of
// *sql.DB this package and its tests use, rebinding placeholders on the way
// through, so a query written once with `?` runs on either engine.
type DB struct {
	raw *sql.DB
	d   dialect
}

// ExecContext runs a statement that returns no rows.
func (h *DB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return h.raw.ExecContext(ctx, h.d.rebind(query), args...)
}

// Exec is ExecContext with a background context.
func (h *DB) Exec(query string, args ...any) (sql.Result, error) {
	return h.ExecContext(context.Background(), query, args...)
}

// QueryContext runs a query that returns rows.
func (h *DB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return h.raw.QueryContext(ctx, h.d.rebind(query), args...)
}

// QueryRowContext runs a query expected to return at most one row.
func (h *DB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return h.raw.QueryRowContext(ctx, h.d.rebind(query), args...)
}

// QueryRow is QueryRowContext with a background context.
func (h *DB) QueryRow(query string, args ...any) *sql.Row {
	return h.QueryRowContext(context.Background(), query, args...)
}

// BeginTx opens a transaction at the engine's default isolation level.
func (h *DB) BeginTx(ctx context.Context, opts *sql.TxOptions) (*Tx, error) {
	tx, err := h.raw.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &Tx{tx: tx, d: h.d}, nil
}

// Tx is one open transaction, rebinding placeholders like DB does.
type Tx struct {
	tx *sql.Tx
	d  dialect
}

// ExecContext runs a statement that returns no rows.
func (t *Tx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return t.tx.ExecContext(ctx, t.d.rebind(query), args...)
}

// QueryContext runs a query that returns rows.
func (t *Tx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return t.tx.QueryContext(ctx, t.d.rebind(query), args...)
}

// QueryRowContext runs a query expected to return at most one row.
func (t *Tx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return t.tx.QueryRowContext(ctx, t.d.rebind(query), args...)
}

// PrepareContext prepares a statement for repeated execution in this
// transaction.
func (t *Tx) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	return t.tx.PrepareContext(ctx, t.d.rebind(query))
}

// Commit commits the transaction.
func (t *Tx) Commit() error { return t.tx.Commit() }

// Rollback aborts the transaction. After Commit it returns sql.ErrTxDone,
// which deferred callers ignore.
func (t *Tx) Rollback() error { return t.tx.Rollback() }

// forUpdate is the transaction's row-lock suffix; see dialect.forUpdate.
func (t *Tx) forUpdate() string { return t.d.forUpdate() }

// lockServer takes the per-server aggregate lock: the first step of the lock
// order every multi-statement write in this package follows.
//
//  1. the server row (this call)
//  2. the server's live session row, when the operation is session-scoped
//  3. manifests, envelopes, actions, events, and enrollment-token rows
//
// Every operation that creates a session, issues an enrollment token, applies
// a manifest, dispatches an action, or inserts an outbound envelope starts
// here, so the count-then-insert queue bound, the end-then-insert session
// rule, the delete-then-insert token rule, and the manifest-revision recheck
// before a dispatch each see committed state and commit before the next one
// reads. The order matters as much as the locks: a session-scoped operation
// that may later queue a notice (ApplyInbound) takes the server first and the
// session second, the same way StartSession does, so the two can never wait
// on each other. On SQLite the lock suffix is empty and this is a plain
// existence check; the single connection is the lock.
//
// The order governs the server and session rows. Child rows are locked in
// whatever order a statement visits them, and the maintenance passes
// (ExpireActions, PruneSnapshots) run without a server lock because they
// span every server. Two transactions can therefore still meet over child
// rows in opposite orders: a poll applying action results while the expiry
// sweep flips the same actions, or a snapshot trim against the snapshot
// prune. Postgres detects such a cycle and aborts one side with a deadlock
// error; nothing is written by the aborted side (an aborted ApplyInbound
// rolls back its ack, counts, snapshots, and notices together), the poll
// answers an error its plugin retries, and the sweep runs again on its next
// tick. That is the accepted resolution. The cycles are real and need no
// coincidence of deadlines (a snapshot trim and the snapshot prune can meet
// over two expired history rows whenever both run), but they cost latency
// and a retried poll, never a wrong committed state, and ordering every
// child-row lock across the sweeps would cost each pass a locking pre-read
// on every run to avoid that.
//
// It returns ErrNotFound when the server does not exist.
func lockServer(ctx context.Context, tx *Tx, serverID string) error {
	var exists string
	err := tx.QueryRowContext(ctx,
		`SELECT id FROM servers WHERE id = ?`+tx.forUpdate(), serverID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
