package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// mockHub is the hub half of the Plugin API: just conformant enough that a
// correct plugin behaves normally against it, and instrumented so checks can
// watch what the plugin does and misbehave on purpose (withhold acks, redeliver
// envelopes, sever the transport, kill the session).
//
// It speaks HTTP and nothing else. Like everything under conformance/, it must
// never import hub code, or the suite stops being able to grade an
// implementation that is not this repository's.
//
// Everything is guarded by one mutex. Methods suffixed Locked assume it is
// held; the await predicate runs under it too, so predicates read fields
// directly and never call locking methods.
type mockHub struct {
	mu      sync.Mutex
	changed chan struct{}

	baseURL  string
	listener net.Listener
	server   *http.Server

	// Enrollment (spec section 5.2). One candidate, one token, one use.
	enrollmentToken string
	enrollBurned    bool
	enrollCount     int
	enrolledGame    string
	serverID        string
	serverSecret    string

	// The current session (spec section 5.3). At most one is live; issuing a
	// new one supersedes the old, exactly as a hub must.
	sessionOrdinal     int
	sessionToken       string
	sessionLive        bool
	sessionExpiresAt   string
	pollTimeoutSeconds int
	issuedTokens       map[string]bool
	pollsThisSession   int
	totalPolls         int

	// Hub -> plugin. The queue outlives sessions; unacked items are renumbered
	// into each new session's sequence space, which is the hub's own section
	// 9.1 duty and what keeps a correct plugin correct against this mock.
	outbound        []*outboundItem
	nextOutboundSeq int64
	resend          []deliverable
	hubEnvCounter   int64

	// Plugin -> hub. processedTop is the highest contiguous seq taken in this
	// session; ackLimit (when not -1) freezes the ack the hub reports, so a
	// check can leave plugin envelopes deliberately unacked.
	processedTop     int64
	ackLimit         int64
	lastReportedAck  int64
	highestPluginAck int64
	inbound          []*inboundEnvelope
	bySeq            map[int64]*inboundEnvelope
	idToSeq          map[string]int64
	retransmissions  []retransmission
	// idContent survives session changes, unlike idToSeq: section 4 makes an
	// id name one message on this server in any session, because cross-session
	// dedup (sections 8.1 and 8.3) treats equal ids as the same message. The
	// same id reappearing with the same content is a legal replay; with
	// different content it is a fresh message a hub would silently drop.
	idContent map[string]*inboundEnvelope

	// What the plugin has published and reported, decoded for the checks.
	manifest *manifestInfo
	actions  map[string]*actionTrack
	// Context enumeration (spec section 6.2). Every context.entries the
	// plugin sends is acked like any other envelope and kept here under the
	// requestId it echoes, so the enumerate stage can await the answer to
	// its own question; contextReplies counts every reply that arrived,
	// including one echoing a requestId this hub never asked about, which
	// section 6.2 has a hub ack and ignore rather than refuse.
	contextEntries map[string]*inboundEnvelope
	contextReplies int
	// Telemetry (spec section 8): validated on arrival, counted for the
	// telemetry stage. Faults found before that stage has run are held for
	// it rather than charged to whatever stage the first poll landed in, so
	// the report names the telemetry rather than the poll; once the stage
	// has run, later telemetry faults fail the stage they arrive in like any
	// other fault.
	telemetry       telemetryStats
	telemetryFaults []fault
	telemetryGraded bool

	// Session-change grading (spec section 9.1). killSession records every
	// plugin envelope above the reported ack; ingest marks each one off as it
	// arrives renumbered in a later session.
	expectedRenumber map[string]*renumberExpectation

	// Outage simulation. While severed, every request is aborted at the
	// transport level (connection reset, no HTTP response).
	severed         bool
	abortedRequests int

	faults []fault

	pluginExited  bool
	pluginExitMsg string

	// Inline errors (spec section 2.3). legacyErrors makes this hub behave
	// like one that predates the option: the parameter is ignored and every
	// refusal is an ordinary status, which is how a candidate's opaque-error
	// fallback gets graded. inlineSeen records whether the candidate ever
	// asked; the error stages read it to decide what a compliant candidate
	// could have known.
	legacyErrors bool
	inlineSeen   bool

	// Provocations the error stages arm.
	//
	// rejectArmed refuses the next poll batch that carries a fresh envelope
	// (above processedTop, so not a legal retransmission of something already
	// accepted) with envelope_invalid at the first fresh envelope's index,
	// recording it in rejected. When that refusal was opaque, rejectRemaining
	// further batches carrying the condemned id are refused as well, so a
	// plugin that answers every opaque 4xx with a new session shows itself.
	//
	// refuseSessionsArmed starts a window, on the next session request, in
	// which every session request is answered credentials_revoked; attempts
	// are logged in sessionAttempts and refusals counted in refusedSessions.
	//
	// garbleArmed answers the next poll that carries envelopes with a 200
	// whose body is not JSON, without ingesting them, recording in garbled
	// what the plugin will have to send again. When that poll opted in to
	// inline errors, garbleAckArmed then answers the poll carrying them again
	// with a JSON object whose error member is a string, not an object with a
	// code (malformed, section 2.3), and whose ack covers the whole batch: a
	// plugin that applies an ack from such a body drops envelopes the hub
	// never took and never sends them again.
	rejectArmed     bool
	rejectRemaining int
	rejected        *batchRejection
	// acceptGen counts fresh acceptances; lastAccepted maps each id to the
	// generation at which it was last accepted as a fresh envelope, in any
	// session, so a stage can tell an arrival after its provocation from one
	// before it by order rather than by clock (idContent only remembers the
	// first, and timestamps can tie on a coarse clock).
	acceptGen    uint64
	lastAccepted map[string]uint64
	// returned records at which seq and session a snapshotted envelope came
	// back, so a stage can bind the seq it expects to the recovery it saw.
	returned map[string]returnRecord
	// pollsInFlight and overlappingPolls (atomic, outside the mutex) watch
	// whether the candidate ever has more than one poll open at once. The
	// timing and resend-count assertions of the error stages assume one
	// request at a time, which is how every reference plugin works; against
	// a candidate that overlaps polls they cannot tell a retry from a
	// request already in flight, so they stand down.
	pollsInFlight    atomic.Int32
	overlappingPolls atomic.Int64
	// pollArrivals numbers polls as their handlers start, before the body is
	// read, so the backlog grading can order a poll against an answer
	// without trusting a clock that can tie on a coarse timer.
	pollArrivals atomic.Int64
	// expectedContent is what a provocation saw of each fresh envelope it
	// refused or swallowed; ingest faults a later arrival of that id whose
	// type, ts or body changed.
	expectedContent     map[string]contentSnapshot
	refuseSessionsArmed bool
	refuseSessionsFor   time.Duration
	refuseSessionsUntil time.Time
	sessionAttempts     []time.Time
	refusedSessions     int
	// sessionStarts is when each session ordinal was issued, so a stage can
	// measure the pause between a refusal and a replacement session.
	sessionStarts  map[int]time.Time
	garbleArmed    bool
	garbleAckArmed bool
	garbled        *garbleRecord

	// The installation ban list (spec section 13) the bans stage walks the
	// candidate through; see bans.go.
	bans *mockBans

	// A backlog (spec section 3.1.2). pollsSayingMore and pollsCarryingFresh
	// count the polls that set more and the polls that carried an envelope
	// above the accepted top, for the backlog stage. moreFollowUp is set when
	// a poll said more and its answer acked exactly what it carried: the
	// next poll of that session to arrive after the answer must then carry
	// envelopes, or the claim was false (the MUST of section 3.1.2). It need not carry new ones: an
	// answer the plugin never received has it send the same batch again,
	// which is the retransmission section 9.1 requires, and a write that
	// succeeded here is no evidence the plugin read it.
	pollsSayingMore    int
	pollsCarryingFresh int
	moreFollowUp       *moreClaim
}

