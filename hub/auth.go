package hub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/That1Drifter/vyshka/hub/internal/token"
	"github.com/That1Drifter/vyshka/hub/store"
)

// contextKey keeps the values this package attaches to a request out of the
// reach of every other package's keys.
type contextKey int

const (
	principalKey contextKey = iota
	auditKey
)

// bootstrapTokenName is what the audit log calls the credential configured
// through -admin-token or minted on first run. It has no row in admin_tokens,
// so it has no id either, and the log says so rather than leaving the field
// looking like a missing value.
const bootstrapTokenName = "bootstrap"

// admin gates one Admin API route (spec section 10). It authenticates the
// caller, refuses a token that holds no grant at all on the route's
// resource:verb, and records every mutation in the audit log.
//
// The scope check here is deliberately the coarse one: it asks whether the
// token could ever be allowed to do this, before the request body is read. The
// routes whose answer depends on a value in the request (which action code is
// being dispatched, which event types are being read) run a second, exact check
// inside the handler, because that value does not exist yet at this point.
func (s *Server) admin(resource, verb string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller, ok := s.authenticate(w, r)
		if !ok {
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), principalKey, caller))

		// Reads are not audited. Auditing them would bury the changes an
		// operator is looking for under a panel's polling, and section 10 asks
		// for a record of mutations.
		mutation := r.Method != http.MethodGet && r.Method != http.MethodHead
		var entry *auditEntry
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		if mutation {
			// The entry exists before the body is read, so that a request that
			// dies while its body is in flight is still recorded. Creating it
			// afterwards would give an authenticated caller a way to make a
			// refused mutation attempt vanish from the log by hanging up.
			entry = &auditEntry{detail: map[string]any{}}
			r = r.WithContext(context.WithValue(r.Context(), auditKey, entry))

			digest, body, ok := captureBody(recorder, r)
			if !ok {
				s.recordAudit(r, caller, entry, recorder.status)
				return
			}
			entry.payloadDigest = digest
			r.Body = body
		}

		if caller.allowsAny(resource, verb) {
			next(recorder, r)
		} else {
			// 403 rather than 404: this hub is a single operator's trust
			// boundary, so hiding a route's existence from a credential that
			// already authenticated buys nothing and costs a debuggable answer.
			writeError(recorder, http.StatusForbidden, codeForbidden,
				"this token does not carry the "+Scope{Resource: resource, Verb: verb}.String()+" scope")
		}

		if mutation {
			s.recordAudit(r, caller, entry, recorder.status)
		}
	}
}

// requireScope is the value-level check a handler runs once it knows what is
// being acted on. It answers the client itself and returns false when the
// caller may not proceed.
func (s *Server) requireScope(w http.ResponseWriter, r *http.Request, resource, verb, value string) bool {
	if principalFrom(r.Context()).allows(resource, verb, value) {
		return true
	}
	writeError(w, http.StatusForbidden, codeForbidden,
		"this token does not carry the "+Scope{Resource: resource, Verb: verb, Pattern: value}.String()+" scope")
	return false
}

