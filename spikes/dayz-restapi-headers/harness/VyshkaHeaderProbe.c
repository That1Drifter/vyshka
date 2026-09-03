// Vyshka spike: DayZ RestApi request shape probe.
//
// Paste this into a mission init.c and call VyshkaHeaderProbe.Run() from main().
// It fires a fixed matrix of requests at the spike stub and prints one machine
// readable line per event into the server script log. The stub logs what
// reached the socket, so the two logs together answer:
//
//   1. Can script send an Authorization header at all? RestContext exposes
//      only SetHeader(contentType); the probe tries smuggling a second header
//      through a CRLF or LF in that value, and a value that is a whole header.
//   2. Does a 4xx or 5xx response reach OnSuccess with its body, or OnError
//      with nothing? The plugin must read the protocol's error code.
//   3. How large a body can be posted, and how large a response reaches
//      script intact?
//   4. What error code a refused connection produces.
//
// Line format (tab separated):
//   VYSHKA_HPROBE<TAB>step=<n><TAB>event=<name><TAB>t=<ms since fire><TAB>...

class VyshkaHeaderStep
{
	string m_Label;
	string m_Method;     // "GET" or "POST"
	string m_Path;
	string m_Header;     // value passed to SetHeader before firing; "" leaves it
	string m_Body;
	string m_BaseURL;    // "" means the shared stub context
	int m_BudgetMs;

	void VyshkaHeaderStep(string label, string method, string path, string header, string body, string baseURL, int budgetMs)
	{
		m_Label = label;
		m_Method = method;
		m_Path = path;
		m_Header = header;
		m_Body = body;
		m_BaseURL = baseURL;
		m_BudgetMs = budgetMs;
	}
}

class VyshkaHeaderCallback : RestCallback
{
	int m_Step;
	int m_FiredAt;
	bool m_Settled;

	void VyshkaHeaderCallback(int step, int firedAt)
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
		Print("VYSHKA_HPROBE\tstep=" + m_Step + "\tevent=" + evt + "\tt=" + Elapsed() + "\tsettled=" + m_Settled + "\t" + extra);
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
		string head = data;
		if (head.Length() > 160)
			head = head.Substring(0, 160);
		Emit("success", "size=" + dataSize + "\tlen=" + data.Length() + "\tdata=" + head);
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
			if (VyshkaHeaderProbe.s_Instance)
				VyshkaHeaderProbe.s_Instance.OnStepSettled(m_Step);
		}
	}
}

class VyshkaHeaderProbe
{
	static ref VyshkaHeaderProbe s_Instance;

	static const string BASE_URL = "http://127.0.0.1:8098/";
	static const string DEAD_URL = "http://127.0.0.1:8097/";
	static const int GAP_MS = 3000;
	static const int TICK_MS = 250;

	ref array<ref VyshkaHeaderStep> m_Steps;
	ref array<ref VyshkaHeaderCallback> m_Callbacks;
	RestContext m_Context;
	RestContext m_DeadContext;
	int m_Current;
	int m_FiredAt;
	bool m_StepDone;

	static void Run()
	{
		s_Instance = new VyshkaHeaderProbe();
		s_Instance.Start();
	}

	void VyshkaHeaderProbe()
	{
		m_Current = -1;
		m_FiredAt = 0;
		m_StepDone = false;
		m_Steps = new array<ref VyshkaHeaderStep>;
		m_Callbacks = new array<ref VyshkaHeaderCallback>;
	}

	void Log(int step, string evt, int t, string extra)
	{
		Print("VYSHKA_HPROBE\tstep=" + step + "\tevent=" + evt + "\tt=" + t + "\t" + extra);
	}

	static string Repeat(string unit, int times)
	{
		string result = "";
		for (int i = 0; i < times; i++)
			result += unit;
		return result;
	}

