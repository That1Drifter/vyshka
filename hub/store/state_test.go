package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/That1Drifter/vyshka/hub/store"
)

// applySnapshots pushes snapshots through ApplyInbound the way a poll does,
// advancing the session's ack by one envelope per call.
func applySnapshots(t *testing.T, st *store.Store, sessionID string, ack int64, snapshots ...store.NewSnapshot) {
	t.Helper()

	applied, err := st.ApplyInbound(context.Background(), sessionID,
		func(int64) store.InboundApplication {
			return store.InboundApplication{Ack: ack, Accepted: 1, Snapshots: snapshots}
		}, 100)
	if err != nil {
		t.Fatalf("apply snapshots: %v", err)
	}
	if applied.SnapshotsStored != len(snapshots) {
		t.Fatalf("stored %d snapshots, want %d", applied.SnapshotsStored, len(snapshots))
	}
}

// snapshotIDs hands out unique envelope ids, because the store deduplicates
// snapshots on (server, envelope id) and a fixture reusing one would silently
// insert nothing.
var snapshotIDs atomic.Int64

func nextSnapshotID() string {
	return fmt.Sprintf("snapshot-envelope-%d", snapshotIDs.Add(1))
}

func playersSnapshot(retention time.Duration, depth int, payload string) store.NewSnapshot {
	return store.NewSnapshot{
		EnvelopeID:   nextSnapshotID(),
		Type:         "players",
		Body:         json.RawMessage(payload),
		Retention:    retention,
		HistoryDepth: depth,
	}
}

func TestSnapshotLatestAndHistoryOrder(t *testing.T) {
	ctx := context.Background()
	st := migrated(t)
	serverID := enrolledServer(t, st, "state order")
	session := startSession(t, st, serverID, "state-order-token")

	// Three snapshots in one application: acceptance order decides latest,
	// even though all three share one received_at millisecond.
	applySnapshots(t, st, session.ID, 1,
		playersSnapshot(time.Hour, 10, `{"players":[{"n":1}]}`),
		playersSnapshot(time.Hour, 10, `{"players":[{"n":2}]}`),
		playersSnapshot(time.Hour, 10, `{"players":[{"n":3}]}`),
	)

	latest, err := st.LatestSnapshot(ctx, serverID, "players")
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if string(latest.Body) != `{"players":[{"n":3}]}` {
		t.Errorf("latest body = %s, want the last accepted", latest.Body)
	}

	history, err := st.SnapshotHistory(ctx, serverID, "players", 10)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(history) != 3 {
		t.Fatalf("history has %d snapshots, want 3", len(history))
	}
	for i, want := range []string{`{"players":[{"n":3}]}`, `{"players":[{"n":2}]}`, `{"players":[{"n":1}]}`} {
		if string(history[i].Body) != want {
			t.Errorf("history[%d] = %s, want %s", i, history[i].Body, want)
		}
	}

	// Types are independent: no vehicles snapshot has been accepted.
	if _, err := st.LatestSnapshot(ctx, serverID, "vehicles"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("latest vehicles = %v, want ErrNotFound", err)
	}
}

func TestSnapshotHistoryDepthTrim(t *testing.T) {
	ctx := context.Background()
	st := migrated(t)
	serverID := enrolledServer(t, st, "state depth")
	session := startSession(t, st, serverID, "state-depth-token")

	for i := 1; i <= 5; i++ {
		applySnapshots(t, st, session.ID, int64(i),
			playersSnapshot(time.Hour, 3, fmt.Sprintf(`{"players":[{"n":%d}]}`, i)))
	}

	history, err := st.SnapshotHistory(ctx, serverID, "players", 10)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(history) != 3 {
		t.Fatalf("history has %d snapshots after depth-3 inserts, want 3", len(history))
	}
	if string(history[0].Body) != `{"players":[{"n":5}]}` {
		t.Errorf("newest = %s, want the fifth snapshot", history[0].Body)
	}
	if string(history[2].Body) != `{"players":[{"n":3}]}` {
		t.Errorf("oldest survivor = %s, want the third snapshot", history[2].Body)
	}
}

