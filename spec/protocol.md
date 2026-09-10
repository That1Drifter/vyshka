---
title: Protocol Specification
layout: default
nav_order: 2
---

# Vyshka Protocol Specification

**Status:** draft 0.19 (2026-09-10)
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

Every 4xx and 5xx response from either realm carries this body (a Plugin API request can
also ask for it inside a `200`, section 2.3):

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

### 2.3 Inline errors (Plugin API)

Some engine HTTP clients deliver a non-2xx response to script as an opaque error, with
neither the status nor the body (Appendix A). On such an engine section 2.2 is unreadable:
the plugin cannot branch on `error.code` and cannot even see the status it would fall back
on. Inline errors let a plugin ask, per request, for the hub's error responses to arrive
where its HTTP client can read them.

**Opt-in.** A Plugin API request opts in by carrying the query parameter `errors=inline`
exactly once, for example `POST /plugin/v1/poll?errors=inline`. The opt-in covers that
request only and nothing else; a plugin that wants it everywhere sends it on every request.
It is a request option, not a session property, so it works on the requests a session
cannot cover: enrollment, a session request that fails, and a request presenting a session
token the hub does not recognize.

**What the hub does.** For a request that opted in, every response the hub would otherwise
have sent with a 4xx or 5xx status it MUST instead send with status `200`, the
`application/json` content type, and the section 2.2 body extended with one field:

```json
{
  "error": {
    "code": "session_invalid",
    "status": 401,
    "message": "session token is expired, unknown, or superseded",
    "details": { }
  }
}
```

- `error.status` is REQUIRED in an inline error and carries the HTTP status the hub would
  have used. It is the fallback for a `code` the client does not recognize, exactly as the
  real status is in section 2.2.
- The obligation covers every Plugin API endpoint in this document, including enrollment
  (section 5.2), a failed session request (section 5.3), a held poll the hub ends because
  the session was superseded or revoked (sections 3.1.2 and 5.4), and the generic refusals
  of section 2.2 such as `payload_too_large`. A hub SHOULD apply it to any other path under
  `/plugin/` as well.
- Only the status line and the body change. Headers the hub would have sent with the
  error, other than those describing the body itself (`Content-Length`), the choice of
  `code`, `message`, and `details`, and the hub's own logging and metrics of the failure
  are unchanged: an inline error is still a failed request.
- Successful responses are unchanged: `201` for enrollment stays `201`.
- A value of `errors` other than `inline`, more than one occurrence of the parameter, or a
  query string the hub cannot parse MUST be refused with `400 bad_request` in ordinary
  form, since the hub cannot know what such a client expects. The parameter has no meaning
  on the Admin API, which always answers with ordinary statuses.
- A hub that implements inline errors MUST report `"inlineErrors": true` in the `features`
  object of the session response (section 5.3).

**What the plugin does.** A success body from any Plugin API endpoint never carries a
top-level `error` member; hubs MUST NOT add one. A plugin that opted in MUST therefore treat
a `200` whose body is a JSON object with an `error` member as the failure it describes, and
MUST NOT treat it as success. A `200` whose body is not a JSON object, or whose `error`
member is present but is not an object carrying a `code`, is a malformed response: the
plugin retries after backoff and changes no session or delivery state over it. In
particular it MUST NOT apply an `ack` from such a body.

A hub that predates this section, or one that does not implement it, ignores the parameter
and answers with ordinary statuses, as do proxies and other intermediaries that answer in
the hub's place. Opting in therefore adds information when the hub provides it and changes
nothing when it does not: a plugin MUST keep whatever handling it has for an opaque error
alongside its handling of inline ones.

**Recovery.** Whichever way an error arrives, these rules govern what a plugin does about
it. Each is stated once here rather than with its endpoint.

| Error | Plugin obligation |
|---|---|
| `session_invalid` (401) | Start a new session (section 5.3). The outbox is kept and renumbered (section 9.1). |
| `credentials_invalid`, `credentials_revoked` (401) | Do not start a session loop: retry slowly and surface the message to the operator, who has to issue a fresh enrollment token (section 5.4). |
| `enrollment_token_invalid`, `enrollment_token_used`, `game_mismatch` | Surface to the operator and retry slowly; no retry fixes these. |
| `protocol_version_unsupported` | Surface to the operator; the plugin and hub need upgrading, not retrying. |
| `envelope_invalid` (400) | The hub applied nothing, including the `ack`. The plugin MUST take the envelope at `details.index` out of its outbox before retrying, MUST NOT count it as delivered, and MUST surface it (log it, set it aside on disk) rather than discard it silently. It closes the gap by moving every queued envelope behind the removed one, in the batch or not yet sent, down one `seq` each. That shift is the one exception to sections 9.1 and 9.3, and it is safe only under the sending discipline this document already requires: a batch is a contiguous run of the outbox in ascending `seq`, and framing validity depends on the envelope's content alone, so an envelope behind the refused one can have been accepted by an earlier poll only if the refused one was too, which its refusal rules out. A plugin that cannot establish that (one that sends batches which are not such a run) MUST start a new session instead, which renumbers the whole outbox. |
| `ack_out_of_range` (400) | The plugin's inbound ack is ahead of anything the hub sent on this session, which no retry reconciles. Start a new session, whose sequence space begins at 0. |
| `bad_request` on a poll (400) | Usually the batch is over the hub's cap: retry with a smaller batch. Do not start a new session. |
| `internal` (5xx), or any unrecognized code with a 5xx `status` | Retry the same request after backoff, on the same session. |
| Unrecognized code with a 401 `status` | As `session_invalid`. |
| Unrecognized code with any other 4xx `status` | The request was wrong and a new session does not make it right: log, back off, and retry; MUST NOT start a new session over it. |

The rule the table exists for: a client error is never answered by churning sessions. A
plugin that meets `session_invalid` starts one new session, and a plugin that meets a
malformed-batch refusal corrects the batch; neither loops.

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
  When the poll opted in to inline errors (section 2.3), that answer is a `200` carrying
  the same error, still sent at once.
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

`id` is opaque to the receiver: it MUST be unique per message and identical on every
retransmission of that message, and a receiver MUST NOT parse it or require any particular
format. The reference implementations mint ULIDs, and new implementations SHOULD, but a
receiver that rejected anything else would force a ULID encoder into every game engine to
buy nothing: deduplication only needs equality.

Uniqueness does not end with the session. A renumbered envelope keeps its `id` across a
session change (section 9.1), and that surviving `id` is exactly what the cross-session
deduplication of sections 8.1 and 8.3 keys on, so a sender MUST NOT reuse an `id` for a
different message on the same server, in any session. An id generator that resets with the
session (a counter, a coarse timestamp) can make a fresh message equal a stored one, and a
receiver still holding that `id` in the matching dedup record is obliged (sections 8.1,
8.3) to treat the fresh message as a retransmission and silently drop it. ULIDs satisfy
the rule for free.

