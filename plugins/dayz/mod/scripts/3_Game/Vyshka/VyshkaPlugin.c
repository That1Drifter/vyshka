// Vyshka DayZ plugin: the link to the hub.
//
// One object drives the whole lifecycle of spec sections 5, 6, 7 and 9:
// enroll once, start a session on every boot, long-poll forever, publish the
// manifest, execute dispatched actions behind an executed-actionId LRU, keep
// unacked envelopes in the outbox, and renumber them across session changes.
//
// Everything runs on the script tick. A repeating call-queue timer wakes the
// plugin, and the transport's callbacks land on the same thread, so there is
// no locking anywhere. One request is in flight at a time, which is all a
// long-poll link needs: the poll is held open by the hub, and the plugin's
// own traffic rides in the next poll.

class VyshkaPlugin
{
	static ref VyshkaPlugin s_Instance;

	static const string PLUGIN_NAME = "vyshka-dayz";
	static const string PLUGIN_VERSION = "0.1.0";
	static const int PROTOCOL_VERSION = 1;

	static const int TICK_MS = 200;
	static const int REQUEST_BUDGET_MS = 15000;      // enroll and session
	static const int EXECUTED_LRU_CAPACITY = 512;
	static const int BACKOFF_MIN_MS = 1000;
	static const int BACKOFF_MAX_MS = 30000;
	static const int BACKOFF_CREDENTIALS_MS = 30000;  // session refused
	static const int BACKOFF_ENROLL_REFUSED_MS = 60000;
	static const int RENEW_MARGIN_SECONDS = 60;      // start a new session this long before expiry

	static const int REQUEST_ENROLL = 1;
	static const int REQUEST_SESSION = 2;
	static const int REQUEST_POLL = 3;

	ref VyshkaConfig m_Config;
	ref VyshkaCredentials m_Credentials;
	ref VyshkaOutbox m_Outbox;
	ref VyshkaTransport m_Transport;
	ref VyshkaActionRegistry m_Actions;

	string m_SessionToken;
	int m_SessionExpiresEpoch;
	int m_RenewMarginSeconds;
	int m_ExecutedAppends;
	int m_PollTimeoutSeconds;
	int m_InAck;               // highest contiguous hub -> plugin seq processed
	bool m_ManifestQueued;

	// The executed-actionId LRU of spec section 9.2.
	ref array<string> m_ExecutedOrder;
	ref map<string, bool> m_Executed;

	bool m_Running;
	int m_NextAttemptMs;
	int m_BackoffMs;
	string m_LinkState;        // connected | degraded | buffering (section 9.4)
	int m_LastWarnMs;

	static void Start(VyshkaActionRegistry actions)
	{
		if (s_Instance)
			return;
		s_Instance = new VyshkaPlugin();
		s_Instance.Boot(actions);
	}

	static void Stop()
	{
		if (!s_Instance)
			return;
		s_Instance.Shutdown();
		s_Instance = null;
	}

	static string LinkState()
	{
		if (!s_Instance)
			return "stopped";
		return s_Instance.m_LinkState;
	}

	void VyshkaPlugin()
	{
		m_RenewMarginSeconds = RENEW_MARGIN_SECONDS;
		m_ExecutedOrder = new array<string>;
		m_Executed = new map<string, bool>;
		m_LinkState = "buffering";
		m_PollTimeoutSeconds = 25;
	}

	void Boot(VyshkaActionRegistry actions)
	{
		if (!GetGame().IsServer())
			return;
		m_Actions = actions;

		VyshkaFiles.EnsureLayout();
		m_Config = VyshkaConfig.Load();
		if (!m_Config)
		{
			VyshkaLog.Error("no usable config at " + VyshkaFiles.CONFIG_PATH + "; it needs at least {\"hubUrl\": \"...\", \"enrollmentToken\": \"...\"}. The plugin is idle.");
			return;
		}

		m_Credentials = VyshkaCredentials.Load();
		if (m_Credentials && m_Config.m_EnrollmentToken != "" && m_Credentials.m_EnrolledWithToken != "" && m_Credentials.m_EnrolledWithToken != m_Config.m_EnrollmentToken)
		{
			// A fresh token in the config file is the operator's recovery
			// path after a lost secret or a revocation (spec section 5.2).
			VyshkaLog.Info("the configured enrollment token is not the one these credentials came from; enrolling again");
			VyshkaCredentials.Delete();
			m_Credentials = null;
		}

		m_Outbox = new VyshkaOutbox();
		m_Outbox.Load();
		LoadExecuted();

		m_Transport = new VyshkaTransport();
		if (!m_Transport.Init(m_Config.m_HubUrl, this))
			return;
		m_Transport.SetReadTimeout(m_Config.m_PollTimeoutSeconds + 5);

		m_Running = true;
		GetGame().GetCallQueue(CALL_CATEGORY_SYSTEM).CallLater(Tick, TICK_MS, true);
		VyshkaLog.Info("started; hub " + m_Config.m_HubUrl + ", " + m_Actions.Count().ToString() + " action(s) declared");
	}

