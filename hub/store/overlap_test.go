package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/That1Drifter/vyshka/hub/internal/dbtest"
)

// Deterministic overlap tests for the lock order documented at lockServer.
//
// Each test parks one transaction at a pause point (the classify callback of
// ApplyInbound, or one of the testHooks) while it starts a second transaction
// on the same server, and asserts two things: that the second one does not
// commit while the first is parked, and that once released it observes what
// the first committed. On Postgres that is the row locks at work; on SQLite
// it is the single connection. Either way the outcome must not depend on
// scheduling, which is what the pause point buys over a bare goroutine race.
//
// These tests set package-level hooks, so none of them runs in parallel.

// gate is one pause point. The first transaction to reach it parks until the
// test releases it; later arrivals pass straight through, whether or not the
// first has been released yet. That second part is what makes "the other
// transaction did not commit while the first was parked" a claim about the
// locks: a second transaction that reached the same pause point would
// otherwise park on the gate and look blocked without being blocked.
type gate struct {
	once     sync.Once
	release  sync.Once
	reached  chan struct{}
	released chan struct{}
}

func newGate(t *testing.T) *gate {
	g := &gate{reached: make(chan struct{}), released: make(chan struct{})}
	// A fatal assertion must not leave the parked goroutine parked.
	t.Cleanup(g.doRelease)
	return g
}

func (g *gate) hook() func() {
	return func() {
		first := false
		g.once.Do(func() {
			first = true
			close(g.reached)
		})
		if first {
			<-g.released
		}
	}
}

func (g *gate) doRelease() { g.release.Do(func() { close(g.released) }) }

// awaitReached fails the test if no transaction parks at the gate in time.
func (g *gate) awaitReached(t *testing.T) {
	t.Helper()
	select {
	case <-g.reached:
	case <-time.After(10 * time.Second):
		t.Fatal("no transaction reached the pause point")
	}
}

// blockedWindow is how long a second transaction is watched for a commit that
// must not happen while the first is parked. It only bounds the false-pass
// direction: a correct implementation never commits inside it, so a longer
// window makes the test slower, never flakier.
const blockedWindow = 400 * time.Millisecond

// assertBlocked fails if done closes while the other transaction is parked.
func assertBlocked(t *testing.T, what string, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
		t.Fatalf("%s committed while the other transaction was still parked", what)
	case <-time.After(blockedWindow):
	}
}

// awaitDone fails if done does not close once the other transaction was released.
func awaitDone(t *testing.T, what string, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s never finished after the other transaction was released", what)
	}
}

func openMigrated(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	st, err := Open(ctx, dbtest.URL(t))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

func plantServer(t *testing.T, st *Store, name string) string {
	t.Helper()
	ctx := context.Background()
	server, err := st.CreateServer(ctx, name, "test-game")
	if err != nil {
		t.Fatalf("create server: %v", err)
	}
	if _, err := st.IssueEnrollmentToken(ctx, server.ID, "enroll-"+server.ID, time.Hour); err != nil {
		t.Fatalf("issue enrollment token: %v", err)
	}
	if _, err := st.Enroll(ctx, "enroll-"+server.ID, "test-game", "secret-"+server.ID, PluginInfo{}); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	return server.ID
}

func plantSession(t *testing.T, st *Store, serverID, tokenHash string) Session {
	t.Helper()
	session, err := st.StartSession(context.Background(), NewSession{
		ServerID: serverID, TokenHash: tokenHash, TTL: time.Hour,
		ProtocolVersion: 1, PollTimeoutSeconds: 25,
	})
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	return session
}

func countRows(t *testing.T, st *Store, query string, args ...any) int {
	t.Helper()
	var count int
	if err := st.DB().QueryRowContext(context.Background(), query, args...).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	return count
}

// Two inbound applications on one session classify against committed state:
// the second sees the first's ack, so it can neither lower it nor count the
// same envelopes twice (spec section 9.1).
func TestOverlapInboundApplicationsSeeEachOthersCommit(t *testing.T) {
	ctx := context.Background()
	st := openMigrated(t)
	serverID := plantServer(t, st, "overlap inbound")
	session := plantSession(t, st, serverID, "overlap-inbound")

	first := newGate(t)
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		_, err := st.ApplyInbound(ctx, session.ID, func(ack int64) InboundApplication {
			first.hook()()
			return InboundApplication{Ack: 2, Accepted: 2}
		}, 100)
		if err != nil {
			t.Errorf("first apply: %v", err)
		}
	}()
	first.awaitReached(t)

	var (
		observed   int64 = -1
		secondDone       = make(chan struct{})
	)
	go func() {
		defer close(secondDone)
		_, err := st.ApplyInbound(ctx, session.ID, func(ack int64) InboundApplication {
			observed = ack
			// What a poll carrying envelopes 1..3 would do: accept what is
			// above the committed ack. Against a stale 0 it would accept
			// three and land the double count the section forbids.
			return InboundApplication{Ack: 3, Accepted: int(3 - ack)}
		}, 100)
		if err != nil {
			t.Errorf("second apply: %v", err)
		}
	}()
	assertBlocked(t, "the second application", secondDone)

	first.doRelease()
	awaitDone(t, "the first application", firstDone)
	awaitDone(t, "the second application", secondDone)

	if observed != 2 {
		t.Errorf("the second classifier saw ack %d, want the committed 2", observed)
	}
	var ack, count int64
	if err := st.DB().QueryRow(`SELECT inbound_ack, inbound_count FROM sessions WHERE id = ?`,
		session.ID).Scan(&ack, &count); err != nil {
		t.Fatalf("read session: %v", err)
	}
	if ack != 3 || count != 3 {
		t.Errorf("session ended at ack %d over %d envelopes, want 3 over 3", ack, count)
	}
}

