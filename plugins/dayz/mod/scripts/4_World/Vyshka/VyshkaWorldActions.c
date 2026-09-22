// Vyshka DayZ plugin: the position and world actions (issue #66).
//
// Teleport (to coordinates, to another player, or back to where the last
// teleport took the player from), spawn one item next to a player, and set
// the world clock. Each is one manifest entry (spec section 6) plus the
// code that runs when the hub dispatches it (section 7). The two
// player-context actions take the player's plain Steam64 id as their
// referenceKey, the same identity the telemetry publishes (section 8.2),
// and need the player online.
//
// Everything here uses what the engine gives every server: SetPosition on
// the character (or on the vehicle it sits in, the way the engine's own
// teleport tooling moves a player), CreateObjectEx with the placement flags
// the central economy uses, and the world's SetDate.

class VyshkaWorld
{
	static const int MAX_CLASS_NAME = 128;
	static const float SPAWN_DISTANCE = 1.5;     // metres in front of the player
	static const float ARRIVE_ASIDE = 1.5;       // metres beside the player teleported to
	static const float ALTITUDE_MAX = 10000.0;   // metres; no map's sky goes higher
	static const string MODE_POSITION = "position";
	static const string MODE_PLAYER = "player";
	static const string MODE_PREVIOUS = "previous";

	// The position each identity was teleported from, one per identity:
	// the next teleport of that identity replaces it, so "previous" after a
	// "previous" swaps back again. In memory only; a restart forgets it.
	static ref map<string, vector> s_Previous;

	static map<string, vector> Previous()
	{
		if (!s_Previous)
			s_Previous = new map<string, vector>;
		return s_Previous;
	}

	static void Reset()
	{
		Previous().Clear();
	}

	// ReadPosition reads a dispatched position: an array of two numbers
	// ([x, z] on the map, placed on the terrain) or three ([x, y, z], the
	// engine's own frame with y the elevation, the same frame the snapshots
	// publish). False with the reason when it is not one.
	static bool ReadPosition(VyshkaJsonValue value, out vector position, out string error)
	{
		if (!value || !value.IsArray() || (value.Count() != 2 && value.Count() != 3))
		{
			error = "position must be [x, y, z] in the game's frame, or [x, z] to be placed on the terrain";
			return false;
		}
		array<float> components = new array<float>;
		for (int i = 0; i < value.Count(); i++)
		{
			VyshkaJsonValue component = value.At(i);
			if (!component || !component.IsNumber())
			{
				error = "position must be [x, y, z] in the game's frame, or [x, z] to be placed on the terrain";
				return false;
			}
			components.Insert(component.m_Number);
		}
		if (components.Count() == 2)
		{
			position[0] = components.Get(0);
			position[2] = components.Get(1);
			position[1] = GetGame().SurfaceY(position[0], position[2]);
		}
		else
		{
			position[0] = components.Get(0);
			position[1] = components.Get(1);
			position[2] = components.Get(2);
		}
		return CheckPosition(position, error);
	}

	// CheckPosition refuses a position off the map or above any sky, and
	// lifts one below the terrain, or below sea level, up to it: measured
	// on DayZ 1.29 (issue #66), the engine leaves a character exactly where
	// an explicit y puts it, 280 m under the ground included, and a
	// character put outside the world is lost, not moved. The engine's own
	// teleport lifts to sea level only; the terrain lift is this plugin's,
	// and it means an interior below the heightmap cannot be a destination.
	static bool CheckPosition(inout vector position, out string error)
	{
		int size = GetGame().GetWorld().GetWorldSize();
		if (position[0] < 0 || position[0] > size || position[2] < 0 || position[2] > size)
		{
			error = "position is outside the map (x and z must lie within 0 and " + size.ToString() + ")";
			return false;
		}
		if (position[1] > ALTITUDE_MAX)
		{
			int altitudeMax = ALTITUDE_MAX;
			error = "position is above the world (y must not exceed " + altitudeMax.ToString() + ")";
			return false;
		}
		float floor = GetGame().SurfaceY(position[0], position[2]);
		float seaLevel = GetGame().SurfaceGetSeaLevel();
		if (seaLevel > floor)
			floor = seaLevel;
		if (position[1] < floor)
			position[1] = floor;
		return true;
	}

