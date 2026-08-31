// Package hub is the reference implementation of the Vyshka hub. This file
// holds the server skeleton: configuration, routing, middleware, and lifecycle.
// Protocol surfaces (/plugin/v1, /api/v1) arrive with their own slices.
package hub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/That1Drifter/vyshka/hub/internal/token"
	"github.com/That1Drifter/vyshka/hub/store"
)

// Version is the build version, overridden at link time with
// -ldflags "-X github.com/That1Drifter/vyshka/hub.Version=v0.1.0".
var Version = "dev"

// Config is everything the server needs to boot.
type Config struct {
	// Addr is the listen address, host:port.
	Addr string
	// DatabaseURL selects the database. Empty means the default SQLite file.
	DatabaseURL string
	// AdminToken is the bootstrap Admin API credential, which carries the
	// `admin` scope. Empty means the hub mints one at boot and logs it, but
	// only while no scoped token exists yet: see New.
	AdminToken string
	// Logger receives structured logs. Required.
	Logger *slog.Logger
	// SessionTTL bounds how long a plugin session token stays valid.
	SessionTTL time.Duration
	// EnrollmentTokenTTL is the default lifetime of a one-time enrollment
	// token when the operator does not ask for another.
	EnrollmentTokenTTL time.Duration
	// ReadHeaderTimeout bounds how long a client may take to send headers.
	ReadHeaderTimeout time.Duration
	// ReadTimeout bounds reading one request's headers and body on every
	// route: the slow-loris backstop. It does not need sizing against the
	// 60 s poll hold, because net/http clears the connection's read deadline
	// the moment the request body reaches EOF (startBackgroundRead): a
	// completed body disarms this before the handler holds anything, so the
	// timeout only ever cuts a body that is still dribbling in, or the
	// post-handler drain of one a refused or capped request left unread.
	ReadTimeout time.Duration
	// AdminBodyTimeout bounds how long an authorized Admin API mutation may
	// spend delivering its body, much tighter than ReadTimeout because admin
	// bodies are capped at 1 MiB and admin handlers hold nothing. It also
	// bounds the post-response drain behind a refusal's hang-up.
	AdminBodyTimeout time.Duration
	// WriteTimeout bounds one response at the connection: the write-side
	// mirror of ReadTimeout, against a client that stops reading its answer.
	// It is a loose backstop, not the working bound, because net/http arms it
	// at request start and nothing disarms it mid-request, so it must be
	// sized above the worst legal request (a body trickled to ReadTimeout,
	// then the full 60 s poll hold) or it would cut held polls at the
	// deadline. The tight bound is ResponseWriteTimeout; this one covers the
	// writes that bound cannot reach because no handler makes them (net/http's
	// own error answers, its 100-continue interim line) and any future path
	// that skips the logging middleware. Negative disables it.
	WriteTimeout time.Duration
	// ResponseWriteTimeout bounds a response's write-side progress. It is
	// armed when the first header is written rather than at request start,
	// so it needs no sizing against the poll hold (the hold ends before the
	// response begins), and re-armed at every written chunk of the body
	// (responseWriteChunk) with an allowance for that chunk's size at
	// responseByteRateFloor, so it needs no sizing against the largest legal
	// response either: a big page over a slow link earns time chunk by
	// chunk, while a client that stops reading is cut within one chunk's
	// budget of its last accepted byte. It cannot poison the next request on
	// a keep-alive connection, because net/http clears the connection's
	// write deadline after every response it finishes. Keep it comfortably
	// above a second: the final buffered bytes flush after the last chunk's
	// arm, on that chunk's budget. Negative disables it, leaving responses
	// to WriteTimeout alone.
	ResponseWriteTimeout time.Duration
	// IdleTimeout bounds how long a keep-alive connection may sit idle
	// between requests. Set explicitly, because net/http would otherwise
	// reuse ReadTimeout for it and idle keep-alives would ride whatever that
	// gets tuned to. Negative disables it.
	IdleTimeout time.Duration
	// MaxConns caps concurrent accepted connections. Every other bound here
	// is per connection, so without a cap an attacker multiplies whatever a
	// connection costs (roughly two goroutines plus buffers) by as many
	// connections as the host will give it. At the cap, further connections
	// wait unaccepted in the kernel's backlog until a slot frees. The cap is
	// global rather than per source IP: the reference deployment puts a
	// reverse proxy in front of public traffic (spec section 3.3), and that
	// is where per-client fairness belongs. Negative disables it.
	MaxConns int
	// ShutdownTimeout bounds graceful shutdown before connections are cut.
	ShutdownTimeout time.Duration
	// EventRetention decides how long an ingested event is kept, by type
	// pattern (spec section 8.4). Nil means DefaultEventRetention. Retention is
	// hub configuration, not protocol: a hub is conformant whatever it keeps.
	EventRetention []RetentionRule
	// AuditRetention decides how long an audit record is kept (spec section
	// 10). Zero means DefaultAuditRetention.
	AuditRetention time.Duration
	// RetentionInterval is how often the retention passes run, for events and
	// for audit records alike.
	RetentionInterval time.Duration
	// WebhookRetryDelays are the waits between a failed webhook delivery
	// attempt and the next one (spec section 11.5); total attempts are one more
	// than the number of delays. Nil means the reference schedule. The first
	// delay is clamped to 60 s because the protocol requires the first retry
	// within that window, whatever the configuration says.
	WebhookRetryDelays []time.Duration
	// WebhookDeliveryTimeout bounds one delivery attempt end to end.
	WebhookDeliveryTimeout time.Duration
	// WebhookDeliveryRetention is how long delivered and dead deliveries stay
	// readable before the retention pass prunes them.
	WebhookDeliveryRetention time.Duration
	// StateSnapshotRetention is how long a state snapshot stays in history
	// (spec section 8.3). The latest snapshot per (server, type) outlives it.
	StateSnapshotRetention time.Duration
	// StateHistoryDepth bounds how many snapshots per (server, type) are kept,
	// enforced at insert so a fast-pushing plugin cannot outrun the retention
	// pass. Zero means the default; the latest snapshot always survives.
	StateHistoryDepth int
}

