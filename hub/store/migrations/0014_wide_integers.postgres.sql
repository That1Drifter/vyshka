-- Protocol-sized integers (issue #20, Postgres backend), Postgres variant of
-- 0014_wide_integers.sql.
--
-- Postgres INTEGER is 32 bits. The protocol bounds these values at 2^53
-- (spec/protocol.md sections 6.1, 9.1, and 12), so a valid manifest revision,
-- key revision, sequence number, counter, or duration above 2^31 would be
-- refused by the driver and roll back the transaction that carried it,
-- including the ack. Widened here rather than in the migrations that created
-- them, because those have shipped.
ALTER TABLE sessions ALTER COLUMN outbound_seq TYPE BIGINT;
ALTER TABLE sessions ALTER COLUMN outbound_ack TYPE BIGINT;
ALTER TABLE sessions ALTER COLUMN inbound_ack TYPE BIGINT;
ALTER TABLE sessions ALTER COLUMN inbound_count TYPE BIGINT;
ALTER TABLE outbound_envelopes ALTER COLUMN seq TYPE BIGINT;
ALTER TABLE manifests ALTER COLUMN revision TYPE BIGINT;
ALTER TABLE actions ALTER COLUMN duration_ms TYPE BIGINT;
ALTER TABLE kv ALTER COLUMN revision TYPE BIGINT;
