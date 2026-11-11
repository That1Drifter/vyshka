package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Rate limiting (spec section 2.2.1) is optional for a hub, so these checks
// have three outcomes, not two. Each one bursts a cheap authenticated request
// from a credential of its own, up to rateBurstRequests times. A hub that
// answers some of them 429 is graded on the refusal's shape, on having
// applied nothing from a refused mutation, and on admitting the same request
// once Retry-After has passed. A hub that answers none is reported as not
// exercised: that is a conformant hub, but the suite says plainly that it
// graded nothing rather than passing it.
//
// The credentials are the check's own (a fresh server, a freshly minted
// token), so a bucket a check drains never throttles another check, and
// nothing they leave behind is used again.

// rateBurstRequests is how many requests a check sends before concluding the
// hub does not limit at that rate. rateBurstWorkers send them concurrently,
// so the burst outruns a refill even over a link with real latency.
const (
	rateBurstRequests = 1000
	rateBurstWorkers  = 16
	// rateRetrySlack is added to every Retry-After the checks wait out, for
	// timer granularity on the suite's side.
	rateRetrySlack = 100 * time.Millisecond
	// rateMaxWait bounds the Retry-After the checks will honor, so a hub
	// naming an absurd wait fails the check instead of hanging the suite.
	rateMaxWait = 60 * time.Second
)

// notExercised is what a check returns when the behavior it grades is
// optional and the hub never showed it. The runner reports it apart from a
// pass, so a green run cannot be mistaken for one that graded rate limits.
type notExercised struct{ reason string }

func (n notExercised) Error() string { return n.reason }

// rateRefusal is one 429 as the suite read it.
type rateRefusal struct {
	Status     int
	RetryAfter int // the header, parsed; -1 when absent or not an integer
	Raw        string
	Body       []byte
}

// wait is how long the suite waits before sending the refused request again.
func (r rateRefusal) wait() time.Duration {
	return time.Duration(r.RetryAfter)*time.Second + rateRetrySlack
}

// gradeRateRefusal holds one 429 to section 2.2.1: code rate_limited, a
// Retry-After of whole seconds at least 1, and details.retryAfterSeconds
// carrying the same integer for a client that cannot read headers.
func gradeRateRefusal(what string, refusal rateRefusal) error {
	if refusal.Status != http.StatusTooManyRequests {
		return fmt.Errorf("%s: want status 429, got %d", what, refusal.Status)
	}
	if err := assertErrorCode("", what, refusal.Body, "rate_limited"); err != nil {
		return fmt.Errorf("%w (section 2.2.1: a 429 carries code rate_limited)", err)
	}
	if refusal.RetryAfter < 1 {
		return fmt.Errorf("%s: Retry-After is %q; a rate_limited refusal MUST carry whole seconds, at least 1 (section 2.2.1)", what, refusal.Raw)
	}
	if time.Duration(refusal.RetryAfter)*time.Second > rateMaxWait {
		// The spec sets no ceiling, so a long wait is not a fault; it is
		// longer than this suite waits, so nothing past it is graded.
		return notExercised{fmt.Sprintf("%s: Retry-After is %d s, longer than the %s this suite waits, so what follows a refusal was not graded", what, refusal.RetryAfter, rateMaxWait)}
	}
	var failure struct {
		Error struct {
			Details struct {
				RetryAfterSeconds json.RawMessage `json:"retryAfterSeconds"`
			} `json:"details"`
		} `json:"error"`
	}
	_ = json.Unmarshal(refusal.Body, &failure)
	raw := strings.TrimSpace(string(failure.Error.Details.RetryAfterSeconds))
	if raw == "" {
		return fmt.Errorf("%s: the refusal carries no details.retryAfterSeconds; section 2.2.1 requires it beside Retry-After, for a plugin that cannot read headers (body %q)", what, truncate(refusal.Body))
	}
	if raw != strconv.Itoa(refusal.RetryAfter) {
		return fmt.Errorf("%s: details.retryAfterSeconds is %s and Retry-After is %d; both carry the same integer (section 2.2.1)", what, raw, refusal.RetryAfter)
	}
	return nil
}

