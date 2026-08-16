package hub

import (
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/That1Drifter/vyshka/hub/store"
)

func TestValidEventType(t *testing.T) {
	valid := []string{
		"core.player.death",
		"example-mod.raid.started",
		"example_mod.raid",
		"a.b",
		"ns.Name9",
		strings.Repeat("a", 60) + "." + strings.Repeat("b", 67), // exactly 128
	}
	for _, eventType := range valid {
		if !validEventType(eventType) {
			t.Errorf("validEventType(%q) = false, want true", eventType)
		}
	}

	invalid := map[string]string{
		"empty":           "",
		"noNamespace":     "death",
		"leadingDot":      ".death",
		"trailingDot":     "core.",
		"doubleDot":       "core..death",
		"wildcard":        "core.player.*",
		"space":           "core.player death",
		"slash":           "core/player.death",
		"nonASCII":        "cœur.player",
		"tooLong":         strings.Repeat("a", 60) + "." + strings.Repeat("b", 68),
		"percentWildcard": "core.play%r",
	}
	for name, eventType := range invalid {
		t.Run(name, func(t *testing.T) {
			if validEventType(eventType) {
				t.Errorf("validEventType(%q) = true, want false", eventType)
			}
		})
	}
}

func TestEventTimestampNeverRejects(t *testing.T) {
	now := time.Date(2026, 8, 16, 18, 0, 0, 0, time.UTC)
	at := func(raw string) *time.Time {
		return eventTimestamp(json.RawMessage(raw), now)
	}

	stamped := at(`"2026-08-16T17:59:00Z"`)
	if stamped == nil || !stamped.Equal(now.Add(-time.Minute)) {
		t.Errorf("a usable timestamp = %v, want the moment it names", stamped)
	}
	// An offset is RFC 3339 and normalizes to UTC.
	stamped = at(`"2026-08-16T19:59:00+02:00"`)
	if stamped == nil || !stamped.Equal(now.Add(-time.Minute)) {
		t.Errorf("an offset timestamp = %v, want the same instant in UTC", stamped)
	}

	// Everything a hub cannot use falls back on receipt time, which the store
	// substitutes for a nil. The wrongly typed cases matter as much as the
	// unparseable ones: `ts` must never cost a batch of real events, and a
	// typed field would have failed the whole body's decode instead.
	for name, raw := range map[string]string{
		"absent":       ``,
		"null":         `null`,
		"number":       `1755367200`,
		"boolean":      `true`,
		"object":       `{"seconds": 1755367200}`,
		"emptyString":  `""`,
		"unparseable":  `"yesterday"`,
		"wrongFormat":  `"2026-08-16 18:00:00"`,
		"farFuture":    `"2027-08-16T18:00:00Z"`,
		"justTooFarUp": `"2026-08-16T19:00:01Z"`,
	} {
		t.Run(name, func(t *testing.T) {
			if stamped := at(raw); stamped != nil {
				t.Errorf("eventTimestamp(%s) = %v, want nil so receipt time stands in", raw, stamped)
			}
		})
	}

	// Inside the skew allowance a clock that runs slightly fast is kept as sent:
	// clamping it would be a lie about a difference of seconds.
	if stamped := at(`"2026-08-16T18:59:00Z"`); stamped == nil {
		t.Error("a timestamp inside the skew allowance was dropped")
	}
}

func TestRetentionResolvesTheMostSpecificRule(t *testing.T) {
	server := &Server{cfg: Config{EventRetention: []RetentionRule{
		{Pattern: "*", TTL: 30 * 24 * time.Hour},
		{Pattern: "core.*", TTL: 7 * 24 * time.Hour},
		{Pattern: "core.player.chat", TTL: 90 * 24 * time.Hour},
		// A rule that would mean "delete on arrival" is a misconfiguration, not
		// a policy, and is ignored rather than obeyed.
		{Pattern: "core.server.fps", TTL: 0},
	}}}

	cases := map[string]time.Duration{
		"core.player.chat":         90 * 24 * time.Hour,
		"core.player.death":        7 * 24 * time.Hour,
		"core.server.fps":          7 * 24 * time.Hour,
		"example-mod.raid.started": 30 * 24 * time.Hour,
	}
	for eventType, want := range cases {
		if got := server.retentionFor(eventType); got != want {
			t.Errorf("retentionFor(%q) = %s, want %s", eventType, got, want)
		}
	}

	// With no catch-all configured, a type nothing matches still gets a bound:
	// an unbounded table is the one retention outcome an operator cannot undo.
	narrow := &Server{cfg: Config{EventRetention: []RetentionRule{
		{Pattern: "core.*", TTL: time.Hour},
	}}}
	if got := narrow.retentionFor("example-mod.raid.started"); got != defaultRetentionTTL {
		t.Errorf("unmatched type retention = %s, want the %s fallback", got, defaultRetentionTTL)
	}
}