// Supersession and revocation contend with a session-scoped write under the
// lock order: the write commits before the session ends, or observes the
// ended session; never after on a stale liveness decision.
func TestOverlapSessionEndWaitsForTheSessionWrite(t *testing.T) {
	ctx := context.Background()
	for name, end := range map[string]func(*Store, string) error{
		"supersession": func(st *Store, serverID string) error {
			_, err := st.StartSession(ctx, NewSession{ServerID: serverID, TokenHash: "successor-" + serverID,
				TTL: time.Hour, ProtocolVersion: 1, PollTimeoutSeconds: 25})
			return err
		},
		"revocation": func(st *Store, serverID string) error {
			return st.RevokeCredentials(ctx, serverID)
		},
	} {
		t.Run(name, func(t *testing.T) {
			st := openMigrated(t)
			serverID := plantServer(t, st, "overlap end "+name)
			session := plantSession(t, st, serverID, "overlap-end-"+name)

			write := newGate(t)
			writeDone := make(chan struct{})
			var writeErr error
			go func() {
				defer close(writeDone)
				_, writeErr = st.ApplyInbound(ctx, session.ID, func(int64) InboundApplication {
					write.hook()()
					return InboundApplication{Ack: 1, Accepted: 1}
				}, 100)
			}()
			write.awaitReached(t)

			endDone := make(chan struct{})
			var endErr error
			go func() {
				defer close(endDone)
				endErr = end(st, serverID)
			}()
			assertBlocked(t, name, endDone)

			write.doRelease()
			awaitDone(t, "the session write", writeDone)
			awaitDone(t, name, endDone)
			if writeErr != nil || endErr != nil {
				t.Fatalf("write err = %v, %s err = %v", writeErr, name, endErr)
			}

			// The write landed, on the session that was live when it ran.
			var ack int64
			if err := st.DB().QueryRow(`SELECT inbound_ack FROM sessions WHERE id = ?`, session.ID).Scan(&ack); err != nil {
				t.Fatalf("read session: %v", err)
			}
			if ack != 1 {
				t.Errorf("inbound_ack = %d after the write committed, want 1", ack)
			}
			// And nothing lands on it now: the ended session is not found.
			_, err := st.ApplyInbound(ctx, session.ID, func(int64) InboundApplication {
				return InboundApplication{Ack: 2, Accepted: 1}
			}, 100)
			if !errors.Is(err, ErrNotFound) {
				t.Errorf("a write on the ended session returned %v, want ErrNotFound", err)
			}
			if live := countRows(t, st, `SELECT COUNT(*) FROM sessions WHERE server_id = ? AND ended_at IS NULL`, serverID); live > 1 {
				t.Errorf("%d live sessions after %s, want at most 1", live, name)
			}
		})
	}
}