// authenticate resolves the bearer credential to a principal, answering 401
// itself when it cannot.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (*principal, bool) {
	unauthorized := func() (*principal, bool) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="vyshka-admin"`)
		writeError(w, http.StatusUnauthorized, codeUnauthorized,
			"a valid admin token is required")
		return nil, false
	}

	presented := bearerToken(r)
	if presented == "" {
		return unauthorized()
	}
	hash := token.Hash(presented)

	// The bootstrap credential is checked first and compared in constant time,
	// because it is the one secret this hub holds in memory rather than looking
	// up by its digest.
	if s.bootstrapTokenHash != "" && token.Equal(s.bootstrapTokenHash, hash) {
		return &principal{Name: bootstrapTokenName, Scopes: []Scope{{Resource: resourceAdmin}}}, true
	}

	stored, err := s.store.AdminTokenByHash(r.Context(), hash)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return unauthorized()
	case errors.Is(err, store.ErrTokenRevoked):
		// Logged apart from an unknown token because the two mean very
		// different things to an operator reading the log, and reported the
		// same on the wire because the caller has no business learning which.
		s.log.Warn("admin token refused", "reason", "revoked or expired", "remote", r.RemoteAddr)
		return unauthorized()
	case err != nil:
		s.writeInternalError(w, r, err)
		return nil, false
	}

	caller := &principal{TokenID: stored.ID, Name: stored.Name}
	for _, text := range stored.Scopes {
		scope, err := ParseScope(text)
		if err != nil {
			// A stored scope that no longer parses is a hub bug or a hand-edited
			// database. Dropping the grant is the safe direction: the token can
			// do less than it was minted for, never more.
			s.log.Error("stored scope is not parseable, ignoring it",
				"tokenId", stored.ID, "scope", text, "error", err.Error())
			continue
		}
		caller.Scopes = append(caller.Scopes, scope)
	}
	return caller, true
}

// principalFrom returns the authenticated caller. Every Admin API handler runs
// behind admin, so the value is always present; a handler wired without it
// would get a principal holding nothing, which fails closed.
func principalFrom(ctx context.Context) *principal {
	if caller, ok := ctx.Value(principalKey).(*principal); ok {
		return caller
	}
	return &principal{}
}

// auditEntry accumulates what a handler wants recorded beyond the request line
// itself. It is written from one request's goroutine only, so it needs no lock.
type auditEntry struct {
	payloadDigest string
	// serverID is set by handlers on routes whose path names no {serverId},
	// which is how creating a server still lands in that server's audit view.
	serverID string
	detail   map[string]any
}

// auditDetail annotates the audit record of the request in flight. It is a
// no-op on a read, which has no record to annotate.
func auditDetail(r *http.Request, key string, value any) {
	entry, ok := r.Context().Value(auditKey).(*auditEntry)
	if !ok {
		return
	}
	entry.detail[key] = value
}

// auditServerID names the server a mutation acted on when the route itself does
// not. Without it, `GET /api/v1/audit?serverId=...` would miss the one entry an
// operator most wants to find there: the call that created the server.
func auditServerID(r *http.Request, serverID string) {
	entry, ok := r.Context().Value(auditKey).(*auditEntry)
	if !ok {
		return
	}
	entry.serverID = serverID
}

// recordAudit writes one entry once the handler has answered.
//
// The context is deliberately detached from the request's: a client that hangs
// up mid-request has still made whatever change it made, and an audit log that
// dropped exactly the records of abandoned requests would be missing the ones
// worth reading.
func (s *Server) recordAudit(r *http.Request, caller *principal, entry *auditEntry, status int) {
	detail, err := json.Marshal(entry.detail)
	if err != nil {
		detail = []byte(`{}`)
	}
	serverID := r.PathValue("serverId")
	if serverID == "" {
		serverID = entry.serverID
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer cancel()

	if _, err := s.store.RecordAudit(ctx, store.NewAuditRecord{
		TokenID:       caller.TokenID,
		TokenName:     caller.Name,
		Method:        r.Method,
		Path:          r.URL.Path,
		Status:        status,
		SourceIP:      sourceIP(r),
		PayloadDigest: entry.payloadDigest,
		ServerID:      serverID,
		Detail:        detail,
		Retention:     s.cfg.AuditRetention,
	}); err != nil {
		// The mutation already happened; failing the response now would tell
		// the client the opposite. Loud in the log is the honest answer.
		s.log.Error("audit record could not be written",
			"method", r.Method, "path", r.URL.Path, "tokenId", caller.TokenID,
			"error", err.Error())
	}
}

// sourceIP is the peer address of the connection, never a forwarded header. A
// header is whatever the client typed, and an audit field an attacker can set
// is worse than one that merely names a reverse proxy.
func sourceIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// captureBody reads a mutation's body so the audit log can record its digest,
// and hands back a reader the handler consumes in its place.
//
// It reads one byte past the request cap so that an over-cap body still trips
// the MaxBytesReader inside decodeJSON, which is what produces the protocol's
// payload_too_large answer. An over-cap body gets **no** digest rather than a
// digest of the prefix: two different oversized requests share their first
// megabyte often enough that a prefix hash would collide for bodies that are
// not the same, and a field that looks like a body digest and is not is worse
// than an empty one. Nothing was accepted from such a request anyway.
func captureBody(w http.ResponseWriter, r *http.Request) (string, io.ReadCloser, bool) {
	if r.Body == nil || r.Body == http.NoBody {
		return "", http.NoBody, true
	}

	buffered, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, codeBadRequest, "request body could not be read")
		return "", io.NopCloser(bytes.NewReader(buffered)), false
	}
	if len(buffered) == 0 {
		return "", http.NoBody, true
	}
	if len(buffered) > maxRequestBody {
		auditDetail(r, "payloadOversized", true)
		return "", io.NopCloser(bytes.NewReader(buffered)), true
	}

	sum := sha256.Sum256(buffered)
	return hex.EncodeToString(sum[:]), io.NopCloser(bytes.NewReader(buffered)), true
}
