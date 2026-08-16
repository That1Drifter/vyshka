// Stub HTTP server for the DayZ RestApi long-poll timeout spike.
//
// It answers a handful of endpoints that hold a response open in different
// ways, and logs one line per request phase so the server side view can be
// compared against what the game script observed:
//
//	/hold?ms=N            headers + body written after N ms of silence
//	/headers?ms=N         headers written immediately, body after N ms
//	/drip?ms=N&every=M    headers immediately, one chunk every M ms for N ms
//	/now                  immediate response (connectivity check)
//
// Every request logs arrival, each write, and how it ended: either the
// handler completed, or the client went away first (context cancelled),
// which is how a client side timeout shows up here.
//
// Run: go run main.go -addr 127.0.0.1:8099
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync/atomic"
	"time"
)

var reqNo atomic.Int64

func qint(r *http.Request, key string, def int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// logf prefixes every line with a wall clock stamp and the request number so
// script side and server side timelines can be lined up.
func logf(id int64, format string, args ...any) {
	log.Printf("[req %03d] %s", id, fmt.Sprintf(format, args...))
}

func handle(w http.ResponseWriter, r *http.Request) {
	id := reqNo.Add(1)
	start := time.Now()
	logf(id, ">> arrive %s %s from %s ua=%q len=%d",
		r.Method, r.URL.RequestURI(), r.RemoteAddr, r.UserAgent(), r.ContentLength)

	flusher, canFlush := w.(http.Flusher)
	done := r.Context().Done()

	// sleep returns false if the client disconnected while waiting.
	sleep := func(d time.Duration) bool {
		select {
		case <-time.After(d):
			return true
		case <-done:
			logf(id, "!! client gone after %.3fs (waiting %s)", time.Since(start).Seconds(), d)
			return false
		}
	}

	switch r.URL.Path {
	case "/now":
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"endpoint":"now","elapsedMs":0}`)

	case "/hold":
		// Nothing at all goes out until the delay has passed: this is what a
		// hub holding a long-poll open looks like.
		ms := qint(r, "ms", 5000)
		if !sleep(time.Duration(ms) * time.Millisecond) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"endpoint":"hold","heldMs":%d,"elapsedMs":%d}`, ms, time.Since(start).Milliseconds())
		logf(id, "-> body written after %.3fs", time.Since(start).Seconds())

	case "/headers":
		// Headers immediately, body after the delay. Distinguishes a timeout
		// on "no response at all" from one on "no complete response".
		ms := qint(r, "ms", 5000)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if canFlush {
			flusher.Flush()
		}
		logf(id, "-> headers flushed at %.3fs", time.Since(start).Seconds())
		if !sleep(time.Duration(ms) * time.Millisecond) {
			return
		}
		fmt.Fprintf(w, `{"endpoint":"headers","heldMs":%d,"elapsedMs":%d}`, ms, time.Since(start).Milliseconds())
		logf(id, "-> body written after %.3fs", time.Since(start).Seconds())

	case "/drip":
		// Periodic chunks for the whole window: tells us whether the client
		// timeout is an idle timer (kept alive by traffic) or a wall clock
		// budget for the entire response.
		total := qint(r, "ms", 30000)
		every := qint(r, "every", 4000)
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		if canFlush {
			flusher.Flush()
		}
		logf(id, "-> headers flushed at %.3fs", time.Since(start).Seconds())
		deadline := start.Add(time.Duration(total) * time.Millisecond)
		for time.Now().Before(deadline) {
			if !sleep(time.Duration(every) * time.Millisecond) {
				return
			}
			fmt.Fprintf(w, "keepalive %d\n", time.Since(start).Milliseconds())
			if canFlush {
				flusher.Flush()
			}
			logf(id, "-> chunk at %.3fs", time.Since(start).Seconds())
		}
		fmt.Fprintf(w, "done %d\n", time.Since(start).Milliseconds())

	default:
		http.NotFound(w, r)
		logf(id, "-- 404")
		return
	}

	logf(id, "== complete after %.3fs", time.Since(start).Seconds())
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8099", "listen address")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.SetOutput(os.Stdout)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           http.HandlerFunc(handle),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: the whole point is holding responses open.
	}
	log.Printf("stub listening on %s", *addr)
	log.Fatal(srv.ListenAndServe())
}
