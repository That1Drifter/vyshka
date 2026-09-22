// Vyshka DayZ plugin: the inventory actions (issue #74).
//
// vyshka.inventory.read answers with one player's inventory tree: what is
// in the hands, what is worn in each slot, and inside every container
// what is attached and what is in its cargo, down to the last round. It is
// a request with a result (spec section 7: "dump this player's inventory"
// is an action whose result is the answer), not a snapshot: a snapshot is
// a whole list the plugin pushes on its own cadence (section 8.3), and a
// tree per player at that cadence would cost bytes nobody asked for.
// Beside it, vyshka.inventory.strip drops everything the player wears and
// holds on the ground around them (a warning: nothing is lost, the player
// can pick it back up), and vyshka.inventory.clear deletes it all
// (destructive: the items are gone).
//
// The result of a read is bounded by the hub's 64 KiB result cap (section
// 7): over it, the hub keeps the outcome and drops the payload, which for
// a read is the whole answer. A fully loaded character is close to that
// bound (spikes/dayz-inventory-tree measured it), so the tree is
// serialized before it is answered and, when it does not fit, cut at the
// deepest level of containers until it does, with the count of what each
// cut container holds kept on it and `truncated` and `depth` saying what
// happened. The `slot` parameter then gives one worn container's subtree
// the whole budget to itself (a subtree alone over it is cut the same
// way).
//
// Everything here uses what the engine gives every server: the inventory
// hierarchy (attachments, cargo, hands), the item's own quantity, health
// level, magazine count, chamber state, liquid, and food stage, and for
// the two mutations the engine's own server-side drop (the same move a
// player's drop makes, synchronized to the client through a juncture) and
// safe delete (queued on the character and run on its next update, once
// no inventory action is in flight).

class VyshkaInventory
{
	// How many serialized bytes a read's result may take. The hub keeps a
	// result of at most 65536 bytes (spec section 7) and drops one over it
	// whole; the rest is headroom for the fields around the tree.
	static const int RESULT_BUDGET = 60000;

	// The pseudo-slot name of the item in hands, which the engine keeps as
	// a location of its own rather than an attachment slot.
	static const string SLOT_HANDS = "Hands";

	static const string ZONE_GLOBAL = "";
	static const string HEALTH_TYPE = "Health";

	static const int SLOT_NAME_MAX = 64;

	// HasHealth says whether GetMaxHealth can be asked: a class whose config
	// declares no DamageSystem (the launchers, the dart gun, and the shock
	// pistol; issue #75's spike) makes it log a script error instead. The
	// engine's own HasDamageSystem cannot say so: it reads false for every
	// item in the frame the item is created, those with health included.
	static bool HasHealth(EntityAI item)
	{
		return item.ConfigIsExisting("DamageSystem");
	}

	// State names the health level (GameConstants.STATE_*), the words the
	// client shows.
	static string StateName(int level)
	{
		if (level == GameConstants.STATE_PRISTINE)
			return "pristine";
		if (level == GameConstants.STATE_WORN)
			return "worn";
		if (level == GameConstants.STATE_DAMAGED)
			return "damaged";
		if (level == GameConstants.STATE_BADLY_DAMAGED)
			return "badlyDamaged";
		if (level == GameConstants.STATE_RUINED)
			return "ruined";
		return "unknown";
	}

	// SlotNames lists the attachment slots this character has, in the
	// engine's order: the worn slots (Head, Body, Back, ...), plus Hands
	// (which the engine declares among them on DayZ 1.29, and which is
	// added here in case a character does not).
	static array<string> SlotNames(PlayerBase player)
	{
		array<string> names = new array<string>;
		GameInventory inventory = player.GetInventory();
		int count = inventory.GetAttachmentSlotsCount();
		for (int i = 0; i < count; i++)
			names.Insert(InventorySlots.GetSlotName(inventory.GetAttachmentSlotId(i)));
		if (names.Find(SLOT_HANDS) < 0)
			names.Insert(SLOT_HANDS);
		return names;
	}

	static string SlotList(PlayerBase player)
	{
		array<string> names = SlotNames(player);
		string list = "";
		for (int i = 0; i < names.Count(); i++)
		{
			if (i > 0)
				list += ", ";
			list += names.Get(i);
		}
		return list;
	}

