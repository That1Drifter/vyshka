// Vyshka DayZ plugin: the key/value store client (spec section 12).
//
// The store is the hub's, installation-wide, addressed as {namespace}/{key}
// and confined to the namespaces the manifest declares (section 6.6). This
// client speaks the Plugin API side of it: get, set with an optional
// compare-and-swap, and delete, through their POST spellings (section 12.2),
// because the engine's HTTP client can issue only GET and POST and carries
// the bearer credential on a POST alone (the spike behind
// VyshkaTransport: a GET reaches the wire with no content type and so with
// no smuggled Authorization line).
//
// Requests are queued and sent one at a time on a transport of the client's
// own, so a store call does not wait behind the held poll of the link. Each
// request is retried on a transport failure or a hub-side error until its
// own deadline, waits for a fresh session when the hub refuses the session
// token, and otherwise reports what the hub said. The callback runs on the
// script thread like everything else, once, with a result that says which
// of the four things happened: the key was read (found or absent), written,
// refused with a revision mismatch, or the operation failed.

class VyshkaStoreResult
{
	bool m_Ok;                   // the operation did what was asked (an absent key on get or delete is still ok)
	bool m_Found;                // get: the key exists; set: always true
	bool m_Mismatch;             // set: ifRevision did not match; m_Revision is the current revision (0: no key)
	ref VyshkaJsonValue m_Value; // get: the stored value, or null when absent
	int m_Revision;              // get and set: the key's revision (0 when absent)
	string m_Error;              // when !m_Ok

	static VyshkaStoreResult Failure(string error)
	{
		VyshkaStoreResult result = new VyshkaStoreResult();
		result.m_Ok = false;
		result.m_Error = error;
		return result;
	}
}

// VyshkaStoreCallback is subclassed by whoever asked; OnStore runs once per
// request, whether it ended well or not.
class VyshkaStoreCallback
{
	void OnStore(VyshkaStoreResult result)
	{
	}
}

class VyshkaStoreRequest
{
	int m_Op;
	string m_Namespace;
	string m_Key;
	string m_Body;               // set: the JSON body; "" otherwise
	ref VyshkaStoreCallback m_Callback;
	int m_QueuedMs;              // monotonic time of the enqueue; the deadline counts from here
	int m_NotBeforeMs;           // a retry waits until then
	int m_Attempts;
	string m_RefusedToken;       // the session token the hub called invalid; the request waits for another
}

class VyshkaStore : VyshkaResponseSink
{
	static const int OP_GET = 1;
	static const int OP_SET = 2;
	static const int OP_DELETE = 3;

	static const int REQUEST_BUDGET_MS = 15000;   // the watchdog, per attempt
	static const int DEADLINE_MS = 60000;         // per request, across attempts
	static const int RETRY_MIN_MS = 1000;
	static const int RETRY_MAX_MS = 10000;
	static const int QUEUE_CAPACITY = 64;         // requests waiting; more is a plugin bug, not a burst

	ref VyshkaTransport m_Transport;
	ref array<ref VyshkaStoreRequest> m_Queue;
	ref VyshkaStoreRequest m_Current;
	ref array<string> m_Namespaces;   // what the manifest declares; anything else is refused here
	int m_Completed;
	int m_Failed;

	// Init opens the client's own transport at the kv path of the Plugin
	// API; namespaces are those the manifest declares (VyshkaActionRegistry).
	bool Init(string hubUrl, array<string> namespaces)
	{
		m_Queue = new array<ref VyshkaStoreRequest>;
		m_Namespaces = new array<string>;
		for (int i = 0; i < namespaces.Count(); i++)
			m_Namespaces.Insert(namespaces.Get(i));
		m_Transport = new VyshkaTransport();
		return m_Transport.Init(hubUrl + "/plugin/v1/kv/", this);
	}

	// Get reads one key. The result's m_Found says whether it exists;
	// m_Value and m_Revision are set when it does.
	void Get(string namespace, string key, VyshkaStoreCallback callback)
	{
		Enqueue(OP_GET, namespace, key, "", callback);
	}

	// Set writes one key. ifRevision below 0 is an unconditional write; 0
	// means only if the key does not exist; n >= 1 only if its revision is
	// exactly n (section 12.2). A mismatch comes back as m_Mismatch with the
	// current revision, not as a failure.
	void Set(string namespace, string key, VyshkaJsonValue value, int ifRevision, VyshkaStoreCallback callback)
	{
		VyshkaJsonValue body = VyshkaJsonValue.NewObject();
		body.Set("value", value);
		if (ifRevision >= 0)
			body.Set("ifRevision", VyshkaJsonValue.NewInt(ifRevision));
		Enqueue(OP_SET, namespace, key, body.Serialize(), callback);
	}

	// Delete removes one key; a key already gone is success.
	void Delete(string namespace, string key, VyshkaStoreCallback callback)
	{
		Enqueue(OP_DELETE, namespace, key, "", callback);
	}

