# Vyshka DayZ plugin

The reference game plugin for DayZ: a server-side Enforce Script mod that enrolls a DayZ
dedicated server with a Vyshka hub, long-polls it for work, publishes a manifest, executes
dispatched actions, and publishes telemetry: the core player and vehicle events a feed
needs and the `state.players`, `state.vehicles`, and `state.entities` snapshots a live map
needs. It ships seventeen built-in actions (heal, vitals, stop bleeding, dry, broken legs,
bloody hands, flags, kick, ban, unban, message, broadcast, teleport, spawn, set time,
unstuck, delete destroyed vehicles), so an operator can moderate a server, patch a player
up, and move things around it from the panel or a `curl` against the hub, and a surface
other server mods build on ("Writing a mod against the plugin" below, with a sample under
`sample/`). Protocol: `spec/protocol.md`.

Clean-room: written from the engine's public script headers and the measurements under
`spikes/`, per `CONTRIBUTING.md`.

## Layout

| Path | What it is |
|---|---|
| `mod/config.cpp` | Addon and script-module registration |
| `mod/scripts/3_Game/Vyshka/` | Protocol code: JSON, clock and ids, files, outbox, transport, the link itself, the registry of actions, contexts, events, and namespaces, the event buffer, the ban list, the key/value store client (`VyshkaStoreClient`) and the namespace-bound handle mods use (`VyshkaStore`), the map markers (`VyshkaMapMarker`), and the mod-facing facade (`GetVyshka()`) |
| `mod/scripts/4_World/Vyshka/` | Game-facing code: the heal action, the vitals and condition actions (`VyshkaVitalsActions`), the moderation actions, the position and world actions, the admin flags (`VyshkaFlags`), the player roster and telemetry (`VyshkaPlayerTelemetry`), the vehicle list, telemetry, and actions (`VyshkaVehicles`) |
| `mod/scripts/5_Mission/Vyshka/` | The `MissionServer` hooks that start and stop the plugin and feed it connects, disconnects, and chat, and the `VyshkaRegister` hook a mod overrides to add its own actions |
| `sample/` | A self-contained sample mod built on the surface below: one action, one event, one context, a map marker, a store counter. Copy it to start your own |
| `selftest/` | The engine-limit self-test: a script appended to a mission's `init.c` that runs the plugin's file and JSON classes past the engine's limits inside a real server (see "Conformance") |
| `pbo/` | Go package that packs and reads PBO archives |
| `cmd/vyshka-dayz/` | Developer tool: `build` packs the mod, `build-sample` packs the sample, `harness` runs a server as a conformance candidate, `selftest` runs a server on the self-test and grades it |

## Building

```
go run ./plugins/dayz/cmd/vyshka-dayz build
```

writes `plugins/dayz/build/@Vyshka/addons/Vyshka.pbo` and a `mod.cpp`, and prints the PBO's
SHA-256. No DayZ Tools are needed: the packer is pure Go and the mod is script only. The PBO
is unsigned, which is fine for a server-side mod; clients never load it.

The build is reproducible: the archive is a function of the source tree's bytes alone.
Entries are sorted, line endings are normalized to LF whatever the checkout's, and every
entry carries one timestamp, the commit time of the last commit that touched `mod/`
(`SOURCE_DATE_EPOCH` overrides it). A build at a tagged commit on any machine with a full
clone prints the digest the release publishes as `Vyshka.pbo.sha256`, so a downloaded mod
can be checked against the repository (a shallow clone stamps its boundary commit and the
tool warns). The version in `mod.cpp` and in the manifest both come from `PLUGIN_VERSION`
in `VyshkaPlugin.c`, printed by `vyshka-dayz version`; a release tag must equal it
(`RELEASING.md`).

Releases carry the `@Vyshka` folder as `vyshka-dayz-plugin_<version>.zip` on the GitHub
release for the `dayz-plugin-v<version>` tag. There is no Steam Workshop item: the mod is
server-side, clients never load it, so nothing needs a Workshop id to pair against.

`go run ./plugins/dayz/cmd/vyshka-dayz build-sample` packs the sample mod the same way into
`plugins/dayz/build/@VyshkaSample/`. The sample is source to copy, not a release artifact.

## Installing on a server

1. Copy `@Vyshka` into the DayZ dedicated server directory.
2. Create the server record on the hub and take its one-time enrollment token
   (`POST /api/v1/servers`, or the panel once it exists).
3. Create `<profiles>/Vyshka/config.json`, where `<profiles>` is the directory the server's
   `-profiles=` argument names:

   ```json
   {
     "hubUrl": "https://hub.example.net",
     "enrollmentToken": "<one-time enrollment token>",
     "pollTimeoutSeconds": 25
   }
   ```

   `pollTimeoutSeconds` is optional (default 25, honored between 5 and 60). `game` is optional
   and defaults to `dayz`. `snapshotIntervalSeconds` is optional (default 10, honored between
   2 and 600; `0` turns the `state.players`, `state.vehicles`, and `state.entities`
   snapshots off).
   `fpsIntervalSeconds` is optional
   (default 60, honored between 5 and 3600; `0` turns `core.server.fps` samples off).
4. Start the server with the mod as a server mod:

   ```
   DayZServer_x64.exe -config=serverDZ.cfg -profiles=<profiles> -serverMod=@Vyshka -dologs
   ```

On first boot the plugin exchanges the token for permanent credentials and writes them to
`<profiles>/Vyshka/credentials.json`. The token is burned at that point; the file is what
the server uses from then on. To re-enroll after a revocation, put a fresh token in
`config.json`: the plugin notices it differs from the one the credentials came from and
enrolls again.

Everything the plugin says goes to the server's script log (`script_*.log` under the
profile directory) prefixed `[Vyshka]`, including manifest rejections and the link state
(`connected`, `degraded`, `buffering`).

## Actions

With the server enrolled and a player online:

```
curl -X POST https://hub.example.net/api/v1/servers/<serverId>/actions \
  -H "Authorization: Bearer <admin token>" -H "Content-Type: application/json" \
  -d '{"code":"vyshka.heal","context":"player","referenceKey":"<Steam64 id>","params":{}}'
```

The `referenceKey` of a player-context action is the player's plain Steam64 id, the same
identity the telemetry publishes; that of a vehicle-context action is the vehicle's `id`
from the latest `state.vehicles` snapshot. The manifest (declaring the `vyshka` key/value
namespace for the admin flags, plus whatever the mods on the server register; its revision
is derived from its content, see "Writing a mod against the plugin") declares:

| Code | Context | Danger | Params | Result |
|---|---|---|---|---|
| `vyshka.heal` | player | none | `restoreBlood` (default true) | `health`, `blood`, `shock`, `name` after the heal |
| `vyshka.vitals` | player | warning | `stat` (required: `health`, `blood`, `shock`, `energy`, `water`, `stamina`, `heatBuffer`), `value` (required, a number within the stat's range) | `name`, `stat`, `before`, `after` (read back from the engine), `min`, `max` (the range this character's engine holds for the stat); a value outside the range fails the action and names the range |
| `vyshka.stopbleeding` | player | none | none | `name`, `sourcesRemoved`, `bleeding` (read back), `infectionPrevented` (the engine's self-closing infection roll hit and was undone) |
| `vyshka.dry` | player | none | none | `name`, `wasWet` (the player's wet flag before), `items` (in the inventory tree), `dried` (how many were wet) |
| `vyshka.brokenlegs` | player | warning | `broken` (required) | `name`, `before` and `after` (`none`, `broken`, `splint`), `legHealth` (the four leg zones after) |
| `vyshka.bloodyhands` | player | none | `bloody` (required) | `name`, `before`, `after` |
| `vyshka.flags` | player | warning | any of `god`, `freeze`, `unlimitedStamina`, `unlimitedAmmo`, `ignoredByAi` (booleans; an absent one is left as it is; at least one is required) | `player`, `name` (when known), `online`, `flags` (all five after the change), `changed` (the ones named), `revision` (of the store key; 0 when nothing was ever stored); the player need not be online |
| `vyshka.kick` | player | warning | `reason` | `name`, `reason`; the player is disconnected through the engine's own disconnect call and `core.player.kick` is emitted |
| `vyshka.ban` | player | destructive | `reason`, `durationMinutes` (0, the default, is permanent) | `player`, `name` (when known), `kicked`, `expiresAt`, `activeBans`; the identity goes on the ban list, the player is kicked if online, and `core.player.ban` is emitted. The player need not be online: an offline identity is banned by its plain Steam64 id |
| `vyshka.unban` | player | warning | none | `removed` (the entry), `activeBans`; fails when the identity is not banned. Emits `vyshka.player.unban` |
| `vyshka.message` | player | none | `message` (required), `title`, `seconds` (1 to 60, default 10), `style` (`notification`, the default, or `chat`) | `name`, `style` |
| `vyshka.broadcast` | world | none | the same | `recipients`, `style` |
| `vyshka.teleport` | player | warning | exactly one of `position` (`[x, y, z]`, or `[x, z]` placed on the terrain), `toPlayer` (a Steam64 id), `previous` (true) | `name`, `mode` (`position`, `player`, `previous`), `from`, `to`, and `toPlayer` with `toPlayerName` when a player was the destination, `vehicle` when the player's vehicle was moved with them |
| `vyshka.spawn` | player | warning | `className` (required) | `className` (as the engine reports it), `displayName`, `config` (the tree that declares it), `position`, `name`; one item is created on the ground in front of the player |
| `vyshka.settime` | world | warning | `hour` (0 to 23, required), `minute` (0 to 59, default 0) | `before` and `after`, each `{ year, month, day, hour, minute }` read from the world clock |
| `vyshka.unstuck` | vehicle | warning | `lift` (metres, 0 to 10, default 1), `level` (default true) | `vehicle`, `type`, `kind`, `position`, `from`, `to`, `orientationBefore`, `orientationAfter`, `crew`; the vehicle is lifted, levelled, stopped, and its physics woken |
| `vyshka.deletedestroyed` | world | destructive | `dryRun` (default false) | `deleted` and `skipped` (each a list of `{ vehicle, type, kind, position }`, a skipped entry with its `reason`; the two lists share a 40 000-byte budget so the result stays inside the hub's 64 KiB cap whatever the class names), `deletedCount` and `skippedCount` (always complete), `truncated` (true when a list was cut), `intact` (how many were left alone), `dryRun` |

Kick, message, teleport, spawn, the ban's own kick, vitals, stop bleeding, dry, broken legs,
and bloody hands need the player online and fail with `player <id> is not online` otherwise;
ban, unban, and flags take an offline identity by its plain Steam64 id. Unstuck fails with `no vehicle <id> exists on this
server` when the id is not in the current vehicle list.

**Kicks** run the mission's own logout finalization (the same code a logout timer running
out reaches): the disconnect hook fires, so `core.player.disconnect` follows the kick event,
the character is saved, and the body is handled before the engine drops the client. A
player already counting down a logout is retired from the logout queues first, and a queued
logout registration whose player is no longer pending (withdrawn by a kick, or by the
engine's own logout cancellation) is refused, so the timer cannot finalize the same
character again. The
engine's bare disconnect call alone does none of that, measured on DayZ 1.29 while building
this slice: it drops the connection, fires no disconnect event, and leaves the plugin's
roster believing the player is still there. A `reason` is cut to 200 characters, a `message` to 1000, a `title` to
100: the schema subset of protocol section 6.1 has no length keyword, so the plugin bounds
them itself rather than refuse.

**Messages** use what every vanilla client already renders, so nothing needs installing on
the client: the `notification` style is the engine's own pop-up (the title over the message,
or the message alone as the headline, for `seconds`), and the `chat` style is a line in the
player's chat window in the engine's "important" color, `title: message` when a title is
given.

**Bans** are the plugin's own list, because the engine exposes no scripted ban API (only a
disconnect call): `<profiles>/Vyshka/bans.json`, one entry per identity with the name the
player had, the reason, when the ban was made, when it expires (`null` for permanent), and
the `actionId` that made it. A banned identity that connects is kicked as soon as its
character attaches, the earliest hook that has something to disconnect, so the feed shows
the attempt (`core.player.connect`) and the refusal (`core.player.kick` with `cause: "ban"`)
in order; the ban is looked up again at that moment, so one lifted or expired in between is
no ban. Expired entries are dropped when the list is loaded, when the identity is next
looked up, and before a result counts `activeBans`. The list is independent of the engine's
and BattlEye's own ban lists and an operator can edit it while the server is down, keeping
it as the plugin writes it, one entry and one member per line: the engine's file reader
takes the server down on a single line of 64 KiB or more, which a list of a few hundred
entries reaches if a tool minifies it to one line (see "What the engine imposes"). A `null`
or absent `expiresAt` is permanent, a timestamp is honored whatever it says (one in the past
lifts the ban), one that does not parse is treated as permanent so a typo cannot lift a ban,
and every text member is cut to the same bounds a dispatch gets. A file that does not parse
is left alone, enforces nothing, and makes the ban and unban actions refuse until it is
fixed or removed, which the log says at boot. The plugin's clock is a 32-bit epoch: a
duration that would end after 2038-01-19T03:14:07Z, or a timestamp written past it, is read
as that instant rather than wrapped into the past.

**Teleports** move the character with the engine's own position call, the one its restricted
area enforcement and its developer tooling use; a player seated in a vehicle is moved with the
vehicle and everyone in it, which the result reports as `vehicle`, and the result's `from`
and `to` describe whatever was moved (the vehicle, in that case, which is what arrives
beside a `toPlayer` target). A `position` is in the
frame the snapshots publish (`[x, y, z]`, `y` the elevation in metres); given as `[x, z]` it
is placed on the terrain at that point. A position off the map (outside `0` to the world
size on `x` and `z`) or above 10 000 m is refused, and one below the terrain, or below sea
level, is lifted up to it: measured on DayZ 1.29 while building this slice, the engine
leaves a character exactly where an explicit `y` puts it, 280 m under the ground included,
where it is stranded rather than moved. The engine's own teleport lifts to sea level only;
the terrain lift is the plugin's, and its cost is that an interior below the heightmap
cannot be a teleport destination. `toPlayer` puts the player 1.5 m beside the other player,
at that player's elevation rather than the terrain's, so a player standing on a floor is
not placed under it, and a target standing somewhere the plugin would refuse to send a
player (off the map after the step aside is refused too) fails the action rather than
copying the bad position. Every teleport records where what moved stood (the vehicle's
position when a vehicle was moved, the character's otherwise, so the record and the move
are in one frame and repeated undos swap between two places instead of drifting by a seat
offset) as that identity's previous position, one per identity, and `previous` goes back
there: the undo of a teleport, and a second `previous` undoes the undo. The record is in
memory only; a restart forgets it, and `previous` then fails until something has teleported
the player again.
The schema subset of protocol section 6.1 cannot say "exactly one of", so a dispatch naming
no destination, or more than one, is refused by the plugin.

**Spawn** creates one object of `className` on the ground 1.5 m in front of the player, with
the placement flags the central economy uses (traced to the surface, physics on, navmesh
updated) and the item's default quantity and condition; the inventory target, quantity,
health, and attachments belong to the spawning extension slice. The name must be made of
letters, digits, and underscores, and must be a public class (`scope` 2) declared in
`CfgVehicles`, `CfgWeapons`, or `CfgMagazines`; anything else is refused before the engine
is asked, with the reason in the result. That admits every vanilla and modded item, and also
vehicles, infected, and animals, which is what an event host wants; a blocklist arrives with
the spawning extension. `className` in the result is the class as the engine reports it.

**Set time** writes the world clock through the engine's `SetDate`, keeping the year, month,
and day and replacing the hour and minute; the clients follow the server's clock as they
always do. The result carries the clock as read back after the write, so what the engine
made of the request is what is reported: on DayZ 1.29 a request for 03:15 read back as
03:14 and 14:30 as 14:30, so expect the minute to land within one of what was asked.

**Unstuck** is what an admin does for a car wedged in a rock, rolled onto its roof, or sunk
into the terrain: the vehicle is raised by `lift` metres above where it stands, or above
the terrain or sea surface when it is below one, its pitch and roll are zeroed with the
heading kept (`level: false` keeps the orientation), its linear and angular velocity are
zeroed, its physics body is woken, and its state is synchronized to the clients. Whoever
is in it moves with it, the way a teleport moves a seated player, and the result lists
them as `crew`. A destroyed vehicle can be unstuck like any other.

**Delete destroyed vehicles** removes every vehicle whose damage state is destroyed, the
wrecks a long-running server accumulates: each is deleted through the engine's own safe
delete, which schedules the removal and synchronizes it to the clients. A wreck with
someone still seated in it is left alone and listed under `skipped` with the reason, and
`dryRun: true` lists what would be deleted without deleting anything, which is what to run
first on a server whose wrecks may be someone's base furniture. Intact vehicles are only
counted. The action is `destructive`: a deleted wreck's cargo goes with it.

**Vitals** sets one stat of one player to one value; `vyshka.heal` stays as the shortcut
for health, shock, blood, and bleeding together (it does not touch energy, water, stamina,
or the heat buffer). `health`, `blood`, and `shock` go through the damage system on the
character's global zone, the values the heal writes (`state.players` reports health and
blood, not shock);
`energy` and `water` are the character's own stats (the client learns of them through the
engine's hunger and thirst notifiers, as it does for a meal); `stamina` goes through the
stamina handler, which synchronizes it at once and brings a value above the player's
load-dependent cap down to that cap on its next tick; `heatBuffer` is the synced stat whose
HUD stage (the plus signs) the engine's own modifier recomputes on its next tick. The range
is read from the character at dispatch time, not fixed in the schema (a mod may change a
maximum, and the heat buffer runs from -30 to 30 while the others start at 0), so a value
outside it fails the action with the range in the error rather than being clamped
silently, and the result carries `min` and `max` beside `before` and `after`. The action is
`warning` because 0 health or 0 blood is death and 0 shock is unconsciousness. On vanilla
DayZ 1.29 the ranges read back as health 0 to 100, blood 0 to 5000, shock 0 to 100, energy
0 to 5000, water 0 to 5000, stamina 0 to 100, heat buffer -30 to 30.

