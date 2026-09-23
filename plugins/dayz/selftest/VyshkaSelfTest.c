// Vyshka DayZ plugin: the engine-limit self-test.
//
// Appended to the init.c of a mission run with the Vyshka mod loaded and
// started from main() with VyshkaSelfTest.Run(): `vyshka-dayz selftest`
// derives the mission, boots a dedicated server on it, and grades the
// lines this prints. The plugin needs no config for it (it idles); only its
// classes are used, the same ones the plugin runs in production.
//
// What is graded here cannot be graded from outside the process: the
// engine's file reader faults the server on a line of 64 KiB or more, its
// Substring returns at most 8 191 characters, and its per-character string
// reads cost the length of the string (spikes/dayz-bans-pull-size, issue
// #108). Each check below writes or parses something past one of those
// limits through the plugin's own classes and says whether it came back
// whole and how long it took. The conformance harness grades the same
// plugin from the wire; this is the half it cannot see.
//
// Line format (tab separated, under the 255 characters Print keeps):
//   VYSHKA_SELFTEST<TAB>plan=<n>
//   VYSHKA_SELFTEST<TAB>check=<id><TAB>result=PASS|FAIL<TAB><detail>
//   VYSHKA_SELFTEST<TAB>finished<TAB>passed=<n><TAB>failed=<n><TAB>wallMs=<frame clock ms>

class VyshkaSelfTestAction : VyshkaAction
{
	string m_Code;

	void VyshkaSelfTestAction(string code)
	{
		m_Code = code;
	}

	override string Code()
	{
		return m_Code;
	}

	override string Name()
	{
		return "Self-test action " + m_Code;
	}

	override string Namespace()
	{
		return "selftest";
	}

	// A schema wide enough that a few hundred declarations make a manifest
	// past the reader's line limit as one string: eight text properties
	// with a description each.
	override VyshkaJsonValue ParamsSchema()
	{
		VyshkaJsonValue properties = VyshkaJsonValue.NewObject();
		for (int i = 0; i < 8; i++)
		{
			VyshkaJsonValue property = VyshkaJsonValue.NewObject();
			property.Set("type", VyshkaJsonValue.NewString("string"));
			property.Set("description", VyshkaJsonValue.NewString("A property of the self-test action, described at some length so the declaration is wide."));
			string name = "property" + i;
			properties.Set(name, property);
		}
		VyshkaJsonValue schema = VyshkaJsonValue.NewObject();
		schema.Set("type", VyshkaJsonValue.NewString("object"));
		schema.Set("properties", properties);
		return schema;
	}
}

class VyshkaSelfTest
{
	static ref VyshkaSelfTest s_Instance;

	static const string TAG = "VYSHKA_SELFTEST";
	static const int PLAN = 23;
	static const int SETTLE_MS = 3000;
	// The gap between checks: each runs in a frame of its own, so the
	// engine's frame clock advances between them and the finished line
	// can say how long the checks took by a clock the performance counter's
	// wrap cannot fool.
	static const int GAP_MS = 50;
	// The performance counter runs at 10 MHz (measured against the frame
	// clock in the spike).
	static const int TICKS_PER_MS = 10000;
	// Every timed phase has to finish inside this: generous against the
	// linear cost, far below the minutes the first parser took.
	static const int BUDGET_MS = 30000;

	static const int BAN_COUNT = 400;
	static const int MANIFEST_ACTIONS = 200;
	static const int BATCH_EVENTS = 200;
	static const int LONG_STRING = 100000;
	static const int SPEED_ENTRIES = 5000;

	static const string OVERSIZED_PATH = "$profile:Vyshka/selftest-oversized.json";
	static const string SPEED_PATH = "$profile:Vyshka/selftest-bans.json";
	static const string REPLACE_PATH = "$profile:Vyshka/selftest-replace.json";
	// A directory the replace check makes where a file would go, so the
	// file cannot be opened; left in place for the next run.
	static const string BLOCKED_PATH = "$profile:Vyshka/selftest-blocked.json";
	static const string BLOCKED_LOG_PATH = "$profile:Vyshka/selftest-blocked.log";
	// A file whose staging copy the replace check makes a directory, so the
	// staging copy cannot be opened; also left in place.
	static const string UNREADABLE_PATH = "$profile:Vyshka/selftest-unreadable.json";

	int m_Passed;
	int m_Failed;
	int m_Step;
	int m_PlanTime;   // the frame clock when the plan line went out

	static void Run()
	{
		if (s_Instance)
			return;
		s_Instance = new VyshkaSelfTest();
		s_Instance.Schedule();
	}

	// Schedule runs the checks once the mission has settled, off the init
	// script's own stack.
	void Schedule()
	{
		GetGame().GetCallQueue(CALL_CATEGORY_SYSTEM).CallLater(Start, SETTLE_MS, false);
	}

	void Start()
	{
		VyshkaFiles.EnsureLayout();
		m_Step = 0;
		m_PlanTime = GetGame().GetTime();
		Print(TAG + "\tplan=" + PLAN);
		Next();
	}

	// Next runs one check per frame, in a fixed order, and ends with the
	// finished line carrying the frame clock's reading of the whole.
	void Next()
	{
		int step = m_Step;
		m_Step++;
		if (step == 0)
			CheckBans();
		else if (step == 1)
			CheckManifest();
		else if (step == 2)
			CheckManifestLegacy();
		else if (step == 3)
			CheckOutbox();
		else if (step == 4)
			CheckLongValue();
		else if (step == 5)
			CheckRefused();
		else if (step == 6)
			CheckMarkerLiteral();
		else if (step == 7)
			CheckLegacyLiteral();
		else if (step == 8)
			CheckEscapedValue();
		else if (step == 9)
			CheckUtf8Pieces();
		else if (step == 10)
			CheckDepth();
		else if (step == 11)
			CheckDeepLongValue();
		else if (step == 12)
			CheckLongString();
		else if (step == 13)
			CheckEscapes();
		else if (step == 14)
			CheckSpeed();
		else if (step == 15)
			CheckExecutedKey();
		else if (step == 16)
			CheckInstallationBans();
		else if (step == 17)
			CheckReplace();
		else if (step == 18)
			CheckBansCrash();
		else if (step == 19)
			CheckManifestCrash();
		else if (step == 20)
			CheckCredentialsCrash();
		else if (step == 21)
			CheckExecutedCrash();
		else if (step == 22)
			CheckExecutedOrder();
		if (m_Step < PLAN)
		{
			GetGame().GetCallQueue(CALL_CATEGORY_SYSTEM).CallLater(Next, GAP_MS, false);
			return;
		}
		// The finished line goes out a frame later, so the frame clock has
		// moved past the last check as well.
		GetGame().GetCallQueue(CALL_CATEGORY_SYSTEM).CallLater(Finish, GAP_MS, false);
	}

	void Finish()
	{
		int wallMs = GetGame().GetTime() - m_PlanTime;
		string passed = m_Passed.ToString();
		string failed = m_Failed.ToString();
		string wall = wallMs.ToString();
		Print(TAG + "\tfinished\tpassed=" + passed + "\tfailed=" + failed + "\twallMs=" + wall);
	}

