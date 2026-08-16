-- Baseline schema. Deliberately carries no protocol tables: enrollment,
-- sessions, envelopes, and actions arrive with the slices that implement them.
-- This table exists so a fresh boot has something to create and so operational
-- facts about the database itself have a home.
CREATE TABLE IF NOT EXISTS meta (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
