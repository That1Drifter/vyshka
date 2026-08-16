package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"sync/atomic"
	"time"
)

// This file is the suite's fake plugin: a minimal but correct implementation of
// the plugin half of the protocol, used to drive a hub through the exchanges
// only a plugin can start. Later slices grow it rather than replace it.
//
// It speaks HTTP and nothing else. Like everything under conformance/, it must
// never import hub code, or the suite stops being able to grade an
// implementation that is not this repository's.

// Poll timeout bounds a hub must honor (spec section 3.1.1).
const (
	minPollTimeout = 5 * time.Second
	maxPollTimeout = 60 * time.Second
	// shortPollTimeout is what checks negotiate when they intend to wait a hold
	// out. It is the protocol floor, which is the least time that costs.
	shortPollTimeoutSeconds = 5
)

// envelope is the wire form of spec section 4. It is hand-written here, not
// shared with the hub, so that a change in the reference implementation shows up
// as a suite failure rather than as a silently redefined contract.
type envelope struct {
	// V omits itself when zero so a check can send an envelope with no `v` at
	// all, which is a plugin's right and means the negotiated version. An
	// explicit `"v": 0` is a different thing entirely and must be refused, so
	// checks that want it build the body by hand.
	V    int             `json:"v,omitempty"`
	ID   string          `json:"id"`
	Type string          `json:"type"`
	Seq  int64           `json:"seq"`
	TS   string          `json:"ts"`
	Body json.RawMessage `json:"body"`
}

// pollRequest omits both fields when they are zero, so that the suite actually
// exercises the rule that every field is optional and `{}` is a valid idle poll.
// Always sending `"ack": 0` would leave that untested.
type pollRequest struct {
	Ack       int64      `json:"ack,omitempty"`
	Envelopes []envelope `json:"envelopes,omitempty"`
}

type pollResponse struct {
	Envelopes          []envelope `json:"envelopes"`
	Ack                int64      `json:"ack"`
	PollTimeoutSeconds int        `json:"pollTimeoutSeconds"`
	SessionExpiresAt   string     `json:"sessionExpiresAt"`
}

// queuedEnvelope is what the Admin API reports for a queued envelope. It carries
// no seq: sequence numbers belong to a session (spec section 5.5).
type queuedEnvelope struct {
	Envelope struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		TS   string `json:"ts"`
	} `json:"envelope"`
}

// fakePlugin is one enrolled game server with a live session, tracking the
// sequence state of both directions the way a real plugin must.
type fakePlugin struct {
	env     Env
	Name    string
	Server  createdServer
	Creds   credentials
	Session sessionRecord

	// inboundAck is the highest contiguous hub -> plugin seq this plugin has
	// processed, and what it reports on the next poll. outboundSeq is the last
	// seq it assigned to an envelope of its own.
	inboundAck  int64
	outboundSeq int64
}

// newFakePlugin walks a fresh server all the way to a live session.
func (e Env) newFakePlugin(ctx context.Context, name string, pollTimeoutSeconds int) (*fakePlugin, error) {
	creds, created, err := e.newEnrolled(ctx, name)
	if err != nil {
		return nil, err
	}
	session, err := e.startSession(ctx, creds, pollTimeoutSeconds)
	if err != nil {
		return nil, err
	}
	return &fakePlugin{env: e, Name: name, Server: created, Creds: creds, Session: session}, nil
}

// reconnect starts a new session on the same credentials, which resets both
// sequence spaces (spec section 9.1).
func (p *fakePlugin) reconnect(ctx context.Context, pollTimeoutSeconds int) error {
	session, err := p.env.startSession(ctx, p.Creds, pollTimeoutSeconds)
	if err != nil {
		return err
	}
	p.Session = session
	p.inboundAck = 0
	p.outboundSeq = 0
	return nil
}