	void Shutdown()
	{
		if (!m_Running)
			return;
		m_Running = false;
		GetGame().GetCallQueue(CALL_CATEGORY_SYSTEM).Remove(Tick);
		VyshkaLog.Info("stopped with " + m_Outbox.Count().ToString() + " unacked envelope(s) on disk");
	}

	// ---- scheduling ----

	void Tick()
	{
		if (!m_Running)
			return;
		m_Transport.CheckWatchdog();
		if (m_Transport.IsInFlight())
			return;
		if (VyshkaClock.MonotonicMs() < m_NextAttemptMs)
			return;
		Advance();
	}

	// Advance issues whichever request the link needs next. The name matters:
	// a method named Step is shadowed by an engine built-in and never runs.
	void Advance()
	{
		if (!m_Running || m_Transport.IsInFlight())
			return;

		if (!m_Credentials)
		{
			if (m_Config.m_EnrollmentToken == "")
			{
				WarnThrottled("no credentials and no enrollment token in " + VyshkaFiles.CONFIG_PATH + "; waiting for one");
				Delay(BACKOFF_ENROLL_REFUSED_MS);
				return;
			}
			Enroll();
			return;
		}

		if (m_SessionToken == "")
		{
			StartSession();
			return;
		}

		if (m_SessionExpiresEpoch > 0 && VyshkaClock.EpochSeconds() >= m_SessionExpiresEpoch - m_RenewMarginSeconds)
		{
			VyshkaLog.Info("session is about to expire; starting a new one");
			m_SessionToken = "";
			StartSession();
			return;
		}

		Poll();
	}

	void Backoff()
	{
		if (m_BackoffMs < BACKOFF_MIN_MS)
			m_BackoffMs = BACKOFF_MIN_MS;
		else
			m_BackoffMs = m_BackoffMs * 2;
		if (m_BackoffMs > BACKOFF_MAX_MS)
			m_BackoffMs = BACKOFF_MAX_MS;
		m_NextAttemptMs = VyshkaClock.MonotonicMs() + m_BackoffMs;
	}

	void Delay(int ms)
	{
		m_NextAttemptMs = VyshkaClock.MonotonicMs() + ms;
	}

	void ResetBackoff()
	{
		m_BackoffMs = 0;
		m_NextAttemptMs = 0;
	}

	void SetLinkState(string state)
	{
		if (m_LinkState == state)
			return;
		m_LinkState = state;
		VyshkaLog.Info("link " + state);
	}

	// WarnThrottled keeps a persistent failure from filling the log: one
	// line per 30 s per plugin, whatever the retry cadence.
	void WarnThrottled(string message)
	{
		int now = VyshkaClock.MonotonicMs();
		if (m_LastWarnMs != 0 && now - m_LastWarnMs < 30000)
			return;
		m_LastWarnMs = now;
		VyshkaLog.Warn(message);
	}

	// ---- requests ----

	VyshkaJsonValue PluginDescriptor()
	{
		VyshkaJsonValue plugin = VyshkaJsonValue.NewObject();
		plugin.Set("name", VyshkaJsonValue.NewString(PLUGIN_NAME));
		plugin.Set("version", VyshkaJsonValue.NewString(PLUGIN_VERSION));
		return plugin;
	}

	VyshkaJsonValue Transports()
	{
		VyshkaJsonValue transports = VyshkaJsonValue.NewArray();
		transports.Add(VyshkaJsonValue.NewString("poll"));
		return transports;
	}

	void Enroll()
	{
		VyshkaJsonValue body = VyshkaJsonValue.NewObject();
		body.Set("enrollmentToken", VyshkaJsonValue.NewString(m_Config.m_EnrollmentToken));
		body.Set("game", VyshkaJsonValue.NewString(m_Config.m_Game));
		body.Set("plugin", PluginDescriptor());
		body.Set("transports", Transports());
		VyshkaLog.Info("enrolling with the hub");
		m_Transport.Post(REQUEST_ENROLL, "enroll", "", body.Serialize(), REQUEST_BUDGET_MS);
	}

