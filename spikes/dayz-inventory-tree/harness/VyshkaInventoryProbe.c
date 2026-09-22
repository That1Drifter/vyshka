// Vyshka spike: what a fully loaded character's inventory tree costs
// against the hub's 64 KiB action result cap (issue #74).
//
// Appended to the init.c of a mission run with the Vyshka mod loaded,
// together with VyshkaLoadout.c, and started from main() with
// VyshkaInventoryProbe.Run(). It creates a survivor body with no client
// behind it, gears it with the heaviest loadout a stock character can
// carry (VyshkaLoadout: the largest containers, three rifles with every
// attachment, cases nested in every cargo, every cargo filled with
// one-slot items), then builds the tree the plugin's read action builds
// (VyshkaInventoryTree, the plugin's own class) at every depth and
// serializes each, so the bytes per depth are known against the
// 60 000-byte budget the action keeps under the cap, and runs the strip
// and the clear on the body as a smoke test of the two engine calls with
// no client to synchronize to.
//
// Line format (tab separated, under the 255 characters Print keeps):
//   VYSHKA_INVENTORY<TAB>slot=<name><TAB>class=<garment><TAB>result=<where it landed><TAB>cargo=<w>x<h>
//   VYSHKA_INVENTORY<TAB>geared<TAB>items=<n><TAB>ms=<ms>
//   VYSHKA_INVENTORY<TAB>stuffed<TAB>containers=<n><TAB>nested=<n><TAB>filled=<n>
//   VYSHKA_INVENTORY<TAB>filled<TAB>items=<n><TAB>ms=<ms>
//   VYSHKA_INVENTORY<TAB>carried<TAB>slot=<name><TAB>class=<c><TAB>items=<n><TAB>cargo=<w>x<h>
//   VYSHKA_INVENTORY<TAB>depth=<d><TAB>items=<n><TAB>included=<n><TAB>bytes=<b><TAB>buildMs=<ms><TAB>serializeMs=<ms><TAB>cut=<bool>
//   VYSHKA_INVENTORY<TAB>fits<TAB>depth=<d><TAB>bytes=<b><TAB>attempts=<n>
//   VYSHKA_INVENTORY<TAB>sample<TAB><one serialized entry, cut to fit>
//   VYSHKA_INVENTORY<TAB>before-strip<TAB>carried=<n><TAB>nearby=<n>
//   VYSHKA_INVENTORY<TAB>strip<TAB>dropped=<n><TAB>skipped=<n><TAB>items=<n>
//   VYSHKA_INVENTORY<TAB>after-strip<TAB>carried=<n><TAB>nearby=<n>
//   VYSHKA_INVENTORY<TAB>clear<TAB>deleted=<n><TAB>items=<n>
//   VYSHKA_INVENTORY<TAB>after-clear<TAB>carried=<n><TAB>nearby=<n>
//   VYSHKA_INVENTORY<TAB>finished<TAB>wallMs=<frame clock ms>

class VyshkaInventoryProbe
{
	static ref VyshkaInventoryProbe s_Instance;

	static const string TAG = "VYSHKA_INVENTORY";
	static const int SETTLE_MS = 3000;
	static const int GAP_MS = 50;
	static const int WAIT_MS = 2000;      // for a deferred drop or delete to run
	// The performance counter runs at 10 MHz (spikes/dayz-bans-pull-size).
	static const int TICKS_PER_MS = 10000;
	static const int SAMPLE_MAX = 220;
	static const float NEARBY_RADIUS = 6.0;

	static const string BODY = "SurvivorM_Mirek";

	PlayerBase m_Body;
	vector m_Position;
	int m_Step;
	int m_PlanTime;
	int m_Deepest;

	static void Run()
	{
		if (s_Instance)
			return;
		s_Instance = new VyshkaInventoryProbe();
		s_Instance.Schedule();
	}

	void Schedule()
	{
		GetGame().GetCallQueue(CALL_CATEGORY_SYSTEM).CallLater(Start, SETTLE_MS, false);
	}

	void Start()
	{
		m_Step = 0;
		m_PlanTime = GetGame().GetTime();
		Print(TAG + "\tplan=10");
		Next();
	}

