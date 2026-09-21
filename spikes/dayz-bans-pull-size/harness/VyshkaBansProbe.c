// Vyshka spike: what an installation ban list pull costs the DayZ server.
//
// Appended to the init.c of a mission run with the Vyshka mod loaded (the
// plugin needs no config: it idles, and only its classes are used), and
// started from main() with VyshkaBansProbe.Run(). Three series:
//
//   bans-N   fetch a synthetic ban list of N entries from the spike stub over
//            the same RestApi the plugin uses, then do on the server thread
//            what the pull of issue #80 will do: parse the body with the
//            plugin's JSON parser, build a per-identity map of ban entries,
//            look identities up, serialize the list back to text, and write
//            it under $profile: with the plugin's writer.
//   line-L   write a one-line file of L bytes with the plugin's writer and
//            read it back with its reader (an FGets loop). Run 2 of this
//            spike found the engine faults natively on a long line, so this
//            series brackets the limit; each fault costs a boot.
//   raw-B    fetch a padded body of B bytes, above the 1 MiB the earlier
//            spike (../../dayz-restapi-headers) stopped at.
//
// Because a step can take the process down, the probe persists the index
// of the next step in $profile:VyshkaSpike/progress.txt before firing each
// one, and the runner reboots the server until the series reports finished.
// A step that crashed is therefore skipped on the next boot, and the runner
// pairs the crash with the last "fire" line of the boot that died.
//
// Every phase is timed with the engine's performance counter, which runs
// inside one frame, unlike GetGame().GetTime(), which advances per frame.
// The counter's unit is not documented, so every line also carries the
// frame clock and the runner derives ticks per millisecond from the pairs.
//
// Line format (tab separated):
//   VYSHKA_BPROBE<TAB>step=<n><TAB>event=<name><TAB>t=<frame ms since fire><TAB>ticks=<counter>...
//
// Everything runs server side; no game client is required.

class VyshkaBansStep
{
	string m_Label;
	string m_Kind;     // "bans", "line", or "raw"
	string m_Path;     // stub path for bans and raw
	int m_Size;        // entries for bans, bytes for line
	int m_BudgetMs;

	void VyshkaBansStep(string label, string kind, string path, int size, int budgetMs)
	{
		m_Label = label;
		m_Kind = kind;
		m_Path = path;
		m_Size = size;
		m_BudgetMs = budgetMs;
	}
}

class VyshkaBansCallback : RestCallback
{
	int m_Step;
	string m_Kind;
	int m_FiredAt;
	bool m_Settled;

	void VyshkaBansCallback(int step, string kind, int firedAt)
	{
		m_Step = step;
		m_Kind = kind;
		m_FiredAt = firedAt;
		m_Settled = false;
	}

	int Elapsed()
	{
		return GetGame().GetTime() - m_FiredAt;
	}

	void Emit(string evt, string extra)
	{
		Print("VYSHKA_BPROBE\tstep=" + m_Step + "\tevent=" + evt + "\tt=" + Elapsed() + "\tticks=" + TickCount(0) + "\t" + extra);
	}

	override void OnError(int errorCode)
	{
		Emit("error", "code=" + errorCode);
		Settle();
	}

	override void OnTimeout()
	{
		Emit("timeout", "");
		Settle();
	}

	override void OnSuccess(string data, int dataSize)
	{
		Emit("success", "size=" + dataSize + "\tlen=" + data.Length());
		if (m_Kind == "bans")
			Measure(data);
		Settle();
	}

	override void OnFileCreated(string fileName, int dataSize)
	{
		Emit("filecreated", "size=" + dataSize);
		Settle();
	}

