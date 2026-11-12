package hub

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/That1Drifter/vyshka/hub/internal/token"
	"github.com/That1Drifter/vyshka/hub/store"
)

// pollBackstop bounds how long a held poll goes without re-reading the database.
// The waiter wakes it as soon as work is queued, so this only has to catch what
// an in-process notification cannot see: a session that expired, and a hub that
// grows a second process one day.
const pollBackstop = time.Second

// maxNoticesPerPoll caps how many rejection notices one poll may queue, across
// every notice kind together (spec sections 6.4 and 8.1).
//
// A poll may legally carry 500 envelopes, and every refusable one owes the
// plugin a notice. Uncapped, a plugin looping on traffic it keeps getting wrong
// buys 500 queue inserts inside the store's transaction, which on SQLite's
// single connection is the whole hub's critical section, and fills its own
// outbound queue with complaints about its own bug until that server can no
// longer be dispatched an action. The first notices name real envelopes, which
// is what a plugin author needs to fix it; the rest reach the hub's log as a
// count. The budget is shared rather than one per kind because what it protects
// is the queue, not either notice type, and a poll wrong in two ways at once
// should not cost twice as much as one wrong in a single way.
const maxNoticesPerPoll = 20

// maxHeldPollsPerSession bounds how many polls one session may park at once.
// It is an implementation limit rather than a protocol rule: a poll over the
// ceiling is answered at once instead of held, which the protocol allows at any
// moment. See the `holds` type for why the ceiling exists.
const maxHeldPollsPerSession = 4

type pollRequest struct {
	// Ack is the highest contiguous hub -> plugin seq the plugin has durably
	// processed. Absent or 0 acks nothing.
	Ack       int64             `json:"ack"`
	Envelopes []inboundEnvelope `json:"envelopes"`
	// More says the plugin cut this batch short and holds envelopes behind
	// it (spec section 3.1.2). Absent means false. It stays raw because
	// decoding into a bool would take a JSON null for false without a word.
	More json.RawMessage `json:"more"`
}

type pollResponse struct {
	// Envelopes is always present. An empty batch is a normal answer to a hold
	// that expired with nothing queued, not an error.
	Envelopes []envelope `json:"envelopes"`
	// Ack is the highest contiguous plugin -> hub seq the hub has durably
	// processed, which is what the plugin trims its ring buffer against.
	Ack                int64  `json:"ack"`
	PollTimeoutSeconds int    `json:"pollTimeoutSeconds"`
	SessionExpiresAt   string `json:"sessionExpiresAt"`
}

