// Vyshka spike: what an item catalog costs the DayZ server (issue #73).
//
// Appended to the init.c of a mission run with the Vyshka mod loaded and
// started from main() with VyshkaCatalogProbe.Run(). It walks the three
// config trees an item can live in (CfgVehicles, CfgWeapons, CfgMagazines),
// keeps every public class (scope 2), sorts each into one of the catalog's
// types by its inheritance path, and then builds each type's entries the
// way a VyshkaContext would (the plugin's own VyshkaContextList and JSON
// classes), with the display name translated and the per-type stats read
// from config. Every phase is timed with the engine's performance counter
// and the whole with the frame clock.
//
// What it answers, and what the catalog's shape depends on:
//   how many public classes each type has, against the 5000-entry bound of
//   one context.entries reply (spec section 6.2);
//   how many bytes each type's entries serialize to, against the 262144
//   byte bound;
//   how long the walk and each build take on the main thread, which decides
//   whether the catalog is built at boot or on the first request;
//   whether Widget.TranslateString resolves $STR_ display names on a
//   dedicated server, or the class name has to stand in.
//
// Line format (tab separated, under the 255 characters Print keeps):
//   VYSHKA_CATALOG<TAB>tree=<name><TAB>children=<n><TAB>public=<n><TAB>sorted=<n><TAB>ms=<ms>
//   VYSHKA_CATALOG<TAB>type=<t><TAB>count=<n><TAB>bytes=<b><TAB>buildMs=<ms><TAB>serializeMs=<ms><TAB>untranslated=<n><TAB>dropped=<n>
//   VYSHKA_CATALOG<TAB>sample=<t><TAB><one serialized entry, cut to fit>
//   VYSHKA_CATALOG<TAB>finished<TAB>wallMs=<frame clock ms>

class VyshkaCatalogProbe
{
	static ref VyshkaCatalogProbe s_Instance;

	static const string TAG = "VYSHKA_CATALOG";
	static const int SETTLE_MS = 3000;
	static const int GAP_MS = 50;
	// The performance counter runs at 10 MHz (spikes/dayz-bans-pull-size).
	static const int TICKS_PER_MS = 10000;
	static const int SAMPLES_PER_TYPE = 2;
	static const int SAMPLE_MAX = 200;

	// The types, in classification order: the first base found on the
	// class's inheritance path wins, so a magazine that is also an item is
	// a magazine, and an optic that is also inventory is an optic.
	static const int TYPE_FIREARM = 0;
	static const int TYPE_OPTIC = 1;
	static const int TYPE_AMMO = 2;
	static const int TYPE_MAGAZINE = 3;
	static const int TYPE_EDIBLE = 4;
	static const int TYPE_CLOTHING = 5;
	static const int TYPE_VEHICLE = 6;
	static const int TYPE_OTHER = 7;
	static const int TYPE_COUNT = 8;

	ref array<string> m_TypeNames;
	ref array<ref array<string>> m_Names;   // class names per type
	ref array<ref array<string>> m_Trees;   // the config tree each class was found in, parallel
	int m_Step;
	int m_PlanTime;

	static void Run()
	{
		if (s_Instance)
			return;
		s_Instance = new VyshkaCatalogProbe();
		s_Instance.Schedule();
	}

	void VyshkaCatalogProbe()
	{
		m_TypeNames = new array<string>;
		m_TypeNames.Insert("firearm");
		m_TypeNames.Insert("optic");
		m_TypeNames.Insert("ammo");
		m_TypeNames.Insert("magazine");
		m_TypeNames.Insert("edible");
		m_TypeNames.Insert("clothing");
		m_TypeNames.Insert("vehicle");
		m_TypeNames.Insert("other");
		m_Names = new array<ref array<string>>;
		m_Trees = new array<ref array<string>>;
		for (int i = 0; i < TYPE_COUNT; i++)
		{
			m_Names.Insert(new array<string>);
			m_Trees.Insert(new array<string>);
		}
	}

	void Schedule()
	{
		GetGame().GetCallQueue(CALL_CATEGORY_SYSTEM).CallLater(Start, SETTLE_MS, false);
	}

	void Start()
	{
		m_Step = 0;
		m_PlanTime = GetGame().GetTime();
		Print(TAG + "\tplan=" + (TYPE_COUNT + 1).ToString());
		Next();
	}

	// Next runs one phase per frame: the walk, then one type at a time, so
	// the frame clock sees each and a phase that stalls the server stalls
	// it once.
	void Next()
	{
		int step = m_Step;
		m_Step++;
		if (step == 0)
		{
			Walk("CfgVehicles");
			Walk("CfgWeapons");
			Walk("CfgMagazines");
		}
		else if (step <= TYPE_COUNT)
		{
			Build(step - 1);
		}
		else
		{
			Finish();
			return;
		}
		GetGame().GetCallQueue(CALL_CATEGORY_SYSTEM).CallLater(Next, GAP_MS, false);
	}

