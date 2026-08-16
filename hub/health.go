package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// HealthResponse is the body of GET /healthz. It is deliberately small: an
// operator or a load balancer should be able to read it at a glance, and the
// conformance runner asserts on it.
type HealthResponse struct {
	Status        string         `json:"status"` // "ok" or "degraded"
	Version       string         `json:"version"`
	UptimeSeconds int64          `json:"uptimeSeconds"`
	Database      DatabaseHealth `json:"database"`
}

// DatabaseHealth reports whether the store is answering.
type DatabaseHealth struct {
	Driver        string `json:"driver"`
	OK            bool   `json:"ok"`
	SchemaVersion int    `json:"schemaVersion"`
	Error         string `json:"error,omitempty"`
}

// handleHealthz answers 200 when the hub can serve traffic and 503 when it
// cannot, so that a proxy or orchestrator can act on the status code alone.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	health := HealthResponse{
		Status:        "ok",
		Version:       Version,
		UptimeSeconds: int64(time.Since(s.started).Seconds()),
		Database:      DatabaseHealth{Driver: s.store.Driver(), OK: true},
	}

	if err := s.store.Ping(ctx); err != nil {
		health.Status = "degraded"
		health.Database.OK = false
		health.Database.Error = err.Error()
	} else if version, err := s.store.SchemaVersion(ctx); err != nil {
		health.Status = "degraded"
		health.Database.OK = false
		health.Database.Error = err.Error()
	} else {
		health.Database.SchemaVersion = version
	}

	status := http.StatusOK
	if health.Status != "ok" {
		status = http.StatusServiceUnavailable
		s.log.Warn("health check failed", "error", health.Database.Error)
	}

	writeJSON(w, status, health)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(body)
}
