// Command driver is the reference candidate for the plugin conformance
// harness: a minimal but correct autonomous plugin. It enrolls, keeps a
// session, long-polls, publishes a manifest, enumerates the one custom
// context that manifest declares, executes dispatched actions with an
// executed-actionId LRU, buffers unacked envelopes across outages, renumbers
// them across session changes, says more when a poll leaves some of them
// behind, and keeps the installation ban list (it has
// no players to refuse, so keeping it is walking it whole and reporting what
// it applied). CI runs the harness against it to prove the suite goes green
// against a compliant implementation.
//
// Like everything under conformance/, it speaks only HTTP and imports no hub
// or plugin code.
//
// Configuration comes from the harness: VYSHKA_HUB_URL and
// VYSHKA_ENROLLMENT_TOKEN in the environment (or -url and -token flags). The
// driver exits when its stdin reaches EOF, which is how the harness shuts it
// down, or after 30 s of continuous transport failure, so it never outlives a
// harness that died.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type envelope struct {
	V    int             `json:"v,omitempty"`
	ID   string          `json:"id"`
	Type string          `json:"type"`
	Seq  int64           `json:"seq"`
	TS   string          `json:"ts"`
	Body json.RawMessage `json:"body"`
}

type pollResponse struct {
	Envelopes          []envelope `json:"envelopes"`
	Ack                int64      `json:"ack"`
	PollTimeoutSeconds int        `json:"pollTimeoutSeconds"`
}

const executedLRUCap = 128

// driverContext is the one custom context this driver declares (spec section
// 6.2). It has two members, so a hub asking for them gets an answer that is
// clearly this context's rather than a canned empty list.
const driverContext = "driver.zone"

// hubError is a refusal as the driver understands it, whichever way it
// arrived: from the status and body of a 4xx or 5xx, from the body of a 200
// that carried error.status (spec section 2.3), or, in opaque mode, from the
// status class alone with no code at all.
type hubError struct {
	Code    string
	Status  int
	Message string
	Index   int
	HasIdx  bool
}

type driver struct {
	baseURL string
	client  *http.Client

	// inline asks for inline errors on every request; opaque discards the
	// status and body of every non-2xx response, keeping only whether it was
	// a client or a server error, which is what an engine like DayZ's leaves
	// a plugin with. Together they let CI grade both recovery paths.
	inline bool
	opaque bool

	serverID     string
	serverSecret string
	sessionToken string

	// polledThisSession and unpolledRefusals drive the opaque fallback: a
	// refused poll on a session that has polled successfully means the
	// session is gone; a refused poll on a session that never polled is more
	// likely the driver's own batch, so a new session is not the answer twice
	// in a row.
	polledThisSession bool
	unpolledRefusals  int
	sessionBackoff    time.Duration

	// batchLimit caps how much of the buffer one poll carries. It starts at
	// the 200 every hub must accept (section 3.1.2), halves on a poll
	// bad_request (section 2.3), and is restored by a new session.
	batchLimit int

	// inAck is the highest contiguous hub -> plugin seq processed; outSeq the
	// last seq assigned to an envelope of the driver's own. buffer holds every
	// outbound envelope the hub has not acked, in seq order.
	inAck  int64
	outSeq int64
	buffer []envelope

	// The executed-actionId LRU of spec section 9.2.
	executed      map[string]bool
	executedOrder []string

	idCounter     int64
	manifestSent  bool
	telemetrySent bool
	firstFailure  time.Time

	// The installation ban list (spec section 13.4). bansEnforced is the
	// revision of the list the driver holds, -1 before it holds any;
	// bansKnown the latest revision the hub has told it of; bansDue whether a
	// walk is owed, retried no sooner than bansNextTry after a failed one.
	// A real plugin keeps the list on disk; the driver never restarts, so it
	// keeps it in memory.
	bansEnforced int64
	bansKnown    int64
	bansDue      bool
	bansNextTry  time.Time
	bans         []banEntry

	// The walk in progress, one page per turn of the run loop: the revision
	// its first page was served at (-1 before it), the cursor of the next
	// page, what it has read, and whether the hub reported a revision while
	// it ran.
	walking        bool
	walkRevision   int64
	walkCursor     string
	walkPages      int
	walkRestarts   int
	walked         []banEntry
	toldDuringWalk bool
	// noticeUnacked says a bans.changed was taken and the poll carrying its
	// ack has not gone out yet; a walk page waits for that poll. pageWaited
	// says the next page has already waited one turn.
	noticeUnacked bool
	pageWaited    bool
}

