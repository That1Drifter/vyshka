package store_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/That1Drifter/vyshka/hub/store"
)

func newWebhook(t *testing.T, st *store.Store, id string, events, serverIDs []string) store.Webhook {
	t.Helper()
	webhook, err := st.CreateWebhook(context.Background(), store.Webhook{
		ID: id, URL: "http://127.0.0.1:1/hook", Secret: "vyw_secret_" + id,
		Template: "generic-json", Events: events, ServerIDs: serverIDs,
	})
	if err != nil {
		t.Fatalf("create webhook: %v", err)
	}
	return webhook
}

// notifyAll fans every unnotified event out to one webhook, unfiltered, which
// is what most of these tests need to get deliveries on the queue.
func notifyAll(t *testing.T, st *store.Store, webhookID string, bound int) int {
	t.Helper()
	marked, err := st.NotifyEvents(context.Background(), 100,
		func(events []store.Event, _ []store.Webhook) []store.NewWebhookDelivery {
			deliveries := make([]store.NewWebhookDelivery, 0, len(events))
			for i, event := range events {
				deliveries = append(deliveries, store.NewWebhookDelivery{
					ID:        "dlv-" + event.ID + "-" + string(rune('a'+i)),
					WebhookID: webhookID,
					Type:      event.Type,
					ServerID:  event.ServerID,
					Body:      json.RawMessage(`{"probe":true}`),
				})
			}
			return deliveries
		}, bound)
	if err != nil {
		t.Fatalf("notify events: %v", err)
	}
	return marked
}

