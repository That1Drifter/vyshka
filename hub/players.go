package hub

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/That1Drifter/vyshka/hub/internal/id"
	"github.com/That1Drifter/vyshka/hub/store"
)

// Player profile limits (spec section 8.6). The identity bounds are
// maxPlatformLength and maxPlayerIDLength, the ones section 8.3 sets for a
// snapshot's player entry, so any identity a plugin can report is one a
// profile can be asked for.
const (
	maxNoteLength      = 4000
	maxNotesPerPlayer  = 1000
	defaultProfilePage = 100
	maxProfilePage     = 500
	// identityBackfillStep bounds one batch of the background walk that
	// indexes events stored before the identity index existed.
	identityBackfillStep = 500
)

// identityOf reads one member of an event's data as an identity object (spec
// section 8.2): a JSON object whose platform and id members, matched by exact
// name, are non-empty strings within the section 8.3 bounds. Anything else is
// not an identity, and reports false.
func identityOf(raw json.RawMessage) (platform, playerID string, ok bool) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return "", "", false
	}
	platform, ok = identityString(object["platform"], maxPlatformLength)
	if !ok {
		return "", "", false
	}
	playerID, ok = identityString(object["id"], maxPlayerIDLength)
	if !ok {
		return "", "", false
	}
	return platform, playerID, true
}

func identityString(raw json.RawMessage, bound int) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	if value == "" || utf8.RuneCountInString(value) > bound || strings.ContainsRune(value, 0) {
		// U+0000 is a legal JSON string character that Postgres text cannot
		// hold, so an identity carrying one could never be indexed; it is
		// data like any other member (spec section 8.2).
		return "", false
	}
	return value, true
}

// identitiesOf lists the identities one event's data refers to, each with the
// roles it holds, in a stable order: identities by platform then id, roles by
// name. Only top-level members count (spec section 8.2).
func identitiesOf(data map[string]json.RawMessage) []store.EventIdentity {
	if len(data) == 0 {
		return nil
	}
	type key struct{ platform, id string }
	roles := map[key][]string{}
	for member, raw := range data {
		platform, playerID, ok := identityOf(raw)
		if !ok {
			continue
		}
		identity := key{platform, playerID}
		roles[identity] = append(roles[identity], member)
	}
	if len(roles) == 0 {
		return nil
	}
	identities := make([]store.EventIdentity, 0, len(roles))
	for identity, names := range roles {
		sort.Strings(names)
		identities = append(identities, store.EventIdentity{
			Platform: identity.platform, ID: identity.id, Roles: names,
		})
	}
	sort.Slice(identities, func(i, j int) bool {
		if identities[i].Platform != identities[j].Platform {
			return identities[i].Platform < identities[j].Platform
		}
		return identities[i].ID < identities[j].ID
	})
	return identities
}

// extractIdentities is identitiesOf over stored data, for the background
// backfill: the same reading the ingest path applies, so an event indexed
// late is indexed exactly as it would have been on arrival.
func extractIdentities(data json.RawMessage) []store.EventIdentity {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return nil
	}
	return identitiesOf(object)
}

// playerIdentity reads the identity a profile route names, refusing one the
// section 8.3 bounds could never produce rather than answering it with an
// empty profile that looks like a real one.
func playerIdentity(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	platform, playerID := r.PathValue("platform"), r.PathValue("playerId")
	check := func(value, name string, bound int) bool {
		if value == "" || !utf8.ValidString(value) || utf8.RuneCountInString(value) > bound ||
			strings.ContainsRune(value, 0) {
			writeError(w, http.StatusBadRequest, codeBadRequest,
				name+" must be a non-empty identity member of at most "+strconv.Itoa(bound)+" characters, without U+0000")
			return false
		}
		return true
	}
	if !check(platform, "platform", maxPlatformLength) || !check(playerID, "playerId", maxPlayerIDLength) {
		return "", "", false
	}
	return platform, playerID, true
}

// boundServers is what a bound token's answers are confined to on a read that
// names no server: its binding, or nil for an unbound token, which the store
// reads as every server (spec section 10.2).
func boundServers(caller *principal) []string {
	if !caller.bound() {
		return nil
	}
	return caller.Servers
}

// playerEventView is one event of a profile: the section 8.5 shape plus the
// roles the identity holds in it.
type playerEventView struct {
	eventView
	Roles []string `json:"roles"`
}

type playerEventsResponse struct {
	Events     []playerEventView `json:"events"`
	NextCursor string            `json:"nextCursor,omitempty"`
}

