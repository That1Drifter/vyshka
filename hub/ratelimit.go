package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/That1Drifter/vyshka/hub/internal/ratelimit"
	"github.com/That1Drifter/vyshka/hub/store"
)

// Rate limits per credential (spec section 2.2.1). Every default lives here;
// the binary's flags and the deployment guide read them from here.
//
// Source: the capacity spike of issue #87 (spikes/hub-capacity/README.md,
// "Request rates per credential"). A legitimate DayZ-shaped plugin's busiest
// 10 s was 0.8 requests/s per server and its busiest second 4 requests (3
// for the realistic profile); session start was outside its window, and
// the reference driver's measures four requests on its own; a
// panel-like admin client's busiest 10 s was 6.9 requests/s per token and its
// busiest second 15, in runs that met every bound.
//
//   - Plugin: 5/s, burst 60. The spike recommended 2/s, burst 30; the figure
//     was widened because a plugin's key/value traffic was not measured and
//     the mod surface hands the KV store to third-party mods. That is about
//     six times the busiest legitimate 10 s, and the burst covers a backlog
//     drain of a full 5000-envelope queue (25 polls of 200).
//   - Admin: 10/s, burst 60, as the spike recommends: about 1.4 times the
//     busiest panel client's 10 s, and four times its busiest second.
//
// What these do not bound is ingest volume: a request may carry up to 200
// envelopes and 1000 events, so 5 polls/s is far more events than the hub
// was measured to ingest within its bounds (about 1230 events/s across 100
// servers on one SQLite connection). An event budget is a slice of its own.
const (
	DefaultPluginRateLimit = 5.0 // requests per second, per server
	DefaultPluginRateBurst = 60
	DefaultAdminRateLimit  = 10.0 // requests per second, per token
	DefaultAdminRateBurst  = 60
)

// rateLimitReportEvery is how often one credential's rate-limited refusals
// reach the warning log, and its rate-limited mutations the audit log: the
// first of a run at once, then one per interval carrying the count of those
// left out (spec section 10.5 caps the interval at 60 s).
const rateLimitReportEvery = time.Minute

// Values of the api label on vyshka_rate_limited_total.
const (
	rateLimitedPlugin = "plugin"
	rateLimitedAdmin  = "admin"
)

// rateLimits is the hub's limiter state: one limiter per API, and the
// reporters that keep a flood out of the logs it would otherwise fill.
type rateLimits struct {
	plugin, admin       *ratelimit.Limiter
	pluginLog, adminLog *ratelimit.Reporter
	adminAudit          *ratelimit.Reporter

	// banNotices names the servers holding a bans.changed that an admin's
	// ban edit or the hub's expiry sweep queued and no poll has sent yet.
	// The notice row itself cannot say who caused it (a manifest newly
	// declaring the bans capability queues, or refreshes, the same row), so
	// the cause is carried here, in memory. A restart forgets it, which only
	// means that one poll is charged.
	banMu      sync.Mutex
	banNotices map[string]bool
}

// markBanNotice records that an admin-side ban change queued a bans.changed
// for serverID.
func (l *rateLimits) markBanNotice(serverID string) {
	if !l.plugin.Enabled() {
		return
	}
	l.banMu.Lock()
	defer l.banMu.Unlock()
	l.banNotices[serverID] = true
}

// takeBanNotice reports whether a bans.changed being sent to serverID for
// the first time was caused by an admin-side ban change, and consumes the
// mark: one refund per mark, so a plugin cannot multiply it.
func (l *rateLimits) takeBanNotice(serverID string) bool {
	l.banMu.Lock()
	defer l.banMu.Unlock()
	marked := l.banNotices[serverID]
	delete(l.banNotices, serverID)
	return marked
}

func newRateLimits(cfg Config) *rateLimits {
	return &rateLimits{
		plugin:     ratelimit.New(cfg.PluginRateLimit, cfg.PluginRateBurst, nil),
		admin:      ratelimit.New(cfg.AdminRateLimit, cfg.AdminRateBurst, nil),
		pluginLog:  ratelimit.NewReporter(rateLimitReportEvery, nil),
		adminLog:   ratelimit.NewReporter(rateLimitReportEvery, nil),
		adminAudit: ratelimit.NewReporter(rateLimitReportEvery, nil),
		banNotices: map[string]bool{},
	}
}

// limitPlugin charges one request to the server a Plugin API request
// authenticated as, and answers the refusal itself when the server is over
// its limit. It runs once the credential is known and before anything in the
// request is read or applied, so a refused request changes nothing.
func (s *Server) limitPlugin(w http.ResponseWriter, r *http.Request, serverID string) bool {
	allowed, wait := s.limits.plugin.Allow(serverID)
	if allowed {
		return true
	}
	s.metrics.rateLimited.Inc(rateLimitedPlugin)
	if report, skipped := s.limits.pluginLog.Refused(serverID, nil); report {
		s.log.Warn("plugin request refused: the server is over its rate limit",
			"serverId", serverID, "path", r.URL.Path, "retryAfter", wait.String(),
			"refusedSincePreviousWarning", skipped)
		s.limits.pluginLog.Recorded(serverID, true)
	}
	s.writeRateLimited(w, r, wait, "this server is over the hub's request rate limit")
	return false
}

