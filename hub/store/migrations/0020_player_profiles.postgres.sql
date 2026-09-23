-- Player profiles, per-webhook redaction, and audit records as webhook
-- material (spec/protocol.md sections 8.2, 8.6, 10.5, 11.1, 11.2; draft 0.31),
-- Postgres variant of 0020_player_profiles.sql: identical except that every
-- text column the store orders, range-scans, or joins on is pinned to the
-- "C" collation, for the reasons migration 0015 gives.

-- The identity index behind the profile's event read. One row per event per
-- identity the event refers to: a top-level member of its data holding a
-- { platform, id } object (section 8.2). roles is the JSON array of the member
-- names that held it, in name order, so an event naming one identity twice
-- (a suicide reported as both player and killer) is one row, and a page of
-- the profile is a page of events rather than of roles.
--
-- server_id, type, and occurred_at are copied from the event so the profile
-- read filters and orders on this table's own index and joins the event only
-- for the rows of the page. They never change: an event is append-only. The
-- row goes with its event when retention deletes it, which is what keeps the
-- profile exactly as deep as the events (section 8.6).
CREATE TABLE IF NOT EXISTS event_identities (
    event_id    TEXT COLLATE "C" NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    platform    TEXT COLLATE "C" NOT NULL,
    player_id   TEXT COLLATE "C" NOT NULL,
    roles       TEXT NOT NULL,
    server_id   TEXT COLLATE "C" NOT NULL,
    type        TEXT COLLATE "C" NOT NULL,
    occurred_at TEXT COLLATE "C" NOT NULL,
    PRIMARY KEY (event_id, platform, player_id)
);

-- The profile's feed: one identity, newest first, with the id as the
-- tiebreak that makes the order total, exactly as the server feed orders.
CREATE INDEX IF NOT EXISTS event_identities_profile
    ON event_identities (platform, player_id, occurred_at DESC, event_id DESC);

-- The events stored before this migration have no index rows. The hub indexes
-- them in the background, walking event ids in order up to the newest one
-- that existed here; everything ingested afterwards is indexed as it lands.
-- The single row is the walk's progress and is deleted when the walk is done.
-- No row is written when there are no events, so there is nothing to walk.
CREATE TABLE IF NOT EXISTS event_identity_backfill (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    after_id   TEXT COLLATE "C" NOT NULL, -- the last event id indexed; '' before the first
    through_id TEXT COLLATE "C" NOT NULL  -- the newest event id at migration time
);
INSERT INTO event_identity_backfill (id, after_id, through_id)
SELECT 1, '', (SELECT MAX(id) FROM events) WHERE EXISTS (SELECT 1 FROM events);

-- Operator notes on an identity (section 8.6). Kept until deleted: notes are
-- an operator's own records, not telemetry, and no retention applies. The
-- writer is recorded by id and by the name the token had at the time, the way
-- the audit log records it, so a note survives its author's renaming and
-- revocation.
CREATE TABLE IF NOT EXISTS player_notes (
    id         TEXT COLLATE "C" PRIMARY KEY, -- ULID
    platform   TEXT COLLATE "C" NOT NULL,
    player_id  TEXT COLLATE "C" NOT NULL,
    text       TEXT NOT NULL,
    created_at TEXT COLLATE "C" NOT NULL,
    token_id   TEXT NOT NULL,            -- '' for the bootstrap credential
    token_name TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS player_notes_by_player
    ON player_notes (platform, player_id, created_at DESC, id DESC);

-- The profile's action read: player-context actions by referenceKey, newest
-- first. A player's referenceKey is the platform id alone (section 7).
-- Migration 0015 left these three columns alone because nothing ordered or
-- range-scanned them; the profile read orders by created_at.
ALTER TABLE actions ALTER COLUMN context TYPE TEXT COLLATE "C";
ALTER TABLE actions ALTER COLUMN reference_key TYPE TEXT COLLATE "C";
ALTER TABLE actions ALTER COLUMN created_at TYPE TEXT COLLATE "C";
CREATE INDEX IF NOT EXISTS actions_by_reference
    ON actions (context, reference_key, created_at DESC, id DESC);

-- The audit notification outbox (section 11.1). An audit record is written
-- with a row here in the same transaction, and the webhook dispatcher fans
-- the row out and deletes it. A separate table rather than a flag column on
-- audit_records, because the audit log is append-only and nothing in the hub
-- updates its rows. Records written before this migration have no row: a hub
-- gaining the notification does not replay its history into anything.
CREATE TABLE IF NOT EXISTS audit_notifications (
    audit_id TEXT COLLATE "C" PRIMARY KEY REFERENCES audit_records (id) ON DELETE CASCADE
);

-- Per-webhook redaction paths (section 11.2): a JSON array of member paths
-- stripped from every notification's data before the delivery is rendered.
-- Every existing webhook redacts nothing.
ALTER TABLE webhooks ADD COLUMN redact TEXT NOT NULL DEFAULT '[]';
