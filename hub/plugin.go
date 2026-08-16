package hub

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/That1Drifter/vyshka/hub/internal/token"
	"github.com/That1Drifter/vyshka/hub/store"
)

type pluginDescriptor struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type enrollRequest struct {
	EnrollmentToken string           `json:"enrollmentToken"`
	Game            string           `json:"game"`
	Plugin          pluginDescriptor `json:"plugin"`
	Transports      []string         `json:"transports"`
}

type enrollResponse struct {
	ServerID     string         `json:"serverId"`
	ServerSecret string         `json:"serverSecret"`
	Server       serverIdentity `json:"server"`
}

// handleEnroll exchanges a one-time enrollment token for permanent server
// credentials (spec section 5.2). The secret is returned here and nowhere else.
func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	var request enrollRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}

	enrollmentToken := strings.TrimSpace(request.EnrollmentToken)
	if enrollmentToken == "" {
		writeError(w, http.StatusBadRequest, codeBadRequest, "enrollmentToken is required")
		return
	}
	game := normalizeGame(request.Game)
	if game == "" {
		writeError(w, http.StatusBadRequest, codeBadRequest, "game is required")
		return
	}
	if len(game) > maxGameLength {
		writeError(w, http.StatusBadRequest, codeBadRequest, "game is too long")
		return
	}

	secret := token.New(token.Server)
	server, err := s.store.Enroll(r.Context(), token.Hash(enrollmentToken), game, token.Hash(secret),
		store.PluginInfo{
			Name:       request.Plugin.Name,
			Version:    request.Plugin.Version,
			Transports: request.Transports,
		})
	switch {
	case errors.Is(err, store.ErrEnrollmentTokenInvalid):
		writeError(w, http.StatusUnauthorized, codeEnrollmentTokenInvalid,
			"enrollment token is unknown or expired")
		return
	case errors.Is(err, store.ErrEnrollmentTokenUsed):
		writeError(w, http.StatusConflict, codeEnrollmentTokenUsed,
			"enrollment token has already been used; ask the operator for a new one")
		return
	case errors.Is(err, store.ErrGameMismatch):
		writeError(w, http.StatusConflict, codeGameMismatch,
			"this server record is registered for a different game")
		return
	case err != nil:
		s.writeInternalError(w, r, err)
		return
	}

	// Enrolling replaces the secret and ends every session derived from the old
	// one, so any poll held on those sessions has to stop holding.
	s.waiters.notify(server.ID)

	s.log.Info("server enrolled",
		"serverId", server.ID, "game", server.Game,
		"plugin", request.Plugin.Name, "pluginVersion", request.Plugin.Version)

	writeJSON(w, http.StatusCreated, enrollResponse{
		ServerID:     server.ID,
		ServerSecret: secret,
		Server:       newServerIdentity(server),
	})
}

type sessionRequest struct {
	ServerID           string           `json:"serverId"`
	ServerSecret       string           `json:"serverSecret"`
	ProtocolVersion    int              `json:"protocolVersion"`
	PollTimeoutSeconds int              `json:"pollTimeoutSeconds"`
	Plugin             pluginDescriptor `json:"plugin"`
	Transports         []string         `json:"transports"`
}

// sessionResponse is authoritative for everything the plugin asked for: what
// comes back governs the session, not what was requested.
type sessionResponse struct {
	SessionID string `json:"sessionId"`
	// SessionToken is present only in the response that mints it.
	SessionToken       string         `json:"sessionToken,omitempty"`
	ExpiresAt          time.Time      `json:"expiresAt"`
	ProtocolVersion    int            `json:"protocolVersion"`
	EnvelopeVersion    int            `json:"envelopeVersion"`
	PollTimeoutSeconds int            `json:"pollTimeoutSeconds"`
	Transports         []string       `json:"transports"`
	Features           map[string]any `json:"features"`
	Server             serverIdentity `json:"server"`
}

