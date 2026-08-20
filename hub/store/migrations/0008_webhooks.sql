-- Webhooks: signed push delivery of telemetry and lifecycle notifications
-- (spec/protocol.md section 11).

-- One row per registered webhook. The secret is stored as itself, not as a
-- digest: signing needs the key, so unlike the credentials of section 5 there
-- is nothing irreversible to store. Read access to this table is read access
-- to every webhook's signing key, which section 11.2 says out loud.
CREATE TABLE IF NOT EXISTS webhooks (
    id          TEXT PRIMARY KEY,       -- ULID assigned at registration
    url         TEXT NOT NULL,          -- http(s) target
    secret      TEXT NOT NULL,          -- HMAC-SHA256 signing key
    template    TEXT NOT NULL,          -- 'generic-json'
    events      TEXT NOT NULL,          -- JSON array of type patterns; [] means every type
    server_ids  TEXT NOT NULL,          -- JSON array of server ids; [] means every server
    created_at  TEXT NOT NULL
);

-- One row per delivery: one matching notification for one webhook. The body is
-- rendered once at enqueue and stored, so every attempt sends byte-identical
-- bytes and carries the same signature (section 11.3).
CREATE TABLE IF NOT EXISTS webhook_deliveries (
    id              TEXT PRIMARY KEY,   -- ULID; stable across attempts
    webhook_id      TEXT NOT NULL REFERENCES webhooks (id) ON DELETE CASCADE,
    type            TEXT NOT NULL,      -- the notification type delivered
    server_id       TEXT NOT NULL,      -- the server it concerns
    body            TEXT NOT NULL,      -- the signed JSON payload
    state           TEXT NOT NULL,      -- pending | delivered | dead
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TEXT NOT NULL,      -- when the dispatcher owes it an attempt
    last_status     INTEGER,            -- last HTTP status; NULL when the failure was transport-level
    last_error      TEXT,               -- human-readable account of the last failure
    created_at      TEXT NOT NULL,
    finished_at     TEXT,               -- when it became delivered or dead; retention counts from here
    delivered_at    TEXT
);

-- What the dispatcher owes an attempt: pending rows whose time has come.
CREATE INDEX IF NOT EXISTS webhook_deliveries_due
    ON webhook_deliveries (state, next_attempt_at);

-- The operator's per-webhook delivery record, newest first (section 11.5).
CREATE INDEX IF NOT EXISTS webhook_deliveries_by_webhook
    ON webhook_deliveries (webhook_id, created_at DESC, id DESC);

-- The retention pass over finished deliveries, keyed on when they finished so
-- a delivery that spent its whole schedule pending gets the same readable
-- afterlife as one that died at once (section 11.5).
CREATE INDEX IF NOT EXISTS webhook_deliveries_finished
    ON webhook_deliveries (finished_at) WHERE state <> 'pending';

-- The notification outbox flags. A stored event or a finished action is
-- "unnotified" until the dispatcher has fanned it out to whatever webhooks
-- match (possibly none); flagging rows beats a cursor because it cannot skip a
-- row whose transaction committed out of timestamp order. Existing rows start
-- notified = 0, which is harmless: the webhooks table is created empty in this
-- same migration, so the first dispatcher pass marks history off against zero
-- webhooks and delivers nothing, exactly the no-backfill rule of section 11.2.
ALTER TABLE events ADD COLUMN notified INTEGER NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS events_unnotified ON events (id) WHERE notified = 0;

ALTER TABLE actions ADD COLUMN notified INTEGER NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS actions_unnotified
    ON actions (id) WHERE notified = 0 AND finished_at IS NOT NULL;

-- Link-state bookkeeping for server.link.lost / server.link.restored (section
-- 11.1). 'unknown' means no session has ever been observed, which fires
-- nothing; transitions move it between 'up' and 'down'.
ALTER TABLE servers ADD COLUMN link_state TEXT NOT NULL DEFAULT 'unknown';
