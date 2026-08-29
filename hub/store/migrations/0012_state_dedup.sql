-- Cross-session state snapshot dedup markers (spec/protocol.md section 8.3).

-- One row per accepted state.* envelope, living for the history window from
-- acceptance whatever happens to the snapshot's history row. The history table
-- cannot carry the obligation alone: history is bounded by depth as well as
-- time, so a superseded row (and the envelope id it remembers) can be trimmed
-- minutes after it landed while the latest row of its type survives every
-- pass. A replay of the trimmed snapshot across a session change would then
-- look new, insert with a fresh acceptance seq, and regress "latest" to a
-- state that was already superseded; this table is what remembers the id
-- after the row is gone. The marker expires with the history window, which is
-- as far as section 8.3 lets deduplication promise anything; a replay past
-- that horizon is a new snapshot by spec.
CREATE TABLE IF NOT EXISTS state_snapshot_dedup (
    server_id   TEXT NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    envelope_id TEXT NOT NULL,           -- the state.* envelope's id
    expires_at  TEXT NOT NULL,           -- receipt + the history window
    PRIMARY KEY (server_id, envelope_id)
);

-- The retention pass, a range scan over one column.
CREATE INDEX IF NOT EXISTS state_snapshot_dedup_retention
    ON state_snapshot_dedup (expires_at);

-- Backfill from the rows that still remember their envelope id, so an upgrade
-- does not reopen the replay window for snapshots already stored. Ids already
-- lost to pruning are lost; the unique index on state_snapshots still guards
-- the surviving latest rows. No conflict handling: that index guarantees the
-- selected pairs are distinct, and this runs once.
INSERT INTO state_snapshot_dedup (server_id, envelope_id, expires_at)
SELECT server_id, envelope_id, expires_at FROM state_snapshots;
