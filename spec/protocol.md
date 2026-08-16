# Vyshka Protocol Specification

**Status:** draft 0.1 (2026-08-15)
**Protocol version (`v`):** 1
**License:** Apache-2.0

This document specifies the Vyshka protocol: the contract between game-server **plugins**,
the **hub**, and **admin clients**. It is the normative artifact of the project; the hub and
the per-game plugins shipped in this repository are reference implementations. A third party
should be able to implement a compliant plugin for any game, or a compliant hub, from this
document and the conformance suites alone.

## 1. Introduction

### 1.1 Terminology

The key words MUST, MUST NOT, REQUIRED, SHALL, SHALL NOT, SHOULD, SHOULD NOT, RECOMMENDED,
MAY, and OPTIONAL in this document are to be interpreted as described in RFC 2119.

| Term | Meaning |
|---|---|
| **Hub** | The self-hosted backend. Terminates both APIs, owns persistent state. |
| **Plugin** | Code running inside (or alongside) a game server that speaks the Plugin API. |
| **Admin client** | Any consumer of the Admin API: the embedded panel, bots, scripts. |
| **Operator** | The human who runs the hub and administers game servers through it. |
| **Envelope** | The framed message unit exchanged between plugin and hub (section 4). |
| **Action** | A tracked job dispatched through the hub for execution by a plugin (section 7). |
| **Manifest** | A plugin's published declaration of its actions, contexts, and events (section 6). |

### 1.2 Design principles

1. **Game-agnostic.** Nothing in this protocol assumes a particular game or engine. The
   baseline transport is deliberately the lowest common denominator (HTTP + JSON) so that
   even heavily sandboxed scripting environments can implement it.
2. **Everything is a tracked job.** Actions have observable lifecycles and results. Delivery
   in both directions is at-least-once with deduplication. Nothing is dropped silently.
3. **Forward compatible by default.** Unknown envelope `type` values MUST be acked and
   ignored, never treated as fatal. The envelope version `v` bumps only on
   envelope-breaking changes; body-level evolution is additive.
4. **Least privilege.** Admin credentials are scoped, down to per-action granularity, and
   every Admin API mutation is audited.

## 2. Architecture

```
+------------------+        Plugin API         +-----------+       Admin API        +----------------+
| Game server      |  <== long-poll / WS ==>   |  Vyshka   |  <== REST + webhooks ==| Admin tools,   |
|  + Vyshka        |                           |    hub    |                        | bots, panel,   |
|    plugin (mod)  |                           | (1 binary)|                        | Discord bridge |
+------------------+                           +-----------+                        +----------------+
                                                    |
                                              SQLite / Postgres
```

The hub exposes two API surfaces with **separate authentication realms**:

- **Plugin API** (`/plugin/v1/...`): game-server facing. Credentials identify exactly one
  game server (section 5).
- **Admin API** (`/api/v1/...`): operator facing. Scoped bearer tokens (section 10).

A credential valid in one realm MUST NOT be accepted in the other.

## 3. Transport

### 3.1 Baseline: HTTP long-poll (mandatory)

Every plugin and every hub MUST support long-poll. It is the only transport a plugin can
rely on, because some game scripting environments allow nothing beyond callback-based HTTP.

- The plugin calls `POST /plugin/v1/poll` with its session token and its last-acked
  sequence numbers (section 9).
- The hub MUST hold the request open up to the negotiated `pollTimeout` (default 25 s) and
  SHOULD respond early as soon as messages are queued for the plugin. The response body is
  a batch of envelopes.
- The plugin SHOULD re-poll immediately after each response so a request is normally held
  open, giving near-zero command latency over plain HTTP.

> **Open question (pre-1.0):** some engine HTTP clients may enforce a client-side timeout
> below 25 s. The `pollTimeout` floor will be fixed after empirical testing; hubs MUST
> honor a plugin-requested lower value down to 5 s.

### 3.2 Upgrade: WebSocket (optional)

A hub SHOULD additionally offer `GET /plugin/v1/ws` (WebSocket), carrying the same
envelopes with the same sequencing and ack rules. A plugin advertises its capabilities at
registration with `"transports": ["poll"]` or `["poll", "ws"]`. A hub MUST NOT require
WebSocket support.

### 3.3 Security

