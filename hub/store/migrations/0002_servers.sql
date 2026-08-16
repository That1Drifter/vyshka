-- Enrollment and sessions (spec/protocol.md section 5).
--
-- Credentials live on the server row rather than in a side table: the relation
-- is 1:1, every lookup wants them together, and the history of who changed a
-- credential belongs to the audit log, not to a second copy of the state.
-- Only digests are stored; a hub cannot reveal a token it has issued.
CREATE TABLE IF NOT EXISTS servers (
    id             TEXT PRIMARY KEY,
    name           TEXT NOT NULL,
    game           TEXT NOT NULL DEFAULT '',
    created_at     TEXT NOT NULL,
    secret_hash    TEXT NOT NULL DEFAULT '',   -- empty until the plugin enrolls
    enrolled_at    TEXT,
    revoked_at     TEXT,
    plugin_name    TEXT NOT NULL DEFAULT '',
    plugin_version TEXT NOT NULL DEFAULT '',
    transports     TEXT NOT NULL DEFAULT '',   -- comma-separated, as advertised at enroll
    last_seen_at   TEXT
);

-- Session lookup is by secret digest, so it must be unique and indexed. The
-- partial predicate keeps the many not-yet-enrolled rows out of the index and
-- off each other's toes, since they all share the empty digest.
CREATE UNIQUE INDEX IF NOT EXISTS servers_secret_hash
    ON servers (secret_hash) WHERE secret_hash <> '';

CREATE INDEX IF NOT EXISTS servers_created_at ON servers (created_at DESC);

-- One-time enrollment tokens. Rows are kept after use: burning a token is
-- recording when it was spent, and a reuse attempt must be able to tell the
-- difference between "unknown" and "already used".
CREATE TABLE IF NOT EXISTS enrollment_tokens (
    token_hash TEXT PRIMARY KEY,
    server_id  TEXT NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    used_at    TEXT
);

CREATE INDEX IF NOT EXISTS enrollment_tokens_server ON enrollment_tokens (server_id);

-- Sessions. At most one is live per server; issuing a new one ends the old,
-- which is what keeps per-session sequence state unambiguous.
CREATE TABLE IF NOT EXISTS sessions (
    id                   TEXT PRIMARY KEY,
    token_hash           TEXT NOT NULL UNIQUE,
    server_id            TEXT NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    created_at           TEXT NOT NULL,
    expires_at           TEXT NOT NULL,
    ended_at             TEXT,
    end_reason           TEXT NOT NULL DEFAULT '',
    protocol_version     INTEGER NOT NULL,
    poll_timeout_seconds INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS sessions_server_live ON sessions (server_id, ended_at);