	// Measure runs the pull's work on the body, synchronously, on the thread
	// the callback runs on (the server's main thread), and logs one line.
	void Measure(string data)
	{
		int t0 = TickCount(0);
		VyshkaJsonValue root = VyshkaJson.Parse(data);
		int parseTicks = TickCount(t0);
		if (!root || !root.IsObject())
		{
			Emit("measured", "phase=parse\tok=0\tparseTicks=" + parseTicks);
			return;
		}
		VyshkaJsonValue list = root.Get("bans");
		if (!list || !list.IsArray())
		{
			Emit("measured", "phase=parse\tok=0\tparseTicks=" + parseTicks + "\treason=no-bans-array");
			return;
		}

		// Build: the per-identity map the connect-time check will consult.
		int t1 = TickCount(0);
		ref map<string, ref VyshkaBanEntry> entries = new map<string, ref VyshkaBanEntry>;
		int finite = 0;
		int skipped = 0;
		for (int i = 0; i < list.Count(); i++)
		{
			VyshkaJsonValue item = list.At(i);
			if (!item || !item.IsObject())
			{
				skipped++;
				continue;
			}
			VyshkaJsonValue identity = item.Get("identity");
			if (!identity || !identity.IsObject())
			{
				skipped++;
				continue;
			}
			string platform = identity.GetString("platform", "");
			string id = identity.GetString("id", "");
			if (platform != "steam" || id == "")
			{
				skipped++;
				continue;
			}
			VyshkaBanEntry entry = new VyshkaBanEntry();
			entry.m_Id = id;
			entry.m_Name = VyshkaAction.Bound(item.GetString("name", ""), VyshkaBanEntry.MAX_NAME);
			entry.m_Reason = VyshkaAction.Bound(item.GetString("reason", ""), VyshkaBanEntry.MAX_REASON);
			entry.m_BannedAt = VyshkaAction.Bound(item.GetString("createdAt", ""), 40);
			entry.m_ActionId = VyshkaAction.Bound(item.GetString("banId", ""), VyshkaBanEntry.MAX_ACTION_ID);
			entry.m_Permanent = true;
			entry.m_ExpiresEpoch = 0;
			VyshkaJsonValue expires = item.Get("expiresAt");
			if (expires && expires.IsString())
			{
				int epoch;
				if (VyshkaClock.ParseRfc3339(expires.m_Text, epoch))
				{
					entry.m_Permanent = false;
					entry.m_ExpiresEpoch = epoch;
					finite++;
				}
			}
			entries.Set(id, entry);
		}
		int buildTicks = TickCount(t1);

		// Lookup: 1000 connect-time checks, half hits and half misses.
		int t2 = TickCount(0);
		int hits = 0;
		for (int k = 0; k < 1000; k++)
		{
			string probe;
			if (k % 2 == 0)
				probe = VyshkaBansProbe.SyntheticId(k % list.Count());
			else
				probe = "76561190000000000";
			if (entries.Contains(probe))
				hits++;
		}
		int lookupTicks = TickCount(t2);

		// Serialize the parsed tree back to text, as the file write will.
		int t3 = TickCount(0);
		string text = root.Serialize();
		int serializeTicks = TickCount(t3);

		// Write it where the pull would keep its copy. No read back here:
		// the line-L series measures the reader, and run 2 showed a read of
		// this file at 500 entries takes the process down.
		int t4 = TickCount(0);
		bool written = VyshkaFiles.WriteAll(VyshkaBansProbe.FILE_PATH, text);
		int writeTicks = TickCount(t4);

		// One statement per phase: the engine's parser does not accept an
		// expression continued on the next line with a leading operator.
		string line = "phase=all\tok=1\tn=" + list.Count() + "\tentries=" + entries.Count() + "\tfinite=" + finite + "\tskipped=" + skipped;
		line += "\tparseTicks=" + parseTicks + "\tbuildTicks=" + buildTicks + "\tlookupTicks=" + lookupTicks + "\thits=" + hits;
		line += "\tserializeTicks=" + serializeTicks + "\ttextLen=" + text.Length() + "\twriteTicks=" + writeTicks + "\twritten=" + written;
		Emit("measured", line);
	}

