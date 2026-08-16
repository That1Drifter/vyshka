package store_test

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/That1Drifter/vyshka/hub/store"
)

// publishManifestRow stores a manifest at the given revision through the same
// ingest path the poll handler uses.
func publishManifestRow(t *testing.T, st *store.Store, sessionID string, revision int64) {
	t.Helper()
	_, err := st.ApplyInbound(context.Background(), sessionID, func(ack int64) store.InboundApplication {
		return store.InboundApplication{Ack: ack, Manifests: []store.ManifestPublish{{
			Revision: revision,
			Body:     []byte(`{"manifestRevision": ` + strconv.FormatInt(revision, 10) + `}`),
		}}}
	}, 100)
	if err != nil {
		t.Fatalf("publish manifest: %v", err)
	}
}

func dispatch(t *testing.T, st *store.Store, serverID, actionID, key string, ttl time.Duration, limit int) (store.Action, bool) {
	t.Helper()
	action, created, err := st.DispatchAction(context.Background(), store.NewAction{
		ID:               actionID,
		ServerID:         serverID,
		Code:             "test.heal",
		Params:           []byte(`{"amount": 1}`),
		IdempotencyKey:   key,
		ExpiresAt:        time.Now().UTC().Add(ttl),
		ManifestRevision: 1,
		EnvelopeType:     "action.dispatch",
		EnvelopeBody:     []byte(`{"actionId": "` + actionID + `"}`),
		QueueLimit:       limit,
	})
	if err != nil {
		t.Fatalf("dispatch action: %v", err)
	}
	return action, created
}

// The idempotency contract of spec section 7: a retry with the same key
// returns the original action and queues nothing, even when the queue is full.
func TestDispatchActionIdempotency(t *testing.T) {
	ctx := context.Background()
	st := migrated(t)
	serverID := enrolledServer(t, st, "action idempotency")
	session := startSession(t, st, serverID, "token-hash")
	publishManifestRow(t, st, session.ID, 1)

	first, created := dispatch(t, st, serverID, "action-1", "retry-key", time.Minute, 1)
	if !created || first.State != store.ActionQueued {
		t.Fatalf("first dispatch: created = %v, state = %q, want a fresh queued action", created, first.State)
	}

	// The queue is now at its bound of 1, and the retry must still answer
	// with the original rather than outbound_queue_full.
	retry, created := dispatch(t, st, serverID, "action-2", "retry-key", time.Minute, 1)
	if created {
		t.Error("a retry with the same idempotency key created a second action")
	}
	if retry.ID != first.ID {
		t.Errorf("retry returned action %q, want the original %q", retry.ID, first.ID)
	}
	pending, err := st.PendingEnvelopeCount(ctx, serverID)
	if err != nil {
		t.Fatalf("pending count: %v", err)
	}
	if pending != 1 {
		t.Errorf("pendingEnvelopeCount = %d after an idempotent retry, want the original 1", pending)
	}

	// A fresh dispatch against the full queue is refused, and the refusal
	// leaves no half-recorded action behind.
	_, _, err = st.DispatchAction(ctx, store.NewAction{
		ID: "action-3", ServerID: serverID, Code: "test.heal", Params: []byte(`{}`),
		ExpiresAt: time.Now().UTC().Add(time.Minute), ManifestRevision: 1,
		EnvelopeType: "action.dispatch", EnvelopeBody: []byte(`{}`), QueueLimit: 1,
	})
	if !errors.Is(err, store.ErrOutboundQueueFull) {
		t.Fatalf("err = %v at the bound, want ErrOutboundQueueFull", err)
	}
	if _, err := st.ActionByID(ctx, "action-3"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a refused dispatch left an action row behind (err = %v)", err)
	}
}

