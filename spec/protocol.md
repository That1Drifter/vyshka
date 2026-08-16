---
title: Protocol Specification
layout: default
nav_order: 2
---

# Vyshka Protocol Specification

**Status:** draft 0.6 (2026-08-16)
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

### 2.1 HTTP conventions

Both realms are JSON over HTTP:

- Request and response bodies are UTF-8 JSON. A request carrying a body MUST use
  `Content-Type: application/json`; a hub MUST reject anything else with `415`.
- Receivers MUST ignore unknown fields in any request or response body. This is the
  body-level half of the forward-compatibility rule in section 13: new optional fields may
  appear at any time.
- Bearer credentials travel in `Authorization: Bearer <token>`. All tokens defined by this
  document (enrollment token, server secret, session token, admin token) are **opaque**:
  clients MUST NOT parse them or depend on their length, alphabet, or any prefix.
- Identifiers assigned by the hub (`serverId`, `sessionId`, `actionId`) are opaque strings,
  stable for the lifetime of the object they name.
- Timestamps are RFC 3339 with a UTC offset.

### 2.2 Error model

Every 4xx and 5xx response from either realm carries this body:

```json
{
  "error": {
    "code": "enrollment_token_invalid",
    "message": "enrollment token is unknown or expired",
    "details": { }
  }
}
```

`code` is a stable machine-readable identifier; `message` is human-readable and MUST NOT be
parsed; `details` is OPTIONAL and its contents are code-specific. Clients MUST branch on
`code` and MUST tolerate codes they do not recognize, falling back on the HTTP status.

General codes, usable by any endpoint:

| `code` | HTTP | Meaning |
|---|---|---|
| `bad_request` | 400 | Malformed body, or a field that is missing, wrongly typed, or out of range |
| `unauthorized` | 401 | Missing or invalid credential for this realm |
| `forbidden` | 403 | Valid credential, insufficient scope (section 10) |
| `not_found` | 404 | No such object, or the credential may not see it |
| `method_not_allowed` | 405 | Path exists, method does not |
| `conflict` | 409 | The request contradicts current state |
| `unsupported_media_type` | 415 | Body was not `application/json` |
| `payload_too_large` | 413 | Body exceeded the hub's limit |
| `rate_limited` | 429 | Reserved; hubs MAY rate-limit and SHOULD send `Retry-After` |
| `internal` | 500 | The hub failed; the client MAY retry |

Endpoint-specific codes are defined with the endpoints that raise them (section 5).

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

#### 3.1.1 Negotiating `pollTimeout`

Engine HTTP clients abort a request that has not produced a complete response within a
client-side limit, and that limit can be well below 25 s. The negotiated `pollTimeout` is
therefore the contract that keeps the hub's response ahead of the plugin's own deadline.

- A plugin MAY request a `pollTimeout` at session start (section 5). A hub MUST honor any
  requested value in the range **5 s to 60 s** inclusive, and MUST return the effective
  value in the session response. A hub MAY clamp a value outside that range to the nearest
  end of it; it MUST NOT silently apply a value the plugin did not request from inside the
  range.
- The default, when a plugin requests nothing, is 25 s.
- The hub MUST send a response no later than the effective `pollTimeout`. When no envelopes
  are queued, that response is an empty batch, and the plugin re-polls.
- A plugin MUST configure its HTTP client's response timeout to at least the effective
  `pollTimeout` plus 5 s, so that the hub's own timely response always wins the race against
  the client-side abort. A plugin that cannot configure a timeout that high MUST request a
  correspondingly lower `pollTimeout`.
- Hubs and plugins MUST NOT rely on partial writes (chunked keepalives, early headers,
  whitespace padding) to keep a held request alive. At least one engine client applies its
  timeout to the complete response regardless of bytes already received.
- An aborted poll is not a delivery failure. Sequence numbers and acks (section 9) already
  make the next poll recover anything undelivered, so a plugin MUST simply re-poll rather
  than treat a timeout as a session error.

> **Measured basis:** DayZ 1.29 aborts a held response after 10 s by default, applies that
> limit to the whole response rather than as an idle timer, and lets script raise it to any
> value from 3 s to 120 s. A 25 s hold is reliable there once the plugin raises its own read
> timeout, which is why 25 s stays the default and 60 s is the ceiling a hub must honor.