// banEntry is one entry of the installation ban list as the driver keeps it.
type banEntry struct {
	ID     string `json:"id"`
	Player struct {
		Platform string `json:"platform"`
		ID       string `json:"id"`
	} `json:"player"`
	Reason    string  `json:"reason"`
	ExpiresAt *string `json:"expiresAt"`
}

func main() {
	url := flag.String("url", os.Getenv("VYSHKA_HUB_URL"), "hub base URL (env VYSHKA_HUB_URL)")
	token := flag.String("token", os.Getenv("VYSHKA_ENROLLMENT_TOKEN"), "one-time enrollment token (env VYSHKA_ENROLLMENT_TOKEN)")
	game := flag.String("game", "conformance", "game id to enroll as")
	inline := flag.Bool("inline", true, "ask for inline errors (?errors=inline) on every request")
	opaque := flag.Bool("opaque", false, "discard the status and body of every non-2xx response, keeping only its class, like a constrained engine's HTTP client")
	flag.Parse()

	log.SetOutput(os.Stderr)
	log.SetPrefix("driver: ")
	log.SetFlags(0)

	if *url == "" || *token == "" {
		log.Println("need -url and -token (or VYSHKA_HUB_URL and VYSHKA_ENROLLMENT_TOKEN)")
		os.Exit(2)
	}

	// The harness closes the driver's stdin when the run is over.
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		log.Println("stdin closed; exiting")
		os.Exit(0)
	}()

	d := &driver{
		baseURL:      *url,
		client:       &http.Client{Timeout: 10 * time.Second},
		executed:     map[string]bool{},
		inline:       *inline,
		opaque:       *opaque,
		bansEnforced: -1,
		bansKnown:    -1,
	}
	if err := d.enroll(*token, *game); err != nil {
		log.Println("enroll:", err)
		os.Exit(2)
	}
	d.run(*game)
}

// post sends one request. The status and body come back as the transport
// delivered them; classify turns them into a success or a hubError.
func (d *driver) post(path, bearer string, body any) (int, []byte, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	return d.do(http.MethodPost, path, bearer, encoded)
}

// get sends one bodyless GET, the spelling a client with full HTTP uses for
// the ban list read (spec section 13.3). query is appended as it is.
func (d *driver) get(path, query, bearer string) (int, []byte, error) {
	if query != "" {
		path += "?" + query
	}
	return d.do(http.MethodGet, path, bearer, nil)
}

// do sends one request with the driver's error mode and credential.
func (d *driver) do(method, path, bearer string, encoded []byte) (int, []byte, error) {
	if d.inline {
		if strings.Contains(path, "?") {
			path += "&errors=inline"
		} else {
			path += "?errors=inline"
		}
	}
	var reader io.Reader = http.NoBody
	if encoded != nil {
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, d.baseURL+path, reader)
	if err != nil {
		return 0, nil, err
	}
	if encoded != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	response, err := d.client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return 0, nil, err
	}
	return response.StatusCode, responseBody, nil
}