// Concurrent queue inserts cannot exceed the bound (spec section 9.2): the
// second insert waits for the first and then sees a full queue.
func TestOverlapQueueInsertsRespectTheBound(t *testing.T) {
	ctx := context.Background()
	st := openMigrated(t)
	serverID := plantServer(t, st, "overlap queue")

	count := newGate(t)
	testHooks.afterQueueCount = count.hook()
	t.Cleanup(func() { testHooks.afterQueueCount = nil })

	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		if _, err := st.QueueEnvelope(ctx, serverID, "test.first", []byte(`{}`), 1); err != nil {
			t.Errorf("first queue: %v", err)
		}
	}()
	count.awaitReached(t)

	secondDone := make(chan struct{})
	var secondErr error
	go func() {
		defer close(secondDone)
		_, secondErr = st.QueueEnvelope(ctx, serverID, "test.second", []byte(`{}`), 1)
	}()
	assertBlocked(t, "the second queue insert", secondDone)

	count.doRelease()
	awaitDone(t, "the first queue insert", firstDone)
	awaitDone(t, "the second queue insert", secondDone)

	if !errors.Is(secondErr, ErrOutboundQueueFull) {
		t.Errorf("second queue err = %v, want ErrOutboundQueueFull", secondErr)
	}
	if pending := countRows(t, st, `SELECT COUNT(*) FROM outbound_envelopes WHERE server_id = ? AND acked_at IS NULL`, serverID); pending != 1 {
		t.Errorf("%d envelopes pending against a bound of 1", pending)
	}
}

// Concurrent session starts leave exactly one unended session (spec section
// 5.3), and the schema refuses a second one if the code ever lets it through.
func TestOverlapSessionStartsLeaveOneLiveSession(t *testing.T) {
	ctx := context.Background()
	st := openMigrated(t)
	serverID := plantServer(t, st, "overlap sessions")

	ended := newGate(t)
	testHooks.afterSessionsEnded = ended.hook()
	t.Cleanup(func() { testHooks.afterSessionsEnded = nil })

	start := func(token string, done chan struct{}, out *Session, outErr *error) {
		defer close(done)
		*out, *outErr = st.StartSession(ctx, NewSession{ServerID: serverID, TokenHash: token,
			TTL: time.Hour, ProtocolVersion: 1, PollTimeoutSeconds: 25})
	}
	var (
		first, second       Session
		firstErr, secondErr error
		firstDone           = make(chan struct{})
		secondDone          = make(chan struct{})
	)
	go start("overlap-first", firstDone, &first, &firstErr)
	ended.awaitReached(t)
	go start("overlap-second", secondDone, &second, &secondErr)
	assertBlocked(t, "the second session start", secondDone)

	ended.doRelease()
	awaitDone(t, "the first session start", firstDone)
	awaitDone(t, "the second session start", secondDone)
	if firstErr != nil || secondErr != nil {
		t.Fatalf("start errs = %v, %v", firstErr, secondErr)
	}

	if live := countRows(t, st, `SELECT COUNT(*) FROM sessions WHERE server_id = ? AND ended_at IS NULL`, serverID); live != 1 {
		t.Fatalf("%d live sessions, want exactly 1", live)
	}
	current, err := st.LiveSession(ctx, serverID)
	if err != nil || current == nil {
		t.Fatalf("live session = %v, %v", current, err)
	}
	if current.ID != second.ID {
		t.Errorf("the live session is %s, want the later start %s (first was %s)", current.ID, second.ID, first.ID)
	}

	// The backstop: a second unended row for the server is a constraint
	// violation whatever wrote it.
	now := formatTime(time.Now().UTC())
	if _, err := st.DB().Exec(
		`INSERT INTO sessions (id, token_hash, server_id, created_at, expires_at, protocol_version, poll_timeout_seconds)
		 VALUES (?, ?, ?, ?, ?, 1, 25)`,
		"overlap-raw", "overlap-raw-token", serverID, now, formatTime(time.Now().Add(time.Hour).UTC()),
	); err == nil {
		t.Error("a second unended session row was accepted; the partial unique index of migration 0013 is missing")
	} else if !strings.Contains(strings.ToLower(err.Error()), "unique") {
		// SQLite says "UNIQUE constraint failed", Postgres "violates unique
		// constraint"; any other refusal is not the index doing its job.
		t.Errorf("the second unended session row was refused for the wrong reason: %v", err)
	}
}

