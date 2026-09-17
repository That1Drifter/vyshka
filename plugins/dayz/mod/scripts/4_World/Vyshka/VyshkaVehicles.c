// Vyshka DayZ plugin: vehicles (issue #67).
//
// The state.vehicles snapshot a live map needs (spec section 8.3), the
// enter, exit, and destroy events a feed wants, and the two chores every
// long-running server needs: delete every destroyed vehicle, and unstuck
// one. The engine keeps no list of its vehicles that script can ask for, so
// this file keeps one: every car, boat, and helicopter registers itself as
// it is initialized and leaves as it is deleted, whichever way it arrived
// (the hive loading it at boot, the central economy respawning it, or a
// spawn action). The hooks sit on the three scripted vehicle bases
// (CarScript, BoatScript, HelicopterScript) because their common parent,
// Transport, is an engine class the script compiler refuses to mod.
//
// A vehicle is named by its network id, which the engine keeps for the
// object's whole life on this server and which is what "stable for the
// lifetime of the thing it names" (section 8.3) asks for. A restart is a new
// life: the ids start over with it. The vehicle-context actions take that id
// as their referenceKey, the same string the snapshot publishes.

// VyshkaSeat is what the plugin remembers about a seated player, so that
// the exit event can still name the vehicle after the engine has let go of
// the player's vehicle command.
class VyshkaSeat
{
	Transport m_Vehicle;   // not owned; null once the engine has deleted it
	string m_VehicleId;
	string m_Type;
	string m_Kind;
	int m_Seat;
	bool m_Driver;
}

class VyshkaVehicles
{
	static const string KIND_CAR = "car";
	static const string KIND_BOAT = "boat";
	static const string KIND_HELICOPTER = "helicopter";
	static const string KIND_VEHICLE = "vehicle";
	static const string EXIT_LEFT = "left";
	static const string EXIT_DISCONNECT = "disconnect";

	// Every vehicle alive on this server, in the order it was initialized.
	// The engine clears a plain reference when it destroys the object, so a
	// vehicle that left without EEDelete reads as null and is skipped.
	static ref array<Transport> s_Vehicles;
	// The vehicles whose destruction has been reported. The engine's kill
	// hook fires again on every later hit on a destroyed vehicle (measured
	// on DayZ 1.29: a destroyed boat's decay tick fired it every 10 s), and
	// one destruction is one event. A vehicle brought back from ruined
	// leaves this list as its health level changes (OnRepaired), and at the
	// next capture as a fallback, so a second destruction is reported.
	static ref array<Transport> s_Destroyed;
	static const int REPORT_BUDGET = 40000;   // serialized bytes the delete-destroyed lists may take together (the hub's result cap is 64 KiB)
	// The vehicle each seated identity is in, by plain Steam64 id.
	static ref map<string, ref VyshkaSeat> s_Seated;

	static array<Transport> Vehicles()
	{
		if (!s_Vehicles)
			s_Vehicles = new array<Transport>;
		return s_Vehicles;
	}

	static array<Transport> Destroyed()
	{
		if (!s_Destroyed)
			s_Destroyed = new array<Transport>;
		return s_Destroyed;
	}

	static map<string, ref VyshkaSeat> Seated()
	{
		if (!s_Seated)
			s_Seated = new map<string, ref VyshkaSeat>;
		return s_Seated;
	}

	// Reset forgets who is seated. The vehicle list is deliberately kept:
	// the hive initializes its vehicles before the mission is up, and the
	// plugin starting must not forget them.
	static void Reset()
	{
		Seated().Clear();
	}

	static void Register(Transport vehicle)
	{
		if (!vehicle)
			return;
		if (Vehicles().Find(vehicle) >= 0)
			return;
		Vehicles().Insert(vehicle);
	}

	static void Unregister(Transport vehicle)
	{
		if (!vehicle)
			return;
		int at = Vehicles().Find(vehicle);
		if (at >= 0)
			Vehicles().Remove(at);
		int reported = Destroyed().Find(vehicle);
		if (reported >= 0)
			Destroyed().Remove(reported);
	}

