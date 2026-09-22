// Vyshka DayZ plugin: presets (issue #76).
//
// Named records an operator keeps in the hub's key/value store (spec section
// 12), and the actions that apply them: loadouts (an item tree put on a
// player), locations (a teleport destination with a scatter radius), and
// vehicle presets (a car with the parts and the fluids it needs to drive). A
// loadout can also be captured from what a player wears and holds.
//
// Each kind lives in a namespace of its own (vyshka.loadouts,
// vyshka.locations, vyshka.vehicles), so a token may be granted the editing
// of one kind without the others, and without the admin flags the plugin
// keeps under `vyshka`. Dispatching an apply needs no store grant at all: the
// plugin reads the record under its own session, so who may apply presets
// and who may edit them are separate grants. The one exception is capture,
// which writes a loadout under the plugin's session: a token that may
// dispatch vyshka.loadout.capture may create loadouts, and with overwrite
// replace them, whatever store grant it holds. Each action's name param
// carries the kvNamespace annotation (section 6.1), so a panel offers the
// names the store holds.
//
// A record is read when the action runs, so an edit takes effect on the next
// dispatch with no restart and no manifest change. The store answers on its
// own schedule, so every apply is pending until the read comes back, bounded
// by the dispatch's own deadline; a read that lands after the dispatch was
// failed changes nothing in the game.
//
// Everything here uses what the engine gives every server: attachment and
// cargo creation by slot, the inventory's own placement search, the hands'
// own creation, the firearm's spawn-with-magazine call, the item's quantity,
// health, and liquid setters, CreateObjectEx for a car, and the car's own
// fluid calls.

class VyshkaPresets
{
	static const string NS_LOADOUTS = "vyshka.loadouts";
	static const string NS_LOCATIONS = "vyshka.locations";
	static const string NS_VEHICLES = "vyshka.vehicles";

	// A preset's name is its key (spec section 12.1).
	static const int NAME_MAX = 128;
	// The store's bound on one value's encoding (spec section 12.1).
	static const int VALUE_MAX = 16384;
	// How many levels of items a preset may nest: a worn bag is 1, what is
	// in it 2, a magazine in a rifle in the bag 4. The stock game needs 5;
	// the bound keeps a hand-written record inside what the plugin's parser
	// reads (32 levels, two per item level).
	static const int DEPTH_MAX = 8;
	// How many items one apply creates at most, a hand-written record's
	// bound on the frame it can stall; a heavy stock loadout is 131
	// (spikes/dayz-inventory-tree).
	static const int ITEMS_MAX = 400;
	// How many problems a result lists; the rest are counted.
	static const int PROBLEMS_MAX = 40;
	// The widest scatter a location may ask for, in metres.
	static const float RADIUS_MAX = 1000.0;
	// How many scattered points are tried before the location's own
	// position is used: a point past the map edge is drawn again.
	static const int SCATTER_TRIES = 8;
	// How far in front of a player a car is put, in metres: clear of the
	// character, close enough to walk to.
	static const float VEHICLE_DISTANCE = 6.0;
	// The store gets less than the dispatch has, so a read that runs out of
	// time fails the apply while the plugin still holds the dispatch.
	static const int DEADLINE_MARGIN_MS = 5000;

	static const string PREVIOUS_KEEP = "keep";
	static const string PREVIOUS_DROP = "drop";

	// Register declares the three namespaces and the preset actions.
	static void Register(VyshkaRegistry registry)
	{
		registry.DeclareNamespace(NS_LOADOUTS);
		registry.DeclareNamespace(NS_LOCATIONS);
		registry.DeclareNamespace(NS_VEHICLES);
		registry.Register(new VyshkaLoadoutApplyAction());
		registry.Register(new VyshkaLoadoutCaptureAction());
		registry.Register(new VyshkaLocationTeleportAction());
		registry.Register(new VyshkaVehicleSpawnAction());
	}

	// NameSchema is a preset name param: a string whose values a panel
	// offers from the namespace's keys (the kvNamespace annotation).
	static VyshkaJsonValue NameSchema(string namespace)
	{
		VyshkaJsonValue name = VyshkaJsonValue.NewObject();
		name.Set("type", VyshkaJsonValue.NewString("string"));
		name.Set("kvNamespace", VyshkaJsonValue.NewString(namespace));
		return name;
	}

	// ReadName reads a preset name param, which must be a key the store
	// accepts (spec section 12.1).
	static string ReadName(VyshkaJsonValue params, string key, out string error)
	{
		string name = VyshkaAction.ReadText(params, key, NAME_MAX + 1);
		if (name == "")
		{
			error = key + " is required: the name of the preset";
			return "";
		}
		if (!VyshkaStoreClient.ValidName(name, NAME_MAX))
		{
			error = key + " must be a store key: dot-separated segments of letters, digits, _, and -, at most " + NAME_MAX.ToString() + " characters";
			return "";
		}
		return name;
	}

	static int DeadlineMs()
	{
		return VyshkaPlugin.CurrentDeadlineMs() - DEADLINE_MARGIN_MS;
	}

	static bool SameText(string first, string second)
	{
		string a = first;
		string b = second;
		a.ToLower();
		b.ToLower();
		return a == b;
	}
}

// VyshkaPresetBuilder creates the items a preset's entries describe and keeps
// the account: how many were created, and what could not be, with the reason.
// An entry is { "class", "slot", "health", "quantity", "liquid", "loaded",
// "attachments": [ ... ], "cargo": [ ... ] }, every member but class optional.
class VyshkaPresetBuilder
{
	int m_Created;
	int m_ProblemCount;
	ref VyshkaJsonValue m_Problems;

	void VyshkaPresetBuilder()
	{
		m_Problems = VyshkaJsonValue.NewArray();
	}

	void Problem(string className, string reason)
	{
		m_ProblemCount++;
		if (m_Problems.Count() >= VyshkaPresets.PROBLEMS_MAX)
			return;
		VyshkaJsonValue problem = VyshkaJsonValue.NewObject();
		if (className != "")
			problem.Set("class", VyshkaJsonValue.NewString(VyshkaAction.Bound(className, VyshkaWorld.MAX_CLASS_NAME)));
		problem.Set("reason", VyshkaJsonValue.NewString(reason));
		m_Problems.Add(problem);
	}

	// Report adds the account to a result.
	void Report(VyshkaJsonValue result)
	{
		result.Set("created", VyshkaJsonValue.NewInt(m_Created));
		result.Set("problems", m_Problems);
		if (m_ProblemCount > m_Problems.Count())
			result.Set("problemCount", VyshkaJsonValue.NewInt(m_ProblemCount));
	}

