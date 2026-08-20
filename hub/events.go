package hub

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/That1Drifter/vyshka/hub/internal/schema"
	"github.com/That1Drifter/vyshka/hub/store"
)

// Envelope types telemetry ingest uses (spec section 8.1).
const (
	envelopeTypeEventBatch  = "event.batch"
	envelopeTypeEventReject = "event.reject"
)

// Event ingest limits.
const (
	// maxEventsPerBatch is the batch bound of spec section 8.1.
	maxEventsPerBatch = 200
	// maxEventsPerPoll bounds how many events one poll may carry across all of
	// its batches. Without it an authenticated plugin could fill a 1 MiB body
	// with tiny events and force a five-figure row insert inside the store's
	// transaction, which on SQLite's single connection is the whole hub's
	// critical section.
	maxEventsPerPoll = 1000
	// maxEventType bounds `t`, in the same unit as the rest of the protocol's
	// identifier limits.
	maxEventType = 128
	// maxEventDataBytes caps one event's `data`, measured on the raw bytes of
	// the serialized value. It is well under the action result cap because
	// events are the high-volume channel: the point of an event is to be one
	// of thousands, not to carry a payload.
	maxEventDataBytes = 16 << 10
	// maxEventFaults caps how many faults an event.reject echoes back, as the
	// manifest rejection does.
	maxEventFaults = 20
	// How many event.reject notices one poll may queue is bounded too, by the
	// budget every notice kind shares: see maxNoticesPerPoll in poll.go.
	//
	// maxEventClockSkew is how far ahead of the hub's own clock an event's `ts`
	// may sit before receipt time is used instead.
	maxEventClockSkew = time.Hour
)

// Event query limits (spec section 8.5).
const (
	defaultEventPageSize = 100
	maxEventPageSize     = 500
	// maxEventTypeFilters bounds how many `type` terms one query may OR
	// together, because each one becomes a term in the SQL the hub builds.
	maxEventTypeFilters = 20
)

// RetentionRule keeps every event whose type matches Pattern for TTL. Pattern
// is an exact event type, a namespace with `.*` appended (`core.player.*`), or
// `*` for everything. Retention is hub configuration, not protocol (spec
// section 8.4).
type RetentionRule struct {
	Pattern string
	TTL     time.Duration
}

// DefaultEventRetention is the reference configuration of spec section 8.4.
// Chat is kept three times as long as everything else because it is the record
// an operator reaches for when a ban is appealed weeks after the fact.
func DefaultEventRetention() []RetentionRule {
	return []RetentionRule{
		{Pattern: "*", TTL: 30 * 24 * time.Hour},
		{Pattern: "core.player.chat", TTL: 90 * 24 * time.Hour},
	}
}

// retentionFor resolves how long one event type is kept. The most specific
// matching rule wins, so an operator can lengthen one type without restating
// the catch-all.
func (s *Server) retentionFor(eventType string) time.Duration {
	best, bestScore := time.Duration(0), -1
	for _, rule := range s.cfg.EventRetention {
		if rule.TTL <= 0 {
			// A non-positive TTL would mean "delete on arrival", which is a
			// misconfiguration rather than a policy: refusing to store an event
			// type at all is a different feature, and one nobody asked for.
			continue
		}
		if score := retentionMatch(rule.Pattern, eventType); score > bestScore {
			best, bestScore = rule.TTL, score
		}
	}
	if bestScore < 0 {
		// Configured rules that match nothing must not mean "keep forever": an
		// unbounded table is the one retention outcome an operator cannot undo.
		return defaultRetentionTTL
	}
	return best
}

// defaultRetentionTTL backs a type no configured rule matches.
const defaultRetentionTTL = 30 * 24 * time.Hour

// retentionMatch scores how specifically a pattern matches a type: -1 for no
// match, 0 for the catch-all, the length of the matched prefix for a namespace
// rule, and one past the type's own length for an exact rule. Scoring by
// matched length is what makes `core.player.*` beat `core.*` without the
// configuration having to be ordered.
func retentionMatch(pattern, eventType string) int {
	switch {
	case pattern == "*":
		return 0
	case pattern == eventType:
		return len(eventType) + 1
	case strings.HasSuffix(pattern, ".*"):
		prefix := strings.TrimSuffix(pattern, "*")
		if strings.HasPrefix(eventType, prefix) {
			return len(prefix)
		}
	}
	return -1
}

