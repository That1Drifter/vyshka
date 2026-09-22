// Vyshka spike: what `attachments: auto` attaches, and how the spawn
// action's quantity, health, and placement calls behave (issue #75).
//
// Appended to the init.c of a mission run with the Vyshka mod loaded and
// started from main() with VyshkaSpawnProbe.Run(). Everything it measures
// goes through the plugin's own class (VyshkaSpawn), so what is printed is
// what the action does:
//
//   1. the attachment index: how long the build takes, how many parts over
//      how many slots;
//   2. every public firearm of the stock game (the item catalog's firearm
//      type), created on the ground and equipped as `auto` equips: what it
//      was loaded with, the rounds read back, each part attached per slot,
//      each slot left empty, and the time per firearm;
//   3. a handful of other items with slots (a plate carrier, a belt, a
//      helmet, a car): what `auto` fills on them;
//   4. the quantity and health setters on named classes, with the value
//      read back or the refusal the action would answer with;
//   5. the placements on a body with no client: into the inventory of an
//      undressed and of a dressed body, and into empty and full hands;
//   6. the blocklist's config walk, timed, with a name in the wrong case
//      and a name no tree declares.
//
// Line format (tab separated, under the 255 characters Print keeps):
//   VYSHKA_SPAWN<TAB>index<TAB>ms=<ms>
//   VYSHKA_SPAWN<TAB>firearms<TAB>count=<n>
//   VYSHKA_SPAWN<TAB>firearm<TAB>class=<c><TAB>loaded=<mag or ammo><TAB>rounds=<n><TAB>ammo=<n><TAB>parts=<n><TAB>empty=<n><TAB>ms=<ms>
//   VYSHKA_SPAWN<TAB>part<TAB><owner><TAB><slot>=<class>
//   VYSHKA_SPAWN<TAB>gap<TAB><owner><TAB><slot>
//   VYSHKA_SPAWN<TAB>machine<TAB>class=<c><TAB>at=<create|later|loaded><TAB>running=<bool><TAB>rounds=<n>
//   VYSHKA_SPAWN<TAB>item<TAB>class=<c><TAB>parts=<n><TAB>empty=<n><TAB>ms=<ms>
//   VYSHKA_SPAWN<TAB>quantity<TAB>class=<c><TAB>asked=<q><TAB>ok=<bool><TAB>read=<q>/<max><TAB>error=<text>
//   VYSHKA_SPAWN<TAB>health<TAB>class=<c><TAB>asked=<p><TAB>ok=<bool><TAB>read=<p><TAB>state=<level>
//   VYSHKA_SPAWN<TAB>place<TAB>case=<name><TAB>class=<c><TAB>placed=<where><TAB>slot=<s><TAB>container=<c>
//   VYSHKA_SPAWN<TAB>blocklist<TAB>entries=<n><TAB>ms=<ms><TAB><name>=<published>...
//   VYSHKA_SPAWN<TAB>budget<TAB>case=<name><TAB>listed=<bytes><TAB>result=<bytes><TAB>truncated=<bool><TAB>count=<n><TAB>shown=<n>
//   VYSHKA_SPAWN<TAB>finished<TAB>wallMs=<frame clock ms>

class VyshkaSpawnProbe
{
	static ref VyshkaSpawnProbe s_Instance;

	static const string TAG = "VYSHKA_SPAWN";
	static const int SETTLE_MS = 3000;
	static const int GAP_MS = 30;
	// The performance counter runs at 10 MHz (spikes/dayz-bans-pull-size).
	static const int TICKS_PER_MS = 10000;
	static const string BODY = "SurvivorM_Mirek";

	vector m_Origin;
	ref array<string> m_Firearms;
	ref array<EntityAI> m_Machines;
	int m_Next;
	int m_StartTime;
	int m_Phase;

	static void Run()
	{
		if (s_Instance)
			return;
		s_Instance = new VyshkaSpawnProbe();
		GetGame().GetCallQueue(CALL_CATEGORY_SYSTEM).CallLater(s_Instance.Start, SETTLE_MS, false);
	}

