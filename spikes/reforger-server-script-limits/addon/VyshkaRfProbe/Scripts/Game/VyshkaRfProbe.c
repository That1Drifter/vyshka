// Vyshka spike #159: what a Reforger dedicated server's script can do over HTTP, with
// files, and with JSON, measured against the stub in ../../../stub. Every result is one
// tab-separated line starting VYSHKA_RF in the server's console log:
//
//   VYSHKA_RF <tick ms> <test> ms=<elapsed> <result> http=<code> len=<bytes> <data head>
//
// The probe starts on the server once the game mode starts and runs the steps in order.
// Each request step waits for its callback (or a 200 s watchdog) before the next begins.

class VyshkaRfProbe
{
	static ref VyshkaRfProbe s_Instance;
	static const string BASE = "http://127.0.0.1:8099/";
	static const string TLS = "https://vyshka-branch.solvaryx.cloud/";
	static const int WATCHDOG_MS = 200000;
	//! Which step list to run; results/runN-* were taken with RUN = N.
	static const int RUN = 4;

	protected ref array<ref RestCallback> m_Cbs = {};
	protected ref array<string> m_Names = {};
	protected ref array<int> m_Starts = {};
	protected int m_Pending;
	protected int m_Step;
	protected int m_Gen;

	//------------------------------------------------------------------------------------------------
	static void Start()
	{
		if (s_Instance)
			return;
		s_Instance = new VyshkaRfProbe();
		Out("probe.start", 0, "ok", 0, 0, "");
		GetGame().GetCallqueue().CallLater(s_Instance.Next, 3000);
	}

	//------------------------------------------------------------------------------------------------
	static void Out(string test, int ms, string result, int http, int len, string data)
	{
		data.Replace("\n", " ");
		data.Replace("\r", " ");
		data.Replace("\t", " ");
		if (data.Length() > 160)
			data = data.Substring(0, 160);
		Print(string.Format("VYSHKA_RF\t%1\t%2\tms=%3\t%4\thttp=%5\tlen=%6\t%7", System.GetTickCount(), test, ms, result, http, len, data), LogLevel.NORMAL);
	}

	//------------------------------------------------------------------------------------------------
	//! n characters of 'a'. Runs 1 to 3 trimmed with Substring, which run 3 found caps its
	//! result at 8191 characters, so their request-body, file-line, and JSON-blob figures stop
	//! at 8191 (an artifact, kept so those logs stay reproducible). Run 4 concatenates only.
	static string Fill(int n)
	{
		if (RUN < 4)
		{
			string s = "a";
			while (s.Length() < n)
				s += s;
			return s.Substring(0, n);
		}
		string result = "";
		string chunk = "a";
		while (n > 0)
		{
			if (n & 1)
				result += chunk;
			chunk += chunk;
			n = n >> 1;
		}
		return result;
	}

	//------------------------------------------------------------------------------------------------
	protected RestContext Ctx(string base, int timeoutS, string headers, string tag)
	{
		RestApi api = GetGame().GetRestApi();
		int before = api.GetContextCount();
		RestContext ctx = api.GetContext(base);
		int after = api.GetContextCount();
		if (timeoutS >= 0)
			ctx.SetTimeout(timeoutS);
		if (headers != "")
		{
			bool ok = ctx.SetHeaders(headers);
			Out(tag + ".setHeaders", 0, string.Format("returned=%1", ok), 0, headers.Length(), headers);
		}
		Out(tag + ".context", 0, string.Format("contexts %1->%2 timeout=%3", before, after, timeoutS), 0, 0, base);
		return ctx;
	}

	//------------------------------------------------------------------------------------------------
	//! Issue one request in its own slot; the step advances when every slot has answered.
	protected void Issue(string name, string method, RestContext ctx, string path, string body)
	{
		RestCallback cb = new RestCallback();
		cb.SetOnSuccess(OnOk);
		cb.SetOnError(OnErr);
		m_Cbs.Insert(cb);
		m_Names.Insert(name);
		m_Starts.Insert(System.GetTickCount());
		m_Pending++;

		int rc;
		switch (method)
		{
			case "GET": rc = ctx.GET(cb, path); break;
			case "POST": rc = ctx.POST(cb, path, body); break;
			case "PUT": rc = ctx.PUT(cb, path, body); break;
			case "DELETE": rc = ctx.DELETE(cb, path, body); break;
		}
		Out(name + ".issued", 0, string.Format("%1 rc=%2", method, rc), 0, body.Length(), path);
	}