	// OnRepaired runs when a vehicle's global health level leaves ruined:
	// its next destruction is a new one to report.
	static void OnRepaired(Transport vehicle)
	{
		if (!vehicle)
			return;
		int reported = Destroyed().Find(vehicle);
		if (reported >= 0)
			Destroyed().Remove(reported);
	}

	// IsDriverSeat says whether a crew index is the driver's, by the seat
	// animation the vehicle declares for it.
	static bool IsDriverSeat(Transport vehicle, int seat)
	{
		if (seat < 0 || seat >= vehicle.CrewSize())
			return false;
		return vehicle.GetSeatAnimationType(seat) == DayZPlayerConstants.VEHICLESEAT_DRIVER;
	}

	// Id renders the engine's network id as one string, high bits first,
	// separated so two ids can never read as one.
	static string Id(Object vehicle)
	{
		int low;
		int high;
		vehicle.GetNetworkID(low, high);
		return high.ToString() + "-" + low.ToString();
	}

	// Live returns the registered vehicles that still exist and are not on
	// their way out, dropping the rest from the list as it goes.
	static array<Transport> Live()
	{
		array<Transport> live = new array<Transport>;
		array<Transport> all = Vehicles();
		for (int i = all.Count() - 1; i >= 0; i--)
		{
			Transport vehicle = all.Get(i);
			if (!vehicle)
			{
				all.Remove(i);
				continue;
			}
			if (vehicle.IsSetForDeletion())
				continue;
			live.Insert(vehicle);
		}
		// The removal loop walked backwards; hand the list back in the
		// order the vehicles were registered.
		array<Transport> ordered = new array<Transport>;
		for (int j = live.Count() - 1; j >= 0; j--)
			ordered.Insert(live.Get(j));
		return ordered;
	}

	static Transport Find(string id)
	{
		array<Transport> live = Live();
		for (int i = 0; i < live.Count(); i++)
		{
			if (Id(live.Get(i)) == id)
				return live.Get(i);
		}
		return null;
	}

	// Kind names what the engine's class hierarchy makes of the vehicle. A
	// modded vehicle that extends none of the scripted bases is a "vehicle".
	static string Kind(Transport vehicle)
	{
		if (CarScript.Cast(vehicle))
			return KIND_CAR;
		if (BoatScript.Cast(vehicle))
			return KIND_BOAT;
		if (HelicopterScript.Cast(vehicle))
			return KIND_HELICOPTER;
		return KIND_VEHICLE;
	}

	// Crew lists the identities seated in the vehicle, with their seat index
	// and whether the seat is the driver's. A seat holding something with no
	// identity (a corpse whose player has left) is skipped.
	static VyshkaJsonValue Crew(Transport vehicle)
	{
		VyshkaJsonValue crew = VyshkaJsonValue.NewArray();
		Human driver = vehicle.CrewDriver();
		for (int seat = 0; seat < vehicle.CrewSize(); seat++)
		{
			Human occupant = vehicle.CrewMember(seat);
			if (!occupant)
				continue;
			PlayerBase player = PlayerBase.Cast(occupant);
			if (!player || !player.GetIdentity() || player.GetIdentity().GetPlainId() == "")
				continue;
			VyshkaJsonValue member = VyshkaJsonValue.NewObject();
			member.Set("player", VyshkaPlayers.Identity(player.GetIdentity().GetPlainId()));
			member.Set("name", VyshkaJsonValue.NewString(player.GetIdentity().GetName()));
			member.Set("seat", VyshkaJsonValue.NewInt(seat));
			member.Set("driver", VyshkaJsonValue.NewBool(occupant == driver));
			crew.Add(member);
		}
		return crew;
	}

	// Label puts the fields every vehicle payload shares onto data: the id,
	// the engine class, the kind, and the position.
	static void Label(Transport vehicle, VyshkaJsonValue data)
	{
		data.Set("vehicle", VyshkaJsonValue.NewString(Id(vehicle)));
		data.Set("type", VyshkaJsonValue.NewString(vehicle.GetType()));
		data.Set("kind", VyshkaJsonValue.NewString(Kind(vehicle)));
		VyshkaJsonValue position = VyshkaPlayers.Position(vehicle.GetPosition());
		if (position)
			data.Set("position", position);
	}

