// Vyshka DayZ plugin: player telemetry (spec section 8).
//
// The core player events a feed needs (connect, disconnect, death, damage)
// and the state.players snapshots a live map needs, built from what the
// world module can see: PlayerBase, PlayerIdentity, and the players list.
// The protocol side (buffering, batching, the outbox) is in the game module;
// this file only knows what a player is and what happened to one.
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

// VyshkaHit is what the character's hit hook saw: kept on the character so
// the death that follows a fatal hit can say where it landed and with what.
class VyshkaHit
{
	string m_Zone;
	string m_Ammo;
	string m_Type;
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

	// Position renders a world position as [x, y, z], or null when a
	// component is not a number NewFloat can carry (section 8.3 wants finite
	// numbers, and a snapshot with a bad position is rejected whole).
	static VyshkaJsonValue Position(vector pos)
	{
		VyshkaJsonValue position = VyshkaJsonValue.NewArray();
		for (int axis = 0; axis < 3; axis++)
		{
			VyshkaJsonValue component = VyshkaJsonValue.NewFloat(pos[axis]);
			if (!component)
				return null;
			position.Add(component);
		}
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
			// character, which gets the identity's admin flags like any.
			entry.m_Player = player;
			entry.m_Name = identity.GetName();
			VyshkaFlags.OnConnect(player, id);
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

		// The identity's admin flags (VyshkaFlags): what this process last
		// knew at once, the store's word when it answers.
		VyshkaFlags.OnConnect(player, id);

		// A banned identity is refused here, the earliest hook that has a
		// character to disconnect through the engine's ordinary logout. The
		// connect above is reported first so the feed shows the attempt and
		// the refusal in order. The kick runs on the next tick rather than
		// inside the engine's connect event, which still has work to do for
		// the new character after this hook returns. Either list refuses:
		// the server's own and the hub's installation list (spec section
		// 13.4) are enforced as their union.
		if (VyshkaBans.Find(id) || VyshkaInstallationBans.Find(id))
			GetGame().GetCallQueue(CALL_CATEGORY_GAMEPLAY).CallLater(KickBanned, 100, false, player);
	}

	// KickBanned is the deferred half of the ban check above, and what an
	// applied installation list runs for each player online on it. The bans
	// are looked up again here: one that expired or was lifted in the
	// meantime is no ban, and the entry's current reason is the one to
	// report. The server's own ban is named first when both apply.
	static void KickBanned(PlayerBase player)
	{
		if (!player || !player.GetIdentity())
			return;
		string id = player.GetIdentity().GetPlainId();
		string reason = "banned";
		string error;
		VyshkaBanEntry ban = VyshkaBans.Find(id);
		if (ban)
		{
			if (ban.m_Reason != "")
				reason = "banned: " + ban.m_Reason;
			if (!VyshkaModeration.KickFor(player, reason, "ban", ban.m_ActionId, "server", "", error))
				VyshkaLog.Error("banned player " + id + " could not be kicked: " + error);
			return;
		}
		VyshkaInstallationBanEntry installation = VyshkaInstallationBans.Find(id);
		if (!installation)
			return;
		if (installation.m_Reason != "")
			reason = "banned: " + installation.m_Reason;
		if (!VyshkaModeration.KickFor(player, reason, "ban", "", "installation", installation.m_BanId, error))
			VyshkaLog.Error("player " + id + " under installation ban " + installation.m_BanId + " could not be kicked: " + error);
	}

	// OnChat runs from the server mission's chat event: channel is the
	// engine's channel bitmask, sender the display name the engine attached.
	// The engine identifies the sender by name only, so the identity is
	// resolved against the roster and omitted when two online players share
	// the name (or none has it: a system line).
	static void OnChat(int channel, string sender, string text)
	{
		if (text == "")
			return;
		VyshkaJsonValue data = VyshkaJsonValue.NewObject();
		int matches;
		VyshkaRosterEntry entry = FindByName(sender, matches);
		if (entry && matches == 1)
			data.Set("player", Identity(entry.m_Id));
		data.Set("name", VyshkaJsonValue.NewString(sender));
		data.Set("channel", VyshkaJsonValue.NewString(ChannelName(channel)));
		// The raw value too: the direct chat of a retail client arrived as a
		// value outside the engine's documented CC* set on DayZ 1.29 (issue
		// #59), so the name alone would hide what the engine actually said.
		data.Set("channelId", VyshkaJsonValue.NewInt(channel));
		data.Set("text", VyshkaJsonValue.NewString(text));
		VyshkaPlugin.Emit("core.player.chat", data);
	}

