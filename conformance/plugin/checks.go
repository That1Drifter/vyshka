package main

import (
	"fmt"
	"time"
)

// Stage is one graded step. Unlike the hub suite's checks, stages are not
// independent: the candidate is a single long-lived process being walked
// through its lifecycle, so each stage builds on the state the previous ones
// established. A stage marked Fatal takes the rest of the run with it, because
// nothing after it could mean anything.
type Stage struct {
	ID    string
	Title string
	// Section cites the clause of spec/protocol.md the stage enforces.
	Section string
	Fatal   bool
	Run     func(h *harness) error
}

// harness carries the mock hub plus the state stages hand each other.
type harness struct {
	hub          *mockHub
	enrollWait   time.Duration
	checkTimeout time.Duration
	faultCursor  int

	// The action the manifest stage chose; every dispatch stage uses it.
	action manifestAction
	// The delivered dispatch envelope of the round-trip stage, kept so the
	// dedup stage can force its re-delivery verbatim.
	firstDispatch *outboundItem
}

const (
	actionRoundTrip = "conformance-act-roundtrip"
	actionOutage    = "conformance-act-outage"
	actionRenumber  = "conformance-act-renumber"
	actionInvalid   = "conformance-act-invalid"
	actionRecovery  = "conformance-act-recovery"
)