#### 3.1.2 The poll exchange

One poll carries both directions at once: what the plugin has to say, and what the hub has
been holding for it.

```
POST /plugin/v1/poll
Authorization: Bearer <sessionToken>
{
  "ack": 4181,
  "envelopes": [
    { "v": 1, "id": "01J5QN...", "type": "event.batch", "seq": 918, "ts": "...", "body": { } }
  ]
}

-> 200 OK
{
  "envelopes": [
    { "v": 1, "id": "01J5QK...", "type": "action.dispatch", "seq": 4182, "ts": "...", "body": { } }
  ],
  "ack": 918,
  "pollTimeoutSeconds": 25,
  "sessionExpiresAt": "2026-08-16T19:00:00Z"
}
```

Both request fields are OPTIONAL: `{}` is a valid idle poll, and so is a poll that only
acks or only sends.

| Field | Direction | Meaning |
|---|---|---|
| `ack` (request) | plugin -> hub | Highest contiguous hub -> plugin `seq` the plugin has durably processed. `0`, or absent, acks nothing. |
| `envelopes` (request) | plugin -> hub | Envelopes the plugin is sending, in ascending `seq` order. |
| `envelopes` (response) | hub -> plugin | Envelopes queued for the plugin, in ascending `seq` order. Always present; an empty batch is `[]`, never `null`. |
| `ack` (response) | hub -> plugin | Highest contiguous plugin -> hub `seq` the hub has durably processed. |
| `pollTimeoutSeconds` | hub -> plugin | The effective hold time for this session, repeated so a plugin never has to cache it. |
| `sessionExpiresAt` | hub -> plugin | When the session token stops working, so the plugin can renew before a poll fails. |

Rules:

- A hub MUST answer with `200` and an empty `envelopes` array when the hold expires with
  nothing queued. An empty batch is a normal answer, not an error.
- A hub MUST answer immediately, without holding, whenever any unacked envelope is already
  queued for the session.
- A hub MUST apply the request's `ack` and ingest the request's `envelopes` before it
  begins to hold, so that a poll which only acks takes effect at once rather than at the
  end of the hold. It MUST apply the `ack` first: the ack frees queued work, and applying
  it late means re-sending an envelope the plugin has already reported done.
- A hub MUST validate the whole inbound batch before applying any of it. A poll that
  carries a malformed envelope changes nothing at all, including its `ack`, so a plugin can
  correct the batch and retry the request as a whole rather than reason about which half
  landed.
- A hub MUST answer a held poll with `401 session_invalid` as soon as its session stops
  being live (superseded, revoked, or expired) rather than letting the hold run to term.
- A hub MUST accept at least 200 envelopes in one poll request. It MAY cap the batch above
  that, and MUST reject anything over its cap with `bad_request` rather than truncate it
  silently: a plugin that believes an envelope was delivered will never resend it.
- A hub MUST update the server record's `lastSeenAt` on every poll.

| `code` | HTTP | Raised when |
|---|---|---|
| `session_invalid` | 401 | The session token is expired, unknown, or superseded (section 5.3) |
| `envelope_invalid` | 400 | An inbound envelope is missing `id` or `type`, carries a `seq` that is absent or below 1, exceeds the hub's length limit on `id` or `type`, or declares an envelope version the hub does not speak (section 4). `details.index` is REQUIRED and names the envelope's position in the batch; `details.seq` is OPTIONAL and carries its `seq` when that was usable |
| `ack_out_of_range` | 400 | `ack` is above the highest `seq` the hub has sent on this session |
| `bad_request` | 400 | The batch exceeds the hub's envelope limit, or the body is malformed |

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
| `id` | string | Opaque, unique per message, and stable across retransmissions. ULID RECOMMENDED. |
| `type` | string | Namespaced message type (`action.dispatch`, `event.batch`, ...). |
| `seq` | integer | Per-session, per-direction monotonic sequence number, starting at 1 (section 9). |
| `ts` | string | RFC 3339 UTC timestamp of when the message was created, not of when it was last sent. |
| `body` | object | Type-specific payload. |

`id`, `type`, `seq` and `ts` are REQUIRED on every envelope a sender emits. `v` is REQUIRED
from the hub and OPTIONAL from a plugin, where its absence means the version the session
negotiated as `envelopeVersion`.

