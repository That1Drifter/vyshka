package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/That1Drifter/vyshka/hub/internal/id"
	"github.com/That1Drifter/vyshka/hub/internal/schema"
	"github.com/That1Drifter/vyshka/hub/store"
)

// Envelope types of the context enumeration exchange (spec section 6.2).
const (
	envelopeTypeContextEnumerate = "context.enumerate"
	envelopeTypeContextEntries   = "context.entries"
)

// Bounds of a context.entries reply (spec section 6.2): the same entry and
// byte caps a snapshot has, and the field lengths the section states.
const (
	maxContextEntries      = 5000
	maxContextEntriesBytes = 256 << 10
	maxContextRequestID    = 128
	maxContextEntryKey     = 128
	maxContextEntryLabel   = 200
	maxContextIDLength     = 64
)

// contextEnumerateBody is what the hub asks with (spec section 6.2).
type contextEnumerateBody struct {
	RequestID string `json:"requestId"`
	Context   string `json:"context"`
}

// contextEntriesBody is a plugin's reply as it arrives. entries is raw so it
// can be handed back verbatim and whole; reason is raw because a JSON null
// there reads as no reason, not as a type error (section 6.4).
type contextEntriesBody struct {
	RequestID json.RawMessage `json:"requestId"`
	Context   json.RawMessage `json:"context"`
	Entries   json.RawMessage `json:"entries"`
	Reason    json.RawMessage `json:"reason"`
}

// contextEntry is one member of a reply, decoded for validation only. label
// is a pointer so a JSON null is told apart from an empty string: a null is
// not a display string, and would otherwise decode into "" without complaint.
type contextEntry struct {
	ReferenceKey string          `json:"referenceKey"`
	Label        *string         `json:"label"`
	Position     json.RawMessage `json:"position"`
	Data         json.RawMessage `json:"data"`
}

// contextEnumeration is one answered enumeration: what the Admin API hands
// out and what the cache holds.
type contextEnumeration struct {
	Context      string
	Entries      json.RawMessage
	Reason       *string
	EnumeratedAt time.Time
}

// contextEntriesView is the Admin API representation of an enumeration
// (spec section 6.2): the plugin's entries verbatim, the hub's metadata
// beside them.
type contextEntriesView struct {
	Context      string          `json:"context"`
	Entries      json.RawMessage `json:"entries"`
	Reason       *string         `json:"reason"`
	EnumeratedAt time.Time       `json:"enumeratedAt"`
}

func newContextEntriesView(enumeration *contextEnumeration) contextEntriesView {
	return contextEntriesView{
		Context:      enumeration.Context,
		Entries:      enumeration.Entries,
		Reason:       enumeration.Reason,
		EnumeratedAt: enumeration.EnumeratedAt,
	}
}

// preparedContextEntries is one context.entries envelope after body
// validation: the enumeration it carries, or the faults that refuse it. Either
// way it is delivered to whoever asked, so a plugin that answers wrongly fails
// the read at once instead of leaving it to time out.
type preparedContextEntries struct {
	requestID   string
	enumeration *contextEnumeration
	faults      []schema.Fault
}

// prepareContextEntries validates every context.entries body in a batch,
// keyed by batch index, before classification, for the reason the other
// prepare functions give: validity depends only on content, while which
// envelopes are newly accepted is only known inside the store's transaction.
func prepareContextEntries(envelopes []inboundEnvelope, now time.Time) map[int]preparedContextEntries {
	var prepared map[int]preparedContextEntries
	for index, e := range envelopes {
		if e.Type != envelopeTypeContextEntries {
			continue
		}
		if prepared == nil {
			prepared = make(map[int]preparedContextEntries)
		}
		prepared[index] = validateContextEntries(e, now)
	}
	return prepared
}