	void Finish()
	{
		int wallMs = GetGame().GetTime() - m_PlanTime;
		Print(TAG + "\tfinished\twallMs=" + wallMs.ToString());
	}

	static int Millis(int ticks)
	{
		return ticks / TICKS_PER_MS;
	}

	// Walk sorts every public class of one config tree into a type.
	void Walk(string tree)
	{
		int t0 = TickCount(0);
		int children = GetGame().ConfigGetChildrenCount(tree);
		int publicCount = 0;
		int sorted = 0;
		TStringArray path = new TStringArray;
		for (int i = 0; i < children; i++)
		{
			string name;
			if (!GetGame().ConfigGetChildName(tree, i, name))
				continue;
			string classPath = tree + " " + name;
			if (GetGame().ConfigGetInt(classPath + " scope") != 2)
				continue;
			publicCount++;
			path.Clear();
			GetGame().ConfigGetFullPath(classPath, path);
			int type = Classify(path);
			if (type < 0)
				continue;
			sorted++;
			m_Names.Get(type).Insert(name);
			m_Trees.Get(type).Insert(tree);
		}
		int ms = Millis(TickCount(t0));
		Print(TAG + "\ttree=" + tree + "\tchildren=" + children.ToString() + "\tpublic=" + publicCount.ToString() + "\tsorted=" + sorted.ToString() + "\tms=" + ms.ToString());
	}

	// Classify reads a class's inheritance path (itself first, then its
	// bases up to the root) and names the first catalog type it belongs to.
	// The path's own first element is the class, so a base class that is
	// itself public (scope 2 on Weapon_Base would be a config oddity) is
	// sorted by its parents like anything else.
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
			return TYPE_OTHER;
		return -1;
	}

	// Build makes one type's entries the way a context would and measures
	// what they cost and weigh.
	void Build(int type)
	{
		array<string> names = m_Names.Get(type);
		array<string> trees = m_Trees.Get(type);
		string typeName = m_TypeNames.Get(type);
		int untranslated = 0;
		int t0 = TickCount(0);
		VyshkaContextList list = new VyshkaContextList();
		for (int i = 0; i < names.Count(); i++)
		{
			string name = names.Get(i);
			string classPath = trees.Get(i) + " " + name;
			string label = DisplayName(classPath, name, untranslated);
			VyshkaJsonValue data = Stats(type, classPath);
			list.AddEntry(name, label, null, data);
		}
		int buildMs = Millis(TickCount(t0));
		int t1 = TickCount(0);
		string serialized = list.ToJson().Serialize();
		int serializeMs = Millis(TickCount(t1));
		int bytes = serialized.Length();
		Print(TAG + "\ttype=" + typeName + "\tcount=" + list.Count().ToString() + "\tbytes=" + bytes.ToString() + "\tbuildMs=" + buildMs.ToString() + "\tserializeMs=" + serializeMs.ToString() + "\tuntranslated=" + untranslated.ToString() + "\tdropped=" + list.Dropped().ToString());
		VyshkaJsonValue entries = list.ToJson();
		int samples = SAMPLES_PER_TYPE;
		if (entries.Count() < samples)
			samples = entries.Count();
		for (int s = 0; s < samples; s++)
		{
			// Spread the samples over the list rather than taking the first
			// two, which in CfgVehicles are the same base's variants.
			int index = (s * entries.Count()) / samples;
			VyshkaJsonValue entry = entries.At(index);
			if (!entry)
				continue;
			string sample = entry.Serialize();
			if (sample.Length() > SAMPLE_MAX)
				sample = sample.Substring(0, SAMPLE_MAX);
			Print(TAG + "\tsample=" + typeName + "\t" + sample);
		}
	}

	// DisplayName resolves a class's displayName the way Object.GetDisplayName
	// does (the config text through Widget.TranslateString), falling back to
	// the class name, and counts the names the server could not translate.
	static string DisplayName(string classPath, string name, out int untranslated)
	{
		string raw = GetGame().ConfigGetTextOut(classPath + " displayName");
		if (raw == "")
			return name;
		string translated = Widget.TranslateString(raw);
		if (translated == "" || translated == raw && raw.Get(0) == "$")
		{
			untranslated++;
			return name;
		}
		return translated;
	}

	// Stats reads the per-type extras the catalog would carry in data.
	static VyshkaJsonValue Stats(int type, string classPath)
	{
		VyshkaJsonValue data = VyshkaJsonValue.NewObject();
		data.Set("type", VyshkaJsonValue.NewString(s_Instance.m_TypeNames.Get(type)));
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
			SetFloat(data, "energy", GetGame().ConfigGetFloat(classPath + " Nutrition energy"));
			SetFloat(data, "water", GetGame().ConfigGetFloat(classPath + " Nutrition water"));
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

	static void SetFloat(VyshkaJsonValue data, string key, float value)
	{
		VyshkaJsonValue rendered = VyshkaJsonValue.NewFloat(value);
		if (rendered)
			data.Set(key, rendered);
	}
}
