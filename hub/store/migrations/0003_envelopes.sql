-- Envelope exchange over long-poll (spec/protocol.md sections 3.1.2, 4 and 9).

-- The hub -> plugin queue is keyed by server, not by session: an envelope queued
-- while the game server is down must survive until it comes back (section 9.2).
-- Sequence numbers, in contrast, belong to a session, so `session_id` and `seq`
-- are assignment state written on first delivery and rewritten when a later
-- session inherits an envelope its predecessor never acked.
CREATE TABLE IF NOT EXISTS outbound_envelopes (
    id         TEXT PRIMARY KEY,           -- the envelope's `id`, a ULID
    server_id  TEXT NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    type       TEXT NOT NULL,
    body       TEXT NOT NULL,              -- JSON object, verbatim
    created_at TEXT NOT NULL,              -- the envelope's `ts`, stable across retransmissions
    session_id TEXT,                       -- session the current seq belongs to
    seq        INTEGER,                    -- assigned on first delivery within that session
    sent_at    TEXT,                       -- last time it went out on a poll
    acked_at   TEXT                        -- set once the plugin's ack covers its seq
);

-- The delivery query: everything unacked for one server, oldest first.
CREATE INDEX IF NOT EXISTS outbound_envelopes_pending
    ON outbound_envelopes (server_id, acked_at, created_at);

-- Seq must be unique within a session, and the ack query looks up by it.
CREATE UNIQUE INDEX IF NOT EXISTS outbound_envelopes_seq
    ON outbound_envelopes (session_id, seq) WHERE session_id IS NOT NULL;

-- Per-session, per-direction sequence state. It lives on the session row because
-- it dies with the session: a new session starts both directions at 1 again.
ALTER TABLE sessions ADD COLUMN outbound_seq INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN outbound_ack INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN inbound_ack INTEGER NOT NULL DEFAULT 0;

-- How many envelopes the hub has accepted from the plugin on this session.
-- Nothing routes on it; it is the counter that makes the link legible.
ALTER TABLE sessions ADD COLUMN inbound_count INTEGER NOT NULL DEFAULT 0;
