// Vyshka DayZ plugin: telemetry (spec section 8).
//
// Events are buffered and flushed as one event.batch envelope every 2 s or
// at 200 events, whichever comes first (section 8.1); the batch then rides
// the outbox like any envelope, so it is persisted before it is sent, acked
// by the hub, and renumbered across a session change. Up to 2 s of events
// can be lost to a crash before the flush; the outbox's own loss window is
// documented in the README and this one sits inside it.
//
// Snapshots say what is rather than what happened, so they are not
// buffered at all: the plugin asks a game-side source for the current
// state.players body on a cadence, and only when the previous snapshot has
// been acked (see VyshkaOutbox.HasUnacked).

class VyshkaEventBuffer
{
	static const int FLUSH_MS = 2000;
	static const int FLUSH_COUNT = 200;

	ref VyshkaJsonValue m_Pending;   // the events array of the next batch
	int m_FirstPendingMs;            // when the oldest pending event was added
	int m_Emitted;                   // events ever added, for the log

	void VyshkaEventBuffer()
	{
		m_Pending = VyshkaJsonValue.NewArray();
		m_FirstPendingMs = 0;
		m_Emitted = 0;
	}

	// Add queues one event. t is a {namespace}.{name} type; data is the
	// payload object or null for none. The timestamp is taken now, from the
	// game clock, which is what "when the event happened on the game server"
	// means (section 8.1).
	void Add(string t, VyshkaJsonValue data)
	{
		// Not named "event": that is a keyword in Enforce Script.
		VyshkaJsonValue record = VyshkaJsonValue.NewObject();
		record.Set("t", VyshkaJsonValue.NewString(t));
		record.Set("ts", VyshkaJsonValue.NewString(VyshkaClock.NowRfc3339()));
		if (data && data.IsObject())
			record.Set("data", data);
		if (m_Pending.Count() == 0)
			m_FirstPendingMs = VyshkaClock.MonotonicMs();
		m_Pending.Add(record);
		m_Emitted++;
	}

	int Count()
	{
		return m_Pending.Count();
	}

	// Due reports whether the pending events should be flushed now.
	bool Due()
	{
		int count = m_Pending.Count();
		if (count == 0)
			return false;
		if (count >= FLUSH_COUNT)
			return true;
		return VyshkaClock.MonotonicMs() - m_FirstPendingMs >= FLUSH_MS;
	}

	// TakeBatch returns the event.batch body for the pending events, at most
	// FLUSH_COUNT of them, and removes them from the buffer. The remainder,
	// if any, is the next batch.
	string TakeBatch()
	{
		VyshkaJsonValue events = VyshkaJsonValue.NewArray();
		int take = m_Pending.Count();
		if (take > FLUSH_COUNT)
			take = FLUSH_COUNT;
		for (int i = 0; i < take; i++)
			events.Add(m_Pending.At(i));
		VyshkaJsonValue rest = VyshkaJsonValue.NewArray();
		for (int j = take; j < m_Pending.Count(); j++)
			rest.Add(m_Pending.At(j));
		m_Pending = rest;
		if (m_Pending.Count() > 0)
			m_FirstPendingMs = VyshkaClock.MonotonicMs();

		VyshkaJsonValue body = VyshkaJsonValue.NewObject();
		body.Set("events", events);
		return body.Serialize();
	}
}

// VyshkaSnapshotSource is what the game side implements to feed state.*
// snapshots (section 8.3). It lives in the game module so the protocol code
// can ask for a snapshot without knowing what a player is; the world module
// subclasses it.
class VyshkaSnapshotSource
{
	// CapturePlayers returns a serialized state.players body, or "" when no
	// snapshot can be taken right now.
	string CapturePlayers()
	{
		return "";
	}
}