// eventBatchBody is the event.batch body (spec section 8.1). `events` is a
// pointer so that an absent array and an empty one stay distinguishable: the
// first is a body that is not an event batch at all, the second is a plugin
// flushing nothing, which is legal and stores nothing.
type eventBatchBody struct {
	Events *[]inboundEvent `json:"events"`
}

// inboundEvent is one event as the plugin sends it.
//
// `ts` is raw rather than a string, and deliberately not a time.Time. Section
// 8.1 forbids refusing an event over `ts` alone, and a typed field would break
// that at the decoder before any rule of ours ran: one `"ts": 1755367200` in a
// batch of two hundred good events would fail the whole body's unmarshal and
// cost every one of them.
type inboundEvent struct {
	T    string          `json:"t"`
	TS   json.RawMessage `json:"ts"`
	Data json.RawMessage `json:"data"`
}

// validEventType reports whether t is a `{namespace}.{name}` event type.
//
// The grammar is enforced rather than merely documented because this string is
// what the `events:read:example-mod.*` scopes of section 10 and the webhook
// filters of section 11 match on. A type with no namespace could not be granted
// or filtered by namespace at all, and one carrying `*`, whitespace, or a dot
// at either end would make those patterns ambiguous. The alphabet can be
// widened later without breaking anyone; narrowing it could not.
func validEventType(t string) bool {
	// Length first, in bytes: every legal type is ASCII, so this is also its
	// character count, and checking it before splitting bounds the work a
	// pathological `t` can buy.
	if t == "" || len(t) > maxEventType {
		return false
	}
	segments := strings.Split(t, ".")
	if len(segments) < 2 {
		return false
	}
	for _, segment := range segments {
		if segment == "" {
			return false
		}
		for i := range len(segment) {
			c := segment[i]
			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
				c == '_', c == '-':
			default:
				return false
			}
		}
	}
	return true
}

// eventTimestamp reads one event's `ts`. It never fails: an absent,
// unparseable, or implausibly future timestamp yields nil, and the store fills
// in receipt time. Section 4's rule that a wrong clock must not cost a batch of
// real events applies to the events as much as to the envelope carrying them.
//
// The future bound is the part that is not mere tolerance. An event stamped a
// year ahead would pin itself to the top of the operator's feed until it was
// pruned, pushing everything real underneath it, and nobody can fix another
// machine's clock retroactively.
func eventTimestamp(raw json.RawMessage, now time.Time) *time.Time {
	var text string
	if len(raw) == 0 || json.Unmarshal(raw, &text) != nil {
		// Absent, null, or a JSON type that is not a string at all. All three
		// mean the same thing here: no timestamp this hub can use.
		return nil
	}
	parsed, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return nil
	}
	utc := parsed.UTC().Truncate(time.Millisecond)
	if utc.After(now.Add(maxEventClockSkew)) {
		return nil
	}
	return &utc
}

// preparedEvents is one event.batch envelope after body validation: either the
// records to store or the rejection notice that answers it. Both empty is a
// valid empty batch.
type preparedEvents struct {
	events []store.NewEvent
	reject *store.Notice
}

// prepareEvents validates every event.batch body in a batch, keyed by batch
// index, running before classification for the same reason prepareManifests
// does: validity depends only on content, while which envelopes are newly
// accepted is only known inside the store's transaction.
//
// The per-poll budget is deliberately not applied here. Charging it during
// validation would charge it for duplicates too, which store nothing, and a
// poll that retransmitted a few already-acked batches alongside new ones could
// push a real batch over the line and lose its events. The budget is arithmetic
// rather than parsing, so it costs nothing to apply later, against the
// envelopes that are actually being accepted.
func (s *Server) prepareEvents(envelopes []inboundEnvelope, now time.Time) map[int]preparedEvents {
	var prepared map[int]preparedEvents
	for index, e := range envelopes {
		if e.Type != envelopeTypeEventBatch {
			continue
		}
		if prepared == nil {
			prepared = make(map[int]preparedEvents)
		}

		events, faults := s.validateEventBatch(e.Body, now)
		if len(faults) > 0 {
			notice := newEventReject(e.ID, faults)
			prepared[index] = preparedEvents{reject: &notice}
			continue
		}
		prepared[index] = preparedEvents{events: events}
	}
	return prepared
}

