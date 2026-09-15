# Roadmap

**As of:** 2026-09-14 (roadmap review; every proposed item settled). This document lists
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

M5 (custom contexts, Arma Reforger plugin, `--auto-tls`) was dissolved on 2026-09-14:
custom contexts moved to Horizon 1 with a DayZ customer, `--auto-tls` to Horizon 2, and the
Reforger plugin was tabled (see Horizon 4). The player identity shape froze the same day
(protocol draft 0.22, section 8.2) on DayZ evidence, with the platform registry left open.

The hub runs on SQLite by default and on Postgres since 2026-09-13. The protocol document
is at draft 0.23 and is pre-1.0: it can still change shape where Horizon 3 says so.

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
2. **Outbox across a server crash** (#65, spike): the file-backed outbox has an
   undocumented fsync window. Kill the server mid-batch, count what the hub received, fix
   what the number demands. The release cannot claim crash safety without it.
3. **Position and world actions** (#66): teleport to coordinates, to a player, and to
   previous (one saved position per identity as the undo); spawn one item near a player;
   set time. Teleport and spawn were out of scope for the tracer bullet and never
   rescheduled until now.
4. **Vehicles 1** (#67): `state.vehicles` snapshots with enter, exit, and destroy events;
   delete all destroyed; unstuck. Wreck cleanup is the routine chore on a long-running
   server and unstuck is the most-requested action in every admin tool.
5. **Vitals** (#68): one action with a `stat` parameter (health, blood, shock, energy,
   water, stamina, heat buffer), plus stop bleeding, dry, and the legs and hands toggles.
   The generalization of `vyshka.heal`.
6. **First release** (#69): hub 0.1.0 as static binaries for Linux, Windows, and macOS and
   a published container image; a systemd unit under `deploy/`; the DayZ plugin on the
   Steam Workshop with the `.pbo` build reproducible from the repo; per-artifact SemVer in
   `CHANGELOG.md` (protocol draft, hub, DayZ plugin).

After the tag, in this order:

7. **Damage telemetry** (#70): `core.player.damage` with source, weapon, body part, and
   distance; hit events; the natural-death breakdown (water, energy, bleeding sources) on
   `core.player.death` when there is no killer.
8. **Admin flags** (#71): god mode, freeze, unlimited stamina and ammo, ignored by AI, set
   per player, persisted per identity in the key/value store so they survive a reconnect,
   and reported as `flags` in `state.players`. Includes a spike on whether invisibility and
   no-collision can be done server-side; they stay parked until it says yes.
9. **Mod surface with a self-contained sample mod** (#72): `class MyAction extends
   VyshkaAction` from a modded `MissionServer` hook, `#ifdef VYSHKA` guards, `GetVyshka()`,
   `VyshkaStore("my-mod")`, `VyshkaMapMarker`. The sample declares one custom action, one
   custom event, and one custom context with two hard-coded entries, and depends on nothing
   else, because a sample that needs another mod installed cannot be copied.
10. **Item catalog as the first custom context** (#73): the plugin publishes its class-name
    catalog with display names and per-type stats once per session; the hub gains the
    missing half of protocol section 6.2 (sending `context.enumerate`, caching the answer)
    and the panel its dropdown. Spawn and loadout forms then autocomplete on names an
    operator recognizes.
11. **Inventory** (#74): `vyshka.inventory.read` returns one player's inventory tree as an
    action result (a request with a result, not a snapshot, so a snapshot keeps meaning a
    whole list the plugin pushes on its own cadence); strip (`warning`) and clear cargo
    (`destructive`). The first body to check against the 256 KiB cap with a real payload.
12. **Spawning extension** (#75): spawn into a target inventory with quantity and health;
    `attachments: auto`; the class-name blocklist expressed in the action's parameter schema
    so the panel greys entries out with no manifest change and third-party spawn actions
    get it for free.
13. **Presets** (#76): loadouts captured from a player's gear or a placed object; named
    teleport locations with a scatter radius; vehicle spawn presets with the parts a car
    needs to drive; operator-defined weather presets. All live in the key/value store, so
    the read-only browser from #64 becomes an editor here. The store's first real customers.
14. **Vehicles 2** (#77): refuel, repair, and intact, destroyed, and exploded states in the
    snapshot. Delete-all-unclaimed depends on an ownership mod and becomes a documented
    mod-surface example instead of a plugin action.
15. **World** (#78): `state.world` first (the panel needs current time and conditions
    before a weather form makes sense), then one action with every engine knob optional, a
    fixed preset enum shipped in the plugin (clear, cloudy, storm), and freeze time.
16. **Hub features an in-game menu cannot offer** (#79): a player profile per identity
    across every server on the installation, honestly a 30-day view until a per-identity
    roll-up exists (event retention is 30 days, chat 90); per-webhook redaction of named
    fields so one death event feeds an admin channel with coordinates and a public kill
    feed without them; audit entries as webhook material.

Two protocol discussions run alongside, because both change the spec and the persona needs
them early:

- **Installation-wide ban list** (#80): hub-managed, pushed to every enrolled server, with
  the per-server `vyshka.ban` staying the primitive. Needs a sync action or a new envelope
  type; the shape is settled in the issue before any slice.
- **Server-scoped token dimension** (#81, protocol section 10.1): scopes are
  installation-wide, so a moderator for server A can act on server B. With a few admins of
  unequal trust that is the narrowing the spec anticipates and forbids a hub from inventing.
  The issue settles the term and whether scopes also narrow by action-code prefix; the
  implementation follows after the release. Role bundles in #64 will then have a server
  term to use.

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
| Custom contexts and `context.enumerate` | 6.2 | Committed (#73). The hub accepts context declarations in the manifest and stores them; it never sends `context.enumerate` and the panel has no dropdown to feed. The item catalog is the customer |
| WebSocket transport at `/plugin/v1/ws` | 3.2 | Committed (SHOULD), no issue yet. Deliberately after a plugin exists that can use it; DayZ cannot. Sidecar plugins are the customer |
| Position telemetry cadence | 8.3 | Parked. The plugin captures a snapshot as it builds each poll, so the cadence equals the poll cycle (25 s measured at `pollTimeout` 25; the earlier 50 s figure from #55 was plugin 0.2.0 and is fixed). Only a second channel beats that. Trigger: the WebSocket transport lands. A position-only snapshot at the same cadence would save bytes, not time, and is not planned |
| Server-scoped token dimension | 10.1 | Committed as a spec discussion (#81) |
| Cursor over webhook deliveries | 11.3 | Parked. Trigger: a delivery list exceeds the maximum `limit` in practice. Today the remedies are a wider `limit` and shorter retention |
| Snapshot diffs after the first full snapshot per session | 8.3 | Parked. Trigger: a real plugin hits the 256 KiB body cap. Unknown-type tolerance makes a diff form a backward-safe addition |
| Platform registry (player identity) | 8.2 | Shape frozen at draft 0.22; the registry stays open and a new game adds its platform identifier additively |
| Multi-instance hub (claims or leases for sweeps and the webhook dispatcher) | design notes 12 | Parked. Trigger: a single installation exceeds the capacity measured in #87. One hub per database is the supported deployment on both engines until then; the hub is one process however many boxes the game servers occupy |

## Horizon 4: the ecosystem and the second game

- **Arma Reforger plugin**: tabled 2026-09-14; issue #15 closed. Wanted in principle, no
  trigger, and nothing on this roadmap depends on it: the identity shape froze on DayZ
  evidence and protocol 1.0 gates on a third-party plugin rather than on this project
  writing a second one. A roadmap review reopens it.
- **Admin API client libraries**: parked. Generated from `spec/openapi-admin.yaml`, Go and
  TypeScript first. Trigger: the first tagged release (#69).
- **Conformance suites as a published tool**: parked. A versioned binary third-party
  implementers can run without cloning this repository, and a badge policy for what
  "conformant" may claim. Trigger: the first tagged release (#69).
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
| Invisibility and no-collision admin flags | The #71 spike shows they can be done server-side |
| Position telemetry cadence | The WebSocket transport lands |
| Cursor over webhook deliveries | A delivery list exceeds the maximum `limit` in practice |
| Snapshot diffs | A real plugin hits the 256 KiB body cap |
| Multi-instance hub | A single installation exceeds the capacity measured in #87 |
| Admin API client libraries; conformance suites as a published tool | The first tagged release |

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
