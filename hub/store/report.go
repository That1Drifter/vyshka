package store

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// StorageReport is what `vyshka-hub storage` prints: how big each table is,
// and what the hub's background passes would do if they ran now.
type StorageReport struct {
	Driver string `json:"driver"`
	Target string `json:"target"`
	// FileBytes, FreeBytes, and WALBytes are SQLite only: the database file,
	// the part of it a vacuum would hand back to the filesystem, and the
	// write-ahead log beside it.
	FileBytes int64        `json:"fileBytes,omitempty"`
	FreeBytes int64        `json:"freeBytes,omitempty"`
	WALBytes  int64        `json:"walBytes,omitempty"`
	Tables    []TableSize  `json:"tables"`
	Sweeps    []SweepCount `json:"sweeps"`
	At        time.Time    `json:"at"`
}

// TableSize is one table's row count and its bytes on disk, indexes included.
type TableSize struct {
	Name  string `json:"name"`
	Rows  int64  `json:"rows"`
	Bytes int64  `json:"bytes"`
}

// SweepCount is one background pass and how many rows it would touch now.
type SweepCount struct {
	Sweep string `json:"sweep"`
	Table string `json:"table"`
	// Effect is "delete" for a retention pass and "update" for a pass that
	// moves rows to a terminal state without removing them.
	Effect string `json:"effect"`
	Rows   int64  `json:"rows"`
}

// ReportOptions carries the one retention window the store cannot read from
// the rows themselves: webhook delivery retention is applied at prune time
// from hub configuration, where every other window is stamped into an
// expires_at column at insert.
type ReportOptions struct {
	WebhookDeliveryRetention time.Duration
}

