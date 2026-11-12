package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// The byte bounds of sections 2.1 and 4. Every Plugin API answer is graded
// against the first in Env.send; the checks here push answers toward it.
const (
	pluginResponseBound = 524288
	envelopeBodyBound   = 262144
)

var boundChecks = []Check{
	{
		ID:      "plugin.poll.byteBound",
		Title:   "A queue larger than one response drains over several polls, each answered at once and inside the bound",
		Section: "2.1, 3.1.2",
		Run:     checkPollByteBound,
	},
	{
		ID:      "admin.envelopes.bodyBound",
		Title:   "An envelope or dispatch whose body passes 262144 bytes is refused with 413 and nothing is queued",
		Section: "4, 5.5, 7",
		Run:     checkEnvelopeBodyBound,
	},
	{
		ID:      "plugin.kv.byteBound",
		Title:   "A small, deeply nested key/value value reads back through the Plugin API inside the bound",
		Section: "2.1, 12.1",
		Run:     checkKVByteBound,
	},
	{
		ID:      "plugin.bans.byteBound",
		Title:   "A ban-list page ends early to stay inside the bound, and the walk still reads every entry",
		Section: "2.1, 13.3",
		Run:     checkBanPageByteBound,
	},
}

func init() {
	checks = append(checks, boundChecks...)
}

// boundPadding is a body member long enough that four envelopes carrying it
// cannot share one response, yet short enough that any encoding of one
// stays under the body bound.
const boundPadding = 200000

func checkPollByteBound(ctx context.Context, env Env) error {
	plugin, err := env.newFakePlugin(ctx, "conformance: poll byte bound", shortPollTimeoutSeconds)
	if err != nil {
		return err
	}
	pad := strings.Repeat("a", boundPadding)
	const queued = 4
	var ids []string
	for i := range queued {
		id, err := env.queueEnvelope(ctx, plugin.Server.Server.ID, unknownType(),
			map[string]any{"marker": i, "pad": pad})
		if err != nil {
			return err
		}
		ids = append(ids, id)
	}

	var delivered []string
	for polls := 1; len(delivered) < queued; polls++ {
		if polls > 2*queued {
			return fmt.Errorf("%d polls delivered %d of the %d queued envelopes; a cut batch must leave the rest for the next poll",
				polls-1, len(delivered), queued)
		}
		clock := env.stopwatch()
		// Env.send fails this poll if the answer runs past the bound.
		response, err := plugin.pollAndAck(ctx)
		if err != nil {
			return err
		}
		if elapsed := clock(); elapsed > 2*time.Second {
			return fmt.Errorf("poll %d was held for %s with %d envelopes still queued; the hub answers at once while unacked work is waiting (section 3.1.2)",
				polls, elapsed, queued-len(delivered))
		}
		if len(response.Envelopes) == 0 {
			return fmt.Errorf("poll %d answered an empty batch with %d envelopes still queued; the oldest one always fits alone (section 3.1.2)",
				polls, queued-len(delivered))
		}
		for _, envelope := range response.Envelopes {
			delivered = append(delivered, envelope.ID)
		}
	}
	for i, id := range ids {
		if i >= len(delivered) || delivered[i] != id {
			return fmt.Errorf("envelopes arrived as %v, want the queued order %v", delivered, ids)
		}
	}
	return nil
}

func checkEnvelopeBodyBound(ctx context.Context, env Env) error {
	plugin, err := env.newFakePlugin(ctx, "conformance: envelope body bound", shortPollTimeoutSeconds)
	if err != nil {
		return err
	}
	if _, err := plugin.publishManifest(ctx, manifestBody(1)); err != nil {
		return err
	}
	serverID := plugin.Server.Server.ID
	oversized := strings.Repeat("a", envelopeBodyBound+1000)

	if err := env.expectError(ctx, http.MethodPost, "/api/v1/servers/"+serverID+"/envelopes", env.AdminToken,
		map[string]any{"type": unknownType(), "body": map[string]any{"pad": oversized}},
		http.StatusRequestEntityTooLarge, "payload_too_large"); err != nil {
		return err
	}
	// The manifest's item param is a string with no length bound, so only
	// the envelope body bound can refuse this.
	if err := env.expectError(ctx, http.MethodPost, "/api/v1/servers/"+serverID+"/actions", env.AdminToken,
		map[string]any{"code": "example-mod.heal", "params": map[string]any{"amount": 10, "item": oversized}},
		http.StatusRequestEntityTooLarge, "payload_too_large"); err != nil {
		return err
	}

	// A body under the bound is queued as usual, and it is the first thing
	// the plugin receives: neither refusal left anything behind.
	pad := strings.Repeat("a", boundPadding)
	actionID, _, err := env.dispatchAction(ctx, serverID, map[string]any{
		"code": "example-mod.heal", "params": map[string]any{"amount": 10, "item": pad},
	})
	if err != nil {
		return fmt.Errorf("a dispatch whose body is under the bound was refused: %w", err)
	}
	response, err := plugin.pollAndAck(ctx)
	if err != nil {
		return err
	}
	if len(response.Envelopes) != 1 || response.Envelopes[0].Type != "action.dispatch" {
		types := make([]string, len(response.Envelopes))
		for i, envelope := range response.Envelopes {
			types[i] = envelope.Type
		}
		return fmt.Errorf("the poll after two refusals delivered %v, want the one action.dispatch that was accepted", types)
	}
	var body struct {
		ActionID string `json:"actionId"`
		Params   struct {
			Item string `json:"item"`
		} `json:"params"`
	}
	if err := json.Unmarshal(response.Envelopes[0].Body, &body); err != nil {
		return fmt.Errorf("decode action.dispatch body: %w", err)
	}
	if body.ActionID != actionID || len(body.Params.Item) != boundPadding {
		return fmt.Errorf("action.dispatch carried action %q with an item of %d bytes, want %q with %d",
			body.ActionID, len(body.Params.Item), actionID, boundPadding)
	}
	return nil
}