func (c *Config) withDefaults() {
	if c.Addr == "" {
		c.Addr = "127.0.0.1:8080"
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.SessionTTL == 0 {
		c.SessionTTL = time.Hour
	}
	if c.EnrollmentTokenTTL == 0 {
		c.EnrollmentTokenTTL = 24 * time.Hour
	}
	if c.ReadHeaderTimeout == 0 {
		c.ReadHeaderTimeout = 10 * time.Second
	}
	// 30 s from the connection's first byte, headers and body under one
	// deadline: a client that spends the whole 10 s header budget still
	// leaves a 1 MiB body 20 s (about 50 KiB/s); a prompt one leaves it
	// nearly the full 30. Held polls are immune whatever this says: a
	// completed body disarms the deadline (see the field comment), so this
	// never needs to clear the hold. Negative disables it, matching what
	// http.Server makes of a non-positive value, for an embedder whose
	// plugins sit behind links slower than the default assumes.
	if c.ReadTimeout == 0 {
		c.ReadTimeout = 30 * time.Second
	}
	// 15 s moves the 1 MiB cap at about 70 KiB/s, generous for an operator
	// panel and half of what the backstop would let a trickled admin body
	// hold. Negative disables it, leaving those bodies to ReadTimeout.
	if c.AdminBodyTimeout == 0 {
		c.AdminBodyTimeout = 15 * time.Second
	}
	// Derived from the effective read bound, not from the default one: a
	// body may legally dribble until ReadTimeout fires (measured from the
	// connection's first byte, so at most ReadTimeout past where net/http
	// arms this deadline), the hold may then run its full 60 s, and the
	// response still has to go out, so the backstop is ReadTimeout plus the
	// hold plus 30 s of write headroom: 120 s under the defaults, and still
	// above the slowest conforming poll when an operator raises ReadTimeout
	// for a slow link. An operator who disables ReadTimeout has declared the
	// read side may legally take forever, and no finite write backstop is
	// safe against a hold that begins arbitrarily late, so the default
	// follows it off; either can still be set explicitly.
	if c.WriteTimeout == 0 {
		if c.ReadTimeout < 0 {
			c.WriteTimeout = -1
		} else {
			c.WriteTimeout = c.ReadTimeout + MaxPollTimeout + 30*time.Second
			if c.WriteTimeout < c.ReadTimeout {
				// A ReadTimeout within 90 s of the duration ceiling wraps
				// the sum negative, which would silently disable the
				// backstop; pin it to the ceiling instead.
				c.WriteTimeout = math.MaxInt64
			}
		}
	}
	// 10 s of progress bound per written chunk, plus the size allowance
	// described on the field: enough that no legal response is ever cut for
	// being large, while a client that stops reading holds the connection
	// seconds instead of the minutes WriteTimeout would allow it.
	if c.ResponseWriteTimeout == 0 {
		c.ResponseWriteTimeout = 10 * time.Second
	}
	// Two minutes comfortably spans the gap between a plugin's back-to-back
	// polls and a panel's sporadic requests.
	if c.IdleTimeout == 0 {
		c.IdleTimeout = 2 * time.Minute
	}
	// Generous for a single-operator hub: one plugin holds one poll
	// connection and a panel a handful, so 256 is an order of magnitude of
	// headroom before refusal, while still refusing at a ceiling the host
	// survives.
	if c.MaxConns == 0 {
		c.MaxConns = 256
	}
	if c.ShutdownTimeout == 0 {
		c.ShutdownTimeout = 15 * time.Second
	}
	if c.EventRetention == nil {
		c.EventRetention = DefaultEventRetention()
	}
	// Clamped rather than merely defaulted: a negative retention would stamp
	// every audit record as already expired, and the next prune pass would
	// delete the log as fast as it was written.
	if c.AuditRetention <= 0 {
		c.AuditRetention = DefaultAuditRetention
	}
	// Not just the zero value: Config is exported, and a negative interval here
	// would panic time.NewTicker inside the maintenance goroutine, taking the
	// process down moments after New returned successfully.
	if c.RetentionInterval <= 0 {
		c.RetentionInterval = 5 * time.Minute
	}
	// The retry schedule is sanitized rather than trusted, because section 11.5
	// makes promises no configuration may opt the hub out of: a failed delivery
	// is retried at least once, delays are never negative, and the first retry
	// is scheduled within 60 s (clamped to 50 s here, leaving the dispatcher's
	// own cadence inside the bound).
	if len(c.WebhookRetryDelays) == 0 {
		// Nil and empty alike: the reference schedule of spec section 11.5,
		// five attempts over roughly a quarter hour.
		c.WebhookRetryDelays = []time.Duration{
			10 * time.Second, time.Minute, 4 * time.Minute, 10 * time.Minute,
		}
	}
	if c.WebhookRetryDelays[0] < 0 {
		c.WebhookRetryDelays[0] = 0
	}
	if c.WebhookRetryDelays[0] > 50*time.Second {
		c.WebhookRetryDelays[0] = 50 * time.Second
	}
	// Section 11.5 promises increasing delays; a configured schedule that dips
	// is raised to monotone rather than honored.
	for i := 1; i < len(c.WebhookRetryDelays); i++ {
		if c.WebhookRetryDelays[i] < c.WebhookRetryDelays[i-1] {
			c.WebhookRetryDelays[i] = c.WebhookRetryDelays[i-1]
		}
	}
	if c.WebhookDeliveryTimeout <= 0 {
		c.WebhookDeliveryTimeout = 10 * time.Second
	}
	if c.WebhookDeliveryRetention <= 0 {
		c.WebhookDeliveryRetention = 7 * 24 * time.Hour
	}
	// The reference window of spec section 8.4: snapshots are kept a day.
	if c.StateSnapshotRetention <= 0 {
		c.StateSnapshotRetention = 24 * time.Hour
	}
	if c.StateHistoryDepth <= 0 {
		c.StateHistoryDepth = 500
	}
}

// Server is a booted hub: an HTTP handler, its store, and its lifecycle.
type Server struct {
	cfg   Config
	log   *slog.Logger
	store *store.Store
	// bootstrapTokenHash is the digest of the bootstrap admin token; the token
	// itself is never held in memory after boot. Empty means this hub has no
	// bootstrap credential and authenticates only its minted tokens.
	bootstrapTokenHash string
	// waiters wakes held long-polls when something changes for their server,
	// and holds bounds how many of them one session may park at once.
	waiters *waiters
	holds   *holds
	handler http.Handler
	started time.Time
	// stopSweeper ends the maintenance loop; sweeperDone confirms it ended, so
	// Close never races the loop against the store it is closing. The webhook
	// dispatcher shares the stop channel and confirms through dispatcherDone.
	stopSweeper chan struct{}
	sweeperDone chan struct{}
	// webhookWake nudges the webhook dispatcher when fresh work landed;
	// webhookClient makes its delivery attempts.
	webhookWake    chan struct{}
	dispatcherDone chan struct{}
	webhookClient  *http.Client
	// baseCtx parents every in-flight delivery attempt; Close cancels it so
	// shutdown never waits out a slow webhook target's timeout.
	baseCtx    context.Context
	baseCancel context.CancelFunc
	closeOnce  sync.Once
}

// New opens the store, runs migrations, and builds the HTTP handler. The caller
// owns the returned server and must Close it.
func New(ctx context.Context, cfg Config) (*Server, error) {
	cfg.withDefaults()

	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}

	applied, err := st.Migrate(ctx)
	if err != nil {
		st.Close()
		return nil, err
	}
	version, err := st.SchemaVersion(ctx)
	if err != nil {
		st.Close()
		return nil, err
	}
	cfg.Logger.Info("database ready",
		"driver", st.Driver(),
		"target", st.Target(),
		"schemaVersion", version,
		"migrationsApplied", len(applied),
	)

	// The bootstrap credential of spec section 10, confined to first run.
	//
	// A configured token is always honoured: operators put it in a unit file
	// and CI puts it in an env var, and it is the documented way back in after
	// every minted token has been revoked. What is confined is the *generated*
	// one. Minting a fresh `admin` token and printing it on every boot would
	// make the whole tokens table decorative, because revoking a token would
	// not survive a restart and anyone who could read the log would hold
	// permanent superuser.
	//
	// The condition is specifically whether a live token can still *manage
	// tokens*, not whether any live token exists. A hub whose only credential
	// is a `servers:read` panel token has nothing that can mint or revoke, so
	// suppressing the bootstrap there would lock the operator out of their own
	// Admin API on the next restart, with no way back that does not involve
	// the host.
	bootstrapToken := cfg.AdminToken
	cfg.AdminToken = ""
	if bootstrapToken == "" {
		live, err := st.LiveAdminTokens(ctx)
		if err != nil {
			st.Close()
			return nil, err
		}
		administrators := 0
		for _, stored := range live {
			for _, text := range stored.Scopes {
				if scope, err := ParseScope(text); err == nil && scope.Resource == resourceAdmin {
					administrators++
					break
				}
			}
		}
		if administrators > 0 {
			cfg.Logger.Info("no bootstrap admin token; this hub authenticates with its scoped tokens only",
				"liveTokens", len(live), "withAdminScope", administrators,
				"hint", "set VYSHKA_ADMIN_TOKEN (or -admin-token) to restore a bootstrap credential")
		} else {
			bootstrapToken = token.New(token.Admin)
			cfg.Logger.Warn("no admin token configured and none can manage tokens, generated an ephemeral one",
				"adminToken", bootstrapToken, "liveTokens", len(live),
				"hint", "set VYSHKA_ADMIN_TOKEN (or -admin-token) to keep it stable across restarts")
		}
	}

	s := &Server{
		cfg:            cfg,
		log:            cfg.Logger,
		store:          st,
		waiters:        newWaiters(),
		holds:          newHolds(),
		started:        time.Now(),
		stopSweeper:    make(chan struct{}),
		sweeperDone:    make(chan struct{}),
		webhookWake:    make(chan struct{}, 1),
		dispatcherDone: make(chan struct{}),
		webhookClient: &http.Client{
			Timeout: cfg.WebhookDeliveryTimeout,
			// A 3xx is handed back as the final answer, never followed: a
			// redirect would resend the signed body to an address nobody
			// registered and no audit names (spec section 11.3).
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
	s.baseCtx, s.baseCancel = context.WithCancel(context.Background())
	if bootstrapToken != "" {
		s.bootstrapTokenHash = token.Hash(bootstrapToken)
	}
	s.handler = s.routes()
	go s.runMaintenance()
	go s.runWebhookDispatcher()
	return s, nil
}

// pruneBatch bounds one retention pass, so a long-neglected database is
// drained over several passes instead of in one statement that would hold the
// store's single connection for the duration.
const pruneBatch = 5000

// runMaintenance owns the hub's background jobs: action expiry and the
// retention passes. They share a goroutine because they share a shutdown, and
// Close waits on exactly one loop rather than on a set it has to keep in sync.
func (s *Server) runMaintenance() {
	defer close(s.sweeperDone)

	// The expiry job of spec section 7: every non-terminal action past its
	// deadline flips to expired. Reads also expire lazily, so this ticker
	// exists for the states nobody is currently reading, which are exactly the
	// ones an operator will ask about later.
	expiry := time.NewTicker(500 * time.Millisecond)
	defer expiry.Stop()
	// The retention jobs of spec sections 8.4 and 10, both measured in days and
	// so with nothing to gain from running often.
	prune := time.NewTicker(s.cfg.RetentionInterval)
	defer prune.Stop()

	for {
		select {
		case <-s.stopSweeper:
			return
		case <-expiry.C:
			// Bounded, because Close waits for this loop before closing the
			// store: a job that could block forever could hang shutdown.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			expired, err := s.store.ExpireActions(ctx)
			cancel()
			if err != nil {
				s.log.Error("action expiry sweep failed", "error", err.Error())
				continue
			}
			if expired > 0 {
				s.log.Info("actions expired", "count", expired)
				// Expiry is a terminal state, and terminal states owe the
				// action.completed webhooks a notification (spec section 11.1).
				s.nudgeWebhooks()
			}
		case <-prune.C:
			s.prune("events", s.store.PruneEvents)
			s.prune("audit records", s.store.PruneAudit)
			s.prune("kv entries", s.store.PruneKV)
			s.prune("state snapshots", s.store.PruneSnapshots)
			s.prune("webhook deliveries", func(ctx context.Context, limit int) (int, error) {
				cutoff := time.Now().UTC().Add(-s.cfg.WebhookDeliveryRetention)
				return s.store.PruneWebhookDeliveries(ctx, cutoff, limit)
			})
		}
	}
}

// prune runs one retention pass, in bounded batches, until it stops finding
// work or the pass has taken long enough that the next tick can pick up where
// it left off. Nothing is lost by stopping early: expired rows stay expired.
func (s *Server) prune(what string, delete func(context.Context, int) (int, error)) {
	deadline := time.Now().Add(30 * time.Second)
	total := 0
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		pruned, err := delete(ctx, pruneBatch)
		cancel()
		if err != nil {
			s.log.Error("retention pass failed", "what", what, "error", err.Error())
			return
		}
		total += pruned
		if pruned < pruneBatch {
			break
		}
		select {
		case <-s.stopSweeper:
			return
		default:
		}
	}
	if total > 0 {
		s.log.Info("retention pass pruned rows", "what", what, "count", total)
	}
}

// Handler exposes the routed handler, for tests and for embedding.
func (s *Server) Handler() http.Handler { return s.handler }

// Store exposes the database handle to packages built on top of the server.
func (s *Server) Store() *store.Store { return s.store }

// routes wires both realms. Every path is registered twice: once per method it
// answers, and once without a method, which ServeMux treats as the less
// specific pattern and therefore only reaches on a method mismatch. That is
// what turns net/http's plain-text 405 into the protocol error shape.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("/healthz", methodNotAllowed("GET"))

	// Admin API: operator facing, scoped bearer tokens (spec sections 5.1, 10).
	// The scope on each route is the coarse gate; the routes whose answer
	// depends on a value in the request check that value again inside the
	// handler.
	//
	// Creating a server, issuing an enrollment token, revoking credentials, and
	// queueing a raw envelope all require `admin`. The first three are server
	// enrollment, which section 10 assigns to `admin`; the fourth is a raw
	// write channel to the game server that no narrower scope in the grammar
	// describes, and defaulting it to something weaker would let a scoped token
	// route around the validation the modelled endpoints perform.
	mux.HandleFunc("POST /api/v1/servers", s.admin(resourceAdmin, "", s.handleCreateServer))
	mux.HandleFunc("GET /api/v1/servers", s.admin(resourceServers, verbRead, s.handleListServers))
	mux.HandleFunc("/api/v1/servers", methodNotAllowed("GET", "POST"))

	mux.HandleFunc("GET /api/v1/servers/{serverId}",
		s.admin(resourceServers, verbRead, s.handleGetServer))
	mux.HandleFunc("/api/v1/servers/{serverId}", methodNotAllowed("GET"))

	mux.HandleFunc("POST /api/v1/servers/{serverId}/enrollment-token",
		s.admin(resourceAdmin, "", s.handleIssueEnrollmentToken))
	mux.HandleFunc("/api/v1/servers/{serverId}/enrollment-token", methodNotAllowed("POST"))

	mux.HandleFunc("DELETE /api/v1/servers/{serverId}/credentials",
		s.admin(resourceAdmin, "", s.handleRevokeCredentials))
	mux.HandleFunc("/api/v1/servers/{serverId}/credentials", methodNotAllowed("DELETE"))

	mux.HandleFunc("POST /api/v1/servers/{serverId}/envelopes",
		s.admin(resourceAdmin, "", s.handleQueueEnvelope))
	mux.HandleFunc("/api/v1/servers/{serverId}/envelopes", methodNotAllowed("POST"))

	mux.HandleFunc("GET /api/v1/servers/{serverId}/manifest",
		s.admin(resourceServers, verbRead, s.handleGetManifest))
	mux.HandleFunc("/api/v1/servers/{serverId}/manifest", methodNotAllowed("GET"))

	mux.HandleFunc("POST /api/v1/servers/{serverId}/actions",
		s.admin(resourceActions, verbDispatch, s.handleDispatchAction))
	mux.HandleFunc("/api/v1/servers/{serverId}/actions", methodNotAllowed("POST"))

	mux.HandleFunc("GET /api/v1/actions/{actionId}",
		s.admin(resourceActions, verbRead, s.handleGetAction))
	mux.HandleFunc("/api/v1/actions/{actionId}", methodNotAllowed("GET"))

	mux.HandleFunc("GET /api/v1/servers/{serverId}/events",
		s.admin(resourceEvents, verbRead, s.handleListEvents))
	mux.HandleFunc("/api/v1/servers/{serverId}/events", methodNotAllowed("GET"))

	// State snapshots (spec section 8.3), behind servers:read like the rest of
	// a server's own record.
	mux.HandleFunc("GET /api/v1/servers/{serverId}/state/{stateType}",
		s.admin(resourceServers, verbRead, s.handleGetState))
	mux.HandleFunc("/api/v1/servers/{serverId}/state/{stateType}", methodNotAllowed("GET"))

	mux.HandleFunc("GET /api/v1/servers/{serverId}/state/{stateType}/history",
		s.admin(resourceServers, verbRead, s.handleGetStateHistory))
	mux.HandleFunc("/api/v1/servers/{serverId}/state/{stateType}/history", methodNotAllowed("GET"))

	// Token management and the audit log (spec section 10), all `admin`: a
	// token that could mint tokens could grant itself anything, and the audit
	// log names every credential and everything each one changed.
	mux.HandleFunc("POST /api/v1/tokens", s.admin(resourceAdmin, "", s.handleCreateToken))
	mux.HandleFunc("GET /api/v1/tokens", s.admin(resourceAdmin, "", s.handleListTokens))
	mux.HandleFunc("/api/v1/tokens", methodNotAllowed("GET", "POST"))

	mux.HandleFunc("DELETE /api/v1/tokens/{tokenId}",
		s.admin(resourceAdmin, "", s.handleRevokeToken))
	mux.HandleFunc("/api/v1/tokens/{tokenId}", methodNotAllowed("DELETE"))

	mux.HandleFunc("GET /api/v1/audit", s.admin(resourceAdmin, "", s.handleListAudit))
	mux.HandleFunc("/api/v1/audit", methodNotAllowed("GET"))

	// Webhooks (spec section 11), all behind webhooks:manage: registration can
	// aim signed POSTs at anything the hub can reach, and the delivery record
	// names every target, so reading and writing carry the same weight here.
	mux.HandleFunc("POST /api/v1/webhooks", s.admin(resourceWebhooks, verbManage, s.handleCreateWebhook))
	mux.HandleFunc("GET /api/v1/webhooks", s.admin(resourceWebhooks, verbManage, s.handleListWebhooks))
	mux.HandleFunc("/api/v1/webhooks", methodNotAllowed("GET", "POST"))

	mux.HandleFunc("DELETE /api/v1/webhooks/{webhookId}",
		s.admin(resourceWebhooks, verbManage, s.handleDeleteWebhook))
	mux.HandleFunc("/api/v1/webhooks/{webhookId}", methodNotAllowed("DELETE"))

	mux.HandleFunc("GET /api/v1/webhooks/{webhookId}/deliveries",
		s.admin(resourceWebhooks, verbManage, s.handleListWebhookDeliveries))
	mux.HandleFunc("/api/v1/webhooks/{webhookId}/deliveries", methodNotAllowed("GET"))

	// The key/value store (spec section 12), the same operations on both
	// realms. The namespace lives in the path, so the exact kv:rw:{namespace}
	// check runs in the gate at the headers, before any body is read; adminKV
	// keeps its own requireScope behind it as the belt if a route is ever
	// rewired without the path-scoped gate.
	kvNamespace := func(r *http.Request) string { return r.PathValue("namespace") }
	mux.HandleFunc("GET /api/v1/kv/{namespace}/{key}", s.adminPathScoped(resourceKV, verbRW, kvNamespace, s.adminKV(kvGet)))
	mux.HandleFunc("PUT /api/v1/kv/{namespace}/{key}", s.adminPathScoped(resourceKV, verbRW, kvNamespace, s.adminKV(kvSet)))
	mux.HandleFunc("DELETE /api/v1/kv/{namespace}/{key}", s.adminPathScoped(resourceKV, verbRW, kvNamespace, s.adminKV(kvDelete)))
	mux.HandleFunc("/api/v1/kv/{namespace}/{key}", methodNotAllowed("GET", "PUT", "DELETE"))

	mux.HandleFunc("POST /api/v1/kv/{namespace}/{key}/incr", s.adminPathScoped(resourceKV, verbRW, kvNamespace, s.adminKV(kvIncr)))
	mux.HandleFunc("/api/v1/kv/{namespace}/{key}/incr", methodNotAllowed("POST"))

	// Plugin API: game-server facing, a separate credential realm entirely
	// (spec sections 5.2 and 5.3).
	mux.HandleFunc("POST /plugin/v1/enroll", s.handleEnroll)
	mux.HandleFunc("/plugin/v1/enroll", methodNotAllowed("POST"))

	mux.HandleFunc("POST /plugin/v1/session", s.handleCreateSession)
	mux.HandleFunc("GET /plugin/v1/session", s.handleGetSession)
	mux.HandleFunc("/plugin/v1/session", methodNotAllowed("GET", "POST"))

	// The transport heartbeat (spec section 3.1.2).
	mux.HandleFunc("POST /plugin/v1/poll", s.handlePoll)
	mux.HandleFunc("/plugin/v1/poll", methodNotAllowed("POST"))

	// The plugin side of the key/value store, confined inside pluginKV to the
	// namespaces the server's stored manifest declares (spec section 12.3).
	mux.HandleFunc("GET /plugin/v1/kv/{namespace}/{key}", s.pluginKV(kvGet))
	mux.HandleFunc("PUT /plugin/v1/kv/{namespace}/{key}", s.pluginKV(kvSet))
	mux.HandleFunc("DELETE /plugin/v1/kv/{namespace}/{key}", s.pluginKV(kvDelete))
	mux.HandleFunc("/plugin/v1/kv/{namespace}/{key}", methodNotAllowed("GET", "PUT", "DELETE"))

	mux.HandleFunc("POST /plugin/v1/kv/{namespace}/{key}/incr", s.pluginKV(kvIncr))
	mux.HandleFunc("/plugin/v1/kv/{namespace}/{key}/incr", methodNotAllowed("POST"))

	mux.HandleFunc("/", s.handleNotFound)

	return logRequests(s.log, s.cfg.ResponseWriteTimeout, mux)
}

