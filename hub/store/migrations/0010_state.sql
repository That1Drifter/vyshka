-- State snapshots (spec/protocol.md section 8.3).

-- One row per accepted state.* snapshot. `seq` is the order of record: a
-- snapshot is "latest" because it was accepted last, never because its
-- capturedAt says so, since that clock belongs to the game server. ULIDs are
-- not used here because two snapshots accepted in one poll share a
-- millisecond, and their order within it must still be the acceptance order.
-- AUTOINCREMENT (rather than a bare INTEGER PRIMARY KEY) keeps seq values
-- from ever being reused after a delete, so "newer" stays monotone under the
-- trimming below.
--
-- History is bounded twice: rows are stamped with an expiry at insert
-- (received_at + the configured window, reference 24 h) for the retention
-- pass, and the insert itself trims each (server_id, type) down to the
-- configured depth (reference 500). The retention pass MUST leave the newest
-- row per (server_id, type) standing whatever its age: a server's last known
-- state stays readable however stale, and capturedAt is what tells the
-- reader how stale.
CREATE TABLE IF NOT EXISTS state_snapshots (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    server_id   TEXT NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    envelope_id TEXT NOT NULL,           -- the state.* envelope's id, for cross-session dedup
    type        TEXT NOT NULL,           -- players | vehicles | entities
    captured_at TEXT NOT NULL,           -- body capturedAt, envelope ts, or receipt, in that order
    received_at TEXT NOT NULL,           -- when the hub durably took responsibility
    expires_at  TEXT NOT NULL,           -- received_at + the history window
    body        TEXT NOT NULL            -- the accepted state.* body, verbatim
);

-- Latest and history are both one descending range over this index.
CREATE INDEX IF NOT EXISTS state_snapshots_feed
    ON state_snapshots (server_id, type, seq DESC);

-- Within a session a retransmission is deduplicated by seq before it gets
-- here; across a session change seq is renumbered and only the envelope id
-- survives (spec section 9.1), so it is the key that keeps a replayed
-- snapshot from landing in history twice with a fresh received_at.
CREATE UNIQUE INDEX IF NOT EXISTS state_snapshots_envelope
    ON state_snapshots (server_id, envelope_id);

-- The retention pass, a range scan over one column.
CREATE INDEX IF NOT EXISTS state_snapshots_retention ON state_snapshots (expires_at);
