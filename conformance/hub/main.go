// Command conformance-hub grades a running hub against the Vyshka protocol.
//
// It is deliberately a black-box client: it talks HTTP to a URL and nothing
// else, so it can grade any implementation, not just the reference hub.
//
// Usage:
//
//	go run ./conformance/hub -url http://127.0.0.1:8080 -admin-token TOKEN [-wait 30s] [-json]
//	go run ./conformance/hub -url https://hub.example:8443 -ca-file root.pem -connect-to 127.0.0.1:8443 ...
//
// Exit code is 0 when every check passes, 1 when any check fails, and 2 when
// the suite could not be run at all.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

func main() {
	url := flag.String("url", "http://127.0.0.1:8080", "base URL of the hub under test")
	adminToken := flag.String("admin-token", os.Getenv("VYSHKA_ADMIN_TOKEN"),
		"admin token of the hub under test (env VYSHKA_ADMIN_TOKEN)")
	timeout := flag.Duration("timeout", 10*time.Second, "per-request timeout for everything but a long-poll")
	checkTimeout := flag.Duration("check-timeout", 90*time.Second, "budget for one check, which may hold several long-polls")
	wait := flag.Duration("wait", 0, "wait up to this long for the hub to become reachable before running checks")
	webhookListen := flag.String("webhook-listen", "127.0.0.1:0",
		"address the webhook checks bind their receiver on; the hub under test must be able to reach it")
	webhookAdvertise := flag.String("webhook-advertise", "",
		"base URL the hub is told to deliver to, when it differs from the bound address (containers, tunnels)")
	stateHistoryDepth := flag.Int("state-history-depth", 500,
		"snapshot history depth the hub under test is configured with (reference: 500); the state replay-after-prune check pushes one snapshot more than this to force a history trim")
	asJSON := flag.Bool("json", false, "emit machine-readable results")
	caFile := flag.String("ca-file", "",
		"PEM file of roots to trust for an https hub, on top of the system's (a hub with a private or test CA)")
	connectTo := flag.String("connect-to", "",
		"host:port to dial for the hub instead of the URL's host; TLS still verifies the URL's name (a name with no DNS record here)")
	only := flag.String("run", "", "run only the checks whose id matches this regular expression")
	flag.Parse()

	// Refusing to start beats skipping checks. A suite that runs green while
	// half of it never executed is worse than no suite.
	if strings.TrimSpace(*adminToken) == "" {
		fmt.Fprintln(os.Stderr,
			"conformance: -admin-token (or VYSHKA_ADMIN_TOKEN) is required; the suite grades both API realms")
		os.Exit(2)
	}

	baseURL := strings.TrimRight(*url, "/")
	roots, transport, err := hubTransport(baseURL, *caFile, *connectTo)
	if err != nil {
		fmt.Fprintln(os.Stderr, "conformance:", err)
		os.Exit(2)
	}
	env := Env{
		BaseURL: baseURL,
		Client:  &http.Client{Timeout: *timeout, Transport: transport},
		// A poll is meant to be held. Its client waits out the longest timeout
		// the protocol lets a hub negotiate, plus the margin a plugin must also
		// leave (spec section 3.1.1), so a held request is never mistaken for a
		// hung one.
		PollClient:        &http.Client{Timeout: maxPollTimeout + 5*time.Second, Transport: transport},
		RootCAs:           roots,
		ConnectTo:         *connectTo,
		AdminToken:        *adminToken,
		WebhookListen:     *webhookListen,
		WebhookAdvertise:  *webhookAdvertise,
		StateHistoryDepth: *stateHistoryDepth,
		Slept:             new(atomic.Int64),
	}

	ctx := context.Background()
	if *wait > 0 {
		if err := waitForHub(ctx, env, *wait); err != nil {
			fmt.Fprintln(os.Stderr, "conformance: hub never became reachable:", err)
			os.Exit(1)
		}
	}

	selected := checks
	if *only != "" {
		pattern, err := regexp.Compile(*only)
		if err != nil {
			fmt.Fprintln(os.Stderr, "conformance: -run:", err)
			os.Exit(2)
		}
		selected = nil
		for _, check := range checks {
			if pattern.MatchString(check.ID) {
				selected = append(selected, check)
			}
		}
	}
	results := runChecks(ctx, env, selected, *checkTimeout)

	failed, unexercised := 0, 0
	for _, result := range results {
		if !result.Passed {
			failed++
		}
		if result.Exercised != nil && !*result.Exercised {
			unexercised++
		}
	}

	if *asJSON {
		report := map[string]any{
			"target": baseURL,
			"total":  len(results),
			"failed": failed,
			// Checks of optional behavior the hub did not show: passed,
			// because the behavior is optional, but graded on nothing.
			"notExercised": unexercised,
			"results":      results,
		}
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		_ = encoder.Encode(report)
	} else {
		fmt.Printf("conformance: hub suite against %s\n\n", baseURL)
		for _, result := range results {
			status := "PASS"
			switch {
			case !result.Passed:
				status = "FAIL"
			case result.Exercised != nil && !*result.Exercised:
				status = "N/A "
			}
			fmt.Printf("%s  %-30s %s\n", status, result.ID, result.Title)
			if !result.Passed {
				fmt.Printf("      %s\n", result.Error)
			}
			if result.Note != "" {
				fmt.Printf("      not exercised: %s\n", result.Note)
			}
		}
		fmt.Printf("\n%d checks, %d failed, %d not exercised\n", len(results), failed, unexercised)
	}

	if failed > 0 {
		os.Exit(1)
	}
}

