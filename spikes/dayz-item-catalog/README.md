# Spike: what an item catalog costs the DayZ server

Part of issue #73 (the item catalog as the first custom context). The plugin publishes its
class-name catalog as custom contexts the hub enumerates (protocol section 6.2), and three
things about that were unmeasured and decide its shape: how many public item classes a
stock server declares against the 5000-entry bound of one reply, how many bytes their
entries take against the 262144-byte bound, and how long walking the config trees and
building the entries holds the main thread, which decides whether the catalog is built at
boot or on the first request. A fourth was a guess until measured: whether the dedicated
server can translate a `$STR_` display name at all.

Findings: [`results/findings.md`](results/findings.md). Short answer: about 2050 public
items and vehicles, 243 KiB as one list, too close to the bound for a modded server, so the
catalog is one context per type (the largest, clothing, is 786 entries and 107 KiB); the
whole walk and every build together cost about 40 ms, so the catalog is built once per
session on the first request; and the server translates all but a few dozen names, which
fall back to the class name.

## Layout

| Path | What it is |
|---|---|
| `harness/VyshkaCatalogProbe.c` | Enforce Script probe appended to the spike mission's `init.c`. Walks `CfgVehicles`, `CfgWeapons`, and `CfgMagazines`, keeps every `scope = 2` class, sorts each by its inheritance path (`ConfigGetFullPath`) into firearm, optic, ammunition, magazine, edible, clothing, vehicle, or other, then builds each type's entries through the plugin's own `VyshkaContextList` and JSON classes, with the display name through `Widget.TranslateString` and the per-type stats read from config. Each phase runs in a frame of its own and is timed with the performance counter; the finished line carries the frame clock. |
| `runner/main.go` | Derives the mission from the stock offline mission, boots the dedicated server with the built `@Vyshka` mod, follows the script log for the probe's lines, prints them, and stops the server. |
| `results/` | The probe's lines per run and the write-up. `probe-run1.log` is the first run, whose sample lines printed the whole list instead of one entry (a probe bug, fixed for run 2); the counts and timings are the same in both. |

## Reproducing

From the repository root, with the mod built (`go run ./plugins/dayz/cmd/vyshka-dayz build`)
and the DayZ dedicated server installed under Steam:

```
go run ./spikes/dayz-item-catalog/runner > probe.log
```

The runner derives `mpmissions/vyshkaCatalogSpike.chernarusplus` from the stock offline
mission on every run and writes `vyshka_catalog_spike_serverDZ.cfg` beside `serverDZ.cfg`.
The plugin runs with no config and idles; only its classes are used. No game client is
needed; the run takes about a minute, most of it the server's boot.

## Provenance

Clean-room: the probe and the measurements are original work written from the public engine
script headers shipped with the game (`scripts/3_game/global/game.c` for the config API,
`scripts/3_game/entities/object.c` for how a display name is resolved,
`scripts/4_world/entities/itembase/edible_base.c` and `scripts/4_world/classes/foodstage/foodstage.c`
for where nutrition lives) and from observed behavior. No third-party integration product
was consulted or inspected.
