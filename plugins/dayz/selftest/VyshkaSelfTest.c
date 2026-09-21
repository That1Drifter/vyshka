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
	static const int PLAN = 15;
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
