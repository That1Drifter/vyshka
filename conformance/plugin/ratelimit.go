package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// The rate-limit stage (spec section 2.2.1). The mock refuses one poll that
// carries a fresh envelope with 429 rate_limited and a wait longer than the
// 1 s minimum backoff, so a plugin that treats the refusal as any other
// client error and retries after a second shows itself. A plugin that read
// the refusal waits what it was told, keeps its session, and sends the same
// envelopes again at the same seq; whatever it could read, it delivers them.

const (
	actionRateLimited      = "conformance-act-rate-limited"
	actionAfterRateLimited = "conformance-act-after-rate-limit"

	// rateLimitWaitSeconds is the Retry-After the mock names: two seconds,
	// so honoring it is distinguishable from the 1 s backoff of section 2.3.
	rateLimitWaitSeconds = 2
	// rateLimitWaitSlack is taken off the wait the stage demands, for timer
	// and scheduling granularity, as minBackoff does for the 1 s floor.
	rateLimitWaitSlack = 50 * time.Millisecond
)

// rateLimitRecord is what the refused poll carried and what came next.
type rateLimitRecord struct {
	// Refused are the fresh envelopes of the refused batch, id and seq.
	Refused []batchField
	At      time.Time
	Ordinal int
	GenAt   uint64
	Inline  bool
	// NextAt, NextOrdinal and NextCarried describe the first poll to arrive
	// after the refusal: when, on which session, and whether it carried every
	// refused envelope again at the seq it was refused at (traced, not
	// graded: section 9.3 lets them come back on a later poll).
	NextAt      time.Time
	NextOrdinal int
	NextCarried bool
}

func (h *mockHub) armRateLimit() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.rateLimitArmed = true
	h.rateLimited = nil
}

// refuseRateLimitedLocked answers the poll with the refusal of section 2.2.1
// and releases the lock. Nothing in the request is applied: not its ack, not
// its envelopes.
func (h *mockHub) refuseRateLimitedLocked(w http.ResponseWriter, inline bool, fresh []batchField) {
	h.rateLimitArmed = false
	refused := append([]batchField(nil), fresh...)
	h.rateLimited = &rateLimitRecord{
		Refused: refused, At: time.Now(), Ordinal: h.sessionOrdinal, GenAt: h.acceptGen, Inline: inline,
	}
	// What comes back must come back unchanged (section 9.1); ingest faults
	// a later arrival whose content moved.
	h.rememberContentLocked(refused, 0)
	trace("rate limiting a poll carrying %d fresh envelope(s) from seq %d", len(refused), refused[0].Seq)
	h.signalLocked()
	h.mu.Unlock()
	w.Header().Set("Retry-After", strconv.Itoa(rateLimitWaitSeconds))
	answer(w, inline, http.StatusTooManyRequests, "rate_limited",
		"this harness rate-limits one poll to grade recovery; nothing in it was applied",
		map[string]any{"retryAfterSeconds": rateLimitWaitSeconds})
}

// noteAfterRateLimitLocked records the first poll to arrive after the
// refusal. Call with the lock held, for every poll on a live session.
func (h *mockHub) noteAfterRateLimitLocked(raws []json.RawMessage) {
	record := h.rateLimited
	if record == nil || !record.NextAt.IsZero() {
		return
	}
	record.NextAt = time.Now()
	record.NextOrdinal = h.sessionOrdinal
	carried := map[string]int64{}
	for _, one := range batchIDs(raws) {
		carried[one.ID] = one.Seq
	}
	record.NextCarried = true
	for _, refused := range record.Refused {
		if seq, ok := carried[refused.ID]; !ok || seq != refused.Seq {
			record.NextCarried = false
		}
	}
}

func refusedIDs(record *rateLimitRecord) []string {
	ids := make([]string, 0, len(record.Refused))
	for _, one := range record.Refused {
		ids = append(ids, one.ID)
	}
	return ids
}

