package hub

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
)

// Inline errors (spec section 2.3): a Plugin API request that carries
// `?errors=inline` gets every refusal the hub would have sent as a 4xx or 5xx
// delivered as a 200 with the same body plus error.status, because some engine
// HTTP clients hand script an opaque error code for anything but a 2xx and the
// protocol's error.code would otherwise never reach the plugin.
//
// It is a request option and not a session property on purpose: the requests
// where a plugin most needs to read the refusal are the ones no session can
// cover (enrollment, a session request that fails, a token the hub no longer
// knows). The rewrite is done by a ResponseWriter wrapper around the whole
// Plugin API so that no handler has to know about it, and so that a refusal
// written from any depth (a body-size cap, a mid-hold supersession) is
// rewritten the same way.

const (
	inlineErrorsParam = "errors"
	inlineErrorsMode  = "inline"
)

// inlineErrors is the middleware in front of every /plugin/ path.
func (s *Server) inlineErrors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modes := r.URL.Query()[inlineErrorsParam]
		switch {
		case len(modes) == 0:
			next.ServeHTTP(w, r)
		case len(modes) == 1 && modes[0] == inlineErrorsMode:
			writer := &inlineErrorWriter{ResponseWriter: w}
			next.ServeHTTP(writer, r)
			writer.finish()
		default:
			// The hub cannot know what a client asking for a mode it does not
			// offer expects, so it refuses in ordinary form (section 2.3).
			writeError(w, http.StatusBadRequest, codeBadRequest,
				inlineErrorsParam+"="+strings.Join(modes, ",")+" is not a mode this hub offers; only "+
					inlineErrorsMode+" exists")
		}
	})
}

// inlineErrorWriter passes successes through untouched and captures a failure
// so that its status can be moved into the body. The header write of a failure
// is deferred until finish, when the rewritten body is known; headers set on
// the way (Connection: close, WWW-Authenticate) are on the underlying map and
// go out then, so nothing but the status line changes.
type inlineErrorWriter struct {
	http.ResponseWriter
	wroteHeader bool
	status      int
	capturing   bool
	body        bytes.Buffer
}

func (w *inlineErrorWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = status
	if status >= 400 {
		w.capturing = true
		return
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *inlineErrorWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.capturing {
		return w.body.Write(b)
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the connection underneath, which
// is how the session refusal sets its read deadline.
func (w *inlineErrorWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// finish sends a captured failure as a 200 carrying the error with its status
// inside. A handler that wrote nothing (a poll whose client went away) leaves
// nothing to send.
func (w *inlineErrorWriter) finish() {
	if !w.capturing {
		return
	}
	var failure errorResponse
	if err := json.Unmarshal(w.body.Bytes(), &failure); err != nil || failure.Error.Code == "" {
		// Not the protocol shape: something below the handlers answered
		// (net/http itself, on a request it refused to route). Give it the
		// shape so the plugin can still branch on the status.
		failure = errorResponse{Error: errorDetail{
			Code:    codeForStatus(w.status),
			Message: strings.TrimSpace(w.body.String()),
		}}
		if failure.Error.Message == "" {
			failure.Error.Message = http.StatusText(w.status)
		}
	}
	failure.Error.Status = w.status
	writeJSON(w.ResponseWriter, http.StatusOK, failure)
}

// codeForStatus is the general code of spec section 2.2 for a status, used only
// when a refusal reached the wrapper without a protocol body.
func codeForStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return codeBadRequest
	case http.StatusUnauthorized:
		return codeUnauthorized
	case http.StatusForbidden:
		return codeForbidden
	case http.StatusNotFound:
		return codeNotFound
	case http.StatusMethodNotAllowed:
		return codeMethodNotAllowed
	case http.StatusConflict:
		return codeConflict
	case http.StatusRequestEntityTooLarge:
		return codePayloadTooLarge
	case http.StatusUnsupportedMediaType:
		return codeUnsupportedMediaType
	}
	if status >= 500 {
		return codeInternal
	}
	return codeBadRequest
}
