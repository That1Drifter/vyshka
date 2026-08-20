// Command driver is the reference candidate for the plugin conformance
// harness: a minimal but correct autonomous plugin. It enrolls, keeps a
// session, long-polls, publishes a manifest, executes dispatched actions with
// an executed-actionId LRU, buffers unacked envelopes across outages, and
// renumbers them across session changes. CI runs the harness against it to
// prove the suite goes green against a compliant implementation.
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
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
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

type driver struct {
	baseURL string
	client  *http.Client

	serverID     string
	serverSecret string
	sessionToken string

	// inAck is the highest contiguous hub -> plugin seq processed; outSeq the
	// last seq assigned to an envelope of the driver's own. buffer holds every
	// outbound envelope the hub has not acked, in seq order.
	inAck  int64
	outSeq int64
	buffer []envelope

	// The executed-actionId LRU of spec section 9.2.
	executed      map[string]bool
	executedOrder []string

	idCounter    int64
	manifestSent bool
	firstFailure time.Time
}

func main() {
	url := flag.String("url", os.Getenv("VYSHKA_HUB_URL"), "hub base URL (env VYSHKA_HUB_URL)")
	token := flag.String("token", os.Getenv("VYSHKA_ENROLLMENT_TOKEN"), "one-time enrollment token (env VYSHKA_ENROLLMENT_TOKEN)")
	game := flag.String("game", "conformance", "game id to enroll as")
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
		baseURL:  *url,
		client:   &http.Client{Timeout: 10 * time.Second},
		executed: map[string]bool{},
	}
	if err := d.enroll(*token, *game); err != nil {
		log.Println("enroll:", err)
		os.Exit(2)
	}
	d.run(*game)
}

func (d *driver) post(path, bearer string, body any) (int, []byte, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	request, err := http.NewRequest(http.MethodPost, d.baseURL+path, bytes.NewReader(encoded))
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Content-Type", "application/json")
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

func (d *driver) enroll(token, game string) error {
	for attempt := 0; attempt < 50; attempt++ {
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
		if status != http.StatusCreated {
			return fmt.Errorf("status %d: %s", status, body)
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
	return fmt.Errorf("hub unreachable")
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
	if status != http.StatusOK {
		return fmt.Errorf("session: status %d: %s", status, body)
	}
	var session struct {
		SessionToken       string `json:"sessionToken"`
		PollTimeoutSeconds int    `json:"pollTimeoutSeconds"`
	}
	if err := json.Unmarshal(body, &session); err != nil {
		return err
	}
	d.sessionToken = session.SessionToken

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

func (d *driver) run(game string) {
	for {
		if d.sessionToken == "" {
			if err := d.startSession(game); err != nil {
				if d.transportFailed() {
					return
				}
				time.Sleep(200 * time.Millisecond)
				continue
			}
			d.firstFailure = time.Time{}
			if !d.manifestSent {
				d.send("manifest.publish", d.manifest(game))
				d.manifestSent = true
			}
		}

		status, body, err := d.post("/plugin/v1/poll", d.sessionToken, map[string]any{
			"ack":       d.inAck,
			"envelopes": d.buffer,
		})
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

		if status == http.StatusUnauthorized {
			// session_invalid: request a new session, never re-enroll.
			log.Println("session invalid; starting a new one")
			d.sessionToken = ""
			continue
		}
		if status != http.StatusOK {
			log.Printf("poll: status %d: %s", status, body)
			time.Sleep(150 * time.Millisecond)
			continue
		}

		var response pollResponse
		if err := json.Unmarshal(body, &response); err != nil {
			log.Println("poll: bad response body:", err)
			time.Sleep(150 * time.Millisecond)
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
			d.handle(delivered)
		}
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
				},
			},
		}},
		"contexts": []any{},
		"events":   []any{},
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
		d.send("action.result", map[string]any{
			"actionId": body.ActionID, "ok": true,
			"result": map[string]any{"echo": params}, "durationMs": 1,
		})

	default:
		// Unknown types are acked and ignored (spec section 4).
	}
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
