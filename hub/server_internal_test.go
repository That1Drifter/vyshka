package hub

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// discardLog is a logger for tests that only care about the traffic.
func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// okHandler answers "ok" to everything: the smallest handler a transport test
// can put behind the middleware under test.
var okHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	_, _ = w.Write([]byte("ok"))
})

// deadlineRecordingListener hands out connections that record every write
// deadline set on them, which is how a test observes ResponseWriteTimeout
// being armed without having to make a write actually fail.
type deadlineRecordingListener struct {
	net.Listener
	mu        sync.Mutex
	deadlines []time.Time
}

func (l *deadlineRecordingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return deadlineRecordingConn{Conn: conn, listener: l}, nil
}

// armed returns the non-zero write deadlines set so far. net/http clears the
// connection's write deadline (sets it to zero) after every response it
// finishes, and those clears land at their own pace; the arming this suite
// asserts on always precedes the response bytes, so filtering to non-zero
// keeps the assertions race-free.
func (l *deadlineRecordingListener) armed() []time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	var armed []time.Time
	for _, deadline := range l.deadlines {
		if !deadline.IsZero() {
			armed = append(armed, deadline)
		}
	}
	return armed
}

type deadlineRecordingConn struct {
	net.Conn
	listener *deadlineRecordingListener
}

func (c deadlineRecordingConn) SetWriteDeadline(deadline time.Time) error {
	c.listener.mu.Lock()
	c.listener.deadlines = append(c.listener.deadlines, deadline)
	c.listener.mu.Unlock()
	return c.Conn.SetWriteDeadline(deadline)
}

// serveOn runs an http.Server over the listener for the duration of the test.
func serveOn(t *testing.T, server *http.Server, listener net.Listener) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = server.Serve(listener)
	}()
	t.Cleanup(func() {
		_ = server.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Serve did not return within 5s of Close")
		}
	})
}

// readUntil reads from the connection until the accumulated bytes contain
// want, returning what was read; an empty want reads until the deadline or
// EOF.
func readUntil(t *testing.T, conn net.Conn, want string, deadline time.Duration) string {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(deadline))
	var got strings.Builder
	buf := make([]byte, 4096)
	for want == "" || !strings.Contains(got.String(), want) {
		n, err := conn.Read(buf)
		got.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return got.String()
}

// ResponseWriteTimeout is armed once per response, as the response begins:
// each request on a keep-alive connection gets its own fresh deadline, and it
// reaches the connection through the middleware's recorder.
func TestResponseWriteTimeoutArmsPerResponse(t *testing.T) {
	t.Parallel()

	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := &deadlineRecordingListener{Listener: inner}
	const timeout = 10 * time.Second
	// WriteTimeout deliberately unset: every recorded write deadline below
	// was therefore armed by the middleware, not by net/http.
	serveOn(t, &http.Server{Handler: logRequests(discardLog(), timeout, okHandler)}, listener)

	conn, err := net.Dial("tcp", inner.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	request := "GET / HTTP/1.1\r\nHost: hub\r\n\r\n"
	for exchange := 1; exchange <= 2; exchange++ {
		before := time.Now()
		if _, err := conn.Write([]byte(request)); err != nil {
			t.Fatalf("exchange %d: write: %v", exchange, err)
		}
		if answer := readUntil(t, conn, "\r\n\r\nok", 5*time.Second); !strings.Contains(answer, "200") {
			t.Fatalf("exchange %d: answer = %q, want a 200", exchange, answer)
		}
		armed := listener.armed()
		if len(armed) != exchange {
			t.Fatalf("after exchange %d: %d write deadlines armed, want one per response", exchange, len(armed))
		}
		deadline := armed[exchange-1]
		if deadline.Before(before.Add(timeout)) || deadline.After(time.Now().Add(timeout)) {
			t.Errorf("exchange %d: deadline armed for %s, want about %s after the response began",
				exchange, deadline, timeout)
		}
	}
}

// A response's tight deadline must die with its response instead of lingering
// on the keep-alive connection and, once in the past, eating whatever the
// server writes next. The arming leans on net/http clearing the connection's
// write deadline after every response it finishes; this pins that behavior
// with WriteTimeout deliberately unset, so nothing else could clear it, and a
// second exchange that begins only after the first one's deadline has passed.
// The second request is malformed on purpose: its 400 is written by net/http
// outside any handler, so no re-arming can hide a lingering deadline.
func TestStaleResponseDeadlineDoesNotOutliveItsResponse(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveOn(t, &http.Server{
		Handler: logRequests(discardLog(), 250*time.Millisecond, okHandler),
	}, listener)

	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: hub\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	if answer := readUntil(t, conn, "\r\n\r\nok", 5*time.Second); !strings.Contains(answer, "200") {
		t.Fatalf("first exchange answered %q, want a 200", answer)
	}

	// Let the first response's 250 ms deadline pass into the past while the
	// connection idles, then provoke a response only net/http writes.
	time.Sleep(500 * time.Millisecond)
	if _, err := conn.Write([]byte("BOGUS\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	if answer := readUntil(t, conn, "400", 5*time.Second); !strings.Contains(answer, "400") {
		t.Fatalf("after an idle past the first response's deadline, the connection answered %q, want net/http's 400", answer)
	}
}

// At the cap, a further connection is not served until an accepted one
// closes; when one does, the waiter is picked up.
func TestConnectionCapHoldsExcessConnections(t *testing.T) {
	t.Parallel()

	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveOn(t, &http.Server{Handler: okHandler}, capConnections(inner, 1))

	request := "GET / HTTP/1.1\r\nHost: hub\r\n\r\n"
	first, err := net.Dial("tcp", inner.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := first.Write([]byte(request)); err != nil {
		t.Fatal(err)
	}
	if answer := readUntil(t, first, "\r\n\r\nok", 5*time.Second); !strings.Contains(answer, "200") {
		t.Fatalf("the connection holding the only slot answered %q, want a 200", answer)
	}

	// The kernel completes the handshake from its backlog, so the dial and
	// the write succeed; being answered is what the cap withholds.
	second, err := net.Dial("tcp", inner.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if _, err := second.Write([]byte(request)); err != nil {
		t.Fatal(err)
	}
	if answer := readUntil(t, second, "", 400*time.Millisecond); answer != "" {
		t.Fatalf("a connection past the cap was answered %q while the slot was held", answer)
	}

	// Closing the slot-holder must free its slot and let the waiter in.
	first.Close()
	if answer := readUntil(t, second, "\r\n\r\nok", 10*time.Second); !strings.Contains(answer, "200") {
		t.Fatalf("after the slot freed, the waiting connection answered %q, want a 200", answer)
	}
}

// Closing the listener while every slot is taken must unblock the Accept
// parked on the full house, or shutdown would hang behind the connections it
// is draining. serveOn's cleanup is the assertion: it fails the test if Serve
// has not returned within its bound.
func TestConnectionCapUnblocksAcceptOnClose(t *testing.T) {
	t.Parallel()

	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveOn(t, &http.Server{Handler: okHandler}, capConnections(inner, 1))

	conn, err := net.Dial("tcp", inner.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: hub\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	if answer := readUntil(t, conn, "\r\n\r\nok", 5*time.Second); !strings.Contains(answer, "200") {
		t.Fatalf("the slot-holding connection answered %q, want a 200", answer)
	}
	// The connection idles holding the only slot; the serve loop's next
	// Accept is now parked acquiring one. Cleanup closes the server under it.
}
