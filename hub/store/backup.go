package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// ErrSnapshotUnsupported is what Snapshot and Vacuum answer on a backend that
// has no file to copy. A Postgres installation is backed up with the tools
// Postgres ships (pg_dump), which see the database the way the hub's own
// transactions do; the hub has no business reimplementing them.
var ErrSnapshotUnsupported = errors.New("the hub snapshots SQLite only; back up Postgres with pg_dump")

// ErrSnapshotNotAFile is what Snapshot answers for an in-memory SQLite
// database. A second connection to `:memory:` opens a new, empty database, so
// a snapshot through it would be a well-formed copy of nothing.
var ErrSnapshotNotAFile = errors.New("this hub's SQLite database is in memory; there is no file to back up")

// CanSnapshot reports whether Snapshot can work on this store at all:
// ErrSnapshotUnsupported on Postgres, ErrSnapshotNotAFile for an in-memory
// SQLite database, nil otherwise.
func (s *Store) CanSnapshot() error {
	if s.driver != dialectSQLite {
		return ErrSnapshotUnsupported
	}
	if inMemory(s.target) {
		return ErrSnapshotNotAFile
	}
	return nil
}

// inMemory reports whether a SQLite target names an in-memory database rather
// than a file, in any of the spellings SQLite accepts.
func inMemory(target string) bool {
	return target == ":memory:" || strings.HasPrefix(target, "file::memory:") ||
		strings.Contains(target, "mode=memory")
}

// Snapshot writes a consistent copy of a SQLite database to dest, which must
// not exist yet. It runs `VACUUM INTO` on a connection of its own rather than
// on the store's pool: the pool is one connection on SQLite, and that
// connection is the lock every read-then-write in this package relies on, so
// a snapshot taken through it would stall every poll, ingest, and admin call
// for as long as the copy took. The separate connection only reads. In WAL
// mode a reader holds one snapshot of the database for its whole transaction
// and neither waits on the writer nor makes the writer wait, so the copy is
// the database as of one committed instant while the hub keeps serving. The
// one cost is that a WAL checkpoint cannot move past the reader's snapshot
// until the copy finishes; the WAL grows for that long and is folded back at
// the next checkpoint.
//
// One thing in the copy is deliberately not as it was: every session live at
// that instant is closed in it (closeSnapshotSessions says why). The copy is
// also compacted (`VACUUM INTO` rewrites it page by page), and it is fsynced
// before Snapshot returns, so a caller that renames it into place after a nil
// error has a durable file.
func (s *Store) Snapshot(ctx context.Context, dest string) error {
	if err := s.CanSnapshot(); err != nil {
		return err
	}
	if _, err := os.Lstat(dest); err == nil {
		return fmt.Errorf("snapshot: %s already exists", dest)
	}

	reader, err := s.sideConnection(ctx)
	if err != nil {
		return err
	}
	defer reader.Close()

	// The file name is a bound parameter: SQLite takes any expression there,
	// so no quoting of a path is ever needed.
	if _, err := reader.ExecContext(ctx, `VACUUM INTO ?`, dest); err != nil {
		os.Remove(dest)
		return fmt.Errorf("snapshot: %w", err)
	}

	if err := closeSnapshotSessions(ctx, dest, time.Now().UTC()); err != nil {
		os.Remove(dest)
		return err
	}

	file, err := os.OpenFile(dest, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("snapshot: reopen for sync: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("snapshot: sync: %w", err)
	}
	return file.Close()
}

// ErrDatabaseInUse is what Vacuum answers when another connection, a running
// hub's most likely, has the database open.
var ErrDatabaseInUse = errors.New("another process has this database open; stop the hub first")

// Vacuum rebuilds a SQLite database in place, returning the pages the
// retention passes freed to the filesystem. It needs the database to itself:
// measured against a running hub under write load, a plain VACUUM succeeded
// and made one of the hub's writes fail at once with SQLITE_BUSY (a WAL
// writer that loses the race is not retried by the busy timeout). So it first
// takes SQLite's exclusive locking mode and an exclusive transaction, which
// fails while any other connection holds the file open, and answers
// ErrDatabaseInUse instead of touching the database. Once it holds the lock,
// a hub started during the vacuum waits for it or fails to boot, rather than
// failing requests.
func (s *Store) Vacuum(ctx context.Context) error {
	if s.driver != dialectSQLite {
		return ErrSnapshotUnsupported
	}
	conn, err := s.db.raw.Conn(ctx)
	if err != nil {
		return fmt.Errorf("vacuum: %w", err)
	}
	defer conn.Close()
	for _, statement := range []string{
		"PRAGMA busy_timeout = 1000",
		"PRAGMA locking_mode = EXCLUSIVE",
	} {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("vacuum: %s: %w", statement, err)
		}
	}
	// The exclusive locking mode takes its lock at the next write and keeps
	// it until the connection closes; an empty exclusive transaction is that
	// write, so the in-use check happens before the rebuild starts.
	if _, err := conn.ExecContext(ctx, "BEGIN EXCLUSIVE"); err != nil {
		return fmt.Errorf("vacuum: %w (%v)", ErrDatabaseInUse, err)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("vacuum: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `VACUUM`); err != nil {
		return fmt.Errorf("vacuum: %w", err)
	}
	// Fold the WAL back into the main file, so the size the operator sees
	// on disk is the size the vacuum produced.
	if _, err := conn.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("vacuum: checkpoint: %w", err)
	}
	return nil
}