	void Settle()
	{
		if (!m_Settled)
		{
			m_Settled = true;
			if (VyshkaBansProbe.s_Instance)
				VyshkaBansProbe.s_Instance.OnStepSettled(m_Step);
		}
	}
}

class VyshkaBansProbe
{
	static ref VyshkaBansProbe s_Instance;

	static const string BASE_URL = "http://127.0.0.1:8096/";
	static const string DIR = "$profile:VyshkaSpike";
	static const string FILE_PATH = "$profile:VyshkaSpike/installation-bans.json";
	static const string LINE_PATH = "$profile:VyshkaSpike/line.txt";
	static const string PROGRESS_PATH = "$profile:VyshkaSpike/progress.txt";
	static const int GAP_MS = 3000;
	static const int TICK_MS = 250;

	ref array<ref VyshkaBansStep> m_Steps;
	ref array<ref VyshkaBansCallback> m_Callbacks;
	RestContext m_Context;
	int m_Current;
	int m_FiredAt;
	bool m_StepDone;

	static void Run()
	{
		if (s_Instance)
			return;
		s_Instance = new VyshkaBansProbe();
		s_Instance.Start();
	}

	void VyshkaBansProbe()
	{
		m_Current = -1;
		m_FiredAt = 0;
		m_StepDone = false;
		m_Steps = new array<ref VyshkaBansStep>;
		m_Callbacks = new array<ref VyshkaBansCallback>;
	}

	// SyntheticId is the Steam64 the stub gives entry i; the lookup phase
	// uses it to hit real keys. 17 digits like a real one: a fixed 8-digit
	// prefix and i zero-padded to 9, built without arithmetic a 32-bit int
	// cannot hold.
	static string SyntheticId(int i)
	{
		string digits = i.ToString();
		while (digits.Length() < 9)
			digits = "0" + digits;
		return "76561198" + digits;
	}

	void Log(int step, string evt, int t, string extra)
	{
		Print("VYSHKA_BPROBE\tstep=" + step + "\tevent=" + evt + "\tt=" + t + "\tticks=" + TickCount(0) + "\t" + extra);
	}

