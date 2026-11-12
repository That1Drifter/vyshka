// Command stub is the HTTP counterpart of the Reforger probe addon: each endpoint
// answers in one shape the probe needs to measure, and every request is logged as
// one tab-separated line with what the server script actually put on the wire.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8099", "listen address")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/echo", echo)
	mux.HandleFunc("/hold", hold)
	mux.HandleFunc("/drip", drip)
	mux.HandleFunc("/status", status)
	mux.HandleFunc("/big", big)
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) {
		logReq(r, 0, "redirect")
		http.Redirect(w, r, "/echo?redirected=1", http.StatusFound)
	})
	mux.HandleFunc("/nocontent", func(w http.ResponseWriter, r *http.Request) {
		logReq(r, 0, "204")
		w.WriteHeader(http.StatusNoContent)
	})

	log.SetFlags(0)
	log.Printf("STUB\tlisten\t%s", *addr)
	srv := &http.Server{Addr: *addr, Handler: stripContext(mux), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

// stripContext drops a leading "/c<digits>" path segment. The engine keeps one REST
// context per base URL, and SetHeaders and SetTimeout belong to the context, so the
// probe gives each test group its own base (http://127.0.0.1:8099/c3/) to keep one
// group's settings from leaking into the next.
func stripContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if len(p) > 2 && p[1] == 'c' {
			if i := strings.IndexByte(p[2:], '/'); i > 0 {
				if _, err := strconv.Atoi(p[2 : 2+i]); err == nil {
					r.URL.Path = p[2+i:]
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func ts() string { return time.Now().UTC().Format("15:04:05.000") }

func headerLine(r *http.Request) string {
	keys := make([]string, 0, len(r.Header))
	for k := range r.Header {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+strings.Join(r.Header[k], "|"))
	}
	return strings.Join(parts, "; ")
}

func logReq(r *http.Request, bodyLen int, note string) {
	// Go moves Content-Length and Transfer-Encoding out of r.Header, so they are logged
	// apart (from run 5 on): a chunked request shows te=chunked and cl=-1.
	log.Printf("STUB\t%s\t%s\t%s\tbody=%d\t%s\tproto=%s cl=%d te=%s\theaders: %s", ts(), r.Method, r.URL.RequestURI(), bodyLen, note, r.Proto, r.ContentLength, strings.Join(r.TransferEncoding, ","), headerLine(r))
}

// echo answers with what arrived: method, query, every header, the body's length and
// digest, and its first 64 bytes.
func echo(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	note := "ok"
	if err != nil {
		note = "read-error: " + err.Error()
	}
	sum := sha256.Sum256(body)
	logReq(r, len(body), note+" sha256="+hex.EncodeToString(sum[:8]))
	head := body
	if len(head) > 64 {
		head = head[:64]
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Probe", "echo")
	json.NewEncoder(w).Encode(map[string]any{
		"method":  r.Method,
		"query":   r.URL.RawQuery,
		"headers": r.Header,
		"bodyLen": len(body),
		"sha256":  hex.EncodeToString(sum[:8]),
		"head":    string(head),
	})
}

// hold sends nothing until s seconds have passed, then a small JSON body, and logs
// whether the client gave up first.
func hold(w http.ResponseWriter, r *http.Request) {
	s, _ := strconv.ParseFloat(r.URL.Query().Get("s"), 64)
	start := time.Now()
	logReq(r, 0, fmt.Sprintf("hold-begin s=%g", s))
	select {
	case <-time.After(time.Duration(s * float64(time.Second))):
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"held":%g}`, s)
		log.Printf("STUB\t%s\thold-answered\ts=%g\tafter=%.3fs", ts(), s, time.Since(start).Seconds())
	case <-r.Context().Done():
		log.Printf("STUB\t%s\thold-client-gone\ts=%g\tafter=%.3fs", ts(), s, time.Since(start).Seconds())
	}
}

// drip sends the status line and headers at once, then one byte per second for s
// seconds, which separates a whole-response limit from an idle limit.
func drip(w http.ResponseWriter, r *http.Request) {
	s, _ := strconv.Atoi(r.URL.Query().Get("s"))
	every, _ := strconv.Atoi(r.URL.Query().Get("every"))
	if every < 1 {
		every = 1
	}
	start := time.Now()
	logReq(r, 0, fmt.Sprintf("drip-begin s=%d every=%d", s, every))
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	for i := 0; i < s; i += every {
		if _, err := w.Write([]byte("x")); err != nil {
			log.Printf("STUB\t%s\tdrip-client-gone\tafter=%.3fs", ts(), time.Since(start).Seconds())
			return
		}
		if fl != nil {
			fl.Flush()
		}
		select {
		case <-time.After(time.Duration(every) * time.Second):
		case <-r.Context().Done():
			log.Printf("STUB\t%s\tdrip-client-gone\tafter=%.3fs", ts(), time.Since(start).Seconds())
			return
		}
	}
	log.Printf("STUB\t%s\tdrip-done\ts=%d\tafter=%.3fs", ts(), s, time.Since(start).Seconds())
}

// status answers with the given code, a JSON error body, and a custom header.
func status(w http.ResponseWriter, r *http.Request) {
	code, _ := strconv.Atoi(r.URL.Query().Get("code"))
	logReq(r, 0, fmt.Sprintf("status=%d", code))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Probe", "status")
	w.WriteHeader(code)
	fmt.Fprintf(w, `{"error":{"code":"probe_%d","message":"body of a %d"}}`, code, code)
}

// big answers with a JSON document of exactly n bytes: {"d":"aaa...a"}, with a
// Content-Length unless chunked=1, which streams it in 64 KiB pieces the way a hub's
// JSON encoder does, and logs whether the whole body was written.
func big(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	if n < 8 {
		n = 8
	}
	chunked := r.URL.Query().Get("chunked") == "1"
	logReq(r, 0, fmt.Sprintf("big n=%d chunked=%t", n, chunked))
	w.Header().Set("Content-Type", "application/json")
	if !chunked {
		w.Header().Set("Content-Length", strconv.Itoa(n))
	}
	fl, _ := w.(http.Flusher)
	written, err := io.WriteString(w, `{"d":"`)
	for left := n - 8; left > 0 && err == nil; {
		k := min(left, 65536)
		var m int
		m, err = io.WriteString(w, strings.Repeat("a", k))
		written += m
		left -= k
		if chunked && fl != nil {
			fl.Flush()
		}
	}
	if err == nil {
		var m int
		m, err = io.WriteString(w, `"}`)
		written += m
	}
	log.Printf("STUB\t%s\tbig-written\tn=%d\twritten=%d\terr=%v", ts(), n, written, err)
}
