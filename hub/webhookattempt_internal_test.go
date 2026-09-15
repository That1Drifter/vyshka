package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/That1Drifter/vyshka/hub/store"
)

// A pause or a URL change that lands after the dispatcher selected its batch
// and before a worker reached the row must still hold (spec section 11.2):
// the webhook is read again at the moment of the attempt, not trusted from
// the batch.
func TestAttemptDeliveryReadsTheWebhookAtAttemptTime(t *testing.T) {
	s := bootBare(t)
	ctx := context.Background()

	var oldHits, newHits atomic.Int32
	oldTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		oldHits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(oldTarget.Close)
	newTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		newHits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(newTarget.Close)

	if _, err := s.store.CreateWebhook(ctx, store.Webhook{
		ID: "wh-attempt", URL: oldTarget.URL, Secret: "secret", Template: templateGenericJSON,
	}); err != nil {
		t.Fatalf("create webhook: %v", err)
	}
	// The batch as the dispatcher read it, before either edit below.
	batch := store.DueDelivery{
		Delivery: store.WebhookDelivery{
			ID: "dlv-attempt", WebhookID: "wh-attempt", Type: "example-mod.probe",
			Body: json.RawMessage(`{"deliveryId":"dlv-attempt"}`), State: store.DeliveryPending,
			NextAttemptAt: time.Now().UTC(),
		},
		URL: oldTarget.URL, Secret: "secret",
	}

	paused := true
	if _, err := s.store.UpdateWebhook(ctx, "wh-attempt", store.WebhookUpdate{Paused: &paused}, nil); err != nil {
		t.Fatalf("pause: %v", err)
	}
	s.attemptDelivery(batch)
	if oldHits.Load() != 0 || newHits.Load() != 0 {
		t.Fatalf("an attempt began after the pause landed: old %d, new %d", oldHits.Load(), newHits.Load())
	}

	resumed, target := false, newTarget.URL
	if _, err := s.store.UpdateWebhook(ctx, "wh-attempt", store.WebhookUpdate{Paused: &resumed, URL: &target}, nil); err != nil {
		t.Fatalf("resume and retarget: %v", err)
	}
	s.attemptDelivery(batch)
	if oldHits.Load() != 0 || newHits.Load() != 1 {
		t.Fatalf("the attempt went to the batch's URL rather than the current one: old %d, new %d",
			oldHits.Load(), newHits.Load())
	}
}