	void Start()
	{
		if (!FileExist(DIR))
			MakeDirectory(DIR);

		// Series 1: ban lists in the planned pull shape, ascending.
		m_Steps.Insert(new VyshkaBansStep("bans-100",    "bans", "bans?n=100",    100,    60000));
		m_Steps.Insert(new VyshkaBansStep("bans-500",    "bans", "bans?n=500",    500,    60000));
		m_Steps.Insert(new VyshkaBansStep("bans-1000",   "bans", "bans?n=1000",   1000,   60000));
		m_Steps.Insert(new VyshkaBansStep("bans-2000",   "bans", "bans?n=2000",   2000,   60000));
		m_Steps.Insert(new VyshkaBansStep("bans-5000",   "bans", "bans?n=5000",   5000,   90000));
		m_Steps.Insert(new VyshkaBansStep("bans-10000",  "bans", "bans?n=10000",  10000,  120000));
		m_Steps.Insert(new VyshkaBansStep("bans-20000",  "bans", "bans?n=20000",  20000,  180000));
		m_Steps.Insert(new VyshkaBansStep("bans-50000",  "bans", "bans?n=50000",  50000,  300000));

		// Series 2: the reader's line limit, ascending; the runner stops the
		// series at the first fault.
		// Sizes are multiples of the 16-byte padding unit, because the line is
		// built by appending units: run 4 trimmed it with Substring and got
		// 8 191 bytes every time, which series 4 checks directly.
		m_Steps.Insert(new VyshkaBansStep("line-16k",    "line", "", 16384,  10000));
		m_Steps.Insert(new VyshkaBansStep("line-32k",    "line", "", 32768,  10000));
		m_Steps.Insert(new VyshkaBansStep("line-48k",    "line", "", 49152,  10000));
		m_Steps.Insert(new VyshkaBansStep("line-65520",  "line", "", 65520,  10000));
		m_Steps.Insert(new VyshkaBansStep("line-65536",  "line", "", 65536,  10000));
		m_Steps.Insert(new VyshkaBansStep("line-65552",  "line", "", 65552,  10000));
		m_Steps.Insert(new VyshkaBansStep("line-80k",    "line", "", 81920,  10000));
		m_Steps.Insert(new VyshkaBansStep("line-128k",   "line", "", 131072, 10000));

		// Series 4: is string indexing itself linear in the index? The ban
		// list series grew four times slower for every doubling, and the
		// parser reads one character at a time with string.Get. The same
		// step checks whether Substring caps its result.
		m_Steps.Insert(new VyshkaBansStep("index-1m", "index", "", 1048576, 60000));

		// Series 3: raw responses above the 1 MiB the earlier spike reached.
		m_Steps.Insert(new VyshkaBansStep("raw-2m",  "raw", "big?n=2097152",  0, 60000));
		m_Steps.Insert(new VyshkaBansStep("raw-4m",  "raw", "big?n=4194304",  0, 60000));
		m_Steps.Insert(new VyshkaBansStep("raw-8m",  "raw", "big?n=8388608",  0, 90000));
		m_Steps.Insert(new VyshkaBansStep("raw-16m", "raw", "big?n=16777216", 0, 120000));
		m_Steps.Insert(new VyshkaBansStep("raw-32m", "raw", "big?n=33554432", 0, 180000));

		// Series 5: where the quadratic parse cost comes from. Run 5 showed
		// character indexing is constant-time, so the suspects are string
		// append (copying the whole string each time) and object allocation
		// slowing as the live count grows. The alloc step times both in
		// isolation; the padded lists make the body large without adding
		// objects, so a parse that stays fast is quadratic in objects, and
		// one that explodes is quadratic in bytes.
		m_Steps.Insert(new VyshkaBansStep("bans-100-pad1m",   "bans", "bans?n=100&pad=1048576", 100, 300000));
		m_Steps.Insert(new VyshkaBansStep("bans-100-pad256k", "bans", "bans?n=100&pad=262144",  100, 120000));
		m_Steps.Insert(new VyshkaBansStep("alloc", "alloc", "", 0, 120000));

		// Series 6: run 6 showed the parse cost is per byte of total input
		// and proportional to the input's length, while run 5 showed a read
		// on a local string is constant-time. The parser reads a member
		// string; this step times the same reads on a member, a local, and
		// a by-value parameter of the same content.
		m_Steps.Insert(new VyshkaBansStep("member", "member", "", 262144, 120000));

		// Resume where the previous boot left off.
		string progress;
		if (VyshkaFiles.ReadAll(PROGRESS_PATH, progress))
		{
			progress = progress.Trim();
			int resumeAt = progress.ToInt();
			if (resumeAt > 0)
				m_Current = resumeAt - 1;
		}
		Log(-1, "boot", 0, "resumeAt=" + (m_Current + 1) + "\tsteps=" + m_Steps.Count());

		RestApi api = GetRestApi();
		if (!api)
			api = CreateRestApi();
		if (!api)
		{
			Log(-1, "abort", 0, "reason=no-restapi");
			return;
		}
		m_Context = api.GetRestContext(BASE_URL);
		Log(-1, "context", 0, "ctx=" + (m_Context != null));
		if (!m_Context)
		{
			Log(-1, "abort", 0, "reason=no-context");
			return;
		}
		m_Context.SetHeader("application/json");

		GetGame().GetCallQueue(CALL_CATEGORY_SYSTEM).CallLater(Next, GAP_MS, false);
	}

