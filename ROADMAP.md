# Roadmap

**As of:** 2026-09-22 (the world slice of #78 landed as protocol draft 0.30, `state.world`
the first snapshot that is not a list, with a weather action carrying every engine knob,
fixed and stored presets, and freeze time; the vehicles 2 slice of #77 landed with no protocol change, the
vehicle's damage state, health, and fluids in its snapshot and refuel and repair beside
unstuck; the presets of #76 landed as protocol draft 0.29, the schema subset
gaining the `kvNamespace` annotation and the panel's key/value view becoming an editor,
with operator-defined weather presets moved to #78; the spawning extension of #75 landed as protocol draft 0.28, the
schema subset gaining its one form of `not`; the inventory actions of #74 landed, the first action result
measured against its cap; the item catalog of #73 landed as protocol draft 0.27, closing the
hub side of `context.enumerate`; the server binding of #81 landed as draft 0.26 and the
plugin fixes of #108 on 2026-09-21; the shape of #80 settled in its issue, its pull spike
measured, and the plugin's string and file defects filed as #108; the mod surface and its
sample landed 2026-09-18). Last full review 2026-09-14, at which every proposed item was settled. This document lists
where Vyshka stands and what comes next, in order. It is the public companion to the
milestone table kept with the design notes; the milestone letters (M0 to M4) are the same
in both places.

How to read it. The states are defined in `CONTEXT.md`; in brief:

- **Shipped** means merged on `main`, graded by the conformance suites or the browser test,
  and where it involves a game, demonstrated against a live DayZ 1.29 server.
- **Committed** means an issue is open for it. It will be built; only the order can move.
- **Parked** means wanted, not scheduled, and carrying a named trigger written next to it.
  When the trigger fires the item becomes committed.
- **Tabled** means wanted in principle with no trigger. Only a roadmap review reopens it,
  and nothing else on the roadmap may depend on it.
- **Dropped** items are listed under "Not on the roadmap" with their reason.

There are no proposed items after the review of 2026-09-14. A new idea enters as proposed
and must leave that state at the next review.

Every committed item is delivered as a slice: one issue, one branch, the spec change first
where there is one, the conformance suite grading the new behavior, the two-round
adversarial review, and a `CHANGELOG.md` entry in the landing commit. Engine limits that
constrain a plugin are measured under `spikes/` before anything is designed around them.

The ordering rule is **DayZ first, to the first tagged release**. The design persona is one
operator or small team running one hub with a handful of game servers enrolled in it, the
servers usually on other boxes than the hub, and a few admins who do not all hold the same
trust. A hosting outfit running servers for other people is out of scope until 1.0.

## Where things stand

