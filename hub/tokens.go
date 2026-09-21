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

// Admin token limits.
const (
	maxTokenNameLength = 200
	// minTokenTTL keeps a mistyped `expiresInSeconds` from minting a credential
	// that is already useless by the time the operator has copied it.
	minTokenTTL = time.Minute
	// maxTokenTTL bounds the other end. A century is not a lifetime anyone
	// means, and the cap is what keeps the conversion below from overflowing.
	maxTokenTTL = 100 * 365 * 24 * time.Hour
)

// clampTokenTTL applies the bounds to a requested lifetime, in seconds. Zero or
// negative returns zero, which callers read as "no expiry".
//
// The comparison happens in seconds rather than in time.Duration, for the same
// reason clampActionTTL does it that way: a large request multiplied by
// time.Second overflows int64 and wraps, and a wrapped value would sail under
// whichever bound it was meant to hit. Both ends are checked before the
// multiplication rather than after, and the non-positive case is handled here
// rather than left to the one caller that currently guards it, because a helper
// that is only correct given its caller's guard is a trap for the next caller.
func clampTokenTTL(requestedSeconds int) time.Duration {
	switch {
	case requestedSeconds <= 0:
		return 0
	case requestedSeconds > int(maxTokenTTL/time.Second):
		return maxTokenTTL
	default:
		return max(time.Duration(requestedSeconds)*time.Second, minTokenTTL)
	}
}

type createTokenRequest struct {
	Name   string   `json:"name"`
	Scopes []string `json:"scopes"`
	// Servers is the server binding of spec section 10.1: absent or empty
	// mints an unbound token.
	Servers []string `json:"servers"`
	// ExpiresInSeconds is optional. Absent or zero mints a token that does not
	// expire on its own, which is what a hub-to-panel credential wants;
	// negative is a mistake rather than a synonym for either.
	ExpiresInSeconds int `json:"expiresInSeconds"`
}

// adminTokenView is the Admin API representation of a token record. It carries
// no secret and no digest: the secret exists only in the response that mints
// it, and the digest is not something an operator has any use for.
type adminTokenView struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Scopes []string `json:"scopes"`
	// Servers is always present, `[]` for an unbound token, so a client never
	// has to guess what an absent member means (spec section 10.4).
	Servers   []string   `json:"servers"`
	CreatedAt time.Time  `json:"createdAt"`
	CreatedBy string     `json:"createdBy,omitempty"`
	ExpiresAt *time.Time `json:"expiresAt"`
	RevokedAt *time.Time `json:"revokedAt"`
}

func newAdminTokenView(stored store.AdminToken) adminTokenView {
	scopes := stored.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	servers := stored.Servers
	if servers == nil {
		servers = []string{}
	}
	return adminTokenView{
		ID:        stored.ID,
		Name:      stored.Name,
		Scopes:    scopes,
		Servers:   servers,
		CreatedAt: stored.CreatedAt,
		CreatedBy: stored.CreatedBy,
		ExpiresAt: stored.ExpiresAt,
		RevokedAt: stored.RevokedAt,
	}
}

type createTokenResponse struct {
	Token adminTokenView `json:"token"`
	// Secret is returned here and nowhere else, ever: the hub stores a digest.
	Secret string `json:"secret"`
}

// handleCreateToken mints a scoped Admin API token (spec section 10).
func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	var request createTokenRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}

	name := strings.TrimSpace(request.Name)
	switch {
	case name == "":
		writeError(w, http.StatusBadRequest, codeBadRequest, "name is required")
		return
	case len(name) > maxTokenNameLength:
		writeError(w, http.StatusBadRequest, codeBadRequest, "name is too long")
		return
	case request.ExpiresInSeconds < 0:
		writeError(w, http.StatusBadRequest, codeBadRequest,
			"expiresInSeconds cannot be negative; omit it for a token that does not expire")
		return
	}

	scopes, ok := s.parseRequestedScopes(w, r, request.Scopes)
	if !ok {
		return
	}
	servers, ok := s.parseRequestedBinding(w, r, request.Servers, scopes)
	if !ok {
		return
	}

	ttl := time.Duration(0)
	if request.ExpiresInSeconds > 0 {
		ttl = clampTokenTTL(request.ExpiresInSeconds)
	}

	secret := token.New(token.Admin)
	stored, err := s.store.CreateAdminToken(r.Context(), store.NewAdminToken{
		Name:      name,
		TokenHash: token.Hash(secret),
		Scopes:    scopes,
		Servers:   servers,
		CreatedBy: principalFrom(r.Context()).TokenID,
		TTL:       ttl,
	})
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}

	auditDetail(r, "tokenId", stored.ID)
	auditDetail(r, "tokenName", stored.Name)
	auditDetail(r, "scopes", scopes)
	auditDetail(r, "servers", servers)
	s.log.Info("admin token minted", "tokenId", stored.ID, "name", stored.Name, "scopes", scopes, "servers", servers)
	writeJSON(w, http.StatusCreated, createTokenResponse{
		Token:  newAdminTokenView(stored),
		Secret: secret,
	})
}

// maxTokenServers bounds a server binding, the same bound a webhook's
// serverIds carries: a token confined to more than fifty named servers is
// one the operator meant to leave unbound.
const maxTokenServers = 50

