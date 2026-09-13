-- At most one live session per server (spec/protocol.md section 5.3), at the
-- schema level.
--
-- StartSession ends the old session and inserts the new one in one
-- transaction, serialized per server by a row lock on Postgres and by the
-- single connection on SQLite. This index is the backstop behind that: if a
-- future change ever let two session starts interleave, the second insert
-- fails on this index instead of leaving two sessions that both number
-- envelopes into their own sequence space. Sessions that have ended are out
-- of the index, so history is unaffected.
CREATE UNIQUE INDEX IF NOT EXISTS sessions_one_live
    ON sessions (server_id) WHERE ended_at IS NULL;
