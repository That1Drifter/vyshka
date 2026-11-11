// Command metrics-scrape measures what one Prometheus scrape costs the hub on
// a database with realistic row counts. It seeds a fresh SQLite file with the
// rows a busy installation accumulates (see seedPlan), then times the queue
// statement alone (store.QueueStats) and a whole scrape through the hub's
// metrics handler, which adds the hook, the registry, and the rendering.
//
//	go run ./spikes/metrics-scrape [-servers 20] [-scrapes 300] [-keep]
//
// Results and what they decided: README.md in this directory.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/That1Drifter/vyshka/hub"
	"github.com/That1Drifter/vyshka/hub/store"
)

// seedPlan is the database a long-lived hub of the design persona holds:
// about twenty servers, each with a long history of acked envelopes, a
// day or more of events, finished actions, and a webhook with its delivery
// record. One server has a queue at its 5000 bound, because that is the
// worst case for the depth count.
type seedPlan struct {
	servers          int
	ackedEnvelopes   int // per server
	pendingEnvelopes int // per server
	fullQueue        int // pending envelopes on server 0
	events           int // per server
	actions          int // per server
	deliveries       int // total, 5% pending
}

func main() {
	servers := flag.Int("servers", 20, "enrolled servers")
	scrapes := flag.Int("scrapes", 300, "timed repetitions of each measurement")
	keep := flag.Bool("keep", false, "keep the database file and print its path")
	flag.Parse()

	plan := seedPlan{
		servers: *servers, ackedEnvelopes: 10000, pendingEnvelopes: 300, fullQueue: 5000,
		events: 25000, actions: 2500, deliveries: 20000,
	}

	dir, err := os.MkdirTemp("", "vyshka-metrics-scrape-")
	if err != nil {
		fail(err)
	}
	if !*keep {
		defer os.RemoveAll(dir)
	}
	dbPath := filepath.Join(dir, "scrape.db")

	ctx := context.Background()
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		fail(err)
	}
	if _, err := st.Migrate(ctx); err != nil {
		fail(err)
	}
	started := time.Now()
	rows, err := seed(ctx, st, plan)
	if err != nil {
		fail(err)
	}
	fmt.Printf("seeded %d rows in %s (%s)\n", rows, time.Since(started).Round(time.Millisecond), dbPath)
	if err := st.Close(); err != nil {
		fail(err)
	}
	if info, err := os.Stat(dbPath); err == nil {
		fmt.Printf("database file: %.0f MiB\n", float64(info.Size())/(1<<20))
	}

	// A hub over the seeded file, booted the way the binary boots it; its
	// metrics handler is what a scraper reaches.
	server, err := hub.New(ctx, hub.Config{
		DatabaseURL: dbPath,
		AdminToken:  "vya_spike",
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		fail(err)
	}
	defer server.Close()

	queue := time.Duration(0)
	var queueRuns []time.Duration
	for range *scrapes {
		t := time.Now()
		stats, err := server.Store().QueueStats(ctx)
		queue = time.Since(t)
		if err != nil {
			fail(err)
		}
		if len(stats.Servers) != plan.servers {
			fail(fmt.Errorf("QueueStats saw %d servers, want %d", len(stats.Servers), plan.servers))
		}
		queueRuns = append(queueRuns, queue)
	}
	report("store.QueueStats (one statement)", queueRuns)

	handler := server.MetricsHandler()
	var scrapeRuns []time.Duration
	var size int
	for range *scrapes {
		recorder := httptest.NewRecorder()
		t := time.Now()
		handler.ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
		scrapeRuns = append(scrapeRuns, time.Since(t))
		size = recorder.Body.Len()
	}
	report(fmt.Sprintf("whole scrape (%d bytes)", size), scrapeRuns)
}

func report(label string, runs []time.Duration) {
	sort.Slice(runs, func(i, j int) bool { return runs[i] < runs[j] })
	at := func(q float64) time.Duration { return runs[int(float64(len(runs)-1)*q)] }
	fmt.Printf("%-40s n=%d  min %v  median %v  p99 %v  max %v\n",
		label, len(runs), runs[0], at(0.5), at(0.99), runs[len(runs)-1])
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "metrics-scrape:", err)
	os.Exit(1)
}