// burstUntilRefused sends send() from rateBurstWorkers goroutines until one
// answers 429 or rateBurstRequests have gone out. It returns the first
// refusal, or nil with the count sent when none came. It returns only once
// every request it started has been answered, so nothing of the burst is
// still in flight to spend tokens behind the caller's back.
func burstUntilRefused(ctx context.Context, send func(context.Context) (*http.Response, []byte, error)) (*rateRefusal, int, time.Duration, error) {
	var (
		sent     atomic.Int64
		stop     atomic.Bool
		mu       sync.Mutex
		first    *rateRefusal
		firstErr error
		wg       sync.WaitGroup
	)
	started := time.Now()
	for range rateBurstWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() && ctx.Err() == nil {
				if sent.Add(1) > rateBurstRequests {
					return
				}
				resp, body, err := send(ctx)
				mu.Lock()
				switch {
				case err != nil:
					if firstErr == nil {
						firstErr = err
					}
					stop.Store(true)
				case resp.StatusCode == http.StatusTooManyRequests:
					if first == nil {
						raw := resp.Header.Get("Retry-After")
						seconds, parseErr := strconv.Atoi(strings.TrimSpace(raw))
						if parseErr != nil {
							seconds = -1
						}
						first = &rateRefusal{Status: resp.StatusCode, RetryAfter: seconds, Raw: raw, Body: body}
					}
					stop.Store(true)
				case resp.StatusCode >= 300:
					if firstErr == nil {
						firstErr = fmt.Errorf("a burst request answered %d (body %q); the burst sends a request the credential is allowed, so only 2xx or 429 is expected", resp.StatusCode, truncate(body))
					}
					stop.Store(true)
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	count := int(min(sent.Load(), rateBurstRequests))
	return first, count, time.Since(started), firstErr
}

// readRefusal turns one answer into a rateRefusal, whatever its status.
func readRefusal(resp *http.Response, body []byte) rateRefusal {
	raw := resp.Header.Get("Retry-After")
	seconds, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		seconds = -1
	}
	return rateRefusal{Status: resp.StatusCode, RetryAfter: seconds, Raw: raw, Body: body}
}

// sleepCtx waits d or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// checkPluginRateLimit grades a Plugin API limit with a server of its own. A
// hub-queued envelope gives the refused poll an ack to (not) apply, and the
// poll carries an envelope of its own to (not) ingest; both are read back
// through channels the limit cannot touch: the Admin API, and the ack in the
// next admitted poll's answer.
func checkPluginRateLimit(ctx context.Context, env Env) error {
	enrolled, created, err := env.newEnrolled(ctx, "conformance: rate limit (plugin)")
	if err != nil {
		return err
	}
	session, err := env.startSession(ctx, enrolled, 5)
	if err != nil {
		return err
	}
	// A second server, untouched by the burst, and a third whose real
	// credential must survive a burst of wrong secrets naming it.
	otherEnrolled, _, err := env.newEnrolled(ctx, "conformance: rate limit (plugin, bystander)")
	if err != nil {
		return err
	}
	other, err := env.startSession(ctx, otherEnrolled, 5)
	if err != nil {
		return err
	}
	claimed, _, err := env.newEnrolled(ctx, "conformance: rate limit (plugin, claimed id)")
	if err != nil {
		return err
	}
	// The control for the claimed-id check: a server the wrong-secret burst
	// never names.
	unnamed, _, err := env.newEnrolled(ctx, "conformance: rate limit (plugin, control)")
	if err != nil {
		return err
	}
	// This check grades the 429s of its own servers itself.
	ctx = withRawRateLimits(ctx, session.SessionToken, other.SessionToken)
	serverID := created.Server.ID
	if _, err := env.queueEnvelope(ctx, serverID, "conformance.rate-limit.outbound", map[string]any{}); err != nil {
		return err
	}
	var first pollResponse
	resp, body, err := env.doWith(env.PollClient, ctx, http.MethodPost, "/plugin/v1/poll", session.SessionToken, map[string]any{})
	if err != nil {
		return fmt.Errorf("the first poll, before any burst: %w", err)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		// The session request and this poll are the server's first two
		// requests. A burst that small breaks a SHOULD (a behaving plugin's
		// session start needs more), not a MUST, so it is not a failure.
		return notExercised{"the hub refused this server's first poll, its second request after the session request, so its burst is below what the check needs before it can begin (section 2.2.1 asks a hub to admit a behaving plugin's session start, a SHOULD); nothing was graded"}
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the first poll, before any burst: want status 200, got %d, body %q", resp.StatusCode, truncate(body))
	}
	if err := json.Unmarshal(body, &first); err != nil {
		return fmt.Errorf("the first poll, before any burst: decode %q: %w", truncate(body), err)
	}
	if len(first.Envelopes) != 1 || first.Envelopes[0].Seq != 1 {
		return fmt.Errorf("the first poll delivered %d envelope(s), want the one queued at seq 1", len(first.Envelopes))
	}

	refusal, sent, took, err := burstUntilRefused(ctx, func(ctx context.Context) (*http.Response, []byte, error) {
		return env.do(ctx, http.MethodGet, "/plugin/v1/session", session.SessionToken, nil)
	})
	if err != nil {
		return err
	}
	if refusal == nil {
		return notExercised{fmt.Sprintf("%d session reads from one server in %s drew no 429: this hub does not rate-limit the Plugin API at that rate, or at all, which section 2.2.1 allows; nothing was graded", sent, took.Round(time.Millisecond))}
	}
	if err := gradeRateRefusal(fmt.Sprintf("GET /plugin/v1/session (request %d of the burst)", sent), *refusal); err != nil {
		return err
	}

	// The mutation, sent at once while the bucket is empty: it acks the
	// delivered envelope and carries one of the plugin's own.
	probe := map[string]any{
		"v": 1, "id": "conformance-rate-limit-" + strconv.FormatInt(time.Now().UnixNano(), 36),
		"type": "conformance.rate-limit.inbound", "seq": 1,
		"ts": time.Now().UTC().Format(time.RFC3339), "body": map[string]any{},
	}
	refusedPoll := map[string]any{"ack": 1, "envelopes": []any{probe}}
	resp, body, err = env.doWith(env.PollClient, ctx, http.MethodPost, "/plugin/v1/poll", session.SessionToken, refusedPoll)
	if err != nil {
		return fmt.Errorf("the poll sent while throttled: %w", err)
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		return notExercised{fmt.Sprintf("the hub refused request %d of a burst of session reads with a well-formed 429, but admitted the poll sent straight after it (status %d), so whether a refused poll applies nothing could not be graded", sent, resp.StatusCode)}
	}
	pollRefusal := readRefusal(resp, body)
	if err := gradeRateRefusal("POST /plugin/v1/poll while throttled", pollRefusal); err != nil {
		return err
	}
	// One credential's flood spends no one else's allowance: a different
	// server's first request is admitted while this one is drained.
	resp, body, err = env.do(ctx, http.MethodGet, "/plugin/v1/session", other.SessionToken, nil)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return fmt.Errorf("a different server's first request was refused 429 while only this check's server was over its limit (body %q); one credential's requests MUST NOT spend another's allowance (section 2.2.1)", truncate(body))
	}

	// Nothing applied: the ack did not free the delivered envelope...
	record, err := env.serverRecord(ctx, serverID)
	if err != nil {
		return err
	}
	if record.PendingEnvelopeCount != 1 {
		return fmt.Errorf("after a poll acking seq 1 was refused rate_limited, the server reports pendingEnvelopeCount %d, want 1: a refused request applies nothing, its ack included (section 2.2.1)", record.PendingEnvelopeCount)
	}
	// ...and, once Retry-After has passed, an admitted poll reports that the
	// probe was never ingested. The envelope still queued makes the hub answer
	// it at once.
	if err := sleepCtx(ctx, pollRefusal.wait()); err != nil {
		return err
	}
	resp, body, err = env.doWith(env.PollClient, ctx, http.MethodPost, "/plugin/v1/poll", session.SessionToken, map[string]any{})
	if err != nil {
		return fmt.Errorf("the poll after Retry-After: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("a poll sent %d s after a rate_limited refusal, as its Retry-After asked, answered %d (body %q); Retry-After names when the credential is admitted again (section 2.2.1)", pollRefusal.RetryAfter, resp.StatusCode, truncate(body))
	}
	var after pollResponse
	if err := json.Unmarshal(body, &after); err != nil {
		return fmt.Errorf("the poll after Retry-After: decode %q: %w", truncate(body), err)
	}
	if after.Ack != 0 {
		return fmt.Errorf("after the poll carrying envelope seq 1 was refused rate_limited, the hub acks %d, want 0: a refused poll ingests nothing (section 2.2.1)", after.Ack)
	}
	if len(after.Envelopes) != 1 || after.Envelopes[0].Seq != 1 {
		return fmt.Errorf("after the refused ack, the hub delivered %d envelope(s), want seq 1 again: it was never acked (section 9.2)", len(after.Envelopes))
	}

	// The refused poll, sent again unchanged, is applied once.
	var applied pollResponse
	for attempt := 1; ; attempt++ {
		resp, body, err = env.doWith(env.PollClient, ctx, http.MethodPost, "/plugin/v1/poll", session.SessionToken, refusedPoll)
		if err != nil {
			return fmt.Errorf("retransmitting the refused poll: %w", err)
		}
		if resp.StatusCode == http.StatusTooManyRequests && attempt == 1 {
			again := readRefusal(resp, body)
			if err := gradeRateRefusal("POST /plugin/v1/poll, the retransmission", again); err != nil {
				return err
			}
			if err := sleepCtx(ctx, again.wait()); err != nil {
				return err
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("the refused poll, retransmitted after Retry-After, answered %d (body %q); want 200 (section 2.2.1)", resp.StatusCode, truncate(body))
		}
		if err := json.Unmarshal(body, &applied); err != nil {
			return fmt.Errorf("the retransmitted poll: decode %q: %w", truncate(body), err)
		}
		break
	}
	if applied.Ack != 1 {
		return fmt.Errorf("the retransmitted poll was answered with ack %d, want 1: the probe envelope it carries must be ingested now", applied.Ack)
	}
	record, err = env.serverRecord(ctx, serverID)
	if err != nil {
		return err
	}
	if record.PendingEnvelopeCount != 0 {
		return fmt.Errorf("after the retransmitted poll acked seq 1, pendingEnvelopeCount is %d, want 0", record.PendingEnvelopeCount)
	}

	// No lockout by a claimed id: twice as many session requests as drained
	// this check's server, all naming another server with a wrong secret,
	// and then that server's real session request is not refused. A hub
	// that charged the claimed serverId would refuse it (section 2.2.1).
	wrong := map[string]any{"serverId": claimed.ServerID, "serverSecret": "conformance-wrong-secret", "pollTimeoutSeconds": 5}
	if err := sendMany(ctx, env, min(2*sent, rateBurstRequests), http.MethodPost, "/plugin/v1/session", wrong); err != nil {
		return err
	}
	genuine := map[string]any{"serverId": claimed.ServerID, "serverSecret": claimed.ServerSecret, "pollTimeoutSeconds": 5}
	resp, body, err = env.send(env.Client, ctx, http.MethodPost, "/plugin/v1/session", "", mustJSON(genuine), true)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		// Keyed on the claimed id, or on something else the burst shared,
		// such as the source address (a proxy's limit_req, a per-address
		// throttle on failed logins)? A server the burst never named tells.
		control := map[string]any{"serverId": unnamed.ServerID, "serverSecret": unnamed.ServerSecret, "pollTimeoutSeconds": 5}
		controlResp, controlBody, err := env.send(env.Client, ctx, http.MethodPost, "/plugin/v1/session", "", mustJSON(control), true)
		if err != nil {
			return err
		}
		if controlResp.StatusCode == http.StatusTooManyRequests {
			return notExercised{fmt.Sprintf("after %d wrong-secret session requests, a genuine session request was refused 429 for the server they named and also for one they never named (body %q), so the refusals are not keyed on the claimed id but on something the requests shared, such as their source address; whether a claimed id could lock a server out was not graded", min(2*sent, rateBurstRequests), truncate(controlBody))}
		}
		return fmt.Errorf("after %d session requests naming server %s with a wrong secret, its real session request was refused 429 (body %q) while a server they never named was admitted; a limit is never keyed on an identity a request merely claims (section 2.2.1)", min(2*sent, rateBurstRequests), claimed.ServerID, truncate(body))
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the real session request after the wrong-secret burst answered %d (body %q), want 200", resp.StatusCode, truncate(body))
	}
	return nil
}

// sendMany sends count copies of one request from rateBurstWorkers
// goroutines, unretried, and waits for every answer.
func sendMany(ctx context.Context, env Env, count int, method, path string, body any) error {
	encoded := mustJSON(body)
	var next atomic.Int64
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for range rateBurstWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for next.Add(1) <= int64(count) && ctx.Err() == nil {
				if _, _, err := env.send(env.Client, ctx, method, path, "", encoded, true); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
					return
				}
			}
		}()
	}
	wg.Wait()
	return firstErr
}

