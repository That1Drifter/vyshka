-- Scoped Admin API tokens and the audit log (spec/protocol.md section 10).

-- One row per minted Admin API token. The secret itself is never stored: the
-- hub keeps the SHA-256 digest and looks credentials up by it, which is also
-- why the digest is the unique key rather than a secondary index.
--
-- Revocation is a timestamp rather than a delete. An audit record names the
-- token that made a change, and that name has to keep resolving after the
-- operator retires the token; a hard delete would turn the log into a list of
-- orphaned ids exactly when someone is reading it to find out what happened.
CREATE TABLE IF NOT EXISTS admin_tokens (
    id         TEXT PRIMARY KEY,        -- ULID
    token_hash TEXT NOT NULL UNIQUE,    -- SHA-256 of the bearer secret, lowercase hex
    name       TEXT NOT NULL,           -- operator-facing label
    scopes     TEXT NOT NULL,           -- JSON array of canonical scope strings
    created_at TEXT NOT NULL,
    created_by TEXT NOT NULL,           -- id of the minting token, or "" for the bootstrap credential
    expires_at TEXT,                    -- NULL means the token does not expire on its own
    revoked_at TEXT
);

-- Boot asks "does this hub already have a usable admin token?" to decide
-- whether minting an ephemeral bootstrap credential is still first-run
-- behavior or a standing back door.
CREATE INDEX IF NOT EXISTS admin_tokens_live ON admin_tokens (revoked_at, expires_at);

-- One row per authenticated Admin API mutation, successful or refused
-- (section 10). Append-only: an audit log an admin could edit through the
-- Admin API would be worth nothing, so nothing in the hub updates these rows.
--
-- token_name is denormalized on purpose. It records what the credential was
-- called at the time of the change, which is the question a reader of the log
-- is actually asking, and it survives both renaming and deletion.
CREATE TABLE IF NOT EXISTS audit_records (
    id             TEXT PRIMARY KEY,    -- ULID
    at             TEXT NOT NULL,
    token_id       TEXT NOT NULL,       -- "" for the bootstrap credential, which has no row above
    token_name     TEXT NOT NULL,
    method         TEXT NOT NULL,
    path           TEXT NOT NULL,
    status         INTEGER NOT NULL,    -- the HTTP status the hub answered with
    source_ip      TEXT NOT NULL,
    payload_digest TEXT NOT NULL,       -- SHA-256 of the request body, "" when there was none
    server_id      TEXT NOT NULL,       -- "" when the route names no server
    detail         TEXT NOT NULL,       -- JSON object: what the mutation was, per route
    expires_at     TEXT NOT NULL        -- at + the hub's audit retention
);

-- The operator's feed, newest first. The id tiebreaks so that (at, id) is a
-- strict total order and a paginating client can neither skip nor repeat rows
-- that landed in the same millisecond, exactly as the event feed does.
CREATE INDEX IF NOT EXISTS audit_feed ON audit_records (at DESC, id DESC);

-- "What has this credential done" and "what has been done to this server", the
-- two questions worth an index of their own.
CREATE INDEX IF NOT EXISTS audit_by_token ON audit_records (token_id, at DESC, id DESC);
CREATE INDEX IF NOT EXISTS audit_by_server ON audit_records (server_id, at DESC, id DESC);

-- The retention pass, a range scan over one column.
CREATE INDEX IF NOT EXISTS audit_retention ON audit_records (expires_at);
