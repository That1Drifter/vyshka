// Vyshka DayZ plugin: the installation ban list (spec section 13).
//
// The hub holds one ban list for every server of the installation. The plugin
// declares the "bans" capability in its manifest (section 6.7), reads the list
// page by page whenever the revision the hub reports differs from the one it
// holds, and enforces it beside its own ban list (VyshkaBans), the union of
// the two: an identity on either is refused at connect, and applying a new
// revision disconnects anyone online on it.
//
// The applied copy is kept at $profile:Vyshka/installation-bans.json and
// enforced from boot, before any session, so a hub outage never lifts an
// installation ban. It is the hub's list, overwritten wholesale on every
// apply: an operator edits it through the hub, and a hand edit here is lost
// at the next one. It is written one entry per line (VyshkaFiles.WriteJson),
// because the engine's file reader faults the process on a 64 KiB line.
//
//   { "revision": 14, "bans": [ { "id": "01M3...", "player": { "platform": "steam",
//     "id": "7656..." }, "reason": "...", "name": "...", "expiresAt": null } ] }
//
// Revisions travel as the text the hub wrote, never through the engine's
// 32-bit int: the protocol bounds them at 2^53, and a comparison of saturated
// numbers would take two different revisions for one.

class VyshkaInstallationBanEntry
{
	static const int MAX_TEXT = 200;
	static const int MAX_ID = 128;

	string m_BanId;       // the hub's ban id, named in the kick event
	string m_Id;          // plain Steam64 id
	string m_Reason;
	string m_Name;
	bool m_Permanent;     // no expiry; m_ExpiresEpoch is then meaningless
	int m_ExpiresEpoch;
	string m_ExpiresText; // expiresAt as the hub wrote it, written back unchanged

	// Expired honors the entry's expiry by this server's own clock (section
	// 13.4): the hub takes an expired ban off its list within a minute, and
	// the entry is no ban from the instant it passes whatever the hub has yet
	// to do.
	bool Expired(int nowEpoch)
	{
		return !m_Permanent && nowEpoch >= m_ExpiresEpoch;
	}

	VyshkaJsonValue ToJson()
	{
		VyshkaJsonValue player = VyshkaJsonValue.NewObject();
		player.Set("platform", VyshkaJsonValue.NewString(VyshkaInstallationBans.PLATFORM));
		player.Set("id", VyshkaJsonValue.NewString(m_Id));
		VyshkaJsonValue entry = VyshkaJsonValue.NewObject();
		entry.Set("id", VyshkaJsonValue.NewString(m_BanId));
		entry.Set("player", player);
		entry.Set("reason", VyshkaJsonValue.NewString(m_Reason));
		entry.Set("name", VyshkaJsonValue.NewString(m_Name));
		if (m_Permanent)
			entry.Set("expiresAt", VyshkaJsonValue.NewNull());
		else
			entry.Set("expiresAt", VyshkaJsonValue.NewString(m_ExpiresText));
		return entry;
	}