	//------------------------------------------------------------------------------------------------
	protected void Arm()
	{
		m_Gen++;
		GetGame().GetCallqueue().CallLater(Watchdog, WATCHDOG_MS, false, m_Gen);
	}

	//------------------------------------------------------------------------------------------------
	protected void OnOk(RestCallback cb)
	{
		Done(cb, "success");
	}

	//------------------------------------------------------------------------------------------------
	protected void OnErr(RestCallback cb)
	{
		Done(cb, "error");
	}

	//------------------------------------------------------------------------------------------------
	protected void Done(RestCallback cb, string kind)
	{
		int slot = m_Cbs.Find(cb);
		string data = cb.GetData();
		string res = kind + "/" + typename.EnumToString(ERestResult, cb.GetRestResult());
		if (slot < 0)
		{
			Out("late-callback", 0, res, cb.GetHttpCode(), data.Length(), data);
			return;
		}
		Out(m_Names[slot], System.GetTickCount() - m_Starts[slot], res, cb.GetHttpCode(), data.Length(), data);
		m_Pending--;
		if (m_Pending == 0)
			Advance();
	}

	//------------------------------------------------------------------------------------------------
	protected void Watchdog(int gen)
	{
		if (gen != m_Gen || m_Pending == 0)
			return;
		Out("watchdog", WATCHDOG_MS, string.Format("no callback for %1 slot(s)", m_Pending), 0, 0, "");
		Advance();
	}

	//------------------------------------------------------------------------------------------------
	protected void Advance()
	{
		m_Gen++;
		m_Pending = 0;
		m_Cbs.Clear();
		m_Names.Clear();
		m_Starts.Clear();
		GetGame().GetCallqueue().CallLater(Next, 500);
	}

	//------------------------------------------------------------------------------------------------
	protected void One(string name, string method, RestContext ctx, string path, string body)
	{
		Issue(name, method, ctx, path, body);
		Arm();
	}

	//------------------------------------------------------------------------------------------------
	protected void Next()
	{
		m_Step++;
		if (RUN == 1)
			NextRun1();
		else if (RUN == 2)
			NextRun2();
		else if (RUN == 3)
			NextRun3();
		else
			NextRun4();
	}

	//------------------------------------------------------------------------------------------------
	//! Run 4 re-measures, with a Fill that never calls Substring, what runs 1 to 3 cut at 8191.
	protected void NextRun4()
	{
		switch (m_Step)
		{
			case 1: One("req.64k", "POST", Ctx(BASE + "c50/", 60, "", "R"), "echo", Fill(65536)); return;
			case 2: One("req.256k", "POST", Ctx(BASE + "c50/", 60, "", "R"), "echo", Fill(262144)); return;
			case 3: One("req.1m", "POST", Ctx(BASE + "c50/", 60, "", "R"), "echo", Fill(1048576)); return;
			case 4: One("req.4m", "POST", Ctx(BASE + "c50/", 60, "", "R"), "echo", Fill(4194304)); return;
			case 5: One("req.16m", "POST", Ctx(BASE + "c50/", 60, "", "R"), "echo", Fill(16777216)); return;
			case 6:
				Out("string.substring.8192", 0, "Substring(0, 8192) of a 20000-character string", 0, Fill(20000).Substring(0, 8192).Length(), "");
				FileIO.MakeDirectory("$profile:vyshka-probe");
				LongLineWrite("$profile:vyshka-probe/r4-8192.txt", 8192);
				LongLineWrite("$profile:vyshka-probe/r4-65536.txt", 65536);
				LongLineWrite("$profile:vyshka-probe/r4-1m.txt", 1048576);
				JsonBlob(262144);
				JsonBlob(1048576);
				JsonBlob(4194304);
				JsonStrings(2000, 500);
				Advance();
				return;
		}
		Out("probe.done", 0, "ok", 0, m_Step - 1, "");
	}