// Concurrent enrollment-token issuance leaves exactly one unused token, so
// only the most recently issued one can enroll.
func TestOverlapTokenIssuanceLeavesOneUnusedToken(t *testing.T) {
	ctx := context.Background()
	st := openMigrated(t)
	server, err := st.CreateServer(ctx, "overlap tokens", "test-game")
	if err != nil {
		t.Fatalf("create server: %v", err)
	}

	cleared := newGate(t)
	testHooks.afterTokensCleared = cleared.hook()
	t.Cleanup(func() { testHooks.afterTokensCleared = nil })

	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		if _, err := st.IssueEnrollmentToken(ctx, server.ID, "overlap-token-1", time.Hour); err != nil {
			t.Errorf("first issue: %v", err)
		}
	}()
	cleared.awaitReached(t)

	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		if _, err := st.IssueEnrollmentToken(ctx, server.ID, "overlap-token-2", time.Hour); err != nil {
			t.Errorf("second issue: %v", err)
		}
	}()
	assertBlocked(t, "the second issuance", secondDone)

	cleared.doRelease()
	awaitDone(t, "the first issuance", firstDone)
	awaitDone(t, "the second issuance", secondDone)

	if unused := countRows(t, st, `SELECT COUNT(*) FROM enrollment_tokens WHERE server_id = ? AND used_at IS NULL`, server.ID); unused != 1 {
		t.Errorf("%d unused enrollment tokens, want exactly 1", unused)
	}
	if _, err := st.Enroll(ctx, "overlap-token-1", "test-game", "secret", PluginInfo{}); !errors.Is(err, ErrEnrollmentTokenInvalid) {
		t.Errorf("the earlier token enrolled with err = %v, want ErrEnrollmentTokenInvalid", err)
	}
}

// Action dispatch cannot commit against a manifest revision replaced
// concurrently: whichever of the publish and the dispatch holds the server
// first commits first, and the dispatch is refused if the revision it was
// validated against is no longer the stored one.
func TestOverlapDispatchAndManifestPublishSerialize(t *testing.T) {
	ctx := context.Background()
	st := openMigrated(t)
	serverID := plantServer(t, st, "overlap dispatch")
	session := plantSession(t, st, serverID, "overlap-dispatch")

	publish := func(revision int64, pause func()) error {
		_, err := st.ApplyInbound(ctx, session.ID, func(ack int64) InboundApplication {
			if pause != nil {
				pause()
			}
			return InboundApplication{Ack: ack + 1, Accepted: 1,
				Manifests: []ManifestPublish{{Revision: revision, Body: json.RawMessage(`{}`)}}}
		}, 100)
		return err
	}
	dispatch := func(id string, revision int64) (bool, error) {
		_, created, err := st.DispatchAction(ctx, NewAction{
			ID: id, ServerID: serverID, Code: "test.thing", Params: json.RawMessage(`{}`),
			ExpiresAt: time.Now().Add(time.Hour), ManifestRevision: revision,
			EnvelopeType: "action.dispatch", EnvelopeBody: json.RawMessage(`{}`), QueueLimit: 100,
		})
		return created, err
	}
	if err := publish(1, nil); err != nil {
		t.Fatalf("publish 1: %v", err)
	}

	// Publish first: the dispatch validated against revision 1 waits, then
	// finds revision 2 stored and is refused.
	t.Run("publish then dispatch", func(t *testing.T) {
		publishing := newGate(t)
		publishDone := make(chan struct{})
		go func() {
			defer close(publishDone)
			if err := publish(2, publishing.hook()); err != nil {
				t.Errorf("publish 2: %v", err)
			}
		}()
		publishing.awaitReached(t)

		dispatchDone := make(chan struct{})
		var dispatchErr error
		go func() {
			defer close(dispatchDone)
			_, dispatchErr = dispatch("overlap-action-1", 1)
		}()
		assertBlocked(t, "the dispatch", dispatchDone)

		publishing.doRelease()
		awaitDone(t, "the publish", publishDone)
		awaitDone(t, "the dispatch", dispatchDone)
		if !errors.Is(dispatchErr, ErrManifestChanged) {
			t.Errorf("dispatch err = %v, want ErrManifestChanged", dispatchErr)
		}
		if actions := countRows(t, st, `SELECT COUNT(*) FROM actions WHERE server_id = ?`, serverID); actions != 0 {
			t.Errorf("%d actions stored against a replaced manifest, want 0", actions)
		}
	})

	// Dispatch first: the publish waits for the dispatch to commit against the
	// revision it checked, then replaces it.
	t.Run("dispatch then publish", func(t *testing.T) {
		checked := newGate(t)
		testHooks.afterManifestChecked = checked.hook()
		t.Cleanup(func() { testHooks.afterManifestChecked = nil })

		dispatchDone := make(chan struct{})
		go func() {
			defer close(dispatchDone)
			created, err := dispatch("overlap-action-2", 2)
			if err != nil || !created {
				t.Errorf("dispatch = (%v, %v), want (true, nil)", created, err)
			}
		}()
		checked.awaitReached(t)

		publishDone := make(chan struct{})
		go func() {
			defer close(publishDone)
			if err := publish(3, nil); err != nil {
				t.Errorf("publish 3: %v", err)
			}
		}()
		assertBlocked(t, "the publish", publishDone)

		checked.doRelease()
		awaitDone(t, "the dispatch", dispatchDone)
		awaitDone(t, "the publish", publishDone)

		manifest, err := st.Manifest(ctx, serverID)
		if err != nil || manifest.Revision != 3 {
			t.Errorf("manifest = revision %d, %v, want 3", manifest.Revision, err)
		}
		if actions := countRows(t, st, `SELECT COUNT(*) FROM actions WHERE server_id = ?`, serverID); actions != 1 {
			t.Errorf("%d actions stored, want the 1 dispatched before the publish", actions)
		}
	})
}