The receiver's half of that bargain is scoped, not global. The cross-session dedup rules
are defined one kind of envelope at a time, each over its own record: section 8.1
deduplicates an accepted `event.batch` on its `id`, section 8.3 an accepted `state.*`
envelope on its `id`, and no rule compares ids across the two. An `id` reused across them
(an `event.batch` and a `state.players` under the same `id`) violates the sender's MUST
NOT above but suspends no receiver obligation: each rule finds no match in its own record,
so both envelopes are processed normally, both effects stored, and both acked. Global
dedup is deliberately not on offer, and that constrains observable behavior, not storage
layout: a receiver that treated the second envelope as a duplicate would be acking an
envelope whose effect it never stored, which section 9.3 forbids, and one shared record
would merge retention horizons that sections 8.1 and 8.3 set independently, all to punish
the receiver's operator with silent data loss over the sender's bookkeeping collision. The
reference hub keeps one dedup record per rule.

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
  costs more than an approximate timestamp does. A `ts` of the wrong JSON type (a number,
  a boolean, an object, an array, `null`) is unparseable in exactly that sense, not a
  framing error: an implementation that decodes the field into a string-typed variable
  MUST still accept those, because failing the request at its decoder refuses the envelope
  over `ts` alone before any rule of the implementation's own gets to run. It also refuses
  the envelopes travelling with it, and since a sender MUST retransmit unacked envelopes
  unchanged (section 9.1) it goes on refusing them, which wedges the session rather than
  costing one poll.

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
    "linkState": "unknown",
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
- `linkState` is the link monitor's classification (section 11.1): `unknown` until a
  session has been observed, then `up` or `down`.
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
  without a version bump. The one flag this document defines is `inlineErrors: true`,
  which a hub implementing section 2.3 MUST report.
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
MUST be answered with `401 session_invalid` rather than being left to expire (inline, per
section 2.3, when the poll asked for that). The plugin then enters `buffering` (section 9)
and retries its session, which fails with `credentials_revoked` until the operator issues a
fresh enrollment token.

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
  endpoint of section 7, `manifest.*` via section 6, `event.*` via section 8.1, `state.*`
  via section 8.3) with `conflict`. This endpoint must never become a way around the
  validation those surfaces perform, nor a way to queue a message, such as a forged
  `manifest.reject`, `event.reject`, or `state.reject`, that the plugin would take as the
  hub's own word.
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
    "events": [ ],
    "kvNamespaces": [ "example-mod" ]
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
(hub -> plugin) unless the server's outbound queue is at its bound (section 9.2) or the
per-poll notice cap below has been spent, carrying
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

A hub MAY bound how many `manifest.reject` envelopes it queues for a single poll
(reference cap: 20), because a poll may legally carry hundreds of publishes and a plugin
looping on a manifest it keeps failing to fix would otherwise fill its own outbound queue
with notices about its own bug, until nothing could be dispatched to that server at all. A
hub that suppresses a notice under this cap MUST record the suppression where the operator
can see it. The rejections themselves are unaffected: what is capped is how many of them
are narrated. The bound MAY be a single budget shared with the `event.reject` cap of
section 8.1, since what it protects is the outbound queue rather than either notice kind.

The stored revision advances only on acceptance, so a corrected manifest MAY be
republished at the very revision that was just rejected. A plugin SHOULD surface a
`manifest.reject` where the server operator will see it (a log line at least) and MUST NOT
treat one as a transport error.

Within a session, a retransmitted `manifest.publish` is a duplicate like any other
(section 9.1): acked again, processed no further, and answered with no second rejection.
Across a session change a hub is not obliged to retain manifest envelope ids the way
sections 8.1 and 8.3 require for events and state, because a manifest apply is already
idempotent through its revision gate (section 6.1), so it MAY treat a renumbered
republication as new and answer it with another `manifest.reject`. The notice is
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

### 6.6 Declared KV namespaces

The OPTIONAL `kvNamespaces` array names the key/value namespaces (section 12) this
plugin's mods use. It is the sole source of a plugin's KV access: the `namespace` fields
on actions, contexts, and events group things for display and token scoping and grant
nothing, so a mod that stores no data declares no KV namespace, however many actions it
ships.

- Each entry follows the namespace grammar of section 12.1 and is at most 64 code points.
- At most 100 entries; a duplicate entry is a fault.
- An entry that breaks the grammar rejects the manifest (section 6.4), and the check is
  stricter than the length-only checks on the display namespaces above, deliberately: this
  field grants access, and a malformed grant must fail at publish time, where the plugin
  author is looking, not at the first KV call in production.

Publishing a manifest replaces the declared set. A namespace that disappears from the
manifest is refused to the plugin on every KV operation that begins after the publish is
accepted; an operation already past its confinement check when the publish commits MAY
still land under the old declaration, which is the ordering any pair of concurrent
requests already has. The keys under a withdrawn namespace are untouched, because admin
tokens and other servers' manifests may still reach them.

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

- The hub MUST validate a dispatch against the server's stored manifest before anything is
  queued: the `code` must be declared there, and `params` must satisfy the schema that
  declaration carries (section 6.1). A server with no accepted manifest can validate
  nothing, so it can be dispatched nothing. What is never acceptable is queueing first and
  wondering later: schema-invalid input must not exist anywhere on the path to the game
  server. Validation and queueing MUST be atomic with respect to manifest replacement: a
  dispatch never commits validated against a manifest that is no longer the stored one,
  however narrow the window a runtime republish opens.
- `idempotencyKey` (optional, client-chosen): a retry with the same key MUST return the
  original `actionId` instead of double-queueing. This holds in every failure mode too: a
  retry of an action that was already accepted MUST answer with the original even when a
  fresh dispatch would be refused (a full queue, a manifest that no longer declares the
  code). The key binds permanently to the first accepted request, whatever the retry's
  body says, so clients MUST use a fresh key per logical action; reusing one across
  different requests silently answers with the old action, which is exactly what the
  contract promises and exactly not what such a client meant.
- `ttlSeconds` (default 120, clamped into 1 to 86 400; absent, zero, or negative means the
  default): if the action has not reached a terminal state by the deadline, the hub marks
  it `expired`. Expiry is final and wins every race: an `action.ack` or `action.result`
  arriving after the deadline is acked at the envelope level and changes nothing, because
  the operator has already been told the action expired, and a state that flip-flops
  afterwards would be worse than a lost result. Expiry also retires the dispatch envelope
  of an action that was never numbered into a session, so an offline server's expired work
  cannot eat the section 9.2 queue bound; an envelope already numbered keeps
  retransmitting, because deleting it would open a gap the plugin's contiguous ack could
  never cross, and the deadline in its body already tells the plugin to discard it.
- State transitions only ever move forward. `queued -> delivered` happens when the plugin
  acks the envelope carrying the dispatch: the envelope is the action's vehicle, so its
  ack is the delivery receipt, and no separate receipt message exists. An `action.ack`
  also implies receipt, so a hub that learns of execution first backfills the delivery
  timestamp rather than record a run that was never delivered.

| `code` | HTTP | Raised when |
|---|---|---|
| `bad_request` | 400 | `code` missing or too long, `params` not a JSON object, body malformed |
| `unknown_action` | 409 | The server's manifest does not declare `code`, or no manifest was ever accepted |
| `params_invalid` | 400 | `params` fail the declared schema; `details.errors` lists `{ path, message }` faults |
| `not_found` | 404 | No such server |
| `outbound_queue_full` | 409 | The server's queue is at its bound (section 9.2) |

**Execution (Plugin API):** the action arrives as `action.dispatch` in a poll batch,
carrying everything the plugin needs, including the deadline it MUST discard the action
after:

```json
{
  "type": "action.dispatch",
  "body": {
    "actionId": "01J5QK...",
    "code": "example-mod.heal",
    "context": "player",
    "referenceKey": "76561198000000000",
    "params": { "amount": 100 },
    "expiresAt": "2026-08-16T18:01:00.000Z"
  }
}
```

The plugin MUST send `action.ack` on receipt (state becomes `running`) and `action.result`
when done:

```json
{ "type": "action.ack", "body": { "actionId": "01J5QK..." } }
```

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

- `ok` is REQUIRED and decides the terminal state: `completed` or `failed`. `error` is a
  human-readable string, null or absent when `ok` is true, and bounded (reference cap
  4 KiB, truncated beyond it): without a bound it would launder the result cap. A negative
  `durationMs` is dropped as unusable; the field is OPTIONAL and advisory.
- `result` is arbitrary JSON up to 64 KiB, measured on the raw bytes of the serialized
  value as received. This enables **query actions**: "dump this player's inventory" is an
  action whose result is the answer. A payload over the cap does not void the outcome: the
  hub records the terminal state, drops the payload, and says so in `error`, because
  storing it would let one action monopolize the database and rejecting the envelope would
  strand the batch behind it.
- An `action.ack` or `action.result` affects only actions belonging to the session's
  server. An `actionId` is not a credential and MUST NOT act like one: without this rule,
  any enrolled plugin that learned another server's action id could forge its outcome.
