package hub

import (
	"errors"
	"net/http"
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
	session, server, ok := s.authenticateSession(w, r)
	if !ok {
		return
	}
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
	// action.result, and event.batch on the inbound path; every other accepted
	// envelope takes the forward-compatibility path of spec section 4: acked
	// and ignored. Bodies are validated up front because validity depends only
	// on content, while which envelopes are newly accepted is only known inside
	// the transaction.
	manifests := prepareManifests(request.Envelopes)
	actions := prepareActions(request.Envelopes)
	events := s.prepareEvents(request.Envelopes, time.Now().UTC())

	// The classification runs inside the store's transaction against the ack as
	// committed, not against the copy this request authenticated with, so two
	// overlapping polls cannot report acks that disagree. Manifest applies and
	// rejection notices ride the same transaction: acking an envelope promises
	// its effect is already durable (spec section 9.3).
	var batch inboundBatch
	var unusableActionBodies, rejectedManifests, refusedEventBatches, suppressedNotices int
	applied, err := s.store.ApplyInbound(r.Context(), session.ID, func(ack int64) store.InboundApplication {
		batch = classifyInbound(ack, request.Envelopes)
		unusableActionBodies = 0
		rejectedManifests = 0
		refusedEventBatches = 0
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
				case len(prepared.events) > eventBudget:
					refuse(newEventBudgetReject(request.Envelopes[index].ID))
				default:
					eventBudget -= len(prepared.events)
					application.Events = append(application.Events, prepared.events...)
				}
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
	if len(request.Envelopes) > 0 {
		s.log.Info("poll ingested envelopes",
			"serverId", server.ID, "sessionId", session.ID,
			"accepted", len(batch.Accepted), "duplicate", batch.Duplicate,
			"gapped", batch.Gapped, "ack", inboundAck,
			"manifestsApplied", applied.ManifestsApplied,
			"rejectsQueued", applied.NoticesQueued,
			"actionsStarted", applied.ActionsStarted,
			"actionsFinished", applied.ActionsFinished,
			"eventsStored", applied.EventsStored)
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
		s.nudgeWebhooks()
	}

	if err := s.store.TouchServer(r.Context(), server.ID); err != nil {
		s.writeInternalError(w, r, err)
		return
	}

	s.holdPoll(w, r, session, sessionTokenHash, inboundAck)
}

// holdPoll answers as soon as there is anything to send, and otherwise holds the
// request until the negotiated timeout, the session's expiry, or the session
// being ended out from under it.
//
// The ack it reports is refreshed from committed state on every database read,
// never left at the value this request ingested: a concurrent poll may commit a
// higher ack and be answered while this one is still held, and an ack the hub
// has reported must never be lowered by a later response (spec section 9.1).
func (s *Server) holdPoll(w http.ResponseWriter, r *http.Request, session store.Session, sessionTokenHash string, inboundAck int64) {
	pollTimeout := clampPollTimeout(time.Duration(session.PollTimeoutSeconds) * time.Second)
	deadline := time.Now().Add(pollTimeout)
	// A hold that outlives its own session would answer 401 late; ending it at
	// the expiry lets the plugin renew and re-poll instead.
	if session.ExpiresAt.Before(deadline) {
		deadline = session.ExpiresAt
	}

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

		queued, _, committedAck, err := s.store.NextOutbound(r.Context(), session.ID, session.ServerID, maxOutboundBatch)
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
		if len(queued) > 0 {
			release()
			envelopes := make([]envelope, len(queued))
			for i, one := range queued {
				envelopes[i] = newOutboundEnvelope(one)
			}
			s.log.Info("poll delivered envelopes",
				"serverId", session.ServerID, "sessionId", session.ID,
				"count", len(envelopes), "throughSeq", envelopes[len(envelopes)-1].Seq)
			respond(envelopes)
			return
		}

		remaining := time.Until(deadline)
		if remaining <= 0 || !mayHold {
			release()
			respond(nil)
			return
		}

		timer := time.NewTimer(min(remaining, pollBackstop))
		select {
		case <-signal:
		case <-timer.C:
		case <-r.Context().Done():
			// The plugin gave up on this request. Nothing was acked and nothing
			// was sent, so the next poll picks up exactly where this one was.
			timer.Stop()
			release()
			return
		}
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
