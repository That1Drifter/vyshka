-- Byte-order comparison of text columns (issue #20, Postgres backend),
-- Postgres variant of 0015_byte_order_collation.sql.
--
-- Every TEXT column the store compares with <, >, <=, >= or sorts with ORDER
-- BY is pinned to the "C" collation, which is byte order. Under a glibc
-- locale collation (en_US.utf8 is the common default) the fixed-width
-- timestamps, the ULID tiebreaks, and the events namespace range from "ns."
-- to "ns/" all misorder: a namespace filter matches nothing and an action
-- can read as expired before its deadline.
--
-- The id and foreign-key columns are pinned as well, although equality is
-- the same under any deterministic collation: a join or subquery that
-- compares a "C" column with a default-collated one takes the "C" collation
-- for the comparison, and the planner will not probe the default-collated
-- side's index for it. Pinning both sides keeps every server_id, session_id,
-- envelope_id, and webhook_id lookup on its index. Columns compared only
-- with a parameter or a literal (states, hashes, keys, link_state,
-- last_seen_at) are left alone. Indexes on the altered columns are rebuilt
-- by the ALTER; foreign keys stay valid, since the type is unchanged.
ALTER TABLE servers ALTER COLUMN id TYPE TEXT COLLATE "C";
ALTER TABLE servers ALTER COLUMN created_at TYPE TEXT COLLATE "C";
ALTER TABLE enrollment_tokens ALTER COLUMN server_id TYPE TEXT COLLATE "C";
ALTER TABLE sessions ALTER COLUMN id TYPE TEXT COLLATE "C";
ALTER TABLE sessions ALTER COLUMN server_id TYPE TEXT COLLATE "C";
ALTER TABLE sessions ALTER COLUMN created_at TYPE TEXT COLLATE "C";
ALTER TABLE sessions ALTER COLUMN expires_at TYPE TEXT COLLATE "C";
ALTER TABLE outbound_envelopes ALTER COLUMN id TYPE TEXT COLLATE "C";
ALTER TABLE outbound_envelopes ALTER COLUMN server_id TYPE TEXT COLLATE "C";
ALTER TABLE outbound_envelopes ALTER COLUMN session_id TYPE TEXT COLLATE "C";
ALTER TABLE outbound_envelopes ALTER COLUMN created_at TYPE TEXT COLLATE "C";
ALTER TABLE manifests ALTER COLUMN server_id TYPE TEXT COLLATE "C";
ALTER TABLE actions ALTER COLUMN id TYPE TEXT COLLATE "C";
ALTER TABLE actions ALTER COLUMN server_id TYPE TEXT COLLATE "C";
ALTER TABLE actions ALTER COLUMN envelope_id TYPE TEXT COLLATE "C";
ALTER TABLE actions ALTER COLUMN expires_at TYPE TEXT COLLATE "C";
ALTER TABLE events ALTER COLUMN id TYPE TEXT COLLATE "C";
ALTER TABLE events ALTER COLUMN server_id TYPE TEXT COLLATE "C";
ALTER TABLE events ALTER COLUMN type TYPE TEXT COLLATE "C";
ALTER TABLE events ALTER COLUMN occurred_at TYPE TEXT COLLATE "C";
ALTER TABLE events ALTER COLUMN expires_at TYPE TEXT COLLATE "C";
ALTER TABLE event_batches ALTER COLUMN server_id TYPE TEXT COLLATE "C";
ALTER TABLE event_batches ALTER COLUMN envelope_id TYPE TEXT COLLATE "C";
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
ALTER TABLE webhook_deliveries ALTER COLUMN webhook_id TYPE TEXT COLLATE "C";
ALTER TABLE webhook_deliveries ALTER COLUMN server_id TYPE TEXT COLLATE "C";
ALTER TABLE webhook_deliveries ALTER COLUMN next_attempt_at TYPE TEXT COLLATE "C";
ALTER TABLE webhook_deliveries ALTER COLUMN created_at TYPE TEXT COLLATE "C";
ALTER TABLE webhook_deliveries ALTER COLUMN finished_at TYPE TEXT COLLATE "C";
ALTER TABLE kv ALTER COLUMN expires_at TYPE TEXT COLLATE "C";
ALTER TABLE state_snapshots ALTER COLUMN server_id TYPE TEXT COLLATE "C";
ALTER TABLE state_snapshots ALTER COLUMN envelope_id TYPE TEXT COLLATE "C";
ALTER TABLE state_snapshots ALTER COLUMN expires_at TYPE TEXT COLLATE "C";
ALTER TABLE state_snapshot_dedup ALTER COLUMN server_id TYPE TEXT COLLATE "C";
ALTER TABLE state_snapshot_dedup ALTER COLUMN envelope_id TYPE TEXT COLLATE "C";
ALTER TABLE state_snapshot_dedup ALTER COLUMN expires_at TYPE TEXT COLLATE "C";
