// Stub HTTP server for the DayZ RestApi request-shape spike.
//
// It logs every request in full (method, URI, every header, body length and a
// body prefix) so the script side's attempts to shape a request can be
// compared with what actually reached the socket. Endpoints:
//
//	/post              200 with a small JSON body
//	/echo              200 echoing the request body back verbatim
//	/big?n=N           200 with a JSON body padded to N bytes
//	/status?code=N     status N with a protocol-shaped JSON error body
//	/utf8              200 with a JSON body carrying non-ASCII text
//
// Run: go run main.go -addr 127.0.0.1:8098
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
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

func logf(id int64, format string, args ...any) {
	log.Printf("[req %03d] %s", id, fmt.Sprintf(format, args...))
}

func handle(w http.ResponseWriter, r *http.Request) {
	id := reqNo.Add(1)
	logf(id, ">> %s %s proto=%s from %s", r.Method, r.URL.RequestURI(), r.Proto, r.RemoteAddr)

	names := make([]string, 0, len(r.Header))
	for name := range r.Header {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, value := range r.Header[name] {
			logf(id, "   header %s: %q", name, value)
		}
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		logf(id, "!! body read error: %v", err)
	}
	prefix := string(body)
	if len(prefix) > 160 {
		prefix = prefix[:160] + "..."
	}
	logf(id, "   body len=%d prefix=%q", len(body), prefix)

	switch r.URL.Path {
	case "/post":
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true,"bodyLen":%d}`, len(body))

	case "/echo":
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)

	case "/big":
		n := qint(r, "n", 1000)
		head := `{"pad":"`
		tail := `"}`
		padLen := n - len(head) - len(tail)
		if padLen < 0 {
			padLen = 0
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, head)
		_, _ = io.WriteString(w, strings.Repeat("x", padLen))
		_, _ = io.WriteString(w, tail)

	case "/status":
		code := qint(r, "code", 400)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		fmt.Fprintf(w, `{"error":{"code":"probe_status","message":"status %d"}}`, code)

	case "/utf8":
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":"žluťoučký kůň"}`)

	default:
		http.NotFound(w, r)
		logf(id, "-- 404")
		return
	}
	logf(id, "== answered")
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8098", "listen address")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.SetOutput(os.Stdout)
	log.Printf("stub listening on %s", *addr)
	if err := http.ListenAndServe(*addr, http.HandlerFunc(handle)); err != nil {
		log.Fatal(err)
	}
}