- An `action.ack` or `action.result` naming an unknown `actionId`, another server's
  action, an action already terminal, or one past its deadline is a no-op, never an error:
  at-least-once delivery makes repeats ordinary. A body the hub cannot use at all (missing
  `actionId`, missing `ok`) is acked and ignored like an unknown type, because failing the
  batch would stall every envelope behind a message a retry cannot fix.

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

| Field | Type | Rules |
|---|---|---|
| `events` | array | REQUIRED. MAY be empty, which stores nothing and is not an error. |
| `t` | string | REQUIRED. The event type (below). |
| `ts` | string | OPTIONAL. RFC 3339, when the event happened on the game server. |
| `data` | object | OPTIONAL. Type-specific payload; absent and `null` both read as `{}`. |

**Event types** are `{namespace}.{name}`: two or more non-empty segments separated by `.`,
drawn from ASCII letters, digits, `_` and `-`, and at most 128 characters in total. A hub
MUST refuse an event whose `t` is outside that grammar. The grammar is enforced rather than
merely recommended because this string is what the `events:read:{namespace}.*` scopes of
section 10 and the webhook filters of section 11 match on: a type with no namespace could
not be granted or filtered by namespace at all, and one carrying `*` or whitespace would
make those patterns ambiguous.

**Core event types** (`core.*`, normative; every plugin SHOULD emit what its game
supports): `player.connect`, `player.disconnect`, `player.death`, `player.damage`,
`player.chat`, `player.kick`, `player.ban`, `vehicle.spawn`, `vehicle.destroy`,
`object.placed`, `item.interact`, `server.start`, `server.fps` (periodic performance
sample), `server.stop`.

**Custom events** are any other type of the same form, with one exception: the `action`
and `server` namespaces are **reserved** for the hub's own lifecycle notifications
(section 11.1), and a hub MUST refuse an event whose `t` begins with `action.` or
`server.` exactly as it refuses one outside the grammar. Without the reservation, a
plugin's telemetry could impersonate the hub's own word to every webhook receiver, since
both arrive through the same fan-out. Core `server` telemetry is unaffected: those types
live under `core.` (`core.server.fps`), not under bare `server.`. Otherwise: same channel,
same storage, same webhook fan-out; hubs MUST NOT privilege core events over custom events
in routing or retention capability. A custom event MUST be accepted whether or not the
manifest declared it (section 6.3).

**Timestamps.** A hub MUST NOT refuse an event over `ts` alone; when `ts` is absent or
unparseable it MUST substitute its own receipt time, exactly as section 4 requires for the
envelope. A hub MAY also substitute receipt time for a `ts` implausibly far ahead of its own
clock (reference allowance: one hour). That bound is not mere tolerance: an event stamped a
year ahead would pin itself to the top of every operator's feed until it was pruned, and
nobody can fix another machine's clock retroactively. A hub MUST record the substituted
value where it records an event's time, and SHOULD keep receipt time alongside it, so an
operator chasing a discrepancy can see the two clocks apart.

**Rejection.** A hub MUST refuse a batch that carries more than 200 events, or any event
whose `t` is outside the grammar, whose `data` is not an object, or whose `data` exceeds the
hub's documented size limit (reference cap: 16 KiB per event). A hub MAY additionally bound
how many events one poll may carry across all of its batches (reference cap: 1 000) and
refuse the batches past that bound.

Refusal is whole-batch: a refused `event.batch` MUST store none of its events. Partial
acceptance would leave the plugin unable to tell which of its events landed, because the ack
is per envelope and this protocol has no per-event receipt; it would face the same choice a
whole rejection gives it, minus the certainty.

Like a rejected manifest (section 6.4), a rejected batch is envelope-level success: the
envelope is acked, the session stays up, the other envelopes in the poll are unaffected, and
a hub MUST NOT fail the poll over it. The rejection MUST NOT be silent either: unless the
server's outbound queue is at its bound (section 9.2) or the per-poll notice cap below has
been spent, the hub MUST queue an `event.reject`
envelope carrying the `id` of the refused envelope and `errors`, a list of
`{ path, message }` faults into the rejected body:

```json
{
  "type": "event.reject",
  "body": {
    "envelopeId": "01J5QM...",
    "errors": [
      { "path": "events[3].t",
        "message": "t must be a {namespace}.{name} type" }
    ]
  }
}
```

A plugin SHOULD surface an `event.reject` where the operator will see it and MUST NOT treat
one as a transport error. The events it names are gone; the notice is the visible counter
that section 9.4 requires before anything is dropped.

A hub MAY bound how many `event.reject` envelopes it queues for a single poll (reference cap:
20), because a poll may legally carry hundreds of batches and a plugin refusing every one of
them would otherwise fill its own outbound queue with notices about its own bug. A hub that
suppresses a notice under this cap MUST record the suppression where the operator can see it.
The refusals themselves are unaffected: what is capped is how many of them are narrated. The
bound MAY be a single budget shared with the `manifest.reject` cap of section 6.4.

**Duplicates** are deduplicated twice over, because `seq` alone cannot cover them; this is
the same two-layer rule as section 8.3, and events carry no identity of their own, so the
batch is the unit with an identity to deduplicate on. Within a session a retransmitted
`event.batch` is a duplicate like any other envelope (section 9.1): acked again, processed
no further. Across a session change `seq` is renumbered and only the envelope `id` survives,
so a hub MUST deduplicate an accepted `event.batch` on its `id` (per server), storing none
of its events when a batch under that `id` has already been stored. Without that, a restart
with an ack in flight would put every event of the replayed batch in the feed twice and fire
the webhook fan-out of section 11 twice for each. The dedup record MUST be kept at least as
long as any event the batch stored, or pruning it would reopen the replay window while the
duplicates it guards against are still visible. The obligation is bounded by retention, not
perpetual: once every event a batch stored has passed out of retention (section 8.4), a hub
MAY forget the batch's `id`, and a replay arriving after that horizon is a new batch to it,
stored and fanned out again. A plugin holding a buffer across an outage longer than the
longest retention its events resolve to is past what deduplication can promise.

A machine-readable schema for both bodies is `spec/events.schema.json`, a companion to this
section rather than a replacement for it: where the two disagree, this document wins.

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

Where events say what happened, snapshots say what *is*: the data feed for a live map.
Three plugin -> hub envelope types each carry the full current list of one kind of thing:

| `type` | List field | One entry |
|---|---|---|
| `state.players` | `players` | `{ "player": { "platform", "id" }, "name"?, "position"?, "data"? }` |
| `state.vehicles` | `vehicles` | `{ "id", "kind"?, "position"?, "data"? }` |
| `state.entities` | `entities` | `{ "id", "kind"?, "position"?, "data"? }` |

```json
{
  "type": "state.players",
  "body": {
    "capturedAt": "2026-08-25T18:00:00Z",
    "players": [
      { "player": { "platform": "steam", "id": "76561198000000000" },
        "name": "Survivor", "position": [4231.5, 300.2, 10620.0],
        "data": { "health": 82 } }
    ]
  }
}
```

