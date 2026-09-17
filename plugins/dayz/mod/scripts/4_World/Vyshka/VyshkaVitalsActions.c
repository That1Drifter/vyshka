// Vyshka DayZ plugin: the vitals action and the small condition actions
// (issue #68).
//
// vyshka.vitals sets one stat of one player to one value: health, blood,
// shock, energy, water, stamina, or the heat buffer. vyshka.heal (the tracer
// bullet) stays as the "everything to full" shortcut; this is the
// generalization the roadmap promised. Beside it, four actions an admin
// reaches for after a bugged fall or a long swim: stop bleeding, dry,
// broken legs on or off, bloody hands on or off. Each is a player-context
// action taking the player's plain Steam64 id as its referenceKey (spec
// section 8.2) and needs the player online.
//
// Everything here uses what the engine gives every server: the damage
// system's health, blood, and shock on the global zone, the player stats
// for energy, water, and the heat buffer, the stamina handler, the bleeding
// source manager, an item's wetness, the broken-legs modifier (the engine's
// own path when a leg zone is ruined), and the bloody-hands flag.

class VyshkaVitals
{
	static const string STAT_HEALTH = "health";
	static const string STAT_BLOOD = "blood";
	static const string STAT_SHOCK = "shock";
	static const string STAT_ENERGY = "energy";
	static const string STAT_WATER = "water";
	static const string STAT_STAMINA = "stamina";
	static const string STAT_HEAT_BUFFER = "heatBuffer";

	static const string LEGS_NONE = "none";
	static const string LEGS_BROKEN = "broken";
	static const string LEGS_SPLINT = "splint";

	static const string ZONE_GLOBAL = "";
	static const string HEALTH_TYPE = "Health";
	static const string BLOOD_TYPE = "Blood";
	static const string SHOCK_TYPE = "Shock";

	// Stats lists every stat name the action accepts, in the order the
	// manifest declares them.
	static array<string> Stats()
	{
		array<string> stats = new array<string>;
		stats.Insert(STAT_HEALTH);
		stats.Insert(STAT_BLOOD);
		stats.Insert(STAT_SHOCK);
		stats.Insert(STAT_ENERGY);
		stats.Insert(STAT_WATER);
		stats.Insert(STAT_STAMINA);
		stats.Insert(STAT_HEAT_BUFFER);
		return stats;
	}

	static string StatList()
	{
		array<string> stats = Stats();
		string list = "";
		for (int i = 0; i < stats.Count(); i++)
		{
			if (i > 0)
				list += ", ";
			list += stats.Get(i);
		}
		return list;
	}

	// Stat reads the player's stat by name (its own PlayerStat for energy,
	// water, and the heat buffer; null for the three the damage system
	// holds and for stamina, which the stamina handler owns).
	static PlayerStat<float> Stat(PlayerBase player, string stat)
	{
		if (stat == STAT_ENERGY)
			return player.GetStatEnergy();
		if (stat == STAT_WATER)
			return player.GetStatWater();
		if (stat == STAT_HEAT_BUFFER)
			return player.GetStatHeatBuffer();
		return null;
	}

	// Range gives the values the engine will hold for a stat: the damage
	// system's maximum for health, blood, and shock (the minimum is 0, and 0
	// health or blood is death), the stat's own bounds for energy, water,
	// and the heat buffer (which runs negative), and the gameplay-config
	// maximum for stamina. False for a name that is not a stat.
	static bool Range(PlayerBase player, string stat, out float min, out float max)
	{
		min = 0;
		if (stat == STAT_HEALTH || stat == STAT_BLOOD || stat == STAT_SHOCK)
		{
			max = player.GetMaxHealth(ZONE_GLOBAL, HealthType(stat));
			return true;
		}
		if (stat == STAT_STAMINA)
		{
			StaminaHandler stamina = player.GetStaminaHandler();
			if (!stamina)
				return false;
			max = stamina.GetStaminaMax();
			return true;
		}
		PlayerStat<float> stored = Stat(player, stat);
		if (!stored)
			return false;
		min = stored.GetMin();
		max = stored.GetMax();
		return true;
	}

	static string HealthType(string stat)
	{
		if (stat == STAT_BLOOD)
			return BLOOD_TYPE;
		if (stat == STAT_SHOCK)
			return SHOCK_TYPE;
		return HEALTH_TYPE;
	}