func mustJSON(value any) []byte {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return encoded
}

// checkAdminRateLimit grades an Admin API limit with a token of its own,
// holding only kv:rw on a namespace no other check touches. The refused
// mutation is a key write, read back with the suite's own credential; the
// audit log must hold a record of it (section 10.5 lets a hub collapse a run
// of rate-limited mutations, never drop the first).
func checkAdminRateLimit(ctx context.Context, env Env) error {
	namespace := uniqueKVNamespace()
	minted, err := env.mintToken(ctx, "conformance-rate-limit", "kv:rw:"+namespace)
	if err != nil {
		return err
	}
	defer func() {
		_, _, _ = env.do(context.WithoutCancel(ctx), http.MethodDelete, "/api/v1/tokens/"+url.PathEscape(minted.Token.ID), env.AdminToken, nil)
	}()
	bystander, err := env.mintToken(ctx, "conformance-rate-limit-bystander", "kv:rw:"+namespace)
	if err != nil {
		return err
	}
	defer func() {
		_, _, _ = env.do(context.WithoutCancel(ctx), http.MethodDelete, "/api/v1/tokens/"+url.PathEscape(bystander.Token.ID), env.AdminToken, nil)
	}()
	// This check grades the 429s of its own tokens itself.
	ctx = withRawRateLimits(ctx, minted.Secret, bystander.Secret)
	keyPath := kvPath("/api/v1", namespace, "probe")

	refusal, sent, took, err := burstUntilRefused(ctx, func(ctx context.Context) (*http.Response, []byte, error) {
		resp, body, err := env.do(ctx, http.MethodGet, kvPath("/api/v1", namespace, "absent"), minted.Secret, nil)
		if err == nil && resp.StatusCode == http.StatusNotFound {
			// The cheap read the burst is made of: an authenticated lookup
			// of a key that does not exist.
			resp.StatusCode = http.StatusOK
		}
		return resp, body, err
	})
	if err != nil {
		return err
	}
	if refusal == nil {
		return notExercised{fmt.Sprintf("%d key reads from one token in %s drew no 429: this hub does not rate-limit the Admin API at that rate, or at all, which section 2.2.1 allows; nothing was graded", sent, took.Round(time.Millisecond))}
	}
	if err := gradeRateRefusal(fmt.Sprintf("GET %s (request %d of the burst)", kvPath("/api/v1", namespace, "absent"), sent), *refusal); err != nil {
		return err
	}

	write := map[string]any{"value": "written once"}
	resp, body, err := env.do(ctx, http.MethodPut, keyPath, minted.Secret, write)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		return notExercised{fmt.Sprintf("the hub refused request %d of a burst of key reads with a well-formed 429, but admitted the write sent straight after it (status %d), so whether a refused mutation applies nothing could not be graded", sent, resp.StatusCode)}
	}
	writeRefusal := readRefusal(resp, body)
	if err := gradeRateRefusal("PUT "+keyPath+" while throttled", writeRefusal); err != nil {
		return err
	}
	// One token's flood spends no other token's allowance.
	resp, body, err = env.do(ctx, http.MethodGet, kvPath("/api/v1", namespace, "absent"), bystander.Secret, nil)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return fmt.Errorf("a different token's first request was refused 429 while only this check's token was over its limit (body %q); one credential's requests MUST NOT spend another's allowance (section 2.2.1)", truncate(body))
	}
	if err := env.expectError(ctx, http.MethodGet, keyPath, env.AdminToken, nil, http.StatusNotFound, "not_found"); err != nil {
		return fmt.Errorf("a key write refused rate_limited must not be applied (section 2.2.1): %w", err)
	}

	page, err := env.auditRecords(ctx, url.Values{"tokenId": {minted.Token.ID}})
	if err != nil {
		return err
	}
	recorded := false
	for _, record := range page.Records {
		if record.Method == http.MethodPut && record.Path == keyPath && record.Status == http.StatusTooManyRequests {
			recorded = true
		}
	}
	if !recorded {
		return fmt.Errorf("the audit log holds no record of the token's first rate-limited mutation (PUT %s, 429); a hub may collapse a run of them, but records the first (section 10.5)", keyPath)
	}

	if err := sleepCtx(ctx, writeRefusal.wait()); err != nil {
		return err
	}
	resp, body, err = env.do(ctx, http.MethodPut, keyPath, minted.Secret, write)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("the refused write, sent again %d s later as Retry-After asked, answered %d (body %q); want it admitted (section 2.2.1)", writeRefusal.RetryAfter, resp.StatusCode, truncate(body))
	}
	var stored kvEntry
	if err := env.expect(ctx, http.MethodGet, keyPath, env.AdminToken, nil, http.StatusOK, &stored); err != nil {
		return err
	}
	if stored.Revision != 1 {
		return fmt.Errorf("the key's revision after the refused write and its admitted retry is %d, want 1: written exactly once", stored.Revision)
	}
	return nil
}

// serverRecord reads one server record with the suite's own credential.
func (e Env) serverRecord(ctx context.Context, serverID string) (serverRecord, error) {
	var record serverRecord
	err := e.expect(ctx, http.MethodGet, "/api/v1/servers/"+url.PathEscape(serverID), e.AdminToken, nil, http.StatusOK, &record)
	return record, err
}

var rateLimitChecks = []Check{
	{
		ID:      "plugin.rateLimit",
		Title:   "A rate-limited poll is a well-formed 429, applies nothing, and is admitted after Retry-After",
		Section: "2.2.1",
		Run:     checkPluginRateLimit,
	},
	{
		ID:      "admin.rateLimit",
		Title:   "A rate-limited admin write is a well-formed 429, applies nothing, is audited, and is admitted after Retry-After",
		Section: "2.2.1",
		Run:     checkAdminRateLimit,
	},
}

// Last in the suite (init runs in file-name order): a check that drains a
// bucket does it to a credential of its own, and running after everything
// else also keeps the burst's load off every other check's timing.
func init() {
	checks = append(checks, rateLimitChecks...)
}
