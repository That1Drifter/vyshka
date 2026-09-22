# Findings: a fully loaded character's inventory tree on DayZ 1.29

Measured 2026-09-22 on DayZ 1.29.163709 dedicated server, Windows 11, stock Chernarus
offline mission, the Vyshka plugin (0.8.0, unreleased) loaded idle, its
`VyshkaInventoryTree` class building the tree exactly as `vyshka.inventory.read` does. Four
runs; the fourth (`probe-run4.log`) is the one cited. The third (`probe-run3-config-walk.log`)
is kept for two engine facts it found the hard way, below; the first two crashed or
measured an undressed body and are not kept. A fifth run (`probe-run5-slot-order.log`),
after the review fix that lists worn items in the character's slot order, measured the same
bytes and counts; only the order of its `carried` lines differs from run 4, which listed
them in the order they were attached.

## The body

A `SurvivorM_Mirek` created with `CreateObjectEx` on the terrain, alive, with no client
behind it, 17 attachment slots (`Headgear`, `Gloves`, `Shoulder`, `Armband`, `Back`,
`Body`, `Feet`, `Head`, `Hips`, `Legs`, `Mask`, `Vest`, `Hands`, `Melee`, `LeftHand`,
`Splint_Right`, `Eyewear`; the engine declares `Hands` among them, though the held item
lives at a location of its own).

## The loadout

`VyshkaLoadout.Gear` then `Stuff` (`harness/VyshkaLoadout.c`), every class created where
it was asked for:

| Slot | Class | Cargo | Items inside after the fill |
|---|---|---|---|
| Hands | `M4A1` with a full STANAG, ACOG, suppressor, RIS handguard, and buttstock | none | 5 |
| Back | `AliceBag_Green` | 8x8 | 56 |
| Body | `HuntingJacket_Brown` | 6x4 | 16 |
| Vest | `HighCapacityVest_Black` | 5x4 | 12 |
| Legs | `HunterPants_Brown` | 5x4 | 12 |
| Hips | `MilitaryBelt` with a canteen, a sheath holding a combat knife, and a holster holding an FNX45 with a full magazine | none | 6 |
| Shoulder | `M4A1` as in hands | none | 5 |
| Melee | `AKM` with a full magazine, Kashtan optic, suppressor, wood handguard, and buttstock | none | 5 |
| Headgear, Mask, Eyewear, Gloves, Armband, Feet | ballistic helmet, gas mask, aviators, tactical gloves, armband, military boots | none | 0 |

The fill nested a protector case or a first aid kit in each of the four cargo containers
(4 nested) and then created `Battery9V` (one slot, not stackable) in every cargo, the
nested ones included, until the engine refused: 92 fillers. Gearing cost 9 ms and the fill
9 ms on the main thread.

| | |
|---|---|
| Top-level items (hands plus worn) | 14 |
| Items in all | 131 |
| Deepest level | 4 (belt, holster, pistol, magazine) |

## The tree

| Depth | Items described | Bytes | Build | Serialize |
|---|---|---|---|---|
| 4 (whole) | 131 | 14 035 | 1 ms | 9 ms |
| 3 | 130 | 13 894 | 1 ms | 8 ms |
| 2 | 128 | 13 667 | 1 ms | 7 ms |
| 1 | 14 | 1 504 | 0 ms | 0 ms |

The read action's own loop (whole tree first, one level less each time until the bytes
fit its 60 000-byte budget) settled on the whole tree at the first attempt. One entry is
about 107 bytes: class, display name, slot, health, state, and the fields its kind adds
(`quantity` and `quantityMax`, `ammo` and `ammoMax`, `rounds`).

Two controls run the probe with the budget lowered in a copy (`-probe`), since this
loadout never exercises the cut: at 14 000 bytes the loop stepped down once and settled on
depth 3 (13 894 bytes, 2 attempts, `probe-control-budget14000.log`); at 1 000 bytes it
walked down to depth 1, still 1 504 bytes, and reported `over-budget` after 4 attempts
(`probe-control-budget1000.log`), the condition on which the action fails the read
rather than answer a payload the hub would drop.

## Strip and clear with no client

`before-strip carried=131 nearby=0`; the strip reported 14 dropped, 0 skipped; two seconds
later `carried=131 nearby=0`. The server-side drop (`Man.ServerDropEntity`, `InventoryMode.SERVER`)
of an alive character goes through an inventory juncture and a sync juncture the client has
to take part in, so on a body with no client nothing moves; the live run with a retail
client is what shows the strip. The clear (`EntityAI.DeleteSafe` on each top-level item)
reported 14 deleted, and two seconds later `carried=0 nearby=0`: the juncture delete is
honored on the character's next update even with no client, and the contents went with
their containers (131 items gone for 14 deletes, nothing left on the ground).

## What follows

1. **A character carrying this loadout fits the cap with room to spare.** 14 KiB against
   the hub's 64 KiB result cap and the action's 60 000-byte budget; the action answers it
   whole at the first attempt. The loadout is a named list (`harness/VyshkaLoadout.c`),
   not a search of every stock garment, so this is the size of a heavy stock load, not a
   proven stock maximum. The shrink loop exists for far larger loads (a modded bag with
   several times the cargo, a mod that adds slots) and costs about 10 ms per attempt at
   this size, so it needs no budgeting of its own. At about 107 bytes an entry, the budget
   holds about 560 entries.
2. **Depth 4 is what this tree reaches.** A magazine on a pistol in a holster on a belt.
   Cutting at depth 3 loses that one magazine; cutting at 2 loses what the nested cases
   hold; only depth 1 loses the bulk. The action reports the depth it settled on and
   `truncated`, and a `slot` read gives one container's subtree the whole budget to
   itself (a subtree alone over the budget is cut the same way).
3. **The strip cannot be shown without a client; the clear can.** Recorded above. The
   live run (the slice's notes) is the evidence for the strip.

## Two engine facts from run 3

- `ConfigGetIntArray("CfgVehicles <class> Cargo itemsCargoSize", ...)` returns nothing for
  every one of the 786 public clothing classes (`walk clothing=786 ... best=0`): the
  script config reader does not return that key, so cargo dimensions must be read from an
  instance (`GetInventory().GetCargo().GetWidth()` and `GetHeight()`), as the loadout does.
- A weapon created for a character with empty hands lands in the hands whatever slot was
  asked for, through `CreateAttachmentEx` (run 2) and through an explicit attachment
  `InventoryLocation` with `LocationCreateEntity` alike (run 3: `slot=Shoulder class=M4A1+5
  in Hands`). Filling the hands first (run 4) makes the shoulder creation land on the
  shoulder.
