// Vyshka DayZ plugin: HTTP through the engine's RestApi.
//
// The engine offers callback-based async HTTP and nothing else: one
// content-type header, a POST body, and a callback that receives either the
// response body of a 2xx or an error code. Two measured facts shape this
// file (spikes/dayz-restapi-headers/results/findings.md):
//
//   - SetHeader's value goes to the wire verbatim after "Content-Type: ", so
//     a value carrying CRLF and "Authorization: Bearer ..." arrives as two
//     headers. That is how the bearer token of spec section 2.1 travels.
//   - A 4xx or 5xx never delivers its body. Script sees only an error code:
//     5 for a client error, 6 for a server error, 7 when the request never
//     completed (connection refused), 8 on timeout. The protocol's error
//     codes are invisible, so the plugin reasons from these four instead.
//
// One request is in flight at a time. Every request carries a generation
// number, and a callback from a superseded generation is ignored, so a
// response that arrives after the watchdog gave up cannot corrupt state.

class VyshkaRequestCallback : RestCallback
{
	VyshkaTransport m_Transport;
	int m_Generation;

	void VyshkaRequestCallback(VyshkaTransport transport, int generation)
	{
		m_Transport = transport;
		m_Generation = generation;
	}

	override void OnSuccess(string data, int dataSize)
	{
		if (m_Transport)
			m_Transport.Complete(m_Generation, true, 0, data);
	}

	override void OnError(int errorCode)
	{
		if (m_Transport)
			m_Transport.Complete(m_Generation, false, errorCode, "");
	}

	override void OnTimeout()
	{
		if (m_Transport)
			m_Transport.Complete(m_Generation, false, VyshkaTransport.ERROR_TIMEOUT, "");
	}

	override void OnFileCreated(string fileName, int dataSize)
	{
		if (m_Transport)
			m_Transport.Complete(m_Generation, false, VyshkaTransport.ERROR_UNEXPECTED, "");
	}
}

class VyshkaTransport
{
	// RestApi.SetOption ids, by declaration order in the engine's
	// ERestOption enum; the enum names do not always resolve from script.
	static const int OPTION_READ_TIMEOUT = 1;
	static const int OPTION_CONNECT_TIMEOUT = 2;

	// Runtime error codes as measured, not as the script enum orders them.
	static const int ERROR_CLIENT = 5;        // the hub answered 4xx
	static const int ERROR_SERVER = 6;        // the hub answered 5xx
	static const int ERROR_UNREACHABLE = 7;   // the request never completed
	static const int ERROR_TIMEOUT = 8;       // the engine's read timeout hit
	static const int ERROR_WATCHDOG = 100;    // no callback within the plugin's own budget
	static const int ERROR_UNEXPECTED = 101;  // a callback the plugin never asked for

	RestContext m_Context;
	VyshkaPlugin m_Plugin;
	ref VyshkaRequestCallback m_Callback;
	int m_Generation;
	bool m_InFlight;
	int m_Kind;
	int m_StartedMs;
	int m_BudgetMs;
	string m_Path;

	bool Init(string hubUrl, VyshkaPlugin plugin)
	{
		m_Plugin = plugin;
		RestApi api = GetRestApi();
		if (!api)
			api = CreateRestApi();
		if (!api)
		{
			VyshkaLog.Error("the engine's RestApi is unavailable; the plugin cannot talk to a hub");
			return false;
		}
		m_Context = api.GetRestContext(hubUrl + "/plugin/v1/");
		if (!m_Context)
		{
			VyshkaLog.Error("could not create a RestContext for " + hubUrl);
			return false;
		}
		api.SetOption(OPTION_CONNECT_TIMEOUT, 10);
		return true;
	}

	// SetReadTimeout raises the engine's whole-response budget. The option
	// is process-wide and takes effect on the next request (measured in
	// spikes/dayz-restapi-poll-timeout).
	void SetReadTimeout(int seconds)
	{
		if (seconds < 3)
			seconds = 3;
		if (seconds > 120)
			seconds = 120;
		RestApi api = GetRestApi();
		if (api)
			api.SetOption(OPTION_READ_TIMEOUT, seconds);
	}

	bool IsInFlight()
	{
		return m_InFlight;
	}

	// Post sends one JSON request. bearer, when non-empty, becomes the
	// Authorization header. budgetMs bounds how long the plugin waits for a
	// callback before declaring the request dead.
	bool Post(int kind, string path, string bearer, string body, int budgetMs)
	{
		if (m_InFlight || !m_Context)
			return false;

		int cr = 13;
		int lf = 10;
		string header = "application/json";
		if (bearer != "")
			header += cr.AsciiToString() + lf.AsciiToString() + "Authorization: Bearer " + bearer;
		m_Context.SetHeader(header);

		m_Generation++;
		m_Kind = kind;
		m_Path = path;
		m_StartedMs = VyshkaClock.MonotonicMs();
		m_BudgetMs = budgetMs;
		m_InFlight = true;
		m_Callback = new VyshkaRequestCallback(this, m_Generation);

		int rc = m_Context.POST(m_Callback, path, body);
		if (rc != 1)
			VyshkaLog.Warn("POST " + path + " was submitted with result state " + rc.ToString());
		return true;
	}

	// Complete is the callback's entry point. Stale generations are dropped.
	void Complete(int generation, bool ok, int code, string data)
	{
		if (!m_InFlight || generation != m_Generation)
			return;
		m_InFlight = false;
		m_Callback = null;
		if (m_Plugin)
			m_Plugin.OnResponse(m_Kind, ok, code, data);
	}

	// CheckWatchdog fails a request whose callback never came. Bumping the
	// generation means a callback arriving afterwards is ignored.
	void CheckWatchdog()
	{
		if (!m_InFlight)
			return;
		if (VyshkaClock.MonotonicMs() - m_StartedMs < m_BudgetMs)
			return;
		VyshkaLog.Warn("POST " + m_Path + " produced no callback within " + m_BudgetMs.ToString() + " ms; treating it as failed");
		m_Generation++;
		m_InFlight = false;
		m_Callback = null;
		if (m_Plugin)
			m_Plugin.OnResponse(m_Kind, false, ERROR_WATCHDOG, "");
	}

	static string DescribeError(int code)
	{
		if (code == ERROR_CLIENT)
			return "client error (4xx)";
		if (code == ERROR_SERVER)
			return "server error (5xx)";
		if (code == ERROR_UNREACHABLE)
			return "hub unreachable";
		if (code == ERROR_TIMEOUT)
			return "timeout";
		if (code == ERROR_WATCHDOG)
			return "no callback";
		return "error " + code.ToString();
	}
}
