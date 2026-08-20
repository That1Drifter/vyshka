package hub

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/That1Drifter/vyshka/hub/store"
)

func TestWebhookMatches(t *testing.T) {
	cases := []struct {
		name      string
		events    []string
		serverIDs []string
		eventType string
		serverID  string
		want      bool
	}{
		{"empty filters match everything", nil, nil, "core.player.death", "srv-1", true},
		{"exact type", []string{"core.player.death"}, nil, "core.player.death", "srv-1", true},
		{"exact type mismatch", []string{"core.player.death"}, nil, "core.player.chat", "srv-1", false},
		{"namespace pattern", []string{"core.player.*"}, nil, "core.player.death", "srv-1", true},
		{"namespace keeps its dot", []string{"core.player.*"}, nil, "core.players.death", "srv-1", false},
		{"catch-all", []string{"*"}, nil, "example-mod.raid.start", "srv-1", true},
		{"lifecycle by exact type", []string{"action.completed"}, nil, "action.completed", "srv-1", true},
		{"lifecycle by namespace", []string{"server.link.*"}, nil, "server.link.lost", "srv-1", true},
		{"server filter admits", []string{"*"}, []string{"srv-1", "srv-2"}, "core.player.death", "srv-2", true},
		{"server filter refuses", []string{"*"}, []string{"srv-1"}, "core.player.death", "srv-9", false},
		{"one of several patterns", []string{"example-mod.*", "action.completed"}, nil, "action.completed", "srv-1", true},
	}
	for _, tc := range cases {
		webhook := store.Webhook{Events: tc.events, ServerIDs: tc.serverIDs}
		if got := webhookMatches(webhook, tc.eventType, tc.serverID); got != tc.want {
			t.Errorf("%s: webhookMatches = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The RFC 4231-adjacent known-answer vector everyone uses for HMAC-SHA256,
// so the signature is pinned to the algorithm, not to this implementation.
func TestSignWebhookBodyKnownVector(t *testing.T) {
	got := signWebhookBody("key", []byte("The quick brown fox jumps over the lazy dog"))
	want := "sha256=f7bc83f430538424b13298e6aa6fb143ef4d59a14946175997479dbc2d1a3cd8"
	if got != want {
		t.Fatalf("signWebhookBody = %q, want %q", got, want)
	}
}

// bootBare boots a hub inside the package, for tests that drive unexported
// machinery directly.
func bootBare(t *testing.T) *Server {
	t.Helper()
	server, err := New(context.Background(), Config{
		DatabaseURL: filepath.Join(t.TempDir(), "test.db"),
		AdminToken:  "vya_INTERNALTESTTOKENINTERNAL",
		Logger:      slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("boot hub: %v", err)
	}
	t.Cleanup(func() { server.Close() })
	return server
}

// plantLinkedServer inserts a server with a live session directly, because the
// link monitor cares about rows, not about how enrollment produced them.
func plantLinkedServer(t *testing.T, s *Server, serverID string, lastSeen time.Time) {
	t.Helper()
	now := time.Now().UTC()
	db := s.store.DB()
	if _, err := db.Exec(
		`INSERT INTO servers (id, name, game, created_at, secret_hash, enrolled_at, last_seen_at)
		 VALUES (?, ?, 'test-game', ?, ?, ?, ?)`,
		serverID, serverID, envelopeTimestamp(now), "hash-"+serverID,
		envelopeTimestamp(now), envelopeTimestamp(lastSeen),
	); err != nil {
		t.Fatalf("insert server: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO sessions (id, token_hash, server_id, created_at, expires_at,
		                       protocol_version, poll_timeout_seconds)
		 VALUES (?, ?, ?, ?, ?, 1, 5)`,
		"sess-"+serverID, "sess-hash-"+serverID, serverID,
		envelopeTimestamp(now), envelopeTimestamp(now.Add(time.Hour)),
	); err != nil {
		t.Fatalf("insert session: %v", err)
	}
}

func setLastSeen(t *testing.T, s *Server, serverID string, lastSeen time.Time) {
	t.Helper()
	if _, err := s.store.DB().Exec(
		`UPDATE servers SET last_seen_at = ? WHERE id = ?`,
		envelopeTimestamp(lastSeen), serverID); err != nil {
		t.Fatalf("update last_seen_at: %v", err)
	}
}

func linkState(t *testing.T, s *Server, serverID string) string {
	t.Helper()
	var state string
	if err := s.store.DB().QueryRow(
		`SELECT link_state FROM servers WHERE id = ?`, serverID).Scan(&state); err != nil {
		t.Fatalf("read link_state: %v", err)
	}
	return state
}

// awaitCondition tolerates the running dispatcher racing these tests' explicit
// checkLinks calls: the transition guard means exactly one of them wins, so
// assertions watch state rather than return values.
func awaitCondition(t *testing.T, what string, pred func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if pred() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestCheckLinksFiresTransitionsOncePerEdge(t *testing.T) {
	s := bootBare(t)
	ctx := context.Background()

	webhook, err := s.store.CreateWebhook(ctx, store.Webhook{
		ID: "wh-link", URL: "http://127.0.0.1:1/hook", Secret: "vyw_link",
		Template: templateGenericJSON, Events: []string{"server.link.*"},
	})
	if err != nil {
		t.Fatalf("create webhook: %v", err)
	}

	deliveryTypes := func() []string {
		listed, err := s.store.WebhookDeliveries(ctx, webhook.ID, 10)
		if err != nil {
			t.Fatalf("list deliveries: %v", err)
		}
		types := make([]string, 0, len(listed))
		for _, delivery := range listed {
			types = append(types, delivery.Type)
		}
		return types
	}

	// A server whose link ended before the monitor ever classified it is
	// caught up silently: the first classification fires nothing in either
	// direction (spec section 11.1).
	plantLinkedServer(t, s, "srv-stale", time.Now().Add(-time.Hour))
	s.checkLinks(ctx)
	awaitCondition(t, "silent unknown -> down", func() bool { return linkState(t, s, "srv-stale") == store.LinkDown })
	if types := deliveryTypes(); len(types) != 0 {
		t.Fatalf("the first classification fired %v, want nothing", types)
	}

	// Fresh traffic: unknown -> up, silently. First sight is not a restoration.
	plantLinkedServer(t, s, "srv-live", time.Now())
	s.checkLinks(ctx)
	awaitCondition(t, "unknown -> up", func() bool { return linkState(t, s, "srv-live") == store.LinkUp })
	if types := deliveryTypes(); len(types) != 0 {
		t.Fatalf("unknown -> up fired %v, want nothing", types)
	}

	// Silence past twice the pollTimeout plus grace: up -> down, with the lost
	// notification, exactly once even when checked repeatedly.
	setLastSeen(t, s, "srv-live", time.Now().Add(-100*time.Second))
	s.checkLinks(ctx)
	s.checkLinks(ctx)
	awaitCondition(t, "up -> down", func() bool { return linkState(t, s, "srv-live") == store.LinkDown })
	if types := deliveryTypes(); len(types) != 1 || types[0] != notifyServerLinkLost {
		t.Fatalf("up -> down fired %v, want one %s", types, notifyServerLinkLost)
	}

	// Traffic resumes: down -> up, with the restoration.
	setLastSeen(t, s, "srv-live", time.Now())
	s.checkLinks(ctx)
	awaitCondition(t, "down -> up", func() bool { return linkState(t, s, "srv-live") == store.LinkUp })
	awaitCondition(t, "the restored notification", func() bool {
		types := deliveryTypes()
		return len(types) == 2 && (types[0] == notifyServerLinkRestore || types[1] == notifyServerLinkRestore)
	})
}

// The registration boundary inside fanOut: an older notification is never
// delivered to a younger webhook, whatever the outbox hands the pass.
func TestFanOutHonorsTheRegistrationBoundary(t *testing.T) {
	now := time.Now().UTC()
	webhook := store.Webhook{ID: "wh-young", Events: nil, CreatedAt: now}
	older := notification{Type: "core.player.death", ServerID: "srv-1",
		OccurredAt: now, LandedAt: now.Add(-time.Minute)}
	newer := notification{Type: "core.player.death", ServerID: "srv-1",
		OccurredAt: now, LandedAt: now}

	if deliveries := fanOut([]store.Webhook{webhook}, []notification{older}); len(deliveries) != 0 {
		t.Fatalf("a notification that landed before registration produced %d deliveries; registration is not a backfill (section 11.2)", len(deliveries))
	}
	if deliveries := fanOut([]store.Webhook{webhook}, []notification{newer}); len(deliveries) != 1 {
		t.Fatalf("a notification landing after registration produced %d deliveries, want 1", len(deliveries))
	}
}
