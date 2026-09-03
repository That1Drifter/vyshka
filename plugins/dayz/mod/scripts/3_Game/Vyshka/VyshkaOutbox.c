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

	ref array<ref VyshkaOutboxEntry> m_Entries;
	int m_NextOrdinal;
	int m_NextSeq;
	int m_Dropped;

	void VyshkaOutbox()
	{
		m_Entries = new array<ref VyshkaOutboxEntry>;
		m_NextOrdinal = 1;
		m_NextSeq = 0;
		m_Dropped = 0;
	}

	int Count()
	{
		return m_Entries.Count();
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
			entry.m_Body = body.Serialize();
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
	// The body is a JSON object, already serialized.
	VyshkaOutboxEntry Append(string envelopeType, string bodyJson)
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
		for (int i = 0; i < m_Entries.Count(); i++)
		{
			m_NextSeq++;
			m_Entries.Get(i).m_Seq = m_NextSeq;
		}
	}

	// BatchJson frames the first BATCH_LIMIT unacked envelopes, in ascending
	// seq order, as the poll request's envelopes array.
	string BatchJson()
	{
		string result = "[";
		int count = m_Entries.Count();
		if (count > BATCH_LIMIT)
			count = BATCH_LIMIT;
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
