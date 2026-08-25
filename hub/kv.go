package hub

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/That1Drifter/vyshka/hub/store"
)

// The key/value store of spec section 12: the same operations on both
// realms, as synchronous request/response, never as envelopes. The Admin API
// side is gated by kv:rw:{namespace}; the Plugin API side is confined to the
// namespaces the server's stored manifest declares in kvNamespaces (section
// 6.6). Both checks run before the key is looked up, so the difference
// between forbidden and not_found cannot probe a namespace the caller was not
// granted.

// KV limits (spec section 12.1).
const (
	maxKVKeyLength  = 128
	maxKVValueBytes = 16384
	minKVTTLSeconds = 1
	maxKVTTLSeconds = 10 * 365 * 24 * 60 * 60 // ten years; a longer TTL is a no-expiry key wearing a costume
	maxKVExactAbs   = int64(1)<<53 - 1
)

// validKVName checks the section 12.1 grammar shared by namespaces and keys:
// one or more non-empty segments of letters, digits, _, and -, separated by
// ".", within a length cap. The alphabet is ASCII, so bytes and code points
// agree and the caps need no rune counting.
func validKVName(value string, maxLength int) bool {
	if value == "" || len(value) > maxLength {
		return false
	}
	segmentLength := 0
	for i := range len(value) {
		c := value[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '_', c == '-':
			segmentLength++
		case c == '.':
			if segmentLength == 0 {
				return false
			}
			segmentLength = 0
		default:
			return false
		}
	}
	return segmentLength > 0
}

// kvPath validates the {namespace} and {key} path values, answering the
// client itself when either breaks the grammar.
func kvPath(w http.ResponseWriter, r *http.Request) (namespace, key string, ok bool) {
	namespace = r.PathValue("namespace")
	key = r.PathValue("key")
	if !validKVName(namespace, maxNamespaceLength) {
		writeError(w, http.StatusBadRequest, codeBadRequest,
			"namespace must be dot-separated segments of letters, digits, _, and -, at most 64 characters")
		return "", "", false
	}
	if !validKVName(key, maxKVKeyLength) {
		writeError(w, http.StatusBadRequest, codeBadRequest,
			"key must be dot-separated segments of letters, digits, _, and -, at most 128 characters")
		return "", "", false
	}
	return namespace, key, true
}

// adminKV adapts one KV operation to the Admin API: the route-level admin
// gate has already run, and this adds the grammar check and the value-level
// kv:rw:{namespace} check of section 10.2.
func (s *Server) adminKV(op func(s *Server, w http.ResponseWriter, r *http.Request, namespace, key string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		namespace, key, ok := kvPath(w, r)
		if !ok {
			return
		}
		if !s.requireScope(w, r, resourceKV, verbRW, namespace) {
			return
		}
		auditDetail(r, "namespace", namespace)
		auditDetail(r, "key", key)
		op(s, w, r, namespace, key)
	}
}

// pluginKV adapts one KV operation to the Plugin API: session authentication,
// the grammar check, and the manifest confinement of section 12.3.
func (s *Server) pluginKV(op func(s *Server, w http.ResponseWriter, r *http.Request, namespace, key string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, server, ok := s.authenticateSession(w, r)
		if !ok {
			return
		}
		namespace, key, ok := kvPath(w, r)
		if !ok {
			return
		}
		if !s.kvNamespaceDeclared(w, r, server.ID, namespace) {
			return
		}
		op(s, w, r, namespace, key)
	}
}

// kvNamespaceDeclared enforces section 12.3's plugin confinement: the
// namespace must appear in the server's stored manifest's kvNamespaces. It
// answers the client itself and returns false when the plugin may not
// proceed. No manifest and an undeclared namespace are the same refusal: both
// mean the manifest grants nothing here.
func (s *Server) kvNamespaceDeclared(w http.ResponseWriter, r *http.Request, serverID, namespace string) bool {
	manifest, err := s.store.Manifest(r.Context(), serverID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusForbidden, codeForbidden,
			"this server has no stored manifest, so no KV namespace is declared; publish one with kvNamespaces")
		return false
	case err != nil:
		s.writeInternalError(w, r, err)
		return false
	}

	var declared struct {
		KVNamespaces []string `json:"kvNamespaces"`
	}
	if err := json.Unmarshal(manifest.Body, &declared); err != nil {
		// A stored manifest was validated before it was stored, so this is a
		// hub bug or a hand-edited database; failing closed is the only safe
		// answer for an authorization source that no longer parses.
		s.writeInternalError(w, r, err)
		return false
	}
	for _, one := range declared.KVNamespaces {
		if one == namespace {
			return true
		}
	}
	writeError(w, http.StatusForbidden, codeForbidden,
		"namespace "+namespace+" is not declared in this server's manifest kvNamespaces")
	return false
}

// kvEntryView is the wire shape of one key (spec section 12.2). Value is
// omitted on a set's answer, which does not echo what the caller sent.
type kvEntryView struct {
	Namespace string          `json:"namespace"`
	Key       string          `json:"key"`
	Value     json.RawMessage `json:"value,omitempty"`
	Revision  int64           `json:"revision"`
	ExpiresAt *time.Time      `json:"expiresAt,omitempty"`
}