	// FromJson reads one entry of a page or of the stored file; null when it
	// is not an entry of this server's platform or carries no identity. The
	// platform is checked here because one list serves an installation that
	// runs several games (section 13.4), and an entry for another game's
	// platform names nobody who can connect here. An expiresAt that does not
	// parse reads as permanent rather than as expired, as a local ban's does:
	// a typo widens a ban rather than lifting it.
	static VyshkaInstallationBanEntry FromJson(VyshkaJsonValue value)
	{
		if (!value || !value.IsObject())
			return null;
		VyshkaJsonValue player = value.Get("player");
		if (!player || !player.IsObject())
			return null;
		if (player.GetString("platform", "") != VyshkaInstallationBans.PLATFORM)
			return null;
		VyshkaInstallationBanEntry entry = new VyshkaInstallationBanEntry();
		entry.m_Id = VyshkaAction.Bound(player.GetString("id", ""), MAX_ID);
		if (entry.m_Id == "")
			return null;
		entry.m_BanId = VyshkaAction.Bound(value.GetString("id", ""), MAX_ID);
		entry.m_Reason = VyshkaAction.Bound(value.GetString("reason", ""), MAX_TEXT);
		entry.m_Name = VyshkaAction.Bound(value.GetString("name", ""), MAX_TEXT);
		entry.m_Permanent = true;
		entry.m_ExpiresEpoch = 0;
		entry.m_ExpiresText = "";
		VyshkaJsonValue expires = value.Get("expiresAt");
		if (expires && expires.IsString())
		{
			int epoch;
			if (VyshkaClock.ParseRfc3339(expires.m_Text, epoch))
			{
				entry.m_Permanent = false;
				entry.m_ExpiresEpoch = epoch;
				entry.m_ExpiresText = expires.m_Text;
			}
			else
				VyshkaLog.Warn("installation ban " + entry.m_BanId + " has an unreadable expiresAt " + expires.m_Text + "; treating it as permanent");
		}
		return entry;
	}
}

// VyshkaBanEnforcer is what applying a revision calls to disconnect the
// players online on it. Disconnecting needs the world module's player
// classes, which this module cannot name, so the world module assigns a
// subclass at boot (VyshkaBoot.Start); this base does nothing.
class VyshkaBanEnforcer
{
	void OnApplied()
	{
	}
}

class VyshkaInstallationBans
{
	// The platform this server's identities live on (spec section 8.2): the
	// plain Steam64 id every Vyshka player reference on DayZ carries.
	static const string PLATFORM = "steam";

	static ref map<string, ref VyshkaInstallationBanEntry> s_Entries;
	static string s_Revision;     // the revision enforced, as the hub wrote it; "" before any
	static bool s_Loaded;
	static ref VyshkaBanEnforcer s_Enforcer;

	static map<string, ref VyshkaInstallationBanEntry> Entries()
	{
		if (!s_Entries)
			s_Entries = new map<string, ref VyshkaInstallationBanEntry>;
		return s_Entries;
	}

	// Load reads the stored copy once per boot. Missing is no list at all
	// (revision ""), so the first session walks one; a file that does not
	// parse is the same, with a line saying so, since the hub's copy is the
	// truth and the next walk replaces the file.
	static void Load()
	{
		if (s_Loaded)
			return;
		s_Loaded = true;
		s_Revision = "";
		Entries().Clear();
		if (!FileExist(VyshkaFiles.INSTALLATION_BANS_PATH))
			return;
		VyshkaJsonValue root = VyshkaFiles.ReadJson(VyshkaFiles.INSTALLATION_BANS_PATH);
		VyshkaJsonValue list;
		VyshkaJsonValue revision;
		if (root && root.IsObject())
		{
			list = root.Get("bans");
			revision = root.Get("revision");
		}
		if (!list || !list.IsArray() || !revision || !revision.IsNumber())
		{
			VyshkaLog.Error(VyshkaFiles.INSTALLATION_BANS_PATH + " is not the installation ban list this plugin writes; no installation ban is enforced until the hub's list is read again");
			return;
		}
		for (int i = 0; i < list.Count(); i++)
		{
			VyshkaInstallationBanEntry entry = VyshkaInstallationBanEntry.FromJson(list.At(i));
			if (entry)
				Entries().Set(entry.m_Id, entry);
		}
		s_Revision = revision.m_Text;
		int count = Entries().Count();
		VyshkaLog.Info("installation ban list loaded: revision " + s_Revision + ", " + count.ToString() + " entr(ies)");
	}

	// Reset forgets the loaded state so the next Load rereads the file; the
	// harness restarts the plugin inside one process.
	static void Reset()
	{
		s_Loaded = false;
	}

	static string Revision()
	{
		Load();
		return s_Revision;
	}