// newEventBudgetReject answers a batch that fit the protocol but not what was
// left of this poll's event budget. The plugin can fix it by spreading its
// backlog across polls, which is exactly what the notice says.
func newEventBudgetReject(envelopeID string) store.Notice {
	return newEventReject(envelopeID, []schema.Fault{{Path: "events", Message: fmt.Sprintf(
		"this poll has already used its budget of %d events; send this batch on a later poll",
		maxEventsPerPoll)}})
}

// validateEventBatch checks one event.batch body against section 8.1. It
// returns the records to store, or the faults that reject the batch.
//
// The batch is rejected whole, never partially. Partial acceptance would leave
// the plugin unable to tell which of its events landed, because the ack is per
// envelope and the protocol has no per-event receipt; it would have to either
// drop everything it just sent or resend all of it, which is the choice a whole
// rejection gives it anyway, minus the ambiguity.
func (s *Server) validateEventBatch(body json.RawMessage, now time.Time) ([]store.NewEvent, []schema.Fault) {
	if len(body) == 0 {
		return nil, []schema.Fault{{Path: "", Message: "body is required"}}
	}
	var batch eventBatchBody
	if err := json.Unmarshal(body, &batch); err != nil {
		return nil, []schema.Fault{{Path: "",
			Message: "body does not match the event batch shape: " + err.Error()}}
	}
	if batch.Events == nil {
		return nil, []schema.Fault{{Path: "events", Message: "events is required"}}
	}
	inbound := *batch.Events
	if len(inbound) > maxEventsPerBatch {
		// One fault and no per-event pass: the batch is refused either way, and
		// walking a hundred thousand events to say so in more detail would let
		// a sender buy CPU with one envelope.
		return nil, []schema.Fault{{Path: "events", Message: fmt.Sprintf(
			"a batch carries at most %d events, got %d", maxEventsPerBatch, len(inbound))}}
	}

	var faults []schema.Fault
	fault := func(path, format string, args ...any) {
		faults = append(faults, schema.Fault{Path: path, Message: fmt.Sprintf(format, args...)})
	}

	events := make([]store.NewEvent, 0, len(inbound))
	for i, event := range inbound {
		path := fmt.Sprintf("events[%d]", i)
		if !validEventType(event.T) {
			fault(path+".t",
				"t must be a {namespace}.{name} type of at most %d characters drawn from "+
					"letters, digits, _ and -, got %q",
				maxEventType, truncateUTF8(event.T, 64))
			continue
		}
		// The action and server namespaces belong to the hub's own lifecycle
		// notifications (spec sections 8.1 and 11.1). Telemetry admitted there
		// would reach webhook receivers looking exactly like the hub's word.
		if strings.HasPrefix(event.T, "action.") || strings.HasPrefix(event.T, "server.") {
			fault(path+".t",
				"the action and server namespaces are reserved for the hub's lifecycle notifications; core server telemetry lives under core.server.*")
			continue
		}

		data := json.RawMessage(`{}`)
		if len(event.Data) > 0 && string(event.Data) != "null" {
			if len(event.Data) > maxEventDataBytes {
				fault(path+".data", "data is larger than the %d byte cap", maxEventDataBytes)
				continue
			}
			var object map[string]json.RawMessage
			if err := json.Unmarshal(event.Data, &object); err != nil {
				fault(path+".data", "data must be a JSON object")
				continue
			}
			data = event.Data
		}

		events = append(events, store.NewEvent{
			Type:       event.T,
			OccurredAt: eventTimestamp(event.TS, now),
			Data:       data,
			Retention:  s.retentionFor(event.T),
		})
	}

	if len(faults) > 0 {
		return nil, faults
	}
	return events, nil
}

// eventRejectBody is what a hub sends back for an event batch it refused (spec
// section 8.1), shaped like the manifest rejection of section 6.4 so a plugin
// handles both the same way.
type eventRejectBody struct {
	// EnvelopeID names the event.batch envelope this rejection answers.
	EnvelopeID string `json:"envelopeId"`
	// Errors is capped at maxEventFaults entries.
	Errors []schema.Fault `json:"errors"`
}