var stages = []Stage{
	{
		ID:      "enroll.exchange",
		Title:   "The plugin exchanges the one-time token for credentials",
		Section: "5.2",
		Fatal:   true,
		Run: func(h *harness) error {
			hub := h.hub
			err := hub.await(h.enrollWait, "the candidate to enroll", func() bool {
				return hub.enrollBurned
			})
			if err != nil {
				return fmt.Errorf("%w; the candidate must POST /plugin/v1/enroll with the printed enrollment token and a non-empty game (section 5.2)", err)
			}
			return nil
		},
	},
	{
		ID:      "session.start",
		Title:   "The plugin trades its credentials for a session",
		Section: "5.3",
		Fatal:   true,
		Run: func(h *harness) error {
			hub := h.hub
			err := hub.await(h.checkTimeout, "a session to start", func() bool {
				return hub.sessionOrdinal >= 1 && hub.sessionLive
			})
			if err != nil {
				return fmt.Errorf("%w; after enrolling, a plugin must POST /plugin/v1/session with the serverId and serverSecret it was issued (section 5.3)", err)
			}
			return nil
		},
	},
	{
		ID:      "poll.repolls",
		Title:   "The plugin long-polls and re-polls after each response",
		Section: "3.1",
		Fatal:   true,
		Run: func(h *harness) error {
			hub := h.hub
			var base int
			hub.view(func() { base = hub.pollsThisSession })
			err := hub.await(h.checkTimeout, "the plugin to poll twice", func() bool {
				return hub.pollsThisSession >= base+2
			})
			if err != nil {
				return fmt.Errorf("%w; a plugin drives everything through POST /plugin/v1/poll and re-polls after each response (section 3.1)", err)
			}
			return nil
		},
	},
	{
		ID:      "manifest.publish",
		Title:   "The plugin publishes a manifest declaring at least one action",
		Section: "6",
		Fatal:   true,
		Run: func(h *harness) error {
			hub := h.hub
			err := hub.await(h.checkTimeout, "a manifest.publish envelope", func() bool {
				return hub.manifest != nil
			})
			if err != nil {
				return fmt.Errorf("%w; a plugin sends manifest.publish at session start (section 6)", err)
			}
			var manifest manifestInfo
			hub.view(func() { manifest = *hub.manifest })
			if manifest.Revision < 1 || manifest.Revision >= 1<<53 {
				return fmt.Errorf("the manifest carries manifestRevision %d; revisions are integers in [1, 2^53) (section 6.1)", manifest.Revision)
			}
			if len(manifest.Actions) == 0 {
				return fmt.Errorf("the manifest declares no actions; this harness dispatches the first declared action to grade the lifecycle, so a candidate must declare at least one (section 6.1)")
			}
			// Grade what a conformant hub's section 6.4 validation would
			// refuse, so a candidate learns here rather than from the first
			// real hub it meets.
			seenCodes := map[string]bool{}
			for index, action := range manifest.Actions {
				if action.Code == "" {
					return fmt.Errorf("manifest action %d carries no code (section 6.1)", index)
				}
				if seenCodes[action.Code] {
					return fmt.Errorf("manifest action code %q is declared twice; a hub rejects the whole manifest over a duplicated code (section 6.4)", action.Code)
				}
				seenCodes[action.Code] = true
				if action.Params != nil {
					if err := validateSubset(action.Params, fmt.Sprintf("actions[%d].params", index)); err != nil {
						return fmt.Errorf("a hub would reject this manifest: %w", err)
					}
				}
			}
			h.action = manifest.Actions[0]
			return nil
		},
	},
	{
		ID:      "action.roundTrip",
		Title:   "A dispatched action is acked and answered with a result",
		Section: "7",
		Fatal:   true,
		Run: func(h *harness) error {
			hub := h.hub
			h.firstDispatch = h.dispatch(actionRoundTrip)

			err := hub.await(h.checkTimeout, "the plugin to ack the dispatch envelope", func() bool {
				return h.firstDispatch.acked
			})
			if err != nil {
				return fmt.Errorf("%w; the poll ack is the delivery receipt for an action.dispatch (section 7)", err)
			}
			err = hub.await(h.checkTimeout, "an action.ack", func() bool {
				track := hub.actions[actionRoundTrip]
				return track != nil && track.acks >= 1
			})
			if err != nil {
				return fmt.Errorf("%w; the plugin MUST send action.ack on receipt (section 7)", err)
			}
			err = hub.await(h.resultBudget(), "an action.result", func() bool {
				track := hub.actions[actionRoundTrip]
				return track != nil && track.results >= 1
			})
			if err != nil {
				return fmt.Errorf("%w; the plugin MUST send action.result when done, whatever the outcome (section 7)", err)
			}
			return nil
		},
	},
	{
		ID:      "action.redeliveryDedup",
		Title:   "A forced re-delivery of the same envelope is not executed again",
		Section: "9.1",
		Run: func(h *harness) error {
			hub := h.hub
			var before int
			hub.view(func() { before = hub.actions[actionRoundTrip].results })
			if err := hub.redeliver(h.firstDispatch); err != nil {
				return err
			}
			if err := h.awaitMorePolls(2, "after the forced re-delivery"); err != nil {
				return err
			}
			var after int
			hub.view(func() { after = hub.actions[actionRoundTrip].results })
			if after != before {
				return fmt.Errorf("the plugin sent %d new action.result envelope(s) for a redelivered envelope; an envelope at or below the ack is a duplicate, acknowledged again and processed no further (section 9.1)", after-before)
			}
			return nil
		},
	},
	{
		ID:      "action.idDedup",
		Title:   "A fresh envelope carrying an executed actionId is not executed again",
		Section: "9.2",
		Run: func(h *harness) error {
			hub := h.hub
			var before int
			hub.view(func() { before = hub.actions[actionRoundTrip].results })
			duplicate := h.dispatch(actionRoundTrip)
			err := hub.await(h.checkTimeout, "the plugin to ack the duplicate dispatch", func() bool {
				return duplicate.acked
			})
			if err != nil {
				return err
			}
			if err := h.awaitMorePolls(2, "after the duplicate actionId dispatch"); err != nil {
				return err
			}
			var after int
			hub.view(func() { after = hub.actions[actionRoundTrip].results })
			if after != before {
				return fmt.Errorf("the plugin sent another action.result when actionId %s arrived in a fresh envelope; a plugin keeps an LRU of executed action ids and never executes the same actionId twice (section 9.2), and to a black-box grader a repeated result is indistinguishable from a repeated execution, so a recognized duplicate is acked and answered with silence", actionRoundTrip)
			}
			return nil
		},
	},
	{
		ID:      "compat.unknownType",
		Title:   "An unknown envelope type is acked and ignored",
		Section: "4",
		Run: func(h *harness) error {
			hub := h.hub
			probe := hub.queueOutbound("conformance.unknown-type-1", map[string]any{"probe": true})
			err := hub.await(h.checkTimeout, "the plugin to ack an envelope of an unknown type", func() bool {
				return probe.acked
			})
			if err != nil {
				return fmt.Errorf("%w; unknown envelope types advance the ack like anything else, because an ack stalled on an unrecognized message blocks every message behind it (sections 4, 9.1)", err)
			}
			return nil
		},
	},
	{
		ID:      "outage.bufferAndFlush",
		Title:   "Envelopes unacked through an outage are buffered and flushed on reconnect",
		Section: "9.3",
		Run: func(h *harness) error {
			hub := h.hub

			// Freeze the ack, then dispatch: the plugin's ack and result for
			// this action stay unacked however often it retransmits them.
			hub.freezeAck()
			h.dispatch(actionOutage)
			err := hub.await(h.resultBudget(), "the action.result the harness will refuse to ack", func() bool {
				track := hub.actions[actionOutage]
				return track != nil && track.results >= 1
			})
			if err != nil {
				return err
			}
			var original inboundEnvelope
			hub.view(func() {
				for i := len(hub.inbound) - 1; i >= 0; i-- {
					e := hub.inbound[i]
					if e.Type == "action.result" && e.ActionID == actionOutage {
						original = *e
						break
					}
				}
			})

			// Sever the transport. Every request is met with a connection
			// reset, and the plugin must keep trying rather than give up.
			var abortedBefore int
			hub.view(func() { abortedBefore = hub.abortedRequests })
			hub.sever()
			err = hub.await(h.checkTimeout, "the plugin to keep retrying through the outage", func() bool {
				return hub.abortedRequests >= abortedBefore+2
			})
			if err != nil {
				hub.restore()
				return fmt.Errorf("%w; an aborted poll is not a session error, the plugin simply re-polls (section 3.1.1); a candidate whose reconnect backoff legitimately exceeds this window can raise -check-timeout", err)
			}
			restoredAt := time.Now()
			hub.restore()

			// The flush: the unacked result must come back, unchanged. Two
			// paths are legal. A plugin that keeps its session retransmits the
			// envelope with its old seq; a plugin that answers the outage by
			// starting a fresh session (legal at any time, section 5.3) sends
			// it renumbered into the new session's space instead.
			var renumbered *inboundEnvelope
			var firstFlushAt time.Time
			err = hub.await(h.checkTimeout, "the unacked envelope to be re-sent after the outage", func() bool {
				for _, r := range hub.retransmissions {
					if r.Session == original.Session && r.Seq == original.Seq && r.At.After(restoredAt) {
						firstFlushAt = r.At
						return true
					}
				}
				for _, e := range hub.inbound {
					if e.Session > original.Session && e.ID == original.ID {
						renumbered = e
						return true
					}
				}
				return false
			})
			if err != nil {
				hub.releaseAck()
				return fmt.Errorf("%w; envelope seq %d (the action.result for %s) was never re-sent, but nothing may be dropped before the hub acks it (section 9.3)", err, original.Seq, actionOutage)
			}
			if renumbered != nil && (renumbered.Type != original.Type || !tsEqual(renumbered.TS, original.TS) || !jsonEqual(renumbered.Body, original.Body)) {
				hub.releaseAck()
				return fmt.Errorf("the unacked envelope came back in a new session but changed; renumbering moves seq alone, keeping id, type, ts and body (section 9.1)")
			}

			// One retransmission proves flushing; retention until acked needs
			// more, because a plugin could send once and delete. The ack stays
			// frozen, so every poll response keeps reporting the envelope
			// unacked, and the plugin must send it again.
			if renumbered == nil {
				err = hub.await(h.checkTimeout, "a further retransmission while the envelope stays unacked", func() bool {
					for _, r := range hub.retransmissions {
						if r.Session == original.Session && r.Seq == original.Seq && r.At.After(firstFlushAt) {
							return true
						}
					}
					return false
				})
				if err != nil {
					hub.releaseAck()
					return fmt.Errorf("%w; the envelope was re-sent once and then never again, though every response reported it unacked; a plugin drops an envelope only when the hub's ack covers it (section 9.3)", err)
				}
			}

			// Let the buffer drain and the plugin see the ack, so the next
			// stage starts from a settled state.
			hub.releaseAck()
			return h.awaitMorePolls(2, "after the ack was released")
		},
	},
	{
		ID:      "session.renumber",
		Title:   "Envelopes unacked across a session change are renumbered, not replayed",
		Section: "9.1",
		Run: func(h *harness) error {
			hub := h.hub

			hub.freezeAck()
			h.dispatch(actionRenumber)
			err := hub.await(h.resultBudget(), "the action.result the session change will strand", func() bool {
				track := hub.actions[actionRenumber]
				return track != nil && track.results >= 1
			})
			if err != nil {
				return err
			}

			var oldOrdinal int
			hub.view(func() { oldOrdinal = hub.sessionOrdinal })
			expected := hub.killSession()
			if expected == 0 {
				return fmt.Errorf("harness error: the session ended with nothing unacked, so renumbering cannot be graded")
			}

			err = hub.await(h.checkTimeout, "a new session after session_invalid", func() bool {
				return hub.sessionOrdinal > oldOrdinal && hub.sessionLive
			})
			if err != nil {
				return fmt.Errorf("%w; a plugin answers session_invalid by requesting a new session with its stored credentials, never by re-enrolling (section 5.3)", err)
			}

			err = hub.await(h.checkTimeout, "every stranded envelope to arrive renumbered", func() bool {
				for _, expectation := range hub.expectedRenumber {
					if !expectation.arrived {
						return false
					}
				}
				return true
			})
			if err != nil {
				missing := ""
				hub.view(func() {
					for id, expectation := range hub.expectedRenumber {
						if !expectation.arrived {
							missing += fmt.Sprintf(" %s (type %s, old seq %d);", id, expectation.typ, expectation.oldSeq)
						}
					}
				})
				return fmt.Errorf("%w; still missing:%s an envelope unacked when its session ends is renumbered into the new session's sequence space, keeping its id, type, ts and body (section 9.1)", err, missing)
			}
			return nil
		},
	},
	{
		ID:      "dispatch.invalidTolerated",
		Title:   "A schema-invalid dispatch is survived, not fatal",
		Section: "7",
		Run: func(h *harness) error {
			hub := h.hub
			invalidParams, how := synthesizeInvalidParams(h.action.Params)
			invalid := hub.queueDispatch(actionInvalid, h.action, invalidParams, h.checkTimeout)
			err := hub.await(h.checkTimeout, "the plugin to ack the schema-invalid dispatch", func() bool {
				return invalid.acked
			})
			if err != nil {
				return fmt.Errorf("%w; the dispatch params payload %s, and a body the plugin cannot use is acked and ignored rather than allowed to wedge the session (section 7)", err, how)
			}

			// The real assertion: the plugin is still alive and still executes.
			h.dispatch(actionRecovery)
			err = hub.await(h.resultBudget(), "a full round-trip after the schema-invalid dispatch", func() bool {
				track := hub.actions[actionRecovery]
				return track != nil && track.results >= 1
			})
			if err != nil {
				return fmt.Errorf("%w; a schema-invalid dispatch must never crash the game server side: the plugin has to keep executing after one (issue #9, section 7)", err)
			}
			return nil
		},
	},
}

