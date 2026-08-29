package hub_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/That1Drifter/vyshka/hub"
)

// poisonedBody fails the test the moment anything reads it, which is how a
// test proves a request's payload was never touched.
type poisonedBody struct{ t *testing.T }

func (p poisonedBody) Read([]byte) (int, error) {
	p.t.Error("the request body was read before the scope check refused the request")
	return 0, io.EOF
}

// refuseUnread sends one request carrying a poisoned body and asserts the
// scope refusal of spec section 10.2: 403 without touching the payload, and
// Connection: close so net/http does not park the goroutine draining the
// unread body for a keep-alive the refused caller does not deserve.
func refuseUnread(t *testing.T, server *hub.Server, method, path, bearer string) {
	t.Helper()

	request := httptest.NewRequest(method, path, poisonedBody{t})
	request.Header.Set("Authorization", "Bearer "+bearer)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("%s %s: status = %d, want 403", method, path, recorder.Code)
	}
	if got := recorder.Header().Get("Connection"); got != "close" {
		t.Errorf("%s %s: Connection header = %q on a scope refusal, want close", method, path, got)
	}
}

// The scope checks that need no body run before the body is read (spec
// section 10.2): a token refused there is answered 403 at the headers, on
// reads and mutations alike, instead of getting to occupy the connection
// trickling a payload nothing will use.
func TestScopeRefusalAnswersBeforeReadingTheBody(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	secret, minted := mintToken(t, server, "kv narrow", "kv:rw:allowed-ns")

	// The coarse gate: no grant on the route's resource at all.
	refuseUnread(t, server, http.MethodPost, "/api/v1/tokens", secret)
	// A read may legally frame a body too, and its drain held the connection
	// just the same until the refusal learned to close it.
	refuseUnread(t, server, http.MethodGet, "/api/v1/servers", secret)
	// The KV namespace lives in the path, so its exact check needs no body
	// either: a grant on the resource must not buy a body read on a
	// namespace outside it.
	refuseUnread(t, server, http.MethodPut, "/api/v1/kv/other-ns/key", secret)

	// The mutations are still audited (section 10.5), digest-less because
	// nothing was read to digest; the refused read is not, because reads
	// never are.
	page := queryAudit(t, server, url.Values{"tokenId": {minted.ID}})
	if len(page.Records) != 2 {
		t.Fatalf("the log holds %d records for this token, want the two refused mutations", len(page.Records))
	}
	for _, record := range page.Records {
		if record.Status != http.StatusForbidden {
			t.Errorf("audited %s %s status = %d, want 403", record.Method, record.Path, record.Status)
		}
		if record.PayloadDigest != "" {
			t.Errorf("a header-time refusal of %s carries payload digest %q, want empty: the body was never read",
				record.Path, record.PayloadDigest)
		}
	}
}

// A body-dependent refusal is different: the coarse gate passed, the exact
// check needed the body's value (the dispatch code), so the body was read and
// its digest belongs in the log.
func TestBodyLevelRefusalStillDigestsTheBody(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, _ := manifestFirst(t, server, "body-level refusal digest")
	secret, minted := mintToken(t, server, "heal only", "actions:dispatch:example-mod.heal")

	if got := errorCode(t, server, http.MethodPost, "/api/v1/servers/"+created.Server.ID+"/actions",
		secret, map[string]any{"code": "example-mod.wipe"}, http.StatusForbidden); got != "forbidden" {
		t.Fatalf("dispatch outside the grant: error code = %q, want forbidden", got)
	}

	page := queryAudit(t, server, url.Values{"tokenId": {minted.ID}})
	if len(page.Records) != 1 {
		t.Fatalf("the log holds %d records for this token, want the one refusal", len(page.Records))
	}
	if len(page.Records[0].PayloadDigest) != 64 {
		t.Errorf("a body-level refusal carries payload digest %q, want a full SHA-256: its body had to be read",
			page.Records[0].PayloadDigest)
	}
}

// rawExchange opens a raw connection, sends the headers and body it is given,
// pauses like a client that writes before it reads, then reads until the
// server hangs up. The elapsed time is the resource the request cost the hub:
// how long a connection and its goroutine were held, not just how fast the
// status line came back; and the answer proves what a real client, with no
// reader parked on the socket, actually received.
func rawExchange(t *testing.T, addr, request string, pause time.Duration) (time.Duration, string) {
	t.Helper()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer conn.Close()

	start := time.Now()
	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatalf("write request: %v", err)
	}
	time.Sleep(pause)
	_ = conn.SetReadDeadline(time.Now().Add(20 * time.Second))
	answered, _ := io.ReadAll(conn)
	return time.Since(start), string(answered)
}

