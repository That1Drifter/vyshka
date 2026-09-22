// Vyshka DayZ plugin: the item catalog, the plugin's own custom contexts
// (spec section 6.2, issue #73).
//
// An operator spawning an item types a class name the engine knows, and
// there are about two thousand of them on a stock server. The catalog is
// the plugin's answer to "which ones": every public class of CfgVehicles,
// CfgWeapons, and CfgMagazines that is an item or a vehicle, sorted by type
// into one custom context each, with the display name the client would show
// as the label and a few stats of the type in data (weight, a firearm's
// ammunition, a magazine's capacity, a food's energy and water, a garment's
// slot and cargo). The spawn action's className names these contexts in its
// params schema (the `context` annotation of section 6.1), so a panel offers
// the names an operator recognizes, and any mod's action can do the same.
//
// The split is by type rather than one list because one reply carries at
// most 5000 entries and 262144 bytes (section 6.2): a stock 1.29 server's
// items serialize to about 243 KiB in one list, too close to the bound for
// a modded server, while the largest type (clothing, 786 entries) is
// 107 KiB (spikes/dayz-item-catalog). A type that still grows past the bound
// on a heavily modded server is answered with no entries and a reason by the
// plugin's enumeration handler, never with a cut list that reads as
// complete.
//
// The catalog is built once per session, on the first enumeration of any of
// its contexts: the walk over the config trees and the stat reads cost the
// main thread about 40 ms on the stock server (measured, same spike), which
// is paid once, since the config does not change while the server runs. A
// new mission starts a new catalog.

class VyshkaCatalogEntry
{
	string m_Key;      // the class name, what the hub hands back as a referenceKey
	string m_Label;    // the translated display name, or the class name when there is none
	ref VyshkaJsonValue m_Data;

	void VyshkaCatalogEntry(string key, string label, VyshkaJsonValue data)
	{
		m_Key = key;
		m_Label = label;
		m_Data = data;
	}
}

// VyshkaCatalogContext is one type's context. Eight are registered, one per
// type, all answered from the same catalog.
class VyshkaCatalogContext : VyshkaContext
{
	int m_Type;

	void VyshkaCatalogContext(int type)
	{
		m_Type = type;
	}

	override string Id()        { return VyshkaCatalog.ContextId(m_Type); }
	override string Name()      { return VyshkaCatalog.ContextName(m_Type); }
	override string Namespace() { return VyshkaRegistry.KV_NAMESPACE; }

	override void Enumerate(VyshkaContextList list)
	{
		VyshkaCatalog.Instance().Fill(m_Type, list);
	}
}

class VyshkaCatalog
{
	static ref VyshkaCatalog s_Instance;

	// The types, in classification order: the first base found on a class's
	// inheritance path wins, so a magazine that is also an item is a
	// magazine and an optic that is also inventory is an optic.
	static const int TYPE_FIREARM = 0;
	static const int TYPE_OPTIC = 1;
	static const int TYPE_AMMO = 2;
	static const int TYPE_MAGAZINE = 3;
	static const int TYPE_EDIBLE = 4;
	static const int TYPE_CLOTHING = 5;
	static const int TYPE_VEHICLE = 6;
	static const int TYPE_GEAR = 7;
	static const int TYPE_COUNT = 8;

	// The config trees an item can be declared in (the same three the spawn
	// action looks a class up in).
	static const string TREE_VEHICLES = "CfgVehicles";
	static const string TREE_WEAPONS = "CfgWeapons";
	static const string TREE_MAGAZINES = "CfgMagazines";

	// The performance counter runs at 10 MHz (spikes/dayz-bans-pull-size);
	// the frame clock cannot time a build, which runs inside one frame.
	static const int TICKS_PER_MS = 10000;

	ref array<ref array<ref VyshkaCatalogEntry>> m_Entries;
	bool m_Built;

	// ContextId is the id an action's context annotation names.
	static string ContextId(int type)
	{
		if (type == TYPE_FIREARM)
			return "dayz.items.firearms";
		if (type == TYPE_OPTIC)
			return "dayz.items.optics";
		if (type == TYPE_AMMO)
			return "dayz.items.ammo";
		if (type == TYPE_MAGAZINE)
			return "dayz.items.magazines";
		if (type == TYPE_EDIBLE)
			return "dayz.items.edibles";
		if (type == TYPE_CLOTHING)
			return "dayz.items.clothing";
		if (type == TYPE_VEHICLE)
			return "dayz.vehicles";
		return "dayz.items.gear";
	}

	static string ContextName(int type)
	{
		if (type == TYPE_FIREARM)
			return "Firearms";
		if (type == TYPE_OPTIC)
			return "Optics";
		if (type == TYPE_AMMO)
			return "Ammunition";
		if (type == TYPE_MAGAZINE)
			return "Magazines";
		if (type == TYPE_EDIBLE)
			return "Edibles";
		if (type == TYPE_CLOTHING)
			return "Clothing";
		if (type == TYPE_VEHICLE)
			return "Vehicles";
		return "Gear";
	}