	// TeleportRoot is what moves when a player is teleported: the vehicle
	// the player sits in, with everyone else in it, or the character
	// itself. Moving a seated character out of its vehicle would leave the
	// engine's vehicle command pointing at a seat the character is no
	// longer in.
	static Object TeleportRoot(PlayerBase player)
	{
		HumanCommandVehicle inVehicle = player.GetCommand_Vehicle();
		if (inVehicle && inVehicle.GetTransport())
			return inVehicle.GetTransport();
		return player;
	}

	// Move teleports the player to the destination, recording where what
	// moved stood as the identity's previous position. The record is the
	// root's position, not the seated character's: the two differ by the
	// seat's offset from the vehicle, and a previous position saved in one
	// frame and applied in the other would drift by that offset on every
	// undo instead of swapping between two fixed places.
	static vector Move(PlayerBase player, string plainId, vector destination)
	{
		Object root = TeleportRoot(player);
		vector from = root.GetPosition();
		Previous().Set(plainId, from);
		root.SetPosition(destination);
		return from;
	}

	static const string CLASS_NAME_ALPHABET = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_";

	// ValidClassName admits the characters a config class name is made of.
	// Length() and Get() work in bytes, so a multi-byte character fails the
	// alphabet byte by byte, which is the right answer for a class name.
	static bool ValidClassName(string className)
	{
		if (className == "")
			return false;
		for (int i = 0; i < className.Length(); i++)
		{
			if (CLASS_NAME_ALPHABET.IndexOf(className.Get(i)) < 0)
				return false;
		}
		return true;
	}

	// FindConfig names the config class tree that declares className
	// (CfgVehicles, CfgWeapons, or CfgMagazines) and reads its scope, or
	// returns "" when no tree does. Scope 2 is a public class the engine
	// will create; 0 and 1 are the abstract bases items inherit from.
	static string FindConfig(string className, out int scope)
	{
		scope = 0;
		array<string> trees = new array<string>;
		trees.Insert("CfgVehicles");
		trees.Insert("CfgWeapons");
		trees.Insert("CfgMagazines");
		for (int i = 0; i < trees.Count(); i++)
		{
			string path = trees.Get(i) + " " + className;
			if (!GetGame().ConfigIsExisting(path))
				continue;
			scope = GetGame().ConfigGetInt(path + " scope");
			return trees.Get(i);
		}
		return "";
	}

	static VyshkaJsonValue Date()
	{
		int year;
		int month;
		int day;
		int hour;
		int minute;
		GetGame().GetWorld().GetDate(year, month, day, hour, minute);
		VyshkaJsonValue date = VyshkaJsonValue.NewObject();
		date.Set("year", VyshkaJsonValue.NewInt(year));
		date.Set("month", VyshkaJsonValue.NewInt(month));
		date.Set("day", VyshkaJsonValue.NewInt(day));
		date.Set("hour", VyshkaJsonValue.NewInt(hour));
		date.Set("minute", VyshkaJsonValue.NewInt(minute));
		return date;
	}
}

class VyshkaTeleportAction : VyshkaAction
{
	override string Code()    { return "vyshka.teleport"; }
	override string Name()    { return "Teleport player"; }
	override string Context() { return "player"; }
	override string Danger()  { return "warning"; }