	// Refusal checks an entry before anything is created for it: an object
	// naming a public class the server declares, not blocked, inside the
	// depth and the item budget. "" when it may be created; the class name
	// is read out either way, for the account.
	string Refusal(VyshkaJsonValue entry, int depth, out string className)
	{
		className = "";
		if (!entry || !entry.IsObject())
			return "an entry is not an object";
		VyshkaJsonValue named = entry.Get("class");
		if (!named || !named.IsString())
			return "an entry has no class";
		className = named.m_Text;
		className = className.Trim();
		if (!VyshkaWorld.ValidClassName(className) || className.Length() > VyshkaWorld.MAX_CLASS_NAME)
			return "not a class name";
		if (depth > VyshkaPresets.DEPTH_MAX)
			return "nested deeper than " + VyshkaPresets.DEPTH_MAX.ToString() + " levels";
		if (m_Created >= VyshkaPresets.ITEMS_MAX)
			return "past the " + VyshkaPresets.ITEMS_MAX.ToString() + " items one preset may create";
		if (VyshkaSpawn.IsBlocked(className))
			return "blocked on this server (spawnBlocklist)";
		int scope;
		string tree = VyshkaWorld.FindConfig(className, scope);
		if (tree == "")
			return "no such class on this server";
		if (scope != 2)
			return "a base class, not an item the engine will create";
		return "";
	}

	// Child creates one entry inside a container: into the slot it names
	// when it is an attachment, into cargo when it is cargo, and where the
	// container's own placement search finds room when that fails.
	EntityAI Child(EntityAI parent, VyshkaJsonValue entry, bool asCargo, int depth)
	{
		string className;
		string refusal = Refusal(entry, depth, className);
		if (refusal != "")
		{
			Problem(className, refusal);
			return null;
		}
		GameInventory inventory = parent.GetInventory();
		if (!inventory)
		{
			Problem(className, parent.GetType() + " has no inventory");
			return null;
		}
		EntityAI child = null;
		if (asCargo)
			child = inventory.CreateEntityInCargo(className);
		else
		{
			int slotId = SlotId(entry);
			if (slotId != InventorySlots.INVALID)
				child = inventory.CreateAttachmentEx(className, slotId);
			else
				child = inventory.CreateAttachment(className);
		}
		if (!child)
			child = inventory.CreateInInventory(className);
		if (!child)
		{
			Problem(className, "no room for it in " + parent.GetType());
			return null;
		}
		Created(child, entry, depth);
		return child;
	}

	static int SlotId(VyshkaJsonValue entry)
	{
		string slot = entry.GetString("slot", "");
		if (slot == "")
			return InventorySlots.INVALID;
		return InventorySlots.GetSlotIdFromString(slot);
	}

	// Created applies an entry's own state to the item made for it, then
	// fills it.
	void Created(EntityAI item, VyshkaJsonValue entry, int depth)
	{
		m_Created++;
		State(item, entry);
		Fill(item, entry, depth);
	}

	// State sets what an entry says about the item itself: its liquid, then
	// its quantity (a magazine's rounds, a stack's count, a bottle's fill),
	// then its health as a percent of its maximum. A value the item refuses
	// is an account entry and the item stays as the engine made it.
	void State(EntityAI item, VyshkaJsonValue entry)
	{
		string error;
		string liquid = entry.GetString("liquid", "");
		if (liquid != "")
		{
			ItemBase container = ItemBase.Cast(item);
			LiquidInfo info = Liquid.m_LiquidInfosByName.Get(liquid);
			if (!container || !container.IsLiquidContainer())
				Problem(item.GetType(), "holds no liquid, so " + liquid + " was not put in it");
			else if (!info)
				Problem(item.GetType(), "no liquid named " + liquid + " on this server");
			else
				container.SetLiquidType(info.m_LiquidType);
		}
		float quantity;
		if (VyshkaSpawn.ReadNumber(entry, "quantity", quantity))
		{
			if (!VyshkaSpawn.SetQuantity(item, quantity, error))
				Problem(item.GetType(), error);
		}
		float health;
		if (VyshkaSpawn.ReadNumber(entry, "health", health))
		{
			if (health < 0 || health > VyshkaSpawn.HEALTH_MAX)
				Problem(item.GetType(), "health is a percent of the item's maximum, within 0 and 100");
			else if (!VyshkaSpawn.SetHealth(item, health, error))
				Problem(item.GetType(), error);
		}
	}

	// Fill creates an entry's contents inside the item: a firearm's magazine
	// first, through the engine's spawn-with-magazine call so the weapon
	// knows it is loaded, then every other attachment, then the cargo.
	void Fill(EntityAI item, VyshkaJsonValue entry, int depth)
	{
		VyshkaJsonValue attachments = entry.Get("attachments");
		if (attachments && !attachments.IsNull() && !attachments.IsArray())
		{
			Problem(item.GetType(), "attachments is not an array");
			attachments = null;
		}
		VyshkaJsonValue cargo = entry.Get("cargo");
		if (cargo && !cargo.IsNull() && !cargo.IsArray())
		{
			Problem(item.GetType(), "cargo is not an array");
			cargo = null;
		}
		int magazineAt = -1;
		Weapon_Base weapon = Weapon_Base.Cast(item);
		if (weapon)
			magazineAt = Load(weapon, entry, attachments, depth);
		if (attachments && attachments.IsArray())
		{
			for (int i = 0; i < attachments.Count(); i++)
			{
				if (i != magazineAt)
					Child(item, attachments.At(i), false, depth + 1);
			}
		}
		if (cargo && cargo.IsArray())
		{
			for (int c = 0; c < cargo.Count(); c++)
				Child(item, cargo.At(c), true, depth + 1);
		}
	}