func TestWebhookCRUD(t *testing.T) {
	ctx := context.Background()
	st := migrated(t)

	created := newWebhook(t, st, "wh-1", []string{"core.player.*"}, []string{"srv-1"})
	if created.CreatedAt.IsZero() {
		t.Fatal("createdAt was not assigned")
	}

	read, err := st.WebhookByID(ctx, "wh-1")
	if err != nil {
		t.Fatalf("read webhook: %v", err)
	}
	if read.URL != created.URL || read.Secret != created.Secret {
		t.Errorf("read back %+v, want %+v", read, created)
	}
	if len(read.Events) != 1 || read.Events[0] != "core.player.*" {
		t.Errorf("events round-tripped as %v", read.Events)
	}
	if len(read.ServerIDs) != 1 || read.ServerIDs[0] != "srv-1" {
		t.Errorf("serverIds round-tripped as %v", read.ServerIDs)
	}

	newWebhook(t, st, "wh-2", nil, nil)
	all, err := st.Webhooks(ctx)
	if err != nil {
		t.Fatalf("list webhooks: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("listed %d webhooks, want 2", len(all))
	}

	if err := st.DeleteWebhook(ctx, "wh-1"); err != nil {
		t.Fatalf("delete webhook: %v", err)
	}
	if err := st.DeleteWebhook(ctx, "wh-1"); err != store.ErrNotFound {
		t.Errorf("second delete = %v, want ErrNotFound", err)
	}
	if _, err := st.WebhookByID(ctx, "wh-1"); err != store.ErrNotFound {
		t.Errorf("read after delete = %v, want ErrNotFound", err)
	}
}

// The outbox contract: an event is fanned out exactly once, and the flag and
// the deliveries commit together.
func TestNotifyEventsFansOutOnce(t *testing.T) {
	ctx := context.Background()
	st := migrated(t)
	serverID := enrolledServer(t, st, "hooks-1")
	session := startSession(t, st, serverID, "hash-hooks-1")
	newWebhook(t, st, "wh-1", nil, nil)

	ingest(t, st, session.ID, event("core.player.death", time.Now(), `{"weapon":"M4"}`))

	if marked := notifyAll(t, st, "wh-1", 100); marked != 1 {
		t.Fatalf("first pass marked %d events, want 1", marked)
	}
	if marked := notifyAll(t, st, "wh-1", 100); marked != 0 {
		t.Fatalf("second pass marked %d events, want 0: fan-out must be once", marked)
	}

	due, err := st.DueWebhookDeliveries(ctx, time.Now().UTC().Add(time.Second), 10)
	if err != nil {
		t.Fatalf("due deliveries: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("%d deliveries due, want 1", len(due))
	}
	if due[0].Secret == "" || due[0].URL == "" {
		t.Error("due delivery is missing the webhook join")
	}
	if due[0].Delivery.Type != "core.player.death" || due[0].Delivery.ServerID != serverID {
		t.Errorf("due delivery carries %q for %q", due[0].Delivery.Type, due[0].Delivery.ServerID)
	}
}

func TestDeliveryOutcomeBookkeeping(t *testing.T) {
	ctx := context.Background()
	st := migrated(t)
	serverID := enrolledServer(t, st, "hooks-2")
	session := startSession(t, st, serverID, "hash-hooks-2")
	newWebhook(t, st, "wh-1", nil, nil)
	ingest(t, st, session.ID, event("core.player.chat", time.Now(), `{}`))
	notifyAll(t, st, "wh-1", 100)

	due, err := st.DueWebhookDeliveries(ctx, time.Now().UTC().Add(time.Second), 10)
	if err != nil || len(due) != 1 {
		t.Fatalf("due = %d (%v), want 1", len(due), err)
	}
	deliveryID := due[0].Delivery.ID

	// First failure schedules the retry.
	status := 500
	next := time.Now().UTC().Add(time.Hour)
	if err := st.RecordDeliveryFailure(ctx, deliveryID, &status, "status 500", &next); err != nil {
		t.Fatalf("record failure: %v", err)
	}
	listed, err := st.WebhookDeliveries(ctx, "wh-1", 10)
	if err != nil || len(listed) != 1 {
		t.Fatalf("deliveries = %d (%v), want 1", len(listed), err)
	}
	if listed[0].State != store.DeliveryPending || listed[0].Attempts != 1 {
		t.Errorf("after one failure: state %q attempts %d", listed[0].State, listed[0].Attempts)
	}
	if listed[0].LastStatus == nil || *listed[0].LastStatus != 500 || listed[0].LastError != "status 500" {
		t.Errorf("failure detail not recorded: %+v", listed[0])
	}
	// An hour out means not due now.
	if due, _ := st.DueWebhookDeliveries(ctx, time.Now().UTC(), 10); len(due) != 0 {
		t.Errorf("a delivery scheduled an hour out is already due")
	}

	// Exhausted schedule means dead, and dead is terminal.
	if err := st.RecordDeliveryFailure(ctx, deliveryID, nil, "connection refused", nil); err != nil {
		t.Fatalf("record dead: %v", err)
	}
	listed, _ = st.WebhookDeliveries(ctx, "wh-1", 10)
	if listed[0].State != store.DeliveryDead || listed[0].Attempts != 2 {
		t.Errorf("after exhaustion: state %q attempts %d, want dead/2", listed[0].State, listed[0].Attempts)
	}
	if listed[0].LastStatus != nil {
		t.Errorf("a transport failure recorded status %d", *listed[0].LastStatus)
	}
	if err := st.RecordDeliverySuccess(ctx, deliveryID, 200); err != nil {
		t.Fatalf("record success on dead: %v", err)
	}
	listed, _ = st.WebhookDeliveries(ctx, "wh-1", 10)
	if listed[0].State != store.DeliveryDead {
		t.Errorf("a dead delivery was revived to %q", listed[0].State)
	}
}

// The section 11.5 bound: at the pending cap a delivery is created dead with a
// visible reason, never silently discarded.
func TestPendingBoundDeadOnArrival(t *testing.T) {
	ctx := context.Background()
	st := migrated(t)
	serverID := enrolledServer(t, st, "hooks-3")
	session := startSession(t, st, serverID, "hash-hooks-3")
	newWebhook(t, st, "wh-1", nil, nil)
	ingest(t, st, session.ID,
		event("core.player.chat", time.Now(), `{}`),
		event("core.player.death", time.Now(), `{}`))

	if marked := notifyAll(t, st, "wh-1", 1); marked != 2 {
		t.Fatalf("marked %d, want 2", marked)
	}
	listed, err := st.WebhookDeliveries(ctx, "wh-1", 10)
	if err != nil || len(listed) != 2 {
		t.Fatalf("deliveries = %d (%v), want 2", len(listed), err)
	}
	pending, dead := 0, 0
	for _, delivery := range listed {
		switch delivery.State {
		case store.DeliveryPending:
			pending++
		case store.DeliveryDead:
			dead++
			if !strings.Contains(delivery.LastError, "pending deliveries") {
				t.Errorf("dead-on-arrival lastError %q does not say why", delivery.LastError)
			}
		}
	}
	if pending != 1 || dead != 1 {
		t.Errorf("pending %d dead %d, want 1 and 1", pending, dead)
	}
}

func TestApplyLinkTransitionGuards(t *testing.T) {
	ctx := context.Background()
	st := migrated(t)
	serverID := enrolledServer(t, st, "hooks-4")
	startSession(t, st, serverID, "hash-hooks-4")
	newWebhook(t, st, "wh-1", nil, nil)

	build := func([]store.Webhook) []store.NewWebhookDelivery {
		return []store.NewWebhookDelivery{{
			ID: "dlv-link-" + time.Now().Format("150405.000000000"), WebhookID: "wh-1",
			Type: "server.link.lost", ServerID: serverID, Body: json.RawMessage(`{}`),
		}}
	}
	// The observed evidence is whatever the monitor would have read.
	observed := func() *time.Time {
		server, err := st.Server(ctx, serverID)
		if err != nil {
			t.Fatalf("read server: %v", err)
		}
		return server.LastSeenAt
	}

	applied, err := st.ApplyLinkTransition(ctx, serverID, store.LinkUnknown, store.LinkUp, observed(), nil, 100)
	if err != nil || !applied {
		t.Fatalf("unknown -> up: applied=%v err=%v", applied, err)
	}
	// A racing pass that still believes the old state loses the guard and
	// enqueues nothing.
	applied, err = st.ApplyLinkTransition(ctx, serverID, store.LinkUnknown, store.LinkUp, observed(), build, 100)
	if err != nil || applied {
		t.Fatalf("repeat unknown -> up: applied=%v err=%v, want false", applied, err)
	}
	if listed, _ := st.WebhookDeliveries(ctx, "wh-1", 10); len(listed) != 0 {
		t.Errorf("a lost race still enqueued %d deliveries", len(listed))
	}

	applied, err = st.ApplyLinkTransition(ctx, serverID, store.LinkUp, store.LinkDown, observed(), build, 100)
	if err != nil || !applied {
		t.Fatalf("up -> down: applied=%v err=%v", applied, err)
	}
	listed, _ := st.WebhookDeliveries(ctx, "wh-1", 10)
	if len(listed) != 1 || listed[0].Type != "server.link.lost" {
		t.Errorf("transition deliveries = %+v, want one server.link.lost", listed)
	}

	// The last_seen_at guard: a poll landing between the monitor's read and
	// its write changes the evidence, and the stale decision must not commit.
	if err := st.TouchServer(ctx, serverID); err != nil {
		t.Fatalf("touch server: %v", err)
	}
	stale := time.Now().UTC().Add(-time.Hour)
	applied, err = st.ApplyLinkTransition(ctx, serverID, store.LinkDown, store.LinkUp, &stale, build, 100)
	if err != nil || applied {
		t.Fatalf("transition on stale last_seen_at: applied=%v err=%v, want false", applied, err)
	}
}

// A restoration cannot be bought for a server whose session ended between the
// monitor's read and its write: the transition to up requires a live session
// inside the transaction.
func TestLinkTransitionToUpRequiresALiveSession(t *testing.T) {
	ctx := context.Background()
	st := migrated(t)
	serverID := enrolledServer(t, st, "hooks-6")
	newWebhook(t, st, "wh-1", nil, nil)

	if applied, err := st.ApplyLinkTransition(ctx, serverID, store.LinkUnknown, store.LinkDown, nil, nil, 100); err != nil || !applied {
		t.Fatalf("unknown -> down: applied=%v err=%v", applied, err)
	}
	// No session exists, so up is refused however fresh the evidence claims to be.
	if err := st.TouchServer(ctx, serverID); err != nil {
		t.Fatalf("touch server: %v", err)
	}
	server, err := st.Server(ctx, serverID)
	if err != nil {
		t.Fatalf("read server: %v", err)
	}
	applied, err := st.ApplyLinkTransition(ctx, serverID, store.LinkDown, store.LinkUp, server.LastSeenAt, nil, 100)
	if err != nil || applied {
		t.Fatalf("down -> up with no live session: applied=%v err=%v, want false", applied, err)
	}

	startSession(t, st, serverID, "hash-hooks-6")
	server, err = st.Server(ctx, serverID)
	if err != nil {
		t.Fatalf("read server: %v", err)
	}
	applied, err = st.ApplyLinkTransition(ctx, serverID, store.LinkDown, store.LinkUp, server.LastSeenAt, nil, 100)
	if err != nil || !applied {
		t.Fatalf("down -> up with a live session: applied=%v err=%v, want true", applied, err)
	}
}

func TestPruneWebhookDeliveriesSparesPending(t *testing.T) {
	ctx := context.Background()
	st := migrated(t)
	serverID := enrolledServer(t, st, "hooks-5")
	session := startSession(t, st, serverID, "hash-hooks-5")
	newWebhook(t, st, "wh-1", nil, nil)
	ingest(t, st, session.ID,
		event("core.player.chat", time.Now(), `{}`),
		event("core.player.death", time.Now(), `{}`))
	notifyAll(t, st, "wh-1", 100)

	due, err := st.DueWebhookDeliveries(ctx, time.Now().UTC().Add(time.Second), 10)
	if err != nil || len(due) != 2 {
		t.Fatalf("due = %d (%v), want 2", len(due), err)
	}
	if err := st.RecordDeliverySuccess(ctx, due[0].Delivery.ID, 204); err != nil {
		t.Fatalf("record success: %v", err)
	}

	// A cutoff in the future would prune everything finished; the pending
	// delivery must survive it regardless.
	pruned, err := st.PruneWebhookDeliveries(ctx, time.Now().UTC().Add(time.Hour), 100)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if pruned != 1 {
		t.Errorf("pruned %d, want 1 (the delivered one)", pruned)
	}
	listed, _ := st.WebhookDeliveries(ctx, "wh-1", 10)
	if len(listed) != 1 || listed[0].State != store.DeliveryPending {
		t.Errorf("survivors = %+v, want the pending delivery", listed)
	}
}
