-- Telemetry event ingest (spec/protocol.md section 8).

-- One row per event, append-only: an event is a statement about something that
-- already happened, so the only writes are the insert and, at the end of its
-- retention, the delete. Core and custom events share this table and every
-- query over it, which is what makes "hubs MUST NOT privilege core events over
-- custom events" (section 8.1) true by construction rather than by discipline.
CREATE TABLE IF NOT EXISTS events (
    id          TEXT PRIMARY KEY,        -- ULID assigned at ingest
    server_id   TEXT NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    type        TEXT NOT NULL,           -- the event's `t`, a {namespace}.{name} type
    occurred_at TEXT NOT NULL,           -- the plugin's `ts`, or receipt time when it sent none
    received_at TEXT NOT NULL,           -- when the hub durably took responsibility
    expires_at  TEXT NOT NULL,           -- received_at + the retention this type resolved to
    data        TEXT NOT NULL            -- JSON object
);

-- The operator's feed: one server, newest first. The id tiebreaks so that
-- (occurred_at, id) is a strict total order, which is what lets a paginating
-- client resume without skipping or repeating rows that share a millisecond.
CREATE INDEX IF NOT EXISTS events_feed ON events (server_id, occurred_at DESC, id DESC);

-- The same feed narrowed to a type or a namespace. Namespace filters run as
-- range scans over this index (`type >= 'ns.' AND type < 'ns/'`) rather than as
-- LIKE: `_` is a LIKE wildcard and a legal namespace character, so a `LIKE
-- 'example_mod.%'` would quietly also match `exampleXmod.raid`.
CREATE INDEX IF NOT EXISTS events_by_type ON events (server_id, type, occurred_at DESC, id DESC);

-- The retention pass, which is then a range scan over one column.
CREATE INDEX IF NOT EXISTS events_retention ON events (expires_at);
