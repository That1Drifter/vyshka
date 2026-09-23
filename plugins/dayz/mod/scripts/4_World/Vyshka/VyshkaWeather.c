// Vyshka DayZ plugin: the world (issue #78).
//
// The state.world snapshot (spec section 8.3): the game's clock and the
// weather as the engine reports them, with what this plugin has set beside
// them (whether the clock is stopped, and the weather's behaviour mode). And
// the actions that change them: one weather action with every engine knob an
// optional param, three fixed presets shipped here (clear, cloudy, storm)
// and any number an operator keeps in the key/value store under
// vyshka.weather, applied through the same action; and freeze time.
//
// Measured on DayZ 1.29 (spikes/dayz-world-clock):
// - World.SetTimeMultiplier(0) stops the server's clock, -1 gives it back to
//   the server config's rate, and a date set while it is stopped holds.
// - A weather phenomenon set with a hold time keeps its value that long. The
//   map's own controller (the world's WorldData) then chooses the next
//   forecast and moves on, re-applying its own storm, rain thresholds, wind
//   maximum, and snowfall limits as it does (Chernarus closes the snowfall
//   limits every time). That is the `world` mode.
// - With the engine's own weather in charge (Weather.MissionWeather) the map's
//   controller is skipped and the engine picks random forecasts within the
//   limits. That is the `engine` mode.
// - With the weather update frozen nothing chooses a new forecast, the wind
//   included, and every set value holds; except that rain still stops, over
//   the rain threshold's stop time, when the overcast is outside the rain
//   threshold. That is the `hold` mode.
// - A transition interpolates linearly over its seconds, and the engine
//   clamps a phenomenon to its limits, so a value outside them is let in by
//   widening them.
//
// None of it survives a restart: the engine starts the clock at the server
// config's rate and the weather under the map's controller, and so does this
// plugin's record of them, so the snapshot tells the truth after a restart.
// The one thing the engine cannot be asked is whether its clock was stopped,
// so timeFrozen is what this plugin set, and a mod that stops the clock
// itself is not seen.

class VyshkaWeather
{
	static const string NS_WEATHER = "vyshka.weather";

	static const string MODE_WORLD = "world";
	static const string MODE_ENGINE = "engine";
	static const string MODE_HOLD = "hold";

	static const string PRESET_CLEAR = "clear";
	static const string PRESET_CLOUDY = "cloudy";
	static const string PRESET_STORM = "storm";

	// Bounds on the knobs. The stock maps cap the wind at 20 m/s; the bound
	// leaves room above that without admitting a typo's thousand.
	static const float WIND_SPEED_MAX = 100.0;
	static const float SECONDS_MAX = 86400.0;
	static const float FOG_BIAS_MIN = -1000.0;
	static const float FOG_BIAS_MAX = 10000.0;
	// How long a set value stands, in the world and engine modes, before the
	// controller may choose the next one, when the dispatch does not say.
	static const float HOLD_DEFAULT = 1800.0;
	// The engine's defaults (scripts/3_game/weather.c) for what this plugin
	// cannot read back: the storm's least seconds between strikes, and a
	// threshold's minimum overcast and stop time.
	static const float STORM_TIMEOUT_DEFAULT = 45.0;
	static const float THRESHOLD_MIN_DEFAULT = 0.6;
	static const float THRESHOLD_STOP_DEFAULT = 30.0;

	// What this plugin set that the engine cannot be asked about: whether it
	// stopped the clock, and the rain and snowfall thresholds it last set,
	// for the notes of a later dispatch. The engine has no getter for a
	// threshold, and the map's controller or a mod may replace one at any
	// time between dispatches, so a threshold set by an earlier dispatch is
	// only ever named in a note as the one last set here, never as the one
	// in force.
	static bool s_TimeFrozen;
	static bool s_RainSet;
	static float s_RainMin;
	static float s_RainMax;
	static bool s_SnowfallSet;
	static float s_SnowfallMin;
	static float s_SnowfallMax;

	static void Reset()
	{
		s_TimeFrozen = false;
		s_RainSet = false;
		s_SnowfallSet = false;
	}

	// Register declares the weather presets' namespace and the two actions.
	static void Register(VyshkaRegistry registry)
	{
		registry.DeclareNamespace(NS_WEATHER);
		registry.Register(new VyshkaWeatherAction());
		registry.Register(new VyshkaTimeFreezeAction());
	}

	// Mode reads the weather's behaviour from the engine, so a mod that took
	// the weather over itself is reported as it is, in the order the engine
	// asks (WeatherPhenomenon.OnBeforeChange): the mission's own weather
	// first, which wins over a frozen update when a mod sets both.
	static string Mode()
	{
		Weather weather = GetGame().GetWeather();
		if (weather.GetMissionWeather())
			return MODE_ENGINE;
		if (weather.GetWeatherUpdateFrozen())
			return MODE_HOLD;
		return MODE_WORLD;
	}

	static void SetMode(string mode)
	{
		Weather weather = GetGame().GetWeather();
		weather.SetWeatherUpdateFreeze(mode == MODE_HOLD);
		weather.MissionWeather(mode == MODE_ENGINE);
	}

	static void SetTimeFrozen(bool frozen)
	{
		if (frozen)
			GetGame().GetWorld().SetTimeMultiplier(0);
		else
			GetGame().GetWorld().SetTimeMultiplier(-1);
		s_TimeFrozen = frozen;
	}

