package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/That1Drifter/vyshka/hub/store"
)

// ingest appends events through the same transaction the poll handler uses, so
// these tests exercise the durability path rather than a private shortcut.
func ingest(t *testing.T, st *store.Store, sessionID string, events ...store.NewEvent) store.InboundApplied {
	t.Helper()

	applied, err := st.ApplyInbound(context.Background(), sessionID,
		func(ack int64) store.InboundApplication {
			return store.InboundApplication{Ack: ack, Events: events}
		}, 100)
	if err != nil {
		t.Fatalf("ingest events: %v", err)
	}
	return applied
}

func event(eventType string, occurredAt time.Time, data string) store.NewEvent {
	prepared := store.NewEvent{
		Type:      eventType,
		Data:      json.RawMessage(data),
		Retention: time.Hour,
	}
	if !occurredAt.IsZero() {
		at := occurredAt.UTC()
		prepared.OccurredAt = &at
	}
	return prepared
}

// The load-bearing requirement of section 8.1: a custom event is stored and
// queried exactly like a core one. Nothing in the store may branch on the
// namespace, so the same query returns both.
func TestEventsTreatCoreAndCustomAlike(t *testing.T) {
	ctx := context.Background()
	st := migrated(t)
	serverID := enrolledServer(t, st, "events alike")
	session := startSession(t, st, serverID, "events-token")

	base := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	applied := ingest(t, st, session.ID,
		event("core.player.death", base, `{"weapon":"M4A1"}`),
		event("example-mod.raid.started", base.Add(time.Second), `{"territoryId":"t-19"}`),
	)
	if applied.EventsStored != 2 {
		t.Fatalf("EventsStored = %d, want 2", applied.EventsStored)
	}

	found, err := st.Events(ctx, store.EventQuery{ServerID: serverID, Limit: 10})
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if len(found) != 2 {
		t.Fatalf("read %d events, want 2", len(found))
	}
	// Newest first.
	if found[0].Type != "example-mod.raid.started" || found[1].Type != "core.player.death" {
		t.Errorf("feed order = %q, %q; want the custom event first, it is newer",
			found[0].Type, found[1].Type)
	}
	if !found[1].OccurredAt.Equal(base) {
		t.Errorf("occurredAt = %s, want the timestamp the plugin sent (%s)", found[1].OccurredAt, base)
	}
	if found[1].ReceivedAt.Before(base) == false && found[1].ReceivedAt.IsZero() {
		t.Error("receivedAt was not recorded")
	}
	if string(found[0].Data) != `{"territoryId":"t-19"}` {
		t.Errorf("data = %s, want what the plugin sent", found[0].Data)
	}
}

// An event with no usable timestamp takes receipt time, so the feed never has
// a hole where a game server's clock should have been (spec section 8.1).
func TestEventsSubstituteReceiptTimeForAMissingTimestamp(t *testing.T) {
	ctx := context.Background()
	st := migrated(t)
	serverID := enrolledServer(t, st, "events clockless")
	session := startSession(t, st, serverID, "clockless-token")

	before := time.Now().UTC().Add(-time.Second)
	ingest(t, st, session.ID, event("core.server.start", time.Time{}, `{}`))

	found, err := st.Events(ctx, store.EventQuery{ServerID: serverID, Limit: 10})
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("read %d events, want 1", len(found))
	}
	if found[0].OccurredAt.Before(before) {
		t.Errorf("occurredAt = %s, want receipt time (at or after %s)", found[0].OccurredAt, before)
	}
	if !found[0].OccurredAt.Equal(found[0].ReceivedAt) {
		t.Errorf("occurredAt = %s and receivedAt = %s; a substituted timestamp is receipt time",
			found[0].OccurredAt, found[0].ReceivedAt)
	}
}