// Serve listens and serves until ctx is cancelled, then shuts down gracefully.
func (s *Server) Serve(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.cfg.Addr, err)
	}
	if s.cfg.MaxConns > 0 {
		listener = capConnections(listener, s.cfg.MaxConns)
	}

	httpServer := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: s.cfg.ReadHeaderTimeout,
		ReadTimeout:       s.cfg.ReadTimeout,
		WriteTimeout:      s.cfg.WriteTimeout,
		IdleTimeout:       s.cfg.IdleTimeout,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}

	serveErr := make(chan error, 1)
	go func() {
		s.log.Info("hub listening", "addr", listener.Addr().String(), "version", Version)
		serveErr <- httpServer.Serve(listener)
	}()

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		s.log.Info("shutting down", "timeout", s.cfg.ShutdownTimeout.String())
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.ShutdownTimeout)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("graceful shutdown: %w", err)
		}
		return nil
	}
}

// Close releases resources owned by the server. It is idempotent: a test that
// closes a hub explicitly to restart it will be closed again by its cleanup.
func (s *Server) Close() error {
	var err error
	s.closeOnce.Do(func() {
		close(s.stopSweeper)
		s.baseCancel()
		<-s.sweeperDone
		<-s.dispatcherDone
		err = s.store.Close()
	})
	return err
}