// checkKVByteBound stores a value of about 4 KB nested 2000 deep, well inside
// the 16384-byte value bound, and reads it back through the Plugin API on both
// spellings. Written compact it is a few kilobytes; indented two spaces a
// level it would be about 8 MB, which is why section 2.1 asks for compact
// bodies and why the response bound catches a hub that indents.
func checkKVByteBound(ctx context.Context, env Env) error {
	namespace := uniqueKVNamespace()
	plugin, err := env.newKVPlugin(ctx, "conformance: kv byte bound", namespace)
	if err != nil {
		return err
	}
	const depth = 2000
	nested := json.RawMessage(strings.Repeat("[", depth) + "1" + strings.Repeat("]", depth))
	path := kvPath("/plugin/v1", namespace, "nested")
	// Env.send fails any of these whose answer runs past the bound.
	if err := env.expect(ctx, http.MethodPut, path, plugin.Session.SessionToken,
		map[string]any{"value": nested}, http.StatusOK, nil); err != nil {
		return err
	}
	for _, read := range []struct{ method, path string }{
		{http.MethodGet, path},
		{http.MethodPost, path + "/get"},
	} {
		var entry kvEntry
		if err := env.expect(ctx, read.method, read.path, plugin.Session.SessionToken,
			nil, http.StatusOK, &entry); err != nil {
			return err
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, entry.Value); err != nil || compact.String() != string(nested) {
			return fmt.Errorf("%s %s read back a value that is not the one stored", read.method, read.path)
		}
	}
	return nil
}

// checkBanPageByteBound fills the list with entries large enough that a page
// at the 500 cap could not fit the bound, then walks it. Bans are placed and
// lifted through several tokens at once, each inside its rate-limit burst, so
// the setup costs seconds rather than a minute at one token's pace.
func checkBanPageByteBound(ctx context.Context, env Env) error {
	const (
		tokens   = 11
		perToken = 30
	)
	// Four bytes in UTF-8 in any encoder; 200 code points is the bound on
	// both fields (section 13.1), so an entry runs to about 1.7 KB and 330
	// of them to more than the response bound.
	long := strings.Repeat("\U0001F6E1", 200)
	subjects := make([][]string, tokens)
	bearers := make([]string, tokens)
	for t := range tokens {
		minted, err := env.mintToken(ctx, fmt.Sprintf("conformance: ban filler %d", t), "bans:manage")
		if err != nil {
			return err
		}
		bearers[t] = minted.Secret
		for range perToken {
			subjects[t] = append(subjects[t], uniqueIdentity())
		}
	}

	banIDs := make([][]string, tokens)
	inParallel := func(each func(t int) error) error {
		var wait sync.WaitGroup
		errs := make([]error, tokens)
		for t := range tokens {
			wait.Add(1)
			go func() {
				defer wait.Done()
				errs[t] = each(t)
			}()
		}
		wait.Wait()
		for _, err := range errs {
			if err != nil {
				return err
			}
		}
		return nil
	}
	lift := func() error {
		return inParallel(func(t int) error {
			for _, id := range banIDs[t] {
				if _, err := env.liftBan(ctx, bearers[t], id); err != nil {
					return err
				}
			}
			return nil
		})
	}
	placed := inParallel(func(t int) error {
		for _, subject := range subjects[t] {
			change, err := env.createBan(ctx, bearers[t],
				banRequest(subject, long, map[string]any{"name": long}))
			if err != nil {
				return err
			}
			banIDs[t] = append(banIDs[t], change.Ban.ID)
		}
		return nil
	})
	// Lifted whatever happens, so the list goes back to the size the other
	// checks expect to walk.
	defer func() { _ = lift() }()
	if placed != nil {
		return placed
	}

	plugin, err := env.newFakePlugin(ctx, "conformance: ban byte bound", shortPollTimeoutSeconds)
	if err != nil {
		return err
	}
	// Env.send fails any page that runs past the bound.
	_, met, err := env.walkBans(ctx, plugin.Session.SessionToken, false, "", -1, 500)
	if err != nil {
		return err
	}
	seen := map[string]int{}
	for _, key := range met {
		if key.platform == "steam" {
			seen[key.id]++
		}
	}
	for t := range tokens {
		for _, subject := range subjects[t] {
			if seen[subject] != 1 {
				return fmt.Errorf("the walk met steam:%s %d times, want once; a page that ends early still hands its cursor on (section 13.3)",
					subject, seen[subject])
			}
		}
	}
	return nil
}