	void Next()
	{
		m_Current++;
		if (m_Current >= m_Steps.Count())
		{
			VyshkaFiles.WriteAll(PROGRESS_PATH, m_Current.ToString());
			Log(-1, "finished", 0, "steps=" + m_Steps.Count());
			return;
		}

		// If this step takes the process down, the next boot skips it. A
		// checkpoint that cannot be saved means the step could be run again
		// on every boot, so the series stops here instead.
		if (!VyshkaFiles.WriteAll(PROGRESS_PATH, (m_Current + 1).ToString()))
		{
			Log(-1, "abort", 0, "reason=progress-unwritable\tstep=" + m_Current);
			return;
		}

		VyshkaBansStep step = m_Steps.Get(m_Current);
		m_StepDone = false;
		m_FiredAt = GetGame().GetTime();
		Log(m_Current, "fire", 0, "label=" + step.m_Label + "\tkind=" + step.m_Kind + "\tpath=" + step.m_Path + "\tsize=" + step.m_Size);

		if (step.m_Kind == "line")
		{
			MeasureLine(step.m_Size);
			m_StepDone = true;
		}
		else if (step.m_Kind == "index")
		{
			MeasureIndex(step.m_Size);
			m_StepDone = true;
		}
		else if (step.m_Kind == "alloc")
		{
			MeasureAlloc();
			m_StepDone = true;
		}
		else if (step.m_Kind == "member")
		{
			MeasureMember(step.m_Size);
			m_StepDone = true;
		}
		else
		{
			VyshkaBansCallback cb = new VyshkaBansCallback(m_Current, step.m_Kind, m_FiredAt);
			m_Callbacks.Insert(cb);
			int rc = m_Context.GET(cb, step.m_Path);
			Log(m_Current, "submitted", GetGame().GetTime() - m_FiredAt, "rc=" + rc);
		}

		GetGame().GetCallQueue(CALL_CATEGORY_SYSTEM).CallLater(Watchdog, TICK_MS, true);
	}

	// MeasureLine writes one line of the given length with the plugin's
	// writer and reads it back with its reader. A native fault here ends the
	// process; the runner reads that from the crash report.
	void MeasureLine(int length)
	{
		string unit = "0123456789abcdef";
		string text = "";
		while (text.Length() < length)
			text += unit;

		int t0 = TickCount(0);
		bool written = VyshkaFiles.WriteAll(LINE_PATH, text);
		int writeTicks = TickCount(t0);
		Log(m_Current, "line-written", GetGame().GetTime() - m_FiredAt, "len=" + text.Length() + "\twritten=" + written + "\twriteTicks=" + writeTicks);

		int t1 = TickCount(0);
		string back;
		bool read = VyshkaFiles.ReadAll(LINE_PATH, back);
		int readTicks = TickCount(t1);
		Log(m_Current, "line-read", GetGame().GetTime() - m_FiredAt, "len=" + back.Length() + "\tread=" + read + "\tintact=" + (back == text) + "\treadTicks=" + readTicks);
	}

	// MeasureIndex times 1000 single-character reads and 1000 one-character
	// Substring calls at the start, the middle, and the end of a string of
	// the given length. Equal times mean constant-time indexing; times that
	// grow with the position mean the engine walks the string, and every
	// character-at-a-time parser is quadratic in the input.
	void MeasureIndex(int length)
	{
		string unit = "0123456789abcdef";
		string text = "";
		while (text.Length() < length)
			text += unit;

		// Does Substring cap its result? Run 4's trim came back 8 191 long.
		string sub8191 = text.Substring(0, 8191);
		string sub8192 = text.Substring(0, 8192);
		string sub100k = text.Substring(0, 100000);
		string subTail = text.Substring(length - 100, 100);
		string line = "len=" + text.Length() + "\tsub8191=" + sub8191.Length() + "\tsub8192=" + sub8192.Length() + "\tsub100k=" + sub100k.Length() + "\tsubTail100=" + subTail.Length();
		for (int p = 0; p < 3; p++)
		{
			int at = 16;
			if (p == 1)
				at = length / 2;
			else if (p == 2)
				at = length - 2;
			int sink = 0;
			int t0 = TickCount(0);
			for (int i = 0; i < 1000; i++)
			{
				string c = text.Get(at);
				sink += c.Length();
			}
			int getTicks = TickCount(t0);
			int t1 = TickCount(0);
			for (int j = 0; j < 1000; j++)
			{
				string s = text.Substring(at, 1);
				sink += s.Length();
			}
			int subTicks = TickCount(t1);
			line += "\tat" + p.ToString() + "=" + at + "\tget" + p.ToString() + "Ticks=" + getTicks + "\tsub" + p.ToString() + "Ticks=" + subTicks + "\tsink" + p.ToString() + "=" + sink;
		}
		Log(m_Current, "index-measured", GetGame().GetTime() - m_FiredAt, line);
	}

