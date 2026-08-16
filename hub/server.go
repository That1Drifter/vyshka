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
	// AdminToken is the bootstrap Admin API credential. Empty means the hub
	// mints one at boot and logs it, which keeps first run to one command at
	// the cost of a token that changes on every restart.
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
}

// Server is a booted hub: an HTTP handler, its store, and its lifecycle.
type Server struct {
	cfg   Config
	log   *slog.Logger
	store *store.Store
	// adminTokenHash is the digest of the bootstrap admin token; the token
	// itself is never held in memory after boot.
	adminTokenHash string
	// waiters wakes held long-polls when something changes for their server,
	// and holds bounds how many of them one session may park at once.
	waiters *waiters
	holds   *holds
	handler http.Handler
	started time.Time
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

	adminToken := cfg.AdminToken
	if adminToken == "" {
		adminToken = token.New(token.Admin)
		cfg.Logger.Warn("no admin token configured, generated an ephemeral one",
			"adminToken", adminToken,
			"hint", "set VYSHKA_ADMIN_TOKEN (or -admin-token) to keep it stable across restarts")
	}
	cfg.AdminToken = ""

	s := &Server{
		cfg:            cfg,
		log:            cfg.Logger,
		store:          st,
		adminTokenHash: token.Hash(adminToken),
		waiters:        newWaiters(),
		holds:          newHolds(),
		started:        time.Now(),
	}
	s.handler = s.routes()
	return s, nil
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

	// Admin API: operator facing, bearer tokens (spec section 5.1).
	mux.HandleFunc("POST /api/v1/servers", s.requireAdmin(s.handleCreateServer))
	mux.HandleFunc("GET /api/v1/servers", s.requireAdmin(s.handleListServers))
	mux.HandleFunc("/api/v1/servers", methodNotAllowed("GET", "POST"))

	mux.HandleFunc("GET /api/v1/servers/{serverId}", s.requireAdmin(s.handleGetServer))
	mux.HandleFunc("/api/v1/servers/{serverId}", methodNotAllowed("GET"))

	mux.HandleFunc("POST /api/v1/servers/{serverId}/enrollment-token",
		s.requireAdmin(s.handleIssueEnrollmentToken))
	mux.HandleFunc("/api/v1/servers/{serverId}/enrollment-token", methodNotAllowed("POST"))

	mux.HandleFunc("DELETE /api/v1/servers/{serverId}/credentials",
		s.requireAdmin(s.handleRevokeCredentials))
	mux.HandleFunc("/api/v1/servers/{serverId}/credentials", methodNotAllowed("DELETE"))

	mux.HandleFunc("POST /api/v1/servers/{serverId}/envelopes",
		s.requireAdmin(s.handleQueueEnvelope))
	mux.HandleFunc("/api/v1/servers/{serverId}/envelopes", methodNotAllowed("POST"))

	mux.HandleFunc("GET /api/v1/servers/{serverId}/manifest",
		s.requireAdmin(s.handleGetManifest))
	mux.HandleFunc("/api/v1/servers/{serverId}/manifest", methodNotAllowed("GET"))

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

// Close releases resources owned by the server.
func (s *Server) Close() error { return s.store.Close() }

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