	// FindSlot resolves a slot name the way an operator types it (any
	// case) to the name the engine uses, or "" when the character has no
	// such slot.
	static string FindSlot(PlayerBase player, string wanted)
	{
		string lowered = wanted;
		lowered.ToLower();
		array<string> names = SlotNames(player);
		for (int i = 0; i < names.Count(); i++)
		{
			string name = names.Get(i);
			string candidate = name;
			candidate.ToLower();
			if (candidate == lowered)
				return name;
		}
		return "";
	}

	// SlotOf names the attachment slot an item sits in, "" for an item
	// that is not an attachment (in cargo, in hands, on the ground).
	static string SlotOf(EntityAI item)
	{
		InventoryLocation location = new InventoryLocation();
		if (!item.GetInventory() || !item.GetInventory().GetCurrentInventoryLocation(location))
			return "";
		if (location.GetType() != InventoryLocationType.ATTACHMENT)
			return "";
		return InventorySlots.GetSlotName(location.GetSlot());
	}

	// Worn lists the player's worn attachments in the character's slot
	// order (Headgear, Gloves, Shoulder, ..., the order the engine declares
	// the slots in), which is a fixed order for a character whatever was
	// put on first; the engine's own attachment list is in the order the
	// items were attached. An attachment in a slot the character does not
	// declare (none is known, but a mod could add one) follows at the end.
	static array<EntityAI> Worn(PlayerBase player)
	{
		array<EntityAI> items = new array<EntityAI>;
		GameInventory inventory = player.GetInventory();
		int slots = inventory.GetAttachmentSlotsCount();
		int i;
		for (i = 0; i < slots; i++)
		{
			EntityAI inSlot = inventory.FindAttachment(inventory.GetAttachmentSlotId(i));
			if (inSlot && items.Find(inSlot) < 0)
				items.Insert(inSlot);
		}
		int count = inventory.AttachmentCount();
		for (i = 0; i < count; i++)
		{
			EntityAI attached = inventory.GetAttachmentFromIndex(i);
			if (attached && items.Find(attached) < 0)
				items.Insert(attached);
		}
		return items;
	}

	// TopLevel lists what the player carries directly: the item in hands
	// first, then every worn attachment in slot order. A copy, so a caller
	// that drops or deletes as it walks is not walking a list the engine
	// is changing under it.
	static array<EntityAI> TopLevel(PlayerBase player)
	{
		array<EntityAI> items = new array<EntityAI>;
		EntityAI inHands = player.GetHumanInventory().GetEntityInHands();
		if (inHands)
			items.Insert(inHands);
		array<EntityAI> worn = Worn(player);
		for (int i = 0; i < worn.Count(); i++)
			items.Insert(worn.Get(i));
		return items;
	}

	// CountTree counts an item and everything inside it.
	static int CountTree(EntityAI item)
	{
		int count = 1;
		GameInventory inventory = item.GetInventory();
		if (!inventory)
			return count;
		int attachments = inventory.AttachmentCount();
		for (int i = 0; i < attachments; i++)
		{
			EntityAI attached = inventory.GetAttachmentFromIndex(i);
			if (attached)
				count += CountTree(attached);
		}
		CargoBase cargo = inventory.GetCargo();
		if (cargo)
		{
			int inCargo = cargo.GetItemCount();
			for (int c = 0; c < inCargo; c++)
			{
				EntityAI carried = cargo.GetItem(c);
				if (carried)
					count += CountTree(carried);
			}
		}
		return count;
	}

	// Player resolves the referenceKey of a player-context action to the
	// online player, or explains why it cannot.
	static PlayerBase Player(string referenceKey, out string error)
	{
		return VyshkaVitals.Player(referenceKey, error);
	}

	// Brief is the short description of one top-level item a strip or a
	// clear lists: its class, its display name, and where it was.
	static VyshkaJsonValue Brief(EntityAI item, string slot)
	{
		VyshkaJsonValue entry = VyshkaJsonValue.NewObject();
		entry.Set("class", VyshkaJsonValue.NewString(item.GetType()));
		entry.Set("name", VyshkaJsonValue.NewString(item.GetDisplayName()));
		entry.Set("slot", VyshkaJsonValue.NewString(slot));
		return entry;
	}