	// ---- ids.executedKey: what the executed-id log keeps for an actionId
	// past the line the reader can read: a fingerprint key that is the
	// same on every call, differs for ids that differ in one byte, and is
	// short; a shorter id is kept as it is ----
	void CheckExecutedKey()
	{
		string shortId = "01K5SELFTESTACTIONID000001";
		string longId = Repeat("0123456789abcdef", 600);   // 9 600 bytes
		string other = Repeat("0123456789abcdef", 599) + "0123456789abcdeX";
		string shortKey = VyshkaPlugin.ExecutedKey(shortId);
		string longKey = VyshkaPlugin.ExecutedKey(longId);
		string longKeyAgain = VyshkaPlugin.ExecutedKey(longId);
		string otherKey = VyshkaPlugin.ExecutedKey(other);
		bool same = shortKey == shortId;
		bool stable = longKey == longKeyAgain;
		bool distinct = longKey != otherKey;
		bool shortEnough = longKey.Length() < 32;
		// An id equal to a long id's key, and an id starting with the
		// marker byte, get keys of their own; a key read back from the log
		// is recognized as one, a bare id is not.
		string impostorKey = VyshkaPlugin.ExecutedKey(longKey);
		string markedId = VyshkaPlugin.KeyMarker() + "abc";
		string markedKey = VyshkaPlugin.ExecutedKey(markedId);
		bool unambiguous = impostorKey != longKey && markedKey != markedId && VyshkaPlugin.ExecutedKey(markedKey) != markedKey;
		bool recognized = VyshkaPlugin.IsExecutedKey(longKey) && VyshkaPlugin.IsExecutedKey(markedKey) && !VyshkaPlugin.IsExecutedKey(shortId) && !VyshkaPlugin.IsExecutedKey(longId);
		string fingerprint = VyshkaIds.Fingerprint("");
		bool ok = same && stable && distinct && shortEnough && unambiguous && recognized && fingerprint == "811c9dc5";
		string detail = "same=" + same;
		detail += "\tstable=" + stable;
		detail += "\tdistinct=" + distinct;
		detail += "\tunambiguous=" + unambiguous;
		detail += "\trecognized=" + recognized;
		detail += "\tkeyLength=" + longKey.Length();
		detail += "\temptyFingerprint=" + fingerprint;
		Report("ids.executedKey", ok, detail);
	}

	// ---- files.markerLiteral: a genuine document that looks like the file
	// writer's long-string marker, or carries keys in its reserved prefix,
	// reads back as itself ----
	void CheckMarkerLiteral()
	{
		if (FileExist(OVERSIZED_PATH))
			DeleteFile(OVERSIZED_PATH);
		VyshkaJsonValue marker = VyshkaJsonValue.NewObject();
		VyshkaJsonValue pieces = VyshkaJsonValue.NewArray();
		pieces.Add(VyshkaJsonValue.NewString("a"));
		pieces.Add(VyshkaJsonValue.NewString("b"));
		marker.Set(VyshkaJsonWriter.LONG_STRING_KEY, pieces);
		VyshkaJsonValue document = VyshkaJsonValue.NewObject();
		document.Set("result", marker);
		document.Set("$vyshka.other", VyshkaJsonValue.NewInt(1));
		document.Set("$$vyshka.longString", VyshkaJsonValue.NewInt(2));
		document.Set("plain", VyshkaJsonValue.NewString("x"));
		// Two reserved-prefix keys past what one Substring returns, alike
		// but for their last character.
		string longKey = "$vyshka." + Repeat("a", 8183);
		document.Set(longKey + "x", VyshkaJsonValue.NewInt(3));
		document.Set(longKey + "y", VyshkaJsonValue.NewInt(4));
		string compact = document.Serialize();
		bool written = VyshkaFiles.WriteJson(OVERSIZED_PATH, document);
		VyshkaJsonValue back = VyshkaFiles.ReadJson(OVERSIZED_PATH);
		bool equal = back && back.Serialize() == compact;
		bool shape = false;
		if (back && back.IsObject())
		{
			VyshkaJsonValue result = back.Get("result");
			shape = result && result.IsObject() && result.Count() == 1 && result.KeyAt(0) == VyshkaJsonWriter.LONG_STRING_KEY && back.GetInt("$vyshka.other", 0) == 1 && back.GetInt("$$vyshka.longString", 0) == 2;
		}
		bool ok = written && equal && shape;
		string detail = "written=" + written;
		detail += "\tequal=" + equal;
		detail += "\tshape=" + shape;
		Report("files.markerLiteral", ok, detail);
		if (FileExist(OVERSIZED_PATH))
			DeleteFile(OVERSIZED_PATH);
	}

	// ---- files.legacyLiteral: a one-line file as plugin 0.8.0 wrote them,
	// carrying keys and an object that look like the writer's marks, reads
	// back as it is ----
	void CheckLegacyLiteral()
	{
		if (FileExist(OVERSIZED_PATH))
			DeleteFile(OVERSIZED_PATH);
		VyshkaJsonValue marker = VyshkaJsonValue.NewObject();
		VyshkaJsonValue pieces = VyshkaJsonValue.NewArray();
		pieces.Add(VyshkaJsonValue.NewString("a"));
		pieces.Add(VyshkaJsonValue.NewString("b"));
		marker.Set(VyshkaJsonWriter.LONG_STRING_KEY, pieces);
		VyshkaJsonValue result = VyshkaJsonValue.NewObject();
		result.Set("$$vyshka.x", VyshkaJsonValue.NewInt(7));
		result.Set("$vyshka.x", VyshkaJsonValue.NewInt(8));
		result.Set("shape", marker);
		VyshkaJsonValue document = VyshkaJsonValue.NewObject();
		document.Set("result", result);
		string compact = document.Serialize();
		bool written = VyshkaFiles.WriteAll(OVERSIZED_PATH, compact);
		VyshkaJsonValue back = VyshkaFiles.ReadJson(OVERSIZED_PATH);
		bool equal = back && back.Serialize() == compact;
		bool ok = written && equal;
		string detail = "written=" + written;
		detail += "\tequal=" + equal;
		Report("files.legacyLiteral", ok, detail);
		if (FileExist(OVERSIZED_PATH))
			DeleteFile(OVERSIZED_PATH);
	}

	// ---- files.escapedValue: a value short in characters but long once
	// escaped (10 001 control characters, six bytes each quoted) is written
	// in pieces and read back equal ----
	void CheckEscapedValue()
	{
		if (FileExist(OVERSIZED_PATH))
			DeleteFile(OVERSIZED_PATH);
		int code = 1;
		string one = code.AsciiToString();
		string value = Repeat(one, 10001);
		// And a member whose key and value each fit a line but together
		// would not: an 11 000-character key with a value of 8 191 control
		// characters, 60 153 bytes compact.
		string longKey = Repeat("k", 11000);
		string shortValue = Repeat(one, 8191);
		VyshkaJsonValue document = VyshkaJsonValue.NewObject();
		document.Set("v", VyshkaJsonValue.NewString(value));
		document.Set(longKey, VyshkaJsonValue.NewString(shortValue));
		bool written = VyshkaFiles.WriteJson(OVERSIZED_PATH, document);
		VyshkaJsonValue back = VyshkaFiles.ReadJson(OVERSIZED_PATH);
		bool equal = back && back.IsObject() && back.GetString("v", "") == value && back.GetString(longKey, "") == shortValue;
		bool ok = written && equal;
		string detail = "length=" + value.Length();
		detail += "\twritten=" + written;
		detail += "\tequal=" + equal;
		Report("files.escapedValue", ok, detail);
		if (FileExist(OVERSIZED_PATH))
			DeleteFile(OVERSIZED_PATH);
	}

	// ---- files.utf8Pieces: a long value of two-byte characters is cut into
	// pieces on character boundaries, and reads back equal ----
	void CheckUtf8Pieces()
	{
		if (FileExist(OVERSIZED_PATH))
			DeleteFile(OVERSIZED_PATH);
		string value = "a" + Repeat(VyshkaJson.EncodeUtf8(233), 8192);
		VyshkaJsonValue document = VyshkaJsonValue.NewObject();
		document.Set("v", VyshkaJsonValue.NewString(value));
		bool written = VyshkaFiles.WriteJson(OVERSIZED_PATH, document);
		// Every piece line ends with a quote (and maybe a comma); the byte
		// before that quote must not be the lead byte of a sequence whose
		// rest went to the next piece.
		bool whole = true;
		int pieceLines = 0;
		array<string> lines;
		if (VyshkaFiles.ReadSegments(OVERSIZED_PATH, lines))
		{
			for (int i = 0; i < lines.Count(); i++)
			{
				string line = lines.Get(i);
				int length = line.Length();
				int quoteAt = length - 2;
				if (quoteAt >= 1 && line.Get(quoteAt) == ",")
					quoteAt--;
				if (quoteAt < 1 || line.Get(quoteAt) != "\"" || line.Get(0) != "\t")
					continue;
				string last = line.Get(quoteAt - 1);
				int lastCode = last.ToAscii();
				if (lastCode < 0)
					lastCode += 256;
				if (lastCode >= 192)
					whole = false;
				pieceLines++;
			}
		}
		VyshkaJsonValue back = VyshkaFiles.ReadJson(OVERSIZED_PATH);
		bool equal = back && back.IsObject() && back.GetString("v", "") == value;
		bool ok = written && equal && whole && pieceLines >= 4;
		string detail = "bytes=" + value.Length();
		detail += "\twritten=" + written;
		detail += "\tequal=" + equal;
		detail += "\twhole=" + whole;
		detail += "\tpieceLines=" + pieceLines;
		Report("files.utf8Pieces", ok, detail);
		if (FileExist(OVERSIZED_PATH))
			DeleteFile(OVERSIZED_PATH);
	}