	void StartSession()
	{
		VyshkaJsonValue body = VyshkaJsonValue.NewObject();
		body.Set("serverId", VyshkaJsonValue.NewString(m_Credentials.m_ServerId));
		body.Set("serverSecret", VyshkaJsonValue.NewString(m_Credentials.m_ServerSecret));
		body.Set("protocolVersion", VyshkaJsonValue.NewInt(PROTOCOL_VERSION));
		body.Set("pollTimeoutSeconds", VyshkaJsonValue.NewInt(m_Config.m_PollTimeoutSeconds));
		body.Set("plugin", PluginDescriptor());
		body.Set("transports", Transports());
		m_Transport.Post(REQUEST_SESSION, "session", "", body.Serialize(), REQUEST_BUDGET_MS);
	}

	void Poll()
	{
		string body = "{\"ack\":" + m_InAck.ToString() + ",\"envelopes\":" + m_Outbox.BatchJson() + "}";
		// The hub answers within pollTimeout; the engine's read timeout is
		// pollTimeout + 5 s; the watchdog sits behind both.
		m_Transport.Post(REQUEST_POLL, "poll", m_SessionToken, body, (m_PollTimeoutSeconds + 10) * 1000);
	}

	// ---- responses ----

	void OnResponse(int kind, bool ok, int code, string data)
	{
		if (!m_Running)
			return;
		if (kind == REQUEST_ENROLL)
			OnEnrollResponse(ok, code, data);
		else if (kind == REQUEST_SESSION)
			OnSessionResponse(ok, code, data);
		else if (kind == REQUEST_POLL)
			OnPollResponse(ok, code, data);
	}

	void OnEnrollResponse(bool ok, int code, string data)
	{
		if (!ok)
		{
			if (code == VyshkaTransport.ERROR_CLIENT)
			{
				// 401 enrollment_token_invalid, 409 enrollment_token_used, or
				// 409 game_mismatch: the engine hides which. None is fixed by
				// retrying, so retry slowly and tell the operator.
				VyshkaLog.Error("enrollment refused: the token is unknown, expired, or already used, or the hub's server record declares a different game. Issue a fresh enrollment token and put it in " + VyshkaFiles.CONFIG_PATH);
				Delay(BACKOFF_ENROLL_REFUSED_MS);
			}
			else
			{
				WarnThrottled("enrollment failed: " + VyshkaTransport.DescribeError(code) + "; retrying");
				Backoff();
			}
			return;
		}

		VyshkaJsonValue root = VyshkaJson.Parse(data);
		if (!root || !root.IsObject())
		{
			VyshkaLog.Warn("enrollment answered with a body that is not a JSON object; retrying");
			Backoff();
			return;
		}
		VyshkaCredentials credentials = new VyshkaCredentials();
		credentials.m_ServerId = root.GetString("serverId", "");
		credentials.m_ServerSecret = root.GetString("serverSecret", "");
		credentials.m_EnrolledWithToken = m_Config.m_EnrollmentToken;
		if (credentials.m_ServerId == "" || credentials.m_ServerSecret == "")
		{
			VyshkaLog.Warn("enrollment answered without serverId and serverSecret; retrying");
			Backoff();
			return;
		}
		if (!credentials.Save())
			VyshkaLog.Error("could not write " + VyshkaFiles.CREDENTIALS_PATH + "; the plugin will try to enroll again after a restart, which will fail because the token is burned");
		m_Credentials = credentials;
		VyshkaLog.Info("enrolled as server " + credentials.m_ServerId);
		ResetBackoff();
		Advance();
	}