	//------------------------------------------------------------------------------------------------
	//! A realistic batch: an array of count strings of len characters each.
	protected void JsonStrings(int count, int len)
	{
		string item = "\"" + Fill(len) + "\"";
		string doc = "{\"a\":[" + item;
		for (int i = 1; i < count; i++)
			doc += "," + item;
		doc += "]}";
		int t0 = System.GetTickCount();
		JsonLoadContext ld = new JsonLoadContext();
		bool ok = ld.LoadFromString(doc);
		array<string> a = {};
		bool rd = ld.ReadValue("a", a);
		Out(string.Format("json.parse.strings.%1x%2", count, len), System.GetTickCount() - t0, string.Format("load=%1 read=%2 count=%3 lastLen=%4", ok, rd, a.Count(), a[a.Count() - 1].Length()), 0, doc.Length(), "");
	}

	//------------------------------------------------------------------------------------------------
	//! Run 3 finishes what run 2's crash cut short (step 19, a 10000-character request path,
	//! took the server down), then waits for a game client so the identity hooks can print,
	//! then measures request paths only as far as 2048 characters, so as not to crash it again.
	protected void NextRun3()
	{
		string e4096 = Repeat("é", 4096);
		switch (m_Step)
		{
			case 1:
				Out("string.length.e4096", 0, "Length() of 4096 two-byte characters", 0, e4096.Length(), "");
				One("req.utf8.5000x2", "POST", Ctx(BASE + "c40/", 60, "", "U"), "echo", Repeat("é", 5000));
				return;
			case 2: One("req.mixed.8000a+500e", "POST", Ctx(BASE + "c40/", 60, "", "U"), "echo", Fill(8000) + Repeat("é", 500)); return;
			case 3:
				Issue("q.hold4.local", "GET", Ctx(BASE + "c41/", 30, "", "Q"), "hold?s=4", "");
				Issue("q.tls.healthz", "GET", Ctx(TLS, 30, "", "Q"), "healthz", "");
				Arm();
				return;
			case 4: Run2Local(); Advance(); return;
			case 5:
				Out("probe.await-player", 0, "waiting up to 600 s for a client", 0, 0, "");
				m_WaitStart = System.GetTickCount();
				GetGame().GetCallqueue().CallLater(AwaitPlayer, 2000);
				return;
			case 6: One("req.query.1024", "GET", Ctx(BASE + "c42/", 30, "", "P"), "echo?q=" + Fill(1024), ""); return;
			case 7: One("req.query.2048", "GET", Ctx(BASE + "c42/", 30, "", "P"), "echo?q=" + Fill(2048), ""); return;
		}
		Out("probe.done", 0, "ok", 0, m_Step - 1, "");
	}

	protected int m_WaitStart;

	//------------------------------------------------------------------------------------------------
	protected void AwaitPlayer()
	{
		int n = GetGame().GetPlayerManager().GetPlayerCount();
		int waited = System.GetTickCount() - m_WaitStart;
		if (n > 0 && waited > 20000)
		{
			Out("probe.player-seen", waited, string.Format("players=%1", n), 0, 0, "");
			Advance();
			return;
		}
		if (waited > 600000)
		{
			Out("probe.no-player", waited, "gave up", 0, 0, "");
			Advance();
			return;
		}
		GetGame().GetCallqueue().CallLater(AwaitPlayer, 2000);
	}