`id` is opaque to the receiver: it MUST be unique per message within a session and
identical on every retransmission of that message, and a receiver MUST NOT parse it or
require any particular format. The reference implementations mint ULIDs, and new
implementations SHOULD, but a receiver that rejected anything else would force a ULID
encoder into every game engine to buy nothing: deduplication only needs equality.

Receivers enforce the rest unevenly, on purpose:

- A receiver MUST reject an envelope missing `id`, `type` or `seq`, declaring a version it
  does not speak, or exceeding a documented length limit on `id` or `type`. Without those
  it cannot deduplicate, route, order, or parse the message, and guessing would be worse
  than refusing. A receiver MUST accept an `id` and a `type` of at least 128 characters.
- An absent `v` and a `v` of `0` are different: absent means the negotiated version, while
  `0` names a version no implementation speaks and MUST be rejected like any other unknown
  version. Implementations MUST NOT collapse the two.
- A receiver MUST NOT reject an envelope over `ts` alone. When `ts` is missing or
  unparseable it MUST substitute its own receipt time wherever it records one. Some game
  engines have no trustworthy clock, and losing a batch of real events to a wrong clock
  costs more than an approximate timestamp does.

Receivers MUST ack the highest contiguous `seq` they have durably processed; senders MUST
retransmit anything above the ack. Unknown `type` values MUST be acked and ignored.

A machine-readable schema for the envelope and the poll exchange is
`spec/envelopes.schema.json`. It is a companion to this section, not a replacement for it:
where the two disagree, this document wins.

## 5. Enrollment and sessions

Three credentials, each with one job:

| Credential | Lifetime | Held by | Purpose |
|---|---|---|---|
| Enrollment token | One use, short expiry | Plugin config file | Bootstraps a server record into credentials |
| Server secret | Permanent until revoked | Plugin, on disk | Proves which server is calling |
| Session token | Short-lived | Plugin, in memory | Authenticates every other Plugin API call |

The flow:

1. The operator creates a server record (`POST /api/v1/servers` or the panel) and receives
   a one-time **enrollment token**.
2. The plugin's config file holds the hub URL and the enrollment token.
3. First boot: `POST /plugin/v1/enroll` exchanges the enrollment token for permanent
   **server credentials** (server id + secret). The enrollment token is burned: a second
   enroll attempt with it MUST fail.
4. Every boot, and again before the session expires: `POST /plugin/v1/session` with the
   server credentials returns a short-lived **session token**, the negotiated protocol
   version, the effective `pollTimeout`, and feature flags. All subsequent Plugin API calls
   authenticate with the session token.

Rationale: one-time enrollment tokens keep long-lived secrets out of chat logs and support
tickets, and per-server credentials make revocation surgical.

A hub MUST store only an irreversible digest of every credential it issues. Each secret is
returned exactly once, in the response that mints it, and MUST NOT be retrievable
afterwards through any API.

### 5.1 Server records (Admin API)

```
POST /api/v1/servers
Authorization: Bearer <admin token>
{ "name": "Chernarus #1", "game": "dayz", "enrollmentTokenTtlSeconds": 86400 }

-> 201 Created
{
  "server": {
    "id": "01J5QK...",
    "name": "Chernarus #1",
    "game": "dayz",
    "createdAt": "2026-08-16T18:00:00Z",
    "enrolledAt": null,
    "revokedAt": null,
    "credentialState": "none",
    "lastSeenAt": null,
    "pendingEnvelopeCount": 0,
    "plugin": null,
    "session": null
  },
  "enrollment": {
    "token": "<one-time enrollment token>",
    "expiresAt": "2026-08-17T18:00:00Z"
  }
}
```

- `name` is REQUIRED and MUST be non-empty. `game` is OPTIONAL: it declares what the
  operator expects to enroll here, and the hub MUST reject a mismatching enrollment
  (`game_mismatch`). When omitted, the game is whatever enrolls.
- `enrollmentTokenTtlSeconds` is OPTIONAL (reference default 86400). A hub MAY clamp it and
  MUST report the effective expiry in `expiresAt`.
- `credentialState` is `none` before enrollment, `active` once enrolled, `revoked` after
  revocation.
- `pendingEnvelopeCount` is how many envelopes are queued for the server and not yet acked
  (section 9.4).