	void OnSessionResponse(bool ok, int code, string data)
	{
		if (!ok)
		{
			if (code == VyshkaTransport.ERROR_CLIENT)
			{
				// credentials_invalid, credentials_revoked, or
				// protocol_version_unsupported. Only a fresh enrollment token
				// (spec section 5.4) gets the plugin out of this.
				WarnThrottled("session refused: the credentials are invalid or revoked. If the server was revoked, issue a fresh enrollment token and put it in " + VyshkaFiles.CONFIG_PATH);
				SetLinkState("buffering");
				Delay(BACKOFF_CREDENTIALS_MS);
			}
			else
			{
				WarnThrottled("session request failed: " + VyshkaTransport.DescribeError(code) + "; retrying");
				SetLinkState("buffering");
				Backoff();
			}
			return;
		}

		VyshkaJsonValue root = VyshkaJson.Parse(data);
		if (!root || !root.IsObject())
		{
			VyshkaLog.Warn("session answered with a body that is not a JSON object; retrying");
			Backoff();
			return;
		}
		string token = root.GetString("sessionToken", "");
		if (token == "")
		{
			VyshkaLog.Warn("session answered without a sessionToken; retrying");
			Backoff();
			return;
		}

		m_SessionToken = token;
		m_PollTimeoutSeconds = root.GetInt("pollTimeoutSeconds", 25);
		if (m_PollTimeoutSeconds < 1)
			m_PollTimeoutSeconds = 25;
		m_Transport.SetReadTimeout(m_PollTimeoutSeconds + 5);
		int expires;
		if (VyshkaClock.ParseRfc3339(root.GetString("expiresAt", ""), expires))
			m_SessionExpiresEpoch = expires;
		else
			m_SessionExpiresEpoch = 0;

		// The renewal margin must be smaller than the session's own lifetime,
		// or a hub that issues a short session (the protocol sets no minimum)
		// would make the plugin renew on the very next tick and never poll.
		// Cap it at half the lifetime.
		m_RenewMarginSeconds = RENEW_MARGIN_SECONDS;
		int lifetime = m_SessionExpiresEpoch - VyshkaClock.EpochSeconds();
		if (lifetime > 0 && m_RenewMarginSeconds > lifetime / 2)
			m_RenewMarginSeconds = lifetime / 2;

		// A new session is a new sequence space in both directions (spec
		// section 9.1): the inbound ack restarts at 0, and every buffered
		// envelope is renumbered from 1, keeping its id, type, ts and body.
		m_InAck = 0;
		m_Outbox.Renumber();

		if (!m_ManifestQueued && m_Outbox.Append("manifest.publish", m_Actions.ManifestBody(m_Config.m_Game, PLUGIN_NAME, PLUGIN_VERSION)))
			m_ManifestQueued = true;

		VyshkaLog.Info("session started; pollTimeout " + m_PollTimeoutSeconds.ToString() + " s, " + m_Outbox.Count().ToString() + " envelope(s) to send");
		SetLinkState("connected");
		ResetBackoff();
		Advance();
	}

	void OnPollResponse(bool ok, int code, string data)
	{
		if (!ok)
		{
			if (code == VyshkaTransport.ERROR_CLIENT)
			{
				// 401 session_invalid is the expected reason (superseded,
				// expired, or revoked). A 400 over the plugin's own batch
				// looks the same from here; either way a fresh session is
				// legal and is the right answer to the common case.
				VyshkaLog.Info("poll refused; starting a new session");
				m_SessionToken = "";
				SetLinkState("degraded");
				Backoff();
			}
			else
			{
				WarnThrottled("poll failed: " + VyshkaTransport.DescribeError(code) + "; " + m_Outbox.Count().ToString() + " envelope(s) buffered");
				SetLinkState("buffering");
				Backoff();
			}
			return;
		}

		VyshkaJsonValue root = VyshkaJson.Parse(data);
		if (!root || !root.IsObject())
		{
			VyshkaLog.Warn("poll answered with a body that is not a JSON object; re-polling");
			Backoff();
			return;
		}

		// The hub's ack releases everything it covers. A lower ack than one
		// already applied changes nothing, which is what section 9.1 asks.
		int ack = root.GetInt("ack", 0);
		if (ack > 0)
			m_Outbox.Ack(ack);

		int pollTimeout = root.GetInt("pollTimeoutSeconds", m_PollTimeoutSeconds);
		if (pollTimeout >= 1 && pollTimeout != m_PollTimeoutSeconds)
		{
			m_PollTimeoutSeconds = pollTimeout;
			m_Transport.SetReadTimeout(m_PollTimeoutSeconds + 5);
		}
		int expires;
		if (VyshkaClock.ParseRfc3339(root.GetString("sessionExpiresAt", ""), expires))
			m_SessionExpiresEpoch = expires;

		// Take delivery in arrival order: contiguous envelopes advance the
		// ack and are handled, duplicates at or below it are acknowledged
		// again and processed no further, anything above a gap waits for the
		// hub's retransmission (section 9.1).
		VyshkaJsonValue envelopes = root.Get("envelopes");
		if (envelopes && envelopes.IsArray())
		{
			for (int i = 0; i < envelopes.Count(); i++)
			{
				VyshkaJsonValue envelope = envelopes.At(i);
				if (!envelope || !envelope.IsObject())
					continue;
				VyshkaJsonValue seqValue = envelope.Get("seq");
				if (!seqValue || !seqValue.IsNumber() || !seqValue.m_IsInteger)
					continue;
				int seq = seqValue.m_Int;
				if (seq <= m_InAck)
					continue;
				if (seq != m_InAck + 1)
					continue;
				// Validate the framing of the next envelope before taking
				// delivery (section 4): an envelope missing id or type, or
				// declaring a version this plugin does not speak, is rejected,
				// not acked. Advancing the ack past it would tell the hub the
				// plugin processed something it could not. Unknown types are
				// not rejected here; they are acked and ignored in Handle.
				if (!FramingOk(envelope))
				{
					VyshkaLog.Warn("rejecting hub envelope seq " + seq.ToString() + ": missing id or type, or an unsupported envelope version");
					break;
				}
				m_InAck = seq;
				Handle(envelope);
			}
		}

		SetLinkState("connected");
		ResetBackoff();
		Advance();
	}