// moreClaim is a poll that said more and was answered with an ack of exactly
// its batch: Top is the highest seq it carried, in session Session, and
// Arrivals is the poll arrival count when the answer was framed.
type moreClaim struct {
	Session  int
	Top      int64
	Arrivals int64
}

// batchRejection is what the mock refused: the condemned envelope at index 0,
// the ids of the rest of the batch, whether the refusal travelled inline (so
// the candidate could read it) or as an opaque 400, and how many times the
// condemned id has been refused in all.
type batchRejection struct {
	ID        string
	Type      string
	Seq       int64
	Index     int
	Others    []string // the other fresh envelopes of the refused batch
	OtherSeqs []int64  // their seqs as sent in the refused batch
	Inline    bool
	// Refusals counts every refusal of the condemned id; Events records each
	// one with the session it happened on and whether that session had
	// polled successfully before it, and Accepted records the poll that
	// finally carried the batch through, so a stage can grade the pause the
	// plugin left after each refusal on a never-polled session.
	Refusals int
	Events   []refusalEvent
	Accepted *refusalEvent
	At       time.Time
	PollsAt  int    // totalPolls when the batch was first refused
	GenAt    uint64 // acceptGen when the batch was first refused
}

// returnRecord is where a snapshotted envelope was accepted again.
type returnRecord struct {
	Seq     int64
	Session int
}

// contentSnapshot is what a provocation saw of a fresh envelope it did not
// ingest, so that when the plugin sends it again the mock can check it came
// back unchanged (section 9.1): same type, same ts, same body, and, within
// the same session, the seq it had less the shift the section 2.3 quarantine
// allows (one for every envelope refused ahead of it, none otherwise).
type contentSnapshot struct {
	Type    string
	TS      string
	Body    string
	Seq     int64
	Session int
	Shift   int64
}

type refusalEvent struct {
	At      time.Time
	Ordinal int
	Polled  bool // the session had at least one accepted poll before this
}

// garbleRecord is what a garbled poll answer swallowed: the ids of the batch
// the plugin sent and did not get an ack for, and the poll count at the time,
// so the stage can tell a re-poll after the garble from one before it.
type garbleRecord struct {
	IDs     []string
	PollsAt int
	GenAt   uint64
	At      time.Time
	// RetryAt is when the poll carrying the swallowed envelopes again
	// arrived, so the stage can grade the pause before the retry itself
	// rather than before some poll the plugin already had in flight.
	RetryAt time.Time
	// AckAt is when that retry was answered with the malformed body carrying
	// an ack, and AckServed the ack it carried; zero when none was served
	// (the candidate did not opt in to inline errors). AckRetryAt is when the
	// swallowed envelopes arrived once more after it.
	AckAt      time.Time
	AckServed  int64
	AckRetryAt time.Time
}

// batchField is the framing of one envelope in a batch as the provocations
// read it: id, type and seq decoded, ts and body kept raw.
type batchField struct {
	ID   string          `json:"id"`
	Type string          `json:"type"`
	Seq  int64           `json:"seq"`
	TS   json.RawMessage `json:"ts"`
	Body json.RawMessage `json:"body"`
}

func (f batchField) snapshot(session int, shift int64) contentSnapshot {
	body := "{}"
	if len(f.Body) > 0 {
		body = string(f.Body)
	}
	return contentSnapshot{Type: f.Type, TS: string(f.TS), Body: body, Seq: f.Seq, Session: session, Shift: shift}
}

// batchIDs decodes the framing of every envelope in a batch, in order,
// without validating anything else.
func batchIDs(raws []json.RawMessage) []batchField {
	out := make([]batchField, len(raws))
	for i, raw := range raws {
		_ = json.Unmarshal(raw, &out[i])
	}
	return out
}

// fault is a protocol violation the plugin committed. Faults are recorded
// rather than answered with errors wherever the spec lets the harness keep the
// session alive, so one mistake surfaces as a named failure instead of
// deadlocking every later check.
type fault struct {
	Section string
	Message string
}

func (f fault) String() string { return fmt.Sprintf("[spec section %s] %s", f.Section, f.Message) }

type inboundEnvelope struct {
	Session    int
	Seq        int64
	ID         string
	Type       string
	TS         string // raw JSON, preserved for the unchanged-retransmission rule
	Body       string // raw JSON, same reason
	ActionID   string // parsed from action.* bodies, for the checks
	ReceivedAt time.Time
}

type retransmission struct {
	Session int
	Seq     int64
	At      time.Time
}

type outboundItem struct {
	id   string
	typ  string
	ts   string
	body json.RawMessage
	// seq is the item's number in the current session, 0 until delivered
	// there. A new session zeroes every unacked item, which is renumbering.
	seq   int64
	acked bool
}

// deliverable is one envelope as it goes on the wire.
type deliverable struct {
	V    int             `json:"v"`
	ID   string          `json:"id"`
	Type string          `json:"type"`
	Seq  int64           `json:"seq"`
	TS   string          `json:"ts"`
	Body json.RawMessage `json:"body"`
}

type manifestAction struct {
	Code    string
	Context string
	Params  map[string]any
}

// manifestContext is one custom context the manifest declares (spec section
// 6.2), which is what the enumerate stage asks the plugin about.
type manifestContext struct {
	ID   string
	Name string
}

type manifestInfo struct {
	Game         string
	Revision     int64
	Actions      []manifestAction
	Contexts     []manifestContext
	KVNamespaces []string
	// EventPayloads are the declared events' payload schemas by index into
	// the manifest's events (nil where an event declares none), which a hub
	// compiles like a params schema (spec section 6.4).
	EventPayloads []map[string]any
	// Capabilities are the optional parts of the protocol the plugin says it
	// implements (spec section 6.7).
	Capabilities []string
}

type actionTrack struct {
	acks    int
	results int
	// executions holds the execution witnesses (executionWitnessType)
	// naming this action, one per event, keyed by the envelope id and the
	// event's index in its batch so a retransmitted batch is not a second
	// execution.
	executions map[string]bool
}

// executionWitnessType is the event a candidate may emit each time it
// executes an action, carrying the actionId in its data. It is a convention
// of this harness, not of the protocol: a black-box grader cannot see a game
// effect, so a candidate that wants its execution graded, and not only its
// action.result messages, reports each execution this way. The reference
// driver does.
const executionWitnessType = "conformance.executed"

type renumberExpectation struct {
	oldSeq  int64
	typ     string
	ts      string
	body    string
	arrived bool
}

func startMockHub(listen string) (*mockHub, error) {
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", listen, err)
	}

	h := &mockHub{
		changed:         make(chan struct{}),
		listener:        listener,
		baseURL:         "http://" + listener.Addr().String(),
		enrollmentToken: "conformance-enroll-" + randomHex(),
		ackLimit:        -1,
		issuedTokens:    map[string]bool{},
		bySeq:           map[int64]*inboundEnvelope{},
		idToSeq:         map[string]int64{},
		idContent:       map[string]*inboundEnvelope{},
		actions:         map[string]*actionTrack{},
		bans:            newMockBans(),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /plugin/v1/enroll", h.handleEnroll)
	mux.HandleFunc("POST /plugin/v1/session", h.handleSession)
	mux.HandleFunc("POST /plugin/v1/poll", h.handlePoll)
	mux.HandleFunc("GET /plugin/v1/bans", h.handleBans)
	mux.HandleFunc("POST /plugin/v1/bans/get", h.handleBans)
	h.server = &http.Server{Handler: mux}
	go func() { _ = h.server.Serve(listener) }()
	return h, nil
}

