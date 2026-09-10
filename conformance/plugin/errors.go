package main

import (
	"fmt"
	"time"
)

// Error recovery stages (spec section 2.3). They run last, after the
// candidate has proven it can round-trip an action, and each one provokes a
// refusal the way a real hub would raise it, then watches what the candidate
// does about it. The rule they grade is the one the section ends on: a client
// error is never answered by churning sessions.
//
// Two kinds of candidate reach these stages. One asked for inline errors on
// its polls and can read every refusal; it is held to the full recovery
// table. The other did not (or the mock is running in -legacy-errors mode to
// stand in for an older hub), so a 400 and a 401 look the same to it; it is
// held to the fallback the appendix describes: one new session, then
// backoff, and no re-enrollment ever.
//
// Every "did it arrive" question below is asked about arrivals after the
// provocation, never about the mock's lifetime history: an id the plugin had
// legitimately delivered before the stage began must not count as a resend.

const (
	actionRejected    = "conformance-act-rejected"
	actionGarbled     = "conformance-act-garbled"
	actionAfterErrors = "conformance-act-after-errors"

	// How long the credentials-refused stage keeps refusing, counted from the
	// plugin's first attempt. Long enough for a hammering plugin to show
	// itself, short enough not to wait out a compliant plugin's slow retry
	// more than once.
	refuseSessionsWindow = 5 * time.Second
	// The most session attempts a plugin may make inside that window before it
	// counts as hammering. A compliant plugin retries slowly (section 2.3);
	// the reference DayZ plugin waits 30 s, the reference driver 2 s doubling.
	maxSessionAttemptsInWindow = 4
	// How long a plugin that backs off after opaque refusals, or after a
	// credential refusal, is given to come back, on top of -check-timeout.
	// The reference DayZ plugin waits 30 s before each retry on a fresh
	// session and meets two such refusals in the opaque stage, so it needs
	// a little over a minute plus session overhead; a candidate that waits
	// longer can raise -check-timeout.
	slowRetryAllowance = 90 * time.Second
	// The least a plugin must wait before its next attempt after a refusal
	// it backs off from: section 2.3 makes 1 s normative. Measured between
	// the refusal and the next request on the same session, less a small
	// allowance for timer and scheduling granularity.
	minBackoff = 950 * time.Millisecond
)

func init() {
	stages = append(stages, errorStages...)
}

// armBatchRejection makes the next poll batch that carries a fresh envelope
// fail with envelope_invalid at that envelope's index.
func (h *mockHub) armBatchRejection() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.rejectArmed = true
	h.rejectRemaining = 0
	h.rejected = nil
}

// refuseSessions arms a window, opening on the plugin's next session
// request, in which every session request is answered credentials_revoked.
func (h *mockHub) refuseSessions(window time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.refuseSessionsArmed = true
	h.refuseSessionsFor = window
	h.refuseSessionsUntil = time.Time{}
	h.sessionAttempts = nil
	h.refusedSessions = 0
}

func (h *mockHub) armGarble() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.garbleArmed = true
	h.garbled = nil
}

// acceptedAfterLocked reports whether an envelope with this id was accepted
// as a fresh envelope, in any session, after the provocation that recorded
// the given acceptance generation. Generations order events exactly where a
// clock cannot: on Windows the monotonic clock advances in ticks of up to
// 15 ms, and a plugin on loopback can resend inside the tick that refused it.
// Call with the lock held (inside view or await).
func (h *mockHub) acceptedAfterLocked(id string, gen uint64) bool {
	at, seen := h.lastAccepted[id]
	return seen && at > gen
}

func (h *mockHub) allAcceptedAfterLocked(ids []string, gen uint64) bool {
	for _, id := range ids {
		if !h.acceptedAfterLocked(id, gen) {
			return false
		}
	}
	return true
}

