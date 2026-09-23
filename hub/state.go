package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/That1Drifter/vyshka/hub/internal/schema"
	"github.com/That1Drifter/vyshka/hub/store"
)

// Envelope types state ingest uses (spec section 8.3).
const (
	envelopeTypeStatePlayers  = "state.players"
	envelopeTypeStateVehicles = "state.vehicles"
	envelopeTypeStateEntities = "state.entities"
	envelopeTypeStateWorld    = "state.world"
	envelopeTypeStateReject   = "state.reject"
)

// State snapshot limits (spec section 8.3). The byte and entry caps are
// protocol floors a plugin may rely on, not hub configuration.
const (
	maxSnapshotBytes   = 256 << 10
	maxSnapshotEntries = 5000
	maxStateFaults     = 20

	maxEntryIDLength   = 128
	maxEntryKindLength = 128
	maxEntryNameLength = 200
	maxPlatformLength  = 64
	maxPlayerIDLength  = 128
)

// State query limits (spec section 8.3).
const (
	defaultStateHistoryPage = 20
	maxStateHistoryPage     = 100
)

// stateListField maps each state envelope type to the field its body must
// carry: a list for three of them, the one world object for state.world.
// Membership here is also what makes a type a snapshot at all: any other
// state.* type takes the forward-compatibility path of section 4, acked and
// ignored.
var stateListField = map[string]string{
	envelopeTypeStatePlayers:  "players",
	envelopeTypeStateVehicles: "vehicles",
	envelopeTypeStateEntities: "entities",
	envelopeTypeStateWorld:    "world",
}