func TestValidateEventBatch(t *testing.T) {
	server := &Server{cfg: Config{EventRetention: DefaultEventRetention()}}
	now := time.Now().UTC()

	events, faults := server.validateEventBatch(json.RawMessage(
		`{"events": [
			{"t": "core.player.death", "ts": "2026-08-16T18:00:00Z", "data": {"weapon": "M4A1"}},
			{"t": "example-mod.raid.started"},
			{"t": "core.player.chat", "data": null}
		]}`), now)
	if len(faults) > 0 {
		t.Fatalf("a valid batch was rejected: %v", faults)
	}
	if len(events) != 3 {
		t.Fatalf("prepared %d events, want 3", len(events))
	}
	if string(events[1].Data) != `{}` {
		t.Errorf("an absent data = %s, want an empty object", events[1].Data)
	}
	if string(events[2].Data) != `{}` {
		t.Errorf("a null data = %s, want an empty object", events[2].Data)
	}
	if events[2].Retention != 90*24*time.Hour {
		t.Errorf("chat retention = %s, want the 90 day rule", events[2].Retention)
	}

	// An empty batch is a plugin flushing nothing, which is legal.
	events, faults = server.validateEventBatch(json.RawMessage(`{"events": []}`), now)
	if len(faults) > 0 || len(events) != 0 {
		t.Errorf("an empty batch gave %d events and %v, want neither", len(events), faults)
	}

	// A `ts` of the wrong JSON type must cost that one timestamp, not the
	// batch: section 8.1 forbids refusing an event over `ts` alone, and a
	// decoder that typed the field would fail the whole body here.
	events, faults = server.validateEventBatch(json.RawMessage(
		`{"events": [
			{"t": "core.player.death", "ts": 1755367200},
			{"t": "core.player.chat", "ts": {"seconds": 1}},
			{"t": "core.server.fps", "ts": "2026-08-16T18:00:00Z"}
		]}`), now)
	if len(faults) > 0 {
		t.Fatalf("a batch with wrongly typed timestamps was rejected: %v", faults)
	}
	if len(events) != 3 {
		t.Fatalf("prepared %d events, want all 3 kept", len(events))
	}
	if events[0].OccurredAt != nil || events[1].OccurredAt != nil {
		t.Error("a wrongly typed ts produced a timestamp; receipt time must stand in")
	}

	oversized := make([]string, 0, maxEventsPerBatch+1)
	for range maxEventsPerBatch + 1 {
		oversized = append(oversized, `{"t": "core.player.damage"}`)
	}

	rejected := map[string]string{
		"noBody":           ``,
		"notAnObject":      `[]`,
		"eventsAbsent":     `{}`,
		"eventsNull":       `{"events": null}`,
		"eventsNotAnArray": `{"events": {}}`,
		"typeMissing":      `{"events": [{"data": {}}]}`,
		"typeUnnamespaced": `{"events": [{"t": "death"}]}`,
		"dataNotAnObject":  `{"events": [{"t": "core.player.death", "data": 7}]}`,
		"dataTooLarge": `{"events": [{"t": "core.player.chat", "data": {"m": "` +
			strings.Repeat("x", maxEventDataBytes) + `"}}]}`,
		"overBatchLimit": `{"events": [` + strings.Join(oversized, ",") + `]}`,
	}
	for name, body := range rejected {
		t.Run("rejected/"+name, func(t *testing.T) {
			events, faults := server.validateEventBatch(json.RawMessage(body), now)
			if len(faults) == 0 {
				t.Fatalf("body %.60s was accepted, want faults", body)
			}
			if events != nil {
				t.Errorf("a rejected batch prepared %d events; rejection is whole-batch", len(events))
			}
			for _, fault := range faults {
				if fault.Message == "" {
					t.Error("a fault carried no message")
				}
			}
		})
	}
}