// The revision gate: a dispatch validated against a manifest that was replaced
// before the insert is refused, never queued with params the current manifest
// did not approve. The idempotent-retry path stays exempt: the original was
// already accepted.
func TestDispatchActionRefusesAStaleManifestRevision(t *testing.T) {
	ctx := context.Background()
	st := migrated(t)
	serverID := enrolledServer(t, st, "stale revision")
	session := startSession(t, st, serverID, "token-hash")
	publishManifestRow(t, st, session.ID, 1)

	// Accepted while revision 1 was current.
	dispatch(t, st, serverID, "accepted-1", "kept-key", time.Minute, 100)

	// The manifest moves on; a dispatch still carrying revision 1 is refused.
	publishManifestRow(t, st, session.ID, 2)
	_, _, err := st.DispatchAction(ctx, store.NewAction{
		ID: "stale-1", ServerID: serverID, Code: "test.heal", Params: []byte(`{}`),
		ExpiresAt: time.Now().UTC().Add(time.Minute), ManifestRevision: 1,
		EnvelopeType: "action.dispatch", EnvelopeBody: []byte(`{}`), QueueLimit: 100,
	})
	if !errors.Is(err, store.ErrManifestChanged) {
		t.Fatalf("err = %v for a stale revision, want ErrManifestChanged", err)
	}
	pending, err := st.PendingEnvelopeCount(ctx, serverID)
	if err != nil {
		t.Fatalf("pending count: %v", err)
	}
	if pending != 1 {
		t.Errorf("pendingEnvelopeCount = %d, want only the accepted dispatch's envelope", pending)
	}

	// The accepted action is still resolvable by key: the handler consults it
	// before the revision is ever compared, so retries survive the republish.
	if _, err := st.ActionByIdempotencyKey(ctx, serverID, "kept-key"); err != nil {
		t.Fatalf("read by idempotency key: %v", err)
	}
}

// Expiry retires the dispatch envelope of undelivered work, so an offline
// server's expired actions stop counting against the queue bound. An envelope
// already numbered into a session is left for retransmission instead, because
// deleting it would open a gap the plugin's contiguous ack could never cross.
func TestExpiryRetiresUndeliveredDispatchEnvelopes(t *testing.T) {
	ctx := context.Background()
	st := migrated(t)
	serverID := enrolledServer(t, st, "expiry retires")
	session := startSession(t, st, serverID, "token-hash")
	publishManifestRow(t, st, session.ID, 1)

	// Numbered into the live session: survives expiry for retransmission.
	numbered, _ := dispatch(t, st, serverID, "numbered-1", "", 30*time.Millisecond, 100)
	if _, _, _, err := st.NextOutbound(ctx, session.ID, serverID, 10); err != nil {
		t.Fatalf("next outbound: %v", err)
	}
	// Never delivered: retired with the action.
	undelivered, _ := dispatch(t, st, serverID, "undelivered-1", "", 30*time.Millisecond, 100)

	time.Sleep(50 * time.Millisecond)
	if _, err := st.ExpireActions(ctx); err != nil {
		t.Fatalf("expire actions: %v", err)
	}

	for _, id := range []string{numbered.ID, undelivered.ID} {
		action, err := st.ActionByID(ctx, id)
		if err != nil {
			t.Fatalf("read action %s: %v", id, err)
		}
		if action.State != store.ActionExpired {
			t.Errorf("action %s state = %q, want expired", id, action.State)
		}
	}

	pending, err := st.PendingEnvelopeCount(ctx, serverID)
	if err != nil {
		t.Fatalf("pending count: %v", err)
	}
	if pending != 1 {
		t.Errorf("pendingEnvelopeCount = %d, want only the numbered envelope kept for retransmission", pending)
	}
	envelopes, _, _, err := st.NextOutbound(ctx, session.ID, serverID, 10)
	if err != nil {
		t.Fatalf("next outbound after expiry: %v", err)
	}
	if len(envelopes) != 1 || envelopes[0].ID != numbered.EnvelopeID {
		t.Errorf("delivery after expiry = %+v, want only the numbered envelope", envelopes)
	}
}