func (h *mockHub) Close() { _ = h.server.Close() }

func randomHex() string {
	buf := make([]byte, 12)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

// signalLocked wakes everything waiting on hub state.
func (h *mockHub) signalLocked() {
	close(h.changed)
	h.changed = make(chan struct{})
}

func (h *mockHub) faultLocked(section, format string, args ...any) {
	h.faults = append(h.faults, fault{Section: section, Message: fmt.Sprintf(format, args...)})
	h.signalLocked()
}

// await blocks until pred is true, the plugin process exits, or the timeout
// passes. pred runs with the hub mutex held.
func (h *mockHub) await(timeout time.Duration, what string, pred func() bool) error {
	deadline := time.Now().Add(timeout)
	for {
		h.mu.Lock()
		done := pred()
		exited, exitMsg := h.pluginExited, h.pluginExitMsg
		ch := h.changed
		h.mu.Unlock()
		if done {
			return nil
		}
		if exited {
			return fmt.Errorf("waiting for %s: the candidate plugin exited (%s)", what, exitMsg)
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("timed out after %s waiting for %s", timeout, what)
		}
		wait := 100 * time.Millisecond
		if remaining < wait {
			wait = remaining
		}
		select {
		case <-ch:
		case <-time.After(wait):
		}
	}
}

// view runs read under the hub mutex, for one-shot assertions between awaits.
func (h *mockHub) view(read func()) {
	h.mu.Lock()
	read()
	h.mu.Unlock()
}

func (h *mockHub) notePluginExit(message string) {
	h.mu.Lock()
	h.pluginExited = true
	h.pluginExitMsg = message
	h.signalLocked()
	h.mu.Unlock()
}

// abortIfSevered simulates the network being down: the connection is reset
// with no HTTP response at all, which is what a real outage looks like to an
// engine HTTP client.
func (h *mockHub) abortIfSevered() {
	h.mu.Lock()
	severed := h.severed
	if severed {
		h.abortedRequests++
		h.signalLocked()
	}
	h.mu.Unlock()
	if severed {
		panic(http.ErrAbortHandler)
	}
}

func (h *mockHub) sever() {
	h.mu.Lock()
	h.severed = true
	h.signalLocked()
	h.mu.Unlock()
}

func (h *mockHub) restore() {
	h.mu.Lock()
	h.severed = false
	h.signalLocked()
	h.mu.Unlock()
}

// freezeAck stops the reported ack at everything processed so far, so the
// plugin's next envelopes stay unacked however often it retransmits them.
func (h *mockHub) freezeAck() {
	h.mu.Lock()
	h.ackLimit = h.processedTop
	h.signalLocked()
	h.mu.Unlock()
}

func (h *mockHub) releaseAck() {
	h.mu.Lock()
	h.ackLimit = -1
	h.signalLocked()
	h.mu.Unlock()
}

// killSession invalidates the live session the way a hub supersedes one, and
// records every plugin envelope above the reported ack: section 9.1 obliges
// the plugin to renumber exactly those into its next session.
func (h *mockHub) killSession() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	reported := h.reportedAckLocked()
	h.expectedRenumber = map[string]*renumberExpectation{}
	for seq := reported + 1; seq <= h.processedTop; seq++ {
		if e := h.bySeq[seq]; e != nil {
			h.expectedRenumber[e.ID] = &renumberExpectation{
				oldSeq: seq, typ: e.Type, ts: e.TS, body: e.Body,
			}
		}
	}
	h.sessionLive = false
	h.signalLocked()
	return len(h.expectedRenumber)
}

// queueOutbound puts one hub envelope on the queue, exactly like the real
// hub's transport primitive (spec section 5.5): no seq until a session
// delivers it.
func (h *mockHub) queueOutbound(envelopeType string, body any) *outboundItem {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.queueOutboundLocked(envelopeType, body)
}

func (h *mockHub) queueOutboundLocked(envelopeType string, body any) *outboundItem {
	encoded, err := json.Marshal(body)
	if err != nil {
		encoded = json.RawMessage(`{}`)
	}
	h.hubEnvCounter++
	item := &outboundItem{
		id:   fmt.Sprintf("conformance-hub-%d", h.hubEnvCounter),
		typ:  envelopeType,
		ts:   time.Now().UTC().Format(time.RFC3339),
		body: encoded,
	}
	h.outbound = append(h.outbound, item)
	h.signalLocked()
	return item
}

// queueDispatch frames an action.dispatch the way the hub's action lifecycle
// does (spec section 7). params is marshalled as given, so a check can send
// schema-invalid or even non-object params on purpose. ttl becomes the body's
// expiresAt: the deadline the plugin is told, which the checks keep aligned
// with how long they are willing to wait.
func (h *mockHub) queueDispatch(actionID string, action manifestAction, params any, ttl time.Duration) *outboundItem {
	return h.queueOutbound("action.dispatch", dispatchBody(actionID, action, params, ttl))
}

// queueDispatches queues one dispatch per actionId under a single hold of the
// lock, so a held poll cannot wake between two of them: the whole burst goes
// out in one response.
func (h *mockHub) queueDispatches(actionIDs []string, action manifestAction, params any, ttl time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, actionID := range actionIDs {
		h.queueOutboundLocked("action.dispatch", dispatchBody(actionID, action, params, ttl))
	}
}

func dispatchBody(actionID string, action manifestAction, params any, ttl time.Duration) map[string]any {
	context := action.Context
	if context == "" {
		context = "world"
	}
	body := map[string]any{
		"actionId":  actionID,
		"code":      action.Code,
		"context":   context,
		"params":    params,
		"expiresAt": time.Now().Add(ttl).UTC().Format(time.RFC3339),
	}
	if context != "world" {
		body["referenceKey"] = "conformance-target"
	}
	return body
}

// redeliver queues a verbatim copy of an already-delivered envelope: same id,
// same seq, same ts, same body. That is the forced re-delivery of an
// at-least-once transport, and the plugin must treat it as the duplicate it is.
func (h *mockHub) redeliver(item *outboundItem) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if item.seq == 0 {
		return fmt.Errorf("redeliver: envelope %s was never delivered", item.id)
	}
	h.resend = append(h.resend, deliverable{
		V: 1, ID: item.id, Type: item.typ, Seq: item.seq, TS: item.ts, Body: item.body,
	})
	h.signalLocked()
	return nil
}

// consumeFaults drains faults recorded past the cursor and advances it.
func (h *mockHub) consumeFaults(cursor *int) error {
	h.mu.Lock()
	pending := h.faults[*cursor:]
	*cursor = len(h.faults)
	h.mu.Unlock()
	if len(pending) == 0 {
		return nil
	}
	messages := make([]string, 0, len(pending))
	for _, f := range pending {
		messages = append(messages, f.String())
	}
	return fmt.Errorf("protocol fault: %s", strings.Join(messages, "; "))
}

// reportedAckLocked is the ack the hub tells the plugin: everything processed,
// capped by a frozen limit, and never lower than already reported, because a
// hub's acks are monotonic too.
func (h *mockHub) reportedAckLocked() int64 {
	ack := h.pendingAckLocked()
	h.lastReportedAck = ack
	return ack
}

// pendingAckLocked is the ack the next answer would report, without
// recording it as reported.
func (h *mockHub) pendingAckLocked() int64 {
	ack := h.processedTop
	if h.ackLimit >= 0 && h.ackLimit < ack {
		ack = h.ackLimit
	}
	if ack < h.lastReportedAck {
		ack = h.lastReportedAck
	}
	return ack
}