	// PlaceOf is the slot a top-level item is in, or Hands.
	static string PlaceOf(PlayerBase player, EntityAI item)
	{
		if (item == player.GetHumanInventory().GetEntityInHands())
			return SLOT_HANDS;
		string slot = SlotOf(item);
		if (slot == "")
			return "unknown";
		return slot;
	}
}

// VyshkaInventoryTree builds the JSON tree of one player's inventory down
// to a depth, counting as it goes. Depth 1 is what the player carries
// directly (hands and worn slots); each container's contents are one
// deeper. A container past the depth keeps its `items` count and loses its
// `attachments` and `cargo`.
class VyshkaInventoryTree
{
	int m_MaxDepth;   // 0 for no limit
	int m_Items;      // every item counted, included or not
	int m_Deepest;    // the deepest level any item sits at
	bool m_Cut;       // some container's contents were left out

	void VyshkaInventoryTree(int maxDepth)
	{
		m_MaxDepth = maxDepth;
	}

	// Root builds the result's `hands` and `worn` members for the player,
	// the worn list narrowed to one slot when slot is not "".
	void Root(PlayerBase player, string slot, VyshkaJsonValue result)
	{
		EntityAI inHands = player.GetHumanInventory().GetEntityInHands();
		if (inHands && (slot == "" || slot == VyshkaInventory.SLOT_HANDS))
			result.Set("hands", Entry(inHands, 1, VyshkaInventory.SLOT_HANDS));
		else
			result.Set("hands", VyshkaJsonValue.NewNull());
		VyshkaJsonValue worn = VyshkaJsonValue.NewArray();
		if (slot != VyshkaInventory.SLOT_HANDS)
		{
			array<EntityAI> items = VyshkaInventory.Worn(player);
			for (int i = 0; i < items.Count(); i++)
			{
				EntityAI attached = items.Get(i);
				string at = VyshkaInventory.SlotOf(attached);
				if (slot != "" && at != slot)
					continue;
				worn.Add(Entry(attached, 1, at));
			}
		}
		result.Set("worn", worn);
	}

	// Entry describes one item at a depth: what it is, its condition and
	// contents-related state, and, inside the depth, what it holds.
	VyshkaJsonValue Entry(EntityAI item, int depth, string slot)
	{
		m_Items++;
		if (depth > m_Deepest)
			m_Deepest = depth;
		VyshkaJsonValue entry = VyshkaJsonValue.NewObject();
		entry.Set("class", VyshkaJsonValue.NewString(item.GetType()));
		entry.Set("name", VyshkaJsonValue.NewString(item.GetDisplayName()));
		if (slot != "")
			entry.Set("slot", VyshkaJsonValue.NewString(slot));
		Condition(item, entry);

		// What it holds: attachments (a magazine on a rifle, a battery in
		// a light) and cargo (what is in a bag), each entry recursively,
		// inside the depth; past it only the count.
		int inside = 0;
		VyshkaJsonValue attachments = VyshkaJsonValue.NewArray();
		VyshkaJsonValue cargoList = VyshkaJsonValue.NewArray();
		bool include = m_MaxDepth <= 0 || depth < m_MaxDepth;
		GameInventory inventory = item.GetInventory();
		if (inventory)
		{
			int attachmentCount = inventory.AttachmentCount();
			for (int i = 0; i < attachmentCount; i++)
			{
				EntityAI attached = inventory.GetAttachmentFromIndex(i);
				if (!attached)
					continue;
				if (include)
					attachments.Add(Entry(attached, depth + 1, VyshkaInventory.SlotOf(attached)));
				else
					inside += Skip(attached, depth + 1);
			}
			CargoBase cargo = inventory.GetCargo();
			if (cargo)
			{
				int cargoCount = cargo.GetItemCount();
				for (int c = 0; c < cargoCount; c++)
				{
					EntityAI carried = cargo.GetItem(c);
					if (!carried)
						continue;
					if (include)
						cargoList.Add(Entry(carried, depth + 1, ""));
					else
						inside += Skip(carried, depth + 1);
				}
			}
		}
		if (include)
			inside = Held(attachments) + Held(cargoList);
		if (attachments.Count() > 0)
			entry.Set("attachments", attachments);
		if (cargoList.Count() > 0)
			entry.Set("cargo", cargoList);
		if (inside > 0)
			entry.Set("items", VyshkaJsonValue.NewInt(inside));
		return entry;
	}