	// TypeName is what an entry's data.type says.
	static string TypeName(int type)
	{
		if (type == TYPE_FIREARM)
			return "firearm";
		if (type == TYPE_OPTIC)
			return "optic";
		if (type == TYPE_AMMO)
			return "ammo";
		if (type == TYPE_MAGAZINE)
			return "magazine";
		if (type == TYPE_EDIBLE)
			return "edible";
		if (type == TYPE_CLOTHING)
			return "clothing";
		if (type == TYPE_VEHICLE)
			return "vehicle";
		return "gear";
	}

	// ItemContextIds is what a spawn action's className annotation names:
	// every catalog context, vehicles included, since the engine creates a
	// vehicle class like any other.
	static VyshkaJsonValue ContextIds()
	{
		VyshkaJsonValue ids = VyshkaJsonValue.NewArray();
		for (int type = 0; type < TYPE_COUNT; type++)
			ids.Add(VyshkaJsonValue.NewString(ContextId(type)));
		return ids;
	}

	// RegisterContexts declares the eight contexts with the registry, from
	// the mission's VyshkaRegister hook.
	static void RegisterContexts(VyshkaRegistry registry)
	{
		for (int type = 0; type < TYPE_COUNT; type++)
			registry.RegisterContext(new VyshkaCatalogContext(type));
	}

	// Reset forgets the catalog: a new mission builds its own.
	static void Reset()
	{
		s_Instance = null;
	}

	static VyshkaCatalog Instance()
	{
		if (!s_Instance)
			s_Instance = new VyshkaCatalog();
		return s_Instance;
	}

	void VyshkaCatalog()
	{
		m_Entries = new array<ref array<ref VyshkaCatalogEntry>>;
		for (int type = 0; type < TYPE_COUNT; type++)
			m_Entries.Insert(new array<ref VyshkaCatalogEntry>);
	}

	// Fill answers one type's enumeration from the catalog, building it
	// first when this is the session's first request.
	void Fill(int type, VyshkaContextList list)
	{
		if (!m_Built)
			Build();
		if (type < 0 || type >= TYPE_COUNT)
			return;
		array<ref VyshkaCatalogEntry> entries = m_Entries.Get(type);
		for (int i = 0; i < entries.Count(); i++)
		{
			VyshkaCatalogEntry entry = entries.Get(i);
			list.AddEntry(entry.m_Key, entry.m_Label, null, entry.m_Data);
		}
	}

	// Count is how many classes one type holds, for the log and the tests.
	int Count(int type)
	{
		if (type < 0 || type >= TYPE_COUNT)
			return 0;
		return m_Entries.Get(type).Count();
	}

	// Build walks the three trees once and sorts every public class into
	// its type, reading the label and the stats as it goes.
	void Build()
	{
		m_Built = true;
		int t0 = TickCount(0);
		int sorted = Walk(TREE_VEHICLES) + Walk(TREE_WEAPONS) + Walk(TREE_MAGAZINES);
		int ms = TickCount(t0) / TICKS_PER_MS;
		string counts = "";
		for (int type = 0; type < TYPE_COUNT; type++)
		{
			if (counts != "")
				counts = counts + ", ";
			counts = counts + TypeName(type) + " " + Count(type).ToString();
		}
		VyshkaLog.Info("item catalog built: " + sorted.ToString() + " classes (" + counts + ") in " + ms.ToString() + " ms");
	}

	int Walk(string tree)
	{
		int children = GetGame().ConfigGetChildrenCount(tree);
		int sorted = 0;
		TStringArray path = new TStringArray;
		for (int i = 0; i < children; i++)
		{
			string name;
			if (!GetGame().ConfigGetChildName(tree, i, name))
				continue;
			string classPath = tree + " " + name;
			// Scope 2 is a public class the engine will create; 0 and 1
			// are the bases items inherit from (VyshkaWorld.FindConfig).
			if (GetGame().ConfigGetInt(classPath + " scope") != 2)
				continue;
			path.Clear();
			GetGame().ConfigGetFullPath(classPath, path);
			int type = Classify(path);
			if (type < 0)
				continue;
			m_Entries.Get(type).Insert(new VyshkaCatalogEntry(name, DisplayName(classPath, name), Stats(type, classPath)));
			sorted++;
		}
		return sorted;
	}

	// Classify reads a class's inheritance path (itself first, then its
	// bases up to the root) and names the first catalog type it belongs
	// to, or -1 for what is not an item or a vehicle (buildings, animals,
	// the infected, effects).
	static int Classify(TStringArray path)
	{
		bool item = false;
		for (int i = 0; i < path.Count(); i++)
		{
			string base = path.Get(i);
			base.ToLower();
			if (base == "weapon_base")
				return TYPE_FIREARM;
			if (base == "itemoptics")
				return TYPE_OPTIC;
			if (base == "ammunition_base")
				return TYPE_AMMO;
			if (base == "magazine_base")
				return TYPE_MAGAZINE;
			if (base == "edible_base")
				return TYPE_EDIBLE;
			if (base == "clothing_base")
				return TYPE_CLOTHING;
			if (base == "carscript" || base == "boatscript")
				return TYPE_VEHICLE;
			if (base == "inventory_base" || base == "itembase")
				item = true;
		}
		if (item)
			return TYPE_GEAR;
		return -1;
	}

