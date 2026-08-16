package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
	V    int             `json:"v"`
	ID   string          `json:"id"`
	Type string          `json:"type"`
	Seq  int64           `json:"seq"`
	TS   string          `json:"ts"`
	Body json.RawMessage `json:"body"`
}

type pollRequest struct {
	Ack       int64      `json:"ack"`
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
		if _, err := time.Parse(time.RFC3339, delivered.TS); err != nil {
			return pollResponse{}, fmt.Errorf("poll: envelope %d has ts %q, which is not RFC 3339",
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

// uniqueType mints an envelope type no hub can have heard of, for the checks
// that grade the forward-compatibility rule.
var unknownTypeCounter atomic.Int64

func unknownType() string {
	return "conformance.unknown-type-" + strconv.FormatInt(unknownTypeCounter.Add(1), 10)
}