	// Describe is one state.vehicles entry (section 8.3): id and kind at the
	// top, the engine class, its display name, the seat count, and the crew
	// under data.
	static VyshkaJsonValue Describe(Transport vehicle)
	{
		VyshkaJsonValue entry = VyshkaJsonValue.NewObject();
		entry.Set("id", VyshkaJsonValue.NewString(Id(vehicle)));
		entry.Set("kind", VyshkaJsonValue.NewString(Kind(vehicle)));
		VyshkaJsonValue position = VyshkaPlayers.Position(vehicle.GetPosition());
		if (position)
			entry.Set("position", position);
		VyshkaJsonValue data = VyshkaJsonValue.NewObject();
		data.Set("type", VyshkaJsonValue.NewString(vehicle.GetType()));
		data.Set("displayName", VyshkaJsonValue.NewString(vehicle.GetDisplayName()));
		data.Set("seats", VyshkaJsonValue.NewInt(vehicle.CrewSize()));
		data.Set("crew", Crew(vehicle));
		entry.Set("data", data);
		return entry;
	}

	// Capture builds the state.vehicles body: every vehicle alive right now.
	static string Capture()
	{
		array<Transport> live = Live();
		VyshkaJsonValue vehicles = VyshkaJsonValue.NewArray();
		for (int i = 0; i < live.Count(); i++)
			vehicles.Add(Describe(live.Get(i)));
		// A vehicle reported destroyed and since repaired can be destroyed
		// again; one that is gone has nothing left to report.
		array<Transport> reported = Destroyed();
		for (int j = reported.Count() - 1; j >= 0; j--)
		{
			Transport wreck = reported.Get(j);
			if (!wreck || !wreck.IsDamageDestroyed())
				reported.Remove(j);
		}
		VyshkaJsonValue body = VyshkaJsonValue.NewObject();
		body.Set("capturedAt", VyshkaJsonValue.NewString(VyshkaClock.NowRfc3339()));
		body.Set("vehicles", vehicles);
		return body.Serialize();
	}

	// ---- events ----

	// OnEnter runs when a player's vehicle command starts, on the server:
	// the character is taking a seat. The seat is read from the vehicle,
	// which knows the crew slot the command was started for.
	static void OnEnter(PlayerBase player)
	{
		if (!player)
			return;
		PlayerIdentity identity = player.GetIdentity();
		if (!identity || identity.GetPlainId() == "")
			return;
		HumanCommandVehicle command = player.GetCommand_Vehicle();
		if (!command)
			return;
		Transport vehicle = command.GetTransport();
		if (!vehicle)
			return;
		string plainId = identity.GetPlainId();
		int seat = vehicle.CrewMemberIndex(player);

		VyshkaSeat record = new VyshkaSeat();
		record.m_Vehicle = vehicle;
		record.m_VehicleId = Id(vehicle);
		record.m_Type = vehicle.GetType();
		record.m_Kind = Kind(vehicle);
		record.m_Seat = seat;
		record.m_Driver = vehicle.CrewDriver() == player;
		Seated().Set(plainId, record);

		VyshkaJsonValue data = VyshkaJsonValue.NewObject();
		data.Set("player", VyshkaPlayers.Identity(plainId));
		data.Set("name", VyshkaJsonValue.NewString(identity.GetName()));
		Label(vehicle, data);
		data.Set("seat", VyshkaJsonValue.NewInt(seat));
		data.Set("driver", VyshkaJsonValue.NewBool(record.m_Driver));
		VyshkaPlugin.Emit("vyshka.vehicle.enter", data);
	}

	// OnSwitchSeat runs when a seated player moves to another seat of the
	// same vehicle: the command carries on, so the record follows the seat
	// and the exit that ends it names the seat actually left.
	static void OnSwitchSeat(PlayerBase player, int seat)
	{
		if (!player)
			return;
		PlayerIdentity identity = player.GetIdentity();
		if (!identity || identity.GetPlainId() == "")
			return;
		VyshkaSeat record;
		if (!Seated().Find(identity.GetPlainId(), record))
			return;
		record.m_Seat = seat;
		record.m_Driver = false;
		if (record.m_Vehicle)
			record.m_Driver = IsDriverSeat(record.m_Vehicle, seat);
	}

