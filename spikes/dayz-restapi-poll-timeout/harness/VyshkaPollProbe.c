// Vyshka spike: DayZ RestApi long-poll hold time probe.
//
// Paste this into a mission init.c and call VyshkaPollProbe.Run() from main().
// It walks a fixed matrix of held responses against the spike stub server and
// prints one machine readable line per event into the server script log.
//
// Line format (tab separated, so results parsing stays trivial):
//   VYSHKA_PROBE<TAB>step=<n><TAB>event=<name><TAB>t=<ms since fire><TAB>...
//
// Everything runs server side; no game client is required.

// The ERestOption enum constants are declared in the engine script headers but
// do not resolve from a mission init script, so the probe passes the raw option
// ids. Values follow the declaration order in scripts/3_game/http/restapi.c.
const int VYSHKA_RESTOPT_READ = 1;   // ERESTOPTION_READOPERATION
const int VYSHKA_RESTOPT_CONN = 2;   // ERESTOPTION_CONNECTION

class VyshkaProbeStep
{
	string m_Label;      // what this step is testing
	string m_Path;       // request path appended to the context base URL
	int m_OptionId;      // RestApi option to set before firing, 0 = leave alone
	int m_OptionValue;   // value for that option, in seconds
	int m_BudgetMs;      // how long to wait before giving up on this step

	void VyshkaProbeStep(string label, string path, int optionId, int optionValue, int budgetMs)
	{
		m_Label = label;
		m_Path = path;
		m_OptionId = optionId;
		m_OptionValue = optionValue;
		m_BudgetMs = budgetMs;
	}
}

class VyshkaProbeCallback : RestCallback
{
	int m_Step;
	int m_FiredAt;
	bool m_Settled;      // first terminal callback wins; later ones are logged as extras

	void VyshkaProbeCallback(int step, int firedAt)
	{
		m_Step = step;
		m_FiredAt = firedAt;
		m_Settled = false;
	}

	int Elapsed()
	{
		return GetGame().GetTime() - m_FiredAt;
	}

	void Emit(string evt, string extra)
	{
		Print("VYSHKA_PROBE\tstep=" + m_Step + "\tevent=" + evt + "\tt=" + Elapsed() + "\tsettled=" + m_Settled + "\t" + extra);
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
		// Print() truncates past ~1024 bytes, so log the size plus a prefix.
		string head = data;
		if (head.Length() > 120)
			head = head.Substring(0, 120);
		Emit("success", "size=" + dataSize + "\tdata=" + head);
		Settle();
	}

	override void OnFileCreated(string fileName, int dataSize)
	{
		Emit("filecreated", "size=" + dataSize);
		Settle();
	}

	void Settle()
	{
		if (!m_Settled)
		{
			m_Settled = true;
			if (VyshkaPollProbe.s_Instance)
				VyshkaPollProbe.s_Instance.OnStepSettled(m_Step);
		}
	}
}

class VyshkaPollProbe
{
	static ref VyshkaPollProbe s_Instance;

	static const string BASE_URL = "http://127.0.0.1:8099/";
	static const int GAP_MS = 4000;      // quiet time between steps
	static const int TICK_MS = 250;      // budget watchdog resolution

	ref array<ref VyshkaProbeStep> m_Steps;
	ref array<ref VyshkaProbeCallback> m_Callbacks;   // keeps callbacks alive
	RestContext m_Context;
	int m_Current;
	int m_FiredAt;
	bool m_StepDone;

	static void Run()
	{
		s_Instance = new VyshkaPollProbe();
		s_Instance.Start();
	}

	void VyshkaPollProbe()
	{
		m_Current = -1;
		m_FiredAt = 0;
		m_StepDone = false;
		m_Steps = new array<ref VyshkaProbeStep>;
		m_Callbacks = new array<ref VyshkaProbeCallback>;
	}

	void Log(int step, string evt, int t, string extra)
	{
		Print("VYSHKA_PROBE\tstep=" + step + "\tevent=" + evt + "\tt=" + t + "\t" + extra);
	}

