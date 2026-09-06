# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Until the
first release, entries accumulate under **Unreleased**; dates mark when a change landed on
`main`. The protocol document, the hub, and each plugin will be versioned independently
(SemVer) once implementation starts, and this file will gain per-artifact sections at that
point if needed.

## [Unreleased]

### Added

- 2026-09-05: completed the DayZ live-player heal acceptance demo for issue #14:
  curl-to-hub dispatch returned health 100, blood 5000, and shock 100, with player visual
  confirmation; a fresh heal also cleared bleeding. Idempotent retry, nonexistent-player
  failure, and invalid-parameter rejection passed. Recorded the M2 decision to defer readable
  Plugin API transport errors to issue #43, a hardening gate before claiming readiness for
  unattended operation. The live action tests do not establish transport-error recovery.
  No runtime or normative protocol changes; evidence and decision are recorded in #14 and #43.

- 2026-09-03: DayZ reference plugin, clean-room (issue #14), the first real game plugin and
  the first evidence the protocol is implementable on a constrained engine rather than only
  against a fake plugin. A server-side Enforce Script mod under `plugins/dayz/mod` speaks the
  full Plugin API baseline: it enrolls once and persists its credentials, starts a session on
  every boot, long-polls, publishes a one-action manifest, executes dispatched actions behind
  an executed-actionId LRU, and answers each with an `action.ack` and `action.result`. The one
  built-in action, `vyshka.heal`, restores a player's health, blood, and shock and clears
  bleeding, keyed on the platform-qualified identity of protocol section 8.2 (the plain
  Steam64 id). The plugin passes all eleven checks of the plugin conformance harness against a
  live DayZ 1.29 dedicated server, including the forced re-delivery, the transport outage, and
  the session-change renumbering that section 9.1 warns is the rule an author is most likely to
  get wrong. The outbox is one file per unacked envelope under the profile directory, written
  before an envelope is first sent and deleted only when the hub's ack covers it; sequence
  numbers are assigned at send time and never stored, so renumbering across a session change is
  automatic and a verbatim replay is impossible by construction. Generic JSON is hand-parsed
  into a tree (`VyshkaJson.c`) rather than decoded through a typed serializer, because a typed
  decoder that failed on one unexpected field would refuse a whole poll response and wedge the
  session. A Go tool, `plugins/dayz/cmd/vyshka-dayz`, packs the mod into a PBO with a pure-Go
  packer (`plugins/dayz/pbo`, round-trip tested against a shipped archive) and launches a
  dedicated server as a conformance candidate; no DayZ Tools are required.

  Two engine limits were measured first rather than assumed, extending the spike started for
  the poll-timeout floor. `spikes/dayz-restapi-headers` establishes that the one request header
  the engine exposes reaches the wire verbatim, so the bearer token of section 2.1 travels as a
  smuggled `Authorization` line; that a non-2xx response reaches script as an opaque error code
  with neither status nor body, so the protocol's `error.code` is unreadable on DayZ and the
  plugin must reason from the error class alone; and that 64 KiB requests and 1 MiB responses
  pass intact. Protocol draft 0.18: Appendix A gains implementation notes for engines that
  expose one header and hide error bodies. The design companion records the unreadable-error
  finding as a new pre-1.0 open question (a hub-side soft-error mode) and updates the DayZ
  reference-plugin notes; milestone M2's remaining item is the live-player heal demo, which
  needs a human-joined client and is a manual smoke test beyond the harness. New `plugins/dayz`
  tree, new `spikes/dayz-restapi-headers`, no changes to the hub or the conformance suites.

- 2026-08-25: state snapshots and live state endpoint (spec section 8.3, issue #12). The
  data feed for the live map: section 8.3 grows from a stub into the full contract, and
  the hub implements it. Three plugin -> hub envelope types (`state.players`,
  `state.vehicles`, `state.entities`) each carry the full current list of one kind of
  thing; a snapshot replaces its predecessor whole, an empty list is a meaningful answer
  ("nobody online"), and there is no diff form (the pre-1.0 open question stays open, with
  the 256 KiB body cap named as its tripwire). Player entries must carry the
  platform-qualified identity of section 8.2, the one field enforced deeply, because
  identity is what lets a reader correlate a snapshot with events and actions; vehicle and
  entity entries need a stable `id`. Optional `capturedAt` falls back through the
  envelope's `ts` to receipt time with section 4's wrong-clock tolerance. An invalid
  snapshot is rejected whole (a partially applied snapshot would be a state nobody ever
  observed), acked, and narrated with a `state.reject` notice sharing the per-poll notice
  budget; the `state.*` family joins the reserved list on the raw envelope queue so the
  notice cannot be forged. Snapshots apply in acceptance order, never reordered by
  `capturedAt`; storage keys "latest" off an autoincrement sequence because ULIDs go
  arbitrary within a millisecond. The hub keeps the latest snapshot per (server, type),
  which survives every retention pass however stale, plus bounded history (config:
  24 h window, depth 500, the depth enforced at insert so a fast plugin cannot outrun the
  prune). Two Admin API reads behind `servers:read`:
  `GET /api/v1/servers/{id}/state/{type}` for the live answer and `.../history` for
  recent snapshots newest first. Section 5.5's reserved-family list now also names
  `event.*`, closing an existing spec-code drift. Four new conformance checks (70 total)
  grade replacement and history order, whole-rejection with the notice, cross-session
  replay dedup, and the type/scope/forgery guards; `scripts/demo-state.sh` walks the
  issue's demo path (plugin pushes a player snapshot, admin curl returns the live list).
  New migration `0010_state.sql`, new companion `spec/state.schema.json`, Admin OpenAPI
  updated; protocol draft 0.13.

  An adversarial review pass then trued up the edges before landing. The real find:
  a snapshot retransmitted across a session change is renumbered (section 9.1), so `seq`
  alone cannot deduplicate it, and the first cut would have stored it twice with a fresh
  receipt time; snapshots now dedup on the envelope `id` per server (a unique index the
  insert defers to), and section 8.3 states the two-layer rule honestly instead of
  claiming `seq` covers everything. Unknown-field tolerance now genuinely holds inside
  snapshot bodies (the unconsulted list fields decode lazily, so a `state.players` body
  carrying a strangely shaped `vehicles` field is tolerated per section 2.1 rather than
  rejected at the decoder). The entry length caps the hub enforces (platform 64, player
  id 128, name 200, kind 128) are now in the normative prose, not just the code and
  companion schema. State-type and limit validation run before the server lookup, so an
  unusable request is `bad_request` whether or not the server exists. The four clamped
  `limit` query parameters across the Admin OpenAPI no longer declare a `maximum` a
  validating client would enforce against a server that clamps, and
  `spec/state.schema.json` joined CI's schema validation list. Conformance grew the
  replay-dedup check plus teeth the review showed were missing: a byte-honest verbatim
  round-trip with unknown fields, a replacement whose `capturedAt` is older than its
  predecessor's (acceptance order, not the game's clock, decides "latest"), a
  cap-boundary player id, history behind `servers:read`, and raw-queue refusal of the
  whole `state.*` family.

- 2026-08-25: per-mod key/value store (spec section 12, issue #11). Section 12 grows from a
  stub into the full contract and the hub implements it on both realms: `get`, `set`,
  `delete`, and atomic `incr` on `{namespace}/{key}` paths under `/api/v1/kv/...` and
  `/plugin/v1/kv/...`, as synchronous request/response rather than envelopes, because a
  compare-and-swap over an at-least-once queue could not tell its caller whether it won.
  Values are any JSON value except null up to 16384 bytes; every key carries a revision (1
  at creation, +1 per write) and `set` takes an optional `ifRevision` guard whose loss is
  `409 revision_mismatch` with the current revision in `details.revision`, so the loser
  retries without a second read. `incr` takes a signed `delta` (decrement included),
  creates absent keys, refuses non-integer or out-of-exactness-bound arithmetic with
  `conflict`, and must land concurrent bumps exactly once. Optional `ttlSeconds` expires a
  key into absence the moment its deadline passes, with physical deletion left to the
  retention pass; `incr` preserves a key's TTL while `set` redefines it. The store is
  installation-wide like the scope grammar that guards it: admin tokens need
  `kv:rw:{namespace}` checked against the path before the key is looked up, and plugins are
  confined to the namespaces their manifest declares in the new `kvNamespaces` array
  (section 6.6), which is validated against the full namespace grammar at publish time
  because it grants access. Six new conformance checks grade the cross-realm round-trip,
  CAS arbitration, incr atomicity, TTL expiry, confinement on both realms, and input
  validation; `scripts/demo-kv.sh` walks the issue's demo path (plugin writes, a bot's
  stale CAS is rejected, the fresh one wins). New migration `0009_kv.sql`; protocol draft
  0.12; both OpenAPI documents and `manifest.schema.json` updated.

  An adversarial review pass then trued up the edges. The store now reads its clock after
  winning the database connection, not before, so a TTL cannot arrive partly spent and a
  liveness decision cannot be made from a timestamp that aged in the queue. The ten-year
  `ttlSeconds` ceiling the hub enforces is now in the spec and both OpenAPI schemas rather
  than being an undocumented refusal, the OpenAPI schemas gained the exactness bounds on
  `ifRevision` and `delta` and the not-null rule on `value`, and `manifest.schema.json`
  gained `uniqueItems` on `kvNamespaces`. Section 6.6 now states the concurrency of
  namespace withdrawal precisely (a publish stops operations that begin after it commits,
  not one already past its confinement check), and section 12.2 says a JSON `null` incr
  body reads as an absent body. Conformance grew to cover the Plugin API's incr and TTL
  surfaces, a null `value`, an out-of-bound `delta`, TTL clearing by a TTL-less set, and
  the revision restart of a compare-and-swap over an expired key.

- 2026-08-20: webhooks with signed delivery (spec section 11, issue #10). Section 11 grows
  from a stub into the full contract, and the hub implements it: `POST /api/v1/webhooks`
  registers a target URL with an event filter (type patterns in the section 10.1 grammar,
  optional exact server ids) behind the `webhooks:manage` scope, and every matching
  notification is POSTed to the target as a `generic-json` body signed with
  `X-Vyshka-Signature: sha256=<hex HMAC-SHA256>` under a per-webhook secret returned once at
  registration. Subscribable notifications are every stored telemetry event plus three the
  hub emits itself: `action.completed` on every terminal action state, expiry included, and
  `server.link.lost` / `server.link.restored`, driven by a link monitor that derives its
  loss threshold from the negotiated `pollTimeout` (twice it, plus 10 s) and fires once per
  transition, never for a server that was never seen. Delivery is an outbox: stored events
  and finished actions carry a `notified` flag cleared in the same transaction that enqueues
  their deliveries, so a crash costs neither a lost notification nor a duplicate, and the
  payload is rendered once so every attempt sends byte-identical, identically signed bytes
  with the attempt number in a header. Failures retry on a schedule whose first retry the
  protocol pins inside 60 s, precisely so retry behavior is observable by a conformance
  suite and an operator alike (reference: 10 s, 1 min, 4 min, 10 min); an exhausted schedule
  dead-letters the delivery, dead being a state that stays readable through
  `GET /api/v1/webhooks/{id}/deliveries` along with attempt counts and last failures, and a
  per-webhook pending bound creates over-bound deliveries dead with a reason rather than
  discarding the evidence. Five new conformance checks grade registration and secret
  hygiene, scope enforcement, the signed delivery, visible retries, and both link
  transitions; the suite gains `-webhook-listen` because grading push delivery means being a
  target. `scripts/demo-webhooks.sh` shows a local receiver getting a signed
  `core.player.death` and verifying it with openssl. Protocol draft 0.11; the `discord`
  template stays reserved for a follow-up.

  An adversarial review pass then hardened the security model beyond the first cut, and the
  spec with it. Registration now requires grants covering what the filter subscribes to,
  because `webhooks:manage` alone would otherwise have been a quiet installation-wide read
  grant: `events:read` coverage per telemetry pattern, `actions:read` for a filter admitting
  `action.completed`, `servers:read` for the link notifications or a named `serverIds` list.
  The `action` and `server` event namespaces are reserved at ingest (section 8.1) so plugin
  telemetry can never impersonate the hub's lifecycle notifications to a receiver, which is
  also what makes the coverage rule decidable. Redirects are never followed, a webhook's URL
  is redacted in logs and the audit trail, and webhooks are read inside the fan-out
  transaction with a registration-time boundary on every notification, closing both the
  backfill and the lost-notification races a snapshot outside the transaction allowed. Link
  transitions are guarded on the exact `lastSeenAt` they were computed from and a
  restoration additionally on a live session existing inside the transaction, a server's
  first classification is silent in both directions, the server record now exposes
  `linkState` (section 5.1) so the state the bounds govern is observable, and the spec
  gained a normative ceiling (four `pollTimeout`s plus 30 s) bounding both the loss and the
  classification lag. Delivery retention counts
  from the terminal instant, the deliveries page takes `limit`, attempts record their
  outcome even when the target is slower than the bookkeeping, and shutdown cancels
  in-flight attempts instead of waiting them out. The conformance checks grew teeth to
  match: all four routes are scope-checked, the secret's value is searched for in raw
  response bytes rather than under a guessed field name, deliveries must be well-formed
  POSTs with exactly one signature header, lifecycle deliveries are signature-verified too,
  the retry schedule is graded through `nextAttemptAt`, and `-webhook-advertise` separates
  where the receiver binds from where the hub is told to deliver.

- 2026-08-20: the plugin conformance harness (spec section 14, issue #9), the artifact that
  makes third-party plugins possible. `conformance/plugin` is a mock hub a candidate plugin
  points at instead of a real one: it walks the plugin through enrollment, sessions,
  long-poll, manifest publish, and action round-trips, then misbehaves on purpose the way
  only a hub can, redelivering a delivered envelope verbatim, dispatching the same actionId
  in a fresh envelope, withholding acks, resetting every connection to simulate an outage,
  and invalidating the session with envelopes still unacked. Eleven staged checks grade the
  responses; the stage that earns the harness its keep is the session-change one, which
  forces the section 9.1 renumbering case (seq moves, id, type, ts and body do not) and
  names a verbatim buffer replay explicitly, because that failure only surfaces in the wild
  when a game server restarts with traffic in flight. Everything the plugin sends is also
  validated as it arrives, and violations are recorded as faults citing the spec section
  broken, failing the stage they occurred in. The checks are stages rather than the hub
  suite's independent checks because the candidate is one long-lived process: each stage
  builds on the last, and a failed prerequisite fails its dependents loudly instead of
  skipping them. The harness synthesizes dispatch params from whatever schema the
  candidate's manifest declares (and, for the crash check, params that violate it), so it
  grades real plugins, not just ones that declare a blessed test action; the one requirement
  beyond the protocol is that the manifest declare at least one action. A reference
  candidate lives in `conformance/plugin/driver`: a minimal autonomous plugin with an
  executed-actionId LRU, an unacked-envelope buffer, and session-change renumbering, which
  CI now grades on every push to keep the suite honest in the green direction, while
  `harness_test.go` points deliberately broken clients at the mock hub to keep it honest in
  the red direction. Neither imports hub code; both speak only HTTP. Until now a single
  bootstrap credential could do everything on the Admin API, and every slice since enrollment
  had widened what that meant. A token now carries an explicit set of grants in a closed
  `resource:verb[:pattern]` grammar, and every route enforces one: `servers:read`,
  `events:read`, `actions:read`, `actions:dispatch`, `kv:rw`, `webhooks:manage`, and `admin`.
  Where the answer depends on a value in the request rather than the route, the value is what
  gets checked, so a token scoped to `actions:dispatch:example-mod.heal` dispatches that code
  and is refused on every other one. Two implications are normative and neither runs backwards:
  `admin` implies everything, because it already carries token minting; and dispatching an
  action implies reading it, because a token that could start a job but never learn its outcome
  would be unusable alone. `actions:read` is new in this draft, added so that a monitoring
  token does not have to hold write access to watch a job. A scope the hub does not define is
  refused at mint rather than stored, since a token minted from a typo grants nothing and says
  so nowhere. An `events:read:{pattern}` grant does not merely gate the section 8.5 feed, it
  shapes it: a query with no `type` filter is narrowed to what the token may see, while an
  explicit term the grant does not *cover* is refused, because narrowing an explicit filter
  would answer it with a feed that looks complete and is not. Covering is deliberately not
  matching, so a grant on one event type does not admit a query spanning its whole namespace.
  Refusals are `403 forbidden`, never a quiet empty result, and the dispatch check runs before
  both the manifest lookup and the idempotency lookup so neither can be used to learn about an
  action the token may not dispatch. Every authenticated mutation is recorded in an append-only
  audit log with the token's id and its name as it was at the time, the request line, the
  status, the peer address, and a SHA-256 digest of the request body; refused mutations are
  recorded alongside successful ones, because an attempt to use a credential outside its grant
  is exactly what the log is read for. Reads are not recorded, and neither are requests that
  never authenticated: an unauthenticated caller could otherwise fill the log an operator
  depends on. `sourceIp` is the connection's peer address and never a forwarded header, which
  is whatever the client typed. New endpoints: `POST`/`GET /api/v1/tokens`,
  `DELETE /api/v1/tokens/{tokenId}`, and `GET /api/v1/audit`, all requiring `admin`, with the
  same opaque-cursor pagination the event feed uses. A secret is returned by the mint response
  and nowhere else; revocation takes effect on the next request while the record survives, so
  the log's references to a retired token keep resolving. The generated bootstrap credential is
  now confined to first run: a configured `VYSHKA_ADMIN_TOKEN` is always honoured, but the hub
  stops minting an ephemeral one once it holds a usable scoped token, because a fresh superuser
  credential on every boot would make revocation not survive a restart and hand permanent
  access to anyone who could read the logs. Audit records are pruned on the same maintenance
  loop as events (reference retention: 365 days), and `Config.EventPruneInterval` is renamed
  `RetentionInterval` now that it drives both passes. Protocol draft 0.10 makes all of this
  normative in a rewritten section 10 (scopes, enforcement, narrowed feeds, token management,
  the audit log, and bootstrap credentials), and `spec/openapi-admin.yaml` 0.9.0 covers the new
  endpoints and the `403` answers. Scopes are installation-wide in this draft: the grammar
  narrows by action code, event type, and KV namespace, and has no term naming a server, which
  section 10.1 now states outright rather than leaving to be discovered. Six new hub
  conformance checks (fifty-five total) grade the token lifecycle, a narrowly scoped token
  reaching nothing outside its grant, the idempotency-key oracle below, mint-time grammar
  enforcement, a namespace-scoped feed narrowing and refusing, and the audit record of a
  dispatch and its refusal. Migration `0007_tokens_audit.sql` adds the `admin_tokens` and
  `audit_records` tables. `scripts/demo-tokens.sh` walks the whole thing in curl.

  Hardened under adversarial review before landing. The one that mattered: an idempotency key
  is client-chosen and keyed only on the server, so the action it names can carry a different
  code than the request presenting it, and authorizing only the request's code let a token
  present a key somebody else had bound and receive the id and state of an action it could
  never have dispatched. The retry is now authorized against both codes, which is the rule
  section 10.2 states and the new conformance check grades. Reading an action no longer
  expires it before the scope check: the full read applies lazy expiry, so a token with no
  grant over an action could drive its terminal state through a route that is not even
  audited, and the code is now read on its own first. The first-run bootstrap condition was
  wrong in the direction that locks operators out: it suppressed the generated credential when
  *any* live token existed, so minting a read-only panel token and restarting shut the
  operator out of their own Admin API, and it now asks whether a live token can still manage
  tokens. `expiresInSeconds` is clamped in seconds before conversion, so a request for a
  century no longer overflows `time.Duration` and mints a one-minute credential, the same trap
  `clampActionTTL` already avoided. An audit entry is created before the request body is read,
  so an authenticated caller cannot make a refused mutation vanish by hanging up mid-body; an
  over-cap body records no digest rather than a digest of the truncated prefix, since two
  oversized bodies routinely share a prefix and a colliding field that looks like a body
  digest is worse than an empty one; and creating a server now stamps the audit record's
  `serverId`, which the route's own path does not carry, so the entry an operator most wants
  in a per-server view is actually in it.

- 2026-08-16: telemetry event ingest and query (spec section 8), the first surface that runs
  plugin to operator rather than the other way. A plugin pushes `event.batch` envelopes over
  the existing transport; core `core.*` events and mod-defined `{namespace}.{name}` events
  share one channel, one append-only table, and one query, so nothing in the hub can
  privilege a core event over a custom one. Undeclared custom events are stored like any
  other (section 6.3). Events land in the same transaction as the inbound ack that covers
  them, which makes ingest idempotent through the section 9.1 sequence machinery alone: a
  retransmitted batch is stored once, with no per-event identity. An event whose `ts` is
  absent, unparseable, or implausibly far ahead of the hub's clock takes receipt time
  instead, and both clocks are reported side by side rather than reconciled. A batch over
  200 events, or carrying an event whose type is outside the `{namespace}.{name}` grammar or
  whose `data` is not an object or exceeds 16 KiB, is refused whole and answered with a
  queued `event.reject`; the poll still succeeds and the envelope is still acked, as with
  `manifest.reject`. Operators read the feed at
  `GET /api/v1/servers/{serverId}/events` with repeatable `type` patterns (exact,
  `{namespace}.*`, or `*`, ORed), a half-open `since`/`until` window, and opaque cursor
  pagination ordered on `(occurredAt, id)` so events sharing a millisecond can neither repeat
  nor go missing. Retention is per type pattern (reference defaults: 30 days, 90 for
  `core.player.chat`), stamped at ingest and counted from receipt, with a bounded prune pass
  on the hub's maintenance loop. Protocol draft 0.8 makes the batch shape, the type grammar,
  the timestamp substitution, whole-batch rejection, and the query endpoint normative
  (sections 8.1, 8.4, and the new 8.5); `spec/events.schema.json` is the machine-readable
  companion, validated in CI, and `spec/openapi-admin.yaml` 0.8.0 covers the query. Five new
  hub conformance checks (forty-eight total) grade a core and a custom event coming back
  through one filter, whole-batch rejection with its notice, filtering and pagination,
  retransmission storing once, and one server's telemetry staying out of another's feed.
  Migration `0006_events.sql` adds the `events` table. `scripts/demo-events.sh` walks the
  whole exchange in curl. The raw queue endpoint now refuses `event.*` alongside `manifest.*`
  and `action.*`, so a forged `event.reject` cannot be queued as the hub's own word.
  Hardened under adversarial review before landing: an event's `ts` is decoded as raw JSON
  rather than a string, so one wrongly typed timestamp costs that timestamp instead of
  failing the whole body's decode and every good event in the batch (graded by the ingest
  check, which now sends an unparseable, a numeric, and a null `ts`); the per-poll event
  budget is charged inside the ingest transaction against the envelopes actually being
  accepted, so retransmitted batches, which store nothing, can no longer push a new batch
  over the line and lose its events; `event.reject` notices are capped per poll (draft 0.8
  permits the cap and requires the suppression be recorded), because a poll may carry 500
  refusable batches and each notice is a queue insert inside the store's single-connection
  transaction; a negative `EventPruneInterval` no longer panics `time.NewTicker` moments
  after a successful boot; `PruneEvents` clamps a non-positive bound rather than letting
  SQLite read it as no bound at all; query bounds finer than a millisecond round to the
  stored resolution instead of truncating, which had let an inclusive `since` admit an event
  below it; and an empty `type=` is refused rather than read as the catch-all, which had
  answered an unset filter with the whole feed.

- 2026-08-16: the action lifecycle end to end (spec section 7), the core tracer bullet.
  `POST /api/v1/servers/{serverId}/actions` validates a dispatch against the server's stored
  manifest before anything is queued (undeclared code: `unknown_action`; schema faults:
  `params_invalid` with the faults named), records the action and its `action.dispatch`
  envelope in one transaction, and honors `idempotencyKey` even when a fresh dispatch would
  be refused. The plugin's envelope ack is the delivery receipt (`queued -> delivered`);
  `action.ack` and `action.result` commit in the same transaction as the inbound ack that
  covers them, moving the action to `running` and `completed`/`failed`. A sweeper plus lazy
  expiry on read flip anything non-terminal past its TTL to `expired`, and expiry is final:
  a late ack or result is acked at the envelope level and changes nothing. Result payloads
  over the 64 KiB cap keep their outcome but drop the payload, saying so in `error`.
  `GET /api/v1/actions/{actionId}` shows the full record. Protocol draft 0.7 makes all of
  this normative (dispatch error table, envelope body shapes, forward-only transitions,
  TTL clamping); `spec/actions.schema.json` is the machine-readable companion, validated in
  CI, and `spec/openapi-admin.yaml` 0.7.0 covers both endpoints. Four new hub conformance
  checks (forty-two total) walk the full lifecycle including the failure path, idempotent
  retry, refused dispatches never reaching the queue, and TTL expiry with a late result.
  Migration `0005_actions.sql` adds the `actions` table. `scripts/demo-action.sh` walks the
  whole exchange in curl. Hardened under adversarial review before landing: `action.ack` and
  `action.result` are scoped to the session's server (an actionId is not a credential, so
  another server's plugin can no longer forge an outcome; graded by a new isolation check,
  forty-three total), the idempotency lookup runs before validation so a retry survives a
  manifest change that would refuse a fresh dispatch, dispatches commit gated on the
  manifest revision they were validated against (a racing republish refuses and revalidates
  instead of queueing unapproved params), expiry retires the dispatch envelope of
  never-delivered work so an offline server's expired actions cannot eat the queue bound,
  the wire deadline and the stored deadline are the same computed instant, `ttlSeconds`
  clamps without overflow, dispatch length limits count code points like the manifest's,
  `error` is capped at 4 KiB so it cannot launder the result cap, and a negative
  `durationMs` is dropped.

- 2026-08-16: manifest publish and validation (spec section 6), the first inbound envelope
  type the hub models. A plugin declares its actions, contexts, and events with
  `manifest.publish` at session start or at any later moment; the hub validates the body,
  including every `params` and event `payload` schema against the protocol's closed JSON
  Schema subset (`hub/internal/schema`, which rejects any keyword it would not enforce),
  stores accepted manifests gated on `manifestRevision` (higher replaces, equal or lower
  ignored), and answers an invalid manifest with a queued `manifest.reject` naming the
  faults instead of failing the poll or the session. Manifest applies and rejection notices
  commit in the same transaction as the ack that covers them (section 9.3). Operators read
  the stored manifest at `GET /api/v1/servers/{serverId}/manifest`; the raw queue endpoint
  now refuses `manifest.*` alongside `action.*` so a forged `manifest.reject` cannot be
  queued as the hub's own word. Migration `0004_manifests.sql` adds the `manifests` table.
  `spec/protocol.md` draft 0.6 adds sections 6.4 (validation and rejection) and 6.5 (the
  Admin API read), pins the equal-revision rule, and formalizes the event declaration
  shape; `spec/manifest.schema.json` is the machine-readable companion, validated in CI,
  and `spec/openapi-admin.yaml` 0.6.0 covers the new endpoint. Four new hub conformance
  checks (thirty-eight total) grade publish and Admin read, runtime republish with revision
  monotonicity, rejection without dropping the session, and the 404 before a manifest is
  accepted. `scripts/demo-manifest.sh` walks the whole exchange in curl. Hardened under
  adversarial review before landing: integer constants in schemas and `manifestRevision`
  are bounded to what survives a float64 round-trip exactly (strictly within ±2^53), length
  limits count Unicode code points rather than bytes, wrongly typed advisory fields
  (`game`, `plugin`) reject instead of storing what the companion schema calls invalid, an
  over-cap declaration list faults once instead of validating every item, and a readable
  but invalid revision (0, negative) is echoed in the rejection instead of clamped away.

- 2026-08-16: the envelope exchange over long-poll, the transport every later capability
  rides on. `POST /plugin/v1/poll` carries both directions in one request: it applies the
  plugin's ack and ingests its envelopes, then holds up to the negotiated `pollTimeout` and
  flushes the moment anything is queued. Sequence numbers are per session and per direction,
  starting at 1; acks are cumulative and monotonic; unacked envelopes are retransmitted
  unchanged; envelopes above a gap are discarded rather than reordered; unknown types are
  acked and ignored. The hub -> plugin queue is per server, so an envelope queued while the
  game server is down survives and is renumbered into its next session. A held poll is
  answered `401 session_invalid` the moment its session is revoked or superseded, rather
  than being left to expire. `POST /api/v1/servers/{serverId}/envelopes` queues one envelope
  (Admin API), refusing the types a dedicated endpoint owns so it can never route around
  their validation, and server records now report `pendingEnvelopeCount`. Migration
  `0003_envelopes.sql` adds `outbound_envelopes` and the per-session sequence columns.
- 2026-08-16: `spec/envelopes.schema.json`, the machine-readable envelope and poll exchange
  (JSON Schema draft 2020-12). CI now resolves its refs alongside the two OpenAPI documents
  and rejects any ref that leaves its document.
- 2026-08-16: the conformance suite's fake plugin (`conformance/hub/plugin.go`): a minimal
  correct plugin that enrolls, opens a session, and tracks both directions' sequence state,
  reused by every later slice rather than rewritten per check. Twelve new hub conformance
  checks (twenty-nine total) covering poll authentication, the idle hold, delivery framing,
  early flush, retransmission, cumulative and monotonic acks, out-of-range acks, contiguous
  inbound acks with gaps and duplicates, malformed envelopes, queue durability across
  sessions, revocation during a held poll, and queue-endpoint validation. The runner splits
  `-timeout` (per request) from the new `-check-timeout` (per check) and gives long-polls
  their own patient client.
- 2026-08-16: `scripts/demo-poll.sh`, a curl walkthrough of the envelope exchange: a poll
  held open, an envelope queued mid-hold and flushed at once, retransmission without an ack,
  the ack retiring it, and the sequence rules in the other direction.
- 2026-08-16: server enrollment and sessions, the first implemented protocol surface. The
  Admin API creates server records with one-time enrollment tokens
  (`POST /api/v1/servers`), lists and reads them, reissues a token
  (`POST /api/v1/servers/{serverId}/enrollment-token`), and revokes credentials
  (`DELETE /api/v1/servers/{serverId}/credentials`). The Plugin API exchanges a burned-on-use
  enrollment token for permanent credentials (`POST /plugin/v1/enroll`) and those for a
  short-lived session with a negotiated poll timeout (`POST /plugin/v1/session`,
  `GET /plugin/v1/session`). Credentials are stored as digests only and each secret is
  returned exactly once. Migration `0002_servers.sql` adds the `servers`,
  `enrollment_tokens`, and `sessions` tables. The hub takes `-admin-token`
  (env `VYSHKA_ADMIN_TOKEN`, `file:` indirection supported) and mints an ephemeral one when
  none is configured.
- 2026-08-16: `spec/openapi-admin.yaml` and `spec/openapi-plugin.yaml`, machine-readable
  companions to the protocol document covering the endpoints implemented so far. CI parses
  both and resolves every `$ref`.
- 2026-08-16: fifteen new hub conformance checks (seventeen total) covering the error
  envelope, admin authentication, server creation, enrollment (including single use, unknown
  tokens, and recovery through a reissued token), session negotiation, poll timeout clamping,
  session supersession, revocation, realm separation, and unknown-field tolerance. The runner
  now requires `-admin-token`, and refuses to start without one rather than skipping half the
  suite.
- 2026-08-16: `scripts/demo-enrollment.sh`, a curl walkthrough of the enrollment flow from
  record creation to revocation.
- 2026-08-16: the walking skeleton. Go module `github.com/That1Drifter/vyshka` with the
  repository layout the spec commits to (`hub/`, `conformance/hub/`, `conformance/plugin/`,
  `plugins/`, `panel/`, placeholder READMEs where a slice has not landed yet).
  `vyshka-hub serve` boots with an embedded cgo-free SQLite database, runs embedded
  migrations on every boot, serves `GET /healthz`, logs structured JSON, and shuts down
  gracefully. `conformance/hub` is a black-box runner that grades any hub URL and exits
  nonzero on failure; usage is documented in `conformance/README.md`. GitHub Actions runs
  gofmt, vet, tests, a build, and the conformance suite against a live hub on every push and
  pull request.
- 2026-08-15: `spikes/dayz-restapi-poll-timeout/`, the reproducible harness and measurements
  behind the poll timeout decision: a Go stub server that holds responses open in three
  shapes, an Enforce Script probe driven from a mission `init.c`, the captured script-side
  and socket-side logs, and `results/findings.md` (measured on game build 1.29.0.163709).
- 2026-08-15: repository published to GitHub
  ([That1Drifter/vyshka](https://github.com/That1Drifter/vyshka)) with a GitHub Pages site
  (just-the-docs theme): `_config.yml`, `index.md` landing page, Jekyll front matter on
  `spec/protocol.md`.
- 2026-08-15: `.gitattributes` normalizing line endings to LF in the repository.
- 2026-08-15: `spec/protocol.md` draft 0.1, the public normative protocol specification
  (RFC 2119): transport, envelope, enrollment and sessions, manifest, action lifecycle,
  telemetry and state, delivery guarantees, admin authorization and audit, webhooks, KV
  store, versioning, conformance. Three unresolved items are marked inline as open
  pre-1.0 questions (poll timeout floor, player identity platform registry, snapshot
  diffs).
- 2026-08-15: standard repository documents: README, LICENSE (Apache-2.0, per design
  decision D-001), CONTRIBUTING (including the binding clean-room provenance policy),
  SECURITY, CODE_OF_CONDUCT.
- 2026-08-15: repository initialized on branch `main`.

### Fixed

- 2026-08-31: the hub had no write timeout and no connection cap (issue #38, surfaced by
  the adversarial review of the admin body/timeout hardening but pre-existing; both were
  declared out of scope there). The read side was fully bounded and the write side not at
  all: a client that stopped reading its response held the connection and its goroutines
  indefinitely, the mirror image of the slow-loris body, and nothing capped how many
  connections one attacker could hold, which multiplied every per-connection bound by N.
  Three new `Config` knobs, all hub configuration rather than protocol. A per-response
  progress deadline (`ResponseWriteTimeout`, default 10 s, negative disables) is armed by
  the log middleware's recorder as each response's first header is written and re-armed
  per 256 KiB chunk of the body, each chunk earning an allowance for its own size at a
  50 KiB/s floor (matching what the read side's defaults grant a trickled body), through
  the same `http.ResponseController` path the admin read deadline uses. Arming at the
  response rather than at request start is what needs no sizing against the 60 s poll
  hold, since the hold ends before the response begins; the size allowance is what needs
  no sizing against the largest legal response, since a deep state history or a poll
  draining raw-queued envelopes can legally run to tens of megabytes and a flat bound
  would cut it on a slow link; the chunking is what keeps it a progress bound rather than
  a total-transfer allowance, because a handler hands the encoder one Write however large
  and the TCP stack completes one write under one deadline, so without the split a reader
  stalling at the first byte of a huge legal response would hold its connection for the
  whole allowance the size earned, and it also makes the arming arithmetic unoverflowable
  whatever a handler writes; and it cannot poison the next keep-alive request because
  net/http clears the connection's write deadline after every response it finishes. A blanket
  `WriteTimeout` (negative disables) backs that up at the `http.Server` for the writes no
  handler makes (net/http's own 400s, the 100-continue interim line) and any future path
  that skips the middleware; its default is derived from the effective configuration
  rather than fixed, `ReadTimeout` plus the 60 s hold plus 30 s of write headroom (120 s
  under the defaults, pinned to the duration ceiling rather than wrapped if `ReadTimeout`
  is raised absurdly close to it), because net/http arms it at request start and a fixed
  default would cut legal polls the moment an operator raised `ReadTimeout` for a slow
  link; a disabled `ReadTimeout` takes the default backstop off with it, since no finite
  bound armed at request start is safe against a hold that may legally begin arbitrarily
  late.
  `MaxConns` (default 256, negative disables) caps concurrent accepted connections with a
  hand-rolled semaphore listener rather than a dependency: at the cap the listener stops
  accepting and excess connections queue in the kernel backlog until a slot frees on
  connection close; closing the listener unblocks an Accept parked on a full house so
  shutdown cannot hang behind the connections it is draining; and the wrapper forwards
  `CloseWrite`, because embedding the `net.Conn` interface would otherwise strip the
  half-close off the TCP connection and silently disable the FIN-before-close that keeps
  a refused request's queued answer out of an RST's blast radius, the exact behavior the
  refusal hardening measured for. The cap is global, not per source IP: the reference
  deployment puts a reverse proxy in front of public traffic (spec section 3.3), and
  per-client fairness belongs there. Tests: a held poll survives a
  `ResponseWriteTimeout` set below the hold (arming is late, the way
  `TestHeldPollIsImmuneToReadTimeout` proves the read deadline is disarmed early); a
  deadline-recording listener sees the per-response arms and, on a 1 MiB body parsed for
  its exact length, one arm per chunk with never more than one chunk's allowance; a
  stalled reader (pinned kernel buffers, nothing read until past the bound) is actually
  cut mid-body where an unbounded server would deliver everything on the late drain; the
  derived `WriteTimeout` defaults, the follow-the-read-side disable, and the ceiling pin
  are pinned by a table test; a stale response deadline does not outlive its response
  (pinned against a malformed second request whose 400 only net/http writes);
  raw-socket cap tests prove a connection past the cap is unanswered until a slot
  frees, then served, that closing the listener unparks an Accept while the slot is
  still provably held, and that the half-close survives the wrapper on a real TCP pair.
  Two adversarial review rounds shaped the final form. The first found that a flat 10 s
  deadline would have truncated large legal responses (its "responses are small"
  premise was false against the history and raw-queue limits), that a fixed 120 s
  backstop broke under a legally raised `ReadTimeout`, that the cap wrapper silently
  dropped `CloseWrite`, and that the listener-close test released the slot before
  asserting and so proved nothing. The second found that the first round's fix, a
  size-proportional allowance armed once per Write, was a total-transfer budget rather
  than the progress bound it claimed, letting a reader stall at a huge response's first
  byte and ride out the full hour its size earned, plus the two duration overflows; the
  chunking is what closed it, and its verification added the last residue: the chunk
  loop honors io.Writer's short-write contract instead of silently skipping the
  remainder, the arming sum pins to the ceiling like the derived backstop does, and the
  stalled-reader cut is exercised for real rather than only armed. No spec change and
  no conformance change: the suite cannot grade connection-level bounds black-box in
  reasonable time.
  server carried no read timeout beyond the header one (issue #30, surfaced by the
  adversarial review of the KV slice but pre-existing since the tokens/audit slice). Any
  authenticated token, including one holding no grant on the route at all, could keep a
  connection and goroutine busy by trickling up to the 1 MiB body cap instead of being
  answered 403 at the headers, and nothing on the hub side bounded how long that trickle
  could take. The admin gate now refuses every scope check that needs no body before the
  body is read: the coarse route check everywhere, and the exact `kv:rw:{namespace}` check
  too (new `adminPathScoped` gate), because the namespace lives in the path, so a token
  granted one namespace no longer buys a 1 MiB body read on another. Answering is not
  enough by itself: net/http drains an unread body before releasing the connection, both
  ahead of the response and again after it even on a connection it is about to close, and
  raw-socket probes showed a stalling client holding the goroutine until the read timeout
  either way. Every refusal that reads nothing further, the admin gate's 403s, the admin
  realm's 401, and the plugin realm's `session_invalid` alike, now hangs up: Connection:
  close skips the pre-response drain so the answer is delivered at the headers, and a read
  deadline of `AdminBodyTimeout` bounds the post-response drain so a stalled body costs a
  refusal no more than it costs an authorized request. The drain bound is deliberately not
  an immediate expiry: a drain that fails on timeout hard-closes the socket without
  net/http's FIN-then-wait, and the RST drops the queued response out of the client's
  receive buffer (golang.org/issue/3595); an immediate-deadline draft of this was measured
  losing the 403 for ordinary clients pausing as little as 50 ms between writing and
  reading, which a protocol whose primary client is an in-game HTTP binding cannot afford.
  Refused mutations stay audited (the entry
  is created before any of this), with an empty `payloadDigest` because nothing was read
  to digest; a body-dependent refusal (a dispatch outside the granted codes) keeps its
  digest because its body had to be read for the exact check, and a path-carried value a
  refusal echoes is truncated the way scope patterns already were. Behind the ordering,
  the server gains a `ReadTimeout` (default 30 s covering headers and body under one
  deadline; negative disables) and authorized admin mutations with a body a tighter
  `AdminBodyTimeout` (default 15 s, likewise) set through `http.ResponseController`, so
  the log middleware's wrapper now unwraps; a new `IdleTimeout` config (default 2 min)
  pins keep-alive idling explicitly so it no longer silently rides whatever `ReadTimeout`
  becomes. Neither
  deadline needed sizing against the 60 s poll hold: net/http clears the connection's
  read deadline the moment a request body reaches EOF (`startBackgroundRead`), so a
  completed body disarms both and only a dribbling body or the drain of an unread one is
  ever cut, which is also why the deadline is armed only when a body exists: on a
  bodyless mutation it would bound nothing and instead cancel the request context
  mid-handler when it fired. A test holds a poll through a `ReadTimeout` set below the
  hold, and another proves both refusal properties on raw sockets: a prompt client that
  paused before reading still receives its 403, and refused or unauthenticated stalls are
  released at the drain bound, not at ReadTimeout. Three adversarial review rounds shaped
  all of this: the first found the refused-read drain, the KV path-value ordering, and the
  deadline-clearing semantics the initial sizing rationale had wrong; the second found the
  post-response drain surviving Connection: close, the untouched unauthenticated path, and
  the bodyless guillotine; the third caught the immediate-deadline RST eating the refusal
  itself. Section 10.2 gains a fourth ordering bullet (checks that need no body
  SHOULD precede the body read, the path-carried KV namespace included), 10.5's
  `payloadDigest` wording admits the unread-refusal case, and section 12.3 notes that a
  malformed namespace outside the grant may answer 403 before its grammar is ever judged;
  spec bumped to draft 0.16. No new conformance check: the ordering is a SHOULD a
  conforming hub may decline, and the existing audit check's refusal is body-dependent,
  so it keeps grading the digest.

- 2026-08-29: a snapshot replayed after history pruning could regress latest state across
  a session change (issue #34, surfaced by the adversarial review of the event-batch dedup
  fix but pre-existing in the state slice). Snapshots used their history row as their own
  cross-session dedup record, and history is bounded by depth as well as time, so a
  superseded row could be pruned (within minutes, via the depth trim) while the latest of
  its type survived every pass; the pruned envelope id was forgotten with the row, and a
  buffer replayed across a session change re-inserted the superseded snapshot with a fresh
  acceptance seq, silently regressing latest and breaking 8.3's acceptance-order rule. A
  new `state_snapshot_dedup` table (migration 0012, the `event_batches` shape) now claims
  each accepted `state.*` envelope id per server inside the ingest transaction and holds
  it for the full history window whatever happens to the row; the migration backfills
  markers from surviving rows so an upgrade does not reopen the replay window for anything
  still stored. Ids the old schema had already trimmed are unrecoverable, so a buffer
  replayed across the upgrade itself keeps the old exposure for that one deployment
  window; everything accepted after the upgrade is covered. The history table keeps its
  unique `(server_id, envelope_id)` index as the backstop for the one row that outlives
  every marker, the latest per (server, type), which survives retention indefinitely.
  Expired markers ride the snapshot retention pass; sweeping one early is safe here
  (unlike the event-batch sweep) because a still-standing row blocks the replay itself,
  and (also unlike that sweep) swept markers count toward the pass's return, because the
  depth trim deletes rows long before their markers expire and a pacing loop that ignored
  markers would let an expired-marker backlog grow without bound (found by the adversarial
  review of this fix). Section 8.3 now states the obligation the way 8.1 does: the dedup
  record MUST outlive the history row and hold at least the history window from
  acceptance, MAY be forgotten after, and a replay whose id the hub has forgotten is a new
  snapshot, latest again however stale, with `capturedAt` left to say so; spec bumped to
  draft 0.15. A new
  `state.replayAfterPrune` check (72 total) grades it by pushing one snapshot past the
  configured depth (new runner flag `-state-history-depth`, reference 500), replaying the
  trimmed victim on a fresh session, and requiring latest not to regress.

- 2026-08-27: an `event.batch` replayed across a session change was stored twice (issue
  #32). Within a session `seq` deduplicates a retransmission, but section 9.1 makes a
  plugin renumber unacked envelopes into the new session's sequence space, so a replay
  arrived with a fresh `seq`, passed classification, and inserted every event again with
  fresh ids and a fresh `received_at`, firing webhook fan-out twice for each; section 8.1
  meanwhile claimed events were "stored exactly once". The same exposure was found and
  fixed for snapshots in the state slice, and events now follow that precedent, with one
  structural difference: a batch fans out into up to 200 rows with hub-assigned ids, so
  the constraint cannot live on the events table. A new `event_batches` table records each
  accepted batch's envelope id per server (`ON CONFLICT DO NOTHING`; a claimed id skips
  the batch whole), inside the same transaction as the ack, which also suppresses the
  duplicate webhook fan-out. Dedup rows expire with the batch's longest-lived event, so
  pruning one cannot reopen the replay window while its duplicates would still be visible,
  and they ride the existing event retention pass. Section 8.1's duplicates paragraph now
  states the two-layer rule the way 8.3 does (in-session by `seq`, cross-session by
  envelope `id` per server, MUST) plus the retention floor on the dedup record; spec bumped
  to draft 0.14. `plugin.events.retransmitDedup` (71 checks total) grades the hub;
  the plugin suite's `session.renumber` stage already grades the plugin half (id preserved,
  only `seq` moves). Found by the adversarial review of the state slice. The adversarial
  review of this fix then tightened four edges. A replayed batch is now recognized before
  the per-poll budget is charged (an advisory read of the dedup table; the transactional
  claim stays the authority), because a replay that spent budget could refuse a genuinely
  fresh batch travelling with it, losing real events, or itself be answered with a notice
  claiming its events are gone while they sit in the feed. The dedup-row sweep in the
  retention pass now runs only after the expired-event backlog is drained, since sweeping a
  marker while its expired events were still query-visible would let a replay land beside
  them. Section 4 now states that envelope id uniqueness does not end with the session: a
  sender MUST NOT reuse an id for a different message on the same server, in any session,
  because cross-session dedup is obliged to treat equal ids as the same message and would
  silently drop a fresh batch under a recycled id. Section 8.1 says plainly that the dedup
  obligation is bounded by retention: once every event a batch stored has aged out, the hub
  may forget the id, and a replay after that horizon stores and fans out again. A second
  review round then caught the loose ends of the first: the retention horizon in 8.1 read
  "shortest" where the marker's lifetime is the batch's longest event retention; section
  6.4's "the receiver cannot tell a renumbered republication from a new publish" contradicted
  the new section 4 (ids survive the session), so it now says a hub need not retain manifest
  envelope ids because the revision gate already makes applies idempotent; and the plugin
  suite's mock hub now faults an id reused for a different message in a later session, which
  its per-session id map could not see. Two narrow races are accepted and documented in
  place: a retention sweep between the advisory budget read and the ingest transaction can
  let one poll overshoot the MAY-bound budget, and the drained-backlog inference in the
  prune pass leans on the single SQLite connection (the refused Postgres backend would need
  both deletes in one snapshot; noted alongside issue #20). One limitation is accepted
  rather than fixed: batches stored before this migration have no dedup record (the old
  schema kept no envelope ids), so a replay straddling the upgrade itself can still
  double-store once. The review also surfaced a pre-existing snapshot defect, filed as
  issue #34: a snapshot replayed after history pruning can regress latest state.
- 2026-08-16: an envelope whose `ts` was the wrong JSON type wedged the session. The inbound
  `ts` was decoded into a string field, so `"ts": 1755367200` failed the whole poll body at
  `encoding/json`, before any rule of the hub's own ran: `400 bad_request`, nothing in the
  poll processed, including the good envelopes travelling with it. Because a sender must
  retransmit unacked envelopes unchanged (section 9.1), the same envelope came back on every
  poll and the ack never advanced, so a plugin with a wrong clock type could not recover
  without being redeployed. The field is now raw and read tolerantly, the way the event `ts`
  already was: absent, null, and wrong-type all mean "no usable timestamp" and receipt time
  stands in. Section 4 now says plainly that a wrongly typed `ts` is unparseable for this
  purpose, and in `spec/envelopes.schema.json` the receiver-view `envelope` no longer
  constrains `ts` to a string at all, since that definition is advertised as safe to use as
  an admission filter; `hubEnvelope` and `pluginEnvelope` carry the constraint, which is
  where the sender's obligation belongs. `plugin.poll.inboundTolerance` grades it. Found by
  adversarial review of the event ingest slice; the defect predates it.
- 2026-08-16: one poll could queue an unbounded number of `manifest.reject` notices. A poll
  may legally carry 500 envelopes, so a plugin looping on a manifest it kept failing to fix
  bought up to 500 queue inserts inside the store's single transaction and filled its own
  outbound queue with complaints about its own bug, until that server could no longer be
  dispatched an action. Rejection notices now share one per-poll budget across every notice
  kind (`maxNoticesPerPoll`, 20), replacing the event-only cap; the refusals themselves are
  unaffected and the suppressed count reaches the hub's log. Section 6.4 carries the same
  permission clause section 8.1 had, including the requirement that suppression be recorded
  where an operator can see it, and both sections now state the cap as an exception to their
  "the rejection MUST NOT be silent" rule rather than leaving the two paragraphs to
  contradict each other. `plugin.manifest.rejectStorm` grades what every hub owes whether or
  not it caps: a poll made entirely of refusals succeeds, real notices come back naming
  envelopes the plugin actually sent, and no refusal is answered with more than one notice.
  Found by adversarial review of the event ingest slice; the defect predates it.
- 2026-08-16: a superseded session could take delivery of envelopes belonging to its
  successor. A poll authenticates and only then reaches the store; in that window a
  restarting game server could open a new session, and the stale poll would renumber
  envelopes into the dead session, stealing them from the live one and voiding an ack the
  live session had already sent. Every envelope operation now resolves its session through
  a liveness predicate, so a dead session is indistinguishable from no session and the poll
  gets `401 session_invalid`. Graded by a store-level test, because the window is not
  reachable from outside the HTTP layer.
- 2026-08-16: two polls in flight at once could make the hub report an inbound ack lower
  than one it had already reported, which section 9.1 forbids, and double-count the
  envelopes behind it. Each poll classified its batch against the ack it captured at
  authentication. `RecordInbound` is replaced by `AdvanceInbound`, which runs the
  classification inside the transaction against committed state and returns the ack that is
  now durable.
- 2026-08-16: an explicit `"v": 0` on an inbound envelope was accepted as though `v` had
  been omitted. Absence means the negotiated version; zero names a version no implementation
  speaks. The hub now tells them apart, and the envelope types are split by direction, since
  the hub's and the plugin's obligations for `v` and `ts` are not the same.
- 2026-08-16: one session could park unboundedly many held polls, each costing a goroutine
  and a database read per backstop tick against SQLite's single connection. Polls past a
  ceiling of four per session are answered immediately instead of held, which the protocol
  permits at any time. This is an implementation limit, not a protocol rule.

### Fixed

- 2026-08-16: request bodies must be a single JSON value on every endpoint. The decoder
  used to stop at the first value, silently swallowing anything after it, and the bytes
  behind that point never counted against the 1 MiB request cap. Trailing content is now
  `bad_request`. Found by adversarial review of the action slice; the defect predates it.
- 2026-08-16: a held poll now reports the inbound ack as committed when it answers, not the
  value it ingested before the hold. A concurrent poll on the same session could commit a
  higher ack and be answered first, leaving the held poll to later report an ack the hub had
  already exceeded, which section 9.1 forbids. `NextOutbound` returns the committed inbound
  ack alongside the batch, and the hold refreshes what it will report on every database
  read. Found by adversarial review of the manifest slice; the defect predates it.

### Changed

- 2026-08-31: `spec/protocol.md` draft 0.17 scopes the receiver's envelope-id dedup
  obligation explicitly (issue #39, surfaced by the adversarial review of the
  snapshot-replay fix; a spec-reading gap, not a code defect). Section 4's rationale said
  the surviving `id` is what the cross-session deduplication of sections 8.1 and 8.3 keys
  on, and read alone that could be taken to oblige a receiver to treat equal ids as the
  same message globally, across message families. The obligations were always narrower:
  8.1 dedups an accepted `event.batch` on its `id`, 8.3 an accepted `state.*` envelope on
  its `id`, each within its own family, and the reference hub keeps separate dedup records
  per family (`event_batches`, `state_snapshot_dedup`). Section 4 now says so: an `id`
  reused across the two rules is a sender violation that suspends no receiver obligation,
  each rule finds no match in its own record so both effects are stored and both envelopes
  acked, and global dedup is not on offer as observable behavior, derived from section 9.3
  rather than legislated fresh: a receiver that treated the second envelope as a duplicate
  would be acking an envelope whose effect it never stored, and one shared record would
  merge retention horizons that 8.1 and 8.3 set independently, silently dropping data over
  the sender's bookkeeping collision. An adversarial review round tightened the first
  draft of the paragraph: the rationale sentence about colliding id generators now
  conditions the silent drop on a live dedup record instead of overstating a global
  obligation, the paragraph names the two dedup rules concretely instead of leaning on an
  undefined "message family" term that would have collided with section 5.5's type
  families, and a "MAY store independently" that could have been read as licensing a
  global-dedup hub to suppress the second effect was replaced by the 9.3 derivation.
  Wording only; no behavior change anywhere.
- 2026-08-16: `spec/protocol.md` draft 0.5 makes the plugin conformance suite grade a forced
  session change with envelopes still unacked, and says why that case gets its own mention:
  every other retransmission rule says "resend exactly what you sent", while this one says the
  opposite about `seq` alone. A plugin that replays its ring buffer verbatim after reconnecting
  strands the whole buffer above a gap the new session can never close, and the symptom only
  appears after a game server restarts with traffic in flight. The obligation is recorded
  against the two issues that will implement and grade it (#9, #14), because the rule is
  currently graded only against a fake plugin the same commit wrote.
- 2026-08-16: the Postgres row-lock requirement is tracked as issue #20 rather than left as a
  code comment. Adding Postgres is not a driver swap: the single SQLite connection is what
  serializes the read-modify-write on per-session sequence state, so a real pool needs
  `SELECT ... FOR UPDATE` in `liveSessionSeq` or it reintroduces the concurrent-poll ack
  defects fixed above.
- 2026-08-16: `spec/protocol.md` closes a hole in the sequencing design: retransmission
  across a session change was not implementable. Section 9.1 required retransmitting
  unchanged, including `seq`, while sequence spaces restart at 1 per session, so an envelope
  left unacked when a session ended could be neither resent, nor renumbered, nor dropped.
  The unchanged rule is now scoped to within a session, and renumbering across a session
  change is normative in both directions, keeping `id`, `type`, `ts` and `body` so that
  deduplication still works. Section 9.2 extends the same rule to envelopes delivered but
  never acked, not only those queued while disconnected.
- 2026-08-16: `spec/protocol.md` resolves four ambiguities that would have let two good-faith
  implementers build incompatible hubs: envelope order is taken as received and never
  reordered; an absent `v` and a `v` of `0` are different; `durably processed` is defined,
  and explicitly does not mean an external side effect finished; and a poll rejected for a
  malformed envelope applies nothing at all, including its `ack`, which must otherwise be
  applied before the envelopes.
- 2026-08-16: envelope `id` is now opaque with ULID RECOMMENDED, rather than normatively a
  ULID. Requiring a ULID encoder inside every game engine bought nothing: deduplication only
  needs equality, and the conformance suite's own fake plugin could not satisfy the stricter
  rule it was grading. Length floors for `id` and `type` are stated so that a prose-only
  implementer knows what "too long" means.
- 2026-08-16: `spec/envelopes.schema.json` and `spec/openapi-plugin.yaml` split the envelope
  into a receiver view plus per-direction sender views. The shared definition required `ts`,
  so anyone generating a request validator from it would have rejected envelopes the spec
  says a receiver must accept. `lastSeenAt` joins `pendingEnvelopeCount` as required on the
  Admin API server record, since section 9.4 obliges a hub to expose both.
- 2026-08-16: coverage for three obligations that had none. `plugin.poll.inboundRenumber`
  grades the new cross-session renumbering rule from both sides: an envelope still carrying
  its previous session's `seq` lands above a gap and moves nothing, while the same envelope
  renumbered into the new session is accepted. That check is also what proves the rule is
  necessary rather than merely satisfiable. Section 9.2's hub-restart durability and section
  9.2's queue bound are graded by Go tests instead of conformance checks, because a
  black-box suite cannot restart the implementation it is pointed at and should not have to
  queue five thousand envelopes to observe one error code; a hub holding its queue in memory
  would otherwise have passed every check in the suite.
- 2026-08-16: the single SQLite connection is documented as load-bearing for correctness
  rather than only for locking, and `resolveDSN` carries the note that a Postgres backend
  must take row locks in `liveSessionSeq`. Raising the pool without them reintroduces the
  concurrent-poll ack defects fixed above.
- 2026-08-16: five hub conformance checks (thirty-four total) covering `lastSeenAt` on every
  poll, inbound tolerance of an omitted `v` and an unparseable `ts` against rejection of an
  explicit `v` of `0`, the 200-envelope batch floor with refusal rather than truncation above
  the cap, and a held poll answered at once when a new session supersedes it. Existing checks
  were tightened where they would have passed against a violating hub: the idle hold bound no
  longer allows nearly double the negotiated timeout, retransmission compares the body,
  `envelope_invalid` must name the offending envelope by index and apply nothing, delivery of
  already-queued work must not be held, and `pendingEnvelopeCount` is graded across the whole
  lifecycle rather than at queue time. `plugin.poll.queueOutlivesSession` was renamed: it
  grades survival across a session change, not the hub restart its old name implied, which a
  black-box suite cannot perform.
- 2026-08-16: `spec/protocol.md` draft 0.4 makes the transport normative. New section 3.1.2
  specifies the poll request and response shapes and their error codes (`envelope_invalid`,
  `ack_out_of_range`). Section 4 pins which envelope fields are required in which direction
  and what a missing `v` means. Section 5.5 defines the Admin API envelope queue, and
  section 5.1 gains `pendingEnvelopeCount` on the server record. Section 9 grows from three
  bullets into the sequencing rules both sides implement: 9.1 sequence numbers and acks, 9.2
  hub to plugin, 9.3 plugin to hub, 9.4 blocked-state honesty.
- 2026-08-16: `README.md` and the Pages landing page now describe what actually runs. The
  landing page had been left claiming draft 0.1 and no runnable code since before the
  enrollment slice landed.
- 2026-08-16: `spec/protocol.md` draft 0.3 makes enrollment and sessions normative rather
  than descriptive. New section 2.1 (HTTP conventions: JSON, opaque tokens, unknown fields
  tolerated) and 2.2 (the error envelope, with a table of general codes). Section 5 now
  specifies every request and response shape, the endpoint-specific error codes, the rule
  that a hub holds at most one live session per server, and that revocation takes effect
  immediately rather than at the next session boundary.
- 2026-08-15: adopted the platform-qualified player identity shape
  (`{ "platform": "...", "id": "..." }`) in the protocol body and examples; previously it
  was only listed as an open question.
- 2026-08-15: `spec/protocol.md` draft 0.2 replaces the section 3.1 poll timeout open
  question with normative rules (new section 3.1.1): hubs MUST honor a plugin-requested
  `pollTimeout` from 5 s to 60 s, default 25 s, and MUST respond by that deadline; plugins
  MUST set their client response timeout to `pollTimeout` + 5 s and MUST NOT rely on partial
  writes to keep a held request alive; an aborted poll is a re-poll, not a session error.
  Session requests may now carry a requested `pollTimeout` (section 5).
