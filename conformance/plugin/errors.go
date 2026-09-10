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
	// How long a plugin that backs off after an opaque refusal, or after a
	// credential refusal, is given to come back. The reference DayZ plugin
	// waits 30 s in both cases; a candidate that waits longer can raise
	// -check-timeout.
	slowRetryAllowance = 40 * time.Second
)

func init() {
	stages = append(stages, errorStages...)
}

// armBatchRejection makes the next poll batch whose first envelope is fresh
// fail with envelope_invalid at index 0.
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

// seenIDLocked reports whether an envelope with this id has ever been
// accepted, in any session. Call with the lock held (inside view or await).
func (h *mockHub) seenIDLocked(id string) bool {
	_, seen := h.idContent[id]
	return seen
}

func (h *mockHub) allSeenLocked(ids []string) bool {
	for _, id := range ids {
		if !h.seenIDLocked(id) {
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
			err := hub.await(h.checkTimeout, "a batch with a fresh first envelope to refuse", func() bool {
				return hub.rejected != nil
			})
			if err != nil {
				return fmt.Errorf("%w; harness error: the plugin sent nothing new after a dispatch, so no refusal could be graded", err)
			}
			var rejection batchRejection
			hub.view(func() { rejection = *hub.rejected })

			if rejection.Inline {
				// The plugin could read envelope_invalid with details.index.
				// Everything else in the batch must come back, the condemned
				// envelope must not, and the session must be the same one.
				// Nothing in the batch had been accepted before (the mock only
				// condemns a fresh first envelope), so "seen" means "resent".
				if len(rejection.Others) > 0 {
					err = hub.await(h.checkTimeout, "the rest of the refused batch to be resent", func() bool {
						return hub.allSeenLocked(rejection.Others)
					})
					if err != nil {
						return fmt.Errorf("%w; after envelope_invalid the plugin removes the envelope at details.index and resends the rest of the batch (section 2.3)", err)
					}
				}
				// Give a plugin that resends the condemned envelope time to do so.
				if err := h.awaitMorePolls(2, "after the corrected batch"); err != nil {
					return err
				}
				var condemnedBack bool
				var ordinalAfter, enrollAfter int
				hub.view(func() {
					condemnedBack = hub.seenIDLocked(rejection.ID)
					ordinalAfter = hub.sessionOrdinal
					enrollAfter = hub.enrollCount
				})
				if condemnedBack {
					return fmt.Errorf("the envelope the hub refused (%s, type %s) was sent again; a plugin sets it aside and surfaces it rather than resending what the hub will refuse forever (section 2.3)", rejection.ID, rejection.Type)
				}
				if ordinalAfter != ordinalBefore {
					return fmt.Errorf("the plugin started a new session over a 400; a client error other than 401 is never answered with a new session (section 2.3)")
				}
				if enrollAfter != enrollBefore {
					return fmt.Errorf("the plugin re-enrolled over a refused batch; enrollment is never the answer to a poll refusal (section 5.3)")
				}
			} else {
				// An opaque 400, refused three times in all (the batch and two
				// more polls carrying the condemned envelope), so a plugin that
				// answers every opaque client error with a new session opens
				// three of them. The fallback (Appendix A) allows one new
				// session for the first refusal and demands backoff on the
				// next, so a compliant plugin opens at most two, and the mock
				// then accepts the batch.
				err = hub.await(h.checkTimeout+slowRetryAllowance, "the refused batch to be accepted after the refusals", func() bool {
					return hub.rejectRemaining == 0 && hub.seenIDLocked(rejection.ID) && hub.allSeenLocked(rejection.Others)
				})
				if err != nil {
					return fmt.Errorf("%w; nothing in a refused batch was applied, so a plugin that cannot read the refusal still has to deliver the batch once the hub accepts it (sections 3.1.2 and 9.3); a candidate whose backoff exceeds this window can raise -check-timeout", err)
				}
				var ordinalAfter, enrollAfter, refusals int
				hub.view(func() {
					ordinalAfter = hub.sessionOrdinal
					enrollAfter = hub.enrollCount
					refusals = hub.rejected.Refusals
				})
				if ordinalAfter > ordinalBefore+2 {
					return fmt.Errorf("the plugin opened %d sessions over %d opaque client errors on its batch; the fallback is one new session and then backoff, not a session per refusal (Appendix A)", ordinalAfter-ordinalBefore, refusals)
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
			err := hub.await(h.checkTimeout, "a poll carrying envelopes to answer with garbage", func() bool {
				return hub.garbled != nil
			})
			if err != nil {
				return fmt.Errorf("%w; harness error: the plugin sent nothing after a dispatch, so no garbled answer could be served", err)
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
				return hub.allSeenLocked(garbled.IDs)
			})
			if err != nil {
				return fmt.Errorf("%w; the garbled answer acked nothing, so the plugin must still deliver every envelope it carried (section 9.3)", err)
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
