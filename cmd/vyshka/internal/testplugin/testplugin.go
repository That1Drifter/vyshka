// Package testplugin is a small, autonomous fake game-server plugin for
// end-to-end tests of the vyshka command-line client. It enrolls against a hub,
// opens a session, publishes a manifest, answers action dispatches and context
// enumerations, and lets a test push events and state snapshots, all over the
// HTTP long-poll transport of spec section 3.1 and nothing else.
//
// It imports only the standard library, so a test can boot a hub in-process,
// point this plugin at it, and drive the whole thing through the client under
// test without a game server.
//
// One poll in flight. The spec asks a plugin to keep at most one poll open per
// session (section 3.1), and the hub holds every poll it has nothing for until
// the negotiated timeout. A plugin that wants a result to reach the hub at
// once therefore cannot just fire a second poll beside the held one: the hub
// parks at most four polls per session and answers the rest empty and
// immediately, which would send a listener loop into a spin. Instead the one
// poll loop is interrupted whenever the plugin queues something new: the held
// poll is aborted (an aborted poll is not a delivery failure, section 3.1.1) and
// the next carries every unacked envelope, so a result is on the wire within a
// millisecond of being produced, not after the hold.
package testplugin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultGame        = "cli-test"
	defaultPollTimeout = 5

	pluginName    = "vyshka-testplugin"
	pluginVersion = "0.1.0"

	// maxBatch is the number of envelopes every hub must accept in one poll
	// (spec section 3.1.2). A longer outbox goes out in several polls, with
	// `more` telling the hub the rest is waiting.
	maxBatch = 200
	// maxEventsPerBatch is the largest event.batch a hub accepts (section 8.1).
	maxEventsPerBatch = 200
	// maxEventsPerPoll is the per-poll event budget of section 8.1 as the
	// reference hub applies it: event.batch envelopes past it in one poll are
	// refused with an event.reject, so a poll's batch stays within it and
	// the rest waits for the next.
	maxEventsPerPoll = 1000

	// persistentFailure is how long transport failure has to go on, unbroken,
	// before the plugin gives up and reports it through Err. A single failed
	// poll is ordinary and is retried after retryDelay.
	persistentFailure = 10 * time.Second
	retryDelay        = 200 * time.Millisecond

	// executedCap bounds the executed-actionId memory of spec section 9.2.
	executedCap = 128

	maxResponseBytes = 16 << 20
)

// Options configures a plugin. Only HubURL and EnrollmentToken are required.
type Options struct {
	// HubURL is the hub's base URL, without a trailing slash.
	HubURL string
	// EnrollmentToken is the one-time token from the Admin API's servers create.
	EnrollmentToken string
	// Game is the game id to enroll as. Default "cli-test".
	Game string
	// Manifest is the manifest.publish body. Nil means DefaultManifest().
	Manifest map[string]any
	// Contexts answers context.enumerate by context id. Nil means the members
	// of the contexts DefaultManifest declares.
	Contexts map[string][]ContextEntry
	// Handler decides what to do with each action.dispatch. Nil means
	// DefaultHandler.
	Handler func(Dispatch) Outcome
	// PollTimeoutSeconds is the hold time to ask the hub for. Default 5, the
	// floor every hub must honor (spec section 3.1.1).
	PollTimeoutSeconds int
	// Logf receives one line per notable protocol step. Optional.
	Logf func(format string, args ...any)
}

// Dispatch is an action.dispatch as the handler sees it.
type Dispatch struct {
	ActionID     string
	Code         string
	Context      string
	ReferenceKey string
	Params       map[string]any
	// ExpiresAt is the deadline the plugin must discard the action after. The
	// zero time means the hub sent none.
	ExpiresAt time.Time
}

// Outcome is what a handler wants done with a dispatch.
type Outcome struct {
	// Ack sends an action.ack (state becomes running) on receipt.
	Ack bool
	// Result is the action.result to send. Nil means never answer, so the hub
	// expires the action at its deadline.
	Result *Result
	// Delay is waited, after the ack is queued, before the result is queued.
	Delay time.Duration
}

// Result is the body of an action.result.
type Result struct {
	OK     bool
	Result any
	Error  string
	// DurationMs is reported as durationMs. Zero means measure it: the time
	// from receiving the dispatch to queueing the result.
	DurationMs int64
}