var errorStages = []Stage{
	{
		ID:      "errors.batchRefused",
		Title:   "A refused batch is corrected, not answered with a session loop",
		Section: "2.3",
		Run: func(h *harness) error {
			hub := h.hub
			var ordinalBefore, enrollBefore int
			hub.view(func() {
				ordinalBefore = hub.sessionOrdinal
				enrollBefore = hub.enrollCount
			})

			hub.armBatchRejection()
			h.dispatch(actionRejected)
			err := hub.await(h.checkTimeout, "a batch carrying a fresh envelope to refuse", func() bool {
				return hub.rejected != nil
			})
			if err != nil {
				return fmt.Errorf("%w; harness error: the plugin sent nothing new after a dispatch, so no refusal could be graded", err)
			}
			var rejection batchRejection
			hub.view(func() { rejection = *hub.rejected })

			// Two recoveries are legal, and which one a plugin takes depends
			// on what it could read of the refusal, which the mock cannot
			// know from the outside (a plugin may read an ordinary 400 body
			// perfectly well without ever opting in). So the stage watches
			// for either and grades the one it sees.
			//
			// Setting aside: every other fresh envelope of the batch comes
			// back, the condemned one never does, and the plugin keeps
			// polling. Resending: the plugin could not tell the condemned
			// envelope apart, sends it again, meets the further refusals the
			// mock has ready, and delivers the whole batch once the mock
			// relents.
			setAside := func() bool {
				return hub.allAcceptedAfterLocked(rejection.Others, rejection.GenAt) &&
					!hub.acceptedAfterLocked(rejection.ID, rejection.GenAt) &&
					hub.totalPolls >= rejection.PollsAt+2
			}
			resent := func() bool {
				return hub.rejectRemaining == 0 && hub.acceptedAfterLocked(rejection.ID, rejection.GenAt) &&
					hub.allAcceptedAfterLocked(rejection.Others, rejection.GenAt)
			}
			err = hub.await(h.checkTimeout+slowRetryAllowance, "the plugin to recover from the refused batch", func() bool {
				return setAside() || resent()
			})
			if err != nil {
				var othersBack, condemnedBack bool
				var pollsSince, remaining int
				hub.view(func() {
					othersBack = hub.allAcceptedAfterLocked(rejection.Others, rejection.GenAt)
					condemnedBack = hub.acceptedAfterLocked(rejection.ID, rejection.GenAt)
					pollsSince = hub.totalPolls - rejection.PollsAt
					remaining = hub.rejectRemaining
				})
				return fmt.Errorf("%w (rest of batch %v resent: %v, condemned envelope %s resent: %v, accepted polls since: %d, refusals still pending: %d); after envelope_invalid a plugin that can read the refusal removes the envelope at details.index and resends the rest, and one that cannot still has to deliver the batch once the hub accepts it (sections 2.3, 3.1.2 and 9.3); a candidate whose backoff exceeds this window can raise -check-timeout",
					err, rejection.Others, othersBack, rejection.ID, condemnedBack, pollsSince, remaining)
			}
			var tookResend bool
			var refusals int
			hub.view(func() {
				tookResend = resent()
				refusals = hub.rejected.Refusals
			})
			if rejection.Inline && refusals > 1 {
				// The refusal travelled inline because the plugin asked for
				// it, so it could read details.index; sending the condemned
				// envelope again, even once before setting it aside, is not
				// a recovery for such a plugin, it is ignoring the refusal.
				return fmt.Errorf("the plugin asked for inline errors, received envelope_invalid naming index %d, and sent the envelope again %d time(s) instead of setting it aside at once (section 2.3)", rejection.Index, refusals-1)
			}

			if !tookResend {
				// Give a plugin that resends the condemned envelope late time
				// to show it.
				if err := h.awaitMorePolls(2, "after the corrected batch"); err != nil {
					return err
				}
				var condemnedBack bool
				var ordinalAfter, enrollAfter int
				hub.view(func() {
					condemnedBack = hub.acceptedAfterLocked(rejection.ID, rejection.GenAt)
					ordinalAfter = hub.sessionOrdinal
					enrollAfter = hub.enrollCount
				})
				if condemnedBack {
					return fmt.Errorf("the envelope the hub refused (%s, type %s, index %d) was sent again; a plugin sets it aside and surfaces it rather than resending what the hub will refuse forever (section 2.3)", rejection.ID, rejection.Type, rejection.Index)
				}
				// Section 2.3 lets a plugin that cannot establish the safety of
				// closing the gap start one new session instead; more than one
				// is churn.
				if ordinalAfter > ordinalBefore+1 {
					return fmt.Errorf("the plugin started %d sessions over a 400; a client error other than 401 is never answered with a session loop (section 2.3)", ordinalAfter-ordinalBefore)
				}
				if enrollAfter != enrollBefore {
					return fmt.Errorf("the plugin re-enrolled over a refused batch; enrollment is never the answer to a poll refusal (section 5.3)")
				}
			} else {
				// Refused four times in all (the batch and three more polls
				// carrying the condemned envelope). The fallback (Appendix A)
				// allows one new session for the first refusal and demands
				// backoff after a refusal on a session that has never polled,
				// whatever the plugin does next, so a compliant plugin opens
				// at most two sessions and leaves a real pause after each
				// such refusal.
				var ordinalAfter, enrollAfter int
				var events []refusalEvent
				var accepted *refusalEvent
				sessionStarts := map[int]time.Time{}
				hub.view(func() {
					ordinalAfter = hub.sessionOrdinal
					enrollAfter = hub.enrollCount
					events = append([]refusalEvent(nil), hub.rejected.Events...)
					if hub.rejected.Accepted != nil {
						copied := *hub.rejected.Accepted
						accepted = &copied
					}
					for ordinal, at := range hub.sessionStarts {
						sessionStarts[ordinal] = at
					}
				})
				if ordinalAfter > ordinalBefore+2 {
					return fmt.Errorf("the plugin opened %d sessions over %d opaque client errors on its batch; the fallback is one new session and then backoff, not a session per refusal (Appendix A)", ordinalAfter-ordinalBefore, len(events))
				}
				// What may follow a refusal without a pause is one thing only:
				// a new session after a refusal on a session that had polled
				// (the first branch of the fallback, bounded by the count
				// above). Everything else is a retry the plugin backs off
				// from: another attempt on the same session, or, after a
				// refusal on a session that had never polled, any attempt at
				// all, a replacement session included. Section 2.3 makes the
				// wait at least 1 s.
				for i, refusal := range events {
					var next *refusalEvent
					if i+1 < len(events) {
						next = &events[i+1]
					} else if accepted != nil {
						next = accepted
					}
					if next == nil {
						continue
					}
					sameSession := next.Ordinal == refusal.Ordinal
					if !sameSession && refusal.Polled {
						continue
					}
					nextAt := next.At
					if !sameSession {
						// The replacement session's own start is the attempt.
						if at, known := sessionStarts[next.Ordinal]; known {
							nextAt = at
						}
					}
					if pause := nextAt.Sub(refusal.At); pause < minBackoff {
						if sameSession {
							return fmt.Errorf("the plugin retried %s after a refused poll on the same session; a plugin that backs off waits at least 1 s before its next attempt at the same request (section 2.3)", pause)
						}
						return fmt.Errorf("the plugin opened another session %s after a refused poll on a session that had never polled; that refusal is backed off for at least 1 s, not answered with yet another session (section 2.3, Appendix A)", pause)
					}
				}
				if enrollAfter != enrollBefore {
					return fmt.Errorf("the plugin re-enrolled over a refused poll; enrollment is never the answer to a poll refusal (section 5.3)")
				}
			}

			// Either way the link still works.
			h.dispatch(actionAfterErrors)
			err = hub.await(h.resultBudget(), "a full round-trip after the refused batch", func() bool {
				track := hub.actions[actionAfterErrors]
				return track != nil && track.results >= 1
			})
			if err != nil {
				return fmt.Errorf("%w; a refused batch must leave the plugin able to execute the next action", err)
			}
			return nil
		},
	},
	{
		ID:      "errors.garbledSuccess",
		Title:   "A 200 that is not JSON changes no session or delivery state",
		Section: "2.3",
		Run: func(h *harness) error {
			hub := h.hub
			var ordinalBefore, enrollBefore int
			hub.view(func() {
				ordinalBefore = hub.sessionOrdinal
				enrollBefore = hub.enrollCount
			})

			// The garbled answer swallows a batch the plugin is waiting on an
			// ack for, so the stage can see both that the plugin polls again
			// and that it still delivers what the garbage did not acknowledge.
			hub.armGarble()
			h.dispatch(actionGarbled)
			err := hub.await(h.checkTimeout, "a poll carrying a fresh envelope to answer with garbage", func() bool {
				return hub.garbled != nil
			})
			if err != nil {
				return fmt.Errorf("%w; harness error: the plugin sent nothing new after a dispatch, so no garbled answer could be served", err)
			}
			var garbled garbleRecord
			hub.view(func() { garbled = *hub.garbled })

			err = hub.await(h.checkTimeout, "the plugin to poll again after the garbled answer", func() bool {
				return hub.totalPolls > garbled.PollsAt
			})
			if err != nil {
				return fmt.Errorf("%w; a 200 whose body is not a JSON object is retried after backoff on the same session (section 2.3)", err)
			}
			err = hub.await(h.checkTimeout, "the batch the garbled answer swallowed to be sent again", func() bool {
				return hub.allAcceptedAfterLocked(garbled.IDs, garbled.GenAt)
			})
			if err != nil {
				return fmt.Errorf("%w; the garbled answer acked nothing, so the plugin must still deliver every envelope it carried (section 9.3)", err)
			}
			// The pause is measured to the retry itself, the poll that carried
			// a swallowed envelope again, not to whatever poll the plugin may
			// already have had in flight.
			var retryAt time.Time
			hub.view(func() { retryAt = hub.garbled.RetryAt })
			if pause := retryAt.Sub(garbled.At); pause < minBackoff {
				return fmt.Errorf("the plugin sent the swallowed batch again %s after a malformed 200; a malformed response is retried after backoff, at least 1 s (section 2.3)", pause)
			}
			var ordinalAfter, enrollAfter int
			hub.view(func() {
				ordinalAfter = hub.sessionOrdinal
				enrollAfter = hub.enrollCount
			})
			if ordinalAfter != ordinalBefore {
				return fmt.Errorf("the plugin started a new session over a malformed 200; a malformed response changes no session state (section 2.3)")
			}
			if enrollAfter != enrollBefore {
				return fmt.Errorf("the plugin re-enrolled over a malformed 200")
			}
			return nil
		},
	},
	{
		ID:      "errors.credentialsRefused",
		Title:   "Revoked credentials are retried slowly, never by re-enrolling",
		Section: "2.3",
		Run: func(h *harness) error {
			hub := h.hub
			var ordinalBefore, enrollBefore int
			hub.view(func() {
				ordinalBefore = hub.sessionOrdinal
				enrollBefore = hub.enrollCount
			})

			hub.refuseSessions(refuseSessionsWindow)
			hub.killSession()

			// The window opens on the plugin's first attempt, so the refusal
			// is delivered whatever the plugin's own delay after
			// session_invalid.
			err := hub.await(h.checkTimeout+slowRetryAllowance, "the plugin to ask for a new session and be refused", func() bool {
				return hub.refusedSessions >= 1
			})
			if err != nil {
				return fmt.Errorf("%w; session_invalid is answered by requesting a new session (section 5.3)", err)
			}
			var firstAttempt time.Time
			hub.view(func() { firstAttempt = hub.sessionAttempts[0] })
			if remaining := time.Until(firstAttempt.Add(refuseSessionsWindow)); remaining > 0 {
				time.Sleep(remaining)
			}
			var attempts, refused, enrollDuringWindow int
			hub.view(func() {
				for _, at := range hub.sessionAttempts {
					if !at.After(firstAttempt.Add(refuseSessionsWindow)) {
						attempts++
					}
				}
				refused = hub.refusedSessions
				enrollDuringWindow = hub.enrollCount
			})
			if refused < 1 {
				return fmt.Errorf("harness error: no credentials_revoked was delivered, so nothing was graded")
			}
			if attempts > maxSessionAttemptsInWindow {
				return fmt.Errorf("%d session attempts in the %s after credentials_revoked; revoked or invalid credentials are retried slowly, because no retry fixes them until the operator acts (section 2.3)", attempts, refuseSessionsWindow)
			}
			if enrollDuringWindow != enrollBefore {
				return fmt.Errorf("the plugin re-enrolled after credentials_revoked; only a fresh enrollment token from the operator does that (section 5.4)")
			}

			// The operator has not acted, but the refusal has lifted: the
			// plugin's slow retry must eventually get it a session again.
			err = hub.await(h.checkTimeout+slowRetryAllowance, "a new session once credentials are accepted again", func() bool {
				return hub.sessionLive && hub.sessionOrdinal > ordinalBefore
			})
			if err != nil {
				return fmt.Errorf("%w; a plugin that backs off after credentials_revoked still has to retry, or an operator's fix never takes effect; a candidate that waits longer than %s can raise -check-timeout", err, h.checkTimeout+slowRetryAllowance)
			}
			return nil
		},
	},
}