// seed fills the database with plan's rows in a handful of transactions and
// returns how many it wrote. Rows the hub's background passes would act on at
// boot (unnotified events and actions, due deliveries, expired anything) are
// written already settled, so the timing is of the scrape and not of a
// dispatcher chewing a backlog.
func seed(ctx context.Context, st *store.Store, plan seedPlan) (int, error) {
	const layout = "2006-01-02T15:04:05.000Z"
	now := time.Now().UTC()
	stamp := func(offset time.Duration) string { return now.Add(offset).Format(layout) }
	future := now.Add(30 * 24 * time.Hour).Format(layout)
	db := st.DB()
	total := 0

	insert := func(query string, count int, args func(i int) []any) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		stmt, err := tx.PrepareContext(ctx, query)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for i := range count {
			if _, err := stmt.ExecContext(ctx, args(i)...); err != nil {
				return err
			}
		}
		total += count
		return tx.Commit()
	}

	ids := make([]string, plan.servers)
	for i := range ids {
		ids[i] = fmt.Sprintf("srv%03d", i)
	}
	if err := insert(`INSERT INTO servers (id, name, game, created_at) VALUES (?, ?, 'dayz', ?)`,
		plan.servers, func(i int) []any { return []any{ids[i], "Server " + ids[i], stamp(-90 * 24 * time.Hour)} }); err != nil {
		return 0, err
	}

	for s, server := range ids {
		pending := plan.pendingEnvelopes
		if s == 0 {
			pending = plan.fullQueue
		}
		if err := insert(`INSERT INTO outbound_envelopes (id, server_id, type, body, created_at, acked_at)
		                  VALUES (?, ?, 'demo.filler', '{}', ?, ?)`,
			plan.ackedEnvelopes, func(i int) []any {
				at := stamp(-time.Duration(plan.ackedEnvelopes-i) * time.Minute)
				return []any{fmt.Sprintf("a-%s-%06d", server, i), server, at, at}
			}); err != nil {
			return 0, err
		}
		if err := insert(`INSERT INTO outbound_envelopes (id, server_id, type, body, created_at)
		                  VALUES (?, ?, 'demo.filler', '{}', ?)`,
			pending, func(i int) []any {
				return []any{fmt.Sprintf("p-%s-%06d", server, i), server, stamp(-time.Duration(pending-i) * time.Second)}
			}); err != nil {
			return 0, err
		}
		if err := insert(`INSERT INTO events (id, server_id, type, occurred_at, received_at, expires_at, data, notified)
		                  VALUES (?, ?, 'core.player.position', ?, ?, ?, '{"x":1,"y":2,"z":3}', 1)`,
			plan.events, func(i int) []any {
				at := stamp(-time.Duration(plan.events-i) * 3 * time.Second)
				return []any{fmt.Sprintf("e-%s-%06d", server, i), server, at, at, future}
			}); err != nil {
			return 0, err
		}
		if err := insert(`INSERT INTO actions (id, server_id, code, params, envelope_id, state, created_at, expires_at, finished_at, ok, notified)
		                  VALUES (?, ?, 'dayz.player.kick', '{}', ?, 'completed', ?, ?, ?, 1, 1)`,
			plan.actions, func(i int) []any {
				at := stamp(-time.Duration(plan.actions-i) * time.Minute)
				return []any{fmt.Sprintf("x-%s-%06d", server, i), server, fmt.Sprintf("a-%s-%06d", server, i), at, at, at}
			}); err != nil {
			return 0, err
		}
	}

	if err := insert(`INSERT INTO webhooks (id, url, secret, template, events, server_ids, created_at)
	                  VALUES ('wh1', 'http://127.0.0.1:1/never', 'secret', 'generic-json', '[]', '[]', ?)`,
		1, func(int) []any { return []any{stamp(-90 * 24 * time.Hour)} }); err != nil {
		return 0, err
	}
	pendingDeliveries := plan.deliveries / 20
	if err := insert(`INSERT INTO webhook_deliveries (id, webhook_id, type, server_id, body, state, attempts, next_attempt_at, created_at, finished_at, delivered_at)
	                  VALUES (?, 'wh1', 'core.player.death', 'srv000', '{}', 'delivered', 1, ?, ?, ?, ?)`,
		plan.deliveries-pendingDeliveries, func(i int) []any {
			at := stamp(-time.Duration(plan.deliveries-i) * time.Minute)
			return []any{fmt.Sprintf("d-%06d", i), at, at, at, at}
		}); err != nil {
		return 0, err
	}
	if err := insert(`INSERT INTO webhook_deliveries (id, webhook_id, type, server_id, body, state, attempts, next_attempt_at, created_at)
	                  VALUES (?, 'wh1', 'core.player.death', 'srv000', '{}', 'pending', 1, ?, ?)`,
		pendingDeliveries, func(i int) []any {
			return []any{fmt.Sprintf("q-%06d", i), future, stamp(-time.Duration(i) * time.Second)}
		}); err != nil {
		return 0, err
	}
	return total, nil
}