	static string Pad4(int value)
	{
		string text = value.ToString();
		while (text.Length() < 4)
			text = "0" + text;
		return text;
	}

	static bool LeapYear(int year)
	{
		if (year % 400 == 0)
			return true;
		if (year % 100 == 0)
			return false;
		return year % 4 == 0;
	}

	static int DaysInMonth(int year, int month)
	{
		if (month == 2)
		{
			if (LeapYear(year))
				return 29;
			return 28;
		}
		if (month == 4 || month == 6 || month == 9 || month == 11)
			return 30;
		return 31;
	}

	// Time is the game's clock in the form section 8.3 names
	// (YYYY-MM-DDTHH:MM, no offset), or "" when the engine's date is not one
	// a calendar has, so the snapshot leaves it out rather than being
	// refused over it. The year is copied into a local first: an
	// int.ToString() still pending in an expression takes the value of one
	// run inside a function it calls (VyshkaClock).
	static string Time()
	{
		int year;
		int month;
		int day;
		int hour;
		int minute;
		GetGame().GetWorld().GetDate(year, month, day, hour, minute);
		if (year < 0 || year > 9999 || month < 1 || month > 12 || day < 1 || day > DaysInMonth(year, month))
			return "";
		if (hour < 0 || hour > 23 || minute < 0 || minute > 59)
			return "";
		string y = Pad4(year);
		return y + "-" + VyshkaClock.Pad2(month) + "-" + VyshkaClock.Pad2(day) + "T" + VyshkaClock.Pad2(hour) + ":" + VyshkaClock.Pad2(minute);
	}

	static int Degrees(float radians)
	{
		return (int)Math.Round(radians * Math.RAD2DEG);
	}

	// Conditions is the world's data member: the clock's state and the
	// weather as it stands. The phenomena are their actual values (0 to 1),
	// the wind in metres a second and degrees of the engine's wind angle,
	// the dynamic fog only where the world's config enables it.
	static VyshkaJsonValue Conditions()
	{
		Weather weather = GetGame().GetWeather();
		VyshkaJsonValue data = VyshkaJsonValue.NewObject();
		data.Set("timeFrozen", VyshkaJsonValue.NewBool(s_TimeFrozen));
		data.Set("night", VyshkaJsonValue.NewBool(GetGame().GetWorld().IsNight()));
		data.Set("weatherMode", VyshkaJsonValue.NewString(Mode()));
		data.Set("overcast", VyshkaVitals.Number(weather.GetOvercast().GetActual()));
		data.Set("fog", VyshkaVitals.Number(weather.GetFog().GetActual()));
		data.Set("rain", VyshkaVitals.Number(weather.GetRain().GetActual()));
		data.Set("snowfall", VyshkaVitals.Number(weather.GetSnowfall().GetActual()));
		data.Set("windSpeed", VyshkaVitals.Number(weather.GetWindSpeed()));
		data.Set("windDirection", VyshkaJsonValue.NewInt(Degrees(weather.GetWindDirection().GetActual())));
		data.Set("windMaxSpeed", VyshkaVitals.Number(weather.GetWindMaximumSpeed()));
		if (weather.IsDynVolFogEnabled())
		{
			VyshkaJsonValue dynamicFog = VyshkaJsonValue.NewObject();
			dynamicFog.Set("distanceDensity", VyshkaVitals.Number(weather.GetDynVolFogDistanceDensity()));
			dynamicFog.Set("heightDensity", VyshkaVitals.Number(weather.GetDynVolFogHeightDensity()));
			dynamicFog.Set("heightBias", VyshkaVitals.Number(weather.GetDynVolFogHeightBias()));
			data.Set("dynamicFog", dynamicFog);
		}
		return data;
	}

	// Capture is the state.world body.
	static VyshkaJsonValue Capture()
	{
		VyshkaJsonValue world = VyshkaJsonValue.NewObject();
		string time = Time();
		if (time != "")
			world.Set("time", VyshkaJsonValue.NewString(time));
		world.Set("data", Conditions());
		VyshkaJsonValue body = VyshkaJsonValue.NewObject();
		body.Set("capturedAt", VyshkaJsonValue.NewString(VyshkaClock.NowRfc3339()));
		body.Set("world", world);
		return body;
	}

	// Preset fills knobs with one of the fixed presets; false for a name
	// that is not one.
	static bool Preset(string name, VyshkaWeatherKnobs knobs)
	{
		if (name == PRESET_CLEAR)
		{
			knobs.Phenomena(0.05, 0, 0, 0);
			knobs.SetWindSpeed(2);
			return true;
		}
		if (name == PRESET_CLOUDY)
		{
			// Below the engine's default rain threshold (0.6), so it does
			// not rain.
			knobs.Phenomena(0.55, 0.05, 0, 0);
			knobs.SetWindSpeed(6);
			return true;
		}
		if (name == PRESET_STORM)
		{
			knobs.Phenomena(1, 0.1, 1, 0);
			knobs.SetWindSpeed(15);
			knobs.SetStorm(1, 0.8, 20);
			return true;
		}
		return false;
	}