// ---- HTTP handlers ----

func writeJSONBody(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeProtocolError(w http.ResponseWriter, status int, code, message string) {
	writeJSONBody(w, status, map[string]any{
		"error": map[string]any{"code": code, "message": message},
	})
}

// errorMode reads the request's ?errors= parameter (spec section 2.3). It
// answers false when the request asked for a mode this hub does not offer,
// which is a candidate fault and an ordinary 400. Call without the lock.
func (h *mockHub) errorMode(w http.ResponseWriter, r *http.Request) (inline bool, ok bool) {
	modes := r.URL.Query()["errors"]
	if len(modes) == 0 {
		return false, true
	}
	if len(modes) != 1 || modes[0] != "inline" {
		h.mu.Lock()
		h.faultLocked("2.3", "a request carried errors=%s; the only mode defined is inline", strings.Join(modes, ","))
		h.mu.Unlock()
		writeProtocolError(w, http.StatusBadRequest, "bad_request", "errors="+strings.Join(modes, ",")+" is not a mode this hub offers")
		return false, false
	}
	h.mu.Lock()
	h.inlineSeen = true
	legacy := h.legacyErrors
	h.mu.Unlock()
	return !legacy, true
}

// answer writes a refusal the way the request asked for it: inline as a 200
// with error.status, or as an ordinary status.
func answer(w http.ResponseWriter, inline bool, status int, code, message string, details map[string]any) {
	failure := map[string]any{"code": code, "message": message}
	if details != nil {
		failure["details"] = details
	}
	if inline {
		failure["status"] = status
		writeJSONBody(w, http.StatusOK, map[string]any{"error": failure})
		return
	}
	writeJSONBody(w, status, map[string]any{"error": failure})
}

func bearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

func readBody(r *http.Request) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r.Body, 4<<20))
}

func (h *mockHub) handleEnroll(w http.ResponseWriter, r *http.Request) {
	h.abortIfSevered()
	inline, ok := h.errorMode(w, r)
	if !ok {
		return
	}
	raw, err := readBody(r)
	if err != nil {
		answer(w, inline, http.StatusBadRequest, "bad_request", "unreadable body", nil)
		return
	}
	var request struct {
		EnrollmentToken string `json:"enrollmentToken"`
		Game            string `json:"game"`
	}
	decodeErr := json.Unmarshal(raw, &request)

	var response map[string]any
	var status int
	var code, message string

	h.mu.Lock()
	h.enrollCount++
	switch {
	case decodeErr != nil:
		h.faultLocked("5.2", "the enroll request body was not a JSON object")
		status, code, message = http.StatusBadRequest, "bad_request", "body is not JSON"
	case request.EnrollmentToken == "" || strings.TrimSpace(request.Game) == "":
		h.faultLocked("5.2", "the enroll request must carry enrollmentToken and a non-empty game")
		status, code, message = http.StatusBadRequest, "bad_request", "enrollmentToken and game are required"
	case request.EnrollmentToken != h.enrollmentToken:
		h.faultLocked("5.2", "the plugin presented an enrollment token this harness never issued")
		status, code, message = http.StatusUnauthorized, "enrollment_token_invalid", "unknown enrollment token"
	case h.enrollBurned:
		h.faultLocked("5.3", "the enrollment token was presented a second time; a one-time token is burned at first use (section 5.2), and after session_invalid a plugin re-sessions with its stored credentials rather than re-enrolling")
		status, code, message = http.StatusConflict, "enrollment_token_used", "enrollment token already used"
	default:
		h.enrollBurned = true
		h.enrolledGame = request.Game
		h.serverID = "conformance-server-1"
		h.serverSecret = "conformance-secret-" + randomHex()
		response = map[string]any{
			"serverId":     h.serverID,
			"serverSecret": h.serverSecret,
			"server": map[string]any{
				"id": h.serverID, "name": "conformance-candidate", "game": h.enrolledGame,
			},
		}
	}
	h.signalLocked()
	h.mu.Unlock()

	if response != nil {
		writeJSONBody(w, http.StatusCreated, response)
		return
	}
	answer(w, inline, status, code, message, nil)
}

func (h *mockHub) handleSession(w http.ResponseWriter, r *http.Request) {
	h.abortIfSevered()
	inline, ok := h.errorMode(w, r)
	if !ok {
		return
	}
	raw, err := readBody(r)
	if err != nil {
		answer(w, inline, http.StatusBadRequest, "bad_request", "unreadable body", nil)
		return
	}
	var request struct {
		ServerID           string `json:"serverId"`
		ServerSecret       string `json:"serverSecret"`
		ProtocolVersion    *int   `json:"protocolVersion"`
		PollTimeoutSeconds *int   `json:"pollTimeoutSeconds"`
	}
	decodeErr := json.Unmarshal(raw, &request)

	h.mu.Lock()
	h.sessionAttempts = append(h.sessionAttempts, time.Now())

	if decodeErr != nil {
		h.faultLocked("5.3", "the session request body was not a JSON object")
		h.mu.Unlock()
		answer(w, inline, http.StatusBadRequest, "bad_request", "body is not JSON", nil)
		return
	}
	if !h.enrollBurned || request.ServerID != h.serverID || request.ServerSecret != h.serverSecret {
		h.faultLocked("5.3", "the session request did not carry the serverId and serverSecret that enrollment issued")
		h.mu.Unlock()
		answer(w, inline, http.StatusUnauthorized, "credentials_invalid", "unknown server credentials", nil)
		return
	}
	if request.ProtocolVersion != nil && *request.ProtocolVersion != 1 {
		h.faultLocked("5.3", "the plugin requested protocol version %d; this harness speaks version 1", *request.ProtocolVersion)
		h.mu.Unlock()
		answer(w, inline, http.StatusBadRequest, "protocol_version_unsupported", "this harness speaks protocol version 1", nil)
		return
	}
	if h.refuseSessionsArmed {
		// The window opens on the first attempt, not when the stage armed
		// it, so a plugin that waits before retrying still meets the refusal.
		h.refuseSessionsArmed = false
		h.refuseSessionsUntil = time.Now().Add(h.refuseSessionsFor)
	}
	if time.Now().Before(h.refuseSessionsUntil) {
		// The credentials-refused stage: the operator revoked this server,
		// and the plugin is expected to retry slowly rather than hammer.
		h.refusedSessions++
		h.signalLocked()
		h.mu.Unlock()
		answer(w, inline, http.StatusUnauthorized, "credentials_revoked",
			"these credentials were revoked; enroll again with a new enrollment token", nil)
		return
	}

	// Honor a requested pollTimeout inside 5 s to 60 s, clamp outside, default
	// 25 (spec section 3.1.1).
	effective := 25
	if request.PollTimeoutSeconds != nil {
		effective = *request.PollTimeoutSeconds
		if effective < 5 {
			effective = 5
		}
		if effective > 60 {
			effective = 60
		}
	}

	// A new session supersedes the old one and resets both sequence spaces.
	// Unacked outbound items lose their seq here; delivery under the new
	// session renumbers them, which is the hub's own section 9.1 duty.
	h.sessionOrdinal++
	if h.sessionStarts == nil {
		h.sessionStarts = map[int]time.Time{}
	}
	h.sessionStarts[h.sessionOrdinal] = time.Now()
	h.sessionToken = fmt.Sprintf("conformance-session-%d-%s", h.sessionOrdinal, randomHex())
	h.issuedTokens[h.sessionToken] = true
	h.sessionLive = true
	h.pollTimeoutSeconds = effective
	h.pollsThisSession = 0
	h.nextOutboundSeq = 0
	for _, item := range h.outbound {
		if !item.acked {
			item.seq = 0
		}
	}
	h.resend = nil
	h.processedTop = 0
	h.ackLimit = -1
	h.lastReportedAck = 0
	h.highestPluginAck = 0
	h.bySeq = map[int64]*inboundEnvelope{}
	h.idToSeq = map[string]int64{}
	h.sessionExpiresAt = time.Now().Add(15 * time.Minute).UTC().Format(time.RFC3339)
	h.signalLocked()

	response := map[string]any{
		"sessionId":          fmt.Sprintf("conformance-session-%d", h.sessionOrdinal),
		"sessionToken":       h.sessionToken,
		"expiresAt":          h.sessionExpiresAt,
		"protocolVersion":    1,
		"envelopeVersion":    1,
		"pollTimeoutSeconds": effective,
		"transports":         []string{"poll"},
		"features":           map[string]any{"inlineErrors": !h.legacyErrors},
		"server": map[string]any{
			"id": h.serverID, "name": "conformance-candidate", "game": h.enrolledGame,
			"bansRevision": h.bans.revision,
		},
	}
	h.mu.Unlock()
	writeJSONBody(w, http.StatusOK, response)
}