	// Load loads a firearm the way the entry describes, returning the index
	// of the attachment entry it used, -1 when none. A magazine among the
	// attachments is attached through the engine's spawn-with-magazine call
	// (chambered when the entry says `loaded`), then set to the entry's
	// rounds. A firearm fed by hand (an internal magazine, a single chamber)
	// with `loaded` is filled through the engine's spawn-with-ammo call,
	// full rather than to a count. A firearm whose state machine does not
	// run (spikes/dayz-spawn-attachments) takes its magazine as a plain
	// attachment in the loop instead.
	int Load(Weapon_Base weapon, VyshkaJsonValue entry, VyshkaJsonValue attachments, int depth)
	{
		bool loaded = entry.GetBool("loaded", false);
		if (!weapon.VyshkaFsmRunning())
			return -1;
		if (attachments && attachments.IsArray())
		{
			for (int i = 0; i < attachments.Count(); i++)
			{
				VyshkaJsonValue part = attachments.At(i);
				if (!part || !part.IsObject())
					continue;
				string partClass = part.GetString("class", "");
				int scope;
				if (VyshkaWorld.FindConfig(partClass, scope) != VyshkaCatalog.TREE_MAGAZINES)
					continue;
				string className;
				string refusal = Refusal(part, depth + 1, className);
				if (refusal != "")
				{
					Problem(className, refusal);
					return i;
				}
				int flags = WeaponWithAmmoFlags.NONE;
				if (loaded)
					flags = WeaponWithAmmoFlags.CHAMBER;
				Magazine magazine = weapon.SpawnAttachedMagazine(className, flags);
				if (!magazine)
				{
					Problem(className, weapon.GetType() + " did not take it as its magazine");
					return i;
				}
				Created(magazine, part, depth + 1);
				return i;
			}
		}
		if (!loaded)
			return -1;
		if (weapon.HasInternalMagazine(-1) || weapon.GetMagazineTypeCount(0) == 0)
		{
			string ammo = VyshkaSpawn.LoadWith(weapon);
			if (ammo == "" || !weapon.SpawnAmmo(ammo, WeaponWithAmmoFlags.CHAMBER))
				Problem(weapon.GetType(), "could not be loaded");
		}
		else
			Problem(weapon.GetType(), "a round in the chamber of a firearm with no magazine is not restored");
		return -1;
	}
}

// VyshkaPresetApply is one dispatch of an apply action from its store read
// to its completion: read the named record, and, while the dispatch is still
// pending, hand it to the kind's Apply and complete the dispatch with what
// that answers.
class VyshkaPresetApply : VyshkaStoreCallback
{
	string m_ActionId;
	string m_ReferenceKey;
	string m_Namespace;
	string m_Name;
	string m_Kind;                  // "loadout", "location", "vehicle preset": for the messages
	ref VyshkaJsonValue m_Params;
	int m_DeadlineMs;
	string m_RevisionText;          // the revision of the record read

	void Init(string actionId, string referenceKey, string namespace, string name, string kind, VyshkaJsonValue params)
	{
		m_ActionId = actionId;
		m_ReferenceKey = referenceKey;
		m_Namespace = namespace;
		m_Name = name;
		m_Kind = kind;
		m_Params = params;
		m_DeadlineMs = VyshkaPresets.DeadlineMs();
	}

	// Start sends the read and answers the dispatch's outcome for now:
	// pending, or a failure when there is no store to ask.
	VyshkaActionOutcome Start()
	{
		VyshkaStoreClient store = VyshkaPlugin.StoreClient();
		if (!store)
			return VyshkaActionOutcome.Failure("the plugin is not connected to a hub, so the " + m_Kind + " cannot be read");
		store.Get(m_Namespace, m_Name, this, m_DeadlineMs);
		return VyshkaActionOutcome.Pending();
	}

	// Late says whether the dispatch's own deadline has passed. The plugin
	// fails an expired dispatch on a tick of its own, so a callback can run
	// after the deadline while IsPending still answers true; nothing may be
	// changed in the game then.
	bool Late()
	{
		return VyshkaClock.MonotonicMs() >= m_DeadlineMs + VyshkaPresets.DEADLINE_MARGIN_MS;
	}

	override void OnStore(VyshkaStoreResult result)
	{
		if (!VyshkaPlugin.IsPending(m_ActionId) || Late())
		{
			// Failed while the read was out (its deadline passed, or the
			// plugin stopped): nothing is applied on its behalf now.
			VyshkaLog.Warn(m_Kind + " " + m_Name + " for dispatch " + m_ActionId + " arrived after the dispatch ended; nothing was applied");
			return;
		}
		if (!result.m_Ok)
		{
			VyshkaPlugin.Complete(m_ActionId, VyshkaActionOutcome.Failure("the " + m_Kind + " could not be read from the store: " + result.m_Error));
			return;
		}
		if (!result.m_Found || !result.m_Value)
		{
			VyshkaPlugin.Complete(m_ActionId, VyshkaActionOutcome.Failure("no " + m_Kind + " named " + m_Name + " in the store (" + m_Namespace + "/" + m_Name + ")"));
			return;
		}
		if (!result.m_Value.IsObject())
		{
			VyshkaPlugin.Complete(m_ActionId, VyshkaActionOutcome.Failure("the " + m_Kind + " " + m_Name + " is not a JSON object"));
			return;
		}
		m_RevisionText = result.m_RevisionText;
		VyshkaActionOutcome outcome = Apply(result.m_Value);
		// A kind with more to do after this frame answers pending and calls
		// Finish itself.
		if (outcome.m_Pending)
			return;
		Finish(outcome);
	}

	// Finish names the record in a successful result and completes the
	// dispatch.
	void Finish(VyshkaActionOutcome outcome)
	{
		if (outcome.m_Ok && outcome.m_Result)
		{
			outcome.m_Result.Set(m_Kind, VyshkaJsonValue.NewString(m_Name));
			VyshkaJsonValue revision = VyshkaJson.Parse(m_RevisionText);
			if (revision && revision.IsNumber())
				outcome.m_Result.Set("revision", revision);
		}
		VyshkaPlugin.Complete(m_ActionId, outcome);
	}

	VyshkaActionOutcome Apply(VyshkaJsonValue record)
	{
		return VyshkaActionOutcome.Failure("this preset kind cannot be applied");
	}
}

class VyshkaLoadoutApply : VyshkaPresetApply
{
	// How often, and how many times, a dress after a drop looks whether the
	// drops have left the character: the engine's drop is an inventory move
	// the character finishes on a later frame, and until it does the slots
	// are still held (measured on DayZ 1.29 while building this slice: a
	// dress in the frame of the drop found no room for anything).
	static const int DROP_WAIT_MS = 250;
	static const int DROP_WAIT_TRIES = 20;

	// The applies waiting on a drop, held here so the call queue's callback
	// has an instance to run on.
	static ref array<ref VyshkaLoadoutApply> s_Waiting;

	ref VyshkaJsonValue m_Items;
	ref VyshkaJsonValue m_Result;
	int m_Tries;