// classify decides what a response was. A 2xx whose body is a JSON object
// without a top-level error member is a success. A 2xx carrying an error
// member is an inline refusal. A non-2xx is a refusal read from its body, or,
// in opaque mode, from its class alone. A 2xx whose body is not a JSON
// object is reported as a refusal with no code and status 0: malformed, to
// be retried without touching any state.
func (d *driver) classify(status int, body []byte) (ok bool, failure *hubError) {
	if status >= 200 && status < 300 {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(body, &raw); err != nil || raw == nil {
			snippet := string(body)
			if len(snippet) > 120 {
				snippet = snippet[:120] + "..."
			}
			return false, &hubError{Message: fmt.Sprintf("response body is not a JSON object: %q", snippet)}
		}
		errorRaw, present := raw["error"]
		if !present {
			return true, nil
		}
		failure = decodeHubError(errorRaw)
		if failure.Code == "" {
			// Present but unusable: malformed, not a refusal to act on
			// (section 2.3), so nothing changes and the request is retried.
			return false, &hubError{Message: "the error member is not an object with a code"}
		}
		if failure.Status == 0 {
			failure.Status = http.StatusInternalServerError
		}
		return false, failure
	}
	if d.opaque {
		class := http.StatusBadRequest
		if status >= 500 {
			class = http.StatusInternalServerError
		}
		return false, &hubError{Status: class, Message: fmt.Sprintf("opaque %dxx", status/100)}
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err == nil {
		if errorRaw, present := raw["error"]; present {
			failure = decodeHubError(errorRaw)
			failure.Status = status
			return false, failure
		}
	}
	return false, &hubError{Status: status, Message: string(body)}
}

func decodeHubError(raw json.RawMessage) *hubError {
	var wire struct {
		Code    string `json:"code"`
		Status  int    `json:"status"`
		Message string `json:"message"`
		Details struct {
			Index json.RawMessage `json:"index"`
		} `json:"details"`
	}
	_ = json.Unmarshal(raw, &wire)
	failure := &hubError{Code: wire.Code, Status: wire.Status, Message: wire.Message}
	// The index is usable only as an integer. A wrongly typed one is not an
	// instruction to remove anything: decoded separately, so a failed decode
	// cannot leave a zero behind that looks like "the first envelope".
	var index int
	rawIndex := bytes.TrimSpace(wire.Details.Index)
	if len(rawIndex) > 0 && string(rawIndex) != "null" && json.Unmarshal(rawIndex, &index) == nil {
		failure.Index, failure.HasIdx = index, true
	}
	return failure
}

func (d *driver) enroll(token, game string) error {
	// Transport and hub failures are retried until the harness shuts the
	// driver down (stdin closing exits the process), so the driver's
	// patience is the harness's -enroll-wait and nothing of its own.
	for {
		status, body, err := d.post("/plugin/v1/enroll", "", map[string]any{
			"enrollmentToken": token,
			"game":            game,
			"plugin":          map[string]any{"name": "vyshka-conformance-driver", "version": "0.1.0"},
			"transports":      []string{"poll"},
		})
		if err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		if ok, failure := d.classify(status, body); !ok {
			// A hub failure or a malformed answer is retried like a transport
			// failure. A refusal is the operator's to fix; the driver has no
			// operator, so it reports and exits.
			if failure.Status == 0 || failure.Status >= 500 {
				log.Printf("enroll: %s (status %d): %s; retrying", failure.Code, failure.Status, failure.Message)
				time.Sleep(time.Second)
				continue
			}
			return fmt.Errorf("refused: %s (status %d): %s", failure.Code, failure.Status, failure.Message)
		}
		var enrolled struct {
			ServerID     string `json:"serverId"`
			ServerSecret string `json:"serverSecret"`
		}
		if err := json.Unmarshal(body, &enrolled); err != nil {
			return err
		}
		d.serverID = enrolled.ServerID
		d.serverSecret = enrolled.ServerSecret
		log.Println("enrolled as", d.serverID)
		return nil
	}
}

// startSession trades the stored credentials for a session and renumbers the
// unacked buffer into the new session's sequence space (spec section 9.1):
// seq moves, everything else stays.
func (d *driver) startSession(game string) error {
	status, body, err := d.post("/plugin/v1/session", "", map[string]any{
		"serverId":           d.serverID,
		"serverSecret":       d.serverSecret,
		"protocolVersion":    1,
		"pollTimeoutSeconds": 25,
		"plugin":             map[string]any{"name": "vyshka-conformance-driver", "version": "0.1.0"},
		"transports":         []string{"poll"},
	})
	if err != nil {
		return err
	}
	if ok, failure := d.classify(status, body); !ok {
		// Refused credentials are retried slowly: nothing but the operator
		// fixes them, and hammering the hub helps no one (section 2.3). A
		// server error or a malformed answer is an ordinary retry.
		// An opaque client error on a session request is treated the same way
		// (Appendix A): the class is all the driver has, and rejected
		// credentials are its likeliest meaning.
		opaqueClientError := failure.Code == "" && failure.Status >= 400 && failure.Status < 500
		if failure.Status == http.StatusUnauthorized || failure.Code == "credentials_invalid" || failure.Code == "credentials_revoked" || failure.Code == "protocol_version_unsupported" || opaqueClientError {
			return errRefused{failure}
		}
		// A hub failure or a malformed answer: retried after the section 2.3
		// minimum, unlike a transport failure, which is not a refusal.
		return errRetry{failure}
	}
	var session struct {
		SessionToken       string `json:"sessionToken"`
		PollTimeoutSeconds int    `json:"pollTimeoutSeconds"`
		Server             struct {
			BansRevision *int64 `json:"bansRevision"`
		} `json:"server"`
	}
	if err := json.Unmarshal(body, &session); err != nil {
		return err
	}
	d.sessionToken = session.SessionToken
	d.polledThisSession = false
	d.batchLimit = 200

	// The client-side response timeout must beat the hub's hold by 5 s
	// (spec section 3.1.1).
	d.client = &http.Client{Timeout: time.Duration(session.PollTimeoutSeconds+5) * time.Second}

	d.inAck = 0
	d.outSeq = 0
	for i := range d.buffer {
		d.outSeq++
		d.buffer[i].Seq = d.outSeq
	}
	log.Println("session started")

	// The ban list's revision (spec section 13.4): a session that begins with
	// the revision already enforced is reported, and one that differs is
	// walked. A hub that reports none serves no list.
	if session.Server.BansRevision != nil {
		d.bansKnown = *session.Server.BansRevision
		d.toldDuringWalk = d.walking
		if d.bansKnown == d.bansEnforced {
			d.send("bans.applied", map[string]any{"revision": d.bansEnforced})
		} else {
			d.bansDue = true
			d.bansNextTry = time.Time{}
		}
	}
	return nil
}

// send frames one envelope of the driver's own and buffers it until acked.
func (d *driver) send(envelopeType string, body any) {
	encoded, err := json.Marshal(body)
	if err != nil {
		encoded = json.RawMessage(`{}`)
	}
	d.outSeq++
	d.idCounter++
	d.buffer = append(d.buffer, envelope{
		V:    1,
		ID:   "driver-" + strconv.FormatInt(d.idCounter, 10),
		Type: envelopeType,
		Seq:  d.outSeq,
		TS:   time.Now().UTC().Format(time.RFC3339),
		Body: encoded,
	})
}

// errRefused is a session refusal that no quick retry fixes.
type errRefused struct{ failure *hubError }

func (e errRefused) Error() string {
	return fmt.Sprintf("session refused: %s (status %d): %s", e.failure.Code, e.failure.Status, e.failure.Message)
}

// errRetry is a session answer to back off from and try again: a hub failure
// or a malformed body.
type errRetry struct{ failure *hubError }

func (e errRetry) Error() string {
	return fmt.Sprintf("session: %s (status %d): %s", e.failure.Code, e.failure.Status, e.failure.Message)
}

func (d *driver) run(game string) {
	for {
		if d.sessionToken == "" {
			if err := d.startSession(game); err != nil {
				var refused errRefused
				if errors.As(err, &refused) {
					// Slow retry, doubling to a ceiling: the operator has to
					// act, and a plugin that hammers the hub is not helping.
					if d.sessionBackoff < 2*time.Second {
						d.sessionBackoff = 2 * time.Second
					} else if d.sessionBackoff < 8*time.Second {
						d.sessionBackoff *= 2
					}
					log.Printf("%v; retrying in %s", err, d.sessionBackoff)
					time.Sleep(d.sessionBackoff)
					continue
				}
				var retry errRetry
				if errors.As(err, &retry) {
					log.Printf("%v; retrying", err)
					time.Sleep(time.Second)
					continue
				}
				if d.transportFailed() {
					return
				}
				time.Sleep(200 * time.Millisecond)
				continue
			}
			d.firstFailure = time.Time{}
			d.sessionBackoff = 0
			if !d.manifestSent {
				d.send("manifest.publish", d.manifest(game))
				d.manifestSent = true
			}
			if !d.telemetrySent {
				// One batch and one snapshot, once per process, so the
				// harness has telemetry to grade (spec section 8). A real
				// plugin publishes as its game produces them.
				now := time.Now().UTC().Format(time.RFC3339)
				d.send("event.batch", map[string]any{"events": []map[string]any{
					{"t": "core.server.start", "ts": now, "data": map[string]any{"game": game}},
					{"t": "conformance-driver.hello", "ts": now},
				}})
				d.send("state.players", map[string]any{
					"capturedAt": now,
					"players": []map[string]any{{
						"player":   map[string]any{"platform": "conformance", "id": "driver-1"},
						"name":     "Driver",
						"position": []float64{100.5, 12, 200.25},
						"data":     map[string]any{"health": 100},
					}},
				})
				d.telemetrySent = true
			}
		}

		// A page of the ban list waits while a bans.changed the driver has
		// taken is still unacked: the poll after it carries the ack, so a
		// notice that arrived mid-walk is on record as taken before the walk
		// goes on. Only that notice gates the walk, so a hub with something
		// to deliver on every poll does not hold it up, and a page waits one
		// turn at most, so a list changing on every poll slows the walk but
		// cannot stop it (the spec asks no such order; the wait only lets the
		// suite see it).
		if d.bansDue && !time.Now().Before(d.bansNextTry) {
			if d.noticeUnacked && !d.pageWaited {
				d.pageWaited = true
			} else {
				d.pageWaited = false
				d.walkBans()
			}
		}

		batch := d.buffer
		if len(batch) > d.batchLimit {
			batch = batch[:d.batchLimit]
		}
		request := map[string]any{
			"ack":       d.inAck,
			"envelopes": batch,
		}
		// What the batch left behind is waiting, so the hub may answer at
		// once rather than hold (spec section 3.1.2).
		if len(batch) < len(d.buffer) {
			request["more"] = true
		}
		status, body, err := d.post("/plugin/v1/poll", d.sessionToken, request)
		if err != nil {
			// A transport failure is not a delivery failure: the buffer holds
			// everything unacked, and the next successful poll recovers it.
			if d.transportFailed() {
				return
			}
			time.Sleep(150 * time.Millisecond)
			continue
		}
		d.firstFailure = time.Time{}

		if ok, failure := d.classify(status, body); !ok {
			d.recover(failure)
			continue
		}
		d.polledThisSession = true
		d.unpolledRefusals = 0
		// This poll carried the ack of everything taken before it.
		d.noticeUnacked = false

		var response pollResponse
		if err := json.Unmarshal(body, &response); err != nil {
			log.Println("poll: bad response body:", err)
			time.Sleep(time.Second)
			continue
		}

		// Drop everything the hub has now durably processed.
		remaining := d.buffer[:0]
		for _, unacked := range d.buffer {
			if unacked.Seq > response.Ack {
				remaining = append(remaining, unacked)
			}
		}
		d.buffer = remaining

		// Take delivery in order: contiguous envelopes advance the ack and are
		// handled, duplicates at or below the ack are acknowledged again and
		// processed no further, anything above a gap is left for the hub's
		// retransmission to recover.
		for _, delivered := range response.Envelopes {
			if delivered.Seq <= d.inAck {
				continue
			}
			if delivered.Seq != d.inAck+1 {
				continue
			}
			d.inAck = delivered.Seq
			if delivered.Type == "bans.changed" {
				d.noticeUnacked = true
			}
			d.handle(delivered)
		}
	}
}

// recover applies the recovery table of spec section 2.3 to a refused poll.
func (d *driver) recover(failure *hubError) {
	switch {
	case failure.Status == 0:
		// A 200 that was not JSON: retry, touching nothing.
		log.Printf("poll: malformed answer (%s); re-polling", failure.Message)
		time.Sleep(time.Second)

	case failure.Code == "session_invalid", failure.Status == http.StatusUnauthorized:
		// Named or not, a 401 on a poll says the session is gone (section
		// 2.3: an unrecognized code with a 401 status is session_invalid).
		log.Println("session invalid; starting a new one")
		d.sessionToken = ""

	case failure.Code == "bad_request":
		// Usually the batch is over the hub's cap: send less next time. The
		// limit never drops below one envelope.
		if d.batchLimit > 1 {
			d.batchLimit /= 2
		}
		log.Printf("poll refused: %s; retrying with a batch of %d", failure.Message, d.batchLimit)
		time.Sleep(time.Second)

	case failure.Code == "envelope_invalid":
		// The hub applied nothing. Take the named envelope out, keep it where
		// an operator could find it (here, the log), and resend the rest with
		// the gap closed: the entries after it move down one seq, which is
		// safe because none of them was accepted either.
		if !failure.HasIdx || failure.Index < 0 || failure.Index >= len(d.buffer) || failure.Index >= d.batchLimit {
			log.Printf("poll: envelope_invalid without a usable details.index (%v); backing off", failure.HasIdx)
			time.Sleep(time.Second)
			return
		}
		condemned := d.buffer[failure.Index]
		log.Printf("hub refused envelope %s (type %s, seq %d): %s; setting it aside", condemned.ID, condemned.Type, condemned.Seq, failure.Message)
		d.buffer = append(d.buffer[:failure.Index], d.buffer[failure.Index+1:]...)
		for i := failure.Index; i < len(d.buffer); i++ {
			d.buffer[i].Seq--
		}
		d.outSeq--

	case failure.Code == "ack_out_of_range":
		log.Println("poll: ack out of range; starting a new session to reset the sequence space")
		d.sessionToken = ""

	case failure.Code == "" && failure.Status >= 400 && failure.Status < 500:
		// The opaque fallback (Appendix A): a client error on a session that
		// has polled means the session is gone; on one that never polled it
		// is more likely the batch, so back off before trying a session
		// again, rather than looping.
		if d.polledThisSession || d.unpolledRefusals >= 2 {
			if !d.polledThisSession {
				// A refusal on a session that never polled is backed off
				// whatever comes next, a replacement session included.
				time.Sleep(time.Second)
			}
			log.Println("poll refused; starting a new session")
			d.sessionToken = ""
			d.unpolledRefusals = 0
			return
		}
		d.unpolledRefusals++
		log.Println("poll refused again on a fresh session; backing off before retrying it")
		time.Sleep(2 * time.Second)

	case failure.Status >= 500:
		log.Printf("poll: hub error %s: %s; retrying", failure.Code, failure.Message)
		time.Sleep(time.Second)

	default:
		// bad_request, or a code this driver does not know with a 4xx
		// status: the request was wrong, a new session does not fix it.
		log.Printf("poll refused: %s (status %d): %s; backing off", failure.Code, failure.Status, failure.Message)
		time.Sleep(time.Second)
	}
}

// transportFailed tracks continuous transport failure and reports whether the
// driver should give up because the harness is clearly gone.
func (d *driver) transportFailed() bool {
	if d.firstFailure.IsZero() {
		d.firstFailure = time.Now()
		return false
	}
	if time.Since(d.firstFailure) > 30*time.Second {
		log.Println("hub unreachable for 30s; exiting")
		return true
	}
	return false
}

func (d *driver) manifest(game string) map[string]any {
	return map[string]any{
		"game":             game,
		"plugin":           map[string]any{"name": "vyshka-conformance-driver", "version": "0.1.0"},
		"manifestRevision": 1,
		"actions": []map[string]any{{
			"code":      "conformance-driver.echo",
			"name":      "Echo",
			"context":   "world",
			"namespace": "conformance-driver",
			"danger":    "none",
			"params": map[string]any{
				"type":     "object",
				"required": []string{"amount"},
				"properties": map[string]any{
					"amount": map[string]any{"type": "integer", "minimum": 1, "maximum": 100},
					// A string drawing on the declared context (the `context`
					// annotation of section 6.1), so the driver's manifest
					// exercises what a hub validates and a panel offers.
					"zone": map[string]any{"type": "string", "context": driverContext},
				},
			},
		}},
		"contexts": []map[string]any{{
			"id":        driverContext,
			"name":      "Zone",
			"namespace": "conformance-driver",
		}},
		"events": []any{},
		// The installation ban list (spec sections 6.7 and 13).
		"capabilities": []string{"bans"},
	}
}

func (d *driver) handle(delivered envelope) {
	switch delivered.Type {
	case "action.dispatch":
		var body struct {
			ActionID string          `json:"actionId"`
			Code     string          `json:"code"`
			Params   json.RawMessage `json:"params"`
		}
		// A body this driver cannot use is acked and ignored; nothing a hub
		// sends may crash the game server side.
		if err := json.Unmarshal(delivered.Body, &body); err != nil || body.ActionID == "" {
			log.Println("ignoring an action.dispatch with an unusable body")
			return
		}
		if d.executed[body.ActionID] {
			log.Println("skipping already-executed action", body.ActionID)
			return
		}
		d.markExecuted(body.ActionID)
		d.send("action.ack", map[string]any{"actionId": body.ActionID})

		var params map[string]any
		if err := json.Unmarshal(body.Params, &params); err != nil || params == nil {
			d.send("action.result", map[string]any{
				"actionId": body.ActionID, "ok": false,
				"error": "params was not a JSON object",
			})
			return
		}
		// The params come back as the result, which is what makes the
		// driver's round trip checkable, unless they would pass the 64 KiB a
		// hub keeps of a result (section 7): then their size stands in.
		result := map[string]any{"echo": params}
		if len(body.Params) > 64<<10 {
			result = map[string]any{"echoBytes": len(body.Params)}
		}
		d.send("action.result", map[string]any{
			"actionId": body.ActionID, "ok": true,
			"result": result, "durationMs": 1,
		})

	case "context.enumerate":
		// The members of a declared context (spec section 6.2). A request
		// naming a context this driver does not declare is answered too,
		// with an empty entries and a reason, never treated as a fault of
		// the link.
		var body struct {
			RequestID string `json:"requestId"`
			Context   string `json:"context"`
		}
		if err := json.Unmarshal(delivered.Body, &body); err != nil || body.RequestID == "" {
			log.Println("ignoring a context.enumerate with an unusable body")
			return
		}
		reply := map[string]any{"requestId": body.RequestID, "context": body.Context}
		if body.Context == driverContext {
			reply["entries"] = []map[string]any{
				{
					"referenceKey": "north-ridge",
					"label":        "North Ridge",
					"position":     []float64{4231.5, 300.25, 10620},
				},
				{
					"referenceKey": "south-hollow",
					"label":        "South Hollow",
					"data":         map[string]any{"guarded": true},
				},
			}
		} else {
			reply["entries"] = []map[string]any{}
			reply["reason"] = "this plugin declares no context named " + body.Context
		}
		d.send("context.entries", reply)

	case "bans.changed":
		// A nudge (spec section 13.3): the list is walked when the revision
		// differs from the one enforced, lower included.
		var body struct {
			Revision *int64 `json:"revision"`
		}
		if err := json.Unmarshal(delivered.Body, &body); err != nil || body.Revision == nil {
			log.Println("ignoring a bans.changed with an unusable body")
			return
		}
		d.bansKnown = *body.Revision
		d.toldDuringWalk = d.toldDuringWalk || d.walking
		if d.bansKnown != d.bansEnforced {
			d.bansDue = true
			d.bansNextTry = time.Time{}
		}

	default:
		// Unknown types are acked and ignored (spec section 4).
	}
}

// walkBans reads one page of the installation ban list (spec section 13.4)
// per turn of the run loop, so the driver keeps polling while it walks, as a
// plugin on an engine that cannot block its main loop must: a bans.changed
// can then arrive in the middle of a walk, and the walk's end must not take
// its own revision for the latest one. The walk is held to the revision its
// first page was served at, and applied and reported only once it is whole. A
// conflict starts it over at once; any other failure keeps the list the
// driver holds and tries again later.
func (d *driver) walkBans() {
	if !d.walking {
		d.walking = true
		d.walkRevision = -1
		d.walkCursor = ""
		d.walkPages = 0
		d.walked = nil
		d.toldDuringWalk = false
	}
	fail := func(why string) {
		log.Printf("bans: %s; trying again later", why)
		d.walking = false
		d.bansNextTry = time.Now().Add(2 * time.Second)
	}
	restart := func(why string) {
		d.walkRestarts++
		if d.walkRestarts > 3 {
			d.walkRestarts = 0
			fail(why + ", and the walk has started over three times")
			return
		}
		log.Printf("bans: %s; starting the walk over", why)
		d.walking = false
	}
	if d.walkPages > 10000 {
		fail("a walk did not end")
		return
	}
	query := "limit=100"
	if d.walkCursor != "" {
		query += "&cursor=" + d.walkCursor
	}
	status, body, err := d.get("/plugin/v1/bans", query, d.sessionToken)
	if err != nil {
		fail("walk failed: " + err.Error())
		return
	}
	if ok, failure := d.classify(status, body); !ok {
		if failure.Code == "conflict" || (failure.Code == "" && failure.Status == http.StatusConflict) {
			restart("the hub cannot serve the walk's revision any more")
			return
		}
		fail(fmt.Sprintf("walk refused: %s (status %d): %s", failure.Code, failure.Status, failure.Message))
		return
	}
	var page struct {
		Revision   int64      `json:"revision"`
		Bans       []banEntry `json:"bans"`
		NextCursor string     `json:"nextCursor"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		fail("unusable page: " + err.Error())
		return
	}
	if d.walkRevision < 0 {
		d.walkRevision = page.Revision
	} else if page.Revision != d.walkRevision {
		restart(fmt.Sprintf("a page came at revision %d in a walk of %d", page.Revision, d.walkRevision))
		return
	}
	d.walked = append(d.walked, page.Bans...)
	d.walkPages++
	if page.NextCursor != "" {
		d.walkCursor = page.NextCursor
		return
	}

	// Whole: apply, then report. The first page was served at the hub's
	// current revision, fresher than anything the driver was told before the
	// walk; a revision it was told during the walk is fresher still, and one
	// that differs is another walk owed at once.
	revision := d.walkRevision
	d.bans = d.walked
	d.bansEnforced = revision
	d.walking = false
	d.walkRestarts = 0
	if !d.toldDuringWalk {
		d.bansKnown = revision
	}
	d.bansDue = d.bansKnown != revision
	log.Printf("bans: applied revision %d (%d entries)", revision, len(d.walked))
	d.send("bans.applied", map[string]any{"revision": revision})
}

func (d *driver) markExecuted(actionID string) {
	if len(d.executedOrder) >= executedLRUCap {
		oldest := d.executedOrder[0]
		d.executedOrder = d.executedOrder[1:]
		delete(d.executed, oldest)
	}
	d.executed[actionID] = true
	d.executedOrder = append(d.executedOrder, actionID)
}