// A namespace filter is a range scan, not a LIKE, because `_` is a LIKE
// wildcard and a legal namespace character. Without the range, the underscore
// namespace here would also match its lookalike.
func TestEventTypeFiltersDoNotLeakLikeWildcards(t *testing.T) {
	ctx := context.Background()
	st := migrated(t)
	serverID := enrolledServer(t, st, "events filters")
	session := startSession(t, st, serverID, "filters-token")

	now := time.Now().UTC().Truncate(time.Millisecond)
	ingest(t, st, session.ID,
		event("example_mod.raid.started", now, `{}`),
		event("exampleXmod.raid.started", now.Add(time.Millisecond), `{}`),
		event("core.player.death", now.Add(2*time.Millisecond), `{}`),
		event("core.player.chat", now.Add(3*time.Millisecond), `{}`),
		event("core.vehicle.spawn", now.Add(4*time.Millisecond), `{}`),
	)

	cases := []struct {
		name    string
		filters []store.EventTypeFilter
		want    []string
	}{
		{
			name:    "namespace prefix",
			filters: []store.EventTypeFilter{{Prefix: "example_mod."}},
			want:    []string{"example_mod.raid.started"},
		},
		{
			name:    "exact type",
			filters: []store.EventTypeFilter{{Exact: "core.player.chat"}},
			want:    []string{"core.player.chat"},
		},
		{
			name:    "terms are ORed",
			filters: []store.EventTypeFilter{{Prefix: "core.player."}, {Exact: "example_mod.raid.started"}},
			want:    []string{"core.player.chat", "core.player.death", "example_mod.raid.started"},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			found, err := st.Events(ctx, store.EventQuery{
				ServerID: serverID, Types: testCase.filters, Limit: 10,
			})
			if err != nil {
				t.Fatalf("read events: %v", err)
			}
			got := make([]string, 0, len(found))
			for _, one := range found {
				got = append(got, one.Type)
			}
			if len(got) != len(testCase.want) {
				t.Fatalf("matched %v, want %v", got, testCase.want)
			}
			for _, want := range testCase.want {
				matched := false
				for _, one := range got {
					if one == want {
						matched = true
					}
				}
				if !matched {
					t.Errorf("matched %v, missing %q", got, want)
				}
			}
		})
	}
}

// Pagination walks the whole feed exactly once, including across events that
// share a millisecond: that is what the id tiebreak in the cursor buys.
func TestEventPaginationVisitsEveryEventOnce(t *testing.T) {
	ctx := context.Background()
	st := migrated(t)
	serverID := enrolledServer(t, st, "events pagination")
	session := startSession(t, st, serverID, "pagination-token")

	sameInstant := time.Now().UTC().Truncate(time.Millisecond)
	events := make([]store.NewEvent, 0, 12)
	for range 12 {
		events = append(events, event("core.player.damage", sameInstant, `{}`))
	}
	ingest(t, st, session.ID, events...)

	seen := map[string]int{}
	cursor := store.EventCursor{}
	for page := range 10 {
		found, err := st.Events(ctx, store.EventQuery{
			ServerID: serverID, Limit: 5, After: cursor,
		})
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if len(found) == 0 {
			break
		}
		for _, one := range found {
			seen[one.ID]++
		}
		last := found[len(found)-1]
		cursor = store.EventCursor{OccurredAt: last.OccurredAt, ID: last.ID}
	}

	if len(seen) != 12 {
		t.Fatalf("pagination saw %d distinct events, want 12", len(seen))
	}
	for eventID, count := range seen {
		if count != 1 {
			t.Errorf("event %s came back %d times; a cursor must not repeat a row", eventID, count)
		}
	}
}

// since is inclusive and until exclusive, both on occurredAt.
func TestEventTimeBoundsAreHalfOpen(t *testing.T) {
	ctx := context.Background()
	st := migrated(t)
	serverID := enrolledServer(t, st, "events bounds")
	session := startSession(t, st, serverID, "bounds-token")

	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	ingest(t, st, session.ID,
		event("core.player.connect", base, `{}`),
		event("core.player.connect", base.Add(time.Minute), `{}`),
		event("core.player.connect", base.Add(2*time.Minute), `{}`),
	)

	found, err := st.Events(ctx, store.EventQuery{
		ServerID: serverID, Since: base, Until: base.Add(2 * time.Minute), Limit: 10,
	})
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if len(found) != 2 {
		t.Fatalf("matched %d events, want the 2 inside [since, until)", len(found))
	}
	for _, one := range found {
		if !one.OccurredAt.Before(base.Add(2 * time.Minute)) {
			t.Errorf("occurredAt = %s is not below the exclusive until bound", one.OccurredAt)
		}
	}
}

// Timestamps are stored to the millisecond, so a bound with finer precision has
// to round the way that keeps the window half-open. Truncating instead would let
// an inclusive since admit an event below it, and an exclusive until drop one it
// covers.
func TestEventTimeBoundsRoundToTheStoredResolution(t *testing.T) {
	ctx := context.Background()
	st := migrated(t)
	serverID := enrolledServer(t, st, "events sub-ms bounds")
	session := startSession(t, st, serverID, "sub-ms-token")

	at := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	ingest(t, st, session.ID, event("core.player.connect", at, `{}`))
	halfMilli := at.Add(500 * time.Microsecond)

	// since sits half a millisecond above the event, so the event is below the
	// inclusive bound and must not match.
	found, err := st.Events(ctx, store.EventQuery{ServerID: serverID, Since: halfMilli, Limit: 10})
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if len(found) != 0 {
		t.Errorf("since %s matched an event at %s, which is below it", halfMilli, at)
	}

	// until sits half a millisecond above the event, so the event is inside the
	// exclusive bound and must match.
	found, err = st.Events(ctx, store.EventQuery{ServerID: serverID, Until: halfMilli, Limit: 10})
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if len(found) != 1 {
		t.Errorf("until %s excluded an event at %s, which is below it", halfMilli, at)
	}
}