	override VyshkaActionOutcome Apply(VyshkaJsonValue record)
	{
		string error;
		PlayerBase player = VyshkaVitals.Player(m_ReferenceKey, error);
		if (!player)
			return VyshkaActionOutcome.Failure(error);
		m_Items = record.Get("items");
		if (!m_Items || !m_Items.IsArray())
			return VyshkaActionOutcome.Failure("the loadout " + m_Name + " has no items array");

		m_Result = VyshkaVitals.Result(player);
		string previous = VyshkaAction.ReadText(m_Params, "previous", 16);
		if (previous != VyshkaPresets.PREVIOUS_DROP)
			return Dress(player);
		VyshkaJsonValue dropped = VyshkaJsonValue.NewArray();
		VyshkaJsonValue skipped = VyshkaJsonValue.NewArray();
		int droppedItems = VyshkaInventory.Strip(player, dropped, skipped);
		m_Result.Set("dropped", VyshkaJsonValue.NewInt(droppedItems));
		if (skipped.Count() > 0)
			m_Result.Set("skipped", skipped);
		if (dropped.Count() == 0)
			return Dress(player);
		if (!s_Waiting)
			s_Waiting = new array<ref VyshkaLoadoutApply>;
		s_Waiting.Insert(this);
		GetGame().GetCallQueue(CALL_CATEGORY_SYSTEM).CallLater(AfterDrop, DROP_WAIT_MS, false);
		return VyshkaActionOutcome.Pending();
	}

	// AfterDrop dresses the character once what was dropped has left it, or
	// after the last look regardless (a skipped item stays where it was, and
	// the dress puts things where there is room).
	void AfterDrop()
	{
		m_Tries++;
		if (!VyshkaPlugin.IsPending(m_ActionId))
		{
			VyshkaLog.Warn("loadout " + m_Name + " for dispatch " + m_ActionId + " was waiting on a drop when the dispatch ended; nothing was created");
			Release();
			return;
		}
		// The dress happens inside the store's margin before the deadline,
		// or not at all.
		if (VyshkaClock.MonotonicMs() >= m_DeadlineMs)
		{
			Finish(VyshkaActionOutcome.Failure("the dropped items had not left the character when the dispatch's time ran out; the player was stripped and nothing was created"));
			Release();
			return;
		}
		string error;
		PlayerBase player = VyshkaVitals.Player(m_ReferenceKey, error);
		if (!player)
		{
			Finish(VyshkaActionOutcome.Failure(error));
			Release();
			return;
		}
		int skippedCount = 0;
		VyshkaJsonValue skipped = m_Result.Get("skipped");
		if (skipped)
			skippedCount = skipped.Count();
		if (VyshkaInventory.TopLevel(player).Count() > skippedCount && m_Tries < DROP_WAIT_TRIES)
		{
			GetGame().GetCallQueue(CALL_CATEGORY_SYSTEM).CallLater(AfterDrop, DROP_WAIT_MS, false);
			return;
		}
		VyshkaActionOutcome outcome = Dress(player);
		Finish(outcome);
		// Last: the list may hold the only reference to this apply.
		Release();
	}

	void Release()
	{
		if (s_Waiting)
			s_Waiting.RemoveItem(this);
	}

	// Dress creates the loadout's items on the character.
	VyshkaActionOutcome Dress(PlayerBase player)
	{
		VyshkaJsonValue items = m_Items;
		VyshkaJsonValue result = m_Result;
		// Worn items first, so the containers exist before anything is put
		// in the inventory at large; then the items with no slot, through
		// the inventory's own placement search; the hands last.
		VyshkaPresetBuilder builder = new VyshkaPresetBuilder();
		for (int pass = 0; pass < 3; pass++)
		{
			for (int i = 0; i < items.Count(); i++)
			{
				VyshkaJsonValue entry = items.At(i);
				string slot = "";
				if (entry && entry.IsObject())
					slot = entry.GetString("slot", "");
				bool hands = VyshkaPresets.SameText(slot, VyshkaInventory.SLOT_HANDS);
				int wanted = 1;
				if (hands)
					wanted = 2;
				else if (slot != "")
					wanted = 0;
				if (wanted == pass)
					Top(player, entry, slot, hands, builder);
			}
		}
		builder.Report(result);
		VyshkaLog.Info("loadout " + m_Name + " applied to " + VyshkaVitals.Describe(player) + ": " + builder.m_Created.ToString() + " items created, " + builder.m_ProblemCount.ToString() + " problems");
		return VyshkaActionOutcome.Success(result);
	}

	// Top creates one top-level entry on the player: in the hands, in the
	// worn slot it names, or wherever the inventory finds room, and, when
	// the slot it names is taken or refuses it, wherever there is room.
	void Top(PlayerBase player, VyshkaJsonValue entry, string slot, bool hands, VyshkaPresetBuilder builder)
	{
		string className;
		string refusal = builder.Refusal(entry, 1, className);
		if (refusal != "")
		{
			builder.Problem(className, refusal);
			return;
		}
		EntityAI item = null;
		if (hands)
		{
			if (!player.GetHumanInventory().GetEntityInHands())
				item = player.GetHumanInventory().CreateInHands(className);
		}
		else if (slot != "")
		{
			int slotId = InventorySlots.GetSlotIdFromString(slot);
			if (slotId != InventorySlots.INVALID)
				item = player.GetInventory().CreateAttachmentEx(className, slotId);
		}
		if (!item)
			item = player.GetInventory().CreateInInventory(className);
		if (!item)
		{
			builder.Problem(className, "no room for it on the player");
			return;
		}
		builder.Created(item, entry, 1);
	}
}

