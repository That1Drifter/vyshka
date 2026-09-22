# Spike: what a fully loaded character's inventory tree costs

Part of issue #74 (the inventory actions). `vyshka.inventory.read` answers with one
player's whole inventory tree as an action result, and a result is bounded by the hub's
64 KiB cap (protocol section 7): over it, the hub keeps the outcome and drops the payload,
which for a read is the whole answer. Two things about that were unmeasured and decide the
action's shape: how many bytes the tree of the most loaded character a stock server allows
serializes to, and how the bytes fall off when the tree is cut one level of containers at
a time, which is how the action fits a tree that does not. The two mutations, strip and
clear, are run on the same body as a smoke test of the engine calls with no client behind
the character.

Findings: [`results/findings.md`](results/findings.md). Short answer: the heaviest loadout
the probe could build is 131 items, 4 levels deep, and 14 035 bytes as the whole tree,
under the action's 60 000-byte budget by a factor of four and built and serialized in about
10 ms, so a stock character is always answered whole and the cut is for modded servers;
the clear works on a body with no client, the strip's moves wait for one.

## Layout

| Path | What it is |
|---|---|
| `harness/VyshkaInventoryProbe.c` | Enforce Script probe appended to the spike mission's `init.c`. Creates a survivor body, dresses it in the largest container the config declares for each of the character's slots (a config walk over the public clothing, the same classification the item catalog uses), hangs a rifle with a full set of attachments on the shoulder, the second long slot, and the hands, nests a protector case in every container that takes one, fills every cargo with one-slot items until the engine refuses another, then builds the tree through the plugin's own `VyshkaInventoryTree` at every depth and serializes each, runs the read action's shrink loop, and finally strips and clears the body, counting what it still carries and what lies around it. Each phase runs in a frame of its own and is timed with the performance counter. |
| `runner/main.go` | Derives the mission from the stock offline mission, boots the dedicated server with the built `@Vyshka` mod, follows the script log for the probe's lines, prints them, and stops the server. |
| `results/` | The probe's lines per run and the write-up. |

## Reproducing

From the repository root, with the mod built (`go run ./plugins/dayz/cmd/vyshka-dayz build`)
and the DayZ dedicated server installed under Steam:

```
go run ./spikes/dayz-inventory-tree/runner > probe.log
```

The runner derives `mpmissions/vyshkaInventorySpike.chernarusplus` from the stock offline
mission on every run and writes `vyshka_inventory_spike_serverDZ.cfg` beside `serverDZ.cfg`.
The plugin runs with no config and idles; only its classes are used. No game client is
needed; the run takes about a minute, most of it the server's boot.

## Provenance

Clean-room: the probe and the measurements are original work written from the public engine
script headers shipped with the game (`scripts/3_game/systems/inventory/inventory.c` for the
inventory API, `scripts/3_game/entities/entityai.c` for the safe delete, `scripts/3_game/entities/man.c`
for the server-side drop) and from observed behavior. No third-party integration product
was consulted or inspected.