	// ---- json.depth: how deep the parser reads on this engine, measured
	// against nested arrays, from the wire and from a file; the deepest
	// that parses must reach the documented bounds ----
	void CheckDepth()
	{
		if (FileExist(OVERSIZED_PATH))
			DeleteFile(OVERSIZED_PATH);
		int wireMax = 0;
		int fileMax = 0;
		int wireFirstFailed = 0;
		int fileFirstFailed = 0;
		for (int depth = 8; depth <= 96; depth += 8)
		{
			string text = Repeat("[", depth) + Repeat("]", depth);
			VyshkaJsonValue wire = VyshkaJson.Parse(text);
			if (wire && wire.Depth() == depth)
				wireMax = depth;
			else if (wireFirstFailed == 0)
				wireFirstFailed = depth;
			array<string> segments = new array<string>;
			segments.Insert(text + "\n");
			VyshkaJsonValue file = VyshkaJson.ParseSegments(segments);
			if (file && file.Depth() == depth)
				fileMax = depth;
			else if (fileFirstFailed == 0)
				fileFirstFailed = depth;
		}
		bool ok = wireMax >= VyshkaJson.MAX_DEPTH && fileMax >= VyshkaJson.FILE_MAX_DEPTH;
		string detail = "wireMax=" + wireMax;
		detail += "\twireFirstFailed=" + wireFirstFailed;
		detail += "\tfileMax=" + fileMax;
		detail += "\tfileFirstFailed=" + fileFirstFailed;
		detail += "\tmaxDepth=" + VyshkaJson.MAX_DEPTH;
		detail += "\tfileMaxDepth=" + VyshkaJson.FILE_MAX_DEPTH;
		Report("json.depth", ok, detail);
	}

	// ---- files.deepLongValue: a record as deep as the wire parser allows,
	// with a long string at the bottom (which the pieces nest two deeper),
	// is written and read back equal ----
	void CheckDeepLongValue()
	{
		if (FileExist(OVERSIZED_PATH))
			DeleteFile(OVERSIZED_PATH);
		VyshkaJsonValue body = VyshkaJsonValue.NewArray();
		VyshkaJsonValue deep = body;
		for (int i = 2; i < VyshkaJson.MAX_DEPTH; i++)
		{
			VyshkaJsonValue inner = VyshkaJsonValue.NewArray();
			deep.Add(inner);
			deep = inner;
		}
		deep.Add(VyshkaJsonValue.NewString(Repeat("0123456789", 900)));
		VyshkaJsonValue record = VyshkaJsonValue.NewObject();
		record.Set("body", body);
		int depth = record.Depth();
		string compact = record.Serialize();
		// The wire parser reads the compact form back too: what a hub
		// would send at the depth bound.
		VyshkaJsonValue wire = VyshkaJson.Parse(compact);
		bool wireEqual = wire && wire.Serialize() == compact;
		bool written = VyshkaFiles.WriteJson(OVERSIZED_PATH, record);
		VyshkaJsonValue back = VyshkaFiles.ReadJson(OVERSIZED_PATH);
		int backDepth = -1;
		if (back)
			backDepth = back.Depth();
		bool equal = back && back.Serialize() == compact;
		bool ok = written && equal && wireEqual && depth == VyshkaJson.MAX_DEPTH;
		string detail = "depth=" + depth;
		detail += "\twireEqual=" + wireEqual;
		detail += "\twritten=" + written;
		detail += "\tbackDepth=" + backDepth;
		detail += "\tequal=" + equal;
		Report("files.deepLongValue", ok, detail);
		if (FileExist(OVERSIZED_PATH))
			DeleteFile(OVERSIZED_PATH);
	}

	void Report(string check, bool ok, string detail)
	{
		string result = "FAIL";
		if (ok)
		{
			result = "PASS";
			m_Passed++;
		}
		else
			m_Failed++;
		Print(TAG + "\tcheck=" + check + "\tresult=" + result + "\t" + detail);
	}

	// Millis converts a counter delta; a wrapped or negative reading is
	// reported as-is and fails any budget.
	static int Millis(int ticks)
	{
		return ticks / TICKS_PER_MS;
	}

	static bool Within(int ms)
	{
		return ms >= 0 && ms <= BUDGET_MS;
	}

	// Repeat builds a string of n copies of unit by doubling, so the cost
	// is a few appends rather than n of them.
	static string Repeat(string unit, int n)
	{
		string result = "";
		string block = unit;
		int have = 1;
		while (n > 0)
		{
			if (n & 1)
				result += block;
			n = n >> 1;
			if (n > 0)
				block += block;
		}
		return result;
	}

	// SyntheticId is a 17-digit Steam64 for entry i.
	static string SyntheticId(int i)
	{
		string digits = i.ToString();
		while (digits.Length() < 9)
			digits = "0" + digits;
		return "76561198" + digits;
	}

	// ---- files.bans: a ban list past the reader's old limit, saved one
	// entry per line and read back whole through VyshkaBans itself ----
	void CheckBans()
	{
		VyshkaBans.Reset();
		VyshkaBans.Load();
		VyshkaBans.Entries().Clear();
		string reason = Repeat("banned by the self-test for being a synthetic entry; ", 3);
		int now = VyshkaClock.EpochSeconds();
		for (int i = 0; i < BAN_COUNT; i++)
		{
			VyshkaBanEntry entry = new VyshkaBanEntry();
			entry.m_Id = SyntheticId(i);
			entry.m_Name = "Survivor " + i;
			entry.m_Reason = reason;
			entry.m_BannedAt = "2026-09-21T10:00:00Z";
			entry.m_Permanent = (i % 2 == 0);
			entry.m_ExpiresEpoch = now + 86400;
			entry.m_ActionId = "selftest-" + i;
			VyshkaBans.Entries().Set(entry.m_Id, entry);
		}
		int t0 = TickCount(0);
		bool saved = VyshkaBans.Save();
		int saveMs = Millis(TickCount(t0));
		VyshkaBans.Reset();
		int t1 = TickCount(0);
		VyshkaBans.Load();
		int loadMs = Millis(TickCount(t1));
		int count = VyshkaBans.Entries().Count();
		VyshkaBanEntry back = VyshkaBans.Find(SyntheticId(123));
		bool intact = back && back.m_Reason == reason && !back.m_Permanent && back.m_ActionId == "selftest-123";
		bool ok = saved && count == BAN_COUNT && intact && Within(saveMs) && Within(loadMs);
		string detail = "entries=" + count;
		detail += "\tsaved=" + saved;
		detail += "\tintact=" + intact;
		detail += "\tsaveMs=" + saveMs;
		detail += "\tloadMs=" + loadMs;
		Report("files.bans", ok, detail);
		VyshkaBans.Entries().Clear();
		VyshkaBans.Save();
		VyshkaBans.Reset();
	}