// handlePlayerEvents answers one page of the events that refer to one
// identity, across every server the token may read (spec section 8.6). The
// type filter, the narrowing, the bounds, and the cursor are the server
// feed's; the binding confines the answer rather than refusing it.
func (s *Server) handlePlayerEvents(w http.ResponseWriter, r *http.Request) {
	platform, playerID, ok := playerIdentity(w, r)
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

	found, err := s.store.PlayerEvents(r.Context(), store.PlayerEventQuery{
		Platform: platform,
		PlayerID: playerID,
		Servers:  boundServers(principalFrom(r.Context())),
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

	response := playerEventsResponse{Events: make([]playerEventView, 0, min(len(found), limit))}
	if len(found) > limit {
		response.NextCursor = encodeEventCursor(found[limit-1].Event)
		found = found[:limit]
	}
	for _, event := range found {
		data := event.Data
		if len(data) == 0 {
			data = json.RawMessage(`{}`)
		}
		roles := event.Roles
		if roles == nil {
			roles = []string{}
		}
		response.Events = append(response.Events, playerEventView{
			eventView: eventView{
				ID:         event.ID,
				ServerID:   event.ServerID,
				Type:       event.Type,
				OccurredAt: event.OccurredAt,
				ReceivedAt: event.ReceivedAt,
				Data:       data,
			},
			Roles: roles,
		})
	}
	writeJSON(w, http.StatusOK, response)
}

// patternFilters turns scope patterns into store filters: a {prefix}.* grant
// becomes a prefix range, anything else an exact match.
func patternFilters(patterns []string) []store.EventTypeFilter {
	filters := make([]store.EventTypeFilter, 0, len(patterns))
	for _, pattern := range patterns {
		if prefix, isPrefix := strings.CutSuffix(pattern, ".*"); isPrefix {
			filters = append(filters, store.EventTypeFilter{Prefix: prefix + "."})
			continue
		}
		filters = append(filters, store.EventTypeFilter{Exact: pattern})
	}
	return filters
}

// actionReadFilters lists the action codes the caller may read, as store
// filters: nil when it may read every code, otherwise one filter per
// narrowed grant. Dispatch grants count, because dispatching implies reading
// (spec section 10.1).
func actionReadFilters(caller *principal) []store.EventTypeFilter {
	if caller.isAdmin() {
		return nil
	}
	patterns := make([]string, 0, len(caller.Scopes))
	for _, scope := range caller.Scopes {
		if scope.Resource != resourceActions || (scope.Verb != verbRead && scope.Verb != verbDispatch) {
			continue
		}
		if scope.Pattern == "" || scope.Pattern == "*" {
			return nil
		}
		patterns = append(patterns, scope.Pattern)
	}
	return patternFilters(patterns)
}

type playerActionsResponse struct {
	Actions    []actionView `json:"actions"`
	NextCursor string       `json:"nextCursor,omitempty"`
}

// handlePlayerActions answers one page of the player-context actions whose
// referenceKey is the identity's id (spec section 8.6). Nothing is refused:
// the codes the token cannot read and the servers its binding does not name
// are left out, because this read carries no filter a refusal could name.
func (s *Server) handlePlayerActions(w http.ResponseWriter, r *http.Request) {
	_, playerID, ok := playerIdentity(w, r)
	if !ok {
		return
	}
	parameters := r.URL.Query()
	limit, ok := parseLimitParam(w, parameters.Get("limit"), defaultProfilePage, maxProfilePage)
	if !ok {
		return
	}
	after, ok := parseCreatedCursor(w, parameters.Get("cursor"))
	if !ok {
		return
	}

	caller := principalFrom(r.Context())
	found, err := s.store.PlayerActions(r.Context(), store.PlayerActionQuery{
		PlayerID: playerID,
		Servers:  boundServers(caller),
		Codes:    actionReadFilters(caller),
		Limit:    limit + 1,
		After:    store.ActionCursor{CreatedAt: after.at, ID: after.id},
	})
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}

	response := playerActionsResponse{Actions: make([]actionView, 0, min(len(found), limit))}
	if len(found) > limit {
		last := found[limit-1]
		response.NextCursor = encodeCreatedCursor(last.CreatedAt, last.ID)
		found = found[:limit]
	}
	for _, action := range found {
		response.Actions = append(response.Actions, newActionView(action))
	}
	writeJSON(w, http.StatusOK, response)
}

// createdCursor is a position in a list ordered by creation time then id,
// newest first: the action and note lists of a profile.
type createdCursor struct {
	at time.Time
	id string
}

func encodeCreatedCursor(at time.Time, recordID string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(envelopeTimestamp(at) + "|" + recordID))
}

func parseCreatedCursor(w http.ResponseWriter, value string) (createdCursor, bool) {
	if value == "" {
		return createdCursor{}, true
	}
	refuse := func() (createdCursor, bool) {
		writeError(w, http.StatusBadRequest, codeBadRequest, "cursor is not one this hub issued")
		return createdCursor{}, false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return refuse()
	}
	at, recordID, found := strings.Cut(string(decoded), "|")
	if !found || recordID == "" {
		return refuse()
	}
	parsed, err := time.Parse(cursorLayout, at)
	if err != nil {
		return refuse()
	}
	return createdCursor{at: parsed, id: recordID}, true
}