func TestSnapshotPruneKeepsLatest(t *testing.T) {
	ctx := context.Background()
	st := migrated(t)
	serverID := enrolledServer(t, st, "state prune")
	session := startSession(t, st, serverID, "state-prune-token")

	// Every row is stamped with an already-tiny retention, so all of them are
	// expired by the time the prune runs; the newest must survive anyway.
	applySnapshots(t, st, session.ID, 1,
		playersSnapshot(time.Millisecond, 0, `{"players":[{"n":1}]}`),
		playersSnapshot(time.Millisecond, 0, `{"players":[{"n":2}]}`),
	)
	applySnapshots(t, st, session.ID, 2,
		store.NewSnapshot{EnvelopeID: nextSnapshotID(), Type: "vehicles", Body: json.RawMessage(`{"vehicles":[]}`),
			Retention: time.Millisecond},
	)
	time.Sleep(30 * time.Millisecond)

	pruned, err := st.PruneSnapshots(ctx, 100)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if pruned != 1 {
		t.Errorf("pruned %d rows, want 1: only the superseded players snapshot is deletable", pruned)
	}

	latest, err := st.LatestSnapshot(ctx, serverID, "players")
	if err != nil {
		t.Fatalf("latest players after prune: %v", err)
	}
	if string(latest.Body) != `{"players":[{"n":2}]}` {
		t.Errorf("latest after prune = %s, want the newest kept standing", latest.Body)
	}
	if _, err := st.LatestSnapshot(ctx, serverID, "vehicles"); err != nil {
		t.Errorf("latest vehicles after prune: %v, want it kept however stale", err)
	}
}

// The cross-session retransmission case of spec section 8.3: a snapshot whose
// envelope id was already stored inserts nothing, however its seq was
// renumbered on the way in.
func TestSnapshotDedupByEnvelopeID(t *testing.T) {
	ctx := context.Background()
	st := migrated(t)
	serverID := enrolledServer(t, st, "state dedup")
	session := startSession(t, st, serverID, "state-dedup-token")

	replayed := playersSnapshot(time.Hour, 10, `{"players":[{"n":1}]}`)
	applySnapshots(t, st, session.ID, 1, replayed)

	// The plugin reconnects; the buffer replay arrives on a new session with
	// a renumbered seq and the same envelope id.
	second := startSession(t, st, serverID, "state-dedup-token-2")
	applied, err := st.ApplyInbound(ctx, second.ID,
		func(int64) store.InboundApplication {
			return store.InboundApplication{Ack: 1, Accepted: 1,
				Snapshots: []store.NewSnapshot{replayed}}
		}, 100)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if applied.SnapshotsStored != 0 {
		t.Errorf("replay stored %d snapshots, want 0", applied.SnapshotsStored)
	}

	history, err := st.SnapshotHistory(ctx, serverID, "players", 10)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(history) != 1 {
		t.Errorf("history has %d snapshots after a replay, want 1", len(history))
	}
}

func TestSnapshotCapturedAtFallsBackToReceipt(t *testing.T) {
	ctx := context.Background()
	st := migrated(t)
	serverID := enrolledServer(t, st, "state captured")
	session := startSession(t, st, serverID, "state-captured-token")

	sampled := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	applySnapshots(t, st, session.ID, 1,
		store.NewSnapshot{EnvelopeID: nextSnapshotID(), Type: "players", CapturedAt: &sampled,
			Body: json.RawMessage(`{"players":[]}`), Retention: time.Hour},
	)
	applySnapshots(t, st, session.ID, 2,
		store.NewSnapshot{EnvelopeID: nextSnapshotID(), Type: "vehicles",
			Body: json.RawMessage(`{"vehicles":[]}`), Retention: time.Hour},
	)

	withClock, err := st.LatestSnapshot(ctx, serverID, "players")
	if err != nil {
		t.Fatalf("latest players: %v", err)
	}
	if !withClock.CapturedAt.Equal(sampled) {
		t.Errorf("capturedAt = %v, want the sampled %v", withClock.CapturedAt, sampled)
	}

	withoutClock, err := st.LatestSnapshot(ctx, serverID, "vehicles")
	if err != nil {
		t.Fatalf("latest vehicles: %v", err)
	}
	if !withoutClock.CapturedAt.Equal(withoutClock.ReceivedAt) {
		t.Errorf("capturedAt = %v with no clock sent, want receipt %v",
			withoutClock.CapturedAt, withoutClock.ReceivedAt)
	}
}
