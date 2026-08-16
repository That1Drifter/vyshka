package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
)

// Check is one black-box assertion against a hub. Checks only ever speak HTTP
// to the target URL: nothing here may import hub code, or the suite would stop
// grading third-party hubs.
type Check struct {
	ID    string
	Title string
	// Section cites the clause of spec/protocol.md a check enforces, or the
	// slice that introduced it when the behavior is not protocol text.
	Section string
	Run     func(ctx context.Context, env Env) error
}

// Env is what a check gets to work with.
type Env struct {
	BaseURL string
	Client  *http.Client
}

// get issues a GET against the target hub and returns the response body.
func (e Env) get(ctx context.Context, path string) (*http.Response, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.BaseURL+path, nil)
	if err != nil {
		return nil, nil, err
	}
	resp, err := e.Client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, nil, fmt.Errorf("read body: %w", err)
	}
	return resp, body, nil
}

// checks is the full suite, in execution order.
var checks = []Check{
	{
		ID:      "health.responds",
		Title:   "GET /healthz answers 200 with JSON",
		Section: "prefactor",
		Run: func(ctx context.Context, env Env) error {
			resp, body, err := env.get(ctx, "/healthz")
			if err != nil {
				return err
			}
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("want status 200, got %d, body %q", resp.StatusCode, truncate(body))
			}
			contentType := resp.Header.Get("Content-Type")
			mediaType, _, err := mime.ParseMediaType(contentType)
			if err != nil {
				return fmt.Errorf("parse Content-Type %q: %w", contentType, err)
			}
			if mediaType != "application/json" {
				return fmt.Errorf("want Content-Type application/json, got %q", contentType)
			}
			return nil
		},
	},
	{
		ID:      "health.status",
		Title:   "GET /healthz reports status ok",
		Section: "prefactor",
		Run: func(ctx context.Context, env Env) error {
			_, body, err := env.get(ctx, "/healthz")
			if err != nil {
				return err
			}
			var health struct {
				Status  string `json:"status"`
				Version string `json:"version"`
			}
			if err := json.Unmarshal(body, &health); err != nil {
				return fmt.Errorf("decode body %q: %w", truncate(body), err)
			}
			if health.Status != "ok" {
				return fmt.Errorf(`want status "ok", got %q`, health.Status)
			}
			if strings.TrimSpace(health.Version) == "" {
				return fmt.Errorf("version is empty")
			}
			return nil
		},
	},
}

func truncate(body []byte) string {
	const limit = 200
	if len(body) <= limit {
		return string(body)
	}
	return string(body[:limit]) + "..."
}