// validateContextEntries checks one reply against section 6.2. A reply with
// no readable requestId cannot be matched to a question and is reported with
// an empty requestId, which delivery treats as unsolicited.
func validateContextEntries(e inboundEnvelope, now time.Time) preparedContextEntries {
	var faults []schema.Fault
	fault := func(path, format string, args ...any) {
		faults = append(faults, schema.Fault{Path: path, Message: fmt.Sprintf(format, args...)})
	}
	tooLong := func(value string, limit int) bool {
		return utf8.RuneCountInString(value) > limit
	}

	// Every member is decoded raw and typed here one by one, so a reply
	// whose context (say) is a number still yields its requestId: the
	// reader waiting on it is then failed with the fault at once rather
	// than left to time out on a reply the hub could not match.
	var body contextEntriesBody
	if len(e.Body) == 0 || json.Unmarshal(e.Body, &body) != nil {
		fault("", "body does not match the context.entries shape")
		return preparedContextEntries{faults: faults}
	}
	var requestID string
	if err := json.Unmarshal(body.RequestID, &requestID); err != nil {
		requestID = ""
	}
	requestID = boundRequestID(requestID)
	if requestID == "" {
		fault("requestId", "requestId is required, the hub's own of at most %d characters", maxContextRequestID)
		return preparedContextEntries{faults: faults}
	}
	if len(e.Body) > maxContextEntriesBytes {
		fault("", "a context.entries body is at most %d bytes, got %d", maxContextEntriesBytes, len(e.Body))
		return preparedContextEntries{requestID: requestID, faults: faults}
	}
	var contextID string
	if err := json.Unmarshal(body.Context, &contextID); err != nil || contextID == "" || tooLong(contextID, maxContextIDLength) {
		fault("context", "context must echo the enumerated context id, a string of at most %d characters", maxContextIDLength)
	}
	if len(body.Entries) == 0 || string(body.Entries) == "null" {
		fault("entries", "entries is required; an empty array is how a plugin says it has nothing to offer")
		return preparedContextEntries{requestID: requestID, faults: faults}
	}
	var entries []contextEntry
	if err := json.Unmarshal(body.Entries, &entries); err != nil {
		fault("entries", "entries does not match the entry shape: %s", err.Error())
		return preparedContextEntries{requestID: requestID, faults: faults}
	}
	if len(entries) > maxContextEntries {
		fault("entries", "a reply carries at most %d entries, got %d", maxContextEntries, len(entries))
		return preparedContextEntries{requestID: requestID, faults: faults}
	}
	for i, entry := range entries {
		path := "entries[" + strconv.Itoa(i) + "]"
		if entry.ReferenceKey == "" || tooLong(entry.ReferenceKey, maxContextEntryKey) {
			fault(path+".referenceKey", "referenceKey must be a non-empty string of at most %d characters", maxContextEntryKey)
		}
		switch {
		case entry.Label == nil:
			fault(path+".label", "label is required, a display string of at most %d characters", maxContextEntryLabel)
		case tooLong(*entry.Label, maxContextEntryLabel):
			fault(path+".label", "label is longer than %d characters", maxContextEntryLabel)
		}
		faults = append(faults, validatePosition(entry.Position, path+".position")...)
		faults = append(faults, validateEntryData(entry.Data, path+".data")...)
	}
	var reason *string
	if len(body.Reason) > 0 && string(body.Reason) != "null" {
		var text string
		if err := json.Unmarshal(body.Reason, &text); err != nil {
			fault("reason", "reason must be a string when present")
		} else {
			reason = &text
		}
	}
	if len(faults) > 0 {
		return preparedContextEntries{requestID: requestID, faults: faults}
	}
	return preparedContextEntries{
		requestID: requestID,
		enumeration: &contextEnumeration{
			Context:      contextID,
			Entries:      body.Entries,
			Reason:       reason,
			EnumeratedAt: now,
		},
	}
}

// boundRequestID returns a requestId the hub could have assigned, or "" for
// one it could not have (empty, or past the bound the hub itself respects).
func boundRequestID(requestID string) string {
	if requestID == "" || utf8.RuneCountInString(requestID) > maxContextRequestID {
		return ""
	}
	return requestID
}

// enumerationKey names one cacheable question: a context of one server under
// one manifest revision. The revision is part of the key so a republished
// manifest invalidates every answer taken against the old one, whatever the
// cache bound says: the plugin that republished may list different members.
type enumerationKey struct {
	serverID  string
	contextID string
	revision  int64
}

// pendingEnumeration is one question in flight: the requestId it was asked
// with, and the answer once it arrives. Every read of the same key while it
// is in flight waits on the same one (section 6.2: concurrent reads SHOULD
// share one question).
type pendingEnumeration struct {
	key       enumerationKey
	requestID string
	// deadline is when the question is given up on: a pending past it is
	// replaced rather than joined, so a question nobody is waiting on any
	// more (every reader left) cannot pin a stale requestId forever.
	deadline time.Time
	done     chan struct{}
	// Exactly one of these is set when done is closed.
	enumeration *contextEnumeration
	err         error
}

// enumerations holds the in-flight questions and the cache of answers. It is
// in-process, like the waiters: one hub per database is the supported
// deployment, and a second process would simply ask its own question.
type enumerations struct {
	mu       sync.Mutex
	pending  map[string]*pendingEnumeration         // by requestId
	inflight map[enumerationKey]*pendingEnumeration // one per key
	cache    map[enumerationKey]*contextEnumeration
	ttl      time.Duration
	timeout  time.Duration
}

func newEnumerations(ttl, timeout time.Duration) *enumerations {
	return &enumerations{
		pending:  map[string]*pendingEnumeration{},
		inflight: map[enumerationKey]*pendingEnumeration{},
		cache:    map[enumerationKey]*contextEnumeration{},
		ttl:      ttl,
		timeout:  timeout,
	}
}