func newKVEntryView(entry store.KVEntry) kvEntryView {
	return kvEntryView{
		Namespace: entry.Namespace,
		Key:       entry.Key,
		Value:     entry.Value,
		Revision:  entry.Revision,
		ExpiresAt: entry.ExpiresAt,
	}
}

// kvGet answers one key, or not_found when it is absent or expired.
func kvGet(s *Server, w http.ResponseWriter, r *http.Request, namespace, key string) {
	entry, err := s.store.KVGet(r.Context(), namespace, key)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, codeNotFound, "no such key")
		return
	case err != nil:
		s.writeInternalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newKVEntryView(entry))
}

type kvSetRequest struct {
	Value      json.RawMessage `json:"value"`
	IfRevision *int64          `json:"ifRevision"`
	TTLSeconds *int64          `json:"ttlSeconds"`
}

// kvSet writes one key, optionally as a compare-and-swap (spec section 12.2).
func kvSet(s *Server, w http.ResponseWriter, r *http.Request, namespace, key string) {
	var request kvSetRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}

	// A JSON null reads as the field being absent (section 12.1), which for
	// the one required field means the same refusal as leaving it out.
	if len(request.Value) == 0 || string(request.Value) == "null" {
		writeError(w, http.StatusBadRequest, codeBadRequest, "value is required and may not be null")
		return
	}
	if len(request.Value) > maxKVValueBytes {
		writeError(w, http.StatusBadRequest, codeBadRequest,
			"value is larger than the 16384 byte cap")
		return
	}
	if request.IfRevision != nil && (*request.IfRevision < 0 || *request.IfRevision > maxKVExactAbs) {
		writeError(w, http.StatusBadRequest, codeBadRequest,
			"ifRevision must be 0 (the key must not exist) or a revision within [1, 2^53)")
		return
	}
	var ttl *time.Duration
	if request.TTLSeconds != nil {
		if *request.TTLSeconds < minKVTTLSeconds || *request.TTLSeconds > maxKVTTLSeconds {
			writeError(w, http.StatusBadRequest, codeBadRequest,
				"ttlSeconds must be between 1 and 315360000 (ten years); omit it for a key that never expires")
			return
		}
		duration := time.Duration(*request.TTLSeconds) * time.Second
		ttl = &duration
	}

	entry, err := s.store.KVSet(r.Context(), namespace, key, request.Value, request.IfRevision, ttl)
	var mismatch *store.KVRevisionMismatchError
	switch {
	case errors.As(err, &mismatch):
		// details.revision is what lets the loser re-read and retry without a
		// second round-trip; 0 means the key does not exist.
		writeErrorDetails(w, http.StatusConflict, codeRevisionMismatch, mismatch.Error(),
			map[string]any{"revision": mismatch.Current})
		return
	case errors.Is(err, store.ErrKVRevisionExhausted):
		writeError(w, http.StatusConflict, codeConflict, err.Error())
		return
	case err != nil:
		s.writeInternalError(w, r, err)
		return
	}

	auditDetail(r, "revision", entry.Revision)
	writeJSON(w, http.StatusOK, newKVEntryView(entry))
}

// kvDelete removes one key. A retry that finds it already gone gets not_found
// and treats it as success: the key is gone either way (spec section 12.2).
func kvDelete(s *Server, w http.ResponseWriter, r *http.Request, namespace, key string) {
	err := s.store.KVDelete(r.Context(), namespace, key)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, codeNotFound, "no such key")
		return
	case err != nil:
		s.writeInternalError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type kvIncrRequest struct {
	Delta *int64 `json:"delta"`
}

// kvIncr atomically adds an integer to one key (spec section 12.2). The body
// is optional: an empty one means delta 1, and decrement is a negative delta.
func kvIncr(s *Server, w http.ResponseWriter, r *http.Request, namespace, key string) {
	var request kvIncrRequest
	if !s.decodeOptionalJSON(w, r, &request) {
		return
	}

	delta := int64(1)
	if request.Delta != nil {
		delta = *request.Delta
	}
	if delta > maxKVExactAbs || delta < -maxKVExactAbs {
		writeError(w, http.StatusBadRequest, codeBadRequest,
			"delta must be an integer within (-2^53, 2^53)")
		return
	}

	entry, err := s.store.KVIncr(r.Context(), namespace, key, delta)
	switch {
	case errors.Is(err, store.ErrKVNotInteger),
		errors.Is(err, store.ErrKVRangeExceeded),
		errors.Is(err, store.ErrKVRevisionExhausted):
		writeError(w, http.StatusConflict, codeConflict, err.Error())
		return
	case err != nil:
		s.writeInternalError(w, r, err)
		return
	}

	auditDetail(r, "revision", entry.Revision)
	auditDetail(r, "delta", delta)
	writeJSON(w, http.StatusOK, newKVEntryView(entry))
}