type pollWire struct {
	Ack       *int64            `json:"ack"`
	Envelopes []json.RawMessage `json:"envelopes"`
	// More stays raw so a value that is not a boolean is a fault naming the
	// member rather than a body that fails to decode.
	More json.RawMessage `json:"more"`
}

// gradeMoreLocked checks a poll's more member on arrival (spec section
// 3.1.2) and settles the claim the previous poll of the session made, and
// reports whether this poll says more. arrival is the poll's number in
// pollArrivals.
func (h *mockHub) gradeMoreLocked(request pollWire, arrival int64) bool {
	says := false
	if len(request.More) > 0 {
		switch strings.TrimSpace(string(request.More)) {
		case "true":
			says = true
		case "false":
		default:
			h.faultLocked("3.1.2", "a poll carried more = %s; more is a boolean", truncateRaw(request.More))
		}
	}
	if says {
		h.pollsSayingMore++
		if len(request.Envelopes) == 0 {
			h.faultLocked("3.1.2", "a poll said more while carrying no envelopes; more says the plugin left envelopes out of the batch it sent, and a hub answering it at once would only be polled again with nothing")
		}
	}
	if claim := h.moreFollowUp; claim != nil {
		h.moreFollowUp = nil
		// Only a poll of the same session that arrived after the answer was
		// framed, from a candidate that has never had two polls open: with
		// polls overlapping, arrival order says nothing of the order the
		// plugin framed them in, so the grading stands down as the error
		// stages do. Even then arrival is no evidence the plugin read the
		// answer, which is why an empty batch is the only thing faulted:
		// whether or not the answer reached it, a plugin holding envelopes
		// behind the batch carries some of them on its next poll (section
		// 3.1.2 makes that a MUST).
		if claim.Session == h.sessionOrdinal && arrival > claim.Arrivals && h.pollsInFlight.Load() == 1 &&
			h.overlappingPolls.Load() == 0 && len(request.Envelopes) == 0 {
			h.faultLocked("3.1.2", "a poll said more, its answer acked exactly what it carried (through seq %d), and the next poll carried no envelopes; a plugin that sets more carries envelopes on its next poll of the session, the ones it left behind the batch, which the answer cannot have acked", claim.Top)
		}
	}
	return says
}

// truncateRaw shortens a raw JSON value for a fault message.
func truncateRaw(raw json.RawMessage) string {
	const limit = 64
	if len(raw) <= limit {
		return string(raw)
	}
	return string(raw[:limit]) + "..."
}