	// Read reads the stat's current value the way the engine reports it.
	static float Read(PlayerBase player, string stat)
	{
		if (stat == STAT_HEALTH || stat == STAT_BLOOD || stat == STAT_SHOCK)
			return player.GetHealth(ZONE_GLOBAL, HealthType(stat));
		if (stat == STAT_STAMINA)
		{
			StaminaHandler stamina = player.GetStaminaHandler();
			if (!stamina)
				return 0;
			return stamina.GetStamina();
		}
		PlayerStat<float> stored = Stat(player, stat);
		if (!stored)
			return 0;
		return stored.Get();
	}

	// Write sets the stat. Health, blood, and shock go through the damage
	// system, which synchronizes them; energy and water are server-side
	// stats the client learns of through the engine's own notifiers; the
	// heat buffer is a synced stat whose HUD stage the engine's modifier
	// recomputes on its next tick; stamina goes through the handler, which
	// synchronizes it at once and brings a value above the player's
	// load-dependent cap down to that cap on the next stamina tick.
	static void Write(PlayerBase player, string stat, float value)
	{
		if (stat == STAT_HEALTH || stat == STAT_BLOOD || stat == STAT_SHOCK)
		{
			player.SetHealth(ZONE_GLOBAL, HealthType(stat), value);
			return;
		}
		if (stat == STAT_STAMINA)
		{
			StaminaHandler stamina = player.GetStaminaHandler();
			if (stamina)
				stamina.SetStamina(value);
			return;
		}
		PlayerStat<float> stored = Stat(player, stat);
		if (stored)
			stored.Set(value);
	}

	static string LegsState(PlayerBase player)
	{
		eBrokenLegs state = player.GetBrokenLegs();
		if (state == eBrokenLegs.BROKEN_LEGS)
			return LEGS_BROKEN;
		if (state == eBrokenLegs.BROKEN_LEGS_SPLINT)
			return LEGS_SPLINT;
		return LEGS_NONE;
	}

	// LegZones are the damage zones the engine ruins when it breaks a
	// player's legs, and whose health it watches to call them healed.
	static array<string> LegZones()
	{
		array<string> zones = new array<string>;
		zones.Insert("RightLeg");
		zones.Insert("LeftLeg");
		zones.Insert("RightFoot");
		zones.Insert("LeftFoot");
		return zones;
	}

	// Text renders a stat value for a message: whole when it is one, two
	// decimals otherwise.
	static string Text(float value)
	{
		return Number(value).m_Text;
	}

	// Number renders a stat value as a JSON number: whole when it is one,
	// two decimals otherwise, the same as the positions the snapshots carry.
	static VyshkaJsonValue Number(float value)
	{
		int whole = (int)value;
		float wholeAsFloat = whole;
		if (wholeAsFloat == value)
			return VyshkaJsonValue.NewInt(whole);
		VyshkaJsonValue number = VyshkaJsonValue.NewFloat(value);
		if (!number)
			return VyshkaJsonValue.NewInt(whole);
		return number;
	}

	// Player resolves the referenceKey of a player-context action to the
	// online player, or explains why it cannot.
	static PlayerBase Player(string referenceKey, out string error)
	{
		if (referenceKey == "")
		{
			error = "a player-context action needs the player's identity as referenceKey";
			return null;
		}
		PlayerBase player = VyshkaHealAction.FindPlayer(referenceKey);
		if (!player)
			error = "player " + referenceKey + " is not online";
		return player;
	}

	static VyshkaJsonValue Result(PlayerBase player)
	{
		VyshkaJsonValue result = VyshkaJsonValue.NewObject();
		PlayerIdentity identity = player.GetIdentity();
		if (identity)
			result.Set("name", VyshkaJsonValue.NewString(identity.GetName()));
		return result;
	}

	static string Describe(PlayerBase player)
	{
		PlayerIdentity identity = player.GetIdentity();
		if (!identity)
			return "an unidentified player";
		return identity.GetName() + " (" + identity.GetPlainId() + ")";
	}
}

class VyshkaVitalsAction : VyshkaAction
{
	override string Code()    { return "vyshka.vitals"; }
	override string Name()    { return "Set a vital stat"; }
	override string Context() { return "player"; }
	override string Danger()  { return "warning"; }