// ContextEntry is one member of a custom context (spec section 6.2).
type ContextEntry struct {
	ReferenceKey string         `json:"referenceKey"`
	Label        string         `json:"label"`
	Position     []float64      `json:"position,omitempty"`
	Data         map[string]any `json:"data,omitempty"`
}

// Event is one telemetry event (spec section 8.1).
type Event struct {
	Type string
	// At is when the event happened. The zero time omits ts, which a hub
	// answers with its own receipt time.
	At   time.Time
	Data map[string]any
}

// envelope is the wire form of spec section 4. Bodies are kept as encoded
// bytes so a retransmission is byte-identical to the first send (section 9.1).
type envelope struct {
	V    int             `json:"v"`
	ID   string          `json:"id"`
	Type string          `json:"type"`
	Seq  int64           `json:"seq"`
	TS   string          `json:"ts"`
	Body json.RawMessage `json:"body"`
	// events is how many events an event.batch carries, for the per-poll
	// budget. Not on the wire.
	events int
}

type pollRequest struct {
	Ack       int64      `json:"ack"`
	Envelopes []envelope `json:"envelopes,omitempty"`
	More      bool       `json:"more,omitempty"`
}

type pollResponse struct {
	Envelopes []envelope `json:"envelopes"`
	Ack       int64      `json:"ack"`
}

// hubError is a non-2xx answer, read from the error model of spec section 2.2.
type hubError struct {
	Status  int
	Code    string
	Message string
}

func (e *hubError) Error() string {
	return fmt.Sprintf("hub answered %d %s: %s", e.Status, e.Code, e.Message)
}

// Plugin is a running fake plugin. All methods are safe for concurrent use.
type Plugin struct {
	opts   Options
	client *http.Client

	serverID     string
	serverSecret string
	sessionToken string
	envelopeV    int

	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup // the poll loop and every handler goroutine
	idPrefix  string
	wake      chan struct{} // nudges a sleeping loop when something is queued
	closeOnce sync.Once

	mu sync.Mutex
	// inAck is the highest contiguous hub -> plugin seq processed; outSeq the
	// last seq handed to an envelope of ours; outAck the highest of ours the
	// hub has acked. outbound holds every envelope above outAck, in seq order.
	inAck  int64
	outSeq int64
	outAck int64
	// outSent is the highest seq ever put on the wire, aborted polls
	// included since the hub may have ingested one: the most an ack may
	// name.
	outSent         int64
	outbound        []envelope
	dispatches      []Dispatch
	rejections      []map[string]any
	eventRejections []map[string]any
	executed        map[string]bool
	executedQ       []string
	err             error
	idCounter       int64

	// cancelPoll aborts the poll in flight, and pollCarried is the highest
	// outbound seq that poll covers. Something queued above it is not on the
	// wire yet, which is what makes aborting the poll worth it.
	cancelPoll  context.CancelFunc
	pollCarried int64
}