	// OnExit runs when the player's vehicle command finishes: the character
	// is out, or the command gave way to another (a death in the seat ends
	// it too). The record made on entry names the vehicle, since the
	// command may already be gone.
	static void OnExit(PlayerBase player)
	{
		if (!player)
			return;
		PlayerIdentity identity = player.GetIdentity();
		if (!identity || identity.GetPlainId() == "")
			return;
		Leave(identity.GetPlainId(), identity.GetName(), EXIT_LEFT);
	}

	// OnDisconnect runs from the roster's disconnect handling: a player who
	// logged out while seated is out of the vehicle from then on.
	static void OnDisconnect(string plainId, string name)
	{
		Leave(plainId, name, EXIT_DISCONNECT);
	}

	static void Leave(string plainId, string name, string cause)
	{
		VyshkaSeat record;
		if (!Seated().Find(plainId, record))
			return;
		Seated().Remove(plainId);

		VyshkaJsonValue data = VyshkaJsonValue.NewObject();
		data.Set("player", VyshkaPlayers.Identity(plainId));
		data.Set("name", VyshkaJsonValue.NewString(name));
		if (record.m_Vehicle)
		{
			Label(record.m_Vehicle, data);
		}
		else
		{
			// The vehicle went first (deleted with the player still seated):
			// what was recorded on entry is all there is.
			data.Set("vehicle", VyshkaJsonValue.NewString(record.m_VehicleId));
			data.Set("type", VyshkaJsonValue.NewString(record.m_Type));
			data.Set("kind", VyshkaJsonValue.NewString(record.m_Kind));
		}
		data.Set("seat", VyshkaJsonValue.NewInt(record.m_Seat));
		data.Set("driver", VyshkaJsonValue.NewBool(record.m_Driver));
		data.Set("cause", VyshkaJsonValue.NewString(cause));
		VyshkaPlugin.Emit("vyshka.vehicle.exit", data);
	}

	// OnDestroyed runs from the vehicle's own kill hook, on the server: its
	// health reached zero. The hook fires again on every later hit on the
	// wreck, so a vehicle already reported is not reported twice. The
	// killer is read the way the death event reads
	// a player's killer: a player behind the item that did it, an explosive,
	// another vehicle, the vehicle itself (fire, a fall), or something else.
	static void OnDestroyed(Transport vehicle, Object killer)
	{
		if (!vehicle)
			return;
		if (Destroyed().Find(vehicle) >= 0)
			return;
		Destroyed().Insert(vehicle);
		VyshkaJsonValue data = VyshkaJsonValue.NewObject();
		Label(vehicle, data);
		data.Set("crew", Crew(vehicle));
		DescribeKiller(vehicle, killer, data);
		VyshkaPlugin.Emit("core.vehicle.destroy", data);
	}

	static void DescribeKiller(Transport victim, Object killer, VyshkaJsonValue data)
	{
		if (!killer)
		{
			data.Set("cause", VyshkaJsonValue.NewString("unknown"));
			return;
		}
		if (killer == victim)
		{
			data.Set("cause", VyshkaJsonValue.NewString("self"));
			return;
		}
		PlayerBase killerPlayer = PlayerBase.Cast(killer);
		EntityAI killerEntity = EntityAI.Cast(killer);
		if (!killerPlayer && killerEntity)
			killerPlayer = PlayerBase.Cast(killerEntity.GetHierarchyRootPlayer());
		if (killerPlayer)
		{
			data.Set("cause", VyshkaJsonValue.NewString("player"));
			PlayerIdentity killerIdentity = killerPlayer.GetIdentity();
			if (killerIdentity && killerIdentity.GetPlainId() != "")
			{
				data.Set("killer", VyshkaPlayers.Identity(killerIdentity.GetPlainId()));
				data.Set("killerName", VyshkaJsonValue.NewString(killerIdentity.GetName()));
			}
			if (killer != killerPlayer)
				data.Set("weapon", VyshkaJsonValue.NewString(killer.GetDisplayName()));
			return;
		}
		data.Set("killerType", VyshkaJsonValue.NewString(killer.GetType()));
		if (ExplosivesBase.Cast(killer))
		{
			data.Set("cause", VyshkaJsonValue.NewString("explosion"));
			data.Set("weapon", VyshkaJsonValue.NewString(killer.GetDisplayName()));
		}
		else if (Transport.Cast(killer))
			data.Set("cause", VyshkaJsonValue.NewString("vehicle"));
		else
			data.Set("cause", VyshkaJsonValue.NewString("other"));
	}
}

