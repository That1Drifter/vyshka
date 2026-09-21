// Vyshka DayZ plugin: the link to the hub.
//
// One object drives the whole lifecycle of spec sections 5, 6, 7, 8 and 9:
// enroll once, start a session on every boot, long-poll forever, publish the
// manifest, execute dispatched actions behind an executed-actionId LRU,
// answer a hub asking what a custom context holds, flush events and
// snapshots into the outbox, keep unacked envelopes there, and renumber them
// across session changes.
//
// Everything runs on the script tick. A repeating call-queue timer wakes the
// plugin, and the transport's callbacks land on the same thread, so there is
// no locking anywhere. One request is in flight at a time, which is all a
// long-poll link needs: the poll is held open by the hub, and the plugin's
// own traffic rides in the next poll.

// VyshkaSnapshotChannel is the plugin's record of one state.* type: when it
// last captured a snapshot of that type and how many polls in a row went
// out without one. Each type is paced and held back on its own, because the
// hub acks and keeps them separately (spec section 8.3).
class VyshkaSnapshotChannel
{
	string m_Type;      // state.players, state.vehicles, state.entities
	int m_LastMs;       // monotonic time of the last capture; 0 before the first
	int m_Held;         // consecutive polls sent without a capture: the last snapshot unacked, or no room in the batch

	void VyshkaSnapshotChannel(string t)
	{
		m_Type = t;
	}
}

// VyshkaPendingDispatch is a dispatch whose action returned a pending
// outcome (VyshkaActionOutcome.Pending): acked, executed as far as it could
// be, and waiting for VyshkaPlugin.Complete to bring the result.
class VyshkaPendingDispatch
{
	string m_ActionId;
	string m_Code;
	int m_StartedMs;
	int m_DeadlineMs;   // monotonic; the dispatch's TTL or PENDING_MAX_MS, whichever ends first
}

class VyshkaPlugin : VyshkaResponseSink
{
	static ref VyshkaPlugin s_Instance;

	static const string PLUGIN_NAME = "vyshka-dayz";
	static const string PLUGIN_VERSION = "0.8.0";
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
	static const int SNAPSHOTS_HELD_LOG_AFTER = 6;   // consecutive polls sent with the last snapshot still unacked before the first log line (about a minute into an outage at the backoff cadence)
	static const int SNAPSHOTS_HELD_LOG_EVERY = 20;  // and then every this many (10 min at the backoff ceiling)

	// Every request asks for its refusals inline (spec section 2.3): the
	// engine delivers a non-2xx as an opaque code with no body, so this is
	// the only way the protocol's error codes reach the plugin. A hub that
	// predates the option ignores it, and the opaque handling below remains.
	static const string INLINE_ERRORS = "?errors=inline";

	static const string SNAPSHOT_PLAYERS = "state.players";
	static const string SNAPSHOT_VEHICLES = "state.vehicles";
	// The map markers a mod placed (VyshkaMapMarker); the third snapshot
	// type of spec section 8.3, paced like the other two.
	static const string SNAPSHOT_ENTITIES = "state.entities";

	// A context.enumerate requestId is hub-assigned and opaque, at most this
	// many code points (spec section 6.2); the reply echoes it.
	static const int REQUEST_ID_MAX = 128;
	// An actionId is hub-assigned and opaque with no length in the protocol
	// (section 7); the reference hub mints 26-character ids. The executed
	// log keeps each on a line and the engine reads by the line
	// (VyshkaFiles.LINE_MAX), and a byte escapes to at most six in the
	// quoted form, so an id of up to this many bytes is always safe to
	// keep on disk; a longer one is executed and remembered in memory
	// only, so its dedup does not survive a restart (MarkExecuted).
	static const int ACTION_ID_MAX = 8192;
	// A context.entries body is bounded as a snapshot body is (section 6.2):
	// 256 KiB, past which a hub refuses it whole.
	static const int CONTEXT_REPLY_MAX_BYTES = 262144;

	// A pending dispatch (an action waiting on the store) holds the next
	// poll back for up to PENDING_POLL_HOLD_MS, so its result rides the poll
	// that follows the dispatch instead of waiting out a held one; after
	// PENDING_MAX_MS with no completion it is failed, so a callback that
	// never comes cannot leave the hub waiting for the whole TTL.
	static const int PENDING_POLL_HOLD_MS = 3000;
	static const int PENDING_MAX_MS = 90000;

	static const int REQUEST_ENROLL = 1;
	static const int REQUEST_SESSION = 2;
	static const int REQUEST_POLL = 3;

