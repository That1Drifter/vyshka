package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Telemetry grading (spec section 8). The mock validates every event.batch
// and state.* envelope on arrival to the bounds a conformant hub enforces,
// and records violations as faults, so a candidate learns here rather than
// from the first event.reject or state.reject a real hub queues it. The
// stage below only decides whether there was anything to grade: publishing
// telemetry is a SHOULD, so a candidate that publishes none passes with a
// note rather than failing.

// Bounds a hub may not tighten (sections 8.1 and 8.3).
const (
	maxEventsPerBatch  = 200
	maxEventTypeLength = 128
	maxEventDataBytes  = 16 << 10
	maxSnapshotBytes   = 256 << 10
	maxSnapshotEntries = 5000
	maxPlatformLength  = 64
	maxPlayerIDLength  = 128
	maxEntryIDLength   = 128
	maxEntryKindLength = 128
	maxEntryNameLength = 200
)

var eventTypePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)+$`)

// telemetryStats counts what the candidate published, for the stage.
type telemetryStats struct {
	batches   int
	events    int
	snapshots int
}

var telemetryStage = Stage{
	ID:      "telemetry.wellFormed",
	Title:   "Any events and snapshots the plugin publishes are well formed",
	Section: "8",
	Run: func(h *harness) error {
		hub := h.hub
		err := hub.await(h.checkTimeout, "an event.batch or state.* envelope", func() bool {
			return hub.telemetry.batches+hub.telemetry.snapshots > 0
		})
		// From here on, telemetry faults are charged to the stage they
		// arrive in; everything held so far is this stage's to report.
		held := hub.drainTelemetryFaults()
		var exited bool
		hub.view(func() { exited = hub.pluginExited })
		if err != nil {
			if exited {
				return err
			}
			if len(held) > 0 {
				return telemetryFaultError(held)
			}
			return ungraded{fmt.Sprintf("the candidate published no event.batch or state.* envelope within %s; telemetry is a SHOULD (section 8), so a plugin without any is compliant and nothing here could be graded (a plugin on a slower cadence can raise -check-timeout)", h.checkTimeout)}
		}
		if len(held) > 0 {
			return telemetryFaultError(held)
		}
		return nil
	},
}

// drainTelemetryFaults hands back every held telemetry fault and switches
// the hub to charging later ones to the stage they arrive in.
func (h *mockHub) drainTelemetryFaults() []fault {
	h.mu.Lock()
	defer h.mu.Unlock()
	held := h.telemetryFaults
	h.telemetryFaults = nil
	h.telemetryGraded = true
	return held
}

func telemetryFaultError(held []fault) error {
	messages := make([]string, 0, len(held))
	for _, f := range held {
		messages = append(messages, f.String())
	}
	return fmt.Errorf("a conformant hub would refuse what the plugin published: %s", strings.Join(messages, "; "))
}

// telemetryFaultLocked records a section 8 fault: held for the telemetry
// stage until it has run, charged to the current stage after.
func (h *mockHub) telemetryFaultLocked(section, format string, args ...any) {
	if h.telemetryGraded {
		h.faultLocked(section, format, args...)
		return
	}
	h.telemetryFaults = append(h.telemetryFaults, fault{Section: section, Message: fmt.Sprintf(format, args...)})
	trace("telemetry fault held for the telemetry stage: [%s] %s", section, fmt.Sprintf(format, args...))
}

// isJSONObject reports whether raw is a JSON object (not null, not an array).
func isJSONObject(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return strings.HasPrefix(trimmed, "{")
}

// isJSONNull reports whether raw is the JSON null, which for an OPTIONAL
// field reads as absent (section 2.1) rather than as a value to validate.
func isJSONNull(raw json.RawMessage) bool {
	return strings.TrimSpace(string(raw)) == "null"
}

// rfc3339String reads raw as a JSON string holding an RFC 3339 timestamp in
// any offset; the events and snapshots of section 8 say "RFC 3339", not
// "UTC", unlike the envelope ts of section 4.
func rfc3339String(raw json.RawMessage) bool {
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	_, err := time.Parse(time.RFC3339, value)
	return err == nil
}

// validateEventBatchLocked grades one event.batch body (section 8.1) and
// returns how many events it carried.
func (h *mockHub) validateEventBatchLocked(envelope *inboundEnvelope) int {
	var body struct {
		Events *[]json.RawMessage `json:"events"`
	}
	if !isJSONObject(json.RawMessage(envelope.Body)) || json.Unmarshal([]byte(envelope.Body), &body) != nil {
		h.telemetryFaultLocked("8.1", "event.batch %s: the body is not an object carrying an events array", envelope.ID)
		return 0
	}
	if body.Events == nil {
		h.telemetryFaultLocked("8.1", "event.batch %s carries no events array; events is REQUIRED (an empty array is legal, an absent one rejects the batch)", envelope.ID)
		return 0
	}
	events := *body.Events
	if len(events) > maxEventsPerBatch {
		h.telemetryFaultLocked("8.1", "event.batch %s carries %d events; a hub refuses a batch of more than %d, and the whole batch is lost", envelope.ID, len(events), maxEventsPerBatch)
	}
	for index, raw := range events {
		path := fmt.Sprintf("event.batch %s events[%d]", envelope.ID, index)
		var event struct {
			T    json.RawMessage `json:"t"`
			TS   json.RawMessage `json:"ts"`
			Data json.RawMessage `json:"data"`
		}
		if !isJSONObject(raw) || json.Unmarshal(raw, &event) != nil {
			h.telemetryFaultLocked("8.1", "%s is not an object", path)
			continue
		}
		var eventType string
		switch {
		case event.T == nil:
			h.telemetryFaultLocked("8.1", "%s carries no t; the type is REQUIRED", path)
		case json.Unmarshal(event.T, &eventType) != nil:
			h.telemetryFaultLocked("8.1", "%s.t is not a string", path)
		case len(eventType) > maxEventTypeLength || !eventTypePattern.MatchString(eventType):
			h.telemetryFaultLocked("8.1", "%s.t %q is outside the {namespace}.{name} grammar: two or more non-empty segments of letters, digits, _ and - separated by dots, at most %d characters", path, eventType, maxEventTypeLength)
		case strings.HasPrefix(eventType, "action.") || strings.HasPrefix(eventType, "server."):
			h.telemetryFaultLocked("8.1", "%s.t %q begins with a namespace reserved for the hub's own notifications; a hub refuses it (core server telemetry lives under core.server.*)", path, eventType)
		}
		// A present ts is a sender obligation (RFC 3339); a null one is an
		// absent one, which a hub fills with its receipt time and never
		// refuses.
		if event.TS != nil && !isJSONNull(event.TS) && !rfc3339String(event.TS) {
			h.telemetryFaultLocked("8.1", "%s.ts %s is not an RFC 3339 timestamp; a hub would substitute its receipt time and the event would land at the wrong moment", path, string(event.TS))
		}
		if event.Data != nil && !isJSONNull(event.Data) {
			if !isJSONObject(event.Data) {
				h.telemetryFaultLocked("8.1", "%s.data is not an object; a hub refuses the whole batch over it", path)
			} else if len(event.Data) > maxEventDataBytes {
				h.telemetryFaultLocked("8.1", "%s.data is %d bytes; the reference cap is %d and a hub refuses the whole batch over it", path, len(event.Data), maxEventDataBytes)
			}
		}
	}
	return len(events)
}

// validateSnapshotLocked grades one state.* body (section 8.3).
func (h *mockHub) validateSnapshotLocked(envelope *inboundEnvelope) {
	listField := strings.TrimPrefix(envelope.Type, "state.")
	label := fmt.Sprintf("%s %s", envelope.Type, envelope.ID)
	if len(envelope.Body) > maxSnapshotBytes {
		h.telemetryFaultLocked("8.3", "%s: the body is %d bytes; a snapshot is capped at %d and rejected whole above it", label, len(envelope.Body), maxSnapshotBytes)
	}
	var fields map[string]json.RawMessage
	if !isJSONObject(json.RawMessage(envelope.Body)) || json.Unmarshal([]byte(envelope.Body), &fields) != nil {
		h.telemetryFaultLocked("8.3", "%s: the body is not an object", label)
		return
	}
	if raw, present := fields["capturedAt"]; present && !isJSONNull(raw) && !rfc3339String(raw) {
		h.telemetryFaultLocked("8.3", "%s: capturedAt %s is not an RFC 3339 timestamp; a hub would fall back to the envelope ts", label, string(raw))
	}
	listRaw, present := fields[listField]
	if !present {
		h.telemetryFaultLocked("8.3", "%s carries no %s list; a snapshot is whole, so an absent list is not a snapshot at all (an empty list is: nobody is online)", label, listField)
		return
	}
	var entries []json.RawMessage
	if isJSONNull(listRaw) || json.Unmarshal(listRaw, &entries) != nil {
		h.telemetryFaultLocked("8.3", "%s: %s is not an array", label, listField)
		return
	}
	if len(entries) > maxSnapshotEntries {
		h.telemetryFaultLocked("8.3", "%s carries %d entries; a snapshot is capped at %d and rejected whole above it", label, len(entries), maxSnapshotEntries)
	}
	tooLong := func(value string, limit int) bool { return utf8.RuneCountInString(value) > limit }
	for index, raw := range entries {
		path := fmt.Sprintf("%s %s[%d]", label, listField, index)
		var entry map[string]json.RawMessage
		if !isJSONObject(raw) || json.Unmarshal(raw, &entry) != nil {
			h.telemetryFaultLocked("8.3", "%s is not an object", path)
			continue
		}
		if listField == "players" {
			playerRaw, hasPlayer := entry["player"]
			var player struct {
				Platform *string `json:"platform"`
				ID       *string `json:"id"`
			}
			switch {
			case !hasPlayer:
				h.telemetryFaultLocked("8.3", "%s carries no player; the platform-qualified identity is the one field a hub enforces deeply, because it is what correlates a snapshot entry with events and actions", path)
			case !isJSONObject(playerRaw) || json.Unmarshal(playerRaw, &player) != nil:
				h.telemetryFaultLocked("8.3", "%s.player is not an object", path)
			case player.Platform == nil || *player.Platform == "" || tooLong(*player.Platform, maxPlatformLength):
				h.telemetryFaultLocked("8.3", "%s.player.platform must be a non-empty string of at most %d characters", path, maxPlatformLength)
			case player.ID == nil || *player.ID == "" || tooLong(*player.ID, maxPlayerIDLength):
				h.telemetryFaultLocked("8.3", "%s.player.id must be a non-empty string of at most %d characters", path, maxPlayerIDLength)
			}
			if nameRaw, hasName := entry["name"]; hasName {
				var name string
				if json.Unmarshal(nameRaw, &name) != nil {
					h.telemetryFaultLocked("8.3", "%s.name is not a string", path)
				} else if tooLong(name, maxEntryNameLength) {
					h.telemetryFaultLocked("8.3", "%s.name is longer than %d characters", path, maxEntryNameLength)
				}
			}
		} else {
			var id string
			if idRaw, hasID := entry["id"]; !hasID || json.Unmarshal(idRaw, &id) != nil || id == "" || tooLong(id, maxEntryIDLength) {
				h.telemetryFaultLocked("8.3", "%s.id must be a non-empty string of at most %d characters, stable for the lifetime of the thing it names", path, maxEntryIDLength)
			}
			if kindRaw, hasKind := entry["kind"]; hasKind {
				var kind string
				if json.Unmarshal(kindRaw, &kind) != nil {
					h.telemetryFaultLocked("8.3", "%s.kind is not a string", path)
				} else if tooLong(kind, maxEntryKindLength) {
					h.telemetryFaultLocked("8.3", "%s.kind is longer than %d characters", path, maxEntryKindLength)
				}
			}
		}
		// position is OPTIONAL: null is absent. Present, it is two or three
		// numbers, and a null coordinate is not a number however the decoder
		// would like to read it.
		if positionRaw, hasPosition := entry["position"]; hasPosition && !isJSONNull(positionRaw) {
			var position []json.RawMessage
			if json.Unmarshal(positionRaw, &position) != nil || len(position) < 2 || len(position) > 3 {
				h.telemetryFaultLocked("8.3", "%s.position must be an array of two or three numbers", path)
			} else {
				for axis, component := range position {
					var number float64
					if isJSONNull(component) || json.Unmarshal(component, &number) != nil {
						h.telemetryFaultLocked("8.3", "%s.position[%d] is not a number", path, axis)
					}
				}
			}
		}
		if dataRaw, hasData := entry["data"]; hasData && !isJSONNull(dataRaw) && !isJSONObject(dataRaw) {
			h.telemetryFaultLocked("8.3", "%s.data is not an object", path)
		}
	}
}
