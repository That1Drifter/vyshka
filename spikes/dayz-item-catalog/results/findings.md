# Findings: the item catalog on a stock DayZ 1.29 server

Measured 2026-09-22 on DayZ 1.29.163709 dedicated server, Windows 11, stock Chernarus
offline mission, the Vyshka plugin (0.8.0, unreleased) loaded idle. Two runs; the second
(`probe-run2.log`) is the one cited, the first differs only in its sample lines.

## What the config trees hold

| Tree | Children | `scope = 2` | Sorted into a type | Walk |
|---|---|---|---|---|
| `CfgVehicles` | 5369 | 2200 | 1814 | 6 ms |
| `CfgWeapons` | 191 | 115 | 115 | 0 ms |
| `CfgMagazines` | 126 | 121 | 121 | 0 ms |

The 386 public `CfgVehicles` classes left unsorted are what is not an item or a vehicle:
buildings, animals, the infected, effects, and the like. The walk reads each public class's
inheritance path once (`ConfigGetFullPath`) and matches base names on it, which is what the
engine's own `IsKindOf` does for one class at a time.

## Per type

| Type | Base class | Entries | Bytes | Build | Serialize | Names not translated |
|---|---|---|---|---|---|---|
| firearm | `Weapon_Base` | 115 | 14 066 | 1 ms | 13 ms | 15 |
| optic | `ItemOptics` | 32 | 3 920 | 0 ms | 2 ms | 2 |
| ammunition | `Ammunition_Base` | 39 | 5 156 | 0 ms | 4 ms | 4 |
| magazine | `Magazine_Base` | 82 | 11 223 | 0 ms | 11 ms | 12 |
| edible | `Edible_Base` | 127 | 14 621 | 2 ms | 12 ms | 6 |
| clothing | `Clothing_Base` | 786 | 107 098 | 13 ms | 90 ms | 0 |
| vehicle | `CarScript`, `BoatScript` | 20 | 2 070 | 0 ms | 1 ms | 0 |
| other | `Inventory_Base`, `ItemBase` | 849 | 85 042 | 10 ms | 72 ms | 41 |
| **all** | | **2050** | **243 196** | **26 ms** | **205 ms** | 80 |

Bytes are the serialized `entries` array with the label and the per-type stats in `data`
(weight for all; a firearm's first `chamberableFrom` and its `magazines[]` count; a
magazine's `count` and `ammo`; a food's `Nutrition energy` and `water`; a garment's first
`inventorySlot`, its `Cargo itemsCargoSize` product when it has one, and `heatIsolation`;
an optic's `OpticsInfo opticsZoomMin` and `Max`; a vehicle's `fuelCapacity` and the count
of its `Crew` children). The whole run, phases and the 50 ms gaps between them, took 539 ms
by the frame clock.

## What follows

1. **One context per type, not one catalog.** 243 KiB as one list is under the 262 144-byte
   bound of a reply (protocol section 6.2) by 7 percent on a stock server; the first mod
   pack would push it over, and the plugin then answers with no entries and a reason. Split
   by type, the largest context is 107 KiB, and the types are also what a spawn form wants
   to filter by. Entry counts are nowhere near the 5000 bound.
2. **Built once per session, on the first request.** The walk and every type's build
   together cost about 30 ms on the main thread, and serializing the largest type 90 ms,
   which the plugin's enumeration handler pays per reply. Neither needs to be spread over
   frames or done at boot; a request pays once for the walk, and the config does not change
   while the server runs.
3. **The server translates display names.** `Widget.TranslateString` on the dedicated server
   resolves `$STR_CfgVehicles_...` keys to the client-facing names ("Working Gloves", "SCR
   17", ".45 ACP Rounds"). The 80 names it does not translate are classes whose
   `displayName` is empty or not a string-table key (the bows, some magazines, the
   `TestObject`, vehicle parts) and fall back to the class name.
4. **Staged food carries no `Nutrition` class.** A food with food stages (`GreenBellPepper`)
   reads 0 for `Nutrition energy` because its values live in
   `Food FoodStages <Stage> nutrition_properties[]` (fullness index, energy, water,
   nutritional index, toxicity, agents, digestibility, in that order, per the accessors in
   `foodstage.c`); the probe read the class-level `Nutrition` alone. The catalog reads the
   `Raw` stage's array when the class-level one is absent.
5. **`Crew` children count seats.** `ConfigGetChildrenCount(<vehicle> Crew)` reads 4 for
   the rubber boat and the Gunter 2, the seats the engine exposes as `CrewSize()` on a live
   vehicle, without an instance.