	// ---- files.installationBans: the installation ban list written through
	// its staging copy and read back whole, and both crash windows of that
	// write: a staging copy cut short beside a whole list (discarded, the
	// list kept) and a whole staging copy beside a main file cut short (the
	// staging copy wins and the main file is written again) ----
	void CheckInstallationBans()
	{
		string mainPath = VyshkaFiles.INSTALLATION_BANS_PATH;
		string nextPath = VyshkaFiles.StagingPath(mainPath);
		string reason = Repeat("installation-banned by the self-test; ", 5);
		array<ref VyshkaInstallationBanEntry> first = InstallationEntries(BAN_COUNT - 100, reason);
		string error;
		bool applied = VyshkaInstallationBans.Apply("1790000000001", first, error);
		bool staged = FileExist(nextPath);

		// The staging copy cut short beside a whole list.
		// Built from a quote alone: a literal combining escapes can fail to
		// parse on this engine (dayz-kb 09).
		string q = "\"";
		VyshkaFiles.WriteAll(nextPath, "{" + q + "revision" + q + ": 1790000000002, " + q + "bans" + q + ": [ { " + q + "id" + q + ": " + q + "cut");
		VyshkaInstallationBans.Reset();
		string keptRevision = VyshkaInstallationBans.Revision();
		int kept = VyshkaInstallationBans.Count();
		bool discarded = !FileExist(nextPath);

		// A whole staging copy beside a main file cut short.
		array<ref VyshkaInstallationBanEntry> second = InstallationEntries(BAN_COUNT - 200, reason);
		map<string, ref VyshkaInstallationBanEntry> secondMap = new map<string, ref VyshkaInstallationBanEntry>;
		for (int i = 0; i < second.Count(); i++)
		{
			VyshkaInstallationBanEntry one = second.Get(i);
			secondMap.Set(one.m_Id, one);
		}
		bool wroteNext = VyshkaFiles.WriteJson(nextPath, VyshkaInstallationBans.Encode("1790000000003", secondMap));
		VyshkaFiles.WriteAll(mainPath, "{" + q + "revision" + q + ": 17900");
		VyshkaInstallationBans.Reset();
		string finishedRevision = VyshkaInstallationBans.Revision();
		int finished = VyshkaInstallationBans.Count();
		bool finishedClean = !FileExist(nextPath);
		VyshkaInstallationBans.Reset();
		string rereadRevision = VyshkaInstallationBans.Revision();
		VyshkaInstallationBanEntry back = VyshkaInstallationBans.Find(SyntheticId(42));
		bool intact = back && back.m_Reason == reason && back.m_BanId == "selftest-installation-42";

		bool ok = applied && !staged && keptRevision == "1790000000001" && kept == first.Count() && discarded;
		ok = ok && wroteNext && finishedRevision == "1790000000003" && finished == second.Count() && finishedClean;
		ok = ok && rereadRevision == "1790000000003" && intact;
		string detail = "applied=" + applied;
		detail += "\tstagingLeft=" + staged;
		detail += "\tkept=" + keptRevision + "/" + kept;
		detail += "\tdiscarded=" + discarded;
		detail += "\tfinished=" + finishedRevision + "/" + finished;
		detail += "\tfinishedClean=" + finishedClean;
		detail += "\treread=" + rereadRevision;
		detail += "\tintact=" + intact;
		Report("files.installationBans", ok, detail);
		VyshkaFiles.DeleteReplaced(mainPath);
		VyshkaInstallationBans.Reset();
	}

	// ---- files.replace: VyshkaFiles.ReplaceJson and its recovery on a
	// scratch file: a replace leaves no staging copy, a staging copy cut
	// short is discarded, a whole one beside a main file cut short wins and
	// is written over it, and a main file that cannot be opened (a directory
	// in its place) fails the replace with no staging copy left, while a
	// whole staging copy beside it is read and kept ----
	void CheckReplace()
	{
		string path = REPLACE_PATH;
		string staging = VyshkaFiles.StagingPath(path);
		VyshkaFiles.DeleteReplaced(path);
		string name = VyshkaFiles.StagingPath("$profile:Vyshka/a.b/c.json") + "|" + VyshkaFiles.StagingPath("$profile:Vyshka/a.b/c");
		bool names = name == "$profile:Vyshka/a.b/c.next.json|$profile:Vyshka/a.b/c.next";

		bool replaced = VyshkaFiles.ReplaceJson(path, Doc("a"));
		bool clean = !FileExist(staging) && DocIs(VyshkaFiles.ReadReplacedJson(path), "a");
		bool scalarRefused = !VyshkaFiles.ReplaceJson(path, VyshkaJsonValue.NewInt(1)) && DocIs(VyshkaFiles.ReadJson(path), "a");

		PlantCut(staging);
		bool kept = DocIs(VyshkaFiles.ReadReplacedJson(path), "a") && !FileExist(staging);

		VyshkaFiles.WriteJson(staging, Doc("b"));
		PlantCut(path);
		bool won = DocIs(VyshkaFiles.ReadReplacedJson(path), "b") && !FileExist(staging) && DocIs(VyshkaFiles.ReadJson(path), "b");

		// A directory where the main file goes: OpenFile cannot open it.
		string blocked = BLOCKED_PATH;
		string blockedStaging = VyshkaFiles.StagingPath(blocked);
		if (!FileExist(blocked))
			MakeDirectory(blocked);
		bool isBlocked = FileExist(blocked);
		bool blockedFails = !VyshkaFiles.ReplaceJson(blocked, Doc("c")) && !FileExist(blockedStaging);
		VyshkaFiles.WriteJson(blockedStaging, Doc("d"));
		bool stagedRead = DocIs(VyshkaFiles.ReadReplacedJson(blocked), "d") && FileExist(blockedStaging);
		bool stagedKept = !VyshkaFiles.ReplaceJson(blocked, Doc("e")) && DocIs(VyshkaFiles.ReadJson(blockedStaging), "d");

		// A staging copy that cannot be opened (a directory in its place)
		// may be the only whole copy: it is not taken for one cut short,
		// the file is read, and no replace goes ahead.
		string unreadable = UNREADABLE_PATH;
		string unreadableStaging = VyshkaFiles.StagingPath(unreadable);
		if (!FileExist(unreadableStaging))
			MakeDirectory(unreadableStaging);
		VyshkaFiles.WriteJson(unreadable, Doc("f"));
		bool unreadableKept = DocIs(VyshkaFiles.ReadReplacedJson(unreadable), "f") && FileExist(unreadableStaging) && !VyshkaFiles.ReplaceJson(unreadable, Doc("g")) && DocIs(VyshkaFiles.ReadJson(unreadable), "f");
		DeleteFile(unreadable);

		bool ok = names && replaced && clean && scalarRefused && kept && won && isBlocked && blockedFails && stagedRead && stagedKept && unreadableKept;
		string detail = "names=" + names;
		detail += "\treplaced=" + replaced;
		detail += "\tclean=" + clean;
		detail += "\tscalarRefused=" + scalarRefused;
		detail += "\tcutDiscarded=" + kept;
		detail += "\twholeWon=" + won;
		detail += "\tblocked=" + isBlocked;
		detail += "\tblockedFails=" + blockedFails;
		detail += "\tstagedRead=" + stagedRead;
		detail += "\tstagedKept=" + stagedKept;
		detail += "\tunreadableKept=" + unreadableKept;
		Report("files.replace", ok, detail);
		VyshkaFiles.DeleteReplaced(path);
		if (FileExist(blockedStaging))
			DeleteFile(blockedStaging);
	}

