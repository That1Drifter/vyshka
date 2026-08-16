package hub_test

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/That1Drifter/vyshka/hub"
)

// wireEnvelope mirrors spec section 4 on the wire. It is spelled out here rather
// than shared with the hub so a change in the implementation shows up as a test
// failure instead of quietly redefining the contract.
type wireEnvelope struct {
	V    int             `json:"v"`
	ID   string          `json:"id"`
	Type string          `json:"type"`
	Seq  int64           `json:"seq"`
	TS   string          `json:"ts"`
	Body json.RawMessage `json:"body"`
}

type pollResult struct {
	Envelopes          []wireEnvelope `json:"envelopes"`
	Ack                int64          `json:"ack"`
	PollTimeoutSeconds int            `json:"pollTimeoutSeconds"`
	SessionExpiresAt   string         `json:"sessionExpiresAt"`
}

// enrolledSession walks a server all the way to a live session, which every
// poll test needs before it can do anything at all.
func enrolledSession(t *testing.T, server *hub.Server, name string) (createdServer, session) {
	t.Helper()

	created := createServer(t, server, name, "test-game")
	credentials := enroll(t, server, created.Enrollment.Token, "test-game")
	// Five seconds is the protocol floor, and the shortest hold a test can wait
	// out without the suite feeling it.
	return created, startSession(t, server, credentials, 5)
}

// poll sends one poll and returns the decoded answer.
func poll(t *testing.T, server *hub.Server, sessionToken string, request map[string]any) pollResult {
	t.Helper()

	var result pollResult
	status := call(t, server, http.MethodPost, "/plugin/v1/poll", sessionToken, request, &result)
	if status != http.StatusOK {
		t.Fatalf("poll: status = %d, want 200", status)
	}
	return result
}

// pollNow sends a poll that the hub cannot hold: queueing an envelope first
// means it has something to send and must answer at once. Tests of the plugin ->
// hub direction use it so that checking an ack does not cost a full hold.
func pollNow(t *testing.T, server *hub.Server, serverID, sessionToken string, request map[string]any) pollResult {
	t.Helper()

	queueEnvelope(t, server, serverID, "test.nudge", nil)
	return poll(t, server, sessionToken, request)
}

// queueEnvelope puts one envelope on a server's queue through the Admin API.
func queueEnvelope(t *testing.T, server *hub.Server, serverID, envelopeType string, body map[string]any) string {
	t.Helper()

	var queued struct {
		Envelope struct {
			ID   string `json:"id"`
			Type string `json:"type"`
			TS   string `json:"ts"`
		} `json:"envelope"`
	}
	request := map[string]any{"type": envelopeType}
	if body != nil {
		request["body"] = body
	}
	status := call(t, server, http.MethodPost, "/api/v1/servers/"+serverID+"/envelopes",
		testAdminToken, request, &queued)
	if status != http.StatusAccepted {
		t.Fatalf("queue envelope: status = %d, want 202", status)
	}
	if queued.Envelope.ID == "" {
		t.Fatal("queue envelope: response carried no envelope id")
	}
	return queued.Envelope.ID
}

func TestPollRequiresASession(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)

	for _, bearer := range []string{"", "vyt_not-a-session-token"} {
		if code := errorCode(t, server, http.MethodPost, "/plugin/v1/poll", bearer,
			map[string]any{}, http.StatusUnauthorized); code != "session_invalid" {
			t.Errorf("bearer %q: error code = %q, want session_invalid", bearer, code)
		}
	}
}

func TestPollIdleHoldAnswersAnEmptyBatch(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	_, live := enrolledSession(t, server, "idle hold")

	start := time.Now()
	result := poll(t, server, live.SessionToken, map[string]any{})
	held := time.Since(start)

	if result.Envelopes == nil {
		t.Fatal("envelopes is null; an empty batch must be []")
	}
	if len(result.Envelopes) != 0 {
		t.Fatalf("an idle poll returned %d envelopes", len(result.Envelopes))
	}
	if result.PollTimeoutSeconds != 5 {
		t.Errorf("pollTimeoutSeconds = %d, want the negotiated 5", result.PollTimeoutSeconds)
	}
	if result.SessionExpiresAt == "" {
		t.Error("sessionExpiresAt is empty")
	}
	// The hub must answer no later than the negotiated timeout, and must
	// actually hold rather than answer straight away.
	if held < 4*time.Second {
		t.Errorf("the hub answered after %s, well before the 5 s it negotiated", held)
	}
	if held > 8*time.Second {
		t.Errorf("the hub held for %s, past the 5 s it negotiated", held)
	}
}