// prepareEvents validates and nothing more: the per-poll budget is charged
// later, against the envelopes classification says are actually being accepted,
// so that duplicates cannot spend it. Its own contract is that every
// event.batch in the poll comes back keyed by index, valid or not.
func TestPrepareEventsValidatesWithoutBudgeting(t *testing.T) {
	server := &Server{cfg: Config{EventRetention: DefaultEventRetention()}}
	now := time.Now().UTC()

	batch := make([]string, 0, maxEventsPerBatch)
	for range maxEventsPerBatch {
		batch = append(batch, `{"t": "core.player.damage"}`)
	}
	body := json.RawMessage(`{"events": [` + strings.Join(batch, ",") + `]}`)

	// Comfortably past the per-poll budget, plus one envelope of another type
	// and one malformed batch.
	batches := maxEventsPerPoll/maxEventsPerBatch + 2
	envelopes := make([]inboundEnvelope, 0, batches+2)
	for i := range batches {
		envelopes = append(envelopes, inboundEnvelope{
			ID: "batch-" + strconv.Itoa(i), Type: envelopeTypeEventBatch, Seq: int64(i + 1), Body: body,
		})
	}
	envelopes = append(envelopes,
		inboundEnvelope{ID: "not-events", Type: "action.ack", Seq: int64(batches + 1),
			Body: json.RawMessage(`{"actionId": "x"}`)},
		inboundEnvelope{ID: "malformed", Type: envelopeTypeEventBatch, Seq: int64(batches + 2),
			Body: json.RawMessage(`{"events": [{"t": "no-namespace"}]}`)},
	)

	prepared := server.prepareEvents(envelopes, now)
	if len(prepared) != batches+1 {
		t.Fatalf("prepared %d entries, want one per event.batch and nothing for other types",
			len(prepared))
	}
	if _, keyed := prepared[batches]; keyed {
		t.Error("an action.ack was keyed as an event batch")
	}
	// Every well-formed batch is prepared whole, however far past the budget it
	// sits: the budget is not this function's business.
	for i := range batches {
		if prepared[i].reject != nil {
			t.Errorf("batch %d was rejected; prepareEvents rejects only on content", i)
		}
		if len(prepared[i].events) != maxEventsPerBatch {
			t.Errorf("batch %d prepared %d events, want %d", i, len(prepared[i].events), maxEventsPerBatch)
		}
	}
	malformed := prepared[batches+1]
	if malformed.reject == nil {
		t.Fatal("the malformed batch was accepted")
	}
	if malformed.reject.Type != envelopeTypeEventReject {
		t.Errorf("notice type = %q, want %q", malformed.reject.Type, envelopeTypeEventReject)
	}
	var notice eventRejectBody
	if err := json.Unmarshal(malformed.reject.Body, &notice); err != nil {
		t.Fatalf("decode notice: %v", err)
	}
	if notice.EnvelopeID != "malformed" || len(notice.Errors) == 0 {
		t.Errorf("notice = %+v, want it to name the refused envelope and say why", notice)
	}
}

func TestEventCursorRoundTrips(t *testing.T) {
	// The cursor is compared against stored timestamps as a string, so its
	// layout must survive the round trip exactly.
	original := time.Date(2026, 8, 16, 18, 0, 0, 123_000_000, time.UTC)
	encoded := encodeEventCursor(store.Event{OccurredAt: original, ID: "01J5QK"})

	cursor, ok := parseEventCursor(httptest.NewRecorder(), encoded)
	if !ok {
		t.Fatal("a cursor this hub issued was refused")
	}
	if !cursor.OccurredAt.Equal(original) || cursor.ID != "01J5QK" {
		t.Errorf("cursor = %+v, want %s / 01J5QK", cursor, original)
	}

	for name, bad := range map[string]string{
		"notBase64":   "not base64!",
		"noSeparator": "YWJj",
		"badTime":     "eWVzdGVyZGF5fDAxSjVRSw",
		"noID":        "MjAyNi0wOC0xNlQxODowMDowMC4wMDBafA",
	} {
		t.Run("refused/"+name, func(t *testing.T) {
			if _, ok := parseEventCursor(httptest.NewRecorder(), bad); ok {
				t.Errorf("cursor %q was accepted", bad)
			}
		})
	}
}