func (h *mockHub) handlePoll(w http.ResponseWriter, r *http.Request) {
	arrival := h.pollArrivals.Add(1)
	if h.pollsInFlight.Add(1) > 1 {
		h.overlappingPolls.Add(1)
	}
	defer h.pollsInFlight.Add(-1)
	h.abortIfSevered()
	inline, ok := h.errorMode(w, r)
	if !ok {
		return
	}
	token := bearer(r)
	raw, err := readBody(r)
	if err != nil {
		answer(w, inline, http.StatusBadRequest, "bad_request", "unreadable body", nil)
		return
	}
	var request pollWire
	decodeErr := json.Unmarshal(raw, &request)

	h.mu.Lock()

	if token == "" || token != h.sessionToken || !h.sessionLive {
		// A poll on a superseded token is ordinary after a session change; a
		// token the harness never issued is a broken plugin.
		if !h.issuedTokens[token] {
			h.faultLocked("5.3", "a poll carried a bearer token this harness never issued")
		}
		h.mu.Unlock()
		answer(w, inline, http.StatusUnauthorized, "session_invalid", "session is not live", nil)
		return
	}
	if decodeErr != nil {
		h.faultLocked("3.1.2", "a poll request body was not a JSON object")
		h.mu.Unlock()
		answer(w, inline, http.StatusBadRequest, "bad_request", "body is not JSON", nil)
		return
	}
	saysMore := h.gradeMoreLocked(request, arrival)

	// The provocations of the error stages, applied before anything in the
	// request takes effect: a refused batch changes nothing, including its
	// ack (section 3.1.2), and a garbled answer is one the plugin must treat
	// as if it never arrived.
	if len(request.Envelopes) > 0 {
		ids := batchIDs(request.Envelopes)
		// The fresh envelopes of the batch: those above the accepted top.
		// Everything below it is a legal retransmission of something already
		// accepted (section 9.1), which a provocation must neither condemn
		// nor count on being sent again.
		firstFresh := -1
		for index, one := range ids {
			if one.Seq > h.processedTop {
				firstFresh = index
				break
			}
		}
		if h.rejectArmed && firstFresh >= 0 {
			h.rejectArmed = false
			now := time.Now()
			rejection := &batchRejection{
				ID: ids[firstFresh].ID, Type: ids[firstFresh].Type, Seq: ids[firstFresh].Seq, Index: firstFresh,
				Inline: inline, Refusals: 1, At: now, PollsAt: h.totalPolls, GenAt: h.acceptGen,
				Events: []refusalEvent{{At: now, Ordinal: h.sessionOrdinal, Polled: h.pollsThisSession > 0}},
			}
			// The condemned envelope may only ever come back at its own seq;
			// everything behind it moves down by one when it is set aside.
			h.rememberContentLocked(ids[firstFresh:firstFresh+1], 0)
			h.rememberContentLocked(ids[firstFresh+1:], 1)
			for _, other := range ids[firstFresh+1:] {
				rejection.Others = append(rejection.Others, other.ID)
				rejection.OtherSeqs = append(rejection.OtherSeqs, other.Seq)
			}
			h.rejected = rejection
			trace("refusing batch: condemned %s at index %d seq %d, others %v, processedTop %d", rejection.ID, firstFresh, rejection.Seq, rejection.Others, h.processedTop)
			// Three more refusals await a plugin that sends the condemned
			// envelope again (one that could not read the refusal): enough
			// to reach the branch where it opens a second replacement
			// session, which must be backed off too. A plugin that set the
			// envelope aside never triggers them.
			h.rejectRemaining = 3
			h.signalLocked()
			h.mu.Unlock()
			answer(w, inline, http.StatusBadRequest, "envelope_invalid",
				"this harness declares the envelope at index "+strconv.Itoa(firstFresh)+" malformed to grade recovery; nothing in the batch was applied",
				map[string]any{"index": firstFresh, "seq": rejection.Seq})
			return
		}
		if h.rejectRemaining > 0 && h.rejected != nil {
			for index, one := range ids {
				if one.ID != h.rejected.ID {
					continue
				}
				h.rejectRemaining--
				h.rejected.Refusals++
				h.rejected.Events = append(h.rejected.Events, refusalEvent{
					At: time.Now(), Ordinal: h.sessionOrdinal, Polled: h.pollsThisSession > 0,
				})
				h.signalLocked()
				h.mu.Unlock()
				answer(w, inline, http.StatusBadRequest, "envelope_invalid",
					"this harness still declares that envelope malformed; nothing in the batch was applied",
					map[string]any{"index": index, "seq": one.Seq})
				return
			}
		}
		if h.rejected != nil && h.rejected.Accepted == nil && h.rejectRemaining == 0 {
			// The poll that finally carries the condemned id through.
			for _, one := range ids {
				if one.ID == h.rejected.ID {
					h.rejected.Accepted = &refusalEvent{
						At: time.Now(), Ordinal: h.sessionOrdinal, Polled: h.pollsThisSession > 0,
					}
					break
				}
			}
		}
		if h.garbleArmed && firstFresh >= 0 {
			h.garbleArmed = false
			record := &garbleRecord{PollsAt: h.totalPolls, GenAt: h.acceptGen, At: time.Now()}
			for _, one := range ids[firstFresh:] {
				record.IDs = append(record.IDs, one.ID)
			}
			h.rememberContentLocked(ids[firstFresh:], 0)
			h.garbled = record
			h.garbleAckArmed = inline
			h.signalLocked()
			h.mu.Unlock()
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "<html><body>this is not the hub you are looking for</body></html>")
			return
		}
		if h.garbleAckArmed && inline && firstFresh >= 0 && h.garbled != nil && carriesAny(ids, h.garbled.IDs) {
			h.garbleAckArmed = false
			now := time.Now()
			h.garbled.RetryAt = now
			h.garbled.AckAt = now
			h.garbled.AckServed = ids[len(ids)-1].Seq
			// Whatever joined the batch since the first answer is swallowed
			// too and owed again.
			for _, one := range ids[firstFresh:] {
				if !carriesAny([]batchField{one}, h.garbled.IDs) {
					h.garbled.IDs = append(h.garbled.IDs, one.ID)
				}
			}
			h.rememberContentLocked(ids[firstFresh:], 0)
			body := map[string]any{
				"error":              "this harness sends an error member that is not an object with a code; the body is malformed and its ack must not be applied",
				"envelopes":          []any{},
				"ack":                h.garbled.AckServed,
				"pollTimeoutSeconds": h.pollTimeoutSeconds,
			}
			h.signalLocked()
			h.mu.Unlock()
			writeJSONBody(w, http.StatusOK, body)
			return
		}
	}

	if h.garbled != nil && len(request.Envelopes) > 0 && carriesAny(batchIDs(request.Envelopes), h.garbled.IDs) {
		// The retry is the poll that carries a swallowed envelope again, not
		// whichever poll the plugin already had in flight.
		if h.garbled.RetryAt.IsZero() {
			h.garbled.RetryAt = time.Now()
		} else if !h.garbled.AckAt.IsZero() && h.garbled.AckRetryAt.IsZero() {
			h.garbled.AckRetryAt = time.Now()
		}
	}
	h.pollsThisSession++
	h.totalPolls++
	carried := batchIDs(request.Envelopes)
	for _, one := range carried {
		if one.Seq > h.processedTop {
			h.pollsCarryingFresh++
			break
		}
	}
	h.applyAckLocked(request.Ack)
	h.ingestLocked(request.Envelopes)
	h.signalLocked()

	// A poll saying more is answered at once when the ack covers something it
	// carried, as section 3.1.2 asks of a hub; one whose ack covers nothing
	// would only come back with the same batch, so it is held like any other.
	var lowest, highest int64
	for _, one := range carried {
		if one.Seq < 1 {
			continue
		}
		if lowest == 0 || one.Seq < lowest {
			lowest = one.Seq
		}
		highest = max(highest, one.Seq)
	}
	drain := saysMore && lowest > 0 && h.pendingAckLocked() >= lowest

	// Hold briefly when nothing is deliverable, answer at once when something
	// is, and answer 401 the moment the session stops being live, all per
	// section 3.1.2. The idle hold is deliberately short: this hub exists to
	// grade a plugin, and "up to pollTimeout" allows any earlier answer.
	holdDeadline := time.Now().Add(time.Second)
	if drain {
		holdDeadline = time.Now()
	}
	for {
		if !h.sessionLive || h.sessionToken != token {
			h.mu.Unlock()
			answer(w, inline, http.StatusUnauthorized, "session_invalid", "session is not live", nil)
			return
		}
		if h.severed {
			h.abortedRequests++
			h.signalLocked()
			h.mu.Unlock()
			panic(http.ErrAbortHandler)
		}
		if h.hasDeliverableLocked() || time.Now().After(holdDeadline) {
			break
		}
		ch := h.changed
		h.mu.Unlock()
		select {
		case <-ch:
		case <-time.After(50 * time.Millisecond):
		}
		h.mu.Lock()
	}

	envelopes := h.collectDeliverableLocked()
	ack := h.reportedAckLocked()
	// A claim is only worth settling when the answer acked exactly the batch:
	// an ack past it covers envelopes another poll carried, possibly the very
	// ones this poll left behind, and then the plugin may rightly have
	// nothing left to send. The same goes for an answer framed while another
	// poll is open.
	if saysMore && highest > 0 && ack == highest && h.pollsInFlight.Load() == 1 && h.overlappingPolls.Load() == 0 {
		h.moreFollowUp = &moreClaim{Session: h.sessionOrdinal, Top: highest, Arrivals: h.pollArrivals.Load()}
	}
	response := map[string]any{
		"envelopes":          envelopes,
		"ack":                ack,
		"pollTimeoutSeconds": h.pollTimeoutSeconds,
		"sessionExpiresAt":   h.sessionExpiresAt,
	}
	h.mu.Unlock()
	writeJSONBody(w, http.StatusOK, response)
}

func (h *mockHub) hasDeliverableLocked() bool {
	if len(h.resend) > 0 {
		return true
	}
	for _, item := range h.outbound {
		if !item.acked {
			return true
		}
	}
	return false
}

func (h *mockHub) collectDeliverableLocked() []deliverable {
	batch := make([]deliverable, 0, len(h.resend)+len(h.outbound))
	batch = append(batch, h.resend...)
	h.resend = nil
	for _, item := range h.outbound {
		if item.acked {
			continue
		}
		if item.seq == 0 {
			h.nextOutboundSeq++
			item.seq = h.nextOutboundSeq
		}
		batch = append(batch, deliverable{
			V: 1, ID: item.id, Type: item.typ, Seq: item.seq, TS: item.ts, Body: item.body,
		})
	}
	// Verbatim redeliveries carry old, lower seqs; keep the batch ascending
	// the way section 9.1 requires of every sender.
	for i := 1; i < len(batch); i++ {
		for j := i; j > 0 && batch[j].Seq < batch[j-1].Seq; j-- {
			batch[j], batch[j-1] = batch[j-1], batch[j]
		}
	}
	return batch
}

func (h *mockHub) applyAckLocked(ack *int64) {
	// An absent ack, and an explicit 0, ack nothing (section 3.1.2). An ack
	// below the recorded one is ignored, not faulted: section 9.1 obliges the
	// sender to ignore it, precisely because concurrent or re-ordered requests
	// can legally deliver acks out of order.
	if ack == nil || *ack == 0 {
		return
	}
	value := *ack
	if value < 0 {
		h.faultLocked("3.1.2", "a poll carried a negative ack (%d)", value)
		return
	}
	if value > h.nextOutboundSeq {
		h.faultLocked("3.1.2", "a poll acked seq %d, above the highest seq this hub has sent this session (%d); sequence spaces do not survive a session change (section 9.1)", value, h.nextOutboundSeq)
		return
	}
	if value <= h.highestPluginAck {
		return
	}
	h.highestPluginAck = value
	for _, item := range h.outbound {
		if item.seq != 0 && item.seq <= value {
			item.acked = true
		}
	}
}

