package hub

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/That1Drifter/vyshka/hub/internal/id"
	"github.com/That1Drifter/vyshka/hub/store"
)

// The installation ban list (spec section 13): one list for the installation,
// changed through the Admin API and pulled page by page by every plugin that
// enforces it.

// Envelope types the ban list uses (spec sections 13.3 and 13.4).
const (
	envelopeTypeBansChanged = store.BanChangedType
	envelopeTypeBansApplied = "bans.applied"
)

// Ban list limits (spec sections 13.1 to 13.3).
const (
	maxBanReasonLength    = 200
	maxBanNameLength      = 200
	maxBanDurationSeconds = 315360000 // ten years, the bound a KV TTL carries
	defaultBanPage        = 100
	maxBanPage            = 500
	// maxBanRevision is 2^53 - 1: revisions ride through JSON tooling that
	// reads numbers as float64, the bound every revision in the protocol has.
	maxBanRevision = int64(1)<<53 - 1
	// defaultBanSweepInterval is how often the hub takes expired bans off the
	// active list. The protocol allows 60 s (section 13.1), which caps the
	// setting with a margin for the pass itself; the reference answers in a
	// few seconds so a panel watching an expiry sees it happen.
	defaultBanSweepInterval = 5 * time.Second
	maxBanSweepInterval     = 50 * time.Second
)

// Ban states as the Admin API reports them (spec section 13.1).
const (
	banStateActive  = "active"
	banStateLifted  = "lifted"
	banStateExpired = "expired"
)

// banAuthor is the credential that placed or lifted a ban, under the name it
// had at the time (spec section 13.1).
type banAuthor struct {
	TokenID   string `json:"tokenId"`
	TokenName string `json:"tokenName"`
}

// banView is one ban record as the Admin API reports it (spec section 13.1).
type banView struct {
	ID        string       `json:"id"`
	Player    noteIdentity `json:"player"`
	Reason    string       `json:"reason"`
	Name      string       `json:"name"`
	ServerID  *string      `json:"serverId"`
	CreatedAt time.Time    `json:"createdAt"`
	CreatedBy banAuthor    `json:"createdBy"`
	ExpiresAt *time.Time   `json:"expiresAt"`
	State     string       `json:"state"`
	LiftedAt  *time.Time   `json:"liftedAt"`
	LiftedBy  *banAuthor   `json:"liftedBy"`
}

// banState reads a record's state against now: lifted when it was lifted,
// expired from the instant its expiry passes whether or not the sweep has
// taken it off the list yet, and active otherwise.
func banState(ban store.Ban, now time.Time) string {
	switch {
	case ban.LiftedAt != nil:
		return banStateLifted
	case ban.ExpiresAt != nil && !ban.ExpiresAt.After(now):
		return banStateExpired
	case !ban.OnList():
		// Off the list with no lift and no expiry cannot happen; if a
		// hand-edited row says it anyway, it is not a ban anyone enforces.
		return banStateExpired
	default:
		return banStateActive
	}
}

func newBanView(ban store.Ban, now time.Time) banView {
	view := banView{
		ID:        ban.ID,
		Player:    noteIdentity{Platform: ban.Platform, ID: ban.PlayerID},
		Reason:    ban.Reason,
		Name:      ban.Name,
		CreatedAt: ban.CreatedAt,
		CreatedBy: banAuthor{TokenID: ban.TokenID, TokenName: ban.TokenName},
		ExpiresAt: ban.ExpiresAt,
		State:     banState(ban, now),
		LiftedAt:  ban.LiftedAt,
	}
	if ban.ServerID != "" {
		serverID := ban.ServerID
		view.ServerID = &serverID
	}
	if ban.LiftedAt != nil {
		view.LiftedBy = &banAuthor{TokenID: ban.LiftedTokenID, TokenName: ban.LiftedTokenName}
	}
	return view
}