// nextOutbound frames one envelope of the plugin's own, assigning the next seq.
func (p *fakePlugin) nextOutbound(envelopeType string, body any) envelope {
	p.outboundSeq++
	encoded := json.RawMessage(`{}`)
	if body != nil {
		if marshalled, err := json.Marshal(body); err == nil {
			encoded = marshalled
		}
	}
	return envelope{
		V:    1,
		ID:   "conformance-" + strconv.FormatInt(p.outboundSeq, 10) + "-" + envelopeType,
		Type: envelopeType,
		Seq:  p.outboundSeq,
		TS:   time.Now().UTC().Format(time.RFC3339),
		Body: encoded,
	}
}

// renumber rewrites an envelope into a new session's sequence space, which is
// what a plugin must do with anything still unacked when its session ends
// (spec section 9.1). Everything a receiver deduplicates on is preserved; only
// `seq` moves, because only `seq` belonged to the old session.
func (p *fakePlugin) renumber(unacked envelope) envelope {
	p.outboundSeq++
	unacked.Seq = p.outboundSeq
	return unacked
}

// poll sends one poll exactly as given, without touching the plugin's own
// sequence state. Checks that need to lie to the hub use this.
func (p *fakePlugin) poll(ctx context.Context, request pollRequest) (pollResponse, error) {
	var response pollResponse
	if err := p.env.expectPoll(ctx, p.Session.SessionToken, request, http.StatusOK, &response); err != nil {
		return pollResponse{}, err
	}
	if response.Envelopes == nil {
		return pollResponse{}, fmt.Errorf("poll: envelopes is null; an empty batch must be []")
	}
	for i, delivered := range response.Envelopes {
		if delivered.Seq < 1 {
			return pollResponse{}, fmt.Errorf("poll: envelope %d has seq %d, want 1 or above", i, delivered.Seq)
		}
		if delivered.ID == "" || delivered.Type == "" {
			return pollResponse{}, fmt.Errorf("poll: envelope %d is missing id or type: %+v", i, delivered)
		}
		if delivered.V < 1 {
			return pollResponse{}, fmt.Errorf("poll: envelope %d carries v %d; a hub stamps the negotiated envelope version on everything it sends",
				i, delivered.V)
		}
		stamped, err := time.Parse(time.RFC3339, delivered.TS)
		if err != nil {
			return pollResponse{}, fmt.Errorf("poll: envelope %d has ts %q, which is not RFC 3339",
				i, delivered.TS)
		}
		if _, offset := stamped.Zone(); offset != 0 {
			return pollResponse{}, fmt.Errorf("poll: envelope %d has ts %q; section 4 requires UTC",
				i, delivered.TS)
		}
		if i > 0 && delivered.Seq <= response.Envelopes[i-1].Seq {
			return pollResponse{}, fmt.Errorf("poll: batch is not in ascending seq order: %d after %d",
				delivered.Seq, response.Envelopes[i-1].Seq)
		}
	}
	return response, nil
}

// pollAndAck is the loop a real plugin runs: report what was processed last
// time, send anything outbound, and take delivery of whatever comes back.
func (p *fakePlugin) pollAndAck(ctx context.Context, outbound ...envelope) (pollResponse, error) {
	response, err := p.poll(ctx, pollRequest{Ack: p.inboundAck, Envelopes: outbound})
	if err != nil {
		return pollResponse{}, err
	}
	// Contiguity: a plugin acks what it has processed with no gap below it.
	for _, delivered := range response.Envelopes {
		if delivered.Seq != p.inboundAck+1 {
			break
		}
		p.inboundAck = delivered.Seq
	}
	return response, nil
}

// send frames envelopes and pushes them on one poll.
func (p *fakePlugin) send(ctx context.Context, envelopes ...envelope) (pollResponse, error) {
	return p.pollAndAck(ctx, envelopes...)
}

// pollExpectingError asserts a poll fails with a given status and code.
func (p *fakePlugin) pollExpectingError(ctx context.Context, request pollRequest, wantStatus int, wantCode string) error {
	return p.env.expectPollError(ctx, p.Session.SessionToken, request, wantStatus, wantCode)
}

