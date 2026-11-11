# Spike: what a Prometheus scrape costs the hub

Part of issue #82 (`/metrics`). The queue depths are the one part of a scrape that is not
memory: they are read from the database when the scrape arrives, and on SQLite the hub has a
single connection, so a slow scrape would stall every poll queued behind it. The design rule
was one statement, not one query per server. This measures what that statement costs on a
database the size of a long-lived installation.

## Method

`main.go` seeds a fresh SQLite file with the rows below, boots a hub over it the way the
binary does, and times 300 repetitions each of `store.QueueStats` (the statement) and a whole
scrape through the metrics handler (the scrape hook, the registry, and the rendering, without
the network).

| Table | Rows |
|---|---|
| `servers` | 20 |
| `outbound_envelopes` | 20 x 10 000 acked, 19 x 300 pending, 5 000 pending on one server (its queue at the bound) |
| `events` | 20 x 25 000 |
| `actions` | 20 x 2 500 finished |
| `webhook_deliveries` | 20 000, a twentieth of them pending |

That is 780 721 rows in a 295 MiB file, far beyond what the design persona accumulates under
the default retention, so it is a ceiling and not a typical figure.

## Result (2026-11-11, Windows 11, NVMe, Go 1.26.4)

```
store.QueueStats (one statement)         n=300  min 0s  median 1.0054ms  p99 2.0199ms  max 2.248ms
whole scrape (6941 bytes)                n=300  min 496.6µs  median 1.4996ms  p99 2.0629ms  max 2.5626ms
```

The Windows clock ticks in roughly half-millisecond steps (hence the `min 0s`), so read
these as "about a millisecond, two at worst", not as precise figures. The first version of the
statement counted `o.id`, which is not in the index and costs a table lookup per pending
row; counting `o.created_at` keeps it index-only and took the median from 1.5 ms to 1.0 ms. The scrape holds the connection for the one statement;
rendering happens after it is released. The statement walks only the pending range of
`outbound_envelopes_pending` for each server (acked rows are never visited), which is why 760 000
rows of history cost nothing and the 5 000-deep queue is the dominant term.

## Reproducing

```
go run ./spikes/metrics-scrape            # 20 servers, 300 repetitions
go run ./spikes/metrics-scrape -keep      # leave the database file for inspection
```
