// Vyshka spike: the world clock and the weather knobs a server script has
// (issue #78).
//
// Appended to the init.c of a mission and started from main() with
// VyshkaWorldProbe.Run(). No mod is needed: everything here is the engine's
// own script API. Every 5 s it prints the clock and the weather; between the
// samples it runs a fixed schedule of changes:
//
//   t=60    SetTimeMultiplier(0)                     does the clock stop?
//   t=180   SetDate(.., 12, 0) with the clock at 0    does a set date hold?
//   t=240   SetTimeMultiplier(-1)                    does the clock run again at the config's rate?
//   t=360   SetTimeMultiplier(30)                    does the call act at all (the discriminating control)?
//   t=420   SetTimeMultiplier(-1)
//   t=450   weather set with the map's controller running (world mode):
//           overcast 1, rain 1, fog 0.5, each for at least 30 s
//   t=540   SetWeatherUpdateFreeze(true), then overcast 0.1, rain 1, fog 0,
//           and the overcast's next change due in 10 s: does a set value hold
//           past a due change, and does rain outlive an overcast below its
//           threshold?
//   t=660   snow under the freeze: the snowfall limits opened to <0, 1>, its
//           thresholds to <0, 1>, overcast 0.9, snowfall 0.8, rain 0
//   t=720   overcast 0.2 over 60 s: is the change interpolated?
//   t=800   wind: speed 25 m/s with the maximum raised to 30, direction 1 rad
//   t=830   dynamic volumetric fog: distance 0.8, height 0.6, bias 50 m
//   t=860   the freeze lifted and MissionWeather(true) (the engine's own
//           forecast, the map's controller skipped), overcast due in 5 s
//   t=960   MissionWeather(false) (the map's controller again), overcast due
//           in 5 s: does the controller take the snow limits back?
//   t=1020  finished
//
// Line format (tab separated, under the 255 characters Print keeps):
//   VYSHKA_WORLD<TAB>clock<TAB>t=<s><TAB>date=<y-m-d h:m><TAB>time=<weather clock s>
//   VYSHKA_WORLD<TAB>weather<TAB>t=<s><TAB>oc=<actual>/<forecast>/<next><TAB>rain=..<TAB>fog=..<TAB>snow=..<TAB>snowLimits=<min>/<max>
//   VYSHKA_WORLD<TAB>wind<TAB>t=<s><TAB>speed=<m/s><TAB>max=<m/s><TAB>dir=<actual>/<forecast><TAB>dyn=<enabled>:<distance>/<height>/<bias><TAB>mission=<bool><TAB>frozen=<bool>
//   VYSHKA_WORLD<TAB>act<TAB>t=<s><TAB><what>
//   VYSHKA_WORLD<TAB>finished

class VyshkaWorldProbe
{
	static ref VyshkaWorldProbe s_Instance;

	static const string TAG = "VYSHKA_WORLD";
	static const int SETTLE_MS = 3000;
	static const int TICK_S = 5;
	static const int END_S = 1020;

	int m_T;

	static void Run()
	{
		if (s_Instance)
			return;
		s_Instance = new VyshkaWorldProbe();
		GetGame().GetCallQueue(CALL_CATEGORY_SYSTEM).CallLater(s_Instance.Tick, SETTLE_MS, false);
	}

	static string F(float value)
	{
		return value.ToString();
	}

	static string Phenomenon(WeatherPhenomenon p)
	{
		return F(p.GetActual()) + "/" + F(p.GetForecast()) + "/" + F(p.GetNextChange());
	}

	void Act(string what)
	{
		Print(TAG + "\tact\tt=" + m_T.ToString() + "\t" + what);
	}

	void Sample()
	{
		int year;
		int month;
		int day;
		int hour;
		int minute;
		GetGame().GetWorld().GetDate(year, month, day, hour, minute);
		Weather w = GetGame().GetWeather();
		Print(TAG + "\tclock\tt=" + m_T.ToString() + "\tdate=" + year.ToString() + "-" + month.ToString() + "-" + day.ToString() + " " + hour.ToString() + ":" + minute.ToString() + "\ttime=" + F(w.GetTime()));
		float snowMin;
		float snowMax;
		w.GetSnowfall().GetLimits(snowMin, snowMax);
		Print(TAG + "\tweather\tt=" + m_T.ToString() + "\toc=" + Phenomenon(w.GetOvercast()) + "\train=" + Phenomenon(w.GetRain()) + "\tfog=" + Phenomenon(w.GetFog()) + "\tsnow=" + Phenomenon(w.GetSnowfall()) + "\tsnowLimits=" + F(snowMin) + "/" + F(snowMax));
		string dyn = w.IsDynVolFogEnabled().ToString() + ":" + F(w.GetDynVolFogDistanceDensity()) + "/" + F(w.GetDynVolFogHeightDensity()) + "/" + F(w.GetDynVolFogHeightBias());
		Print(TAG + "\twind\tt=" + m_T.ToString() + "\tspeed=" + F(w.GetWindSpeed()) + "\tmax=" + F(w.GetWindMaximumSpeed()) + "\tdir=" + F(w.GetWindDirection().GetActual()) + "/" + F(w.GetWindDirection().GetForecast()) + "\tdyn=" + dyn + "\tmission=" + w.GetMissionWeather().ToString() + "\tfrozen=" + w.GetWeatherUpdateFrozen().ToString());
	}

