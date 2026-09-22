# Findings: `attachments: auto` and the spawn setters on DayZ 1.29

Measured 2026-09-22 on DayZ 1.29.163709 dedicated server, Windows 11, stock Chernarus
offline mission, the Vyshka plugin (0.8.0, unreleased) loaded idle, its `VyshkaSpawn` class
doing the work exactly as `vyshka.spawn` does. Seven runs; the seventh (`probe-run7.log`,
its script errors by source in `probe-run7-script-errors.log`) is the one cited. The second
(`probe-run2-unguarded-load.log`) is kept for the state-machine measurement below, since it
called `SpawnAmmo` directly on the weapons that fault. The others found the defects listed
at the end, each fixed before the next run.

## The part index

`VyshkaSpawn.BuildCandidates` walks `CfgVehicles` once and indexes every public item by
the slots its `inventorySlot` names: 1767 parts over 295 slots in 19 ms (runs 1 to 7:
10 to 23 ms). It is built on the first `auto` spawn of a session and kept. A slot's
candidates are tried in a fixed order, the parts made for fewer slots first (a rifle's own
suppressor before the improvised one that fits every muzzle), then by name, with a name
before its own variants (`M4_MPHndgrd` before `M4_MPHndgrd_Black`). Firearms and magazines
are not in the tree the index walks, so `auto` never attaches a firearm; grenades and
explosives are left out on purpose. The engine refuses a part a slot's name admits when the
item rejects it (a flashlight on a handguard without a rail), so up to 8 candidates are
tried per slot.

## Every stock firearm

| | |
|---|---|
| Public firearms (the item catalog's firearm type) | 115 |
| Loaded | 104 (101 with a round chambered) |
| Parts attached | 340, batteries in optics and lights included |
| Slots left empty | 34 |
| Time per firearm, create and equip | at most 3 ms |

The eleven not loaded: the three bows (`PVCBow`, `QuickieBow`, `RecurveBow`), the five
under-barrel grenade launchers the config declares as firearms (`UnderSlugGrenadeM4`,
`GP25`, `GP25_Standalone`, `M203`, `M203_Standalone`; they list no magazine and no round),
`LAW`, `RPG7`, and `DartGun`. The M249 and the two shock pistols are loaded without a
chambered round (below).

The slots most often left empty: `weaponFlashlight` 12 times (the plain handguard, first in
the order, has no rail), `weaponBayonetAK` 8 (the suppressor takes the muzzle first),
`suppressorImpro` 7, `weaponMuzzleM4` 4 (on the M4 family the bayonet slot comes first and
blocks the muzzle). The engine decides the order: slots are filled in the order the item
declares them.

Examples from the run:

| Item | What `auto` attached |
|---|---|
| `M4A1` | `Mag_STANAG_30Rnd` (30, one chambered), `M4_MPHndgrd`, `ACOGOptic`, `GhillieAtt_Mossy`, `M4_CQBBttstck`, `M9A1_Bayonet`; flashlight and muzzle empty |
| `AKM` | `Mag_AKM_30Rnd`, `AK74_Hndgrd`, `GhillieAtt_Mossy`, `AK74_WoodBttstck`, `AK_Suppressor`, `GrozaOptic`; flashlight and bayonet empty |
| `FNX45` | `Mag_FNX45_15Rnd`, `MakarovPBSuppressor`, `FNP45_MRDSOptic` with a `Battery9V`, `TLRLight` with a `Battery9V` |
| `Mosin9130` | 5 rounds in its internal magazine, `Mosin_Bayonet`, `Mosin_Compensator`, `GhillieAtt_Mossy`, `PUScopeOptic` |
| `PlateCarrierVest` | `PlateCarrierHolster`, `PlateCarrierPouches`; the four grenade slots empty |
| `MilitaryBelt` | `LeatherKnifeSheath` with a `CombatKnife`, `Canteen`, `PlateCarrierHolster` |
| `Mich2001Helmet` | `UniversalLight` and `NVGoggles`, each with a `Battery9V` |
| `OffroadHatchback` | 14 parts: four wheels and the spare (`HatchbackWheel`), both doors, hood, trunk, both headlights, radiator, spark plug, car battery |
| `CivilianSedan` | 16 parts, the same set with four doors |

## The weapons whose state machine never runs

`Weapon_Base.SpawnAmmo` is the engine's own spawn-with-ammo call (the same one its
`CreateWeaponWithAmmo` makes): it attaches a magazine or fills an internal one, chambers a
round, and closes with a synchronization of the weapon's state machine. For six classes the
machine is not running when the weapon is created, and still not half a second later:

| Class | Running at creation | 500 ms later | `SpawnAmmo` called directly (run 2) |
|---|---|---|---|
| `M249` | false | false | one round chambered, script error |
| `LAW` | false | false | one round, script error |
| `RPG7` | false | false | one round, script error |
| `DartGun` | false | false | nothing loaded, script error |
| `Shockpistol` | false | false | one round, script error |
| `PVCBow` (and the other bows) | false | false | nothing loaded, script error |
| `M4A1` (control) | true | true | one round chambered, no error |

The error is `Weapon.SaveCurrentFSMState: trying to save weapon without FSM (or
uninitialized weapon)`, logged by the synchronization. The plugin therefore calls
`SpawnAmmo` only on a weapon whose machine runs (a protected member, read through a modded
`Weapon_Base`); on the others it attaches the first detachable magazine full, as a part,
with nothing chambered, which loads the M249 (`Mag_M249_Box200Rnd`, 200) and the shock
pistols (`Mag_ShockCartridge`, 1) and leaves the launchers, the bows, and the dart gun
unloaded. The action's result says `loaded: null` for those.