	void Start()
	{
		// Phase 1: engine defaults, nothing configured (docs say 10 s read).
		m_Steps.Insert(new VyshkaProbeStep("default/hold-5s",     "hold?ms=5000",             0,   0, 40000));
		m_Steps.Insert(new VyshkaProbeStep("default/hold-9s",     "hold?ms=9000",             0,   0, 40000));
		m_Steps.Insert(new VyshkaProbeStep("default/hold-12s",    "hold?ms=12000",            0,   0, 40000));
		m_Steps.Insert(new VyshkaProbeStep("default/hold-25s",    "hold?ms=25000",            0,   0, 60000));
		m_Steps.Insert(new VyshkaProbeStep("default/headers-25s", "headers?ms=25000",         0,   0, 60000));
		m_Steps.Insert(new VyshkaProbeStep("default/drip-30s-4s", "drip?ms=30000&every=4000", 0,   0, 60000));

		// Phase 2: control. Raising the connection timeout must not buy any
		// extra hold time; if it does, the option ids are not what we think.
		m_Steps.Insert(new VyshkaProbeStep("conn30/hold-25s",     "hold?ms=25000",  VYSHKA_RESTOPT_CONN,  30, 60000));

		// Phase 3: raise the read timeout past the 25 s the spec wants.
		m_Steps.Insert(new VyshkaProbeStep("read30/hold-25s",     "hold?ms=25000",  VYSHKA_RESTOPT_READ,  30, 60000));
		m_Steps.Insert(new VyshkaProbeStep("read30/hold-35s",     "hold?ms=35000",  VYSHKA_RESTOPT_READ,  30, 70000));

		// Phase 4: the documented ceiling, and one value above it to see
		// whether the engine clamps, rejects, or accepts.
		m_Steps.Insert(new VyshkaProbeStep("read120/hold-60s",    "hold?ms=60000",  VYSHKA_RESTOPT_READ, 120, 90000));
		m_Steps.Insert(new VyshkaProbeStep("read200/hold-25s",    "hold?ms=25000",  VYSHKA_RESTOPT_READ, 200, 60000));

		// Phase 5: back down to the floor the spec must honour.
		m_Steps.Insert(new VyshkaProbeStep("read3/hold-5s",       "hold?ms=5000",   VYSHKA_RESTOPT_READ,   3, 40000));

		RestApi api = GetRestApi();
		Log(-1, "boot", 0, "api=" + (api != null) + "\tbase=" + BASE_URL);
		if (!api)
		{
			api = CreateRestApi();
			Log(-1, "boot-created", 0, "api=" + (api != null));
		}
		if (!api)
		{
			Log(-1, "abort", 0, "reason=no-restapi");
			return;
		}
		api.EnableDebug(true);

		m_Context = api.GetRestContext(BASE_URL);
		Log(-1, "context", 0, "ctx=" + (m_Context != null) + "\tcount=" + api.GetContextCount());
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
			Log(-1, "finished", 0, "steps=" + m_Steps.Count());
			return;
		}

		VyshkaProbeStep step = m_Steps.Get(m_Current);
		if (step.m_OptionId > 0)
			GetRestApi().SetOption(step.m_OptionId, step.m_OptionValue);

		m_StepDone = false;
		m_FiredAt = GetGame().GetTime();

		VyshkaProbeCallback cb = new VyshkaProbeCallback(m_Current, m_FiredAt);
		m_Callbacks.Insert(cb);

		// Keep this on one line: the mission script parser rejects a string
		// concatenation broken across lines inside a call argument list.
		Log(m_Current, "fire", 0, "label=" + step.m_Label + "\tpath=" + step.m_Path + "\toption=" + step.m_OptionId + "\tvalue=" + step.m_OptionValue);

		int rc = m_Context.GET(cb, step.m_Path);
		Log(m_Current, "submitted", GetGame().GetTime() - m_FiredAt, "rc=" + rc);

		GetGame().GetCallQueue(CALL_CATEGORY_SYSTEM).CallLater(Watchdog, TICK_MS, true);
	}

	// Moves on once a step settles or blows its budget, so one wedged request
	// cannot stall the rest of the matrix.
	void Watchdog()
	{
		VyshkaProbeStep step = m_Steps.Get(m_Current);
		int elapsed = GetGame().GetTime() - m_FiredAt;

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