	// SetPhenomenon sets one phenomenon, widening its limits first when the
	// value lies outside them (the engine would clamp it otherwise).
	static void SetPhenomenon(WeatherPhenomenon phenomenon, string name, float value, float transition, float hold, VyshkaJsonValue applied, VyshkaJsonValue widened)
	{
		float low;
		float high;
		phenomenon.GetLimits(low, high);
		if (value < low || value > high)
		{
			if (value < low)
				low = value;
			if (value > high)
				high = value;
			phenomenon.SetLimits(low, high);
			widened.Add(VyshkaJsonValue.NewString(name));
		}
		phenomenon.Set(value, transition, hold);
		applied.Add(VyshkaJsonValue.NewString(name));
	}

	// HoldBack makes a phenomenon's next forecast come no sooner than
	// seconds from now.
	static void HoldBack(WeatherPhenomenon phenomenon, float seconds)
	{
		if (phenomenon.GetNextChange() < seconds)
			phenomenon.SetNextChange(seconds);
	}

	// EarliestForecast is the seconds until the first of the six phenomena is
	// due a new forecast, which in world mode is when the map's controller
	// next runs.
	static float EarliestForecast(Weather weather)
	{
		float due = weather.GetOvercast().GetNextChange();
		due = Math.Min(due, weather.GetFog().GetNextChange());
		due = Math.Min(due, weather.GetRain().GetNextChange());
		due = Math.Min(due, weather.GetSnowfall().GetNextChange());
		due = Math.Min(due, weather.GetWindMagnitude().GetNextChange());
		due = Math.Min(due, weather.GetWindDirection().GetNextChange());
		if (due < 0)
			return 0;
		return due;
	}

	// ThresholdNote says when the overcast being moved to lies outside the
	// rain or snowfall threshold, where the engine stops that fall over the
	// threshold's stop time. It judges by the threshold this dispatch set,
	// which is in force; else by the one an earlier dispatch last set, or the
	// engine's default, each said with the caveat that the map or a mod may
	// have set another since, because the engine cannot be asked.
	static void ThresholdNote(VyshkaJsonValue notes, string fall, float overcast, bool setNow, bool setBefore, float setLow, float setHigh)
	{
		float low = setLow;
		float high = setHigh;
		string source = "the " + fall + " threshold this dispatch set";
		string caveat = "";
		if (!setNow && setBefore)
		{
			source = "the " + fall + " threshold last set here";
			caveat = " (unless the map or a mod has set another since)";
		}
		else if (!setNow)
		{
			low = THRESHOLD_MIN_DEFAULT;
			high = 1;
			source = "the engine's default " + fall + " threshold";
			caveat = " (unless the map or a mod set another)";
		}
		if (overcast >= low && overcast <= high)
			return;
		string range = VyshkaJsonValue.FormatFloat(low) + " to " + VyshkaJsonValue.FormatFloat(high);
		notes.Add(VyshkaJsonValue.NewString("the overcast (" + VyshkaJsonValue.FormatFloat(overcast) + ") is outside " + source + ", " + range + caveat + ", so the engine stops the " + fall + " over the threshold's stop time"));
	}