// cached returns the answer held for a key if it is younger than the cache
// bound, and nil otherwise.
func (e *enumerations) cached(key enumerationKey, now time.Time) *contextEnumeration {
	e.mu.Lock()
	defer e.mu.Unlock()
	if held := e.cache[key]; held != nil && now.Sub(held.EnumeratedAt) < e.ttl {
		return held
	}
	return nil
}

// join returns the question in flight for a key, or a fresh one with started
// true, in which case the caller owes the plugin the envelope (and, should
// queueing it fail, a call to abandon).
func (e *enumerations) join(key enumerationKey, now time.Time) (pending *pendingEnumeration, started bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	// Every question past its deadline is completed and forgotten first,
	// this key's included: a reader may have left before its own timer
	// fired (a cancelled request), and nothing else would ever close a
	// question nobody is waiting on.
	e.sweep(now)
	if current := e.inflight[key]; current != nil {
		return current, false
	}
	pending = &pendingEnumeration{
		key:       key,
		requestID: id.NewAt(now),
		deadline:  now.Add(e.timeout),
		done:      make(chan struct{}),
	}
	e.pending[pending.requestID] = pending
	e.inflight[key] = pending
	return pending, true
}

// sweep completes every question whose deadline has passed with the
// timeout, so a reader still on it is answered and its requestId stops
// being one the hub is waiting on. The caller holds the lock.
func (e *enumerations) sweep(now time.Time) {
	for _, stale := range e.pending {
		if now.Before(stale.deadline) {
			continue
		}
		stale.err = errEnumerationTimeout
		e.forget(stale)
		close(stale.done)
	}
}

// abandon fails a question that could not be asked, so anyone who joined it
// is answered rather than left to time out.
func (e *enumerations) abandon(pending *pendingEnumeration, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.pending[pending.requestID] != pending {
		return
	}
	pending.err = err
	e.forget(pending)
	close(pending.done)
}

// expire gives up on a question after the hub's bound. The requestId stays
// unknown from now on, so a reply arriving later takes the acked-and-ignored
// path of section 6.2.
func (e *enumerations) expire(pending *pendingEnumeration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.pending[pending.requestID] != pending {
		return
	}
	pending.err = errEnumerationTimeout
	e.forget(pending)
	close(pending.done)
}

// deliver hands a reply to the question it echoes. It reports false for a
// reply nobody is waiting on, which the poll logs and otherwise ignores. A
// reply naming another server's requestId is unsolicited too: a plugin
// answers only what it was asked.
func (e *enumerations) deliver(serverID string, prepared preparedContextEntries, now time.Time) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sweep(now)
	pending := e.pending[prepared.requestID]
	if pending == nil || pending.key.serverID != serverID {
		return false
	}
	switch {
	case len(prepared.faults) > 0:
		pending.err = &enumerationInvalidError{faults: prepared.faults}
	case prepared.enumeration.Context != pending.key.contextID:
		// The echo is how a hub matches an answer to its question across the
		// poll cycle; a reply that echoes the requestId but another context
		// is an answer to something else.
		pending.err = &enumerationInvalidError{faults: []schema.Fault{{Path: "context",
			Message: fmt.Sprintf("the reply echoes context %q, want the enumerated %q", prepared.enumeration.Context, pending.key.contextID)}}}
	default:
		pending.enumeration = prepared.enumeration
		e.prune(now)
		e.cache[pending.key] = prepared.enumeration
	}
	e.forget(pending)
	close(pending.done)
	return true
}

// forget unregisters a question. The caller holds the lock.
func (e *enumerations) forget(pending *pendingEnumeration) {
	delete(e.pending, pending.requestID)
	if e.inflight[pending.key] == pending {
		delete(e.inflight, pending.key)
	}
}

// prune drops answers past the cache bound, so a hub that has seen many
// manifest revisions or many contexts does not hold every answer it ever
// got. The caller holds the lock; the map is small enough to walk on every
// insert.
func (e *enumerations) prune(now time.Time) {
	for key, held := range e.cache {
		if now.Sub(held.EnumeratedAt) >= e.ttl {
			delete(e.cache, key)
		}
	}
}

var errEnumerationTimeout = errors.New("no context.entries echoing the question arrived within the hub's bound")

// enumerationInvalidError carries the faults of a reply the hub refused, so
// the read that asked can say what was wrong with it.
type enumerationInvalidError struct {
	faults []schema.Fault
}

func (err *enumerationInvalidError) Error() string {
	return "the plugin's context.entries reply was outside the bounds of section 6.2: " + err.faults[0].String()
}

// builtinContexts are the contexts every hub knows without a declaration
// (spec section 6.2). Their members are the state snapshots of section 8.3,
// so they are never enumerated, and a manifest may not declare one as its
// own (section 6.4).
var builtinContexts = map[string]bool{"world": true, "player": true, "vehicle": true, "object": true}