	// ChannelName renders the engine's chat channel bitmask as a word. The
	// values are the engine's CC* constants; a message on more than one
	// channel is named by the first matched.
	static string ChannelName(int channel)
	{
		if (channel & CCDirect)
			return "direct";
		if (channel & CCMegaphone)
			return "megaphone";
		if (channel & CCTransmitter)
			return "transmitter";
		if (channel & CCPublicAddressSystem)
			return "publicAddress";
		if (channel & CCAdmin)
			return "admin";
		if (channel & CCSystem)
			return "system";
		if (channel & CCBattlEye)
			return "battleye";
		return "other";
	}

	// FindByName returns the first roster entry with the name and counts
	// how many have it.
	static VyshkaRosterEntry FindByName(string name, out int matches)
	{
		matches = 0;
		VyshkaRosterEntry found = null;
		for (int i = 0; i < Roster().Count(); i++)
		{
			VyshkaRosterEntry entry = Roster().GetElement(i);
			if (entry.m_Name != name)
				continue;
			matches++;
			if (!found)
				found = entry;
		}
		return found;
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

		// A player who logged out while seated has left the vehicle; the
		// exit is reported before the disconnect, in the order it happened.
		VyshkaVehicles.OnDisconnect(entry.m_Id, entry.m_Name);

		VyshkaJsonValue data = VyshkaJsonValue.NewObject();
		data.Set("player", Identity(entry.m_Id));
		data.Set("name", VyshkaJsonValue.NewString(entry.m_Name));
		VyshkaPlugin.Emit("core.player.disconnect", data);
	}

	// Subject fills the player, name, and position of a payload about a
	// character, from the roster first and the identity second. False when
	// no identity is known, in which case nothing is reported.
	static bool Subject(PlayerBase player, VyshkaJsonValue data)
	{
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
			return false;
		data.Set("player", Identity(id));
		data.Set("name", VyshkaJsonValue.NewString(name));
		VyshkaJsonValue position = Position(player.GetPosition());
		if (position)
			data.Set("position", position);
		return true;
	}

	// OnHit runs from the character's hit hook, after the engine has applied
	// the damage: the arguments are the hook's own. The source is read the
	// way the killer of a death is (DescribeSource), and the damage the way
	// the engine's own admin log reads it: the highest health damage across
	// the zones hit, and the character's health afterwards.
	//
	// A hit on a corpse is not reported: the character is not a player any
	// more, only its body. The hit that killed is, with fatal set, and it is
	// kept on the character so the death that follows can name the zone and
	// the ammunition.
	//
	// reportedDead is whether the character's fatal hit or death had been
	// reported before this hit hook began. The engine reports the hit that
	// killed after applying it, so the character is already dead here, and
	// it evaluates the death (EEKilled) after this hook, inside the same
	// damage call (measured on DayZ 1.29); one path evaluates it inside the
	// engine's own part of the hook instead (a non-lethal round whose shock
	// is converted to health damage there). Either way, a hit on a character
	// already reported dead when the hook began is a hit on the corpse, and
	// otherwise the hit that killed it.
	static void OnHit(PlayerBase player, bool reportedDead, TotalDamageResult damageResult, int damageType, EntityAI source, int component, string dmgZone, string ammo)
	{
		if (!player)
			return;
		bool fatal = false;
		if (!player.IsAlive())
		{
			if (reportedDead)
				return;
			fatal = true;
			player.m_VyshkaDead = true;
		}

		float health = 0;
		float blood = 0;
		float shock = 0;
		if (damageResult)
		{
			health = Loss(damageResult, "Health");
			blood = Loss(damageResult, "Blood");
			shock = Loss(damageResult, "Shock");
		}

		// A fall is two hits, one to health and one to shock, and a third
		// for the gear; like the admin log, only the one that cost health
		// is worth a line, so a fall that costs none is not reported.
		if (damageType == DamageType.CUSTOM && IsFallDamage(ammo) && health <= 0)
			return;

		VyshkaJsonValue data = VyshkaJsonValue.NewObject();
		if (!Subject(player, data))
		{
			VyshkaLog.Info("a character was hit that no known identity was attached to; not reported");
			return;
		}
		DescribeSource(player, source, data, "attacker", "attackerName", "sourceType", damageType == DamageType.FIRE_ARM);
		data.Set("damageType", VyshkaJsonValue.NewString(DamageTypeName(damageType)));
		if (dmgZone != "")
			data.Set("bodyPart", VyshkaJsonValue.NewString(dmgZone));
		if (ammo != "")
			data.Set("ammo", VyshkaJsonValue.NewString(ammo));
		if (!damageResult)
			data.Set("blocked", VyshkaJsonValue.NewBool(true));
		SetNumber(data, "damage", health);
		SetNumber(data, "blood", blood);
		SetNumber(data, "shock", shock);
		SetNumber(data, "health", player.GetHealth("", "Health"));
		if (fatal)
		{
			data.Set("fatal", VyshkaJsonValue.NewBool(true));
			VyshkaHit hit = new VyshkaHit();
			hit.m_Zone = dmgZone;
			hit.m_Ammo = ammo;
			hit.m_Type = DamageTypeName(damageType);
			player.m_VyshkaFatalHit = hit;
		}
		VyshkaPlugin.Emit("core.player.damage", data);
	}