// The three scripted vehicle bases register as they are initialized, leave
// as they are deleted, and report their destruction. Every vanilla vehicle
// and every modded one that extends these is covered; a mod that extends
// the engine's Car, Boat, or Helicopter directly is not, and is not
// modded from script either.
modded class CarScript
{
	override void EEInit()
	{
		super.EEInit();
		if (GetGame().IsServer())
			VyshkaVehicles.Register(this);
	}

	override void EEDelete(EntityAI parent)
	{
		if (GetGame().IsServer())
			VyshkaVehicles.Unregister(this);
		super.EEDelete(parent);
	}

	override void EEKilled(Object killer)
	{
		if (GetGame().IsServer())
			VyshkaVehicles.OnDestroyed(this, killer);
		super.EEKilled(killer);
	}

	// The global zone (an empty zone name) leaving ruined is a repair: the
	// next destruction is a new one.
	override void EEHealthLevelChanged(int oldLevel, int newLevel, string zone)
	{
		super.EEHealthLevelChanged(oldLevel, newLevel, zone);
		if (GetGame().IsServer() && zone == "" && oldLevel == GameConstants.STATE_RUINED && newLevel != GameConstants.STATE_RUINED)
			VyshkaVehicles.OnRepaired(this);
	}
}

modded class BoatScript
{
	override void EEInit()
	{
		super.EEInit();
		if (GetGame().IsServer())
			VyshkaVehicles.Register(this);
	}

	override void EEDelete(EntityAI parent)
	{
		if (GetGame().IsServer())
			VyshkaVehicles.Unregister(this);
		super.EEDelete(parent);
	}

	override void EEKilled(Object killer)
	{
		if (GetGame().IsServer())
			VyshkaVehicles.OnDestroyed(this, killer);
		super.EEKilled(killer);
	}

	// The global zone (an empty zone name) leaving ruined is a repair: the
	// next destruction is a new one.
	override void EEHealthLevelChanged(int oldLevel, int newLevel, string zone)
	{
		super.EEHealthLevelChanged(oldLevel, newLevel, zone);
		if (GetGame().IsServer() && zone == "" && oldLevel == GameConstants.STATE_RUINED && newLevel != GameConstants.STATE_RUINED)
			VyshkaVehicles.OnRepaired(this);
	}
}

modded class HelicopterScript
{
	override void EEInit()
	{
		super.EEInit();
		if (GetGame().IsServer())
			VyshkaVehicles.Register(this);
	}

	override void EEDelete(EntityAI parent)
	{
		if (GetGame().IsServer())
			VyshkaVehicles.Unregister(this);
		super.EEDelete(parent);
	}

	override void EEKilled(Object killer)
	{
		if (GetGame().IsServer())
			VyshkaVehicles.OnDestroyed(this, killer);
		super.EEKilled(killer);
	}

	// The global zone (an empty zone name) leaving ruined is a repair: the
	// next destruction is a new one.
	override void EEHealthLevelChanged(int oldLevel, int newLevel, string zone)
	{
		super.EEHealthLevelChanged(oldLevel, newLevel, zone);
		if (GetGame().IsServer() && zone == "" && oldLevel == GameConstants.STATE_RUINED && newLevel != GameConstants.STATE_RUINED)
			VyshkaVehicles.OnRepaired(this);
	}
}

