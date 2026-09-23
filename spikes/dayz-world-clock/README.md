# Spike: the world clock and the weather knobs on a DayZ server

Part of issue #78 (the world). The slice publishes the world's clock and weather as a
`state.world` snapshot and adds a weather action with every engine knob, fixed and stored
presets, a behaviour mode, and freeze time. Four things about that were unmeasured and
decide the actions' shape: whether the engine's time multiplier stops the server's clock at
all, how long a weather value set from script lasts under the map's own weather controller,
whether freezing the weather update holds every value, and what the map's controller takes
back when it runs again.

Findings: [`results/findings.md`](results/findings.md). Short answer: `SetTimeMultiplier(0)`
stops the clock and a date set while it is stopped holds; a value set under the map's
controller lasts its hold time and no longer, and the controller then closes Chernarus's
snowfall limits and resets the wind maximum; with the update frozen every value holds, the
wind included, except rain, which the engine still stops when the overcast is outside the
rain threshold.

## Layout

| Path | What it is |
|---|---|
| `harness/VyshkaWorldProbe.c` | Enforce Script probe appended to the spike mission's `init.c`. Every 5 s it prints the date, the overcast, fog, rain, and snowfall (actual, forecast, seconds to the next change), the snowfall limits, the wind, the dynamic fog, and the weather's two script flags; between the samples it runs a fixed schedule of changes, listed at the top of the file. |
| `runner/main.go` | Derives the mission from the stock offline mission, boots the dedicated server (no mod), follows the script log for the probe's lines, prints them, and stops the server. |
| `results/` | The probe's lines of the cited run and the write-up. |

## Reproducing

From the repository root, with the DayZ dedicated server installed under Steam:

```
go run ./spikes/dayz-world-clock/runner > probe.log
```

The runner derives `mpmissions/vyshkaWorldSpike.chernarusplus` from the stock offline
mission on every run and writes `vyshka_world_spike_serverDZ.cfg` beside `serverDZ.cfg`. No
game client is needed; the run takes about eighteen minutes, seventeen of them the
schedule.

## Provenance

Clean-room: the probe and the measurements are original work written from the public engine
script headers shipped with the game (`scripts/3_game/weather.c` for the weather controller
and its phenomena, `scripts/3_game/global/world.c` for `SetTimeMultiplier` and the date,
`scripts/3_game/worlddata.c` and `scripts/4_world/classes/worlds/chernarusplus.c` for the
map's controller) and from observed behavior. No third-party integration product was
consulted or inspected.
