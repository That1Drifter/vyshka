-- Manifest publish (spec/protocol.md section 6).

-- One manifest per server, replaced whole. The body is the accepted
-- `manifest.publish` body, verbatim: the hub validates before storing, so what
-- is here has already passed, and storing it unparsed means the Admin API can
-- return exactly what the plugin said. `revision` is duplicated out of the body
-- because the monotonicity gate compares it in SQL.
CREATE TABLE IF NOT EXISTS manifests (
    server_id    TEXT PRIMARY KEY REFERENCES servers (id) ON DELETE CASCADE,
    revision     INTEGER NOT NULL,
    body         TEXT NOT NULL,
    published_at TEXT NOT NULL
);