	override VyshkaJsonValue ParamsSchema()
	{
		VyshkaJsonValue names = VyshkaJsonValue.NewArray();
		array<string> stats = VyshkaVitals.Stats();
		for (int i = 0; i < stats.Count(); i++)
			names.Add(VyshkaJsonValue.NewString(stats.Get(i)));
		VyshkaJsonValue stat = VyshkaJsonValue.NewObject();
		stat.Set("type", VyshkaJsonValue.NewString("string"));
		stat.Set("enum", names);

		VyshkaJsonValue value = VyshkaJsonValue.NewObject();
		value.Set("type", VyshkaJsonValue.NewString("number"));

		VyshkaJsonValue properties = VyshkaJsonValue.NewObject();
		properties.Set("stat", stat);
		properties.Set("value", value);

		VyshkaJsonValue required = VyshkaJsonValue.NewArray();
		required.Add(VyshkaJsonValue.NewString("stat"));
		required.Add(VyshkaJsonValue.NewString("value"));

		VyshkaJsonValue schema = VyshkaJsonValue.NewObject();
		schema.Set("type", VyshkaJsonValue.NewString("object"));
		schema.Set("required", required);
		schema.Set("properties", properties);
		return schema;
	}

	override VyshkaActionOutcome Execute(string actionId, string context, string referenceKey, VyshkaJsonValue params)
	{
		// The params first, so a bad request is refused the same way
		// whether or not the player is online.
		string stat = VyshkaAction.ReadText(params, "stat", 32);
		if (stat == "")
			return VyshkaActionOutcome.Failure("stat is required: one of " + VyshkaVitals.StatList());
		if (VyshkaVitals.Stats().Find(stat) < 0)
			return VyshkaActionOutcome.Failure(stat + " is not a stat; use one of " + VyshkaVitals.StatList());
		VyshkaJsonValue valueParam = null;
		if (params && params.IsObject())
			valueParam = params.Get("value");
		if (!valueParam || !valueParam.IsNumber())
			return VyshkaActionOutcome.Failure("value is required and must be a number");
		float value = valueParam.m_Number;

		string error;
		PlayerBase player = VyshkaVitals.Player(referenceKey, error);
		if (!player)
			return VyshkaActionOutcome.Failure(error);

		// The range is the player's own: the stat objects and the stamina
		// handler exist per character, and a mod may change a maximum.
		float min;
		float max;
		if (!VyshkaVitals.Range(player, stat, min, max))
			return VyshkaActionOutcome.Failure("this character has no " + stat + " stat to set");
		if (value < min || value > max)
			return VyshkaActionOutcome.Failure(stat + " must lie within " + VyshkaVitals.Text(min) + " and " + VyshkaVitals.Text(max));

		float before = VyshkaVitals.Read(player, stat);
		VyshkaVitals.Write(player, stat, value);
		float after = VyshkaVitals.Read(player, stat);
		VyshkaLog.Info("set " + stat + " of " + VyshkaVitals.Describe(player) + " to " + VyshkaVitals.Text(value) + " (was " + VyshkaVitals.Text(before) + ", now " + VyshkaVitals.Text(after) + ")");

		VyshkaJsonValue result = VyshkaVitals.Result(player);
		result.Set("stat", VyshkaJsonValue.NewString(stat));
		result.Set("before", VyshkaVitals.Number(before));
		result.Set("after", VyshkaVitals.Number(after));
		result.Set("min", VyshkaVitals.Number(min));
		result.Set("max", VyshkaVitals.Number(max));
		return VyshkaActionOutcome.Success(result);
	}
}

class VyshkaStopBleedingAction : VyshkaAction
{
	override string Code()    { return "vyshka.stopbleeding"; }
	override string Name()    { return "Stop bleeding"; }
	override string Context() { return "player"; }
	override string Danger()  { return "none"; }

	override VyshkaActionOutcome Execute(string actionId, string context, string referenceKey, VyshkaJsonValue params)
	{
		string error;
		PlayerBase player = VyshkaVitals.Player(referenceKey, error);
		if (!player)
			return VyshkaActionOutcome.Failure(error);

		int sources = 0;
		BleedingSourcesManagerServer bleeding = player.GetBleedingManagerServer();
		if (bleeding)
		{
			sources = bleeding.GetBleedingSourcesCount();
			bleeding.RemoveAllSources();
		}
		VyshkaLog.Info("stopped bleeding of " + VyshkaVitals.Describe(player) + " (" + sources.ToString() + " sources)");

		VyshkaJsonValue result = VyshkaVitals.Result(player);
		result.Set("sourcesRemoved", VyshkaJsonValue.NewInt(sources));
		result.Set("bleeding", VyshkaJsonValue.NewBool(player.IsBleeding()));
		return VyshkaActionOutcome.Success(result);
	}
}