// banIdentityMember checks one member of a ban's identity against the section
// 8.3 bounds, the ones a profile route and a snapshot entry use.
func banIdentityMember(value string, bound int) bool {
	return value != "" && utf8.ValidString(value) && utf8.RuneCountInString(value) <= bound &&
		!strings.ContainsRune(value, 0)
}

type createBanRequest struct {
	Player          *noteIdentity `json:"player"`
	Reason          *string       `json:"reason"`
	DurationSeconds *int64        `json:"durationSeconds"`
	Name            *string       `json:"name"`
	ServerID        *string       `json:"serverId"`
}

type banChangeResponse struct {
	Ban      banView `json:"ban"`
	Revision int64   `json:"revision"`
}

// handleCreateBan places a ban on the installation list (spec section 13.2).
func (s *Server) handleCreateBan(w http.ResponseWriter, r *http.Request) {
	var request createBanRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	switch {
	case request.Player == nil:
		writeError(w, http.StatusBadRequest, codeBadRequest, "player is required: an identity object with platform and id")
		return
	case !banIdentityMember(request.Player.Platform, maxPlatformLength):
		writeError(w, http.StatusBadRequest, codeBadRequest,
			"player.platform must be a non-empty string of at most "+strconv.Itoa(maxPlatformLength)+" characters, without U+0000")
		return
	case !banIdentityMember(request.Player.ID, maxPlayerIDLength):
		writeError(w, http.StatusBadRequest, codeBadRequest,
			"player.id must be a non-empty string of at most "+strconv.Itoa(maxPlayerIDLength)+" characters, without U+0000")
		return
	case request.Reason == nil || strings.TrimSpace(*request.Reason) == "":
		writeError(w, http.StatusBadRequest, codeBadRequest, "reason is required and must not be empty or whitespace alone")
		return
	case strings.ContainsRune(*request.Reason, 0) || utf8.RuneCountInString(*request.Reason) > maxBanReasonLength:
		writeError(w, http.StatusBadRequest, codeBadRequest,
			"reason must be at most "+strconv.Itoa(maxBanReasonLength)+" characters, without U+0000")
		return
	case request.Name != nil && (strings.ContainsRune(*request.Name, 0) || utf8.RuneCountInString(*request.Name) > maxBanNameLength):
		writeError(w, http.StatusBadRequest, codeBadRequest,
			"name must be at most "+strconv.Itoa(maxBanNameLength)+" characters, without U+0000")
		return
	case request.DurationSeconds != nil && (*request.DurationSeconds < 1 || *request.DurationSeconds > maxBanDurationSeconds):
		// Refused rather than clamped, the rule a KV ttlSeconds follows: a
		// duration nobody wrote is not one to enforce on every server.
		writeError(w, http.StatusBadRequest, codeBadRequest,
			"durationSeconds must be an integer from 1 to "+strconv.Itoa(maxBanDurationSeconds)+"; omit it for a ban that does not end on its own")
		return
	}

	serverID := ""
	if request.ServerID != nil && *request.ServerID != "" {
		serverID = *request.ServerID
		switch _, err := s.store.Server(r.Context(), serverID); {
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound,
				"serverId names "+truncateUTF8(serverID, 64)+", which is not a server this hub knows")
			return
		case err != nil:
			s.writeInternalError(w, r, err)
			return
		}
		// The ban's provenance, so it appears in that server's audit view
		// (spec section 13.2), though its route names no server.
		auditServerID(r, serverID)
	}
	name := ""
	if request.Name != nil {
		name = *request.Name
	}
	var duration time.Duration
	if request.DurationSeconds != nil {
		duration = time.Duration(*request.DurationSeconds) * time.Second
	}

	caller := principalFrom(r.Context())
	change, err := s.store.CreateBan(r.Context(), store.NewBan{
		ID:        id.New(),
		Platform:  request.Player.Platform,
		PlayerID:  request.Player.ID,
		Reason:    *request.Reason,
		Name:      name,
		ServerID:  serverID,
		TokenID:   caller.TokenID,
		TokenName: caller.Name,
		Duration:  duration,
	}, outboundQueueLimit)
	var active *store.BanActiveError
	switch {
	case errors.As(err, &active):
		writeErrorDetails(w, http.StatusConflict, codeConflict,
			"this identity already carries active ban "+active.BanID+"; lift it before banning again",
			map[string]any{"banId": active.BanID})
		return
	case err != nil:
		s.writeInternalError(w, r, err)
		return
	}
	s.announceBanChange(change, "ban placed")

	auditDetail(r, "banId", change.Ban.ID)
	auditDetail(r, "player", noteIdentity{Platform: change.Ban.Platform, ID: change.Ban.PlayerID})
	auditDetail(r, "revision", change.Revision)
	writeJSON(w, http.StatusCreated, banChangeResponse{
		Ban:      newBanView(change.Ban, time.Now().UTC()),
		Revision: change.Revision,
	})
}