	// MeasureAlloc times, in batches, the two operations the parser and the
	// serializer lean on: allocating script objects while earlier ones stay
	// alive, and appending to one growing string. A batch time that grows
	// with the batch number is a cost proportional to what already exists.
	void MeasureAlloc()
	{
		string line = "";

		// Six batches of 10 000 JSON objects, all kept alive.
		ref array<ref VyshkaJsonValue> values = new array<ref VyshkaJsonValue>;
		for (int b = 0; b < 6; b++)
		{
			int t0 = TickCount(0);
			for (int i = 0; i < 10000; i++)
				values.Insert(VyshkaJsonValue.NewObject());
			line += "\tobj" + b.ToString() + "Ticks=" + TickCount(t0);
		}
		// Release them all and allocate one batch more: back to the first
		// batch's time, or still slow?
		values.Clear();
		int t1 = TickCount(0);
		for (int k = 0; k < 10000; k++)
			values.Insert(VyshkaJsonValue.NewObject());
		line += "\tobjAfterClearTicks=" + TickCount(t1);
		values.Clear();
		// Print truncates a line at 255 characters: one line per group.
		Log(m_Current, "alloc-objects", GetGame().GetTime() - m_FiredAt, line);
		line = "";

		// The same with a plain script class holding only scalars.
		ref array<ref VyshkaBanEntry> entries = new array<ref VyshkaBanEntry>;
		for (int c = 0; c < 6; c++)
		{
			int t2 = TickCount(0);
			for (int m = 0; m < 10000; m++)
				entries.Insert(new VyshkaBanEntry());
			line += "\tentry" + c.ToString() + "Ticks=" + TickCount(t2);
		}
		entries.Clear();
		Log(m_Current, "alloc-entries", GetGame().GetTime() - m_FiredAt, line);
		line = "";

		// Six batches of 4 096 sixteen-byte appends to one string.
		string unit = "0123456789abcdef";
		string grown = "";
		for (int d = 0; d < 6; d++)
		{
			int t3 = TickCount(0);
			for (int n = 0; n < 4096; n++)
				grown += unit;
			line += "\tappend" + d.ToString() + "Ticks=" + TickCount(t3) + "\tlen" + d.ToString() + "=" + grown.Length();
		}
		Log(m_Current, "alloc-append", GetGame().GetTime() - m_FiredAt, line);
		line = "";

		// Indexing a short string against the 384 KiB one just built.
		string tiny = "0123456789abcdef0123456789abcdef";
		int sink = 0;
		int t4 = TickCount(0);
		for (int p = 0; p < 1000; p++)
		{
			string a = tiny.Get(16);
			sink += a.Length();
		}
		line += "\tgetShortTicks=" + TickCount(t4);
		int t5 = TickCount(0);
		for (int q = 0; q < 1000; q++)
		{
			string z = grown.Get(16);
			sink += z.Length();
		}
		line += "\tgetLongTicks=" + TickCount(t5) + "\tsink=" + sink;
		Log(m_Current, "alloc-get", GetGame().GetTime() - m_FiredAt, line);
	}

	string m_Big;   // the member copy for MeasureMember

	// ReadParam reads 1000 characters from a by-value string parameter.
	int ReadParam(string text)
	{
		int sink = 0;
		for (int i = 0; i < 1000; i++)
		{
			string c = text.Get(16);
			sink += c.Length();
		}
		return sink;
	}