// responseByteRateFloor is the slowest reading client a response write budget
// assumes, matching the roughly 50 KiB/s the read side's defaults grant a
// trickled request body. Each chunk a response writes earns its size at this
// rate on top of ResponseWriteTimeout, which is what keeps a legal
// multi-megabyte page (a deep state history, a poll draining raw-queued
// envelopes) deliverable over a slow link while a stalled reader still costs
// seconds.
const responseByteRateFloor = 50 << 10

// responseWriteChunk bounds how many bytes ride one armed deadline. Handlers
// hand writeJSON's encoder one Write per response however large, and the TCP
// stack completes one Write under one deadline with no callback as bytes make
// progress, so without chunking a size-proportional budget would be a
// total-transfer allowance: a reader that stalled at the first byte of a
// 200 MiB legal response would hold its connection for the whole hour the
// size earned. Splitting at the recorder means each chunk gets only its own
// budget (ResponseWriteTimeout plus about 5 s here), so a stall is cut within
// one chunk's budget of the last accepted byte, and the arming arithmetic
// cannot overflow whatever size a handler writes.
const responseWriteChunk = 256 << 10

// logRequests emits one structured line per request once it completes. It also
// owns arming ResponseWriteTimeout, because it wraps every route and its
// recorder sees every header and body write of every response: arming at the
// first header, rather than at request start, is what lets the bound stay
// tight without ever needing sizing against the poll hold, and re-arming per
// written chunk is what keeps it a progress bound rather than a cap on
// response size. Each response on a keep-alive connection arms fresh
// deadlines for itself.
func logRequests(log *slog.Logger, responseWriteTimeout time.Duration, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		if responseWriteTimeout > 0 {
			recorder.armWrite = func(pending int) {
				// Unsupported writers (tests driving the handler directly)
				// are left to the server-wide WriteTimeout behind this.
				budget := responseWriteTimeout +
					time.Duration(pending)*time.Second/responseByteRateFloor
				_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(budget))
			}
		}

		next.ServeHTTP(recorder, r)

		log.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", recorder.status,
			"bytes", recorder.written,
			"durationMs", time.Since(start).Milliseconds(),
			"remote", r.RemoteAddr,
		)
	})
}