	// Apply sets what knobs holds, in an order where nothing set is undone
	// by what follows: thresholds, storm, and the wind's maximum first, then
	// the phenomena, the dynamic fog, and the mode last.
	static VyshkaActionOutcome Apply(VyshkaWeatherKnobs knobs)
	{
		if (knobs.Empty())
			return VyshkaActionOutcome.Failure("nothing to set: give a preset, a stored weather name, a knob, or a mode");
		Weather weather = GetGame().GetWeather();
		if (knobs.HasDynamicFog() && !weather.IsDynVolFogEnabled())
			return VyshkaActionOutcome.Failure("dynamicFog: this world's config does not enable dynamic volumetric fog, so the engine would ignore it");

		VyshkaJsonValue before = Conditions();
		VyshkaJsonValue applied = VyshkaJsonValue.NewArray();
		VyshkaJsonValue widened = VyshkaJsonValue.NewArray();
		VyshkaJsonValue notes = VyshkaJsonValue.NewArray();
		float transition = 0;
		if (knobs.m_HasTransition)
			transition = knobs.m_Transition;
		float hold = HOLD_DEFAULT;
		if (knobs.m_HasHold)
			hold = knobs.m_Hold;

		if (knobs.m_HasRainThreshold)
		{
			weather.SetRainThresholds(knobs.m_RainMin, knobs.m_RainMax, knobs.m_RainStop);
			s_RainSet = true;
			s_RainMin = knobs.m_RainMin;
			s_RainMax = knobs.m_RainMax;
			applied.Add(VyshkaJsonValue.NewString("rainThreshold"));
		}
		if (knobs.m_HasSnowfallThreshold)
		{
			weather.SetSnowfallThresholds(knobs.m_SnowfallMin, knobs.m_SnowfallMax, knobs.m_SnowfallStop);
			s_SnowfallSet = true;
			s_SnowfallMin = knobs.m_SnowfallMin;
			s_SnowfallMax = knobs.m_SnowfallMax;
			applied.Add(VyshkaJsonValue.NewString("snowfallThreshold"));
		}
		if (knobs.m_HasStorm)
		{
			weather.SetStorm(knobs.m_StormDensity, knobs.m_StormThreshold, knobs.m_StormTimeout);
			applied.Add(VyshkaJsonValue.NewString("storm"));
		}
		if (knobs.m_HasWindMax)
		{
			weather.SetWindMaximumSpeed(knobs.m_WindMax);
			applied.Add(VyshkaJsonValue.NewString("windMaxSpeed"));
		}
		// The wind's magnitude and direction are phenomena with limits of
		// their own like the rest, the magnitude's upper limit being the
		// wind's maximum speed, so they are widened the same way.
		if (knobs.m_HasWindSpeed)
			SetPhenomenon(weather.GetWindMagnitude(), "windSpeed", knobs.m_WindSpeed, transition, hold, applied, widened);
		if (knobs.m_HasWindDirection)
			SetPhenomenon(weather.GetWindDirection(), "windDirection", knobs.m_WindDirection * Math.DEG2RAD, transition, hold, applied, widened);
		if (knobs.m_HasOvercast)
			SetPhenomenon(weather.GetOvercast(), "overcast", knobs.m_Overcast, transition, hold, applied, widened);
		if (knobs.m_HasFog)
			SetPhenomenon(weather.GetFog(), "fog", knobs.m_Fog, transition, hold, applied, widened);
		if (knobs.m_HasRain)
			SetPhenomenon(weather.GetRain(), "rain", knobs.m_Rain, transition, hold, applied, widened);
		if (knobs.m_HasSnowfall)
			SetPhenomenon(weather.GetSnowfall(), "snowfall", knobs.m_Snowfall, transition, hold, applied, widened);
		if (knobs.m_HasFogDistance)
		{
			weather.SetDynVolFogDistanceDensity(knobs.m_FogDistance, transition);
			applied.Add(VyshkaJsonValue.NewString("dynamicFog.distanceDensity"));
		}
		if (knobs.m_HasFogHeight)
		{
			weather.SetDynVolFogHeightDensity(knobs.m_FogHeight, transition);
			applied.Add(VyshkaJsonValue.NewString("dynamicFog.heightDensity"));
		}
		if (knobs.m_HasFogBias)
		{
			weather.SetDynVolFogHeightBias(knobs.m_FogBias, transition);
			applied.Add(VyshkaJsonValue.NewString("dynamicFog.heightBias"));
		}
		if (knobs.m_Mode != "")
		{
			SetMode(knobs.m_Mode);
			applied.Add(VyshkaJsonValue.NewString("mode"));
		}

		string mode = Mode();
		if (mode == MODE_WORLD)
		{
			// The map's controller runs whenever any phenomenon is due a new
			// forecast, and each run re-applies its storm, thresholds, wind
			// maximum, and snowfall limits, so one phenomenon falling due
			// would undo what was set on another. Every phenomenon's next
			// change is held back to the hold, which is then what the note
			// says it is.
			HoldBack(weather.GetOvercast(), hold);
			HoldBack(weather.GetFog(), hold);
			HoldBack(weather.GetRain(), hold);
			HoldBack(weather.GetSnowfall(), hold);
			HoldBack(weather.GetWindMagnitude(), hold);
			HoldBack(weather.GetWindDirection(), hold);
			// The hold is a floor: an earlier dispatch may have held the
			// phenomena longer, and nothing shortens them, so the note says
			// when the first of them actually falls due.
			float due = EarliestForecast(weather);
			int dueSeconds = Math.Ceil(due);
			notes.Add(VyshkaJsonValue.NewString("world mode: the map's own weather takes over in " + dueSeconds.ToString() + " s, when the first phenomenon falls due, and re-applies its storm, thresholds, wind maximum, and snowfall limits; use mode hold to keep these values"));
		}

		// What the engine will do to the values just set, where it can be
		// told from here. The overcast is the one being moved to.
		float overcast = weather.GetOvercast().GetForecast();
		if (knobs.m_HasRain && knobs.m_Rain > 0)
			ThresholdNote(notes, "rain", overcast, knobs.m_HasRainThreshold, s_RainSet, s_RainMin, s_RainMax);
		if (knobs.m_HasSnowfall && knobs.m_Snowfall > 0)
			ThresholdNote(notes, "snowfall", overcast, knobs.m_HasSnowfallThreshold, s_SnowfallSet, s_SnowfallMin, s_SnowfallMax);

		string appliedText = applied.Serialize();
		VyshkaLog.Info("weather set: " + appliedText + ", mode " + mode);

		VyshkaJsonValue result = VyshkaJsonValue.NewObject();
		result.Set("applied", applied);
		result.Set("mode", VyshkaJsonValue.NewString(mode));
		result.Set("transitionSeconds", VyshkaVitals.Number(transition));
		result.Set("holdSeconds", VyshkaVitals.Number(hold));
		if (widened.Count() > 0)
			result.Set("widened", widened);
		if (notes.Count() > 0)
			result.Set("notes", notes);
		result.Set("before", before);
		result.Set("after", Conditions());
		return VyshkaActionOutcome.Success(result);
	}

	// Schema helpers for the weather action's params.
	static VyshkaJsonValue NumberSchema(float min, float max)
	{
		VyshkaJsonValue schema = VyshkaJsonValue.NewObject();
		schema.Set("type", VyshkaJsonValue.NewString("number"));
		schema.Set("minimum", VyshkaVitals.Number(min));
		schema.Set("maximum", VyshkaVitals.Number(max));
		return schema;
	}