func TestPollDeliversAQueuedEnvelope(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, live := enrolledSession(t, server, "delivery")

	queuedID := queueEnvelope(t, server, created.Server.ID, "test.greeting",
		map[string]any{"hello": "world"})

	result := poll(t, server, live.SessionToken, map[string]any{})
	if len(result.Envelopes) != 1 {
		t.Fatalf("got %d envelopes, want 1", len(result.Envelopes))
	}

	delivered := result.Envelopes[0]
	if delivered.ID != queuedID {
		t.Errorf("envelope id = %q, want the queued %q", delivered.ID, queuedID)
	}
	if delivered.Type != "test.greeting" {
		t.Errorf("type = %q, want test.greeting", delivered.Type)
	}
	if delivered.Seq != 1 {
		t.Errorf("seq = %d, want 1: sequence numbers start at 1 per session", delivered.Seq)
	}
	if delivered.V != 1 {
		t.Errorf("v = %d, want 1", delivered.V)
	}
	if _, err := time.Parse(time.RFC3339, delivered.TS); err != nil {
		t.Errorf("ts %q is not RFC 3339: %v", delivered.TS, err)
	}
	var body struct {
		Hello string `json:"hello"`
	}
	if err := json.Unmarshal(delivered.Body, &body); err != nil {
		t.Fatalf("decode body %q: %v", delivered.Body, err)
	}
	if body.Hello != "world" {
		t.Errorf("body.hello = %q, want world", body.Hello)
	}
}

func TestPollFlushesEarlyWhenAnEnvelopeArrivesMidHold(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, live := enrolledSession(t, server, "early flush")

	queued := make(chan struct{})
	go func() {
		time.Sleep(100 * time.Millisecond)
		queueEnvelope(t, server, created.Server.ID, "test.mid-hold", nil)
		close(queued)
	}()

	start := time.Now()
	result := poll(t, server, live.SessionToken, map[string]any{})
	elapsed := time.Since(start)
	<-queued

	if len(result.Envelopes) != 1 {
		t.Fatalf("got %d envelopes, want the one queued during the hold", len(result.Envelopes))
	}
	// The hold was for 5 s. Anything near that means the hub waited it out
	// instead of flushing, which is the whole point of holding the request.
	if elapsed > 2*time.Second {
		t.Errorf("the hub took %s to flush an envelope queued mid-hold", elapsed)
	}
}

func TestPollRetransmitsUntilAcked(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, live := enrolledSession(t, server, "retransmit")

	queuedID := queueEnvelope(t, server, created.Server.ID, "test.retransmit", nil)

	first := poll(t, server, live.SessionToken, map[string]any{})
	if len(first.Envelopes) != 1 {
		t.Fatalf("first poll got %d envelopes, want 1", len(first.Envelopes))
	}

	// No ack: the same envelope must come back, byte for byte.
	second := poll(t, server, live.SessionToken, map[string]any{})
	if len(second.Envelopes) != 1 {
		t.Fatalf("second poll got %d envelopes, want the unacked one again", len(second.Envelopes))
	}
	repeat := second.Envelopes[0]
	if repeat.ID != queuedID || repeat.Seq != first.Envelopes[0].Seq || repeat.TS != first.Envelopes[0].TS {
		t.Errorf("retransmission changed the envelope: %+v then %+v", first.Envelopes[0], repeat)
	}

	// Acked, so it is done: the poll that carries the ack comes back with only
	// what was queued after it.
	third := pollNow(t, server, created.Server.ID, live.SessionToken,
		map[string]any{"ack": repeat.Seq})
	if len(third.Envelopes) != 1 {
		t.Fatalf("after acking, got %d envelopes, want only the one queued since", len(third.Envelopes))
	}
	if third.Envelopes[0].ID == queuedID {
		t.Errorf("the acked envelope %q came back", queuedID)
	}
}

