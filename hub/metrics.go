package hub

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"time"

	"github.com/That1Drifter/vyshka/hub/internal/metrics"
	"github.com/That1Drifter/vyshka/hub/store"
)

// Values of the fixed-vocabulary labels. None of them ever comes from the
// outside, which is what keeps their cardinality bounded.
const (
	sweepActionExpiry    = "action_expiry"
	sweepBanExpiry       = "ban_expiry"
	sweepPruneEvents     = "prune_events"
	sweepPruneAudit      = "prune_audit"
	sweepPruneKV         = "prune_kv"
	sweepPruneSnapshots  = "prune_state_snapshots"
	sweepPruneDeliveries = "prune_webhook_deliveries"

	resultOK    = "ok"
	resultError = "error"

	inboundAccepted  = "accepted"
	inboundDuplicate = "duplicate"
	inboundGapped    = "gapped"

	refusedManifest   = "manifest"
	refusedEventBatch = "event_batch"
	refusedSnapshot   = "state_snapshot"

	droppedQueueFull = "queue_full"
	droppedNoticeCap = "notice_cap"
)

// hubMetrics is every metric the hub keeps. They are defined in one place so
// the exposition (documented in deploy/README.md) can be read off one file;
// the code that owns each event holds the one line that moves it. A later
// slice adds a metric by adding a field here and a call where it happens.
type hubMetrics struct {
	registry *metrics.Registry

	pollHandler      *metrics.Histogram
	pollHold         *metrics.Histogram
	envelopeDelivery *metrics.Histogram

	queueDepth     *metrics.Gauge
	queueOldest    *metrics.Gauge
	webhookPending *metrics.Gauge

	actionsFinished *metrics.Counter
	inbound         *metrics.Counter
	inboundRefused  *metrics.Counter
	outboundDropped *metrics.Counter
	webhookAttempts *metrics.Counter

	sweepRuns     *metrics.Counter
	sweepRemoved  *metrics.Counter
	sweepDuration *metrics.Histogram
	sweepLast     *metrics.Gauge
}