	static VyshkaJsonValue ObjectSchema(VyshkaJsonValue properties, VyshkaJsonValue required)
	{
		VyshkaJsonValue schema = VyshkaJsonValue.NewObject();
		schema.Set("type", VyshkaJsonValue.NewString("object"));
		if (required)
			schema.Set("required", required);
		schema.Set("properties", properties);
		return schema;
	}

	static VyshkaJsonValue ThresholdSchema()
	{
		VyshkaJsonValue properties = VyshkaJsonValue.NewObject();
		properties.Set("min", NumberSchema(0, 1));
		properties.Set("max", NumberSchema(0, 1));
		properties.Set("stopSeconds", NumberSchema(0, SECONDS_MAX));
		VyshkaJsonValue required = VyshkaJsonValue.NewArray();
		required.Add(VyshkaJsonValue.NewString("min"));
		required.Add(VyshkaJsonValue.NewString("max"));
		return ObjectSchema(properties, required);
	}

	static VyshkaJsonValue EnumSchema(array<string> values)
	{
		VyshkaJsonValue names = VyshkaJsonValue.NewArray();
		for (int i = 0; i < values.Count(); i++)
			names.Add(VyshkaJsonValue.NewString(values.Get(i)));
		VyshkaJsonValue schema = VyshkaJsonValue.NewObject();
		schema.Set("type", VyshkaJsonValue.NewString("string"));
		schema.Set("enum", names);
		return schema;
	}

	// KnobsSchema is every knob, the members a stored preset may carry too.
	static void KnobsSchema(VyshkaJsonValue properties)
	{
		properties.Set("overcast", NumberSchema(0, 1));
		properties.Set("fog", NumberSchema(0, 1));
		properties.Set("rain", NumberSchema(0, 1));
		properties.Set("snowfall", NumberSchema(0, 1));
		properties.Set("windSpeed", NumberSchema(0, WIND_SPEED_MAX));
		properties.Set("windDirection", NumberSchema(-180, 180));
		properties.Set("windMaxSpeed", NumberSchema(0, WIND_SPEED_MAX));

		VyshkaJsonValue fogProperties = VyshkaJsonValue.NewObject();
		fogProperties.Set("distanceDensity", NumberSchema(0, 1));
		fogProperties.Set("heightDensity", NumberSchema(0, 1));
		fogProperties.Set("heightBias", NumberSchema(FOG_BIAS_MIN, FOG_BIAS_MAX));
		properties.Set("dynamicFog", ObjectSchema(fogProperties, null));

		VyshkaJsonValue stormProperties = VyshkaJsonValue.NewObject();
		stormProperties.Set("density", NumberSchema(0, 1));
		stormProperties.Set("threshold", NumberSchema(0, 1));
		stormProperties.Set("timeoutSeconds", NumberSchema(0, SECONDS_MAX));
		VyshkaJsonValue stormRequired = VyshkaJsonValue.NewArray();
		stormRequired.Add(VyshkaJsonValue.NewString("density"));
		stormRequired.Add(VyshkaJsonValue.NewString("threshold"));
		properties.Set("storm", ObjectSchema(stormProperties, stormRequired));

		properties.Set("rainThreshold", ThresholdSchema());
		properties.Set("snowfallThreshold", ThresholdSchema());
		properties.Set("transitionSeconds", NumberSchema(0, SECONDS_MAX));
		properties.Set("holdSeconds", NumberSchema(0, SECONDS_MAX));

		array<string> modes = new array<string>;
		modes.Insert(MODE_WORLD);
		modes.Insert(MODE_ENGINE);
		modes.Insert(MODE_HOLD);
		properties.Set("mode", EnumSchema(modes));
	}
}

// VyshkaWeatherKnobs is one set of weather knobs, read from a dispatch's
// params or from a stored preset, each with whether it was given. A later
// Read over the same set overrides what an earlier one gave, which is how a
// dispatch's own knobs win over its preset's.
class VyshkaWeatherKnobs
{
	bool m_HasOvercast;
	float m_Overcast;
	bool m_HasFog;
	float m_Fog;
	bool m_HasRain;
	float m_Rain;
	bool m_HasSnowfall;
	float m_Snowfall;
	bool m_HasWindSpeed;
	float m_WindSpeed;
	bool m_HasWindDirection;
	float m_WindDirection;   // degrees
	bool m_HasWindMax;
	float m_WindMax;
	bool m_HasFogDistance;
	float m_FogDistance;
	bool m_HasFogHeight;
	float m_FogHeight;
	bool m_HasFogBias;
	float m_FogBias;
	bool m_HasStorm;
	float m_StormDensity;
	float m_StormThreshold;
	float m_StormTimeout;
	bool m_HasRainThreshold;
	float m_RainMin;
	float m_RainMax;
	float m_RainStop;
	bool m_HasSnowfallThreshold;
	float m_SnowfallMin;
	float m_SnowfallMax;
	float m_SnowfallStop;
	bool m_HasTransition;
	float m_Transition;
	bool m_HasHold;
	float m_Hold;
	string m_Mode;

	void Phenomena(float overcast, float fog, float rain, float snowfall)
	{
		m_HasOvercast = true;
		m_Overcast = overcast;
		m_HasFog = true;
		m_Fog = fog;
		m_HasRain = true;
		m_Rain = rain;
		m_HasSnowfall = true;
		m_Snowfall = snowfall;
	}