	//------------------------------------------------------------------------------------------------
	//! Run 2 narrows what run 1 found: the timeout floor and its idle semantics on fresh
	//! contexts, the response cap between 256 KiB and 1 MiB, the request cut at 8191 bytes,
	//! whether the one-at-a-time queue spans hosts, long file lines, and the JSON writer.
	protected void NextRun2()
	{
		switch (m_Step)
		{
			case 1: One("t.fresh.1.hold3", "GET", Ctx(BASE + "c20/", 1, "", "T"), "hold?s=3", ""); return;
			case 2: One("t.fresh.2.hold4", "GET", Ctx(BASE + "c21/", 2, "", "T"), "hold?s=4", ""); return;
			case 3: One("t.fresh.3.hold5", "GET", Ctx(BASE + "c22/", 3, "", "T"), "hold?s=5", ""); return;
			case 4: One("t.fresh.0.hold20", "GET", Ctx(BASE + "c23/", 0, "", "T"), "hold?s=20", ""); return;
			case 5: One("t.default.hold14", "GET", Ctx(BASE + "c24/", -1, "", "T"), "hold?s=14", ""); return;
			case 6: One("t.default.hold16", "GET", Ctx(BASE + "c25/", -1, "", "T"), "hold?s=16", ""); return;
			case 7: One("t.5.drip12.every1", "GET", Ctx(BASE + "c26/", 5, "", "T"), "drip?s=12&every=1", ""); return;
			case 8: One("t.5.drip21.every7", "GET", Ctx(BASE + "c27/", 5, "", "T"), "drip?s=21&every=7", ""); return;

			case 9: One("resp.384k", "GET", Ctx(BASE + "c28/", 60, "", "S"), "big?n=393216", ""); return;
			case 10: One("resp.512k", "GET", Ctx(BASE + "c28/", 60, "", "S"), "big?n=524288", ""); return;
			case 11: One("resp.512k+1", "GET", Ctx(BASE + "c28/", 60, "", "S"), "big?n=524289", ""); return;
			case 12: One("resp.768k", "GET", Ctx(BASE + "c28/", 60, "", "S"), "big?n=786432", ""); return;
			case 13: One("resp.1m-1", "GET", Ctx(BASE + "c28/", 60, "", "S"), "big?n=1048575", ""); return;

			case 14: One("req.8191", "POST", Ctx(BASE + "c29/", 60, "", "S"), "echo", Fill(8191)); return;
			case 15: One("req.8192", "POST", Ctx(BASE + "c29/", 60, "", "S"), "echo", Fill(8192)); return;
			case 16: One("req.10000", "PUT", Ctx(BASE + "c29/", 60, "", "S"), "echo", Fill(10000)); return;
			case 17: One("req.utf8.4096x2", "POST", Ctx(BASE + "c29/", 60, "", "S"), "echo", Repeat("é", 4096)); return;
			case 18: One("req.utf8.3000x2", "POST", Ctx(BASE + "c29/", 60, "", "S"), "echo", Repeat("é", 3000)); return;
			case 19: One("req.query.10000", "GET", Ctx(BASE + "c29/", 60, "", "S"), "echo?q=" + Fill(10000), ""); return;

			case 20:
				Issue("q.hold4.local", "GET", Ctx(BASE + "c30/", 30, "", "Q"), "hold?s=4", "");
				Issue("q.tls.healthz", "GET", Ctx(TLS, 30, "", "Q"), "healthz", "");
				Arm();
				return;

			case 21: Run2Local(); Advance(); return;
		}
		Out("probe.done", 0, "ok", 0, m_Step - 1, "");
	}

	//------------------------------------------------------------------------------------------------
	protected void Run2Local()
	{
		string dir = "$profile:vyshka-probe";
		FileIO.MakeDirectory(dir);
		LongLineWrite(dir + "/l8191.txt", 8191);
		LongLineWrite(dir + "/l8192.txt", 8192);
		LongLineWrite(dir + "/l65536.txt", 65536);
		LongLineWrite(dir + "/l1m.txt", 1048576);

		string big = Fill(20000);
		JsonSaveContext sv = new JsonSaveContext();
		sv.WriteValue("d", big);
		string outDoc = sv.SaveToString();
		Out("json.save.write20000", 0, string.Format("scriptLen=%1", big.Length()), 0, outDoc.Length(), "");

		string joined = Fill(8000) + Fill(8000);
		Out("string.concat.16000", 0, "ok", 0, joined.Length(), "");
		string sub = joined.Substring(100, 12000);
		Out("string.substring.12000", 0, "ok", 0, sub.Length(), "");
	}

	//------------------------------------------------------------------------------------------------
	protected void LongLineWrite(string path, int n)
	{
		FileHandle fh = FileIO.OpenFile(path, FileMode.WRITE);
		if (!fh)
		{
			Out("file.open", 0, "failed WRITE", 0, 0, path);
			return;
		}
		fh.WriteLine(Fill(n));
		fh.Close();
		fh = FileIO.OpenFile(path, FileMode.READ);
		string back;
		int r = fh.ReadLine(back);
		int len = fh.GetLength();
		fh.Close();
		Out(string.Format("file.line.%1", n), 0, string.Format("readLine=%1 fileLen=%2", r, len), 0, back.Length(), "");
	}

