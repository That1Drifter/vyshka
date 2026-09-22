# Spike: what `attachments: auto` attaches, and how the spawn setters behave

Part of issue #75 (the spawning extension). `vyshka.spawn` gains `attachments: auto`, which
fills every attachment slot of the spawned item from config and loads a firearm, plus a
`quantity`, a `health`, and a target (`ground`, `inventory`, `hands`). Four things about
that were unmeasured and decide the action's shape: which part the engine accepts in each
slot when the plugin picks by rule rather than from a hand-written list, whether the
engine's own spawn-with-ammo call (`Weapon_Base.SpawnAmmo`) can load every stock firearm,
what the quantity and health setters do at the edges of their ranges, and where the
inventory's own placement search puts a new item on a body.

Findings: [`results/findings.md`](results/findings.md). Short answer: every one of the 115
public firearms of a stock 1.29 server was equipped in at most 3 ms, 104 of them loaded
(101 with a round chambered); the six whose state machine never runs (the bows, the two
launchers, the M249, the dart gun, the shock pistol) cannot go through `SpawnAmmo` without
a script error, so the ones with a detachable magazine get it attached full instead; the
part index builds in about 20 ms; and every script error left in the run comes from the
engine creating those six weapons at all, none from the plugin.

## Layout

| Path | What it is |
|---|---|
| `harness/VyshkaSpawnProbe.c` | Enforce Script probe appended to the spike mission's `init.c`. Builds the plugin's part index (`VyshkaSpawn.BuildCandidates`), then equips every public firearm through `VyshkaSpawn.Equip`, one per frame, and prints what it was loaded with, the rounds, and each part and empty slot; then equips a plate carrier, a belt, a helmet, a light, and two cars; checks whether the faulting firearms' state machines start later; runs the quantity and health setters on named classes; places items on a body with no client; and times the blocklist's config walk. Everything goes through the plugin's own classes, so the lines are what the action does. |
| `runner/main.go` | Derives the mission from the stock offline mission, boots the dedicated server with the built `@Vyshka` mod, follows the script log for the probe's lines, prints them, and stops the server. |
| `results/` | The probe's lines per run, the script errors of the cited run by source, and the write-up. |

## Reproducing

From the repository root, with the mod built (`go run ./plugins/dayz/cmd/vyshka-dayz build`)
and the DayZ dedicated server installed under Steam:

```
go run ./spikes/dayz-spawn-attachments/runner > probe.log
```

The runner derives `mpmissions/vyshkaSpawnSpike.chernarusplus` from the stock offline
mission on every run and writes `vyshka_spawn_spike_serverDZ.cfg` beside `serverDZ.cfg`.
The plugin runs with no config and idles; only its classes are used. No game client is
needed; the run takes about a minute, most of it the server's boot.

## Provenance

Clean-room: the probe and the measurements are original work written from the public engine
script headers shipped with the game (`scripts/4_world/entities/firearms/weapon_base.c` for
`SpawnAmmo` and the weapon state machine, `scripts/3_game/systems/inventory/inventory.c` and
`humaninventory.c` for the placement search, `scripts/4_world/entities/itembase.c` for the
quantity setter, `scripts/3_game/entities/object.c` for the health setter) and from observed
behavior. No third-party integration product was consulted or inspected.