// worldTimeForm is the shape of a world's time (section 8.3): the game's
// calendar with no offset, to the minute or to the second. The form is
// checked here and the calendar by time.Parse, whose hour field alone would
// take a single digit.
var worldTimeForm = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}(:[0-9]{2})?$`)

// stateTypeFor resolves the Admin API's {stateType} path value to the
// envelope family's field name, which is also what the store keys rows by.
func stateTypeFor(pathValue string) (string, bool) {
	switch pathValue {
	case "players", "vehicles", "entities", "world":
		return pathValue, true
	}
	return "", false
}

// exactFields decodes a JSON object into its members by their exact names, or
// reports that raw is not an object. A snapshot is read this way at every
// level rather than into structs, because encoding/json matches struct fields
// case-insensitively: `{"World": {}}` would pass for a body carrying `world`,
// and an unknown `WORLD` or `ID` after the real member would overwrite it.
// Section 2.1 tolerates unknown members; it never lets one stand in for, or
// replace, a member the protocol names. Only the field the envelope type owns
// is consulted, so a `state.players` body carrying a `vehicles` field of any
// shape at all is a body with an unknown field and nothing more.
func exactFields(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, false
	}
	return fields, true
}

// present reports whether a member is there and not null; a JSON null reads
// as the member being absent (section 6.4).
func present(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null"
}

// optionalString reads a string member: "" when absent or null, false when
// present and not a string.
func optionalString(fields map[string]json.RawMessage, key string) (string, bool) {
	raw := fields[key]
	if !present(raw) {
		return "", true
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	return value, true
}

// preparedSnapshot is one state.* envelope after body validation: either the
// snapshot to store or the rejection notice that answers it.
type preparedSnapshot struct {
	snapshot *store.NewSnapshot
	reject   *store.Notice
}

// prepareSnapshots validates every state.* body in a batch, keyed by batch
// index, before classification, for the same reason prepareEvents does:
// validity depends only on content, while which envelopes are newly accepted
// is only known inside the store's transaction. A duplicate delivery pays for
// validation again before being discarded, which is the cheap side of the
// trade (see prepareManifests).
func (s *Server) prepareSnapshots(envelopes []inboundEnvelope, now time.Time) map[int]preparedSnapshot {
	var prepared map[int]preparedSnapshot
	for index, e := range envelopes {
		if _, isSnapshot := stateListField[e.Type]; !isSnapshot {
			continue
		}
		if prepared == nil {
			prepared = make(map[int]preparedSnapshot)
		}

		snapshot, faults := s.validateSnapshot(e, now)
		if len(faults) > 0 {
			notice := newStateReject(e.ID, faults)
			prepared[index] = preparedSnapshot{reject: &notice}
			continue
		}
		prepared[index] = preparedSnapshot{snapshot: snapshot}
	}
	return prepared
}

// validateSnapshot checks one state.* envelope against section 8.3. The
// snapshot is applied or rejected whole, never partially: a partially applied
// snapshot would be a state nobody ever observed.
func (s *Server) validateSnapshot(e inboundEnvelope, now time.Time) (*store.NewSnapshot, []schema.Fault) {
	listField := stateListField[e.Type]

	if len(e.Body) == 0 {
		return nil, []schema.Fault{{Path: "", Message: "body is required"}}
	}
	if len(e.Body) > maxSnapshotBytes {
		return nil, []schema.Fault{{Path: "", Message: fmt.Sprintf(
			"a snapshot body is at most %d bytes, got %d", maxSnapshotBytes, len(e.Body))}}
	}
	body, ok := exactFields(e.Body)
	if !ok {
		return nil, []schema.Fault{{Path: "",
			Message: "body does not match the snapshot shape: it must be a JSON object"}}
	}

	var faults []schema.Fault
	fault := func(path, format string, args ...any) {
		faults = append(faults, schema.Fault{Path: path, Message: fmt.Sprintf(format, args...)})
	}
	tooLong := func(value string, limit int) bool {
		return utf8.RuneCountInString(value) > limit
	}

	// Only the field this envelope type owns is decoded. A JSON null reads as
	// the field being absent (section 6.4), which for the one required field
	// means the same refusal as leaving it out.
	rawList := body[listField]
	if !present(rawList) {
		return nil, []schema.Fault{{Path: listField, Message: listField + " is required"}}
	}

	if e.Type == envelopeTypeStateWorld {
		faults = append(faults, validateWorld(rawList, listField)...)
	} else {
		var entries []json.RawMessage
		if err := json.Unmarshal(rawList, &entries); err != nil {
			return nil, []schema.Fault{{Path: listField,
				Message: listField + " does not match the snapshot shape: it must be a list"}}
		}
		if len(entries) > maxSnapshotEntries {
			return nil, []schema.Fault{{Path: listField, Message: fmt.Sprintf(
				"a snapshot carries at most %d entries, got %d", maxSnapshotEntries, len(entries))}}
		}
		for i, raw := range entries {
			path := fmt.Sprintf("%s[%d]", listField, i)
			entry, ok := exactFields(raw)
			if !ok {
				fault(path, "an entry must be a JSON object")
				continue
			}
			if e.Type == envelopeTypeStatePlayers {
				// The one field enforced deeply: identity is what lets a
				// reader correlate this entry with events and actions.
				player, isObject := exactFields(entry["player"])
				platform, platformOK := optionalString(player, "platform")
				id, idOK := optionalString(player, "id")
				switch {
				case !present(entry["player"]):
					fault(path+".player", "player is required, the platform-qualified identity of section 8.2")
				case !isObject:
					fault(path+".player", "player must be an object, the platform-qualified identity of section 8.2")
				case !platformOK || platform == "" || tooLong(platform, maxPlatformLength):
					fault(path+".player.platform", "platform must be a non-empty string of at most %d characters", maxPlatformLength)
				case !idOK || id == "" || tooLong(id, maxPlayerIDLength):
					fault(path+".player.id", "id must be a non-empty string of at most %d characters", maxPlayerIDLength)
				}
				if name, ok := optionalString(entry, "name"); !ok || tooLong(name, maxEntryNameLength) {
					fault(path+".name", "name must be a string of at most %d characters", maxEntryNameLength)
				}
			} else {
				if id, ok := optionalString(entry, "id"); !ok || id == "" || tooLong(id, maxEntryIDLength) {
					fault(path+".id", "id must be a non-empty string of at most %d characters", maxEntryIDLength)
				}
				if kind, ok := optionalString(entry, "kind"); !ok || tooLong(kind, maxEntryKindLength) {
					fault(path+".kind", "kind must be a string of at most %d characters", maxEntryKindLength)
				}
			}
			faults = append(faults, validatePosition(entry["position"], path+".position")...)
			faults = append(faults, validateEntryData(entry["data"], path+".data")...)
		}
	}

	if len(faults) > 0 {
		return nil, faults
	}

	// capturedAt falls back through the envelope's ts to receipt time, each
	// with the tolerance of section 4: a wrong clock costs precision, never
	// the snapshot.
	capturedAt := eventTimestamp(body["capturedAt"], now)
	if capturedAt == nil {
		capturedAt = eventTimestamp(e.TS, now)
	}
	return &store.NewSnapshot{
		EnvelopeID:   e.ID,
		Type:         listField,
		CapturedAt:   capturedAt,
		Body:         e.Body,
		Retention:    s.cfg.StateSnapshotRetention,
		HistoryDepth: s.cfg.StateHistoryDepth,
	}, nil
}

// validateWorld checks the one object of a state.world body: an object, not
// a list, whose time, when present, is the game's calendar in the form
// section 8.3 names, and whose data is an object or nothing.
func validateWorld(raw json.RawMessage, path string) []schema.Fault {
	var world map[string]json.RawMessage
	if err := json.Unmarshal(raw, &world); err != nil {
		return []schema.Fault{{Path: path,
			Message: "world must be a JSON object: a server has one world, not a list of them"}}
	}
	var faults []schema.Fault
	if rawTime, present := world["time"]; present && string(rawTime) != "null" {
		var value string
		if err := json.Unmarshal(rawTime, &value); err != nil || !validWorldTime(value) {
			faults = append(faults, schema.Fault{Path: path + ".time",
				Message: "time must be the game's clock as YYYY-MM-DDTHH:MM or YYYY-MM-DDTHH:MM:SS, a valid date and time of day with no offset"})
		}
	}
	return append(faults, validateEntryData(world["data"], path+".data")...)
}

// validWorldTime reports whether value is a world time of section 8.3.
func validWorldTime(value string) bool {
	if !worldTimeForm.MatchString(value) {
		return false
	}
	layout := "2006-01-02T15:04"
	if len(value) > len(layout) {
		layout = "2006-01-02T15:04:05"
	}
	_, err := time.Parse(layout, value)
	return err == nil
}

// validatePosition checks the optional position field: an array of two or
// three finite JSON numbers. Absent and null are both fine; a present
// position that is not usable rejects the snapshot rather than being stored
// as something a map client would have to defend against.
func validatePosition(raw json.RawMessage, path string) []schema.Fault {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	// Pointers, because a null coordinate would otherwise decode to 0 without
	// an error and pass as a number.
	var coordinates []*float64
	if err := json.Unmarshal(raw, &coordinates); err != nil {
		return []schema.Fault{{Path: path, Message: "position must be an array of numbers"}}
	}
	if len(coordinates) < 2 || len(coordinates) > 3 {
		return []schema.Fault{{Path: path, Message: "position carries two or three coordinates"}}
	}
	for _, c := range coordinates {
		if c == nil {
			return []schema.Fault{{Path: path, Message: "position must be an array of numbers"}}
		}
		// NaN and infinities cannot arrive through JSON; what can is a number
		// so large it decoded to +Inf.
		if math.IsInf(*c, 0) || math.IsNaN(*c) {
			return []schema.Fault{{Path: path, Message: "position coordinates must be finite"}}
		}
	}
	return nil
}

// validateEntryData checks the optional data field: a JSON object or nothing.
func validateEntryData(raw json.RawMessage, path string) []schema.Fault {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return []schema.Fault{{Path: path, Message: "data must be a JSON object"}}
	}
	return nil
}

// stateRejectBody is what a hub sends back for a snapshot it refused, shaped
// like the rejections of sections 6.4 and 8.1 so a plugin handles all three
// the same way.
type stateRejectBody struct {
	EnvelopeID string         `json:"envelopeId"`
	Errors     []schema.Fault `json:"errors"`
}

// newStateReject builds the state.reject notice for one refused snapshot.
func newStateReject(envelopeID string, faults []schema.Fault) store.Notice {
	if len(faults) > maxStateFaults {
		faults = faults[:maxStateFaults]
	}
	encoded, err := json.Marshal(stateRejectBody{EnvelopeID: envelopeID, Errors: faults})
	if err != nil {
		degraded, _ := json.Marshal(stateRejectBody{
			EnvelopeID: envelopeID,
			Errors:     []schema.Fault{{Path: "", Message: "the snapshot was rejected"}},
		})
		encoded = degraded
	}
	return store.Notice{Type: envelopeTypeStateReject, Body: encoded}
}

// snapshotView is the Admin API representation of a stored snapshot (spec
// section 8.3): the accepted body verbatim, with the hub's metadata beside it
// rather than rewritten into it.
type snapshotView struct {
	Type       string          `json:"type"`
	CapturedAt time.Time       `json:"capturedAt"`
	ReceivedAt time.Time       `json:"receivedAt"`
	Snapshot   json.RawMessage `json:"snapshot"`
}

func newSnapshotView(snapshot store.Snapshot) snapshotView {
	return snapshotView{
		Type:       snapshot.Type,
		CapturedAt: snapshot.CapturedAt,
		ReceivedAt: snapshot.ReceivedAt,
		Snapshot:   snapshot.Body,
	}
}

// handleGetState answers with the latest snapshot of one type. The request's
// own shape is checked before the server is looked up, so an unusable state
// type is bad_request whether or not the server exists.
func (s *Server) handleGetState(w http.ResponseWriter, r *http.Request) {
	stateType, ok := stateTypeFor(r.PathValue("stateType"))
	if !ok {
		writeError(w, http.StatusBadRequest, codeBadRequest,
			"state type must be players, vehicles, entities, or world")
		return
	}
	server, ok := s.lookupServer(w, r)
	if !ok {
		return
	}

	snapshot, err := s.store.LatestSnapshot(r.Context(), server.ID, stateType)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, codeNotFound,
			"this server has no accepted "+stateType+" snapshot")
		return
	case err != nil:
		s.writeInternalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newSnapshotView(snapshot))
}

// handleGetStateHistory answers recent snapshots of one type, newest first.
// Like handleGetState, the request's own shape is checked before the lookup.
func (s *Server) handleGetStateHistory(w http.ResponseWriter, r *http.Request) {
	stateType, ok := stateTypeFor(r.PathValue("stateType"))
	if !ok {
		writeError(w, http.StatusBadRequest, codeBadRequest,
			"state type must be players, vehicles, entities, or world")
		return
	}
	limit, ok := parseLimitParam(w, r.URL.Query().Get("limit"),
		defaultStateHistoryPage, maxStateHistoryPage)
	if !ok {
		return
	}
	server, ok := s.lookupServer(w, r)
	if !ok {
		return
	}

	snapshots, err := s.store.SnapshotHistory(r.Context(), server.ID, stateType, limit)
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}
	views := make([]snapshotView, 0, len(snapshots))
	for _, snapshot := range snapshots {
		views = append(views, newSnapshotView(snapshot))
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshots": views})
}