class VyshkaLocationTeleport : VyshkaPresetApply
{
	override VyshkaActionOutcome Apply(VyshkaJsonValue record)
	{
		string error;
		PlayerBase player = VyshkaVitals.Player(m_ReferenceKey, error);
		if (!player)
			return VyshkaActionOutcome.Failure(error);
		vector center;
		if (!VyshkaWorld.ReadPosition(record.Get("position"), center, error))
			return VyshkaActionOutcome.Failure("the location " + m_Name + ": " + error);
		float radius = 0;
		VyshkaJsonValue radiusValue = record.Get("radius");
		if (radiusValue && !radiusValue.IsNull())
		{
			if (!radiusValue.IsNumber() || radiusValue.m_Number < 0 || radiusValue.m_Number > VyshkaPresets.RADIUS_MAX)
			{
				int radiusMax = VyshkaPresets.RADIUS_MAX;
				return VyshkaActionOutcome.Failure("the location " + m_Name + ": radius must be a number of metres within 0 and " + radiusMax.ToString());
			}
			radius = radiusValue.m_Number;
		}

		// A point drawn evenly over the disc of the radius, put on the
		// terrain (the location's own y is for the centre only: a floor
		// there says nothing about the ground around it). A point past the
		// map edge is drawn again, and after a few the centre is used.
		vector destination = center;
		bool scattered = false;
		for (int attempt = 0; radius > 0 && attempt < VyshkaPresets.SCATTER_TRIES && !scattered; attempt++)
		{
			float angle = Math.RandomFloat(0, Math.PI2);
			float distance = radius * Math.Sqrt(Math.RandomFloat01());
			vector point = center;
			point[0] = center[0] + Math.Cos(angle) * distance;
			point[2] = center[2] + Math.Sin(angle) * distance;
			point[1] = GetGame().SurfaceY(point[0], point[2]);
			string ignored;
			if (VyshkaWorld.CheckPosition(point, ignored))
			{
				destination = point;
				scattered = true;
			}
		}

		string plainId = player.GetIdentity().GetPlainId();
		Object root = VyshkaWorld.TeleportRoot(player);
		vector from = VyshkaWorld.Move(player, plainId, destination);
		VyshkaLog.Info("teleported " + VyshkaVitals.Describe(player) + " to location " + m_Name + " at " + destination.ToString() + " (from " + from.ToString() + ")");

		VyshkaJsonValue result = VyshkaVitals.Result(player);
		VyshkaJsonValue fromJson = VyshkaPlayers.Position(from);
		if (fromJson)
			result.Set("from", fromJson);
		VyshkaJsonValue toJson = VyshkaPlayers.Position(destination);
		if (toJson)
			result.Set("to", toJson);
		result.Set("radius", VyshkaVitals.Number(radius));
		if (radius > 0)
		{
			float offset = vector.Distance(Vector(center[0], 0, center[2]), Vector(destination[0], 0, destination[2]));
			result.Set("offset", VyshkaVitals.Number(Math.Round(offset * 10) / 10));
		}
		if (root != player)
			result.Set("vehicle", VyshkaJsonValue.NewString(root.GetType()));
		return VyshkaActionOutcome.Success(result);
	}
}

class VyshkaVehicleSpawn : VyshkaPresetApply
{
	override VyshkaActionOutcome Apply(VyshkaJsonValue record)
	{
		string error;
		VyshkaPresetBuilder builder = new VyshkaPresetBuilder();
		string className;
		string refusal = builder.Refusal(record, 1, className);
		if (refusal != "")
			return VyshkaActionOutcome.Failure("the vehicle preset " + m_Name + " names " + className + ": " + refusal);
		if (!GetGame().IsKindOf(className, "CarScript"))
			return VyshkaActionOutcome.Failure("the vehicle preset " + m_Name + " names " + className + ", which is not a car");
		VyshkaJsonValue fluids = record.Get("fluids");
		if (fluids && !fluids.IsNull() && !fluids.IsObject())
			return VyshkaActionOutcome.Failure("the vehicle preset " + m_Name + ": fluids must be an object of fractions, such as {\"fuel\": 1}");

		// Where: a position the dispatch names, or in front of a player,
		// facing the way the player faces.
		vector position;
		vector direction = "0 0 1";
		PlayerBase near = null;
		VyshkaJsonValue positionParam = null;
		if (m_Params && m_Params.IsObject())
			positionParam = m_Params.Get("position");
		if (positionParam && !positionParam.IsNull())
		{
			if (!VyshkaWorld.ReadPosition(positionParam, position, error))
				return VyshkaActionOutcome.Failure(error);
		}
		else
		{
			string nearPlayer = VyshkaAction.ReadText(m_Params, "nearPlayer", 64);
			near = VyshkaHealAction.FindPlayer(nearPlayer);
			if (!near)
				return VyshkaActionOutcome.Failure("player " + nearPlayer + " is not online");
			direction = near.GetDirection();
			position = near.GetPosition() + direction * VyshkaPresets.VEHICLE_DISTANCE;
			position[1] = GetGame().SurfaceY(position[0], position[2]);
			if (!VyshkaWorld.CheckPosition(position, error))
				return VyshkaActionOutcome.Failure("there is no room for a car in front of the player: " + error);
		}

		Object created = GetGame().CreateObjectEx(className, position, ECE_PLACE_ON_SURFACE);
		CarScript car = CarScript.Cast(created);
		if (!car)
		{
			if (created)
				GetGame().ObjectDelete(created);
			return VyshkaActionOutcome.Failure("the engine refused to create " + className);
		}
		car.SetDirection(direction);
		builder.Created(car, record, 1);

		// autoParts fills every slot the record left empty the way the spawn
		// action's `auto` does: the first compatible part the engine takes.
		VyshkaJsonValue result = VyshkaJsonValue.NewObject();
		if (record.GetBool("autoParts", false) && builder.m_Created >= VyshkaPresets.ITEMS_MAX)
			builder.Problem(car.GetType(), "autoParts was skipped: the preset's own items used the " + VyshkaPresets.ITEMS_MAX.ToString() + " items one preset may create");
		else if (record.GetBool("autoParts", false))
		{
			// Equip lists every part on the car, the preset's own included,
			// so what it made is counted as the tree's growth. It fills at
			// most two levels of the car's own slots, a bounded set for any
			// class, which is why it is let finish once the budget has room.
			VyshkaJsonValue attached = VyshkaJsonValue.NewArray();
			VyshkaJsonValue empty = VyshkaJsonValue.NewArray();
			int before = VyshkaInventory.CountTree(car);
			VyshkaSpawn.Equip(car, 1, attached, empty);
			int autoCount = VyshkaInventory.CountTree(car) - before;
			builder.m_Created += autoCount;
			result.Set("autoParts", VyshkaJsonValue.NewInt(autoCount));
			// The slots no part was found for, as the spawn action's `auto`
			// reports them ({ slot, on }): a part's own optional slot (a
			// battery's wire) is one, and is no fault of the preset.
			VyshkaJsonValue gaps = VyshkaJsonValue.NewArray();
			for (int e = 0; e < empty.Count() && e < VyshkaPresets.PROBLEMS_MAX; e++)
				gaps.Add(empty.At(e));
			result.Set("empty", gaps);
		}
		if (fluids && fluids.IsObject())
			Fluids(car, fluids, builder);

		result.Set("className", VyshkaJsonValue.NewString(car.GetType()));
		VyshkaJsonValue where = VyshkaPlayers.Position(car.GetPosition());
		if (where)
			result.Set("position", where);
		if (near)
			result.Set("nearPlayer", VyshkaPlayers.Identity(near.GetIdentity().GetPlainId()));
		VyshkaJsonValue levels = VyshkaJsonValue.NewObject();
		levels.Set("fuel", VyshkaVitals.Number(Math.Round(car.GetFluidFraction(CarFluid.FUEL) * 100) / 100));
		levels.Set("oil", VyshkaVitals.Number(Math.Round(car.GetFluidFraction(CarFluid.OIL) * 100) / 100));
		levels.Set("brake", VyshkaVitals.Number(Math.Round(car.GetFluidFraction(CarFluid.BRAKE) * 100) / 100));
		levels.Set("coolant", VyshkaVitals.Number(Math.Round(car.GetFluidFraction(CarFluid.COOLANT) * 100) / 100));
		result.Set("fluids", levels);
		result.Set("wheels", VyshkaJsonValue.NewInt(car.WheelCountPresent()));
		builder.Report(result);
		VyshkaLog.Info("vehicle preset " + m_Name + " spawned " + car.GetType() + " at " + car.GetPosition().ToString() + ": " + builder.m_Created.ToString() + " items, " + builder.m_ProblemCount.ToString() + " problems");
		return VyshkaActionOutcome.Success(result);
	}