// declaredContext reports whether a stored manifest declares a context id.
func declaredContext(manifest json.RawMessage, contextID string) bool {
	var body struct {
		Contexts []manifestContext `json:"contexts"`
	}
	if err := json.Unmarshal(manifest, &body); err != nil {
		return false
	}
	for _, declared := range body.Contexts {
		if declared.ID == contextID {
			return true
		}
	}
	return false
}

// handleEnumerateContext answers with the members of one custom context
// (spec section 6.2): from the cache when it holds a young enough answer
// against the current manifest, otherwise by asking the plugin and holding
// this request until the echoing reply arrives or the hub's bound passes.
func (s *Server) handleEnumerateContext(w http.ResponseWriter, r *http.Request) {
	server, ok := s.lookupServer(w, r)
	if !ok {
		return
	}
	contextID := r.PathValue("contextId")

	manifest, err := s.store.Manifest(r.Context(), server.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, codeNotFound, "this server has not published a manifest")
		return
	case err != nil:
		s.writeInternalError(w, r, err)
		return
	}
	if contextID == "" || utf8.RuneCountInString(contextID) > maxContextIDLength || builtinContexts[contextID] || !declaredContext(manifest.Body, contextID) {
		writeError(w, http.StatusNotFound, codeNotFound,
			"this server's manifest declares no context "+strconv.Quote(contextID)+"; the built-in contexts are read as state snapshots")
		return
	}

	now := time.Now().UTC()
	key := enumerationKey{serverID: server.ID, contextID: contextID, revision: manifest.Revision}
	refresh := r.URL.Query().Get("refresh")
	if refresh != "true" && refresh != "1" {
		if held := s.enumerations.cached(key, now); held != nil {
			writeJSON(w, http.StatusOK, newContextEntriesView(held))
			return
		}
	}

	// A question for a plugin that is not there would be answered into a hub
	// that no longer wants the answer, so it is refused rather than queued.
	live, err := s.store.LiveSession(r.Context(), server.ID)
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}
	if live == nil {
		writeError(w, http.StatusConflict, codeLinkDown,
			"this server has no live session, so its plugin cannot be asked")
		return
	}

	pending, started := s.enumerations.join(key, now)
	if started {
		body, err := json.Marshal(contextEnumerateBody{RequestID: pending.requestID, Context: contextID})
		if err != nil {
			s.enumerations.abandon(pending, err)
			s.writeInternalError(w, r, err)
			return
		}
		_, err = s.store.QueueEnvelope(r.Context(), server.ID, envelopeTypeContextEnumerate, body, outboundQueueLimit)
		switch {
		case errors.Is(err, store.ErrOutboundQueueFull):
			s.enumerations.abandon(pending, err)
			writeError(w, http.StatusConflict, codeOutboundQueueFull,
				"this server already has "+strconv.Itoa(outboundQueueLimit)+" unacked envelopes queued")
			return
		case errors.Is(err, store.ErrNotFound):
			s.enumerations.abandon(pending, err)
			writeError(w, http.StatusNotFound, codeNotFound, "no such server")
			return
		case err != nil:
			s.enumerations.abandon(pending, err)
			s.writeInternalError(w, r, err)
			return
		}
		// Wake the poll holding for this server, so the question goes out now.
		s.waiters.notify(server.ID)
		s.log.Info("context enumeration asked",
			"serverId", server.ID, "context", contextID, "requestId", pending.requestID)
	}

	timer := time.NewTimer(time.Until(pending.deadline))
	defer timer.Stop()
	select {
	case <-pending.done:
	case <-timer.C:
		s.enumerations.expire(pending)
		<-pending.done
	case <-r.Context().Done():
		// The reader left; the question stays in flight for anyone else
		// waiting on it, and its answer still lands in the cache.
		return
	}

	var invalid *enumerationInvalidError
	switch {
	case pending.err == nil:
		writeJSON(w, http.StatusOK, newContextEntriesView(pending.enumeration))
	case errors.Is(pending.err, errEnumerationTimeout):
		s.log.Warn("context enumeration timed out",
			"serverId", server.ID, "context", contextID, "requestId", pending.requestID)
		writeError(w, http.StatusGatewayTimeout, codeEnumerationTimeout,
			"the plugin did not answer the enumeration within "+s.enumerations.timeout.String())
	case errors.As(pending.err, &invalid):
		s.log.Warn("context enumeration answered outside the section 6.2 bounds",
			"serverId", server.ID, "context", contextID, "requestId", pending.requestID,
			"fault", invalid.faults[0].String())
		writeErrorDetails(w, http.StatusBadGateway, codeEnumerationInvalid,
			invalid.Error(), map[string]any{"errors": invalid.faults})
	default:
		s.writeInternalError(w, r, pending.err)
	}
}