// Start enrolls, opens a session, queues the manifest as the first envelope,
// and starts the poll loop. It returns once the session is open; the manifest
// reaches the hub on the loop's first poll. Cancelling ctx stops the plugin
// like Close does.
func Start(ctx context.Context, o Options) (*Plugin, error) {
	if o.HubURL == "" || o.EnrollmentToken == "" {
		return nil, errors.New("testplugin: HubURL and EnrollmentToken are required")
	}
	o.HubURL = strings.TrimRight(o.HubURL, "/")
	if o.Game == "" {
		o.Game = defaultGame
	}
	if o.PollTimeoutSeconds <= 0 {
		o.PollTimeoutSeconds = defaultPollTimeout
	}
	if o.Handler == nil {
		o.Handler = DefaultHandler
	}
	if o.Manifest == nil {
		o.Manifest = DefaultManifest()
		o.Manifest["game"] = o.Game
	}
	if o.Contexts == nil {
		o.Contexts = defaultContexts()
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	manifest, err := json.Marshal(o.Manifest)
	if err != nil {
		return nil, fmt.Errorf("testplugin: encode manifest: %w", err)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	p := &Plugin{
		opts:      o,
		client:    &http.Client{Transport: transport, Timeout: 15 * time.Second},
		idPrefix:  "tp-" + randomHex(6),
		wake:      make(chan struct{}, 1),
		executed:  map[string]bool{},
		envelopeV: 1,
	}
	p.ctx, p.cancel = context.WithCancel(ctx)

	var enrolled struct {
		ServerID     string `json:"serverId"`
		ServerSecret string `json:"serverSecret"`
	}
	if err := p.post(p.ctx, "/plugin/v1/enroll", "", map[string]any{
		"enrollmentToken": o.EnrollmentToken,
		"game":            o.Game,
		"plugin":          map[string]any{"name": pluginName, "version": pluginVersion},
		"transports":      []string{"poll"},
	}, &enrolled); err != nil {
		p.abort()
		return nil, fmt.Errorf("testplugin: enroll: %w", err)
	}
	p.serverID, p.serverSecret = enrolled.ServerID, enrolled.ServerSecret

	var session struct {
		SessionToken       string `json:"sessionToken"`
		PollTimeoutSeconds int    `json:"pollTimeoutSeconds"`
		EnvelopeVersion    int    `json:"envelopeVersion"`
	}
	if err := p.post(p.ctx, "/plugin/v1/session", "", map[string]any{
		"serverId":           p.serverID,
		"serverSecret":       p.serverSecret,
		"protocolVersion":    1,
		"pollTimeoutSeconds": o.PollTimeoutSeconds,
		"plugin":             map[string]any{"name": pluginName, "version": pluginVersion},
		"transports":         []string{"poll"},
	}, &session); err != nil {
		p.abort()
		return nil, fmt.Errorf("testplugin: session: %w", err)
	}
	if session.SessionToken == "" {
		p.abort()
		return nil, errors.New("testplugin: session response carried no token")
	}
	p.sessionToken = session.SessionToken
	if session.EnvelopeVersion > 0 {
		p.envelopeV = session.EnvelopeVersion
	}
	// The client-side response timeout must beat the hub's hold by 5 s, or the
	// hub's timely empty answer loses the race with the abort (section 3.1.1).
	// The hub's answer is authoritative for the hold, so it decides the size.
	effective := session.PollTimeoutSeconds
	if effective <= 0 {
		effective = o.PollTimeoutSeconds
	}
	p.client.Timeout = time.Duration(effective+5) * time.Second

	p.mu.Lock()
	p.enqueueLocked("manifest.publish", manifest, 0)
	p.mu.Unlock()

	p.wg.Add(1)
	go p.run()
	o.Logf("testplugin: enrolled as %s, session open, poll hold %ds", p.serverID, effective)
	return p, nil
}

// ServerID is the hub's id for the server this plugin enrolled as.
func (p *Plugin) ServerID() string { return p.serverID }

// Emit queues events as event.batch envelopes, split at the 200 events a hub
// accepts per batch. No events queues nothing.
func (p *Plugin) Emit(events ...Event) {
	for len(events) > 0 {
		n := min(len(events), maxEventsPerBatch)
		batch := make([]map[string]any, 0, n)
		for _, event := range events[:n] {
			wire := map[string]any{"t": event.Type}
			if !event.At.IsZero() {
				wire["ts"] = stamp(event.At)
			}
			if event.Data != nil {
				wire["data"] = event.Data
			}
			batch = append(batch, wire)
		}
		p.enqueueCounted("event.batch", map[string]any{"events": batch}, n)
		events = events[n:]
	}
}

// Snapshot queues a state.<kind> envelope. kind is players, vehicles,
// entities, or world; body is the whole snapshot body (spec section 8.3).
func (p *Plugin) Snapshot(kind string, body map[string]any) {
	if body == nil {
		body = map[string]any{}
	}
	p.enqueue("state."+kind, body)
}

// Dispatches is every action.dispatch received so far, in order, including
// ones past their deadline that the plugin discarded unexecuted.
func (p *Plugin) Dispatches() []Dispatch {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.dispatches)
}

// Rejections is the body of every manifest.reject received so far.
func (p *Plugin) Rejections() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.rejections)
}

// EventRejections is the body of every event.reject received so far: a
// batch the hub refused, which a test expecting every emitted event to be
// stored needs to see rather than have hidden by the fixture.
func (p *Plugin) EventRejections() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.eventRejections)
}

// Err is the first fatal transport or session error, or nil. After it is set
// the plugin has stopped polling.
func (p *Plugin) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