func (h *mockHub) ingestLocked(raws []json.RawMessage) {
	previousSeq := int64(0)
	orderFaulted := false
	for index, raw := range raws {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			h.faultLocked("4", "envelope %d in a poll batch is not a JSON object", index)
			continue
		}

		id := decodeString(fields["id"])
		envelopeType := decodeString(fields["type"])
		var seq int64
		seqOK := fields["seq"] != nil && json.Unmarshal(fields["seq"], &seq) == nil
		if !seqOK || seq < 1 || id == "" || envelopeType == "" {
			h.faultLocked("4", "envelope %d in a poll batch is missing id, type, or a seq of 1 or above; a receiver cannot deduplicate, route, or order it", index)
			continue
		}
		if versionRaw, present := fields["v"]; present {
			var version int
			if json.Unmarshal(versionRaw, &version) != nil || version != 1 {
				h.faultLocked("4", "envelope seq %d declares envelope version %s; the negotiated version is 1, and an explicit 0 is not the same as absent", seq, string(versionRaw))
			}
		}

		tsRaw := string(fields["ts"])
		if _, ok := parseTS(fields["ts"]); !ok {
			h.faultLocked("4", "envelope seq %d has ts %s; a sender emits an RFC 3339 UTC timestamp", seq, tsRaw)
		}

		bodyRaw := "{}"
		if body, present := fields["body"]; present && len(body) > 0 {
			bodyRaw = string(body)
		}

		if seq <= previousSeq && !orderFaulted {
			h.faultLocked("9.1", "a poll batch was not in ascending seq order: %d after %d", seq, previousSeq)
			orderFaulted = true
		}
		previousSeq = seq

		trace("ingest session %d seq %d id %s type %s (processedTop %d)", h.sessionOrdinal, seq, id, envelopeType, h.processedTop)
		switch {
		case seq <= h.processedTop:
			// A duplicate. Within a session it must be byte-for-byte the same
			// message, and it is how the outage check sees the buffer flush.
			stored := h.bySeq[seq]
			if stored == nil {
				continue
			}
			if stored.ID != id || stored.Type != envelopeType || !tsEqual(stored.TS, tsRaw) || !jsonEqual(stored.Body, bodyRaw) {
				h.faultLocked("9.1", "the retransmission of envelope seq %d changed it; within a session a sender resends unchanged: same id, same seq, same ts, same body", seq)
				continue
			}
			h.retransmissions = append(h.retransmissions, retransmission{
				Session: h.sessionOrdinal, Seq: seq, At: time.Now(),
			})
			h.signalLocked()

		case seq == h.processedTop+1:
			if earlier, seen := h.idToSeq[id]; seen && earlier != seq {
				h.faultLocked("4", "envelope id %s was reused at seq %d after appearing at seq %d; an id is unique per message, identical only across retransmissions of that message", id, seq, earlier)
			}
			h.idToSeq[id] = seq
			envelope := &inboundEnvelope{
				Session: h.sessionOrdinal, Seq: seq, ID: id, Type: envelopeType,
				TS: tsRaw, Body: bodyRaw, ReceivedAt: time.Now(),
			}
			// Reuse across sessions, which idToSeq cannot see because it resets
			// with the session. An earlier appearance with different content
			// means the id was recycled for a fresh message, which a hub's
			// cross-session dedup would silently drop (section 4). Guarded to
			// its own session by the check above, so one reuse is one fault.
			if earlier, seen := h.idContent[id]; seen && earlier.Session != h.sessionOrdinal &&
				(earlier.Type != envelopeType || !tsEqual(earlier.TS, tsRaw) || !jsonEqual(earlier.Body, bodyRaw)) {
				h.faultLocked("4", "envelope id %s was reused in session %d for a different message than it named in session %d; an id names one message on this server in any session, because cross-session dedup (sections 8.1 and 8.3) treats equal ids as the same message and would silently drop this one", id, h.sessionOrdinal, earlier.Session)
			}
			if _, seen := h.idContent[id]; !seen {
				h.idContent[id] = envelope
			}
			if h.lastAccepted == nil {
				h.lastAccepted = map[string]uint64{}
			}
			h.acceptGen++
			h.lastAccepted[id] = h.acceptGen
			if expected, remembered := h.expectedContent[id]; remembered {
				// A provocation saw this envelope and did not ingest it;
				// coming back, it must be the same message (section 9.1),
				// and within the same session it keeps its seq less the
				// shift the quarantine allows.
				if expected.Type != envelopeType || !tsEqual(expected.TS, tsRaw) || !jsonEqualExact(expected.Body, bodyRaw) {
					h.faultLocked("9.1", "envelope %s came back changed after the hub refused or garbled the batch carrying it; a retransmission keeps id, type, ts and body, and the section 2.3 recovery moves seq alone", id)
				}
				// Within the session two seqs are legal: the original (an
				// unchanged retransmission, the resend path) or the original
				// less the shift (the envelope ahead of it was set aside).
				if expected.Session == h.sessionOrdinal && seq != expected.Seq && seq != expected.Seq-expected.Shift {
					h.faultLocked("9.1", "envelope %s came back at seq %d after the hub refused or garbled the batch carrying it; within a session it keeps seq %d (less one for an envelope set aside ahead of it, section 2.3), and only a new session renumbers", id, seq, expected.Seq)
				}
				if h.returned == nil {
					h.returned = map[string]returnRecord{}
				}
				h.returned[id] = returnRecord{Seq: seq, Session: h.sessionOrdinal}
				delete(h.expectedContent, id)
			}
			h.bySeq[seq] = envelope
			h.inbound = append(h.inbound, envelope)
			h.processedTop = seq
			h.gradeRenumberLocked(envelope)
			h.interpretLocked(envelope)
			h.signalLocked()

		default:
			// A gap. The one way a plugin lands here in this harness is by
			// replaying its buffer verbatim after a session change, which is
			// the failure mode worth naming precisely.
			if expectation, known := h.expectedRenumber[id]; known && expectation.oldSeq == seq {
				h.faultLocked("9.1", "envelope %s (type %s) was replayed with its old seq %d after a session change; sequence spaces do not survive their session, so an unacked envelope is renumbered into the new session's space, keeping its id, type, ts and body", id, envelopeType, seq)
			} else {
				h.faultLocked("9.1", "envelope seq %d arrived above a gap; expected %d next", seq, h.processedTop+1)
			}
		}
	}
}

// trace writes one line to stderr when VYSHKA_MOCK_TRACE is set, for
// debugging a stage against a candidate; silent otherwise.
func trace(format string, args ...any) {
	if os.Getenv("VYSHKA_MOCK_TRACE") == "" {
		return
	}
	fmt.Fprintf(os.Stderr, "mock: "+format+"\n", args...)
}

// carriesAny reports whether a batch carries any of the given ids.
func carriesAny(batch []batchField, ids []string) bool {
	for _, one := range batch {
		for _, id := range ids {
			if one.ID == id {
				return true
			}
		}
	}
	return false
}