// noteView is one note as the Admin API reports it (spec section 8.6).
type noteView struct {
	ID        string       `json:"id"`
	Player    noteIdentity `json:"player"`
	Text      string       `json:"text"`
	CreatedAt time.Time    `json:"createdAt"`
	CreatedBy noteAuthor   `json:"createdBy"`
}

type noteIdentity struct {
	Platform string `json:"platform"`
	ID       string `json:"id"`
}

type noteAuthor struct {
	TokenID   string `json:"tokenId"`
	TokenName string `json:"tokenName"`
}

func newNoteView(note store.PlayerNote) noteView {
	return noteView{
		ID:        note.ID,
		Player:    noteIdentity{Platform: note.Platform, ID: note.PlayerID},
		Text:      note.Text,
		CreatedAt: note.CreatedAt,
		CreatedBy: noteAuthor{TokenID: note.TokenID, TokenName: note.TokenName},
	}
}

type notesResponse struct {
	Notes      []noteView `json:"notes"`
	NextCursor string     `json:"nextCursor,omitempty"`
}

// handleListNotes answers one page of an identity's notes, newest first.
func (s *Server) handleListNotes(w http.ResponseWriter, r *http.Request) {
	platform, playerID, ok := playerIdentity(w, r)
	if !ok {
		return
	}
	parameters := r.URL.Query()
	limit, ok := parseLimitParam(w, parameters.Get("limit"), defaultProfilePage, maxProfilePage)
	if !ok {
		return
	}
	after, ok := parseCreatedCursor(w, parameters.Get("cursor"))
	if !ok {
		return
	}

	found, err := s.store.PlayerNotes(r.Context(), platform, playerID, limit+1,
		store.NoteCursor{CreatedAt: after.at, ID: after.id})
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}
	response := notesResponse{Notes: make([]noteView, 0, min(len(found), limit))}
	if len(found) > limit {
		last := found[limit-1]
		response.NextCursor = encodeCreatedCursor(last.CreatedAt, last.ID)
		found = found[:limit]
	}
	for _, note := range found {
		response.Notes = append(response.Notes, newNoteView(note))
	}
	writeJSON(w, http.StatusOK, response)
}

type createNoteRequest struct {
	Text *string `json:"text"`
}

// handleCreateNote writes one note on an identity, recording the credential
// that wrote it under the name it has now (spec section 8.6).
func (s *Server) handleCreateNote(w http.ResponseWriter, r *http.Request) {
	platform, playerID, ok := playerIdentity(w, r)
	if !ok {
		return
	}
	var request createNoteRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	switch {
	case request.Text == nil:
		writeError(w, http.StatusBadRequest, codeBadRequest, "text is required")
		return
	case strings.TrimSpace(*request.Text) == "":
		writeError(w, http.StatusBadRequest, codeBadRequest, "text must not be empty or whitespace alone")
		return
	case strings.ContainsRune(*request.Text, 0):
		writeError(w, http.StatusBadRequest, codeBadRequest, "text must not contain U+0000")
		return
	case utf8.RuneCountInString(*request.Text) > maxNoteLength:
		writeError(w, http.StatusBadRequest, codeBadRequest,
			"text is longer than "+strconv.Itoa(maxNoteLength)+" characters")
		return
	}

	caller := principalFrom(r.Context())
	note, err := s.store.CreatePlayerNote(r.Context(), store.PlayerNote{
		ID:        id.New(),
		Platform:  platform,
		PlayerID:  playerID,
		Text:      *request.Text,
		TokenID:   caller.TokenID,
		TokenName: caller.Name,
	}, maxNotesPerPlayer)
	switch {
	case errors.Is(err, store.ErrNoteLimit):
		writeError(w, http.StatusConflict, codeConflict,
			"this identity already carries "+strconv.Itoa(maxNotesPerPlayer)+" notes; delete one to add another")
		return
	case err != nil:
		s.writeInternalError(w, r, err)
		return
	}

	// The audit record names the note and the identity, never the text: the
	// log is exported to webhooks (spec section 11.1) under admin alone, and
	// notes are read under notes:read.
	auditDetail(r, "noteId", note.ID)
	auditDetail(r, "player", noteIdentity{Platform: platform, ID: playerID})
	writeJSON(w, http.StatusCreated, map[string]any{"note": newNoteView(note)})
}

// handleDeleteNote removes one note of one identity.
func (s *Server) handleDeleteNote(w http.ResponseWriter, r *http.Request) {
	platform, playerID, ok := playerIdentity(w, r)
	if !ok {
		return
	}
	noteID := r.PathValue("noteId")
	switch err := s.store.DeletePlayerNote(r.Context(), platform, playerID, noteID); {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, codeNotFound, "no such note on this identity")
		return
	case err != nil:
		s.writeInternalError(w, r, err)
		return
	}
	auditDetail(r, "noteId", noteID)
	auditDetail(r, "player", noteIdentity{Platform: platform, ID: playerID})
	w.WriteHeader(http.StatusNoContent)
}
