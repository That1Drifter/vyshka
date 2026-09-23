package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
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
	for _, playerID := range []string{"C", "A", "B"} {
		plantBan(t, st, playerID)
	}
	bans, err := st.Bans(ctx, BanQuery{Now: time.Now().UTC(), Platform: "steam", PlayerID: "B", Limit: 10})
	if err != nil || len(bans) != 1 {
		t.Fatalf("B's ban: %v, %v", bans, err)
	}
	if _, err := st.LiftBan(ctx, bans[0].ID, "", "test", 100); err != nil {
		t.Fatalf("lift: %v", err)
	}
	plantBan(t, st, "D")

	for revision, want := range map[int64]string{0: "", 1: "C", 2: "AC", 3: "ABC", 4: "AC", 5: "ACD"} {
		served, page, err := st.BanListAt(ctx, revision, BanListPosition{}, 10)
		if err != nil || served != revision || listedIDs(page) != want {
			t.Errorf("revision %d served %d %q, %v; want %q", revision, served, listedIDs(page), err, want)
		}
	}
	served, page, err := st.BanListAt(ctx, -1, BanListPosition{Platform: "steam", PlayerID: "A"}, 1)
	if err != nil || served != 5 || listedIDs(page) != "C" {
		t.Errorf("the current revision after A served %d %q, %v; want C at 5", served, listedIDs(page), err)
	}
	if _, _, err := st.BanListAt(ctx, 6, BanListPosition{}, 10); !errors.Is(err, ErrBanRevisionGone) {
		t.Errorf("a revision above the current one answered %v, want ErrBanRevisionGone", err)
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
	var got []int
	for revision := range revisions {
		got = append(got, int(revision))
	}
	sort.Ints(got)
	for i, revision := range got {
		if revision != i+1 {
			t.Fatalf("concurrent bans took revisions %v, want 1 to %d once each", got, writers)
		}
	}
	if current, err := st.BanRevision(ctx); err != nil || current != writers {
		t.Errorf("revision after %d bans = %d, %v", writers, current, err)
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
	plantBan(t, st, "A")

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
	if len(numbered) != 1 || numbered[0].Type != BanChangedType || string(numbered[0].Body) != `{"revision":1}` {
		t.Fatalf("numbered = %+v, want the first notice at revision 1", numbered)
	}
	again, _, _, err := st.NextOutbound(ctx, session.ID, serverID, 10)
	if err != nil || len(again) != 2 {
		t.Fatalf("after the second ban the queue holds %+v, %v; want the sent notice and a new one", again, err)
	}
	if again[0].ID != numbered[0].ID || string(again[0].Body) != `{"revision":1}` {
		t.Errorf("the sent notice was rewritten to %s", again[0].Body)
	}
	if want := fmt.Sprintf(`{"revision":%d}`, second.Revision); string(again[1].Body) != want {
		t.Errorf("the new notice carries %s, want %s", again[1].Body, want)
	}
}
