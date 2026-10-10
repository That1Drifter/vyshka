// Vyshka spike: edges of the DayZ script API the plugin leans on.
//
// The runner appends this to the spike mission's init.c and puts
// VyshkaEdgeProbe.Run() first in main(). Every result is one tab-separated
// line in the server script log:
//   VYSHKA_EDGE<TAB>phase=<p><TAB>event=<name><TAB>...
//
// Phase A (first boot): the cargo-size config paths, RestApi status codes,
// the RestApi read-timeout option at both ends of its documented range, and
// whether bytes written through FPrint are visible before CloseFile. It ends
// by writing a file it never closes and asking the runner to kill the process.
// Phase B (later boots): the file reader's line limit, one length per attempt;
// a length that faults the process is recorded by its missing "ok" line in
// the progress file, and the runner boots again.
//
// No game client and no mod are needed.

const int VYSHKA_EDGE_OPT_READ = 1;   // ERESTOPTION_READOPERATION, raw id (see the poll-timeout spike)

class VyshkaEdgeStep
{
	string m_Label;
	string m_Path;
	int m_OptionValue;   // read-timeout option to set first, 0 = leave alone
	int m_BudgetMs;

	void VyshkaEdgeStep(string label, string path, int optionValue, int budgetMs)
	{
		m_Label = label;
		m_Path = path;
		m_OptionValue = optionValue;
		m_BudgetMs = budgetMs;
	}
}

class VyshkaEdgeCallback : RestCallback
{
	int m_Step;
	int m_FiredAt;
	bool m_Settled;

	void VyshkaEdgeCallback(int step, int firedAt)
	{
		m_Step = step;
		m_FiredAt = firedAt;
		m_Settled = false;
	}

	void Emit(string evt, string extra)
	{
		int t = GetGame().GetTime() - m_FiredAt;
		Print("VYSHKA_EDGE\tphase=A\tevent=" + evt + "\tstep=" + m_Step + "\tt=" + t + "\tsettled=" + m_Settled + "\t" + extra);
	}

	override void OnError(int errorCode)
	{
		Emit("rest-error", "code=" + errorCode);
		Settle();
	}

	override void OnTimeout()
	{
		Emit("rest-timeout", "");
		Settle();
	}

	override void OnSuccess(string data, int dataSize)
	{
		string head = data;
		if (head.Length() > 80)
			head = head.Substring(0, 80);
		Emit("rest-success", "size=" + dataSize + "\tdata=" + head);
		Settle();
	}

	override void OnFileCreated(string fileName, int dataSize)
	{
		Emit("rest-filecreated", "size=" + dataSize);
		Settle();
	}

	void Settle()
	{
		if (m_Settled)
			return;
		m_Settled = true;
		if (VyshkaEdgeProbe.s_Instance)
			VyshkaEdgeProbe.s_Instance.OnStepSettled(m_Step);
	}
}

class VyshkaEdgeProbe
{
	static ref VyshkaEdgeProbe s_Instance;

	static const string BASE_URL = "http://127.0.0.1:8094/";
	static const string DIR = "$profile:VyshkaEdge";
	static const string PROGRESS = "$profile:VyshkaEdge/progress.txt";
	static const string UNCLOSED = "$profile:VyshkaEdge/unclosed.txt";
	static const string KILLED = "$profile:VyshkaEdge/killed.txt";
	static const string LINE = "$profile:VyshkaEdge/line.txt";
	static const int GAP_MS = 2000;
	static const int TICK_MS = 250;
	static const int FILE_BYTES = 4096;

	ref array<ref VyshkaEdgeStep> m_Steps;
	ref array<ref VyshkaEdgeCallback> m_Callbacks;
	ref TStringArray m_Progress;
	RestContext m_Context;
	int m_Current;
	int m_FiredAt;
	bool m_StepDone;
	FileHandle m_Unclosed;
	FileHandle m_Killed;

	static void Run()
	{
		s_Instance = new VyshkaEdgeProbe();
		GetGame().GetCallQueue(CALL_CATEGORY_SYSTEM).CallLater(s_Instance.Start, 3000, false);
	}

	void VyshkaEdgeProbe()
	{
		m_Current = -1;
		m_Steps = new array<ref VyshkaEdgeStep>;
		m_Callbacks = new array<ref VyshkaEdgeCallback>;
		m_Progress = new TStringArray;
	}

	void Log(string phase, string evt, string extra)
	{
		Print("VYSHKA_EDGE\tphase=" + phase + "\tevent=" + evt + "\t" + extra);
	}

	void Start()
	{
		MakeDirectory(DIR);
		ReadProgress();
		Log("-", "boot", "progress=" + m_Progress.Count());
		if (m_Progress.Find("A-done") < 0)
			PhaseA();
		else
			PhaseB();
	}

	// ---- progress file: one marker per line, appended and closed each time ----

