// Vyshka spike: outbox crash load generator.
//
// Appended to the init.c of a mission run with the Vyshka mod loaded, and
// started from main() with VyshkaOutboxLoad.Run(). Once the plugin reports
// its link connected, the generator emits numbered spike.load events at a
// fixed rate through the plugin's own Emit, so every event takes the same
// path a real one does: the event buffer, the 2 s or 200-event flush, an
// outbox record on disk, the next poll, the hub's ack.
//
// The runner (../runner) kills the server at a random moment, reads what
// the outbox holds on disk, restarts the server, and counts what the hub
// received against what this generator says it emitted. Three sources say
// how far the generator got, each with its own durability:
//
//   the script log      one VYSHKA_LOAD line per tick, through Print
//   the marker file     $profile:VyshkaSpike/emitted.txt, rewritten every
//                       tick through the same OpenFile/FPrint/CloseFile the
//                       outbox uses
//   the event payloads  { "run": <boot epoch>, "n": <1, 2, 3...> }
//
// Line format (tab separated):
//   VYSHKA_LOAD<TAB>armed<TAB>run=<epoch><TAB>tickMs=<ms><TAB>perTick=<n>
//   VYSHKA_LOAD<TAB>started<TAB>run=<epoch><TAB>t=<game ms>
//   VYSHKA_LOAD<TAB>emitted<TAB>run=<epoch><TAB>first=<n><TAB>last=<n><TAB>t=<game ms>
//
// Everything runs server side; no game client is required.

class VyshkaOutboxLoad
{
	static ref VyshkaOutboxLoad s_Instance;

	static const string DIR = "$profile:VyshkaSpike";
	static const string CONFIG_PATH = "$profile:VyshkaSpike/load.json";
	static const string MARKER_PATH = "$profile:VyshkaSpike/emitted.txt";

	int m_Run;        // this boot's identity in every payload: epoch seconds at arm time
	int m_N;          // events emitted so far this boot
	int m_TickMs;     // generator period
	int m_PerTick;    // events per period
	bool m_Started;   // the link has been seen connected and the load is running

	static void Run()
	{
		if (s_Instance)
			return;
		s_Instance = new VyshkaOutboxLoad();
		s_Instance.Start();
	}

	void Start()
	{
		if (!FileExist(DIR))
			MakeDirectory(DIR);
		m_TickMs = 100;
		m_PerTick = 5;
		VyshkaJsonValue config = VyshkaFiles.ReadJson(CONFIG_PATH);
		if (config && config.IsObject())
		{
			m_TickMs = config.GetInt("tickMs", m_TickMs);
			m_PerTick = config.GetInt("perTick", m_PerTick);
		}
		if (m_TickMs < 20)
			m_TickMs = 20;
		// perTick 0 is a boot that only drains the outbox: no load at all.
		if (m_PerTick < 0)
			m_PerTick = 0;
		if (m_PerTick > 500)
			m_PerTick = 500;
		m_Run = VyshkaClock.EpochSeconds();
		m_N = 0;
		m_Started = false;
		// The marker says "nothing yet" for this run before the first tick,
		// so a kill during boot reads as zero emitted rather than as the
		// previous boot's count.
		VyshkaFiles.WriteAll(MARKER_PATH, m_Run.ToString() + " 0");
		Print("VYSHKA_LOAD\tarmed\trun=" + m_Run.ToString() + "\ttickMs=" + m_TickMs.ToString() + "\tperTick=" + m_PerTick.ToString());
		GetGame().GetCallQueue(CALL_CATEGORY_SYSTEM).CallLater(Tick, m_TickMs, true);
	}

	void Tick()
	{
		if (!m_Started)
		{
			// Emit drops events before the plugin runs; counting those as
			// emitted would report a loss the outbox never saw.
			if (VyshkaPlugin.LinkState() != "connected")
				return;
			m_Started = true;
			Print("VYSHKA_LOAD\tstarted\trun=" + m_Run.ToString() + "\tt=" + GetGame().GetTime().ToString());
		}
		if (m_PerTick == 0)
			return;
		int first = m_N + 1;
		for (int i = 0; i < m_PerTick; i++)
		{
			m_N++;
			VyshkaJsonValue data = VyshkaJsonValue.NewObject();
			data.Set("run", VyshkaJsonValue.NewInt(m_Run));
			data.Set("n", VyshkaJsonValue.NewInt(m_N));
			VyshkaPlugin.Emit("spike.load", data);
		}
		VyshkaFiles.WriteAll(MARKER_PATH, m_Run.ToString() + " " + m_N.ToString());
		Print("VYSHKA_LOAD\temitted\trun=" + m_Run.ToString() + "\tfirst=" + first.ToString() + "\tlast=" + m_N.ToString() + "\tt=" + GetGame().GetTime().ToString());
	}
}