// A prune asked for no usable bound takes a default one rather than SQLite's
// reading of a negative LIMIT, which is no bound at all.
func TestPruneEventsRefusesAnUnusableBound(t *testing.T) {
	ctx := context.Background()
	st := migrated(t)
	serverID := enrolledServer(t, st, "events prune bound")
	session := startSession(t, st, serverID, "prune-bound-token")

	expired := event("core.server.fps", time.Now().UTC(), `{}`)
	expired.Retention = -time.Second
	ingest(t, st, session.ID, expired)

	pruned, err := st.PruneEvents(ctx, -1)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if pruned != 1 {
		t.Errorf("pruned %d, want the 1 expired event under the default bound", pruned)
	}
}

// An event query is scoped to its server. Another server's telemetry is not
// this operator's to read through this endpoint.
func TestEventsAreScopedToTheirServer(t *testing.T) {
	ctx := context.Background()
	st := migrated(t)
	first := enrolledServer(t, st, "events mine")
	second := enrolledServer(t, st, "events theirs")
	firstSession := startSession(t, st, first, "mine-token")
	secondSession := startSession(t, st, second, "theirs-token")

	now := time.Now().UTC().Truncate(time.Millisecond)
	ingest(t, st, firstSession.ID, event("core.player.death", now, `{"who":"mine"}`))
	ingest(t, st, secondSession.ID, event("core.player.death", now, `{"who":"theirs"}`))

	found, err := st.Events(ctx, store.EventQuery{ServerID: first, Limit: 10})
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if len(found) != 1 || string(found[0].Data) != `{"who":"mine"}` {
		t.Fatalf("server %s sees %d events (%v), want only its own", first, len(found), found)
	}
}

// Retention is the deadline stamped at ingest, and the prune pass is bounded so
// a neglected database drains over several passes rather than in one statement.
func TestPruneEventsRemovesOnlyExpiredRowsAndRespectsItsBound(t *testing.T) {
	ctx := context.Background()
	st := migrated(t)
	serverID := enrolledServer(t, st, "events retention")
	session := startSession(t, st, serverID, "retention-token")

	now := time.Now().UTC().Truncate(time.Millisecond)
	expiring := make([]store.NewEvent, 0, 4)
	for range 4 {
		one := event("core.server.fps", now, `{}`)
		// Already past its retention the moment it lands.
		one.Retention = -time.Second
		expiring = append(expiring, one)
	}
	kept := event("core.player.chat", now, `{"message":"hello"}`)
	kept.Retention = time.Hour
	ingest(t, st, session.ID, append(expiring, kept)...)

	pruned, err := st.PruneEvents(ctx, 3)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if pruned != 3 {
		t.Errorf("first pass pruned %d, want the bound of 3", pruned)
	}
	pruned, err = st.PruneEvents(ctx, 3)
	if err != nil {
		t.Fatalf("second prune: %v", err)
	}
	if pruned != 1 {
		t.Errorf("second pass pruned %d, want the remaining 1", pruned)
	}

	found, err := st.Events(ctx, store.EventQuery{ServerID: serverID, Limit: 10})
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if len(found) != 1 || found[0].Type != "core.player.chat" {
		t.Fatalf("after pruning the feed holds %v, want only the unexpired chat event", found)
	}
}

// Deleting a server takes its telemetry with it: the foreign key cascades, so
// an operator removing a server does not leave orphaned rows behind forever.
func TestEventsCascadeWithTheirServer(t *testing.T) {
	ctx := context.Background()
	st := migrated(t)
	serverID := enrolledServer(t, st, "events cascade")
	session := startSession(t, st, serverID, "cascade-token")

	ingest(t, st, session.ID, event("core.player.death", time.Now().UTC(), `{}`))
	if _, err := st.DB().ExecContext(ctx, `DELETE FROM servers WHERE id = ?`, serverID); err != nil {
		t.Fatalf("delete server: %v", err)
	}

	var remaining int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE server_id = ?`, serverID).Scan(&remaining); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if remaining != 0 {
		t.Errorf("%d events survived their server", remaining)
	}
}