// handlePoll is the transport heartbeat (spec section 3.1.2). One request
// carries both directions: what the plugin has to say and what the hub has been
// holding for it. Everything the plugin sent is applied before the hold begins,
// so a poll that only acks takes effect at once rather than in 25 seconds.
func (s *Server) handlePoll(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	session, server, ok := s.authenticateSession(w, r)
	if !ok {
		return
	}
	// Handler time is the whole request less the hold: a long-poll's wait is
	// the plugin's cadence, not the hub's latency, so it is booked apart. The
	// hold runs from the first time the poll parks to the last time it wakes,
	// the backstop rechecks in between included (see holdPoll).
	var held time.Duration
	defer func() {
		s.metrics.pollHandler.ObserveDuration(max(0, time.Since(started)-held))
		s.metrics.pollHold.ObserveDuration(held)
	}()
	sessionTokenHash := token.Hash(bearerToken(r))

	var request pollRequest
	if !s.decodeOptionalJSON(w, r, &request) {
		return
	}
	if len(request.Envelopes) > maxInboundEnvelopes {
		writeError(w, http.StatusBadRequest, codeBadRequest,
			"this hub accepts at most "+strconv.Itoa(maxInboundEnvelopes)+" envelopes per poll")
		return
	}
	if request.Ack < 0 {
		writeError(w, http.StatusBadRequest, codeBadRequest, "ack must not be negative")
		return
	}
	more, ok := decodeMore(request.More)
	if !ok {
		writeError(w, http.StatusBadRequest, codeBadRequest, "more must be a boolean")
		return
	}
	for index, inbound := range request.Envelopes {
		if fault := validateInbound(index, inbound); fault != nil {
			writeErrorDetails(w, http.StatusBadRequest, codeEnvelopeInvalid,
				fault.Message, fault.details())
			return
		}
	}

	// The plugin's ack first: it frees queued work, so applying it before the
	// outbound read is what stops an acked envelope from being sent once more.
	switch err := s.store.AckOutbound(r.Context(), session.ID, request.Ack); {
	case errors.Is(err, store.ErrAckOutOfRange):
		writeError(w, http.StatusBadRequest, codeAckOutOfRange,
			"ack is above the highest seq this hub has sent on this session")
		return
	case errors.Is(err, store.ErrNotFound):
		s.rejectSession(w, "session token is expired, unknown, or superseded")
		return
	case err != nil:
		s.writeInternalError(w, r, err)
		return
	}

	// Then the plugin's envelopes. The hub models manifest.publish, action.ack,
	// action.result, event.batch, the state.* snapshots, and bans.applied on the
	// inbound path; every other accepted envelope takes the forward-compatibility path
	// of spec section 4: acked and ignored. Bodies are validated up front
	// because validity depends only on content, while which envelopes are
	// newly accepted is only known inside the transaction.
	now := time.Now().UTC()
	manifests := prepareManifests(request.Envelopes)
	actions := prepareActions(request.Envelopes)
	events := s.prepareEvents(request.Envelopes, now)
	snapshots := s.prepareSnapshots(request.Envelopes, now)
	contextReplies := prepareContextEntries(request.Envelopes, now)
	bansApplied := prepareBansApplied(request.Envelopes)
	replayedBatches, err := s.ingestedEventBatches(r.Context(), server.ID, request.Envelopes, events)
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}

	// The classification runs inside the store's transaction against the ack as
	// committed, not against the copy this request authenticated with, so two
	// overlapping polls cannot report acks that disagree. Manifest applies and
	// rejection notices ride the same transaction: acking an envelope promises
	// its effect is already durable (spec section 9.3).
	var batch inboundBatch
	var unusableActionBodies, rejectedManifests, refusedEventBatches, refusedSnapshots, suppressedNotices int
	// A manifest declaring the bans capability is applied under the ban
	// list's lock, which is what keeps a change of the list from falling
	// between the plugin's walk and the manifest that makes it a recipient
	// (spec section 13.3).
	apply := s.store.ApplyInbound
	for _, prepared := range manifests {
		if prepared.publish != nil && slices.Contains(prepared.publish.Capabilities, store.CapabilityBans) {
			apply = s.store.ApplyInboundDeclaringBans
		}
	}
	applied, err := apply(r.Context(), session.ID, func(ack int64) store.InboundApplication {
		batch = classifyInbound(ack, request.Envelopes)
		unusableActionBodies = 0
		rejectedManifests = 0
		refusedEventBatches = 0
		refusedSnapshots = 0
		suppressedNotices = 0
		// The event budget is charged here rather than during validation, so
		// that only envelopes actually being accepted spend it: a poll carrying
		// retransmitted batches alongside new ones must not lose the new ones
		// to duplicates that store nothing.
		eventBudget := maxEventsPerPoll
		application := store.InboundApplication{Ack: batch.Ack, Accepted: len(batch.Accepted)}
		// Every refusal below owes the plugin a notice, but only so many of them
		// are narrated: see maxNoticesPerPoll for why one poll cannot be allowed
		// to mint an unbounded number. Suppression counts rather than refuses,
		// because the refusal itself stands either way.
		queueNotice := func(notice store.Notice) {
			if len(application.Notices) >= maxNoticesPerPoll {
				suppressedNotices++
				return
			}
			application.Notices = append(application.Notices, notice)
		}
		for _, index := range batch.Accepted {
			if prepared, isManifest := manifests[index]; isManifest {
				if prepared.publish != nil {
					application.Manifests = append(application.Manifests, *prepared.publish)
				} else {
					rejectedManifests++
					queueNotice(prepared.reject)
				}
				continue
			}
			if prepared, isAction := actions[index]; isAction {
				switch {
				case prepared.ack != "":
					application.ActionAcks = append(application.ActionAcks, prepared.ack)
				case prepared.result != nil:
					application.ActionResults = append(application.ActionResults, *prepared.result)
				default:
					unusableActionBodies++
				}
				continue
			}
			if prepared, isEvents := events[index]; isEvents {
				refuse := func(notice store.Notice) {
					refusedEventBatches++
					queueNotice(notice)
				}
				switch {
				case prepared.reject != nil:
					refuse(*prepared.reject)
				case replayedBatches[request.Envelopes[index].ID]:
					// Replayed across a session change (spec section 8.1): it
					// stores nothing, so it spends no budget, and refusing it
					// would send a notice claiming its events are gone while
					// they sit in the feed. It still goes to the store, whose
					// per-batch claim is the dedup of record.
					application.EventBatches = append(application.EventBatches, store.NewEventBatch{
						EnvelopeID: request.Envelopes[index].ID,
						Events:     prepared.events,
					})
				case len(prepared.events) > eventBudget:
					refuse(newEventBudgetReject(request.Envelopes[index].ID))
				default:
					eventBudget -= len(prepared.events)
					application.EventBatches = append(application.EventBatches, store.NewEventBatch{
						EnvelopeID: request.Envelopes[index].ID,
						Events:     prepared.events,
					})
				}
				continue
			}
			if prepared, isSnapshot := snapshots[index]; isSnapshot {
				if prepared.reject != nil {
					refusedSnapshots++
					queueNotice(*prepared.reject)
				} else {
					application.Snapshots = append(application.Snapshots, *prepared.snapshot)
				}
				continue
			}
			if revision, isReport := bansApplied[index]; isReport {
				application.BansApplied = append(application.BansApplied, revision)
			}
		}
		return application
	}, outboundQueueLimit)
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.rejectSession(w, "session token is expired, unknown, or superseded")
		return
	case err != nil:
		s.writeInternalError(w, r, err)
		return
	}
	inboundAck := applied.Ack
	s.countInbound(batch, applied, rejectedManifests, refusedEventBatches, refusedSnapshots, suppressedNotices)

	// Context enumeration replies change no durable state, so they are
	// handed to the reads waiting on them once the batch is committed, and
	// only the newly accepted ones: a retransmitted reply was delivered the
	// first time (spec section 6.2). One nobody asked for is acked and
	// ignored, as the section says.
	var unsolicitedContextReplies int
	for _, index := range batch.Accepted {
		prepared, isReply := contextReplies[index]
		if !isReply {
			continue
		}
		if !s.enumerations.deliver(server.ID, prepared, now) {
			unsolicitedContextReplies++
		}
	}
	if unsolicitedContextReplies > 0 {
		s.log.Info("poll carried context.entries replies nobody was waiting for; acked and ignored",
			"serverId", server.ID, "sessionId", session.ID, "count", unsolicitedContextReplies)
	}

	if len(request.Envelopes) > 0 {
		s.log.Info("poll ingested envelopes",
			"serverId", server.ID, "sessionId", session.ID,
			"accepted", len(batch.Accepted), "duplicate", batch.Duplicate,
			"gapped", batch.Gapped, "ack", inboundAck,
			"manifestsApplied", applied.ManifestsApplied,
			"rejectsQueued", applied.NoticesQueued,
			"actionsStarted", applied.ActionsStarted,
			"actionsFinished", applied.ActionsFinished,
			"eventsStored", applied.EventsStored,
			"snapshotsStored", applied.SnapshotsStored,
			"bansReported", applied.BansReported)
	}
	if unusableActionBodies > 0 {
		s.log.Warn("poll carried action envelopes with unusable bodies; acked and ignored",
			"serverId", server.ID, "sessionId", session.ID, "count", unusableActionBodies)
	}
	if rejectedManifests > 0 {
		s.log.Warn("poll carried manifests this hub rejected",
			"serverId", server.ID, "sessionId", session.ID, "count", rejectedManifests)
	}
	if refusedEventBatches > 0 {
		s.log.Warn("poll carried event batches this hub refused",
			"serverId", server.ID, "sessionId", session.ID,
			"batches", refusedEventBatches, "eventBudget", maxEventsPerPoll)
	}
	if refusedSnapshots > 0 {
		s.log.Warn("poll carried state snapshots this hub refused",
			"serverId", server.ID, "sessionId", session.ID, "count", refusedSnapshots)
	}
	if suppressedNotices > 0 {
		// The notices the plugin will not receive, so the count survives
		// somewhere even when the cap swallows the envelopes naming them. What
		// was queued is read back from the store rather than assumed to be the
		// cap: a queue at its bound (spec section 9.2) drops notices the cap
		// let through, and a log that reported the cap would be reporting a
		// number nobody delivered.
		s.log.Warn("poll owed more rejection notices than one poll may queue; the rest were suppressed",
			"serverId", server.ID, "sessionId", session.ID, "cap", maxNoticesPerPoll,
			"queued", applied.NoticesQueued, "suppressed", suppressedNotices)
	}
	if applied.NoticesQueued > 0 {
		// A rejection notice is ordinary queued work: wake anything else this
		// server has parked. This request picks it up itself on the read below.
		s.waiters.notify(server.ID)
	}
	if applied.EventsStored > 0 || applied.ActionsFinished > 0 {
		// Fresh telemetry and terminal actions are what webhooks push (spec
		// section 11.1); the dispatcher's ticker would find them anyway, but a
		// nudge makes delivery prompt.
		s.webhooks.Nudge()
	}

	if err := s.store.TouchServer(r.Context(), server.ID); err != nil {
		s.writeInternalError(w, r, err)
		return
	}

	s.holdPoll(w, r, session, sessionTokenHash, inboundAck, backlogFloor(more, request.Envelopes), &held)
}