class VyshkaDryAction : VyshkaAction
{
	override string Code()    { return "vyshka.dry"; }
	override string Name()    { return "Dry player"; }
	override string Context() { return "player"; }
	override string Danger()  { return "none"; }

	override VyshkaActionOutcome Execute(string actionId, string context, string referenceKey, VyshkaJsonValue params)
	{
		string error;
		PlayerBase player = VyshkaVitals.Player(referenceKey, error);
		if (!player)
			return VyshkaActionOutcome.Failure(error);

		// Wetness lives on the items: the player is wet when the wettest
		// thing worn or carried is, and the engine's environment tick
		// recomputes the player's own wet flag from them. Every item in the
		// tree is dried, clothing, hands, and cargo alike, and the flag is
		// cleared now rather than on that tick.
		bool wasWet = player.GetStatWet().Get() > player.GetStatWet().GetMin();
		array<EntityAI> entities = new array<EntityAI>;
		player.GetInventory().EnumerateInventory(InventoryTraversalType.PREORDER, entities);
		int items = 0;
		int dried = 0;
		for (int i = 0; i < entities.Count(); i++)
		{
			ItemBase item = ItemBase.Cast(entities.Get(i));
			if (!item)
				continue;
			items++;
			if (item.GetWet() <= item.GetWetMin())
				continue;
			item.SetWet(item.GetWetMin());
			dried++;
		}
		player.GetStatWet().Set(0);
		VyshkaLog.Info("dried " + VyshkaVitals.Describe(player) + ": " + dried.ToString() + " of " + items.ToString() + " items were wet");

		VyshkaJsonValue result = VyshkaVitals.Result(player);
		result.Set("wasWet", VyshkaJsonValue.NewBool(wasWet));
		result.Set("items", VyshkaJsonValue.NewInt(items));
		result.Set("dried", VyshkaJsonValue.NewInt(dried));
		return VyshkaActionOutcome.Success(result);
	}
}

class VyshkaBrokenLegsAction : VyshkaAction
{
	override string Code()    { return "vyshka.brokenlegs"; }
	override string Name()    { return "Break or mend legs"; }
	override string Context() { return "player"; }
	override string Danger()  { return "warning"; }

	override VyshkaJsonValue ParamsSchema()
	{
		VyshkaJsonValue broken = VyshkaJsonValue.NewObject();
		broken.Set("type", VyshkaJsonValue.NewString("boolean"));

		VyshkaJsonValue properties = VyshkaJsonValue.NewObject();
		properties.Set("broken", broken);

		VyshkaJsonValue required = VyshkaJsonValue.NewArray();
		required.Add(VyshkaJsonValue.NewString("broken"));

		VyshkaJsonValue schema = VyshkaJsonValue.NewObject();
		schema.Set("type", VyshkaJsonValue.NewString("object"));
		schema.Set("required", required);
		schema.Set("properties", properties);
		return schema;
	}

