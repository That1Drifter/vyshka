package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
)

// Protocol error codes (spec/protocol.md section 2.2 and section 5). Clients
// branch on these strings, so they are part of the contract: never reword one
// without a spec change.
const (
	codeBadRequest                 = "bad_request"
	codeUnauthorized               = "unauthorized"
	codeNotFound                   = "not_found"
	codeMethodNotAllowed           = "method_not_allowed"
	codeConflict                   = "conflict"
	codeUnsupportedMediaType       = "unsupported_media_type"
	codePayloadTooLarge            = "payload_too_large"
	codeInternal                   = "internal"
	codeEnrollmentTokenInvalid     = "enrollment_token_invalid"
	codeEnrollmentTokenUsed        = "enrollment_token_used"
	codeGameMismatch               = "game_mismatch"
	codeCredentialsInvalid         = "credentials_invalid"
	codeCredentialsRevoked         = "credentials_revoked"
	codeSessionInvalid             = "session_invalid"
	codeProtocolVersionUnsupported = "protocol_version_unsupported"
	codeEnvelopeInvalid            = "envelope_invalid"
	codeAckOutOfRange              = "ack_out_of_range"
	codeOutboundQueueFull          = "outbound_queue_full"
	codeUnknownAction              = "unknown_action"
	codeParamsInvalid              = "params_invalid"
)

// maxRequestBody caps request bodies. Nothing in this slice is large, and the
// bodies that will be (event batches) get their own limits when they land.
const maxRequestBody = 1 << 20 // 1 MiB

// errorResponse is the body of every 4xx and 5xx answer from either realm.
type errorResponse struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// writeError answers with the protocol error envelope.
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorResponse{Error: errorDetail{Code: code, Message: message}})
}

// writeErrorDetails is writeError for the codes that can point at the part of
// the request they mean, such as the one envelope in a batch that was malformed.
func writeErrorDetails(w http.ResponseWriter, status int, code, message string, details map[string]any) {
	writeJSON(w, status, errorResponse{
		Error: errorDetail{Code: code, Message: message, Details: details},
	})
}

// writeInternalError hides the cause from the client and puts it in the log,
// where it belongs.
func (s *Server) writeInternalError(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("request failed",
		"method", r.Method, "path", r.URL.Path, "error", err.Error())
	writeError(w, http.StatusInternalServerError, codeInternal,
		"the hub failed to handle this request")
}

// decodeJSON reads a JSON request body into dst. It answers the client and
// returns false when the request is unusable, so handlers read as
// `if !s.decodeJSON(...) { return }`.
//
// Unknown fields are deliberately tolerated: additive body evolution is a
// protocol guarantee (spec section 2.1), so a newer plugin sending a field this
// hub has never heard of must still work.
func (s *Server) decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	return s.decodeBody(w, r, dst, false)
}

// decodeOptionalJSON is decodeJSON for endpoints where sending nothing is a
// meaningful request: an idle poll, or a token reissue with no TTL to override.
// An absent body leaves dst at its zero value.
func (s *Server) decodeOptionalJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.ContentLength == 0 {
		return true
	}
	return s.decodeBody(w, r, dst, true)
}

func (s *Server) decodeBody(w http.ResponseWriter, r *http.Request, dst any, allowEmpty bool) bool {
	if contentType := r.Header.Get("Content-Type"); contentType != "" {
		mediaType, _, err := mime.ParseMediaType(contentType)
		if err != nil || mediaType != "application/json" {
			writeError(w, http.StatusUnsupportedMediaType, codeUnsupportedMediaType,
				"request body must be application/json")
			return false
		}
	}

	body := http.MaxBytesReader(w, r.Body, maxRequestBody)
	decoder := json.NewDecoder(body)
	if err := decoder.Decode(dst); err != nil {
		var tooLarge *http.MaxBytesError
		switch {
		case errors.As(err, &tooLarge):
			writeError(w, http.StatusRequestEntityTooLarge, codePayloadTooLarge,
				fmt.Sprintf("request body exceeds %d bytes", maxRequestBody))
		case errors.Is(err, io.EOF):
			if allowEmpty {
				return true
			}
			writeError(w, http.StatusBadRequest, codeBadRequest, "request body is empty")
		default:
			writeError(w, http.StatusBadRequest, codeBadRequest,
				"request body is not valid JSON: "+err.Error())
		}
		return false
	}

	// The body must be one JSON value and nothing more. Without this check a
	// second value, or garbage, after the decoded object would be silently
	// swallowed, and the bytes behind it would never count against the size
	// cap the decoder stopped enforcing.
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, codePayloadTooLarge,
				fmt.Sprintf("request body exceeds %d bytes", maxRequestBody))
			return false
		}
		writeError(w, http.StatusBadRequest, codeBadRequest,
			"request body must be a single JSON value")
		return false
	}
	return true
}

// bearerToken extracts a bearer credential, or "" when the header is missing or
// not a bearer scheme.
func bearerToken(r *http.Request) string {
	header := r.Header.Get("Authorization")
	scheme, credential, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(credential)
}

// handleNotFound answers unrouted paths in the protocol error shape rather than
// with net/http's plain text, so a client can parse every failure the same way.
func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, codeNotFound, "no such endpoint: "+r.URL.Path)
}

// methodNotAllowed answers a known path reached with the wrong method. It is
// registered as the method-less pattern for each route, which ServeMux treats
// as less specific than the same path with a method.
func methodNotAllowed(allowed ...string) http.HandlerFunc {
	allow := strings.Join(allowed, ", ")
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", allow)
		writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed,
			r.Method+" is not allowed here; try "+allow)
	}
}