// pollBudget is what the envelopes of one poll response may take, so that the
// whole body stays inside maxPluginResponse (spec section 3.1.2). The rest of
// the response is measured with the widest ack it could carry, since the ack
// is refreshed after the batch is cut; each envelope is charged one byte more
// than itself for the comma that separates it from the next.
func pollBudget(session store.Session) store.OutboundBudget {
	frame := encodedLen(pollResponse{
		Envelopes:          []envelope{},
		Ack:                math.MaxInt64,
		PollTimeoutSeconds: session.PollTimeoutSeconds,
		SessionExpiresAt:   envelopeTimestamp(session.ExpiresAt),
	}) + 1 // the encoder's trailing newline
	return store.OutboundBudget{
		Bytes: maxPluginResponse - frame,
		Size: func(queued store.OutboundEnvelope) int {
			return encodedLen(newOutboundEnvelope(queued)) + 1
		},
	}
}

// countInbound books what one committed poll did to the inbound envelope
// counters. It runs after the commit, so a poll that failed or retried counts
// nothing.
func (s *Server) countInbound(batch inboundBatch, applied store.InboundApplied, manifests, eventBatches, snapshots, suppressed int) {
	m := s.metrics
	m.inbound.Add(float64(len(batch.Accepted)), inboundAccepted)
	m.inbound.Add(float64(batch.Duplicate), inboundDuplicate)
	m.inbound.Add(float64(batch.Gapped), inboundGapped)
	m.inboundRefused.Add(float64(manifests), refusedManifest)
	m.inboundRefused.Add(float64(eventBatches), refusedEventBatch)
	m.inboundRefused.Add(float64(snapshots), refusedSnapshot)
	m.outboundDropped.Add(float64(applied.NoticesDropped), droppedQueueFull)
	m.outboundDropped.Add(float64(suppressed), droppedNoticeCap)
}

