// Vyshka Reforger plugin: HTTP through the engine's RestApi.
//
// The engine offers callback-based async HTTP: a RestContext per base URL,
// headers set on the context, and a RestCallback whose success and error
// functions receive the callback itself, from which the status code and the
// body are read. What this file relies on was measured on Reforger 1.9
// (spikes/reforger-server-script-limits/results/findings.md):
//
//   - SetHeaders("Name,value,Name,value") sends real headers, and a second
//     call replaces the first set, so every request sets the whole set.
//     Without a Content-Type of application/json the hub answers 415.
//   - SetTimeout(s) is an idle timeout on the context, honored from 3 s up;
//     1, 2 and 0 are ignored. The default is 15 s, below a held poll.
//   - A non-2xx reaches the error function with its status and body
//     readable. The plugin asks for its refusals inline all the same (spec
//     section 2.3), so the protocol logic is the same as on an engine that
//     cannot read them, and an ordinary status is what a legacy hub or a
//     proxy produces.
//   - Responses of 1 MiB or more fail on the client; the protocol bounds
//     every Plugin API answer at 512 KiB (spec section 2.1).
//
// One request is in flight at a time per transport. Every request carries a
// generation number, and a callback from a superseded generation is ignored,
// so a response that arrives after the watchdog gave up cannot corrupt state.

// VyshkaResponseSink is what a transport reports to: the owner of the
// request, told the kind it gave the request, whether the engine reported
// success, the error code otherwise, and the body of a success.
class VyshkaResponseSink
{
	void OnResponse(int kind, bool ok, int code, string data)
	{
	}
}

// VyshkaRequestCallback carries one request's generation to the engine's
// callback, so a late answer to an abandoned request is recognized.
class VyshkaRequestCallback
{
	VyshkaTransport m_Transport;
	int m_Generation;
	ref RestCallback m_Rest;

	void VyshkaRequestCallback(VyshkaTransport transport, int generation)
	{
		m_Transport = transport;
		m_Generation = generation;
		m_Rest = new RestCallback();
		m_Rest.SetOnSuccess(OnOk);
		m_Rest.SetOnError(OnFailed);
	}

	void OnOk(RestCallback cb)
	{
		if (m_Transport)
			m_Transport.Complete(m_Generation, true, 0, cb.GetData());
	}

	void OnFailed(RestCallback cb)
	{
		if (!m_Transport)
			return;
		int http = cb.GetHttpCode();
		int code = VyshkaTransport.ERROR_UNREACHABLE;
		if (cb.GetRestResult() == ERestResult.EREST_ERROR_TIMEOUT)
			code = VyshkaTransport.ERROR_TIMEOUT;
		else if (http >= 500)
			code = VyshkaTransport.ERROR_SERVER;
		else if (http >= 400)
			code = VyshkaTransport.ERROR_CLIENT;
		else if (http >= 300)
			m_Transport.WarnRedirect(http);
		else if (http >= 200 && http < 300)
			code = VyshkaTransport.ERROR_UNEXPECTED;
		m_Transport.Complete(m_Generation, false, code, "");
	}
}

class VyshkaTransport
{
	// Error codes the link reasons from, in the DayZ plugin's numbering so
	// the protocol code is shared.
	static const int ERROR_CLIENT = 5;        // the hub answered 4xx
	static const int ERROR_SERVER = 6;        // the hub answered 5xx
	static const int ERROR_UNREACHABLE = 7;   // the request never completed
	static const int ERROR_TIMEOUT = 8;       // the context's idle timeout hit
	static const int ERROR_WATCHDOG = 100;    // no callback within the plugin's own budget
	static const int ERROR_UNEXPECTED = 101;  // an error callback carrying a success status

	RestContext m_Context;
	VyshkaResponseSink m_Sink;
	ref VyshkaRequestCallback m_Callback;
	int m_Generation;
	bool m_InFlight;
	int m_Kind;
	int m_StartedMs;
	int m_BudgetMs;
	string m_Path;
	int m_TimeoutSeconds;

	// Init opens the transport's context at baseUrl, which every request path
	// is relative to. The engine keys contexts by the base URL string, so two
	// transports with the same one would share a context.
	bool Init(string baseUrl, VyshkaResponseSink sink)
	{
		m_Sink = sink;
		RestApi api = GetGame().GetRestApi();
		if (!api)
		{
			VyshkaLog.Error("the engine's RestApi is unavailable; the plugin cannot talk to a hub");
			return false;
		}
		m_Context = api.GetContext(baseUrl);
		if (!m_Context)
		{
			VyshkaLog.Error("could not create a RestContext for " + baseUrl);
			return false;
		}
		SetReadTimeout(30);
		return true;
	}

	// SetReadTimeout sets the context's idle timeout. Values under 3 are
	// ignored by the engine, so they are raised to 3 here.
	void SetReadTimeout(int seconds)
	{
		if (seconds < 3)
			seconds = 3;
		if (seconds > 120)
			seconds = 120;
		m_TimeoutSeconds = seconds;
		if (m_Context)
			m_Context.SetTimeout(seconds);
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

		string headers = "Content-Type,application/json";
		if (bearer != "")
			headers = "Authorization,Bearer " + bearer + "," + headers;
		if (!m_Context.SetHeaders(headers))
			VyshkaLog.Warn("the engine refused the request headers");

		m_Generation++;
		m_Kind = kind;
		m_Path = path;
		m_StartedMs = VyshkaClock.MonotonicMs();
		m_BudgetMs = budgetMs;
		m_InFlight = true;
		m_Callback = new VyshkaRequestCallback(this, m_Generation);

		m_Context.POST(m_Callback.m_Rest, path, body);
		return true;
	}

	// Complete is the callback's entry point. Stale generations are dropped.
	void Complete(int generation, bool ok, int code, string data)
	{
		if (!m_InFlight || generation != m_Generation)
			return;
		m_InFlight = false;
		m_Callback = null;
		if (m_Sink)
			m_Sink.OnResponse(m_Kind, ok, code, data);
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
		if (m_Sink)
			m_Sink.OnResponse(m_Kind, false, ERROR_WATCHDOG, "");
	}

	// WarnRedirect says, at most once a minute, that the hub URL redirects.
	// The engine does not follow redirects (spikes/reforger-server-script-
	// limits, status.302), so an http:// hub URL behind a proxy that
	// redirects to https:// reads as unreachable without it.
	int m_LastRedirectWarnMs;

	void WarnRedirect(int http)
	{
		int now = VyshkaClock.MonotonicMs();
		if (m_LastRedirectWarnMs != 0 && now - m_LastRedirectWarnMs < 60000)
			return;
		m_LastRedirectWarnMs = now;
		VyshkaLog.Warn("the hub answered with redirect " + http.ToString() + ", which the engine does not follow; set hubUrl to the address it redirects to");
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