	//------------------------------------------------------------------------------------------------
	static string Repeat(string unit, int count)
	{
		string s = "";
		for (int i = 0; i < count; i++)
			s += unit;
		return s;
	}

	//------------------------------------------------------------------------------------------------
	protected void NextRun1()
	{
		RestContext c;
		switch (m_Step)
		{
			// A. Methods and the request as the stub sees it, default context settings.
			case 1: One("get.echo", "GET", Ctx(BASE + "c1/", -1, "", "A"), "echo", ""); return;
			case 2: One("post.echo", "POST", Ctx(BASE + "c1/", -1, "", "A"), "echo", "{\"a\":1}"); return;
			case 3: One("put.echo", "PUT", Ctx(BASE + "c1/", -1, "", "A"), "echo", "{\"a\":2}"); return;
			case 4: One("delete.echo", "DELETE", Ctx(BASE + "c1/", -1, "", "A"), "echo", "{\"a\":3}"); return;
			case 5: One("get.query", "GET", Ctx(BASE + "c1/", -1, "", "A"), "echo?errors=inline&x=1", ""); return;

			// B. Request headers: which SetHeaders spellings reach the wire.
			case 6: One("hdr.colon", "GET", Ctx(BASE + "c2/", -1, "Authorization: Bearer t1", "B1"), "echo", ""); return;
			case 7: One("hdr.comma", "GET", Ctx(BASE + "c3/", -1, "Authorization,Bearer t2", "B2"), "echo", ""); return;
			case 8: One("hdr.comma2", "POST", Ctx(BASE + "c4/", -1, "Content-Type,application/json,Authorization,Bearer t3", "B3"), "echo", "{}"); return;
			case 9: One("hdr.newline", "GET", Ctx(BASE + "c5/", -1, "Content-Type: application/json\nAuthorization: Bearer t4", "B4"), "echo", ""); return;
			case 10: One("hdr.persist.post", "POST", Ctx(BASE + "c4/", -1, "", "B5"), "echo", "{}"); return;

			// C. Status codes: which callback fires, and whether the code and body are readable.
			case 11: One("status.204", "GET", Ctx(BASE + "c6/", -1, "", "C"), "nocontent", ""); return;
			case 12: One("status.302", "GET", Ctx(BASE + "c6/", -1, "", "C"), "redirect", ""); return;
			case 13: One("status.400", "GET", Ctx(BASE + "c6/", -1, "", "C"), "status?code=400", ""); return;
			case 14: One("status.401", "POST", Ctx(BASE + "c6/", -1, "", "C"), "status?code=401", "{}"); return;
			case 15: One("status.404", "GET", Ctx(BASE + "c6/", -1, "", "C"), "status?code=404", ""); return;
			case 16: One("status.409", "POST", Ctx(BASE + "c6/", -1, "", "C"), "status?code=409", "{}"); return;
			case 17: One("status.429", "POST", Ctx(BASE + "c6/", -1, "", "C"), "status?code=429", "{}"); return;
			case 18: One("status.500", "GET", Ctx(BASE + "c6/", -1, "", "C"), "status?code=500", ""); return;
			case 19: One("status.503", "GET", Ctx(BASE + "c6/", -1, "", "C"), "status?code=503", ""); return;

			// D. Held responses: the default limit, the settable range, and idle versus whole.
			case 20: One("hold.default.5", "GET", Ctx(BASE + "c7/", -1, "", "D"), "hold?s=5", ""); return;
			case 21: One("hold.default.12", "GET", Ctx(BASE + "c7/", -1, "", "D"), "hold?s=12", ""); return;
			case 22: One("hold.default.25", "GET", Ctx(BASE + "c7/", -1, "", "D"), "hold?s=25", ""); return;
			case 23: One("hold.t5.8", "GET", Ctx(BASE + "c8/", 5, "", "D"), "hold?s=8", ""); return;
			case 24: One("hold.t40.30", "GET", Ctx(BASE + "c8/", 40, "", "D"), "hold?s=30", ""); return;
			case 25: One("hold.t40.50", "GET", Ctx(BASE + "c8/", 40, "", "D"), "hold?s=50", ""); return;
			case 26: One("hold.t300.150", "GET", Ctx(BASE + "c8/", 300, "", "D"), "hold?s=150", ""); return;
			case 27: One("drip.t10.20", "GET", Ctx(BASE + "c8/", 10, "", "D"), "drip?s=20", ""); return;
			case 28: One("hold.t1.3", "GET", Ctx(BASE + "c8/", 1, "", "D"), "hold?s=3", ""); return;
			case 29: One("hold.t0.3", "GET", Ctx(BASE + "c8/", 0, "", "D"), "hold?s=3", ""); return;

			// E. Sizes, both directions.
			case 30: One("resp.64k", "GET", Ctx(BASE + "c9/", 60, "", "E"), "big?n=65536", ""); return;
			case 31: One("resp.256k", "GET", Ctx(BASE + "c9/", 60, "", "E"), "big?n=262144", ""); return;
			case 32: One("resp.1m", "GET", Ctx(BASE + "c9/", 60, "", "E"), "big?n=1048576", ""); return;
			case 33: One("resp.4m", "GET", Ctx(BASE + "c9/", 60, "", "E"), "big?n=4194304", ""); return;
			case 34: One("resp.16m", "GET", Ctx(BASE + "c9/", 60, "", "E"), "big?n=16777216", ""); return;
			case 35: One("req.64k", "POST", Ctx(BASE + "c9/", 60, "", "E"), "echo", Fill(65536)); return;
			case 36: One("req.256k", "POST", Ctx(BASE + "c9/", 60, "", "E"), "echo", Fill(262144)); return;
			case 37: One("req.1m", "POST", Ctx(BASE + "c9/", 60, "", "E"), "echo", Fill(1048576)); return;
			case 38: One("req.4m", "POST", Ctx(BASE + "c9/", 60, "", "E"), "echo", Fill(4194304)); return;
			case 39: One("req.16m", "POST", Ctx(BASE + "c9/", 60, "", "E"), "echo", Fill(16777216)); return;

			// F. Two requests in flight: a held poll and a POST, on one context and on two.
			case 40:
				c = Ctx(BASE + "c10/", 30, "", "F1");
				Issue("conc.same.hold6", "GET", c, "hold?s=6", "");
				Issue("conc.same.post", "POST", c, "echo", "{}");
				Arm();
				return;
			case 41:
				Issue("conc.two.hold6", "GET", Ctx(BASE + "c11/", 30, "", "F2"), "hold?s=6", "");
				Issue("conc.two.post", "POST", Ctx(BASE + "c12/", 30, "", "F2"), "echo", "{}");
				Arm();
				return;

			// G. TLS to a Let's Encrypt host, and a refusal's status and body over TLS.
			case 42: One("tls.healthz", "GET", Ctx(TLS, 30, "", "G"), "healthz", ""); return;
			case 43: One("tls.session.401", "POST", Ctx(TLS, 30, "", "G"), "plugin/v1/session", "{}"); return;
			case 44: One("tls.session.inline", "POST", Ctx(TLS, 30, "", "G"), "plugin/v1/session?errors=inline", "{}"); return;

			// H. Transport failures.
			case 45: One("fail.refused", "GET", Ctx("http://127.0.0.1:1/", 10, "", "H"), "x", ""); return;
			case 46: One("fail.dns", "GET", Ctx("http://vyshka-probe.invalid/", 10, "", "H"), "x", ""); return;

			// I. The blocking *_now forms.
			case 47: SyncNow(); return;

			// J. JSON.
			case 48: JsonBlob(262144); JsonBlob(1048576); JsonBlob(4194304); JsonInts(10000); JsonInts(100000); Advance(); return;

			// K. Files under $profile:.
			case 49: Files(); Advance(); return;
		}
		Out("probe.done", 0, "ok", 0, m_Step - 1, "");
	}