// handleLiftBan lifts one ban (spec section 13.2). A ban that is no longer
// active is answered as it stands, so a retried lift is harmless.
func (s *Server) handleLiftBan(w http.ResponseWriter, r *http.Request) {
	banID := r.PathValue("banId")
	auditDetail(r, "banId", banID)
	caller := principalFrom(r.Context())
	change, err := s.store.LiftBan(r.Context(), banID, caller.TokenID, caller.Name, outboundQueueLimit)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, codeNotFound, "no such ban")
		return
	case err != nil:
		s.writeInternalError(w, r, err)
		return
	}
	if change.Changed || len(change.Notified) > 0 {
		s.announceBanChange(change, "ban lifted")
	}
	if change.Ban.ServerID != "" {
		auditServerID(r, change.Ban.ServerID)
	}
	auditDetail(r, "player", noteIdentity{Platform: change.Ban.Platform, ID: change.Ban.PlayerID})
	auditDetail(r, "lifted", change.Changed)
	auditDetail(r, "revision", change.Revision)
	writeJSON(w, http.StatusOK, banChangeResponse{
		Ban:      newBanView(change.Ban, time.Now().UTC()),
		Revision: change.Revision,
	})
}

// announceBanChange wakes the held polls of the servers a change queued a
// notice for, and logs what it could not queue.
func (s *Server) announceBanChange(change store.BanChange, what string) {
	for _, serverID := range change.Notified {
		s.waiters.notify(serverID)
	}
	s.log.Info("installation ban list changed", "what", what, "banId", change.Ban.ID,
		"revision", change.Revision, "notified", len(change.Notified))
	if len(change.Dropped) > 0 {
		// A notice is a nudge; the next session response carries the
		// revision. Loud all the same, since the servers named here are
		// running on the list they had until they reconnect.
		s.log.Warn("bans.changed could not be queued for servers whose queue is full",
			"revision", change.Revision, "servers", change.Dropped)
	}
}

// handleGetBan answers one ban record.
func (s *Server) handleGetBan(w http.ResponseWriter, r *http.Request) {
	ban, err := s.store.Ban(r.Context(), r.PathValue("banId"))
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, codeNotFound, "no such ban")
		return
	case err != nil:
		s.writeInternalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ban": newBanView(ban, time.Now().UTC())})
}

type banListResponse struct {
	Revision   int64     `json:"revision"`
	Bans       []banView `json:"bans"`
	NextCursor string    `json:"nextCursor,omitempty"`
}