All transport MUST be TLS in production deployments. The reference hub does not terminate
public traffic itself by default (reverse-proxy snippets are documented) and offers
`--auto-tls` (embedded ACME) for single-box setups; these are implementation details, not
protocol requirements.

## 4. Message envelope

Every plugin<->hub message, in both directions and over both transports, is an envelope:

```json
{
  "v": 1,
  "id": "01J5QK...",
  "type": "action.dispatch",
  "seq": 4182,
  "ts": "2026-08-15T18:00:00Z",
  "body": { }
}
```

| Field | Type | Rules |
|---|---|---|
| `v` | integer | Envelope version. Bumps only on envelope-breaking changes. |
| `id` | string | ULID, unique per message. |
| `type` | string | Namespaced message type (`action.dispatch`, `event.batch`, ...). |
| `seq` | integer | Per-session, per-direction monotonic sequence number (section 9). |
| `ts` | string | RFC 3339 UTC timestamp. |
| `body` | object | Type-specific payload. |

Receivers MUST ack the highest contiguous `seq` they have durably processed; senders MUST
retransmit anything above the ack. Unknown `type` values MUST be acked and ignored.

## 5. Enrollment and sessions

1. The operator creates a server record (`POST /api/v1/servers` or the panel) and receives
   a one-time **enrollment token**.
2. The plugin's config file holds the hub URL and the enrollment token.
3. First boot: `POST /plugin/v1/enroll` exchanges the enrollment token for permanent
   **server credentials** (server id + secret). The enrollment token is burned: a second
   enroll attempt with it MUST fail.
4. Every boot: `POST /plugin/v1/session` with the server credentials returns a short-lived
   **session token**, the negotiated protocol version, the effective `pollTimeout`, and
   feature flags. All subsequent Plugin API calls use the session token.

Rationale: one-time enrollment tokens keep long-lived secrets out of chat logs and support
tickets, and per-server credentials make revocation surgical. Revoking a server's
credentials MUST invalidate its session immediately (an open long-poll returns 401; the
plugin re-enrolls or enters `buffering`, section 9).

## 6. Registration: the manifest

At session start, and at any later moment, the plugin sends `manifest.publish`:

```json
{
  "type": "manifest.publish",
  "body": {
    "game": "dayz",
    "plugin": { "name": "vyshka-dayz", "version": "1.4.0" },
    "manifestRevision": 7,
    "actions": [
      {
        "code": "example-mod.heal",
        "name": "Heal player",
        "context": "player",
        "namespace": "example-mod",
        "danger": "warning",
        "params": {
          "type": "object",
          "required": ["amount"],
          "properties": {
            "amount":  { "type": "integer", "minimum": 1, "maximum": 100 },
            "targets": { "type": "array", "items": { "type": "string" } },
            "item":    { "type": "string", "x-vyshka-widget": "itemlist" }
          }
        }
      }
    ],
    "contexts": [ ],
    "events": [ ]
  }
}
```

### 6.1 Actions

- `code` is globally unique within a server and SHOULD be `{namespace}.{name}`.
- `params` is a **JSON Schema subset** (draft 2020-12): `object`/`array`/scalar types,
  `enum`, `required`, numeric bounds, `default`. The vendor keyword `x-vyshka-widget`
  hints an admin UI widget (`itemlist`, `vector`, `player`, `webhook`) without
  constraining the data model.
- The hub MUST validate dispatch payloads against the schema **before** queueing, so
  schema-invalid input never reaches the game server.
- `danger` is `none | warning | destructive`, advisory, for UI confirmation prompts.
- `manifestRevision` is plugin-owned and monotonic. The hub replaces its stored manifest
  when it receives a higher revision and MUST ignore lower ones. Publishing at runtime is
  legal and expected: changing a mod's actions costs one message, not a server restart.
- `namespace` groups actions by owning mod, for display and token scoping (section 10).

### 6.2 Contexts

Built-in contexts: `world`, `player`, `vehicle`, `object`. A plugin MAY declare custom
contexts:

```json
{ "contexts": [ { "id": "territory", "name": "Territory", "namespace": "example-mod" } ] }
```

For each custom context the plugin MUST answer `context.enumerate` requests (hub ->
plugin) with a list of `{ referenceKey, label, position? }`. The hub SHOULD cache the
enumeration briefly (default 10 s) to feed UI dropdowns.