	// ---- files.bansCrash: the local ban list through VyshkaBans itself: a
	// save leaves no staging copy, a staging copy cut short is discarded
	// and the list kept, and a whole one beside a main file cut short is
	// the list loaded ----
	void CheckBansCrash()
	{
		string path = VyshkaFiles.BANS_PATH;
		string staging = VyshkaFiles.StagingPath(path);
		VyshkaFiles.DeleteReplaced(path);
		VyshkaBans.Reset();
		VyshkaBans.Load();
		for (int i = 0; i < 30; i++)
			VyshkaBans.Entries().Set(SyntheticId(i), BanEntry(i));
		bool saved = VyshkaBans.Save() && !FileExist(staging);

		PlantCut(staging);
		VyshkaBans.Reset();
		int kept = VyshkaBans.Count();
		bool discarded = !FileExist(staging);

		VyshkaJsonValue list = VyshkaJsonValue.NewArray();
		for (int j = 100; j < 120; j++)
			list.Add(BanEntry(j).ToJson());
		VyshkaJsonValue root = VyshkaJsonValue.NewObject();
		root.Set("bans", list);
		VyshkaFiles.WriteJson(staging, root);
		PlantCut(path);
		VyshkaBans.Reset();
		int finished = VyshkaBans.Count();
		string error;
		bool writable = VyshkaBans.Writable(error);
		bool found = VyshkaBans.Find(SyntheticId(110)) != null;
		bool finishedClean = !FileExist(staging);
		VyshkaBans.Reset();
		int reread = VyshkaBans.Count();

		bool ok = saved && kept == 30 && discarded && finished == 20 && writable && found && finishedClean && reread == 20;
		string detail = "saved=" + saved;
		detail += "\tkept=" + kept;
		detail += "\tdiscarded=" + discarded;
		detail += "\tfinished=" + finished;
		detail += "\twritable=" + writable;
		detail += "\tfound=" + found;
		detail += "\tfinishedClean=" + finishedClean;
		detail += "\treread=" + reread;
		Report("files.bansCrash", ok, detail);
		VyshkaFiles.DeleteReplaced(path);
		VyshkaBans.Reset();
	}

	// ---- files.manifestCrash: the manifest record through
	// VyshkaManifestRecord, both crash windows as files.bansCrash ----
	void CheckManifestCrash()
	{
		string path = VyshkaFiles.MANIFEST_PATH;
		string staging = VyshkaFiles.StagingPath(path);
		VyshkaFiles.DeleteReplaced(path);
		VyshkaJsonValue content = VyshkaJsonValue.NewObject();
		content.Set("game", VyshkaJsonValue.NewString("dayz"));
		bool saved = VyshkaManifestRecord.Save(100, content, false, -2) && !FileExist(staging);

		PlantCut(staging);
		string problem;
		VyshkaManifestRecord kept = VyshkaManifestRecord.Load(problem);
		int keptRevision = -1;
		if (kept)
			keptRevision = kept.m_Revision;
		bool discarded = !FileExist(staging);

		VyshkaJsonValue record = VyshkaJsonValue.NewObject();
		record.Set("revision", VyshkaJsonValue.NewInt(200));
		record.Set("pending", VyshkaJsonValue.NewBool(true));
		record.Set("above", VyshkaJsonValue.NewInt(9));
		record.Set("content", content);
		VyshkaFiles.WriteJson(staging, record);
		PlantCut(path);
		VyshkaManifestRecord finished = VyshkaManifestRecord.Load(problem);
		bool marks = finished && finished.m_Revision == 200 && finished.m_Pending && finished.m_Above == 9 && finished.m_Content == content.Serialize();
		bool finishedClean = !FileExist(staging);
		VyshkaManifestRecord reread = VyshkaManifestRecord.Load(problem);
		bool rereadOk = reread && reread.m_Revision == 200;

		bool ok = saved && keptRevision == 100 && discarded && marks && finishedClean && rereadOk;
		string detail = "saved=" + saved;
		detail += "\tkept=" + keptRevision;
		detail += "\tdiscarded=" + discarded;
		detail += "\tmarks=" + marks;
		detail += "\tfinishedClean=" + finishedClean;
		detail += "\treread=" + rereadOk;
		if (!reread)
			detail += "\tproblem=" + problem;
		Report("files.manifestCrash", ok, detail);
		VyshkaFiles.DeleteReplaced(path);
	}

	// ---- files.credentialsCrash: the credentials through
	// VyshkaCredentials, both crash windows as files.bansCrash, and a
	// delete that takes a whole staging copy with it, so re-enrollment
	// cannot be undone by an interrupted write ----
	void CheckCredentialsCrash()
	{
		string path = VyshkaFiles.CREDENTIALS_PATH;
		string staging = VyshkaFiles.StagingPath(path);
		VyshkaFiles.DeleteReplaced(path);
		VyshkaCredentials first = new VyshkaCredentials();
		first.m_ServerId = "selftest-server-a";
		first.m_ServerSecret = "selftest-secret-a";
		first.m_EnrolledWithToken = "selftest-token-a";
		bool saved = first.Save() && !FileExist(staging);

		PlantCut(staging);
		VyshkaCredentials kept = VyshkaCredentials.Load();
		bool keptOk = kept && kept.m_ServerSecret == "selftest-secret-a";
		bool discarded = !FileExist(staging);

		VyshkaJsonValue second = VyshkaJsonValue.NewObject();
		second.Set("serverId", VyshkaJsonValue.NewString("selftest-server-b"));
		second.Set("serverSecret", VyshkaJsonValue.NewString("selftest-secret-b"));
		second.Set("enrolledWithToken", VyshkaJsonValue.NewString("selftest-token-b"));
		VyshkaFiles.WriteJson(staging, second);
		PlantCut(path);
		VyshkaCredentials finished = VyshkaCredentials.Load();
		bool finishedOk = finished && finished.m_ServerId == "selftest-server-b" && finished.m_ServerSecret == "selftest-secret-b";
		bool finishedClean = !FileExist(staging);

		VyshkaFiles.WriteJson(staging, second);
		VyshkaCredentials.Delete();
		bool deleted = !FileExist(path) && !FileExist(staging) && VyshkaCredentials.Load() == null;

		bool ok = saved && keptOk && discarded && finishedOk && finishedClean && deleted;
		string detail = "saved=" + saved;
		detail += "\tkept=" + keptOk;
		detail += "\tdiscarded=" + discarded;
		detail += "\tfinished=" + finishedOk;
		detail += "\tfinishedClean=" + finishedClean;
		detail += "\tdeleted=" + deleted;
		Report("files.credentialsCrash", ok, detail);
		VyshkaFiles.DeleteReplaced(path);
	}

