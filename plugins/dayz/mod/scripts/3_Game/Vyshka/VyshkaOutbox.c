// Vyshka DayZ plugin: the persistent outbound buffer (spec section 9.3).
//
// Every envelope the plugin sends is written to its own file before it is
// ever put on the wire, and the file is deleted only when the hub's ack
// covers it. Records are keyed by a local ordinal, not by seq: the sequence
// number is assigned at send time from the current session's counter, so a
// session change renumbers the whole buffer for free (section 9.1) and a
// verbatim replay, the bug that strands a buffer forever, cannot happen by
// construction. The stored record is id, type, ts and body, which are exactly
// the fields renumbering must keep.
//
// Durability is what the engine gives: FPrint then CloseFile, with no fsync
// exposed to script. A game-server crash loses at most what the OS had not
// flushed; the loss window is documented rather than hidden.

class VyshkaOutboxEntry
{
	int m_Ordinal;   // creation order, also the file name
	string m_Id;
	string m_Type;
	string m_Ts;
	string m_Body;   // serialized JSON object, re-emitted verbatim
	int m_Seq;       // in the current session's space; 0 until numbered
	int m_Events;    // events carried, for an event.batch; 0 otherwise

	string Path()
	{
		return VyshkaFiles.OUTBOX_DIR + "/" + m_Ordinal.ToString() + ".json";
	}

	// Serialize frames the entry as a wire envelope (spec section 4).
	string Serialize()
	{
		return "{\"v\":1,\"id\":" + VyshkaJson.Quote(m_Id) + ",\"type\":" + VyshkaJson.Quote(m_Type) + ",\"seq\":" + m_Seq.ToString() + ",\"ts\":" + VyshkaJson.Quote(m_Ts) + ",\"body\":" + m_Body + "}";
	}

	// Record is what goes on disk: everything but seq.
	string Record()
	{
		return "{\"id\":" + VyshkaJson.Quote(m_Id) + ",\"type\":" + VyshkaJson.Quote(m_Type) + ",\"ts\":" + VyshkaJson.Quote(m_Ts) + ",\"body\":" + m_Body + "}";
	}
}

class VyshkaOutbox
{
	// The reference ring size from spec section 9.3. Beyond it the oldest
	// unacked envelope is dropped and the drop counted, because a buffer that
	// grows without bound on a server that has lost its hub for a week is
	// worse for the operator than an honest counter.
	static const int CAPACITY = 5000;

	// How many envelopes one poll may carry (section 3.1.2 guarantees a hub
	// accepts at least this many).
	static const int BATCH_LIMIT = 200;

	// How many events one poll may carry across all of its event.batch
	// envelopes. Section 8.1 lets a hub refuse the batches past a per-poll
	// budget (reference cap 1000) while acking them, which would lose the
	// events of a backlog flushed after an outage; staying under the
	// reference cap keeps every batch inside what the hub will store.
	static const int EVENTS_PER_POLL = 1000;

	ref array<ref VyshkaOutboxEntry> m_Entries;
	int m_NextOrdinal;
	int m_NextSeq;
	int m_Dropped;
	int m_BatchLimit;   // envelopes per poll; lowered when a hub refuses a batch as too large
	int m_Rejected;     // envelopes the hub refused as malformed and this outbox set aside
	int m_SentCount;    // how many leading entries the poll in flight carried (see Quarantine)

	void VyshkaOutbox()
	{
		m_Entries = new array<ref VyshkaOutboxEntry>;
		m_NextOrdinal = 1;
		m_NextSeq = 0;
		m_Dropped = 0;
		m_BatchLimit = BATCH_LIMIT;
		m_Rejected = 0;
		m_SentCount = 0;
	}

	int BatchLimit()
	{
		return m_BatchLimit;
	}

