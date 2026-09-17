# Spike: invisibility and no-collision as server-side admin flags on DayZ

Part of issue #71 (admin flags). The roadmap parked two flags, invisibility and
no-collision, "until a spike shows they can be done server-side": the plugin is a
server-only mod, so a flag exists only if the dedicated server can impose it on a
character that every client simulates and draws for itself. This spike calls the two
engine entry points a server has for them and records what one client could observe.

Findings: [`results/findings.md`](results/findings.md). Short answer: neither was shown.
`Entity.SetInvisible` runs on the server without error and has no observable effect on the
owner's first-person view, which cannot see the body anyway; whether it reaches another
client needs a second client, which this rig did not have. `dBodySetInteractionLayer`
changes the server body's layer value and nothing a player would notice, because the
character collides on the client it belongs to. Both stay parked, with the trigger
sharpened to the two-client measurement.

## Layout

| Path | What it is |
|---|---|
| `harness/init.c` | The test mission's `init.c` for the live run: the stock offline mission's `main()` plus a `CustomMission` with trigger files under `$profile:` (`vyshka-rig-<mode>.txt`) that act on every alive player and log `VYSHKA_RIG` lines. The spike's modes are `invisible`, `visible`, `nocollision`, `collision`, and `report` (position, health, allow-damage, stamina, interaction layer, the plugin's flags, the held weapon's magazine); the rest (`shot`, `infected`, `melee`, `clearhands`, `rifle`, `fire`) served the slice's live run of the flags themselves |
| `results/findings.md` | The write-up |

## Reproducing

A DayZ 1.29 diagnostic server (`DayZDiag_x64.exe -server`, BattlEye off) with the
`@Vyshka` mod, the mission's `init.c` replaced by `harness/init.c`, and one diagnostic
client. Touch `vyshka-rig-invisible.txt` and the others in the server's profile directory;
the answers are the `VYSHKA_RIG` lines in its script log. The run this spike records is
written up in the slice's notes (`.scratch/flags/NOTES.md`, not tracked).

## Provenance

Clean-room: the rig and the measurements are original work written from the public engine
script headers shipped with the game (`scripts/3_game/entities/entity.c` for
`SetInvisible`, `scripts/1_core/proto/enphysics.c` for the interaction layer) and from
observed behavior. No third-party integration product was consulted or inspected.