// hubTransport builds the HTTP transport for -ca-file and -connect-to. With
// neither it returns nil, which leaves net/http's default transport, so a
// plain run is exactly what it was before the flags existed. -connect-to
// redirects only dials to the hub's own host:port; the webhook checks' local
// receiver and anything else is dialed as addressed.
func hubTransport(baseURL, caFile, connectTo string) (*x509.CertPool, http.RoundTripper, error) {
	if caFile == "" && connectTo == "" {
		return nil, nil, nil
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	var roots *x509.CertPool
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, nil, fmt.Errorf("-ca-file: %w", err)
		}
		roots, err = x509.SystemCertPool()
		if err != nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, nil, fmt.Errorf("-ca-file: %s holds no PEM certificate", caFile)
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: roots}
	}
	if connectTo != "" {
		target, err := neturl.Parse(baseURL)
		if err != nil {
			return nil, nil, fmt.Errorf("-url: %w", err)
		}
		hubAddr := target.Host
		if target.Port() == "" {
			if target.Scheme == "https" {
				hubAddr += ":443"
			} else {
				hubAddr += ":80"
			}
		}
		var dialer net.Dialer
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if addr == hubAddr {
				addr = connectTo
			}
			return dialer.DialContext(ctx, network, addr)
		}
	}
	return roots, transport, nil
}

// Result is one graded check.
type Result struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Section string `json:"section"`
	Passed  bool   `json:"passed"`
	Error   string `json:"error,omitempty"`
	// Exercised is set only on checks of optional behavior (rate limits,
	// spec section 2.2.1): false means the hub never showed the behavior, so
	// the check passed without grading anything, and Note says why.
	Exercised  *bool  `json:"exercised,omitempty"`
	Note       string `json:"note,omitempty"`
	DurationMs int64  `json:"durationMs"`
}

func runChecks(ctx context.Context, env Env, checks []Check, budget time.Duration) []Result {
	results := make([]Result, 0, len(checks))
	for _, check := range checks {
		start := time.Now()
		checkCtx, cancel := context.WithTimeout(ctx, budget)
		err := check.Run(checkCtx, env)
		cancel()

		result := Result{
			ID:         check.ID,
			Title:      check.Title,
			Section:    check.Section,
			Passed:     err == nil,
			DurationMs: time.Since(start).Milliseconds(),
		}
		var skipped notExercised
		if errors.As(err, &skipped) {
			exercised := false
			result.Passed, result.Exercised, result.Note = true, &exercised, skipped.reason
			err = nil
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
