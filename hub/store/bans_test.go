package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"
)

func plantBan(t *testing.T, st *Store, playerID string) BanChange {
	t.Helper()
	change, err := st.CreateBan(context.Background(), NewBan{
		ID: "ban-" + playerID + "-" + fmt.Sprint(time.Now().UnixNano()), Platform: "steam", PlayerID: playerID,
		Reason: "r", TokenName: "test",
	}, 100)
	if err != nil {
		t.Fatalf("ban %s: %v", playerID, err)
	}
	return change
}

func listedIDs(bans []Ban) string {
	ids := ""
	for _, ban := range bans {
		ids += ban.PlayerID
	}
	return ids
}

// A walk reads the list as it stood at its revision (spec section 13.3):
// every past revision is served exactly, and a revision the store has not
// reached is refused.
func TestBanListAtServesPastRevisions(t *testing.T) {
	ctx := context.Background()
	st := openMigrated(t)
	revisions := []int64{0}
	for _, playerID := range []string{"C", "A", "B"} {
		revisions = append(revisions, plantBan(t, st, playerID).Revision)
	}
	bans, err := st.Bans(ctx, BanQuery{Now: time.Now().UTC(), Platform: "steam", PlayerID: "B", Limit: 10})
	if err != nil || len(bans) != 1 {
		t.Fatalf("B's ban: %v, %v", bans, err)
	}
	lifted, err := st.LiftBan(ctx, bans[0].ID, "", "test", 100)
	if err != nil {
		t.Fatalf("lift: %v", err)
	}
	revisions = append(revisions, lifted.Revision, plantBan(t, st, "D").Revision)
	for i := 1; i < len(revisions); i++ {
		if revisions[i] <= revisions[i-1] {
			t.Fatalf("revisions %v do not increase", revisions)
		}
	}

	for i, want := range []string{"", "C", "AC", "ABC", "AC", "ACD"} {
		served, page, err := st.BanListAt(ctx, revisions[i], BanListPosition{}, 10)
		if err != nil || served != revisions[i] || listedIDs(page) != want {
			t.Errorf("revision %d served %d %q, %v; want %q", revisions[i], served, listedIDs(page), err, want)
		}
	}
	current := revisions[len(revisions)-1]
	served, page, err := st.BanListAt(ctx, -1, BanListPosition{Platform: "steam", PlayerID: "A"}, 1)
	if err != nil || served != current || listedIDs(page) != "C" {
		t.Errorf("the current revision after A served %d %q, %v; want C at %d", served, listedIDs(page), err, current)
	}
	if _, _, err := st.BanListAt(ctx, current+1, BanListPosition{}, 10); !errors.Is(err, ErrBanRevisionGone) {
		t.Errorf("a revision above the current one answered %v, want ErrBanRevisionGone", err)
	}
	// A revision between two this hub minted is one it never handed out: a
	// cursor naming it comes from another history (section 13.3).
	if revisions[2]-revisions[1] > 1 {
		if _, _, err := st.BanListAt(ctx, revisions[1]+1, BanListPosition{}, 10); !errors.Is(err, ErrBanRevisionGone) {
			t.Errorf("a revision this hub never minted answered %v, want ErrBanRevisionGone", err)
		}
	}
}