	int Pending()
	{
		int count = m_Queue.Count();
		if (m_Current)
			count++;
		return count;
	}

	void Enqueue(int op, string namespace, string key, string body, VyshkaStoreCallback callback)
	{
		if (!callback)
			return;
		// The hub would refuse these too (section 12.3); refusing here saves
		// the round trip and names the cause.
		if (m_Namespaces.Find(namespace) < 0)
		{
			Finish(callback, VyshkaStoreResult.Failure("namespace " + namespace + " is not declared in the plugin's manifest"));
			return;
		}
		if (!ValidName(namespace, 64) || !ValidName(key, 128))
		{
			Finish(callback, VyshkaStoreResult.Failure("the key " + namespace + "/" + key + " is not a valid store name"));
			return;
		}
		if (m_Queue.Count() >= QUEUE_CAPACITY)
		{
			Finish(callback, VyshkaStoreResult.Failure("the store client has " + QUEUE_CAPACITY.ToString() + " requests waiting already"));
			return;
		}
		VyshkaStoreRequest request = new VyshkaStoreRequest();
		request.m_Op = op;
		request.m_Namespace = namespace;
		request.m_Key = key;
		request.m_Body = body;
		request.m_Callback = callback;
		request.m_QueuedMs = VyshkaClock.MonotonicMs();
		request.m_NotBeforeMs = 0;
		request.m_Attempts = 0;
		request.m_RefusedToken = "";
		m_Queue.Insert(request);
	}

	// ValidName is the section 12.1 grammar: dot-separated segments of
	// letters, digits, _, and -, within the length cap.
	static bool ValidName(string name, int maxLength)
	{
		int length = name.Length();
		if (length == 0 || length > maxLength)
			return false;
		int segment = 0;
		for (int i = 0; i < length; i++)
		{
			string c = name.Get(i);
			if (c == ".")
			{
				if (segment == 0)
					return false;
				segment = 0;
				continue;
			}
			if (!IsNameChar(c))
				return false;
			segment++;
		}
		return segment > 0;
	}

	static bool IsNameChar(string c)
	{
		if (c == "_" || c == "-")
			return true;
		int code = c.ToAscii();
		return (code >= 48 && code <= 57) || (code >= 65 && code <= 90) || (code >= 97 && code <= 122);
	}

	// Tick runs from the plugin's tick: the watchdog, then the next request
	// when the transport is idle, the link has a session, and the request is
	// not waiting out a retry delay or a refused token.
	void Tick(string sessionToken)
	{
		m_Transport.CheckWatchdog();
		if (m_Transport.IsInFlight() || m_Current)
			return;
		if (sessionToken == "")
			return;
		int now = VyshkaClock.MonotonicMs();
		for (int i = 0; i < m_Queue.Count(); i++)
		{
			VyshkaStoreRequest request = m_Queue.Get(i);
			if (now - request.m_QueuedMs > DEADLINE_MS)
			{
				m_Queue.RemoveOrdered(i);
				Fail(request, "the store did not answer within " + (DEADLINE_MS / 1000).ToString() + " s");
				return;
			}
			if (request.m_NotBeforeMs != 0 && now < request.m_NotBeforeMs)
				continue;
			if (request.m_RefusedToken != "" && request.m_RefusedToken == sessionToken)
				continue;
			m_Queue.RemoveOrdered(i);
			Send(request, sessionToken);
			return;
		}
	}

	void Send(VyshkaStoreRequest request, string sessionToken)
	{
		string verb = "get";
		if (request.m_Op == OP_SET)
			verb = "set";
		else if (request.m_Op == OP_DELETE)
			verb = "delete";
		string path = request.m_Namespace + "/" + request.m_Key + "/" + verb + VyshkaPlugin.INLINE_ERRORS;
		request.m_Attempts++;
		request.m_RefusedToken = sessionToken;   // cleared on any answer but session_invalid
		m_Current = request;
		if (!m_Transport.Post(request.m_Op, path, sessionToken, request.m_Body, REQUEST_BUDGET_MS))
		{
			m_Current = null;
			Retry(request, "the transport refused to send");
		}
	}