// Concurrent incrs on one key lose no delta (spec section 12.2). There is no
// pause point in KVIncr, so this one is a plain race; a correct backend
// passes it every time, and a wrong one with a pool of sixteen connections
// fails it almost every time.
func TestOverlapKVIncrsLoseNoDelta(t *testing.T) {
	ctx := context.Background()
	st := openMigrated(t)

	const writers = 24
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := st.KVIncr(ctx, "overlap", "counter", 1); err != nil {
				t.Errorf("incr: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	entry, err := st.KVGet(ctx, "overlap", "counter")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(entry.Value) != "24" || entry.Revision != writers {
		t.Errorf("counter = %s at revision %d after %d incrs, want 24 at 24", entry.Value, entry.Revision, writers)
	}
}

// Two hubs migrating one fresh database at once: one applies everything, the
// other waits on the advisory lock and applies nothing, and the ledger holds
// each version once. Postgres only: two hub processes on one SQLite file is
// not a supported deployment, and there the second process contends on the
// file lock rather than on anything this package arranges.
func TestOverlapConcurrentMigratorsAreSerialized(t *testing.T) {
	if dbtest.Backend() != "postgres" {
		t.Skip("grades the Postgres migration lock; SQLite hubs do not share a file")
	}
	ctx := context.Background()
	url := dbtest.URL(t)

	stores := make([]*Store, 2)
	for i := range stores {
		st, err := Open(ctx, url)
		if err != nil {
			t.Fatalf("open store %d: %v", i, err)
		}
		t.Cleanup(func() { st.Close() })
		stores[i] = st
	}

	applied := make([][]int, len(stores))
	errs := make([]error, len(stores))
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, st := range stores {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			applied[i], errs[i] = st.Migrate(ctx)
		}()
	}
	close(start)
	wg.Wait()

	expected, err := Migrations()
	if err != nil {
		t.Fatalf("migrations: %v", err)
	}
	total := 0
	for i := range stores {
		if errs[i] != nil {
			t.Errorf("migrator %d: %v", i, errs[i])
		}
		total += len(applied[i])
	}
	if total != len(expected) {
		t.Errorf("the two migrators applied %d versions between them, want %d", total, len(expected))
	}
	// One did all the work and the other none: the lock is what makes the
	// second run find every version recorded, rather than the two splitting
	// the list between them.
	if len(applied[0]) != 0 && len(applied[1]) != 0 {
		t.Errorf("both migrators applied versions (%v and %v); the lock did not serialize them", applied[0], applied[1])
	}
	if rows := countRows(t, stores[0], `SELECT COUNT(*) FROM schema_migrations`); rows != len(expected) {
		t.Errorf("schema_migrations holds %d rows, want %d", rows, len(expected))
	}
}