Creating these weapons logs script errors of its own, whatever the plugin does next: every
one of the 21 errors left in run 7 is raised inside the probe's `CreateObjectEx` calls
(`SaveCurrentFSMState` 15 times, once per creation of the nine classes above, the three
bows and both shock pistols included, and `ValidateMuzzleArray` once each for `Groza`,
`Trumpet`, and the four under-barrel launchers), none inside a plugin file. A plain `vyshka.spawn` of an RPG-7 logs the same lines.

## Health

`GetMaxHealth` logs `No DamageSystemData or not initialized yet` for the classes whose
config declares no `DamageSystem` (`LAW`, `RPG7`, `DartGun`, `Shockpistol`,
`UnderSlugGrenadeM4`; run 4, through the inventory read's condition fields). The engine's
`HasDamageSystem` cannot stand guard: it read false for every weapon in the run, at
creation and half a second later, the M4A1 included, and guarding on it (run 5) refused a
health for every item. The plugin checks `ConfigIsExisting("DamageSystem")` instead, which
is false for exactly those five and true for the rest.

A health is a percent of the item's own maximum, the unit the inventory read reports:

| Class | Asked | Read back | Level |
|---|---|---|---|
| `M4A1` | 50 | 50 | damaged |
| `M4A1` | 0 | 0 | ruined |
| `M4A1` | 100 | 100 | pristine |
| `TShirt_Black` | 30 | 30 | badlyDamaged |
| `Apple` | 69.5 | 70 | worn |
| `OffroadHatchback` | 40 | 40 | damaged |

## Quantity

| Class | Asked | Outcome |
|---|---|---|
| `Rag` (stack of 6) | 3 | 3 of 6 |
| `Rag` | 2.5 | refused: a stack's quantity is a whole number |
| `Rag` | 0 | refused: the engine deletes a rag stack at 0 (`varQuantityDestroyOnMin`) |
| `Rag` | 50 | refused: within 0 and 6 |
| `WaterBottle` | 500 | 500 of 1000 |
| `WaterBottle` | 0 | 0 of 1000, an empty bottle |
| `Canteen` | 250.5 | 250.5 of 1000 |
| `Mag_STANAG_30Rnd` | 12 | 12 of 30 rounds |
| `Mag_STANAG_30Rnd` | 31, 7.5 | refused: a count of rounds within 0 and 30 |
| `Ammo_556x45` (a pile) | 20 | 20 of 20 |
| `Ammo_556x45` | 0 | refused: a pile holds at least one round (the engine creates the empty pile; run 6 took it) |
| `Nail` | 70 | 70 of 99 |
| `Battery9V` | 50 | 50 of 50 |
| `SmallGasCanister` | 100 | refused: within 0 and 20 |
| `M4A1` | 1 | refused: no quantity |

## Placement on a body with no client

| Case | Where the item went |
|---|---|
| `inventory`, undressed body | the hands: the inventory's own search finds no cargo or slot and falls back to them (`HumanInventory.CreateInInventory`) |
| `inventory`, a second item | refused: nowhere left |
| `hands`, empty | the hands |
| `hands`, full | refused |
| `inventory`, a jacket and a bag on, an apple in hands, a rag | the jacket's cargo |
| `inventory`, the same, an M4A1 | the `Shoulder` slot |
| `inventory`, a car | refused |

## The blocklist's config walk

Canonicalizing four names against the three config trees took 3 ms: `m4a1` became `M4A1`
and `mag_stanag_30rnd` became `Mag_STANAG_30Rnd`; `NoSuchClassAnywhere` and
`GRENADE_RGD5` (the stock grenade's class has another name) are declared by no tree and are
kept as written, with a warning in the log.

## What follows

1. **`auto` costs nothing worth budgeting.** At most 3 ms a firearm and 2 ms a car, plus
   about 20 ms once a session for the index.
2. **A firearm is loaded only through a running state machine.** 104 of 115 stock firearms
   load; the M249 and the shock pistols through an attached magazine with nothing
   chambered; the rest report `loaded: null`.
3. **The picks are the engine's choice within a fixed rule.** The action answers with every
   part it attached and every slot it left empty, so an operator sees what `auto` did
   rather than trusting a list.
4. **Quantity and health are bounded by the item.** The action refuses what the setter
   would clamp or what the engine would delete, and removes the item it created when it
   refuses, so a refused spawn leaves nothing behind.

## Defects the runs found

- Run 1: the candidates' sort key used `|`, which sorts after `_`, so every variant beat its
  plain class: ruined wheels and rusted doors on the cars, black parts on the rifles,
  `ACOGOptic_6x` for `ACOGOptic`. `auto` also put four flash grenades on the plate carrier.
  The separator is now `#`, below every class-name character, grenades and explosives are
  out of the index, and a part the engine creates ruined is taken off again.
- Run 3: the probe passed a fresh `VyshkaJsonValue.NewArray()` straight into a call as an
  argument, and the value was null by the time the callee used it after other calls (a
  NULL pointer in `Equip`). The action always holds these in locals; the probe does too
  now.
- Run 5: the `HasDamageSystem` guard above.
- Run 6: an ammunition pile took a quantity of 0.