// newEventReject builds the event.reject notice for one refused batch.
func newEventReject(envelopeID string, faults []schema.Fault) store.Notice {
	if len(faults) > maxEventFaults {
		faults = faults[:maxEventFaults]
	}
	encoded, err := json.Marshal(eventRejectBody{EnvelopeID: envelopeID, Errors: faults})
	if err != nil {
		// Faults are strings all the way down; this cannot happen, and a
		// degraded notice beats a dropped one if it somehow does.
		degraded, _ := json.Marshal(eventRejectBody{
			EnvelopeID: envelopeID,
			Errors:     []schema.Fault{{Path: "", Message: "the event batch was rejected"}},
		})
		encoded = degraded
	}
	return store.Notice{Type: envelopeTypeEventReject, Body: encoded}
}

// eventView is the Admin API representation of a stored event (spec section
// 8.5). It reports both clocks rather than reconciling them: occurredAt is what
// the game server said, receivedAt is what the hub witnessed, and an operator
// chasing a discrepancy needs to see the two apart.
type eventView struct {
	ID         string          `json:"id"`
	ServerID   string          `json:"serverId"`
	Type       string          `json:"type"`
	OccurredAt time.Time       `json:"occurredAt"`
	ReceivedAt time.Time       `json:"receivedAt"`
	Data       json.RawMessage `json:"data"`
}

type eventsResponse struct {
	Events []eventView `json:"events"`
	// NextCursor is absent on the last page, which is how a client knows to
	// stop without having to fetch an empty one.
	NextCursor string `json:"nextCursor,omitempty"`
}

// handleListEvents answers one page of a server's event feed (spec section
// 8.5). Core and custom events are read through exactly the same path: there is
// no branch here that could privilege one over the other.
func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	server, ok := s.lookupServer(w, r)
	if !ok {
		return
	}
	parameters := r.URL.Query()

	filters, ok := s.eventFilters(w, r, parameters["type"])
	if !ok {
		return
	}
	since, ok := parseTimeParam(w, parameters.Get("since"), "since")
	if !ok {
		return
	}
	until, ok := parseTimeParam(w, parameters.Get("until"), "until")
	if !ok {
		return
	}
	limit, ok := parseLimitParam(w, parameters.Get("limit"), defaultEventPageSize, maxEventPageSize)
	if !ok {
		return
	}
	after, ok := parseEventCursor(w, parameters.Get("cursor"))
	if !ok {
		return
	}

	// One row past the page, so the presence of a next page is known without a
	// second query and without ever handing out a cursor to nothing.
	found, err := s.store.Events(r.Context(), store.EventQuery{
		ServerID: server.ID,
		Types:    filters,
		Since:    since,
		Until:    until,
		Limit:    limit + 1,
		After:    after,
	})
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}

	response := eventsResponse{Events: make([]eventView, 0, min(len(found), limit))}
	if len(found) > limit {
		response.NextCursor = encodeEventCursor(found[limit-1])
		found = found[:limit]
	}
	for _, event := range found {
		data := event.Data
		if len(data) == 0 {
			data = json.RawMessage(`{}`)
		}
		response.Events = append(response.Events, eventView{
			ID:         event.ID,
			ServerID:   event.ServerID,
			Type:       event.Type,
			OccurredAt: event.OccurredAt,
			ReceivedAt: event.ReceivedAt,
			Data:       data,
		})
	}
	writeJSON(w, http.StatusOK, response)
}

// eventFilters resolves what the caller may read from what it asked for, which
// is where an `events:read:{pattern}` scope stops being a gate and starts
// shaping the answer (spec section 10).
//
// The two cases are deliberately different. An explicit `type` term the token
// does not fully cover is refused, because silently narrowing an explicit
// question answers it with a feed that looks complete and is not. A request
// with no `type` at all is narrowed to the grant instead, because there the
// caller asked for "what I can see" and that is what it gets.
func (s *Server) eventFilters(w http.ResponseWriter, r *http.Request, requested []string) ([]store.EventTypeFilter, bool) {
	filters, ok := parseEventTypeFilters(w, requested)
	if !ok {
		return nil, false
	}

	caller := principalFrom(r.Context())
	if len(requested) > 0 {
		// Coverage is checked after parsing so that a malformed pattern is
		// still answered as the bad request it is, rather than as a denial that
		// would send the operator looking at their scopes.
		for _, value := range requested {
			pattern := strings.TrimSpace(value)
			if !caller.covers(resourceEvents, verbRead, pattern) {
				writeError(w, http.StatusForbidden, codeForbidden,
					"this token does not carry a scope covering events:read:"+pattern)
				return nil, false
			}
		}
		return filters, true
	}

	// patternsFor is empty exactly when the grant is unnarrowed, because the
	// route gate has already established that some events:read grant exists.
	granted := caller.patternsFor(resourceEvents, verbRead)
	narrowed := make([]store.EventTypeFilter, 0, len(granted))
	for _, pattern := range granted {
		if prefix, isPrefix := strings.CutSuffix(pattern, ".*"); isPrefix {
			narrowed = append(narrowed, store.EventTypeFilter{Prefix: prefix + "."})
			continue
		}
		narrowed = append(narrowed, store.EventTypeFilter{Exact: pattern})
	}
	return narrowed, true
}

