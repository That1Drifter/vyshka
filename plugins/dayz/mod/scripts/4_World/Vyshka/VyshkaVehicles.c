// Vyshka DayZ plugin: vehicles (issues #67 and #77).
//
// The state.vehicles snapshot a live map needs (spec section 8.3), with each
// vehicle's damage state, health, and fluids; the enter, exit, and destroy
// events a feed wants; and the chores every long-running server needs:
// delete every destroyed vehicle, unstuck one, refuel one, and repair one.
// The engine keeps no list of its vehicles that script can ask for, so
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

// VyshkaVehicleHit is the hit that destroyed a vehicle, kept from the hit
// hook for the kill hook that follows it in the same damage call.
class VyshkaVehicleHit
{
	Transport m_Vehicle;   // not owned
	EntityAI m_Source;     // not owned; what the hit came from, when the engine named it
	int m_Type;
	string m_Ammo;
}

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
	static const string STATE_INTACT = "intact";
	static const string STATE_DESTROYED = "destroyed";
	static const string STATE_EXPLODED = "exploded";
	static const string FLUID_FUEL = "fuel";
	static const string FLUID_OIL = "oil";
	static const string FLUID_BRAKE = "brake";
	static const string FLUID_COOLANT = "coolant";

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
	// The destroyed vehicles whose destruction was an explosion: the hit
	// that destroyed them carried the engine's explosion damage type, or the
	// killer the engine named was an explosive. Kept in memory only, which
	// loses nothing: a restart brings no wreck back for longer than its load
	// (below).
	static ref array<Transport> s_Exploded;
	// The vehicles the hive is loading. A wreck saved at shutdown is loaded
	// at the next boot, runs its kill hook inside its own load (between the
	// store-load hooks), and is deleted by the engine half a second later
	// (measured on DayZ 1.29, two wrecks over two restarts); a destruction
	// from an earlier run is not a new one, so a kill hook inside a load is
	// recorded and not reported. The list is cleared at every capture as
	// well, since a load that failed never reaches its closing hook.
	static ref array<Transport> s_Loading;
	// The hits that destroyed vehicles, from their hit hooks, until the kill
	// hook that follows each reads its own (OnHit, TakeHit). One per
	// vehicle, so a kill hook another vehicle runs in between (a mod
	// damaging a second vehicle from its own hit hook) takes nothing from
	// it; any left over are cleared at every capture, since a kill hook
	// follows its hit inside the same damage call or not at all.
	static ref array<ref VyshkaVehicleHit> s_FatalHits;
	static const int REPORT_BUDGET = 40000;   // serialized bytes the delete-destroyed lists may take together (the hub's result cap is 64 KiB)
	// A snapshot body is capped at 262144 bytes (section 8.3). A capture
	// that would pass this is made again with less detail (Describe), which
	// keeps every vehicle in it.
	static const int SNAPSHOT_BUDGET = 260000;
	static const int DETAIL_FULL = 0;
	static const int DETAIL_COMPACT = 1;
	static const int DETAIL_PLACED = 2;
	static const int DETAIL_ID = 3;
	static int s_LoggedDetail;   // the detail level of the last capture, so a change is logged once
	static const int PART_DEPTH_MAX = 3;   // levels of parts a repair walks (a car, its door, anything on the door)
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

	static array<Transport> Exploded()
	{
		if (!s_Exploded)
			s_Exploded = new array<Transport>;
		return s_Exploded;
	}

	static array<Transport> Loading()
	{
		if (!s_Loading)
			s_Loading = new array<Transport>;
		return s_Loading;
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
		Forget(vehicle);
	}

	// Forget drops a vehicle from the destruction records: it is gone, or it
	// has been brought back.
	static void Forget(Transport vehicle)
	{
		int reported = Destroyed().Find(vehicle);
		if (reported >= 0)
			Destroyed().Remove(reported);
		int exploded = Exploded().Find(vehicle);
		if (exploded >= 0)
			Exploded().Remove(exploded);
		TakeHit(vehicle);
		int loading = Loading().Find(vehicle);
		if (loading >= 0)
			Loading().Remove(loading);
	}

	// OnLoading and OnLoaded bracket a vehicle's load from the hive.
	static void OnLoading(Transport vehicle)
	{
		if (vehicle && Loading().Find(vehicle) < 0)
			Loading().Insert(vehicle);
	}

	static void OnLoaded(Transport vehicle)
	{
		int at = Loading().Find(vehicle);
		if (at >= 0)
			Loading().Remove(at);
	}

	// OnRepaired runs when a vehicle's global health level leaves ruined:
	// its next destruction is a new one to report.
	static void OnRepaired(Transport vehicle)
	{
		if (!vehicle)
			return;
		Forget(vehicle);
	}

	// State is the vehicle's damage state as the snapshot publishes it:
	// intact, destroyed, or exploded (destroyed by an explosion).
	static string State(Transport vehicle)
	{
		if (!vehicle.IsDamageDestroyed())
			return STATE_INTACT;
		if (Exploded().Find(vehicle) >= 0)
			return STATE_EXPLODED;
		return STATE_DESTROYED;
	}

	// Health is the vehicle's global health as a whole percent of its
	// maximum: the maximum differs from one vehicle class to the next.
	static int Health(Transport vehicle)
	{
		return (int)Math.Round(vehicle.GetHealth01("", "Health") * 100);
	}

	// Fluids reads the levels a vehicle holds, each a fraction of its tank
	// rounded to two decimals: a car's four, a boat's fuel. A vehicle of
	// another kind has none script can read, and gets null.
	static VyshkaJsonValue Fluids(Transport vehicle)
	{
		CarScript car = CarScript.Cast(vehicle);
		if (car)
		{
			VyshkaJsonValue levels = VyshkaJsonValue.NewObject();
			levels.Set(FLUID_FUEL, Fraction(car.GetFluidFraction(CarFluid.FUEL)));
			levels.Set(FLUID_OIL, Fraction(car.GetFluidFraction(CarFluid.OIL)));
			levels.Set(FLUID_BRAKE, Fraction(car.GetFluidFraction(CarFluid.BRAKE)));
			levels.Set(FLUID_COOLANT, Fraction(car.GetFluidFraction(CarFluid.COOLANT)));
			return levels;
		}
		BoatScript boat = BoatScript.Cast(vehicle);
		if (boat)
		{
			VyshkaJsonValue fuel = VyshkaJsonValue.NewObject();
			fuel.Set(FLUID_FUEL, Fraction(boat.GetFluidFraction(BoatFluid.FUEL)));
			return fuel;
		}
		return null;
	}

	static VyshkaJsonValue Fraction(float value)
	{
		return VyshkaVitals.Number(Math.Round(value * 100) / 100);
	}

	// Target resolves the referenceKey of a vehicle-context action to the
	// vehicle, or explains why it cannot.
	static Transport Target(string referenceKey, out string error)
	{
		if (referenceKey == "")
		{
			error = "a vehicle-context action needs the vehicle's id as referenceKey";
			return null;
		}
		Transport vehicle = Find(referenceKey);
		if (!vehicle)
			error = "no vehicle " + referenceKey + " exists on this server (ids are in the state.vehicles snapshot)";
		return vehicle;
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
	// top, and under data the engine class, its display name, the seat
	// count, the crew, the damage state, the health, and the fluids, at
	// DETAIL_FULL. A snapshot too large for that is made again with less
	// (Capture): DETAIL_COMPACT keeps the class, the state, and a crew that
	// is not empty, and nothing else under data, which is always smaller
	// than an entry was before the state existed (the state's bytes are
	// fewer than the display name, seat count, and empty crew it replaces),
	// so a server whose snapshot fitted then still fits; DETAIL_PLACED keeps
	// the id, the kind, and the position a map needs; DETAIL_ID the id
	// alone, which fits 5000 vehicles (the entry cap) whatever their ids.
	static VyshkaJsonValue Describe(Transport vehicle, int detail)
	{
		VyshkaJsonValue entry = VyshkaJsonValue.NewObject();
		entry.Set("id", VyshkaJsonValue.NewString(Id(vehicle)));
		if (detail >= DETAIL_ID)
			return entry;
		entry.Set("kind", VyshkaJsonValue.NewString(Kind(vehicle)));
		VyshkaJsonValue position = VyshkaPlayers.Position(vehicle.GetPosition());
		if (position)
			entry.Set("position", position);
		if (detail >= DETAIL_PLACED)
			return entry;
		VyshkaJsonValue data = VyshkaJsonValue.NewObject();
		data.Set("type", VyshkaJsonValue.NewString(vehicle.GetType()));
		VyshkaJsonValue crew = Crew(vehicle);
		if (detail >= DETAIL_COMPACT)
		{
			if (crew.Count() > 0)
				data.Set("crew", crew);
			data.Set("state", VyshkaJsonValue.NewString(State(vehicle)));
			entry.Set("data", data);
			return entry;
		}
		data.Set("displayName", VyshkaJsonValue.NewString(vehicle.GetDisplayName()));
		data.Set("seats", VyshkaJsonValue.NewInt(vehicle.CrewSize()));
		data.Set("crew", crew);
		data.Set("state", VyshkaJsonValue.NewString(State(vehicle)));
		data.Set("health", VyshkaJsonValue.NewInt(Health(vehicle)));
		VyshkaJsonValue fluids = Fluids(vehicle);
		if (fluids)
			data.Set("fluids", fluids);
		entry.Set("data", data);
		return entry;
	}

	// Capture builds the state.vehicles body: every vehicle alive right now.
	static VyshkaJsonValue Capture()
	{
		array<Transport> live = Live();
		// A vehicle reported destroyed and since repaired can be destroyed
		// again; one that is gone has nothing left to report.
		array<Transport> reported = Destroyed();
		for (int j = reported.Count() - 1; j >= 0; j--)
		{
			Transport wreck = reported.Get(j);
			if (!wreck || !wreck.IsDamageDestroyed())
				reported.Remove(j);
		}
		Loading().Clear();
		FatalHits().Clear();
		array<Transport> exploded = Exploded();
		for (int k = exploded.Count() - 1; k >= 0; k--)
		{
			Transport blasted = exploded.Get(k);
			if (!blasted || !blasted.IsDamageDestroyed())
				exploded.Remove(k);
		}

		// Every vehicle stays in the snapshot, since one absent from it is
		// gone (section 8.3); detail goes instead, a level at a time.
		VyshkaJsonValue body = Body(live, DETAIL_FULL);
		int bytes = body.Serialize().Length();
		int fullBytes = bytes;
		int detail = DETAIL_FULL;
		while (bytes > SNAPSHOT_BUDGET && detail < DETAIL_ID)
		{
			detail++;
			body = Body(live, detail);
			bytes = body.Serialize().Length();
		}
		if (detail != DETAIL_FULL && detail != s_LoggedDetail)
		{
			// Logged when the level changes, not on every poll.
			string line = "the vehicles snapshot came to " + fullBytes.ToString() + " bytes for " + live.Count().ToString() + " vehicles, past the 260000 kept under the 262144 a snapshot may carry";
			VyshkaLog.Warn(line + "; sent at detail level " + detail.ToString() + " of 3 (" + bytes.ToString() + " bytes)");
		}
		s_LoggedDetail = detail;
		return body;
	}

	static VyshkaJsonValue Body(array<Transport> live, int detail)
	{
		VyshkaJsonValue vehicles = VyshkaJsonValue.NewArray();
		for (int i = 0; i < live.Count(); i++)
			vehicles.Add(Describe(live.Get(i), detail));
		VyshkaJsonValue body = VyshkaJsonValue.NewObject();
		body.Set("capturedAt", VyshkaJsonValue.NewString(VyshkaClock.NowRfc3339()));
		body.Set("vehicles", vehicles);
		return body;
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
	//
	// The hit that destroyed it, when one did (a scripted health write
	// destroys without a hit), came through the hit hook first and says how
	// (measured on DayZ 1.29: the hit hook runs, then the kill hook, in the
	// same damage call): its damage type and ammunition go into the event,
	// and an explosion makes the vehicle exploded rather than destroyed. An
	// explosive named as the killer does too, whichever hook ran first. The
	// engine names the vehicle itself as the killer of an explosion
	// (measured: a plastic explosive's blast), so a vehicle that is its own
	// killer is described by what its fatal hit came from, when it came
	// from something else.
	static void OnDestroyed(Transport vehicle, Object killer)
	{
		if (!vehicle)
			return;
		VyshkaVehicleHit hit = TakeHit(vehicle);
		if (Destroyed().Find(vehicle) >= 0)
			return;
		Destroyed().Insert(vehicle);
		if (Loading().Find(vehicle) >= 0)
			return;
		VyshkaJsonValue data = VyshkaJsonValue.NewObject();
		Label(vehicle, data);
		data.Set("crew", Crew(vehicle));
		Object blamed = killer;
		if (killer == vehicle && hit && hit.m_Source && hit.m_Source != vehicle)
			blamed = hit.m_Source;
		DescribeKiller(vehicle, blamed, data);
		bool exploded = data.GetString("cause") == "explosion";
		if (hit)
		{
			data.Set("damageType", VyshkaJsonValue.NewString(VyshkaPlayers.DamageTypeName(hit.m_Type)));
			if (hit.m_Ammo != "")
				data.Set("ammo", VyshkaJsonValue.NewString(hit.m_Ammo));
			if (hit.m_Type == DamageType.EXPLOSION)
				exploded = true;
		}
		if (exploded && Exploded().Find(vehicle) < 0)
			Exploded().Insert(vehicle);
		data.Set("state", VyshkaJsonValue.NewString(State(vehicle)));
		VyshkaPlugin.Emit("core.vehicle.destroy", data);
	}

	// OnHit runs from the vehicle's hit hook, after the engine has applied
	// the damage. A hit that leaves the vehicle destroyed before its
	// destruction was reported is the one that destroyed it, and is kept for
	// the kill hook the engine runs next (OnDestroyed); every other hit, a
	// hit on a wreck included, is not kept.
	static void OnHit(Transport vehicle, int damageType, EntityAI source, string ammo)
	{
		if (!vehicle || !vehicle.IsDamageDestroyed() || Destroyed().Find(vehicle) >= 0)
			return;
		VyshkaVehicleHit hit = new VyshkaVehicleHit();
		hit.m_Vehicle = vehicle;
		hit.m_Source = source;
		hit.m_Type = damageType;
		hit.m_Ammo = ammo;
		TakeHit(vehicle);
		FatalHits().Insert(hit);
	}

	static array<ref VyshkaVehicleHit> FatalHits()
	{
		if (!s_FatalHits)
			s_FatalHits = new array<ref VyshkaVehicleHit>;
		return s_FatalHits;
	}

	// TakeHit removes the fatal hit kept for a vehicle and returns it, or
	// null when none is kept.
	static VyshkaVehicleHit TakeHit(Transport vehicle)
	{
		array<ref VyshkaVehicleHit> hits = FatalHits();
		for (int i = hits.Count() - 1; i >= 0; i--)
		{
			VyshkaVehicleHit hit = hits.Get(i);
			if (hit.m_Vehicle == vehicle)
			{
				hits.Remove(i);
				return hit;
			}
		}
		return null;
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
// as they are deleted, and report their destruction and the hit behind it;
// the store-load hooks say when a destruction belongs to an earlier run. Every vanilla vehicle
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

	override void EEHitBy(TotalDamageResult damageResult, int damageType, EntityAI source, int component, string dmgZone, string ammo, vector modelPos, float speedCoef)
	{
		super.EEHitBy(damageResult, damageType, source, component, dmgZone, ammo, modelPos, speedCoef);
		if (GetGame().IsServer())
			VyshkaVehicles.OnHit(this, damageType, source, ammo);
	}

	override bool OnStoreLoad(ParamsReadContext ctx, int version)
	{
		if (GetGame().IsServer())
			VyshkaVehicles.OnLoading(this);
		return super.OnStoreLoad(ctx, version);
	}

	override void AfterStoreLoad()
	{
		super.AfterStoreLoad();
		if (GetGame().IsServer())
			VyshkaVehicles.OnLoaded(this);
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

	override void EEHitBy(TotalDamageResult damageResult, int damageType, EntityAI source, int component, string dmgZone, string ammo, vector modelPos, float speedCoef)
	{
		super.EEHitBy(damageResult, damageType, source, component, dmgZone, ammo, modelPos, speedCoef);
		if (GetGame().IsServer())
			VyshkaVehicles.OnHit(this, damageType, source, ammo);
	}

	override bool OnStoreLoad(ParamsReadContext ctx, int version)
	{
		if (GetGame().IsServer())
			VyshkaVehicles.OnLoading(this);
		return super.OnStoreLoad(ctx, version);
	}

	override void AfterStoreLoad()
	{
		super.AfterStoreLoad();
		if (GetGame().IsServer())
			VyshkaVehicles.OnLoaded(this);
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

	override void EEHitBy(TotalDamageResult damageResult, int damageType, EntityAI source, int component, string dmgZone, string ammo, vector modelPos, float speedCoef)
	{
		super.EEHitBy(damageResult, damageType, source, component, dmgZone, ammo, modelPos, speedCoef);
		if (GetGame().IsServer())
			VyshkaVehicles.OnHit(this, damageType, source, ammo);
	}

	override bool OnStoreLoad(ParamsReadContext ctx, int version)
	{
		if (GetGame().IsServer())
			VyshkaVehicles.OnLoading(this);
		return super.OnStoreLoad(ctx, version);
	}

	override void AfterStoreLoad()
	{
		super.AfterStoreLoad();
		if (GetGame().IsServer())
			VyshkaVehicles.OnLoaded(this);
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

// VyshkaRefuelAction sets the fluids of one vehicle to a level: a car's fuel,
// oil, brake fluid, and coolant, a boat's fuel. Each tank named is emptied
// and filled to the fraction of its capacity asked for, so the level is the
// one asked for whatever was in it; the default fills the fuel tank.
class VyshkaRefuelAction : VyshkaAction
{
	override string Code()    { return "vyshka.vehicle.refuel"; }
	override string Name()    { return "Refuel vehicle"; }
	override string Context() { return "vehicle"; }
	override string Danger()  { return "warning"; }

	override VyshkaJsonValue ParamsSchema()
	{
		VyshkaJsonValue names = VyshkaJsonValue.NewArray();
		names.Add(VyshkaJsonValue.NewString(VyshkaVehicles.FLUID_FUEL));
		names.Add(VyshkaJsonValue.NewString(VyshkaVehicles.FLUID_OIL));
		names.Add(VyshkaJsonValue.NewString(VyshkaVehicles.FLUID_BRAKE));
		names.Add(VyshkaJsonValue.NewString(VyshkaVehicles.FLUID_COOLANT));
		VyshkaJsonValue item = VyshkaJsonValue.NewObject();
		item.Set("type", VyshkaJsonValue.NewString("string"));
		item.Set("enum", names);
		VyshkaJsonValue fuelOnly = VyshkaJsonValue.NewArray();
		fuelOnly.Add(VyshkaJsonValue.NewString(VyshkaVehicles.FLUID_FUEL));
		VyshkaJsonValue fluids = VyshkaJsonValue.NewObject();
		fluids.Set("type", VyshkaJsonValue.NewString("array"));
		fluids.Set("items", item);
		fluids.Set("default", fuelOnly);

		VyshkaJsonValue level = VyshkaJsonValue.NewObject();
		level.Set("type", VyshkaJsonValue.NewString("number"));
		level.Set("minimum", VyshkaJsonValue.NewInt(0));
		level.Set("maximum", VyshkaJsonValue.NewInt(1));
		level.Set("default", VyshkaJsonValue.NewInt(1));

		VyshkaJsonValue properties = VyshkaJsonValue.NewObject();
		properties.Set("fluids", fluids);
		properties.Set("level", level);

		VyshkaJsonValue schema = VyshkaJsonValue.NewObject();
		schema.Set("type", VyshkaJsonValue.NewString("object"));
		schema.Set("properties", properties);
		return schema;
	}

	override VyshkaActionOutcome Execute(string actionId, string context, string referenceKey, VyshkaJsonValue params)
	{
		string error;
		Transport vehicle = VyshkaVehicles.Target(referenceKey, error);
		if (!vehicle)
			return VyshkaActionOutcome.Failure(error);

		float level = 1;
		VyshkaJsonValue named = null;
		if (params && params.IsObject())
		{
			level = params.GetFloat("level", 1);
			named = params.Get("fluids");
		}
		if (level < 0 || level > 1)
			return VyshkaActionOutcome.Failure("level must be a fraction of the tank within 0 and 1");
		array<string> wanted = new array<string>;
		if (named && !named.IsNull())
		{
			if (!named.IsArray())
				return VyshkaActionOutcome.Failure("fluids must be a list of fluid names (fuel, oil, brake, coolant)");
			for (int i = 0; i < named.Count(); i++)
			{
				VyshkaJsonValue name = named.At(i);
				if (!name || !name.IsString() || !IsFluid(name.m_Text))
					return VyshkaActionOutcome.Failure("fluids names fuel, oil, brake, and coolant only");
				if (wanted.Find(name.m_Text) < 0)
					wanted.Insert(name.m_Text);
			}
			if (wanted.Count() == 0)
				return VyshkaActionOutcome.Failure("fluids names no fluid to fill");
		}
		else
		{
			wanted.Insert(VyshkaVehicles.FLUID_FUEL);
		}

		CarScript car = CarScript.Cast(vehicle);
		BoatScript boat = BoatScript.Cast(vehicle);
		if (!car && !boat)
			return VyshkaActionOutcome.Failure("a " + VyshkaVehicles.Kind(vehicle) + " holds no fluids the plugin can fill (cars and boats do)");
		if (boat)
		{
			for (int b = 0; b < wanted.Count(); b++)
			{
				if (wanted.Get(b) != VyshkaVehicles.FLUID_FUEL)
					return VyshkaActionOutcome.Failure("a boat has a fuel tank and nothing else; " + wanted.Get(b) + " cannot be filled");
			}
		}

		VyshkaJsonValue before = VyshkaVehicles.Fluids(vehicle);
		for (int f = 0; f < wanted.Count(); f++)
		{
			if (car)
			{
				CarFluid carFluid = CarFluidOf(wanted.Get(f));
				car.LeakAll(carFluid);
				float carAmount = car.GetFluidCapacity(carFluid) * level;
				if (carAmount > 0)
					car.Fill(carFluid, carAmount);
			}
			else
			{
				boat.LeakAll(BoatFluid.FUEL);
				float boatAmount = boat.GetFluidCapacity(BoatFluid.FUEL) * level;
				if (boatAmount > 0)
					boat.Fill(BoatFluid.FUEL, boatAmount);
			}
		}
		VyshkaLog.Info("refuelled " + vehicle.GetType() + " (" + referenceKey + "): " + wanted.Count().ToString() + " fluids to " + VyshkaVitals.Text(level));

		VyshkaJsonValue filled = VyshkaJsonValue.NewArray();
		for (int n = 0; n < wanted.Count(); n++)
			filled.Add(VyshkaJsonValue.NewString(wanted.Get(n)));
		VyshkaJsonValue result = VyshkaJsonValue.NewObject();
		VyshkaVehicles.Label(vehicle, result);
		result.Set("fluids", filled);
		result.Set("level", VyshkaVitals.Number(level));
		result.Set("before", before);
		result.Set("after", VyshkaVehicles.Fluids(vehicle));
		result.Set("state", VyshkaJsonValue.NewString(VyshkaVehicles.State(vehicle)));
		return VyshkaActionOutcome.Success(result);
	}

	static bool IsFluid(string name)
	{
		return name == VyshkaVehicles.FLUID_FUEL || name == VyshkaVehicles.FLUID_OIL || name == VyshkaVehicles.FLUID_BRAKE || name == VyshkaVehicles.FLUID_COOLANT;
	}

	static CarFluid CarFluidOf(string name)
	{
		if (name == VyshkaVehicles.FLUID_OIL)
			return CarFluid.OIL;
		if (name == VyshkaVehicles.FLUID_BRAKE)
			return CarFluid.BRAKE;
		if (name == VyshkaVehicles.FLUID_COOLANT)
			return CarFluid.COOLANT;
		return CarFluid.FUEL;
	}
}

// VyshkaRepairReport is what a repair lists: the wheels swapped back and
// the parts it could not repair, sharing one byte budget so the result stays
// inside the hub's cap (section 7: a result over 64 KiB is dropped whole),
// with complete counts beside them.
class VyshkaRepairReport
{
	ref VyshkaJsonValue m_Replaced;
	ref VyshkaJsonValue m_Problems;
	int m_ReplacedCount;
	int m_ProblemCount;
	int m_Budget;

	void VyshkaRepairReport()
	{
		m_Replaced = VyshkaJsonValue.NewArray();
		m_Problems = VyshkaJsonValue.NewArray();
		m_Budget = VyshkaVehicles.REPORT_BUDGET;
	}

	void Replaced(string slot, string from, string to)
	{
		m_ReplacedCount++;
		VyshkaJsonValue entry = VyshkaJsonValue.NewObject();
		entry.Set("slot", VyshkaJsonValue.NewString(slot));
		entry.Set("from", VyshkaJsonValue.NewString(from));
		entry.Set("to", VyshkaJsonValue.NewString(to));
		Keep(m_Replaced, entry);
	}

	void Problem(string className, string slot, string reason)
	{
		m_ProblemCount++;
		VyshkaJsonValue entry = VyshkaJsonValue.NewObject();
		entry.Set("class", VyshkaJsonValue.NewString(className));
		entry.Set("slot", VyshkaJsonValue.NewString(slot));
		entry.Set("reason", VyshkaJsonValue.NewString(reason));
		Keep(m_Problems, entry);
	}

	void Keep(VyshkaJsonValue list, VyshkaJsonValue entry)
	{
		int bytes = entry.Serialize().Length() + 1;
		if (bytes > m_Budget)
			return;
		list.Add(entry);
		m_Budget -= bytes;
	}

	void Report(VyshkaJsonValue result)
	{
		result.Set("replaced", m_Replaced);
		result.Set("problems", m_Problems);
		result.Set("replacedCount", VyshkaJsonValue.NewInt(m_ReplacedCount));
		result.Set("problemCount", VyshkaJsonValue.NewInt(m_ProblemCount));
		result.Set("truncated", VyshkaJsonValue.NewBool(m_ReplacedCount > m_Replaced.Count() || m_ProblemCount > m_Problems.Count()));
	}
}

// VyshkaRepairAction brings one vehicle back to full health: every damage
// zone of the vehicle through the engine's own full-health call, which also
// lifts a destruction, and with parts (the default) every part attached to
// it and the parts on those. A wheel the engine swapped for its ruined class
// when it was ruined is swapped back through the engine's own replacement,
// the way the engine made the swap. Fluids are the refuel action's business,
// and a missing part stays missing.
class VyshkaRepairAction : VyshkaAction
{
	static const string RUINED_SUFFIX = "_Ruined";

	override string Code()    { return "vyshka.vehicle.repair"; }
	override string Name()    { return "Repair vehicle"; }
	override string Context() { return "vehicle"; }
	override string Danger()  { return "none"; }

	override VyshkaJsonValue ParamsSchema()
	{
		VyshkaJsonValue parts = VyshkaJsonValue.NewObject();
		parts.Set("type", VyshkaJsonValue.NewString("boolean"));
		parts.Set("default", VyshkaJsonValue.NewBool(true));

		VyshkaJsonValue properties = VyshkaJsonValue.NewObject();
		properties.Set("parts", parts);

		VyshkaJsonValue schema = VyshkaJsonValue.NewObject();
		schema.Set("type", VyshkaJsonValue.NewString("object"));
		schema.Set("properties", properties);
		return schema;
	}

	override VyshkaActionOutcome Execute(string actionId, string context, string referenceKey, VyshkaJsonValue params)
	{
		string error;
		Transport vehicle = VyshkaVehicles.Target(referenceKey, error);
		if (!vehicle)
			return VyshkaActionOutcome.Failure(error);
		bool withParts = true;
		if (params && params.IsObject())
			withParts = params.GetBool("parts", true);

		string stateBefore = VyshkaVehicles.State(vehicle);
		int healthBefore = VyshkaVehicles.Health(vehicle);

		// The vehicle first: a ruined wheel's intact class is refused by a
		// vehicle that is still destroyed.
		vehicle.SetFullHealth();
		if (!vehicle.IsDamageDestroyed())
			VyshkaVehicles.OnRepaired(vehicle);

		int repaired = 0;
		VyshkaRepairReport report = new VyshkaRepairReport();
		if (withParts)
			repaired = RepairParts(vehicle, 1, report);

		string stateAfter = VyshkaVehicles.State(vehicle);
		int healthAfter = VyshkaVehicles.Health(vehicle);
		// Built in two steps: one expression this long is past what the
		// script compiler takes ("Formula too complex").
		string line = "repaired " + vehicle.GetType() + " (" + referenceKey + "): " + stateBefore + " at " + healthBefore.ToString() + "%";
		line = line + " to " + stateAfter + " at " + healthAfter.ToString() + "%, " + repaired.ToString() + " parts, " + report.m_ReplacedCount.ToString() + " wheels swapped back";
		VyshkaLog.Info(line);
		if (vehicle.IsDamageDestroyed())
			return VyshkaActionOutcome.Failure("the engine kept " + vehicle.GetType() + " (" + referenceKey + ") destroyed after its full-health call");

		VyshkaJsonValue result = VyshkaJsonValue.NewObject();
		VyshkaVehicles.Label(vehicle, result);
		VyshkaJsonValue before = VyshkaJsonValue.NewObject();
		before.Set("state", VyshkaJsonValue.NewString(stateBefore));
		before.Set("health", VyshkaJsonValue.NewInt(healthBefore));
		VyshkaJsonValue after = VyshkaJsonValue.NewObject();
		after.Set("state", VyshkaJsonValue.NewString(stateAfter));
		after.Set("health", VyshkaJsonValue.NewInt(healthAfter));
		result.Set("before", before);
		result.Set("after", after);
		result.Set("parts", VyshkaJsonValue.NewInt(repaired));
		report.Report(result);
		return VyshkaActionOutcome.Success(result);
	}

	// RepairParts brings every part attached to item, and the parts on
	// those down to PART_DEPTH_MAX levels, to full health, and swaps a
	// ruined wheel back to its intact class; it returns how many parts it
	// repaired or swapped. The parts are listed before any is touched, since
	// a swap changes the attachments; a swapped-in wheel's own parts (which
	// the replacement carries over from the ruined one) are walked like any
	// part's.
	static int RepairParts(EntityAI item, int depth, VyshkaRepairReport report)
	{
		if (depth > VyshkaVehicles.PART_DEPTH_MAX || !item.GetInventory())
			return 0;
		array<EntityAI> parts = new array<EntityAI>;
		for (int i = 0; i < item.GetInventory().AttachmentCount(); i++)
		{
			EntityAI part = item.GetInventory().GetAttachmentFromIndex(i);
			if (part)
				parts.Insert(part);
		}
		int repaired = 0;
		for (int p = 0; p < parts.Count(); p++)
		{
			EntityAI current = parts.Get(p);
			if (CarWheel_Ruined.Cast(current))
			{
				// A wheel that could not be swapped stays where it is (or is
				// gone, if the engine's replacement took it and put nothing
				// back), and its own parts are still walked.
				EntityAI swapped = SwapWheel(item, current, report);
				if (swapped)
				{
					current = swapped;
					repaired++;
				}
				if (!current)
					continue;
			}
			else if (VyshkaInventory.HasHealth(current))
			{
				current.SetFullHealth();
				repaired++;
			}
			repaired += RepairParts(current, depth + 1, report);
		}
		return repaired;
	}

	// SwapWheel replaces a ruined wheel with the intact class its name
	// comes from (HatchbackWheel_Ruined is a ruined HatchbackWheel), in the
	// same slot, through the replacement the engine used to ruin it. A
	// ruined wheel with no intact class to go back to is reported, not
	// removed. Returns the wheel now in the slot, or null when there is no
	// intact one there.
	static EntityAI SwapWheel(EntityAI vehicle, EntityAI wheel, VyshkaRepairReport report)
	{
		string ruinedType = wheel.GetType();
		InventoryLocation location = new InventoryLocation();
		int slotId = InventorySlots.INVALID;
		if (wheel.GetInventory().GetCurrentInventoryLocation(location) && location.GetType() == InventoryLocationType.ATTACHMENT)
			slotId = location.GetSlot();
		string slot = "";
		if (slotId != InventorySlots.INVALID)
			slot = InventorySlots.GetSlotName(slotId);
		string intactType = "";
		int cut = ruinedType.Length() - RUINED_SUFFIX.Length();
		if (cut > 0 && ruinedType.Substring(cut, RUINED_SUFFIX.Length()) == RUINED_SUFFIX)
			intactType = ruinedType.Substring(0, cut);
		if (intactType == "" || slotId == InventorySlots.INVALID || !GetGame().IsKindOf(intactType, "CarWheel") || GetGame().IsKindOf(intactType, "CarWheel_Ruined"))
		{
			report.Problem(ruinedType, slot, "no intact wheel class to swap it back to");
			return null;
		}
		ReplaceWheelLambda lambda = new ReplaceWheelLambda(wheel, intactType, null);
		lambda.SetTransferParams(true, true, false);
		wheel.GetInventory().ReplaceItemWithNew(InventoryMode.SERVER, lambda);

		EntityAI now = vehicle.GetInventory().FindAttachment(slotId);
		if (!now || now.GetType() != intactType)
		{
			report.Problem(ruinedType, slot, "the engine did not put " + intactType + " in its place");
			return null;
		}
		now.SetFullHealth();
		report.Replaced(slot, ruinedType, intactType);
		return now;
	}
}
