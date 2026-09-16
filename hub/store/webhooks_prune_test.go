package store

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/That1Drifter/vyshka/hub/internal/dbtest"
)

func plantFinishedDeliveries(t *testing.T, st *Store, webhookID string, count int, finishedAt time.Time) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.CreateWebhook(ctx, Webhook{
		ID: webhookID, URL: "https://example.invalid/hook", Secret: "test-secret", Template: "generic-json",
	}); err != nil {
		t.Fatal(err)
	}
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := 0; i < count; i++ {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO webhook_deliveries
			 (id, webhook_id, type, server_id, body, state, next_attempt_at, created_at, finished_at)
			 VALUES (?, ?, 'core.test', 'test-server', '{}', ?, ?, ?, ?)`,
			fmt.Sprintf("%s-%03d", webhookID, i), webhookID, DeliveryDead,
			formatTime(finishedAt), formatTime(finishedAt), formatTime(finishedAt)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestPruneWebhookDeliveriesBoundsAndEligibility(t *testing.T) {
	st := openMigrated(t)
	ctx := context.Background()
	cutoff := time.Now().UTC()
	plantFinishedDeliveries(t, st, "wh-a", 3, cutoff)
	plantFinishedDeliveries(t, st, "wh-b", 4, cutoff)
	for _, query := range []string{
		`UPDATE webhook_deliveries SET state = 'delivered' WHERE id = 'wh-a-000'`,
		`UPDATE webhook_deliveries SET state = 'pending' WHERE id = 'wh-b-000'`,
		`UPDATE webhook_deliveries SET finished_at = NULL WHERE id = 'wh-b-001'`,
	} {
		if _, err := st.db.ExecContext(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.db.ExecContext(ctx,
		`UPDATE webhook_deliveries SET finished_at = ? WHERE id = 'wh-b-002'`,
		formatTime(cutoff.Add(time.Millisecond))); err != nil {
		t.Fatal(err)
	}
	for i, want := range []int{2, 2, 0} {
		n, err := st.PruneWebhookDeliveries(ctx, cutoff, 2)
		if err != nil || n != want {
			t.Fatalf("pass %d: pruned %d, %v; want %d, nil", i, n, err, want)
		}
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM webhook_deliveries`); n != 3 {
		t.Errorf("survivors = %d, want pending, unfinished, and recent rows", n)
	}
	// An omitted or invalid limit still means one bounded default batch.
	for _, limit := range []int{0, -1} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			st := openMigrated(t)
			plantFinishedDeliveries(t, st, "wh-default", defaultPruneBatch+1, cutoff)
			n, err := st.PruneWebhookDeliveries(ctx, cutoff, limit)
			if err != nil || n != defaultPruneBatch {
				t.Fatalf("pruned %d, %v; want %d, nil", n, err, defaultPruneBatch)
			}
			if n := countRows(t, st, `SELECT COUNT(*) FROM webhook_deliveries`); n != 1 {
				t.Errorf("survivors = %d, want 1", n)
			}
		})
	}
}

func TestPruneWebhookDeliveriesLargeLimit(t *testing.T) {
	st := openMigrated(t)
	ctx := context.Background()
	cutoff := time.Now().UTC()
	plantFinishedDeliveries(t, st, "wh-large", 0, cutoff)
	// One more candidate than the backend could delete using one parameter
	// per id plus the two eligibility parameters. Bulk-seed the rows so this
	// checks the real engine limits without thousands of database round trips.
	count := 32765
	if dbtest.Backend() == "postgres" {
		count = 65534
	}
	if _, err := st.db.ExecContext(ctx,
		`WITH RECURSIVE numbers(n) AS (
		   SELECT 0 UNION ALL SELECT n + 1 FROM numbers WHERE n + 1 < ?
		 )
		 INSERT INTO webhook_deliveries
		   (id, webhook_id, type, server_id, body, state, next_attempt_at, created_at, finished_at)
		 SELECT 'large-' || CAST(n AS TEXT), 'wh-large', 'core.test', 'test-server', '{}', 'dead', ?, ?, ?
		 FROM numbers`, count, formatTime(cutoff), formatTime(cutoff), formatTime(cutoff)); err != nil {
		t.Fatal(err)
	}
	n, err := st.PruneWebhookDeliveries(ctx, cutoff, count)
	if err != nil || n != 32764 {
		t.Fatalf("pruned %d, %v; want 32764, nil", n, err)
	}
	if left := countRows(t, st, `SELECT COUNT(*) FROM webhook_deliveries`); left != count-n {
		t.Errorf("survivors = %d, want %d", left, count-n)
	}
}