	// Next runs one phase per frame, with a longer wait after a strip or a
	// clear so the engine's deferred moves and deletes have run before
	// what is left is counted.
	void Next()
	{
		int step = m_Step;
		m_Step++;
		int gap = GAP_MS;
		if (step == 0)
		{
			// No body, no measurement: the error line is the probe's last
			// word, and the runner fails on it rather than on a finished
			// line that measured nothing.
			if (!CreateBody())
				return;
		}
		else if (step == 1)
			Gear();
		else if (step == 2)
			Stuff();
		else if (step == 3)
			Measure();
		else if (step == 4)
		{
			Report("before-strip");
			Strip();
			gap = WAIT_MS;
		}
		else if (step == 5)
			Report("after-strip");
		else if (step == 6)
			Gear();
		else if (step == 7)
		{
			Clear();
			gap = WAIT_MS;
		}
		else if (step == 8)
			Report("after-clear");
		else
		{
			Finish();
			return;
		}
		GetGame().GetCallQueue(CALL_CATEGORY_SYSTEM).CallLater(Next, gap, false);
	}

	void Finish()
	{
		if (m_Body)
			GetGame().ObjectDelete(m_Body);
		int wallMs = GetGame().GetTime() - m_PlanTime;
		Print(TAG + "\tfinished\twallMs=" + wallMs.ToString());
	}

	static int Millis(int ticks)
	{
		return ticks / TICKS_PER_MS;
	}

	// CreateBody stands a survivor on the terrain near the coast east of
	// Elektrozavodsk, away from anything the economy places.
	bool CreateBody()
	{
		m_Position = "10450 0 2250";
		m_Position[1] = GetGame().SurfaceY(m_Position[0], m_Position[2]);
		Object created = GetGame().CreateObjectEx(BODY, m_Position, ECE_PLACE_ON_SURFACE);
		m_Body = PlayerBase.Cast(created);
		if (!m_Body)
		{
			Print(TAG + "\terror\tno " + BODY + " could be created at " + m_Position.ToString());
			return false;
		}
		Print(TAG + "\tbody\tclass=" + m_Body.GetType() + "\tslots=" + m_Body.GetInventory().GetAttachmentSlotsCount().ToString() + "\talive=" + m_Body.IsAlive().ToString());
		return true;
	}

	void Gear()
	{
		int t0 = TickCount(0);
		int items = VyshkaLoadout.Gear(m_Body, TAG);
		Print(TAG + "\tgeared\titems=" + items.ToString() + "\tms=" + Millis(TickCount(t0)).ToString());
	}

	void Stuff()
	{
		int t0 = TickCount(0);
		int items = VyshkaLoadout.Stuff(m_Body, TAG);
		Print(TAG + "\tfilled\titems=" + items.ToString() + "\tms=" + Millis(TickCount(t0)).ToString());
		VyshkaLoadout.Report(m_Body, TAG);
	}

	// Measure builds and serializes the tree at every depth from the
	// whole tree down to one level, then runs the read action's own
	// shrink loop to see where it settles.
	void Measure()
	{
		int t0 = TickCount(0);
		VyshkaInventoryTree whole = new VyshkaInventoryTree(0);
		VyshkaJsonValue result = Tree(whole);
		int buildMs = Millis(TickCount(t0));
		int t1 = TickCount(0);
		string text = result.Serialize();
		int serializeMs = Millis(TickCount(t1));
		m_Deepest = whole.m_Deepest;
		Print(TAG + "\tdepth=" + m_Deepest.ToString() + "\titems=" + whole.m_Items.ToString() + "\tincluded=" + whole.m_Items.ToString() + "\tbytes=" + text.Length().ToString() + "\tbuildMs=" + buildMs.ToString() + "\tserializeMs=" + serializeMs.ToString() + "\tcut=" + whole.m_Cut.ToString());
		for (int depth = m_Deepest - 1; depth >= 1; depth--)
		{
			int t2 = TickCount(0);
			VyshkaInventoryTree tree = new VyshkaInventoryTree(depth);
			VyshkaJsonValue cut = Tree(tree);
			int cutBuildMs = Millis(TickCount(t2));
			int t3 = TickCount(0);
			string cutText = cut.Serialize();
			int cutSerializeMs = Millis(TickCount(t3));
			Print(TAG + "\tdepth=" + depth.ToString() + "\titems=" + tree.m_Items.ToString() + "\tincluded=" + Included(cut).ToString() + "\tbytes=" + cutText.Length().ToString() + "\tbuildMs=" + cutBuildMs.ToString() + "\tserializeMs=" + cutSerializeMs.ToString() + "\tcut=" + tree.m_Cut.ToString());
		}
		// The read action's loop: whole first, then one level less each
		// time until the bytes fit the budget.
		int maxDepth = 0;
		int attempts = 0;
		int bytes;
		while (true)
		{
			attempts++;
			VyshkaInventoryTree attempt = new VyshkaInventoryTree(maxDepth);
			bytes = Tree(attempt).Serialize().Length();
			if (bytes <= VyshkaInventory.RESULT_BUDGET || maxDepth == 1)
				break;
			if (maxDepth == 0)
				maxDepth = attempt.m_Deepest;
			maxDepth--;
			if (maxDepth < 1)
				maxDepth = 1;
		}
		if (maxDepth == 0)
			maxDepth = m_Deepest;
		Print(TAG + "\tfits\tdepth=" + maxDepth.ToString() + "\tbytes=" + bytes.ToString() + "\tattempts=" + attempts.ToString());
		VyshkaJsonValue hands = result.Get("hands");
		if (hands && hands.IsObject())
		{
			string sample = hands.Serialize();
			if (sample.Length() > SAMPLE_MAX)
				sample = sample.Substring(0, SAMPLE_MAX);
			Print(TAG + "\tsample\t" + sample);
		}
	}