// Close stops the poll loop and every handler goroutine and waits for them.
// It is idempotent. Close reports nothing about the plugin's health: Err does.
func (p *Plugin) Close() error {
	p.closeOnce.Do(p.cancel)
	p.wg.Wait()
	p.client.CloseIdleConnections()
	return nil
}

// abort releases a plugin whose Start failed before any goroutine ran.
func (p *Plugin) abort() {
	p.cancel()
	p.client.CloseIdleConnections()
}

// fail records the first fatal error and stops the loop.
func (p *Plugin) fail(err error) {
	p.mu.Lock()
	if p.err == nil {
		p.err = err
	}
	p.mu.Unlock()
	p.opts.Logf("testplugin: fatal: %v", err)
	p.cancel()
}

// enqueue frames one envelope of ours and queues it until the hub acks it.
func (p *Plugin) enqueue(typ string, body any) {
	p.enqueueCounted(typ, body, 0)
}

// enqueueCounted is enqueue for an event.batch, which says how many events
// it carries so a poll can stay within the hub's budget.
func (p *Plugin) enqueueCounted(typ string, body any, events int) {
	encoded, err := json.Marshal(body)
	if err != nil {
		p.fail(fmt.Errorf("testplugin: encode %s body: %w", typ, err))
		return
	}
	p.mu.Lock()
	p.enqueueLocked(typ, encoded, events)
	p.mu.Unlock()
}

