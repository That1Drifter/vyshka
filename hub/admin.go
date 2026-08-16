package hub

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/That1Drifter/vyshka/hub/internal/token"
	"github.com/That1Drifter/vyshka/hub/store"
)

// Admin API limits. Names and game identifiers are short by nature; the caps
// exist so a bad client cannot fill the database with one field.
const (
	maxServerNameLength = 200
	maxGameLength       = 64

	minEnrollmentTokenTTL = time.Minute
	maxEnrollmentTokenTTL = 30 * 24 * time.Hour
)

// outboundQueueLimit bounds the per-server envelope queue (spec section 9.2).
// At the bound the hub refuses new work: dropping envelopes it has already
// accepted would make "nothing is dropped silently" a lie.
const outboundQueueLimit = 5000

// reservedEnvelopeTypes are the prefixes the hub itself models. The raw queue
// endpoint refuses them, so it can never be used to route around the
// validation the hub performs on them, or to forge a message the plugin will
// take as the hub's own word (spec sections 5.5 and 6).
var reservedEnvelopeTypes = []string{"action.", "manifest."}

// requireAdmin gates a handler on the bootstrap admin token. Scoped tokens
// (spec section 10) replace this in the scoped-tokens slice; until then one
// token carries the `admin` scope and nothing else exists.
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		presented := bearerToken(r)
		if presented == "" || !token.Equal(s.adminTokenHash, token.Hash(presented)) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="vyshka-admin"`)
			writeError(w, http.StatusUnauthorized, codeUnauthorized,
				"a valid admin token is required")
			return
		}
		next(w, r)
	}
}

type createServerRequest struct {
	Name                      string `json:"name"`
	Game                      string `json:"game"`
	EnrollmentTokenTTLSeconds int    `json:"enrollmentTokenTtlSeconds"`
}

type enrollmentView struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type createServerResponse struct {
	Server     serverView     `json:"server"`
	Enrollment enrollmentView `json:"enrollment"`
}

// handleCreateServer records a server and mints its first enrollment token.
// The token is returned here and nowhere else, ever.
func (s *Server) handleCreateServer(w http.ResponseWriter, r *http.Request) {
	var request createServerRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}

	name := strings.TrimSpace(request.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, codeBadRequest, "name is required")
		return
	}
	if len(name) > maxServerNameLength {
		writeError(w, http.StatusBadRequest, codeBadRequest, "name is too long")
		return
	}
	game := normalizeGame(request.Game)
	if len(game) > maxGameLength {
		writeError(w, http.StatusBadRequest, codeBadRequest, "game is too long")
		return
	}

	server, err := s.store.CreateServer(r.Context(), name, game)
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}

	secret, expiresAt, err := s.issueEnrollmentToken(r, server.ID, request.EnrollmentTokenTTLSeconds)
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}

	s.log.Info("server created", "serverId", server.ID, "name", server.Name, "game", server.Game)
	writeJSON(w, http.StatusCreated, createServerResponse{
		Server:     newServerView(server, nil, 0),
		Enrollment: enrollmentView{Token: secret, ExpiresAt: expiresAt},
	})
}

// handleListServers answers with every server record, newest first.
func (s *Server) handleListServers(w http.ResponseWriter, r *http.Request) {
	servers, err := s.store.ListServers(r.Context())
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}

	views := make([]serverView, 0, len(servers))
	for _, server := range servers {
		view, err := s.serverView(r, server)
		if err != nil {
			s.writeInternalError(w, r, err)
			return
		}
		views = append(views, view)
	}

	writeJSON(w, http.StatusOK, map[string]any{"servers": views})
}

// handleGetServer answers with one server record.
func (s *Server) handleGetServer(w http.ResponseWriter, r *http.Request) {
	server, ok := s.lookupServer(w, r)
	if !ok {
		return
	}
	view, err := s.serverView(r, server)
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// serverView assembles the live parts of a server record: its session and how
// much work is queued for it.
func (s *Server) serverView(r *http.Request, server store.Server) (serverView, error) {
	session, err := s.store.LiveSession(r.Context(), server.ID)
	if err != nil {
		return serverView{}, err
	}
	pending, err := s.store.PendingEnvelopeCount(r.Context(), server.ID)
	if err != nil {
		return serverView{}, err
	}
	return newServerView(server, session, pending), nil
}

// handleIssueEnrollmentToken mints a replacement enrollment token, which is how
// an operator recovers a server whose secret was lost or revoked. Any unused
// token the server still had stops working.
func (s *Server) handleIssueEnrollmentToken(w http.ResponseWriter, r *http.Request) {
	server, ok := s.lookupServer(w, r)
	if !ok {
		return
	}

	// The body is optional here: with no TTL to override there is nothing to send.
	var request struct {
		TTLSeconds int `json:"ttlSeconds"`
	}
	if !s.decodeOptionalJSON(w, r, &request) {
		return
	}

	secret, expiresAt, err := s.issueEnrollmentToken(r, server.ID, request.TTLSeconds)
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}

	s.log.Info("enrollment token issued", "serverId", server.ID)
	writeJSON(w, http.StatusCreated, enrollmentView{Token: secret, ExpiresAt: expiresAt})
}

// handleRevokeCredentials disables a server's secret and ends its sessions.
func (s *Server) handleRevokeCredentials(w http.ResponseWriter, r *http.Request) {
	server, ok := s.lookupServer(w, r)
	if !ok {
		return
	}

	if err := s.store.RevokeCredentials(r.Context(), server.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, codeNotFound, "no such server")
			return
		}
		s.writeInternalError(w, r, err)
		return
	}

	// A poll held open must be answered 401 at once, not left to expire
	// (spec section 5.4).
	s.waiters.notify(server.ID)

	s.log.Info("server credentials revoked", "serverId", server.ID)
	w.WriteHeader(http.StatusNoContent)
}

type queueEnvelopeRequest struct {
	Type string          `json:"type"`
	Body json.RawMessage `json:"body"`
}

// queuedEnvelopeView reports what was queued. It carries no `seq`: sequence
// numbers belong to a session, and this envelope may well have been queued while
// the game server was down (spec section 5.5).
type queuedEnvelopeView struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	TS   string `json:"ts"`
}

type queueEnvelopeResponse struct {
	Envelope queuedEnvelopeView `json:"envelope"`
}

// handleQueueEnvelope puts one envelope on a server's queue. It is the
// transport-level primitive: everything the hub models itself is queued the same
// way, through an endpoint that validates what it is queueing.
func (s *Server) handleQueueEnvelope(w http.ResponseWriter, r *http.Request) {
	server, ok := s.lookupServer(w, r)
	if !ok {
		return
	}

	var request queueEnvelopeRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}

	envelopeType := strings.TrimSpace(request.Type)
	switch {
	case envelopeType == "":
		writeError(w, http.StatusBadRequest, codeBadRequest, "type is required")
		return
	case len(envelopeType) > maxEnvelopeType:
		writeError(w, http.StatusBadRequest, codeBadRequest, "type is too long")
		return
	}
	for _, reserved := range reservedEnvelopeTypes {
		if strings.HasPrefix(envelopeType, reserved) {
			writeError(w, http.StatusConflict, codeConflict,
				"envelope type "+envelopeType+" belongs to a dedicated endpoint that validates it")
			return
		}
	}

	body := []byte("{}")
	if len(request.Body) > 0 && string(request.Body) != "null" {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(request.Body, &object); err != nil {
			writeError(w, http.StatusBadRequest, codeBadRequest, "body must be a JSON object")
			return
		}
		body = request.Body
	}

	queued, err := s.store.QueueEnvelope(r.Context(), server.ID, envelopeType, body, outboundQueueLimit)
	switch {
	case errors.Is(err, store.ErrOutboundQueueFull):
		writeError(w, http.StatusConflict, codeOutboundQueueFull,
			"this server already has "+strconv.Itoa(outboundQueueLimit)+" unacked envelopes queued")
		return
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, codeNotFound, "no such server")
		return
	case err != nil:
		s.writeInternalError(w, r, err)
		return
	}

	// Wake whatever poll is holding for this server, so a queued envelope is
	// delivered now rather than whenever the hold happens to expire.
	s.waiters.notify(server.ID)

	s.log.Info("envelope queued",
		"serverId", server.ID, "envelopeId", queued.ID, "type", queued.Type)
	writeJSON(w, http.StatusAccepted, queueEnvelopeResponse{
		Envelope: queuedEnvelopeView{
			ID:   queued.ID,
			Type: queued.Type,
			TS:   envelopeTimestamp(queued.CreatedAt),
		},
	})
}

// lookupServer resolves the {serverId} path value, answering 404 itself when
// there is no such record.
func (s *Server) lookupServer(w http.ResponseWriter, r *http.Request) (store.Server, bool) {
	server, err := s.store.Server(r.Context(), r.PathValue("serverId"))
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, codeNotFound, "no such server")
		return store.Server{}, false
	case err != nil:
		s.writeInternalError(w, r, err)
		return store.Server{}, false
	}
	return server, true
}

// issueEnrollmentToken mints a token, stores its digest, and returns the
// plaintext for the one response that may carry it.
func (s *Server) issueEnrollmentToken(r *http.Request, serverID string, ttlSeconds int) (string, time.Time, error) {
	ttl := s.cfg.EnrollmentTokenTTL
	if ttlSeconds > 0 {
		ttl = time.Duration(ttlSeconds) * time.Second
	}
	ttl = min(max(ttl, minEnrollmentTokenTTL), maxEnrollmentTokenTTL)

	secret := token.New(token.Enrollment)
	expiresAt, err := s.store.IssueEnrollmentToken(r.Context(), serverID, token.Hash(secret), ttl)
	if err != nil {
		return "", time.Time{}, err
	}
	return secret, expiresAt, nil
}