	void SetWindSpeed(float speed)
	{
		m_HasWindSpeed = true;
		m_WindSpeed = speed;
	}

	void SetStorm(float density, float threshold, float timeout)
	{
		m_HasStorm = true;
		m_StormDensity = density;
		m_StormThreshold = threshold;
		m_StormTimeout = timeout;
	}

	bool HasDynamicFog()
	{
		return m_HasFogDistance || m_HasFogHeight || m_HasFogBias;
	}

	bool Empty()
	{
		if (m_HasOvercast || m_HasFog || m_HasRain || m_HasSnowfall || m_HasWindSpeed || m_HasWindDirection || m_HasWindMax)
			return false;
		if (HasDynamicFog() || m_HasStorm || m_HasRainThreshold || m_HasSnowfallThreshold)
			return false;
		return m_Mode == "";
	}

	// Number reads one number member within min and max: 0 when it is
	// absent or null, 1 when read, -1 (with the reason) when it is not a
	// number in range.
	static int Number(VyshkaJsonValue source, string key, string path, float min, float max, out float value, out string error)
	{
		VyshkaJsonValue member = source.Get(key);
		if (!member || member.IsNull())
			return 0;
		if (!member.IsNumber() || member.m_Number < min || member.m_Number > max)
		{
			error = path + key + " must be a number within " + VyshkaJsonValue.FormatFloat(min) + " and " + VyshkaJsonValue.FormatFloat(max);
			return -1;
		}
		value = member.m_Number;
		return 1;
	}

	// Section reads a member that must be an object: null when absent, and
	// false (with the reason) when present and not one.
	static bool Section(VyshkaJsonValue source, string key, string path, out VyshkaJsonValue section, out string error)
	{
		section = null;
		VyshkaJsonValue member = source.Get(key);
		if (!member || member.IsNull())
			return true;
		if (!member.IsObject())
		{
			error = path + key + " must be an object";
			return false;
		}
		section = member;
		return true;
	}

	// SectionMembers refuses a member of the object knob key that is not one
	// of members; an absent or non-object knob is left to Section.
	static bool SectionMembers(VyshkaJsonValue source, string key, array<string> members, string path, out string error)
	{
		VyshkaJsonValue section = source.Get(key);
		if (!section || !section.IsObject())
			return true;
		for (int i = 0; i < section.Count(); i++)
		{
			string member = section.KeyAt(i);
			if (members.Find(member) < 0)
			{
				error = path + key + "." + member + " is not one of its members (" + JoinNames(members) + ")";
				return false;
			}
		}
		return true;
	}

	static string JoinNames(array<string> names)
	{
		string text = "";
		for (int i = 0; i < names.Count(); i++)
		{
			if (i > 0)
				text += ", ";
			text += names.Get(i);
		}
		return text;
	}

	// Read takes the knobs source carries. A stored preset (record) may
	// carry only the knobs, so a misspelt member is refused rather than
	// silently ignored; a dispatch's params carry the preset and name
	// beside them, and the hub has checked them against the schema.
	bool Read(VyshkaJsonValue source, bool record, out string error)
	{
		if (!source)
			return true;
		if (!source.IsObject())
		{
			error = "the weather must be a JSON object of knobs";
			return false;
		}
		string path = "";
		if (record)
		{
			path = "the stored weather's ";
			for (int i = 0; i < source.Count(); i++)
			{
				string key = source.KeyAt(i);
				if (!Known(key))
				{
					error = path + "member " + key + " is not a weather knob (" + KNOWN + ")";
					return false;
				}
			}
			// The same inside the knobs that are objects, so a misspelt
			// stopSeconds does not quietly become the default.
			array<string> fogMembers = {"distanceDensity", "heightDensity", "heightBias"};
			array<string> stormMembers = {"density", "threshold", "timeoutSeconds"};
			array<string> thresholdMembers = {"min", "max", "stopSeconds"};
			if (!SectionMembers(source, "dynamicFog", fogMembers, path, error))
				return false;
			if (!SectionMembers(source, "storm", stormMembers, path, error))
				return false;
			if (!SectionMembers(source, "rainThreshold", thresholdMembers, path, error))
				return false;
			if (!SectionMembers(source, "snowfallThreshold", thresholdMembers, path, error))
				return false;
		}

		float value;
		int got = Number(source, "overcast", path, 0, 1, value, error);
		if (got < 0)
			return false;
		if (got > 0)
		{
			m_HasOvercast = true;
			m_Overcast = value;
		}
		got = Number(source, "fog", path, 0, 1, value, error);
		if (got < 0)
			return false;
		if (got > 0)
		{
			m_HasFog = true;
			m_Fog = value;
		}
		got = Number(source, "rain", path, 0, 1, value, error);
		if (got < 0)
			return false;
		if (got > 0)
		{
			m_HasRain = true;
			m_Rain = value;
		}
		got = Number(source, "snowfall", path, 0, 1, value, error);
		if (got < 0)
			return false;
		if (got > 0)
		{
			m_HasSnowfall = true;
			m_Snowfall = value;
		}
		got = Number(source, "windSpeed", path, 0, VyshkaWeather.WIND_SPEED_MAX, value, error);
		if (got < 0)
			return false;
		if (got > 0)
		{
			m_HasWindSpeed = true;
			m_WindSpeed = value;
		}
		got = Number(source, "windDirection", path, -180, 180, value, error);
		if (got < 0)
			return false;
		if (got > 0)
		{
			m_HasWindDirection = true;
			m_WindDirection = value;
		}
		got = Number(source, "windMaxSpeed", path, 0, VyshkaWeather.WIND_SPEED_MAX, value, error);
		if (got < 0)
			return false;
		if (got > 0)
		{
			m_HasWindMax = true;
			m_WindMax = value;
		}
		got = Number(source, "transitionSeconds", path, 0, VyshkaWeather.SECONDS_MAX, value, error);
		if (got < 0)
			return false;
		if (got > 0)
		{
			m_HasTransition = true;
			m_Transition = value;
		}
		got = Number(source, "holdSeconds", path, 0, VyshkaWeather.SECONDS_MAX, value, error);
		if (got < 0)
			return false;
		if (got > 0)
		{
			m_HasHold = true;
			m_Hold = value;
		}

		VyshkaJsonValue modeValue = source.Get("mode");
		if (modeValue && !modeValue.IsNull())
		{
			string mode = "";
			if (modeValue.IsString())
				mode = modeValue.m_Text;
			if (mode != VyshkaWeather.MODE_WORLD && mode != VyshkaWeather.MODE_ENGINE && mode != VyshkaWeather.MODE_HOLD)
			{
				error = path + "mode must be world, engine, or hold";
				return false;
			}
			m_Mode = mode;
		}

		return ReadSections(source, path, error);
	}