	// Fluids fills each named fluid to its fraction of the car's capacity,
	// emptying it first so the level is the one asked for.
	void Fluids(CarScript car, VyshkaJsonValue fluids, VyshkaPresetBuilder builder)
	{
		for (int i = 0; i < fluids.Count(); i++)
		{
			string name = fluids.KeyAt(i);
			VyshkaJsonValue value = fluids.ValueAt(i);
			CarFluid fluid;
			if (name == "fuel")
				fluid = CarFluid.FUEL;
			else if (name == "oil")
				fluid = CarFluid.OIL;
			else if (name == "brake")
				fluid = CarFluid.BRAKE;
			else if (name == "coolant")
				fluid = CarFluid.COOLANT;
			else
			{
				builder.Problem(car.GetType(), "no fluid named " + name + " (fuel, oil, brake, coolant)");
				continue;
			}
			if (!value || !value.IsNumber() || value.m_Number < 0 || value.m_Number > 1)
			{
				builder.Problem(car.GetType(), name + " must be a fraction of the tank within 0 and 1");
				continue;
			}
			car.LeakAll(fluid);
			float amount = car.GetFluidCapacity(fluid) * value.m_Number;
			if (amount > 0)
				car.Fill(fluid, amount);
		}
	}
}

// VyshkaLoadoutCapture writes a captured loadout to the store and completes
// the dispatch with the answer.
class VyshkaLoadoutCapture : VyshkaStoreCallback
{
	string m_ActionId;
	string m_Name;
	int m_Items;
	int m_Bytes;

	override void OnStore(VyshkaStoreResult result)
	{
		if (!result.m_Ok)
		{
			VyshkaPlugin.Complete(m_ActionId, VyshkaActionOutcome.Failure("the loadout could not be stored: " + result.m_Error));
			return;
		}
		if (result.m_Mismatch)
		{
			VyshkaPlugin.Complete(m_ActionId, VyshkaActionOutcome.Failure("a loadout named " + m_Name + " exists already; dispatch again with overwrite to replace it"));
			return;
		}
		VyshkaJsonValue answer = VyshkaJsonValue.NewObject();
		answer.Set("loadout", VyshkaJsonValue.NewString(m_Name));
		answer.Set("items", VyshkaJsonValue.NewInt(m_Items));
		answer.Set("bytes", VyshkaJsonValue.NewInt(m_Bytes));
		VyshkaJsonValue revision = VyshkaJson.Parse(result.m_RevisionText);
		if (revision && revision.IsNumber())
			answer.Set("revision", revision);
		VyshkaPlugin.Complete(m_ActionId, VyshkaActionOutcome.Success(answer));
	}
}

// VyshkaLoadoutReader turns what a player carries into loadout entries.
class VyshkaLoadoutReader
{
	int m_Items;
	string m_Error;

	// Entry describes one item and, recursively, what it holds; null with
	// m_Error set when the tree passes a bound.
	VyshkaJsonValue Entry(EntityAI item, string slot, int depth)
	{
		if (depth > VyshkaPresets.DEPTH_MAX)
		{
			m_Error = "the gear nests deeper than " + VyshkaPresets.DEPTH_MAX.ToString() + " levels (" + item.GetType() + ")";
			return null;
		}
		m_Items++;
		if (m_Items > VyshkaPresets.ITEMS_MAX)
		{
			m_Error = "the gear holds more than the " + VyshkaPresets.ITEMS_MAX.ToString() + " items a loadout may create";
			return null;
		}
		VyshkaJsonValue entry = VyshkaJsonValue.NewObject();
		entry.Set("class", VyshkaJsonValue.NewString(item.GetType()));
		if (slot != "")
			entry.Set("slot", VyshkaJsonValue.NewString(slot));
		if (VyshkaInventory.HasHealth(item))
		{
			float max = item.GetMaxHealth(VyshkaInventory.ZONE_GLOBAL, VyshkaInventory.HEALTH_TYPE);
			if (max > 0)
			{
				int percent = (int)Math.Round(100.0 * item.GetHealth(VyshkaInventory.ZONE_GLOBAL, VyshkaInventory.HEALTH_TYPE) / max);
				if (percent < 100)
					entry.Set("health", VyshkaJsonValue.NewInt(percent));
			}
		}
		Magazine magazine = Magazine.Cast(item);
		ItemBase asItem = ItemBase.Cast(item);
		if (magazine)
			entry.Set("quantity", VyshkaJsonValue.NewInt(magazine.GetAmmoCount()));
		else if (item.HasQuantity())
			entry.Set("quantity", VyshkaVitals.Number(item.GetQuantity()));
		if (asItem && asItem.IsLiquidContainer() && asItem.GetLiquidType() != LIQUID_NONE && asItem.GetQuantity() > 0)
		{
			string liquid = Liquid.GetLiquidClassname(asItem.GetLiquidType());
			if (liquid != "")
				entry.Set("liquid", VyshkaJsonValue.NewString(liquid));
		}
		Weapon_Base weapon = Weapon_Base.Cast(item);
		if (weapon)
		{
			bool loaded = false;
			for (int m = 0; m < weapon.GetMuzzleCount(); m++)
			{
				if (weapon.IsChamberFull(m) || (weapon.HasInternalMagazine(m) && weapon.GetInternalMagazineCartridgeCount(m) > 0))
					loaded = true;
			}
			if (loaded)
				entry.Set("loaded", VyshkaJsonValue.NewBool(true));
		}

		GameInventory inventory = item.GetInventory();
		if (!inventory)
			return entry;
		VyshkaJsonValue attachments = VyshkaJsonValue.NewArray();
		for (int i = 0; i < inventory.AttachmentCount(); i++)
		{
			EntityAI attached = inventory.GetAttachmentFromIndex(i);
			if (!attached)
				continue;
			VyshkaJsonValue child = Entry(attached, VyshkaInventory.SlotOf(attached), depth + 1);
			if (!child)
				return null;
			attachments.Add(child);
		}
		if (attachments.Count() > 0)
			entry.Set("attachments", attachments);
		CargoBase cargo = inventory.GetCargo();
		if (cargo)
		{
			VyshkaJsonValue carried = VyshkaJsonValue.NewArray();
			for (int c = 0; c < cargo.GetItemCount(); c++)
			{
				EntityAI inCargo = cargo.GetItem(c);
				if (!inCargo)
					continue;
				VyshkaJsonValue held = Entry(inCargo, "", depth + 1);
				if (!held)
					return null;
				carried.Add(held);
			}
			if (carried.Count() > 0)
				entry.Set("cargo", carried);
		}
		return entry;
	}
}