	// ---- files.executedCrash: the executed-action log through
	// VyshkaPlugin.LoadExecuted: an oversized log is compacted to the LRU's
	// capacity with no staging copy left, a staging copy cut short (no end
	// line) is discarded and the log kept, a whole one beside a log cut
	// short is joined with what the log holds and written back, and a whole
	// one a compaction could not delete does not cost a record appended to
	// the log since ----
	void CheckExecutedCrash()
	{
		string path = VyshkaFiles.EXECUTED_PATH;
		string staging = VyshkaFiles.StagingPath(path);
		VyshkaFiles.DeleteReplaced(path);
		int capacity = VyshkaPlugin.EXECUTED_LRU_CAPACITY;
		int total = 2 * capacity + 10;
		VyshkaFiles.WriteAll(path, ExecutedLines("selftest-old-", 0, total, false));
		VyshkaPlugin compacting = new VyshkaPlugin();
		compacting.LoadExecuted();
		int compactedLines = VyshkaFiles.ReadLines(path).Count();
		int last = total - 1;
		string newest = "selftest-old-" + last;
		bool compacted = compactedLines == capacity && !FileExist(staging) && compacting.m_Executed.Contains(newest);

		// A staging copy cut short: its records but no end line.
		VyshkaFiles.WriteAll(staging, ExecutedLines("selftest-cut-", 0, 20, false));
		VyshkaPlugin keeping = new VyshkaPlugin();
		keeping.LoadExecuted();
		bool kept = keeping.m_ExecutedOrder.Count() == capacity && keeping.m_Executed.Contains(newest) && !keeping.m_Executed.Contains("selftest-cut-3");
		bool discarded = !FileExist(staging);

		// A whole staging copy beside a log cut short mid-record: the staged
		// records and the whole ones the log still holds are the history,
		// and the partial last record adds a key no id can match.
		VyshkaFiles.WriteAll(staging, ExecutedLines("selftest-new-", 0, 40, true));
		string q = "\"";
		VyshkaFiles.WriteAll(path, q + "selftest-old-10" + q + "\n" + q + "selftest-o");
		VyshkaPlugin finishing = new VyshkaPlugin();
		finishing.LoadExecuted();
		bool finished = finishing.m_ExecutedOrder.Count() == 41 && finishing.m_Executed.Contains("selftest-new-39") && finishing.m_Executed.Contains("selftest-old-10");
		bool finishedClean = !FileExist(staging) && VyshkaFiles.ReadLines(path).Count() == 42;

		// A whole staging copy a compaction could not delete, beside a log
		// that has had a record appended since: the appended record is kept.
		VyshkaFiles.WriteAll(staging, ExecutedLines("selftest-new-", 0, 40, true));
		VyshkaFiles.WriteAll(path, ExecutedLines("selftest-new-", 0, 40, false) + VyshkaJson.Quote("selftest-late-1") + "\n");
		VyshkaPlugin lingering = new VyshkaPlugin();
		lingering.LoadExecuted();
		bool appendedKept = lingering.m_Executed.Contains("selftest-late-1") && lingering.m_Executed.Contains("selftest-new-0") && !FileExist(staging);

		// A compaction to a full LRU cut short halfway through a record: the
		// partial record must not push a whole one out of the capacity.
		int half = capacity / 2;
		VyshkaFiles.WriteAll(staging, ExecutedLines("selftest-full-", 0, capacity, true));
		VyshkaFiles.WriteAll(path, ExecutedLines("selftest-full-", 0, half, false) + q + "selftest-fu");
		VyshkaPlugin full = new VyshkaPlugin();
		full.LoadExecuted();
		int lastFull = capacity - 1;
		bool capacityKept = full.m_Executed.Contains("selftest-full-0") && full.m_Executed.Contains("selftest-full-" + half) && full.m_Executed.Contains("selftest-full-" + lastFull) && !FileExist(staging);
		// The next id recorded evicts the oldest whole record alone.
		full.MarkExecuted("selftest-full-next");
		capacityKept = capacityKept && full.m_ExecutedOrder.Count() == capacity && !full.m_Executed.Contains("selftest-full-0") && full.m_Executed.Contains("selftest-full-1");

		// A log whose last line was left without its newline: the next
		// append starts a line of its own rather than running into it.
		VyshkaFiles.DeleteReplaced(path);
		VyshkaFiles.WriteAll(path, VyshkaJson.Quote("selftest-a-1"));
		VyshkaPlugin appending = new VyshkaPlugin();
		appending.MarkExecuted("selftest-a-2");
		VyshkaPlugin rebooted = new VyshkaPlugin();
		rebooted.LoadExecuted();
		bool appendSeparated = rebooted.m_Executed.Contains("selftest-a-1") && rebooted.m_Executed.Contains("selftest-a-2");

		// A log that cannot be opened (a directory in its place) beside a
		// whole staging copy: the staging copy is read and kept, and no
		// replace goes ahead.
		string blocked = BLOCKED_LOG_PATH;
		string blockedStaging = VyshkaFiles.StagingPath(blocked);
		if (!FileExist(blocked))
			MakeDirectory(blocked);
		VyshkaFiles.WriteAll(blockedStaging, ExecutedLines("selftest-blocked-", 0, 5, true));
		array<string> blockedRead = VyshkaFiles.ReadReplacedLines(blocked);
		array<string> none = new array<string>;
		bool blockedKept = blockedRead.Count() == 5 && FileExist(blockedStaging) && !VyshkaFiles.ReplaceLines(blocked, none) && VyshkaFiles.ReadLines(blockedStaging).Count() == 6;
		DeleteFile(blockedStaging);

		bool ok = compacted && kept && discarded && finished && finishedClean && appendedKept && capacityKept && appendSeparated && blockedKept;
		string detail = "compacted=" + compactedLines;
		detail += "\tkept=" + kept;
		detail += "\tdiscarded=" + discarded;
		detail += "\tfinished=" + finished;
		detail += "\tfinishedClean=" + finishedClean;
		detail += "\tappendedKept=" + appendedKept;
		detail += "\tcapacityKept=" + capacityKept;
		detail += "\tappendSeparated=" + appendSeparated;
		detail += "\tblockedKept=" + blockedKept;
		Report("files.executedCrash", ok, detail);
		VyshkaFiles.DeleteReplaced(path);
	}

	// ---- files.executedOrder: recency through a recovery of the executed
	// log. A compaction that truncated the log, after which the log took
	// new records (a boot whose recovery could not finish), keeps the new
	// records newest, in memory and on disk, including when a crash stops
	// the next recovery before its staging copy is deleted; a bare id of
	// the earlier raw-line format survives a compaction; a line torn from
	// a record cannot keep a whole record of the same text out of the LRU ----
	void CheckExecutedOrder()
	{
		string path = VyshkaFiles.EXECUTED_PATH;
		string staging = VyshkaFiles.StagingPath(path);
		VyshkaFiles.DeleteReplaced(path);
		int capacity = VyshkaPlugin.EXECUTED_LRU_CAPACITY;

		VyshkaFiles.WriteAll(staging, ExecutedLines("selftest-h-", 0, capacity, true));
		VyshkaFiles.WriteAll(path, ExecutedLines("selftest-x-", 0, 5, false));
		VyshkaPlugin recent = new VyshkaPlugin();
		recent.LoadExecuted();
		bool recencyKept = NewestIs(recent, "selftest-x-4", capacity) && recent.m_Executed.Contains("selftest-x-0") && !FileExist(staging);
		array<string> onDisk = VyshkaFiles.ReadReplacedLines(path);
		recencyKept = recencyKept && onDisk.Count() == capacity + 5 && onDisk.Get(onDisk.Count() - 1) == VyshkaJson.Quote("selftest-x-4");

		// The same log after a crash between the restore and the deletion
		// of the staging copy: the next boot finds every record present.
		VyshkaFiles.WriteAll(staging, ExecutedLines("selftest-h-", 0, capacity, true));
		VyshkaPlugin again = new VyshkaPlugin();
		again.LoadExecuted();
		bool recencyDurable = NewestIs(again, "selftest-x-4", capacity) && again.m_Executed.Contains("selftest-x-0") && !FileExist(staging);

		// Bare ids past the compaction threshold, as the raw-line format
		// wrote them: the newest survive the compaction.
		VyshkaFiles.DeleteReplaced(path);
		int total = 2 * capacity + 6;
		array<string> bare = new array<string>;
		for (int i = 0; i < total; i++)
			bare.Insert("selftest-bare-" + i + "\n");
		VyshkaFiles.WriteAll(path, VyshkaJsonWriter.JoinPieces(bare));
		VyshkaPlugin compacting = new VyshkaPlugin();
		compacting.LoadExecuted();
		VyshkaPlugin reloaded = new VyshkaPlugin();
		reloaded.LoadExecuted();
		int lastBare = total - 1;
		bool legacyKept = VyshkaFiles.ReadLines(path).Count() == capacity && NewestIs(reloaded, "selftest-bare-" + lastBare, capacity);

		// An id that starts with a quote, and after it a record torn to
		// exactly that text.
		VyshkaFiles.DeleteReplaced(path);
		string q = "\"";
		string victim = q + "selftest-victim";
		VyshkaFiles.WriteAll(path, VyshkaJson.Quote(victim) + "\n" + victim);
		VyshkaPlugin torn = new VyshkaPlugin();
		torn.LoadExecuted();
		bool tornIgnored = torn.m_ExecutedOrder.Count() == 1 && torn.m_ExecutedOrder.Get(0) == victim;

		bool ok = recencyKept && recencyDurable && legacyKept && tornIgnored;
		string detail = "recencyKept=" + recencyKept;
		detail += "\trecencyDurable=" + recencyDurable;
		detail += "\tlegacyKept=" + legacyKept;
		detail += "\ttornIgnored=" + tornIgnored;
		Report("files.executedOrder", ok, detail);
		VyshkaFiles.DeleteReplaced(path);
	}

	// NewestIs says whether a plugin's LRU is full to count and ends with id.
	static bool NewestIs(VyshkaPlugin plugin, string id, int count)
	{
		int have = plugin.m_ExecutedOrder.Count();
		return have == count && plugin.m_ExecutedOrder.Get(have - 1) == id;
	}