	void Start()
	{
		m_StartTime = GetGame().GetTime();
		m_Origin = "7500 0 7500";
		m_Origin[1] = GetGame().SurfaceY(m_Origin[0], m_Origin[2]);
		int t0 = TickCount(0);
		VyshkaSpawn.Reset();
		VyshkaSpawn.BuildCandidates();
		int ms = TickCount(t0) / TICKS_PER_MS;
		Print(TAG + "\tindex\tms=" + ms.ToString());

		m_Firearms = new array<string>;
		string tree = VyshkaCatalog.TREE_WEAPONS;
		int children = GetGame().ConfigGetChildrenCount(tree);
		TStringArray path = new TStringArray;
		for (int i = 0; i < children; i++)
		{
			string name;
			if (!GetGame().ConfigGetChildName(tree, i, name))
				continue;
			if (GetGame().ConfigGetInt(tree + " " + name + " scope") != 2)
				continue;
			path.Clear();
			GetGame().ConfigGetFullPath(tree + " " + name, path);
			if (VyshkaCatalog.Classify(path) == VyshkaCatalog.TYPE_FIREARM)
				m_Firearms.Insert(name);
		}
		Print(TAG + "\tfirearms\tcount=" + m_Firearms.Count().ToString());
		m_Next = 0;
		m_Phase = 0;
		Later();
	}

	void Later()
	{
		GetGame().GetCallQueue(CALL_CATEGORY_SYSTEM).CallLater(Step, GAP_MS, false);
	}

	// Step runs one firearm per frame, then the other phases one per frame.
	void Step()
	{
		if (m_Next < m_Firearms.Count())
		{
			Firearm(m_Firearms.Get(m_Next));
			m_Next++;
			Later();
			return;
		}
		m_Phase++;
		if (m_Phase == 1)
		{
			// Machines resumes the steps itself, half a second on.
			Items();
			Machines();
			return;
		}
		else if (m_Phase == 2)
			Quantities();
		else if (m_Phase == 3)
			Healths();
		else if (m_Phase == 4)
			Placements();
		else if (m_Phase == 5)
			Blocklist();
		else if (m_Phase == 6)
			Budget();
		else
		{
			int wall = GetGame().GetTime() - m_StartTime;
			Print(TAG + "\tfinished\twallMs=" + wall.ToString());
			return;
		}
		Later();
	}

	void Firearm(string className)
	{
		int t0 = TickCount(0);
		EntityAI weapon = EntityAI.Cast(GetGame().CreateObjectEx(className, m_Origin, ECE_PLACE_ON_SURFACE));
		if (!weapon)
		{
			Print(TAG + "\tfirearm\tclass=" + className + "\tresult=refused");
			return;
		}
		VyshkaJsonValue attached = VyshkaJsonValue.NewArray();
		VyshkaJsonValue empty = VyshkaJsonValue.NewArray();
		string loaded = VyshkaSpawn.Equip(weapon, 1, attached, empty);
		int ms = TickCount(t0) / TICKS_PER_MS;
		VyshkaJsonValue condition = VyshkaJsonValue.NewObject();
		VyshkaInventoryTree.Condition(weapon, condition);
		int magazineRounds = -1;
		for (int i = 0; i < attached.Count(); i++)
		{
			if (attached.At(i).GetString("slot", "") == "magazine")
				magazineRounds = attached.At(i).GetInt("ammo", -1);
		}
		Print(TAG + "\tfirearm\tclass=" + className + "\tloaded=" + loaded +"\trounds=" + condition.GetInt("rounds", -1).ToString() + "\tammo=" + magazineRounds.ToString() + "\tparts=" + Count(attached).ToString() + "\tempty=" + empty.Count().ToString() + "\tms=" + ms.ToString());
		Report(className, attached, empty);
		GetGame().ObjectDelete(weapon);
	}

	void Items()
	{
		array<string> names = new array<string>;
		names.Insert("PlateCarrierVest");
		names.Insert("MilitaryBelt");
		names.Insert("Mich2001Helmet");
		names.Insert("UniversalLight");
		names.Insert("OffroadHatchback");
		names.Insert("CivilianSedan");
		for (int i = 0; i < names.Count(); i++)
		{
			string className = names.Get(i);
			vector at = m_Origin + Vector(10 + i * 8, 0, 0);
			at[1] = GetGame().SurfaceY(at[0], at[2]);
			int t0 = TickCount(0);
			EntityAI item = EntityAI.Cast(GetGame().CreateObjectEx(className, at, ECE_PLACE_ON_SURFACE));
			if (!item)
			{
				Print(TAG + "\titem\tclass=" + className + "\tresult=refused");
				continue;
			}
			VyshkaJsonValue attached = VyshkaJsonValue.NewArray();
			VyshkaJsonValue empty = VyshkaJsonValue.NewArray();
			VyshkaSpawn.Equip(item, 1, attached, empty);
			int ms = TickCount(t0) / TICKS_PER_MS;
			Print(TAG + "\titem\tclass=" + className + "\tparts=" + Count(attached).ToString() + "\tempty=" + empty.Count().ToString() + "\tms=" + ms.ToString());
			Report(className, attached, empty);
			GetGame().ObjectDelete(item);
		}
	}

