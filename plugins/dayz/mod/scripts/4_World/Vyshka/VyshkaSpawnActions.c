// Vyshka DayZ plugin: the spawn action (issue #66, extended by issue #75).
//
// vyshka.spawn creates one item for a player: on the ground in front of
// them (the default), somewhere in their inventory, or in their hands, with
// a quantity (a stack's count, a bottle's fill, a magazine's rounds) and a
// health (a percent of the item's own maximum) when they are given, and
// with `attachments: auto`, every attachment slot the item has filled from
// config: a firearm gets its first compatible magazine, loaded and
// chambered through the engine's own spawn-with-ammo call, then a part in
// every slot it declares, and each part a part of its own where it has
// slots (a battery in a light), so a weapon arrives ready to use.
//
// The server's class-name blocklist (`spawnBlocklist` in the plugin's
// config.json) is published in the action's params schema as a `not`
// clause (spec section 6.1), so the hub refuses a blocked name before it is
// queued and a panel shows the blocked entries of the item catalog as
// unavailable, with no manifest field of its own. The hub compares names
// exactly and the engine resolves them without regard to case, so the
// action enforces the list itself as well, case-insensitively, and keeps a
// blocked class out of what `auto` attaches.
//
// Everything here uses what the engine gives every server: CreateObjectEx
// with the placement flags the central economy uses, the inventory's own
// placement search (the one a player's pick-up makes), the hands' own
// creation, Weapon_Base.SpawnAmmo, attachment creation by slot, and the
// item's quantity and health setters.

// VyshkaFsmRunning says whether a firearm's state machine runs, which
// SpawnAmmo's closing synchronization needs: on a firearm without one (a
// bow, and the launchers, the M249, the dart gun, and the shock pistol when
// they are just created; spikes/dayz-spawn-attachments) the engine logs a
// script error for every load. The machine is protected, hence the modded
// class.
modded class Weapon_Base
{
	bool VyshkaFsmRunning()
	{
		return m_fsm && m_fsm.IsRunning();
	}
}

class VyshkaSpawn
{
	static const string INTO_GROUND = "ground";
	static const string INTO_INVENTORY = "inventory";
	static const string INTO_HANDS = "hands";
	static const string ATTACH_NONE = "none";
	static const string ATTACH_AUTO = "auto";

	// How many levels `auto` fills: the item's own slots, then the slots of
	// what was attached to it (a battery in a light, a knife in a sheath).
	// A part's parts' parts are not filled; nothing in the stock game needs
	// a third level.
	static const int AUTO_DEPTH = 2;
	// How many compatible classes are tried for one slot before it is left
	// empty: the engine refuses a part a slot's name admits when the item
	// rejects it (an optic a rail does not take).
	static const int SLOT_TRIES = 8;
	// The most classes a blocklist may name, which keeps the manifest a
	// manifest; entries past it are dropped and logged.
	static const int BLOCKLIST_MAX = 500;
	static const float HEALTH_MAX = 100.0;

	// The blocklist: the names as published (canonical case where the
	// server's config declares the class, sorted), and the same lowercased,
	// which is what a class name is checked against.
	static ref array<string> s_Blocklist;
	static ref map<string, bool> s_Blocked;
	// The attachment candidates by slot, lowercased slot name to class
	// names in the order `auto` tries them; built on the first auto spawn.
	static ref map<string, ref array<string>> s_Candidates;

	static void Reset()
	{
		s_Blocklist = new array<string>;
		s_Blocked = new map<string, bool>;
		s_Candidates = null;
	}

	static array<string> Blocklist()
	{
		if (!s_Blocklist)
			Reset();
		return s_Blocklist;
	}

	static bool IsBlocked(string className)
	{
		if (!s_Blocked)
			Reset();
		string lowered = className;
		lowered.ToLower();
		return s_Blocked.Contains(lowered);
	}