	// ExecutedLines is count executed-log records, prefix0 onwards, each
	// JSON-quoted on a line of its own, with the staging copy's end line
	// after them when whole.
	static string ExecutedLines(string prefix, int from, int count, bool whole)
	{
		array<string> pieces = new array<string>;
		for (int i = from; i < from + count; i++)
			pieces.Insert(VyshkaJson.Quote(prefix + i) + "\n");
		if (whole)
			pieces.Insert(VyshkaFiles.LINES_END + "\n");
		return VyshkaJsonWriter.JoinPieces(pieces);
	}

	// PlantCut leaves a JSON file cut short partway, as a crash inside its
	// write would. Built from a quote alone: a literal combining escapes can
	// fail to parse on this engine (dayz-kb 09).
	static void PlantCut(string path)
	{
		string q = "\"";
		VyshkaFiles.WriteAll(path, "{" + q + "bans" + q + ": [ { " + q + "id" + q + ": " + q + "76561");
	}

	// Doc is a small document marked with its name, for files.replace.
	static VyshkaJsonValue Doc(string mark)
	{
		VyshkaJsonValue doc = VyshkaJsonValue.NewObject();
		doc.Set("mark", VyshkaJsonValue.NewString(mark));
		return doc;
	}

	static bool DocIs(VyshkaJsonValue doc, string mark)
	{
		return doc && doc.IsObject() && doc.GetString("mark", "") == mark;
	}

	static VyshkaBanEntry BanEntry(int i)
	{
		VyshkaBanEntry entry = new VyshkaBanEntry();
		entry.m_Id = SyntheticId(i);
		entry.m_Name = "Survivor " + i;
		entry.m_Reason = "banned by the self-test";
		entry.m_BannedAt = "2026-09-23T10:00:00Z";
		entry.m_Permanent = true;
		entry.m_ExpiresEpoch = 0;
		entry.m_ActionId = "selftest-" + i;
		return entry;
	}

	static array<ref VyshkaInstallationBanEntry> InstallationEntries(int count, string reason)
	{
		array<ref VyshkaInstallationBanEntry> entries = new array<ref VyshkaInstallationBanEntry>;
		for (int i = 0; i < count; i++)
		{
			VyshkaInstallationBanEntry entry = new VyshkaInstallationBanEntry();
			entry.m_BanId = "selftest-installation-" + i;
			entry.m_Id = SyntheticId(i);
			entry.m_Reason = reason;
			entry.m_Name = "Survivor " + i;
			entry.m_Permanent = true;
			entry.m_ExpiresEpoch = 0;
			entry.m_ExpiresText = "";
			entries.Insert(entry);
		}
		return entries;
	}

	// ---- files.manifest: a manifest record whose content, as one string,
	// would be past the reader's limit, saved as the object and read back
	// equal ----
	void CheckManifest()
	{
		VyshkaRegistry registry = new VyshkaRegistry();
		for (int i = 0; i < MANIFEST_ACTIONS; i++)
		{
			string code = "selftest.action" + i;
			registry.Register(new VyshkaSelfTestAction(code));
		}
		VyshkaJsonValue content = registry.Manifest("dayz", "vyshka-dayz-selftest", "0.0.0");
		string compact = content.Serialize();
		int bytes = compact.Length();
		int t0 = TickCount(0);
		bool saved = VyshkaManifestRecord.Save(4242, content, true, 7);
		int saveMs = Millis(TickCount(t0));
		string problem;
		int t1 = TickCount(0);
		VyshkaManifestRecord record = VyshkaManifestRecord.Load(problem);
		int loadMs = Millis(TickCount(t1));
		bool equal = record && record.m_Content == compact;
		bool marks = record && record.m_Revision == 4242 && record.m_Pending && record.m_Above == 7;
		bool ok = saved && equal && marks && bytes > VyshkaFiles.LINE_MAX && Within(saveMs) && Within(loadMs);
		string detail = "bytes=" + bytes;
		detail += "\tsaved=" + saved;
		detail += "\tequal=" + equal;
		detail += "\tmarks=" + marks;
		detail += "\tsaveMs=" + saveMs;
		detail += "\tloadMs=" + loadMs;
		if (!record)
			detail += "\tproblem=" + problem;
		Report("files.manifest", ok, detail);
	}

	// ---- files.manifestLegacy: a record plugin 0.8.0 wrote, with the
	// content as one JSON string, still reads ----
	void CheckManifestLegacy()
	{
		string legacy = "{\"game\":\"dayz\",\"actions\":[]}";
		VyshkaJsonValue record = VyshkaJsonValue.NewObject();
		record.Set("revision", VyshkaJsonValue.NewInt(5));
		record.Set("pending", VyshkaJsonValue.NewBool(false));
		record.Set("content", VyshkaJsonValue.NewString(legacy));
		bool written = VyshkaFiles.WriteJson(VyshkaFiles.MANIFEST_PATH, record);
		string problem;
		VyshkaManifestRecord loaded = VyshkaManifestRecord.Load(problem);
		bool ok = written && loaded && loaded.m_Revision == 5 && !loaded.m_Pending && loaded.m_Above == -2 && loaded.m_Content == legacy;
		string detail = "written=" + written;
		detail += "\tloaded=" + (loaded != null);
		if (!loaded)
			detail += "\tproblem=" + problem;
		Report("files.manifestLegacy", ok, detail);
		DeleteFile(VyshkaFiles.MANIFEST_PATH);
	}

	// ---- files.outbox: an event.batch record past the reader's old limit,
	// appended through the outbox and restored by a fresh one ----
	void CheckOutbox()
	{
		VyshkaOutbox outbox = new VyshkaOutbox();
		outbox.Load();
		int before = outbox.Count();
		string note = Repeat("a note long enough to make the batch wide; ", 7);
		VyshkaJsonValue events = VyshkaJsonValue.NewArray();
		for (int i = 0; i < BATCH_EVENTS; i++)
		{
			VyshkaJsonValue data = VyshkaJsonValue.NewObject();
			data.Set("index", VyshkaJsonValue.NewInt(i));
			data.Set("note", VyshkaJsonValue.NewString(note));
			VyshkaJsonValue position = VyshkaJsonValue.NewArray();
			position.Add(VyshkaJsonValue.NewFloat(4231.5 + i));
			position.Add(VyshkaJsonValue.NewFloat(300.25));
			position.Add(VyshkaJsonValue.NewFloat(10620.0 - i));
			data.Set("position", position);
			VyshkaJsonValue record = VyshkaJsonValue.NewObject();
			record.Set("t", VyshkaJsonValue.NewString("selftest.event"));
			record.Set("ts", VyshkaJsonValue.NewString("2026-09-21T10:00:00Z"));
			record.Set("data", data);
			events.Add(record);
		}
		VyshkaJsonValue body = VyshkaJsonValue.NewObject();
		body.Set("events", events);
		string compact = body.Serialize();
		int bytes = compact.Length();
		int t0 = TickCount(0);
		VyshkaOutboxEntry stored = outbox.Append("event.batch", body, BATCH_EVENTS);
		int appendMs = Millis(TickCount(t0));
		VyshkaOutbox again = new VyshkaOutbox();
		int t1 = TickCount(0);
		again.Load();
		int loadMs = Millis(TickCount(t1));
		bool restored = false;
		bool equal = false;
		if (stored && again.Count() == before + 1)
		{
			VyshkaOutboxEntry back = null;
			for (int j = 0; j < again.Count(); j++)
			{
				VyshkaOutboxEntry candidate = again.m_Entries.Get(j);
				if (candidate.m_Id == stored.m_Id)
					back = candidate;
			}
			restored = back != null;
			equal = back && back.m_Body == compact && back.m_Type == "event.batch" && back.m_Events == BATCH_EVENTS;
		}
		bool ok = stored && restored && equal && bytes > VyshkaFiles.LINE_MAX && Within(appendMs) && Within(loadMs);
		string detail = "bytes=" + bytes;
		detail += "\trestored=" + restored;
		detail += "\tequal=" + equal;
		detail += "\tappendMs=" + appendMs;
		detail += "\tloadMs=" + loadMs;
		Report("files.outbox", ok, detail);
		if (stored)
			DeleteFile(stored.Path());
	}