	// Machines creates the firearms whose load the engine faulted in run 1
	// and says whether each one's state machine runs when it is created and
	// half a second later, when it is loaded, so the log shows whether the
	// load's script error is a matter of timing.
	void Machines()
	{
		array<string> names = new array<string>;
		names.Insert("M249");
		names.Insert("LAW");
		names.Insert("RPG7");
		names.Insert("DartGun");
		names.Insert("Shockpistol");
		names.Insert("PVCBow");
		names.Insert("M4A1");
		m_Machines = new array<EntityAI>;
		for (int i = 0; i < names.Count(); i++)
		{
			vector at = m_Origin + Vector(-10 - i * 3, 0, 0);
			at[1] = GetGame().SurfaceY(at[0], at[2]);
			Weapon_Base weapon = Weapon_Base.Cast(GetGame().CreateObjectEx(names.Get(i), at, ECE_PLACE_ON_SURFACE));
			if (!weapon)
				continue;
			Print(TAG + "\tmachine\tclass=" + weapon.GetType() + "\tat=create\trunning=" + weapon.VyshkaFsmRunning().ToString() + "\thasDamageSystem=" + weapon.HasDamageSystem().ToString() + "\tconfigDamageSystem=" + weapon.ConfigIsExisting("DamageSystem").ToString());
			m_Machines.Insert(weapon);
		}
		GetGame().GetCallQueue(CALL_CATEGORY_SYSTEM).CallLater(MachinesLater, 500, false);
	}

	void MachinesLater()
	{
		for (int i = 0; i < m_Machines.Count(); i++)
		{
			Weapon_Base weapon = Weapon_Base.Cast(m_Machines.Get(i));
			if (!weapon)
				continue;
			bool running = weapon.VyshkaFsmRunning();
			Print(TAG + "\tmachine\tclass=" + weapon.GetType() + "\tat=later\trunning=" + running.ToString() + "\thasDamageSystem=" + weapon.HasDamageSystem().ToString() + "\tloading=" + VyshkaSpawn.LoadWith(weapon));
			// Loaded the way `auto` loads, half a second on (run 2 called
			// SpawnAmmo here directly, which is what logged the errors).
			VyshkaJsonValue attached = VyshkaJsonValue.NewArray();
			// Held in a local: a fresh value passed straight in as an argument
			// was released before Equip used it (run 3, a NULL pointer in Equip).
			VyshkaJsonValue gaps = VyshkaJsonValue.NewArray();
			string loaded = VyshkaSpawn.Equip(weapon, 1, attached, gaps);
			int magazineRounds = -1;
			for (int a = 0; a < attached.Count(); a++)
			{
				if (attached.At(a).GetString("slot", "") == "magazine")
					magazineRounds = attached.At(a).GetInt("ammo", -1);
			}
			VyshkaJsonValue condition = VyshkaJsonValue.NewObject();
			VyshkaInventoryTree.Condition(weapon, condition);
			Print(TAG + "\tmachine\tclass=" + weapon.GetType() + "\tat=loaded\tloaded=" + loaded + "\trounds=" + condition.GetInt("rounds", -1).ToString() + "\tammo=" + magazineRounds.ToString());
			GetGame().ObjectDelete(weapon);
		}
		Later();
	}

	void Quantities()
	{
		QuantityCase("Rag", 3);
		QuantityCase("Rag", 2.5);
		QuantityCase("Rag", 0);
		QuantityCase("Rag", 0.0005);
		QuantityCase("WaterBottle", 0.0005);
		QuantityCase("Rag", 50);
		QuantityCase("WaterBottle", 500);
		QuantityCase("WaterBottle", 0);
		QuantityCase("Canteen", 250.5);
		QuantityCase("Mag_STANAG_30Rnd", 12);
		QuantityCase("Mag_STANAG_30Rnd", 31);
		QuantityCase("Mag_STANAG_30Rnd", 7.5);
		QuantityCase("Ammo_556x45", 20);
		QuantityCase("Ammo_556x45", 0);
		QuantityCase("Nail", 70);
		QuantityCase("M4A1", 1);
		QuantityCase("Battery9V", 50);
		QuantityCase("SmallGasCanister", 100);
	}

