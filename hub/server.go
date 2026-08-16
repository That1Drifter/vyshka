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
	// Logger receives structured logs. Required.
	Logger *slog.Logger
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
	if c.ReadHeaderTimeout == 0 {
		c.ReadHeaderTimeout = 10 * time.Second
	}
	if c.ShutdownTimeout == 0 {
		c.ShutdownTimeout = 15 * time.Second
	}
}

// Server is a booted hub: an HTTP handler, its store, and its lifecycle.
type Server struct {
	cfg     Config
	log     *slog.Logger
	store   *store.Store
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

	s := &Server{
		cfg:     cfg,
		log:     cfg.Logger,
		store:   st,
		started: time.Now(),
	}
	s.handler = s.routes()
	return s, nil
}

// Handler exposes the routed handler, for tests and for embedding.
func (s *Server) Handler() http.Handler { return s.handler }

// Store exposes the database handle to packages built on top of the server.
func (s *Server) Store() *store.Store { return s.store }

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
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