// adminRateKey names the bucket an admin principal spends from. The bootstrap
// credential has no token id (spec section 10.6) and is limited like any
// other token under its own key; minted ids never take that form.
func adminRateKey(caller *principal) string {
	if caller.TokenID == "" {
		return "bootstrap"
	}
	return "token:" + caller.TokenID
}

// writeRateLimited is the refusal of spec section 2.2.1: 429 rate_limited,
// with the wait in whole seconds both in Retry-After and in
// details.retryAfterSeconds, the latter for an engine that cannot read
// headers.
//
// A refusal that leaves a request body unread hangs up, like every other
// refusal at the headers, so the answer goes out at once instead of behind a
// drain the client controls. A bodyless one keeps the connection: a flood of
// reads refused with Connection: close would cost a fresh TCP (and, behind a
// proxy, TLS) handshake per refusal, which is more work for the host than the
// refused request was, and measured on Windows it left one TIME_WAIT socket
// per refusal, enough to exhaust the client host's ephemeral ports within
// seconds.
func (s *Server) writeRateLimited(w http.ResponseWriter, r *http.Request, wait time.Duration, message string) {
	seconds := ratelimit.RetryAfterSeconds(wait)
	if r.ContentLength != 0 {
		s.hangUp(w)
	}
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	writeErrorDetails(w, http.StatusTooManyRequests, codeRateLimited,
		message+"; nothing in this request was applied, send it again after "+strconv.Itoa(seconds)+" s",
		map[string]any{"retryAfterSeconds": seconds})
}

// adminCaused reports whether the hub queues an envelope of this type only
// because the Admin API asked for it: an action dispatch, a context question
// asked on an admin read, or anything queued through the raw envelope
// endpoint, which refuses every family the hub models (spec section 5.5).
// A poll answered with the first send of one is refunded (spec section
// 2.2.1). The types a plugin's own traffic can make the hub queue back to it
// are excluded, or a leaked server credential could earn its own refunds:
// the rejection notices its malformed batches earn (up to 20 a poll), and
// bans.changed, which a manifest newly declaring the bans capability queues.
// A bans.changed an admin's ban edit or the expiry sweep queued is refunded
// all the same, through the mark announceBanChange leaves (takeBanNotice):
// the type alone cannot tell the two causes apart. The walk of the list that
// follows a notice is the plugin's own requests and stays charged.
func adminCaused(envelopeType string) bool {
	switch envelopeType {
	case envelopeTypeManifestReject, envelopeTypeEventReject, envelopeTypeStateReject, envelopeTypeBansChanged:
		return false
	}
	return true
}

// rateLimitedMutation is one rate-limited Admin API mutation as the audit log
// records it (spec section 10.5).
type rateLimitedMutation struct {
	tokenID, tokenName, method, path, serverID, sourceIP string
}

// recordRateLimited writes the audit record of one rate-limited mutation,
// standing for it and for unrecorded more of the credential's rate-limited
// mutations since the run's previous record. A closing record ends its run.
// It reports whether the record was written, which is what lets the run move
// on (ratelimit.Reporter).
func (s *Server) recordRateLimited(refused rateLimitedMutation, unrecorded int, closing bool) bool {
	detail := map[string]any{"unrecordedRefusals": unrecorded}
	if closing {
		detail["runClosed"] = true
	}
	encoded, err := json.Marshal(detail)
	if err != nil {
		encoded = []byte(`{}`)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.store.RecordAudit(ctx, store.NewAuditRecord{
		TokenID:   refused.tokenID,
		TokenName: refused.tokenName,
		Method:    refused.method,
		Path:      refused.path,
		Status:    http.StatusTooManyRequests,
		SourceIP:  refused.sourceIP,
		ServerID:  refused.serverID,
		Detail:    encoded,
		Retention: s.cfg.AuditRetention,
	}); err != nil {
		s.log.Error("audit record of a rate-limited mutation could not be written; its count is kept for the next try",
			"tokenId", refused.tokenID, "path", refused.path, "error", err.Error())
		return false
	}
	return true
}

// closeRateLimitRuns writes out the runs of refusals that have ended (an
// interval after their last refusal) or, at shutdown, every run with an
// outstanding count: a closing audit record for rate-limited mutations, a
// closing warning for the logs. A closing record names the first refusal it
// counts and stands for the rest. A write that fails keeps its count for the
// next pass.
func (s *Server) closeRateLimitRuns(shutdown bool) {
	collect := func(reporter *ratelimit.Reporter) []ratelimit.Closing {
		if shutdown {
			return reporter.Drain()
		}
		return reporter.Due()
	}
	for _, run := range collect(s.limits.adminAudit) {
		refused, _ := run.Sample.(rateLimitedMutation)
		s.limits.adminAudit.Closed(run.Key, s.recordRateLimited(refused, run.Count-1, true))
	}
	for _, reporter := range []*ratelimit.Reporter{s.limits.pluginLog, s.limits.adminLog} {
		for _, run := range collect(reporter) {
			s.log.Warn("a run of rate-limited requests ended", "credential", run.Key,
				"refusedSincePreviousWarning", run.Count)
			reporter.Closed(run.Key, true)
		}
	}
}