	// Find returns the installation ban on an identity, or null: none, or one
	// whose expiry has passed by this server's clock.
	static VyshkaInstallationBanEntry Find(string id)
	{
		Load();
		VyshkaInstallationBanEntry entry = Entries().Get(id);
		if (!entry || entry.Expired(VyshkaClock.EpochSeconds()))
			return null;
		return entry;
	}

	// Count is the number of entries in force now.
	static int Count()
	{
		Load();
		int now = VyshkaClock.EpochSeconds();
		int count = 0;
		for (int i = 0; i < Entries().Count(); i++)
		{
			if (!Entries().GetElement(i).Expired(now))
				count++;
		}
		return count;
	}

	// Apply stores a whole revision and puts it in force (section 13.4): the
	// file first, and the memory only once it is written, so a list that
	// could not be kept is not enforced for this boot alone and forgotten at
	// the next; on failure the previous list stays in force and error says
	// why. The enforcer then disconnects whoever is online on the new list.
	static bool Apply(string revisionText, array<ref VyshkaInstallationBanEntry> entries, out string error)
	{
		Load();
		VyshkaJsonValue revision = VyshkaJson.Parse(revisionText);
		if (!revision || !revision.IsNumber())
		{
			error = "the revision " + revisionText + " is not a number";
			return false;
		}
		VyshkaJsonValue list = VyshkaJsonValue.NewArray();
		for (int i = 0; i < entries.Count(); i++)
		{
			VyshkaInstallationBanEntry one = entries.Get(i);
			VyshkaJsonValue encoded = one.ToJson();
			list.Add(encoded);
		}
		VyshkaJsonValue root = VyshkaJsonValue.NewObject();
		root.Set("revision", revision);
		root.Set("bans", list);
		if (!VyshkaFiles.WriteJson(VyshkaFiles.INSTALLATION_BANS_PATH, root))
		{
			error = VyshkaFiles.INSTALLATION_BANS_PATH + " could not be written";
			return false;
		}
		Entries().Clear();
		for (int j = 0; j < entries.Count(); j++)
		{
			VyshkaInstallationBanEntry entry = entries.Get(j);
			Entries().Set(entry.m_Id, entry);
		}
		s_Revision = revisionText;
		if (s_Enforcer)
			s_Enforcer.OnApplied();
		return true;
	}
}

// VyshkaBanSync reads the hub's list (spec section 13.3) on a transport of its
// own, so a walk never waits behind the held poll. It walks whenever the
// revision the hub reports differs from the one enforced (lower included: a
// hub restored from a backup can report less), holds the walk to the revision
// its first page was served at, and hands the list to VyshkaInstallationBans
// only once every page is in hand. Pages go back to back: the plugin's parser
// is linear since issue #108 and a page is bounded, so no one answer is a
// long frame. A walk that fails partway changes nothing and is tried again
// after RETRY_MS; a conflict starts it over at once.
class VyshkaBanSync : VyshkaResponseSink
{
	static const int REQUEST_PAGE = 1;
	static const int PAGE_LIMIT = 100;
	static const int RETRY_MS = 30000;           // the flags lookup's cadence
	static const int SESSION_RETRY_MS = 1000;    // after session_invalid, once a new session exists
	static const int RESTARTS_MAX = 5;           // conflicts in a row before the walk waits a retry out
	static const int PAGES_MAX = 10000;          // a walk this long is a hub defect, not a list

	ref VyshkaTransport m_Transport;
	bool m_Served;          // the hub reports a revision at all; a hub that serves no list is left alone
	string m_Known;         // the latest revision the hub reported
	bool m_Due;             // a walk is owed
	int m_NextTryMs;
	string m_RefusedToken;  // the session token a page was refused under; the walk waits for another
	int m_BudgetMs;

	// The walk in progress.
	bool m_Walking;
	bool m_ToldDuringWalk;  // the hub reported a revision while the walk ran, so m_Known is fresher than the walk
	string m_WalkRevision;  // "" until the first page
	string m_Cursor;
	int m_Pages;
	int m_Restarts;
	ref array<ref VyshkaInstallationBanEntry> m_Walked;

