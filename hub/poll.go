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

type pollRequest struct {
	// Ack is the highest contiguous hub -> plugin seq the plugin has durably
	// processed. Absent or 0 acks nothing.
	Ack       int64      `json:"ack"`
	Envelopes []envelope `json:"envelopes"`
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

	// Then the plugin's envelopes. Nothing in this slice models a type, so every
	// accepted envelope takes the forward-compatibility path of spec section 4:
	// acked and ignored. Type handlers hook in here as their slices land.
	batch := classifyInbound(session.InboundAck, request.Envelopes)
	if err := s.store.RecordInbound(r.Context(), session.ID, batch.Ack, len(batch.Accepted)); err != nil {
		s.writeInternalError(w, r, err)
		return
	}
	if len(request.Envelopes) > 0 {
		s.log.Info("poll ingested envelopes",
			"serverId", server.ID, "sessionId", session.ID,
			"accepted", len(batch.Accepted), "duplicate", batch.Duplicate,
			"gapped", batch.Gapped, "ack", batch.Ack)
	}

	if err := s.store.TouchServer(r.Context(), server.ID); err != nil {
		s.writeInternalError(w, r, err)
		return
	}

	s.holdPoll(w, r, session, sessionTokenHash, batch.Ack)
}

// holdPoll answers as soon as there is anything to send, and otherwise holds the
// request until the negotiated timeout, the session's expiry, or the session
// being ended out from under it.
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

	for {
		// Registered before the read, so work queued between the read and the
		// select still wakes this poll instead of waiting for the backstop.
		signal, release := s.waiters.wait(session.ServerID)

		queued, _, err := s.store.NextOutbound(r.Context(), session.ID, session.ServerID, maxOutboundBatch)
		if err != nil {
			release()
			if errors.Is(err, store.ErrNotFound) {
				s.rejectSession(w, "session token is expired, unknown, or superseded")
				return
			}
			s.writeInternalError(w, r, err)
			return
		}
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
		if remaining <= 0 {
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
		session = refreshed
	}
}