	//------------------------------------------------------------------------------------------------
	protected void SyncNow()
	{
		RestContext c = Ctx(BASE + "c13/", 10, "", "I");
		int t0 = System.GetTickCount();
		string r = c.GET_now("hold?s=2");
		Out("now.get.hold2", System.GetTickCount() - t0, "returned", 0, r.Length(), r);
		t0 = System.GetTickCount();
		r = c.GET_now("status?code=404");
		Out("now.get.404", System.GetTickCount() - t0, "returned", 0, r.Length(), r);
		t0 = System.GetTickCount();
		r = c.POST_now("echo", "{\"now\":1}");
		Out("now.post.echo", System.GetTickCount() - t0, "returned", 0, r.Length(), r);
		Advance();
	}

	//------------------------------------------------------------------------------------------------
	protected void JsonBlob(int n)
	{
		string doc = "{\"d\":\"" + Fill(n) + "\"}";
		int t0 = System.GetTickCount();
		JsonLoadContext ld = new JsonLoadContext();
		bool ok = ld.LoadFromString(doc);
		string d;
		bool rd = ld.ReadValue("d", d);
		Out(string.Format("json.parse.blob.%1", n), System.GetTickCount() - t0, string.Format("load=%1 read=%2", ok, rd), 0, d.Length(), "");

		t0 = System.GetTickCount();
		JsonSaveContext sv = new JsonSaveContext();
		sv.WriteValue("d", d);
		string outDoc = sv.SaveToString();
		Out(string.Format("json.save.blob.%1", n), System.GetTickCount() - t0, "ok", 0, outDoc.Length(), outDoc.Substring(0, 12));
	}