func (p *Plugin) enqueueLocked(typ string, body json.RawMessage, events int) {
	p.outSeq++
	p.idCounter++
	p.outbound = append(p.outbound, envelope{
		V:      p.envelopeV,
		ID:     p.idPrefix + "-" + strconv.FormatInt(p.idCounter, 10),
		Type:   typ,
		Seq:    p.outSeq,
		TS:     stamp(time.Now()),
		Body:   body,
		events: events,
	})
	// A poll already on the wire without this envelope would hold it until the
	// hub's timeout; abort that poll so the next one carries it.
	if p.cancelPoll != nil && p.outSeq > p.pollCarried {
		p.cancelPoll()
	}
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// run is the poll loop. It re-polls at once after every answer (section 3.1),
// so a request is normally held open on the hub and dispatches arrive with no
// delay.
func (p *Plugin) run() {
	defer p.wg.Done()

	var failingSince time.Time
	stalled := 0
	for p.ctx.Err() == nil {
		request, pollCtx, cancel := p.beginPoll()
		started := time.Now()
		var response pollResponse
		err := p.post(pollCtx, "/plugin/v1/poll", p.sessionToken, request, &response)
		superseded := pollCtx.Err() != nil
		p.endPoll()
		cancel()

		if err != nil {
			if p.ctx.Err() != nil {
				return
			}
			if superseded {
				// Aborted because something newer wants to go out. Nothing was
				// acked and nothing was lost: the next poll retransmits.
				continue
			}
			var refused *hubError
			wait := retryDelay
			if errors.As(err, &refused) {
				if refused.Status < 500 {
					// session_invalid included: this plugin does not renew, a
					// test that wants a second session starts a second plugin.
					p.fail(fmt.Errorf("testplugin: poll refused: %w", err))
					return
				}
				wait = time.Second
			}
			if failingSince.IsZero() {
				failingSince = time.Now()
			}
			if time.Since(failingSince) > persistentFailure {
				p.fail(fmt.Errorf("testplugin: hub unreachable for %s: %w", persistentFailure, err))
				return
			}
			if !p.pause(wait) {
				return
			}
			continue
		}
		failingSince = time.Time{}

		delivered, taken := p.apply(response)
		switch {
		case delivered > 0 && taken == 0:
			stalled++
		default:
			stalled = 0
		}
		// A hub that keeps answering at once with nothing usable would have this
		// loop spinning: envelopes it re-sends that were already taken, or an
		// empty answer to a poll that carried nothing. Both are transient in a
		// conforming exchange, so only a run of them earns a pause.
		idle := delivered == 0 && len(request.Envelopes) == 0 && time.Since(started) < 20*time.Millisecond
		if stalled >= 3 || idle {
			if !p.pause(50 * time.Millisecond) {
				return
			}
		}
	}
}

// beginPoll builds the next request from the state as it stands and registers
// the poll as abortable, in one critical section so nothing queued in between
// can be missed by both this poll and the abort.
func (p *Plugin) beginPoll() (pollRequest, context.Context, context.CancelFunc) {
	p.mu.Lock()
	defer p.mu.Unlock()

	request := pollRequest{Ack: p.inAck}
	batch := p.outbound
	// Two limits frame a batch: the 200 envelopes every hub accepts, and
	// the per-poll event budget, which the event.batch envelopes in one poll
	// must stay within or the hub refuses the ones over it. Either limit
	// cuts the batch and says more is waiting.
	cut := len(batch)
	events := 0
	for i, pending := range batch {
		if i >= maxBatch {
			cut = i
			break
		}
		events += pending.events
		if events > maxEventsPerPoll && i > 0 {
			cut = i
			break
		}
	}
	if cut < len(batch) {
		batch = batch[:cut]
		request.More = true
	}
	request.Envelopes = slices.Clone(batch)

	p.pollCarried = p.outSeq
	if len(batch) > 0 {
		p.pollCarried = batch[len(batch)-1].Seq
		p.outSent = max(p.outSent, p.pollCarried)
	}
	pollCtx, cancel := context.WithCancel(p.ctx)
	p.cancelPoll = cancel
	return request, pollCtx, cancel
}

func (p *Plugin) endPoll() {
	p.mu.Lock()
	p.cancelPoll = nil
	p.mu.Unlock()
}

// apply takes one poll answer. The hub's ack frees our outbox, and its
// envelopes are processed only when they are next in line: an envelope above a
// gap or one already taken is left for the hub's retransmission (spec section
// 9.1). It returns how many envelopes came and how many were newly taken.
func (p *Plugin) apply(response pollResponse) (delivered, taken int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Acks are monotonic and never above what was sent. An ack past the
	// highest seq ever put on the wire is a hub fault, and one this fixture
	// must not paper over by dropping envelopes it never sent: it is
	// recorded as fatal instead, without the lock-taking fail.
	if response.Ack > p.outSent {
		if p.err == nil {
			p.err = fmt.Errorf("testplugin: the hub acked seq %d, above the %d sent", response.Ack, p.outSent)
		}
		p.cancel()
		return 0, 0
	}
	if response.Ack > p.outAck {
		p.outAck = response.Ack
		drop := 0
		for drop < len(p.outbound) && p.outbound[drop].Seq <= p.outAck {
			drop++
		}
		p.outbound = p.outbound[drop:]
	}

	for _, inbound := range response.Envelopes {
		delivered++
		if inbound.Seq != p.inAck+1 {
			continue
		}
		p.inAck = inbound.Seq
		taken++
		p.handleLocked(inbound)
	}
	return delivered, taken
}

// handleLocked acts on one hub envelope. Types it does not know are ignored
// and, because the ack has already advanced, acked (spec section 4).
func (p *Plugin) handleLocked(inbound envelope) {
	switch inbound.Type {
	case "action.dispatch":
		var body struct {
			ActionID     string         `json:"actionId"`
			Code         string         `json:"code"`
			Context      string         `json:"context"`
			ReferenceKey string         `json:"referenceKey"`
			Params       map[string]any `json:"params"`
			ExpiresAt    string         `json:"expiresAt"`
		}
		if err := json.Unmarshal(inbound.Body, &body); err != nil || body.ActionID == "" {
			p.opts.Logf("testplugin: ignoring an action.dispatch with an unusable body")
			return
		}
		if p.executed[body.ActionID] {
			// At-least-once delivery: an action id is never run twice (section 9.2).
			return
		}
		p.executed[body.ActionID] = true
		p.executedQ = append(p.executedQ, body.ActionID)
		if len(p.executedQ) > executedCap {
			delete(p.executed, p.executedQ[0])
			p.executedQ = p.executedQ[1:]
		}

		dispatch := Dispatch{
			ActionID:     body.ActionID,
			Code:         body.Code,
			Context:      body.Context,
			ReferenceKey: body.ReferenceKey,
			Params:       body.Params,
		}
		if parsed, err := time.Parse(time.RFC3339, body.ExpiresAt); err == nil {
			dispatch.ExpiresAt = parsed
		}
		p.dispatches = append(p.dispatches, dispatch)
		p.opts.Logf("testplugin: dispatch %s %s", dispatch.ActionID, dispatch.Code)

		if !dispatch.ExpiresAt.IsZero() && time.Now().After(dispatch.ExpiresAt) {
			// The deadline in the body is the one the plugin MUST discard after
			// (section 7); the hub has already told the operator it expired.
			p.opts.Logf("testplugin: discarding %s, past its deadline", dispatch.ActionID)
			return
		}
		p.wg.Add(1)
		go p.execute(dispatch)

	case "context.enumerate":
		var body struct {
			RequestID string `json:"requestId"`
			Context   string `json:"context"`
		}
		if err := json.Unmarshal(inbound.Body, &body); err != nil || body.RequestID == "" {
			p.opts.Logf("testplugin: ignoring a context.enumerate with an unusable body")
			return
		}
		reply := map[string]any{"requestId": body.RequestID, "context": body.Context}
		if entries, declared := p.opts.Contexts[body.Context]; declared {
			if entries == nil {
				entries = []ContextEntry{}
			}
			reply["entries"] = entries
		} else {
			// A context this plugin does not declare is answered too, empty with
			// a reason, so the hub learns the two disagree (section 6.2).
			reply["entries"] = []ContextEntry{}
			reply["reason"] = "this plugin declares no context named " + body.Context
		}
		encoded, err := json.Marshal(reply)
		if err != nil {
			// Only a caller's own Data values can fail to encode.
			p.opts.Logf("testplugin: cannot answer context %s: %v", body.Context, err)
			return
		}
		p.enqueueLocked("context.entries", encoded, 0)

	case "manifest.reject":
		var body map[string]any
		if err := json.Unmarshal(inbound.Body, &body); err != nil {
			body = map[string]any{"raw": string(inbound.Body)}
		}
		p.rejections = append(p.rejections, body)
		p.opts.Logf("testplugin: manifest rejected: %s", inbound.Body)

	case "event.reject":
		var body map[string]any
		if err := json.Unmarshal(inbound.Body, &body); err != nil {
			body = map[string]any{"raw": string(inbound.Body)}
		}
		p.eventRejections = append(p.eventRejections, body)
		p.opts.Logf("testplugin: event batch rejected: %s", inbound.Body)
	}
}

// execute runs one dispatch through the handler and queues what it asked for.
func (p *Plugin) execute(dispatch Dispatch) {
	defer p.wg.Done()
	defer func() {
		if recovered := recover(); recovered != nil {
			p.fail(fmt.Errorf("testplugin: handler panicked on %s: %v", dispatch.Code, recovered))
		}
	}()

	received := time.Now()
	outcome := p.opts.Handler(dispatch)
	if outcome.Ack {
		p.enqueue("action.ack", map[string]any{"actionId": dispatch.ActionID})
	}
	if outcome.Result == nil {
		return
	}
	if outcome.Delay > 0 {
		timer := time.NewTimer(outcome.Delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-p.ctx.Done():
			return
		}
	}

	result := outcome.Result
	body := map[string]any{"actionId": dispatch.ActionID, "ok": result.OK}
	if result.Result != nil {
		body["result"] = result.Result
	}
	if result.Error != "" {
		body["error"] = result.Error
	}
	duration := result.DurationMs
	if duration == 0 {
		duration = time.Since(received).Milliseconds()
	}
	body["durationMs"] = duration
	p.enqueue("action.result", body)
}

// pause sleeps, cut short by anything being queued, and reports false when the
// plugin is stopping.
func (p *Plugin) pause(d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-p.ctx.Done():
		return false
	case <-p.wake:
		return true
	case <-timer.C:
		return true
	}
}

// post sends one JSON request and decodes a 2xx answer into out.
func (p *Plugin) post(ctx context.Context, path, bearer string, in, out any) error {
	encoded, err := json.Marshal(in)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, p.opts.HubURL+path, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	response, err := p.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		refused := &hubError{Status: response.StatusCode}
		var wire struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &wire) == nil {
			refused.Code, refused.Message = wire.Error.Code, wire.Error.Message
		}
		if refused.Message == "" {
			refused.Message = strings.TrimSpace(string(raw))
		}
		return refused
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// stamp is RFC 3339 in UTC with milliseconds, the shape the hub itself uses.
func stamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

func randomHex(n int) string {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(raw)
}