	// ReadSections reads the three knobs that are objects of their own.
	bool ReadSections(VyshkaJsonValue source, string path, out string error)
	{
		VyshkaJsonValue section;
		float value;
		int got;
		if (!Section(source, "dynamicFog", path, section, error))
			return false;
		if (section)
		{
			string fogPath = path + "dynamicFog.";
			got = Number(section, "distanceDensity", fogPath, 0, 1, value, error);
			if (got < 0)
				return false;
			if (got > 0)
			{
				m_HasFogDistance = true;
				m_FogDistance = value;
			}
			got = Number(section, "heightDensity", fogPath, 0, 1, value, error);
			if (got < 0)
				return false;
			if (got > 0)
			{
				m_HasFogHeight = true;
				m_FogHeight = value;
			}
			got = Number(section, "heightBias", fogPath, VyshkaWeather.FOG_BIAS_MIN, VyshkaWeather.FOG_BIAS_MAX, value, error);
			if (got < 0)
				return false;
			if (got > 0)
			{
				m_HasFogBias = true;
				m_FogBias = value;
			}
		}

		if (!Section(source, "storm", path, section, error))
			return false;
		if (section)
		{
			float density;
			float threshold;
			float timeout = VyshkaWeather.STORM_TIMEOUT_DEFAULT;
			string stormPath = path + "storm.";
			if (Number(section, "density", stormPath, 0, 1, density, error) != 1 || Number(section, "threshold", stormPath, 0, 1, threshold, error) != 1)
			{
				if (error == "")
					error = stormPath + "density and " + stormPath + "threshold are both required";
				return false;
			}
			if (Number(section, "timeoutSeconds", stormPath, 0, VyshkaWeather.SECONDS_MAX, timeout, error) < 0)
				return false;
			SetStorm(density, threshold, timeout);
		}

		float low;
		float high;
		float stop;
		if (!Threshold(source, "rainThreshold", path, low, high, stop, got, error))
			return false;
		if (got > 0)
		{
			m_HasRainThreshold = true;
			m_RainMin = low;
			m_RainMax = high;
			m_RainStop = stop;
		}
		if (!Threshold(source, "snowfallThreshold", path, low, high, stop, got, error))
			return false;
		if (got > 0)
		{
			m_HasSnowfallThreshold = true;
			m_SnowfallMin = low;
			m_SnowfallMax = high;
			m_SnowfallStop = stop;
		}
		return true;
	}

	// Threshold reads a rain or snowfall threshold: min and max overcast,
	// min not above max, and the seconds it takes to stop.
	static bool Threshold(VyshkaJsonValue source, string key, string path, out float low, out float high, out float stop, out int got, out string error)
	{
		got = 0;
		VyshkaJsonValue section;
		if (!Section(source, key, path, section, error))
			return false;
		if (!section)
			return true;
		string sectionPath = path + key + ".";
		if (Number(section, "min", sectionPath, 0, 1, low, error) != 1 || Number(section, "max", sectionPath, 0, 1, high, error) != 1)
		{
			if (error == "")
				error = sectionPath + "min and " + sectionPath + "max are both required";
			return false;
		}
		if (low > high)
		{
			error = sectionPath + "min must not be above " + sectionPath + "max";
			return false;
		}
		stop = VyshkaWeather.THRESHOLD_STOP_DEFAULT;
		if (Number(section, "stopSeconds", sectionPath, 0, VyshkaWeather.SECONDS_MAX, stop, error) < 0)
			return false;
		got = 1;
		return true;
	}

	static const string KNOWN = "overcast, fog, rain, snowfall, windSpeed, windDirection, windMaxSpeed, dynamicFog, storm, rainThreshold, snowfallThreshold, transitionSeconds, holdSeconds, mode";