	override VyshkaJsonValue ParamsSchema()
	{
		VyshkaJsonValue number = VyshkaJsonValue.NewObject();
		number.Set("type", VyshkaJsonValue.NewString("number"));
		VyshkaJsonValue position = VyshkaJsonValue.NewObject();
		position.Set("type", VyshkaJsonValue.NewString("array"));
		position.Set("items", number);
		position.Set("x-vyshka-widget", VyshkaJsonValue.NewString("vector"));

		VyshkaJsonValue toPlayer = VyshkaJsonValue.NewObject();
		toPlayer.Set("type", VyshkaJsonValue.NewString("string"));
		toPlayer.Set("x-vyshka-widget", VyshkaJsonValue.NewString("player"));

		VyshkaJsonValue previous = VyshkaJsonValue.NewObject();
		previous.Set("type", VyshkaJsonValue.NewString("boolean"));
		previous.Set("default", VyshkaJsonValue.NewBool(false));

		VyshkaJsonValue properties = VyshkaJsonValue.NewObject();
		properties.Set("position", position);
		properties.Set("toPlayer", toPlayer);
		properties.Set("previous", previous);

		VyshkaJsonValue schema = VyshkaJsonValue.NewObject();
		schema.Set("type", VyshkaJsonValue.NewString("object"));
		schema.Set("properties", properties);
		return schema;
	}

	override VyshkaActionOutcome Execute(string actionId, string context, string referenceKey, VyshkaJsonValue params)
	{
		if (referenceKey == "")
			return VyshkaActionOutcome.Failure("a player-context action needs the player's identity as referenceKey");

		// Exactly one destination: the schema subset cannot say so, so the
		// plugin does, before it looks for the player.
		VyshkaJsonValue positionParam = null;
		string toPlayer = "";
		bool previous = false;
		if (params && params.IsObject())
		{
			positionParam = params.Get("position");
			if (positionParam && positionParam.IsNull())
				positionParam = null;
			toPlayer = VyshkaAction.ReadText(params, "toPlayer", 64);
			previous = params.GetBool("previous", false);
		}
		int destinations = 0;
		if (positionParam)
			destinations++;
		if (toPlayer != "")
			destinations++;
		if (previous)
			destinations++;
		if (destinations != 1)
			return VyshkaActionOutcome.Failure("give exactly one destination: position, toPlayer, or previous");

		PlayerBase player = VyshkaHealAction.FindPlayer(referenceKey);
		if (!player)
			return VyshkaActionOutcome.Failure("player " + referenceKey + " is not online");
		string plainId = player.GetIdentity().GetPlainId();

		string mode;
		vector destination;
		string error;
		PlayerBase target = null;
		if (positionParam)
		{
			mode = VyshkaWorld.MODE_POSITION;
			if (!VyshkaWorld.ReadPosition(positionParam, destination, error))
				return VyshkaActionOutcome.Failure(error);
		}
		else if (toPlayer != "")
		{
			mode = VyshkaWorld.MODE_PLAYER;
			target = VyshkaHealAction.FindPlayer(toPlayer);
			if (!target)
				return VyshkaActionOutcome.Failure("player " + toPlayer + " is not online");
			if (target == player)
				return VyshkaActionOutcome.Failure("toPlayer names the player being teleported");
			// Beside the target rather than inside it, at the target's own
			// elevation: the terrain height would put a player standing on
			// a floor under that floor.
			destination = target.GetPosition() + target.GetDirectionAside() * VyshkaWorld.ARRIVE_ASIDE;
			if (!VyshkaWorld.CheckPosition(destination, error))
			{
				// The step aside crossed the map edge: arrive on the target
				// itself, under the same check, so a target somewhere the
				// plugin would refuse to send a player is refused too.
				destination = target.GetPosition();
				if (!VyshkaWorld.CheckPosition(destination, error))
					return VyshkaActionOutcome.Failure("player " + toPlayer + " is somewhere a player cannot be sent: " + error);
			}
		}
		else
		{
			mode = VyshkaWorld.MODE_PREVIOUS;
			if (!VyshkaWorld.Previous().Find(plainId, destination))
				return VyshkaActionOutcome.Failure("player " + plainId + " has no previous position: nothing has teleported them since the server started");
		}

		Object root = VyshkaWorld.TeleportRoot(player);
		vector from = VyshkaWorld.Move(player, plainId, destination);
		VyshkaLog.Info("teleported " + player.GetIdentity().GetName() + " (" + plainId + ") " + mode + " from " + from.ToString() + " to " + destination.ToString());

		VyshkaJsonValue result = VyshkaJsonValue.NewObject();
		result.Set("name", VyshkaJsonValue.NewString(player.GetIdentity().GetName()));
		result.Set("mode", VyshkaJsonValue.NewString(mode));
		VyshkaJsonValue fromJson = VyshkaPlayers.Position(from);
		if (fromJson)
			result.Set("from", fromJson);
		VyshkaJsonValue toJson = VyshkaPlayers.Position(destination);
		if (toJson)
			result.Set("to", toJson);
		if (target)
		{
			result.Set("toPlayer", VyshkaPlayers.Identity(target.GetIdentity().GetPlainId()));
			result.Set("toPlayerName", VyshkaJsonValue.NewString(target.GetIdentity().GetName()));
		}
		if (root != player)
			result.Set("vehicle", VyshkaJsonValue.NewString(root.GetType()));
		return VyshkaActionOutcome.Success(result);
	}
}

