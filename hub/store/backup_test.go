package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/That1Drifter/vyshka/hub/internal/dbtest"
	"github.com/That1Drifter/vyshka/hub/store"
)

// A snapshot taken while a writer commits in a tight loop is one committed
// instant of the database: every pair the writer commits together is in it
// together or not at all, everything committed before the snapshot began is
// in it (including what is still only in the WAL), and the copy passes
// SQLite's own integrity check. This is the property the backup endpoint
// sells, and an end-to-end run cannot reach it: the window between two
// statements of one transaction is too narrow to hit from outside.
func TestSnapshotIsOneCommittedInstantUnderConcurrentWrites(t *testing.T) {
	if dbtest.Backend() != "sqlite" {
		t.Skip("snapshots are SQLite only")
	}
	ctx := context.Background()
	st := openTemp(t)
	db := st.DB()
	for _, ddl := range []string{
		`CREATE TABLE pair_a (n INTEGER PRIMARY KEY, pad TEXT)`,
		`CREATE TABLE pair_b (n INTEGER PRIMARY KEY, pad TEXT)`,
	} {
		if _, err := db.ExecContext(ctx, ddl); err != nil {
			t.Fatal(err)
		}
	}
	// Enough rows that the copy takes measurable time, so the writer gets to
	// commit while it runs.
	pad := strings.Repeat("x", 2000)
	writePair := func(n int64) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, `INSERT INTO pair_a (n, pad) VALUES (?, ?)`, n, pad); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO pair_b (n, pad) VALUES (?, ?)`, n, pad); err != nil {
			return err
		}
		return tx.Commit()
	}
	var committed atomic.Int64
	for n := int64(1); n <= 3000; n++ {
		if err := writePair(n); err != nil {
			t.Fatal(err)
		}
		committed.Store(n)
	}

	stop := make(chan struct{})
	writerDone := make(chan error, 1)
	duringSnapshot := atomic.Int64{}
	snapshotting := atomic.Bool{}
	go func() {
		for n := committed.Load() + 1; ; n++ {
			select {
			case <-stop:
				writerDone <- nil
				return
			default:
			}
			began := snapshotting.Load()
			if err := writePair(n); err != nil {
				writerDone <- err
				return
			}
			committed.Store(n)
			if began && snapshotting.Load() {
				duringSnapshot.Add(1)
			}
		}
	}()

	time.Sleep(50 * time.Millisecond)
	before := committed.Load()
	dest := filepath.Join(t.TempDir(), "snapshot.db")
	snapshotting.Store(true)
	err := st.Snapshot(ctx, dest)
	snapshotting.Store(false)
	close(stop)
	if werr := <-writerDone; werr != nil {
		t.Fatalf("writer: %v", werr)
	}
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	// The hub's own connection kept committing while the copy ran: writes
	// that began after the snapshot did and committed before it ended. Taken
	// through the pool's one connection, the snapshot would hold every such
	// write until it finished, and at most the one write already in flight
	// could land in the window. Locally about a hundred land.
	if got := duringSnapshot.Load(); got < 10 {
		t.Errorf("%d writes began and committed while the snapshot ran; the snapshot blocked the store's connection", got)
	}

	copied, err := store.Open(ctx, dest)
	if err != nil {
		t.Fatal(err)
	}
	defer copied.Close()
	var check string
	if err := copied.DB().QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&check); err != nil || check != "ok" {
		t.Fatalf("integrity_check = %q, %v", check, err)
	}
	var a, b, maxA int64
	if err := copied.DB().QueryRowContext(ctx,
		`SELECT (SELECT COUNT(*) FROM pair_a), (SELECT COUNT(*) FROM pair_b), (SELECT COALESCE(MAX(n), 0) FROM pair_a)`).
		Scan(&a, &b, &maxA); err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("snapshot holds %d rows of pair_a and %d of pair_b; a transaction was split", a, b)
	}
	if a != maxA {
		t.Errorf("snapshot holds %d rows but its highest is %d; it is not a prefix of the commits", a, maxA)
	}
	if a < before {
		t.Errorf("snapshot holds %d pairs, but %d were committed before it began", a, before)
	}
	t.Logf("pairs committed before the snapshot %d, in it %d, committed during it %d", before, a, duringSnapshot.Load())
}

func TestSnapshotRefusesAnExistingFile(t *testing.T) {
	if dbtest.Backend() != "sqlite" {
		st := openTemp(t)
		if err := st.Snapshot(context.Background(), filepath.Join(t.TempDir(), "x.db")); !errors.Is(err, store.ErrSnapshotUnsupported) {
			t.Fatalf("snapshot on %s = %v, want ErrSnapshotUnsupported", dbtest.Backend(), err)
		}
		return
	}
	st := openTemp(t)
	dest := filepath.Join(t.TempDir(), "taken.db")
	if err := st.Snapshot(context.Background(), dest); err != nil {
		t.Fatal(err)
	}
	if err := st.Snapshot(context.Background(), dest); err == nil {
		t.Fatal("a second snapshot onto the same file succeeded")
	}
}

// Vacuum refuses a database another connection has open, which is what keeps
// `vyshka-hub vacuum` from failing a running hub's writes (measured before
// the guard existed: one PUT answered 500 with SQLITE_BUSY mid-vacuum), and
// vacuums it once that connection is gone.
func TestVacuumRefusesADatabaseInUse(t *testing.T) {
	if dbtest.Backend() != "sqlite" {
		t.Skip("vacuum is SQLite only")
	}
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "in-use.db")
	hubSide, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hubSide.Close() })
	if _, err := hubSide.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	maintenance, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer maintenance.Close()

	if err := maintenance.Vacuum(ctx); !errors.Is(err, store.ErrDatabaseInUse) {
		t.Fatalf("vacuum with another connection open = %v, want ErrDatabaseInUse", err)
	}
	if err := hubSide.Ping(ctx); err != nil {
		t.Fatalf("the other connection after the refused vacuum: %v", err)
	}
	hubSide.Close()
	if err := maintenance.Vacuum(ctx); err != nil {
		t.Fatalf("vacuum once the other connection closed: %v", err)
	}
}