func TestPollAcksAreCumulativeAndMonotonic(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, live := enrolledSession(t, server, "cumulative acks")

	for range 3 {
		queueEnvelope(t, server, created.Server.ID, "test.batch", nil)
	}

	first := poll(t, server, live.SessionToken, map[string]any{})
	if len(first.Envelopes) != 3 {
		t.Fatalf("got %d envelopes, want 3", len(first.Envelopes))
	}
	for i, delivered := range first.Envelopes {
		if delivered.Seq != int64(i+1) {
			t.Fatalf("envelope %d has seq %d, want %d: a batch is ascending and contiguous",
				i, delivered.Seq, i+1)
		}
	}

	// Acking 2 acks 1 as well, and leaves 3 outstanding.
	second := poll(t, server, live.SessionToken, map[string]any{"ack": 2})
	if len(second.Envelopes) != 1 || second.Envelopes[0].Seq != 3 {
		t.Fatalf("after acking 2, got %+v, want only seq 3", second.Envelopes)
	}

	// Acking 3 retires the batch, so the only thing left to send is what was
	// queued after it.
	third := pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{"ack": 3})
	if len(third.Envelopes) != 1 || third.Envelopes[0].Seq != 4 {
		t.Fatalf("after acking 3, got %+v, want only the newly queued seq 4", third.Envelopes)
	}

	// A stale ack must not resurrect what a higher one already retired.
	fourth := poll(t, server, live.SessionToken, map[string]any{"ack": 1})
	if len(fourth.Envelopes) != 1 || fourth.Envelopes[0].Seq != 4 {
		t.Fatalf("a stale ack of 1 brought back %+v, want only the still-unacked seq 4",
			fourth.Envelopes)
	}
}

func TestPollRejectsAnAckAboveWhatWasSent(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	_, live := enrolledSession(t, server, "ack out of range")

	if code := errorCode(t, server, http.MethodPost, "/plugin/v1/poll", live.SessionToken,
		map[string]any{"ack": 99}, http.StatusBadRequest); code != "ack_out_of_range" {
		t.Errorf("error code = %q, want ack_out_of_range", code)
	}
}

func TestPollAcksInboundEnvelopesContiguously(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, live := enrolledSession(t, server, "inbound acks")

	inbound := func(seq int64, envelopeType string) map[string]any {
		return map[string]any{
			"v": 1, "id": "test-inbound-" + envelopeType, "type": envelopeType,
			"seq": seq, "ts": time.Now().UTC().Format(time.RFC3339),
			"body": map[string]any{},
		}
	}

	first := pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{
		"envelopes": []map[string]any{inbound(1, "test.one"), inbound(2, "test.two")},
	})
	if first.Ack != 2 {
		t.Fatalf("ack = %d after two envelopes, want 2", first.Ack)
	}

	// A gap must not move the ack, and the envelope above it is discarded
	// rather than held: the sender's retransmission recovers it.
	gapped := pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{
		"envelopes": []map[string]any{inbound(5, "test.five")},
	})
	if gapped.Ack != 2 {
		t.Fatalf("ack = %d after an envelope above a gap, want it to stay at 2", gapped.Ack)
	}

	// A type this hub has never heard of is ordinary traffic: acked and ignored.
	unknown := pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{
		"envelopes": []map[string]any{inbound(3, "nobody.knows.this.type")},
	})
	if unknown.Ack != 3 {
		t.Fatalf("ack = %d after an unknown type, want 3: unknown types are acked and ignored",
			unknown.Ack)
	}

	// Replaying what was already acked is a duplicate, never an error.
	duplicate := pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{
		"envelopes": []map[string]any{inbound(1, "test.one"), inbound(4, "test.four")},
	})
	if duplicate.Ack != 4 {
		t.Fatalf("ack = %d after a duplicate followed by the next envelope, want 4", duplicate.Ack)
	}
}

