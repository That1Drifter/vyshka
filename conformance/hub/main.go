// Command conformance-hub grades a running hub against the Vyshka protocol.
//
// It is deliberately a black-box client: it talks HTTP to a URL and nothing
// else, so it can grade any implementation, not just the reference hub.
//
// Usage:
//
//	go run ./conformance/hub -url http://127.0.0.1:8080 -admin-token TOKEN [-wait 30s] [-json]
//
// Exit code is 0 when every check passes, 1 when any check fails, and 2 when
// the suite could not be run at all.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	url := flag.String("url", "http://127.0.0.1:8080", "base URL of the hub under test")
	adminToken := flag.String("admin-token", os.Getenv("VYSHKA_ADMIN_TOKEN"),
		"admin token of the hub under test (env VYSHKA_ADMIN_TOKEN)")
	timeout := flag.Duration("timeout", 10*time.Second, "per-request timeout")
	wait := flag.Duration("wait", 0, "wait up to this long for the hub to become reachable before running checks")
	asJSON := flag.Bool("json", false, "emit machine-readable results")
	flag.Parse()

	// Refusing to start beats skipping checks. A suite that runs green while
	// half of it never executed is worse than no suite.
	if strings.TrimSpace(*adminToken) == "" {
		fmt.Fprintln(os.Stderr,
			"conformance: -admin-token (or VYSHKA_ADMIN_TOKEN) is required; the suite grades both API realms")
		os.Exit(2)
	}

	baseURL := strings.TrimRight(*url, "/")
	env := Env{
		BaseURL:    baseURL,
		Client:     &http.Client{Timeout: *timeout},
		AdminToken: *adminToken,
	}

	ctx := context.Background()
	if *wait > 0 {
		if err := waitForHub(ctx, env, *wait); err != nil {
			fmt.Fprintln(os.Stderr, "conformance: hub never became reachable:", err)
			os.Exit(1)
		}
	}

	results := runChecks(ctx, env, *timeout)

	failed := 0
	for _, result := range results {
		if !result.Passed {
			failed++
		}
	}

	if *asJSON {
		report := map[string]any{
			"target":  baseURL,
			"total":   len(results),
			"failed":  failed,
			"results": results,
		}
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		_ = encoder.Encode(report)
	} else {
		fmt.Printf("conformance: hub suite against %s\n\n", baseURL)
		for _, result := range results {
			status := "PASS"
			if !result.Passed {
				status = "FAIL"
			}
			fmt.Printf("%s  %-30s %s\n", status, result.ID, result.Title)
			if !result.Passed {
				fmt.Printf("      %s\n", result.Error)
			}
		}
		fmt.Printf("\n%d checks, %d failed\n", len(results), failed)
	}

	if failed > 0 {
		os.Exit(1)
	}
}

// Result is one graded check.
type Result struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Section    string `json:"section"`
	Passed     bool   `json:"passed"`
	Error      string `json:"error,omitempty"`
	DurationMs int64  `json:"durationMs"`
}

func runChecks(ctx context.Context, env Env, timeout time.Duration) []Result {
	results := make([]Result, 0, len(checks))
	for _, check := range checks {
		start := time.Now()
		checkCtx, cancel := context.WithTimeout(ctx, timeout)
		err := check.Run(checkCtx, env)
		cancel()

		result := Result{
			ID:         check.ID,
			Title:      check.Title,
			Section:    check.Section,
			Passed:     err == nil,
			DurationMs: time.Since(start).Milliseconds(),
		}
		if err != nil {
			result.Error = err.Error()
		}
		results = append(results, result)
	}
	return results
}

// waitForHub polls /healthz until it answers or the budget runs out, so CI can
// start a hub and run the suite without a sleep.
func waitForHub(ctx context.Context, env Env, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	var lastErr error
	for time.Now().Before(deadline) {
		reqCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		resp, _, err := env.get(reqCtx, "/healthz")
		cancel()
		if err == nil && resp.StatusCode == http.StatusOK {
			return nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
		}
		time.Sleep(250 * time.Millisecond)
	}
	return lastErr
}