	// Loss is the largest amount of one health type the engine's damage
	// result holds for a hit: the highest across the zones hit, or the
	// global value, whichever is larger. Measured on DayZ 1.29: the
	// highest-zone reading is 0 for a fall (which names no zone) and half
	// the global value for a head hit, so neither alone is the health the
	// character lost. It is still the engine's figure for the hit, not a
	// before-and-after of the vitals: a hit that overshoots reports more
	// than the character had left.
	static float Loss(TotalDamageResult damageResult, string healthType)
	{
		float zone = damageResult.GetHighestDamage(healthType);
		float global = damageResult.GetDamage("", healthType);
		if (global > zone)
			return global;
		return zone;
	}

	static bool IsFallDamage(string ammo)
	{
		if (ammo == DayZPlayerImplementFallDamage.FALL_DAMAGE_AMMO_HEALTH)
			return true;
		if (ammo == DayZPlayerImplementFallDamage.FALL_DAMAGE_AMMO_SHOCK)
			return true;
		if (ammo == DayZPlayerImplementFallDamage.FALL_DAMAGE_AMMO_HEALTH_ATTACHMENT)
			return true;
		return ammo == DayZPlayerImplementFallDamage.FALL_DAMAGE_AMMO_HEALTH_OTHER_ATTACHMENTS;
	}

	// DamageTypeName words the engine's damage type: melee covers a player's
	// hands and held items, infected, and animals; other is the engine's
	// catch-all for vehicles, falls, fire, and area damage.
	static string DamageTypeName(int damageType)
	{
		if (damageType == DamageType.CLOSE_COMBAT)
			return "melee";
		if (damageType == DamageType.FIRE_ARM)
			return "firearm";
		if (damageType == DamageType.EXPLOSION)
			return "explosion";
		if (damageType == DamageType.STUN)
			return "stun";
		if (damageType == DamageType.CUSTOM)
			return "other";
		return "unknown";
	}

	// SetNumber sets a float member, or nothing when the value is not a
	// number the payload can carry.
	static void SetNumber(VyshkaJsonValue data, string key, float value)
	{
		VyshkaJsonValue number = VyshkaJsonValue.NewFloat(value);
		if (number)
			data.Set(key, number);
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
		VyshkaJsonValue data = VyshkaJsonValue.NewObject();
		if (!Subject(player, data))
		{
			VyshkaLog.Info("a character died that no known identity was attached to; not reported");
			return;
		}
		DescribeSource(player, killer, data, "killer", "killerName", "killerType", killer && killer.IsWeapon());
		if (killer == player)
			DescribeNaturalDeath(player, data);
		VyshkaHit hit = player.m_VyshkaFatalHit;
		if (hit)
		{
			// The hit that killed came through the hit hook first (the
			// engine applies the damage, reports the hit, then evaluates
			// the death), so the death can say where it landed.
			if (hit.m_Zone != "")
				data.Set("bodyPart", VyshkaJsonValue.NewString(hit.m_Zone));
			if (hit.m_Ammo != "")
				data.Set("ammo", VyshkaJsonValue.NewString(hit.m_Ammo));
			data.Set("damageType", VyshkaJsonValue.NewString(hit.m_Type));
			player.m_VyshkaFatalHit = null;
		}
		player.m_VyshkaDead = true;
		VyshkaPlugin.Emit("core.player.death", data);
	}

