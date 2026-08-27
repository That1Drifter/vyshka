-- Cross-session event.batch dedup (spec/protocol.md section 8.1).

-- One row per accepted, non-empty event.batch envelope. Within a session a
-- retransmission is deduplicated by seq before it gets here; across a session
-- change seq is renumbered and only the envelope id survives (spec section
-- 9.1), so this table is what keeps a replayed batch from landing every one of
-- its events in the feed twice with fresh ids and a fresh received_at, and from
-- firing webhook fan-out twice for each.
--
-- The events table cannot carry the constraint itself: one envelope fans out
-- into up to 200 rows with hub-assigned ids, so the batch, not the event, is
-- the unit that has an identity to deduplicate on.
CREATE TABLE IF NOT EXISTS event_batches (
    server_id   TEXT NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    envelope_id TEXT NOT NULL,           -- the event.batch envelope's id
    expires_at  TEXT NOT NULL,           -- receipt + the batch's longest event retention
    PRIMARY KEY (server_id, envelope_id)
);

-- The retention pass, a range scan over one column. A dedup row must outlive
-- every event its batch stored, or pruning it would reopen the replay window
-- while the duplicates it guards against are still visible in the feed.
CREATE INDEX IF NOT EXISTS event_batches_retention ON event_batches (expires_at);