	// LoadBlocklist reads `spawnBlocklist` from the plugin's config.json, at
	// boot and before the actions register, so the manifest carries it from
	// the first publish. Each entry must be a class name; one this server
	// declares is published in the config's own case, which is the case the
	// item catalog enumerates and so the case a panel compares, and one it
	// does not declare is kept as written and logged.
	static void LoadBlocklist()
	{
		Reset();
		if (!FileExist(VyshkaFiles.CONFIG_PATH))
			return;
		VyshkaJsonValue root = VyshkaFiles.ReadJson(VyshkaFiles.CONFIG_PATH);
		if (!root || !root.IsObject())
			return;
		VyshkaJsonValue list = root.Get("spawnBlocklist");
		if (!list || list.IsNull())
			return;
		if (!list.IsArray())
		{
			VyshkaLog.Warn("spawnBlocklist in " + VyshkaFiles.CONFIG_PATH + " is not an array of class names; no class is blocked");
			return;
		}
		map<string, string> wanted = new map<string, string>;   // lowercased to as written
		for (int i = 0; i < list.Count(); i++)
		{
			VyshkaJsonValue entry = list.At(i);
			if (!entry || !entry.IsString())
			{
				VyshkaLog.Warn("spawnBlocklist entry " + i.ToString() + " is not a string and was ignored");
				continue;
			}
			string name = entry.m_Text;
			name = name.Trim();
			if (!VyshkaWorld.ValidClassName(name) || name.Length() > VyshkaWorld.MAX_CLASS_NAME)
			{
				VyshkaLog.Warn("spawnBlocklist entry " + name + " is not a class name (letters, digits, and underscores) and was ignored");
				continue;
			}
			string key = name;
			key.ToLower();
			if (wanted.Contains(key))
				continue;
			if (wanted.Count() >= BLOCKLIST_MAX)
			{
				VyshkaLog.Warn("spawnBlocklist names more than " + BLOCKLIST_MAX.ToString() + " classes; " + name + " and the rest were ignored");
				break;
			}
			wanted.Set(key, name);
		}
		if (wanted.Count() == 0)
			return;
		Canonicalize(wanted);
		array<string> names = new array<string>;
		for (int w = 0; w < wanted.Count(); w++)
		{
			string published = wanted.GetElement(w);
			int at = 0;
			while (at < names.Count() && VyshkaRegistry.Before(names.Get(at), published))
				at++;
			names.InsertAt(published, at);
			s_Blocked.Set(wanted.GetKey(w), true);
		}
		s_Blocklist = names;
		VyshkaLog.Info("spawn blocklist: " + names.Count().ToString() + " class(es) blocked");
	}

	// Canonicalize replaces each wanted name with the case the server's
	// config declares it in, walking the three trees an item can be
	// declared in once, and logs the names no tree declares.
	static void Canonicalize(map<string, string> wanted)
	{
		map<string, bool> found = new map<string, bool>;
		array<string> trees = new array<string>;
		trees.Insert(VyshkaCatalog.TREE_VEHICLES);
		trees.Insert(VyshkaCatalog.TREE_WEAPONS);
		trees.Insert(VyshkaCatalog.TREE_MAGAZINES);
		for (int t = 0; t < trees.Count(); t++)
		{
			string tree = trees.Get(t);
			int children = GetGame().ConfigGetChildrenCount(tree);
			for (int c = 0; c < children; c++)
			{
				string child;
				if (!GetGame().ConfigGetChildName(tree, c, child))
					continue;
				string lowered = child;
				lowered.ToLower();
				if (!wanted.Contains(lowered) || found.Contains(lowered))
					continue;
				wanted.Set(lowered, child);
				found.Set(lowered, true);
			}
		}
		for (int w = 0; w < wanted.Count(); w++)
		{
			if (!found.Contains(wanted.GetKey(w)))
				VyshkaLog.Warn("spawnBlocklist names " + wanted.GetElement(w) + ", which no config tree on this server declares; it is blocked as written");
		}
	}

	static const string SORT_SEPARATOR = "#";