func TestPollRejectsMalformedEnvelopes(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	_, live := enrolledSession(t, server, "malformed envelopes")

	base := map[string]any{
		"v": 1, "id": "test-malformed", "type": "test.thing", "seq": 1,
		"ts": time.Now().UTC().Format(time.RFC3339), "body": map[string]any{},
	}
	broken := func(mutate func(map[string]any)) map[string]any {
		copied := map[string]any{}
		for key, value := range base {
			copied[key] = value
		}
		mutate(copied)
		return copied
	}

	cases := map[string]map[string]any{
		"no type":       broken(func(m map[string]any) { delete(m, "type") }),
		"no id":         broken(func(m map[string]any) { delete(m, "id") }),
		"seq below one": broken(func(m map[string]any) { m["seq"] = 0 }),
		"future v":      broken(func(m map[string]any) { m["v"] = 99 }),
	}
	for name, malformed := range cases {
		t.Run(name, func(t *testing.T) {
			if code := errorCode(t, server, http.MethodPost, "/plugin/v1/poll", live.SessionToken,
				map[string]any{"envelopes": []map[string]any{malformed}},
				http.StatusBadRequest); code != "envelope_invalid" {
				t.Errorf("error code = %q, want envelope_invalid", code)
			}
		})
	}
}

func TestQueuedEnvelopesOutliveTheirSession(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)

	created := createServer(t, server, "queue survives", "test-game")
	credentials := enroll(t, server, created.Enrollment.Token, "test-game")

	// Queued while nothing is connected: no session, so no sequence number yet.
	queuedID := queueEnvelope(t, server, created.Server.ID, "test.deferred", nil)

	var record struct {
		PendingEnvelopeCount int `json:"pendingEnvelopeCount"`
	}
	call(t, server, http.MethodGet, "/api/v1/servers/"+created.Server.ID, testAdminToken, nil, &record)
	if record.PendingEnvelopeCount != 1 {
		t.Errorf("pendingEnvelopeCount = %d, want 1", record.PendingEnvelopeCount)
	}

	live := startSession(t, server, credentials, 5)
	result := poll(t, server, live.SessionToken, map[string]any{})
	if len(result.Envelopes) != 1 {
		t.Fatalf("got %d envelopes on the first poll of a new session, want 1", len(result.Envelopes))
	}
	if result.Envelopes[0].ID != queuedID {
		t.Errorf("envelope id = %q, want the one queued before the session existed %q",
			result.Envelopes[0].ID, queuedID)
	}
	if result.Envelopes[0].Seq != 1 {
		t.Errorf("seq = %d, want 1: a new session numbers from the start", result.Envelopes[0].Seq)
	}

	// A second session renumbers what the first never acked.
	next := startSession(t, server, credentials, 5)
	again := poll(t, server, next.SessionToken, map[string]any{})
	if len(again.Envelopes) != 1 || again.Envelopes[0].Seq != 1 {
		t.Fatalf("the second session got %+v, want the same envelope renumbered to seq 1",
			again.Envelopes)
	}

	poll(t, server, next.SessionToken, map[string]any{"ack": 1})
	call(t, server, http.MethodGet, "/api/v1/servers/"+created.Server.ID, testAdminToken, nil, &record)
	if record.PendingEnvelopeCount != 0 {
		t.Errorf("pendingEnvelopeCount = %d after the ack, want 0", record.PendingEnvelopeCount)
	}
}

func TestRevocationBreaksAHeldPoll(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)

	created := createServer(t, server, "revoked mid-hold", "test-game")
	credentials := enroll(t, server, created.Enrollment.Token, "test-game")
	live := startSession(t, server, credentials, 25)

	go func() {
		time.Sleep(100 * time.Millisecond)
		call(t, server, http.MethodDelete,
			"/api/v1/servers/"+created.Server.ID+"/credentials", testAdminToken, nil, nil)
	}()

	start := time.Now()
	code := errorCode(t, server, http.MethodPost, "/plugin/v1/poll", live.SessionToken,
		map[string]any{}, http.StatusUnauthorized)
	elapsed := time.Since(start)

	if code != "session_invalid" {
		t.Errorf("error code = %q, want session_invalid", code)
	}
	// Revocation takes effect at once, not at the end of a 25 s hold
	// (spec section 5.4).
	if elapsed > 3*time.Second {
		t.Errorf("the held poll took %s to notice the revocation", elapsed)
	}
}

