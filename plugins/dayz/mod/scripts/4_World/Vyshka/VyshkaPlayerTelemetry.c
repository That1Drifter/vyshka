// Vyshka DayZ plugin: player telemetry (spec section 8).
//
// The core player events a feed needs (connect, disconnect, death) and the
// state.players snapshots a live map needs, built from what the world module
// can see: PlayerBase, PlayerIdentity, and the players list. The protocol
// side (buffering, batching, the outbox) is in the game module; this file
// only knows what a player is and what happened to one.
//
// Identity on DayZ is the plain Steam64 id the engine exposes as
// PlayerIdentity.GetPlainId(), published as { "platform": "steam", "id": ... }
// (section 8.2), the same key the heal action resolves a referenceKey by.
//
// Positions are the engine's own vector, [x, y, z] with y the elevation, so a
// DayZ map plots x against z. That is the "game's own map frame" of section
// 8.3; the hub never interprets it and a panel needs to know the game.

class VyshkaRosterEntry
{
	PlayerBase m_Player;   // the character currently attached to this identity; not owned
	string m_Id;           // plain Steam64 id
	string m_Name;
}

// VyshkaPlayers tracks who is connected so a disconnect can be attributed
// after the engine has already let go of the identity, and so a respawn
// (which comes through the connect hook again with a new character) is not
// reported as a second connect.
class VyshkaPlayers
{
	static const string PLATFORM = "steam";

	static ref map<string, ref VyshkaRosterEntry> s_Roster;

	static map<string, ref VyshkaRosterEntry> Roster()
	{
		if (!s_Roster)
			s_Roster = new map<string, ref VyshkaRosterEntry>;
		return s_Roster;
	}

	static void Reset()
	{
		Roster().Clear();
	}

	static VyshkaJsonValue Identity(string plainId)
	{
		VyshkaJsonValue identity = VyshkaJsonValue.NewObject();
		identity.Set("platform", VyshkaJsonValue.NewString(PLATFORM));
		identity.Set("id", VyshkaJsonValue.NewString(plainId));
		return identity;
	}

	static VyshkaJsonValue Position(vector pos)
	{
		VyshkaJsonValue position = VyshkaJsonValue.NewArray();
		position.Add(VyshkaJsonValue.NewFloat(pos[0]));
		position.Add(VyshkaJsonValue.NewFloat(pos[1]));
		position.Add(VyshkaJsonValue.NewFloat(pos[2]));
		return position;
	}

	// OnConnect runs when a character is attached to a connected identity:
	// on first join, and again on every respawn with the new character.
	static void OnConnect(PlayerBase player, PlayerIdentity identity)
	{
		if (!player || !identity)
			return;
		string id = identity.GetPlainId();
		if (id == "")
		{
			VyshkaLog.Warn("a player connected without a plain id; not reported");
			return;
		}
		VyshkaRosterEntry entry = s_Roster.Get(id);
		if (entry)
		{
			// A respawn or a reconnect within the logout window: the identity
			// never left, so there is nothing to announce beyond the new
			// character.
			entry.m_Player = player;
			entry.m_Name = identity.GetName();
			return;
		}
		entry = new VyshkaRosterEntry();
		entry.m_Player = player;
		entry.m_Id = id;
		entry.m_Name = identity.GetName();
		s_Roster.Set(id, entry);

		VyshkaJsonValue data = VyshkaJsonValue.NewObject();
		data.Set("player", Identity(id));
		data.Set("name", VyshkaJsonValue.NewString(entry.m_Name));
		VyshkaPlugin.Emit("core.player.connect", data);
	}

	// OnDisconnect runs once the logout is final. The identity may already be
	// gone by then, so the roster is consulted first and the identity second.
	static void OnDisconnect(PlayerBase player)
	{
		if (!player)
			return;
		VyshkaRosterEntry entry = FindByPlayer(player);
		if (!entry)
		{
			PlayerIdentity identity = player.GetIdentity();
			if (identity && identity.GetPlainId() != "")
				entry = s_Roster.Get(identity.GetPlainId());
		}
		if (!entry)
		{
			VyshkaLog.Info("a character disconnected that no known identity was attached to; not reported");
			return;
		}
		s_Roster.Remove(entry.m_Id);

		VyshkaJsonValue data = VyshkaJsonValue.NewObject();
		data.Set("player", Identity(entry.m_Id));
		data.Set("name", VyshkaJsonValue.NewString(entry.m_Name));
		VyshkaPlugin.Emit("core.player.disconnect", data);
	}

	// OnDeath runs from the character's death hook, while its identity is
	// still attached. The killer object is read the way the engine's own
	// admin log reads it: the character itself for deaths with no outside
	// cause, a weapon or melee item held by a player, a player's bare hands,
	// an infected or animal, an explosive, a vehicle, or something else.
	static void OnDeath(PlayerBase player, Object killer)
	{
		if (!player)
			return;
		VyshkaRosterEntry entry = FindByPlayer(player);
		string id = "";
		string name = "";
		if (entry)
		{
			id = entry.m_Id;
			name = entry.m_Name;
		}
		else
		{
			PlayerIdentity identity = player.GetIdentity();
			if (identity)
			{
				id = identity.GetPlainId();
				name = identity.GetName();
			}
		}
		if (id == "")
		{
			VyshkaLog.Info("a character died that no known identity was attached to; not reported");
			return;
		}

		VyshkaJsonValue data = VyshkaJsonValue.NewObject();
		data.Set("player", Identity(id));
		data.Set("name", VyshkaJsonValue.NewString(name));
		data.Set("position", Position(player.GetPosition()));
		DescribeKiller(player, killer, data);
		VyshkaPlugin.Emit("core.player.death", data);
	}