// A prune waiting on the first parent must not already own a later parent,
// or fan-out (which takes every parent in this order) could deadlock with it.
// State changed while it waits must also be rechecked before deleting.
func TestPruneWebhookDeliveriesParentOrderAndRecheck(t *testing.T) {
	if dbtest.Backend() != "postgres" {
		t.Skip("requires concurrent Postgres row locks")
	}
	st := openMigrated(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cutoff := time.Now().UTC()
	for _, webhookID := range []string{"wh-a", "wh-b", "wh-c", "wh-unrelated"} {
		plantFinishedDeliveries(t, st, webhookID, 3, cutoff.Add(-time.Hour))
	}
	// Expected order: a first by timestamp, then c before b on an equal
	// timestamp. Candidate delivery ids sort a,b,c, so that is not the order.
	if _, err := st.db.ExecContext(ctx, `UPDATE webhooks SET created_at = ?`, formatTime(cutoff)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE webhooks SET created_at = ? WHERE id = 'wh-a'`,
		formatTime(cutoff.Add(time.Second))); err != nil {
		t.Fatal(err)
	}
	for _, first := range []string{"wh-a", "wh-c"} {
		t.Run(first, func(t *testing.T) {
			blocker, err := st.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback()
			var pid int
			if err := blocker.QueryRowContext(ctx, `SELECT pg_backend_pid() FROM webhooks WHERE id = ? FOR UPDATE`, first).Scan(&pid); err != nil {
				t.Fatal(err)
			}
			type pruneResult struct {
				count int
				err   error
			}
			done := make(chan pruneResult, 1)
			go func() {
				n, err := st.PruneWebhookDeliveries(ctx, cutoff, 9)
				done <- pruneResult{n, err}
			}()
			awaitBlockedBy(t, ctx, st, pid)
			// Every later parent is still available. The unrelated parent is
			// outside the selected batch and must not be locked either.
			later := []string{"wh-c", "wh-b", "wh-unrelated"}
			if first == "wh-c" {
				later = later[1:]
			}
			for _, webhookID := range later {
				var id string
				if err := blocker.QueryRowContext(ctx, `SELECT id FROM webhooks WHERE id = ? FOR UPDATE NOWAIT`, webhookID).Scan(&id); err != nil {
					t.Fatalf("prune locked %s before %s: %v", webhookID, first, err)
				}
			}
			if first == "wh-a" {
				// Keep this pass's candidates after checking timestamp order. The next one
				// verifies the id tie-break and a replay committing during its wait.
				// Move candidates out of retention while holding all their parents.
				if _, err := blocker.ExecContext(ctx, `UPDATE webhook_deliveries SET finished_at = ?`, formatTime(cutoff.Add(time.Hour))); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := blocker.ExecContext(ctx, `UPDATE webhook_deliveries SET state = 'pending', finished_at = NULL, generation = generation + 1 WHERE id = 'wh-b-000'`); err != nil {
					t.Fatal(err)
				}
				if _, err := blocker.ExecContext(ctx, `UPDATE webhook_deliveries SET finished_at = ? WHERE id = 'wh-b-001'`, formatTime(cutoff.Add(time.Hour))); err != nil {
					t.Fatal(err)
				}
			}
			if err := blocker.Commit(); err != nil {
				t.Fatal(err)
			}
			want := 7
			if first == "wh-a" {
				want = 0
			}
			select {
			case result := <-done:
				if result.err != nil || result.count != want {
					t.Fatalf("pruned %d, %v; want %d, nil", result.count, result.err, want)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		})
		if t.Failed() {
			return
		}
		if first == "wh-a" {
			if _, err := st.db.ExecContext(ctx, `UPDATE webhook_deliveries SET finished_at = ?`, formatTime(cutoff.Add(-time.Hour))); err != nil {
				t.Fatal(err)
			}
		}
	}
	rows, err := st.db.QueryContext(ctx, `SELECT id FROM webhook_deliveries ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var survivors []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		survivors = append(survivors, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{"wh-b-000", "wh-b-001", "wh-unrelated-000", "wh-unrelated-001", "wh-unrelated-002"}
	if !slices.Equal(survivors, want) {
		t.Errorf("survivors = %v, want %v", survivors, want)
	}
}

// Park a real DeleteWebhook after it owns the parent and one delivery, then
// start retention. The trigger forces a legal cascade lock order (last row
// first) instead of relying on a particular query plan to reproduce the bug.
// Before the fix retention owns earlier deliveries while waiting for the last
// one; releasing the delete creates a cycle and Postgres aborts one operation.
func TestPruneWebhookDeliveriesSerializesWithDelete(t *testing.T) {
	if dbtest.Backend() != "postgres" {
		t.Skip("requires concurrent Postgres row locks")
	}
	st := openMigrated(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cutoff := time.Now().UTC()
	plantFinishedDeliveries(t, st, "wh-delete", 100, cutoff.Add(-time.Hour))

	for _, query := range []string{
		`CREATE FUNCTION park_webhook_delete() RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN
		   PERFORM id FROM webhook_deliveries WHERE id = 'wh-delete-099' FOR UPDATE;
		   PERFORM pg_advisory_xact_lock(890001);
		   RETURN OLD;
		 END $$`,
		`CREATE TRIGGER park_webhook_delete BEFORE DELETE ON webhooks
		 FOR EACH ROW EXECUTE FUNCTION park_webhook_delete()`,
	} {
		if _, err := st.db.ExecContext(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	barrier, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer barrier.Rollback()
	var barrierPID int
	if err := barrier.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&barrierPID); err != nil {
		t.Fatal(err)
	}
	if _, err := barrier.ExecContext(ctx, `SELECT pg_advisory_xact_lock(890001)`); err != nil {
		t.Fatal(err)
	}
	deleted := make(chan error, 1)
	go func() { deleted <- st.DeleteWebhook(ctx, "wh-delete") }()
	deletePID := awaitBlockedBy(t, ctx, st, barrierPID)

	type pruneResult struct {
		count int
		err   error
	}
	pruned := make(chan pruneResult, 1)
	go func() {
		n, err := st.PruneWebhookDeliveries(ctx, cutoff, 100)
		pruned <- pruneResult{n, err}
	}()
	awaitBlockedBy(t, ctx, st, deletePID)
	if err := barrier.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-deleted:
		if err != nil {
			t.Errorf("delete webhook: %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case result := <-pruned:
		if result.err != nil || result.count != 0 {
			t.Errorf("prune after delete = %d, %v; want 0, nil", result.count, result.err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM webhooks`); n != 0 {
		t.Errorf("webhooks remaining = %d, want 0", n)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM webhook_deliveries`); n != 0 {
		t.Errorf("deliveries remaining = %d, want 0", n)
	}
}

// Observe an actual database wait, so the test cannot pass merely because a
// competing goroutine was not scheduled during a fixed sleep.
func awaitBlockedBy(t *testing.T, ctx context.Context, st *Store, blocker int) int {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var pid int
		if err := st.db.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(pid), 0) FROM pg_stat_activity
			 WHERE datname = current_database() AND wait_event_type = 'Lock'
			   AND ? = ANY(pg_blocking_pids(pid))`, blocker).Scan(&pid); err != nil {
			t.Fatal(err)
		}
		if pid != 0 {
			return pid
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-deadline.C:
			t.Fatalf("no database operation blocked on backend %d", blocker)
		case <-ticker.C:
		}
	}
}