// Regression: two polls in flight at once used to classify their batches
// against the ack each had captured at authentication time, so the hub could
// report an ack it had already exceeded. Section 9.1 forbids lowering a
// reported ack, and the counter behind it must not double-count either.
func TestConcurrentPollsNeverLowerTheReportedAck(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, live := enrolledSession(t, server, "concurrent inbound acks")

	inbound := func(seq int64) map[string]any {
		return map[string]any{
			"v": 1, "id": "concurrent-" + strconv.FormatInt(seq, 10),
			"type": "test.concurrent", "seq": seq,
			"ts": time.Now().UTC().Format(time.RFC3339), "body": map[string]any{},
		}
	}

	// Both polls carry queued work so neither is held and both race on ingest.
	queueEnvelope(t, server, created.Server.ID, "test.nudge", nil)
	queueEnvelope(t, server, created.Server.ID, "test.nudge", nil)

	// Acks are recorded in the order the responses actually landed, because that
	// is the order the plugin sees them in and the order monotonicity is a claim
	// about. Slot order says nothing.
	var (
		mu       sync.Mutex
		reported []int64
		wg       sync.WaitGroup
	)
	start := make(chan struct{})

	for slot, seqs := range [][]int64{{1, 2}, {1}} {
		wg.Add(1)
		go func(slot int, seqs []int64) {
			defer wg.Done()
			envelopes := make([]map[string]any, 0, len(seqs))
			for _, seq := range seqs {
				envelopes = append(envelopes, inbound(seq))
			}
			<-start
			var result pollResult
			status := call(t, server, http.MethodPost, "/plugin/v1/poll", live.SessionToken,
				map[string]any{"envelopes": envelopes}, &result)

			mu.Lock()
			defer mu.Unlock()
			if status != http.StatusOK {
				t.Errorf("poll %d: status = %d, want 200", slot, status)
			}
			reported = append(reported, result.Ack)
		}(slot, seqs)
	}
	close(start)
	wg.Wait()

	for i, ack := range reported {
		// Zero is the signature of the old defect: a poll classifying against a
		// snapshot taken before the other poll committed anything.
		if ack < 1 || ack > 2 {
			t.Errorf("response %d reported ack %d, want 1 or 2", i, ack)
		}
		if i > 0 && ack < reported[i-1] {
			t.Errorf("reported acks went %d then %d: an ack the hub gave out was lowered",
				reported[i-1], ack)
		}
	}

	// The settled ack must be exactly 2, reached by two distinct envelopes.
	settled := pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{})
	if settled.Ack != 2 {
		t.Errorf("settled ack = %d, want 2", settled.Ack)
	}
}

// Regression: a held poll used to report the inbound ack it captured at ingest
// time, so a concurrent poll that committed a higher ack and answered first
// left the held poll to lower an ack the hub had already reported, which
// section 9.1 forbids. The response ack is session state, not request state:
// a held poll must report the ack as committed when it answers.
func TestHeldPollReportsTheCommittedAck(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, live := enrolledSession(t, server, "held poll ack")

	// P1 parks with nothing to say and nothing queued.
	held := make(chan pollResult, 1)
	go func() {
		held <- poll(t, server, live.SessionToken, map[string]any{})
	}()
	time.Sleep(250 * time.Millisecond)

	// P2, same session, commits envelopes 1 and 2 and parks too.
	advanced := make(chan pollResult, 1)
	go func() {
		advanced <- poll(t, server, live.SessionToken, map[string]any{
			"envelopes": []map[string]any{
				{"v": 1, "id": "held-ack-1", "type": "test.thing", "seq": 1,
					"ts": time.Now().UTC().Format(time.RFC3339), "body": map[string]any{}},
				{"v": 1, "id": "held-ack-2", "type": "test.thing", "seq": 2,
					"ts": time.Now().UTC().Format(time.RFC3339), "body": map[string]any{}},
			},
		})
	}()
	time.Sleep(250 * time.Millisecond)

	// Wake both. Whatever each answers with, neither may report an ack below
	// the 2 that is committed by now.
	queueEnvelope(t, server, created.Server.ID, "test.wake", nil)
	for name, result := range map[string]pollResult{"held": <-held, "advanced": <-advanced} {
		if result.Ack != 2 {
			t.Errorf("the %s poll reported ack %d, want the committed 2", name, result.Ack)
		}
	}
}

