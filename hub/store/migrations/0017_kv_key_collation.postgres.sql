-- Byte-order comparison of the KV name columns (issue #64, key listing),
-- Postgres variant of 0017_kv_key_collation.sql.
--
-- Migration 0015 pinned `kv.expires_at` and left `kv.namespace` and `kv.key`
-- alone, because at that point neither was ever ordered or range-scanned.
-- The key listing of spec section 12.2 changes that: it orders by `key`,
-- resumes from an exclusive cursor with `key > ?`, and filters by prefix with
-- a half-open `key >= ? AND key < ?` range. `namespace` is pinned alongside
-- it so that the grouped namespace listing orders by bytes too, and so that
-- both halves of the (namespace, key) primary key carry one collation.
--
-- The ALTER rebuilds the primary key index, which is what the listing scans.
ALTER TABLE kv ALTER COLUMN namespace TYPE TEXT COLLATE "C";
ALTER TABLE kv ALTER COLUMN key TYPE TEXT COLLATE "C";