	// Explosive says whether a class's inheritance path (itself first, then
	// its bases) runs through a grenade or an explosive: `auto` never puts
	// one on an item, though a vest's grenade slots admit them.
	static bool Explosive(TStringArray path)
	{
		for (int i = 0; i < path.Count(); i++)
		{
			string base = path.Get(i);
			base.ToLower();
			if (base == "grenade_base" || base == "explosivesbase")
				return true;
		}
		return false;
	}

	// Candidates lists the classes `auto` tries for a slot, in order: the
	// public items whose inventorySlot names the slot, the ones made for
	// fewer slots first (a rifle's own suppressor before the improvised one
	// that fits every muzzle), then by name.
	static array<string> Candidates(string slotName)
	{
		if (!s_Candidates)
			BuildCandidates();
		string lowered = slotName;
		lowered.ToLower();
		array<string> found = s_Candidates.Get(lowered);
		if (!found)
			return new array<string>;
		return found;
	}

	// BuildCandidates indexes every public item of CfgVehicles by the slots
	// it declares. Parts, batteries, clothing, and car parts are declared
	// there; firearms (CfgWeapons) and magazines (CfgMagazines) are not
	// indexed, so `auto` never attaches a firearm, and a firearm's magazine
	// comes from its own magazines list instead. Grenades and explosives are
	// left out as well.
	static void BuildCandidates()
	{
		s_Candidates = new map<string, ref array<string>>;
		int t0 = TickCount(0);
		string tree = VyshkaCatalog.TREE_VEHICLES;
		int children = GetGame().ConfigGetChildrenCount(tree);
		TStringArray path = new TStringArray;
		TStringArray slots = new TStringArray;
		int indexed = 0;
		for (int c = 0; c < children; c++)
		{
			string name;
			if (!GetGame().ConfigGetChildName(tree, c, name))
				continue;
			string classPath = tree + " " + name;
			if (GetGame().ConfigGetInt(classPath + " scope") != 2)
				continue;
			path.Clear();
			GetGame().ConfigGetFullPath(classPath, path);
			int type = VyshkaCatalog.Classify(path);
			if (type < 0 || type == VyshkaCatalog.TYPE_VEHICLE || Explosive(path))
				continue;
			slots.Clear();
			GetGame().ConfigGetTextArray(classPath + " inventorySlot", slots);
			if (slots.Count() == 0)
			{
				string single = GetGame().ConfigGetTextOut(classPath + " inventorySlot");
				if (single != "")
					slots.Insert(single);
			}
			if (slots.Count() == 0)
				continue;
			// A sort key per class, so the native sort orders the list: the
			// count of slots it fits (two digits), then its name. The
			// separator sorts below every character a class name has, so a
			// name comes before its own variants (M4_MPHndgrd before
			// M4_MPHndgrd_Black, HatchbackWheel before HatchbackWheel_Ruined).
			string width = slots.Count().ToString();
			if (slots.Count() < 10)
				width = "0" + width;
			string sortName = name;
			sortName.ToLower();
			string sortKey = width + SORT_SEPARATOR + sortName + SORT_SEPARATOR + name;
			for (int s = 0; s < slots.Count(); s++)
			{
				string slot = slots.Get(s);
				slot.ToLower();
				array<string> list = s_Candidates.Get(slot);
				if (!list)
				{
					list = new array<string>;
					s_Candidates.Set(slot, list);
				}
				list.Insert(sortKey);
			}
			indexed++;
		}
		for (int k = 0; k < s_Candidates.Count(); k++)
		{
			array<string> keys = s_Candidates.GetElement(k);
			keys.Sort();
			for (int j = 0; j < keys.Count(); j++)
			{
				string sorted = keys.Get(j);
				int bar = sorted.LastIndexOf(SORT_SEPARATOR);
				keys.Set(j, sorted.Substring(bar + 1, sorted.Length() - bar - 1));
			}
		}
		int ms = TickCount(t0) / VyshkaCatalog.TICKS_PER_MS;
		VyshkaLog.Info("attachment index built: " + indexed.ToString() + " parts over " + s_Candidates.Count().ToString() + " slots in " + ms.ToString() + " ms");
	}

