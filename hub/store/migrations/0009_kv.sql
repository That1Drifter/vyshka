-- Per-mod key/value store (spec/protocol.md section 12).

-- One row per key. The store is installation-wide on purpose: the scope
-- grammar that guards it (kv:rw:{namespace}) has no server dimension, so the
-- table has none either. Two servers whose manifests declare the same
-- namespace share these rows, which is the feature.
--
-- revision starts at 1 and moves by one per successful write; it is what
-- ifRevision compares against, so it must never be reused while the row
-- lives. expires_at NULL means the key never expires. An expired row reads as
-- absent everywhere from the moment its expiry passes; the retention pass
-- deletes it physically whenever it gets there.
CREATE TABLE IF NOT EXISTS kv (
    namespace  TEXT NOT NULL,           -- section 12.1 grammar, at most 64 code points
    key        TEXT NOT NULL,           -- section 12.1 grammar, at most 128 code points
    value      TEXT NOT NULL,           -- one JSON value, at most 16384 encoded bytes
    revision   INTEGER NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    expires_at TEXT,                    -- NULL = never expires
    PRIMARY KEY (namespace, key)
);

-- The retention pass, a range scan over one column. NULL sorts outside any
-- comparison with <=, so never-expiring rows never match it.
CREATE INDEX IF NOT EXISTS kv_retention ON kv (expires_at);