	// ShrinkBatch halves the batch size after a hub refused a poll as too
	// large (bad_request, spec section 2.3); it never drops below one
	// envelope, and the limit grows back on the next session.
	void ShrinkBatch()
	{
		int shrunk = m_BatchLimit / 2;
		if (shrunk < 1)
			shrunk = 1;
		if (shrunk != m_BatchLimit)
			VyshkaLog.Warn("outbox: lowering the poll batch from " + m_BatchLimit.ToString() + " to " + shrunk.ToString() + " envelope(s)");
		m_BatchLimit = shrunk;
	}

	// Quarantine takes the envelope at a batch index out of the outbox after
	// the hub refused the batch over it (envelope_invalid, spec section 2.3).
	// The record is moved, not deleted: it lands under rejected/ with the
	// hub's reason, where an operator can read what could not be delivered.
	// The entries behind it move down one seq to close the gap, which is
	// safe because a refused batch was applied in no part. Returns false when
	// the index names nothing in the batch as it was sent: telemetry can be
	// appended while a poll is in flight, so the bound is the count recorded
	// when the batch was framed, not what a fresh framing would carry now.
	bool Quarantine(int index, string reason)
	{
		if (index < 0 || index >= m_SentCount)
			return false;
		m_SentCount = 0;

		VyshkaOutboxEntry entry = m_Entries.Get(index);
		// Ordinals restart after a reboot (Load derives the next one from the
		// outbox alone), so the file name carries the time as well; otherwise
		// a later run's ordinal 1 would overwrite an earlier run's record.
		string rejectedBase = VyshkaFiles.REJECTED_DIR + "/" + VyshkaClock.EpochSeconds().ToString() + "-" + entry.m_Ordinal.ToString();
		string rejectedPath = rejectedBase + ".json";
		// A clock set back can still repeat a name; never overwrite a record.
		int collision = 0;
		while (FileExist(rejectedPath))
		{
			collision++;
			rejectedPath = rejectedBase + "-" + collision.ToString() + ".json";
		}
		string record = "{\"rejected\":" + VyshkaJson.Quote(reason) + ",\"envelope\":" + entry.Record() + "}";
		if (!VyshkaFiles.WriteAll(rejectedPath, record))
			VyshkaLog.Warn("outbox: could not write " + rejectedPath + "; the refused envelope is only in this log line: " + entry.Record());
		if (!DeleteFile(entry.Path()))
			VyshkaLog.Warn("outbox: could not delete " + entry.Path() + "; a restart would try to send the refused envelope again");
		m_Entries.RemoveOrdered(index);
		m_Rejected++;

		int seq = entry.m_Seq;
		for (int i = index; i < m_Entries.Count(); i++)
		{
			VyshkaOutboxEntry later = m_Entries.Get(i);
			if (later.m_Seq > 0)
			{
				later.m_Seq = seq;
				seq++;
			}
		}
		if (m_NextSeq > 0)
			m_NextSeq--;
		VyshkaLog.Error("outbox: the hub refused envelope " + entry.m_Id + " (" + entry.m_Type + ", seq " + entry.m_Seq.ToString() + ") as malformed: " + reason + "; set aside at " + rejectedPath + ", " + m_Entries.Count().ToString() + " envelope(s) still queued");
		return true;
	}

	int Count()
	{
		return m_Entries.Count();
	}

	// HasRoom reports whether n more envelopes fit under the capacity bound.
	// Inbound processing consults this before taking a dispatch, so an action
	// is never executed when its ack and result could not both be queued.
	bool HasRoom(int n)
	{
		return m_Entries.Count() + n <= CAPACITY;
	}

	// HasUnacked reports whether an envelope of the given type is still
	// waiting for the hub's ack. Snapshot publishing consults this so a
	// state.* envelope is only queued when the previous one has landed: a
	// snapshot says what is, so a stale one waiting behind an outage is
	// worth nothing, and a buffer full of them would crowd out the action
	// results and events that are worth keeping.
	bool HasUnacked(string envelopeType)
	{
		for (int i = 0; i < m_Entries.Count(); i++)
		{
			if (m_Entries.Get(i).m_Type == envelopeType)
				return true;
		}
		return false;
	}

