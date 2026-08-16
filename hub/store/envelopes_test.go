package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/That1Drifter/vyshka/hub/store"
)

// migrated opens a throwaway store with the schema applied.
func migrated(t *testing.T) *store.Store {
	t.Helper()

	st := openTemp(t)
	if _, err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

// enrolledServer creates a server, enrolls it, and returns its id.
func enrolledServer(t *testing.T, st *store.Store, name string) string {
	t.Helper()
	ctx := context.Background()

	server, err := st.CreateServer(ctx, name, "test-game")
	if err != nil {
		t.Fatalf("create server: %v", err)
	}
	if _, err := st.IssueEnrollmentToken(ctx, server.ID, "token-hash-"+server.ID, time.Hour); err != nil {
		t.Fatalf("issue enrollment token: %v", err)
	}
	if _, err := st.Enroll(ctx, "token-hash-"+server.ID, "test-game",
		"secret-hash-"+server.ID, store.PluginInfo{}); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	return server.ID
}

func startSession(t *testing.T, st *store.Store, serverID, tokenHash string) store.Session {
	t.Helper()

	session, err := st.StartSession(context.Background(), store.NewSession{
		ServerID:           serverID,
		TokenHash:          tokenHash,
		TTL:                time.Hour,
		ProtocolVersion:    1,
		PollTimeoutSeconds: 25,
	})
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	return session
}

// A poll authenticates, and only then reaches the store. In between, its
// session can be superseded by a restarting game server. These cases pin the
// window shut: without them a stale poll renumbers envelopes into a dead
// session, stealing them from the live one and voiding an ack it already sent.
// The HTTP layer cannot reach this window from outside, so it is graded here.
func TestEnvelopeOperationsRefuseASupersededSession(t *testing.T) {
	ctx := context.Background()
	st := migrated(t)
	serverID := enrolledServer(t, st, "superseded")

	stale := startSession(t, st, serverID, "stale-token-hash")
	if _, err := st.QueueEnvelope(ctx, serverID, "test.thing", []byte(`{}`), 100); err != nil {
		t.Fatalf("queue envelope: %v", err)
	}

	// The stale session numbers the envelope, then a restart supersedes it.
	if _, _, _, err := st.NextOutbound(ctx, stale.ID, serverID, 10); err != nil {
		t.Fatalf("next outbound on a live session: %v", err)
	}
	fresh := startSession(t, st, serverID, "fresh-token-hash")

	t.Run("NextOutbound", func(t *testing.T) {
		if _, _, _, err := st.NextOutbound(ctx, stale.ID, serverID, 10); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})
	t.Run("AckOutbound", func(t *testing.T) {
		if err := st.AckOutbound(ctx, stale.ID, 1); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})
	t.Run("ApplyInbound", func(t *testing.T) {
		_, err := st.ApplyInbound(ctx, stale.ID, func(ack int64) store.InboundApplication {
			return store.InboundApplication{Ack: ack + 1, Accepted: 1}
		}, 100)
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})

	// The live session must still hold the envelope, renumbered from 1, and its
	// ack must retire it for good.
	envelopes, _, _, err := st.NextOutbound(ctx, fresh.ID, serverID, 10)
	if err != nil {
		t.Fatalf("next outbound on the live session: %v", err)
	}
	if len(envelopes) != 1 || envelopes[0].Seq != 1 {
		t.Fatalf("live session got %+v, want one envelope at seq 1", envelopes)
	}
	if err := st.AckOutbound(ctx, fresh.ID, 1); err != nil {
		t.Fatalf("ack outbound: %v", err)
	}
	pending, err := st.PendingEnvelopeCount(ctx, serverID)
	if err != nil {
		t.Fatalf("pending count: %v", err)
	}
	if pending != 0 {
		t.Errorf("pendingEnvelopeCount = %d after the live session acked, want 0", pending)
	}
}

// The classifier must see the ack as committed, not as it stood when the caller
// last looked, or two overlapping polls report acks that contradict each other.
func TestApplyInboundClassifiesAgainstCommittedState(t *testing.T) {
	ctx := context.Background()
	st := migrated(t)
	serverID := enrolledServer(t, st, "committed state")
	session := startSession(t, st, serverID, "token-hash")

	applied, err := st.ApplyInbound(ctx, session.ID, func(ack int64) store.InboundApplication {
		if ack != 0 {
			t.Errorf("first classify saw ack %d, want 0", ack)
		}
		return store.InboundApplication{Ack: 2, Accepted: 2}
	}, 100)
	if err != nil || applied.Ack != 2 {
		t.Fatalf("first apply: ack = %d, err = %v, want 2", applied.Ack, err)
	}

	// The second caller must be handed 2, whatever it believed beforehand.
	applied, err = st.ApplyInbound(ctx, session.ID, func(ack int64) store.InboundApplication {
		if ack != 2 {
			t.Errorf("second classify saw ack %d, want the committed 2", ack)
		}
		return store.InboundApplication{Ack: ack}
	}, 100)
	if err != nil || applied.Ack != 2 {
		t.Fatalf("second apply: ack = %d, err = %v, want 2", applied.Ack, err)
	}

	// A classifier that tries to go backwards is refused rather than obeyed,
	// and nothing else it derived is trusted either.
	applied, err = st.ApplyInbound(ctx, session.ID, func(int64) store.InboundApplication {
		return store.InboundApplication{Ack: 1, Accepted: 1,
			Manifests: []store.ManifestPublish{{Revision: 1, Body: []byte(`{}`)}}}
	}, 100)
	if err != nil {
		t.Fatalf("third apply: %v", err)
	}
	if applied.Ack != 2 {
		t.Errorf("ack = %d after a classifier returned 1, want it held at 2", applied.Ack)
	}
	if applied.ManifestsApplied != 0 {
		t.Errorf("a distrusted classification still applied %d manifests", applied.ManifestsApplied)
	}
	if _, err := st.Manifest(ctx, serverID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("manifest read err = %v, want ErrNotFound", err)
	}
}

// The manifest revision gate and the notice queue, both of which ride the same
// transaction as the ack (spec sections 6.1 and 9.3).
func TestApplyInboundManifestsAndNotices(t *testing.T) {
	ctx := context.Background()
	st := migrated(t)
	serverID := enrolledServer(t, st, "manifests")
	session := startSession(t, st, serverID, "token-hash")

	publish := func(revision int64, body string, notices ...store.Notice) store.InboundApplied {
		t.Helper()
		applied, err := st.ApplyInbound(ctx, session.ID, func(ack int64) store.InboundApplication {
			return store.InboundApplication{Ack: ack + 1, Accepted: 1,
				Manifests: []store.ManifestPublish{{Revision: revision, Body: []byte(body)}},
				Notices:   notices}
		}, 2)
		if err != nil {
			t.Fatalf("apply inbound: %v", err)
		}
		return applied
	}

	if applied := publish(3, `{"manifestRevision": 3}`); applied.ManifestsApplied != 1 {
		t.Fatalf("first publish applied %d manifests, want 1", applied.ManifestsApplied)
	}

	// A higher revision replaces; an equal or lower one is ignored.
	if applied := publish(2, `{"manifestRevision": 2}`); applied.ManifestsApplied != 0 {
		t.Error("a lower revision replaced the stored manifest")
	}
	if applied := publish(3, `{"manifestRevision": 3, "changed": true}`); applied.ManifestsApplied != 0 {
		t.Error("an equal revision replaced the stored manifest")
	}
	if applied := publish(4, `{"manifestRevision": 4}`); applied.ManifestsApplied != 1 {
		t.Error("a higher revision did not replace the stored manifest")
	}

	manifest, err := st.Manifest(ctx, serverID)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if manifest.Revision != 4 || string(manifest.Body) != `{"manifestRevision": 4}` {
		t.Errorf("stored manifest = revision %d body %s, want revision 4", manifest.Revision, manifest.Body)
	}

	// Notices are queued against the same bound as everything else, and a full
	// queue drops the notice rather than failing the poll.
	notice := store.Notice{Type: "manifest.reject", Body: []byte(`{}`)}
	if applied := publish(5, `{"manifestRevision": 5}`, notice, notice); applied.NoticesQueued != 2 {
		t.Fatalf("queued %d notices below the bound, want 2", applied.NoticesQueued)
	}
	applied := publish(6, `{"manifestRevision": 6}`, notice)
	if applied.NoticesQueued != 0 {
		t.Errorf("queued %d notices at the bound of 2, want the notice dropped", applied.NoticesQueued)
	}
	if applied.ManifestsApplied != 1 {
		t.Errorf("a dropped notice cost the manifest: applied %d, want 1", applied.ManifestsApplied)
	}
}

// Section 9.2: at the bound a hub refuses new work rather than discarding
// envelopes it has already accepted. Graded here with a limit of two, because
// grading it through the Admin API would mean queueing the reference default of
// 5 000 to prove one error code.
func TestQueueEnvelopeRefusesNewWorkAtTheBound(t *testing.T) {
	ctx := context.Background()
	st := migrated(t)
	serverID := enrolledServer(t, st, "queue bound")

	for i := range 2 {
		if _, err := st.QueueEnvelope(ctx, serverID, "test.thing", []byte(`{}`), 2); err != nil {
			t.Fatalf("queue envelope %d below the bound: %v", i, err)
		}
	}

	if _, err := st.QueueEnvelope(ctx, serverID, "test.thing", []byte(`{}`), 2); !errors.Is(err, store.ErrOutboundQueueFull) {
		t.Fatalf("err = %v at the bound, want ErrOutboundQueueFull", err)
	}

	// Nothing already accepted may be dropped to make room.
	pending, err := st.PendingEnvelopeCount(ctx, serverID)
	if err != nil {
		t.Fatalf("pending count: %v", err)
	}
	if pending != 2 {
		t.Errorf("pendingEnvelopeCount = %d after a refused queue, want the 2 already accepted", pending)
	}

	// Acking frees room again: the bound counts unacked work, not history.
	session := startSession(t, st, serverID, "token-hash")
	if _, _, _, err := st.NextOutbound(ctx, session.ID, serverID, 10); err != nil {
		t.Fatalf("next outbound: %v", err)
	}
	if err := st.AckOutbound(ctx, session.ID, 1); err != nil {
		t.Fatalf("ack outbound: %v", err)
	}
	if _, err := st.QueueEnvelope(ctx, serverID, "test.thing", []byte(`{}`), 2); err != nil {
		t.Errorf("queue after an ack freed room: %v", err)
	}
}

// Sequence state is per session: a new one numbers from 1 again in both
// directions (spec section 9.1).
func TestSequenceStateResetsWithTheSession(t *testing.T) {
	ctx := context.Background()
	st := migrated(t)
	serverID := enrolledServer(t, st, "sequence reset")

	first := startSession(t, st, serverID, "first-token-hash")
	for range 2 {
		if _, err := st.QueueEnvelope(ctx, serverID, "test.thing", []byte(`{}`), 100); err != nil {
			t.Fatalf("queue envelope: %v", err)
		}
	}
	envelopes, high, _, err := st.NextOutbound(ctx, first.ID, serverID, 10)
	if err != nil {
		t.Fatalf("next outbound: %v", err)
	}
	if len(envelopes) != 2 || envelopes[0].Seq != 1 || envelopes[1].Seq != 2 || high != 2 {
		t.Fatalf("first session numbered %+v (high %d), want seq 1 and 2", envelopes, high)
	}
	if _, err := st.ApplyInbound(ctx, first.ID, func(int64) store.InboundApplication {
		return store.InboundApplication{Ack: 7, Accepted: 7}
	}, 100); err != nil {
		t.Fatalf("apply inbound: %v", err)
	}

	second := startSession(t, st, serverID, "second-token-hash")
	envelopes, _, _, err = st.NextOutbound(ctx, second.ID, serverID, 10)
	if err != nil {
		t.Fatalf("next outbound on the second session: %v", err)
	}
	if len(envelopes) != 2 || envelopes[0].Seq != 1 || envelopes[1].Seq != 2 {
		t.Fatalf("second session numbered %+v, want the unacked pair renumbered to 1 and 2",
			envelopes)
	}
	inbound, err := st.ApplyInbound(ctx, second.ID, func(ack int64) store.InboundApplication {
		return store.InboundApplication{Ack: ack}
	}, 100)
	if err != nil {
		t.Fatalf("read inbound ack: %v", err)
	}
	if inbound.Ack != 0 {
		t.Errorf("inbound ack = %d in a fresh session, want 0", inbound.Ack)
	}
}
