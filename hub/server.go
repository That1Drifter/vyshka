// Package hub is the reference implementation of the Vyshka hub. This file
// holds the server skeleton: configuration, routing, middleware, and lifecycle.
// Protocol surfaces (/plugin/v1, /api/v1) arrive with their own slices.
package hub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	// Close never races the loop against the store it is closing.
	stopSweeper chan struct{}
	sweeperDone chan struct{}
	closeOnce   sync.Once
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
		cfg:         cfg,
		log:         cfg.Logger,
		store:       st,
		waiters:     newWaiters(),
		holds:       newHolds(),
		started:     time.Now(),
		stopSweeper: make(chan struct{}),
		sweeperDone: make(chan struct{}),
	}
	if bootstrapToken != "" {
		s.bootstrapTokenHash = token.Hash(bootstrapToken)
	}
	s.handler = s.routes()
	go s.runMaintenance()
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
			}
		case <-prune.C:
			s.prune("events", s.store.PruneEvents)
			s.prune("audit records", s.store.PruneAudit)
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

	mux.HandleFunc("/", s.handleNotFound)

	return logRequests(s.log, mux)
}

// Serve listens and serves until ctx is cancelled, then shuts down gracefully.
func (s *Server) Serve(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.cfg.Addr, err)
	}

	httpServer := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: s.cfg.ReadHeaderTimeout,
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
		<-s.sweeperDone
		err = s.store.Close()
	})
	return err
}

// logRequests emits one structured line per request once it completes.
func logRequests(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

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
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.wroteHeader {
		return
	}
	r.status = status
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(b)
	r.written += n
	return n, err
}