	// DescribeNaturalDeath adds what the engine's admin log adds to a death
	// it names the character itself as the killer of: water, energy, and
	// the open bleeding sources at that moment, so starvation, dehydration,
	// and bleeding out can be told apart; blood, since bleeding out is a
	// blood level; and whether the head was under water, the engine's own
	// eligibility check for drowning (an observation, not a verdict: a
	// submerged character can bleed out). The bleeding manager is read here
	// because the engine deletes it in its own death hook, after this one.
	static void DescribeNaturalDeath(PlayerBase player, VyshkaJsonValue data)
	{
		if (player.GetStatWater())
			SetNumber(data, "water", player.GetStatWater().Get());
		if (player.GetStatEnergy())
			SetNumber(data, "energy", player.GetStatEnergy().Get());
		SetNumber(data, "blood", player.GetHealth("", "Blood"));
		if (player.GetBleedingManagerServer())
			data.Set("bleedingSources", VyshkaJsonValue.NewInt(player.GetBleedingManagerServer().GetBleedingSourcesCount()));
		data.Set("submerged", VyshkaJsonValue.NewBool(player.GetDrowningWaterLevelCheck()));
	}

	// DescribeSource adds cause, and where known the other player (under
	// whoKey and whoNameKey), weapon, distance, and the engine class of a
	// source that is not a player (under typeKey), to a death or damage
	// payload. ranged says whether a distance to the other player is worth
	// reporting: a firearm's shot, not its butt.
	static void DescribeSource(PlayerBase victim, Object source, VyshkaJsonValue data, string whoKey, string whoNameKey, string typeKey, bool ranged)
	{
		if (!source)
		{
			data.Set("cause", VyshkaJsonValue.NewString("unknown"));
			return;
		}
		if (source == victim)
		{
			// Starvation, dehydration, bleeding out, drowning, a fall: the
			// engine reports the character as its own source.
			data.Set("cause", VyshkaJsonValue.NewString("self"));
			return;
		}

		// A player holding the item, or the player itself.
		PlayerBase sourcePlayer = PlayerBase.Cast(source);
		EntityAI sourceEntity = EntityAI.Cast(source);
		if (!sourcePlayer && sourceEntity)
			sourcePlayer = PlayerBase.Cast(sourceEntity.GetHierarchyRootPlayer());

		if (sourcePlayer == victim)
		{
			// The victim's own item did it (a firearm in hand, a grenade
			// still held): self-inflicted, and the item is worth naming.
			data.Set("cause", VyshkaJsonValue.NewString("self"));
			data.Set("weapon", VyshkaJsonValue.NewString(source.GetDisplayName()));
			return;
		}

		if (sourcePlayer)
		{
			data.Set("cause", VyshkaJsonValue.NewString("player"));
			VyshkaRosterEntry sourceEntry = FindByPlayer(sourcePlayer);
			PlayerIdentity sourceIdentity = sourcePlayer.GetIdentity();
			if (sourceEntry)
			{
				data.Set(whoKey, Identity(sourceEntry.m_Id));
				data.Set(whoNameKey, VyshkaJsonValue.NewString(sourceEntry.m_Name));
			}
			else if (sourceIdentity && sourceIdentity.GetPlainId() != "")
			{
				data.Set(whoKey, Identity(sourceIdentity.GetPlainId()));
				data.Set(whoNameKey, VyshkaJsonValue.NewString(sourceIdentity.GetName()));
			}
			if (source != sourcePlayer)
			{
				data.Set("weapon", VyshkaJsonValue.NewString(source.GetDisplayName()));
				if (ranged)
					SetNumber(data, "distance", vector.Distance(victim.GetPosition(), sourcePlayer.GetPosition()));
			}
			return;
		}

		data.Set(typeKey, VyshkaJsonValue.NewString(source.GetType()));
		if (ExplosivesBase.Cast(source))
		{
			data.Set("cause", VyshkaJsonValue.NewString("explosion"));
			data.Set("weapon", VyshkaJsonValue.NewString(source.GetDisplayName()));
		}
		else if (DayZInfected.Cast(source))
			data.Set("cause", VyshkaJsonValue.NewString("infected"));
		else if (DayZAnimal.Cast(source))
			data.Set("cause", VyshkaJsonValue.NewString("animal"));
		else if (Transport.Cast(source))
			data.Set("cause", VyshkaJsonValue.NewString("vehicle"));
		else
		{
			// Fire and barbed wire arrive here too: the engine's area damage
			// names the fireplace or the wire itself as the source (its
			// admin log still tests for a manager class that is not an
			// entity and never arrives), so typeKey and ammo say which.
			data.Set("cause", VyshkaJsonValue.NewString("other"));
		}
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
	override VyshkaJsonValue CapturePlayers()
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
			VyshkaJsonValue position = VyshkaPlayers.Position(player.GetPosition());
			if (position)
				entry.Set("position", position);
			VyshkaJsonValue data = VyshkaJsonValue.NewObject();
			data.Set("alive", VyshkaJsonValue.NewBool(player.IsAlive()));
			data.Set("health", VyshkaJsonValue.NewInt((int)player.GetHealth("", "Health")));
			data.Set("blood", VyshkaJsonValue.NewInt((int)player.GetHealth("", "Blood")));
			// The admin flags in effect on this character, set ones only,
			// so a panel can show who has one (issue #71); absent when none.
			if (player.m_VyshkaFlags && player.m_VyshkaFlags.Any())
				data.Set("flags", player.m_VyshkaFlags.ToJson());
			entry.Set("data", data);
			players.Add(entry);
		}