class VyshkaLoadoutApplyAction : VyshkaAction
{
	override string Code()    { return "vyshka.loadout.apply"; }
	override string Name()    { return "Apply loadout"; }
	override string Context() { return "player"; }
	override string Danger()  { return "warning"; }

	override VyshkaJsonValue ParamsSchema()
	{
		VyshkaJsonValue previous = VyshkaJsonValue.NewObject();
		previous.Set("type", VyshkaJsonValue.NewString("string"));
		VyshkaJsonValue modes = VyshkaJsonValue.NewArray();
		modes.Add(VyshkaJsonValue.NewString(VyshkaPresets.PREVIOUS_KEEP));
		modes.Add(VyshkaJsonValue.NewString(VyshkaPresets.PREVIOUS_DROP));
		previous.Set("enum", modes);
		previous.Set("default", VyshkaJsonValue.NewString(VyshkaPresets.PREVIOUS_KEEP));

		VyshkaJsonValue properties = VyshkaJsonValue.NewObject();
		properties.Set("loadout", VyshkaPresets.NameSchema(VyshkaPresets.NS_LOADOUTS));
		properties.Set("previous", previous);
		VyshkaJsonValue required = VyshkaJsonValue.NewArray();
		required.Add(VyshkaJsonValue.NewString("loadout"));
		VyshkaJsonValue schema = VyshkaJsonValue.NewObject();
		schema.Set("type", VyshkaJsonValue.NewString("object"));
		schema.Set("required", required);
		schema.Set("properties", properties);
		return schema;
	}

	override VyshkaActionOutcome Execute(string actionId, string context, string referenceKey, VyshkaJsonValue params)
	{
		string error;
		string name = VyshkaPresets.ReadName(params, "loadout", error);
		if (name == "")
			return VyshkaActionOutcome.Failure(error);
		string previous = VyshkaAction.ReadText(params, "previous", 16);
		if (previous != "" && previous != VyshkaPresets.PREVIOUS_KEEP && previous != VyshkaPresets.PREVIOUS_DROP)
			return VyshkaActionOutcome.Failure("previous must be keep or drop");
		if (!VyshkaVitals.Player(referenceKey, error))
			return VyshkaActionOutcome.Failure(error);
		VyshkaLoadoutApply apply = new VyshkaLoadoutApply();
		apply.Init(actionId, referenceKey, VyshkaPresets.NS_LOADOUTS, name, "loadout", params);
		return apply.Start();
	}
}

class VyshkaLoadoutCaptureAction : VyshkaAction
{
	override string Code()    { return "vyshka.loadout.capture"; }
	override string Name()    { return "Capture loadout"; }
	override string Context() { return "player"; }
	override string Danger()  { return "warning"; }

	override VyshkaJsonValue ParamsSchema()
	{
		VyshkaJsonValue overwrite = VyshkaJsonValue.NewObject();
		overwrite.Set("type", VyshkaJsonValue.NewString("boolean"));
		overwrite.Set("default", VyshkaJsonValue.NewBool(false));

		VyshkaJsonValue properties = VyshkaJsonValue.NewObject();
		properties.Set("loadout", VyshkaPresets.NameSchema(VyshkaPresets.NS_LOADOUTS));
		properties.Set("overwrite", overwrite);
		VyshkaJsonValue required = VyshkaJsonValue.NewArray();
		required.Add(VyshkaJsonValue.NewString("loadout"));
		VyshkaJsonValue schema = VyshkaJsonValue.NewObject();
		schema.Set("type", VyshkaJsonValue.NewString("object"));
		schema.Set("required", required);
		schema.Set("properties", properties);
		return schema;
	}

