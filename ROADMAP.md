# Roadmap

**As of:** 2026-11-13

Last full review 2026-11-12, which found Horizons 1 and 2 landed and nothing committed that
could move without a trigger, and reopened the Arma Reforger plugin as the next horizon (the
2026-09-14 review before it settled every proposed item). This document lists
where Vyshka stands and what comes next, in order. It is the public companion to the
milestone table kept with the design notes; the milestone letters (M0 to M4) are the same
in both places.

How to read it. The states are defined in `CONTEXT.md`; in brief:

- **Shipped** means merged on `main`, graded by the conformance suites or the browser test,
  and where it involves a game, demonstrated against a live DayZ server (1.30 since
  2026-11-04).
- **Committed** means an issue is open for it. It will be built; only the order can move.
- **Parked** means wanted, not scheduled, and carrying a named trigger written next to it.
  When the trigger fires the item becomes committed.
- **Tabled** means wanted in principle with no trigger. Only a roadmap review reopens it,
  and nothing else on the roadmap may depend on it.
- **Dropped** items are listed under "Not on the roadmap" with their reason.

There are no proposed items after the review of 2026-11-12. A new idea enters as proposed
and must leave that state at the next review.

Every committed item is delivered as a slice: one issue, one branch, the spec change first
where there is one, the conformance suite passing with a check for the new behavior, the
feature shown working on a real hub (`scripts/e2e.sh` for the hub, the DayZ harness or the
staging server for a game feature), one adversarial review round, and a one- or two-line
`CHANGELOG.md` entry in the landing commit. Engine limits that
constrain a plugin are measured under `spikes/` before anything is designed around them.