	ref VyshkaConfig m_Config;
	ref VyshkaCredentials m_Credentials;
	ref VyshkaOutbox m_Outbox;
	ref VyshkaTransport m_Transport;
	ref VyshkaStoreClient m_Store;
	ref VyshkaRegistry m_Actions;
	int m_ManifestRevision;    // what this boot publishes (ResolveManifestRevision, ReconcileManifestRevision)
	ref VyshkaJsonValue m_ManifestJson;   // the manifest body without the revision, as this boot declares it
	string m_ManifestContent;  // the same, serialized compact: what is compared with the record's
	bool m_ManifestChanged;    // the revision was minted here (this boot or an earlier one) and no hub has been seen to accept it, so the number is not one the hub is known to hold; kept in the record as "pending"
	int m_HubRevisionSeen;     // server.manifestRevision from this session's response, -1 when absent
	int m_PublishedAbove;      // the hub revision (1 or more) reported when the pending revision was published, or -2 when it has not been published above a reported one; kept in the record as "above"
	ref VyshkaEventBuffer m_Events;
	ref map<string, ref VyshkaPendingDispatch> m_PendingDispatches;   // by actionId
	// An outcome delivered through Complete while its action's Execute was
	// still running (a store refusal answered on the spot, before the
	// dispatch could be recorded as pending): kept until Execute returns.
	ref map<string, ref VyshkaActionOutcome> m_EarlyOutcomes;
	// The action.result bodies the outbox could not hold when they were
	// ready, appended in order as room frees (AppendHeldResults).
	ref array<ref VyshkaJsonValue> m_HeldResults;
	ref VyshkaSnapshotSource m_Snapshots;
	bool m_SnapshotsOn;        // a source is wired and the configured interval is not 0
	ref array<ref VyshkaSnapshotChannel> m_SnapshotChannels;   // one per state.* type
	int m_NextSnapshotChannel;                                 // the channel that gets the first try at the next poll
	int m_LastFpsMs;           // monotonic time of the last core.server.fps sample, or of the start before the first
	int m_FramesSinceSample;   // mission update frames counted since then (OnFrame)

	string m_Executing;        // the actionId whose Execute is on the stack, "" otherwise
	int m_ExecutingDeadlineMs; // that dispatch's deadline, for the action to bound its own work by

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

	static void Start(VyshkaRegistry actions, VyshkaSnapshotSource snapshots)
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

	// StoreClient is the key/value client, or null before the plugin has
	// started or after it stopped (an action then fails with that as its
	// reason). A mod goes through a namespace-bound VyshkaStore handle
	// instead (GetVyshka().Store("my-mod")), which answers for itself when
	// there is no client.
	static VyshkaStoreClient StoreClient()
	{
		if (!s_Instance || !s_Instance.m_Running)
			return null;
		return s_Instance.m_Store;
	}

	// IsRunning says whether the plugin booted with a usable config and has
	// not stopped. It says nothing about the link: a running plugin with no
	// hub in reach buffers what it is given (LinkState says which).
	static bool IsRunning()
	{
		if (!s_Instance)
			return false;
		return s_Instance.m_Running;
	}

	// Complete delivers the outcome of a dispatch whose action returned
	// pending. An actionId that is not pending (already failed by the hold
	// running out, completed once, or never dispatched) is logged and
	// ignored: the hub has its answer or will get the timeout's.
	static void Complete(string actionId, VyshkaActionOutcome outcome)
	{
		if (!s_Instance)
			return;
		s_Instance.FinishPending(actionId, outcome);
	}

	// IsPending says whether a dispatch is still waiting for its outcome:
	// the one whose Execute is running counts, since it may go pending. An
	// action with work left after a failed or expired dispatch checks this
	// before doing anything on the dispatch's behalf.
	static bool IsPending(string actionId)
	{
		if (!s_Instance || actionId == "")
			return false;
		if (actionId == s_Instance.m_Executing)
			return true;
		return s_Instance.m_PendingDispatches.Contains(actionId);
	}

	// CurrentDeadlineMs is the monotonic deadline of the dispatch whose
	// Execute is on the stack: its TTL, or the pending hold, whichever ends
	// first. An action that will complete later bounds its own work by it.
	// Outside Execute it is the hold counted from now.
	static int CurrentDeadlineMs()
	{
		if (s_Instance && s_Instance.m_Executing != "")
			return s_Instance.m_ExecutingDeadlineMs;
		return VyshkaClock.MonotonicMs() + PENDING_MAX_MS;
	}

	void VyshkaPlugin()
	{
		m_RenewMarginSeconds = RENEW_MARGIN_SECONDS;
		m_ExecutedOrder = new array<string>;
		m_Executed = new map<string, bool>;
		m_LinkState = "buffering";
		m_HubRevisionSeen = -1;
		m_PublishedAbove = -2;
		m_PollTimeoutSeconds = 25;
		m_Events = new VyshkaEventBuffer();
		m_PendingDispatches = new map<string, ref VyshkaPendingDispatch>;
		m_EarlyOutcomes = new map<string, ref VyshkaActionOutcome>;
		m_HeldResults = new array<ref VyshkaJsonValue>;
		m_SnapshotChannels = new array<ref VyshkaSnapshotChannel>;
		m_SnapshotChannels.Insert(new VyshkaSnapshotChannel(SNAPSHOT_PLAYERS));
		m_SnapshotChannels.Insert(new VyshkaSnapshotChannel(SNAPSHOT_VEHICLES));
		m_SnapshotChannels.Insert(new VyshkaSnapshotChannel(SNAPSHOT_ENTITIES));
	}

	void Boot(VyshkaRegistry actions, VyshkaSnapshotSource snapshots)
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
		if (!m_Transport.Init(m_Config.m_HubUrl + "/plugin/v1/", this))
			return;

		// The store client has a transport of its own so a key/value call
		// never waits behind a held poll (VyshkaStoreClient). It is confined
		// to the namespaces the manifest declares: the plugin's own and
		// whatever the mods registered (VyshkaRegistry.DeclareNamespace).
		array<string> namespaces = m_Actions.Namespaces();
		m_Store = new VyshkaStoreClient();
		if (!m_Store.Init(m_Config.m_HubUrl, namespaces))
			return;
		ApplyReadTimeout(m_Config.m_PollTimeoutSeconds);