	// ReadMember reads 1000 characters from the member string.
	int ReadMember()
	{
		int sink = 0;
		for (int i = 0; i < 1000; i++)
		{
			string c = m_Big.Get(16);
			sink += c.Length();
		}
		return sink;
	}

	// AppendOut appends 1000 units to an out-parameter string, the way the
	// serializer's WriteTo builds its result.
	void AppendOut(out string result)
	{
		string unit = "0123456789abcdef";
		for (int i = 0; i < 1000; i++)
			result += unit;
	}

	void MeasureMember(int length)
	{
		string unit = "0123456789abcdef";
		// `local` is a reserved word; the local copy is `held`.
		string held = "";
		while (held.Length() < length)
			held += unit;
		m_Big = held;
		string line = "len=" + held.Length();
		int sink = 0;

		int t0 = TickCount(0);
		for (int i = 0; i < 1000; i++)
		{
			string a = held.Get(16);
			sink += a.Length();
		}
		line += "\tlocalGetTicks=" + TickCount(t0);

		int t1 = TickCount(0);
		for (int j = 0; j < 1000; j++)
		{
			string b = m_Big.Get(16);
			sink += b.Length();
		}
		line += "\tmemberGetTicks=" + TickCount(t1);

		int t2 = TickCount(0);
		sink += ReadMember();
		line += "\tmemberMethodGetTicks=" + TickCount(t2);

		int t3 = TickCount(0);
		sink += ReadParam(held);
		line += "\tparamGetTicks=" + TickCount(t3);
		Log(m_Current, "member-get", GetGame().GetTime() - m_FiredAt, line);
		line = "";

		int t4 = TickCount(0);
		for (int k = 0; k < 1000; k++)
		{
			string s = m_Big.Substring(16, 20);
			sink += s.Length();
		}
		line += "\tmemberSubstringTicks=" + TickCount(t4);

		// Copying the member into a local once, then reading the local.
		int t5 = TickCount(0);
		string copy = m_Big;
		line += "\tcopyTicks=" + TickCount(t5);
		int t6 = TickCount(0);
		for (int m = 0; m < 1000; m++)
		{
			string d = copy.Get(16);
			sink += d.Length();
		}
		line += "\tcopiedLocalGetTicks=" + TickCount(t6);
		Log(m_Current, "member-copy", GetGame().GetTime() - m_FiredAt, line);
		line = "";

		// Appending through an out parameter to a string that is already
		// long, against appending to a local of the same length.
		string grownOut = held;
		int t7 = TickCount(0);
		AppendOut(grownOut);
		line += "\tappendOutTicks=" + TickCount(t7);
		string grownLocal = held;
		int t8 = TickCount(0);
		for (int n = 0; n < 1000; n++)
			grownLocal += unit;
		line += "\tappendLocalTicks=" + TickCount(t8) + "\tsink=" + sink;
		Log(m_Current, "member-append", GetGame().GetTime() - m_FiredAt, line);
	}

	void Watchdog()
	{
		VyshkaBansStep step = m_Steps.Get(m_Current);
		int elapsed = GetGame().GetTime() - m_FiredAt;

		// A calibration pair every second while a step is in flight.
		if (elapsed % 1000 < TICK_MS)
			Log(m_Current, "calib", elapsed, "");

		if (!m_StepDone && elapsed < step.m_BudgetMs)
			return;

		if (!m_StepDone)
			Log(m_Current, "budget-expired", elapsed, "no-callback=1");

		GetGame().GetCallQueue(CALL_CATEGORY_SYSTEM).Remove(Watchdog);
		GetGame().GetCallQueue(CALL_CATEGORY_SYSTEM).CallLater(Next, GAP_MS, false);
	}

	void OnStepSettled(int step)
	{
		if (step == m_Current)
			m_StepDone = true;
	}
}
