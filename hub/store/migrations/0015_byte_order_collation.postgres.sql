-- Byte-order comparison of text columns (issue #20, Postgres backend),
-- Postgres variant of 0015_byte_order_collation.sql.
--
-- Every TEXT column the store compares with <, >, <=, >= or sorts with ORDER
-- BY is pinned to the "C" collation, which is byte order. Under a glibc
-- locale collation (en_US.utf8 is the common default) the fixed-width
-- timestamps, the ULID tiebreaks, and the events namespace range from "ns."
-- to "ns/" all misorder: a namespace filter matches nothing and an action
-- can read as expired before its deadline. Columns only ever compared for
-- equality are left alone; deterministic collations agree on equality.
-- Indexes on the altered columns are rebuilt by the ALTER.
ALTER TABLE servers ALTER COLUMN id TYPE TEXT COLLATE "C";
ALTER TABLE servers ALTER COLUMN created_at TYPE TEXT COLLATE "C";
ALTER TABLE sessions ALTER COLUMN created_at TYPE TEXT COLLATE "C";
ALTER TABLE sessions ALTER COLUMN expires_at TYPE TEXT COLLATE "C";
ALTER TABLE outbound_envelopes ALTER COLUMN id TYPE TEXT COLLATE "C";
ALTER TABLE outbound_envelopes ALTER COLUMN created_at TYPE TEXT COLLATE "C";
ALTER TABLE actions ALTER COLUMN id TYPE TEXT COLLATE "C";
ALTER TABLE actions ALTER COLUMN expires_at TYPE TEXT COLLATE "C";
ALTER TABLE events ALTER COLUMN id TYPE TEXT COLLATE "C";
ALTER TABLE events ALTER COLUMN type TYPE TEXT COLLATE "C";
ALTER TABLE events ALTER COLUMN occurred_at TYPE TEXT COLLATE "C";
ALTER TABLE events ALTER COLUMN expires_at TYPE TEXT COLLATE "C";
ALTER TABLE event_batches ALTER COLUMN expires_at TYPE TEXT COLLATE "C";
ALTER TABLE admin_tokens ALTER COLUMN id TYPE TEXT COLLATE "C";
ALTER TABLE admin_tokens ALTER COLUMN created_at TYPE TEXT COLLATE "C";
ALTER TABLE admin_tokens ALTER COLUMN expires_at TYPE TEXT COLLATE "C";
ALTER TABLE audit_records ALTER COLUMN id TYPE TEXT COLLATE "C";
ALTER TABLE audit_records ALTER COLUMN at TYPE TEXT COLLATE "C";
ALTER TABLE audit_records ALTER COLUMN expires_at TYPE TEXT COLLATE "C";
ALTER TABLE webhooks ALTER COLUMN id TYPE TEXT COLLATE "C";
ALTER TABLE webhooks ALTER COLUMN created_at TYPE TEXT COLLATE "C";
ALTER TABLE webhook_deliveries ALTER COLUMN id TYPE TEXT COLLATE "C";
ALTER TABLE webhook_deliveries ALTER COLUMN next_attempt_at TYPE TEXT COLLATE "C";
ALTER TABLE webhook_deliveries ALTER COLUMN created_at TYPE TEXT COLLATE "C";
ALTER TABLE webhook_deliveries ALTER COLUMN finished_at TYPE TEXT COLLATE "C";
ALTER TABLE kv ALTER COLUMN expires_at TYPE TEXT COLLATE "C";
ALTER TABLE state_snapshots ALTER COLUMN expires_at TYPE TEXT COLLATE "C";
ALTER TABLE state_snapshot_dedup ALTER COLUMN expires_at TYPE TEXT COLLATE "C";