	bool Init(string hubUrl)
	{
		m_BudgetMs = 45000;
		m_Known = "";
		m_Walked = new array<ref VyshkaInstallationBanEntry>;
		m_Transport = new VyshkaTransport();
		return m_Transport.Init(hubUrl + "/plugin/v1/bans/", this);
	}

	// SetBudgetMs keeps the watchdog above the engine's read timeout plus its
	// connection timeout, as the store client's is (VyshkaPlugin.ApplyReadTimeout).
	void SetBudgetMs(int ms)
	{
		if (ms < 5000)
			ms = 5000;
		m_BudgetMs = ms;
	}

	// OnSession reads server.bansRevision from a session response. It returns
	// true when the session began with the revision already enforced, which
	// the plugin reports (section 13.4); otherwise a walk is owed, or, when
	// the hub reports no revision, nothing at all.
	bool OnSession(VyshkaJsonValue revision)
	{
		if (!revision || !revision.IsNumber())
		{
			if (m_Served)
				VyshkaLog.Info("the hub reports no installation ban list; enforcing the stored one, revision " + VyshkaInstallationBans.Revision());
			m_Served = false;
			m_Due = false;
			return false;
		}
		m_Served = true;
		Told(revision.m_Text);
		if (m_Known == VyshkaInstallationBans.Revision())
			return true;
		Owe();
		return false;
	}

	// OnChanged takes a bans.changed nudge (section 13.3).
	void OnChanged(VyshkaJsonValue body)
	{
		if (!body || !body.IsObject())
			return;
		VyshkaJsonValue revision = body.Get("revision");
		if (!revision || !revision.IsNumber())
		{
			VyshkaLog.Warn("ignoring a bans.changed without a revision");
			return;
		}
		m_Served = true;
		Told(revision.m_Text);
		if (m_Known != VyshkaInstallationBans.Revision())
			Owe();
	}

	// Told records a revision the hub reported. One that arrives while a walk
	// runs is fresher than the walk's (the list moved on under it, which the
	// walk's pinned revision hides), so the walk's end must not take its own
	// revision for the latest.
	void Told(string revision)
	{
		m_Known = revision;
		if (m_Walking)
			m_ToldDuringWalk = true;
	}

	void Owe()
	{
		m_Due = true;
		m_NextTryMs = 0;
	}

	void Tick(string sessionToken)
	{
		m_Transport.CheckWatchdog();
		if (m_Transport.IsInFlight() || !m_Served || !m_Due || sessionToken == "")
			return;
		if (m_RefusedToken != "" && m_RefusedToken == sessionToken)
			return;
		if (VyshkaClock.MonotonicMs() < m_NextTryMs)
			return;
		if (!m_Walking)
			Begin();
		Request(sessionToken);
	}

	void Begin()
	{
		m_Walking = true;
		m_ToldDuringWalk = false;
		m_WalkRevision = "";
		m_Cursor = "";
		m_Pages = 0;
		m_Walked.Clear();
	}

	void Request(string sessionToken)
	{
		string path = "get?limit=" + PAGE_LIMIT.ToString();
		if (m_Cursor != "")
			path += "&cursor=" + m_Cursor;
		path += "&errors=inline";
		m_RefusedToken = sessionToken;   // cleared on any answer but session_invalid
		if (!m_Transport.Post(REQUEST_PAGE, path, sessionToken, "{}", m_BudgetMs))
			Fail("the transport refused to send");
	}