	// ---- inbound envelopes ----

	// FramingOk enforces the receiver half of section 4 on a hub -> plugin
	// envelope: it must carry an id and a type, and its version, if stated,
	// must be one this plugin speaks. An absent v means the negotiated
	// version; an explicit 0 is a version no one speaks and is refused.
	bool FramingOk(VyshkaJsonValue envelope)
	{
		if (envelope.GetString("id", "") == "" || envelope.GetString("type", "") == "")
			return false;
		VyshkaJsonValue version = envelope.Get("v");
		if (version && version.IsNumber() && version.m_Int != PROTOCOL_VERSION)
			return false;
		return true;
	}

	void Handle(VyshkaJsonValue envelope)
	{
		string envelopeType = envelope.GetString("type", "");
		VyshkaJsonValue body = envelope.Get("body");
		if (envelopeType == "action.dispatch")
			HandleDispatch(body);
		else if (envelopeType == "manifest.reject")
			HandleManifestReject(body);
		// Anything else is acked and ignored (spec section 4).
	}

	void HandleManifestReject(VyshkaJsonValue body)
	{
		if (!body || !body.IsObject())
		{
			VyshkaLog.Error("the hub rejected the manifest (no details given)");
			return;
		}
		VyshkaLog.Error("the hub rejected manifest revision " + body.GetInt("manifestRevision", 0).ToString() + " (envelope " + body.GetString("envelopeId", "?") + "); no action can be dispatched until a corrected manifest is published:");
		VyshkaJsonValue errors = body.Get("errors");
		if (!errors || !errors.IsArray())
			return;
		for (int i = 0; i < errors.Count(); i++)
		{
			VyshkaJsonValue fault = errors.At(i);
			if (!fault || !fault.IsObject())
				continue;
			VyshkaLog.Error("  " + fault.GetString("path", "") + ": " + fault.GetString("message", ""));
		}
	}