class VyshkaSpawnAction : VyshkaAction
{
	override string Code()    { return "vyshka.spawn"; }
	override string Name()    { return "Spawn item"; }
	override string Context() { return "player"; }
	override string Danger()  { return "warning"; }

	override VyshkaJsonValue ParamsSchema()
	{
		VyshkaJsonValue className = VyshkaJsonValue.NewObject();
		className.Set("type", VyshkaJsonValue.NewString("string"));
		className.Set("x-vyshka-widget", VyshkaJsonValue.NewString("itemlist"));
		// The catalog's contexts (spec section 6.1): a panel offers their
		// entries as the names to pick from, and still sends what is typed.
		className.Set("context", VyshkaCatalog.ContextIds());

		VyshkaJsonValue properties = VyshkaJsonValue.NewObject();
		properties.Set("className", className);

		VyshkaJsonValue required = VyshkaJsonValue.NewArray();
		required.Add(VyshkaJsonValue.NewString("className"));

		VyshkaJsonValue schema = VyshkaJsonValue.NewObject();
		schema.Set("type", VyshkaJsonValue.NewString("object"));
		schema.Set("required", required);
		schema.Set("properties", properties);
		return schema;
	}

	override VyshkaActionOutcome Execute(string actionId, string context, string referenceKey, VyshkaJsonValue params)
	{
		if (referenceKey == "")
			return VyshkaActionOutcome.Failure("a player-context action needs the player's identity as referenceKey");
		string className = VyshkaAction.ReadText(params, "className", VyshkaWorld.MAX_CLASS_NAME);
		if (className == "")
			return VyshkaActionOutcome.Failure("className is required and must not be blank");
		if (!VyshkaWorld.ValidClassName(className))
			return VyshkaActionOutcome.Failure("className may contain only letters, digits, and underscores");
		int scope;
		string tree = VyshkaWorld.FindConfig(className, scope);
		if (tree == "")
			return VyshkaActionOutcome.Failure("no item class named " + className + " exists on this server");
		if (scope != 2)
			return VyshkaActionOutcome.Failure(className + " is a base class, not an item the engine will create");

		PlayerBase player = VyshkaHealAction.FindPlayer(referenceKey);
		if (!player)
			return VyshkaActionOutcome.Failure("player " + referenceKey + " is not online");

		vector position = player.GetPosition() + player.GetDirection() * VyshkaWorld.SPAWN_DISTANCE;
		position[1] = GetGame().SurfaceY(position[0], position[2]);
		Object created = GetGame().CreateObjectEx(className, position, ECE_PLACE_ON_SURFACE);
		if (!created)
			return VyshkaActionOutcome.Failure("the engine refused to create " + className);
		VyshkaLog.Info("spawned " + created.GetType() + " for " + player.GetIdentity().GetName() + " (" + player.GetIdentity().GetPlainId() + ") at " + created.GetPosition().ToString());

		VyshkaJsonValue result = VyshkaJsonValue.NewObject();
		result.Set("className", VyshkaJsonValue.NewString(created.GetType()));
		result.Set("displayName", VyshkaJsonValue.NewString(created.GetDisplayName()));
		result.Set("config", VyshkaJsonValue.NewString(tree));
		VyshkaJsonValue where = VyshkaPlayers.Position(created.GetPosition());
		if (where)
			result.Set("position", where);
		result.Set("name", VyshkaJsonValue.NewString(player.GetIdentity().GetName()));
		return VyshkaActionOutcome.Success(result);
	}
}