### 6.3 Declared custom events

The manifest's `events` array declares custom telemetry types (id, namespace, human name,
optional payload schema). Declaration is advisory: it drives panel display and webhook
filtering. Hubs MUST accept undeclared custom events (storing them with a generic label);
requiring pre-declaration would reintroduce the restart-to-change problem.

## 7. Action lifecycle

Every action is a tracked job:

```
queued -> delivered -> running -> completed | failed
   \--------------------------> expired   (TTL hit before completion)
```

**Dispatch (Admin API):**

```
POST /api/v1/servers/{serverId}/actions
{
  "code": "example-mod.heal",
  "context": "player",
  "referenceKey": "76561198000000000",
  "params": { "amount": 100 },
  "ttlSeconds": 60,
  "idempotencyKey": "heal-76561198000000000-2026-08-15T18:00"
}
-> 202 { "actionId": "01J5QK...", "state": "queued" }
```

- `idempotencyKey` (optional, client-chosen): a retry with the same key MUST return the
  original `actionId` instead of double-queueing.
- `ttlSeconds` (default 120): if the action has not reached a terminal state by then, the
  hub marks it `expired`, and a plugin receiving it late MUST discard it.

**Execution (Plugin API):** the action arrives as `action.dispatch` in a poll batch. The
plugin MUST send `action.ack` on receipt (state becomes `running`) and `action.result` when
done:

```json
{
  "type": "action.result",
  "body": {
    "actionId": "01J5QK...",
    "ok": true,
    "result": { "healedTo": 100.0, "position": [4501.2, 320.1, 9800.4] },
    "error": null,
    "durationMs": 12
  }
}
```

`result` is arbitrary JSON up to 64 KiB. This enables **query actions**: "dump this
player's inventory" is an action whose result is the answer.

**Observation (Admin API):**

- `GET /api/v1/actions/{actionId}`: full record with state, timestamps, result payload.
- The `action.completed` webhook event (section 11) fires on terminal states.
- `POST /api/v1/servers/{serverId}/actions:batch` accepts up to 50 actions and returns
  per-item ids; every batched action stays individually observable.

## 8. Telemetry and state

### 8.1 Events

The plugin pushes `event.batch` envelopes: up to 200 events per batch; the plugin SHOULD
buffer and flush every 2 s or at 200 events, whichever comes first.

```json
{
  "type": "event.batch",
  "body": {
    "events": [
      { "t": "core.player.death", "ts": "...", "data": { "victim": { "platform": "steam", "id": "7656..." }, "weapon": "M4A1", "distance": 312.5 } },
      { "t": "example-mod.raid.started", "ts": "...", "data": { "territoryId": "t-19", "attackers": 4 } }
    ]
  }
}
```

**Core event types** (`core.*`, normative; every plugin SHOULD emit what its game
supports): `player.connect`, `player.disconnect`, `player.death`, `player.damage`,
`player.chat`, `player.kick`, `player.ban`, `vehicle.spawn`, `vehicle.destroy`,
`object.placed`, `item.interact`, `server.start`, `server.fps` (periodic performance
sample), `server.stop`.

**Custom events** are any type of the form `{namespace}.{name}`. Same channel, same
storage, same webhook fan-out; hubs MUST NOT privilege core events over custom events in
routing or retention capability.

### 8.2 Player identity

Player references in protocol payloads use a platform-qualified shape rather than a bare
platform-specific id:

```json
{ "platform": "steam", "id": "76561198000000000" }
```

> **Open question (pre-1.0):** the platform registry (e.g. the identifier scheme for
> Bohemia accounts on Arma Reforger) will be confirmed against a second game before this
> shape freezes.

### 8.3 State snapshots

Separate periodic envelopes (`state.players`, `state.vehicles`, `state.entities`) carry
full current lists. The hub keeps only the latest snapshot per type plus a configurable
history window; these feed the live map.

> **Open question (pre-1.0):** whether `state.*` messages become diffs after the first full
> snapshot per session. Deferred unless real-world payloads prove heavy.

### 8.4 Retention

Events land in an append-only store with per-type retention (reference defaults: 30 days,
`core.player.chat` 90 days, snapshots 24 h). Retention values are hub configuration, not
protocol.

## 9. Delivery guarantees