	void ReadProgress()
	{
		m_Progress.Clear();
		if (!FileExist(PROGRESS))
			return;
		FileHandle fh = OpenFile(PROGRESS, FileMode.READ);
		if (fh == 0)
			return;
		string line;
		while (FGets(fh, line) >= 0)
		{
			line.Trim();
			if (line != "")
				m_Progress.Insert(line);
		}
		CloseFile(fh);
	}

	void Mark(string marker)
	{
		FileHandle fh = OpenFile(PROGRESS, FileMode.APPEND);
		if (fh == 0)
		{
			Log("-", "progress-failed", "marker=" + marker);
			return;
		}
		FPrintln(fh, marker);
		CloseFile(fh);
		m_Progress.Insert(marker);
	}

	// ---- phase A ----

	void PhaseA()
	{
		CargoPaths();

		// Status codes the earlier header spike did not cover, under the
		// default read timeout, then the read-timeout option at both ends.
		m_Steps.Insert(new VyshkaEdgeStep("status-200", "status?code=200", 0, 20000));
		m_Steps.Insert(new VyshkaEdgeStep("status-204", "status?code=204", 0, 20000));
		m_Steps.Insert(new VyshkaEdgeStep("status-302", "status?code=302", 0, 20000));
		m_Steps.Insert(new VyshkaEdgeStep("status-403", "status?code=403", 0, 20000));
		m_Steps.Insert(new VyshkaEdgeStep("status-404", "status?code=404", 0, 20000));
		m_Steps.Insert(new VyshkaEdgeStep("status-429", "status?code=429", 0, 20000));
		m_Steps.Insert(new VyshkaEdgeStep("status-502", "status?code=502", 0, 20000));
		m_Steps.Insert(new VyshkaEdgeStep("status-503", "status?code=503", 0, 20000));
		m_Steps.Insert(new VyshkaEdgeStep("read200/hold-150s", "hold?ms=150000", 200, 180000));
		m_Steps.Insert(new VyshkaEdgeStep("read1/hold-6s", "hold?ms=6000", 1, 30000));
		m_Steps.Insert(new VyshkaEdgeStep("read10/hold-5s", "hold?ms=5000", 10, 30000));

		RestApi api = GetRestApi();
		if (!api)
			api = CreateRestApi();
		if (!api)
		{
			Log("A", "abort", "reason=no-restapi");
			return;
		}
		m_Context = api.GetRestContext(BASE_URL);
		if (!m_Context)
		{
			Log("A", "abort", "reason=no-context");
			return;
		}
		m_Context.SetHeader("application/json");
		GetGame().GetCallQueue(CALL_CATEGORY_SYSTEM).CallLater(Next, GAP_MS, false);
	}

	// CargoPaths reads a cargo grid at the two config paths a class can
	// declare it under, for a few named classes and for every public
	// clothing class.
	void CargoPaths()
	{
		TStringArray named = {"AliceBag_Green", "HighCapacityVest_Black", "HuntingJacket_Brown", "HunterPants_Brown", "SeaChest"};
		foreach (string name : named)
		{
			string classPath = "CfgVehicles " + name;
			TIntArray atClass = new TIntArray;
			TIntArray inCargo = new TIntArray;
			GetGame().ConfigGetIntArray(classPath + " itemsCargoSize", atClass);
			GetGame().ConfigGetIntArray(classPath + " Cargo itemsCargoSize", inCargo);
			// Locals, not calls inside one expression: a pending int.ToString()
			// is overwritten by a later call's (spike dayz-enforce-int-tostring-temporary).
			string gridAtClass = Grid(atClass);
			string gridInCargo = Grid(inCargo);
			Log("A", "cargo-class", "class=" + name + "\tatClass=" + gridAtClass + "\tinCargo=" + gridInCargo);
		}

		string tree = "CfgVehicles";
		int children = GetGame().ConfigGetChildrenCount(tree);
		int clothing = 0;
		int atClassCount = 0;
		int inCargoCount = 0;
		TStringArray path = new TStringArray;
		for (int i = 0; i < children; i++)
		{
			string child;
			if (!GetGame().ConfigGetChildName(tree, i, child))
				continue;
			string childPath = tree + " " + child;
			if (GetGame().ConfigGetInt(childPath + " scope") != 2)
				continue;
			path.Clear();
			GetGame().ConfigGetFullPath(childPath, path);
			if (path.Find("Clothing") < 0)
				continue;
			clothing++;
			TIntArray a = new TIntArray;
			TIntArray c = new TIntArray;
			GetGame().ConfigGetIntArray(childPath + " itemsCargoSize", a);
			GetGame().ConfigGetIntArray(childPath + " Cargo itemsCargoSize", c);
			if (a.Count() >= 2)
				atClassCount++;
			if (c.Count() >= 2)
				inCargoCount++;
		}
		Log("A", "cargo-walk", "clothing=" + clothing + "\tatClass=" + atClassCount + "\tinCargo=" + inCargoCount);
	}