A snapshot is **whole**: it replaces its predecessor of the same type entirely, and an
entry absent from it is gone. An empty list is therefore a meaningful snapshot ("nobody
is online"), where an absent list field is not a snapshot at all and rejects the body.
There is no diff form in this draft; the open question below stays open.

- `capturedAt` is OPTIONAL: when the game sampled the state. Absent, unparseable, or
  implausibly far ahead of the hub's clock, the envelope's `ts` stands in, with the
  section 4 receipt-time substitution behind that.
- Player entries MUST carry `player`, the platform-qualified identity of section 8.2, with
  `platform` (at most 64 code points) and `id` (at most 128) non-empty strings. This is
  the one field the hub enforces deeply, because it is what lets a panel or bot correlate
  a snapshot entry with events, actions, and its own records; a player list without
  stable identity is a list of labels. `name` is an OPTIONAL display label of at most 200
  code points.
- Vehicle and entity entries MUST carry `id`, a non-empty string stable for the lifetime of
  the thing it names, of at most 128 code points. `kind` is an OPTIONAL free-form label
  (`car`, `helicopter`, `tent`) of at most 128 code points.
- `position` is OPTIONAL: an array of two or three finite JSON numbers in the game's own
  map frame, advisory, for display. The hub never interprets it.
- `data` is OPTIONAL, a JSON object of game- or mod-specific extras. Unknown fields on an
  entry are tolerated everywhere, per section 2.1.
- A snapshot body is capped at 262144 bytes (256 KiB) and 5000 entries. A hub MAY lower
  neither: these are the floor a plugin may rely on.

A snapshot that breaks these rules is rejected **whole**: acked like any envelope (the
durable effect is that the stored state did not change), answered with a `state.reject`
notice shaped like the rejections of sections 6.4 and 8.1 (`envelopeId` plus `errors`,
sharing the same per-poll notice budget), and never partially applied, because a partially
applied snapshot would be a state nobody ever observed.

The hub keeps the **latest** accepted snapshot per `(server, type)`, plus a bounded
history behind it. Latest means latest *accepted*: snapshots apply in envelope order
(section 9.1), and a hub MUST NOT reorder them by `capturedAt`, whose clock it does not
own. How much history is kept is configuration in the sense of section 8.4 (reference: 24
hours, at most 500 snapshots per server and type), but the latest snapshot per type MUST
survive every retention pass: a server's last known state stays readable however stale,
and its `capturedAt` is what tells the reader how stale.

Retransmissions are deduplicated twice over, because `seq` alone cannot cover them.
Within a session a retransmitted snapshot is a duplicate like any envelope (section 9.1):
acked again, applied no further. Across a session change `seq` is renumbered and only the
envelope `id` survives, so a hub MUST deduplicate an accepted `state.*` envelope on its
`id` (per server), storing nothing for one it has already stored. Without that, the one
case section 14 calls out, a restart with traffic in flight, would put the same snapshot
in history twice with a fresh receipt time. The dedup record MUST outlive the snapshot's
history row: history is bounded by depth as well as time, so a superseded row can leave
history long before the window passes while the latest of its type survives every pass,
and a hub that forgets the pruned `id` with the row will accept the replay as new and
regress latest to a state it had already superseded, breaking the acceptance order above.
The obligation is bounded by the history window, not perpetual: a hub MUST remember an
accepted `state.*` `id` for at least that window counted from acceptance and MAY forget
it after; a replay whose `id` it has forgotten is a new snapshot to it, latest again
however stale, with `capturedAt` left to say so. A plugin holding a buffer across an
outage longer than the history window is past what deduplication can promise.

**Reading state (Admin API).** Both reads sit behind `servers:read` (section 10):

```
GET /api/v1/servers/{serverId}/state/{stateType}

-> 200 OK
{ "type": "players", "capturedAt": "2026-08-25T18:00:00.000Z",
  "receivedAt": "2026-08-25T18:00:01.000Z", "snapshot": { } }
```

`{stateType}` is `players`, `vehicles`, or `entities`; anything else is `bad_request`,
never an empty answer. `snapshot` is the accepted body, verbatim: the hub adds its
metadata beside what the plugin published rather than rewriting it. `not_found` covers an
unknown server and a server that has never had a snapshot of that type accepted alike.

```
GET /api/v1/servers/{serverId}/state/{stateType}/history?limit=20

-> 200 OK
{ "snapshots": [ { "capturedAt": "...", "receivedAt": "...", "snapshot": { } } ] }
```

History comes back newest first, in acceptance order, the latest snapshot included as its
first element. `limit` is bounded (reference default 20, cap 100) and clamped rather than
refused. There is no cursor: history is bounded by the window above, and a client that
wants more than the cap pages nothing, it asks again later.

> **Open question (pre-1.0):** whether `state.*` messages become diffs after the first full
> snapshot per session. Still deferred: the 256 KiB body cap is the tripwire, and a plugin
> that hits it is the evidence this question reopens on.

### 8.4 Retention

Events land in an append-only store with per-type retention (reference defaults: 30 days,
`core.player.chat` 90 days, snapshots 24 h). Retention values are hub configuration, not
protocol: a hub is conformant whatever it keeps, and no client may assume an event is still
readable.

Two implementation notes the reference hub follows, neither of them binding. Retention is
counted from receipt rather than from `ts`, so a game server with a wrong clock cannot talk
its own telemetry into instant deletion or into immortality. And each event is stamped with
the deadline its type resolved to when it landed, rather than having its deadline re-derived
on every pass, so that changing the configuration governs what arrives next instead of
destroying history the moment a typo is saved.

### 8.5 Reading events (Admin API)

```
GET /api/v1/servers/{serverId}/events?type=core.player.*&since=...&limit=100

-> 200 OK
{
  "events": [
    { "id": "01J5QK...", "serverId": "01J5Q...", "type": "core.player.death",
      "occurredAt": "2026-08-16T18:00:00.000Z", "receivedAt": "2026-08-16T18:00:01.000Z",
      "data": { } }
  ],
  "nextCursor": "..."
}
```

Events come back newest first, by `occurredAt`.

| Parameter | Rules |
|---|---|
| `type` | Repeatable. An exact event type, a `{namespace}.*` pattern, or `*`. Terms are ORed; absent means every type. |
| `since` | RFC 3339, inclusive, on `occurredAt`. |
| `until` | RFC 3339, exclusive, on `occurredAt`. |
| `limit` | Page size. The hub bounds it (reference default 100, cap 500) and clamps rather than refusing. |
| `cursor` | An opaque `nextCursor` from a previous page. |

`nextCursor` is absent on the last page, which is how a client knows to stop without
fetching an empty one. It is opaque per section 2.1: clients MUST NOT parse it or derive
one. A hub MUST NOT return the same event on two pages of one walk, nor skip one that was
present when the walk began, which means ordering by a timestamp alone is not enough:
several events can share a millisecond, so the order MUST be total. A cursor whose event has
since been pruned MUST still resume correctly, because a cursor names a position rather than
a row.

The same query returns core and custom events, and one `type` term may name either. Nothing
about this endpoint distinguishes them; that is what makes the routing requirement of
section 8.1 observable rather than aspirational.

`not_found` (404) covers an unknown server. An unparseable `type`, `since`, `until`,
`limit`, or `cursor` is `bad_request` (400); a hub MUST NOT silently ignore a filter it
could not parse, because the answer would then look like a real, narrower feed.

A token narrowed by an `events:read:{pattern}` scope reads this endpoint through the
intersection rules of section 10.3: an absent `type` is narrowed to what the token may see,
and an explicit term the grant does not cover is `forbidden` (403).

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
  unchanged: same `id`, same `seq`, same `ts`, same `body` (the one exception is the gap
  closed behind an envelope the hub refused as malformed, section 2.3). Only then can the receiver
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
  the hub has acked its `seq`. An envelope the hub refused as malformed (`envelope_invalid`,
  section 2.3) will never be acked; it leaves the ring buffer by being set aside where the
  operator can find it, never by being discarded. A game-server crash loses at most the
  unflushed tail; a hub restart loses nothing, because acks are only sent after durable
  writes.
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

### 10.1 Scopes

Every Admin API credential is a bearer token carrying an explicit set of scopes. A scope is
`resource:verb`, optionally narrowed by a third `:pattern` field:

```
servers:read                     server records, sessions, manifests, and state snapshots
events:read                      every event type
events:read:example-mod.*        one namespace of event types
actions:read                     every action record
actions:read:example-mod.heal    one action code
actions:dispatch                 dispatch anything (dangerous, UIs SHOULD warn)
actions:dispatch:core.player.*   one namespace of action codes
kv:rw:example-mod                one KV namespace
webhooks:manage                  webhook configuration
admin                            everything, including token management and server enrollment
```

Resources and verbs are a **closed set**. A hub MUST refuse a scope it does not define, at
mint time, rather than storing it: a token minted from a typo would grant nothing, and its
holder would find that out later from a request failing for a reason nothing reports.

A `pattern` is one of:

- `*`, or an omitted third field: every value.
- `{prefix}.*`: values under that prefix, where the separating `.` belongs to the prefix, so
  `example-mod.*` matches `example-mod.heal` and not `example-modular.heal`.
- An exact value.

Patterns draw on the identifier alphabet of section 8.1 (letters, digits, `_`, `-`, with `.`
as the separator). A `*` anywhere other than as the whole pattern or as a trailing `.*` is
not a glob this protocol defines, and MUST be refused rather than read as a literal.

Two implications are normative, and neither runs in reverse:

- **`admin` implies every other scope.** It already carries token management, which can mint
  any other scope, so withholding anything from it would be bookkeeping rather than a
  safeguard.
- **`actions:dispatch:{pattern}` implies `actions:read:{pattern}`.** A token that could start
  a job but never learn what became of it would be unusable on its own.

Scopes are **installation-wide**, not per-server: this grammar narrows by action code, event
type, and KV namespace, and has no term that names a server. A token holding
`events:read:example-mod.*` reads that namespace on every server the hub knows. Operators who
need one credential per server need one hub per server until a future draft adds a server
dimension; a hub MUST NOT invent one, because a client that assumed a narrowing this document
does not define would be relying on behavior another hub is free not to have.

### 10.2 Enforcement

A hub MUST enforce scopes on every Admin API call. Where the scope needed depends on a value
in the request, the check MUST run against that value:

| Surface | Scope |
|---|---|
| `GET /api/v1/servers`, `GET /api/v1/servers/{id}`, `GET .../manifest`, `GET .../state/{type}` and `.../history` | `servers:read` |
| `POST /api/v1/servers`, `POST .../enrollment-token`, `DELETE .../credentials` | `admin` |
| `POST /api/v1/servers/{id}/envelopes` | `admin` |
| `POST /api/v1/servers/{id}/actions` | `actions:dispatch:{the request's code}` |
| `GET /api/v1/actions/{actionId}` | `actions:read:{the action's code}` |
| `GET /api/v1/servers/{id}/events` | `events:read`, intersected per section 10.3 |
| `/api/v1/kv/{namespace}/{key}`, `POST .../incr` | `kv:rw:{the path's namespace}` |
| `/api/v1/tokens`, `/api/v1/tokens/{id}`, `GET /api/v1/audit` | `admin` |

The raw envelope endpoint of section 5.5 requires `admin` because no narrower scope in the
grammar describes it: it is an unvalidated write channel to the game server, and granting it
to anything weaker would let a scoped token route around the validation the modelled
endpoints perform.

Refusal is `403 forbidden` (section 2.2), never a silent success or an empty result. A hub
serving more than one trust boundary MAY answer `404` instead, to avoid confirming that an
object exists; the reference hub does not, because it is one operator's installation and
hiding a route from a credential that has already authenticated costs more in debugging than
it buys.

Ordering matters, in four places that are easy to get wrong:

- Every check that needs no body SHOULD run **before** the request body is read, so that a
  token it refuses is answered `403` at the headers, on reads and mutations alike, instead
  of being allowed to occupy the connection for as long as it cares to trickle a payload
  nothing will use. That covers the route-level check everywhere and the exact check
  wherever its value is carried in the path, as the KV namespace is; a check whose value
  must first be resolved from the body or from stored state (a dispatch's `code`, the code
  on the action an idempotency key names) waits for that value, and such a caller holds a
  grant on the resource by then. A refusal that never read the body is still recorded when
  section 10.5 asks for it, with an empty `payloadDigest`, because nothing was read to
  digest.

- The dispatch check MUST run **before** the manifest is consulted, so that the difference
  between `forbidden` and `unknown_action` cannot be used to enumerate a manifest the token
  may not read.
- An idempotency key is client-chosen and scoped only to a server (section 7), so the action
  it names MAY carry a different `code` than the request presenting it. A hub MUST therefore
  authorize the retry against **both** codes: the one in the request, and the one on the
  action it is about to return. Checking only the request's code lets a token present a key
  somebody else bound and receive the id and state of an action it could never have
  dispatched. Where the two differ and the token does not hold both, the answer is
  `forbidden`.
- Authorizing a read MUST NOT itself change state. Hubs that expire actions lazily on read
  (section 7) MUST establish the scope before applying that expiry, or a token with no grant
  over an action can drive its terminal state through a route that is not even audited.

A hub MUST NOT mint a token carrying a scope its minter does not itself hold.

Object ids in this protocol are unguessable (section 2.1), so a hub answering `404` before it
has established scope is not treated as a disclosure: it confirms an id the caller already
had rather than letting one be found. A hub MUST NOT rely on that for anything an operator
chose, such as an action `code` or an event type, which are guessable by construction.

### 10.3 Reading a narrowed feed

`events:read:{pattern}` does not only gate the event query of section 8.5; it shapes the
answer.

- A request carrying **no** `type` filter MUST be answered with the union of the token's
  granted patterns, not with the whole feed. The caller asked for what it may see.
- A request carrying an explicit `type` term the grant does not **cover** MUST be refused
  with `forbidden`. Narrowing an explicit filter instead would answer it with a feed that
  looks complete and is not, and nothing in the response would say otherwise.

Covering is not matching. A grant of `example-mod.heal` matches that one value but does not
cover a request for `example-mod.*`, which spans values the grant says nothing about. A grant
of `core.*` covers a request for `core.player.*`; the reverse does not hold.

A `type` term the hub cannot parse at all stays `bad_request`, not `forbidden`: a caller
debugging a typo must not be sent looking at its scopes.

### 10.4 Token management (Admin API)

`POST /api/v1/tokens` mints a token:

```json
{ "name": "panel", "scopes": ["servers:read", "events:read:example-mod.*"],
  "expiresInSeconds": 2592000 }
```

```json
{ "token": { "id": "01J...", "name": "panel",
             "scopes": ["servers:read", "events:read:example-mod.*"],
             "createdAt": "2026-08-16T12:00:00.000Z", "createdBy": "01J...",
             "expiresAt": "2026-09-15T12:00:00.000Z", "revokedAt": null },
  "secret": "vya_..." }
```

`scopes` is REQUIRED and MUST NOT be empty. `expiresInSeconds` is OPTIONAL; absent or zero
mints a token that does not expire on its own, and a hub MAY clamp a very short request up to
a floor (reference floor: 60 s). The `secret` is returned in this response and MUST NOT be
retrievable afterwards: a hub stores a digest, not the token.

`GET /api/v1/tokens` lists token records, newest first, revoked and expired ones included:
an operator auditing access needs to see what used to exist. No response other than the mint
may carry a secret.

`DELETE /api/v1/tokens/{tokenId}` revokes a token and answers `204`. Revocation MUST take
effect on the next request. The record itself MUST survive, so that the audit log's
references to it keep resolving; revoking an already revoked token is idempotent and MUST NOT
move the recorded revocation time.

### 10.5 The audit log

A hub MUST record every **authenticated** Admin API mutation, whether it succeeded or was
refused, in an append-only log. A mutation is any request whose method is not `GET` or
`HEAD`. Reads are not recorded: burying the changes under a panel's polling would defeat the
purpose of the log.

Requests that never authenticated are deliberately **not** audited. An unauthenticated caller
could otherwise fill the log an operator depends on, turning the audit trail into a denial of
service against itself. Hubs SHOULD report those refusals through ordinary logging instead.
A request that reached no handler at all, such as a wrong method against a known path
(`method_not_allowed`), is not a mutation attempt and need not be recorded either: nothing was
authorized, and nothing could have happened.

Each record MUST carry:

| Field | Meaning |
|---|---|
| `at` | when the hub answered, RFC 3339 |
| `tokenId` | the credential's id; empty for a bootstrap credential (section 10.6) |
| `tokenName` | its name **as it was at the time**, so the record survives renaming and deletion |
| `method`, `path` | the request line |
| `status` | the HTTP status the hub answered with |
| `sourceIp` | the peer address of the connection |
| `payloadDigest` | SHA-256 of the request body; empty when there was none, or when the mutation was refused before its body was read (section 10.2) |

A record MAY also carry a `serverId` and a `detail` object naming what the mutation was: the
action code and resulting id for a dispatch, the granted scopes for a mint. A hub SHOULD
record `serverId` for a mutation that names or creates a server even when the route's path
does not, or the entry an operator most wants in a per-server view, the call that created the
server, is the one missing from it. `sourceIp` MUST be the peer address of the connection and
MUST NOT be taken from a forwarded header, which is whatever the client typed.

`payloadDigest` MUST cover the whole body or be empty. A hub that caps request size MUST NOT
record a digest of the truncated prefix it read: two oversized bodies routinely share a
prefix, and a field that looks like a body digest while colliding for bodies that differ is
worse than no field at all. Nothing was accepted from such a request in any case.

An authenticated token can grow the log by repeating mutations it is not allowed to perform,
since refusals are recorded. That is the intended trade, and the lever against it is
revocation rather than silence: a hub MUST bound retention, and MAY rate-limit
(`rate_limited`, section 2.2). What a hub MUST NOT do is stop recording refusals, which are
the entries this log exists for.

Nothing in the Admin API may update or delete an individual audit record. Retention is hub
configuration rather than protocol, and a hub MUST NOT let it be unbounded in practice; the
reference hub keeps records for 365 days and prunes on the same maintenance loop as
section 8.4.

`GET /api/v1/audit` requires `admin` and answers a page of the log, newest first, with the
same opaque-cursor pagination as section 8.5. It accepts `tokenId`, `serverId`, `since`
(inclusive), `until` (exclusive), `limit`, and `cursor`. The audit log is not optional and
not a plugin.

### 10.6 Bootstrap credentials

A hub MAY accept one **configured** bootstrap credential carrying `admin`, so that a fresh
installation has a way in and an operator who has revoked every token has a way back. The
reference hub reads it from `VYSHKA_ADMIN_TOKEN` or `-admin-token`.

A hub that **generates** a bootstrap credential when none is configured MUST confine that to
first run: it MUST NOT generate one while it already holds a usable minted token. A hub
minting a fresh superuser credential on every boot would make its own token management
decorative, because revocation would not survive a restart and anyone who could read the logs
would hold permanent access.

A bootstrap credential has no token record. It therefore cannot be revoked through the API,
and its audit records carry an empty `tokenId` alongside a `tokenName` that says what it is.
It is retired by removing it from the hub's configuration and restarting.

## 11. Webhooks

Webhooks push what the hub already knows to systems that cannot poll for it: a chat
channel, a moderation bot, an ops dashboard. A webhook is a registered target URL with an
event filter; when a matching notification lands, the hub delivers a signed HTTP POST to
the target, retries failures with increasing delays, and dead-letters what never
succeeds, with every failure visible through the API.

Webhooks are observers. A delivery, failed or not, MUST NOT affect the ingest, storage,
or lifecycle of whatever triggered it, and a hub MUST NOT lose a matching notification
that arrives after the webhook was registered merely because the delivery pipeline was
busy or down: what cannot be delivered yet is queued, and what can never be delivered is
dead-lettered where the operator can see it.

### 11.1 Subscribable notifications

A webhook subscribes by type, using the pattern grammar of section 10.1 (`*`, a
`{prefix}.*` prefix, or an exact value). Two kinds of notification exist, matched by the
same patterns:

- **Telemetry**: every stored event (section 8.1), core and custom alike, under its own
  type. A refused batch stores nothing and therefore notifies nothing, and a hub MUST NOT
  privilege core events over custom events here any more than in section 8.
- **Lifecycle**: notifications the hub itself emits:

| Type | Fires when | `data` carries |
|---|---|---|
| `action.completed` | An action reaches a terminal state: completed, failed, or expired (section 7) | The action record: `actionId`, `code`, `state`, `ok`, `error`, `durationMs`, `createdAt`, `finishedAt` |
| `server.link.lost` | A server that was reachable stops being reachable | `lastSeenAt` |
| `server.link.restored` | A server whose link was lost is reachable again | `lastSeenAt` |

Link semantics: a server is **reachable** while it holds a live session and Plugin API
traffic from it has arrived recently, where "recently" is derived from the negotiated
`pollTimeout`, because that is the longest silence a healthy plugin can produce. The
reference derivation treats the link as lost when nothing has arrived for twice the
`pollTimeout` plus 10 s; a hub MAY derive differently but MUST have classified the link
as lost no later than four times the `pollTimeout` plus 30 s after the last traffic,
because a bound nothing enforces is a bound the conformance suite cannot grade and an
operator cannot rely on.

The notifications fire on observed transitions between reachable and lost, exactly once
per transition rather than once per check. A server's **first** classification, whichever
way it goes, fires nothing: a hub restarting or upgrading must not announce a fleet of
losses for servers that merely predate its bookkeeping, and a server that has never had a
session has no link to lose. `server.link.restored` fires when a server classified lost
becomes reachable again.

A hub MUST expose its current classification on the server record as `linkState`
(section 5.1): `unknown | up | down`. The bounds here are gradeable only because the state
they govern is observable, and the ceiling above bounds staleness in both directions: a
hub MUST have classified a reachable server `up`, and an unreachable one `down`, within
four times the negotiated `pollTimeout` plus 30 s of the evidence changing.

### 11.2 Registration (Admin API)

Every webhook route requires the `webhooks:manage` scope, and registration requires more:
a webhook is a standing export of everything its filter matches, so the registering token
MUST also hold grants **covering** what it subscribes to (the coverage rule of section
10.3), or `webhooks:manage` would quietly be an installation-wide read grant. Concretely:

- A pattern that can match telemetry requires `events:read` coverage of that pattern.
- A filter that admits `action.completed` requires `actions:read` (unnarrowed: the
  notification carries any code's record).
- A filter that admits the link notifications, and a non-empty `serverIds` list (which can
  otherwise be used to probe which server ids exist), require `servers:read`.
- An absent or empty `events` filter subscribes to everything and requires all three.

The namespace reservation of section 8.1 is what makes this decidable: a filter naming
only reserved lifecycle types can never match telemetry, so it needs no `events:read`. A
registration the token's grants do not cover is `forbidden` (403).

`webhooks:manage` still carries real weight on its own: it aims signed POSTs at any URL
the hub can reach, including addresses internal to the hub's own network, so operators
SHOULD grant it like the capability it is.

```
POST /api/v1/webhooks
Authorization: Bearer <admin token>
{
  "url": "https://example.net/hooks/vyshka",
  "events": ["core.player.*", "action.completed"],
  "serverIds": [],
  "template": "generic-json"
}

-> 201 Created
{
  "webhook": {
    "id": "01J5QK...",
    "url": "https://example.net/hooks/vyshka",
    "events": ["core.player.*", "action.completed"],
    "serverIds": [],
    "template": "generic-json",
    "createdAt": "2026-08-20T18:00:00.000Z"
  },
  "secret": "<signing secret>"
}
```

- `url` is REQUIRED and MUST be `http` or `https`.
- `events` is OPTIONAL: patterns in the section 10.1 grammar; absent or empty means every
  type. A pattern outside the grammar is `bad_request`, never a filter that silently
  matches nothing.
- `serverIds` is OPTIONAL: exact server ids the webhook observes; absent or empty means
  every server. An entry naming no server the hub knows is `not_found`, because a typo
  here would otherwise become a webhook that silently never fires.
- `template` is OPTIONAL and defaults to `generic-json`, the shape of section 11.3.
  `discord` is reserved for a future draft; a template the hub does not implement is
  `bad_request`.
- `secret` is minted by the hub and returned only in this response. Unlike the credentials
  of section 5, the hub cannot store a digest of it, because signing needs the secret
  itself; operators should treat read access to the hub's database as read access to
  webhook secrets.
- A webhook observes only what lands after it is registered. Registration is not a
  backfill request, and a hub MUST NOT replay stored history into a new webhook.

| Request | Result |
|---|---|
| `GET /api/v1/webhooks` | `{ "webhooks": [ ... ] }`, newest first, without secrets |
| `DELETE /api/v1/webhooks/{webhookId}` | `204`; the webhook's pending deliveries are abandoned |
| `GET /api/v1/webhooks/{webhookId}/deliveries` | The webhook's most recent deliveries, newest first (see section 11.5) |

A hub SHOULD redact the query, fragment, and userinfo of a webhook's URL wherever it logs
or audits one: target URLs routinely embed bearer credentials in exactly those parts, and
logs outlive and outtravel webhook configuration.

| `code` | HTTP | Raised when |
|---|---|---|
| `bad_request` | 400 | `url` missing or not http(s), a filter pattern outside the grammar, an unknown template |
| `forbidden` | 403 | The token's grants do not cover what the filter subscribes to |
| `not_found` | 404 | Unknown webhook id, or a `serverIds` entry naming no server |

### 11.3 Delivery

One matching notification produces one delivery per matching webhook: an HTTP POST of the
`generic-json` body to the target URL.

```
POST <target url>
Content-Type: application/json
X-Vyshka-Delivery: 01J5QN...
X-Vyshka-Attempt: 2
X-Vyshka-Signature: sha256=7f1d...

{
  "deliveryId": "01J5QN...",
  "webhookId": "01J5QK...",
  "type": "core.player.death",
  "serverId": "01J5QM...",
  "eventId": "01J5QP...",
  "occurredAt": "2026-08-20T18:00:00.000Z",
  "data": { }
}
```

- `deliveryId` is unique per delivery and stable across its retries. It is repeated in
  the `X-Vyshka-Delivery` header as a convenience, but the signed body's copy is the
  authoritative one: a receiver that deduplicates SHOULD verify the signature first and
  key on the body's `deliveryId`, because an unsigned header a forger can set must never
  be able to make a receiver discard the genuine delivery.
- `eventId` names the stored telemetry event behind a telemetry delivery, so a receiver
  can correlate with the section 8.5 feed; lifecycle deliveries omit it.
- `occurredAt` is the event's own `occurredAt` for telemetry, the action's `finishedAt`
  for `action.completed`, and the transition time for the link notifications.
- The body MUST be byte-for-byte identical on every attempt of one delivery, so its
  signature is too. `X-Vyshka-Attempt` is 1-based and lives in a header precisely so the
  body can stay stable.
- A response with a 2xx status is success. Any other status, a transport failure, or a
  response slower than the hub's delivery timeout (reference: 10 s) is failure.
- A hub MUST NOT follow redirects: a 3xx answer is a failure like any other non-2xx
  status. Following one would resend the signed body and headers to an address nobody
  registered and no audit names, which is exactly the laundering the registration-time
  URL scrutiny exists to prevent.
- Delivery order between notifications is not guaranteed; a receiver that needs order has
  `occurredAt`.

### 11.4 Signature

```
X-Vyshka-Signature: sha256=<hex>
```

where `<hex>` is the lowercase hexadecimal HMAC-SHA256 of the raw request body bytes,
keyed with the webhook's secret. Every delivery MUST be signed. The `sha256=` prefix
names the algorithm so a future draft can rotate it without changing the header. A
receiver SHOULD verify the signature with a constant-time comparison and reject anything
unsigned or mis-signed: the target URL is reachable by anyone who can reach the receiver,
and the signature is the only thing that makes a delivery the hub's word.

The signature authenticates content, not freshness: a captured delivery replays verbatim
forever. The stable, signed `deliveryId` makes replays idempotent for a receiver that
deduplicates, and an `https` target denies capture in the first place; a receiver that
needs more than that is asking a shared secret to be a session protocol and should sit
behind TLS regardless.

### 11.5 Retries and the dead letter

A delivery that failed is retried; one that can never succeed is dead-lettered, never
silently discarded. This is section 9.4's principle pointed outward: nothing is dropped
without a visible counter.

- A failed delivery MUST be retried with increasing delays, and the first retry MUST be
  **scheduled** no more than 60 s after the failed attempt: retries measured in minutes
  from the start would make webhook problems undiagnosable in any interactive session,
  and the schedule is visible through `nextAttemptAt`, which is what lets the conformance
  suite grade the bound. The attempt itself then contends with every other due delivery,
  so a hub SHOULD size its delivery concurrency so that a healthy target's retry also
  lands within the minute. Later delays are the hub's own; the reference schedule retries
  after 10 s, 1 min, 4 min, and 10 min, five attempts over roughly a quarter hour.
- When its schedule is exhausted, the delivery MUST be marked **dead** and never tried
  again. Dead is a state, not a deletion: the delivery, its attempt count, and its last
  failure MUST be retained and readable through the deliveries page until retention prunes
  them, and retention is counted from the delivery's terminal instant, not its creation,
  so a delivery that spent its whole schedule pending gets the same readable afterlife as
  one that died at once (reference: delivered and dead deliveries are kept 7 days). The
  page is finite (below), so a record can scroll out of practical reach before retention
  takes it; a cursor over deliveries is deliberately deferred to a future draft, and until
  then the caller's remedies are a wider `limit` and a shorter retention.
- A hub MAY bound how many pending deliveries it holds per webhook (reference: 1 000).
  At the bound, a new delivery for that webhook is created dead, with a `lastError`
  saying so: the queue being full is exactly the kind of failure webhooks exist to make
  visible, so it must not be hidden by discarding the evidence.
- `GET /api/v1/webhooks/{webhookId}/deliveries` exposes the record, newest first:

```
-> 200 OK
{
  "deliveries": [
    { "id": "01J5QN...", "type": "core.player.death", "serverId": "01J5QM...",
      "state": "pending", "attempts": 2, "lastStatus": 500, "lastError": "status 500",
      "createdAt": "...", "nextAttemptAt": "...", "deliveredAt": null }
  ]
}
```

`state` is `pending | delivered | dead`. `attempts` counts attempts made; `lastStatus` is
the last HTTP status received, absent when the failure was transport-level, and
`lastError` a human-readable account of the last failure. `nextAttemptAt` is present
while the delivery is pending. The page is bounded, but the bound is the caller's to
widen: `limit` is clamped into the hub's range (reference default 100, cap 500) rather
than refused, the same contract as section 8.5's feed.

## 12. Key/value store

Per-mod persistence so mods do not need their own database. A key is addressed as
`{namespace}/{key}`; a value is one JSON value.

The store is **installation-wide**, like the scope grammar of section 10 that guards it:
there is no server dimension. Two servers whose manifests declare the same namespace read
and write the same keys, which is the feature; a mod that wants per-server keys encodes the
server in the key. Isolation between mods is the namespace, and nothing else.

### 12.1 Names and values

- A **namespace** is one or more non-empty segments of letters, digits, `_`, and `-`,
  separated by `.` (the identifier alphabet of section 8.1), at most 64 code points in all.
- A **key** follows the same grammar, at most 128 code points. Keys are case-sensitive and
  never contain `/`, so both names travel in a URL path without escaping, which matters on
  engines with no URL-encoding library.
- A **value** is any JSON value except `null` (a `null` where an optional field could
  appear reads as the field being absent, section 6.4, so a null value could not be told
  from no value at all). Its encoding is at most 16384 bytes.
- Every key carries a **revision**: 1 when the key is created, incremented by one on every
  successful write. Revisions live in `[1, 2^53)`, the exactness bound the rest of this
  document uses; a write that would exceed it is refused with `conflict`. Deleting a key
  discards its revision, and a key created again starts at 1: a compare-and-swap answers
  "has this key changed since I read revision n", and a delete-and-recreate that happens to
  realign revisions is a case a mod that needs tombstones must handle with a marker value.
- A key MAY carry a **TTL**. From the moment its expiry passes, the key MUST read as
  absent on every operation; when the hub physically deletes it is the hub's business.

### 12.2 Operations

The same operations exist in both realms, as synchronous HTTP request/response, never as
envelopes: a compare-and-swap over an at-least-once queue could not tell its caller
whether it won. There are four endpoints; the decrement rides `incr` as a negative delta,
and the compare-and-swap rides `set` as `ifRevision`. They carry no sequence numbers and no acks; a client that retries a write
after a network failure uses `ifRevision` when it needs to know whether the first attempt
landed.

| Operation | Plugin API | Admin API |
|---|---|---|
| get | `GET /plugin/v1/kv/{namespace}/{key}` | `GET /api/v1/kv/{namespace}/{key}` |
| set | `PUT /plugin/v1/kv/{namespace}/{key}` | `PUT /api/v1/kv/{namespace}/{key}` |
| delete | `DELETE /plugin/v1/kv/{namespace}/{key}` | `DELETE /api/v1/kv/{namespace}/{key}` |
| incr | `POST /plugin/v1/kv/{namespace}/{key}/incr` | `POST /api/v1/kv/{namespace}/{key}/incr` |

The Plugin API side authenticates with the session token of section 5.3; the Admin API
side with a bearer token holding `kv:rw:{namespace}` (section 10).

**get** answers `200` with the key, or `not_found` when it is absent or expired:

```json
{ "namespace": "example-mod", "key": "balance.76561198000000000",
  "value": 250, "revision": 7, "expiresAt": "2026-09-01T00:00:00Z" }
```

`expiresAt` is present only when the key carries a TTL.

**set** writes one value and answers `200` with the key's new `revision` (and `expiresAt`
when a TTL was set). The body:

```json
{ "value": { "any": "JSON" }, "ifRevision": 7, "ttlSeconds": 3600 }
```

- `value` is REQUIRED.
- `ifRevision` is OPTIONAL and makes the write a compare-and-swap: `0` means "only if the
  key does not exist", `n >= 1` means "only if the current revision is exactly n". A
  mismatch is answered `409 revision_mismatch` with `details.revision` carrying the current
  revision (`0` when the key does not exist), which is what lets the loser re-read and
  retry without a second round-trip. Absent means unconditional.
- `ttlSeconds` is OPTIONAL, an integer in `[1, 315360000]` (ten years); anything longer is
  a no-expiry key wearing a costume, and is refused rather than silently clamped to
  something the caller did not write. A set defines the key entirely: absent means the key
  does not expire, whatever TTL it carried before.

`ifRevision` is REQUIRED behavior, not an extension, because it is the whole concurrency
model: the moment a mod in-game and a bot over the Admin API write the same key, one of
them is wrong unless one of them can lose.

**delete** removes the key, answering `204`, or `not_found` when it is absent or expired.
A client that retries a delete treats `not_found` as success: the key is gone either way.
Delete is unconditional in this draft; a mod that needs a guarded delete keeps a marker
value and uses `ifRevision` on the set that writes it.

**incr** atomically adds an integer to a key, creating it when absent. The body is
OPTIONAL, and a body of JSON `null` reads as an absent body, the same substitution
section 6.4 makes for optional fields:

```json
{ "delta": -5 }
```

- `delta` is an integer in `(-2^53, 2^53)`, defaulting to 1. There is no separate
  decrement operation: `decr` is `incr` with a negative `delta`.
- When the key is absent or expired, it is created with `value` = `delta`, revision 1, and
  no TTL.
- When it exists, its value MUST be a JSON integer in `(-2^53, 2^53)`, and the sum MUST
  stay inside that range; otherwise the answer is `conflict` and nothing changes. A
  successful incr increments the revision and **preserves** the key's TTL: a counter's
  lifetime is set where the counter is defined, not reset by every bump.

The answer is the get shape, with the new value and revision. Concurrent incrs MUST each
land exactly once: two clients adding 1 to a key at revision n leave it at n+2 with both
deltas applied, never n+1. This is the one operation whose atomicity the hub owes the
client outright, with no `ifRevision` in the loop.

### 12.3 Confinement

- **Plugins** may operate only on the namespaces the server's stored manifest declares in
  `kvNamespaces` (section 6.6). Any operation on any other namespace, and any operation
  while the server has no stored manifest, is `403 forbidden`. The check runs before the
  key is looked up, so the difference between `forbidden` and `not_found` cannot probe a
  namespace the plugin was not granted.
- **Admin tokens** need `kv:rw:{namespace}` (or `admin`), checked against the namespace in
  the path, likewise before the key is looked up. The verb is `rw`: this draft defines no
  read-only KV grant, and a hub MUST NOT invent one (section 10.1's closed set). Because
  the namespace is path-carried, a hub following section 10.2's ordering runs this check
  at the headers, so a malformed namespace *outside* the caller's grant MAY answer
  `forbidden` rather than the `bad_request` below: the scope refusal comes first, and the
  grammar of a name the token could never touch is not its business.

| `code` | HTTP | Raised when |
|---|---|---|
| `bad_request` | 400 | Malformed namespace, key, value, `ifRevision`, `ttlSeconds`, or `delta`; value over 16384 bytes |
| `forbidden` | 403 | Undeclared namespace (Plugin API) or missing `kv:rw` grant (Admin API) |
| `not_found` | 404 | get or delete on a key that is absent or expired |
| `revision_mismatch` | 409 | `ifRevision` does not match the current revision; `details.revision` carries it |
| `conflict` | 409 | incr on a non-integer value, an arithmetic result outside `(-2^53, 2^53)`, or a revision at the `2^53` bound |

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

- **Hub conformance** runs against any hub URL and answers "is this hub compliant?",
  including whether every Plugin API refusal arrives inline when asked for (section 2.3).
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
  generated from manifest schemas. A typed decoder must not be allowed to refuse a whole
  poll response over one field it did not expect, though: the response carries every
  envelope in the batch, section 4 obliges the receiver to accept types and fields it does
  not understand, and a decoder that fails the batch would make the hub resend it forever.
  The reference DayZ plugin reads responses into a small generic JSON tree for this reason.
- Some engine HTTP clients expose no way to set a request header other than the content
  type. Where the content-type value reaches the wire verbatim, a plugin can append a CRLF
  and the `Authorization: Bearer ...` line to it; the reference DayZ plugin does, and the
  measurement behind that is in its repository. A hub sees ordinary headers either way.
- Some engine HTTP clients deliver a non-2xx response as an opaque error code, without the
  status or the body, so the `error.code` of section 2.2 and even the status-based fallback
  it names are out of reach. Such a plugin SHOULD opt in to inline errors (section 2.3) on
  every request, which brings the code and the status back within reach of the success
  callback, and SHOULD renew its session ahead of `sessionExpiresAt` so that an expiry never
  has to be inferred from a refusal. It still needs a fallback for the opaque case, because
  an older hub or a proxy answering in the hub's place will produce one: reason from the
  error class alone, and conservatively. A client error on a poll is answered by starting
  one new session, which is legal at any time (section 5.3) and is the right response to
  `session_invalid`; if the new session's first poll fails the same way, the plugin backs
  off rather than loops, because the cause is then almost certainly its own batch. A client
  error on a session request is treated as rejected credentials and retried slowly; a
  client error on enrollment is surfaced to the operator, because no retry fixes a burned
  or unknown token. The reference DayZ plugin takes the query parameter through the same
  `POST` path as everything else; the measurement that it survives the engine's client is
  in its repository.
- File-backed ring buffers get whatever fsync semantics the engine provides; document the
  loss window honestly rather than claiming durability the engine cannot deliver.
- Engines with richer facilities (e.g. Arma Reforger's Enfusion) SHOULD still implement
  long-poll first and treat WebSocket as an upgrade, possibly via an external sidecar
  process bridging to the game.
