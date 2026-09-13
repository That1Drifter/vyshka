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
// Question marks inside single-quoted literals are left alone.
func (d dialect) rebind(query string) string {
	if d != dialectPostgres || !strings.Contains(query, "?") {
		return query
	}
	var out strings.Builder
	out.Grow(len(query) + 16)
	n := 0
	quoted := false
	for i := 0; i < len(query); i++ {
		c := query[i]
		switch {
		case c == '\'':
			quoted = !quoted
			out.WriteByte(c)
		case c == '?' && !quoted:
			n++
			out.WriteByte('$')
			out.WriteString(strconv.Itoa(n))
		default:
			out.WriteByte(c)
		}
	}
	return out.String()
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