	override void OnResponse(int kind, bool ok, int code, string data)
	{
		VyshkaStoreRequest request = m_Current;
		m_Current = null;
		if (!request)
			return;
		if (!ok)
		{
			if (code == VyshkaTransport.ERROR_CLIENT)
			{
				// An opaque refusal: a hub without inline errors, or a proxy.
				// Nothing to branch on, so it is final.
				Fail(request, "the hub refused the request (client error, no details from this hub)");
				return;
			}
			Retry(request, VyshkaTransport.DescribeError(code));
			return;
		}

		// A delete answers 204 with no body; a get or set always has one.
		if (data == "")
		{
			if (request.m_Op == OP_DELETE)
			{
				VyshkaStoreResult gone = new VyshkaStoreResult();
				gone.m_Ok = true;
				gone.m_Found = false;
				Done(request, gone);
				return;
			}
			Retry(request, "an empty answer");
			return;
		}
		VyshkaJsonValue root = VyshkaJson.Parse(data);
		if (!root || !root.IsObject())
		{
			Retry(request, "an answer that is not a JSON object");
			return;
		}
		VyshkaHubError refusal = VyshkaHubError.FromBody(root);
		if (refusal)
		{
			OnRefused(request, refusal);
			return;
		}

		VyshkaStoreResult result = new VyshkaStoreResult();
		result.m_Ok = true;
		result.m_Found = true;
		result.m_Revision = root.GetInt("revision", 0);
		if (request.m_Op == OP_GET)
			result.m_Value = root.Get("value");
		Done(request, result);
	}

	// OnRefused applies section 12.3's error table to a refusal the hub
	// delivered inline. Only a session refusal and a hub-side error are
	// retried; the rest are answers.
	void OnRefused(VyshkaStoreRequest request, VyshkaHubError refusal)
	{
		string code = refusal.m_Code;
		if (refusal.IsMalformed())
		{
			Retry(request, "an unusable error member");
			return;
		}
		if (code == "not_found")
		{
			// Absent on a get; already gone on a delete. Both are answers.
			VyshkaStoreResult absent = new VyshkaStoreResult();
			absent.m_Ok = true;
			absent.m_Found = false;
			absent.m_Revision = 0;
			Done(request, absent);
			return;
		}
		if (code == "revision_mismatch")
		{
			VyshkaStoreResult mismatch = new VyshkaStoreResult();
			mismatch.m_Ok = true;
			mismatch.m_Found = true;
			mismatch.m_Mismatch = true;
			mismatch.m_Revision = refusal.m_Revision;
			Done(request, mismatch);
			return;
		}
		if (code == "session_invalid" || (code != "forbidden" && refusal.IsUnauthorized()))
		{
			// The link will start a new session on its own poll; the
			// request waits for a token other than the one refused.
			VyshkaLog.Info("store request " + request.m_Namespace + "/" + request.m_Key + " refused, " + refusal.Describe() + "; waiting for a new session");
			request.m_NotBeforeMs = 0;
			m_Queue.InsertAt(request, 0);
			return;
		}
		if (refusal.IsServerError())
		{
			Retry(request, refusal.Describe());
			return;
		}
		// forbidden (the namespace is not declared: a manifest the hub has
		// not accepted yet, or a plugin defect), bad_request, conflict, or a
		// code this plugin does not know: the request itself is wrong.
		Fail(request, "the hub refused the request, " + refusal.Describe());
	}

	// Retry requeues a request after a backoff, or fails it once its
	// deadline has passed.
	void Retry(VyshkaStoreRequest request, string why)
	{
		request.m_RefusedToken = "";
		int now = VyshkaClock.MonotonicMs();
		if (now - request.m_QueuedMs > DEADLINE_MS)
		{
			Fail(request, why + "; gave up after " + request.m_Attempts.ToString() + " attempt(s)");
			return;
		}
		int delay = RETRY_MIN_MS;
		for (int i = 1; i < request.m_Attempts && delay < RETRY_MAX_MS; i++)
			delay = delay * 2;
		if (delay > RETRY_MAX_MS)
			delay = RETRY_MAX_MS;
		request.m_NotBeforeMs = now + delay;
		VyshkaLog.Warn("store request " + request.m_Namespace + "/" + request.m_Key + " failed: " + why + "; retrying in " + delay.ToString() + " ms");
		m_Queue.InsertAt(request, 0);
	}

	void Fail(VyshkaStoreRequest request, string error)
	{
		m_Failed++;
		VyshkaLog.Error("store request " + request.m_Namespace + "/" + request.m_Key + " failed: " + error);
		Finish(request.m_Callback, VyshkaStoreResult.Failure(error));
	}

	void Done(VyshkaStoreRequest request, VyshkaStoreResult result)
	{
		m_Completed++;
		Finish(request.m_Callback, result);
	}

	void Finish(VyshkaStoreCallback callback, VyshkaStoreResult result)
	{
		if (callback)
			callback.OnStore(result);
	}

	// Shutdown fails everything still waiting, so no caller is left holding
	// a request that can never be answered.
	void Shutdown()
	{
		array<ref VyshkaStoreRequest> waiting = m_Queue;
		m_Queue = new array<ref VyshkaStoreRequest>;
		VyshkaStoreRequest current = m_Current;
		m_Current = null;
		if (current)
			Finish(current.m_Callback, VyshkaStoreResult.Failure("the plugin stopped"));
		for (int i = 0; i < waiting.Count(); i++)
			Finish(waiting.Get(i).m_Callback, VyshkaStoreResult.Failure("the plugin stopped"));
	}
}