// The session row lock on its own, apart from the server lock: NextOutbound
// takes no server lock, so a supersession that could commit while it is
// parked would be a session-lock defect the ApplyInbound cases cannot see.
// The numbering must land on the session that was live when it read, and
// the successor must then find the envelope and renumber it.
func TestOverlapSupersessionWaitsForNumbering(t *testing.T) {
	ctx := context.Background()
	st := openMigrated(t)
	serverID := plantServer(t, st, "overlap numbering")
	session := plantSession(t, st, serverID, "overlap-numbering")
	if _, err := st.QueueEnvelope(ctx, serverID, "test.thing", []byte(`{}`), 100); err != nil {
		t.Fatalf("queue: %v", err)
	}

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

	endDone := make(chan struct{})
	var successor Session
	var endErr error
	go func() {
		defer close(endDone)
		successor, endErr = st.StartSession(ctx, NewSession{ServerID: serverID, TokenHash: "overlap-numbering-2",
			TTL: time.Hour, ProtocolVersion: 1, PollTimeoutSeconds: 25})
	}()
	assertBlocked(t, "the supersession", endDone)

	read.doRelease()
	awaitDone(t, "the numbering", numberDone)
	awaitDone(t, "the supersession", endDone)
	if numberErr != nil || endErr != nil {
		t.Fatalf("number err = %v, end err = %v", numberErr, endErr)
	}
	if len(numbered) != 1 || numbered[0].Seq != 1 {
		t.Fatalf("numbered = %+v, want one envelope at seq 1", numbered)
	}
	again, _, _, err := st.NextOutbound(ctx, successor.ID, serverID, 10)
	if err != nil || len(again) != 1 || again[0].Seq != 1 || again[0].ID != numbered[0].ID {
		t.Errorf("successor read %+v, %v; want the same envelope renumbered to 1", again, err)
	}
}