// pollInBackground starts a poll and hands back a channel that carries its
// result, so a check can do something to the hub while the request is held.
func (p *fakePlugin) pollInBackground(ctx context.Context, request pollRequest) <-chan heldPoll {
	done := make(chan heldPoll, 1)
	started := time.Now()
	go func() {
		response, err := p.poll(ctx, request)
		done <- heldPoll{Response: response, Err: err, Elapsed: time.Since(started)}
	}()
	// Give the hub a moment to actually be holding the request before the check
	// does whatever is supposed to wake it.
	time.Sleep(250 * time.Millisecond)
	return done
}

// heldPoll is the outcome of a poll that ran while the check did something else.
type heldPoll struct {
	Response pollResponse
	Err      error
	Elapsed  time.Duration
}

// queueEnvelope asks the hub, as an operator, to queue one envelope for a
// server. It is how the suite drives the hub -> plugin direction without
// depending on any higher-level feature.
func (e Env) queueEnvelope(ctx context.Context, serverID, envelopeType string, body any) (string, error) {
	request := map[string]any{"type": envelopeType}
	if body != nil {
		request["body"] = body
	}

	var queued queuedEnvelope
	if err := e.expect(ctx, http.MethodPost, "/api/v1/servers/"+serverID+"/envelopes",
		e.AdminToken, request, http.StatusAccepted, &queued); err != nil {
		return "", err
	}
	if queued.Envelope.ID == "" {
		return "", fmt.Errorf("queue envelope: response carried no envelope.id")
	}
	return queued.Envelope.ID, nil
}

// manifestBody builds a manifest.publish body (spec section 6). With no
// actions given it declares the section's example: one heal action whose
// params schema stays inside the subset.
func manifestBody(revision int64, actions ...map[string]any) map[string]any {
	if actions == nil {
		actions = []map[string]any{{
			"code": "example-mod.heal", "name": "Heal player",
			"context": "player", "namespace": "example-mod", "danger": "warning",
			"params": map[string]any{
				"type":     "object",
				"required": []string{"amount"},
				"properties": map[string]any{
					"amount": map[string]any{"type": "integer", "minimum": 1, "maximum": 100},
					"item":   map[string]any{"type": "string", "x-vyshka-widget": "itemlist"},
				},
			},
		}}
	}
	return map[string]any{
		"game":             fixtureGame,
		"plugin":           map[string]any{"name": "conformance-plugin", "version": "0.1.0"},
		"manifestRevision": revision,
		"actions":          actions,
		"contexts":         []any{},
		"events":           []any{},
	}
}

// publishManifest frames and sends one manifest.publish, acking as it goes.
func (p *fakePlugin) publishManifest(ctx context.Context, body map[string]any) (pollResponse, error) {
	return p.send(ctx, p.nextOutbound("manifest.publish", body))
}

// manifestRecord is the Admin API manifest view (spec section 6.5).
type manifestRecord struct {
	Revision    int64           `json:"revision"`
	PublishedAt string          `json:"publishedAt"`
	Manifest    json.RawMessage `json:"manifest"`
}

// storedManifest reads a server's manifest back through the Admin API.
func (e Env) storedManifest(ctx context.Context, serverID string) (manifestRecord, error) {
	var record manifestRecord
	err := e.expect(ctx, http.MethodGet, "/api/v1/servers/"+serverID+"/manifest",
		e.AdminToken, nil, http.StatusOK, &record)
	return record, err
}

// awaitEnvelope polls until an envelope of the wanted type arrives, acking
// everything on the way, for up to attempts polls. The type's other deliveries
// in the same responses are returned too, so a check can assert exactly one.
func (p *fakePlugin) awaitEnvelope(ctx context.Context, envelopeType string, attempts int) ([]envelope, error) {
	var matched []envelope
	for range attempts {
		response, err := p.pollAndAck(ctx)
		if err != nil {
			return nil, err
		}
		for _, delivered := range response.Envelopes {
			if delivered.Type == envelopeType {
				matched = append(matched, delivered)
			}
		}
		if len(matched) > 0 {
			return matched, nil
		}
	}
	return nil, fmt.Errorf("no %s envelope arrived in %d polls", envelopeType, attempts)
}