**Stop bleeding** removes every bleeding source through the server-side bleeding manager,
the same call the heal makes, and reports how many there were. The engine treats a source
removed with no bandage as a wound that closed by itself and rolls a wound infection on
each (40% on 1.29); an admin's stop is a perfect bandage, so the action puts the wound
agent back to what it was before the removal and reports `infectionPrevented` when a roll
had hit (an infection the player already carried is kept). **Dry** sets every item in
the player's inventory tree (clothing, hands, cargo, and attachments alike) to its minimum
wetness and clears the player's own wet flag at once rather than waiting for the engine's
environment tick to notice the dry clothes. **Broken legs** with `broken: true` activates
the engine's broken-legs modifier, the path a hit that ruins a leg zone takes (the modifier
zeroes the leg zones, raises the fracture notifier, and sets the state the client animates;
one already active is reset first, which also takes a splint off; the modifier is activated
in the same frame rather than left to its request, which the engine honors on the manager's
next tick up to 3 s later, so the result's `after` describes what was done); `broken: false` restores
the four leg zones to full health and turns the modifier off, which is what the modifier
does by itself once both legs are back at full health, and removes a splint if one is worn.
**Bloody hands** sets or clears the flag the engine shows on the character's hands and
uses for the salmonella roll when eating.

**Flags** are five server-authoritative per-player switches set with one action: `god` (no
damage), `freeze` (no movement), `unlimitedStamina`, `unlimitedAmmo`, and `ignoredByAi`. A
flag belongs to the identity, not the character: the set is kept in the hub's key/value
store as `vyshka/flags.<Steam64>` (protocol section 12; the manifest declares the
namespace), so it survives a respawn, a reconnect, and a server restart, and follows the
identity to every server enrolled in the installation, because the store is
installation-wide (a player frozen on one server is frozen on the operator's other servers
too). The store is the truth and the game follows it: the action reads the identity's
record, merges the flags it names, writes the record back guarded by the revision it read
(a bot writing the same key in between makes it start over, three times at most), and only
then applies the set to the character if one is online and answers; a write the store
refuses fails the action and changes nothing in the game. Clearing the last flag writes the
record back with an empty set rather than deleting the key: the store's delete is
unconditional (protocol section 12.2), so a delete could erase a flag another writer set in
between, while the guarded write cannot; an operator who wants the key gone deletes it
through the Admin API (`DELETE /api/v1/kv/vyshka/flags.<Steam64>`; the panel's store view is
read-only). Every store call the action makes is bounded by the action's own TTL (and by 90
s at most), so a store that does not answer in time fails the action while the hub still
listens. A write abandoned at that deadline may still land at the hub, so an action that
failed for time is not proof the flags did not change: the plugin reads the record again
afterwards and the game follows whatever the store holds. A character is
given its identity's flags as it attaches (first join, respawn, or reconnect): what this
server process last knew at once, then the store's answer, which may have changed while the
player was away; a lookup the store does not answer is retried every 30 s while the player
is online. The record is `{ "flags": { "god": true }, "name", "updatedAt", "actionId" }`
with the set flags only, readable in the panel's key/value browser. The set flags also ride
`state.players` as `data.flags` (below).