	void QuantityCase(string className, float quantity)
	{
		EntityAI item = EntityAI.Cast(GetGame().CreateObjectEx(className, m_Origin, ECE_PLACE_ON_SURFACE));
		if (!item)
		{
			Print(TAG + "\tquantity\tclass=" + className + "\tresult=refused");
			return;
		}
		string error;
		bool ok = VyshkaSpawn.SetQuantity(item, quantity, error);
		VyshkaJsonValue condition = VyshkaJsonValue.NewObject();
		VyshkaInventoryTree.Condition(item, condition);
		string read = "";
		if (condition.Get("ammo"))
			read = condition.Get("ammo").Serialize() + "/" + condition.Get("ammoMax").Serialize();
		else if (condition.Get("quantity"))
			read = condition.Get("quantity").Serialize() + "/" + condition.Get("quantityMax").Serialize();
		Print(TAG + "\tquantity\tclass=" + className + "\tasked=" + quantity.ToString() + "\tok=" + ok.ToString() + "\tread=" + read + "\terror=" + error);
		GetGame().ObjectDelete(item);
	}

	void Healths()
	{
		HealthCase("M4A1", 50);
		HealthCase("M4A1", 0);
		HealthCase("M4A1", 100);
		HealthCase("TShirt_Black", 30);
		HealthCase("Apple", 69.5);
		HealthCase("OffroadHatchback", 40);
	}

	void HealthCase(string className, float percent)
	{
		EntityAI item = EntityAI.Cast(GetGame().CreateObjectEx(className, m_Origin + Vector(0, 0, 20), ECE_PLACE_ON_SURFACE));
		if (!item)
		{
			Print(TAG + "\thealth\tclass=" + className + "\tresult=refused");
			return;
		}
		string error;
		bool ok = VyshkaSpawn.SetHealth(item, percent, error);
		VyshkaJsonValue condition = VyshkaJsonValue.NewObject();
		VyshkaInventoryTree.Condition(item, condition);
		Print(TAG + "\thealth\tclass=" + className + "\tasked=" + percent.ToString() + "\tok=" + ok.ToString() + "\tread=" + condition.GetInt("health", -1).ToString() + "\tstate=" + condition.GetString("state", "") + "\terror=" + error);
		GetGame().ObjectDelete(item);
	}

	void Placements()
	{
		vector at = m_Origin + Vector(0, 0, -20);
		at[1] = GetGame().SurfaceY(at[0], at[2]);
		PlayerBase body = PlayerBase.Cast(GetGame().CreateObjectEx(BODY, at, ECE_PLACE_ON_SURFACE));
		if (!body)
		{
			Print(TAG + "\tplace\tcase=body\tresult=refused");
			return;
		}
		PlaceCase("naked-inventory-rag", body, body.GetInventory().CreateInInventory("Rag"));
		PlaceCase("naked-inventory-second", body, body.GetInventory().CreateInInventory("Apple"));
		Clear(body);
		PlaceCase("empty-hands", body, body.GetHumanInventory().CreateInHands("M4A1"));
		PlaceCase("full-hands", body, body.GetHumanInventory().CreateInHands("AKM"));
		Clear(body);
		body.GetInventory().CreateAttachment("HuntingJacket_Brown");
		body.GetInventory().CreateAttachment("AliceBag_Green");
		body.GetHumanInventory().CreateInHands("Apple");
		PlaceCase("dressed-inventory-rag", body, body.GetInventory().CreateInInventory("Rag"));
		PlaceCase("dressed-inventory-rifle", body, body.GetInventory().CreateInInventory("M4A1"));
		PlaceCase("dressed-inventory-car", body, body.GetInventory().CreateInInventory("OffroadHatchback"));
		GetGame().ObjectDelete(body);
	}

	void Clear(PlayerBase body)
	{
		array<EntityAI> items = VyshkaInventory.TopLevel(body);
		for (int i = 0; i < items.Count(); i++)
			GetGame().ObjectDelete(items.Get(i));
	}

	void PlaceCase(string name, PlayerBase body, EntityAI created)
	{
		if (!created)
		{
			Print(TAG + "\tplace\tcase=" + name + "\tresult=refused");
			return;
		}
		VyshkaJsonValue where = VyshkaJsonValue.NewObject();
		VyshkaSpawn.Place(created, where);
		Print(TAG + "\tplace\tcase=" + name + "\tclass=" + created.GetType() + "\tplaced=" + where.GetString("placed", "") + "\tslot=" + where.GetString("slot", "") + "\tcontainer=" + where.GetString("container", ""));
	}

