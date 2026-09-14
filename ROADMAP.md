# Roadmap

**As of:** 2026-09-14. This document lists where Vyshka stands and what comes next, in
order. It is the public companion to the milestone table kept with the design notes; the
milestone letters (M0 to M5) are the same in both places.

How to read it:

- **Shipped** means merged on `main`, graded by the conformance suites or the browser test,
  and where it involves a game, demonstrated against a live DayZ 1.29 server.
- **Committed** means the protocol already specifies it, a milestone's definition of done
  requires it, or an issue is open for it. It will be built; only the order can move.
- **Proposed** means it is a candidate that has not been decided. It is listed so the
  discussion has a fixed text to argue with. Proposed items become committed by getting a
  GitHub issue, and they may be dropped instead.

Every committed item is delivered as a slice: one issue, one branch, the spec change first
where there is one, the conformance suite grading the new behavior, the two-round
adversarial review, and a `CHANGELOG.md` entry in the landing commit. Engine limits that
constrain a plugin are measured under `spikes/` before anything is designed around them.

The ordering rule since 2026-09-11 is **DayZ first, end to end**. The second game (Arma
Reforger, M5) is on hold until the DayZ path is complete enough that an operator could run
it unattended.

## Where things stand

| Milestone | Deliverable | Status |
|---|---|---|
| M0 | Spec, envelope schemas, OpenAPI; conformance suites run against stubs | Shipped |
| M1 | Hub core: enrollment, sessions, long-poll, manifests, action lifecycle, SQLite | Shipped |
| M2 | DayZ plugin (clean-room) | Shipped 2026-09-05; readable inline errors landed 2026-09-10; moderation slice landed 2026-09-14 |
| M3 | Telemetry, state snapshots, webhooks, KV store; Discord kill feed from a live server | Shipped 2026-09-14 (the staging repeat of the Discord demo is the last box) |
| M4 | Scoped tokens, audit log, panel v1 (servers, actions, event feed, live map) | Panel v1 shipped, with a live DayZ player plotted on the staging map 2026-09-12 (issue #46); the panel's management views remain |
| M5 | Custom contexts, Arma Reforger plugin, `--auto-tls` | Not started; Reforger on hold |

The hub runs on SQLite by default and on Postgres since 2026-09-13. The protocol document is
at draft 0.21 and is pre-1.0: it can still change shape where the open questions below say
so.

## Horizon 1: close M4 and make the DayZ path unattended

These are the next slices, in the order they are expected to land.

### 1.1 Panel management views (committed, M4)

M4's definition of done is "an operator with zero API knowledge can run everything from the
panel". Today the panel signs in, lists servers, dispatches actions, shows the event feed,
and plots the live map; everything else still needs `curl`. The views that close the gap,
each a thin page over an Admin API surface that already exists:

- Register a server, mint its enrollment token, revoke its credentials (protocol section 5.1
  and 5.4).
- Manage Admin API tokens: issue with scopes, list, revoke (section 10.4).
- Manage webhooks: register, edit, pause, read deliveries and the dead letter, replay one
  (sections 11.2 and 11.3).
- Read the audit log with its filters (section 10.5).
- Browse the key/value store by namespace (section 12), read-only first.

### 1.2 DayZ plugin: the rest of the operator toolkit (committed in spirit, one issue per group)

The plugin has heal, kick, ban, unban, message, broadcast, the core player events, chat,
fps, and `state.players`. The groups that turn it from a demonstration into a daily tool:

- **Position and world actions**: teleport (to coordinates, to another player), spawn item
  on or near a player, set weather and time. Teleport and spawn were named as out of scope
  for the tracer bullet and never rescheduled.
- **Damage and combat telemetry**: `core.player.damage` with source and body part, hit
  events, and the distance already carried on deaths. Named as a later slice in #49.
- **Vehicle telemetry and state**: `state.vehicles` snapshots (type, position, occupants)
  and vehicle enter, exit, and destroy events. The map's marker layer is ready for a second
  state type.
- **Position telemetry cadence**: `state.players` lands every two poll cycles (50 s
  measured, #55). A faster cadence or a lighter position-only snapshot is a plugin and
  protocol question together; see the snapshot diff item under Horizon 3.
- **Restart resilience**: the file-backed outbox has an undocumented fsync window and no
  measured behavior across a server crash. A spike that kills the server mid-batch and
  counts what the hub received is the prerequisite for calling unattended operation safe.

The groups below are **proposed** as of 2026-09-14. They come from a survey of what
established in-game admin menus for DayZ offer and Vyshka does not, translated into plugin
actions, events, and snapshots. Each becomes committed by getting an issue; several can fold
into the committed groups above when those are sliced.

- **Vitals and admin flags** (proposed): set one vital at a time (health, blood, shock,
  energy, water, stamina, heat buffer) as a single action with a `stat` parameter; stop
  bleeding, dry, and the broken-legs and bloody-hands toggles. Per-player admin flags (god
  mode, invisibility, freeze, unlimited ammo and stamina, admin night vision, ignored by AI,
  no collision) persisted per identity so they survive a reconnect, and reported as a
  `flags` object in `state.players` so the panel shows who has one set.
- **Inventory** (proposed): strip (drop everything, `warning`) and clear cargo (delete
  everything, `destructive`); an on-request `state.inventory` snapshot for one player, which
  is the first snapshot that needs the 256 KiB body cap checked against a real payload.
- **Spawning beyond the basics** (proposed): spawn into a target inventory with quantity and
  health, an `attachments: auto` option that fills a weapon's compatible attachments from
  config, random infected or animals near a point for event hosts, and a server-side
  blocklist of class names that the manifest advertises so the panel greys them out.
- **Loadouts and teleport locations** (proposed): named item trees captured from a player's
  current gear or a placed object and applied to a player, a point, or a target; named
  teleport locations with a scatter radius; teleport-to-previous as an undo. Both lists live
  in the key/value store and get a panel editor, which makes them the store's first real
  customers. Vehicle spawn presets that include the parts a car needs to drive (wheels,
  battery, plugs) belong to the same family.
- **Vehicle operations** (proposed, alongside the committed telemetry): intact, destroyed,
  and exploded states in `state.vehicles`; delete one, delete all destroyed, delete all
  unclaimed where an ownership mod defines that; refuel, unstuck (lift and reset), and
  repair as vehicle-context actions. Wreck cleanup is the routine chore on a long-running
  server and unstuck is the most-requested action in every admin tool.
- **Weather and world beyond set-time** (proposed): every engine knob (overcast, static and
  dynamic fog, rain, snow, storm, wind direction and magnitude, thresholds, behaviour mode)
  as optional fields on one action; freeze time; named presets with clear, cloudy, and storm
  shipped; and a `state.world` snapshot of current time and conditions, which the panel
  needs before a weather form makes sense.
- **Entities and base building** (proposed): set position, orientation, and health, heal,
  delete, and duplicate on ids from `state.entities`; build, dismantle, and repair base
  parts, with a build-without-materials option; and `dayz.build.place` and `dismantle`
  events naming who built what where.
- **Telemetry extras** (proposed): the natural-death breakdown (water, energy, bleeding
  sources at the moment of death) as extra fields on `core.player.death` when the cause is
  not a player, and a `talking` flag in `state.players` for voice transmissions.
- **Item catalog** (proposed): the plugin publishes its class-name catalog with display
  names and per-type stats (clothing, edibles, firearms, magazines, optics, vehicles) once
  per session, so spawn and loadout forms autocomplete on names an operator recognizes. See
  the custom-contexts row under Horizon 3: this is a candidate first customer for
  `context.enumerate`.

### 1.3 The third-party mod surface on DayZ (committed by section 13 of the design notes)

The point of the manifest is that other mods add actions and events without touching the
plugin. The surface is designed and not yet built: `class MyAction extends VyshkaAction`
registered from a modded `MissionServer` hook, `#ifdef VYSHKA` guards so mods load without
the plugin, a `GetVyshka()` accessor for link state, `VyshkaStore("my-mod")` for the KV
store, and `VyshkaMapMarker` for map objects that are not players. Ships with a sample mod
that declares one custom action and one custom event, and a page in `plugins/dayz/README.md`.

Two candidates for the sample mod (proposed): a map-specific world-event manager (list the
events a map mod defines, start one, cancel one), because it exercises a custom context with
`context.enumerate` and a mod-declared action together; or the item catalog from 1.2, which
exercises the context half alone.

### 1.4 Hub and panel features an in-game menu cannot offer (proposed)

The same survey turned up things that only a hub with history across servers can do. None
has an issue yet.

- **Player profile**: a page per identity with its history across every server on the
  installation (connects, deaths, kicks, bans, chat, admin actions taken against it) and
  operator notes. Every input is already in the event store and the audit log; the work is
  the query and the page.
- **Installation-wide ban list**: a hub-managed list pushed to every enrolled server's
  plugin, with the per-server `vyshka.ban` staying the primitive. Needs a sync action or a
  new envelope type, so it is a protocol change discussed in an issue first.
- **Per-webhook redaction**: a webhook option that strips named fields (positions, killer
  position) from events before delivery, so one `core.player.death` feeds both an admin
  channel with coordinates and a public kill feed without them.
- **Admin actions as webhook material**: treat audit entries as webhook-eligible events so an
  operations channel sees who did what to whom, not only what the game reported.
- **Roles as scope bundles**: named presets (moderator, event host, owner) when issuing an
  Admin API token in the panel. Convenience over the existing scopes, no protocol change.
  A future draft may also narrow scopes by action-code prefix; that belongs with the
  server-scoped token dimension under Horizon 3.
- **Chat-triggered actions**: a hub rule that dispatches an action when a whitelisted identity
  sends a chat line matching a pattern, audited as that identity. Keeps the hub as the only
  dispatch path instead of adding in-game commands that bypass it.
- **Small conveniences**: pinned quick actions per server in the panel, and a soft delete
  with a backup copy for key/value namespaces.

### 1.5 Packaging and first release (proposed)

Nothing has been tagged. The first release is the point where "self-hosted, single binary"
is a download rather than `go build`:

- Tagged hub releases with static binaries for Linux, Windows, and macOS, and a published
  container image. The `Dockerfile` and compose file exist; the release automation does not.
- A systemd unit under `deploy/`, which the design notes promise and the tree lacks.
- The DayZ plugin published to the Steam Workshop, with the `.pbo` build reproducible from
  the repo.
- Per-artifact SemVer as `CHANGELOG.md` already anticipates: protocol, hub, DayZ plugin.

## Horizon 2: operations, as promised in the design notes

Section 12 of the design notes describes an operational surface that is still mostly on
paper. Each of these is a small slice on its own.

- **`/metrics` for Prometheus** (committed): poll latency, queue depths, action outcomes by
  code, dropped envelopes, webhook failure rate, and the sweep counters. The hub already
  counts most of these internally; the endpoint and the naming are the work.
- **Backup endpoint** (committed): `POST /api/v1/admin/backup` taking a consistent SQLite
  snapshot, and a documented Postgres story. Today a backup is "stop the hub and copy the
  file".
- **Config file** (committed): one TOML file as an alternative to flags and environment
  variables, with `file:` indirection for secrets. Flags stay; the file is for operators who
  run more than one hub.
- **`--auto-tls`** (committed, M5): embedded ACME for the single-box case, so the nginx
  snippet is optional rather than required.
- **Resource bounds** (proposed): per-token and per-server rate limits on the Admin and
  Plugin APIs, and a documented capacity number replacing the "20 servers at 100 players"
  estimate with a measurement.
- **Retention tooling** (proposed): a command that reports table sizes and what the sweeps
  will remove, and an explicit `vacuum` for SQLite. Retention itself already runs.

## Horizon 3: protocol items the spec names and the hub does not implement

These are gaps between `spec/protocol.md` and the reference hub. They are listed with their
section so the spec reader is not surprised.

| Item | Section | Status |
|---|---|---|
| Custom contexts and `context.enumerate` | 6.2 | Committed (M5). The hub accepts context declarations in the manifest and stores them; it never sends `context.enumerate` and the panel has no dropdown to feed. Needs a plugin with a real custom context to grade against; the item catalog and the world-event manager proposed under 1.2 and 1.3 are DayZ-side candidates that would unblock this without waiting for the second game |
| WebSocket transport at `/plugin/v1/ws` | 3.2 | Committed (SHOULD). Deliberately after a plugin exists that can use it; DayZ cannot. Sidecar plugins and Reforger are the customers |
| Server-scoped token dimension | 10.1 | Proposed for a future draft. Scopes are installation-wide today; a term that names a server is the one narrowing operators keep asking for and the spec explicitly forbids a hub from inventing it |
| Cursor over webhook deliveries | 11.3 | Proposed for a future draft; today the remedies are a wider `limit` and shorter retention |
| Snapshot diffs after the first full snapshot per session | 8.3 | Deferred with a tripwire: a real plugin hitting the 256 KiB body cap reopens it. Unknown-type tolerance makes a diff form a backward-safe addition |
| `actions:batch` dispatch | 7 | Proposed; named as a follow-up when #6 landed and not needed by anything since |
| Platform registry freeze (player identity) | 8.2 | Blocked on the second game. Stays open while Reforger is on hold |
| Multi-instance hub (claims or leases for sweeps and the webhook dispatcher) | design notes 12 | Proposed. One hub per database is the supported deployment on both engines until this is designed |

## Horizon 4: the second game and the ecosystem

- **Arma Reforger plugin** (committed, M5, on hold since 2026-09-11, issue #15): the same
  tracer scope as the DayZ one, long-poll first, then the identity registry entry for
  Bohemia accounts, then WebSocket if a sidecar proves necessary. This is the test that the
  protocol is game-agnostic; it is deliberately not started until the DayZ path is complete.
- **Admin API client libraries** (proposed): generated from `spec/openapi-admin.yaml`, Go
  and TypeScript first, published with the release. The panel is the only client today.
- **Discord bridge** (proposed): the architecture diagram lists a Discord bridge among the
  admin tools. The outbound half exists as the `discord` webhook template. The inbound half,
  a bot that dispatches actions from a channel through a narrowly scoped token, is a separate
  process and a separate repository if it happens at all.
- **Conformance suites as a published tool** (proposed): a versioned binary third-party
  implementers can run against their hub or plugin without cloning this repository, and a
  badge policy for what "conformant" may claim.
- **Protocol 1.0** (committed, no date): the freeze happens after the second game confirms
  the identity shape and after at least one third-party plugin has been written from the
  spec alone. Until then `v` stays at 1 with draft numbering, and every envelope-level
  change is discussed in an issue first.

## Not on the roadmap

These are non-goals from the design notes, restated so they are not re-proposed by accident:

- Multi-tenant SaaS. Single operator, many game servers. Nothing forbids wrapping the hub in
  multi-tenancy later; it is not designed for it.
- Process supervision, mod deployment, file management. Vyshka is not a game server manager.
- An RCON or BattlEye replacement. The hub complements them.
- Interoperability with any existing commercial product's mods, endpoints, or accounts.
- Unaudited admin actions. Some in-game admin menus offer a "no log" variant of an action;
  Vyshka's audit log is unconditional and no action, scope, or flag may suppress an entry.
- Client-side admin tooling: ESP overlays, free cameras, spectating. The hub's equivalents
  are the live map and the snapshots; anything drawn in the player's own client is a mod's
  business, not the plugin's.

## Keeping this document honest

Update this file in the same commit as any change that shifts an item between the three
states, and date the header. When a proposed item gets an issue, replace "proposed" with the
issue number. When a slice lands, move its line into "Where things stand" and delete it from
its horizon. The changelog records what happened; this file records what is intended.