// Report measures the database. It only reads, through the store's own
// handle, and each count is its own statement: against a database a running
// hub has open the numbers are each true of some instant during the report,
// not of one shared instant, which is all a sizing report needs.
//
// The sweep counts repeat the predicates of the passes they describe
// (ExpireActions, PruneEvents, PruneAudit, PruneKV, PruneSnapshots,
// PruneWebhookDeliveries, SweepExpiredBans), as of the moment the report
// runs. A pass prunes at most a bounded batch per run, so a large count is
// drained over several runs rather than at the next one.
func (s *Store) Report(ctx context.Context, options ReportOptions) (StorageReport, error) {
	now := time.Now().UTC()
	report := StorageReport{Driver: string(s.driver), Target: s.target, At: now, Tables: []TableSize{}, Sweeps: []SweepCount{}}

	names, err := s.tableNames(ctx)
	if err != nil {
		return StorageReport{}, err
	}
	sizes, err := s.tableBytes(ctx)
	if err != nil {
		return StorageReport{}, err
	}
	for _, name := range names {
		var rows int64
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM "`+name+`"`).Scan(&rows); err != nil {
			return StorageReport{}, fmt.Errorf("count %s: %w", name, err)
		}
		report.Tables = append(report.Tables, TableSize{Name: name, Rows: rows, Bytes: sizes[name]})
	}
	sort.Slice(report.Tables, func(i, j int) bool {
		if report.Tables[i].Bytes != report.Tables[j].Bytes {
			return report.Tables[i].Bytes > report.Tables[j].Bytes
		}
		return report.Tables[i].Name < report.Tables[j].Name
	})

	if s.driver == dialectSQLite {
		var pageSize, pages, free int64
		for pragma, into := range map[string]*int64{"page_size": &pageSize, "page_count": &pages, "freelist_count": &free} {
			if err := s.db.QueryRowContext(ctx, `PRAGMA `+pragma).Scan(into); err != nil {
				return StorageReport{}, fmt.Errorf("pragma %s: %w", pragma, err)
			}
		}
		report.FileBytes = pageSize * pages
		report.FreeBytes = pageSize * free
		if info, err := os.Stat(s.target + "-wal"); err == nil {
			report.WALBytes = info.Size()
		}
	}

	stamp := formatTime(now)
	webhookCutoff := formatTime(now.Add(-options.WebhookDeliveryRetention))
	sweeps := []struct {
		sweep, table, effect, query string
		args                        []any
	}{
		{"action expiry", "actions", "update",
			`SELECT COUNT(*) FROM actions WHERE state IN (?, ?, ?) AND expires_at <= ?`,
			[]any{ActionQueued, ActionDelivered, ActionRunning, stamp}},
		{"action expiry", "outbound_envelopes", "delete",
			`SELECT COUNT(*) FROM outbound_envelopes
			  WHERE acked_at IS NULL AND session_id IS NULL
			    AND id IN (SELECT envelope_id FROM actions WHERE state IN (?, ?) AND expires_at <= ?)`,
			[]any{ActionQueued, ActionExpired, stamp}},
		{"event retention", "events", "delete",
			`SELECT COUNT(*) FROM events WHERE expires_at <= ?`, []any{stamp}},
		// Counted as the pass would leave them once the events above are
		// gone: a marker goes only when its server has no expired event left.
		{"event retention", "event_batches", "delete",
			`SELECT COUNT(*) FROM event_batches WHERE expires_at <= ?`, []any{stamp}},
		{"audit retention", "audit_records", "delete",
			`SELECT COUNT(*) FROM audit_records WHERE expires_at <= ?`, []any{stamp}},
		{"kv expiry", "kv", "delete",
			`SELECT COUNT(*) FROM kv WHERE expires_at IS NOT NULL AND expires_at <= ?`, []any{stamp}},
		{"state snapshot retention", "state_snapshots", "delete",
			`SELECT COUNT(*) FROM state_snapshots AS stale
			  WHERE stale.expires_at <= ?
			    AND stale.seq < (SELECT MAX(newest.seq) FROM state_snapshots AS newest
			                      WHERE newest.server_id = stale.server_id AND newest.type = stale.type)`,
			[]any{stamp}},
		{"state snapshot retention", "state_snapshot_dedup", "delete",
			`SELECT COUNT(*) FROM state_snapshot_dedup WHERE expires_at <= ?`, []any{stamp}},
		{"webhook delivery retention", "webhook_deliveries", "delete",
			`SELECT COUNT(*) FROM webhook_deliveries
			  WHERE state <> ? AND finished_at IS NOT NULL AND finished_at <= ?`,
			[]any{DeliveryPending, webhookCutoff}},
		{"ban expiry", "bans", "update",
			`SELECT COUNT(*) FROM bans
			  WHERE delisted_revision IS NULL AND expires_at IS NOT NULL AND expires_at <= ?`,
			[]any{stamp}},
	}
	for _, sweep := range sweeps {
		var rows int64
		if err := s.db.QueryRowContext(ctx, sweep.query, sweep.args...).Scan(&rows); err != nil {
			return StorageReport{}, fmt.Errorf("count %s: %w", sweep.sweep, err)
		}
		report.Sweeps = append(report.Sweeps, SweepCount{Sweep: sweep.sweep, Table: sweep.table, Effect: sweep.effect, Rows: rows})
	}
	return report, nil
}

func (s *Store) tableNames(ctx context.Context) ([]string, error) {
	query := `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`
	if s.driver == dialectPostgres {
		query = `SELECT tablename FROM pg_tables WHERE schemaname = current_schema()`
	}
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("list tables: %w", err)
		}
		// Every name here came from the catalog, but it is spliced into a
		// statement, so anything that would need quoting is skipped rather
		// than trusted.
		if strings.Trim(name, "abcdefghijklmnopqrstuvwxyz0123456789_") == "" {
			names = append(names, name)
		}
	}
	return names, rows.Err()
}

// tableBytes reports each table's size with its indexes. SQLite reads it from
// the dbstat virtual table (compiled into the pinned driver), attributing each
// index to the table it belongs to; Postgres from pg_total_relation_size.
func (s *Store) tableBytes(ctx context.Context) (map[string]int64, error) {
	query := `SELECT COALESCE(m.tbl_name, d.name), SUM(d.pgsize)
	            FROM dbstat AS d LEFT JOIN sqlite_master AS m ON m.name = d.name
	           GROUP BY 1`
	if s.driver == dialectPostgres {
		query = `SELECT tablename, pg_total_relation_size(quote_ident(tablename)::regclass)
		           FROM pg_tables WHERE schemaname = current_schema()`
	}
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("table sizes: %w", err)
	}
	defer rows.Close()
	sizes := map[string]int64{}
	for rows.Next() {
		var name string
		var bytes int64
		if err := rows.Scan(&name, &bytes); err != nil {
			return nil, fmt.Errorf("table sizes: %w", err)
		}
		sizes[name] = bytes
	}
	return sizes, rows.Err()
}
