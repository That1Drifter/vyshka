# Findings: the world clock and the weather knobs (issue #78)

One run on 2026-09-22, DayZ 1.29 retail dedicated server (`DayZServer_x64.exe`), the stock
Chernarus offline mission with the probe appended, `serverTimeAcceleration = 1`, no mod, no
client. The lines are [`probe-run1.log`](probe-run1.log); `t` is seconds since the probe
started, sampled every 5 s. The run logged no script error.

## The clock

| t | Change | Clock |
|---|---|---|
| 0 to 55 | none | 21:05, rolling to 21:06 at t=60 |
| 60 | `SetTimeMultiplier(0)` | 21:06 at every sample to t=175 (115 s) |
| 180 | `SetDate(.., 12, 0)` with the multiplier at 0 | 12:00 at every sample to t=235 |
| 240 | `SetTimeMultiplier(-1)` | 12:00 to 12:01 at t=300, 12:02 at t=360: one game minute a minute, the config's rate |
| 360 | `SetTimeMultiplier(30)` | 12:12 at t=380, 12:22 at t=400, 12:32 at t=420: thirty game minutes in 60 s |
| 420 | `SetTimeMultiplier(-1)` | 12:32 to t=460, then the config's rate again |

So the multiplier is a factor on real time, 0 stops the server's clock, -1 hands it back to
the server config, and a date set while the clock is stopped stays set. The 30x phase is the
control that shows the call acts at all. The engine has no getter for the multiplier, so a
plugin that stops the clock must remember that it did. Whether a client's clock follows the
server's stop was not measured here (no client).

## The weather under the map's controller (`world`)

At t=450 overcast 1, rain 1, and fog 0.5 were set with no transition and a hold of 30 s. All
three read their value at once and held it for 30 s (next change 30, 15, 0). At t=495 the
map's controller had chosen new forecasts (overcast 0.30, rain 0.82, fog 0.13) and the actual
values were moving toward them. A value set from script lasts its hold time under the
map's controller, and no longer.

## The weather with the update frozen (`hold`)

`SetWeatherUpdateFreeze(true)` at t=540, then overcast 0.1, rain 1, fog 0, and the
overcast's next change due in 10 s:

- The overcast held 0.1 to t=655 while its next change counted down past zero (-5, -20,
  ... -110): with the update frozen nothing chooses a new forecast.
- The rain did not hold. With the overcast (0.1) below the rain threshold's minimum (the
  map sets 0.6), its forecast went to 0 and it decayed from 1 to 0.17 over 105 s. The
  engine stops rain outside the rain threshold whatever the freeze says, so a weather action
  that asks for rain must also ask for an overcast inside the threshold, or move the
  threshold.
- At t=660 the snowfall limits were opened to 0..1 (Chernarus keeps them at 0..0), the
  snowfall thresholds to 0..1, the overcast set to 0.9, and snowfall 0.8: the snow read 0.8
  and held to t=860, through the overcast falling to 0.2 at t=720 (inside the opened
  threshold).
- At t=720 the overcast was set to 0.2 over 60 s: 0.84, 0.78, 0.72, 0.67, 0.61 at 5 s steps,
  a straight line, reaching 0.2 by t=780. A transition interpolates linearly.
- At t=800 the wind maximum was raised to 30, the speed set to 25, and the direction to
  1 rad: all three read back and held to t=860. Before that the wind had held 9.68 m/s and
  -0.64 rad from t=540 on; the freeze holds the wind too.
- At t=830 the dynamic volumetric fog (enabled in Chernarus's config) was set to distance
  0.8, height 0.6, bias 50 m, and read back so.

## The engine's own weather (`engine`)

At t=860 the freeze was lifted and `MissionWeather(true)` set, with the overcast and the
snowfall due in 5 s. By t=870 the overcast had a new forecast (0.44) and the fog one of its
own (0.51, beyond anything the map's controller picks), while the snowfall limits stayed
open (0..1) and the wind maximum stayed 30: the map's controller was skipped and the engine
picked forecasts within the limits.

## Back to the map's controller

At t=960 `MissionWeather(false)`, with the overcast and the snowfall due in 5 s. By t=975
the controller had closed the snowfall limits (0..0, so the snow went from 0.8 to 0 at once),
set the wind maximum back to 20, and taken the wind speed down from 24.6. It re-applies its
own settings each time it chooses a forecast, so a snowfall, a wind maximum, a storm, or a
threshold set from script lasts under it only until its next change.

## What the slice takes from this

- Freeze time is `SetTimeMultiplier(0)` and unfreeze is `-1`, with the plugin remembering
  which it set.
- The weather action has a behaviour mode: `world` (the map's controller, values last their
  hold time), `engine` (`MissionWeather`), and `hold` (the update frozen). The default hold
  time has to be long enough to matter under `world`.
- A value outside a phenomenon's limits is let in by widening the limits; the result says
  which were widened, since under `world` the map closes Chernarus's snowfall limits again.
- Rain asked for with the overcast below the rain threshold is noted in the result, because
  the engine will stop it.