class VyshkaSetTimeAction : VyshkaAction
{
	static const int HOUR_MAX = 23;
	static const int MINUTE_MAX = 59;

	override string Code()    { return "vyshka.settime"; }
	override string Name()    { return "Set time of day"; }
	override string Context() { return "world"; }
	override string Danger()  { return "warning"; }

	override VyshkaJsonValue ParamsSchema()
	{
		VyshkaJsonValue hour = VyshkaJsonValue.NewObject();
		hour.Set("type", VyshkaJsonValue.NewString("integer"));
		hour.Set("minimum", VyshkaJsonValue.NewInt(0));
		hour.Set("maximum", VyshkaJsonValue.NewInt(HOUR_MAX));

		VyshkaJsonValue minute = VyshkaJsonValue.NewObject();
		minute.Set("type", VyshkaJsonValue.NewString("integer"));
		minute.Set("minimum", VyshkaJsonValue.NewInt(0));
		minute.Set("maximum", VyshkaJsonValue.NewInt(MINUTE_MAX));
		minute.Set("default", VyshkaJsonValue.NewInt(0));

		VyshkaJsonValue properties = VyshkaJsonValue.NewObject();
		properties.Set("hour", hour);
		properties.Set("minute", minute);

		VyshkaJsonValue required = VyshkaJsonValue.NewArray();
		required.Add(VyshkaJsonValue.NewString("hour"));

		VyshkaJsonValue schema = VyshkaJsonValue.NewObject();
		schema.Set("type", VyshkaJsonValue.NewString("object"));
		schema.Set("required", required);
		schema.Set("properties", properties);
		return schema;
	}

	override VyshkaActionOutcome Execute(string actionId, string context, string referenceKey, VyshkaJsonValue params)
	{
		if (!params || !params.IsObject() || !params.Get("hour") || !params.Get("hour").IsNumber())
			return VyshkaActionOutcome.Failure("hour is required");
		int hour = params.GetInt("hour", -1);
		int minute = params.GetInt("minute", 0);
		int hourMax = HOUR_MAX;
		int minuteMax = MINUTE_MAX;
		if (hour < 0 || hour > hourMax)
			return VyshkaActionOutcome.Failure("hour must lie within 0 and " + hourMax.ToString());
		if (minute < 0 || minute > minuteMax)
			return VyshkaActionOutcome.Failure("minute must lie within 0 and " + minuteMax.ToString());

		VyshkaJsonValue before = VyshkaWorld.Date();
		int year;
		int month;
		int day;
		int oldHour;
		int oldMinute;
		World world = GetGame().GetWorld();
		world.GetDate(year, month, day, oldHour, oldMinute);
		world.SetDate(year, month, day, hour, minute);
		VyshkaJsonValue after = VyshkaWorld.Date();
		VyshkaLog.Info("world time set to " + hour.ToString() + ":" + minute.ToString() + " (was " + oldHour.ToString() + ":" + oldMinute.ToString() + ")");

		VyshkaJsonValue result = VyshkaJsonValue.NewObject();
		result.Set("before", before);
		result.Set("after", after);
		return VyshkaActionOutcome.Success(result);
	}
}