		VyshkaJsonValue body = VyshkaJsonValue.NewObject();
		body.Set("capturedAt", VyshkaJsonValue.NewString(VyshkaClock.NowRfc3339()));
		body.Set("players", players);
		return body;
	}

	// The vehicle list lives with the vehicles (VyshkaVehicles); this is
	// the one snapshot source the plugin holds, so it answers for both.
	override VyshkaJsonValue CaptureVehicles()
	{
		return VyshkaVehicles.Capture();
	}

	// The world's clock and weather (VyshkaWeather), likewise.
	override VyshkaJsonValue CaptureWorld()
	{
		return VyshkaWeather.Capture();
	}
}

modded class PlayerBase
{
	// Whether this character's fatal hit or death has been reported, which
	// keeps hits on the corpse out of the feed (VyshkaPlayers.OnHit); and
	// the hit that killed, for the death report that follows it.
	bool m_VyshkaDead;
	ref VyshkaHit m_VyshkaFatalHit;
	// The admin flags in effect on this character (VyshkaFlags.Apply); null
	// until the plugin has applied a set. Read by the stamina and fire hooks
	// and by the AI-targeting question below.
	ref VyshkaFlagSet m_VyshkaFlags;

	override void EEKilled(Object killer)
	{
		if (GetGame().IsServer())
			VyshkaPlayers.OnDeath(this, killer);
		super.EEKilled(killer);
	}

	// The engine's AI asks this before an infected or an animal takes a
	// character as a target; a character ignored by AI says no, which is
	// what the engine's own diagnostic builds do for an untargetable player.
	override bool CanBeTargetedByAI(EntityAI ai)
	{
		if (VyshkaFlags.Has(this, VyshkaFlagSet.IGNORED_BY_AI))
			return false;
		return super.CanBeTargetedByAI(ai);
	}

	override void EEHitBy(TotalDamageResult damageResult, int damageType, EntityAI source, int component, string dmgZone, string ammo, vector modelPos, float speedCoef)
	{
		// Read before the engine's own part of the hook: a death it
		// evaluates in there (a non-lethal round's shock converted to health
		// damage) belongs to this hit, not to the corpse.
		bool reportedDead = m_VyshkaDead;
		super.EEHitBy(damageResult, damageType, source, component, dmgZone, ammo, modelPos, speedCoef);
		if (GetGame().IsServer())
			VyshkaPlayers.OnHit(this, reportedDead, damageResult, damageType, source, component, dmgZone, ammo);
	}

	// The engine starts a character's vehicle command as it takes a seat
	// and finishes it as it leaves (or is pulled out dead): the enter and
	// exit events of the vehicle telemetry (VyshkaVehicles).
	override void OnCommandVehicleStart()
	{
		super.OnCommandVehicleStart();
		if (GetGame().IsServer())
			VyshkaVehicles.OnEnter(this);
	}

	override void OnCommandVehicleFinish()
	{
		super.OnCommandVehicleFinish();
		if (GetGame().IsServer())
			VyshkaVehicles.OnExit(this);
	}

	// A seat switch keeps the command; the plugin's record follows the seat.
	override void OnVehicleSwitchSeat(int seatIndex)
	{
		super.OnVehicleSwitchSeat(seatIndex);
		if (GetGame().IsServer())
			VyshkaVehicles.OnSwitchSeat(this, seatIndex);
	}
}