| Milestone | Deliverable | Status |
|---|---|---|
| M0 | Spec, envelope schemas, OpenAPI; conformance suites run against stubs | Shipped |
| M1 | Hub core: enrollment, sessions, long-poll, manifests, action lifecycle, SQLite | Shipped |
| M2 | DayZ plugin (clean-room) | Shipped 2026-09-05; readable inline errors landed 2026-09-10; moderation slice landed 2026-09-14 |
| M3 | Telemetry, state snapshots, webhooks, KV store; Discord kill feed from a live server | Shipped 2026-09-14 (the staging repeat of the Discord demo is the last box) |
| M4 | Scoped tokens, audit log, panel v1 (servers, actions, event feed, live map) | Shipped 2026-09-15: panel v1 with a live DayZ player plotted on the staging map 2026-09-12 (issue #46), and the management views landed 2026-09-15 (issue #64) |
| 1.0 | Protocol freeze | After at least one third-party plugin has been written from the spec alone |

The first tagged release is out: hub 0.1.0 (`hub-v0.1.0`, static binaries for Linux, macOS,
and Windows plus the container image) and DayZ plugin 0.7.0 (`dayz-plugin-v0.7.0`), both
on 2026-09-17 (issue #69). Releases are described in `RELEASING.md`.

M5 (custom contexts, Arma Reforger plugin, `--auto-tls`) was dissolved on 2026-09-14:
custom contexts moved to Horizon 1 with a DayZ customer, `--auto-tls` to Horizon 2, and the
Reforger plugin was tabled (see Horizon 4). The player identity shape froze the same day
(protocol draft 0.22, section 8.2) on DayZ evidence, with the platform registry left open.

The hub runs on SQLite by default and on Postgres since 2026-09-13. The protocol document
is at draft 0.29 and is pre-1.0: it can still change shape where Horizon 3 says so.

## Horizon 1: close M4 and ship the first release

These are the next slices, in the order they are expected to land. The first five gate the
release.

1. **Panel management views** (#64, M4), landed 2026-09-15: register a server and mint
   or revoke its credentials; Admin API tokens with role bundles (moderator, event host,
   owner) as scope-set presets; webhooks with deliveries, dead letter, and replay; the
   audit log with its filters; the key/value store by namespace, read-only; pinned quick
   actions per server. Closed M4: an operator with zero API knowledge can run everything
   from the panel. The webhook edit, pause, and replay endpoints and the two key/value
   listings arrived with it as protocol draft 0.23.
2. **Outbox across a server crash** (#65, spike), landed 2026-09-16: the server was killed
   under event load twenty times across three load and poll settings; every outbox record
   on disk was intact and delivered after the restart, nothing was stored twice, and the
   loss was the event buffer's unflushed tail alone, 0.1 s to 2.1 s of events. The number
   demanded no code change; the loss window is now stated with its measurement in the
   plugin README and `spikes/dayz-outbox-crash`. A power loss is not measured and cannot be
   closed from script.
3. **Position and world actions** (#66), landed 2026-09-16: teleport to coordinates, to a
   player, and to previous (one saved position per identity as the undo); spawn one item
   near a player; set time. Teleport and spawn were out of scope for the tracer bullet and
   never rescheduled until then. Each was verified live on DayZ 1.29 with a retail client;
   the engine was found to leave a character wherever an explicit `y` puts it, so the
   plugin lifts a position below the terrain up to it.
4. **Vehicles 1** (#67), landed 2026-09-16: `state.vehicles` snapshots (the plugin's own
   vehicle list, since the engine keeps none script can read) with enter, exit, and
   destroy events; delete all destroyed, with a dry run; unstuck. The panel's map plots the
   vehicles beside the players and hands a vehicle to the vehicle-context actions. Wreck
   cleanup is the routine chore on a long-running server and unstuck is the most-requested
   action in every admin tool.
5. **Vitals** (#68), landed 2026-09-17: one action with a `stat` parameter (health, blood,
   shock, energy, water, stamina, heat buffer) and a value the engine's own range for that
   character bounds, plus stop bleeding, dry, and the broken-legs and bloody-hands toggles,
   the legs through the engine's own broken-legs modifier. The generalization of
   `vyshka.heal`, which stays as the shortcut for health, shock, blood, and bleeding
   together. Each was verified live on DayZ 1.29 with a retail client.
6. **First release** (#69), tooling landed 2026-09-17: a release workflow on per-artifact
   tags (`hub-v<version>`, `dayz-plugin-v<version>`) that builds the hub's static binaries
   for Linux, macOS, and Windows, grades the shipped Linux binary with the hub conformance
   suite, publishes the archives with checksums and the container image for amd64 and
   arm64, and publishes the DayZ plugin's `@Vyshka` folder from a `.pbo` build that is now
   byte-reproducible; a systemd unit under `deploy/`; per-artifact sections in
   `CHANGELOG.md`; the steps in `RELEASING.md`. **Tagged 2026-09-17**: `hub-v0.1.0` and
   `dayz-plugin-v0.7.0`, both from `main` after a rehearsal run of each job whose archives
   matched a local rebuild byte for byte. No Steam Workshop item: the plugin is a
   server-side mod, so the release zip is the distribution. The two items parked on this
   trigger became committed: #96 (client libraries) and #97 (the conformance suites as a
   published tool).

After the tag, in this order:

7. **Damage telemetry** (#70), landed 2026-09-17: `core.player.damage` from the
   character's hit hook with the source read as a death's killer is, the engine's damage
   type, body part, hit type, the damage and the health left, and `fatal` on the hit that
   killed; the death carries that hit's body part and hit type, and the natural-death
   breakdown (water, energy, blood, bleeding sources, submersion) when the engine names the
   character itself as the killer. The `discord` template words both.
8. **Admin flags** (#71), landed 2026-09-17: `vyshka.flags` sets god mode, freeze,
   unlimited stamina, unlimited ammo, and ignored by AI per identity, online or not; the
   set lives in the key/value store (the store's first real customer, through the Plugin
   API's new POST spellings, protocol draft 0.24), so it survives a respawn, a reconnect,
   and a restart and follows the identity across the installation's servers, and rides
   `state.players` as `flags`, which the panel shows as badges. God, freeze, stamina, and
   AI were verified live on DayZ 1.29; ammo through the weapon's fire event from the test
   rig. The spike (`spikes/dayz-admin-flags`) could not show invisibility or no-collision
   from the server with one client: the character draws and collides on the client, and
   whether a server-side `SetInvisible` reaches other clients needs a second one to see.
   Both stay parked with that trigger.
9. **Mod surface with a self-contained sample mod** (#72), landed 2026-09-18: a mod
   loaded after `@Vyshka` overrides the modded `MissionServer`'s `VyshkaRegister` hook to
   register `VyshkaAction` subclasses, `VyshkaContext` subclasses (the plugin answers the
   hub's `context.enumerate` for them, protocol draft 0.25 section 6.2), declared events,
   and key/value namespaces, all inside `#ifdef VYSHKA` guards the plugin's `defines[]`
   makes true, so the mod loads without the plugin; `GetVyshka()` gives the link state and
   `Emit`, `VyshkaStore("my-mod")` a namespace-bound store handle, and `VyshkaMapMarker`
   a map object that rides `state.entities`, which the panel's live map now plots. The
   manifest revision is derived from the content and persisted, since the mod set changes
   it. The sample under `plugins/dayz/sample/` declares one action, one event, and one
   context with two fixed entries, depends on nothing but the game, and was booted with and
   without the plugin on DayZ 1.29; the plugin conformance suite gained the
   `context.enumerate` stage.
10. **Item catalog as the first custom context** (#73), landed 2026-09-22: the plugin
    declares eight catalog contexts (firearms, optics, ammunition, magazines, edibles,
    clothing, gear, vehicles), each enumerating the public classes of that type with the
    translated display name and the type's declared stats, built once per session on the
    first request; the hub gained the missing half of protocol section 6.2 as draft 0.27
    (`GET .../contexts/{contextId}/entries` sends `context.enumerate`, holds for the
    echoing reply, caches it 10 s against the manifest revision, shares one question
    between concurrent reads, and refuses a context the manifest does not declare or a
    server with no live session without asking) and the schema subset the `context`
    annotation that names which contexts a string param draws on; the panel enumerates
    every context a form names and suggests the entries, so the spawn form autocompletes
    on names an operator recognizes. The split by type comes from
    `spikes/dayz-item-catalog`: a stock 1.29 server's 2050 items are 243 KiB as one list,
    7 percent under one reply's bound, and 107 KiB for the largest type. Loadout forms
    (#76) get the same suggestions for free through the annotation.
11. **Inventory** (#74), landed 2026-09-22: `vyshka.inventory.read` returns one player's
    inventory tree as an action result (a request with a result, not a snapshot, so a
    snapshot keeps meaning a whole list the plugin pushes on its own cadence): the held
    item, every worn item, and inside each its attachments and cargo, with condition,
    quantity, rounds, liquid, and food stage; `vyshka.inventory.strip` (`warning`) drops it
    all beside the player through the engine's own drop and `vyshka.inventory.clear`
    (`destructive`) deletes it all through the engine's safe delete. The first result
    checked against a cap with a real payload: the bound that applies to a result is the
    64 KiB of protocol section 7 (the 256 KiB figure is the snapshot body cap), and
    `spikes/dayz-inventory-tree` measured a heavy loadout built from named stock classes
    at 131 items and 14 KiB, a quarter of the action's budget, so a character carrying
    that much is answered whole and the action's cut (one level of containers less until
    it fits, the counts kept) is for far larger loads, modded containers first of all.
    Each action was verified live on DayZ 1.29 with a retail client.
12. **Spawning extension** (#75), landed 2026-09-22: `vyshka.spawn` takes `into` (the
    ground, the inventory through the engine's own placement search, or the hands), a
    `quantity` and a `health` bounded by the item's own ranges, and `attachments: auto`,
    which loads a firearm through the engine's spawn-with-ammo call and fills every slot
    with the first compatible part the engine accepts in a fixed order. The operator's
    `spawnBlocklist` rides the action's params schema as `not`/`enum`, the one form of
    `not` the schema subset now admits (protocol draft 0.28), so the hub refuses a blocked
    name before it is queued, the panel marks it in the catalog's suggestions, and any
    third-party action gets the same by writing the same clause; the plugin also refuses
    it in any case. `spikes/dayz-spawn-attachments` measured `auto` on all 115 stock
    firearms (104 loaded, at most 3 ms each) and found the six classes whose weapon state
    machine never runs. Verified live on DayZ 1.29 with a retail client.
13. **Presets** (#76), landed 2026-09-22: loadouts captured from a player's gear and
    applied to a player (dressing them, or stripping them first), named teleport
    locations with a scatter radius, and vehicle presets that spawn a car with the parts
    and fluids it needs to drive (the engine started on the live run). Each kind is a
    key/value namespace of its own, so editing presets is a grant apart from dispatching
    them and apart from the admin flags, and the read-only browser from #64 became an
    editor: create only, save guarded by the revision opened, delete confirmed. The
    protocol gained the `kvNamespace` annotation (draft 0.29), so a form offers the names
    the store holds without the panel knowing any game. The store's first
    operator-written data. Operator-defined weather presets moved to #78, which builds
    the weather action they feed. Capture from a placed object was left out: vehicles
    are the only placed objects with ids today, and nothing asked for it yet.
14. **Vehicles 2** (#77), landed 2026-09-22: refuel (any of a car's four fluids, a boat's
    fuel, to a level) and repair (the engine's full-health call, which lifts a destruction,
    on the vehicle and its parts, with a ruined wheel swapped back to its intact class),
    and the intact, destroyed, and exploded states in the snapshot beside health and
    fluids; exploded is a destruction by an explosion's hit, since the engine names the
    vehicle itself as the killer of a blast. The live run found that every restart had
    reported each saved wreck as a new destruction (the hive loads it, runs its kill hook,
    and deletes it), which the slice fixed. No protocol change: the fields ride `data`.
    Delete-all-unclaimed depends on an ownership mod and became a documented mod-surface
    example instead of a plugin action, compiled and dispatched on a live server.
15. **World** (#78), landed 2026-09-22: `state.world` (protocol draft 0.30), the first
    snapshot that is one object rather than a list, carrying the game's clock and whatever
    the plugin reports about its world, shown on the panel's server page and above every
    world-context form. The DayZ plugin publishes the weather in it and adds
    `vyshka.weather`, one action with every engine knob optional (the four phenomena, the
    wind, the dynamic fog, the storm, the rain and snowfall thresholds, a transition, a
    hold, and a behaviour mode: the map's own controller, the engine's, or held), a fixed
    preset enum (clear, cloudy, storm), and operator presets in a `vyshka.weather`
    namespace applied through the same action; and `vyshka.time.freeze`.
    `spikes/dayz-world-clock` measured the clock stopping under the engine's time
    multiplier, a set value lasting only its hold time under the map's controller (which
    closes Chernarus's snowfall again), and every value holding under the frozen update
    except rain outside its threshold. Whether a connected client's clock stops with the
    server's was not measured.
16. **Hub features an in-game menu cannot offer** (#79): a player profile per identity
    across every server on the installation, honestly a 30-day view until a per-identity
    roll-up exists (event retention is 30 days, chat 90); per-webhook redaction of named
    fields so one death event feeds an admin channel with coordinates and a public kill
    feed without them; audit entries as webhook material.

Two protocol discussions run alongside, because both change the spec and the persona needs
them early:

- **Installation-wide ban list** (#80): hub-managed, with the per-server `vyshka.ban`
  staying the primitive. Shape settled in the issue on 2026-09-21: a hub-owned list with
  its own revision, a `bans.changed` notification, a paged pull over plain HTTP, a
  `bans.applied` report, a `capabilities` manifest member, and the closed-set scopes
  `bans:read` and `bans:manage`. The pull is paged because `spikes/dayz-bans-pull-size`
  (2026-09-21) measured the plugin's parser as quadratic on this engine and the file
  reader as fatal on a 64 KiB line; the plugin fixes landed the same day as #108 (a
  windowed parser, a chunked writer, files written one element per line, a self-test on a
  local server, and a large-dispatch stage in the plugin conformance suite). Lands after
  #81.
- **Server-scoped token dimension** (#81, protocol section 10.1), landed 2026-09-21 as
  protocol draft 0.26: a token-level `servers` binding that intersects every grant, not a
  per-scope field, so a moderator for server A no longer acts on server B. `admin` and
  `webhooks:manage` are refused on a bound token (and `bans:manage` will be, with #80);
  `kv:rw` is allowed with the store stated as installation-wide. A bound token is refused
  at the headers on every route naming a server outside the binding, on the action read
  once the action's server is known, and sees a filtered server list. The hub conformance
  suite grades it (`admin.tokens.serverBinding`, including a stalled-body probe that a
  refused token is not left waiting on a body), the panel's mint form has the server picker,
  and every token minted before the draft stays unbound. Role bundles in #64 now have a
  server term to use.

## Horizon 2: operations, as promised in the design notes

Section 12 of the design notes describes an operational surface that is still mostly on
paper. Each is a small slice on its own, after the release unless the release needs it.

- **`/metrics` for Prometheus** (#82): poll latency, queue depths, action outcomes by code,
  dropped envelopes, webhook failure rate, and the sweep counters. The hub already counts
  most of these; the endpoint and the naming are the work.
- **Backup endpoint with retention tooling** (#83): `POST /api/v1/admin/backup` taking a
  consistent SQLite snapshot, a documented Postgres story, a command that reports table
  sizes and what the sweeps will remove, and an explicit `vacuum` for SQLite. Its restore
  runbook is the small-team answer to a hub outage.
- **Config file** (#84): one TOML file as an alternative to flags and environment variables,
  with `file:` indirection for secrets. Flags stay.
- **`--auto-tls`** (#85): embedded ACME. Under the design persona the game servers live on
  other boxes than the hub, so every plugin connects over TLS and this is the default
  deployment story, not a single-box convenience.
- **Rate limits per credential** (#86): per-server and per-token bounds on the Plugin and
  Admin APIs. Hardening, not scaling: the Plugin API is internet-facing, so a leaked server
  credential must be bounded in what it can do to the hub.
- **Measured capacity** (#87, spike): replaces the "20 servers at 100 players" estimate,
  which has no provenance, with a number under `spikes/`. That number is also the trigger
  for the parked multi-instance hub and what the rate-limit defaults are set against.

## Horizon 3: protocol items the spec names and the hub does not implement

Gaps between `spec/protocol.md` and the reference hub, listed with their section so the
spec reader is not surprised.

| Item | Section | Status |
|---|---|---|
| Custom contexts and `context.enumerate` | 6.2 | Shipped 2026-09-22 (#73, draft 0.27): the hub sends `context.enumerate` behind an Admin API read, caches the answer, and the hub conformance suite grades it (`admin.contexts.enumerate`); the DayZ item catalog is the first customer and the panel's spawn form the first consumer |
| WebSocket transport at `/plugin/v1/ws` | 3.2 | Committed (SHOULD), no issue yet. Deliberately after a plugin exists that can use it; DayZ cannot. Sidecar plugins are the customer |
| Position telemetry cadence | 8.3 | Parked. The plugin captures a snapshot as it builds each poll, so the cadence equals the poll cycle (25 s measured at `pollTimeout` 25; the earlier 50 s figure from #55 was plugin 0.2.0 and is fixed). Only a second channel beats that. Trigger: the WebSocket transport lands. A position-only snapshot at the same cadence would save bytes, not time, and is not planned |
| Server-scoped token dimension | 10.1 | Shipped 2026-09-21 (#81, draft 0.26): a token-level `servers` binding |
| Cursor over webhook deliveries | 11.3 | Parked. Trigger: a delivery list exceeds the maximum `limit` in practice. Today the remedies are a wider `limit` and shorter retention |
| A poll that says "more queued" is answered at once | 3.1 | Committed (#91), post-release. Today a backlog drains at the per-poll event budget per poll cycle (40 events/s at `pollTimeout` 25), because the hub holds a poll it has nothing to answer with even when the plugin cut its batch at the budget. Measured in `spikes/dayz-outbox-crash` |
| Snapshot diffs after the first full snapshot per session | 8.3 | Parked. Trigger: a real plugin hits the 256 KiB body cap. Unknown-type tolerance makes a diff form a backward-safe addition |
| Platform registry (player identity) | 8.2 | Shape frozen at draft 0.22; the registry stays open and a new game adds its platform identifier additively |
| Multi-instance hub (claims or leases for sweeps and the webhook dispatcher) | design notes 12 | Parked. Trigger: a single installation exceeds the capacity measured in #87. One hub per database is the supported deployment on both engines until then; the hub is one process however many boxes the game servers occupy |

## Horizon 4: the ecosystem and the second game

- **Arma Reforger plugin**: tabled 2026-09-14; issue #15 closed. Wanted in principle, no
  trigger, and nothing on this roadmap depends on it: the identity shape froze on DayZ
  evidence and protocol 1.0 gates on a third-party plugin rather than on this project
  writing a second one. A roadmap review reopens it.
- **Admin API client libraries** (#96): committed 2026-09-17, when the release tooling
  landed. Generated from `spec/openapi-admin.yaml`, Go and TypeScript first. After the
  post-release DayZ slices unless a consumer asks sooner.
- **Conformance suites as a published tool** (#97): committed 2026-09-17, the same
  trigger. A versioned binary third-party implementers can run without cloning this
  repository, and a badge policy for what "conformant" may claim.
- **Protocol 1.0**: committed, no date. The freeze happens after at least one third-party
  plugin has been written from the spec alone. Until then `v` stays at 1 with draft
  numbering, and every envelope-level change is discussed in an issue first.

## Parked items with their triggers

Collected here so no trigger is lost in a horizon:

| Item | Trigger |
|---|---|
| Chat-triggered actions (a hub rule dispatching an action when a whitelisted identity's chat matches a pattern, audited as that identity) | An operator asks for in-game commands. It is a rule engine, and rule engines grow |
| Random infected or animals spawned near a point | An event host asks |
| Entities and base building (`state.entities`, set position and orientation and health on ids, duplicate, build and dismantle with a no-materials option, `dayz.build.place` and `dismantle` events) | The mod surface has a customer that places map objects. `state.entities` is the likeliest snapshot to hit the 256 KiB cap. The build and dismantle hook spike rides along with any plugin slice |
| Invisibility and no-collision admin flags | A two-client measurement (the #71 spike had one) shows a server-side `SetInvisible` hides the character from another client, or the client-side simulation can be told to ignore geometry from the server |
| Position telemetry cadence | The WebSocket transport lands |
| Cursor over webhook deliveries | A delivery list exceeds the maximum `limit` in practice |
| Snapshot diffs | A real plugin hits the 256 KiB body cap |
| Multi-instance hub | A single installation exceeds the capacity measured in #87 |

## Not on the roadmap

Non-goals from the design notes, plus what the 2026-09-14 review dropped, so none is
re-proposed by accident:

- Multi-tenant SaaS. Single operator, many game servers. Nothing forbids wrapping the hub in
  multi-tenancy later; it is not designed for it.
- Process supervision, mod deployment, file management. Vyshka is not a game server manager.
- An RCON or BattlEye replacement. The hub complements them.
- Interoperability with any existing commercial product's mods, endpoints, or accounts.
- Unaudited admin actions. Some in-game admin menus offer a "no log" variant of an action;
  Vyshka's audit log is unconditional and no action, scope, or flag may suppress an entry.
- Client-side admin tooling: ESP overlays, free cameras, spectating, admin night vision. The
  hub's equivalents are the live map and the snapshots; anything drawn in the player's own
  client is a mod's business, not the plugin's.
- Dropped 2026-09-14: soft delete with a backup copy for key/value namespaces (the backup
  endpoint covers the loss case); `actions:batch` dispatch (no customer since #6, and it
  weakens one job, one audit entry); a `talking` flag in `state.players` (voice lasts
  seconds, the snapshot cadence is the poll cycle); the inbound Discord bridge (a bot is a
  client of the Admin API like any other, which is what the client libraries are for; the
  outbound half shipped as the `discord` webhook template); "delete all unclaimed vehicles"
  as a plugin action (depends on an ownership mod; documented as a mod-surface example);
  the "unattended operation" gate (undefined, replaced by the first tagged release).

## Keeping this document honest

Update this file in the same commit as any change that shifts an item between states, and
date the header. A new idea enters as proposed and leaves that state at the next review.
When a slice lands, move its line into "Where things stand" and delete it from its horizon.
The changelog records what happened; this file records what is intended.
