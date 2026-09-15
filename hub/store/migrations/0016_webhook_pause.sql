-- Pausing a webhook (spec/protocol.md sections 11.2 and 11.5).
--
-- A paused webhook is one the hub may not talk to: no attempt, no retry. It
-- still owes what it matched, so matching notifications keep creating pending
-- deliveries that wait for the resume, because nothing may be dropped
-- silently. NULL means active, which is what every existing row is.
--
-- The column is compared only against NULL and written only as a timestamp,
-- so it needs no collation pin of its own (migration 0015) and plain ALTER
-- TABLE ... ADD COLUMN is valid on both engines.
ALTER TABLE webhooks ADD COLUMN paused_at TEXT;

-- Replaying a delivery (section 11.5) re-arms a row an attempt may still be
-- in flight for. The generation counts replays; an attempt books its outcome
-- only against the generation it was made under, so an outcome that lands
-- after a replay is discarded rather than consuming the attempt the replay
-- promised. Every existing row starts at 0.
ALTER TABLE webhook_deliveries ADD COLUMN generation INTEGER NOT NULL DEFAULT 0;