	void Start()
	{
		int cr = 13;
		int lf = 10;
		string CR = cr.AsciiToString();
		string LF = lf.AsciiToString();
		string JSON = "application/json";

		// Phase 1: baseline, then three ways of smuggling a second header
		// through the one header the API exposes.
		m_Steps.Insert(new VyshkaHeaderStep("baseline-post",   "POST", "post", JSON, "{\"probe\":0}", "", 15000));
		m_Steps.Insert(new VyshkaHeaderStep("crlf-auth",       "POST", "post", JSON + CR + LF + "Authorization: Bearer crlf-token", "{\"probe\":1}", "", 15000));
		m_Steps.Insert(new VyshkaHeaderStep("lf-auth",         "POST", "post", JSON + LF + "Authorization: Bearer lf-token", "{\"probe\":2}", "", 15000));
		m_Steps.Insert(new VyshkaHeaderStep("whole-header",    "POST", "post", "Authorization: Bearer whole-token", "{\"probe\":3}", "", 15000));
		m_Steps.Insert(new VyshkaHeaderStep("restore-json",    "POST", "post", JSON, "{\"probe\":4}", "", 15000));

		// Phase 2: does an error status reach script with its body?
		m_Steps.Insert(new VyshkaHeaderStep("status-401",      "GET", "status?code=401", "", "", "", 15000));
		m_Steps.Insert(new VyshkaHeaderStep("status-400",      "GET", "status?code=400", "", "", "", 15000));
		m_Steps.Insert(new VyshkaHeaderStep("status-409",      "GET", "status?code=409", "", "", "", 15000));
		m_Steps.Insert(new VyshkaHeaderStep("status-500",      "GET", "status?code=500", "", "", "", 15000));
		m_Steps.Insert(new VyshkaHeaderStep("post-status-401", "POST", "status?code=401", "", "{\"probe\":9}", "", 15000));

		// Phase 3: sizes. A 64 KiB post covers the largest event batch a
		// plugin should send; responses are probed at 256 KiB and 1 MiB.
		m_Steps.Insert(new VyshkaHeaderStep("post-4k-echo",    "POST", "echo", "", "{\"pad\":\"" + Repeat("x", 4000) + "\"}", "", 15000));
		m_Steps.Insert(new VyshkaHeaderStep("post-64k",        "POST", "post", "", "{\"pad\":\"" + Repeat("xxxxxxxxxxxxxxxx", 4096) + "\"}", "", 15000));
		m_Steps.Insert(new VyshkaHeaderStep("get-256k",        "GET", "big?n=262144", "", "", "", 15000));
		m_Steps.Insert(new VyshkaHeaderStep("get-1m",          "GET", "big?n=1048576", "", "", "", 20000));

		// Phase 4: non-ASCII response text and a refused connection.
		m_Steps.Insert(new VyshkaHeaderStep("utf8-response",   "GET", "utf8", "", "", "", 15000));
		m_Steps.Insert(new VyshkaHeaderStep("refused",         "POST", "post", "", "{\"probe\":15}", DEAD_URL, 30000));
		m_Steps.Insert(new VyshkaHeaderStep("after-refused",   "POST", "post", "", "{\"probe\":16}", "", 15000));

		RestApi api = GetRestApi();
		Log(-1, "boot", 0, "api=" + (api != null));
		if (!api)
			api = CreateRestApi();
		if (!api)
		{
			Log(-1, "abort", 0, "reason=no-restapi");
			return;
		}

		m_Context = api.GetRestContext(BASE_URL);
		m_DeadContext = api.GetRestContext(DEAD_URL);
		Log(-1, "context", 0, "ctx=" + (m_Context != null) + "\tdead=" + (m_DeadContext != null));
		if (!m_Context || !m_DeadContext)
		{
			Log(-1, "abort", 0, "reason=no-context");
			return;
		}
		m_Context.SetHeader("application/json");
		m_DeadContext.SetHeader("application/json");

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

		VyshkaHeaderStep step = m_Steps.Get(m_Current);
		RestContext ctx = m_Context;
		if (step.m_BaseURL != "")
			ctx = m_DeadContext;
		if (step.m_Header != "")
			ctx.SetHeader(step.m_Header);

		m_StepDone = false;
		m_FiredAt = GetGame().GetTime();

		VyshkaHeaderCallback cb = new VyshkaHeaderCallback(m_Current, m_FiredAt);
		m_Callbacks.Insert(cb);

		Log(m_Current, "fire", 0, "label=" + step.m_Label + "\tmethod=" + step.m_Method + "\tpath=" + step.m_Path + "\tbodyLen=" + step.m_Body.Length());

		int rc;
		if (step.m_Method == "POST")
			rc = ctx.POST(cb, step.m_Path, step.m_Body);
		else
			rc = ctx.GET(cb, step.m_Path);
		Log(m_Current, "submitted", GetGame().GetTime() - m_FiredAt, "rc=" + rc);

		GetGame().GetCallQueue(CALL_CATEGORY_SYSTEM).CallLater(Watchdog, TICK_MS, true);
	}

	void Watchdog()
	{
		VyshkaHeaderStep step = m_Steps.Get(m_Current);
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