	void HandleDispatch(VyshkaJsonValue body)
	{
		// A body the plugin cannot use is acked and ignored (spec section 7);
		// nothing a hub sends may take the game server down.
		if (!body || !body.IsObject())
		{
			VyshkaLog.Warn("ignoring an action.dispatch with an unusable body");
			return;
		}
		string actionId = body.GetString("actionId", "");
		if (actionId == "")
		{
			VyshkaLog.Warn("ignoring an action.dispatch without an actionId");
			return;
		}
		if (m_Executed.Contains(actionId))
		{
			// At-least-once delivery makes repeats ordinary. The hub treats a
			// repeated ack or result as a no-op, and a black-box observer
			// cannot tell a repeated result from a repeated execution, so a
			// recognized duplicate is acked (by the poll) and answered with
			// silence.
			VyshkaLog.Info("action " + actionId + " was already executed; ignoring the repeat");
			return;
		}
		MarkExecuted(actionId);

		string ackBody = "{\"actionId\":" + VyshkaJson.Quote(actionId) + "}";
		m_Outbox.Append("action.ack", ackBody);

		string code = body.GetString("code", "");
		string context = body.GetString("context", "");
		string referenceKey = body.GetString("referenceKey", "");

		int deadline;
		// The engine clock is whole-second, and the parser floors expiresAt to
		// its second, so the true deadline lies anywhere in [deadline,
		// deadline+1). Discarding once the current second reaches that second
		// (>=, not >) is the only rule that never runs an action past its real
		// deadline; it can discard up to a second early, which is harmless
		// against a TTL measured in seconds (section 7).
		if (VyshkaClock.ParseRfc3339(body.GetString("expiresAt", ""), deadline) && VyshkaClock.EpochSeconds() >= deadline)
		{
			// Past its deadline the hub has already reported the action
			// expired and will ignore a result (section 7), so the work is
			// not done and nothing is sent.
			VyshkaLog.Warn("action " + actionId + " (" + code + ") arrived after its deadline and was discarded");
			return;
		}

		int started = VyshkaClock.MonotonicMs();
		VyshkaActionOutcome outcome = m_Actions.Execute(code, context, referenceKey, body.Get("params"));
		int durationMs = VyshkaClock.MonotonicMs() - started;
		if (!outcome)
			outcome = VyshkaActionOutcome.Failure("the action produced no outcome");

		VyshkaJsonValue result = VyshkaJsonValue.NewObject();
		result.Set("actionId", VyshkaJsonValue.NewString(actionId));
		result.Set("ok", VyshkaJsonValue.NewBool(outcome.m_Ok));
		if (outcome.m_Result)
			result.Set("result", outcome.m_Result);
		else
			result.Set("result", VyshkaJsonValue.NewNull());
		if (outcome.m_Ok || outcome.m_Error == "")
			result.Set("error", VyshkaJsonValue.NewNull());
		else
			result.Set("error", VyshkaJsonValue.NewString(outcome.m_Error));
		result.Set("durationMs", VyshkaJsonValue.NewInt(durationMs));
		m_Outbox.Append("action.result", result.Serialize());

		if (outcome.m_Ok)
			VyshkaLog.Info("action " + code + " (" + actionId + ") completed in " + durationMs.ToString() + " ms");
		else
			VyshkaLog.Info("action " + code + " (" + actionId + ") failed: " + outcome.m_Error);
	}

	// MarkExecuted records an executed actionId in the in-memory LRU and on
	// disk. Persistence closes the cross-restart dedup hole: the hub renumbers
	// and re-delivers a dispatch that was executed but not yet poll-acked when
	// the server crashed (section 9.1), and without the durable record the
	// reloaded plugin would execute it a second time (section 9.2).
	void MarkExecuted(string actionId)
	{
		while (m_ExecutedOrder.Count() >= EXECUTED_LRU_CAPACITY)
		{
			string oldest = m_ExecutedOrder.Get(0);
			m_ExecutedOrder.RemoveOrdered(0);
			m_Executed.Remove(oldest);
		}
		m_Executed.Set(actionId, true);
		m_ExecutedOrder.Insert(actionId);

		VyshkaFiles.AppendLine(VyshkaFiles.EXECUTED_PATH, actionId);
		m_ExecutedAppends++;
		// Compact once the log has roughly doubled, so the file stays bounded
		// at about the LRU size rather than growing for the life of the server.
		if (m_ExecutedAppends >= EXECUTED_LRU_CAPACITY)
			RewriteExecuted();
	}

	// LoadExecuted repopulates the LRU from disk on boot, keeping the most
	// recent ids up to the cap, and compacts an oversized log.
	void LoadExecuted()
	{
		array<string> ids = VyshkaFiles.ReadLines(VyshkaFiles.EXECUTED_PATH);
		int start = 0;
		if (ids.Count() > EXECUTED_LRU_CAPACITY)
			start = ids.Count() - EXECUTED_LRU_CAPACITY;
		for (int i = start; i < ids.Count(); i++)
		{
			string id = ids.Get(i);
			if (!m_Executed.Contains(id))
			{
				m_Executed.Set(id, true);
				m_ExecutedOrder.Insert(id);
			}
		}
		if (ids.Count() > 2 * EXECUTED_LRU_CAPACITY)
			RewriteExecuted();
		if (m_ExecutedOrder.Count() > 0)
			VyshkaLog.Info("restored " + m_ExecutedOrder.Count().ToString() + " executed action id(s) from disk");
	}

	// RewriteExecuted replaces the log with exactly the current LRU contents.
	void RewriteExecuted()
	{
		string content = "";
		for (int i = 0; i < m_ExecutedOrder.Count(); i++)
			content += m_ExecutedOrder.Get(i) + "\n";
		if (VyshkaFiles.WriteAll(VyshkaFiles.EXECUTED_PATH, content))
			m_ExecutedAppends = 0;
	}
}