// A refusal owes two things at once: the answer must actually reach an
// ordinary client, and a stalled body must not hold the goroutine past the
// drain bound. Both directions have failed here before. An immediate
// read-deadline hang-up freed the goroutine instantly but hard-closed the
// socket under the drain, and the RST dropped the queued 403 out of the
// client's receive buffer (golang.org/issue/3595) for any client that paused
// between writing and reading; a hang-up with no deadline at all delivered
// reliably but parked the goroutine on a stalled body until ReadTimeout.
func TestRefusalIsDeliveredAndBounded(t *testing.T) {
	t.Parallel()

	server, err := hub.New(context.Background(), hub.Config{
		DatabaseURL: filepath.Join(t.TempDir(), "refusal.db"),
		AdminToken:  testAdminToken,
		Logger:      slog.New(slog.NewJSONHandler(io.Discard, nil)),
		// Short, so the stalled-body cases prove the bound without the test
		// spending the production default.
		AdminBodyTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("boot hub: %v", err)
	}
	t.Cleanup(func() { server.Close() })
	secret, _ := mintToken(t, server, "release probe", "servers:read")

	ts := httptest.NewUnstartedServer(server.Handler())
	// Long enough that a regression to draining until ReadTimeout cannot be
	// mistaken for the AdminBodyTimeout bound this asserts.
	ts.Config.ReadTimeout = 15 * time.Second
	ts.Start()
	t.Cleanup(ts.Close)
	addr := ts.Listener.Addr().String()

	request := func(bearer, body string) string {
		head := "POST /api/v1/tokens HTTP/1.1\r\nHost: hub\r\n"
		if bearer != "" {
			head += "Authorization: Bearer " + bearer + "\r\n"
		}
		return head + fmt.Sprintf("Content-Type: application/json\r\nContent-Length: 1024\r\n\r\n%s", body)
	}
	fullBody := "{" + strings.Repeat(" ", 1022) + "}"

	// The correctness half: a prompt client that wrote its whole body and
	// paused before reading, exactly the shape the RST used to rob.
	_, answer := rawExchange(t, addr, request(secret, fullBody), 300*time.Millisecond)
	if !strings.Contains(answer, "403") {
		t.Fatalf("a prompt client's refusal was lost; got %q, want a 403", answer)
	}

	// The bound half: a stalled body is answered at the headers and released
	// at the drain bound, not held to ReadTimeout.
	held, answer := rawExchange(t, addr, request(secret, "{"), 0)
	if !strings.Contains(answer, "403") {
		t.Fatalf("scope refusal answered %q, want a 403", answer)
	}
	if held > 5*time.Second {
		t.Errorf("a refused mutation with a stalled body held the connection for %s; the drain bound is 2s", held)
	}

	// No credential at all is the cheapest attacker and gets the same bound.
	held, answer = rawExchange(t, addr, request("", "{"), 0)
	if !strings.Contains(answer, "401") {
		t.Fatalf("unauthenticated mutation answered %q, want a 401", answer)
	}
	if held > 5*time.Second {
		t.Errorf("an unauthenticated stalled body held the connection for %s; the drain bound is 2s", held)
	}
}

// ReadTimeout never needs sizing against the poll hold: net/http clears the
// connection's read deadline the moment the request body reaches EOF
// (startBackgroundRead), before the handler starts holding. A deadline BELOW
// the hold proves it, which a deadline above the hold could not.
func TestHeldPollIsImmuneToReadTimeout(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	_, live := enrolledSession(t, server, "read timeout hold")

	ts := httptest.NewUnstartedServer(server.Handler())
	ts.Config.ReadTimeout = 2 * time.Second // well under the 5 s hold
	ts.Start()
	t.Cleanup(ts.Close)

	request, err := http.NewRequest(http.MethodPost, ts.URL+"/plugin/v1/poll", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+live.SessionToken)
	request.Header.Set("Content-Type", "application/json")

	started := time.Now()
	response, err := (&http.Client{Timeout: 15 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("held poll: %v", err)
	}
	defer response.Body.Close()
	held := time.Since(started)

	if response.StatusCode != http.StatusOK {
		t.Fatalf("held poll: status = %d, want 200", response.StatusCode)
	}
	if held < 4*time.Second {
		t.Errorf("the idle poll answered after %s; a completed body must disarm ReadTimeout before the hold", held)
	}
}