	//------------------------------------------------------------------------------------------------
	protected void JsonInts(int n)
	{
		string doc = "{\"a\":[0";
		for (int i = 1; i < n; i++)
			doc += "," + i.ToString();
		doc += "]}";
		int t0 = System.GetTickCount();
		JsonLoadContext ld = new JsonLoadContext();
		bool ok = ld.LoadFromString(doc);
		array<int> a = {};
		bool rd = ld.ReadValue("a", a);
		Out(string.Format("json.parse.ints.%1", n), System.GetTickCount() - t0, string.Format("load=%1 read=%2 last=%3", ok, rd, a[a.Count() - 1]), 0, doc.Length(), "");
	}

	//------------------------------------------------------------------------------------------------
	protected void Files()
	{
		string dir = "$profile:vyshka-probe";
		Out("file.mkdir", 0, string.Format("%1", FileIO.MakeDirectory(dir)), 0, 0, dir);
		LongLine(dir + "/line64k.txt", 65536);
		LongLine(dir + "/line1m.txt", 1048576);

		// Does WRITE truncate an existing file, and does OPENW?
		string p = dir + "/trunc.txt";
		WriteText(p, FileMode.WRITE, "0123456789012345678901234567890123456789");
		WriteText(p, FileMode.WRITE, "short");
		Out("file.write.mode.WRITE", 0, "rewritten with 5 bytes", 0, Size(p), "");
		WriteText(p, FileMode.OPENW, "0123456789012345678901234567890123456789");
		WriteText(p, FileMode.OPENW, "short");
		Out("file.write.mode.OPENW", 0, "rewritten with 5 bytes", 0, Size(p), "");

		// A rename, built from what FileIO offers: copy, then delete.
		string q = dir + "/copy.txt";
		bool cp = FileIO.CopyFile(p, q);
		bool del = FileIO.DeleteFile(p);
		Out("file.copy-delete", 0, string.Format("copy=%1 delete=%2 srcExists=%3 dstExists=%4", cp, del, FileIO.FileExists(p), FileIO.FileExists(q)), 0, Size(q), "");

		// Writing outside the sandbox roots.
		FileHandle fh = FileIO.OpenFile("vyshka-probe-outside.txt", FileMode.WRITE);
		Out("file.outside-sandbox", 0, string.Format("opened=%1", fh != null), 0, 0, "");
		if (fh)
			fh.Close();
	}

	//------------------------------------------------------------------------------------------------
	protected void LongLine(string path, int n)
	{
		WriteText(path, FileMode.OPENW, Fill(n));
		FileHandle fh = FileIO.OpenFile(path, FileMode.READ);
		if (!fh)
		{
			Out("file.line", 0, "open-read-failed", 0, n, path);
			return;
		}
		string back;
		int r = fh.ReadLine(back);
		int len = fh.GetLength();
		fh.Close();
		Out(string.Format("file.line.%1", n), 0, string.Format("readLine=%1 fileLen=%2", r, len), 0, back.Length(), "");
	}

