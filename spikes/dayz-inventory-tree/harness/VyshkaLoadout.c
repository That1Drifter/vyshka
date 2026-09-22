// Vyshka spike helper: a heavy stock DayZ 1.29 loadout built from named
// classes (issue #74), a list chosen for cargo rather than a search of
// every garment, so a heavy load and not a proven maximum. Shared by the spike's
// probe and the live run's gear rig, so the tree measured on a body with
// no client and the tree read live from a player are the same tree.
//
// The classes are chosen for cargo: the largest bag, vest, jacket, and
// trousers the stock game declares, a belt with its canteen, sheath, and
// holster filled, three rifles with every attachment their family takes
// and a full magazine each, a protector case and a first aid kit nested in
// every cargo that takes them, and then every cargo (the nested ones
// included) filled with one-slot items until the engine refuses another.
// A class the engine will not create in its slot is reported, not
// papered over. Each garment's real cargo is read from the instance
// (config declares it under Cargo itemsCargoSize, which the script config
// reader does not return), so the report says what the slots hold.

class VyshkaLoadout
{
	static const string FILLER = "Battery9V";   // one slot, not stackable
	static const int FILL_MAX = 400;            // per container, a guard against a cargo that never fills

	// Gear dresses the player: hands first (the engine puts a weapon
	// created for an empty-handed character into the hands whatever slot
	// was asked for), then every worn slot, then the belt's and the
	// rifles' attachments. Returns how many items the player carries.
	static int Gear(PlayerBase p, string tag)
	{
		HumanInventory hands = p.GetHumanInventory();
		if (!hands.GetEntityInHands())
			Rifle(p, "M4A1", "", tag);
		Garment(p, "Headgear", "BallisticHelmet_Green", tag);
		Garment(p, "Mask", "GasMask", tag);
		Garment(p, "Eyewear", "AviatorGlasses", tag);
		Garment(p, "Gloves", "TacticalGloves_Black", tag);
		Garment(p, "Armband", "Armband_White", tag);
		Garment(p, "Body", "HuntingJacket_Brown", tag);
		Garment(p, "Vest", "HighCapacityVest_Black", tag);
		Garment(p, "Back", "AliceBag_Green", tag);
		Garment(p, "Legs", "HunterPants_Brown", tag);
		Garment(p, "Feet", "MilitaryBoots_Black", tag);
		EntityAI belt = Garment(p, "Hips", "MilitaryBelt", tag);
		if (belt)
		{
			Part(belt, "Canteen", tag);
			EntityAI sheath = Part(belt, "NylonKnifeSheath", tag);
			if (sheath)
				Part(sheath, "CombatKnife", tag);
			EntityAI holster = Part(belt, "PlateCarrierHolster", tag);
			if (holster)
			{
				EntityAI pistol = Part(holster, "FNX45", tag);
				if (pistol)
				{
					EntityAI magazine = Part(pistol, "Mag_FNX45_15Rnd", tag);
					Fill(magazine);
				}
			}
		}
		Rifle(p, "M4A1", "Shoulder", tag);
		Rifle(p, "AKM", "Melee", tag);
		return Carried(p);
	}

	// Garment creates one class in a named slot through an explicit
	// inventory location and reports where it landed and what cargo it
	// has.
	static EntityAI Garment(PlayerBase p, string slotName, string className, string tag)
	{
		int slotId = InventorySlots.GetSlotIdFromString(slotName);
		if (slotId == InventorySlots.INVALID)
		{
			Print(tag + "\tslot=" + slotName + "\tclass=" + className + "\tresult=no such slot");
			return null;
		}
		if (p.GetInventory().FindAttachment(slotId))
		{
			Print(tag + "\tslot=" + slotName + "\tclass=" + className + "\tresult=slot already taken by " + p.GetInventory().FindAttachment(slotId).GetType());
			return null;
		}
		InventoryLocation location = new InventoryLocation();
		location.SetAttachment(p, null, slotId);
		EntityAI created = GameInventory.LocationCreateEntity(location, className, ECE_IN_INVENTORY, RF_DEFAULT);
		if (!created)
		{
			Print(tag + "\tslot=" + slotName + "\tclass=" + className + "\tresult=refused");
			return null;
		}
		Print(tag + "\tslot=" + slotName + "\tclass=" + className + "\tresult=" + VyshkaInventory.PlaceOf(p, created) + "\tcargo=" + CargoText(created));
		return created;
	}

	// Part creates an attachment on an item (a magazine on a rifle, a
	// canteen on a belt) and reports it.
	static EntityAI Part(EntityAI parent, string className, string tag)
	{
		EntityAI created = parent.GetInventory().CreateAttachment(className);
		if (!created)
		{
			Print(tag + "\tpart=" + className + "\ton=" + parent.GetType() + "\tresult=refused");
			return null;
		}
		return created;
	}

