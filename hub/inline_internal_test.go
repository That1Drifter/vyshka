package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The wrapper is exercised end to end in inline_test.go; this checks the one
// thing a recorder cannot show from outside: a handler that measured its
// ordinary error body must not leave that length on the rewritten one, or the
// real response writer would cut the inline body short.
func TestInlineErrorWriterDropsAStaleContentLength(t *testing.T) {
	t.Parallel()
	recorder := httptest.NewRecorder()
	writer := &inlineErrorWriter{ResponseWriter: recorder}

	body := `{"error":{"code":"bad_request","message":"no"}}`
	writer.Header().Set("Content-Length", "47")
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusBadRequest)
	_, _ = writer.Write([]byte(body))
	writer.finish()

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if length := recorder.Header().Get("Content-Length"); length != "" {
		t.Errorf("Content-Length = %q survived the rewrite; the body is now longer than the handler measured", length)
	}
	var failure errorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &failure); err != nil {
		t.Fatalf("rewritten body %q: %v", recorder.Body.String(), err)
	}
	if failure.Error.Status != http.StatusBadRequest || failure.Error.Code != codeBadRequest {
		t.Errorf("rewritten error = %+v, want bad_request with status 400", failure.Error)
	}
}

// Details travel unchanged: a seq above 2^53 must not be rounded through a
// float on its way into the rewritten body.
func TestInlineErrorWriterKeepsLargeNumbersInDetails(t *testing.T) {
	t.Parallel()
	recorder := httptest.NewRecorder()
	writer := &inlineErrorWriter{ResponseWriter: recorder}
	const seq = "9007199254740993"
	writer.WriteHeader(http.StatusBadRequest)
	_, _ = writer.Write([]byte(`{"error":{"code":"envelope_invalid","message":"no","details":{"index":0,"seq":` + seq + `}}}`))
	writer.finish()

	if body := recorder.Body.String(); !strings.Contains(body, `"seq": `+seq) {
		t.Errorf("details.seq was not preserved exactly: %s", body)
	}
}

// A refusal that reached the wrapper without a protocol body (net/http's own
// plain-text answers) still comes out in the protocol shape.
func TestInlineErrorWriterShapesAPlainTextRefusal(t *testing.T) {
	t.Parallel()
	recorder := httptest.NewRecorder()
	writer := &inlineErrorWriter{ResponseWriter: recorder}
	http.Error(writer, "431 Request Header Fields Too Large", http.StatusRequestHeaderFieldsTooLarge)
	writer.finish()

	var failure errorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &failure); err != nil {
		t.Fatalf("rewritten body %q: %v", recorder.Body.String(), err)
	}
	if failure.Error.Status != http.StatusRequestHeaderFieldsTooLarge || failure.Error.Code != codeBadRequest {
		t.Errorf("rewritten error = %+v", failure.Error)
	}
}