	VyshkaJsonValue Tree(VyshkaInventoryTree tree)
	{
		VyshkaJsonValue result = VyshkaJsonValue.NewObject();
		tree.Root(m_Body, "", result);
		return result;
	}

	// Included counts the entries a serialized tree describes.
	static int Included(VyshkaJsonValue result)
	{
		int count = 0;
		VyshkaJsonValue hands = result.Get("hands");
		if (hands && hands.IsObject())
			count += Entries(hands);
		VyshkaJsonValue worn = result.Get("worn");
		if (worn)
		{
			for (int i = 0; i < worn.Count(); i++)
				count += Entries(worn.At(i));
		}
		return count;
	}

	static int Entries(VyshkaJsonValue entry)
	{
		int count = 1;
		VyshkaJsonValue attachments = entry.Get("attachments");
		int i;
		if (attachments)
		{
			for (i = 0; i < attachments.Count(); i++)
				count += Entries(attachments.At(i));
		}
		VyshkaJsonValue cargo = entry.Get("cargo");
		if (cargo)
		{
			for (i = 0; i < cargo.Count(); i++)
				count += Entries(cargo.At(i));
		}
		return count;
	}

	// Strip drops every top-level item the way the action does.
	void Strip()
	{
		array<EntityAI> items = VyshkaInventory.TopLevel(m_Body);
		int dropped = 0;
		int skipped = 0;
		int total = 0;
		for (int i = 0; i < items.Count(); i++)
		{
			EntityAI item = items.Get(i);
			int inside = VyshkaInventory.CountTree(item);
			if (!m_Body.CanDropEntity(item) || !m_Body.ServerDropEntity(item))
			{
				skipped++;
				continue;
			}
			dropped++;
			total += inside;
		}
		Print(TAG + "\tstrip\tdropped=" + dropped.ToString() + "\tskipped=" + skipped.ToString() + "\titems=" + total.ToString());
	}

	// Clear deletes every top-level item the way the action does.
	void Clear()
	{
		array<EntityAI> items = VyshkaInventory.TopLevel(m_Body);
		int total = 0;
		for (int i = 0; i < items.Count(); i++)
		{
			EntityAI item = items.Get(i);
			total += VyshkaInventory.CountTree(item);
			item.DeleteSafe();
		}
		Print(TAG + "\tclear\tdeleted=" + items.Count().ToString() + "\titems=" + total.ToString());
	}

	// Report counts what the body still carries and what lies around it.
	void Report(string label)
	{
		Print(TAG + "\t" + label + "\tcarried=" + VyshkaLoadout.Carried(m_Body).ToString() + "\tnearby=" + Nearby().ToString());
	}

	// Nearby counts the items on the ground within a few metres of the
	// body: what a strip left there, what a clear did not.
	int Nearby()
	{
		array<Object> objects = new array<Object>;
		array<CargoBase> proxies = new array<CargoBase>;
		GetGame().GetObjectsAtPosition3D(m_Position, NEARBY_RADIUS, objects, proxies);
		int count = 0;
		for (int i = 0; i < objects.Count(); i++)
		{
			ItemBase item = ItemBase.Cast(objects.Get(i));
			if (!item || item.IsSetForDeletion())
				continue;
			if (item.GetHierarchyRoot() != item)
				continue;
			count++;
		}
		return count;
	}
}
