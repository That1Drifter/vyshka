// Vyshka DayZ plugin: the built-in heal action.
//
// The tracer bullet of issue #14: one action that touches the game, so the
// whole path from an operator's curl to a player's health bar is exercised
// end to end. The referenceKey of a player-context action is the player's
// platform identity (spec section 8.2), which on DayZ is the plain Steam id
// the engine exposes as PlayerIdentity.GetPlainId().

class VyshkaHealAction : VyshkaAction
{
	override string Code()      { return "vyshka.heal"; }
	override string Name()      { return "Heal player"; }
	override string Context()   { return "player"; }
	override string Namespace() { return "vyshka"; }
	override string Danger()    { return "none"; }

	override VyshkaJsonValue ParamsSchema()
	{
		VyshkaJsonValue restoreBlood = VyshkaJsonValue.NewObject();
		restoreBlood.Set("type", VyshkaJsonValue.NewString("boolean"));
		restoreBlood.Set("default", VyshkaJsonValue.NewBool(true));

		VyshkaJsonValue properties = VyshkaJsonValue.NewObject();
		properties.Set("restoreBlood", restoreBlood);

		VyshkaJsonValue schema = VyshkaJsonValue.NewObject();
		schema.Set("type", VyshkaJsonValue.NewString("object"));
		schema.Set("properties", properties);
		return schema;
	}

	override VyshkaActionOutcome Execute(string context, string referenceKey, VyshkaJsonValue params)
	{
		if (referenceKey == "")
			return VyshkaActionOutcome.Failure("a player-context action needs the player's identity as referenceKey");

		PlayerBase player = FindPlayer(referenceKey);
		if (!player)
			return VyshkaActionOutcome.Failure("player " + referenceKey + " is not online");

		bool restoreBlood = true;
		if (params && params.IsObject())
			restoreBlood = params.GetBool("restoreBlood", true);

		player.SetHealth("", "Health", player.GetMaxHealth("", "Health"));
		player.SetHealth("", "Shock", player.GetMaxHealth("", "Shock"));
		if (restoreBlood)
			player.SetHealth("", "Blood", player.GetMaxHealth("", "Blood"));
		if (player.GetBleedingManagerServer())
			player.GetBleedingManagerServer().RemoveAllSources();

		VyshkaJsonValue result = VyshkaJsonValue.NewObject();
		result.Set("health", VyshkaJsonValue.NewInt((int)player.GetHealth("", "Health")));
		result.Set("blood", VyshkaJsonValue.NewInt((int)player.GetHealth("", "Blood")));
		result.Set("shock", VyshkaJsonValue.NewInt((int)player.GetHealth("", "Shock")));
		PlayerIdentity identity = player.GetIdentity();
		if (identity)
			result.Set("name", VyshkaJsonValue.NewString(identity.GetName()));
		return VyshkaActionOutcome.Success(result);
	}

	// FindPlayer matches the plain (Steam64) id first and the hashed id
	// second, so either form of the identity resolves.
	static PlayerBase FindPlayer(string playerId)
	{
		array<Man> players = new array<Man>;
		GetGame().GetPlayers(players);
		for (int i = 0; i < players.Count(); i++)
		{
			PlayerBase player = PlayerBase.Cast(players.Get(i));
			if (!player)
				continue;
			PlayerIdentity identity = player.GetIdentity();
			if (!identity)
				continue;
			if (identity.GetPlainId() == playerId || identity.GetId() == playerId)
				return player;
		}
		return null;
	}
}
