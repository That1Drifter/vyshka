// Vyshka Reforger plugin: the game mode hooks that start and stop the
// plugin, report players, and the built-in actions.
//
// The plugin rides SCR_BaseGameMode, which every scenario's game mode
// derives from, so it runs under any scenario without prefab edits. It
// starts on the server only (OnGameStart runs there with RplMode.Dedicated,
// measured in spikes/reforger-server-script-limits).
//
// A player is identified by the game's backend identity, registered as
// platform "reforger" (spec section 8.2): a UUID the server reads from
// OnPlayerAuditSuccess onward, and only when the server is registered with
// the game's backend. Before the audit, and on a server that is not
// registered, it reads empty, and such a player is left out of everything
// that needs an identity (spikes/reforger-player-identity).

class VyshkaPlayers
{
	static ref map<int, string> s_Identities = new map<int, string>;
	// The name each identified player joined under, for the disconnect
	// event: whether the engine still answers GetPlayerName inside
	// OnPlayerDisconnected is unmeasured.
	static ref map<int, string> s_Names = new map<int, string>;
	static bool s_WarnedUnidentified;

	// Reset forgets every player, at the start and end of a game: a world
	// torn down without a disconnect per player must not leave identities
	// that vyshka.info counts and vyshka.kick finds.
	static void Reset()
	{
		s_Identities.Clear();
		s_Names.Clear();
	}

	// Identity reads a player's backend identity, lowercased as the
	// registry asks, or "" when the game gives none.
	static string ReadIdentity(int playerId)
	{
		BackendApi api = GetGame().GetBackendApi();
		if (!api)
			return "";
		string id = api.GetPlayerIdentityId(playerId);
		id.ToLower();
		return id;
	}

	static VyshkaJsonValue IdentityJson(string id)
	{
		VyshkaJsonValue identity = VyshkaJsonValue.NewObject();
		identity.Set("platform", VyshkaJsonValue.NewString("reforger"));
		identity.Set("id", VyshkaJsonValue.NewString(id));
		return identity;
	}

	static void OnAudited(int playerId)
	{
		string name = GetGame().GetPlayerManager().GetPlayerName(playerId);
		string id = ReadIdentity(playerId);
		if (id == "")
		{
			// Spec section 8.2: an unidentifiable player is left out, and
			// the operator is told once why players are missing.
			if (!s_WarnedUnidentified)
			{
				s_WarnedUnidentified = true;
				VyshkaLog.Warn("player " + name + " has no backend identity; is the server registered with the game's backend (started with -config)? Players without one are not reported");
			}
			VyshkaLog.Info("player " + name + " joined unidentified and is not reported");
			return;
		}
		s_Identities.Set(playerId, id);
		s_Names.Set(playerId, name);
		VyshkaJsonValue data = VyshkaJsonValue.NewObject();
		data.Set("player", IdentityJson(id));
		data.Set("name", VyshkaJsonValue.NewString(name));
		VyshkaPlugin.Emit("core.player.connect", data);
	}

	static void OnLeft(int playerId)
	{
		string id;
		if (!s_Identities.Find(playerId, id))
			return;
		string name = s_Names.Get(playerId);
		s_Identities.Remove(playerId);
		s_Names.Remove(playerId);
		VyshkaJsonValue data = VyshkaJsonValue.NewObject();
		data.Set("player", IdentityJson(id));
		data.Set("name", VyshkaJsonValue.NewString(name));
		VyshkaPlugin.Emit("core.player.disconnect", data);
	}

	// FindPlayer returns the player id online under the given identity, or
	// 0 when none is.
	static int FindPlayer(string identity)
	{
		string wanted = identity;
		wanted.ToLower();
		foreach (int playerId, string id : s_Identities)
		{
			if (id == wanted)
				return playerId;
		}
		return 0;
	}
}

// VyshkaInfoAction reports what the server is running. It changes nothing,
// which makes it the right action for the plugin conformance suite to
// dispatch: the suite takes the first action of the manifest, which the
// registry sorts by code, and vyshka.info sorts ahead of vyshka.kick.
class VyshkaInfoAction : VyshkaAction
{
	override string Code()      { return "vyshka.info"; }
	override string Name()      { return "Server info"; }
	override string Context()   { return "world"; }
	override string Namespace() { return "vyshka"; }
	override string Danger()    { return "none"; }

	override VyshkaActionOutcome Execute(string actionId, string context, string referenceKey, VyshkaJsonValue params)
	{
		VyshkaJsonValue result = VyshkaJsonValue.NewObject();
		result.Set("world", VyshkaJsonValue.NewString(GetGame().GetWorldFile()));
		array<int> players = new array<int>;
		GetGame().GetPlayerManager().GetPlayers(players);
		result.Set("players", VyshkaJsonValue.NewInt(players.Count()));
		result.Set("identified", VyshkaJsonValue.NewInt(VyshkaPlayers.s_Identities.Count()));
		result.Set("uptimeSeconds", VyshkaJsonValue.NewInt(System.GetTickCount() / 1000));
		return VyshkaActionOutcome.Success(result);
	}
}

// VyshkaKickAction disconnects a player, named by their identity as the
// referenceKey of a player-context action.
class VyshkaKickAction : VyshkaAction
{
	override string Code()      { return "vyshka.kick"; }
	override string Name()      { return "Kick player"; }
	override string Context()   { return "player"; }
	override string Namespace() { return "vyshka"; }
	override string Danger()    { return "warning"; }

	override VyshkaActionOutcome Execute(string actionId, string context, string referenceKey, VyshkaJsonValue params)
	{
		if (referenceKey == "")
			return VyshkaActionOutcome.Failure("a player-context action needs the player's identity as referenceKey");
		int playerId = VyshkaPlayers.FindPlayer(referenceKey);
		if (playerId == 0)
			return VyshkaActionOutcome.Failure("player " + referenceKey + " is not online");
		string name = GetGame().GetPlayerManager().GetPlayerName(playerId);
		GetGame().GetPlayerManager().KickPlayer(playerId, PlayerManagerKickReason.KICK, 0);
		VyshkaJsonValue result = VyshkaJsonValue.NewObject();
		result.Set("name", VyshkaJsonValue.NewString(name));
		return VyshkaActionOutcome.Success(result);
	}
}

modded class SCR_BaseGameMode
{
	protected override void OnGameStart()
	{
		super.OnGameStart();
		if (!Replication.IsServer())
			return;
		VyshkaPlayers.Reset();
		VyshkaRegistry registry = new VyshkaRegistry();
		VyshkaRegister(registry);
		VyshkaPlugin.Start(registry);
	}

	// VyshkaRegister is where the built-in actions are declared; a mod that
	// loads after this one overrides it to add its own, calling super first.
	void VyshkaRegister(VyshkaRegistry registry)
	{
		registry.Register(new VyshkaInfoAction());
		registry.Register(new VyshkaKickAction());
	}

	override void OnPlayerAuditSuccess(int iPlayerID)
	{
		super.OnPlayerAuditSuccess(iPlayerID);
		if (Replication.IsServer())
			VyshkaPlayers.OnAudited(iPlayerID);
	}

	protected override void OnPlayerDisconnected(int playerId, KickCauseCode cause, int timeout)
	{
		if (Replication.IsServer())
			VyshkaPlayers.OnLeft(playerId);
		super.OnPlayerDisconnected(playerId, cause, timeout);
	}

	override void OnGameEnd()
	{
		if (Replication.IsServer())
		{
			VyshkaPlugin.Stop();
			VyshkaPlayers.Reset();
		}
		super.OnGameEnd();
	}
}
