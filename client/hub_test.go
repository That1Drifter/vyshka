package client_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/That1Drifter/vyshka/client"
	"github.com/That1Drifter/vyshka/hub"
)

const hubToken = "vya_CLIENTHUBTESTTOKEN0000000"

// newHub boots a real hub in-process and returns a client of it.
func newHub(t *testing.T) (*client.Client, string) {
	t.Helper()
	server, err := hub.New(context.Background(), hub.Config{
		DatabaseURL: filepath.Join(t.TempDir(), "hub.db"),
		AdminToken:  hubToken,
		Logger:      slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("boot hub: %v", err)
	}
	t.Cleanup(func() { server.Close() })
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	c, err := client.New(httpServer.URL, hubToken)
	if err != nil {
		t.Fatal(err)
	}
	return c, httpServer.URL
}

func refusalCode(err error) string {
	var refusal *client.Error
	if errors.As(err, &refusal) {
		return refusal.Code
	}
	return ""
}

// eventually retries check until it passes or ten seconds go by.
func eventually(t *testing.T, what string, check func() error) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := check()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("gave up waiting for %s: %v", what, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestTokensAndTheAuditLogAgainstAHub(t *testing.T) {
	c, url := newHub(t)
	ctx := context.Background()

	created, err := c.CreateServer(ctx, client.CreateServerRequest{Name: "tokens"})
	if err != nil {
		t.Fatal(err)
	}
	minted, err := c.CreateToken(ctx, client.CreateTokenRequest{
		Name: "reader", Scopes: []string{"servers:read"}, Servers: []string{created.Server.ID}, ExpiresInSeconds: 3600,
	})
	if err != nil {
		t.Fatal(err)
	}
	if minted.Secret == "" || minted.Token.ExpiresAt == nil || !slices.Equal(minted.Token.Servers, []string{created.Server.ID}) {
		t.Fatalf("minted = %+v", minted)
	}

	reader, err := client.New(url, minted.Secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.GetServer(ctx, created.Server.ID); err != nil {
		t.Errorf("the bound token reads its server: %v", err)
	}
	if _, err := reader.ListTokens(ctx); refusalCode(err) != "forbidden" {
		t.Errorf("the bound token lists tokens: %v, want forbidden", err)
	}

	list, err := c.ListTokens(ctx)
	if err != nil || !slices.ContainsFunc(list.Tokens, func(tok client.Token) bool { return tok.ID == minted.Token.ID }) {
		t.Fatalf("ListTokens = %+v, %v", list, err)
	}
	if err := c.RevokeToken(ctx, minted.Token.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.RevokeToken(ctx, minted.Token.ID); err != nil {
		t.Errorf("revoking twice: %v", err)
	}
	if _, err := reader.GetServer(ctx, created.Server.ID); refusalCode(err) == "" {
		t.Errorf("a revoked token still reads: %v", err)
	}

	// The server's creation is the one mutation recorded against it; the
	// mint, the revocations, and the creation make four in all.
	against, err := c.ListAudit(ctx, client.AuditQuery{ServerID: created.Server.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(against.Records) != 1 || against.Records[0].Path != "/api/v1/servers" || against.Records[0].Status != http.StatusCreated {
		t.Fatalf("the records against the server: %d, %s", len(against.Records), against.Raw)
	}
	var seen []string
	query := client.AuditQuery{Limit: 1}
	for {
		page, err := c.ListAudit(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		for _, record := range page.Records {
			seen = append(seen, record.Method+" "+record.Path)
		}
		if page.NextCursor == "" {
			break
		}
		query.Cursor = page.NextCursor
	}
	want := []string{
		"DELETE /api/v1/tokens/" + minted.Token.ID, "DELETE /api/v1/tokens/" + minted.Token.ID,
		"POST /api/v1/tokens", "POST /api/v1/servers",
	}
	if !slices.Equal(seen, want) {
		t.Errorf("the log, one record a page: %q, want %q", seen, want)
	}
}

func TestEnvelopesAndNotesAgainstAHub(t *testing.T) {
	c, _ := newHub(t)
	ctx := context.Background()

	created, err := c.CreateServer(ctx, client.CreateServerRequest{Name: "envelopes"})
	if err != nil {
		t.Fatal(err)
	}
	queued, err := c.QueueEnvelope(ctx, created.Server.ID, client.EnvelopeRequest{Type: "example-mod.reload", Body: map[string]any{"modules": []string{"loot"}}})
	if err != nil || queued.ID == "" || queued.Type != "example-mod.reload" || queued.TS.IsZero() {
		t.Fatalf("QueueEnvelope = %+v, %v", queued, err)
	}
	if _, err := c.QueueEnvelope(ctx, created.Server.ID, client.EnvelopeRequest{Type: "action.dispatch"}); refusalCode(err) != "conflict" {
		t.Errorf("queueing a modelled type: %v, want conflict", err)
	}
	server, err := c.GetServer(ctx, created.Server.ID)
	if err != nil || server.PendingEnvelopeCount != 1 {
		t.Errorf("the queue holds %d, want 1 (%v)", server.PendingEnvelopeCount, err)
	}

	// A dot segment travels as %2e, so an identity of exactly "." is
	// reachable, which no fetch-based client can do.
	for _, playerID := range []string{"76561198000000001", "."} {
		note, err := c.CreatePlayerNote(ctx, "steam", playerID, "seen duping")
		if err != nil {
			t.Fatalf("%s: CreatePlayerNote: %v", playerID, err)
		}
		if note.Player != (client.Identity{Platform: "steam", ID: playerID}) || note.Text != "seen duping" {
			t.Errorf("%s: note = %+v", playerID, note)
		}
		notes, err := c.PlayerNotes(ctx, "steam", playerID, client.PageQuery{})
		if err != nil || len(notes.Notes) != 1 || notes.Notes[0].ID != note.ID {
			t.Fatalf("%s: PlayerNotes = %+v, %v", playerID, notes, err)
		}
		if err := c.DeletePlayerNote(ctx, "steam", playerID, note.ID); err != nil {
			t.Fatal(err)
		}
		if err := c.DeletePlayerNote(ctx, "steam", playerID, note.ID); !client.IsNotFound(err) {
			t.Errorf("%s: deleting twice: %v, want not_found", playerID, err)
		}
	}
}

func TestBansAgainstAHub(t *testing.T) {
	c, _ := newHub(t)
	ctx := context.Background()

	player := client.Identity{Platform: "steam", ID: "76561198000000002"}
	permanent, err := c.CreateBan(ctx, client.CreateBanRequest{Player: player, Reason: "speed hack"})
	if err != nil {
		t.Fatal(err)
	}
	if permanent.Ban.ExpiresAt != nil || permanent.Ban.ServerID != nil || permanent.Ban.State != "active" || permanent.Revision < 1 {
		t.Errorf("a ban without a duration or a server: %+v", permanent)
	}
	_, err = c.CreateBan(ctx, client.CreateBanRequest{Player: player, Reason: "again"})
	var refusal *client.Error
	if !errors.As(err, &refusal) || refusal.Code != "conflict" || refusal.Details["banId"] != permanent.Ban.ID {
		t.Errorf("a second active ban: %v, want conflict naming %s", err, permanent.Ban.ID)
	}

	got, err := c.GetBan(ctx, permanent.Ban.ID)
	if err != nil || got.ID != permanent.Ban.ID || got.Player != player {
		t.Fatalf("GetBan = %+v, %v", got, err)
	}
	lifted, err := c.LiftBan(ctx, permanent.Ban.ID)
	if err != nil || lifted.Ban.State != "lifted" || lifted.Ban.LiftedAt == nil || lifted.Ban.LiftedBy == nil {
		t.Fatalf("LiftBan = %+v, %v", lifted, err)
	}

	timed, err := c.CreateBan(ctx, client.CreateBanRequest{Player: player, Reason: "again", DurationSeconds: 3600, Name: "Survivor"})
	if err != nil || timed.Ban.ExpiresAt == nil || timed.Ban.Name != "Survivor" {
		t.Fatalf("a timed ban = %+v, %v", timed, err)
	}
	active, err := c.ListBans(ctx, client.BanQuery{})
	if err != nil || len(active.Bans) != 1 || active.Bans[0].ID != timed.Ban.ID || active.Revision != timed.Revision {
		t.Errorf("the active list = %+v, %v", active, err)
	}
	history, err := c.ListBans(ctx, client.BanQuery{State: "all", Player: &player, Limit: 1})
	if err != nil || len(history.Bans) != 1 || history.NextCursor == "" {
		t.Fatalf("the first page of the history = %+v, %v", history, err)
	}
	rest, err := c.ListBans(ctx, client.BanQuery{State: "all", Player: &player, Cursor: history.NextCursor})
	if err != nil || len(rest.Bans) != 1 || rest.Bans[0].ID == history.Bans[0].ID {
		t.Fatalf("the rest of the history = %+v, %v", rest, err)
	}
}

func TestWebhooksAgainstAHub(t *testing.T) {
	c, _ := newHub(t)
	ctx := context.Background()

	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if r.URL.Path == "/ok" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer receiver.Close()

	// The audit notification needs no plugin: every mutation below is one.
	failing, err := c.CreateWebhook(ctx, client.CreateWebhookRequest{
		URL: receiver.URL + "/fail", Events: []string{"audit.recorded"}, Redact: []string{"detail"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if failing.Secret == "" || failing.Webhook.Template != "generic-json" || failing.Webhook.PausedAt != nil {
		t.Errorf("registration = %+v", failing)
	}
	ok, err := c.CreateWebhook(ctx, client.CreateWebhookRequest{URL: receiver.URL + "/ok", Events: []string{"audit.*"}, Template: "generic-json"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateServer(ctx, client.CreateServerRequest{Name: "noise"}); err != nil {
		t.Fatal(err)
	}

	eventually(t, "a delivered delivery", func() error {
		list, err := c.WebhookDeliveries(ctx, ok.Webhook.ID, 10)
		if err != nil {
			return err
		}
		for _, d := range list.Deliveries {
			if d.State == "delivered" && d.DeliveredAt != nil {
				return nil
			}
		}
		return errors.New("none delivered yet")
	})
	var attempted client.Delivery
	eventually(t, "a failed attempt", func() error {
		list, err := c.WebhookDeliveries(ctx, failing.Webhook.ID, 0)
		if err != nil {
			return err
		}
		for _, d := range list.Deliveries {
			if d.Attempts > 0 && d.LastStatus != nil {
				attempted = d
				return nil
			}
		}
		return errors.New("none attempted yet")
	})
	if *attempted.LastStatus != http.StatusInternalServerError || attempted.DeliveredAt != nil {
		t.Errorf("the failed attempt = %+v", attempted)
	}

	replayed, err := c.ReplayDelivery(ctx, failing.Webhook.ID, attempted.ID)
	if err != nil || replayed.ID != attempted.ID || replayed.State != "pending" || replayed.NextAttemptAt == nil {
		t.Fatalf("ReplayDelivery = %+v, %v", replayed, err)
	}
	if _, err := c.ReplayDelivery(ctx, ok.Webhook.ID, attempted.ID); !client.IsNotFound(err) {
		t.Errorf("replaying another webhook's delivery: %v, want not_found", err)
	}

	paused := true
	edited, err := c.UpdateWebhook(ctx, failing.Webhook.ID, client.UpdateWebhookRequest{Paused: &paused})
	if err != nil || edited.PausedAt == nil {
		t.Fatalf("pausing = %+v, %v", edited, err)
	}
	// A pointer to a nil slice clears the list rather than sending null.
	var none []string
	cleared, err := c.UpdateWebhook(ctx, failing.Webhook.ID, client.UpdateWebhookRequest{Redact: &none})
	if err != nil || len(cleared.Redact) != 0 || cleared.PausedAt == nil {
		t.Fatalf("clearing redact = %+v, %v", cleared, err)
	}
	if none != nil {
		t.Errorf("UpdateWebhook replaced the caller's slice")
	}
	if _, err := c.UpdateWebhook(ctx, failing.Webhook.ID, client.UpdateWebhookRequest{}); refusalCode(err) != "bad_request" {
		t.Errorf("an empty edit: %v, want bad_request", err)
	}

	list, err := c.ListWebhooks(ctx)
	if err != nil || len(list.Webhooks) != 2 {
		t.Fatalf("ListWebhooks = %+v, %v", list, err)
	}
	for _, id := range []string{ok.Webhook.ID, failing.Webhook.ID} {
		if err := c.DeleteWebhook(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.DeleteWebhook(ctx, ok.Webhook.ID); !client.IsNotFound(err) {
		t.Errorf("deleting twice: %v, want not_found", err)
	}
}