	// LoadWith picks what a firearm is loaded with: the first magazine its
	// config lists (the one the designers put first), or for a firearm fed
	// by hand (an internal magazine, a single chamber) the first round it
	// chambers; a blocked one is passed over. "" when there is none.
	static string LoadWith(EntityAI weapon)
	{
		string path = VyshkaCatalog.TREE_WEAPONS + " " + weapon.GetType();
		TStringArray names = new TStringArray;
		GetGame().ConfigGetTextArray(path + " magazines", names);
		if (names.Count() == 0)
			GetGame().ConfigGetTextArray(path + " chamberableFrom", names);
		for (int i = 0; i < names.Count(); i++)
		{
			if (!IsBlocked(names.Get(i)))
				return names.Get(i);
		}
		return "";
	}

	// Equip fills an item's empty attachment slots, down to AUTO_DEPTH
	// levels, and describes what it attached in `attached` (one
	// `{ slot, class }` per part, with the part's own `attachments` inside)
	// and what it left empty in `empty` (`{ slot, on }`, the slot and the
	// class of the item it is on). A firearm is loaded first; the class it
	// was loaded with is returned, "" when it was not loaded.
	static string Equip(EntityAI item, int depth, VyshkaJsonValue attached, VyshkaJsonValue empty)
	{
		string loaded = "";
		GameInventory inventory = item.GetInventory();
		if (!inventory)
			return loaded;
		Weapon_Base weapon = Weapon_Base.Cast(item);
		string ammo = "";
		if (weapon && depth == 1)
			ammo = LoadWith(item);
		if (ammo != "" && weapon.VyshkaFsmRunning())
		{
			// The engine's own spawn-with-ammo: a magazine attached (or an
			// internal one filled) and a round chambered, and the weapon's
			// state machine told, so it fires at once.
			if (weapon.SpawnAmmo(ammo, WeaponWithAmmoFlags.CHAMBER))
				loaded = ammo;
		}
		else if (ammo != "" && weapon.GetMagazineTypeCount(0) > 0)
		{
			// No running state machine (measured: the M249 and the shock
			// pistol), so SpawnAmmo would log a script error on its closing
			// synchronization. A detachable magazine is attached full
			// instead, as a part, with nothing chambered.
			Magazine box = Magazine.Cast(inventory.CreateAttachment(ammo));
			if (box)
			{
				box.ServerSetAmmoMax();
				loaded = ammo;
			}
		}
		int slotCount = inventory.GetAttachmentSlotsCount();
		for (int i = 0; i < slotCount; i++)
		{
			int slotId = inventory.GetAttachmentSlotId(i);
			string slotName = InventorySlots.GetSlotName(slotId);
			EntityAI part = inventory.FindAttachment(slotId);
			// The M249 declares a slot without a name (the live run of
			// issue #75): no part can be looked up for it, so an empty one
			// is passed over rather than reported as a gap.
			if (!part && slotName == "")
				continue;
			if (!part)
			{
				array<string> candidates = Candidates(slotName);
				int tries = 0;
				for (int c = 0; c < candidates.Count() && tries < SLOT_TRIES && !part; c++)
				{
					string candidate = candidates.Get(c);
					if (IsBlocked(candidate))
						continue;
					tries++;
					part = inventory.CreateAttachmentEx(candidate, slotId);
					// A part the config makes ruined (a ruined wheel is a
					// class of its own) is taken off again.
					if (part && part.GetHealthLevel() == GameConstants.STATE_RUINED)
					{
						GetGame().ObjectDelete(part);
						part = null;
					}
				}
				if (!part)
				{
					VyshkaJsonValue gap = VyshkaJsonValue.NewObject();
					gap.Set("slot", VyshkaJsonValue.NewString(slotName));
					gap.Set("on", VyshkaJsonValue.NewString(item.GetType()));
					empty.Add(gap);
					continue;
				}
			}
			VyshkaJsonValue entry = VyshkaJsonValue.NewObject();
			entry.Set("slot", VyshkaJsonValue.NewString(slotName));
			entry.Set("class", VyshkaJsonValue.NewString(part.GetType()));
			Magazine magazine = Magazine.Cast(part);
			if (magazine)
				entry.Set("ammo", VyshkaJsonValue.NewInt(magazine.GetAmmoCount()));
			if (depth < AUTO_DEPTH)
			{
				VyshkaJsonValue inner = VyshkaJsonValue.NewArray();
				Equip(part, depth + 1, inner, empty);
				if (inner.Count() > 0)
					entry.Set("attachments", inner);
			}
			attached.Add(entry);
		}
		return loaded;
	}