	override VyshkaActionOutcome Execute(string actionId, string context, string referenceKey, VyshkaJsonValue params)
	{
		if (!params || !params.IsObject() || !params.Get("broken") || !params.Get("broken").IsBool())
			return VyshkaActionOutcome.Failure("broken is required: true to break the legs, false to mend them");
		bool broken = params.GetBool("broken", false);

		string error;
		PlayerBase player = VyshkaVitals.Player(referenceKey, error);
		if (!player)
			return VyshkaActionOutcome.Failure(error);
		ModifiersManager modifiers = player.GetModifiersManager();
		if (!modifiers)
			return VyshkaActionOutcome.Failure("this character has no modifiers manager");

		string before = VyshkaVitals.LegsState(player);
		array<string> zones = VyshkaVitals.LegZones();
		int i;
		if (broken)
		{
			// The engine's own path when a hit ruins a leg zone: the
			// broken-legs modifier, reset first when it is already on. Its
			// activation ruins the leg zones, raises the fracture notifier,
			// and sets the state the client animates; a splint is taken
			// off by the reset. The request alone is honored on the
			// manager's next tick (up to 3 s away, measured: the read-back
			// in the same frame still said none), so the modifier is
			// activated now as well; the tick then finds it active and
			// does not activate it a second time.
			if (modifiers.IsModifierActive(eModifiers.MDF_BROKEN_LEGS))
				modifiers.DeactivateModifier(eModifiers.MDF_BROKEN_LEGS);
			modifiers.ActivateModifier(eModifiers.MDF_BROKEN_LEGS);
			ModifierBase brokenLegs = modifiers.GetModifier(eModifiers.MDF_BROKEN_LEGS);
			if (brokenLegs && !brokenLegs.IsActive())
				brokenLegs.Activate();
		}
		else
		{
			// Mending is what the modifier does on its own once both legs
			// are back at full health: the zones are restored first, so
			// its next tick agrees, and it is turned off now rather than
			// on that tick (which also removes a splint and the notifier).
			// A state left without the modifier (a character loaded with
			// it off) is cleared directly.
			for (i = 0; i < zones.Count(); i++)
				player.SetHealth(zones.Get(i), VyshkaVitals.HEALTH_TYPE, player.GetMaxHealth(zones.Get(i), VyshkaVitals.HEALTH_TYPE));
			if (modifiers.IsModifierActive(eModifiers.MDF_BROKEN_LEGS))
				modifiers.DeactivateModifier(eModifiers.MDF_BROKEN_LEGS);
			if (player.GetBrokenLegs() != eBrokenLegs.NO_BROKEN_LEGS)
				player.SetBrokenLegs(eBrokenLegs.NO_BROKEN_LEGS);
		}
		string after = VyshkaVitals.LegsState(player);
		VyshkaLog.Info("legs of " + VyshkaVitals.Describe(player) + ": " + before + " to " + after);

		VyshkaJsonValue result = VyshkaVitals.Result(player);
		result.Set("before", VyshkaJsonValue.NewString(before));
		result.Set("after", VyshkaJsonValue.NewString(after));
		VyshkaJsonValue legs = VyshkaJsonValue.NewObject();
		for (i = 0; i < zones.Count(); i++)
			legs.Set(zones.Get(i), VyshkaVitals.Number(player.GetHealth(zones.Get(i), VyshkaVitals.HEALTH_TYPE)));
		result.Set("legHealth", legs);
		return VyshkaActionOutcome.Success(result);
	}
}

class VyshkaBloodyHandsAction : VyshkaAction
{
	override string Code()    { return "vyshka.bloodyhands"; }
	override string Name()    { return "Bloody or clean hands"; }
	override string Context() { return "player"; }
	override string Danger()  { return "none"; }

	override VyshkaJsonValue ParamsSchema()
	{
		VyshkaJsonValue bloody = VyshkaJsonValue.NewObject();
		bloody.Set("type", VyshkaJsonValue.NewString("boolean"));

		VyshkaJsonValue properties = VyshkaJsonValue.NewObject();
		properties.Set("bloody", bloody);

		VyshkaJsonValue required = VyshkaJsonValue.NewArray();
		required.Add(VyshkaJsonValue.NewString("bloody"));

		VyshkaJsonValue schema = VyshkaJsonValue.NewObject();
		schema.Set("type", VyshkaJsonValue.NewString("object"));
		schema.Set("required", required);
		schema.Set("properties", properties);
		return schema;
	}

	override VyshkaActionOutcome Execute(string actionId, string context, string referenceKey, VyshkaJsonValue params)
	{
		if (!params || !params.IsObject() || !params.Get("bloody") || !params.Get("bloody").IsBool())
			return VyshkaActionOutcome.Failure("bloody is required: true to bloody the hands, false to clean them");
		bool bloody = params.GetBool("bloody", false);

		string error;
		PlayerBase player = VyshkaVitals.Player(referenceKey, error);
		if (!player)
			return VyshkaActionOutcome.Failure(error);

		bool before = player.HasBloodyHands();
		player.SetBloodyHands(bloody);
		bool after = player.HasBloodyHands();
		VyshkaLog.Info("hands of " + VyshkaVitals.Describe(player) + ": bloody " + before.ToString() + " to " + after.ToString());

		VyshkaJsonValue result = VyshkaVitals.Result(player);
		result.Set("before", VyshkaJsonValue.NewBool(before));
		result.Set("after", VyshkaJsonValue.NewBool(after));
		return VyshkaActionOutcome.Success(result);
	}
}