// A hub restored from a backup does not hand out a revision again for a
// different list (section 13.1): the next change lands above everything the
// hub minted before the restore, because revisions are floored at the clock.
func TestBanRevisionsDoNotRepeatAcrossARestore(t *testing.T) {
	ctx := context.Background()
	st := openMigrated(t)
	before := plantBan(t, st, "A").Revision
	lost := plantBan(t, st, "B").Revision
	// The restore: the list and the register go back to where they stood
	// after the first ban, as a database restored from that moment would.
	if _, err := st.DB().ExecContext(ctx, `DELETE FROM bans WHERE player_id = ?`, "B"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx, `DELETE FROM ban_revisions WHERE revision > ?`, before); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE ban_list SET revision = ?`, before); err != nil {
		t.Fatal(err)
	}
	after := plantBan(t, st, "C").Revision
	if after <= lost {
		t.Errorf("after a restore to %d, the next change took revision %d, not above the %d handed out before the restore", before, after, lost)
	}
	if _, _, err := st.BanListAt(ctx, lost, BanListPosition{}, 10); !errors.Is(err, ErrBanRevisionGone) {
		t.Errorf("a cursor naming the pre-restore revision %d answered %v, want ErrBanRevisionGone", lost, err)
	}
}

// Concurrent changes are distinct revisions, none lost, and concurrent bans
// of one identity leave one active ban (spec section 13.1).
func TestOverlapBanChangesAreSerialized(t *testing.T) {
	ctx := context.Background()
	st := openMigrated(t)

	const writers = 24
	var wg sync.WaitGroup
	start := make(chan struct{})
	revisions := make(chan int64, writers)
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			change, err := st.CreateBan(ctx, NewBan{
				ID: fmt.Sprintf("distinct-%02d", i), Platform: "steam", PlayerID: fmt.Sprintf("P%02d", i),
				Reason: "r", TokenName: "test",
			}, 100)
			if err != nil {
				t.Errorf("ban %d: %v", i, err)
				return
			}
			revisions <- change.Revision
		}()
	}
	close(start)
	wg.Wait()
	close(revisions)
	var got []int64
	for revision := range revisions {
		got = append(got, revision)
	}
	slices.Sort(got)
	if len(got) != writers || len(slices.Compact(slices.Clone(got))) != writers {
		t.Fatalf("concurrent bans took revisions %v, want %d distinct ones", got, writers)
	}
	if current, err := st.BanRevision(ctx); err != nil || current != got[len(got)-1] {
		t.Errorf("revision after %d bans = %d, %v; want the highest taken, %d", writers, current, err, got[len(got)-1])
	}

	start = make(chan struct{})
	var won, conflicted int
	var mu sync.Mutex
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := st.CreateBan(ctx, NewBan{
				ID: fmt.Sprintf("same-%02d", i), Platform: "steam", PlayerID: "SAME", Reason: "r", TokenName: "test",
			}, 100)
			var active *BanActiveError
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				won++
			case errors.As(err, &active):
				conflicted++
			default:
				t.Errorf("ban of SAME: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if won != 1 || conflicted != writers-1 {
		t.Errorf("concurrent bans of one identity: %d succeeded and %d conflicted, want 1 and %d", won, conflicted, writers-1)
	}
}

// A manifest declaring the capability, accepted while a change of the list is
// between choosing its servers and committing, waits for the change and
// queues a notice carrying its revision (section 13.3): without the ban
// list's lock the manifest would commit first with the old revision, the
// change would reach nobody, and the plugin would hold a list it never hears
// has moved on. Postgres only proves it; on SQLite the single connection
// serializes the two anyway.
func TestOverlapCapabilityAcceptanceWaitsForABanChange(t *testing.T) {
	ctx := context.Background()
	st := openMigrated(t)
	serverID := plantServer(t, st, "overlap capability")
	session := plantSession(t, st, serverID, "overlap-capability")

	chosen := newGate(t)
	testHooks.afterBanTargets = chosen.hook()
	t.Cleanup(func() { testHooks.afterBanTargets = nil })

	banDone := make(chan struct{})
	var change BanChange
	var banErr error
	go func() {
		defer close(banDone)
		change, banErr = st.CreateBan(ctx, NewBan{ID: "raced", Platform: "steam", PlayerID: "R", Reason: "r", TokenName: "test"}, 100)
	}()
	chosen.awaitReached(t)

	publishDone := make(chan struct{})
	var publishErr error
	go func() {
		defer close(publishDone)
		_, publishErr = st.ApplyInboundDeclaringBans(ctx, session.ID, func(ack int64) InboundApplication {
			return InboundApplication{Ack: ack + 1, Accepted: 1, Manifests: []ManifestPublish{{
				Revision: 1, Body: json.RawMessage(`{"capabilities":["bans"]}`), Capabilities: []string{"bans"},
			}}}
		}, 100)
	}()
	assertBlocked(t, "the capable manifest", publishDone)

	chosen.doRelease()
	awaitDone(t, "the ban", banDone)
	awaitDone(t, "the capable manifest", publishDone)
	if banErr != nil || publishErr != nil {
		t.Fatalf("ban err = %v, publish err = %v", banErr, publishErr)
	}
	queued, _, _, err := st.NextOutbound(ctx, session.ID, serverID, 10)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf(`{"revision":%d}`, change.Revision)
	found := false
	for _, envelope := range queued {
		if envelope.Type == BanChangedType && string(envelope.Body) == want {
			found = true
		}
	}
	if !found {
		t.Errorf("the server's queue holds %+v, want a bans.changed carrying %s", queued, want)
	}
}

// A bans.changed a poll has read and is numbering is sent as it was read: a
// change that lands meanwhile waits for the numbering and then queues a notice
// of its own rather than rewriting the one on its way to the plugin, which
// must stay byte-stable once sent (spec sections 9.1 and 13.3).
func TestOverlapBanNoticeRefreshWaitsForNumbering(t *testing.T) {
	ctx := context.Background()
	st := openMigrated(t)
	serverID := plantServer(t, st, "overlap ban notice")
	session := plantSession(t, st, serverID, "overlap-ban-notice")
	if _, err := st.ApplyInbound(ctx, session.ID, func(ack int64) InboundApplication {
		return InboundApplication{Ack: ack + 1, Accepted: 1, Manifests: []ManifestPublish{{
			Revision: 1, Body: json.RawMessage(`{"capabilities":["bans"]}`), Capabilities: []string{"bans"},
		}}}
	}, 100); err != nil {
		t.Fatalf("publish: %v", err)
	}
	first := plantBan(t, st, "A")

	read := newGate(t)
	testHooks.afterOutboundRead = read.hook()
	t.Cleanup(func() { testHooks.afterOutboundRead = nil })

	numberDone := make(chan struct{})
	var numbered []OutboundEnvelope
	var numberErr error
	go func() {
		defer close(numberDone)
		numbered, _, _, numberErr = st.NextOutbound(ctx, session.ID, serverID, 10)
	}()
	read.awaitReached(t)

	banDone := make(chan struct{})
	var second BanChange
	var banErr error
	go func() {
		defer close(banDone)
		second, banErr = st.CreateBan(ctx, NewBan{
			ID: "second", Platform: "steam", PlayerID: "B", Reason: "r", TokenName: "test",
		}, 100)
	}()
	assertBlocked(t, "the second ban", banDone)

	read.doRelease()
	awaitDone(t, "the numbering", numberDone)
	awaitDone(t, "the second ban", banDone)
	if numberErr != nil || banErr != nil {
		t.Fatalf("number err = %v, ban err = %v", numberErr, banErr)
	}
	firstBody := fmt.Sprintf(`{"revision":%d}`, first.Revision)
	if len(numbered) != 1 || numbered[0].Type != BanChangedType || string(numbered[0].Body) != firstBody {
		t.Fatalf("numbered = %+v, want the first notice at %s", numbered, firstBody)
	}
	again, _, _, err := st.NextOutbound(ctx, session.ID, serverID, 10)
	if err != nil || len(again) != 2 {
		t.Fatalf("after the second ban the queue holds %+v, %v; want the sent notice and a new one", again, err)
	}
	if again[0].ID != numbered[0].ID || string(again[0].Body) != firstBody {
		t.Errorf("the sent notice was rewritten to %s", again[0].Body)
	}
	if want := fmt.Sprintf(`{"revision":%d}`, second.Revision); string(again[1].Body) != want {
		t.Errorf("the new notice carries %s, want %s", again[1].Body, want)
	}
}
