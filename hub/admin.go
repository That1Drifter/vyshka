package hub

import (
	"errors"
	"net/http"
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
		Server:     newServerView(server, nil),
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
		session, err := s.store.LiveSession(r.Context(), server.ID)
		if err != nil {
			s.writeInternalError(w, r, err)
			return
		}
		views = append(views, newServerView(server, session))
	}

	writeJSON(w, http.StatusOK, map[string]any{"servers": views})
}

// handleGetServer answers with one server record.
func (s *Server) handleGetServer(w http.ResponseWriter, r *http.Request) {
	server, ok := s.lookupServer(w, r)
	if !ok {
		return
	}
	session, err := s.store.LiveSession(r.Context(), server.ID)
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newServerView(server, session))
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
	if r.ContentLength != 0 {
		if !s.decodeJSON(w, r, &request) {
			return
		}
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

	s.log.Info("server credentials revoked", "serverId", server.ID)
	w.WriteHeader(http.StatusNoContent)
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