// statusRecorder remembers what the handler actually sent.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	written     int
	wroteHeader bool
	// armWrite, when set, runs as the first header is written and again
	// before every written chunk of the body, carrying that chunk's size:
	// the hook logRequests uses to keep a progress deadline on the response
	// from the moment it begins. The admin middleware's inner recorder
	// leaves it nil and the hook still runs, because that recorder delegates
	// its writes to this one.
	armWrite func(pending int)
}

// Unwrap lets http.ResponseController reach the connection through this
// wrapper (and through logRequests' instance of it), which is how the admin
// middleware sets its per-request read deadline.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *statusRecorder) WriteHeader(status int) {
	if r.wroteHeader {
		return
	}
	r.status = status
	r.wroteHeader = true
	if r.armWrite != nil {
		r.armWrite(0)
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	if r.armWrite == nil {
		n, err := r.ResponseWriter.Write(b)
		r.written += n
		return n, err
	}
	total := 0
	for len(b) > 0 {
		chunk := b
		if len(chunk) > responseWriteChunk {
			chunk = chunk[:responseWriteChunk]
		}
		r.armWrite(len(chunk))
		n, err := r.ResponseWriter.Write(chunk)
		total += n
		r.written += n
		if err != nil {
			return total, err
		}
		b = b[len(chunk):]
	}
	return total, nil
}