	override void OnResponse(int kind, bool ok, int code, string data)
	{
		if (!ok)
		{
			// An opaque refusal (a hub or proxy without inline errors) or a
			// transport failure: nothing to branch on, so the walk waits a
			// retry out and starts over.
			m_RefusedToken = "";
			Fail(VyshkaTransport.DescribeError(code));
			return;
		}
		VyshkaJsonValue root = VyshkaJson.Parse(data);
		if (!root || !root.IsObject())
		{
			m_RefusedToken = "";
			Fail("a page that is not a JSON object");
			return;
		}
		VyshkaHubError refusal = VyshkaHubError.FromBody(root);
		if (refusal)
		{
			OnRefused(refusal);
			return;
		}
		m_RefusedToken = "";
		VyshkaJsonValue revision = root.Get("revision");
		VyshkaJsonValue bans = root.Get("bans");
		if (!revision || !revision.IsNumber() || !bans || !bans.IsArray())
		{
			Fail("a page without a revision and a bans array");
			return;
		}
		if (m_WalkRevision == "")
			m_WalkRevision = revision.m_Text;
		else if (revision.m_Text != m_WalkRevision)
		{
			// A walk reads one revision (section 13.3); a page at another is
			// a hub defect, and the list is read again from the start.
			Restart("a page came at revision " + revision.m_Text + " in a walk of " + m_WalkRevision);
			return;
		}
		for (int i = 0; i < bans.Count(); i++)
		{
			VyshkaInstallationBanEntry entry = VyshkaInstallationBanEntry.FromJson(bans.At(i));
			if (entry)
				m_Walked.Insert(entry);
		}
		m_Pages++;
		string next = root.GetString("nextCursor", "");
		if (next != "")
		{
			if (m_Pages >= PAGES_MAX)
			{
				Fail("the walk passed " + PAGES_MAX.ToString() + " pages without ending");
				return;
			}
			m_Cursor = next;
			return;
		}
		Finish();
	}

	void OnRefused(VyshkaHubError refusal)
	{
		string code = refusal.m_Code;
		if (code == "session_invalid" || (!refusal.IsMalformed() && refusal.IsUnauthorized()))
		{
			// The link starts a new session on its own poll; the walk goes on
			// under the new token, its cursor still good.
			m_NextTryMs = VyshkaClock.MonotonicMs() + SESSION_RETRY_MS;
			return;
		}
		m_RefusedToken = "";
		if (code == "conflict")
		{
			Restart("the hub cannot serve the walk's revision any more");
			return;
		}
		Fail("the hub refused a page, " + refusal.Describe());
	}

	void Restart(string why)
	{
		m_Restarts++;
		if (m_Restarts > RESTARTS_MAX)
		{
			m_Restarts = 0;
			Fail(why + ", and the walk has started over " + RESTARTS_MAX.ToString() + " times");
			return;
		}
		VyshkaLog.Info("installation ban list: " + why + "; reading it again from the first page");
		Begin();
	}

	void Fail(string why)
	{
		m_Walking = false;
		m_Walked.Clear();
		m_NextTryMs = VyshkaClock.MonotonicMs() + RETRY_MS;
		VyshkaLog.Warn("installation ban list could not be read: " + why + "; still enforcing revision " + VyshkaInstallationBans.Revision() + ", trying again in " + (RETRY_MS / 1000).ToString() + " s");
	}

	void Finish()
	{
		string revision = m_WalkRevision;
		string error;
		int count = m_Walked.Count();
		if (!VyshkaInstallationBans.Apply(revision, m_Walked, error))
		{
			Fail(error);
			return;
		}
		m_Walking = false;
		m_Restarts = 0;
		m_Walked = new array<ref VyshkaInstallationBanEntry>;
		// The first page was served at the hub's revision as it stood, which
		// is fresher than any report that came before the walk; one that came
		// during it is fresher still, and a different revision there is
		// another walk owed at once (the list moved on while this one ran).
		if (!m_ToldDuringWalk)
			m_Known = revision;
		m_Due = m_Known != revision;
		m_NextTryMs = 0;
		VyshkaLog.Info("installation ban list applied: revision " + revision + ", " + count.ToString() + " entr(ies) for this server's platform");
		VyshkaPlugin.BansApplied(revision);
	}
}