	void Schedule()
	{
		World world = GetGame().GetWorld();
		Weather w = GetGame().GetWeather();
		int year;
		int month;
		int day;
		int hour;
		int minute;
		if (m_T == 60)
		{
			world.SetTimeMultiplier(0);
			Act("SetTimeMultiplier(0)");
		}
		else if (m_T == 180)
		{
			world.GetDate(year, month, day, hour, minute);
			world.SetDate(year, month, day, 12, 0);
			Act("SetDate 12:00 with the multiplier at 0");
		}
		else if (m_T == 240)
		{
			world.SetTimeMultiplier(-1);
			Act("SetTimeMultiplier(-1)");
		}
		else if (m_T == 360)
		{
			world.SetTimeMultiplier(30);
			Act("SetTimeMultiplier(30)");
		}
		else if (m_T == 420)
		{
			world.SetTimeMultiplier(-1);
			Act("SetTimeMultiplier(-1)");
		}
		else if (m_T == 450)
		{
			w.GetOvercast().Set(1.0, 0, 30);
			w.GetRain().Set(1.0, 0, 30);
			w.GetFog().Set(0.5, 0, 30);
			Act("world mode: overcast 1, rain 1, fog 0.5, each for 30 s");
		}
		else if (m_T == 540)
		{
			w.SetWeatherUpdateFreeze(true);
			w.GetOvercast().Set(0.1, 0, 0);
			w.GetRain().Set(1.0, 0, 0);
			w.GetFog().Set(0, 0, 0);
			w.GetOvercast().SetNextChange(10);
			Act("frozen: overcast 0.1, rain 1, fog 0, overcast due in 10 s");
		}
		else if (m_T == 660)
		{
			w.GetSnowfall().SetLimits(0, 1);
			w.SetSnowfallThresholds(0, 1, 30);
			w.GetOvercast().Set(0.9, 0, 0);
			w.GetSnowfall().Set(0.8, 0, 0);
			w.GetRain().Set(0, 0, 0);
			Act("frozen: snow limits 0..1, thresholds 0..1, overcast 0.9, snow 0.8, rain 0");
		}
		else if (m_T == 720)
		{
			w.GetOvercast().Set(0.2, 60, 0);
			Act("frozen: overcast 0.2 over 60 s");
		}
		else if (m_T == 800)
		{
			w.SetWindMaximumSpeed(30);
			w.SetWindSpeed(25);
			w.GetWindDirection().Set(1.0, 0, 0);
			Act("frozen: wind max 30, speed 25, direction 1 rad");
		}
		else if (m_T == 830)
		{
			w.SetDynVolFogDistanceDensity(0.8, 0);
			w.SetDynVolFogHeightDensity(0.6, 0);
			w.SetDynVolFogHeightBias(50, 0);
			Act("frozen: dynamic fog distance 0.8, height 0.6, bias 50");
		}
		else if (m_T == 860)
		{
			w.SetWeatherUpdateFreeze(false);
			w.MissionWeather(true);
			w.GetOvercast().SetNextChange(5);
			w.GetSnowfall().SetNextChange(5);
			Act("mission weather: freeze lifted, overcast and snow due in 5 s");
		}
		else if (m_T == 960)
		{
			w.MissionWeather(false);
			w.GetOvercast().SetNextChange(5);
			w.GetSnowfall().SetNextChange(5);
			Act("world mode again: overcast and snow due in 5 s");
		}
	}

	void Tick()
	{
		Schedule();
		Sample();
		if (m_T >= END_S)
		{
			Print(TAG + "\tfinished");
			return;
		}
		m_T += TICK_S;
		GetGame().GetCallQueue(CALL_CATEGORY_SYSTEM).CallLater(Tick, TICK_S * 1000, false);
	}
}
