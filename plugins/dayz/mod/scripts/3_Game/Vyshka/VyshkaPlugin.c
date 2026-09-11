// Vyshka DayZ plugin: the link to the hub.
//
// One object drives the whole lifecycle of spec sections 5, 6, 7, 8 and 9:
// enroll once, start a session on every boot, long-poll forever, publish the
// manifest, execute dispatched actions behind an executed-actionId LRU, flush
// events and snapshots into the outbox, keep unacked envelopes there, and
// renumber them across session changes.
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
	static const string PLUGIN_VERSION = "0.2.0";
	static const int PROTOCOL_VERSION = 1;

	static const int TICK_MS = 200;
	static const int REQUEST_BUDGET_MS = 15000;      // enroll and session
	static const int EXECUTED_LRU_CAPACITY = 512;
	static const int BACKOFF_MIN_MS = 1000;
	static const int BACKOFF_MAX_MS = 30000;
	static const int BACKOFF_CREDENTIALS_MS = 30000;  // session refused
	static const int BACKOFF_ENROLL_REFUSED_MS = 60000;
	static const int BACKOFF_REQUEST_MS = 60000;      // the plugin's own request was refused (a bug, not a cadence problem)
	static const int BACKOFF_PROTOCOL_MS = 300000;    // protocol version refused: something needs upgrading
	static const int RENEW_MARGIN_SECONDS = 60;      // start a new session this long before expiry
	static const int SNAPSHOTS_HELD_LOG_AFTER = 12;  // consecutive held snapshot ticks before the first log line (2 min at the default cadence)
	static const int SNAPSHOTS_HELD_LOG_EVERY = 60;  // and then every this many (10 min)

	// Every request asks for its refusals inline (spec section 2.3): the
	// engine delivers a non-2xx as an opaque code with no body, so this is
	// the only way the protocol's error codes reach the plugin. A hub that
	// predates the option ignores it, and the opaque handling below remains.
	static const string INLINE_ERRORS = "?errors=inline";

	static const int REQUEST_ENROLL = 1;
	static const int REQUEST_SESSION = 2;
	static const int REQUEST_POLL = 3;

	ref VyshkaConfig m_Config;
	ref VyshkaCredentials m_Credentials;
	ref VyshkaOutbox m_Outbox;
	ref VyshkaTransport m_Transport;
	ref VyshkaActionRegistry m_Actions;
	ref VyshkaEventBuffer m_Events;
	ref VyshkaSnapshotSource m_Snapshots;
	int m_NextSnapshotMs;
	int m_SnapshotsHeld;       // snapshot ticks skipped because the last one is still unacked

	string m_SessionToken;
	int m_SessionExpiresEpoch;
	int m_RenewMarginSeconds;
	int m_PollTimeoutSeconds;
	int m_InAck;               // highest contiguous hub -> plugin seq processed
	bool m_ManifestQueued;
	bool m_PolledThisSession;
	int m_UnpolledRefusals;    // opaque poll refusals on a session that has never polled (see OnPollResponse)

	// The executed-actionId LRU of spec section 9.2.
	ref array<string> m_ExecutedOrder;
	ref map<string, bool> m_Executed;

	bool m_Running;
	int m_NextAttemptMs;
	int m_BackoffMs;
	string m_LinkState;        // connected | degraded | buffering (section 9.4)
	int m_LastWarnMs;

	static void Start(VyshkaActionRegistry actions, VyshkaSnapshotSource snapshots)
	{
		if (s_Instance)
			return;
		s_Instance = new VyshkaPlugin();
		s_Instance.Boot(actions, snapshots);
	}

	// Emit queues one telemetry event (spec section 8.1) from anywhere in the
	// mod. t is a {namespace}.{name} type, data the payload object or null.
	// Before the plugin has started, or after it stopped, the event is
	// dropped: there is nothing to carry it and no session it could belong to.
	static void Emit(string t, VyshkaJsonValue data)
	{
		if (!s_Instance || !s_Instance.m_Running)
			return;
		s_Instance.m_Events.Add(t, data);
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
		m_Events = new VyshkaEventBuffer();
	}

	void Boot(VyshkaActionRegistry actions, VyshkaSnapshotSource snapshots)
	{
		if (!GetGame().IsServer())
			return;
		m_Actions = actions;
		m_Snapshots = snapshots;

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
		if (m_Config.m_SnapshotIntervalSeconds > 0 && m_Snapshots)
			m_NextSnapshotMs = VyshkaClock.MonotonicMs() + m_Config.m_SnapshotIntervalSeconds * 1000;
		GetGame().GetCallQueue(CALL_CATEGORY_SYSTEM).CallLater(Tick, TICK_MS, true);
		string snapshotNote = "snapshots off";
		if (m_NextSnapshotMs > 0)
			snapshotNote = "state.players every " + m_Config.m_SnapshotIntervalSeconds.ToString() + " s";
		VyshkaLog.Info("started; hub " + m_Config.m_HubUrl + ", " + m_Actions.Count().ToString() + " action(s) declared, " + snapshotNote);
		Emit("core.server.start", ServerEventData());
	}

	void Shutdown()
	{
		if (!m_Running)
			return;
		// The stop event cannot be sent by this process, which is going away;
		// it is flushed to the outbox so the next boot delivers it, stamped
		// with the time it happened, ahead of that boot's own start event.
		Emit("core.server.stop", ServerEventData());
		FlushEvents();
		m_Running = false;
		GetGame().GetCallQueue(CALL_CATEGORY_SYSTEM).Remove(Tick);
		VyshkaLog.Info("stopped with " + m_Outbox.Count().ToString() + " unacked envelope(s) on disk");
	}

	// ServerEventData is the payload of core.server.start and core.server.stop:
	// enough for a feed to say which server and world came up, and which
	// plugin build says so.
	VyshkaJsonValue ServerEventData()
	{
		VyshkaJsonValue data = VyshkaJsonValue.NewObject();
		data.Set("game", VyshkaJsonValue.NewString(m_Config.m_Game));
		data.Set("plugin", PluginDescriptor());
		string world;
		GetGame().GetWorldName(world);
		if (world != "")
			data.Set("world", VyshkaJsonValue.NewString(world));
		return data;
	}

	// ---- scheduling ----

	void Tick()
	{
		if (!m_Running)
			return;
		// Telemetry goes into the outbox whether or not a request is in
		// flight; whatever is queued rides the next poll.
		PublishTelemetry();
		m_Transport.CheckWatchdog();
		if (m_Transport.IsInFlight())
			return;
		if (VyshkaClock.MonotonicMs() < m_NextAttemptMs)
			return;
		Advance();
	}

	// ---- telemetry (spec section 8) ----

	void PublishTelemetry()
	{
		if (m_Events.Due())
			FlushEvents();
		if (m_NextSnapshotMs > 0 && VyshkaClock.MonotonicMs() >= m_NextSnapshotMs)
		{
			PublishSnapshot();
			m_NextSnapshotMs = VyshkaClock.MonotonicMs() + m_Config.m_SnapshotIntervalSeconds * 1000;
		}
	}

	// FlushEvents moves every pending event into event.batch envelopes of at
	// most 200 events each (section 8.1). An outbox refusal (it is full)
	// drops the batch; the outbox counts and logs the refusal, which is the
	// visible counter section 9.4 asks for.
	void FlushEvents()
	{
		while (m_Events.Count() > 0)
		{
			int count = m_Events.Count();
			if (count > VyshkaEventBuffer.FLUSH_COUNT)
				count = VyshkaEventBuffer.FLUSH_COUNT;
			string body = m_Events.TakeBatch();
			if (!m_Outbox.Append("event.batch", body))
				VyshkaLog.Warn("dropped " + count.ToString() + " event(s) the outbox could not hold");
		}
	}

	// PublishSnapshot queues one state.players envelope unless the previous
	// one is still unacked, in which case this tick is skipped: the map
	// wants the latest state, and a queue of stale snapshots behind an
	// outage serves no one (section 8.3 keeps the latest per type anyway).
	// Skipping is ordinary on a healthy link, because an ack arrives only
	// when the held poll returns (up to pollTimeout later), so the effective
	// cadence is the longer of the configured interval and the poll cycle;
	// it is logged only once it has gone on long enough to mean an outage.
	void PublishSnapshot()
	{
		if (m_Outbox.HasUnacked("state.players"))
		{
			m_SnapshotsHeld++;
			if (m_SnapshotsHeld == SNAPSHOTS_HELD_LOG_AFTER || m_SnapshotsHeld % SNAPSHOTS_HELD_LOG_EVERY == 0)
				VyshkaLog.Info("state.players snapshot held back: the previous one is still unacked (" + m_SnapshotsHeld.ToString() + " skipped)");
			return;
		}
		m_SnapshotsHeld = 0;
		string body = m_Snapshots.CapturePlayers();
		if (body == "")
			return;
		m_Outbox.Append("state.players", body);
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

		// Renew before expiry, but only after at least one poll on this
		// session. The poll gate guarantees forward progress even against a
		// hub that issues a session shorter than the renew margin (or one that
		// floors to a zero-second lifetime), which would otherwise renew every
		// tick and never poll.
		if (m_PolledThisSession && m_SessionExpiresEpoch > 0 && VyshkaClock.EpochSeconds() >= m_SessionExpiresEpoch - m_RenewMarginSeconds)
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
		m_Transport.Post(REQUEST_ENROLL, "enroll" + INLINE_ERRORS, "", body.Serialize(), REQUEST_BUDGET_MS);
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
		m_Transport.Post(REQUEST_SESSION, "session" + INLINE_ERRORS, "", body.Serialize(), REQUEST_BUDGET_MS);
	}

	void Poll()
	{
		string body = "{\"ack\":" + m_InAck.ToString() + ",\"envelopes\":" + m_Outbox.BatchJson() + "}";
		// The hub answers within pollTimeout; the engine's read timeout is
		// pollTimeout + 5 s; the watchdog sits behind both.
		m_Transport.Post(REQUEST_POLL, "poll" + INLINE_ERRORS, m_SessionToken, body, (m_PollTimeoutSeconds + 10) * 1000);
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
		VyshkaHubError refusal = VyshkaHubError.FromBody(root);
		if (refusal)
		{
			OnEnrollRefused(refusal);
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

	// OnEnrollRefused applies the recovery table of spec section 2.3 to an
	// enrollment refusal the plugin could read.
	void OnEnrollRefused(VyshkaHubError refusal)
	{
		string code = refusal.m_Code;
		if (refusal.IsMalformed())
		{
			VyshkaLog.Warn("enrollment answered with an unusable error member; retrying");
			Backoff();
		}
		else if (code == "enrollment_token_invalid" || code == "enrollment_token_used" || code == "game_mismatch")
		{
			VyshkaLog.Error("enrollment refused, " + refusal.Describe() + ". No retry fixes this: issue a fresh enrollment token and put it in " + VyshkaFiles.CONFIG_PATH);
			Delay(BACKOFF_ENROLL_REFUSED_MS);
		}
		else if (refusal.IsServerError())
		{
			WarnThrottled("enrollment failed at the hub, " + refusal.Describe() + "; retrying");
			Backoff();
		}
		else
		{
			// bad_request, or a code this plugin does not know with a client
			// status: the request itself is wrong, which no cadence fixes.
			VyshkaLog.Error("enrollment refused, " + refusal.Describe() + "; this looks like a plugin or hub defect, retrying slowly");
			Delay(BACKOFF_REQUEST_MS);
		}
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
		VyshkaHubError refusal = VyshkaHubError.FromBody(root);
		if (refusal)
		{
			OnSessionRefused(refusal);
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
		m_PolledThisSession = false;
		m_Outbox.Renumber();

		if (!m_ManifestQueued && m_Outbox.Append("manifest.publish", m_Actions.ManifestBody(m_Config.m_Game, PLUGIN_NAME, PLUGIN_VERSION)))
			m_ManifestQueued = true;

		VyshkaLog.Info("session started; pollTimeout " + m_PollTimeoutSeconds.ToString() + " s, " + m_Outbox.Count().ToString() + " envelope(s) to send");
		SetLinkState("connected");
		ResetBackoff();
		Advance();
	}

	// OnSessionRefused applies the recovery table of spec section 2.3 to a
	// session refusal the plugin could read. Nothing here starts a loop: the
	// refusals that matter need the operator, and the plugin says so.
	void OnSessionRefused(VyshkaHubError refusal)
	{
		string code = refusal.m_Code;
		SetLinkState("buffering");
		if (refusal.IsMalformed())
		{
			VyshkaLog.Warn("session answered with an unusable error member; retrying");
			Backoff();
		}
		else if (code == "credentials_invalid" || code == "credentials_revoked" || (code != "protocol_version_unsupported" && refusal.IsUnauthorized()))
		{
			WarnThrottled("session refused, " + refusal.Describe() + ". Issue a fresh enrollment token and put it in " + VyshkaFiles.CONFIG_PATH + "; retrying every " + (BACKOFF_CREDENTIALS_MS / 1000).ToString() + " s meanwhile");
			Delay(BACKOFF_CREDENTIALS_MS);
		}
		else if (code == "protocol_version_unsupported")
		{
			VyshkaLog.Error("session refused, " + refusal.Describe() + ". This plugin speaks protocol version " + PROTOCOL_VERSION.ToString() + " and the hub does not; upgrade the hub or the plugin");
			Delay(BACKOFF_PROTOCOL_MS);
		}
		else if (refusal.IsServerError())
		{
			WarnThrottled("session request failed at the hub, " + refusal.Describe() + "; retrying");
			Backoff();
		}
		else
		{
			VyshkaLog.Error("session refused, " + refusal.Describe() + "; this looks like a plugin or hub defect, retrying slowly");
			Delay(BACKOFF_REQUEST_MS);
		}
	}

	void OnPollResponse(bool ok, int code, string data)
	{
		if (!ok)
		{
			if (code == VyshkaTransport.ERROR_CLIENT)
				OnPollRefusedOpaque();
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
			// A malformed answer changes nothing: same session, same outbox,
			// re-poll after backoff (spec section 2.3).
			VyshkaLog.Warn("poll answered with a body that is not a JSON object; re-polling");
			Backoff();
			return;
		}
		VyshkaHubError refusal = VyshkaHubError.FromBody(root);
		if (refusal)
		{
			OnPollRefused(refusal);
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
				// Back-pressure: a dispatch produces an action.ack and an
				// action.result, so it is only taken when the outbox can hold
				// both. Otherwise the envelope is left undelivered (m_InAck is
				// not advanced) and the hub re-delivers it once the outbox has
				// drained, rather than the plugin executing an action whose
				// outcome it could never report (section 9.4). The hub ack was
				// already applied above, so a poll that frees space unblocks
				// this on the same tick.
				if (!m_Outbox.HasRoom(2))
				{
					VyshkaLog.Warn("outbox full; deferring hub envelope seq " + seq.ToString() + " until it drains");
					break;
				}
				m_InAck = seq;
				Handle(envelope);
			}
		}

		m_PolledThisSession = true;
		m_UnpolledRefusals = 0;
		SetLinkState("connected");
		ResetBackoff();
		Advance();
	}

	// OnPollRefused applies the recovery table of spec section 2.3 to a poll
	// refusal the plugin could read. The rule the table exists for: a client
	// error is never answered by churning sessions.
	void OnPollRefused(VyshkaHubError refusal)
	{
		string code = refusal.m_Code;
		if (refusal.IsMalformed())
		{
			// Same session, same outbox, no ack applied: re-poll after backoff.
			VyshkaLog.Warn("poll answered with an unusable error member; re-polling");
			Backoff();
		}
		else if (code == "session_invalid" || (code != "ack_out_of_range" && code != "envelope_invalid" && code != "bad_request" && refusal.IsUnauthorized()))
		{
			// Superseded, expired, or revoked: one new session, which is
			// legal at any time (section 5.3). The outbox is kept and
			// renumbered when the session starts.
			VyshkaLog.Info("poll refused, " + refusal.Describe() + "; starting a new session");
			m_SessionToken = "";
			SetLinkState("degraded");
			Backoff();
		}
		else if (code == "envelope_invalid")
		{
			// The hub applied nothing, so the batch can be corrected and sent
			// again at once. The refused envelope is set aside on disk with
			// the hub's reason; it is never resent and never counted as
			// delivered.
			if (refusal.m_Index < 0 || !m_Outbox.Quarantine(refusal.m_Index, refusal.m_Message))
			{
				VyshkaLog.Error("poll refused, " + refusal.Describe() + ", but details.index names nothing in the batch; backing off");
				SetLinkState("degraded");
				Backoff();
				return;
			}
			ResetBackoff();
			Advance();
		}
		else if (code == "ack_out_of_range")
		{
			// The plugin's inbound ack is ahead of anything the hub sent on
			// this session, which no retry reconciles: a new session starts
			// the sequence space over.
			VyshkaLog.Warn("poll refused, " + refusal.Describe() + "; starting a new session to reset the sequence space");
			m_SessionToken = "";
			SetLinkState("degraded");
			Backoff();
		}
		else if (code == "bad_request")
		{
			WarnThrottled("poll refused, " + refusal.Describe() + "; retrying with a smaller batch");
			m_Outbox.ShrinkBatch();
			SetLinkState("degraded");
			Backoff();
		}
		else if (refusal.IsServerError())
		{
			WarnThrottled("poll failed at the hub, " + refusal.Describe() + "; retrying, " + m_Outbox.Count().ToString() + " envelope(s) buffered");
			SetLinkState("buffering");
			Backoff();
		}
		else
		{
			// A code this plugin does not know with a client status: the
			// request was wrong and a new session does not make it right.
			WarnThrottled("poll refused, " + refusal.Describe() + "; backing off and retrying the same session");
			SetLinkState("degraded");
			Backoff();
		}
	}

	// OnPollRefusedOpaque is the fallback for a hub that did not deliver the
	// refusal inline (one that predates spec section 2.3, or a proxy
	// answering in its place): script sees a client error and nothing else.
	// A 401 session_invalid is the common cause and a new session is its
	// answer, but a 400 over the plugin's own batch looks identical, and a
	// new session would not fix that. So: a refusal on a session that has
	// polled successfully means the session is gone, and one new session is
	// started; a refusal on a session that has never polled is more likely
	// the batch, so the plugin backs off and retries the same session first,
	// and only opens another session after a second refusal there. Worst
	// case is one new session a minute with a log line, not a loop.
	void OnPollRefusedOpaque()
	{
		if (m_PolledThisSession || m_UnpolledRefusals >= 2)
		{
			VyshkaLog.Info("poll refused (client error, no details from this hub); starting a new session");
			m_SessionToken = "";
			m_UnpolledRefusals = 0;
			SetLinkState("degraded");
			Backoff();
			return;
		}
		m_UnpolledRefusals++;
		WarnThrottled("poll refused on a session that has not polled yet (client error, no details from this hub); the hub may be refusing this batch rather than the session, so backing off " + (BACKOFF_MAX_MS / 1000).ToString() + " s before retrying it. A hub that supports inline errors (spec section 2.3) would say which; " + m_Outbox.Count().ToString() + " envelope(s) buffered");
		SetLinkState("degraded");
		Delay(BACKOFF_MAX_MS);
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
		// A present v must be an integer equal to the version this plugin
		// speaks. A string, boolean, fraction, or wrong integer all name a
		// version this plugin cannot honor and are refused; only an absent v
		// (the negotiated version) or an exact integer match passes.
		if (version && (!version.IsNumber() || !version.m_IsInteger || version.m_Int != PROTOCOL_VERSION))
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

		// The id is JSON-quoted so an opaque id containing a newline stays one
		// record; a raw write would split it and let a later restart re-execute
		// the action (section 9.2). The log is append-only at runtime, never
		// truncated, so a crash cannot leave it half-rewritten; it is compacted
		// only at boot. A failed append is surfaced because it widens the
		// re-execution window the engine's lack of fsync already leaves open.
		if (!VyshkaFiles.AppendLine(VyshkaFiles.EXECUTED_PATH, VyshkaJson.Quote(actionId)))
			VyshkaLog.Warn("could not persist executed action id " + actionId + "; a crash before its dispatch is acked could re-execute it");
	}

	// LoadExecuted repopulates the LRU from disk on boot, keeping the most
	// recent ids up to the cap, and compacts an oversized log once, at boot,
	// where a truncating rewrite is safest.
	void LoadExecuted()
	{
		if (FileExist(VyshkaFiles.EXECUTED_PATH))
		{
			FileHandle probe = OpenFile(VyshkaFiles.EXECUTED_PATH, FileMode.READ);
			if (probe == 0)
			{
				VyshkaLog.Warn("executed-action log exists but could not be read; dedup history is unavailable and a re-delivered action may run again");
				return;
			}
			CloseFile(probe);
		}
		array<string> lines = VyshkaFiles.ReadLines(VyshkaFiles.EXECUTED_PATH);
		int start = 0;
		if (lines.Count() > EXECUTED_LRU_CAPACITY)
			start = lines.Count() - EXECUTED_LRU_CAPACITY;
		for (int i = start; i < lines.Count(); i++)
		{
			// Records are JSON-quoted strings. A line that does not parse as
			// one is tolerated as a bare id, so a log written in an earlier
			// raw-line format still deduplicates rather than being discarded.
			string line = lines.Get(i);
			VyshkaJsonValue parsed = VyshkaJson.Parse(line);
			string id = line;
			if (parsed && parsed.IsString())
				id = parsed.m_Text;
			if (!m_Executed.Contains(id))
			{
				m_Executed.Set(id, true);
				m_ExecutedOrder.Insert(id);
			}
		}
		if (lines.Count() > 2 * EXECUTED_LRU_CAPACITY)
			RewriteExecuted();
		if (m_ExecutedOrder.Count() > 0)
			VyshkaLog.Info("restored " + m_ExecutedOrder.Count().ToString() + " executed action id(s) from disk");
	}

	// RewriteExecuted replaces the log with exactly the current LRU contents,
	// each id JSON-quoted. Called only at boot.
	void RewriteExecuted()
	{
		string content = "";
		for (int i = 0; i < m_ExecutedOrder.Count(); i++)
			content += VyshkaJson.Quote(m_ExecutedOrder.Get(i)) + "\n";
		VyshkaFiles.WriteAll(VyshkaFiles.EXECUTED_PATH, content);
	}
}