// VyshkaUnstuckAction lifts one vehicle a little, levels it, stops it, and
// wakes its physics: what an admin does for a car wedged in a rock, rolled
// onto its roof, or sunk into the terrain. The vehicle is moved with
// whoever is in it, the way a teleport moves a seated player.
class VyshkaUnstuckAction : VyshkaAction
{
	static const float LIFT_DEFAULT = 1.0;
	static const float LIFT_MAX = 10.0;

	override string Code()    { return "vyshka.unstuck"; }
	override string Name()    { return "Unstuck vehicle"; }
	override string Context() { return "vehicle"; }
	override string Danger()  { return "warning"; }

	override VyshkaJsonValue ParamsSchema()
	{
		VyshkaJsonValue lift = VyshkaJsonValue.NewObject();
		lift.Set("type", VyshkaJsonValue.NewString("number"));
		lift.Set("minimum", VyshkaJsonValue.NewInt(0));
		lift.Set("maximum", VyshkaJsonValue.NewInt(10));
		lift.Set("default", VyshkaJsonValue.NewInt(1));

		VyshkaJsonValue level = VyshkaJsonValue.NewObject();
		level.Set("type", VyshkaJsonValue.NewString("boolean"));
		level.Set("default", VyshkaJsonValue.NewBool(true));

		VyshkaJsonValue properties = VyshkaJsonValue.NewObject();
		properties.Set("lift", lift);
		properties.Set("level", level);

		VyshkaJsonValue schema = VyshkaJsonValue.NewObject();
		schema.Set("type", VyshkaJsonValue.NewString("object"));
		schema.Set("properties", properties);
		return schema;
	}

	override VyshkaActionOutcome Execute(string actionId, string context, string referenceKey, VyshkaJsonValue params)
	{
		if (referenceKey == "")
			return VyshkaActionOutcome.Failure("a vehicle-context action needs the vehicle's id as referenceKey");
		Transport vehicle = VyshkaVehicles.Find(referenceKey);
		if (!vehicle)
			return VyshkaActionOutcome.Failure("no vehicle " + referenceKey + " exists on this server (ids are in the state.vehicles snapshot)");

		float lift = LIFT_DEFAULT;
		bool level = true;
		if (params && params.IsObject())
		{
			lift = params.GetFloat("lift", LIFT_DEFAULT);
			level = params.GetBool("level", true);
		}
		if (lift < 0 || lift > LIFT_MAX)
			return VyshkaActionOutcome.Failure("lift must lie within 0 and 10 metres");

		// Never below the terrain or the sea: a vehicle that sank into the
		// heightmap is lifted from the surface, not from where it sank to.
		vector from = vehicle.GetPosition();
		vector to = from;
		float floor = GetGame().SurfaceY(to[0], to[2]);
		float seaLevel = GetGame().SurfaceGetSeaLevel();
		if (seaLevel > floor)
			floor = seaLevel;
		if (to[1] < floor)
			to[1] = floor;
		to[1] = to[1] + lift;

		vector orientationBefore = vehicle.GetOrientation();
		vector orientationAfter = orientationBefore;
		if (level)
		{
			// Keep the heading, drop the pitch and the roll: the engine's
			// orientation is yaw, pitch, roll in degrees.
			orientationAfter = Vector(orientationBefore[0], 0, 0);
			vehicle.SetOrientation(orientationAfter);
		}
		vehicle.SetPosition(to);
		SetVelocity(vehicle, vector.Zero);
		dBodySetAngularVelocity(vehicle, vector.Zero);
		dBodyActive(vehicle, ActiveState.ACTIVE);
		vehicle.Synchronize();
		VyshkaLog.Info("unstuck " + vehicle.GetType() + " (" + referenceKey + ") from " + from.ToString() + " to " + to.ToString());

		VyshkaJsonValue result = VyshkaJsonValue.NewObject();
		VyshkaVehicles.Label(vehicle, result);
		VyshkaJsonValue fromJson = VyshkaPlayers.Position(from);
		if (fromJson)
			result.Set("from", fromJson);
		VyshkaJsonValue toJson = VyshkaPlayers.Position(to);
		if (toJson)
			result.Set("to", toJson);
		VyshkaJsonValue before = VyshkaPlayers.Position(orientationBefore);
		if (before)
			result.Set("orientationBefore", before);
		VyshkaJsonValue after = VyshkaPlayers.Position(orientationAfter);
		if (after)
			result.Set("orientationAfter", after);
		result.Set("crew", VyshkaVehicles.Crew(vehicle));
		return VyshkaActionOutcome.Success(result);
	}
}