	string Grid(TIntArray values)
	{
		if (values.Count() < 2)
			return "none(" + values.Count() + ")";
		string w = values.Get(0).ToString();
		string h = values.Get(1).ToString();
		return w + "x" + h;
	}

	void Next()
	{
		m_Current++;
		if (m_Current >= m_Steps.Count())
		{
			Log("A", "rest-finished", "steps=" + m_Steps.Count());
			FileVisibility();
			return;
		}
		VyshkaEdgeStep step = m_Steps.Get(m_Current);
		if (step.m_OptionValue > 0)
			GetRestApi().SetOption(VYSHKA_EDGE_OPT_READ, step.m_OptionValue);
		m_StepDone = false;
		m_FiredAt = GetGame().GetTime();
		VyshkaEdgeCallback cb = new VyshkaEdgeCallback(m_Current, m_FiredAt);
		m_Callbacks.Insert(cb);
		Log("A", "rest-fire", "step=" + m_Current + "\tlabel=" + step.m_Label + "\tpath=" + step.m_Path + "\toption=" + step.m_OptionValue);
		m_Context.GET(cb, step.m_Path);
		GetGame().GetCallQueue(CALL_CATEGORY_SYSTEM).CallLater(Watchdog, TICK_MS, true);
	}

	void Watchdog()
	{
		VyshkaEdgeStep step = m_Steps.Get(m_Current);
		int elapsed = GetGame().GetTime() - m_FiredAt;
		if (!m_StepDone && elapsed < step.m_BudgetMs)
			return;
		if (!m_StepDone)
			Log("A", "rest-budget-expired", "step=" + m_Current + "\tt=" + elapsed);
		GetGame().GetCallQueue(CALL_CATEGORY_SYSTEM).Remove(Watchdog);
		GetGame().GetCallQueue(CALL_CATEGORY_SYSTEM).CallLater(Next, GAP_MS, false);
	}

	void OnStepSettled(int step)
	{
		if (step == m_Current)
			m_StepDone = true;
	}

	// FileVisibility writes FILE_BYTES through FPrint and holds the handle
	// open for 10 s before closing it; the runner reads the file's size from
	// outside the process at each marker.
	void FileVisibility()
	{
		m_Unclosed = OpenFile(UNCLOSED, FileMode.WRITE);
		if (m_Unclosed == 0)
		{
			Log("A", "abort", "reason=open-unclosed");
			return;
		}
		FPrint(m_Unclosed, Block(FILE_BYTES));
		Log("A", "unclosed-written", "bytes=" + FILE_BYTES);
		GetGame().GetCallQueue(CALL_CATEGORY_SYSTEM).CallLater(CloseUnclosed, 10000, false);
	}

	void CloseUnclosed()
	{
		CloseFile(m_Unclosed);
		Log("A", "unclosed-closed", "bytes=" + FILE_BYTES);
		GetGame().GetCallQueue(CALL_CATEGORY_SYSTEM).CallLater(KillStep, 5000, false);
	}

	// KillStep marks phase A done, then writes a file it never closes and
	// asks the runner to kill the process.
	void KillStep()
	{
		Mark("A-done");
		m_Killed = OpenFile(KILLED, FileMode.WRITE);
		if (m_Killed == 0)
		{
			Log("A", "abort", "reason=open-killed");
			return;
		}
		FPrint(m_Killed, Block(FILE_BYTES));
		Log("A", "kill-now", "bytes=" + FILE_BYTES);
	}

	string Block(int bytes)
	{
		string unit = "0123456789abcdef";
		string text = "";
		while (text.Length() + 16 <= bytes)
			text += unit;
		while (text.Length() < bytes)
			text += "x";
		return text;
	}

	// ---- phase B ----

	void PhaseB()
	{
		TIntArray lengths = {65534, 65535, 65536};
		foreach (int length : lengths)
		{
			string ok = "ok " + length;
			string attempt = "attempt " + length;
			if (m_Progress.Find(ok) >= 0)
				continue;
			if (m_Progress.Find(attempt) >= 0)
			{
				Log("B", "line-faulted-before", "len=" + length);
				continue;
			}
			Mark(attempt);
			MeasureLine(length);
			Mark(ok);
		}
		Log("-", "done", "progress=" + m_Progress.Count());
	}

	void MeasureLine(int length)
	{
		string text = Block(length);
		FileHandle writer = OpenFile(LINE, FileMode.WRITE);
		if (writer == 0)
		{
			Log("B", "abort", "reason=open-line");
			return;
		}
		FPrint(writer, text);
		CloseFile(writer);
		Log("B", "line-written", "len=" + text.Length());

		FileHandle back = OpenFile(LINE, FileMode.READ);
		string line;
		int got = FGets(back, line);
		CloseFile(back);
		Log("B", "line-read", "len=" + length + "\tfgets=" + got + "\tgot=" + line.Length() + "\tintact=" + (line == text));
	}
}