	// Place describes where an item ended up: `placed` (ground, hands,
	// attachment, cargo), and for an attachment or cargo the `slot` and the
	// `container` it is in.
	static void Place(EntityAI item, VyshkaJsonValue result)
	{
		InventoryLocation location = new InventoryLocation();
		string placed = "ground";
		if (item.GetInventory() && item.GetInventory().GetCurrentInventoryLocation(location))
		{
			int type = location.GetType();
			if (type == InventoryLocationType.HANDS)
				placed = "hands";
			else if (type == InventoryLocationType.ATTACHMENT)
			{
				placed = "attachment";
				result.Set("slot", VyshkaJsonValue.NewString(InventorySlots.GetSlotName(location.GetSlot())));
			}
			else if (type == InventoryLocationType.CARGO)
				placed = "cargo";
			if ((type == InventoryLocationType.ATTACHMENT || type == InventoryLocationType.CARGO) && location.GetParent())
				result.Set("container", VyshkaJsonValue.NewString(location.GetParent().GetType()));
		}
		result.Set("placed", VyshkaJsonValue.NewString(placed));
	}

	// SetQuantity applies a dispatched quantity, or explains why it cannot:
	// a magazine's or an ammunition pile's rounds (a whole number up to its
	// capacity, at least one for a pile), otherwise the item's own quantity
	// within the range the engine holds for it (a whole number for a
	// stack). An item the engine deletes at its minimum quantity cannot be
	// spawned at it.
	static bool SetQuantity(EntityAI item, float quantity, out string error)
	{
		string className = item.GetType();
		Magazine magazine = Magazine.Cast(item);
		if (magazine)
		{
			int capacity = magazine.GetAmmoMax();
			int rounds = (int)quantity;
			float roundsAsFloat = rounds;
			if (roundsAsFloat != quantity)
			{
				error = "the quantity of " + className + " is a count of rounds and must be a whole number";
				return false;
			}
			// A pile of loose rounds is its rounds: an empty one is not an
			// item a player can hold, though the engine creates it. A
			// magazine may be empty.
			int least = 0;
			if (Ammunition_Base.Cast(item))
				least = 1;
			if (rounds < least || rounds > capacity)
			{
				error = "the quantity of " + className + " is a count of rounds within " + least.ToString() + " and " + capacity.ToString();
				return false;
			}
			magazine.ServerSetAmmoCount(rounds);
			return true;
		}
		ItemBase asItem = ItemBase.Cast(item);
		if (!asItem || !asItem.HasQuantity())
		{
			error = className + " has no quantity";
			return false;
		}
		int min = asItem.GetQuantityMin();
		int max = asItem.GetQuantityMax();
		if (quantity < min || quantity > max)
		{
			error = "the quantity of " + className + " must lie within " + min.ToString() + " and " + max.ToString();
			return false;
		}
		int whole = (int)quantity;
		float wholeAsFloat = whole;
		if (asItem.IsSplitable() && wholeAsFloat != quantity)
		{
			error = className + " is a stack, so its quantity must be a whole number";
			return false;
		}
		if (quantity <= min && asItem.ConfigGetBool("varQuantityDestroyOnMin"))
		{
			error = "the engine deletes " + className + " at quantity " + min.ToString() + "; give more than " + min.ToString();
			return false;
		}
		asItem.SetQuantity(quantity);
		return true;
	}