// handleListBans answers one page of ban records, newest first (spec section
// 13.2): the active ones by default, every one with state=all, and optionally
// those of one identity.
func (s *Server) handleListBans(w http.ResponseWriter, r *http.Request) {
	parameters := r.URL.Query()
	all := false
	switch state := parameters.Get("state"); state {
	case "", banStateActive:
	case "all":
		all = true
	default:
		writeError(w, http.StatusBadRequest, codeBadRequest, "state must be active or all")
		return
	}
	platform, playerID := parameters.Get("platform"), parameters.Get("playerId")
	_, hasPlatform := parameters["platform"]
	_, hasPlayer := parameters["playerId"]
	if hasPlatform != hasPlayer {
		writeError(w, http.StatusBadRequest, codeBadRequest, "platform and playerId go together: give both, or neither")
		return
	}
	if hasPlatform && (!banIdentityMember(platform, maxPlatformLength) || !banIdentityMember(playerID, maxPlayerIDLength)) {
		writeError(w, http.StatusBadRequest, codeBadRequest,
			"platform and playerId must be non-empty identity members within their bounds, without U+0000")
		return
	}
	limit, ok := parseLimitParam(w, parameters.Get("limit"), defaultBanPage, maxBanPage)
	if !ok {
		return
	}
	after, ok := parseCreatedCursor(w, parameters.Get("cursor"))
	if !ok {
		return
	}

	// The revision and the page are one read, so the revision is the one the
	// records belong to and a reader comparing it with the servers' reports
	// compares like with like.
	now := time.Now().UTC()
	revision, found, err := s.store.BansWithRevision(r.Context(), store.BanQuery{
		All:      all,
		Now:      now,
		Platform: platform,
		PlayerID: playerID,
		Limit:    limit + 1,
		After:    store.BanCursor{CreatedAt: after.at, ID: after.id},
	})
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}
	response := banListResponse{Revision: revision, Bans: make([]banView, 0, min(len(found), limit))}
	if len(found) > limit {
		last := found[limit-1]
		response.NextCursor = encodeCreatedCursor(last.CreatedAt, last.ID)
		found = found[:limit]
	}
	for _, ban := range found {
		response.Bans = append(response.Bans, newBanView(ban, now))
	}
	writeJSON(w, http.StatusOK, response)
}

// pluginBanEntry is one entry of the list a plugin pulls (spec section 13.3):
// the record's id, identity, reason, name, and expiry, and nothing a server
// has no use for.
type pluginBanEntry struct {
	ID        string       `json:"id"`
	Player    noteIdentity `json:"player"`
	Reason    string       `json:"reason"`
	Name      string       `json:"name"`
	ExpiresAt *time.Time   `json:"expiresAt"`
}

type pluginBanPage struct {
	Revision   int64            `json:"revision"`
	Bans       []pluginBanEntry `json:"bans"`
	NextCursor string           `json:"nextCursor,omitempty"`
}

// banWalkCursor is a position in one walk of the active list: the revision
// the walk reads and the last identity it was served. It travels as
// unpadded base64url, whose alphabet is letters, digits, - and _, which the
// section 13.3 rule asks for so a plugin with no URL encoder can send it back
// as it came.
type banWalkCursor struct {
	revision int64
	position store.BanListPosition
}

func encodeBanWalkCursor(cursor banWalkCursor) string {
	encoded, _ := json.Marshal([]any{cursor.revision, cursor.position.Platform, cursor.position.PlayerID})
	return base64.RawURLEncoding.EncodeToString(encoded)
}

func parseBanWalkCursor(value string) (banWalkCursor, bool) {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return banWalkCursor{}, false
	}
	var fields []json.RawMessage
	if json.Unmarshal(decoded, &fields) != nil || len(fields) != 3 {
		return banWalkCursor{}, false
	}
	var cursor banWalkCursor
	if json.Unmarshal(fields[0], &cursor.revision) != nil || cursor.revision < 0 ||
		json.Unmarshal(fields[1], &cursor.position.Platform) != nil ||
		json.Unmarshal(fields[2], &cursor.position.PlayerID) != nil ||
		cursor.position.Platform == "" {
		return banWalkCursor{}, false
	}
	return cursor, true
}