	// Known says whether key is one of the knobs KNOWN names. The list is
	// spelt out rather than split from KNOWN: the engine's string Split
	// steps one character past a separator, so a two-character one leaves a
	// space on every name after the first.
	static bool Known(string key)
	{
		array<string> known = {"overcast", "fog", "rain", "snowfall", "windSpeed", "windDirection", "windMaxSpeed", "dynamicFog", "storm", "rainThreshold", "snowfallThreshold", "transitionSeconds", "holdSeconds", "mode"};
		return known.Find(key) >= 0;
	}
}

// VyshkaWeatherApply applies a stored weather preset once the store has
// answered, with the dispatch's own knobs over it.
class VyshkaWeatherApply : VyshkaPresetApply
{
	override VyshkaActionOutcome Apply(VyshkaJsonValue record)
	{
		VyshkaWeatherKnobs knobs = new VyshkaWeatherKnobs();
		string error;
		if (!knobs.Read(record, true, error))
			return VyshkaActionOutcome.Failure("the weather " + m_Name + ": " + error);
		if (!knobs.Read(m_Params, false, error))
			return VyshkaActionOutcome.Failure(error);
		return VyshkaWeather.Apply(knobs);
	}
}

class VyshkaWeatherAction : VyshkaAction
{
	override string Code()    { return "vyshka.weather"; }
	override string Name()    { return "Set weather"; }
	override string Context() { return "world"; }
	override string Danger()  { return "warning"; }

	override VyshkaJsonValue ParamsSchema()
	{
		array<string> presets = new array<string>;
		presets.Insert(VyshkaWeather.PRESET_CLEAR);
		presets.Insert(VyshkaWeather.PRESET_CLOUDY);
		presets.Insert(VyshkaWeather.PRESET_STORM);

		VyshkaJsonValue properties = VyshkaJsonValue.NewObject();
		properties.Set("preset", VyshkaWeather.EnumSchema(presets));
		properties.Set("name", VyshkaPresets.NameSchema(VyshkaWeather.NS_WEATHER));
		VyshkaWeather.KnobsSchema(properties);
		return VyshkaWeather.ObjectSchema(properties, null);
	}

	override VyshkaActionOutcome Execute(string actionId, string context, string referenceKey, VyshkaJsonValue params)
	{
		string preset = VyshkaAction.ReadText(params, "preset", 32);
		bool named = params && params.IsObject() && params.Get("name") && !params.Get("name").IsNull();
		if (preset != "" && named)
			return VyshkaActionOutcome.Failure("give a preset or a stored weather name, not both");

		// The dispatch's own knobs are read before anything else, so a bad
		// one fails before the store is asked.
		VyshkaWeatherKnobs knobs = new VyshkaWeatherKnobs();
		string error;
		if (preset != "" && !VyshkaWeather.Preset(preset, knobs))
			return VyshkaActionOutcome.Failure("preset must be clear, cloudy, or storm");
		if (!knobs.Read(params, false, error))
			return VyshkaActionOutcome.Failure(error);

		if (named)
		{
			string name = VyshkaPresets.ReadName(params, "name", error);
			if (name == "")
				return VyshkaActionOutcome.Failure(error);
			VyshkaWeatherApply apply = new VyshkaWeatherApply();
			apply.Init(actionId, referenceKey, VyshkaWeather.NS_WEATHER, name, "weather", params);
			return apply.Start();
		}

		VyshkaActionOutcome outcome = VyshkaWeather.Apply(knobs);
		if (outcome.m_Ok && outcome.m_Result && preset != "")
			outcome.m_Result.Set("preset", VyshkaJsonValue.NewString(preset));
		return outcome;
	}
}

class VyshkaTimeFreezeAction : VyshkaAction
{
	override string Code()    { return "vyshka.time.freeze"; }
	override string Name()    { return "Freeze time"; }
	override string Context() { return "world"; }
	override string Danger()  { return "warning"; }

	override VyshkaJsonValue ParamsSchema()
	{
		VyshkaJsonValue frozen = VyshkaJsonValue.NewObject();
		frozen.Set("type", VyshkaJsonValue.NewString("boolean"));
		frozen.Set("default", VyshkaJsonValue.NewBool(true));
		VyshkaJsonValue properties = VyshkaJsonValue.NewObject();
		properties.Set("frozen", frozen);
		return VyshkaWeather.ObjectSchema(properties, null);
	}

	override VyshkaActionOutcome Execute(string actionId, string context, string referenceKey, VyshkaJsonValue params)
	{
		bool frozen = true;
		if (params && params.IsObject())
			frozen = params.GetBool("frozen", true);
		bool was = VyshkaWeather.s_TimeFrozen;
		VyshkaWeather.SetTimeFrozen(frozen);
		string time = VyshkaWeather.Time();
		if (frozen)
			VyshkaLog.Info("world time frozen at " + time);
		else
			VyshkaLog.Info("world time running again from " + time + " at the server config's rate");

		VyshkaJsonValue result = VyshkaJsonValue.NewObject();
		result.Set("frozen", VyshkaJsonValue.NewBool(frozen));
		result.Set("wasFrozen", VyshkaJsonValue.NewBool(was));
		if (time != "")
			result.Set("time", VyshkaJsonValue.NewString(time));
		return VyshkaActionOutcome.Success(result);
	}
}
