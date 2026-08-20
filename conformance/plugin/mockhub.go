package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"strings"
	"sync"
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

	// What the plugin has published and reported, decoded for the checks.
	manifest *manifestInfo
	actions  map[string]*actionTrack

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

type manifestInfo struct {
	Game     string
	Revision int64
	Actions  []manifestAction
}

type actionTrack struct {
	acks    int
	results int
}

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
		actions:         map[string]*actionTrack{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /plugin/v1/enroll", h.handleEnroll)
	mux.HandleFunc("POST /plugin/v1/session", h.handleSession)
	mux.HandleFunc("POST /plugin/v1/poll", h.handlePoll)
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
	encoded, err := json.Marshal(body)
	if err != nil {
		encoded = json.RawMessage(`{}`)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
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
	return h.queueOutbound("action.dispatch", body)
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
	ack := h.processedTop
	if h.ackLimit >= 0 && h.ackLimit < ack {
		ack = h.ackLimit
	}
	if ack < h.lastReportedAck {
		ack = h.lastReportedAck
	}
	h.lastReportedAck = ack
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

func bearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

func readBody(r *http.Request) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r.Body, 4<<20))
}

func (h *mockHub) handleEnroll(w http.ResponseWriter, r *http.Request) {
	h.abortIfSevered()
	raw, err := readBody(r)
	if err != nil {
		writeProtocolError(w, http.StatusBadRequest, "bad_request", "unreadable body")
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
	writeProtocolError(w, status, code, message)
}

func (h *mockHub) handleSession(w http.ResponseWriter, r *http.Request) {
	h.abortIfSevered()
	raw, err := readBody(r)
	if err != nil {
		writeProtocolError(w, http.StatusBadRequest, "bad_request", "unreadable body")
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

	if decodeErr != nil {
		h.faultLocked("5.3", "the session request body was not a JSON object")
		h.mu.Unlock()
		writeProtocolError(w, http.StatusBadRequest, "bad_request", "body is not JSON")
		return
	}
	if !h.enrollBurned || request.ServerID != h.serverID || request.ServerSecret != h.serverSecret {
		h.faultLocked("5.3", "the session request did not carry the serverId and serverSecret that enrollment issued")
		h.mu.Unlock()
		writeProtocolError(w, http.StatusUnauthorized, "credentials_invalid", "unknown server credentials")
		return
	}
	if request.ProtocolVersion != nil && *request.ProtocolVersion != 1 {
		h.faultLocked("5.3", "the plugin requested protocol version %d; this harness speaks version 1", *request.ProtocolVersion)
		h.mu.Unlock()
		writeProtocolError(w, http.StatusBadRequest, "protocol_version_unsupported", "this harness speaks protocol version 1")
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
		"features":           map[string]any{},
		"server": map[string]any{
			"id": h.serverID, "name": "conformance-candidate", "game": h.enrolledGame,
		},
	}
	h.mu.Unlock()
	writeJSONBody(w, http.StatusOK, response)
}

type pollWire struct {
	Ack       *int64            `json:"ack"`
	Envelopes []json.RawMessage `json:"envelopes"`
}

func (h *mockHub) handlePoll(w http.ResponseWriter, r *http.Request) {
	h.abortIfSevered()
	token := bearer(r)
	raw, err := readBody(r)
	if err != nil {
		writeProtocolError(w, http.StatusBadRequest, "bad_request", "unreadable body")
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
		writeProtocolError(w, http.StatusUnauthorized, "session_invalid", "session is not live")
		return
	}
	if decodeErr != nil {
		h.faultLocked("3.1.2", "a poll request body was not a JSON object")
		h.mu.Unlock()
		writeProtocolError(w, http.StatusBadRequest, "bad_request", "body is not JSON")
		return
	}

	h.pollsThisSession++
	h.totalPolls++
	h.applyAckLocked(request.Ack)
	h.ingestLocked(request.Envelopes)
	h.signalLocked()

	// Hold briefly when nothing is deliverable, answer at once when something
	// is, and answer 401 the moment the session stops being live, all per
	// section 3.1.2. The idle hold is deliberately short: this hub exists to
	// grade a plugin, and "up to pollTimeout" allows any earlier answer.
	holdDeadline := time.Now().Add(time.Second)
	for {
		if !h.sessionLive || h.sessionToken != token {
			h.mu.Unlock()
			writeProtocolError(w, http.StatusUnauthorized, "session_invalid", "session is not live")
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
	response := map[string]any{
		"envelopes":          envelopes,
		"ack":                h.reportedAckLocked(),
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
				h.faultLocked("4", "envelope id %s was reused at seq %d after appearing at seq %d; an id is unique per message within a session, identical only across retransmissions of that message", id, seq, earlier)
			}
			h.idToSeq[id] = seq
			envelope := &inboundEnvelope{
				Session: h.sessionOrdinal, Seq: seq, ID: id, Type: envelopeType,
				TS: tsRaw, Body: bodyRaw, ReceivedAt: time.Now(),
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
		info := &manifestInfo{Game: body.Game, Revision: revision}
		for _, action := range body.Actions {
			info.Actions = append(info.Actions, manifestAction{
				Code: action.Code, Context: action.Context, Params: action.Params,
			})
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
