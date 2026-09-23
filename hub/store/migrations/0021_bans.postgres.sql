-- The installation ban list (spec/protocol.md section 13, draft 0.32),
-- Postgres variant of 0021_bans.sql: identical except that every text column
-- the store orders, range-scans, or joins on is pinned to the "C" collation,
-- for the reasons migration 0015 gives, and every revision is a BIGINT, for
-- the reasons migration 0014 gives (revisions are bounded at 2^53, not 2^31).

-- One row per ban ever placed, kept after it is lifted or expires: the rows are
-- an operator's own history, like notes, and no retention applies (section
-- 13.1). The identity is the section 8.2 pair; name, reason, and the writer are
-- stored as they were when the ban was placed.
--
-- listed_revision and delisted_revision place the ban in the history of the
-- active list: it is on the list at revision R when listed_revision <= R and
-- delisted_revision is NULL or above R. That is what lets a plugin's walk of
-- the list read one revision whole while the list moves under it (section
-- 13.3): every page of the walk asks for the same R. delisted_revision is NULL
-- exactly while the ban is on the active list; lifted_at says whether it left
-- by a lift, and otherwise it left because it expired.
CREATE TABLE IF NOT EXISTS bans (
    id                TEXT COLLATE "C" PRIMARY KEY,  -- ULID
    platform          TEXT COLLATE "C" NOT NULL,
    player_id         TEXT COLLATE "C" NOT NULL,
    reason            TEXT NOT NULL,
    name              TEXT NOT NULL DEFAULT '',
    server_id         TEXT NOT NULL DEFAULT '',      -- provenance; '' when none was given
    created_at        TEXT COLLATE "C" NOT NULL,
    token_id          TEXT NOT NULL,                 -- '' for the bootstrap credential
    token_name        TEXT NOT NULL,
    expires_at        TEXT COLLATE "C",              -- NULL for a ban that does not end on its own
    lifted_at         TEXT,
    lifted_token_id   TEXT NOT NULL DEFAULT '',
    lifted_token_name TEXT NOT NULL DEFAULT '',
    listed_revision   BIGINT NOT NULL,
    delisted_revision BIGINT
);

-- An identity carries at most one active ban (section 13.1). The store checks
-- before it inserts; this is the backstop behind the check.
CREATE UNIQUE INDEX IF NOT EXISTS bans_one_active
    ON bans (platform, player_id) WHERE delisted_revision IS NULL;

-- A walk of one revision reads in identity order (section 13.3).
CREATE INDEX IF NOT EXISTS bans_by_identity
    ON bans (platform, player_id, listed_revision);

-- The Admin API list, newest first, whole and for one identity.
CREATE INDEX IF NOT EXISTS bans_by_created
    ON bans (created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS bans_by_identity_created
    ON bans (platform, player_id, created_at DESC, id DESC);

-- The expiry sweep: the active bans that can expire, by when.
CREATE INDEX IF NOT EXISTS bans_expiring
    ON bans (expires_at) WHERE delisted_revision IS NULL AND expires_at IS NOT NULL;

-- The active list's revision (section 13.1), one row. Every change to the list
-- reads it under a lock, bumps it, and writes it back in the transaction that
-- made the change, so two changes are always two revisions.
CREATE TABLE IF NOT EXISTS ban_list (
    id       INTEGER PRIMARY KEY CHECK (id = 1),
    revision BIGINT NOT NULL
);
INSERT INTO ban_list (id, revision) VALUES (1, 0);

-- What each server's plugin last reported enforcing (section 13.4): the
-- plugin's word, kept as said, NULL until its first report.
ALTER TABLE servers ADD COLUMN bans_applied_revision BIGINT;
ALTER TABLE servers ADD COLUMN bans_applied_at TEXT;

-- The capabilities a stored manifest declares (section 6.7), as a JSON array,
-- written with the manifest so that choosing which servers a bans.changed goes
-- to does not parse every stored manifest. NULL on a manifest stored before
-- this migration, which a reader treats as unknown and derives from the body.
ALTER TABLE manifests ADD COLUMN capabilities TEXT;