	// SetHealth applies a dispatched health, a percent of the item's own
	// maximum, the unit the inventory read reports.
	static bool SetHealth(EntityAI item, float percent, out string error)
	{
		float max = 0;
		if (VyshkaInventory.HasHealth(item))
			max = item.GetMaxHealth(VyshkaInventory.ZONE_GLOBAL, VyshkaInventory.HEALTH_TYPE);
		if (max <= 0)
		{
			error = item.GetType() + " has no health to set";
			return false;
		}
		item.SetHealth01(VyshkaInventory.ZONE_GLOBAL, VyshkaInventory.HEALTH_TYPE, percent / HEALTH_MAX);
		return true;
	}

	// Discard removes an item the action created and then failed on, so a
	// refused spawn leaves nothing behind: in an inventory through the
	// engine's safe delete, on the ground at once.
	static void Discard(Object created)
	{
		EntityAI entity = EntityAI.Cast(created);
		InventoryLocation location = new InventoryLocation();
		if (entity && entity.GetInventory() && entity.GetInventory().GetCurrentInventoryLocation(location) && location.GetType() != InventoryLocationType.GROUND)
		{
			entity.DeleteSafe();
			return;
		}
		GetGame().ObjectDelete(created);
	}

	// ReadNumber reads an optional number param: false when it is absent
	// (or null), true with the value when it is a number.
	static bool ReadNumber(VyshkaJsonValue params, string key, out float value)
	{
		if (!params || !params.IsObject())
			return false;
		VyshkaJsonValue number = params.Get(key);
		if (!number || number.IsNull() || !number.IsNumber())
			return false;
		value = number.m_Number;
		return true;
	}
}

class VyshkaSpawnAction : VyshkaAction
{
	override string Code()    { return "vyshka.spawn"; }
	override string Name()    { return "Spawn item"; }
	override string Context() { return "player"; }
	override string Danger()  { return "warning"; }

	override VyshkaJsonValue ParamsSchema()
	{
		VyshkaJsonValue className = VyshkaJsonValue.NewObject();
		className.Set("type", VyshkaJsonValue.NewString("string"));
		className.Set("x-vyshka-widget", VyshkaJsonValue.NewString("itemlist"));
		// The catalog's contexts (spec section 6.1): a panel offers their
		// entries as the names to pick from, and still sends what is typed.
		className.Set("context", VyshkaCatalog.ContextIds());
		// The blocklist as the schema's exclusion (section 6.1): the hub
		// refuses a blocked name and a panel shows it as unavailable.
		array<string> blocked = VyshkaSpawn.Blocklist();
		if (blocked.Count() > 0)
		{
			VyshkaJsonValue names = VyshkaJsonValue.NewArray();
			for (int i = 0; i < blocked.Count(); i++)
				names.Add(VyshkaJsonValue.NewString(blocked.Get(i)));
			VyshkaJsonValue exclusion = VyshkaJsonValue.NewObject();
			exclusion.Set("enum", names);
			className.Set("not", exclusion);
		}

		VyshkaJsonValue into = VyshkaJsonValue.NewObject();
		into.Set("type", VyshkaJsonValue.NewString("string"));
		VyshkaJsonValue places = VyshkaJsonValue.NewArray();
		places.Add(VyshkaJsonValue.NewString(VyshkaSpawn.INTO_GROUND));
		places.Add(VyshkaJsonValue.NewString(VyshkaSpawn.INTO_INVENTORY));
		places.Add(VyshkaJsonValue.NewString(VyshkaSpawn.INTO_HANDS));
		into.Set("enum", places);
		into.Set("default", VyshkaJsonValue.NewString(VyshkaSpawn.INTO_GROUND));

		VyshkaJsonValue quantity = VyshkaJsonValue.NewObject();
		quantity.Set("type", VyshkaJsonValue.NewString("number"));
		quantity.Set("minimum", VyshkaJsonValue.NewInt(0));

		VyshkaJsonValue health = VyshkaJsonValue.NewObject();
		health.Set("type", VyshkaJsonValue.NewString("number"));
		health.Set("minimum", VyshkaJsonValue.NewInt(0));
		health.Set("maximum", VyshkaJsonValue.NewInt(100));

		VyshkaJsonValue attachments = VyshkaJsonValue.NewObject();
		attachments.Set("type", VyshkaJsonValue.NewString("string"));
		VyshkaJsonValue modes = VyshkaJsonValue.NewArray();
		modes.Add(VyshkaJsonValue.NewString(VyshkaSpawn.ATTACH_NONE));
		modes.Add(VyshkaJsonValue.NewString(VyshkaSpawn.ATTACH_AUTO));
		attachments.Set("enum", modes);
		attachments.Set("default", VyshkaJsonValue.NewString(VyshkaSpawn.ATTACH_NONE));

		VyshkaJsonValue properties = VyshkaJsonValue.NewObject();
		properties.Set("className", className);
		properties.Set("into", into);
		properties.Set("quantity", quantity);
		properties.Set("health", health);
		properties.Set("attachments", attachments);

		VyshkaJsonValue required = VyshkaJsonValue.NewArray();
		required.Add(VyshkaJsonValue.NewString("className"));

		VyshkaJsonValue schema = VyshkaJsonValue.NewObject();
		schema.Set("type", VyshkaJsonValue.NewString("object"));
		schema.Set("required", required);
		schema.Set("properties", properties);
		return schema;
	}