// VyshkaDeleteDestroyedAction removes every destroyed vehicle: the wrecks a
// long-running server accumulates. A wreck with someone still in it is left
// alone and reported, and dryRun lists what would go without deleting it.
class VyshkaDeleteDestroyedAction : VyshkaAction
{
	override string Code()    { return "vyshka.deletedestroyed"; }
	override string Name()    { return "Delete destroyed vehicles"; }
	override string Context() { return "world"; }
	override string Danger()  { return "destructive"; }

	override VyshkaJsonValue ParamsSchema()
	{
		VyshkaJsonValue dryRun = VyshkaJsonValue.NewObject();
		dryRun.Set("type", VyshkaJsonValue.NewString("boolean"));
		dryRun.Set("default", VyshkaJsonValue.NewBool(false));

		VyshkaJsonValue properties = VyshkaJsonValue.NewObject();
		properties.Set("dryRun", dryRun);

		VyshkaJsonValue schema = VyshkaJsonValue.NewObject();
		schema.Set("type", VyshkaJsonValue.NewString("object"));
		schema.Set("properties", properties);
		return schema;
	}

	override VyshkaActionOutcome Execute(string actionId, string context, string referenceKey, VyshkaJsonValue params)
	{
		bool dryRun = false;
		if (params && params.IsObject())
			dryRun = params.GetBool("dryRun", false);

		// The lists are bounded by their serialized size, shared, so the
		// result stays inside the hub's cap (section 7: a result over
		// 64 KiB is dropped whole, counts included); the counts are always
		// complete, and truncated says when the lists are not.
		array<Transport> live = VyshkaVehicles.Live();
		VyshkaJsonValue deleted = VyshkaJsonValue.NewArray();
		VyshkaJsonValue skipped = VyshkaJsonValue.NewArray();
		int deletedCount = 0;
		int skippedCount = 0;
		int intact = 0;
		int budget = VyshkaVehicles.REPORT_BUDGET;
		for (int i = 0; i < live.Count(); i++)
		{
			Transport vehicle = live.Get(i);
			if (!vehicle.IsDamageDestroyed())
			{
				intact++;
				continue;
			}
			VyshkaJsonValue entry = VyshkaJsonValue.NewObject();
			VyshkaVehicles.Label(vehicle, entry);
			if (vehicle.IsAnyCrewPresent())
			{
				skippedCount++;
				entry.Set("reason", VyshkaJsonValue.NewString("someone is still in it"));
				int skippedBytes = entry.Serialize().Length() + 1;
				if (skippedBytes <= budget)
				{
					skipped.Add(entry);
					budget -= skippedBytes;
				}
				continue;
			}
			deletedCount++;
			int deletedBytes = entry.Serialize().Length() + 1;
			if (deletedBytes <= budget)
			{
				deleted.Add(entry);
				budget -= deletedBytes;
			}
			if (!dryRun)
			{
				VyshkaLog.Info("deleting destroyed " + vehicle.GetType() + " (" + VyshkaVehicles.Id(vehicle) + ") at " + vehicle.GetPosition().ToString());
				vehicle.DeleteSafe();
			}
		}

		VyshkaJsonValue result = VyshkaJsonValue.NewObject();
		result.Set("dryRun", VyshkaJsonValue.NewBool(dryRun));
		result.Set("deleted", deleted);
		result.Set("skipped", skipped);
		result.Set("deletedCount", VyshkaJsonValue.NewInt(deletedCount));
		result.Set("skippedCount", VyshkaJsonValue.NewInt(skippedCount));
		result.Set("intact", VyshkaJsonValue.NewInt(intact));
		result.Set("truncated", VyshkaJsonValue.NewBool(deletedCount > deleted.Count() || skippedCount > skipped.Count()));
		return VyshkaActionOutcome.Success(result);
	}
}