// capConnections bounds how many accepted connections may be open at once: the
// MaxConns cap. A slot is taken before the inner Accept and returned when the
// accepted connection closes, so at the cap the listener simply stops
// accepting and excess connections queue in the kernel's backlog instead of
// each buying goroutines and buffers. This is hand-rolled rather than a
// dependency because it is the whole of what the dependency would bring.
func capConnections(inner net.Listener, capacity int) net.Listener {
	return &capListener{
		Listener: inner,
		slots:    make(chan struct{}, capacity),
		closed:   make(chan struct{}),
	}
}

type capListener struct {
	net.Listener
	slots     chan struct{}
	closeOnce sync.Once
	closed    chan struct{}
}

func (l *capListener) Accept() (net.Conn, error) {
	select {
	case l.slots <- struct{}{}:
	case <-l.closed:
		// Closing the listener must unblock an Accept parked on a full house,
		// or shutdown would hang behind the very connections it is draining.
		// The inner Accept on a closed listener fails without blocking; a
		// connection racing in anyway is closed, not leaked.
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		conn.Close()
		return nil, net.ErrClosed
	}
	conn, err := l.Listener.Accept()
	if err != nil {
		<-l.slots
		return nil, err
	}
	return &capConn{Conn: conn, release: func() { <-l.slots }}, nil
}

func (l *capListener) Close() error {
	err := l.Listener.Close()
	l.closeOnce.Do(func() { close(l.closed) })
	return err
}

// capConn returns its listener slot exactly once, on first Close. net/http
// closes every connection it serves, on every exit path including a panicking
// handler, so a slot cannot leak short of a leaked connection.
type capConn struct {
	net.Conn
	releaseOnce sync.Once
	release     func()
}

func (c *capConn) Close() error {
	err := c.Conn.Close()
	c.releaseOnce.Do(c.release)
	return err
}

// CloseWrite forwards the half-close net/http reaches for when it closes a
// connection that still has unread body bytes (closeWriteAndWait): FIN first,
// then the full close, so the queued response is not in an RST's blast radius
// (golang.org/issue/3595). Embedding the net.Conn interface would otherwise
// strip the method off the wrapped *net.TCPConn, and the refusal path's
// bounded drain depends on it.
func (c *capConn) CloseWrite() error {
	if half, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return half.CloseWrite()
	}
	return fmt.Errorf("connection %T does not support half-close", c.Conn)
}