// parseEventTypeFilters turns the repeated `type` parameter into query terms.
// A `*` anywhere in the list means the caller asked for everything, so the
// narrowing is dropped entirely rather than ORed with terms it already covers.
func parseEventTypeFilters(w http.ResponseWriter, values []string) ([]store.EventTypeFilter, bool) {
	if len(values) > maxEventTypeFilters {
		writeError(w, http.StatusBadRequest, codeBadRequest,
			"at most "+strconv.Itoa(maxEventTypeFilters)+" type filters may be combined")
		return nil, false
	}

	// Every term is checked even once one of them turns out to be the
	// catch-all: a hub that stopped early would silently accept a filter it
	// could not parse, and answer with a feed that looks real and is not
	// (spec section 8.5).
	everything := false
	filters := make([]store.EventTypeFilter, 0, len(values))
	for _, value := range values {
		pattern := strings.TrimSpace(value)
		switch {
		case pattern == "*":
			everything = true
		case pattern == "":
			// `?type=` is a filter the caller meant to set and did not. Reading
			// it as the catch-all would answer with the whole feed, which looks
			// exactly like a real answer to a real filter.
			writeError(w, http.StatusBadRequest, codeBadRequest,
				"type filter is empty; omit the parameter to match every type")
			return nil, false
		case strings.HasSuffix(pattern, ".*"):
			namespace := strings.TrimSuffix(pattern, ".*")
			// The namespace of a pattern must itself be a legal type prefix, so
			// that nothing a caller writes here can reach the query as anything
			// but the alphabet the event grammar allows.
			if !validEventType(namespace + ".x") {
				writeError(w, http.StatusBadRequest, codeBadRequest,
					"type filter "+pattern+" is not a {namespace}.* pattern")
				return nil, false
			}
			filters = append(filters, store.EventTypeFilter{Prefix: namespace + "."})
		case validEventType(pattern):
			filters = append(filters, store.EventTypeFilter{Exact: pattern})
		default:
			writeError(w, http.StatusBadRequest, codeBadRequest,
				"type filter "+pattern+" is neither an event type nor a {namespace}.* pattern")
			return nil, false
		}
	}
	if everything {
		// The catch-all subsumes every other term, so the narrowing is dropped
		// rather than ORed with conditions it already covers.
		return nil, true
	}
	return filters, true
}

// cursorLayout must render exactly what envelopeTimestamp does, because a
// cursor is compared against stored timestamps as a string.
const cursorLayout = "2006-01-02T15:04:05.000Z"

// The cursor is opaque by contract (spec section 2.1), so its encoding may
// change; what it must never be is an offset a caller could do arithmetic on,
// which is why it carries a position in the feed instead.
func encodeEventCursor(event store.Event) string {
	return base64.RawURLEncoding.EncodeToString(
		[]byte(envelopeTimestamp(event.OccurredAt) + "|" + event.ID))
}

func parseEventCursor(w http.ResponseWriter, value string) (store.EventCursor, bool) {
	if value == "" {
		return store.EventCursor{}, true
	}
	refuse := func() (store.EventCursor, bool) {
		writeError(w, http.StatusBadRequest, codeBadRequest,
			"cursor is not one this hub issued")
		return store.EventCursor{}, false
	}

	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return refuse()
	}
	occurredAt, eventID, found := strings.Cut(string(decoded), "|")
	if !found || eventID == "" {
		return refuse()
	}
	parsed, err := time.Parse(cursorLayout, occurredAt)
	if err != nil {
		return refuse()
	}
	return store.EventCursor{OccurredAt: parsed, ID: eventID}, true
}