// Regression: a poll that authenticated before its session was superseded used
// to reach the store anyway, renumbering envelopes into a dead session and
// voiding the ack the live session had already sent.
func TestSupersededSessionCannotTakeDelivery(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)

	created := createServer(t, server, "superseded delivery", "test-game")
	credentials := enroll(t, server, created.Enrollment.Token, "test-game")
	stale := startSession(t, server, credentials, 5)
	fresh := startSession(t, server, credentials, 5)

	queueEnvelope(t, server, created.Server.ID, "test.contested", nil)

	// The superseded token must not be served, however plausible it looks.
	if code := errorCode(t, server, http.MethodPost, "/plugin/v1/poll", stale.SessionToken,
		map[string]any{}, http.StatusUnauthorized); code != "session_invalid" {
		t.Errorf("stale session: error code = %q, want session_invalid", code)
	}

	// The live session still gets the envelope, at seq 1, and its ack sticks.
	delivered := poll(t, server, fresh.SessionToken, map[string]any{})
	if len(delivered.Envelopes) != 1 || delivered.Envelopes[0].Seq != 1 {
		t.Fatalf("live session got %+v, want one envelope at seq 1", delivered.Envelopes)
	}
	if code := errorCode(t, server, http.MethodPost, "/plugin/v1/poll", stale.SessionToken,
		map[string]any{"ack": 1}, http.StatusUnauthorized); code != "session_invalid" {
		t.Errorf("stale session ack: error code = %q, want session_invalid", code)
	}

	queueEnvelope(t, server, created.Server.ID, "test.after", nil)
	after := poll(t, server, fresh.SessionToken, map[string]any{"ack": 1})
	if len(after.Envelopes) != 1 || after.Envelopes[0].Seq != 2 {
		t.Fatalf("after acking seq 1, got %+v, want only the new envelope at seq 2",
			after.Envelopes)
	}
}

// An explicit "v": 0 is not an absent v: it names a version no hub speaks.
func TestPollRejectsExplicitZeroEnvelopeVersion(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, live := enrolledSession(t, server, "explicit zero version")

	envelope := map[string]any{
		"v": 0, "id": "zero-version", "type": "test.thing", "seq": 1,
		"ts": time.Now().UTC().Format(time.RFC3339), "body": map[string]any{},
	}
	if code := errorCode(t, server, http.MethodPost, "/plugin/v1/poll", live.SessionToken,
		map[string]any{"envelopes": []map[string]any{envelope}},
		http.StatusBadRequest); code != "envelope_invalid" {
		t.Errorf(`"v": 0: error code = %q, want envelope_invalid`, code)
	}

	// An omitted v, by contrast, means the negotiated version and is accepted.
	delete(envelope, "v")
	result := pollNow(t, server, created.Server.ID, live.SessionToken,
		map[string]any{"envelopes": []map[string]any{envelope}})
	if result.Ack != 1 {
		t.Errorf("ack = %d after an envelope with no v, want 1: absent means negotiated", result.Ack)
	}
}

// A ts the hub cannot parse must never cost the batch (spec section 4).
func TestPollAcceptsAnUnparseableTimestamp(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, live := enrolledSession(t, server, "bad timestamp")

	result := pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{
		"envelopes": []map[string]any{{
			"v": 1, "id": "bad-ts", "type": "test.thing", "seq": 1,
			"ts": "last tuesday", "body": map[string]any{},
		}},
	})
	if result.Ack != 1 {
		t.Errorf("ack = %d, want 1: a receiver must not reject an envelope over ts alone", result.Ack)
	}
}

func TestPollUpdatesLastSeenAt(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, live := enrolledSession(t, server, "last seen")

	record := func() string {
		var view struct {
			LastSeenAt string `json:"lastSeenAt"`
		}
		call(t, server, http.MethodGet, "/api/v1/servers/"+created.Server.ID,
			testAdminToken, nil, &view)
		return view.LastSeenAt
	}

	atSession := record()
	time.Sleep(10 * time.Millisecond)
	pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{})

	if afterPoll := record(); afterPoll == atSession {
		t.Errorf("lastSeenAt is still %q after a poll; it must move on every poll", afterPoll)
	}
}