	override VyshkaActionOutcome Execute(string actionId, string context, string referenceKey, VyshkaJsonValue params)
	{
		string error;
		string name = VyshkaPresets.ReadName(params, "loadout", error);
		if (name == "")
			return VyshkaActionOutcome.Failure(error);
		bool overwrite = false;
		if (params && params.IsObject())
			overwrite = params.GetBool("overwrite", false);
		PlayerBase player = VyshkaVitals.Player(referenceKey, error);
		if (!player)
			return VyshkaActionOutcome.Failure(error);
		VyshkaStoreClient store = VyshkaPlugin.StoreClient();
		if (!store)
			return VyshkaActionOutcome.Failure("the plugin is not connected to a hub, so the loadout cannot be stored");

		// What the player carries directly: the worn items in slot order,
		// then the item in hands.
		VyshkaLoadoutReader reader = new VyshkaLoadoutReader();
		VyshkaJsonValue items = VyshkaJsonValue.NewArray();
		array<EntityAI> worn = VyshkaInventory.Worn(player);
		for (int i = 0; i < worn.Count(); i++)
		{
			EntityAI wornItem = worn.Get(i);
			VyshkaJsonValue entry = reader.Entry(wornItem, VyshkaInventory.SlotOf(wornItem), 1);
			if (!entry)
				return VyshkaActionOutcome.Failure("the loadout was not captured: " + reader.m_Error);
			items.Add(entry);
		}
		EntityAI inHands = player.GetHumanInventory().GetEntityInHands();
		if (inHands)
		{
			VyshkaJsonValue held = reader.Entry(inHands, VyshkaInventory.SLOT_HANDS, 1);
			if (!held)
				return VyshkaActionOutcome.Failure("the loadout was not captured: " + reader.m_Error);
			items.Add(held);
		}
		if (items.Count() == 0)
			return VyshkaActionOutcome.Failure(VyshkaVitals.Describe(player) + " carries nothing to capture");

		VyshkaJsonValue record = VyshkaJsonValue.NewObject();
		record.Set("items", items);
		PlayerIdentity identity = player.GetIdentity();
		if (identity)
			record.Set("capturedFrom", VyshkaJsonValue.NewString(VyshkaAction.Bound(identity.GetName(), VyshkaRegistry.LABEL_MAX)));
		record.Set("capturedAt", VyshkaJsonValue.NewString(VyshkaClock.NowRfc3339()));
		record.Set("actionId", VyshkaJsonValue.NewString(actionId));
		// The store's bound is on the value's encoding; the serializer here
		// writes no whitespace, so its length is what the hub will measure
		// (plus nothing: the value is sent as it is serialized).
		int bytes = record.Serialize().Length();
		if (bytes > VyshkaPresets.VALUE_MAX)
			return VyshkaActionOutcome.Failure("the loadout of " + reader.m_Items.ToString() + " items is " + bytes.ToString() + " bytes, over the " + VyshkaPresets.VALUE_MAX.ToString() + " the store keeps in one value; carry less and capture again");

		VyshkaLoadoutCapture capture = new VyshkaLoadoutCapture();
		capture.m_ActionId = actionId;
		capture.m_Name = name;
		capture.m_Items = reader.m_Items;
		capture.m_Bytes = bytes;
		// "0" writes only when no loadout of that name exists (spec section
		// 12.2); "" replaces whatever is there.
		string ifRevision = "0";
		if (overwrite)
			ifRevision = "";
		store.Set(VyshkaPresets.NS_LOADOUTS, name, record, ifRevision, capture, VyshkaPresets.DeadlineMs());
		VyshkaLog.Info("capturing the gear of " + VyshkaVitals.Describe(player) + " as loadout " + name + ": " + reader.m_Items.ToString() + " items, " + bytes.ToString() + " bytes");
		return VyshkaActionOutcome.Pending();
	}
}

class VyshkaLocationTeleportAction : VyshkaAction
{
	override string Code()    { return "vyshka.location.teleport"; }
	override string Name()    { return "Teleport to location"; }
	override string Context() { return "player"; }
	override string Danger()  { return "warning"; }

	override VyshkaJsonValue ParamsSchema()
	{
		VyshkaJsonValue properties = VyshkaJsonValue.NewObject();
		properties.Set("location", VyshkaPresets.NameSchema(VyshkaPresets.NS_LOCATIONS));
		VyshkaJsonValue required = VyshkaJsonValue.NewArray();
		required.Add(VyshkaJsonValue.NewString("location"));
		VyshkaJsonValue schema = VyshkaJsonValue.NewObject();
		schema.Set("type", VyshkaJsonValue.NewString("object"));
		schema.Set("required", required);
		schema.Set("properties", properties);
		return schema;
	}

	override VyshkaActionOutcome Execute(string actionId, string context, string referenceKey, VyshkaJsonValue params)
	{
		string error;
		string name = VyshkaPresets.ReadName(params, "location", error);
		if (name == "")
			return VyshkaActionOutcome.Failure(error);
		if (!VyshkaVitals.Player(referenceKey, error))
			return VyshkaActionOutcome.Failure(error);
		VyshkaLocationTeleport apply = new VyshkaLocationTeleport();
		apply.Init(actionId, referenceKey, VyshkaPresets.NS_LOCATIONS, name, "location", params);
		return apply.Start();
	}
}

class VyshkaVehicleSpawnAction : VyshkaAction
{
	override string Code()    { return "vyshka.vehicle.spawn"; }
	override string Name()    { return "Spawn vehicle preset"; }
	override string Context() { return "world"; }
	override string Danger()  { return "warning"; }

	override VyshkaJsonValue ParamsSchema()
	{
		VyshkaJsonValue number = VyshkaJsonValue.NewObject();
		number.Set("type", VyshkaJsonValue.NewString("number"));
		VyshkaJsonValue position = VyshkaJsonValue.NewObject();
		position.Set("type", VyshkaJsonValue.NewString("array"));
		position.Set("items", number);
		position.Set("x-vyshka-widget", VyshkaJsonValue.NewString("vector"));

		VyshkaJsonValue nearPlayer = VyshkaJsonValue.NewObject();
		nearPlayer.Set("type", VyshkaJsonValue.NewString("string"));
		nearPlayer.Set("x-vyshka-widget", VyshkaJsonValue.NewString("player"));

		VyshkaJsonValue properties = VyshkaJsonValue.NewObject();
		properties.Set("vehicle", VyshkaPresets.NameSchema(VyshkaPresets.NS_VEHICLES));
		properties.Set("position", position);
		properties.Set("nearPlayer", nearPlayer);
		VyshkaJsonValue required = VyshkaJsonValue.NewArray();
		required.Add(VyshkaJsonValue.NewString("vehicle"));
		VyshkaJsonValue schema = VyshkaJsonValue.NewObject();
		schema.Set("type", VyshkaJsonValue.NewString("object"));
		schema.Set("required", required);
		schema.Set("properties", properties);
		return schema;
	}

	override VyshkaActionOutcome Execute(string actionId, string context, string referenceKey, VyshkaJsonValue params)
	{
		string error;
		string name = VyshkaPresets.ReadName(params, "vehicle", error);
		if (name == "")
			return VyshkaActionOutcome.Failure(error);
		// Exactly one place: the schema subset cannot say so, so the plugin
		// does, before it reads the store.
		VyshkaJsonValue position = null;
		if (params && params.IsObject())
			position = params.Get("position");
		if (position && position.IsNull())
			position = null;
		string nearPlayer = VyshkaAction.ReadText(params, "nearPlayer", 64);
		int places = 0;
		if (position)
			places++;
		if (nearPlayer != "")
			places++;
		if (places != 1)
			return VyshkaActionOutcome.Failure("give exactly one place: position, or nearPlayer");
		if (nearPlayer != "" && !VyshkaHealAction.FindPlayer(nearPlayer))
			return VyshkaActionOutcome.Failure("player " + nearPlayer + " is not online");
		VyshkaVehicleSpawn apply = new VyshkaVehicleSpawn();
		apply.Init(actionId, referenceKey, VyshkaPresets.NS_VEHICLES, name, "vehicle", params);
		return apply.Start();
	}
}
