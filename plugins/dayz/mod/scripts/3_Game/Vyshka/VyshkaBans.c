// Vyshka DayZ plugin: the ban list.
//
// The engine exposes no scripted ban API, only DisconnectPlayer, so a ban is
// the plugin's own record: an identity the plugin refuses at connect until
// the entry is removed or expires. The list is independent of the engine's
// and BattlEye's own lists. It lives with the rest of the plugin's state as
// $profile:Vyshka/bans.json, one object per entry, so an operator can read
// and edit it by hand while the server is down:
//
//   { "bans": [ { "id": "7656...", "name": "Survivor", "reason": "...",
//                 "bannedAt": "2026-09-14T10:00:00Z", "expiresAt": null,
//                 "actionId": "01M2..." } ] }
//
// A file that does not parse is left alone: the list is read as empty, the
// ban and unban actions refuse to run rather than overwrite what the
// operator wrote, and the log says so. Expired entries are dropped when the
// file is loaded and when an identity is looked up.

class VyshkaBanEntry
{
	// Bounds on what an entry carries into a kick event, whether the entry
	// came from a dispatch (already bounded) or from the operator's editor.
	static const int MAX_NAME = 200;
	static const int MAX_REASON = 200;
	static const int MAX_ACTION_ID = 64;

	string m_Id;          // plain Steam64 id
	string m_Name;        // the name the player had when banned, for the operator
	string m_Reason;
	string m_BannedAt;    // RFC 3339
	bool m_Permanent;     // no expiry; m_ExpiresEpoch is then meaningless
	int m_ExpiresEpoch;   // when a finite ban ends, epoch seconds
	string m_ActionId;    // the dispatch that made the entry, for the audit log

	bool Expired(int nowEpoch)
	{
		return !m_Permanent && nowEpoch >= m_ExpiresEpoch;
	}

	VyshkaJsonValue ToJson()
	{
		VyshkaJsonValue entry = VyshkaJsonValue.NewObject();
		entry.Set("id", VyshkaJsonValue.NewString(m_Id));
		entry.Set("name", VyshkaJsonValue.NewString(m_Name));
		entry.Set("reason", VyshkaJsonValue.NewString(m_Reason));
		entry.Set("bannedAt", VyshkaJsonValue.NewString(m_BannedAt));
		if (m_Permanent)
			entry.Set("expiresAt", VyshkaJsonValue.NewNull());
		else
			entry.Set("expiresAt", VyshkaJsonValue.NewString(VyshkaClock.FormatRfc3339(m_ExpiresEpoch)));
		if (m_ActionId != "")
			entry.Set("actionId", VyshkaJsonValue.NewString(m_ActionId));
		return entry;
	}

	// FromJson reads one entry; null when it carries no id. A missing or
	// null expiresAt is a permanent ban; one that parses is finite, whatever
	// instant it names (an operator who writes 1970 has lifted the ban); one
	// that does not parse reads as permanent rather than as expired, so a
	// hand-edited typo widens a ban instead of lifting it. Text members are
	// bounded here because the file is operator-editable and the reason
	// travels in the kick event.
	static VyshkaBanEntry FromJson(VyshkaJsonValue value)
	{
		if (!value || !value.IsObject())
			return null;
		VyshkaBanEntry entry = new VyshkaBanEntry();
		entry.m_Id = VyshkaAction.Bound(value.GetString("id", ""), 128);
		if (entry.m_Id == "")
			return null;
		entry.m_Name = VyshkaAction.Bound(value.GetString("name", ""), MAX_NAME);
		entry.m_Reason = VyshkaAction.Bound(value.GetString("reason", ""), MAX_REASON);
		entry.m_BannedAt = VyshkaAction.Bound(value.GetString("bannedAt", ""), 40);
		entry.m_ActionId = VyshkaAction.Bound(value.GetString("actionId", ""), MAX_ACTION_ID);
		entry.m_Permanent = true;
		entry.m_ExpiresEpoch = 0;
		VyshkaJsonValue expires = value.Get("expiresAt");
		if (expires && expires.IsString())
		{
			int epoch;
			if (VyshkaClock.ParseRfc3339(expires.m_Text, epoch))
			{
				entry.m_Permanent = false;
				entry.m_ExpiresEpoch = epoch;
			}
			else
				VyshkaLog.Warn("ban entry " + entry.m_Id + " has an unreadable expiresAt " + expires.m_Text + "; treating the ban as permanent");
		}
		else if (expires && !expires.IsNull())
			VyshkaLog.Warn("ban entry " + entry.m_Id + " has an expiresAt that is not a string; treating the ban as permanent");
		return entry;
	}
}

class VyshkaBans
{
	static ref map<string, ref VyshkaBanEntry> s_Entries;
	static bool s_Loaded;
	static bool s_Unwritable;   // the file exists but is not the plugin's shape; never overwrite it