What each flag does, with the engine's own mechanisms: `god` is `SetAllowDamage(false)`,
the call the engine's invincibility cheat makes, which also stops the bleeding manager
from opening a source (verified live: a firearm hit through the engine's direct damage
call left health at 100); `freeze` holds the input controller's movement-speed override at
zero, the override the engine's developer "server walk" drives a character with from the
server (verified live: held W and Shift+W moved the server position by nothing where the
same input had moved it 8.4 m in 2.5 s before; the client may show its own predicted step
snapping back); `unlimitedStamina` keeps the stamina handler from depleting and regenerates
as if idle whatever the character does, and fills the bar when set (verified live: twenty
seconds of sprint at 100 where eight unflagged seconds read 100 to 82; the client's own
predicted bar may dip and snap back at the engine's half-second sync); `unlimitedAmmo`
refills the magazine after every shot from the weapon's fire event on the server, the way
the engine's own debug option does at five rounds, and an internal magazine with cartridges
of the type just fired (verified through the weapon's fire event from the test rig: a
magazine at 28 read 27 after the round was taken and 30 after the event, and 28 after it
unflagged; a client-fired shot and an internal magazine were not exercised live). A weapon
with neither, a single-shot break-action such as the IZH-18, is not refilled: its one
round is the chamber itself, whose state the plugin leaves to the engine;
`ignoredByAi` answers the engine's AI-targeting question with no, which is what the
engine's diagnostic builds do for an untargetable character, so infected and animals do not
take the player as a target (verified live: the engine's answer for a spawned infected read
true, then false once flagged). The engine's own stamina and AI switches
compile only in its diagnostic builds, so the plugin reproduces them with overrides on the
stamina handler and the character. A frozen player seated in a vehicle drives it as usual:
the override is the character's movement, not the vehicle's controls. The action is
`warning` because god and freeze change what a player can do to and with others.

## Telemetry

The plugin publishes events (protocol section 8.1) and `state.players`, `state.vehicles`,
and `state.entities` snapshots (section 8.3) as soon as it is running; nothing needs
configuring. Player identity everywhere is
`{ "platform": "steam", "id": "<Steam64>" }` (section 8.2), the same id the heal action
takes as its `referenceKey`.

**Events.** Buffered and flushed as one `event.batch` every 2 s or at 200 events, then
persisted, acked, and renumbered like every other envelope. A crash loses the unflushed
tail of that buffer: up to 2 s or 200 events, plus the 200 ms tick that notices a flush is
due, for a script thread that keeps ticking (a stalled server holds its pending events for
the length of the stall). Measured by killing the server under load
(`spikes/dayz-outbox-crash`), that tail was the only loss: every batch that had reached
the outbox was delivered after the restart, none was torn, and the loss ran from
0.1 s to 2.1 s of events. A kill that lands inside the outbox's own
write of a batch would lose that batch too; it did not happen in those trials and the
restart discards the unreadable record. A poll carries at most 1000 events across its batches, the
reference hub's per-poll budget, so a backlog flushed after an outage is never refused
over it. A batch or snapshot the hub does refuse (`event.reject`, `state.reject`) is gone;
the plugin logs the hub's reasons as `ERROR` lines and carries on.

| Type | When | `data` |
|---|---|---|
| `core.server.start` | The plugin starts with the mission | `game`, `plugin { name, version }`, `world` |
| `core.server.stop` | The mission finishes; written to the outbox and delivered by the next boot, ahead of that boot's start event | same |
| `core.player.connect` | A character is attached to a newly connected identity (first join); a respawn or a reconnect inside the logout window is not a second connect | `player`, `name` |
| `core.player.disconnect` | The logout is final (a cancelled logout never fires it) | `player`, `name` |
| `core.player.death` | The character dies | `player`, `name`, `position`, `cause`, and where known `killer`, `killerName`, `weapon`, `distance`, `killerType`; when the hit that killed came through the hit hook first (the engine's order, measured; the one exception is a non-lethal round whose converted shock kills inside the engine's own part of the hook, which reads as a `self` death followed by the fatal hit), `bodyPart`, `ammo`, and `damageType` as on the damage event; when the engine names the character itself as the killer (`cause` `self` with no `weapon`: starvation, dehydration, bleeding out, drowning, a fall), the vitals at that moment: `water`, `energy`, `blood`, `bleedingSources`, and `submerged` (the head was under water, the engine's own eligibility check for drowning; an observation, since a submerged character can bleed out) |
| `core.player.damage` | The character takes a hit the engine reports to it (the hit hook, after the damage is applied), the fatal hit included; a hit on the corpse afterwards is not reported, and neither is a fall that cost no health (a fall is a health hit and a shock hit, and the admin log keeps the first only) | `player`, `name`, `position`, `cause` and its companions as on a death but named `attacker`, `attackerName`, and `sourceType`; `damageType` (`melee`, `firearm`, `explosion`, `stun`, or `other` for a vehicle, a fall, fire, or area damage: the engine's own categories); `bodyPart` (the engine's damage zone, `Torso`, `Head`, `Brain`, `LeftLeg`, ...); `ammo` (the engine's hit type, `Bullet_556x45`, `MeleeInfected`, `FallDamageHealth`, ...); `damage`, `blood`, `shock` (the engine's damage result for the hit, per health type: the larger of the highest value across the zones hit and the whole character's value, since on DayZ 1.29 the zone reading is 0 for a fall, which names no zone, and half the character's loss for a head hit; the figure for the hit, not a before-and-after of the vitals, so a hit that overshoots reports more than the character had left), `health` (left after the hit); `blocked` when the engine reports a hit with no damage result; `fatal` on the hit that killed |
| `core.player.chat` | A chat line reaches the server mission | `name`, `channel` (`direct`, `megaphone`, `transmitter`, `publicAddress`, `admin`, `system`, `battleye`, or `other`), `channelId` (the engine's raw channel value), `text`, and `player` when exactly one online player has that name (the engine names the sender, it does not identify them). A retail client's direct chat arrived with a channel value outside the engine's documented set on DayZ 1.29, so it reads `other`; `channelId` carries what the engine said |
| `core.player.kick` | A player is disconnected by `vyshka.kick`, or a banned identity is refused at connect | `player`, `name`, `reason`, `cause` (`action` or `ban`), `actionId` |
| `core.player.ban` | `vyshka.ban` records an identity | `player`, `name` when known, `reason`, `expiresAt` when not permanent, `actionId` |
| `core.server.fps` | Every `fpsIntervalSeconds` after the first interval | `fps` (the server's frame rate over the interval, one decimal, counted from the mission's update frames because the engine's own `GetFps()` reads a constant 0.1 on a dedicated server), `players` |
| `vyshka.player.unban` | `vyshka.unban` lifts a ban (a custom type: the core set has no unban) | `player`, `name` when known, `actionId` |
| `core.vehicle.destroy` | A vehicle's health reaches zero (the engine's kill hook on the vehicle), once per destruction: the hook fires again on every later hit on the wreck (measured on DayZ 1.29, a destroyed boat's decay tick fired it every 10 s), and the plugin reports the first, until the vehicle is deleted or its global health level leaves ruined (a repair, which the health-level hook reports as it happens) | `vehicle` (the snapshot id), `type`, `kind`, `position`, `crew` (who was in it), `cause` (`player` with `killer`, `killerName`, and `weapon`; `explosion` with `weapon`; `vehicle`; `self`; `other` with `killerType`; or `unknown`) |
| `vyshka.vehicle.enter` | A player's vehicle command starts: the character takes a seat (a custom type: the core set has no enter) | `player`, `name`, `vehicle`, `type`, `kind`, `position`, `seat` (the crew index), `driver` |
| `vyshka.vehicle.exit` | The vehicle command finishes (the character got out, or the command gave way to another, a death in the seat included), or a seated player disconnects; a seat switch inside the vehicle is not an exit, and the `seat` and `driver` reported are those of the seat actually left | the same, plus `cause` (`left` or `disconnect`) |

A moderation event's `actionId` is the hub's id for the dispatch that caused it, so a feed
entry can be joined to the action record and the audit log. `core.vehicle.spawn` is not
emitted: the hive initializes every persisted vehicle at boot through the same hook a new
one arrives by, and a feed entry per vehicle on every restart is noise, not news.

`cause` is one of `player` (another player, bare hands or a held item; `killer` names them,
`weapon` is the item's display name, `distance` in metres is present for a firearm's shot),
`self` (the engine names the character as its own killer: starvation, dehydration, bleeding
out, drowning, a fall), `infected`, `animal`, `explosion` (`weapon` is the device),
`vehicle`, `other` (with `killerType`, the engine class of the killer: a fireplace or barbed
wire arrives here, since the engine's area damage names the object itself as the source,
and `ammo` on the hit says how), or `unknown`. This reading of the killer object matches
the one the engine's own admin log makes, and the damage event reads the hit's source the
same way with `attacker`, `attackerName`, and `sourceType` in place of the killer words.

A damage event per hit is the busiest telemetry the plugin publishes: an infected fight is
a hit every second or two, and a firefight more. The batching absorbs it, but a webhook
subscribed to `core.player.*` now receives every hit; subscribe a kill feed to
`core.player.death` alone.

**Snapshots.** The plugin captures the full list of characters with an identity attached,
alive or not, and the full list of vehicles, as it builds each poll request, so the samples
ride that very request instead of waiting behind a held poll. `snapshotIntervalSeconds`
(default 10) is a floor on the spacing between captures of each type, not a timer: a poll
built sooner than that after the last capture carries none, and when the interval is shorter
than the poll cycle it is the poll cycle that sets the cadence (see below):

```json
{ "capturedAt": "2026-09-11T14:00:00Z",
  "players": [ { "player": { "platform": "steam", "id": "7656..." }, "name": "Survivor",
                 "position": [4231.5, 300.2, 10620.0],
                 "data": { "alive": true, "health": 100, "blood": 5000,
                           "flags": { "god": true } } } ] }
```

`data.flags` lists the admin flags in effect on the character, the set ones only, and is
absent when none is; the panel shows them as badges beside the name.

```json
{ "capturedAt": "2026-09-16T23:00:00Z",
  "vehicles": [ { "id": "0-2147", "kind": "car", "position": [6512.3, 284.6, 7498.1],
                  "data": { "type": "OffroadHatchback", "displayName": "ADA 4x4", "seats": 4,
                            "crew": [ { "player": { "platform": "steam", "id": "7656..." },
                                        "name": "Survivor", "seat": 0, "driver": true } ] } } ] }
```

A vehicle's `id` is the engine's network id for the object (its high and low halves joined
with a dash), which the engine keeps for the object's whole life on this server; a restart
is a new life and the ids start over. `kind` is `car`, `boat`, or `helicopter` by the
scripted base the vehicle extends, and `vehicle` for one that extends none of them. The
list is the plugin's own, kept by hooks on those three bases (`CarScript`, `BoatScript`,
`HelicopterScript`; their common engine parent cannot be modded from script): every vanilla
vehicle and every modded one built on them is in it, one that extends the engine's `Car`,
`Boat`, or `Helicopter` directly is not. The damage state (`intact`, `destroyed`,
`exploded`) and the fluids arrive with the vehicles 2 slice; `vyshka.deletedestroyed` with
`dryRun` says today which are wrecks.

```json
{ "capturedAt": "2026-09-18T10:00:00Z",
  "entities": [ { "id": "sample:beacon:nwaf", "kind": "beacon",
                  "position": [4600.0, 340.0, 10400.0],
                  "data": { "label": "Northwest Airfield", "landmark": "nwaf" } } ] }
```

`state.entities` carries the map markers mods place through `VyshkaMapMarker` ("Writing a
mod against the plugin" below): anything on the map that is not a player or a vehicle. The
plugin itself places none, so the snapshot is published once at boot (empty, so a marker
the previous process placed is cleared from the hub, which keeps the latest snapshot
across a restart), then only while a marker exists, and once more, empty, when the last
one is removed; a server with no mod that places markers sends one empty snapshot per
boot and nothing after. `id` is whatever the mod chose (the convention is
`<namespace>:<thing>`), `kind` its label for the kind of thing, and `data.label` the
display name the panel shows.

When a poll has room for fewer snapshots than are due (the hub has made the plugin shrink
its batch), the types take turns: the type that went last time waits for the others, so
none is left behind for the rest of the session; with room for all, all go on every poll.

No capture is made while the previous snapshot of that type is still unacked, or while the outbox holds
more than one poll can carry. A snapshot says what *is*, so a stale one waiting behind an
outage is worth nothing, an envelope already in the outbox cannot be replaced by a fresher
one (it may already have been stored, and the retransmission is then deduplicated), and a
buffer full of superseded snapshots would crowd out the events and action results that are
worth keeping; the hub keeps the latest per type regardless, with `capturedAt` saying how
stale it is. On a healthy link the ack for the previous snapshot arrives in the response that
ended the last poll, so a skipped capture means an outage or a backlog, and a run of them
long enough to matter is logged.

The cadence on a healthy link with quiet, fully held polls is therefore one snapshot per
poll cycle (`pollTimeout` plus a round trip) when the interval is shorter than that, and
otherwise the interval rounded up to the next poll:

```
capture gap  ~=  poll cycle * ceil(snapshotIntervalSeconds / poll cycle)
```

Receipt lags capture by about a round trip, because the hub applies a poll's envelopes
before it begins to hold. A poll the hub answers early (because it had something to
deliver) shifts that grid rather than shortening it: the re-poll that follows may be built
before the interval has elapsed, so it carries no snapshot and is then held in full, and the
gap runs to the interval plus a cycle. So with successful polls, room for a snapshot at
every poll, and no session renewal in between, a gap lies between the interval and the
interval plus the longest time between two polls being built, which for held polls is about
`pollTimeout` plus transport and processing time; only quiet, fully held polls sit on the grid
above. A backlog that fills the batch, a failed poll, or a session renewal extends the gap
past that, and the floor is kept by the running plugin, not across a restart.

Measured on a live server against a local hub with `pollTimeout` 25 and
`snapshotIntervalSeconds` 10, no players online: twenty-one snapshots, so twenty gaps, of
which 25 s eighteen times and 26 s twice; receipt lagged capture by under a second
(`capturedAt` has whole-second resolution), and no capture was held.

Plugin 0.2.0 captured on a timer instead and skipped the capture while the previous snapshot
was unacked, which put two waits between captures (the poll already in flight, then the hold
on the poll that carried the ack) and measured 50 s between snapshots at `pollTimeout` 25 and
`snapshotIntervalSeconds` 10 (issue #55).

`core.server.stop` is emitted when the mission finishes, which a graceful shutdown reaches
and a process kill does not: a server killed from the outside leaves no stop event.

**Positions** are the engine's own vector, `[x, y, z]` with `y` the elevation in metres, so
a DayZ map plots `x` against `z`. That is the game's own map frame of section 8.3; the hub
never interprets it and a map view has to know the game.

## Writing a mod against the plugin

Any server mod loaded after `@Vyshka` can add its own actions, events, contexts, key/value
namespaces, and map markers, and they arrive at the hub in the same manifest and the same
telemetry as the plugin's own. The surface is five things, all in Enforce Script and all
server-side; `sample/` is a complete mod built on them, which builds and loads with nothing
installed but the game and the plugin, and is meant to be copied.

**Load order and the `VYSHKA` define.** The plugin's `config.cpp` declares `defines[] =
{ "VYSHKA" }`, which the engine defines for every mod loaded after it. Wrap everything that
names a Vyshka class in `#ifdef VYSHKA ... #endif`, and your mod compiles and loads on a
server that does not run the plugin (it registers nothing and says so in the log). Do not
put `Vyshka` in your `requiredAddons`: that would make the plugin a hard dependency, which
the guard exists to avoid. The define is only visible to mods loaded after the plugin, so
the server's mod list must put `@Vyshka` first:

```
DayZServer_x64.exe ... -serverMod=@Vyshka;@VyshkaSample
```

**Registration.** The plugin's modded `MissionServer` has a `VyshkaRegister(VyshkaRegistry
registry)` method it calls once, from `OnInit`, before the link starts, so what you register
there is in the first manifest the server publishes. Override it, call `super` first:

```c
#ifdef VYSHKA
modded class MissionServer
{
	override void VyshkaRegister(VyshkaRegistry registry)
	{
		super.VyshkaRegister(registry);
		registry.Register(new SampleBeaconAction());
		registry.RegisterContext(new SampleLandmarkContext());
		registry.DeclareEvent("sample.beacon.placed", "Beacon placed", "sample", null);
		registry.DeclareNamespace("sample");
	}
}
#endif
```

`Register` takes a `VyshkaAction` subclass: override `Code()` (`<namespace>.<name>`,
unique on the server), `Name()`, `Context()` (`world`, `player`, `vehicle`, `object`, or a
context id you registered), `Namespace()`, `Danger()` (`none`, `warning`, `destructive`),
`ParamsSchema()` (the protocol's schema subset, section 6.1; the hub validates a dispatch
against it before it reaches the server), and `Execute(actionId, context, referenceKey,
params)`, which returns `VyshkaActionOutcome.Success(result)`, `.Failure(reason)`, or
`.Pending()` when the outcome comes later, through `VyshkaPlugin.Complete(actionId,
outcome)` (a store round trip is the usual reason; bound that work by
`VyshkaPlugin.CurrentDeadlineMs()`, and the plugin fails the dispatch itself when the
completion never comes). `RegisterContext` takes a `VyshkaContext` subclass (`Id()`,
`Name()`, `Namespace()`, and `Enumerate(VyshkaContextList list)`, which fills the list with
`list.Add(referenceKey, label)` or `list.AddAt(referenceKey, label, position)`); the plugin
answers the hub's `context.enumerate` requests for it (protocol section 6.2), and an action
in that context gets one of those reference keys as its `referenceKey`; a context offering
more than a reply may carry (5000 entries, 256 KiB) is answered with no entries and a
reason rather than a list that reads as complete. `DeclareEvent`
declares a custom event type for display and webhook filtering (declaration is advisory;
an undeclared type is stored too). `DeclareNamespace` names a key/value namespace your mod
will read and write: the manifest's `kvNamespaces` is the sole source of the plugin's
store access (section 6.6), so a store call in a namespace nobody declared fails at once.

**`GetVyshka()`** is the global accessor for the link: `Version()`, `LinkState()`
(`connected`, `degraded`, `buffering`, or `stopped`), `IsRunning()`, `Emit(type, data)` to
queue a telemetry event (`<namespace>.<name>`, a `VyshkaJsonValue` object or `null`;
dropped when the plugin is not running), `Store(namespace)` for a store handle, and
`Mark(...)` for a map marker. Events ride the same buffer and outbox as the plugin's own,
and the hub keeps them under your namespace, so a webhook can subscribe to `sample.*`.

**`VyshkaStore`** is a handle bound to one key/value namespace (protocol section 12): `new
VyshkaStore("sample")`, then `Get(key, callback)`, `Set(key, value, ifRevision, callback)`,
and `Delete(key, callback)`, each taking a `VyshkaStoreCallback` subclass whose `OnStore`
runs once with the result (`m_Ok`, `m_Found`, `m_Value`, `m_RevisionText`, `m_Mismatch`,
`m_Error`), and an optional deadline in monotonic milliseconds. The store is the hub's and
installation-wide: what one server writes, every enrolled server reads, which is what the
admin flags use it for. Pass `""` as `ifRevision` for an unconditional write, or the
revision text a get returned to write only if nobody else has since; revisions travel as
text because they may exceed the engine's 32-bit int.

**`VyshkaMapMarker`** puts something on the panel's live map that is not a player or a
vehicle: `VyshkaMapMarker.Place(id, kind, label, position, data)` registers or replaces the
marker with that id (use `<namespace>:<thing>` ids so mods cannot collide), the returned
marker has `Move(position)`, `SetData(data)`, and `Remove()`, and `VyshkaMapMarker.Find(id)`
finds one. Markers ride `state.entities` (above), captured with the other snapshots, and
they live in memory: a restart starts with none, so a mod that wants its markers back
places them again from its own state. The set is bounded by what one snapshot may carry
(5000 entries, 256 KiB, section 8.3): a placement, a move, or a data change that would pass
either is refused with a log line and the marker keeps what it had, so a capture can never
build a body the hub rejects. `Place` returns `null` when it refused (a marker already on
the map is then unchanged; `Find` gives it back), and a mod that reports or counts a
placement checks that before it does. The marker keeps a copy of the `data` it was given,
so changing the object afterwards changes nothing on the map.

**Manifest revision.** The hub replaces its stored manifest only for a higher revision
(protocol section 6.1), and which mods are loaded changes the manifest, so the revision is
not a constant. The plugin keeps `<profiles>/Vyshka/manifest.json` with the last content it
published (as the manifest object itself, one member per line, since a record holding it as
one string ran past the engine's line reader at a few hundred actions; a record written by
plugin 0.8.0 that way is still read) and the revision it used: unchanged content
republishes at the same revision, and
changed content takes the larger of the stored revision plus one and the current epoch
second. The hub reports the revision it holds with every session (`server.manifestRevision`,
section 5.3), and a plugin whose revision is not above it moves to the hub's plus one, so
a wiped profile directory or a clock set back cannot leave the manifest stranded below what
the hub holds. A revision minted by the plugin is marked `pending` in the record, with the
hub revision it was published above, until a later session (a renewal, hourly, or the next
boot) reports that very number: the hub got there through this publish, so the content is
accepted and the mark is cleared. An ack alone is not acceptance, since the hub acks a
manifest it rejected as well, and a hub that reported no revision (it holds none, or it
predates the field) is not known to have stood below, so a publish sent to it leaves no
mark and the number is accepted only after one more publish above the hub's first report.
While a revision is pending, an equal number reported by a
hub it was not published above is taken for the collision it may be and the content goes
out above it, across restarts too; a record from before the mark existed reads as pending,
and a `manifest.publish` an earlier boot left in the outbox is published above as well. The lists are published in a fixed order (by code or id), so the load order
of the mods does not change the content. There is nothing to do when you add or change a
mod; the next boot publishes. The registry refuses what the hub would reject the whole
manifest over, with a log line: an action past 500, a context past 100, an event past 500,
a namespace past 100, an event id over 128 characters, a context id over 64.

**What the sample shows.** `sample/` declares the namespace `sample`, a context
`sample.landmark` with two fixed entries (Green Mountain and the Northwest Airfield), an
action `sample.beacon` in that context that places a `VyshkaMapMarker` at the landmark,
emits `sample.beacon.placed`, counts the placement in the store under
`sample/beacons.<landmark>` with a guarded write, and completes with the count; and its
`config.cpp` requires only the game's own addons. Its README says how to build and load
it. Anything in it can be renamed and kept.

## Conformance

The plugin conformance harness (`conformance/plugin`) can drive a real DayZ server:

```
go run ./plugins/dayz/cmd/vyshka-dayz build
go run ./conformance/plugin -enroll-wait 240s -check-timeout 30s -- go run ./plugins/dayz/cmd/vyshka-dayz harness
```

`harness` reads the hub URL and token the harness puts in the environment, writes them into
a fresh profile directory under `plugins/dayz/build/`, starts the server from its default
install location (`-server` overrides it) with the built mod, mirrors the plugin's log lines
to stderr, and stops the server when the harness closes its stdin. The `-enroll-wait` covers
the server's boot time. This needs a local DayZ dedicated server install, so CI does not run
it; CI runs the Go tooling's tests and the reference driver instead. With no player on the
server the telemetry stage grades the start event and the empty snapshots; the player events
need a client to join, which the staging demo in issue #49 covers.

`-extra-mod <dir>` loads another built mod after `@Vyshka` (repeat it for several), which is
how the sample is graded: with `-extra-mod plugins/dayz/build/@VyshkaSample` the manifest
carries the sample's action, event, and context, and the `context.enumerate` stage has a
context to enumerate. Without a mod that declares one the stage reports `PART`.

The harness grades the plugin from the wire. What it cannot see is the plugin's own file
and JSON handling against the engine's limits (the next section), so that half has a
self-test of its own:

```
go run ./plugins/dayz/cmd/vyshka-dayz build
go run ./plugins/dayz/cmd/vyshka-dayz selftest
```

`selftest` derives a mission whose `init.c` carries `selftest/VyshkaSelfTest.c`, boots a
server on it under `plugins/dayz/build/selftest-profile` with the plugin idle (no config),
and grades the lines the script prints, one per check, the way the conformance suites
report: a ban list of 400 entries, a manifest record of 250 KB, and an outbox record of
84 KB written and read back whole through the plugin's classes (each past the reader's old
limit as one line); a document with a single 70 KB string refused rather than written; a
100 000-character string value and a 56 000-character one dense with escapes parsed whole;
and the 5 000-entry pull shape of issue #80 (1.1 MB) serialized, parsed, written, and read
inside a 30 s budget each, with the milliseconds in the report. A check that faults the
process ends the run, which the tool reports as a failure with the engine's crash log,
since that is the failure the file checks exist to catch. Run it after any change to
`VyshkaJson.c` or `VyshkaFiles.c`; it needs the same local server install as the harness,
so CI does not run it.

## What the engine imposes

The live-player heal acceptance demo passed on 2026-09-05; [issue #14](https://github.com/That1Drifter/vyshka/issues/14)
records the results and validation limits. Readable Plugin API errors landed on 2026-09-10
([issue #43](https://github.com/That1Drifter/vyshka/issues/43)); see "Errors and recovery"
below for what the plugin does with each refusal and what an operator will find in the log.

Measured under `spikes/` rather than assumed; the details are in each spike's findings.

- **TLS works.** The engine's REST client speaks HTTPS to a TLS-terminating reverse proxy
  with a publicly trusted certificate and needs nothing configured for it; the `deploy/`
  layout, which exposes no plain-HTTP port, was exercised end to end on 2026-09-10 (issue
  #13). Whether it accepts a private CA is unmeasured. When testing with a local client on
  the same machine, launch it through `DayZ_BE.exe`: starting `DayZ_x64.exe` directly gets
  the player kicked by BattlEye about 20 s after joining, even with `BattlEye = 0;` in the
  server config.
- **Poll hold time.** The engine aborts a request after 10 s by default, as a budget for the
  whole response. The plugin raises the read timeout to `pollTimeout` + 5 s at session start
  (`spikes/dayz-restapi-poll-timeout`).
- **Bearer token.** `RestContext` exposes only a content-type header, written to the wire
  verbatim, so the plugin appends a CRLF and the `Authorization` line to it. Undocumented
  engine behavior; a patch that changed it would make every poll fail with `401`, which the
  log shows as "poll refused" (`spikes/dayz-restapi-headers`).
- **Error bodies are invisible, so the plugin asks for them inline.** A 4xx reaches script as
  error code 5 with no body, a 5xx as code 6, an unreachable hub as 7, a timeout as 8. Every
  request therefore carries `?errors=inline` (protocol section 2.3), which makes a hub deliver
  its refusals as a `200` whose body is the protocol error with the status inside; the query
  string survives the engine's client on both `GET` and `POST` (`spikes/dayz-restapi-headers`,
  finding 7). Against a hub that predates the option, or a proxy answering in its place, the
  refusal is still opaque and the plugin falls back to reasoning from the error class. Both
  paths are described under "Errors and recovery".
- **The credential travels on a `POST` alone.** The engine writes the content-type header,
  and so the `Authorization` line smuggled after it, only on a `POST`; a `GET` reaches the
  wire with `Accept: */*` and nothing else (the same stub logs, read again for issue #71).
  The plugin therefore never issues a `GET`, `PUT`, or `DELETE` against the hub: its
  key/value calls use the Plugin API's POST spellings (protocol section 12.2), which exist
  for this client.
- **Two contexts run at once.** `GetRestContext` returns one context per base URL string,
  and a request on a second context completes while the first holds a long-poll open: the
  store client's get then set finished in under 300 ms under a held 25 s poll (measured on
  DayZ 1.29 during the admin flags run). One request in flight per context is the plugin's
  own rule, so a store call never waits behind a poll.
- **Strings cost their length, files are read by the line.** `string.Get(i)` and
  `Substring` pay about 0.2 µs per KiB of the string they read from on every call,
  whatever the index; `+=` pays for the string's current length; `Substring` returns at
  most 8 191 characters, silently; and the engine's line reader faults the process, with
  nothing script can catch, on a line of 65 536 bytes or more (`spikes/dayz-bans-pull-size`,
  issue #108). So the plugin's parser reads its input through windows cut once and never
  copies a value with one `Substring`, its serializer collects pieces and joins once (the
  5 000-entry pull shape of #80, 1.1 MB, parses in 439 ms and serializes in 577 ms where
  the first parser took 353 s and 45 s), and every JSON file it writes goes out one array
  element and one object member per line, refused with an error rather than written when
  a single string value would still make a line over 60 000 bytes. `selftest` (above)
  proves each of those on a local server. Reading a large body is still not free (about
  0.4 µs a byte on this engine), which is why the pull of #80 is paged.

## Errors and recovery

What the plugin does with each refusal, per protocol section 2.3. Every line below goes to
the script log with the hub's own message.

| Refusal | What the plugin does |
|---|---|
| `session_invalid` on a poll (superseded, expired, revoked) | Starts one new session. The outbox is kept and renumbered. |
| `envelope_invalid` on a poll | The hub applied nothing. The envelope at `details.index` is moved from `outbox/` to `rejected/<time>-<n>.json` with the hub's reason, the rest of the batch closes the gap and is resent at once. An `ERROR` line names the envelope and the file. It is never resent and never counted as delivered. |
| A `200` whose `error` member is not an object with a code | Treated as malformed, not as success: no ack applied, same session, retry after backoff. |
| `ack_out_of_range` on a poll | Starts a new session, which resets the sequence space. |
| `bad_request` on a poll | Halves the batch size and retries the same session. |
| `credentials_invalid`, `credentials_revoked` | Stays on the same credentials and retries every 30 s, logging that a fresh enrollment token in `config.json` is the fix. Never re-enrolls on its own. |
| `enrollment_token_invalid`, `enrollment_token_used`, `game_mismatch` | Logs that the token needs replacing and retries every 60 s. |
| `protocol_version_unsupported` | Logs that the hub or the plugin needs upgrading and retries every 5 min. |
| Any 5xx | Retries the same request with backoff (1 s doubling to 30 s). |
| A `200` that is not JSON | Retries after backoff on the same session; nothing changes. |
| An unrecognized code | By status: 401 as `session_invalid`; any other 4xx is backed off and retried on the same session; 5xx as above. |

**Opaque refusals** (an older hub, or a proxy's own error page): script sees "client error"
and nothing else. A poll refused on a session that has already polled successfully is taken
as a lost session and answered with one new session. A poll refused on a session that has
never polled is more likely the plugin's own batch, so the plugin backs off 30 s and retries
the same session, and only opens another session after a second refusal there. The worst
case is one new session a minute with a log line saying why; a malformed envelope cannot be
identified without the inline error, so it stays in the outbox until the hub is upgraded.
Session and enrollment refusals are handled as before: credentials rejected, retry every
30 s; token refused, tell the operator, retry every 60 s.

The `rejected/` directory is never read by the plugin. Delete its files once you have looked
at them.
- **Durability.** The outbox is one file per unacked envelope under the profile directory,
  written before the envelope is first sent and deleted when the hub's ack covers it. The
  engine exposes no fsync. Against a process kill (a crash, a `taskkill`, a watchdog) the
  file is in the OS page cache once `CloseFile` returns, and the spike under
  `spikes/dayz-outbox-crash` found every record intact and delivered after the restart in
  twenty kills, with the loss confined to the event buffer's unflushed tail (see
  "Events" above). A kill inside the write itself would leave an unreadable record, which
  the next boot discards and counts, and the batch in it is gone. A batch delivered but
  not yet acked when the server dies is on disk and at the hub both; the restart re-sends
  it and the hub drops it as a duplicate, so nothing is stored twice. What a power loss or
  an OS crash takes from the page cache is not measured and cannot be shortened from
  script.
  Sequence numbers are assigned at send time, never stored, which is what makes renumbering
  across a session change (spec section 9.1) automatic. Executed action ids are persisted to
  `executed.log` (append-only, JSON-encoded so an opaque id cannot split a record) and reloaded
  on boot, so a crash between executing an action and acking its dispatch does not let the
  re-delivery run it again. Two residual windows the engine cannot close, and which the hub
  absorbs by deduplicating: if a file deletion fails (an antivirus or backup lock), an acked
  envelope may be re-sent after a restart and the hub drops it as a duplicate; and if the OS
  loses the tail of `executed.log` in a crash, a re-delivered action may run twice.
- **Whole-second clock.** The engine's UTC clock is whole-second, so an action's `expiresAt`
  is compared at second granularity. The plugin discards an action once the current second
  reaches the deadline second, which never runs an action past its deadline but can discard one
  up to a second early. Harmless against a TTL measured in seconds; the hub is the authority on
  expiry regardless.
- **Integers are 32-bit.** Sequence numbers, acks, and epoch seconds live in script ints.
  Sequence spaces restart with every session, so this is not a practical bound; epoch seconds
  are for deadline comparisons and for seeding the manifest revision, which a script int
  holds until 2038.