// parseRequestedBinding validates the server binding a mint asks for (spec
// section 10.1) and returns it deduplicated in request order; an empty result
// is an unbound token. It refuses, with `bad_request`, a binding that is
// oversized, an id outside the identifier alphabet, and a scope the binding
// cannot carry (`admin`, `webhooks:manage`), and with `not_found` an id that
// names no server the hub knows, the rule a webhook's serverIds follows: a
// typo would otherwise become a token that silently grants nothing.
//
// The existence check runs against the store one id at a time. A binding is
// at most fifty ids and a mint is rare, so the round trips are not worth a
// batch query, and the first unknown id is the answer.
func (s *Server) parseRequestedBinding(w http.ResponseWriter, r *http.Request, requested, scopes []string) ([]string, bool) {
	if len(requested) == 0 {
		return []string{}, true
	}
	if len(requested) > maxTokenServers {
		writeError(w, http.StatusBadRequest, codeBadRequest,
			"a token is bound to at most "+strconv.Itoa(maxTokenServers)+" servers; leave servers empty for a token that applies to every server")
		return nil, false
	}
	for _, text := range scopes {
		scope, err := ParseScope(text)
		if err != nil {
			continue
		}
		if scope.Resource == resourceAdmin || scope.Resource == resourceWebhooks {
			writeError(w, http.StatusBadRequest, codeBadRequest,
				"a token bound to servers cannot carry "+text+": that grant has no server dimension (spec section 10.1)")
			return nil, false
		}
	}
	seen := make(map[string]bool, len(requested))
	servers := make([]string, 0, len(requested))
	for _, serverID := range requested {
		// An id is exact: no wildcard of any spelling, since a pattern in a
		// binding would be a grammar this draft does not define, and one
		// that fell through to the lookup would be misreported as an
		// unknown server.
		if serverID == "" || len(serverID) > maxScopePattern || strings.Contains(serverID, "*") || !validScopePattern(serverID) {
			writeError(w, http.StatusBadRequest, codeBadRequest,
				"servers must be exact server ids: "+truncateUTF8(serverID, 64)+" is not one")
			return nil, false
		}
		if seen[serverID] {
			continue
		}
		switch _, err := s.store.Server(r.Context(), serverID); {
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound,
				"servers names "+serverID+", which is not a server this hub knows")
			return nil, false
		case err != nil:
			s.writeInternalError(w, r, err)
			return nil, false
		}
		seen[serverID] = true
		servers = append(servers, serverID)
	}
	return servers, true
}

// parseRequestedScopes validates the scopes a mint asks for and returns them in
// canonical form, deduplicated.
//
// A token may never be minted with a scope its minter does not itself hold.
// Only `admin` reaches this endpoint today, and `admin` implies everything, so
// the check refuses nothing yet; it is here because the day the mint scope is
// narrowed is not the day anyone will remember to add it.
func (s *Server) parseRequestedScopes(w http.ResponseWriter, r *http.Request, requested []string) ([]string, bool) {
	if len(requested) == 0 {
		writeError(w, http.StatusBadRequest, codeBadRequest,
			"scopes is required; a token with no scopes could do nothing")
		return nil, false
	}
	if len(requested) > maxScopesPerToken {
		writeError(w, http.StatusBadRequest, codeBadRequest,
			"a token carries at most "+strconv.Itoa(maxScopesPerToken)+" scopes")
		return nil, false
	}

	minter := principalFrom(r.Context())
	seen := make(map[string]bool, len(requested))
	scopes := make([]string, 0, len(requested))
	for _, text := range requested {
		scope, err := ParseScope(text)
		if err != nil {
			writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
			return nil, false
		}
		canonical := scope.String()
		if seen[canonical] {
			continue
		}
		if !minter.covers(scope.Resource, scope.Verb, scope.Pattern) {
			writeError(w, http.StatusForbidden, codeForbidden,
				"this token cannot grant "+canonical+", which it does not itself hold")
			return nil, false
		}
		seen[canonical] = true
		scopes = append(scopes, canonical)
	}
	return scopes, true
}

// handleListTokens answers with every token record, newest first, revoked ones
// included: an operator auditing access needs to see what used to exist.
func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	tokens, err := s.store.ListAdminTokens(r.Context())
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}

	views := make([]adminTokenView, 0, len(tokens))
	for _, stored := range tokens {
		views = append(views, newAdminTokenView(stored))
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": views})
}

// handleRevokeToken retires a token. The record survives, so the audit log's
// references to it keep resolving.
//
// The bootstrap credential cannot be revoked here because it has no record to
// revoke: it is retired by unsetting VYSHKA_ADMIN_TOKEN and restarting, which
// is also the only way back in if every minted token is revoked.
func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	tokenID := r.PathValue("tokenId")
	auditDetail(r, "tokenId", tokenID)

	switch err := s.store.RevokeAdminToken(r.Context(), tokenID); {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, codeNotFound, "no such token")
		return
	case err != nil:
		s.writeInternalError(w, r, err)
		return
	}

	s.log.Info("admin token revoked", "tokenId", tokenID,
		"revokedBy", principalFrom(r.Context()).Name)
	w.WriteHeader(http.StatusNoContent)
}