func newHubMetrics(st *store.Store, started time.Time) *hubMetrics {
	r := metrics.New()
	m := &hubMetrics{registry: r}

	r.NewGauge("vyshka_build_info",
		"Always 1; the labels carry the hub version, the Go version it was built with, and the database driver.",
		"version", "goversion", "database").Set(1, Version, runtime.Version(), st.Driver())
	r.NewGauge("vyshka_start_time_seconds", "When the hub started, in seconds since the Unix epoch.").
		Set(float64(started.UnixNano()) / 1e9)

	m.pollHandler = r.NewHistogram("vyshka_poll_handler_seconds",
		"Time an authenticated poll request spent in the hub doing work: reading the request, applying the plugin's "+
			"acks and envelopes, and reading the queue, with the time parked in the long-poll hold taken out.",
		[]float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10})
	m.pollHold = r.NewHistogram("vyshka_poll_hold_seconds",
		"Time an authenticated poll request spent parked in the long-poll hold; 0 for a poll answered at once.",
		[]float64{0.05, 0.25, 1, 2.5, 5, 10, 20, 30, 45, 60})
	m.envelopeDelivery = r.NewHistogram("vyshka_envelope_delivery_seconds",
		"Time from an envelope being queued for a plugin to the first poll response that carried it. Retransmissions are not observed again.",
		[]float64{0.01, 0.05, 0.25, 1, 2.5, 5, 10, 30, 60, 300, 900, 3600, 21600})

	m.queueDepth = r.NewGauge("vyshka_outbound_queue_depth",
		"Envelopes queued for a server and not yet acked by its plugin, sent or not. The bound is 5000 (spec section 9.2).",
		"server")
	m.queueOldest = r.NewGauge("vyshka_outbound_queue_oldest_seconds",
		"Age of the longest-waiting unacked envelope queued for a server; 0 when its queue is empty.", "server")
	m.webhookPending = r.NewGauge("vyshka_webhook_deliveries_pending",
		"Webhook deliveries waiting for an attempt or a retry.")

	m.actionsFinished = r.NewCounter("vyshka_actions_finished_total",
		"Actions that reached a terminal state, by action code and outcome (completed, failed, or expired).",
		"code", "outcome")
	m.inbound = r.NewCounter("vyshka_inbound_envelopes_total",
		"Envelopes plugins sent in polls, by result: accepted, duplicate (already acked, ignored), or gapped (ahead of a missing seq, not accepted).",
		"result")
	m.inboundRefused = r.NewCounter("vyshka_inbound_envelopes_refused_total",
		"Accepted envelopes the hub refused to apply and answered with a rejection notice, by kind (manifest, event_batch, state_snapshot).",
		"kind")
	m.outboundDropped = r.NewCounter("vyshka_outbound_envelopes_dropped_total",
		"Hub-originated notices that were never queued, by reason: queue_full (the server's queue was at its bound) or notice_cap (more than 20 owed by one poll).",
		"reason")
	m.webhookAttempts = r.NewCounter("vyshka_webhook_attempts_total",
		"Webhook delivery attempts, by webhook and result: delivered (2xx), retry (failed, another attempt is scheduled), or dead (failed, schedule exhausted).",
		"webhook", "result")

	m.sweepRuns = r.NewCounter("vyshka_sweep_runs_total",
		"Background sweeps run, by sweep and result (ok or error).", "sweep", "result")
	m.sweepRemoved = r.NewCounter("vyshka_sweep_removed_total",
		"Rows a sweep expired or deleted: actions flipped to expired, bans lifted, rows pruned by retention.", "sweep")
	m.sweepDuration = r.NewHistogram("vyshka_sweep_duration_seconds",
		"How long one sweep run took, which on SQLite is how long it held the database's only connection.",
		[]float64{0.001, 0.005, 0.025, 0.1, 0.5, 1, 5, 10, 30}, "sweep")
	m.sweepLast = r.NewGauge("vyshka_sweep_last_success_timestamp_seconds",
		"When a sweep last finished without error, in seconds since the Unix epoch.", "sweep")

	// Queue depths are read when scraped, in one statement (Store.QueueStats).
	r.OnScrape("queues", func(ctx context.Context) error {
		m.queueDepth.Reset()
		m.queueOldest.Reset()
		m.webhookPending.Reset()
		stats, err := st.QueueStats(ctx)
		if err != nil {
			return err
		}
		now := time.Now()
		for _, queue := range stats.Servers {
			m.queueDepth.Set(float64(queue.Depth), queue.ServerID)
			age := 0.0
			if queue.Oldest != nil {
				age = max(0, now.Sub(*queue.Oldest).Seconds())
			}
			m.queueOldest.Set(age, queue.ServerID)
		}
		m.webhookPending.Set(float64(stats.WebhookPending))
		return nil
	})

	st.OnActionFinished(func(outcome store.ActionOutcome) {
		m.actionsFinished.Inc(outcome.Code, outcome.State)
	})
	return m
}

// sweep books one sweep run: that it ran, how long it took, what it removed,
// and, when it did not fail, when.
func (m *hubMetrics) sweep(name string, started time.Time, removed int, err error) {
	m.sweepDuration.ObserveDuration(time.Since(started), name)
	if err != nil {
		m.sweepRuns.Inc(name, resultError)
		return
	}
	m.sweepRuns.Inc(name, resultOK)
	m.sweepRemoved.Add(float64(removed), name)
	m.sweepLast.Set(float64(time.Now().UnixNano())/1e9, name)
}

// MetricsHandler serves the Prometheus exposition for every path it is given.
// The reference binary mounts it on its own listener (Config.MetricsAddr);
// the main router never routes it, because the Plugin API listener is
// reachable from the internet and what the numbers say about activity is the
// operator's business.
func (s *Server) MetricsHandler() http.Handler { return s.metrics.registry.Handler() }

// metricsMux answers GET /metrics and nothing else.
func (s *Server) metricsMux() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", s.MetricsHandler())
	return mux
}

// newMetricsServer builds the server for the metrics listener, which is
// separate from Serve's listener on purpose: see MetricsHandler. The bounds are
// tight because the only legitimate client is a scraper.
func (s *Server) newMetricsServer() *http.Server {
	return &http.Server{
		Handler:           s.metricsMux(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       time.Minute,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}
}

// warnIfExposed logs when the unauthenticated metrics listener is bound
// somewhere other than loopback, which is where the documentation says to
// leave it unless the network around it is private.
func (s *Server) warnIfExposed(addr net.Addr) {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return
	}
	s.log.Warn("the metrics listener is not on loopback and has no authentication; keep it on a private network",
		"addr", addr.String())
}
