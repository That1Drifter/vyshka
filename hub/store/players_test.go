package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/That1Drifter/vyshka/hub/internal/id"
	"github.com/That1Drifter/vyshka/hub/store"
)

// playerOnly is a stand-in for the hub's reading of section 8.2: the one
// member "player", when it holds an identity.
func playerOnly(data json.RawMessage) []store.EventIdentity {
	var decoded struct {
		Player *struct {
			Platform string `json:"platform"`
			ID       string `json:"id"`
		} `json:"player"`
	}
	if json.Unmarshal(data, &decoded) != nil || decoded.Player == nil || decoded.Player.ID == "" {
		return nil
	}
	return []store.EventIdentity{{Platform: decoded.Player.Platform, ID: decoded.Player.ID, Roles: []string{"player"}}}
}

// Events stored before the identity index existed are indexed by the walk,
// batch by batch, up to the newest event of the migration and no further;
// the walk ends by deleting its progress row, and after that it is a no-op.
func TestBackfillIndexesEventsStoredBeforeTheIndex(t *testing.T) {
	ctx := context.Background()
	st := migrated(t)
	serverID := enrolledServer(t, st, "backfill")
	session := startSession(t, st, serverID, "backfill-token")

	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	for i := range 5 {
		// Ingested with no identities, as a hub before the index stored them.
		ingest(t, st, session.ID, event("core.player.connect", base.Add(time.Duration(i)*time.Second),
			`{"player":{"platform":"steam","id":"A"}}`))
	}
	ingest(t, st, session.ID, event("core.server.fps", base.Add(10*time.Second), `{"fps":60}`))
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO event_identity_backfill (id, after_id, through_id)
		 SELECT 1, '', (SELECT MAX(id) FROM events)`); err != nil {
		t.Fatalf("arm the backfill: %v", err)
	}
	// An event after the migration is indexed at ingest and lies past the
	// walk's end. Ids minted in one millisecond sort arbitrarily among
	// themselves, so an id minted in the boundary's millisecond can sort below
	// it and the walk would read it too (the ingest index absorbs that in
	// production, where the count is not asserted). The clock is first let
	// past that millisecond, read from the boundary id itself: an id's first
	// ten characters are its timestamp.
	var through string
	if err := st.DB().QueryRowContext(ctx, `SELECT through_id FROM event_identity_backfill`).Scan(&through); err != nil {
		t.Fatalf("read the armed boundary: %v", err)
	}
	for id.New()[:10] <= through[:10] {
		time.Sleep(100 * time.Microsecond)
	}
	later := event("core.player.disconnect", base.Add(20*time.Second), `{"player":{"platform":"steam","id":"A"}}`)
	later.Identities = playerOnly(later.Data)
	ingest(t, st, session.ID, later)

	walked, rounds := 0, 0
	for {
		rounds++
		if rounds > 10 {
			t.Fatal("the backfill never finished")
		}
		indexed, done, err := st.BackfillEventIdentities(ctx, 2, playerOnly)
		if err != nil {
			t.Fatalf("backfill: %v", err)
		}
		walked += indexed
		if done {
			break
		}
	}
	if walked != 6 {
		t.Errorf("the walk read %d events, want the six stored before it was armed", walked)
	}

	found, err := st.PlayerEvents(ctx, store.PlayerEventQuery{Platform: "steam", PlayerID: "A", Limit: 50})
	if err != nil {
		t.Fatalf("read the profile: %v", err)
	}
	if len(found) != 6 {
		t.Fatalf("the profile holds %d events, want the five backfilled and the one indexed at ingest", len(found))
	}
	if found[0].Type != "core.player.disconnect" || len(found[0].Roles) != 1 || found[0].Roles[0] != "player" {
		t.Errorf("the newest profile event is %s with roles %v, want the disconnect as player", found[0].Type, found[0].Roles)
	}

	indexed, done, err := st.BackfillEventIdentities(ctx, 2, playerOnly)
	if err != nil || !done || indexed != 0 {
		t.Errorf("a finished backfill answered %d, %v, %v; want 0, done, no error", indexed, done, err)
	}
}

// A pruned event takes its index rows with it, so the profile is exactly as
// deep as retention (spec section 8.6).
func TestPrunedEventsLeaveTheProfile(t *testing.T) {
	ctx := context.Background()
	st := migrated(t)
	serverID := enrolledServer(t, st, "profile prune")
	session := startSession(t, st, serverID, "profile-prune-token")

	gone := event("core.player.connect", time.Now().UTC(), `{"player":{"platform":"steam","id":"A"}}`)
	gone.Identities = playerOnly(gone.Data)
	gone.Retention = time.Millisecond
	kept := event("core.player.chat", time.Now().UTC(), `{"player":{"platform":"steam","id":"A"}}`)
	kept.Identities = playerOnly(kept.Data)
	ingest(t, st, session.ID, gone, kept)
	time.Sleep(20 * time.Millisecond)
	if _, err := st.PruneEvents(ctx, 100); err != nil {
		t.Fatalf("prune: %v", err)
	}
	found, err := st.PlayerEvents(ctx, store.PlayerEventQuery{Platform: "steam", PlayerID: "A", Limit: 50})
	if err != nil {
		t.Fatalf("read the profile: %v", err)
	}
	if len(found) != 1 || found[0].Type != "core.player.chat" {
		t.Errorf("after the prune the profile holds %+v, want the chat alone", found)
	}
}

// The per-identity note bound holds under concurrent writers: twenty racing
// for five slots land five notes and refuse fifteen. On SQLite the single
// connection makes this pass trivially; on Postgres it is the identity lock.
func TestNoteBoundHoldsUnderConcurrentWriters(t *testing.T) {
	ctx := context.Background()
	st := migrated(t)
	const bound, writers = 5, 20
	var wg sync.WaitGroup
	var stored, refused atomic.Int64
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := st.CreatePlayerNote(ctx, store.PlayerNote{
				ID: fmt.Sprintf("note-%02d", i), Platform: "steam", PlayerID: "A", Text: "x",
			}, bound)
			switch {
			case err == nil:
				stored.Add(1)
			case errors.Is(err, store.ErrNoteLimit):
				refused.Add(1)
			default:
				t.Errorf("writer %d: %v", i, err)
			}
		}()
	}
	wg.Wait()
	notes, err := st.PlayerNotes(ctx, "steam", "A", 100, store.NoteCursor{})
	if err != nil {
		t.Fatalf("read notes: %v", err)
	}
	if stored.Load() != bound || refused.Load() != writers-bound || len(notes) != bound {
		t.Errorf("stored %d, refused %d, holding %d; want %d, %d, %d",
			stored.Load(), refused.Load(), len(notes), bound, writers-bound, bound)
	}
}
