package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ActionOutcome is one action reaching a terminal state: its code and the
// state it ended in (completed, failed, or expired).
type ActionOutcome struct {
	Code  string
	State string
}

// OnActionFinished registers the function called once for every action that
// moves into a terminal state, after the transition committed. It exists for
// the hub's metrics: a transition is observed here at the one place that
// makes it, whichever path made it (a plugin's result, the expiry sweep, or
// the lazy expiry on a read), so no path can be forgotten and none counts
// twice. It must be set before the store is used concurrently and must not
// block or call back into the store.
func (s *Store) OnActionFinished(observe func(ActionOutcome)) { s.actionFinished = observe }

func (s *Store) observeFinished(outcomes ...ActionOutcome) {
	if s.actionFinished == nil {
		return
	}
	for _, outcome := range outcomes {
		s.actionFinished(outcome)
	}
}

// ServerQueue is one server's outbound envelope queue as the operator sees it.
type ServerQueue struct {
	ServerID string
	// Depth counts the envelopes queued for the server and not yet acked,
	// whether or not they have been sent (spec section 9.2).
	Depth int
	// Oldest is when the longest-waiting of them was queued; nil when empty.
	Oldest *time.Time
}

// QueueStats is what a metrics scrape reads from the database.
type QueueStats struct {
	// Servers has one entry per enrolled server, the empty queues included, so
	// a gauge built from it reads 0 rather than disappearing.
	Servers []ServerQueue
	// WebhookPending counts webhook deliveries waiting for an attempt or a
	// retry (spec section 11.5).
	WebhookPending int
}

// QueueStats reads every queue depth in one statement, so a scrape holds the
// store's single SQLite connection for one short query rather than one per
// server. Both halves are index-only: the outbound count walks the pending
// range of outbound_envelopes_pending (server_id, acked_at, created_at) for
// each server, which the queue bound (spec section 9.2) caps, and the webhook
// count walks webhook_deliveries_due (state, next_attempt_at).
func (s *Store) QueueStats(ctx context.Context) (QueueStats, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT 'outbound', s.id, COUNT(o.created_at), MIN(o.created_at)
		   FROM servers s
		   LEFT JOIN outbound_envelopes o ON o.server_id = s.id AND o.acked_at IS NULL
		  GROUP BY s.id
		 UNION ALL
		 SELECT 'webhook', '', COUNT(*), NULL
		   FROM webhook_deliveries WHERE state = 'pending'`)
	if err != nil {
		return QueueStats{}, fmt.Errorf("read queue depths: %w", err)
	}
	defer rows.Close()

	var stats QueueStats
	for rows.Next() {
		var (
			kind, serverID string
			depth          int
			oldest         sql.NullString
		)
		if err := rows.Scan(&kind, &serverID, &depth, &oldest); err != nil {
			return QueueStats{}, fmt.Errorf("scan queue depths: %w", err)
		}
		if kind == "webhook" {
			stats.WebhookPending = depth
			continue
		}
		queue := ServerQueue{ServerID: serverID, Depth: depth}
		if oldest.Valid {
			at, err := parseTime(oldest.String)
			if err != nil {
				return QueueStats{}, err
			}
			queue.Oldest = &at
		}
		stats.Servers = append(stats.Servers, queue)
	}
	if err := rows.Err(); err != nil {
		return QueueStats{}, fmt.Errorf("read queue depths: %w", err)
	}
	return stats, nil
}