	// Held counts what a list of child entries holds: one per child plus
	// each child's own `items`, which it carries when it holds anything.
	static int Held(VyshkaJsonValue list)
	{
		int total = 0;
		for (int i = 0; i < list.Count(); i++)
		{
			VyshkaJsonValue child = list.At(i);
			total += 1 + child.GetInt("items", 0);
		}
		return total;
	}

	// Skip counts a subtree that is past the depth without describing it,
	// still recording how deep it goes, so `depth` can be read against the
	// tree's real depth whatever was cut.
	int Skip(EntityAI item, int depth)
	{
		m_Cut = true;
		int count = Measure(item, depth);
		m_Items += count;
		return count;
	}

	// Measure counts an item and everything inside it, recording the
	// deepest level reached, without describing anything.
	int Measure(EntityAI item, int depth)
	{
		if (depth > m_Deepest)
			m_Deepest = depth;
		int count = 1;
		GameInventory inventory = item.GetInventory();
		if (!inventory)
			return count;
		int attachments = inventory.AttachmentCount();
		for (int i = 0; i < attachments; i++)
		{
			EntityAI attached = inventory.GetAttachmentFromIndex(i);
			if (attached)
				count += Measure(attached, depth + 1);
		}
		CargoBase cargo = inventory.GetCargo();
		if (cargo)
		{
			int inCargo = cargo.GetItemCount();
			for (int c = 0; c < inCargo; c++)
			{
				EntityAI carried = cargo.GetItem(c);
				if (carried)
					count += Measure(carried, depth + 1);
			}
		}
		return count;
	}

	// Condition adds what state the item is in: its health as a percent
	// and the level the client names it by; its quantity when it has one
	// (a stack, a bandage's uses, a bottle's fill); a magazine's or an
	// ammunition pile's rounds; a firearm's rounds in the chamber and any
	// internal magazine (an attached magazine is its own entry); the
	// liquid in a container that holds one; a food's stage.
	static void Condition(EntityAI item, VyshkaJsonValue entry)
	{
		float max = 0;
		if (VyshkaInventory.HasHealth(item))
			max = item.GetMaxHealth(VyshkaInventory.ZONE_GLOBAL, VyshkaInventory.HEALTH_TYPE);
		if (max > 0)
		{
			float health = item.GetHealth(VyshkaInventory.ZONE_GLOBAL, VyshkaInventory.HEALTH_TYPE);
			int percent = (int)Math.Round(100.0 * health / max);
			entry.Set("health", VyshkaJsonValue.NewInt(percent));
		}
		entry.Set("state", VyshkaJsonValue.NewString(VyshkaInventory.StateName(item.GetHealthLevel())));

		Magazine magazine = Magazine.Cast(item);
		if (magazine)
		{
			entry.Set("ammo", VyshkaJsonValue.NewInt(magazine.GetAmmoCount()));
			entry.Set("ammoMax", VyshkaJsonValue.NewInt(magazine.GetAmmoMax()));
		}
		else if (item.HasQuantity())
		{
			entry.Set("quantity", VyshkaVitals.Number(item.GetQuantity()));
			entry.Set("quantityMax", VyshkaJsonValue.NewInt(item.GetQuantityMax()));
		}

		Weapon_Base weapon = Weapon_Base.Cast(item);
		if (weapon)
		{
			int rounds = 0;
			int muzzles = weapon.GetMuzzleCount();
			for (int m = 0; m < muzzles; m++)
			{
				if (weapon.IsChamberFull(m))
					rounds++;
				if (weapon.HasInternalMagazine(m))
					rounds += weapon.GetInternalMagazineCartridgeCount(m);
			}
			entry.Set("rounds", VyshkaJsonValue.NewInt(rounds));
		}

		ItemBase asItem = ItemBase.Cast(item);
		if (asItem)
		{
			if (asItem.IsLiquidContainer() && asItem.GetLiquidType() != LIQUID_NONE && asItem.GetQuantity() > 0)
			{
				string liquid = Liquid.GetLiquidClassname(asItem.GetLiquidType());
				if (liquid != "")
					entry.Set("liquid", VyshkaJsonValue.NewString(liquid));
			}
			Edible_Base edible = Edible_Base.Cast(item);
			if (edible && edible.HasFoodStage())
			{
				string stage = FoodStage.GetFoodStageName(edible.GetFoodStageType());
				if (stage != "")
				{
					stage.ToLower();
					entry.Set("stage", VyshkaJsonValue.NewString(stage));
				}
			}
		}
	}
}