// The expiry sweep retires the unnumbered dispatch envelope of an expired
// action. If it could do so between a poll reading that envelope and
// numbering it, the seq would be spent on a row that no longer exists: a gap
// in the session's sequence space the plugin's contiguous ack could never
// cross. The read locks the rows, so the sweep waits and then finds the
// envelope numbered, which it leaves alone.
func TestOverlapExpirySweepWaitsForNumbering(t *testing.T) {
	ctx := context.Background()
	st := openMigrated(t)
	serverID := plantServer(t, st, "overlap expiry")
	session := plantSession(t, st, serverID, "overlap-expiry")
	if _, err := st.ApplyInbound(ctx, session.ID, func(ack int64) InboundApplication {
		return InboundApplication{Ack: ack + 1, Accepted: 1,
			Manifests: []ManifestPublish{{Revision: 1, Body: json.RawMessage(`{}`)}}}
	}, 100); err != nil {
		t.Fatalf("publish: %v", err)
	}
	// Already past its deadline when the sweep looks, but not yet numbered.
	if _, _, err := st.DispatchAction(ctx, NewAction{
		ID: "overlap-expired", ServerID: serverID, Code: "test.thing", Params: json.RawMessage(`{}`),
		ExpiresAt: time.Now().Add(-time.Second), ManifestRevision: 1,
		EnvelopeType: "action.dispatch", EnvelopeBody: json.RawMessage(`{}`), QueueLimit: 100,
	}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

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

	sweepDone := make(chan struct{})
	var sweepErr error
	go func() {
		defer close(sweepDone)
		_, sweepErr = st.ExpireActions(ctx)
	}()
	assertBlocked(t, "the expiry sweep", sweepDone)

	read.doRelease()
	awaitDone(t, "the numbering", numberDone)
	awaitDone(t, "the expiry sweep", sweepDone)
	if numberErr != nil || sweepErr != nil {
		t.Fatalf("number err = %v, sweep err = %v", numberErr, sweepErr)
	}
	if len(numbered) != 1 || numbered[0].Seq != 1 {
		t.Fatalf("numbered = %+v, want the dispatch envelope at seq 1", numbered)
	}
	// The numbered envelope survives the sweep and is retransmitted: no gap.
	if pending := countRows(t, st, `SELECT COUNT(*) FROM outbound_envelopes WHERE server_id = ? AND seq = 1 AND acked_at IS NULL`, serverID); pending != 1 {
		t.Errorf("seq 1 holds %d envelopes after the sweep, want 1: the sequence has a gap", pending)
	}
	action, err := st.ActionByID(ctx, "overlap-expired")
	if err != nil || action.State != ActionExpired {
		t.Errorf("action = %s, %v; want expired", action.State, err)
	}
}

// A delete cannot slip between a compare-and-swap's read and its write: under
// the key lock it lands before the read (the swap then fails) or after the
// write (the key is then gone). Here it is the second; a delete that ran in
// between would let the swap report success and leave the key standing.
func TestOverlapKVDeleteWaitsForTheSwap(t *testing.T) {
	ctx := context.Background()
	st := openMigrated(t)
	if _, err := st.KVSet(ctx, "overlap", "cas", []byte(`1`), nil, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}

	read := newGate(t)
	testHooks.afterKVRead = read.hook()
	t.Cleanup(func() { testHooks.afterKVRead = nil })

	swapDone := make(chan struct{})
	var swapErr error
	go func() {
		defer close(swapDone)
		one := int64(1)
		_, swapErr = st.KVSet(ctx, "overlap", "cas", []byte(`2`), &one, nil)
	}()
	read.awaitReached(t)

	deleteDone := make(chan struct{})
	var deleteErr error
	go func() {
		defer close(deleteDone)
		deleteErr = st.KVDelete(ctx, "overlap", "cas")
	}()
	assertBlocked(t, "the delete", deleteDone)

	read.doRelease()
	awaitDone(t, "the swap", swapDone)
	awaitDone(t, "the delete", deleteDone)
	if swapErr != nil || deleteErr != nil {
		t.Fatalf("swap err = %v, delete err = %v", swapErr, deleteErr)
	}
	if _, err := st.KVGet(ctx, "overlap", "cas"); !errors.Is(err, ErrNotFound) {
		t.Errorf("after set-then-delete the key reads %v, want ErrNotFound", err)
	}
}

// The retention pass cannot delete a key a writer refreshed while the pass
// waited on its row: the delete re-tests expiry on the committed row.
func TestOverlapPruneKVSparesARefreshedKey(t *testing.T) {
	ctx := context.Background()
	st := openMigrated(t)
	ttl := time.Millisecond
	if _, err := st.KVSet(ctx, "overlap", "refresh", []byte(`1`), nil, &ttl); err != nil {
		t.Fatalf("seed: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, err := st.KVGet(ctx, "overlap", "refresh"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("seed still live: %v", err)
	}

	written := newGate(t)
	testHooks.afterKVWrite = written.hook()
	t.Cleanup(func() { testHooks.afterKVWrite = nil })

	setDone := make(chan struct{})
	var setErr error
	go func() {
		defer close(setDone)
		zero := int64(0)
		// Recreates the expired key without a TTL, holding its row until
		// released.
		_, setErr = st.KVSet(ctx, "overlap", "refresh", []byte(`2`), &zero, nil)
	}()
	written.awaitReached(t)

	pruneDone := make(chan struct{})
	var pruneErr error
	go func() {
		defer close(pruneDone)
		_, pruneErr = st.PruneKV(ctx, 10)
	}()
	assertBlocked(t, "the prune", pruneDone)

	written.doRelease()
	awaitDone(t, "the set", setDone)
	awaitDone(t, "the prune", pruneDone)
	if setErr != nil || pruneErr != nil {
		t.Fatalf("set err = %v, prune err = %v", setErr, pruneErr)
	}
	entry, err := st.KVGet(ctx, "overlap", "refresh")
	if err != nil || string(entry.Value) != "2" || entry.ExpiresAt != nil {
		t.Errorf("after the prune the refreshed key reads %+v, %v; want value 2 with no expiry", entry, err)
	}
}