// rememberContentLocked snapshots fresh envelopes a provocation is about to
// refuse or swallow, so their return can be checked for changes. shift is
// how far down their seq may legally move within the session.
func (h *mockHub) rememberContentLocked(fields []batchField, shift int64) {
	if h.expectedContent == nil {
		h.expectedContent = map[string]contentSnapshot{}
	}
	for _, one := range fields {
		if one.ID != "" {
			h.expectedContent[one.ID] = one.snapshot(h.sessionOrdinal, shift)
		}
	}
}

// jsonEqualExact is jsonEqual with numbers compared as written rather than
// through float64, so a body field above 2^53 that changed by one is a change.
func jsonEqualExact(a, b string) bool {
	var left, right any
	da := json.NewDecoder(strings.NewReader(a))
	da.UseNumber()
	db := json.NewDecoder(strings.NewReader(b))
	db.UseNumber()
	if da.Decode(&left) != nil || db.Decode(&right) != nil {
		return a == b
	}
	return reflect.DeepEqual(left, right)
}

// gradeRenumberLocked marks off an envelope the previous session left unacked,
// verifying that renumbering changed seq and nothing else.
func (h *mockHub) gradeRenumberLocked(envelope *inboundEnvelope) {
	expectation, known := h.expectedRenumber[envelope.ID]
	if !known || expectation.arrived {
		return
	}
	if expectation.typ != envelope.Type || !tsEqual(expectation.ts, envelope.TS) || !jsonEqual(expectation.body, envelope.Body) {
		h.faultLocked("9.1", "envelope %s came back renumbered but changed; renumbering moves seq alone, keeping id, type, ts and body", envelope.ID)
	}
	expectation.arrived = true
}

func (h *mockHub) interpretLocked(envelope *inboundEnvelope) {
	switch envelope.Type {
	case "manifest.publish":
		var body struct {
			Game             string `json:"game"`
			ManifestRevision *int64 `json:"manifestRevision"`
			Actions          []struct {
				Code    string         `json:"code"`
				Context string         `json:"context"`
				Params  map[string]any `json:"params"`
			} `json:"actions"`
			Contexts []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"contexts"`
			KVNamespaces []string `json:"kvNamespaces"`
			Events       []struct {
				Payload map[string]any `json:"payload"`
			} `json:"events"`
			Capabilities []string `json:"capabilities"`
		}
		if json.Unmarshal([]byte(envelope.Body), &body) != nil {
			h.faultLocked("6", "a manifest.publish body could not be decoded as an object")
			return
		}
		revision := int64(0)
		if body.ManifestRevision != nil {
			revision = *body.ManifestRevision
		}
		if h.manifest != nil && revision <= h.manifest.Revision {
			return
		}
		info := &manifestInfo{Game: body.Game, Revision: revision, KVNamespaces: body.KVNamespaces,
			Capabilities: body.Capabilities}
		for _, action := range body.Actions {
			info.Actions = append(info.Actions, manifestAction{
				Code: action.Code, Context: action.Context, Params: action.Params,
			})
		}
		for _, context := range body.Contexts {
			info.Contexts = append(info.Contexts, manifestContext{ID: context.ID, Name: context.Name})
		}
		for _, event := range body.Events {
			info.EventPayloads = append(info.EventPayloads, event.Payload)
		}
		h.manifest = info

	case "action.ack", "action.result":
		var body struct {
			ActionID string `json:"actionId"`
			OK       *bool  `json:"ok"`
		}
		if json.Unmarshal([]byte(envelope.Body), &body) != nil || body.ActionID == "" {
			h.faultLocked("7", "an %s body carried no usable actionId", envelope.Type)
			return
		}
		envelope.ActionID = body.ActionID
		track := h.actions[body.ActionID]
		if track == nil {
			track = &actionTrack{}
			h.actions[body.ActionID] = track
		}
		if envelope.Type == "action.ack" {
			track.acks++
		} else {
			if body.OK == nil {
				h.faultLocked("7", "the action.result for %s carried no boolean ok; ok is REQUIRED and decides the terminal state", body.ActionID)
			}
			track.results++
		}

	case "context.entries":
		// The answer to a context.enumerate (section 6.2). It is kept, not
		// graded here: the enumerate stage matches it to the question it
		// asked and grades it against the bounds of that section.
		h.contextReplies++
		var body struct {
			RequestID string `json:"requestId"`
		}
		if json.Unmarshal([]byte(envelope.Body), &body) != nil || body.RequestID == "" {
			h.faultLocked("6.2", "a context.entries body carried no requestId string; the reply echoes the hub's requestId, which is how a hub matches an answer to the question it asked across the poll cycle that separates them")
			return
		}
		if h.contextEntries == nil {
			h.contextEntries = map[string]*inboundEnvelope{}
		}
		if _, seen := h.contextEntries[body.RequestID]; !seen {
			h.contextEntries[body.RequestID] = envelope
		}

	case "event.batch":
		h.telemetry.batches++
		h.telemetry.events += h.validateEventBatchLocked(envelope)
		h.recordWitnessesLocked(envelope)

	case "state.players", "state.vehicles", "state.entities", "state.world":
		h.telemetry.snapshots++
		h.validateSnapshotLocked(envelope)

	case "bans.applied":
		h.interpretBansAppliedLocked(envelope)
	}
}

// recordWitnessesLocked counts the execution witnesses an event.batch
// carries against the action each names.
func (h *mockHub) recordWitnessesLocked(envelope *inboundEnvelope) {
	// Each event's data is decoded only once its type names a witness: data
	// is any object the candidate likes, and one event whose data does not fit
	// the witness shape must not hide the witnesses beside it.
	var body struct {
		Events []struct {
			T    string          `json:"t"`
			Data json.RawMessage `json:"data"`
		} `json:"events"`
	}
	if json.Unmarshal([]byte(envelope.Body), &body) != nil {
		return
	}
	for index, event := range body.Events {
		if event.T != executionWitnessType {
			continue
		}
		var data struct {
			ActionID string `json:"actionId"`
		}
		if json.Unmarshal(event.Data, &data) != nil || data.ActionID == "" {
			continue
		}
		track := h.actions[data.ActionID]
		if track == nil {
			track = &actionTrack{}
			h.actions[data.ActionID] = track
		}
		if track.executions == nil {
			track.executions = map[string]bool{}
		}
		track.executions[envelope.ID+"#"+strconv.Itoa(index)] = true
	}
}

// ---- small JSON helpers ----

func decodeString(raw json.RawMessage) string {
	var value string
	if raw == nil || json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value
}

// parseTS accepts a ts only in the form a sender is obliged to emit: an
// RFC 3339 UTC string.
func parseTS(raw json.RawMessage) (string, bool) {
	value := decodeString(raw)
	if value == "" {
		return "", false
	}
	stamp, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return "", false
	}
	if _, offset := stamp.Zone(); offset != 0 {
		return "", false
	}
	return value, true
}

// tsEqual compares two ts values as timestamps when both parse, and as raw
// text otherwise, so a resend that reformats the same instant is not punished
// while a changed instant is.
func tsEqual(a, b string) bool {
	if a == b {
		return true
	}
	var as, bs string
	if json.Unmarshal([]byte(a), &as) != nil || json.Unmarshal([]byte(b), &bs) != nil {
		return false
	}
	at, aErr := time.Parse(time.RFC3339, as)
	bt, bErr := time.Parse(time.RFC3339, bs)
	if aErr != nil || bErr != nil {
		return as == bs
	}
	return at.Equal(bt)
}

// jsonEqual compares two raw JSON values structurally, so a resend that
// re-serializes the same body with different key order is not punished.
func jsonEqual(a, b string) bool {
	if a == b {
		return true
	}
	var av, bv any
	if json.Unmarshal([]byte(a), &av) != nil || json.Unmarshal([]byte(b), &bv) != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}
