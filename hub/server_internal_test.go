package hub

import (
	"bufio"
	"io"
	"log/slog"
	"math"
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

// ResponseWriteTimeout is a progress bound: armed at each response's first
// header, re-armed at each body write with an allowance for that write's
// size, and each request on a keep-alive connection gets its own fresh
// deadlines, reaching the connection through the middleware's recorder.
func TestResponseWriteTimeoutArmsPerResponse(t *testing.T) {
	t.Parallel()

	const bigSize = 1 << 20
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/big" {
			_, _ = w.Write(make([]byte, bigSize))
			return
		}
		_, _ = w.Write([]byte("ok"))
	})

	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := &deadlineRecordingListener{Listener: inner}
	const timeout = 10 * time.Second
	// WriteTimeout deliberately unset: every recorded write deadline below
	// was therefore armed by the middleware, not by net/http.
	serveOn(t, &http.Server{Handler: logRequests(discardLog(), timeout, handler)}, listener)

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
		// Two per response: the header's arm and the tiny body write's.
		if len(armed) != 2*exchange {
			t.Fatalf("after exchange %d: %d write deadlines armed, want two per response", exchange, len(armed))
		}
		for _, deadline := range armed[2*(exchange-1):] {
			if deadline.Before(before.Add(timeout)) || deadline.After(time.Now().Add(timeout+time.Second)) {
				t.Errorf("exchange %d: deadline armed for %s, want about %s after the response began",
					exchange, deadline, timeout)
			}
		}
	}

	// A large body is written in chunks, each earning its own size allowance
	// on top of the base bound: a legal big page over a slow link is never
	// cut for being big, while no single deadline ever covers more than one
	// chunk, so a stalled reader cannot ride a whole response's allowance.
	before := time.Now()
	if _, err := conn.Write([]byte("GET /big HTTP/1.1\r\nHost: hub\r\nConnection: close\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	// Parsed rather than counted raw: the big response arrives chunked, and
	// a raw byte count would let framing bytes mask a short body.
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read big response: %v", err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || len(body) != bigSize {
		t.Fatalf("big response delivered a %d-byte body (err %v), want exactly %d", len(body), err, bigSize)
	}
	armed := listener.armed()
	// The four arms so far, plus the header's and one per 256 KiB chunk.
	chunks := bigSize / responseWriteChunk
	if len(armed) != 4+1+chunks {
		t.Fatalf("after the big response: %d write deadlines armed, want %d", len(armed), 4+1+chunks)
	}
	allowance := time.Duration(responseWriteChunk) * time.Second / responseByteRateFloor
	for _, deadline := range armed[5:] {
		if deadline.Before(before.Add(timeout+allowance-time.Second)) ||
			deadline.After(time.Now().Add(timeout+allowance+time.Second)) {
			t.Errorf("chunk deadline armed for %s, want one chunk's allowance (%s) past the base bound, never more",
				deadline, allowance)
		}
	}
}

// The WriteTimeout backstop is derived from the effective read bound, because
// net/http arms it at request start: it must clear a body legally read until
// ReadTimeout plus a full poll hold, whatever ReadTimeout was raised to, and
// it follows a disabled ReadTimeout off since no finite bound armed at
// request start is safe against a hold that begins arbitrarily late.
func TestWriteTimeoutDefaultFollowsReadTimeout(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		read, write time.Duration
		want        time.Duration
	}{
		{"defaults", 0, 0, 120 * time.Second},
		{"raised read", 90 * time.Second, 0, 180 * time.Second},
		{"disabled read disables the backstop", -1, 0, -1},
		{"explicit write wins", 90 * time.Second, 45 * time.Second, 45 * time.Second},
		{"explicit disable wins", 0, -1, -1},
		{"ceiling read pins instead of wrapping", math.MaxInt64 - time.Second, 0, math.MaxInt64},
	}
	for _, tc := range cases {
		cfg := Config{ReadTimeout: tc.read, WriteTimeout: tc.write}
		cfg.withDefaults()
		if cfg.WriteTimeout != tc.want {
			t.Errorf("%s: WriteTimeout = %v, want %v", tc.name, cfg.WriteTimeout, tc.want)
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
// is draining. The slot-holding connection stays open until after the
// assertion, so the release path cannot unpark the Accept and mask a broken
// close path.
func TestConnectionCapUnblocksAcceptOnClose(t *testing.T) {
	t.Parallel()

	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := capConnections(inner, 1)
	server := &http.Server{Handler: okHandler}
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = server.Serve(listener)
	}()
	t.Cleanup(func() { _ = server.Close() })

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

	// The connection idles on holding the only slot, so wherever the serve
	// loop's next Accept is, the semaphore cannot let it through; closing the
	// listener is the only thing that can end it.
	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return within 5s of closing the listener under a full cap")
	}
}

// net/http half-closes a connection it is about to close with body bytes
// still unread (closeWriteAndWait), so the queued refusal is not lost to an
// RST; the cap wrapper must not strip that ability off the TCP connection.
// Driven over a real TCP pair: after CloseWrite the peer sees EOF while the
// wrapped side can still read.
func TestCapConnForwardsCloseWrite(t *testing.T) {
	t.Parallel()

	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer inner.Close()
	client, err := net.Dial("tcp", inner.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	accepted, err := inner.Accept()
	if err != nil {
		t.Fatal(err)
	}
	wrapped := &capConn{Conn: accepted, release: func() {}}
	defer wrapped.Close()

	if err := wrapped.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite through the wrapper: %v", err)
	}
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err := client.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("after the half-close the peer read (%d, %v), want EOF", n, err)
	}
	if _, err := client.Write([]byte("x")); err != nil {
		t.Fatalf("write toward the half-closed side: %v", err)
	}
	_ = wrapped.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 1)
	if _, err := wrapped.Read(buf); err != nil || buf[0] != 'x' {
		t.Fatalf("read on the half-closed side: %q, %v; the read direction must survive CloseWrite", buf, err)
	}
}