// handlePluginBans serves one page of the active list to a plugin (spec
// section 13.3), at the revision its walk reads: the current one for a first
// page, the cursor's for the rest. Both spellings, GET and POST .../get, land
// here; the POST's body is ignored like a KV get's.
func (s *Server) handlePluginBans(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.authenticateSession(w, r); !ok {
		return
	}
	parameters := r.URL.Query()
	limit, ok := parseLimitParam(w, parameters.Get("limit"), defaultBanPage, maxBanPage)
	if !ok {
		return
	}
	revision := int64(-1)
	var position store.BanListPosition
	if value := parameters.Get("cursor"); value != "" {
		cursor, ok := parseBanWalkCursor(value)
		if !ok {
			writeError(w, http.StatusBadRequest, codeBadRequest, "cursor is not one this hub issued")
			return
		}
		revision, position = cursor.revision, cursor.position
	}

	served, found, err := s.store.BanListAt(r.Context(), revision, position, limit+1)
	switch {
	case errors.Is(err, store.ErrBanRevisionGone):
		writeError(w, http.StatusConflict, codeConflict,
			"this hub cannot serve the revision the cursor names; start the walk over from the first page")
		return
	case err != nil:
		s.writeInternalError(w, r, err)
		return
	}
	page := pluginBanPage{Revision: served, Bans: make([]pluginBanEntry, 0, min(len(found), limit))}
	if len(found) > limit {
		last := found[limit-1]
		page.NextCursor = encodeBanWalkCursor(banWalkCursor{
			revision: served,
			position: store.BanListPosition{Platform: last.Platform, PlayerID: last.PlayerID},
		})
		found = found[:limit]
	}
	for _, ban := range found {
		page.Bans = append(page.Bans, pluginBanEntry{
			ID:        ban.ID,
			Player:    noteIdentity{Platform: ban.Platform, ID: ban.PlayerID},
			Reason:    ban.Reason,
			Name:      ban.Name,
			ExpiresAt: ban.ExpiresAt,
		})
	}
	writeJSON(w, http.StatusOK, page)
}

// prepareBansApplied reads the bans.applied reports in a batch, keyed by batch
// index (spec section 13.4). A body without a usable revision (an integer in
// [0, 2^53)) is left out, which the poll acks and ignores like an unknown
// type: failing the batch over it would stall everything behind a report a
// retry cannot fix.
func prepareBansApplied(envelopes []inboundEnvelope) map[int]int64 {
	var prepared map[int]int64
	for index, e := range envelopes {
		if e.Type != envelopeTypeBansApplied {
			continue
		}
		var body struct {
			Revision *int64 `json:"revision"`
		}
		if json.Unmarshal(e.Body, &body) != nil || body.Revision == nil ||
			*body.Revision < 0 || *body.Revision > maxBanRevision {
			continue
		}
		if prepared == nil {
			prepared = make(map[int]int64)
		}
		prepared[index] = *body.Revision
	}
	return prepared
}

// runBanSweeper takes expired bans off the active list on its own ticker. It
// is not a case of the maintenance loop because that loop's retention passes
// can each run for half a minute, and section 13.1 bounds how long an expired
// ban may stay on the served list at 60 s whatever else the hub is doing.
func (s *Server) runBanSweeper() {
	defer close(s.banSweeperDone)
	ticker := time.NewTicker(s.cfg.BanSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopSweeper:
			return
		case <-ticker.C:
			s.sweepBans()
		}
	}
}

// sweepBans takes expired bans off the active list, once per sweeper tick.
// A failure is logged and the next tick tries again: an expired ban the hub
// still serves costs nothing but a stale entry, which every plugin drops by
// its own clock (spec section 13.4).
func (s *Server) sweepBans() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	change, swept, err := s.store.SweepExpiredBans(ctx, outboundQueueLimit)
	if err != nil {
		s.log.Error("ban expiry sweep failed", "error", err.Error())
		return
	}
	if swept > 0 {
		s.log.Info("expired bans taken off the installation list", "count", swept)
		s.announceBanChange(change, "bans expired")
	}
}