class VyshkaInventoryReadAction : VyshkaAction
{
	override string Code()    { return "vyshka.inventory.read"; }
	override string Name()    { return "Read inventory"; }
	override string Context() { return "player"; }
	override string Danger()  { return "none"; }

	override VyshkaJsonValue ParamsSchema()
	{
		VyshkaJsonValue slot = VyshkaJsonValue.NewObject();
		slot.Set("type", VyshkaJsonValue.NewString("string"));

		VyshkaJsonValue properties = VyshkaJsonValue.NewObject();
		properties.Set("slot", slot);

		VyshkaJsonValue schema = VyshkaJsonValue.NewObject();
		schema.Set("type", VyshkaJsonValue.NewString("object"));
		schema.Set("properties", properties);
		return schema;
	}

	override VyshkaActionOutcome Execute(string actionId, string context, string referenceKey, VyshkaJsonValue params)
	{
		string error;
		PlayerBase player = VyshkaInventory.Player(referenceKey, error);
		if (!player)
			return VyshkaActionOutcome.Failure(error);

		string wanted = VyshkaAction.ReadText(params, "slot", VyshkaInventory.SLOT_NAME_MAX);
		string slot = "";
		if (wanted != "")
		{
			slot = VyshkaInventory.FindSlot(player, wanted);
			if (slot == "")
				return VyshkaActionOutcome.Failure("this character has no slot named " + wanted + "; its slots are " + VyshkaInventory.SlotList(player));
		}

		// The whole tree first; when its bytes do not fit the hub's cap,
		// one level less each time until they do. Depth 1 (what is worn
		// and held, nothing inside) is a few dozen entries on any known
		// character; should a modded one put even that over the budget,
		// the action fails and says so rather than answer a payload the
		// hub would drop whole.
		int maxDepth = 0;
		VyshkaJsonValue result;
		VyshkaInventoryTree tree;
		int bytes;
		while (true)
		{
			tree = new VyshkaInventoryTree(maxDepth);
			result = VyshkaVitals.Result(player);
			result.Set("player", VyshkaJsonValue.NewString(referenceKey));
			result.Set("alive", VyshkaJsonValue.NewBool(player.IsAlive()));
			if (slot != "")
				result.Set("slot", VyshkaJsonValue.NewString(slot));
			tree.Root(player, slot, result);
			result.Set("items", VyshkaJsonValue.NewInt(tree.m_Items));
			bytes = result.Serialize().Length();
			if (bytes <= VyshkaInventory.RESULT_BUDGET || maxDepth == 1)
				break;
			if (maxDepth == 0)
				maxDepth = tree.m_Deepest;
			maxDepth--;
			if (maxDepth < 1)
				maxDepth = 1;
		}
		if (bytes > VyshkaInventory.RESULT_BUDGET)
		{
			int roots = result.Get("worn").Count();
			if (result.Get("hands").IsObject())
				roots++;
			return VyshkaActionOutcome.Failure("this character carries " + tree.m_Items.ToString() + " items; even the " + roots.ToString() + " it carries directly describe to " + bytes.ToString() + " bytes, over the " + VyshkaInventory.RESULT_BUDGET.ToString() + "-byte budget a result may take; read one slot at a time with the slot parameter");
		}
		int depth = maxDepth;
		if (depth == 0)
			depth = tree.m_Deepest;
		result.Set("depth", VyshkaJsonValue.NewInt(depth));
		result.Set("truncated", VyshkaJsonValue.NewBool(tree.m_Cut));
		VyshkaLog.Info("read the inventory of " + VyshkaVitals.Describe(player) + ": " + tree.m_Items.ToString() + " items, " + bytes.ToString() + " bytes at depth " + depth.ToString() + " of " + tree.m_Deepest.ToString() + ", truncated " + tree.m_Cut.ToString());
		return VyshkaActionOutcome.Success(result);
	}
}

class VyshkaInventoryStripAction : VyshkaAction
{
	override string Code()    { return "vyshka.inventory.strip"; }
	override string Name()    { return "Strip inventory"; }
	override string Context() { return "player"; }
	override string Danger()  { return "warning"; }