- `session` is `null` when no live session exists, otherwise
  `{ "id": ..., "expiresAt": ..., "pollTimeoutSeconds": ... }`.

Supporting endpoints, all requiring the `admin` scope:

| Request | Result |
|---|---|
| `GET /api/v1/servers` | `{ "servers": [ ... ] }`, newest first |
| `GET /api/v1/servers/{serverId}` | One server record |
| `POST /api/v1/servers/{serverId}/enrollment-token` | `201` with a fresh one-time token; any unused earlier token for that server MUST be invalidated |
| `DELETE /api/v1/servers/{serverId}/credentials` | `204`; revokes the server secret and kills its sessions (section 5.4) |
| `POST /api/v1/servers/{serverId}/envelopes` | `202`; queues one envelope for the server (section 5.5) |

### 5.2 Enrollment (Plugin API)

```
POST /plugin/v1/enroll
{
  "enrollmentToken": "<one-time enrollment token>",
  "game": "dayz",
  "plugin": { "name": "vyshka-dayz", "version": "1.4.0" },
  "transports": ["poll"]
}

-> 201 Created
{
  "serverId": "01J5QK...",
  "serverSecret": "<permanent server secret>",
  "server": { "id": "01J5QK...", "name": "Chernarus #1", "game": "dayz" }
}
```

- `enrollmentToken` and `game` are REQUIRED; `plugin` and `transports` are OPTIONAL and
  recorded for display.
- Burning the token and issuing the secret MUST be atomic: two concurrent enrollments with
  the same token MUST result in exactly one success.
- Enrolling a server that already has credentials (with a freshly issued token) MUST replace
  the secret, invalidating the previous secret and every session derived from it. This is
  the recovery path after a lost secret or a revocation.
- The plugin MUST persist `serverId` and `serverSecret` and MUST NOT need the enrollment
  token again.

| `code` | HTTP | Raised when |
|---|---|---|
| `enrollment_token_invalid` | 401 | Token is unknown or expired |
| `enrollment_token_used` | 409 | Token was already burned |
| `game_mismatch` | 409 | Server record declares a different `game` |

### 5.3 Sessions (Plugin API)

```
POST /plugin/v1/session
{
  "serverId": "01J5QK...",
  "serverSecret": "<permanent server secret>",
  "protocolVersion": 1,
  "pollTimeoutSeconds": 25,
  "plugin": { "name": "vyshka-dayz", "version": "1.4.0" },
  "transports": ["poll"]
}

-> 200 OK
{
  "sessionId": "01J5QM...",
  "sessionToken": "<short-lived session token>",
  "expiresAt": "2026-08-16T19:00:00Z",
  "protocolVersion": 1,
  "envelopeVersion": 1,
  "pollTimeoutSeconds": 25,
  "transports": ["poll"],
  "features": { },
  "server": { "id": "01J5QK...", "name": "Chernarus #1", "game": "dayz" }
}
```

- `serverId` and `serverSecret` are REQUIRED. The remaining fields are the plugin's
  requests, and the response is authoritative for all of them.
- `protocolVersion` is what the plugin speaks; omitted means the current version. A hub MUST
  support the current and previous major version (section 13) and MUST reject anything else
  with `protocol_version_unsupported`.
- `pollTimeoutSeconds` is honored within 5 s to 60 s and clamped outside it, per section
  3.1.1. `envelopeVersion` is the `v` the hub will send on envelopes (section 4).
- `transports` in the response lists what the hub offers; `features` is an object of
  hub-declared flags, which plugins MUST tolerate not recognizing. Both MAY be extended
  without a version bump.
- A hub MUST hold **at most one live session per server**. Issuing a session MUST invalidate
  any earlier session for the same server, so a restarted game server never contends with
  the sequence state of its own previous session (section 9).
- The session token authenticates every later Plugin API call as
  `Authorization: Bearer <sessionToken>`. A hub MUST reject an expired, unknown, or
  superseded session token with `401` and code `session_invalid`, and the plugin MUST
  respond by requesting a new session rather than by re-enrolling.
- The hub MUST update the server record's `lastSeenAt` on session creation.