	// Load reads every record left on disk by a previous run, in ordinal
	// order, and leaves them unnumbered: the next session start numbers them.
	void Load()
	{
		m_Entries.Clear();
		string fileName;
		FileAttr attributes;
		FindFileHandle handle = FindFile(VyshkaFiles.OUTBOX_DIR + "/*.json", fileName, attributes, FindFileFlags.DIRECTORIES);
		if (handle)
		{
			bool found = true;
			while (found)
			{
				LoadRecord(fileName);
				found = FindNextFile(handle, fileName, attributes);
			}
			CloseFindFile(handle);
		}
		SortByOrdinal();
		int highest = 0;
		for (int i = 0; i < m_Entries.Count(); i++)
		{
			if (m_Entries.Get(i).m_Ordinal > highest)
				highest = m_Entries.Get(i).m_Ordinal;
		}
		m_NextOrdinal = highest + 1;
		if (m_Entries.Count() > 0)
			VyshkaLog.Info("outbox: " + m_Entries.Count().ToString() + " unacked envelope(s) restored from disk");
	}

	protected void LoadRecord(string fileName)
	{
		int dot = fileName.IndexOf(".");
		if (dot <= 0)
			return;
		string ordinalText = fileName.Substring(0, dot);
		for (int i = 0; i < ordinalText.Length(); i++)
		{
			if (!VyshkaJson.IsDigit(ordinalText.Get(i)))
				return;
		}
		int ordinal = ordinalText.ToInt();
		if (ordinal <= 0)
			return;

		string path = VyshkaFiles.OUTBOX_DIR + "/" + fileName;
		VyshkaJsonValue root = VyshkaFiles.ReadJson(path);
		if (!root || !root.IsObject())
		{
			// An unreadable record is a torn write from a crash. It cannot
			// be sent, so it is counted as dropped and cleared.
			VyshkaLog.Warn("outbox: record " + fileName + " is unreadable and was discarded");
			DeleteFile(path);
			m_Dropped++;
			return;
		}
		VyshkaJsonValue body = root.Get("body");
		VyshkaOutboxEntry entry = new VyshkaOutboxEntry();
		entry.m_Ordinal = ordinal;
		entry.m_Id = root.GetString("id", "");
		entry.m_Type = root.GetString("type", "");
		entry.m_Ts = root.GetString("ts", "");
		if (body && body.IsObject())
		{
			entry.m_Body = body.Serialize();
			VyshkaJsonValue events = body.Get("events");
			if (entry.m_Type == "event.batch" && events && events.IsArray())
				entry.m_Events = events.Count();
		}
		else
			entry.m_Body = "{}";
		if (entry.m_Id == "" || entry.m_Type == "")
		{
			VyshkaLog.Warn("outbox: record " + fileName + " has no id or type and was discarded");
			DeleteFile(path);
			m_Dropped++;
			return;
		}
		m_Entries.Insert(entry);
	}

	protected void SortByOrdinal()
	{
		// Insertion sort: the buffer is small and already nearly ordered.
		for (int i = 1; i < m_Entries.Count(); i++)
		{
			VyshkaOutboxEntry current = m_Entries.Get(i);
			int j = i - 1;
			while (j >= 0 && m_Entries.Get(j).m_Ordinal > current.m_Ordinal)
			{
				m_Entries.Set(j + 1, m_Entries.Get(j));
				j--;
			}
			m_Entries.Set(j + 1, current);
		}
	}