// handleCreateSession turns server credentials into a short-lived session
// (spec section 5.3), ending whatever session that server had before.
func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var request sessionRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}

	serverID := strings.TrimSpace(request.ServerID)
	serverSecret := strings.TrimSpace(request.ServerSecret)
	if serverID == "" || serverSecret == "" {
		writeError(w, http.StatusBadRequest, codeBadRequest,
			"serverId and serverSecret are required")
		return
	}

	protocolVersion := request.ProtocolVersion
	if protocolVersion == 0 {
		protocolVersion = ProtocolVersion
	}
	if !protocolVersionSupported(protocolVersion) {
		writeError(w, http.StatusBadRequest, codeProtocolVersionUnsupported,
			"this hub does not speak protocol version "+strconv.Itoa(protocolVersion))
		return
	}

	server, err := s.store.AuthenticateServer(r.Context(), serverID, token.Hash(serverSecret))
	switch {
	case errors.Is(err, store.ErrCredentialsInvalid):
		writeError(w, http.StatusUnauthorized, codeCredentialsInvalid,
			"server id and secret do not match")
		return
	case errors.Is(err, store.ErrCredentialsRevoked):
		writeError(w, http.StatusUnauthorized, codeCredentialsRevoked,
			"these credentials were revoked; enroll again with a new enrollment token")
		return
	case err != nil:
		s.writeInternalError(w, r, err)
		return
	}

	pollTimeout := clampPollTimeout(time.Duration(request.PollTimeoutSeconds) * time.Second)
	sessionToken := token.New(token.Session)
	session, err := s.store.StartSession(r.Context(), store.NewSession{
		ServerID:           server.ID,
		TokenHash:          token.Hash(sessionToken),
		TTL:                s.cfg.SessionTTL,
		ProtocolVersion:    protocolVersion,
		PollTimeoutSeconds: int(pollTimeout.Seconds()),
		Plugin: store.PluginInfo{
			Name:       request.Plugin.Name,
			Version:    request.Plugin.Version,
			Transports: request.Transports,
		},
	})
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}

	// The previous session was just superseded; its held poll must learn that
	// now rather than at the end of its hold.
	s.waiters.notify(server.ID)

	s.log.Info("session started",
		"serverId", server.ID, "sessionId", session.ID,
		"pollTimeoutSeconds", session.PollTimeoutSeconds,
		"protocolVersion", session.ProtocolVersion)

	response := newSessionResponse(session, server)
	response.SessionToken = sessionToken
	writeJSON(w, http.StatusOK, response)
}

// handleGetSession reports the session behind a session token. It is how a
// plugin (and the conformance suite) learns that a token has stopped being
// valid without waiting for a poll to fail.
func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	session, server, ok := s.authenticateSession(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, newSessionResponse(session, server))
}

func newSessionResponse(session store.Session, server store.Server) sessionResponse {
	return sessionResponse{
		SessionID:          session.ID,
		ExpiresAt:          session.ExpiresAt,
		ProtocolVersion:    session.ProtocolVersion,
		EnvelopeVersion:    EnvelopeVersion,
		PollTimeoutSeconds: session.PollTimeoutSeconds,
		Transports:         supportedTransports,
		Features:           map[string]any{},
		Server:             newServerIdentity(server),
	}
}

// authenticateSession resolves the bearer session token on a Plugin API call.
// Expired, unknown, ended, and superseded tokens are all one answer: the plugin
// must ask for a new session, and there is nothing else it could usefully do.
func (s *Server) authenticateSession(w http.ResponseWriter, r *http.Request) (store.Session, store.Server, bool) {
	presented := bearerToken(r)
	if presented == "" {
		s.rejectSession(w, "a session token is required")
		return store.Session{}, store.Server{}, false
	}

	session, server, err := s.store.SessionByToken(r.Context(), token.Hash(presented))
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.rejectSession(w, "session token is expired, unknown, or superseded")
		return store.Session{}, store.Server{}, false
	case err != nil:
		s.writeInternalError(w, r, err)
		return store.Session{}, store.Server{}, false
	}
	if server.RevokedAt != nil {
		s.rejectSession(w, "the credentials behind this session were revoked")
		return store.Session{}, store.Server{}, false
	}
	return session, server, true
}

func (s *Server) rejectSession(w http.ResponseWriter, message string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="vyshka-plugin"`)
	writeError(w, http.StatusUnauthorized, codeSessionInvalid, message)
}