| `code` | HTTP | Raised when |
|---|---|---|
| `credentials_invalid` | 401 | `serverId`/`serverSecret` do not match a server |
| `credentials_revoked` | 401 | The server's credentials were revoked |
| `protocol_version_unsupported` | 400 | Requested version is outside what the hub supports |
| `session_invalid` | 401 | (later calls) session token is expired, unknown, or superseded |

### 5.4 Revocation

Revoking a server's credentials MUST take effect immediately, not at the next session
boundary: the current session is invalidated at once, and a long-poll already held open
MUST be answered with `401 session_invalid` rather than being left to expire. The plugin
then enters `buffering` (section 9) and retries its session, which fails with
`credentials_revoked` until the operator issues a fresh enrollment token.

### 5.5 Queueing an envelope (Admin API)

```
POST /api/v1/servers/{serverId}/envelopes
Authorization: Bearer <admin token>
{ "type": "example-mod.reload", "body": { "modules": ["loot"] } }

-> 202 Accepted
{ "envelope": { "id": "01J5QK...", "type": "example-mod.reload", "ts": "2026-08-16T18:00:00Z" } }
```

This is the transport-level primitive: it puts one envelope on the server's queue and
returns. Everything the hub itself models (action dispatch, section 7) is defined in terms
of the same queue, with its own endpoint and its own validation.

- `type` is REQUIRED and MUST be non-empty after trimming surrounding whitespace. A hub
  MUST accept a `type` of at least 128 characters and MAY reject a longer one with
  `bad_request`. `body` is OPTIONAL, MUST be a JSON object when present, and defaults
  to `{}`.
- The hub assigns `id` and `ts`. It does not assign `seq` here: sequence numbers belong to
  a session, and an envelope may be queued while no session exists (section 9.2). The
  response therefore carries no `seq`.
- A hub MUST reject a `type` family the hub itself models (`action.*` via the dispatch
  endpoint of section 7, `manifest.*` via section 6) with `conflict`. This endpoint must
  never become a way around the validation those surfaces perform, nor a way to queue a
  message, such as a forged `manifest.reject`, that the plugin would take as the hub's own
  word.
- Queueing does not require a live session, and MUST NOT fail because the server has none.

| `code` | HTTP | Raised when |
|---|---|---|
| `bad_request` | 400 | `type` is missing, empty, or too long |
| `conflict` | 409 | `type` is reserved for an endpoint that validates it |
| `not_found` | 404 | No such server |
| `outbound_queue_full` | 409 | The server's queue is at its bound (section 9.2) |

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
  constraining the data model. Integer constants in a schema (bounds and `enum` members)
  MUST lie strictly within ±2^53: every JSON toolchain in this ecosystem passes numbers
  through IEEE doubles somewhere, and a constant that rounds on the way would be enforced
  against a value its author never wrote, so hubs reject what they cannot compare exactly.
- The hub MUST validate dispatch payloads against the schema **before** queueing, so
  schema-invalid input never reaches the game server.
- `danger` is `none | warning | destructive`, advisory, for UI confirmation prompts.
- `manifestRevision` is plugin-owned, an integer in `[1, 2^53)` (the same exactness bound
  as schema constants), and monotonic. The hub
  replaces its stored manifest when it receives a higher revision and MUST ignore an equal
  or lower one, whatever its content: at-least-once delivery means the same publish can
  arrive twice, and an equal revision that changed content is a plugin bug the hub must not
  paper over by guessing which copy is current. Publishing at runtime is legal and
  expected: changing a mod's actions costs one message, not a server restart.
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

The manifest's `events` array declares custom telemetry types:

```json
{ "events": [ { "id": "example-mod.raid.start", "name": "Raid started",
               "namespace": "example-mod", "payload": { "type": "object" } } ] }
```

`payload` is an OPTIONAL schema in the section 6.1 subset. Declaration is advisory: it
drives panel display and webhook filtering. Hubs MUST accept undeclared custom events
(storing them with a generic label); requiring pre-declaration would reintroduce the
restart-to-change problem.

### 6.4 Validation and rejection