	override VyshkaActionOutcome Execute(string actionId, string context, string referenceKey, VyshkaJsonValue params)
	{
		if (referenceKey == "")
			return VyshkaActionOutcome.Failure("a player-context action needs the player's identity as referenceKey");
		string className = VyshkaAction.ReadText(params, "className", VyshkaWorld.MAX_CLASS_NAME);
		if (className == "")
			return VyshkaActionOutcome.Failure("className is required and must not be blank");
		if (!VyshkaWorld.ValidClassName(className))
			return VyshkaActionOutcome.Failure("className may contain only letters, digits, and underscores");
		if (VyshkaSpawn.IsBlocked(className))
			return VyshkaActionOutcome.Failure(className + " is blocked on this server (spawnBlocklist in the plugin's config)");
		int scope;
		string tree = VyshkaWorld.FindConfig(className, scope);
		if (tree == "")
			return VyshkaActionOutcome.Failure("no item class named " + className + " exists on this server");
		if (scope != 2)
			return VyshkaActionOutcome.Failure(className + " is a base class, not an item the engine will create");

		string into = VyshkaAction.ReadText(params, "into", 16);
		if (into == "")
			into = VyshkaSpawn.INTO_GROUND;
		if (into != VyshkaSpawn.INTO_GROUND && into != VyshkaSpawn.INTO_INVENTORY && into != VyshkaSpawn.INTO_HANDS)
			return VyshkaActionOutcome.Failure("into must be ground, inventory, or hands");
		string attachments = VyshkaAction.ReadText(params, "attachments", 16);
		if (attachments == "")
			attachments = VyshkaSpawn.ATTACH_NONE;
		if (attachments != VyshkaSpawn.ATTACH_NONE && attachments != VyshkaSpawn.ATTACH_AUTO)
			return VyshkaActionOutcome.Failure("attachments must be none or auto");
		float quantity;
		bool hasQuantity = VyshkaSpawn.ReadNumber(params, "quantity", quantity);
		float health;
		bool hasHealth = VyshkaSpawn.ReadNumber(params, "health", health);
		if (hasHealth && (health < 0 || health > VyshkaSpawn.HEALTH_MAX))
			return VyshkaActionOutcome.Failure("health is a percent of the item's maximum, within 0 and 100");
		if (hasQuantity && quantity < 0)
			return VyshkaActionOutcome.Failure("quantity must not be negative");

		string error;
		PlayerBase player = VyshkaVitals.Player(referenceKey, error);
		if (!player)
			return VyshkaActionOutcome.Failure(error);

		Object created;
		if (into == VyshkaSpawn.INTO_HANDS)
		{
			EntityAI holding = player.GetHumanInventory().GetEntityInHands();
			if (holding)
				return VyshkaActionOutcome.Failure("the player's hands are full (" + holding.GetType() + "); spawn it into the inventory or on the ground instead");
			created = player.GetHumanInventory().CreateInHands(className);
			if (!created)
				return VyshkaActionOutcome.Failure("the engine would not put " + className + " in the player's hands");
		}
		else if (into == VyshkaSpawn.INTO_INVENTORY)
		{
			// The inventory's own search for a place a new item fits: a free
			// attachment slot or cargo space, and the hands when nothing
			// else has room (the engine's own fallback).
			created = player.GetInventory().CreateInInventory(className);
			if (!created)
				return VyshkaActionOutcome.Failure("the engine found no place for " + className + " in the player's inventory or hands");
		}
		else
		{
			vector position = player.GetPosition() + player.GetDirection() * VyshkaWorld.SPAWN_DISTANCE;
			position[1] = GetGame().SurfaceY(position[0], position[2]);
			created = GetGame().CreateObjectEx(className, position, ECE_PLACE_ON_SURFACE);
			if (!created)
				return VyshkaActionOutcome.Failure("the engine refused to create " + className);
		}

		EntityAI item = EntityAI.Cast(created);
		VyshkaJsonValue attached = null;
		VyshkaJsonValue empty = null;
		string loaded = "";
		if (attachments == VyshkaSpawn.ATTACH_AUTO && item)
		{
			attached = VyshkaJsonValue.NewArray();
			empty = VyshkaJsonValue.NewArray();
			loaded = VyshkaSpawn.Equip(item, 1, attached, empty);
		}
		if (hasQuantity)
		{
			if (!item || !VyshkaSpawn.SetQuantity(item, quantity, error))
			{
				if (!item)
					error = className + " has no quantity";
				VyshkaSpawn.Discard(created);
				return VyshkaActionOutcome.Failure(error + "; nothing was spawned");
			}
		}
		if (hasHealth)
		{
			if (!item || !VyshkaSpawn.SetHealth(item, health, error))
			{
				if (!item)
					error = className + " has no health to set";
				VyshkaSpawn.Discard(created);
				return VyshkaActionOutcome.Failure(error + "; nothing was spawned");
			}
		}

		VyshkaJsonValue result = VyshkaVitals.Result(player);
		result.Set("className", VyshkaJsonValue.NewString(created.GetType()));
		result.Set("displayName", VyshkaJsonValue.NewString(created.GetDisplayName()));
		result.Set("config", VyshkaJsonValue.NewString(tree));
		result.Set("into", VyshkaJsonValue.NewString(into));
		if (item)
		{
			VyshkaSpawn.Place(item, result);
			// The same condition fields the inventory read reports: health
			// and state, quantity or ammo, a firearm's rounds.
			VyshkaInventoryTree.Condition(item, result);
		}
		else
			result.Set("placed", VyshkaJsonValue.NewString("ground"));
		VyshkaJsonValue where = VyshkaPlayers.Position(created.GetPosition());
		if (where)
			result.Set("position", where);
		if (attached)
		{
			if (Weapon_Base.Cast(item))
			{
				if (loaded != "")
					result.Set("loaded", VyshkaJsonValue.NewString(loaded));
				else
					result.Set("loaded", VyshkaJsonValue.NewNull());
			}
			result.Set("attachments", attached);
			result.Set("empty", empty);
		}
		string placedAs = result.GetString("placed", "ground");
		VyshkaLog.Info("spawned " + created.GetType() + " for " + VyshkaVitals.Describe(player) + " (" + placedAs + ") at " + created.GetPosition().ToString());
		return VyshkaActionOutcome.Success(result);
	}
}