	// Rifle creates a rifle in a slot (in hands for "") with a full
	// magazine and the attachments its family takes.
	static EntityAI Rifle(PlayerBase p, string className, string slotName, string tag)
	{
		EntityAI rifle;
		if (slotName == "")
		{
			rifle = p.GetHumanInventory().CreateInHands(className);
			if (!rifle)
			{
				Print(tag + "\tslot=Hands\tclass=" + className + "\tresult=refused");
				return null;
			}
			Print(tag + "\tslot=Hands\tclass=" + className + "\tresult=" + VyshkaInventory.PlaceOf(p, rifle));
		}
		else
		{
			rifle = Garment(p, slotName, className, tag);
			if (!rifle)
				return null;
		}
		TStringArray parts = new TStringArray;
		if (className == "M4A1")
		{
			parts.Insert("Mag_STANAG_30Rnd");
			parts.Insert("ACOGOptic");
			parts.Insert("M4_Suppressor");
			parts.Insert("M4_RISHndgrd");
			parts.Insert("M4_MPBttstck");
		}
		else
		{
			parts.Insert("Mag_AKM_30Rnd");
			parts.Insert("KashtanOptic");
			parts.Insert("AK_Suppressor");
			parts.Insert("AK_WoodHndgrd");
			parts.Insert("AK_WoodBttstck");
		}
		for (int i = 0; i < parts.Count(); i++)
			Fill(Part(rifle, parts.Get(i), tag));
		return rifle;
	}

	// Fill fills a magazine.
	static void Fill(EntityAI item)
	{
		Magazine magazine = Magazine.Cast(item);
		if (magazine)
			magazine.ServerSetAmmoMax();
	}

	// Stuff nests a protector case and a first aid kit in every cargo the
	// player carries that takes them, then fills every cargo, the nested
	// ones included, with one-slot items until the engine refuses another.
	// Returns how many items the player carries afterwards.
	static int Stuff(PlayerBase p, string tag)
	{
		array<EntityAI> entities = new array<EntityAI>;
		p.GetInventory().EnumerateInventory(InventoryTraversalType.LEVELORDER, entities);
		int containers = 0;
		int nested = 0;
		int i;
		for (i = 0; i < entities.Count(); i++)
		{
			EntityAI entity = entities.Get(i);
			if (!entity || entity == p || !entity.GetInventory() || !entity.GetInventory().GetCargo())
				continue;
			containers++;
			if (entity.GetInventory().CreateEntityInCargo("ProtectorCase"))
				nested++;
			if (entity.GetInventory().CreateEntityInCargo("FirstAidKit"))
				nested++;
		}
		entities.Clear();
		p.GetInventory().EnumerateInventory(InventoryTraversalType.LEVELORDER, entities);
		int filled = 0;
		for (i = 0; i < entities.Count(); i++)
		{
			EntityAI container = entities.Get(i);
			if (!container || container == p || !container.GetInventory() || !container.GetInventory().GetCargo())
				continue;
			for (int n = 0; n < FILL_MAX; n++)
			{
				if (!container.GetInventory().CreateEntityInCargo(FILLER))
					break;
				filled++;
			}
		}
		Print(tag + "\tstuffed\tcontainers=" + containers.ToString() + "\tnested=" + nested.ToString() + "\tfilled=" + filled.ToString());
		return Carried(p);
	}

	static string CargoText(EntityAI item)
	{
		CargoBase cargo = item.GetInventory().GetCargo();
		if (!cargo)
			return "none";
		return cargo.GetWidth().ToString() + "x" + cargo.GetHeight().ToString();
	}

	// Carried counts everything the player carries, contents included.
	static int Carried(PlayerBase p)
	{
		array<EntityAI> items = VyshkaInventory.TopLevel(p);
		int count = 0;
		for (int i = 0; i < items.Count(); i++)
			count += VyshkaInventory.CountTree(items.Get(i));
		return count;
	}

	// Report lists what the player carries directly, one line each.
	static void Report(PlayerBase p, string tag)
	{
		array<EntityAI> items = VyshkaInventory.TopLevel(p);
		Print(tag + "\ttop=" + items.Count().ToString() + "\tcarried=" + Carried(p).ToString());
		for (int i = 0; i < items.Count() && i < 24; i++)
		{
			EntityAI item = items.Get(i);
			Print(tag + "\tcarried\tslot=" + VyshkaInventory.PlaceOf(p, item) + "\tclass=" + item.GetType() + "\titems=" + VyshkaInventory.CountTree(item).ToString() + "\tcargo=" + CargoText(item));
		}
	}
}