// Section 9.2 says queued envelopes survive a hub restart. The conformance
// suite cannot grade that, because it cannot restart the implementation it is
// pointed at, so the reference hub proves it here: a hub that kept its queue in
// memory would pass every black-box check and still lose an operator's work.
func TestQueuedEnvelopesSurviveAHubRestart(t *testing.T) {
	t.Parallel()
	databaseURL := filepath.Join(t.TempDir(), "restart.db")

	before := newTestServerAt(t, databaseURL)
	created := createServer(t, before, "survives a restart", "test-game")
	credentials := enroll(t, before, created.Enrollment.Token, "test-game")
	queuedID := queueEnvelope(t, before, created.Server.ID, "test.durable",
		map[string]any{"marker": "written before the restart"})

	// The hub goes away with the envelope queued and no session to deliver it.
	if err := before.Close(); err != nil {
		t.Fatalf("close the first hub: %v", err)
	}

	after := newTestServerAt(t, databaseURL)
	live := startSession(t, after, credentials, 5)

	result := poll(t, after, live.SessionToken, map[string]any{})
	if len(result.Envelopes) != 1 {
		t.Fatalf("got %d envelopes from the restarted hub, want the one queued before it stopped",
			len(result.Envelopes))
	}
	delivered := result.Envelopes[0]
	if delivered.ID != queuedID {
		t.Errorf("envelope id = %q, want the pre-restart %q", delivered.ID, queuedID)
	}
	if delivered.Seq != 1 {
		t.Errorf("seq = %d, want 1: the restarted hub numbers into the new session", delivered.Seq)
	}
	var body struct {
		Marker string `json:"marker"`
	}
	if err := json.Unmarshal(delivered.Body, &body); err != nil {
		t.Fatalf("decode body %q: %v", delivered.Body, err)
	}
	if body.Marker != "written before the restart" {
		t.Errorf("body did not survive the restart: %q", delivered.Body)
	}

	// The ack must survive too, or the restart would replay the envelope for
	// ever.
	poll(t, after, live.SessionToken, map[string]any{"ack": delivered.Seq})
	var record struct {
		PendingEnvelopeCount int `json:"pendingEnvelopeCount"`
	}
	call(t, after, http.MethodGet, "/api/v1/servers/"+created.Server.ID,
		testAdminToken, nil, &record)
	if record.PendingEnvelopeCount != 0 {
		t.Errorf("pendingEnvelopeCount = %d after the ack, want 0", record.PendingEnvelopeCount)
	}
}

func TestQueueEnvelopeValidation(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created := createServer(t, server, "queue validation", "test-game")
	path := "/api/v1/servers/" + created.Server.ID + "/envelopes"

	if code := errorCode(t, server, http.MethodPost, path, testAdminToken,
		map[string]any{"body": map[string]any{}}, http.StatusBadRequest); code != "bad_request" {
		t.Errorf("missing type: error code = %q, want bad_request", code)
	}
	if code := errorCode(t, server, http.MethodPost, path, testAdminToken,
		map[string]any{"type": "test.thing", "body": "not an object"},
		http.StatusBadRequest); code != "bad_request" {
		t.Errorf("scalar body: error code = %q, want bad_request", code)
	}
	// The raw queue must not become a way around the endpoint that validates
	// action payloads (spec sections 5.5 and 6.1).
	if code := errorCode(t, server, http.MethodPost, path, testAdminToken,
		map[string]any{"type": "action.dispatch"}, http.StatusConflict); code != "conflict" {
		t.Errorf("reserved type: error code = %q, want conflict", code)
	}
	if code := errorCode(t, server, http.MethodPost, "/api/v1/servers/nope/envelopes",
		testAdminToken, map[string]any{"type": "test.thing"}, http.StatusNotFound); code != "not_found" {
		t.Errorf("unknown server: error code = %q, want not_found", code)
	}
	if code := errorCode(t, server, http.MethodPost, path, "",
		map[string]any{"type": "test.thing"}, http.StatusUnauthorized); code != "unauthorized" {
		t.Errorf("no admin token: error code = %q, want unauthorized", code)
	}
}