	// DescribeKiller adds cause, and where known killer, killerName, weapon,
	// distance, and killerType, to a death payload.
	static void DescribeKiller(PlayerBase victim, Object killer, VyshkaJsonValue data)
	{
		if (!killer)
		{
			data.Set("cause", VyshkaJsonValue.NewString("unknown"));
			return;
		}
		if (killer == victim)
		{
			// Starvation, dehydration, bleeding out, drowning, a fall: the
			// engine reports the character as its own killer.
			data.Set("cause", VyshkaJsonValue.NewString("self"));
			return;
		}

		// A player holding the killing item, or the player itself.
		PlayerBase killerPlayer = PlayerBase.Cast(killer);
		EntityAI killerEntity = EntityAI.Cast(killer);
		if (!killerPlayer && killerEntity)
			killerPlayer = PlayerBase.Cast(killerEntity.GetHierarchyRootPlayer());

		if (killerPlayer && killerPlayer != victim)
		{
			data.Set("cause", VyshkaJsonValue.NewString("player"));
			VyshkaRosterEntry killerEntry = FindByPlayer(killerPlayer);
			PlayerIdentity killerIdentity = killerPlayer.GetIdentity();
			if (killerEntry)
			{
				data.Set("killer", Identity(killerEntry.m_Id));
				data.Set("killerName", VyshkaJsonValue.NewString(killerEntry.m_Name));
			}
			else if (killerIdentity && killerIdentity.GetPlainId() != "")
			{
				data.Set("killer", Identity(killerIdentity.GetPlainId()));
				data.Set("killerName", VyshkaJsonValue.NewString(killerIdentity.GetName()));
			}
			if (killer != killerPlayer)
			{
				data.Set("weapon", VyshkaJsonValue.NewString(killer.GetDisplayName()));
				if (killer.IsWeapon())
					data.Set("distance", VyshkaJsonValue.NewFloat(vector.Distance(victim.GetPosition(), killerPlayer.GetPosition())));
			}
			return;
		}

		data.Set("killerType", VyshkaJsonValue.NewString(killer.GetType()));
		if (ExplosivesBase.Cast(killer))
		{
			data.Set("cause", VyshkaJsonValue.NewString("explosion"));
			data.Set("weapon", VyshkaJsonValue.NewString(killer.GetDisplayName()));
		}
		else if (DayZInfected.Cast(killer))
			data.Set("cause", VyshkaJsonValue.NewString("infected"));
		else if (DayZAnimal.Cast(killer))
			data.Set("cause", VyshkaJsonValue.NewString("animal"));
		else if (Transport.Cast(killer))
			data.Set("cause", VyshkaJsonValue.NewString("vehicle"));
		else
			data.Set("cause", VyshkaJsonValue.NewString("other"));
	}

	static VyshkaRosterEntry FindByPlayer(PlayerBase player)
	{
		for (int i = 0; i < s_Roster.Count(); i++)
		{
			VyshkaRosterEntry entry = s_Roster.GetElement(i);
			if (entry.m_Player == player)
				return entry;
		}
		return null;
	}
}

// VyshkaPlayerSnapshots builds the state.players body (section 8.3) from
// the engine's players list: everyone with an identity attached, alive or
// not, with position and the vitals the heal action reports.
class VyshkaPlayerSnapshots : VyshkaSnapshotSource
{
	override string CapturePlayers()
	{
		array<Man> men = new array<Man>;
		GetGame().GetPlayers(men);

		VyshkaJsonValue players = VyshkaJsonValue.NewArray();
		for (int i = 0; i < men.Count(); i++)
		{
			PlayerBase player = PlayerBase.Cast(men.Get(i));
			if (!player)
				continue;
			PlayerIdentity identity = player.GetIdentity();
			if (!identity || identity.GetPlainId() == "")
				continue;

			VyshkaJsonValue entry = VyshkaJsonValue.NewObject();
			entry.Set("player", VyshkaPlayers.Identity(identity.GetPlainId()));
			entry.Set("name", VyshkaJsonValue.NewString(identity.GetName()));
			entry.Set("position", VyshkaPlayers.Position(player.GetPosition()));
			VyshkaJsonValue data = VyshkaJsonValue.NewObject();
			data.Set("alive", VyshkaJsonValue.NewBool(player.IsAlive()));
			data.Set("health", VyshkaJsonValue.NewInt((int)player.GetHealth("", "Health")));
			data.Set("blood", VyshkaJsonValue.NewInt((int)player.GetHealth("", "Blood")));
			entry.Set("data", data);
			players.Add(entry);
		}

		VyshkaJsonValue body = VyshkaJsonValue.NewObject();
		body.Set("capturedAt", VyshkaJsonValue.NewString(VyshkaClock.NowRfc3339()));
		body.Set("players", players);
		return body.Serialize();
	}
}

modded class PlayerBase
{
	override void EEKilled(Object killer)
	{
		if (GetGame().IsServer())
			VyshkaPlayers.OnDeath(this, killer);
		super.EEKilled(killer);
	}
}
