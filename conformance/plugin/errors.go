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
	actionAfterErrors = "conformance-act-after-errors"

	// How long the credentials-refused stage keeps refusing. Long enough for
	// a hammering plugin to show itself, short enough not to wait out a
	// compliant plugin's slow retry more than once.
	refuseSessionsWindow = 5 * time.Second
	// The most session attempts a plugin may make inside that window before it
	// counts as hammering. A compliant plugin retries slowly (section 2.3);
	// the reference DayZ plugin waits 30 s, the reference driver 2 s doubling.
	maxSessionAttemptsInWindow = 4
)

func init() {
	stages = append(stages, errorStages...)
}

// armBatchRejection makes the next non-empty poll batch fail with
// envelope_invalid at index 0.
func (h *mockHub) armBatchRejection() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.rejectNextBatch = true
	h.rejected = nil
}

// refuseSessions answers every session request with credentials_revoked for
// the window, counting attempts from now.
func (h *mockHub) refuseSessions(window time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.refuseSessionsUntil = time.Now().Add(window)
	h.sessionAttempts = nil
}

func (h *mockHub) armGarble() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.garbleNextPoll = true
	h.garbled = false
}

// seenID reports whether an envelope with this id has ever arrived, in any
// session. Call with the lock held (inside view or await).
func (h *mockHub) seenIDLocked(id string) bool {
	_, seen := h.idContent[id]
	return seen
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
			err := hub.await(h.checkTimeout, "a non-empty batch to refuse", func() bool {
				return hub.rejected != nil
			})
			if err != nil {
				return fmt.Errorf("%w; harness error: the plugin sent nothing after a dispatch, so no refusal could be graded", err)
			}
			var rejection batchRejection
			hub.view(func() { rejection = *hub.rejected })

			if rejection.Inline {
				// The plugin could read envelope_invalid with details.index.
				// Everything else in the batch must come back, the condemned
				// envelope must not, and the session must be the same one.
				err = hub.await(h.checkTimeout, "the rest of the refused batch to be resent", func() bool {
					for _, id := range rejection.Others {
						if !hub.seenIDLocked(id) {
							return false
						}
					}
					return hub.totalPolls > 0 && hub.rejected != nil
				})
				if err != nil {
					return fmt.Errorf("%w; after envelope_invalid the plugin removes the envelope at details.index and resends the rest of the batch (section 2.3)", err)
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
				// An opaque 400. One new session is the legal fallback for a
				// client error on a poll; the mock accepts the resent batch,
				// so the plugin must then be back to normal with no further
				// session, and never re-enrolled.
				err = hub.await(h.checkTimeout, "the refused batch to be resent", func() bool {
					if !hub.seenIDLocked(rejection.ID) {
						return false
					}
					for _, id := range rejection.Others {
						if !hub.seenIDLocked(id) {
							return false
						}
					}
					return true
				})
				if err != nil {
					return fmt.Errorf("%w; nothing in a refused batch was applied, so a plugin that cannot read the refusal still has to deliver the batch once the hub accepts it (sections 3.1.2 and 9.3)", err)
				}
				if err := h.awaitMorePolls(2, "after the resent batch"); err != nil {
					return err
				}
				var ordinalAfter, enrollAfter int
				hub.view(func() {
					ordinalAfter = hub.sessionOrdinal
					enrollAfter = hub.enrollCount
				})
				if ordinalAfter > ordinalBefore+1 {
					return fmt.Errorf("the plugin opened %d sessions over one opaque client error; the fallback is one new session and then backoff, not a loop (Appendix A)", ordinalAfter-ordinalBefore)
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
			var ordinalBefore, enrollBefore, pollsBefore int
			hub.view(func() {
				ordinalBefore = hub.sessionOrdinal
				enrollBefore = hub.enrollCount
				pollsBefore = hub.totalPolls
			})
			hub.armGarble()
			err := hub.await(h.checkTimeout, "a poll to answer with garbage", func() bool { return hub.garbled })
			if err != nil {
				return err
			}
			err = hub.await(h.checkTimeout, "the plugin to poll again after the garbled answer", func() bool {
				return hub.totalPolls > pollsBefore
			})
			if err != nil {
				return fmt.Errorf("%w; a 200 whose body is not a JSON object is retried after backoff on the same session (section 2.3)", err)
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
			refusedUntil := time.Now().Add(refuseSessionsWindow)
			hub.killSession()

			err := hub.await(h.checkTimeout, "the plugin to ask for a new session", func() bool {
				return len(hub.sessionAttempts) >= 1
			})
			if err != nil {
				return fmt.Errorf("%w; session_invalid is answered by requesting a new session (section 5.3)", err)
			}
			if remaining := time.Until(refusedUntil); remaining > 0 {
				time.Sleep(remaining)
			}
			var attempts, enrollDuringWindow int
			hub.view(func() {
				attempts = len(hub.sessionAttempts)
				enrollDuringWindow = hub.enrollCount
			})
			if attempts > maxSessionAttemptsInWindow {
				return fmt.Errorf("%d session attempts in %s after credentials_revoked; revoked or invalid credentials are retried slowly, because no retry fixes them until the operator acts (section 2.3)", attempts, refuseSessionsWindow)
			}
			if enrollDuringWindow != enrollBefore {
				return fmt.Errorf("the plugin re-enrolled after credentials_revoked; only a fresh enrollment token from the operator does that (section 5.4)")
			}

			// The operator has not acted, but the refusal has lifted: the
			// plugin's slow retry must eventually get it a session again.
			// The budget allows for a plugin that waits half a minute.
			err = hub.await(h.checkTimeout+40*time.Second, "a new session once credentials are accepted again", func() bool {
				return hub.sessionLive && hub.sessionOrdinal > ordinalBefore
			})
			if err != nil {
				return fmt.Errorf("%w; a plugin that backs off after credentials_revoked still has to retry, or an operator's fix never takes effect", err)
			}
			return nil
		},
	},
}