The ordering rule was **DayZ first, to the first tagged release**, and it is met: four
releases are out and every DayZ slice committed in 2026-09 has landed. Since the review of
2026-11-12 it is **Reforger next, measured first**: the spike (#159) comes before the
tracer bullet (#15), and a DayZ bug or a new DayZ game version still goes ahead of both.
The design persona is one
operator or small team running one hub with a handful of game servers enrolled in it, the
servers usually on other boxes than the hub, and a few admins who do not all hold the same
trust. A hosting outfit running servers for other people is out of scope until 1.0.

## Where things stand

| Milestone | Deliverable | Status |
|---|---|---|
| M0 | Spec, envelope schemas, OpenAPI; conformance suites run against stubs | Shipped |
| M1 | Hub core: enrollment, sessions, long-poll, manifests, action lifecycle, SQLite | Shipped |
| M2 | DayZ plugin (clean-room) | Shipped 2026-09-05; readable inline errors landed 2026-09-10; moderation slice landed 2026-09-14 |
| M3 | Telemetry, state snapshots, webhooks, KV store; Discord kill feed from a live server | Shipped 2026-09-14: the kill feed shown in a Discord channel from a live DayZ server (the staging repeat was dropped in the 2026-11-12 review; the live demo stands) |
| M4 | Scoped tokens, audit log, panel v1 (servers, actions, event feed, live map) | Shipped 2026-09-15: panel v1 with a live DayZ player plotted on the staging map 2026-09-12 (issue #46), and the management views landed 2026-09-15 (issue #64) |
| 1.0 | Protocol freeze | After at least one third-party plugin has been written from the spec alone |

The first tagged release is out: hub 0.1.0 (`hub-v0.1.0`, static binaries for Linux, macOS,
and Windows plus the container image) and DayZ plugin 0.7.0 (`dayz-plugin-v0.7.0`), both
on 2026-09-17 (issue #69). The second followed on 2026-09-23: hub 0.2.0 (`hub-v0.2.0`) and
DayZ plugin 0.8.0 (`dayz-plugin-v0.8.0`), carrying every Horizon 1 slice that landed after
the first tag. Hub 0.3.0 (`hub-v0.3.0`) and DayZ plugin 0.9.0
(`dayz-plugin-v0.9.0`) followed on 2026-10-03 with the command-line client, the client
libraries, and the poll `more` signal. Hub 0.4.0 (`hub-v0.4.0`) and DayZ plugin 0.10.0
(`dayz-plugin-v0.10.0`) followed on 2026-11-12 with Horizon 2's operations work and DayZ
1.30 support. Releases are described in `RELEASING.md`.

M5 (custom contexts, Arma Reforger plugin, `--auto-tls`) was dissolved on 2026-09-14:
custom contexts moved to Horizon 1 with a DayZ customer, `--auto-tls` to Horizon 2, and the
Reforger plugin was tabled (see Horizon 4) until the review of 2026-11-12 reopened it. The
player identity shape froze on 2026-09-14
(protocol draft 0.22, section 8.2) on DayZ evidence, with the platform registry left open.

The hub runs on SQLite by default and on Postgres since 2026-09-13. The protocol document
is at draft 0.36 and is pre-1.0: it can still change shape where Horizon 3 says so.

### Landed slices

One line each, newest last; the changelog and the pull request carry the detail. Horizon 1
(close M4 and ship the first release) finished with these.

- 2026-09-15: panel management views, closing M4 (#64, draft 0.23).
- 2026-09-16: outbox across a server crash, measured (#65, spike): nothing on disk lost, the
  unflushed 0.1 s to 2.1 s of events is the window.
- 2026-09-16: position and world actions: teleport, spawn one item, set time (#66).
- 2026-09-16: vehicles 1: `state.vehicles`, vehicle events, delete destroyed, unstuck (#67).
- 2026-09-17: vitals (#68).
- 2026-09-17: release tooling, then hub 0.1.0 and DayZ plugin 0.7.0 (#69).
- 2026-09-17: damage telemetry, `core.player.damage` (#70).
- 2026-09-17: admin flags stored per identity (#71, draft 0.24).
- 2026-09-18: mod surface with a self-contained sample mod (#72, draft 0.25).
- 2026-09-21: server-scoped tokens (#81, draft 0.26) and the plugin's string and file
  fixes (#108).
- 2026-09-22: item catalog, the first custom context (#73, draft 0.27).
- 2026-09-22: inventory read, strip, and clear (#74).
- 2026-09-22: spawning extension with the spawn blocklist (#75, draft 0.28).
- 2026-09-22: loadout, location, and vehicle presets; the key/value editor (#76, draft 0.29).
- 2026-09-22: vehicles 2: refuel, repair, damage state (#77).
- 2026-09-22: world snapshot, weather, freeze time (#78, draft 0.30).
- 2026-09-23: player profiles, webhook redaction, audit records as webhooks (#79, draft 0.31).
- 2026-09-23: installation-wide ban list (#80, draft 0.32) and staged file rewrites (#121).
- 2026-09-23: a poll saying `more` is answered at once (#91, draft 0.33).
- 2026-09-23: hub 0.2.0 and DayZ plugin 0.8.0 released.
- 2026-09-29: the `vyshka` command-line client (#138).
- 2026-09-30: Admin API client libraries for Go and TypeScript (#96).
- 2026-10-03: end-to-end-first testing (#143); `scripts/e2e.sh`, the branch hub, and
  one-line changelog entries (#144).
- 2026-10-03: hub 0.3.0 and DayZ plugin 0.9.0 released (#144).
- 2026-11-11: backup endpoint, restore runbook, `storage` and `vacuum` commands (#83).
- 2026-11-11: `/metrics` for Prometheus, on a listener of its own (#82).
- 2026-11-11: `-auto-tls`, embedded ACME, so the hub serves TLS without a proxy (#85).
- 2026-11-11: TOML config file under flags and the environment, with `file:` secrets (#84).
- 2026-11-11: rate limits per server and per admin token (#86, draft 0.34).
- 2026-11-11: hub capacity measured (#87, spike): 200 servers of 100 players at an assumed
  1.2 events/s and 1 action/min each, or 50 at 12.3 events/s and 6/min, on 1 vCPU and 2 GB
  with SQLite (3 of 3 runs each); disk binds first: about 7.8 servers of the first kind on
  40 GB at default retention after 90 days.
- 2026-11-12: hub 0.4.0 and DayZ plugin 0.10.0 released.
- 2026-11-12: Reforger server script measured (#159, spike, partial): poll timeouts of 3 s
  and up, readable error bodies, real `Authorization` headers, responses under 1 MiB, no
  guaranteed second request while a poll is held; player identity still open.
- 2026-11-12: Plugin API responses bounded at 512 KiB and envelope bodies at 256 KiB, the
  protocol change Reforger needed first (part of #15, draft 0.35).
- 2026-11-13: Reforger player identity measured on a backend-registered server (#159, spike):
  a lowercase UUID from the player's audit onward, registered as platform `reforger` (draft 0.36).
- 2026-11-13: Arma Reforger plugin tracer bullet (#15): the plugin conformance suite passes on
  a Reforger 1.9 dedicated server, and `vyshka.info` round-trips through the branch hub.

## Horizon 2: operations, as promised in the design notes

Every item has landed: backup, `-auto-tls`, metrics, the config file, rate limits, and the
capacity measurement (see "Landed slices" above).

## Horizon 3: protocol items the spec names and the hub does not implement

Gaps between `spec/protocol.md` and the reference hub, listed with their section so the
spec reader is not surprised.

| Item | Section | Status |
|---|---|---|
| Custom contexts and `context.enumerate` | 6.2 | Shipped 2026-09-22 (#73, draft 0.27): the hub sends `context.enumerate` behind an Admin API read, caches the answer, and the hub conformance suite grades it (`admin.contexts.enumerate`); the DayZ item catalog is the first customer and the panel's spawn form the first consumer |
| WebSocket transport at `/plugin/v1/ws` | 3.2 | Parked 2026-11-12 (was committed with no issue). A SHOULD in the spec with no customer: DayZ cannot hold a socket and no sidecar plugin exists. Trigger: a plugin that can hold a WebSocket exists, a sidecar; Reforger's script has no socket API (#159) |
| Position telemetry cadence | 8.3 | Parked. The plugin captures a snapshot as it builds each poll, so the cadence equals the poll cycle (25 s measured at `pollTimeout` 25; the earlier 50 s figure from #55 was plugin 0.2.0 and is fixed). Only a second channel beats that. Trigger: the WebSocket transport lands. A position-only snapshot at the same cadence would save bytes, not time, and is not planned |
| Server-scoped token dimension | 10.1 | Shipped 2026-09-21 (#81, draft 0.26): a token-level `servers` binding |
| Cursor over webhook deliveries | 11.3 | Parked. Trigger: a delivery list exceeds the maximum `limit` in practice. Today the remedies are a wider `limit` and shorter retention |
| A poll that says "more queued" is answered at once | 3.1.2 | Shipped 2026-09-23 (#91, draft 0.33): a plugin that cut its batch short sets `more`, and a hub answers that poll as soon as it is applied when its ack covers something the poll carried, so a backlog drains a batch per round trip instead of a batch per `pollTimeout` (40 events/s at 25 s, measured in `spikes/dayz-outbox-crash`). Graded by `plugin.poll.more` in the hub suite and `poll.more` in the plugin suite; DayZ plugin 0.9.0 sets it |
| Snapshot diffs after the first full snapshot per session | 8.3 | Parked. Trigger: a real plugin hits the 256 KiB body cap. Unknown-type tolerance makes a diff form a backward-safe addition |
| Platform registry (player identity) | 8.2 | Shape frozen at draft 0.22; the registry stays open and a new game adds its platform identifier additively. `steam` and `reforger` (draft 0.36) are registered |
| Multi-instance hub (claims or leases for sweeps and the webhook dispatcher) | design notes 12 | Parked. Trigger: a single installation exceeds the capacity measured in #87: 200 game servers of 100 players at an assumed 1.2 events/s and 1 action/min each (3 of 3 runs passed), or 50 at an assumed 12.3 events/s and 6/min (3 of 3), on 1 vCPU and 2 GB with SQLite and the rate limits on, measured on loopback without TLS in a WSL VM from an empty database (`spikes/hub-capacity`). The hub's 256-connection cap is shared with every other open connection (admin clients, a proxy's upstream connections, a plugin's second context): with the spike's admin share it is reached near 225 servers. At default retention the disk binds first: about 4.7 GiB per server of the first kind after 90 days (about 39 GiB for the second). One hub per database is the supported deployment on both engines until then; the hub is one process however many boxes the game servers occupy |

## Horizon 4: the ecosystem and the second game

- **Arma Reforger plugin**: tabled 2026-09-14, reopened and committed 2026-11-12. The
  spike (#159) measured the server script on Reforger 1.9
  (`spikes/reforger-server-script-limits`): the poll transport works as specified. The
  player identifier, which reads empty unless the server is registered with the game's
  backend, was measured on such a server with the probe published unlisted to the Workshop
  (`spikes/reforger-player-identity`). The protocol change it needed first, a byte
  bound on every Plugin API response because Reforger cannot receive 1 MiB, landed in
  draft 0.35, and Reforger's platform registry entry, `reforger`, in draft 0.36. The
  tracer bullet (#15) landed 2026-11-13 (`plugins/reforger`): the DayZ plugin's protocol
  core ported, two actions, and the plugin conformance suite passing on a dedicated
  server, with no protocol change. Next, when picked: the installation ban list, a
  Workshop release so a `-config` server can load it, and player telemetry shown live.
  Protocol 1.0 still gates only on a third-party plugin; a second reference plugin tests
  the game-agnostic claim, and nothing else on the roadmap waits on it.
- **Conformance suites as a published tool** (#97): committed 2026-09-17, parked
  2026-10-03 (see the parked table). Anyone can `go run` the suites from this module today.
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
| WebSocket transport at `/plugin/v1/ws` | A plugin that can hold a WebSocket exists (a sidecar; Reforger's script has no socket API, #159) |
| Position telemetry cadence | The WebSocket transport lands |
| Cursor over webhook deliveries | A delivery list exceeds the maximum `limit` in practice |
| Snapshot diffs | A real plugin hits the 256 KiB body cap |
| Multi-instance hub | A single installation exceeds the capacity measured in #87: 200 servers of 100 players at an assumed 1.2 events/s and 1 action/min, or 50 at 12.3 events/s and 6/min, on SQLite; near 225 at the connection cap with the spike's admin share; about 7.8 servers of the first kind on a 40 GB disk at default retention after 90 days |
| Conformance suites as a published tool (#97): versioned suite binaries on the release and a policy for what "conformant" may claim | A third-party implementer asks for them |

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
- Dropped 2026-10-03: tightening the conformance checks' timing slack and narrow probes
  (#134; tighter bounds mostly bring flakes, and the four checks that could not see their
  violation were fixed by #137); hardening the plugin suite's param synthesis against
  manifests with tens of thousands of schema entries (#116; no real plugin is that shape).

## Keeping this document honest

Update this file in the same commit as any change that shifts an item between states, and
set the header to that date; the header is a date and nothing else. A new idea enters as
proposed and leaves that state at the next review. When a slice lands, delete its entry
from its horizon and add one line for it under "Landed slices". The changelog and the pull
request record what happened; this file records what is intended.