	override VyshkaActionOutcome Execute(string actionId, string context, string referenceKey, VyshkaJsonValue params)
	{
		string error;
		PlayerBase player = VyshkaInventory.Player(referenceKey, error);
		if (!player)
			return VyshkaActionOutcome.Failure(error);

		// Each top-level item goes to the ground through the engine's own
		// server-side drop, the move a player's drop makes, which finds a
		// free spot beside the character and synchronizes the move to the
		// client through a juncture; what is inside goes with it. The item
		// in hands takes the hand event path of the same drop. A drop the
		// engine refuses (an item under an inventory reservation, the
		// hands of a restrained character) is listed as skipped.
		array<EntityAI> items = VyshkaInventory.TopLevel(player);
		VyshkaJsonValue dropped = VyshkaJsonValue.NewArray();
		VyshkaJsonValue skipped = VyshkaJsonValue.NewArray();
		int total = 0;
		for (int i = 0; i < items.Count(); i++)
		{
			EntityAI item = items.Get(i);
			string place = VyshkaInventory.PlaceOf(player, item);
			int inside = VyshkaInventory.CountTree(item);
			VyshkaJsonValue brief = VyshkaInventory.Brief(item, place);
			if (inside > 1)
				brief.Set("items", VyshkaJsonValue.NewInt(inside - 1));
			if (!player.CanDropEntity(item))
			{
				brief.Set("reason", VyshkaJsonValue.NewString("the engine will not let this item be dropped now (an inventory move in flight, or a restrained character's hands)"));
				skipped.Add(brief);
				continue;
			}
			if (!player.ServerDropEntity(item))
			{
				brief.Set("reason", VyshkaJsonValue.NewString("the engine refused the drop"));
				skipped.Add(brief);
				continue;
			}
			dropped.Add(brief);
			total += inside;
		}
		VyshkaLog.Info("stripped " + VyshkaVitals.Describe(player) + ": " + dropped.Count().ToString() + " items dropped (" + total.ToString() + " with their contents), " + skipped.Count().ToString() + " skipped");

		VyshkaJsonValue result = VyshkaVitals.Result(player);
		result.Set("dropped", dropped);
		result.Set("droppedCount", VyshkaJsonValue.NewInt(dropped.Count()));
		result.Set("skipped", skipped);
		result.Set("items", VyshkaJsonValue.NewInt(total));
		return VyshkaActionOutcome.Success(result);
	}
}

class VyshkaInventoryClearAction : VyshkaAction
{
	override string Code()    { return "vyshka.inventory.clear"; }
	override string Name()    { return "Clear inventory"; }
	override string Context() { return "player"; }
	override string Danger()  { return "destructive"; }

	override VyshkaActionOutcome Execute(string actionId, string context, string referenceKey, VyshkaJsonValue params)
	{
		string error;
		PlayerBase player = VyshkaInventory.Player(referenceKey, error);
		if (!player)
			return VyshkaActionOutcome.Failure(error);

		// Each top-level item is deleted through the engine's safe delete:
		// for a living character it is queued on the character through a
		// juncture and removed on the character's next update once no
		// inventory action is in flight, which is how the engine removes
		// a consumed item; for a dead one it is deleted at once. What is
		// inside goes with it.
		array<EntityAI> items = VyshkaInventory.TopLevel(player);
		VyshkaJsonValue deleted = VyshkaJsonValue.NewArray();
		int total = 0;
		for (int i = 0; i < items.Count(); i++)
		{
			EntityAI item = items.Get(i);
			string place = VyshkaInventory.PlaceOf(player, item);
			int inside = VyshkaInventory.CountTree(item);
			VyshkaJsonValue brief = VyshkaInventory.Brief(item, place);
			if (inside > 1)
				brief.Set("items", VyshkaJsonValue.NewInt(inside - 1));
			item.DeleteSafe();
			deleted.Add(brief);
			total += inside;
		}
		VyshkaLog.Info("cleared the inventory of " + VyshkaVitals.Describe(player) + ": " + deleted.Count().ToString() + " items deleted (" + total.ToString() + " with their contents)");

		VyshkaJsonValue result = VyshkaVitals.Result(player);
		result.Set("deleted", deleted);
		result.Set("deletedCount", VyshkaJsonValue.NewInt(deleted.Count()));
		result.Set("items", VyshkaJsonValue.NewInt(total));
		return VyshkaActionOutcome.Success(result);
	}
}