	void Blocklist()
	{
		map<string, string> wanted = new map<string, string>;
		wanted.Set("m4a1", "m4a1");
		wanted.Set("grenade_rgd5", "GRENADE_RGD5");
		wanted.Set("mag_stanag_30rnd", "mag_stanag_30rnd");
		wanted.Set("nosuchclassanywhere", "NoSuchClassAnywhere");
		int t0 = TickCount(0);
		VyshkaSpawn.Canonicalize(wanted);
		int ms = TickCount(t0) / TICKS_PER_MS;
		string line = TAG + "\tblocklist\tentries=" + wanted.Count().ToString() + "\tms=" + ms.ToString();
		for (int i = 0; i < wanted.Count(); i++)
			line = line + "\t" + wanted.GetKey(i) + "=" + wanted.GetElement(i);
		Print(line);
	}

	// Budget hands the result reporter an `auto` report far larger than any
	// stock item makes (a modded item with many slots, each part with many
	// of its own, long names), so the cut to the result budget is seen: 20
	// parts of 20 parts each, then 600 parts with none.
	void Budget()
	{
		BudgetCase("nested", 20, 20);
		BudgetCase("flat", 600, 0);
	}

	void BudgetCase(string name, int outer, int inner)
	{
		string longName = "";
		while (longName.Length() < 120)
			longName = longName + "VyshkaBudgetProbePart_";
		VyshkaJsonValue attached = VyshkaJsonValue.NewArray();
		for (int i = 0; i < outer; i++)
		{
			VyshkaJsonValue entry = VyshkaJsonValue.NewObject();
			entry.Set("slot", VyshkaJsonValue.NewString("vyshkaBudgetSlot_" + i.ToString() + "_" + longName));
			entry.Set("class", VyshkaJsonValue.NewString(longName + i.ToString()));
			if (inner > 0)
			{
				VyshkaJsonValue children = VyshkaJsonValue.NewArray();
				for (int j = 0; j < inner; j++)
				{
					VyshkaJsonValue child = VyshkaJsonValue.NewObject();
					child.Set("slot", VyshkaJsonValue.NewString("vyshkaBudgetChildSlot_" + j.ToString() + "_" + longName));
					child.Set("class", VyshkaJsonValue.NewString(longName + j.ToString()));
					children.Add(child);
				}
				entry.Set("attachments", children);
			}
			attached.Add(entry);
		}
		VyshkaJsonValue empty = VyshkaJsonValue.NewArray();
		int before = attached.Serialize().Length();
		VyshkaJsonValue result = VyshkaJsonValue.NewObject();
		result.Set("className", VyshkaJsonValue.NewString("VyshkaBudgetProbe"));
		VyshkaSpawn.Report(result, attached, empty);
		int after = result.Serialize().Length();
		Print(TAG + "\tbudget\tcase=" + name + "\tlisted=" + before.ToString() + "\tresult=" + after.ToString() + "\ttruncated=" + result.GetBool("truncated", false).ToString() + "\tcount=" + result.GetInt("attachmentCount", -1).ToString() + "\tshown=" + result.Get("attachments").Count().ToString());
	}

	// Count counts parts at every level of an attached list.
	static int Count(VyshkaJsonValue attached)
	{
		int count = 0;
		for (int i = 0; i < attached.Count(); i++)
		{
			count++;
			VyshkaJsonValue inner = attached.At(i).Get("attachments");
			if (inner)
				count += Count(inner);
		}
		return count;
	}

	static void Report(string owner, VyshkaJsonValue attached, VyshkaJsonValue empty)
	{
		for (int i = 0; i < attached.Count(); i++)
		{
			VyshkaJsonValue part = attached.At(i);
			Print(TAG + "\tpart\t" + owner + "\t" + part.GetString("slot", "") + "=" + part.GetString("class", ""));
			VyshkaJsonValue inner = part.Get("attachments");
			if (inner)
			{
				VyshkaJsonValue none = VyshkaJsonValue.NewArray();
				Report(owner + ">" + part.GetString("class", ""), inner, none);
			}
		}
		for (int g = 0; g < empty.Count(); g++)
			Print(TAG + "\tgap\t" + empty.At(g).GetString("on", "") + "\t" + empty.At(g).GetString("slot", ""));
	}
}