	// ---- files.longValue: a document whose one string value is past the
	// reader's limit is written in pieces and read back whole ----
	void CheckLongValue()
	{
		if (FileExist(OVERSIZED_PATH))
			DeleteFile(OVERSIZED_PATH);
		string value = Repeat("0123456789", 7000);
		VyshkaJsonValue document = VyshkaJsonValue.NewObject();
		document.Set("note", VyshkaJsonValue.NewString(value));
		document.Set("after", VyshkaJsonValue.NewInt(1));
		bool written = VyshkaFiles.WriteJson(OVERSIZED_PATH, document);
		VyshkaJsonValue back = VyshkaFiles.ReadJson(OVERSIZED_PATH);
		bool equal = back && back.IsObject() && back.GetString("note", "") == value && back.GetInt("after", 0) == 1;
		bool ok = written && equal;
		string detail = "written=" + written;
		detail += "\tequal=" + equal;
		Report("files.longValue", ok, detail);
		if (FileExist(OVERSIZED_PATH))
			DeleteFile(OVERSIZED_PATH);
	}

	// ---- files.refused: a document that would still carry a line past the
	// reader's limit (a key of that length), and one nested deeper than the
	// parser reads, are refused, not written ----
	void CheckRefused()
	{
		if (FileExist(OVERSIZED_PATH))
			DeleteFile(OVERSIZED_PATH);
		VyshkaJsonValue longKey = VyshkaJsonValue.NewObject();
		longKey.Set(Repeat("0123456789", 7000), VyshkaJsonValue.NewInt(1));
		bool keyWritten = VyshkaFiles.WriteJson(OVERSIZED_PATH, longKey);
		bool keyExists = FileExist(OVERSIZED_PATH);
		VyshkaJsonValue deep = VyshkaJsonValue.NewArray();
		VyshkaJsonValue root = deep;
		for (int i = 0; i < 80; i++)
		{
			VyshkaJsonValue inner = VyshkaJsonValue.NewArray();
			deep.Add(inner);
			deep = inner;
		}
		int depth = root.Depth();
		bool deepWritten = VyshkaFiles.WriteJson(OVERSIZED_PATH, root);
		bool deepExists = FileExist(OVERSIZED_PATH);
		bool ok = !keyWritten && !keyExists && !deepWritten && !deepExists && depth == 81;
		string detail = "keyWritten=" + keyWritten;
		detail += "\tkeyExists=" + keyExists;
		detail += "\tdeepWritten=" + deepWritten;
		detail += "\tdeepExists=" + deepExists;
		detail += "\tdepth=" + depth;
		Report("files.refused", ok, detail);
		if (FileExist(OVERSIZED_PATH))
			DeleteFile(OVERSIZED_PATH);
	}

	// ---- json.longString: a string value far past what Substring returns
	// is parsed whole ----
	void CheckLongString()
	{
		string value = Repeat("abcdefghij", LONG_STRING / 10);
		string text = "{\"v\":\"" + value + "\"}";
		int t0 = TickCount(0);
		VyshkaJsonValue root = VyshkaJson.Parse(text);
		int parseMs = Millis(TickCount(t0));
		int length = -1;
		bool equal = false;
		if (root && root.IsObject())
		{
			VyshkaJsonValue v = root.Get("v");
			if (v && v.IsString())
			{
				length = v.m_Text.Length();
				equal = v.m_Text == value;
			}
		}
		bool ok = length == LONG_STRING && equal && Within(parseMs);
		string detail = "length=" + length;
		detail += "\tequal=" + equal;
		detail += "\tparseMs=" + parseMs;
		Report("json.longString", ok, detail);
	}

	// ---- json.escapes: a long string dense with escapes round-trips
	// through Quote and Parse ----
	void CheckEscapes()
	{
		string quote = "\"";
		string unit = "ab" + quote + "cd\nef\tgh" + VyshkaJson.Backslash() + "ij";
		string text = Repeat(unit, 4000);
		int t0 = TickCount(0);
		string quoted = VyshkaJson.Quote(text);
		int quoteMs = Millis(TickCount(t0));
		int t1 = TickCount(0);
		VyshkaJsonValue back = VyshkaJson.Parse(quoted);
		int parseMs = Millis(TickCount(t1));
		bool equal = back && back.IsString() && back.m_Text == text;
		// Four escapes per unit, one character each, plus the quotes.
		int expected = text.Length() + 4 * 4000 + 2;
		int quotedLength = quoted.Length();
		bool ok = equal && quotedLength == expected && Within(quoteMs) && Within(parseMs);
		string detail = "length=" + text.Length();
		detail += "\tquoted=" + quotedLength;
		detail += "\tequal=" + equal;
		detail += "\tquoteMs=" + quoteMs;
		detail += "\tparseMs=" + parseMs;
		Report("json.escapes", ok, detail);
	}

	// ---- json.speed: the pull shape of issue #80 at 5 000 entries, about
	// 1.2 MB, serialized, parsed, written, and read back ----
	void CheckSpeed()
	{
		VyshkaJsonValue list = VyshkaJsonValue.NewArray();
		for (int i = 0; i < SPEED_ENTRIES; i++)
		{
			VyshkaJsonValue identity = VyshkaJsonValue.NewObject();
			identity.Set("platform", VyshkaJsonValue.NewString("steam"));
			identity.Set("id", VyshkaJsonValue.NewString(SyntheticId(i)));
			VyshkaJsonValue entry = VyshkaJsonValue.NewObject();
			entry.Set("identity", identity);
			entry.Set("name", VyshkaJsonValue.NewString("Survivor " + i));
			entry.Set("reason", VyshkaJsonValue.NewString("banned by the self-test for being a synthetic entry"));
			entry.Set("createdAt", VyshkaJsonValue.NewString("2026-09-21T10:00:00Z"));
			entry.Set("expiresAt", VyshkaJsonValue.NewNull());
			entry.Set("banId", VyshkaJsonValue.NewString("01K5SELFTEST" + i));
			list.Add(entry);
		}
		VyshkaJsonValue root = VyshkaJsonValue.NewObject();
		root.Set("revision", VyshkaJsonValue.NewInt(17));
		root.Set("bans", list);

		int t0 = TickCount(0);
		string text = root.Serialize();
		int serializeMs = Millis(TickCount(t0));
		int bytes = text.Length();
		int t1 = TickCount(0);
		VyshkaJsonValue parsed = VyshkaJson.Parse(text);
		int parseMs = Millis(TickCount(t1));
		int parsedCount = -1;
		if (parsed && parsed.IsObject())
		{
			VyshkaJsonValue bans = parsed.Get("bans");
			if (bans && bans.IsArray())
				parsedCount = bans.Count();
		}
		int t2 = TickCount(0);
		bool written = VyshkaFiles.WriteJson(SPEED_PATH, root);
		int writeMs = Millis(TickCount(t2));
		int t3 = TickCount(0);
		VyshkaJsonValue read = VyshkaFiles.ReadJson(SPEED_PATH);
		int readMs = Millis(TickCount(t3));
		int readCount = -1;
		if (read && read.IsObject())
		{
			VyshkaJsonValue readBans = read.Get("bans");
			if (readBans && readBans.IsArray())
				readCount = readBans.Count();
		}
		bool ok = parsedCount == SPEED_ENTRIES && written && readCount == SPEED_ENTRIES;
		ok = ok && Within(serializeMs) && Within(parseMs) && Within(writeMs) && Within(readMs);
		string detail = "bytes=" + bytes;
		detail += "\tparsed=" + parsedCount;
		detail += "\tread=" + readCount;
		detail += "\tserializeMs=" + serializeMs;
		detail += "\tparseMs=" + parseMs;
		detail += "\twriteMs=" + writeMs;
		detail += "\treadMs=" + readMs;
		Report("json.speed", ok, detail);
		if (FileExist(SPEED_PATH))
			DeleteFile(SPEED_PATH);
	}
}
