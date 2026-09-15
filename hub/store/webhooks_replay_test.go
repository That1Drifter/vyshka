package store_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/That1Drifter/vyshka/hub/store"
)

// A replay that lands while an attempt is in flight wins (spec section 11.5):
// the in-flight outcome is booked against the generation it was read under,
// which the replay has moved past, so it is discarded and the replay's own
// attempt is the one that counts.
func TestReplayOutranksAnInFlightOutcome(t *testing.T) {
	ctx := context.Background()
	st := migrated(t)
	serverID := enrolledServer(t, st, "hooks-generation")
	session := startSession(t, st, serverID, "hash-hooks-generation")
	newWebhook(t, st, "wh-1", nil, nil)
	ingest(t, st, session.ID, event("core.player.chat", time.Now(), `{}`))
	notifyAll(t, st, "wh-1", 100)

	due, err := st.DueWebhookDeliveries(ctx, time.Now().UTC().Add(time.Second), 10)
	if err != nil || len(due) != 1 {
		t.Fatalf("due = %d (%v), want 1", len(due), err)
	}
	inFlight := due[0].Delivery
	if inFlight.Generation != 0 {
		t.Fatalf("a fresh delivery has generation %d, want 0", inFlight.Generation)
	}
	if types, err := st.PendingDeliveryTypes(ctx, "wh-1"); err != nil || !slices.Equal(types, []string{"core.player.chat"}) {
		t.Fatalf("pending types = %v (%v), want the one queued type", types, err)
	}

	replayed, err := st.ReplayWebhookDelivery(ctx, "wh-1", inFlight.ID)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replayed.Generation != inFlight.Generation+1 {
		t.Fatalf("replay moved the generation to %d, want %d", replayed.Generation, inFlight.Generation+1)
	}

	// The attempt that was in flight books nothing, whichever way it went.
	status := 500
	if err := st.RecordDeliveryFailure(ctx, inFlight.ID, inFlight.Generation, &status, "status 500", nil); !errors.Is(err, store.ErrStaleAttempt) {
		t.Fatalf("a failure from before the replay was booked: err = %v", err)
	}
	if err := st.RecordDeliverySuccess(ctx, inFlight.ID, inFlight.Generation, 204); !errors.Is(err, store.ErrStaleAttempt) {
		t.Fatalf("a success from before the replay was booked: err = %v", err)
	}
	listed, err := st.WebhookDeliveries(ctx, "wh-1", 10)
	if err != nil || len(listed) != 1 {
		t.Fatalf("list: %d (%v)", len(listed), err)
	}
	if listed[0].State != store.DeliveryPending || listed[0].Attempts != 0 {
		t.Fatalf("after stale outcomes the delivery is %s with %d attempts, want pending with 0",
			listed[0].State, listed[0].Attempts)
	}

	// The replay's own attempt books as usual.
	if err := st.RecordDeliverySuccess(ctx, inFlight.ID, replayed.Generation, 204); err != nil {
		t.Fatalf("the replay's attempt could not be booked: %v", err)
	}
	if listed, _ = st.WebhookDeliveries(ctx, "wh-1", 10); listed[0].State != store.DeliveryDelivered {
		t.Fatalf("state = %s after the replay's attempt, want delivered", listed[0].State)
	}
}

// The coverage decision on an edit is taken inside the transaction on the
// locked row, and a refusal there leaves the row untouched.
func TestUpdateWebhookAuthorizesOnTheLockedRow(t *testing.T) {
	ctx := context.Background()
	st := migrated(t)
	newWebhook(t, st, "wh-1", []string{"public.*"}, nil)

	refused := errors.New("refused by the caller")
	var seen store.Webhook
	var seenTypes []string
	_, err := st.UpdateWebhook(ctx, "wh-1", store.WebhookUpdate{URL: stringPtr("http://127.0.0.1:2/moved")},
		func(existing store.Webhook, pendingTypes []string) error {
			seen, seenTypes = existing, pendingTypes
			return refused
		})
	if !errors.Is(err, refused) {
		t.Fatalf("err = %v, want the caller's refusal returned as it is", err)
	}
	if !slices.Equal(seen.Events, []string{"public.*"}) {
		t.Errorf("authorize saw events %v, want the stored filter", seen.Events)
	}
	if seenTypes == nil {
		t.Error("a URL change must hand authorize the pending types, an empty list when there are none")
	}
	current, err := st.WebhookByID(ctx, "wh-1")
	if err != nil || current.URL == "http://127.0.0.1:2/moved" {
		t.Fatalf("a refused edit was written: url = %q (%v)", current.URL, err)
	}

	// Without a URL change the pending types are not read.
	if _, err := st.UpdateWebhook(ctx, "wh-1", store.WebhookUpdate{Events: &[]string{"public.notice"}},
		func(existing store.Webhook, pendingTypes []string) error {
			if pendingTypes != nil {
				t.Errorf("pending types were read for an edit that keeps the URL: %v", pendingTypes)
			}
			return nil
		}); err != nil {
		t.Fatalf("authorized edit: %v", err)
	}
	if current, _ = st.WebhookByID(ctx, "wh-1"); !slices.Equal(current.Events, []string{"public.notice"}) {
		t.Fatalf("an authorized edit was not written: events = %v", current.Events)
	}
}