	// DisplayName resolves a class's displayName the way Object.GetDisplayName
	// does (the config text through Widget.TranslateString), and falls back
	// to the class name for a class without one or with a key the server
	// cannot translate (a stock server translates all but a few dozen).
	static string DisplayName(string classPath, string name)
	{
		string raw = GetGame().ConfigGetTextOut(classPath + " displayName");
		if (raw == "")
			return name;
		string translated = Widget.TranslateString(raw);
		if (translated == "" || translated == raw && raw.Get(0) == "$")
			return name;
		return translated;
	}

	// Stats reads the per-type extras an entry's data carries. Everything
	// is read from config, so it is what the item is declared as, not what
	// any instance has become.
	static VyshkaJsonValue Stats(int type, string classPath)
	{
		VyshkaJsonValue data = VyshkaJsonValue.NewObject();
		data.Set("type", VyshkaJsonValue.NewString(TypeName(type)));
		int weight = GetGame().ConfigGetInt(classPath + " weight");
		if (weight > 0)
			data.Set("weight", VyshkaJsonValue.NewInt(weight));
		TStringArray texts = new TStringArray;
		if (type == TYPE_FIREARM)
		{
			GetGame().ConfigGetTextArray(classPath + " chamberableFrom", texts);
			if (texts.Count() > 0)
				data.Set("ammo", VyshkaJsonValue.NewString(texts.Get(0)));
			TStringArray magazines = new TStringArray;
			GetGame().ConfigGetTextArray(classPath + " magazines", magazines);
			data.Set("magazines", VyshkaJsonValue.NewInt(magazines.Count()));
		}
		else if (type == TYPE_AMMO || type == TYPE_MAGAZINE)
		{
			data.Set("count", VyshkaJsonValue.NewInt(GetGame().ConfigGetInt(classPath + " count")));
			string ammo = GetGame().ConfigGetTextOut(classPath + " ammo");
			if (ammo != "")
				data.Set("ammo", VyshkaJsonValue.NewString(ammo));
		}
		else if (type == TYPE_EDIBLE)
		{
			// A food without stages declares a Nutrition class; one with
			// stages (fruit, meat) keeps each stage's values in an array
			// under Food FoodStages, in the engine's order: fullness,
			// energy, water, nutritional index, toxicity, agents,
			// digestibility (FoodStage.GetEnergy and its siblings). The raw
			// stage is what a spawned one starts as.
			if (GetGame().ConfigIsExisting(classPath + " Nutrition"))
			{
				SetFloat(data, "energy", GetGame().ConfigGetFloat(classPath + " Nutrition energy"));
				SetFloat(data, "water", GetGame().ConfigGetFloat(classPath + " Nutrition water"));
			}
			else
			{
				TFloatArray raw = new TFloatArray;
				GetGame().ConfigGetFloatArray(classPath + " Food FoodStages Raw nutrition_properties", raw);
				if (raw.Count() >= 3)
				{
					SetFloat(data, "energy", raw.Get(1));
					SetFloat(data, "water", raw.Get(2));
				}
			}
		}
		else if (type == TYPE_CLOTHING)
		{
			GetGame().ConfigGetTextArray(classPath + " inventorySlot", texts);
			if (texts.Count() > 0)
				data.Set("slot", VyshkaJsonValue.NewString(texts.Get(0)));
			TIntArray cargo = new TIntArray;
			GetGame().ConfigGetIntArray(classPath + " Cargo itemsCargoSize", cargo);
			if (cargo.Count() >= 2)
				data.Set("cargo", VyshkaJsonValue.NewInt(cargo.Get(0) * cargo.Get(1)));
			SetFloat(data, "heatIsolation", GetGame().ConfigGetFloat(classPath + " heatIsolation"));
		}
		else if (type == TYPE_OPTIC)
		{
			SetFloat(data, "zoomMin", GetGame().ConfigGetFloat(classPath + " OpticsInfo opticsZoomMin"));
			SetFloat(data, "zoomMax", GetGame().ConfigGetFloat(classPath + " OpticsInfo opticsZoomMax"));
		}
		else if (type == TYPE_VEHICLE)
		{
			SetFloat(data, "fuel", GetGame().ConfigGetFloat(classPath + " fuelCapacity"));
			data.Set("crew", VyshkaJsonValue.NewInt(GetGame().ConfigGetChildrenCount(classPath + " Crew")));
		}
		return data;
	}

	// SetFloat sets a number the JSON writer can carry and skips one it
	// cannot (NewFloat returns null for a value that is not finite).
	static void SetFloat(VyshkaJsonValue data, string key, float value)
	{
		VyshkaJsonValue rendered = VyshkaJsonValue.NewFloat(value);
		if (rendered)
			data.Set(key, rendered);
	}
}
