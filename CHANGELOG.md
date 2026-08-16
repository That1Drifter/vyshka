# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Until the
first release, entries accumulate under **Unreleased**; dates mark when a change landed on
`main`. The protocol document, the hub, and each plugin will be versioned independently
(SemVer) once implementation starts, and this file will gain per-artifact sections at that
point if needed.

## [Unreleased]

### Added

- 2026-08-16: scoped Admin API tokens and the audit log (spec section 10). Until now a single
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