// dispatch queues the chosen action with valid synthesized params. The body's
// expiresAt matches the harness's own patience, so a plugin is never failed
// over a result it was told it still had time to deliver.
func (h *harness) dispatch(actionID string) *outboundItem {
	return h.hub.queueDispatch(actionID, h.action, synthesizeParams(h.action.Params), h.checkTimeout)
}

// resultBudget is the patience for an action.result: the deadline the plugin
// was given, plus margin for the poll carrying the answer.
func (h *harness) resultBudget() time.Duration {
	return h.checkTimeout + 5*time.Second
}

// awaitMorePolls waits for the plugin to complete count further polls, which
// is the evidence that it is alive and has had the chance to misbehave.
func (h *harness) awaitMorePolls(count int, context string) error {
	hub := h.hub
	var base int
	hub.view(func() { base = hub.totalPolls })
	err := hub.await(h.checkTimeout, fmt.Sprintf("%d more poll(s) %s", count, context), func() bool {
		return hub.totalPolls >= base+count
	})
	if err != nil {
		return fmt.Errorf("%w; the plugin stopped polling (section 3.1)", err)
	}
	return nil
}

// Result is one graded stage, in the same shape the hub suite reports.
type Result struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Section    string `json:"section"`
	Passed     bool   `json:"passed"`
	Error      string `json:"error,omitempty"`
	DurationMs int64  `json:"durationMs"`
}

func runStages(h *harness) []Result {
	results := make([]Result, 0, len(stages))
	fatal := ""
	for _, stage := range stages {
		result := Result{ID: stage.ID, Title: stage.Title, Section: stage.Section}
		if fatal != "" {
			result.Error = "prerequisite failed: " + fatal
			results = append(results, result)
			continue
		}

		start := time.Now()
		err := stage.Run(h)
		// Protocol faults the mock hub recorded while this stage ran fail the
		// stage even when its own assertions passed.
		if faultErr := h.hub.consumeFaults(&h.faultCursor); faultErr != nil {
			if err == nil {
				err = faultErr
			} else {
				err = fmt.Errorf("%v; also %v", err, faultErr)
			}
		}
		result.DurationMs = time.Since(start).Milliseconds()
		result.Passed = err == nil
		if err != nil {
			result.Error = err.Error()
			if stage.Fatal {
				fatal = stage.ID
			}
		}
		results = append(results, result)
	}
	return results
}