// decodeMore reads the poll's more member: absent is false, and anything but
// a JSON boolean is refused.
func decodeMore(raw json.RawMessage) (bool, bool) {
	switch string(bytes.TrimSpace(raw)) {
	case "":
		return false, true
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, false
}

// backlogFloor is the lowest seq a poll saying more carried, and 0 for any
// other poll. The hub answers such a poll without holding once the ack it
// answers with reaches the floor, covering at least one envelope the poll
// carried (spec section 3.1.2). An ack below it means the next poll carries
// the same batch again, so answering at once would buy a loop rather than
// progress.
func backlogFloor(more bool, envelopes []inboundEnvelope) int64 {
	if !more {
		return 0
	}
	var floor int64
	for _, inbound := range envelopes {
		if floor == 0 || inbound.Seq < floor {
			floor = inbound.Seq
		}
	}
	return floor
}

// holdPoll answers as soon as there is anything to send, and otherwise holds the
// request until the negotiated timeout, the session's expiry, or the session
// being ended out from under it. A backlogFloor above 0 (see backlogFloor)
// ends the hold as soon as the ack it answers with reaches it, which a
// concurrent poll committing the envelopes below the floor can also bring
// about.
//
// The ack it reports is refreshed from committed state on every database read,
// never left at the value this request ingested: a concurrent poll may commit a
// higher ack and be answered while this one is still held, and an ack the hub
// has reported must never be lowered by a later response (spec section 9.1).
//
// held receives how long the poll was held, for the poll metrics: from the first
// time it parks to the last time it wakes. Everything in between is hold, the
// one-second backstop rechecks (a session lookup and a queue read each) as much
// as the waiting, because they scale with how long the poll is held and not with
// anything the hub was asked to do. What runs before the first park (answering
// at once is a poll that never parks) and after the last wake is handler time.
func (s *Server) holdPoll(w http.ResponseWriter, r *http.Request, session store.Session, sessionTokenHash string, inboundAck int64, backlogFloor int64, held *time.Duration) {
	pollTimeout := clampPollTimeout(time.Duration(session.PollTimeoutSeconds) * time.Second)
	deadline := time.Now().Add(pollTimeout)
	// A hold that outlives its own session would answer 401 late; ending it at
	// the expiry lets the plugin renew and re-poll instead.
	if session.ExpiresAt.Before(deadline) {
		deadline = session.ExpiresAt
	}

	var parkedAt time.Time

	respond := func(envelopes []envelope) {
		if envelopes == nil {
			envelopes = []envelope{}
		}
		writeJSON(w, http.StatusOK, pollResponse{
			Envelopes:          envelopes,
			Ack:                inboundAck,
			PollTimeoutSeconds: session.PollTimeoutSeconds,
			SessionExpiresAt:   envelopeTimestamp(session.ExpiresAt),
		})
	}

	// Refused only when this session is already parking its ceiling of polls, in
	// which case this one still reports everything queued, it just does not wait
	// around for more.
	mayHold, releaseHold := s.holds.enter(session.ID, maxHeldPollsPerSession)
	defer releaseHold()

	for {
		// Registered before the read, so work queued between the read and the
		// select still wakes this poll instead of waiting for the backstop.
		signal, release := s.waiters.wait(session.ServerID)

		budget := pollBudget(session)
		queued, _, committedAck, err := s.store.NextOutboundWithin(r.Context(), session.ID, session.ServerID, maxOutboundBatch, budget)
		if err != nil {
			release()
			if errors.Is(err, store.ErrNotFound) {
				s.rejectSession(w, "session token is expired, unknown, or superseded")
				return
			}
			s.writeInternalError(w, r, err)
			return
		}
		inboundAck = max(inboundAck, committedAck)
		drain := backlogFloor > 0 && inboundAck >= backlogFloor
		if len(queued) > 0 {
			release()
			// Only an envelope queued before the hub bounded bodies (protocol
			// draft 0.35) can be too large for a response alone. It is sent
			// anyway, since holding it back would stall everything behind it;
			// an engine that cannot receive it will keep failing this poll.
			if len(queued) == 1 && budget.Size(queued[0]) > budget.Bytes {
				s.log.Warn("poll sent one envelope larger than a Plugin API response may be; it was queued before the body bound",
					"serverId", session.ServerID, "sessionId", session.ID, "envelopeId", queued[0].ID,
					"bytes", budget.Size(queued[0]), "bound", maxPluginResponse)
			}
			envelopes := make([]envelope, len(queued))
			refund := false
			for i, one := range queued {
				envelopes[i] = newOutboundEnvelope(one)
				if one.FirstSend {
					s.metrics.envelopeDelivery.ObserveDuration(time.Since(one.CreatedAt))
					// The ban mark is consumed on every first send of a notice,
					// whether or not this poll is already refunded, so it does
					// not outlive the notice it was set for. One window is
					// left: the mark is set just after the list change
					// commits, so a poll that sends the notice in between is
					// charged and the mark waits for that server's next one.
					banMarked := one.Type == envelopeTypeBansChanged && s.limits.takeBanNotice(session.ServerID)
					refund = refund || adminCaused(one.Type) || banMarked
				}
			}
			// A poll that carries work an admin queued, on its first send, is
			// not charged to the server (spec section 2.2.1): the plugin is
			// answering the hub, and charging it would let the Admin API's
			// pace throttle a plugin that did nothing but keep up.
			if refund {
				s.limits.plugin.Refund(session.ServerID)
			}
			s.log.Info("poll delivered envelopes",
				"serverId", session.ServerID, "sessionId", session.ID,
				"count", len(envelopes), "throughSeq", envelopes[len(envelopes)-1].Seq)
			respond(envelopes)
			return
		}

		remaining := time.Until(deadline)
		if remaining <= 0 || !mayHold || drain {
			release()
			respond(nil)
			return
		}

		timer := time.NewTimer(min(remaining, pollBackstop))
		if parkedAt.IsZero() {
			parkedAt = time.Now()
		}
		select {
		case <-signal:
		case <-timer.C:
		case <-r.Context().Done():
			// The plugin gave up on this request. Nothing was acked and nothing
			// was sent, so the next poll picks up exactly where this one was.
			*held = time.Since(parkedAt)
			timer.Stop()
			release()
			return
		}
		*held = time.Since(parkedAt)
		timer.Stop()
		release()

		// Revocation and supersession must break a held poll at once rather than
		// let it run to term (spec sections 3.1.2 and 5.4).
		refreshed, server, err := s.store.SessionByToken(r.Context(), sessionTokenHash)
		switch {
		case errors.Is(err, store.ErrNotFound):
			s.rejectSession(w, "session token is expired, unknown, or superseded")
			return
		case err != nil:
			s.writeInternalError(w, r, err)
			return
		}
		if server.RevokedAt != nil {
			s.rejectSession(w, "the credentials behind this session were revoked")
			return
		}
		inboundAck = max(inboundAck, refreshed.InboundAck)
		session = refreshed
	}
}