A hub MUST validate a `manifest.publish` body before storing it, and MUST reject the whole
manifest when any of its `params` or event `payload` schemas uses a keyword outside the
section 6.1 subset. This is stricter than JSON Schema's own "ignore what you don't know"
rule, deliberately: the hub validates dispatch payloads against these schemas before they
reach the game server, so a keyword it accepted but did not enforce (`pattern`, say) would
wave through exactly the input the mod author wrote the schema to exclude, with the mod
trusting a guarantee nobody was providing. A manifest is also rejected when
`manifestRevision` is missing or outside `[1, 2^53)`, an action `code` is missing or
duplicated, or a declared field exceeds the hub's length limits (counted in Unicode code
points, the unit `maxLength` means in the companion schema). A JSON `null` where an
OPTIONAL field could appear reads as the field being absent, never as a type error.

Validation runs before the revision comparison: an invalid manifest is answered with
`manifest.reject` whatever its revision says, including one equal to or below the stored
revision. The revision rule of section 6.1 orders *valid* manifests; a rejection touches
nothing, so answering it can contradict nothing.

Rejection is envelope-level success. The envelope is acked like any other (section 9.3:
the durably committed effect is that the stored manifest did not change), the session
stays up, and the other envelopes in its batch are unaffected. A hub MUST NOT fail the
poll or end the session over a manifest it rejected, and a rejected manifest MUST NOT
touch the stored one.

The rejection MUST NOT be silent: the hub MUST queue a `manifest.reject` envelope
(hub -> plugin) unless the server's outbound queue is at its bound (section 9.2), carrying
the `id` of the refused envelope, the refused `manifestRevision` when it was readable, and
`errors`, a list of `{ path, message }` faults into the rejected body:

```json
{
  "type": "manifest.reject",
  "body": {
    "envelopeId": "01J5QM...",
    "manifestRevision": 8,
    "errors": [
      { "path": "actions[0].params.properties.item.pattern",
        "message": "keyword \"pattern\" is outside the schema subset this protocol enforces" }
    ]
  }
}
```

The stored revision advances only on acceptance, so a corrected manifest MAY be
republished at the very revision that was just rejected. A plugin SHOULD surface a
`manifest.reject` where the server operator will see it (a log line at least) and MUST NOT
treat one as a transport error.

Within a session, a retransmitted `manifest.publish` is a duplicate like any other
(section 9.1): acked again, processed no further, and answered with no second rejection.
Across a session change the receiver cannot tell a renumbered republication from a new
publish, because `seq` is the one field renumbering changes and `seq` is what duplicate
detection runs on, so a hub MAY answer it with another `manifest.reject`. The notice is
at-least-once like everything else here; a plugin that cares deduplicates on `envelopeId`.

### 6.5 Reading the manifest (Admin API)

```
GET /api/v1/servers/{serverId}/manifest

-> 200 OK
{ "revision": 7, "publishedAt": "2026-08-16T18:00:00Z", "manifest": { } }
```

`manifest` is the accepted `manifest.publish` body, verbatim: the hub adds its metadata
beside the manifest rather than rewriting what the plugin published. `not_found` (404)
covers an unknown server and a server that has never had a manifest accepted alike.

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

### 9.1 Sequence numbers and acks

Each direction of each session has its own sequence space. Both work the same way, so a
plugin and a hub implement one mechanism twice rather than two mechanisms once.

- A sender assigns `seq` values that start at **1** for each new session and increase by
  one per envelope. Sequence state is per session, which is why a hub holds at most one
  live session per server (section 5.3): a restarted game server never inherits the
  ambiguous sequence state of its own previous session.
- A receiver tracks the highest `seq` it has durably processed **with no gap below it**,
  and reports that number as its ack. Acking N acks everything at or below N; there are no
  selective or negative acks.
- A sender MUST send envelopes in ascending `seq` order. A receiver MUST take them in the
  order they arrived and MUST NOT reorder them: a receiver that sorted first would ack a
  batch its sender never sent in that order, and two hubs would then disagree about what a
  given poll acked.
- A receiver MUST NOT advance its ack past a gap. An envelope above a gap MAY be discarded;
  the sender's retransmission rule recovers it.
- A receiver MUST treat an envelope at or below its ack as a duplicate: acknowledged again,
  processed no further, and never an error. This is what makes at-least-once delivery safe
  to build on.
- Acks are monotonic. A receiver MUST NOT lower an ack it has already reported, and a
  sender MUST ignore an ack below the one it has already recorded. A receiver that answers
  several requests concurrently MUST derive each reported ack from committed state rather
  than from a value read before the other requests committed.