- **Plugin -> hub:** the plugin MUST persist an outbound ring buffer (file-backed where the
  engine allows, memory otherwise; reference default 5 000 envelopes) and MUST NOT drop an
  envelope until the hub has acked its `seq`. A game-server crash loses at most the
  unflushed tail; a hub restart loses nothing, because acks are only sent after durable
  writes.
- **Hub -> plugin:** queued actions are durable, survive hub restarts, and are re-delivered
  until acked or expired. Re-delivery plus `actionId` gives at-least-once delivery with
  plugin-side dedup: the plugin MUST keep a small LRU of executed action ids and MUST NOT
  execute the same `actionId` twice.
- **Blocked-state honesty:** the plugin SHOULD expose its link state in-game
  (`connected | degraded | buffering`), and the hub MUST expose the mirror (`lastSeenAt`,
  a `bufferedEventCount` estimate) on the server record. Nothing may be dropped without a
  visible counter incrementing.

## 10. Admin API authorization and audit

Admin tokens carry explicit scopes:

```
servers:read
events:read
events:read:example-mod.*        // namespace-filtered
actions:dispatch                 // everything (dangerous, UIs SHOULD warn)
actions:dispatch:core.player.*   // pattern over action codes
actions:dispatch:example-mod.heal
kv:rw:example-mod
webhooks:manage
admin                            // token management, server enrollment
```

A hub MUST enforce scopes on every Admin API call and MUST record every Admin API mutation
(action dispatched, token created, webhook changed, KV write) in an audit log with token
id, source IP, timestamp, and payload digest. `GET /api/v1/audit` requires the `admin`
scope. The audit log is not optional and not a plugin.

## 11. Webhooks

`POST /api/v1/webhooks` registers a target URL, an event filter (type patterns, server
ids), and an optional template (`generic-json` | `discord`).

- Deliveries MUST be signed: `X-Vyshka-Signature: hmac-sha256(secret, body)`.
- Failed deliveries MUST be retried with exponential backoff (reference: 5 attempts over
  ~15 min) and then dead-lettered with a visible failure count.
- Subscribable events: every telemetry type plus lifecycle events (`action.completed`,
  `server.link.lost`, `server.link.restored`).

## 12. Key/value store

Per-mod persistence so mods do not need their own database:

- Keys are namespaced per mod: `{namespace}/{key}`; values are JSON up to 16 KiB.
- Operations, available over both APIs: `get`, `set`, `delete`, atomic `incr`/`decr`, and
  `setIfRevision` (compare-and-swap by revision number, required as soon as a mod and an
  external bot write the same key).
- Optional per-key TTL.
- Admin tokens need `kv:rw:{namespace}`; plugins are confined to the namespaces their
  manifest declares.

## 13. Versioning and compatibility

- The protocol version is negotiated at session start; hubs MUST support the current and
  previous major version.
- The envelope version `v` bumps only on envelope-breaking changes. Body-level evolution
  is additive: new optional fields MAY appear at any time, and receivers MUST tolerate
  unknown fields.
- Unknown envelope `type` values MUST be acked and ignored (section 4).
- The spec document, the reference hub, and each plugin are versioned independently
  (SemVer).

## 14. Conformance

Two black-box suites accompany this document:

- **Hub conformance** runs against any hub URL and answers "is this hub compliant?".
- **Plugin conformance** is a mock hub that drives a candidate plugin through enrollment,
  manifest publish, action round-trips (including a forced re-delivery to verify dedup), a
  simulated network outage (to verify buffering), and a schema-invalid dispatch (which
  must never crash the game server).

An implementation that passes its suite is compliant; reading reference-implementation
source is never required.

---

## Appendix A (informative): implementing on constrained engines

The baseline transport exists because of engines like DayZ's Enforce Script, where the
only network primitive is callback-based async HTTP: no sockets, no SSE. Implementation
notes for such environments:

- Keep envelope bodies flat where possible; deep generic JSON handling is painful in some
  serializers. A plugin MAY materialize `params` as typed per-action binding classes
  generated from manifest schemas.
- File-backed ring buffers get whatever fsync semantics the engine provides; document the
  loss window honestly rather than claiming durability the engine cannot deliver.
- Engines with richer facilities (e.g. Arma Reforger's Enfusion) SHOULD still implement
  long-poll first and treat WebSocket as an upgrade, possibly via an external sidecar
  process bridging to the game.
