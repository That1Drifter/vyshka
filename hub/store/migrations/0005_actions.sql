-- The action lifecycle (spec/protocol.md section 7).

-- One row per tracked job. The dispatch envelope's id is recorded so that the
-- plugin's envelope ack can flip queued -> delivered without a second lookup
-- table: the envelope carries the action, so its ack is the delivery receipt.
CREATE TABLE IF NOT EXISTS actions (
    id              TEXT PRIMARY KEY,        -- actionId, a ULID
    server_id       TEXT NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    code            TEXT NOT NULL,
    context         TEXT NOT NULL DEFAULT '',
    reference_key   TEXT NOT NULL DEFAULT '',
    params          TEXT NOT NULL,           -- JSON object, validated before queueing
    idempotency_key TEXT,                    -- NULL when the client sent none
    envelope_id     TEXT NOT NULL,           -- the action.dispatch envelope carrying it
    state           TEXT NOT NULL,           -- queued|delivered|running|completed|failed|expired
    created_at      TEXT NOT NULL,
    expires_at      TEXT NOT NULL,           -- created_at + ttl; expiry flips non-terminal states
    delivered_at    TEXT,
    running_at      TEXT,
    finished_at     TEXT,                    -- when a terminal state was reached
    ok              INTEGER,                 -- action.result ok, NULL until a result lands
    result          TEXT,                    -- action.result result, JSON, up to 64 KiB
    error           TEXT,                    -- action.result error, NULL when ok
    duration_ms     INTEGER
);

-- The idempotency contract: a retry with the same client-chosen key returns
-- the original actionId instead of double-queueing (section 7). Partial, so
-- actions dispatched without a key never collide.
CREATE UNIQUE INDEX IF NOT EXISTS actions_idempotency
    ON actions (server_id, idempotency_key) WHERE idempotency_key IS NOT NULL;

-- The expiry sweep: every non-terminal action past its deadline.
CREATE INDEX IF NOT EXISTS actions_expiry ON actions (state, expires_at);

-- The delivery receipt: queued actions keyed by the envelope that carries them.
CREATE INDEX IF NOT EXISTS actions_envelope ON actions (envelope_id);