- **Within a session**, a sender MUST retransmit every envelope above the receiver's ack
  unchanged: same `id`, same `seq`, same `ts`, same `body`. Only then can the receiver
  deduplicate.
- **Across a session change**, `seq` is the one field that MUST change. Sequence spaces do
  not survive their session, so an envelope still unacked when a session ends MUST be
  renumbered into the new session's space, keeping its `id`, `type`, `ts` and `body`. This
  applies to both directions and to every unacked envelope, whether or not it was ever
  delivered under the old session.

  Renumbering is not optional, and the alternatives do not work: resending an envelope with
  its old `seq` opens a gap the new session can never close, because the receiver is
  counting from 1 again, and dropping it instead would break the rule that nothing is
  discarded before it is acked. Deduplication is unaffected, because it keys on `id`, which
  is exactly why `id` is stable and `seq` is not.
- Unknown `type` values are ordinary traffic for this purpose: they advance the ack like
  anything else (section 4), because a receiver that stalled its ack on a message it did
  not understand would block every later message behind it.

### 9.2 Hub -> plugin

- Queued envelopes are durable. They survive a hub restart, and they survive the session
  they were queued in: an envelope that is unacked when a session ends MUST be delivered on
  the next session, renumbered into that session's sequence space (section 9.1). That covers
  both an envelope queued while no plugin was connected and one delivered under the previous
  session but never acked.
- A hub MAY cap how many envelopes it puts in one poll response (reference default 200).
  The cap does not weaken the retransmission rule: the remainder follows on later polls as
  the earlier envelopes are acked. A plugin that never acks therefore sees the same first
  batch forever, and nothing behind it, which is the same bug reported louder.
- The hub re-delivers until acked or expired. Re-delivery plus `actionId` gives
  at-least-once delivery with plugin-side dedup: the plugin MUST keep a small LRU of
  executed action ids and MUST NOT execute the same `actionId` twice.
- A plugin SHOULD ack on the poll that follows a delivery. A plugin that never acks will be
  handed the same envelopes on every poll, which is correct behavior on the hub's part and
  a bug on the plugin's.
- A hub MUST bound its per-server queue (reference default 5 000 envelopes) and MUST refuse
  new work with `outbound_queue_full` when the bound is reached, rather than discarding
  envelopes it has already accepted.

### 9.3 Plugin -> hub

- The plugin MUST persist an outbound ring buffer (file-backed where the engine allows,
  memory otherwise; reference default 5 000 envelopes) and MUST NOT drop an envelope until
  the hub has acked its `seq`. A game-server crash loses at most the unflushed tail; a hub
  restart loses nothing, because acks are only sent after durable writes.
- A hub MUST NOT ack an envelope it has not durably processed. Answering a poll is not an
  ack: the number in the response body is.
- **Durably processed** means the envelope's effect has been committed to storage that
  survives a restart, together with the ack that covers it. For a `type` the hub models,
  the effect is whatever that type's section defines. For a `type` the hub does not
  recognize, the effect is nothing at all, and the ack alone is what must be durable. What
  it never means is that a side effect outside the hub has finished: acking an envelope
  says the hub has taken responsibility for it, not that the work is done. Progress on that
  work is reported by the mechanisms the type defines, such as the action lifecycle of
  section 7.

### 9.4 Blocked-state honesty

The plugin SHOULD expose its link state in-game (`connected | degraded | buffering`), and
the hub MUST expose the mirror on the server record: `lastSeenAt`, and
`pendingEnvelopeCount`, the number of envelopes queued for the server and not yet acked.
Nothing may be dropped without a visible counter incrementing.

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
  simulated network outage (to verify buffering), a forced session change with envelopes
  still unacked (to verify renumbering, section 9.1), and a schema-invalid dispatch (which
  must never crash the game server).

An implementation that passes its suite is compliant; reading reference-implementation
source is never required.

The session-change case deserves its own mention, because it is the one rule a plugin
author is most likely to get wrong by doing the obvious thing. Everything else about
retransmission says "resend exactly what you sent"; this one case says the opposite about
`seq` alone. A plugin that replays its buffer verbatim after reconnecting will strand every
envelope in it above a gap the new session can never close, and the symptom appears only
after a game server restarts with traffic still in flight, which is precisely when nobody
is watching.

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