// actionRecord is the Admin API action view (spec section 7).
type actionRecord struct {
	ID          string          `json:"id"`
	ServerID    string          `json:"serverId"`
	Code        string          `json:"code"`
	State       string          `json:"state"`
	CreatedAt   string          `json:"createdAt"`
	ExpiresAt   string          `json:"expiresAt"`
	DeliveredAt *string         `json:"deliveredAt"`
	RunningAt   *string         `json:"runningAt"`
	FinishedAt  *string         `json:"finishedAt"`
	OK          *bool           `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Error       *string         `json:"error"`
	DurationMs  *int64          `json:"durationMs"`
}

// dispatchAction dispatches one action through the Admin API.
func (e Env) dispatchAction(ctx context.Context, serverID string, body map[string]any) (string, string, error) {
	var response struct {
		ActionID string `json:"actionId"`
		State    string `json:"state"`
	}
	err := e.expect(ctx, http.MethodPost, "/api/v1/servers/"+serverID+"/actions",
		e.AdminToken, body, http.StatusAccepted, &response)
	if err != nil {
		return "", "", err
	}
	if response.ActionID == "" {
		return "", "", fmt.Errorf("dispatch: response carried no actionId")
	}
	return response.ActionID, response.State, nil
}

// action reads one action record back through the Admin API.
func (e Env) action(ctx context.Context, actionID string) (actionRecord, error) {
	var record actionRecord
	err := e.expect(ctx, http.MethodGet, "/api/v1/actions/"+actionID,
		e.AdminToken, nil, http.StatusOK, &record)
	return record, err
}

// awaitActionState polls the Admin API until the action reaches the wanted
// state or the deadline passes. Used where a state change is the hub's own
// doing (expiry) rather than an immediate consequence of a request.
func (e Env) awaitActionState(ctx context.Context, actionID, want string, patience time.Duration) (actionRecord, error) {
	deadline := time.Now().Add(patience)
	for {
		record, err := e.action(ctx, actionID)
		if err != nil {
			return actionRecord{}, err
		}
		if record.State == want {
			return record, nil
		}
		if time.Now().After(deadline) {
			return actionRecord{}, fmt.Errorf("action %s is still %q after %s, want %q",
				actionID, record.State, patience, want)
		}
		select {
		case <-ctx.Done():
			return actionRecord{}, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// eventBatch frames one event.batch body (spec section 8.1).
func eventBatch(events ...map[string]any) map[string]any {
	if events == nil {
		events = []map[string]any{}
	}
	return map[string]any{"events": events}
}

// sendEvents pushes one event.batch and acks whatever comes back.
func (p *fakePlugin) sendEvents(ctx context.Context, events ...map[string]any) (pollResponse, error) {
	return p.send(ctx, p.nextOutbound("event.batch", eventBatch(events...)))
}

// eventRecord is the Admin API event view (spec section 8.5).
type eventRecord struct {
	ID         string          `json:"id"`
	ServerID   string          `json:"serverId"`
	Type       string          `json:"type"`
	OccurredAt string          `json:"occurredAt"`
	ReceivedAt string          `json:"receivedAt"`
	Data       json.RawMessage `json:"data"`
}

type eventPage struct {
	Events     []eventRecord `json:"events"`
	NextCursor string        `json:"nextCursor"`
}

// events reads one page of a server's feed. parameters are appended as a query
// string, so a check can grade filters and pagination without a URL builder.
func (e Env) events(ctx context.Context, serverID string, parameters url.Values) (eventPage, error) {
	path := "/api/v1/servers/" + serverID + "/events"
	if encoded := parameters.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var page eventPage
	err := e.expect(ctx, http.MethodGet, path, e.AdminToken, nil, http.StatusOK, &page)
	return page, err
}

// eventTypes lists the types on a page, for checks that assert what a filter
// matched rather than what one particular event carried.
func (p eventPage) eventTypes() []string {
	types := make([]string, 0, len(p.Events))
	for _, one := range p.Events {
		types = append(types, one.Type)
	}
	return types
}

// uniqueType mints an envelope type no hub can have heard of, for the checks
// that grade the forward-compatibility rule.
var unknownTypeCounter atomic.Int64

func unknownType() string {
	return "conformance.unknown-type-" + strconv.FormatInt(unknownTypeCounter.Add(1), 10)
}