// The state machine only moves forward, the envelope ack is the delivery
// receipt, and expiry beats anything that arrives after the deadline.
func TestActionLifecycleTransitions(t *testing.T) {
	ctx := context.Background()
	st := migrated(t)
	serverID := enrolledServer(t, st, "action transitions")
	session := startSession(t, st, serverID, "token-hash")
	publishManifestRow(t, st, session.ID, 1)

	action, _ := dispatch(t, st, serverID, "walk-1", "", time.Minute, 100)

	// Delivery receipt: the plugin acks the envelope carrying the dispatch.
	if _, _, _, err := st.NextOutbound(ctx, session.ID, serverID, 10); err != nil {
		t.Fatalf("next outbound: %v", err)
	}
	if err := st.AckOutbound(ctx, session.ID, 1); err != nil {
		t.Fatalf("ack outbound: %v", err)
	}
	delivered, err := st.ActionByID(ctx, action.ID)
	if err != nil {
		t.Fatalf("read action: %v", err)
	}
	if delivered.State != store.ActionDelivered || delivered.DeliveredAt == nil {
		t.Fatalf("state = %q after the envelope ack, want delivered with a timestamp", delivered.State)
	}

	// action.ack -> running, then action.result -> completed.
	apply := func(application store.InboundApplication) store.InboundApplied {
		t.Helper()
		applied, err := st.ApplyInbound(ctx, session.ID, func(ack int64) store.InboundApplication {
			application.Ack = ack
			return application
		}, 100)
		if err != nil {
			t.Fatalf("apply inbound: %v", err)
		}
		return applied
	}

	if applied := apply(store.InboundApplication{ActionAcks: []string{action.ID}}); applied.ActionsStarted != 1 {
		t.Fatalf("ActionsStarted = %d after action.ack, want 1", applied.ActionsStarted)
	}
	duration := int64(12)
	result := store.ActionResult{ActionID: action.ID, OK: true,
		Result: []byte(`{"healedTo": 100}`), DurationMs: &duration}
	if applied := apply(store.InboundApplication{ActionResults: []store.ActionResult{result}}); applied.ActionsFinished != 1 {
		t.Fatalf("ActionsFinished = %d after action.result, want 1", applied.ActionsFinished)
	}

	finished, err := st.ActionByID(ctx, action.ID)
	if err != nil {
		t.Fatalf("read action: %v", err)
	}
	if finished.State != store.ActionCompleted || finished.OK == nil || !*finished.OK {
		t.Fatalf("state = %q ok = %v, want completed true", finished.State, finished.OK)
	}
	if string(finished.Result) != `{"healedTo": 100}` || finished.DurationMs == nil || *finished.DurationMs != 12 {
		t.Errorf("result = %s duration = %v, want the reported payload", finished.Result, finished.DurationMs)
	}

	// Terminal is terminal: a repeat ack or result changes nothing.
	if applied := apply(store.InboundApplication{ActionAcks: []string{action.ID},
		ActionResults: []store.ActionResult{{ActionID: action.ID, OK: false, Error: "late"}}}); applied.ActionsStarted != 0 || applied.ActionsFinished != 0 {
		t.Errorf("a terminal action moved again: started %d finished %d",
			applied.ActionsStarted, applied.ActionsFinished)
	}
	unknown := apply(store.InboundApplication{ActionAcks: []string{"no-such-action"}})
	if unknown.ActionsStarted != 0 {
		t.Errorf("an unknown actionId started %d actions", unknown.ActionsStarted)
	}
}

// Expiry wins over anything that arrives after the deadline (spec section 7):
// a late result is a graceful no-op, never a resurrection.
func TestActionExpiryBeatsALateResult(t *testing.T) {
	ctx := context.Background()
	st := migrated(t)
	serverID := enrolledServer(t, st, "action expiry")
	session := startSession(t, st, serverID, "token-hash")
	publishManifestRow(t, st, session.ID, 1)

	action, _ := dispatch(t, st, serverID, "expire-1", "", 10*time.Millisecond, 100)
	time.Sleep(20 * time.Millisecond)

	// The result arrives after the deadline but before any sweep: expiry
	// still wins, because the deadline passed, not because a sweeper ran.
	applied, err := st.ApplyInbound(ctx, session.ID, func(ack int64) store.InboundApplication {
		return store.InboundApplication{Ack: ack,
			ActionResults: []store.ActionResult{{ActionID: action.ID, OK: true, Result: []byte(`{}`)}}}
	}, 100)
	if err != nil {
		t.Fatalf("apply inbound: %v", err)
	}
	if applied.ActionsFinished != 0 {
		t.Errorf("a result after the deadline finished %d actions, want 0", applied.ActionsFinished)
	}

	expired, err := st.ActionByID(ctx, action.ID)
	if err != nil {
		t.Fatalf("read action: %v", err)
	}
	if expired.State != store.ActionExpired {
		t.Errorf("state = %q, want expired", expired.State)
	}
	if expired.OK != nil || expired.Result != nil {
		t.Errorf("an expired action carries a result: ok = %v result = %s", expired.OK, expired.Result)
	}
}