// SessionEndRestored is recorded on every session that was live when a
// snapshot was taken (see closeSnapshotSessions).
const SessionEndRestored = "restored-from-backup"

// closeSnapshotSessions ends, in the copy only, every session that was live
// when it was taken. A session's sequence state (spec section 9.1) is only
// true of the instant of the snapshot, and its plugin carries on past that
// instant against the running hub. Booted on the copy, a hub that honored the
// session would report an ack below the one the plugin already recorded,
// which the plugin ignores, so the plugin's next envelope lands above a gap
// the hub can never close; and the hub would number new outbound envelopes
// from its rolled-back seq, which the plugin discards as duplicates of ones
// it acked. Ended, the session answers session_invalid, the plugin opens a
// new one with its stored secret (section 5.3), and both sides renumber
// what is still unacked into it (section 9.1), the case sequence spaces
// already handle. Closing them costs a plugin one session start, which a
// restored hub costs it anyway.
func closeSnapshotSessions(ctx context.Context, path string, now time.Time) error {
	copied, err := sql.Open("sqlite", path)
	if err != nil {
		return fmt.Errorf("snapshot: open copy: %w", err)
	}
	defer copied.Close()
	copied.SetMaxOpenConns(1)
	var tables int
	if err := copied.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'sessions'`).Scan(&tables); err != nil {
		return fmt.Errorf("snapshot: read copy: %w", err)
	}
	// A hub's database always has a sessions table once it has booted, so a
	// copy without one is not a copy of the hub's database (an empty file the
	// side connection created, a target that changed under the hub) and must
	// not be renamed into place as a backup.
	if tables == 0 {
		return errors.New("snapshot: the copy has no sessions table; it is not a copy of this hub's database")
	}
	if _, err := copied.ExecContext(ctx,
		`UPDATE sessions SET ended_at = ?, end_reason = ? WHERE ended_at IS NULL`,
		formatTime(now), SessionEndRestored); err != nil {
		return fmt.Errorf("snapshot: close sessions in the copy: %w", err)
	}
	// The copy comes out of VACUUM INTO in rollback-journal mode (header
	// bytes 18-19 read 1 1 on a snapshot of a WAL database, measured on the
	// pinned driver), so the update is in the main file once it commits and
	// no journal is left beside it for the rename to strand.
	return nil
}

// SQLitePath reports the file a DSN names when it names a SQLite database,
// resolved as Open resolves it.
func SQLitePath(dsn string) (string, bool) {
	driver, target, err := resolveDSN(dsn)
	if err != nil || driver != dialectSQLite {
		return "", false
	}
	return target, true
}

// sideConnection opens a second handle on the store's SQLite file, used for
// nothing but one `VACUUM INTO`, which only reads the source. It cannot be
// put in query_only mode: SQLite refuses `VACUUM INTO` there ("attempt to
// write a readonly database", measured on the pinned driver). It is closed by
// the caller and never shared with the pool.
func (s *Store) sideConnection(ctx context.Context) (*sql.DB, error) {
	raw, err := sql.Open("sqlite", s.target)
	if err != nil {
		return nil, fmt.Errorf("snapshot: open: %w", err)
	}
	raw.SetMaxOpenConns(1)
	if _, err := raw.ExecContext(ctx, "PRAGMA busy_timeout = 5000"); err != nil {
		raw.Close()
		return nil, fmt.Errorf("snapshot: busy timeout: %w", err)
	}
	return raw, nil
}