		// The revision the manifest publishes is derived from what the
		// registry holds, so a boot that changed nothing republishes the
		// same revision and the hub keeps what it has (spec section 6.1).
		m_ManifestJson = m_Actions.Manifest(m_Config.m_Game, PLUGIN_NAME, PLUGIN_VERSION);
		m_ManifestContent = m_ManifestJson.Serialize();
		m_ManifestRevision = ResolveManifestRevision(m_ManifestContent);

		m_Running = true;
		m_SnapshotsOn = m_Config.m_SnapshotIntervalSeconds > 0 && m_Snapshots;
		m_LastFpsMs = VyshkaClock.MonotonicMs();
		GetGame().GetCallQueue(CALL_CATEGORY_SYSTEM).CallLater(Tick, TICK_MS, true);
		string snapshotNote = "snapshots off";
		if (m_SnapshotsOn)
			snapshotNote = "state.players, state.vehicles and state.entities with each poll, at least " + m_Config.m_SnapshotIntervalSeconds.ToString() + " s apart";
		string fpsNote = "fps samples off";
		if (m_Config.m_FpsIntervalSeconds > 0)
			fpsNote = "core.server.fps every " + m_Config.m_FpsIntervalSeconds.ToString() + " s";
		int actionCount = m_Actions.Count();
		int contextCount = m_Actions.ContextCount();
		VyshkaLog.Info("started; hub " + m_Config.m_HubUrl + ", " + actionCount.ToString() + " action(s) and " + contextCount.ToString() + " custom context(s) declared at manifest revision " + m_ManifestRevision.ToString() + ", " + snapshotNote + ", " + fpsNote);
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
		// A dispatch still waiting on its action is answered now, so the
		// next boot delivers a failure rather than leaving the hub to time
		// the action out; the store's own callbacks are failed first, so no
		// completion arrives after this.
		m_Store.Shutdown();
		FailPending("the server stopped before the action finished");
		FlushEvents();
		AppendHeldResults();
		if (m_HeldResults.Count() > 0)
			VyshkaLog.Error(m_HeldResults.Count().ToString() + " action result(s) the outbox could not hold are lost with this stop; the hub will expire those actions");
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
		// Events go into the outbox whether or not a request is in flight;
		// whatever is queued rides the next poll. Snapshots are captured by
		// the poll itself (PublishSnapshots), so they never wait for one.
		SampleFps();
		// Results held back for room go first: they were owed before any
		// event, and the events' flush leaves their slots alone.
		AppendHeldResults();
		QueueManifest();
		ExpirePending();
		if (m_Events.Due())
			FlushEvents();
		// The store's own transport: its requests go out whether or not a
		// poll is in flight, which is the point of it having one.
		m_Store.Tick(m_SessionToken);
		m_Transport.CheckWatchdog();
		if (m_Transport.IsInFlight())
			return;
		if (VyshkaClock.MonotonicMs() < m_NextAttemptMs)
			return;
		Advance();
	}

	// ---- telemetry (spec section 8) ----

	// OnFrame counts the mission's update frames, from which SampleFps
	// derives the server's frame rate. The engine's own GetFps() reads 0.1
	// on a dedicated server (measured on DayZ 1.29, issue #59), so the rate
	// is measured here instead: frames between two samples over the wall
	// time between them.
	static void OnFrame(float timeslice)
	{
		if (!s_Instance || !s_Instance.m_Running)
			return;
		s_Instance.m_FramesSinceSample++;
	}

	// SampleFps emits core.server.fps, the periodic performance sample of
	// section 8.1, every fpsIntervalSeconds: the server's measured frame
	// rate over the interval and how many players it is simulating for. The
	// first sample is one interval after start, because the rate during boot
	// says nothing about the server.
	void SampleFps()
	{
		int interval = m_Config.m_FpsIntervalSeconds;
		if (interval <= 0)
			return;
		int now = VyshkaClock.MonotonicMs();
		int elapsed = now - m_LastFpsMs;
		if (elapsed < interval * 1000)
			return;
		float rate = m_FramesSinceSample * 1000.0 / elapsed;
		m_LastFpsMs = now;
		m_FramesSinceSample = 0;
		VyshkaJsonValue fps = VyshkaJsonValue.NewFloat(Math.Round(rate * 10) / 10);
		if (!fps)
			return;
		array<Man> men = new array<Man>;
		GetGame().GetPlayers(men);
		VyshkaJsonValue data = VyshkaJsonValue.NewObject();
		data.Set("fps", fps);
		data.Set("players", VyshkaJsonValue.NewInt(men.Count()));
		Emit("core.server.fps", data);
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
			VyshkaJsonValue body = m_Events.TakeBatch();
			// The slots held for pending results (HandleDispatch) are not
			// the events' to take: a batch that would eat into them is
			// dropped, and counted, as one the outbox could not hold.
			VyshkaOutboxEntry stored = null;
			if (!m_Outbox.HasRoom(1 + ReservedResults()))
				m_Outbox.Refuse("event.batch");
			else
				stored = m_Outbox.Append("event.batch", body, count);
			if (!stored)
				VyshkaLog.Warn("dropped " + count.ToString() + " event(s) the outbox could not hold");
		}
	}

	// QueueManifest appends the manifest.publish once per boot, at the first
	// session start with room for it, or on a later tick when the outbox was
	// full then (a boot that restored a full outbox). It leaves the slots
	// held for owed results alone like everything else that is not one.
	void QueueManifest()
	{
		if (m_ManifestQueued || m_SessionToken == "")
			return;
		if (!m_Outbox.HasRoom(1 + ReservedResults()))
			return;
		VyshkaJsonValue body = m_Actions.ManifestBody(m_Config.m_Game, PLUGIN_NAME, PLUGIN_VERSION, m_ManifestRevision);
		if (m_Outbox.Append("manifest.publish", body))
		{
			m_ManifestQueued = true;
			// A pending revision going out above what the hub reported is
			// remembered as such, in the record too, so a later session
			// reporting this number reads as the hub having got there
			// through this publish (ReconcileManifestRevision), across a
			// restart as well. A hub that reported nothing is not known
			// to have stood below: it may hold none, or predate the field
			// and hold anything, so nothing is remembered and the next
			// equal report is taken for a collision and published above.
			if (m_ManifestChanged && m_HubRevisionSeen >= 1 && m_HubRevisionSeen < m_ManifestRevision)
			{
				m_PublishedAbove = m_HubRevisionSeen;
				SaveManifestRecord(m_ManifestRevision, true, m_PublishedAbove);
			}
		}
	}

	// ResolveManifestRevision derives the revision this boot publishes with
	// and remembers it at VyshkaFiles.MANIFEST_PATH, beside the content it
	// went with: a boot whose actions, contexts, events and namespaces are
	// the ones the file records republishes the same revision, which the hub
	// ignores as the duplicate it is (spec section 6.1), and any change to
	// what the plugin declares takes a new one.
	//
	// A changed manifest takes the stored revision plus one, or the current
	// epoch second when that is larger. The epoch seed matters after the
	// profile directory is wiped or moved: the hub keeps the revision it
	// stored and ignores anything at or below it, so a plugin starting over
	// from 1 would never be heard again, while a revision seeded from the
	// clock is above whatever any earlier boot could have sent. Epoch
	// seconds fit the engine's 32-bit script int until 2038, and stay well
	// inside the [1, 2^53) the spec allows.
	int ResolveManifestRevision(string content)
	{
		int stored = 0;
		string storedContent = "";
		bool storedPending = false;
		int storedAbove = -2;
		string why;
		// The record is read as lines and parsed in place (VyshkaFiles.ReadJson);
		// it is not read into one string and scrubbed of newlines first: with
		// a Replace of the newline here, the string read back measured 0
		// bytes on every boot on DayZ 1.29 (2026-09-18, issue #72).
		VyshkaManifestRecord record = VyshkaManifestRecord.Load(why);
		if (record)
		{
			stored = record.m_Revision;
			storedContent = record.m_Content;
			storedPending = record.m_Pending;
			storedAbove = record.m_Above;
			int storedLength = storedContent.Length();
			int contentLength = content.Length();
			why = "the record at revision " + stored.ToString() + " holds " + storedLength.ToString() + " bytes of content and this boot declares " + contentLength.ToString();
		}
		// A manifest.publish an earlier boot left in the outbox goes to the
		// hub before anything this boot appends, and the hub applies it
		// first: a revision minted now has to lie above it, or the hub
		// would keep that earlier content at the number this boot claims.
		int retained = m_Outbox.HighestManifestRevision();
		if (stored > 0 && storedContent == content && stored > retained)
		{
			// A record still pending was minted by an earlier boot that no
			// hub was seen to accept (an outage, a restart before the next
			// session), and its number stays a guess until one is
			// (ReconcileManifestRevision).
			m_ManifestChanged = storedPending;
			if (storedPending)
				m_PublishedAbove = storedAbove;
			return stored;
		}
		if (stored > 0 && storedContent == content)
			why = "the outbox still holds a manifest.publish at revision " + retained.ToString() + ", not below the record's " + stored.ToString();
		m_ManifestChanged = true;
		int revision = stored + 1;
		if (retained + 1 > revision)
			revision = retained + 1;
		int now = VyshkaClock.EpochSeconds();
		if (now > revision)
			revision = now;
		m_PublishedAbove = -2;
		SaveManifestRecord(revision, true, -2);
		VyshkaLog.Info("publishing manifest revision " + revision.ToString() + ": " + why);
		return revision;
	}

	// SaveManifestRecord writes the revision and the content this boot
	// declares (m_ManifestJson) to VyshkaFiles.MANIFEST_PATH, so the next
	// boot can tell whether it changed anything. pending says the revision
	// was minted here and no hub has yet been seen to accept it
	// (ReconcileManifestRevision clears it), so a restart in between keeps
	// treating the number as a guess; above, when the pending revision has
	// been published above a hub revision the session reported, is that
	// revision, which is what lets a later session's report be read as
	// acceptance; a hub that reported none leaves no mark, since it is not
	// known to have stood below.
	void SaveManifestRecord(int revision, bool pending, int above)
	{
		if (!VyshkaManifestRecord.Save(revision, m_ManifestJson, pending, above))
			VyshkaLog.Warn("could not write " + VyshkaFiles.MANIFEST_PATH + "; every boot will publish a new manifest revision, even one that changed nothing");
	}

	// ReconcileManifestRevision reads the revision the hub says it holds
	// (server.manifestRevision in the session response, spec section 5.3)
	// and moves this plugin's above it when it is not. The hub ignores a
	// publish at an equal or lower revision and says nothing (section 6.1),
	// so without this a record lost with the profile directory, or a clock
	// set back, could leave the plugin publishing below the hub forever.
	// Equal means the hub holds this boot's own publish, or a boot that
	// declared the same, and needs nothing; the manifest is queued at
	// session start regardless, and an equal revision costs one ignored
	// envelope.
	void ReconcileManifestRevision(VyshkaJsonValue session)
	{
		m_HubRevisionSeen = -1;
		VyshkaJsonValue server = session.Get("server");
		if (!server || !server.IsObject())
			return;
		VyshkaJsonValue held = server.Get("manifestRevision");
		if (!held || !held.IsNumber())
			return;
		if (!held.m_IsInteger)
		{
			// A revision past the engine's int (the parser saturates) is one
			// this plugin cannot publish above; the operator has to see it.
			VyshkaLog.Error("the hub holds manifest revision " + held.m_Text + ", beyond what this plugin's 32-bit revision can exceed; the manifest cannot be updated from here");
			return;
		}
		int hubRevision = held.m_Int;
		m_HubRevisionSeen = hubRevision;
		if (hubRevision < m_ManifestRevision)
			return;
		if (hubRevision == m_ManifestRevision)
		{
			// The hub reports this plugin's number. For a revision the hub
			// was seen to accept (the record is not pending) that is this
			// content. For one minted here it is acceptance only when this
			// process published it above what the hub reported before: the
			// hub then got from below to this number through that publish
			// (a retained earlier publish at the same number is ruled out
			// by ResolveManifestRevision, which mints above the outbox).
			// Otherwise the equal number is a collision (a restored record
			// one below the hub's, say, with the clock behind), and this
			// content goes out above it. An ack is not acceptance: the hub
			// acks a manifest it rejected as well (section 6.4), so only
			// its own report of what it holds confirms.
			if (!m_ManifestChanged)
				return;
			if (m_PublishedAbove != -2 && m_PublishedAbove < hubRevision)
			{
				int above = m_PublishedAbove;
				m_ManifestChanged = false;
				m_PublishedAbove = -2;
				SaveManifestRecord(m_ManifestRevision, false, -2);
				VyshkaLog.Info("the hub holds manifest revision " + hubRevision.ToString() + ", the one published above its earlier " + above.ToString() + "; accepted");
				return;
			}
		}
		int revision = hubRevision + 1;
		if (revision <= hubRevision)
		{
			VyshkaLog.Error("the hub holds manifest revision " + hubRevision.ToString() + ", the largest this plugin can represent; the manifest cannot be updated from here");
			return;
		}
		VyshkaLog.Info("the hub holds manifest revision " + hubRevision.ToString() + ", not below this plugin's " + m_ManifestRevision.ToString() + "; publishing at " + revision.ToString());
		m_ManifestRevision = revision;
		m_ManifestChanged = true;
		m_PublishedAbove = -2;
		SaveManifestRecord(revision, true, -2);
		// A publish already queued this process carries the old number; the
		// corrected body has to go out as well.
		m_ManifestQueued = false;
	}


	// ReservedResults is how many outbox slots are spoken for by results not
	// yet appended: one per pending dispatch, one per result held back
	// because the outbox was full when it was ready.
	int ReservedResults()
	{
		return m_PendingDispatches.Count() + m_HeldResults.Count();
	}

	// AppendHeldResults retries the results the outbox could not hold when
	// they were ready, in order, as acks free room.
	void AppendHeldResults()
	{
		while (m_HeldResults.Count() > 0)
		{
			if (!m_Outbox.Append("action.result", m_HeldResults.Get(0)))
				return;
			m_HeldResults.RemoveOrdered(0);
			VyshkaLog.Info("a held action result was stored in the outbox; " + m_HeldResults.Count().ToString() + " still held");
		}
	}

	// PublishSnapshots captures one envelope per state.* type as the poll
	// that will carry them is being built, so each sample is as fresh as the
	// link allows: a snapshot captured on a timer would sit in the outbox
	// until the poll already in flight returned, up to a full pollTimeout
	// later. The configured interval is a floor on the spacing between
	// captures, and the poll cycle sets the cadence when it is longer
	// (issue #55). The types share the interval and are paced separately,
	// because the hub acks and keeps them separately (section 8.3). The
	// type that captured last goes last next time: a batch shrunk to one
	// envelope (a bad_request refusal) would otherwise give the first type
	// the only slot on every poll and starve the rest.
	void PublishSnapshots()
	{
		if (!m_SnapshotsOn)
			return;
		// The traversal starts from where the cursor stood before this
		// poll; the cursor itself moves as channels append, so it must not
		// be the loop's base or a channel would be visited twice.
		int count = m_SnapshotChannels.Count();
		int start = m_NextSnapshotChannel;
		for (int i = 0; i < count; i++)
		{
			int at = (start + i) % count;
			if (PublishSnapshot(m_SnapshotChannels.Get(at)))
				m_NextSnapshotChannel = (at + 1) % count;
		}
	}

	// PublishSnapshot captures one type. No capture is made while the
	// previous snapshot of that type is still unacked, or while the outbox
	// holds more than this poll can carry, so that a snapshot appended now
	// could not ride it: the map wants the latest state, an appended
	// envelope is immutable (section 9.3), and a queue of stale snapshots
	// behind an outage serves no one (section 8.3 keeps the latest per type
	// regardless). On a healthy link the previous snapshot's ack arrived
	// with the response that ended the last poll, so a held capture means
	// an outage, or a backlog of anything (a burst of action results
	// counts), and a run of them long enough to matter is logged. Returns
	// whether a snapshot was appended.
	bool PublishSnapshot(VyshkaSnapshotChannel channel)
	{
		if (m_Outbox.HasUnacked(channel.m_Type) || !m_Outbox.RoomInBatch())
		{
			channel.m_Held++;
			if (channel.m_Held == SNAPSHOTS_HELD_LOG_AFTER || channel.m_Held % SNAPSHOTS_HELD_LOG_EVERY == 0)
				VyshkaLog.Info(channel.m_Type + " snapshot held back: the previous one is still unacked or the outbox has a backlog (" + channel.m_Held.ToString() + " poll(s) without one)");
			return false;
		}
		channel.m_Held = 0;
		int now = VyshkaClock.MonotonicMs();
		if (channel.m_LastMs != 0 && now - channel.m_LastMs < m_Config.m_SnapshotIntervalSeconds * 1000)
			return false;
		VyshkaJsonValue body = null;
		if (channel.m_Type == SNAPSHOT_PLAYERS)
			body = m_Snapshots.CapturePlayers();
		else if (channel.m_Type == SNAPSHOT_VEHICLES)
			body = m_Snapshots.CaptureVehicles();
		else if (channel.m_Type == SNAPSHOT_ENTITIES)
			body = m_Snapshots.CaptureEntities();
		if (!body)
			return false;
		if (!m_Outbox.Append(channel.m_Type, body))
		{
			// The entities capture only speaks when a marker exists or one
			// was removed since the last capture, and it forgets the removal
			// as it captures; a body the outbox refused was never sent, so
			// the removal is owed again or the hub keeps a marker that is
			// gone.
			if (channel.m_Type == SNAPSHOT_ENTITIES)
				VyshkaMapMarkers.Unpublished();
			return false;
		}
		channel.m_LastMs = now;
		return true;
	}

	// Advance issues whichever request the link needs next. The name matters:
	// a method named Step is shadowed by an engine built-in and never runs.
	void Advance()
	{
		if (!m_Running || m_Transport.IsInFlight())
			return;

		// A dispatch whose action is still working (on the store, a round
		// trip away) gets a moment to finish, so its result rides the poll
		// that follows the dispatch instead of waiting out a held one. The
		// hold is short and bounded: past it the poll goes, and the result
		// rides the next.
		if (YoungestPendingMs() < PENDING_POLL_HOLD_MS)
		{
			Delay(50);
			return;
		}

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

	// ApplyReadTimeout sets the engine's process-wide read timeout for a
	// pollTimeout (the hold plus 5 s of slack) and keeps the store client's
	// watchdog above it plus the engine's 10 s connection timeout: a request
	// the watchdog abandoned while the engine still ran it would share the
	// context with the next one (VyshkaStore).
	void ApplyReadTimeout(int pollTimeoutSeconds)
	{
		m_Transport.SetReadTimeout(pollTimeoutSeconds + 5);
		if (m_Store)
			m_Store.SetBudgetMs((pollTimeoutSeconds + 5 + 10 + 5) * 1000);
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
		// The snapshots are captured here, not on the tick, so they ride this
		// very request rather than waiting behind a held poll.
		PublishSnapshots();
		string body = "{\"ack\":" + m_InAck.ToString() + ",\"envelopes\":" + m_Outbox.BatchJson() + "}";
		// The hub answers within pollTimeout; the engine's read timeout is
		// pollTimeout + 5 s; the watchdog sits behind both.
		m_Transport.Post(REQUEST_POLL, "poll" + INLINE_ERRORS, m_SessionToken, body, (m_PollTimeoutSeconds + 10) * 1000);
	}

	// ---- responses ----

	override void OnResponse(int kind, bool ok, int code, string data)
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
		ApplyReadTimeout(m_PollTimeoutSeconds);
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
		// The hub's own word on the revision it holds, before the manifest
		// for this session is queued below.
		ReconcileManifestRevision(root);

		QueueManifest();

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
			ApplyReadTimeout(m_PollTimeoutSeconds);
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
				// A pending dispatch has its result still to append, and
				// a held result its slot, so those are held here against
				// everything dispatched after them.
				if (!m_Outbox.HasRoom(2 + ReservedResults()))
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
		else if (envelopeType == "context.enumerate")
			HandleContextEnumerate(body);
		else if (envelopeType == "manifest.reject")
			HandleManifestReject(body);
		else if (envelopeType == "event.reject" || envelopeType == "state.reject")
			HandleTelemetryReject(envelopeType, body);
		// Anything else is acked and ignored (spec section 4).
	}

	// HandleContextEnumerate answers a hub asking for the members of one
	// custom context (spec section 6.2) with a context.entries reply that
	// echoes the request. A context this plugin does not declare is answered
	// too, with an empty list and a reason, so the hub learns that it and the
	// manifest disagree instead of waiting; it is not a fault of the link.
	// Nothing here is deduplicated: an enumeration is a read of what is, so
	// answering the same request twice changes nothing.
	void HandleContextEnumerate(VyshkaJsonValue body)
	{
		if (!body || !body.IsObject())
		{
			VyshkaLog.Warn("ignoring a context.enumerate with an unusable body");
			return;
		}
		VyshkaJsonValue requestIdValue = body.Get("requestId");
		if (!requestIdValue || !requestIdValue.IsString() || requestIdValue.m_Text == "")
		{
			// Without the id there is nothing the hub could match an answer
			// to, so there is no answer worth sending.
			VyshkaLog.Warn("ignoring a context.enumerate without a requestId");
			return;
		}
		string requestId = VyshkaAction.Bound(requestIdValue.m_Text, REQUEST_ID_MAX);
		string contextId = VyshkaAction.Bound(body.GetString("context", ""), VyshkaRegistry.CONTEXT_ID_MAX);

		VyshkaJsonValue answer = VyshkaJsonValue.NewObject();
		answer.Set("requestId", VyshkaJsonValue.NewString(requestId));
		answer.Set("context", VyshkaJsonValue.NewString(contextId));
		VyshkaContext context = m_Actions.FindContext(contextId);
		if (!context)
		{
			answer.Set("entries", VyshkaJsonValue.NewArray());
			answer.Set("reason", VyshkaJsonValue.NewString("this plugin declares no context " + contextId));
		}
		else
		{
			VyshkaContextList list = new VyshkaContextList();
			context.Enumerate(list);
			int dropped = list.Dropped();
			if (dropped > 0)
			{
				// A reply carries at most 5000 entries (section 6.2). A list
				// cut at the cap would read as complete, so a context past it
				// is answered with none and a reason, the same answer an
				// oversized body gets below.
				int offered = list.Count() + dropped;
				VyshkaLog.Warn("context " + contextId + " offered " + offered.ToString() + " members, more than the " + VyshkaContextList.MAX_ENTRIES.ToString() + " a reply may carry (spec section 6.2); answering with none");
				answer.Set("entries", VyshkaJsonValue.NewArray());
				answer.Set("reason", VyshkaJsonValue.NewString("the members of context " + contextId + " do not fit in one reply"));
			}
			else
			{
				VyshkaJsonValue entries = list.ToJson();
				answer.Set("entries", entries);
			}
		}
		string answerBody = answer.Serialize();
		// A reply is bounded in bytes as a snapshot is (section 6.2), and a
		// hub refuses one over the bound whole. An empty list with a reason
		// still tells the hub something; a refused reply tells it nothing.
		int answerBytes = answerBody.Length();
		if (answerBytes > CONTEXT_REPLY_MAX_BYTES)
		{
			VyshkaLog.Warn("the entries of context " + contextId + " serialize to " + answerBytes.ToString() + " bytes, over the " + CONTEXT_REPLY_MAX_BYTES.ToString() + " a reply may carry (spec section 6.2); answering with none");
			answer.Set("entries", VyshkaJsonValue.NewArray());
			answer.Set("reason", VyshkaJsonValue.NewString("the members of context " + contextId + " do not fit in one reply"));
		}
		if (!m_Outbox.Append("context.entries", answer))
			VyshkaLog.Warn("the outbox could not hold the entries of context " + contextId + " for request " + requestId + "; a hub that still wants them will ask again");
	}

	// HandleTelemetryReject surfaces a refused event.batch or state.*
	// envelope (spec sections 8.1 and 8.3). The refusal is envelope-level
	// success: the batch was acked and its events, or the snapshot, are gone,
	// and this log line is the visible counter section 9.4 asks for. It is
	// never a transport error and changes nothing about the session.
	void HandleTelemetryReject(string envelopeType, VyshkaJsonValue body)
	{
		string what = "an event.batch";
		if (envelopeType == "state.reject")
			what = "a state snapshot";
		if (!body || !body.IsObject())
		{
			VyshkaLog.Error("the hub refused " + what + " (no details given); what it carried is not stored");
			return;
		}
		VyshkaLog.Error("the hub refused " + what + " (envelope " + body.GetString("envelopeId", "?") + "); what it carried is not stored:");
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

		VyshkaJsonValue ackBody = VyshkaJsonValue.NewObject();
		ackBody.Set("actionId", VyshkaJsonValue.NewString(actionId));
		m_Outbox.Append("action.ack", ackBody);

		string code = body.GetString("code", "");
		string context = body.GetString("context", "");
		string referenceKey = body.GetString("referenceKey", "");

		int deadline = 0;
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
		// The deadline an action that completes later works under: the
		// dispatch's own TTL when it is shorter than the pending hold, so
		// nothing lands after the hub has expired the action.
		int deadlineMs = started + PENDING_MAX_MS;
		if (deadline > 0)
		{
			int ttlSeconds = deadline - VyshkaClock.EpochSeconds();
			if (ttlSeconds < PENDING_MAX_MS / 1000)
				deadlineMs = started + ttlSeconds * 1000;
		}
		m_Executing = actionId;
		m_ExecutingDeadlineMs = deadlineMs;
		VyshkaActionOutcome outcome = m_Actions.Execute(actionId, code, context, referenceKey, body.Get("params"));
		m_Executing = "";
		if (!outcome)
			outcome = VyshkaActionOutcome.Failure("the action produced no outcome");
		if (outcome.m_Pending && m_EarlyOutcomes.Contains(actionId))
		{
			// The action completed itself before Execute returned (a store
			// call refused on the spot): that outcome is the result.
			outcome = m_EarlyOutcomes.Get(actionId);
			m_EarlyOutcomes.Remove(actionId);
		}
		if (outcome.m_Pending)
		{
			// The action has more to do (a store round trip) before it can
			// say how it went. The ack is out; the result follows through
			// Complete, or the hold runs out and FailPending answers.
			VyshkaPendingDispatch pending = new VyshkaPendingDispatch();
			pending.m_ActionId = actionId;
			pending.m_Code = code;
			pending.m_StartedMs = started;
			pending.m_DeadlineMs = deadlineMs;
			m_PendingDispatches.Set(actionId, pending);
			VyshkaLog.Info("action " + code + " (" + actionId + ") is pending");
			return;
		}
		AppendResult(actionId, code, started, outcome);
	}

	// AppendResult writes the action.result for a dispatch, whether the
	// outcome came back from Execute at once or through Complete later.
	void AppendResult(string actionId, string code, int started, VyshkaActionOutcome outcome)
	{
		int durationMs = VyshkaClock.MonotonicMs() - started;
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
		// Room for this result was checked at dispatch and held for a
		// pending one (HandleDispatch, FlushEvents). Should the outbox still
		// refuse it, the result is kept and appended as soon as an ack frees
		// room (AppendHeldResults) rather than lost: the executed-id dedup
		// means a re-delivery could never produce it again.
		if (!m_Outbox.Append("action.result", result))
		{
			m_HeldResults.Insert(result);
			VyshkaLog.Warn("the outbox could not hold the result of action " + actionId + " (" + code + "); holding it until room frees");
		}

		if (outcome.m_Ok)
			VyshkaLog.Info("action " + code + " (" + actionId + ") completed in " + durationMs.ToString() + " ms");
		else
			VyshkaLog.Info("action " + code + " (" + actionId + ") failed: " + outcome.m_Error);
	}

	// FinishPending answers a pending dispatch with the outcome its action
	// brought. Pending is checked before anything else, so a completion for
	// an unknown or already-answered actionId changes nothing.
	void FinishPending(string actionId, VyshkaActionOutcome outcome)
	{
		VyshkaPendingDispatch pending = m_PendingDispatches.Get(actionId);
		if (!pending)
		{
			if (actionId != "" && actionId == m_Executing)
			{
				// Execute is still on the stack for this dispatch; the
				// outcome waits for it to return (HandleDispatch).
				m_EarlyOutcomes.Set(actionId, outcome);
				return;
			}
			VyshkaLog.Warn("a completion arrived for action " + actionId + ", which is not pending; ignored");
			return;
		}
		m_PendingDispatches.Remove(actionId);
		if (!outcome)
			outcome = VyshkaActionOutcome.Failure("the action produced no outcome");
		if (outcome.m_Pending)
			outcome = VyshkaActionOutcome.Failure("the action completed with a pending outcome");
		AppendResult(actionId, pending.m_Code, pending.m_StartedMs, outcome);
	}

	// ExpirePending fails every dispatch pending past its deadline (its TTL
	// or PENDING_MAX_MS): a store client that gives up answers its callback
	// ahead of this, so this is the guard against a callback that never
	// comes at all.
	void ExpirePending()
	{
		if (m_PendingDispatches.Count() == 0)
			return;
		int now = VyshkaClock.MonotonicMs();
		array<string> expired = new array<string>;
		for (int i = 0; i < m_PendingDispatches.Count(); i++)
		{
			if (now > m_PendingDispatches.GetElement(i).m_DeadlineMs)
				expired.Insert(m_PendingDispatches.GetKey(i));
		}
		for (int j = 0; j < expired.Count(); j++)
			FinishPending(expired.Get(j), VyshkaActionOutcome.Failure("the action did not finish within its deadline"));
	}

	// FailPending answers every pending dispatch with one failure.
	void FailPending(string error)
	{
		array<string> ids = new array<string>;
		for (int i = 0; i < m_PendingDispatches.Count(); i++)
			ids.Insert(m_PendingDispatches.GetKey(i));
		for (int j = 0; j < ids.Count(); j++)
			FinishPending(ids.Get(j), VyshkaActionOutcome.Failure(error));
	}

	// YoungestPendingMs is the age of the most recent pending dispatch, or
	// a very large number when none is pending.
	int YoungestPendingMs()
	{
		if (m_PendingDispatches.Count() == 0)
			return int.MAX;
		int now = VyshkaClock.MonotonicMs();
		int youngest = int.MAX;
		for (int i = 0; i < m_PendingDispatches.Count(); i++)
		{
			int age = now - m_PendingDispatches.GetElement(i).m_StartedMs;
			if (age < youngest)
				youngest = age;
		}
		return youngest;
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
		if (actionId.Length() > ACTION_ID_MAX)
		{
			VyshkaLog.Warn("action " + actionId.Length().ToString() + " bytes of actionId long, over the " + ACTION_ID_MAX.ToString() + " the executed log keeps on a line; it is executed, and a re-delivery after a restart could execute it again");
			return;
		}
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
		array<string> lines = new array<string>;
		for (int i = 0; i < m_ExecutedOrder.Count(); i++)
			lines.Insert(VyshkaJson.Quote(m_ExecutedOrder.Get(i)) + "\n");
		VyshkaFiles.WriteAll(VyshkaFiles.EXECUTED_PATH, VyshkaJsonWriter.JoinPieces(lines));
	}
}