	//------------------------------------------------------------------------------------------------
	protected void WriteText(string path, FileMode mode, string text)
	{
		FileHandle fh = FileIO.OpenFile(path, mode);
		if (!fh)
		{
			Out("file.open", 0, "failed " + typename.EnumToString(FileMode, mode), 0, 0, path);
			return;
		}
		fh.WriteLine(text);
		fh.Close();
	}

	//------------------------------------------------------------------------------------------------
	protected int Size(string path)
	{
		FileHandle fh = FileIO.OpenFile(path, FileMode.READ);
		if (!fh)
			return -1;
		int len = fh.GetLength();
		fh.Close();
		return len;
	}
}

//------------------------------------------------------------------------------------------------
//! Lifecycle and identity, printed as they happen.
modded class SCR_BaseGameMode
{
	protected override void OnGameStart()
	{
		super.OnGameStart();
		VyshkaRfProbe.Out("hook.OnGameStart", 0, string.Format("isServer=%1 rplMode=%2", Replication.IsServer(), typename.EnumToString(RplMode, RplSession.Mode())), 0, 0, "");
		if (Replication.IsServer())
			VyshkaRfProbe.Start();
	}

	override void OnPlayerAuditSuccess(int iPlayerID)
	{
		super.OnPlayerAuditSuccess(iPlayerID);
		BackendApi api = GetGame().GetBackendApi();
		string identity = string.Format("%1", SCR_PlayerIdentityUtils.GetPlayerIdentityId(iPlayerID));
		string legacy = api.GetPlayerIdentityId(iPlayerID);
		string kind = typename.EnumToString(PlatformKind, api.GetPlayerPlatformKind(iPlayerID));
		string platformId = api.GetPlayerPlatformId(iPlayerID);
		VyshkaRfProbe.Out("hook.OnPlayerAuditSuccess", 0, string.Format("player=%1 identity=%2 legacy=%3 kind=%4 platformId=%5", iPlayerID, identity, legacy, kind, platformId), 0, 0, GetGame().GetPlayerManager().GetPlayerName(iPlayerID));
	}

	override void OnPlayerAuditFail(int iPlayerID)
	{
		super.OnPlayerAuditFail(iPlayerID);
		VyshkaRfProbe.Out("hook.OnPlayerAuditFail", 0, string.Format("player=%1 identity=%2", iPlayerID, GetGame().GetBackendApi().GetPlayerIdentityId(iPlayerID)), 0, 0, GetGame().GetPlayerManager().GetPlayerName(iPlayerID));
	}

	override void OnPlayerAuditTimeouted(int iPlayerID)
	{
		super.OnPlayerAuditTimeouted(iPlayerID);
		VyshkaRfProbe.Out("hook.OnPlayerAuditTimeouted", 0, string.Format("player=%1", iPlayerID), 0, 0, "");
	}

	override void OnPlayerConnected(int playerId)
	{
		super.OnPlayerConnected(playerId);
		VyshkaRfProbe.Out("hook.OnPlayerConnected", 0, string.Format("player=%1 identityNow=%2", playerId, GetGame().GetBackendApi().GetPlayerIdentityId(playerId)), 0, 0, GetGame().GetPlayerManager().GetPlayerName(playerId));
	}

	protected override void OnPlayerRegistered(int playerId)
	{
		super.OnPlayerRegistered(playerId);
		VyshkaRfProbe.Out("hook.OnPlayerRegistered", 0, string.Format("player=%1", playerId), 0, 0, GetGame().GetPlayerManager().GetPlayerName(playerId));
	}

	protected override void OnPlayerDisconnected(int playerId, KickCauseCode cause, int timeout)
	{
		VyshkaRfProbe.Out("hook.OnPlayerDisconnected", 0, string.Format("player=%1 timeout=%2", playerId, timeout), 0, 0, "");
		super.OnPlayerDisconnected(playerId, cause, timeout);
	}

	override void OnGameEnd()
	{
		VyshkaRfProbe.Out("hook.OnGameEnd", 0, "", 0, 0, "");
		super.OnGameEnd();
	}
}