	// Append persists a new envelope and numbers it into the current session.
	// The body is a JSON object, already serialized; events is how many
	// events an event.batch body carries, for the per-poll budget.
	VyshkaOutboxEntry Append(string envelopeType, string bodyJson, int events = 0)
	{
		if (m_Entries.Count() >= CAPACITY)
		{
			// Every entry here is unacked, and section 9.3 forbids dropping an
			// unacked envelope. Evicting the oldest would also strand the
			// buffer: it opens a gap below the entries that keep their seq, and
			// the hub's contiguous ack can never cross it. So the new envelope
			// is refused instead, which keeps the sent sequence contiguous, and
			// the refusal is counted so the blocked state is visible
			// (section 9.4). The refused work is recovered by the hub's own
			// re-delivery: the action stays non-terminal and the hub expires it.
			m_Dropped++;
			VyshkaLog.Warn("outbox: full at " + CAPACITY.ToString() + " envelopes; refused a " + envelopeType + " (total refused " + m_Dropped.ToString() + ")");
			return null;
		}

		VyshkaOutboxEntry entry = new VyshkaOutboxEntry();
		entry.m_Ordinal = m_NextOrdinal;
		m_NextOrdinal++;
		entry.m_Id = VyshkaIds.Next();
		entry.m_Type = envelopeType;
		entry.m_Ts = VyshkaClock.NowRfc3339();
		entry.m_Body = bodyJson;
		entry.m_Events = events;
		m_NextSeq++;
		entry.m_Seq = m_NextSeq;

		if (!VyshkaFiles.WriteAll(entry.Path(), entry.Record()))
			VyshkaLog.Warn("outbox: could not persist envelope " + entry.m_Id + "; it will be lost if the server restarts before the hub acks it");
		m_Entries.Insert(entry);
		return entry;
	}

	// Ack drops every entry the hub has durably processed (seq at or below
	// ack in the current session's space).
	void Ack(int ack)
	{
		int i = 0;
		while (i < m_Entries.Count())
		{
			VyshkaOutboxEntry entry = m_Entries.Get(i);
			if (entry.m_Seq > 0 && entry.m_Seq <= ack)
			{
				// A failed delete leaves the file on disk; after a restart Load
				// would treat it as unacked and re-send it. That replay is
				// benign because the hub deduplicates and treats a repeat of a
				// terminal action as a no-op (sections 7, 8.1), but it is
				// surfaced rather than hidden.
				if (!DeleteFile(entry.Path()))
					VyshkaLog.Warn("outbox: could not delete acked envelope " + entry.Path() + "; the hub will dedup it if a restart re-sends it");
				m_Entries.RemoveOrdered(i);
			}
			else
			{
				i++;
			}
		}
	}

	// Renumber starts a new sequence space: every buffered envelope gets a
	// fresh seq counting from 1 in creation order, keeping everything else
	// (spec section 9.1).
	void Renumber()
	{
		m_NextSeq = 0;
		m_SentCount = 0;
		for (int i = 0; i < m_Entries.Count(); i++)
		{
			m_NextSeq++;
			m_Entries.Get(i).m_Seq = m_NextSeq;
		}
		m_BatchLimit = BATCH_LIMIT;
	}

	// BatchCount is how many leading entries the next poll carries: at most
	// m_BatchLimit envelopes, and at most EVENTS_PER_POLL events across them.
	// The first entry always goes, whatever it carries, so a batch can never
	// be stuck behind the budget.
	int BatchCount()
	{
		int count = 0;
		int events = 0;
		while (count < m_Entries.Count() && count < m_BatchLimit)
		{
			int carried = m_Entries.Get(count).m_Events;
			if (count > 0 && events + carried > EVENTS_PER_POLL)
				break;
			events += carried;
			count++;
		}
		return count;
	}

	// BatchJson frames the leading unacked envelopes, in ascending seq order,
	// as the poll request's envelopes array, and records how many it framed.
	string BatchJson()
	{
		string result = "[";
		int count = BatchCount();
		m_SentCount = count;
		for (int i = 0; i < count; i++)
		{
			if (i > 0)
				result += ",";
			result += m_Entries.Get(i).Serialize();
		}
		result += "]";
		return result;
	}
}