var rateLimitStage = Stage{
	ID:      "errors.rateLimited",
	Title:   "A rate-limited poll is waited out, and what it carried is delivered on the same session",
	Section: "2.2.1",
	Run: func(h *harness) error {
		hub := h.hub
		var ordinalBefore, enrollBefore int
		hub.view(func() {
			ordinalBefore = hub.sessionOrdinal
			enrollBefore = hub.enrollCount
		})
		overlapsBefore := hub.overlappingPolls.Load()

		hub.armRateLimit()
		h.dispatch(actionRateLimited)
		if err := hub.await(h.checkTimeout, "a poll carrying a fresh envelope to rate-limit", func() bool {
			return hub.rateLimited != nil
		}); err != nil {
			return fmt.Errorf("%w; harness error: the plugin sent nothing new after a dispatch, so no rate limit could be served", err)
		}
		var record rateLimitRecord
		hub.view(func() { record = *hub.rateLimited })
		ids := refusedIDs(&record)

		if err := hub.await(h.checkTimeout+slowRetryAllowance, "the rate-limited envelopes to be delivered", func() bool {
			return hub.allAcceptedAfterLocked(ids, record.GenAt)
		}); err != nil {
			return fmt.Errorf("%w; a rate_limited poll applied nothing, so every envelope it carried is still the plugin's to deliver (sections 2.2.1, 9.3)", err)
		}
		var ordinalAfter, enrollAfter int
		hub.view(func() {
			record = *hub.rateLimited
			ordinalAfter = hub.sessionOrdinal
			enrollAfter = hub.enrollCount
		})
		sequential := hub.overlappingPolls.Load() == overlapsBefore
		pause := record.NextAt.Sub(record.At)

		if enrollAfter != enrollBefore {
			return fmt.Errorf("the plugin re-enrolled over a rate_limited poll; it is not a credential failure (section 2.2.1)")
		}
		if record.Inline {
			// The plugin asked for inline errors, so it read the code and
			// details.retryAfterSeconds: it is held to the whole rule.
			if ordinalAfter != ordinalBefore {
				return fmt.Errorf("the plugin started %d new session(s) over a rate_limited poll it could read; a rate limit is not the loss of a session (section 2.2.1)", ordinalAfter-ordinalBefore)
			}
			if sequential && pause < rateLimitWaitSeconds*time.Second-rateLimitWaitSlack {
				return fmt.Errorf("the plugin polled again %s after a rate_limited refusal naming %d s; after a refused poll a plugin MUST NOT poll again before details.retryAfterSeconds has passed (section 2.2.1)", pause.Round(time.Millisecond), rateLimitWaitSeconds)
			}
			// What the refused poll carried stays unacked and comes back
			// under section 9.3, on the next poll or a later one; the mock
			// faults any retransmission whose content moved (section 9.1),
			// and delivery itself was awaited above.
		} else {
			// Without inline errors the mock cannot know whether the plugin
			// read the 429 (a full HTTP client can) or saw an opaque client
			// error (Appendix A), so it grades what both must do.
			if ordinalAfter > ordinalBefore+1 {
				return fmt.Errorf("the plugin started %d sessions over one refused poll; the opaque fallback allows one (Appendix A), and a plugin that read the refusal starts none (section 2.2.1)", ordinalAfter-ordinalBefore)
			}
			if sequential && record.NextOrdinal == record.Ordinal && pause < minBackoff {
				return fmt.Errorf("the plugin retried %s after a refused poll on the same session; a refused request is backed off for at least 1 s (section 2.3)", pause.Round(time.Millisecond))
			}
		}

		// Either way the link still works.
		h.dispatch(actionAfterRateLimited)
		if err := hub.await(h.resultBudget(), "a full round-trip after the rate-limited poll", func() bool {
			track := hub.actions[actionAfterRateLimited]
			return track != nil && track.results >= 1
		}); err != nil {
			return fmt.Errorf("%w; a rate-limited poll must leave the plugin able to execute the next action", err)
		}
		if !record.Inline {
			return ungraded{"the refusal did not travel inline, so the mock could not tell whether the plugin read it; graded only on delivering the refused envelopes, at most one new session, and the 1 s backoff, not on waiting out Retry-After or keeping its session"}
		}
		if !sequential {
			return ungraded{"the candidate had more than one poll in flight during this stage, so the wait before its retry and what that retry carried could not be graded (section 3.1 asks for one poll at a time); everything else passed"}
		}
		return nil
	},
}

func init() {
	stages = append(stages, rateLimitStage)
}
