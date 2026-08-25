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
    type        TEXT NOT NULL,           -- players | vehicles | entities
    captured_at TEXT NOT NULL,           -- body capturedAt, envelope ts, or receipt, in that order
    received_at TEXT NOT NULL,           -- when the hub durably took responsibility
    expires_at  TEXT NOT NULL,           -- received_at + the history window
    body        TEXT NOT NULL            -- the accepted state.* body, verbatim
);

-- Latest and history are both one descending range over this index.
CREATE INDEX IF NOT EXISTS state_snapshots_feed
    ON state_snapshots (server_id, type, seq DESC);

-- The retention pass, a range scan over one column.
CREATE INDEX IF NOT EXISTS state_snapshots_retention ON state_snapshots (expires_at);