	static map<string, ref VyshkaBanEntry> Entries()
	{
		if (!s_Entries)
			s_Entries = new map<string, ref VyshkaBanEntry>;
		return s_Entries;
	}

	// Load reads the file once per boot. Missing is an empty list; malformed
	// is an empty list that refuses writes.
	static void Load()
	{
		if (s_Loaded)
			return;
		s_Loaded = true;
		s_Unwritable = false;
		Entries().Clear();
		if (!FileExist(VyshkaFiles.BANS_PATH))
			return;
		VyshkaJsonValue root = VyshkaFiles.ReadJson(VyshkaFiles.BANS_PATH);
		VyshkaJsonValue list;
		if (root && root.IsObject())
			list = root.Get("bans");
		if (!list || !list.IsArray())
		{
			s_Unwritable = true;
			VyshkaLog.Error(VyshkaFiles.BANS_PATH + " is not a JSON object with a bans array; no ban is enforced from it and the ban and unban actions will refuse to run until it is fixed or removed");
			return;
		}
		int now = VyshkaClock.EpochSeconds();
		int expired = 0;
		for (int i = 0; i < list.Count(); i++)
		{
			VyshkaBanEntry entry = VyshkaBanEntry.FromJson(list.At(i));
			if (!entry)
			{
				VyshkaLog.Warn(VyshkaFiles.BANS_PATH + ": entry " + i.ToString() + " carries no id and is ignored");
				continue;
			}
			if (entry.Expired(now))
			{
				expired++;
				continue;
			}
			Entries().Set(entry.m_Id, entry);
		}
		if (expired > 0)
			Save();
		VyshkaLog.Info("ban list loaded: " + Entries().Count().ToString() + " active entr(ies), " + expired.ToString() + " expired one(s) dropped");
	}

	// Reset forgets the loaded state so the next Load rereads the file; the
	// harness restarts the plugin inside one process.
	static void Reset()
	{
		s_Loaded = false;
	}

	static bool Writable(out string error)
	{
		Load();
		if (s_Unwritable)
		{
			error = VyshkaFiles.BANS_PATH + " could not be read as the plugin's ban list; fix or remove it before banning or unbanning";
			return false;
		}
		return true;
	}

	// Find returns the active entry for an identity, or null. An entry that
	// expired since it was loaded is dropped here.
	static VyshkaBanEntry Find(string id)
	{
		Load();
		VyshkaBanEntry entry = Entries().Get(id);
		if (!entry)
			return null;
		if (entry.Expired(VyshkaClock.EpochSeconds()))
		{
			Entries().Remove(id);
			Save();
			return null;
		}
		return entry;
	}

	// Add records or replaces an entry and writes the file.
	static bool Add(VyshkaBanEntry entry, out string error)
	{
		if (!Writable(error))
			return false;
		Entries().Set(entry.m_Id, entry);
		if (!Save())
		{
			error = "the ban was recorded in memory but " + VyshkaFiles.BANS_PATH + " could not be written; it will not survive a restart";
			return false;
		}
		return true;
	}

	// Remove lifts a ban. false with an error when the identity is not
	// banned or the file cannot be written.
	static bool Remove(string id, out VyshkaBanEntry removed, out string error)
	{
		if (!Writable(error))
			return false;
		removed = Find(id);
		if (!removed)
		{
			error = "player " + id + " is not banned";
			return false;
		}
		Entries().Remove(id);
		if (!Save())
		{
			error = "the ban was lifted in memory but " + VyshkaFiles.BANS_PATH + " could not be written; it will be enforced again after a restart";
			return false;
		}
		return true;
	}

	// Count is the number of active entries: expired ones are dropped first,
	// so a result's activeBans never counts a ban that has already ended.
	static int Count()
	{
		Load();
		Prune();
		return Entries().Count();
	}

	// Prune drops every expired entry and writes the file when any was.
	static void Prune()
	{
		int now = VyshkaClock.EpochSeconds();
		array<string> expired = new array<string>;
		for (int i = 0; i < Entries().Count(); i++)
		{
			if (Entries().GetElement(i).Expired(now))
				expired.Insert(Entries().GetKey(i));
		}
		for (int j = 0; j < expired.Count(); j++)
			Entries().Remove(expired.Get(j));
		if (expired.Count() > 0)
			Save();
	}

	static bool Save()
	{
		VyshkaJsonValue list = VyshkaJsonValue.NewArray();
		for (int i = 0; i < Entries().Count(); i++)
			list.Add(Entries().GetElement(i).ToJson());
		VyshkaJsonValue root = VyshkaJsonValue.NewObject();
		root.Set("bans", list);
		if (VyshkaFiles.WriteJson(VyshkaFiles.BANS_PATH, root))
			return true;
		VyshkaLog.Error("could not write " + VyshkaFiles.BANS_PATH);
		return false;
	}
}
