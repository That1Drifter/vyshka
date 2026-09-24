# Conformance coverage

Which normative clause of [`spec/protocol.md`](../spec/protocol.md) each suite grades. One
row per MUST, MUST NOT, and REQUIRED in the numbered sections (section 1.1 only restates the
keywords, and the appendix is informative), quoted verbatim so the row can be found in the
spec.

The suites are the product, so a clause no check grades is a clause a third-party hub or
plugin can break while passing. This table is how those clauses get found; the ungraded
rows are the backlog for new checks, not a defect list for the reference implementations,
which grade much of it in their own tests.

`conformance/coverage/coverage_test.go` keeps it in step with the spec. It fails when a
clause in the spec has no row, when a row's excerpt no longer appears in its section, when a
cited check is no longer declared in either suite, or when the summary below disagrees with
the rows. A protocol edit that adds or removes a clause, or rewords the words a row quotes,
therefore fails CI until its row is brought along. A change to the rest of a clause's
sentence goes unnoticed, so a protocol edit rereads the rows of every clause it touches.
"Declared" means a `Check` or `Stage` literal in the suite's source; the test does not
prove the suite registers and runs it.

## Summary

| Status | Clauses |
|---|---|
| graded | 107 |
| partial | 98 |
| ungraded | 63 |
| n/a | 16 |

284 clauses in all.

## How to read a row

| Column | Meaning |
|---|---|
| § | The spec section the clause sits in |
| Clause | A verbatim excerpt holding exactly one keyword, unique within its section |
| Binds | Who the clause obliges: `hub`, `plugin`, `either` (hub and plugin alike, usually as a receiver), `sender` (whichever party emits the envelope), `admin client`, or `receiver` (a webhook receiver) |
| Graded by | The checks that fail against an implementation breaking the clause: `hub:<id>` in the hub suite, `plugin:<id>` in the plugin suite |
| Status | `graded`: a violation fails a cited check. `partial`: some violations do, and the notes say which do not. `ungraded`: none do, though a black-box check could. `n/a`: no black-box suite can observe it, and the notes say why |

A plugin suite citation names the stage that is running when the mock hub records the
violation: the mock hub checks framing and sequencing on every exchange and fails whichever
stage is current.

Rows were written by reading each cited check's code, not its section comment. A check that
exercises an area without failing on the violation is not cited.

### 1.2 Design principles

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 1.2 | "Unknown envelope `type` values MUST be acked and ignored, never treated as fatal." | either | hub:plugin.poll.inbound, plugin:compat.unknownType | graded | |

### 2 Architecture

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 2 | "A credential valid in one realm MUST NOT be accepted in the other." | hub | hub:realms.separate | partial | Session token and server secret tried on the Admin API; enrollment token never tried there; admin token tried on one Plugin API route |

### 2.1 HTTP conventions

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 2.1 | "A request carrying a body MUST use `Content-Type: application/json`" | plugin | - | ungraded | The mock hub never inspects the plugin's Content-Type |
| 2.1 | "a hub MUST reject anything else with `415`." | hub | - | ungraded | |
| 2.1 | "Receivers MUST ignore unknown fields in any request or response body." | either | hub:compat.unknownFields, hub:state.latestReplaces | partial | Hub: two request endpoints and one snapshot body. Plugin: never graded (a plugin that fails an action over an unknown params member still passes dispatch.largeParams) |
| 2.1 | "clients MUST NOT parse them or depend on their length, alphabet, or any prefix." | plugin | plugin:enroll.exchange, plugin:session.start | partial | Mock tokens carry a non-reference prefix, so prefix parsing fails; length and alphabet are never varied |

### 2.2 Error model

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 2.2 | "`message` is human-readable and MUST NOT be parsed" | plugin | - | ungraded | |
| 2.2 | "Clients MUST branch on `code`" | plugin | - | ungraded | No two refusals with the same endpoint and status but different recovery are contrasted, so a client that never reads code passes |
| 2.2 | "and MUST tolerate codes they do not recognize, falling back on the HTTP status." | plugin | - | ungraded | The mock hub never sends an unknown code |

### 2.3 Inline errors (Plugin API)

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 2.3 | "every response the hub would otherwise have sent with a 4xx or 5xx status it MUST instead send with status `200`, the `application/json` content type" | hub | hub:plugin.errors.inlineRefusals, hub:plugin.errors.inlineSupersededHold, hub:plugin.bans.pull | partial | Enroll, session, poll, one KV refusal, and a bad cursor on the bans POST route; not payload_too_large, method_not_allowed, or a revoked held poll |
| 2.3 | "`error.status` is REQUIRED in an inline error and carries the HTTP status the hub would have used." | hub | hub:plugin.errors.inlineRefusals, hub:plugin.errors.inlineSupersededHold | graded | |
| 2.3 | "A value of `errors` other than `inline`, more than one occurrence of the parameter, or a query string the hub cannot parse MUST be refused with `400 bad_request` in ordinary form" | hub | hub:plugin.errors.unknownMode | graded | |
| 2.3 | "A hub that implements inline errors MUST report `"inlineErrors": true` in the `features` object of the session response (section 5.3)." | hub | hub:plugin.errors.inlineSuccess | graded | |
| 2.3 | "A success body from any Plugin API endpoint never carries a top-level `error` member; hubs MUST NOT add one." | hub | hub:plugin.errors.inlineSuccess | partial | Checked on enroll, session, and an idle poll only |
| 2.3 | "A plugin that opted in MUST therefore treat a `200` whose body is a JSON object with an `error` member as the failure it describes" | plugin | plugin:errors.batchRefused, plugin:errors.credentialsRefused, plugin:session.renumber | graded | |
| 2.3 | "and MUST NOT treat it as success." | plugin | plugin:errors.batchRefused, plugin:errors.credentialsRefused, plugin:session.renumber | graded | |
| 2.3 | "In particular it MUST NOT apply an `ack` from such a body." | plugin | - | ungraded | errors.garbledSuccess serves a non-JSON body that carries no ack, so applying an ack from an error body is never provoked |
| 2.3 | "a plugin MUST keep whatever handling it has for an opaque error alongside its handling of inline ones." | plugin | plugin:errors.batchRefused | graded | Graded in the -legacy-errors run, where the mock ignores the opt-in |
| 2.3 | "The plugin MUST take the envelope at `details.index` out of its outbox before retrying" | plugin | plugin:errors.batchRefused | partial | Enforced only when the refusal travelled inline and polls are sequential; a plugin reading an ordinary 400 body, or overlapping polls, may resend and pass |
| 2.3 | "MUST NOT count it as delivered" | plugin | - | n/a | Internal outbox bookkeeping, not visible on the wire |
| 2.3 | "and MUST surface it (log it, set it aside on disk) rather than discard it silently." | plugin | - | n/a | Logs and disk state are outside what a wire-level suite sees |
| 2.3 | "(one that sends batches which are not such a run) MUST start a new session instead" | plugin | - | ungraded | The mock faults non-contiguous batches themselves, not the recovery from a refusal of one |
| 2.3 | "log, back off, and retry; MUST NOT start a new session over it." | plugin | - | ungraded | The mock hub never sends an unrecognized 4xx code |
| 2.3 | "Where the table says back off, the plugin MUST wait at least 1 s before its next attempt" | plugin | plugin:errors.batchRefused, plugin:errors.garbledSuccess, plugin:errors.credentialsRefused | partial | The enforced gap is 950 ms, not 1 s; 5xx and unknown-code retries never provoked; credentials path counts attempts, not gaps; stands down when polls overlap |

### 3.1 Baseline: HTTP long-poll (mandatory)

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 3.1 | "Every plugin and every hub MUST support long-poll." | either | hub:plugin.poll.idleHold, plugin:poll.repolls | graded | |
| 3.1 | "The hub MUST hold the request open up to the negotiated `pollTimeout` (default 25 s)" | hub | hub:plugin.poll.idleHold | graded | |

### 3.1.1 Negotiating `pollTimeout`

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 3.1.1 | "A hub MUST honor any requested value in the range **5 s to 60 s** inclusive" | hub | hub:plugin.session.pollTimeout | graded | |
| 3.1.1 | "and MUST return the effective value in the session response." | hub | hub:plugin.session.pollTimeout, hub:plugin.session.exchange | graded | |
| 3.1.1 | "it MUST NOT silently apply a value the plugin did not request from inside the range." | hub | hub:plugin.session.pollTimeout | partial | In-range requests must come back exact, but out-of-range requests accept any in-range value, not only the nearest end |
| 3.1.1 | "The hub MUST send a response no later than the effective `pollTimeout`." | hub | hub:plugin.poll.idleHold | partial | idleHold allows about 2 s of slack past a 5 s pollTimeout, so a hub answering slightly late passes |
| 3.1.1 | "A plugin MUST configure its HTTP client's response timeout to at least the effective `pollTimeout` plus 5 s" | plugin | - | ungraded | The mock hub holds a poll for 1 s, never the negotiated pollTimeout |
| 3.1.1 | "A plugin that cannot configure a timeout that high MUST request a correspondingly lower `pollTimeout`." | plugin | - | ungraded | |
| 3.1.1 | "Hubs and plugins MUST NOT rely on partial writes (chunked keepalives, early headers, whitespace padding) to keep a held request alive." | hub | - | ungraded | |
| 3.1.1 | "so a plugin MUST simply re-poll rather than treat a timeout as a session error." | plugin | - | ungraded | No client timeout is ever induced: aborts are connection resets, and a new session after one is accepted |

### 3.1.2 The poll exchange

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 3.1.2 | "A hub MUST answer with `200` and an empty `envelopes` array when the hold expires with nothing queued." | hub | hub:plugin.poll.idleHold | graded | |
| 3.1.2 | "A hub MUST answer immediately, without holding, whenever any unacked envelope is already queued for the session." | hub | hub:plugin.poll.deliver | partial | Immediately is approximated by 2 s; timed only for a never-delivered envelope, a delivered but unacked one is not timed |
| 3.1.2 | "A hub MUST apply the request's `ack` and ingest the request's `envelopes` before it begins to hold" | hub | hub:plugin.poll.retransmit, hub:plugin.poll.ackContiguous, hub:plugin.poll.more | partial | Request-ack effects are observed through what the answer carries, and inbound progress on a more: true poll; applying the ack or ingesting before an actual hold is not observed |
| 3.1.2 | "It MUST apply the `ack` first: the ack frees queued work" | hub | - | ungraded | The ack-bearing polls in retransmit and ackContiguous carry no envelopes, so the order of ack and ingest is never observable |
| 3.1.2 | "A hub MUST validate the whole inbound batch before applying any of it." | hub | - | ungraded | envelopeInvalid's good envelope sits above the gap its malformed ones leave, so an incremental hub also answers ack 0 |
| 3.1.2 | "A hub MUST answer a held poll with `401 session_invalid` as soon as its session stops being live (superseded, revoked, or expired)" | hub | hub:plugin.poll.supersededDuringHold, hub:plugin.poll.revokedDuringHold, hub:plugin.errors.inlineSupersededHold | partial | As soon as is approximated by about 5 s from the poll's start; session expiry during a hold is not provoked |
| 3.1.2 | "A hub MUST accept at least 200 envelopes in one poll request." | hub | hub:plugin.poll.batchLimit | graded | |
| 3.1.2 | "MUST reject anything over its cap with `bad_request` rather than truncate it silently" | hub | hub:plugin.poll.batchLimit | partial | Truncation is caught, but 413 or any code passes where the clause names bad_request |
| 3.1.2 | "A hub MUST update the server record's `lastSeenAt` on every poll." | hub | hub:plugin.poll.lastSeen | graded | |
| 3.1.2 | "It MUST NOT set it on a request that carries no envelopes" | plugin | plugin:poll.more | graded | The mock faults it on arrival, in whichever stage is running |
| 3.1.2 | "and MUST NOT set it when the batch carries everything it holds." | plugin | plugin:poll.more | partial | Caught only when the follow-up poll then carries nothing; stands down when polls overlap |
| 3.1.2 | "the next poll the plugin sends in that session MUST carry envelopes if any it left behind that batch are still unacked" | plugin | plugin:poll.more | partial | Stands down (ungraded) for a candidate with overlapping polls |
| 3.1.2 | "`details.index` is REQUIRED and names the envelope's position in the batch" | hub | hub:plugin.poll.envelopeInvalid, hub:plugin.errors.inlineRefusals | graded | |

### 3.2 Upgrade: WebSocket (optional)

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 3.2 | "A hub MUST NOT require WebSocket support." | hub | hub:plugin.session.exchange, hub:plugin.poll.idleHold | graded | |

### 3.3 Security

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 3.3 | "All transport MUST be TLS in production deployments." | hub | - | n/a | Deployment requirement; the suites run against plain HTTP on loopback |

### 4 Message envelope

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 4 | "`id`, `type`, `seq` and `ts` are REQUIRED on every envelope a sender emits." | sender | hub:plugin.poll.deliver, plugin:manifest.publish | graded | The hub suite's poll helper and the mock hub's ingest check every envelope |
| 4 | "`v` is REQUIRED from the hub and OPTIONAL from a plugin" | hub | hub:plugin.poll.deliver | graded | |
| 4 | "`id` is opaque to the receiver: it MUST be unique per message and identical on every retransmission of that message" | sender | hub:plugin.poll.retransmit, plugin:outage.bufferAndFlush, plugin:session.renumber | partial | Plugin ids are checked for reuse; hub ids only for identity across a retransmission, not uniqueness |
| 4 | "a receiver MUST NOT parse it or require any particular format." | either | hub:plugin.poll.inbound, plugin:action.roundTrip | partial | Both suites send non-ULID ids; parsing that tolerates failure is invisible |
| 4 | "so a sender MUST NOT reuse an `id` for a different message on the same server, in any session." | sender | plugin:session.renumber | partial | The mock faults plugin id reuse in and across sessions; hub-minted ids are never compared |
| 4 | "violates the sender's MUST NOT above but suspends no receiver obligation" | hub | - | ungraded | No check sends an event.batch and a state.* envelope under one id |
| 4 | "A receiver MUST reject an envelope missing `id`, `type` or `seq`, declaring a version it does not speak, or exceeding a documented length limit on `id` or `type`." | either | hub:plugin.poll.envelopeInvalid | partial | Hub side only, and a missing seq is never sent (the fixture sends an explicit 0); length-limit refusal not probed; the plugin as receiver is never sent a malformed envelope |
| 4 | "A receiver MUST accept an `id` and a `type` of at least 128 characters." | hub | - | ungraded | |
| 4 | "`0` names a version no implementation speaks and MUST be rejected like any other unknown version." | either | hub:plugin.poll.inboundTolerance | partial | The plugin as receiver is never sent a v of 0 |
| 4 | "Implementations MUST NOT collapse the two." | either | hub:plugin.poll.inboundTolerance | partial | The plugin as receiver is never sent an envelope without v |
| 4 | "A receiver MUST NOT reject an envelope over `ts` alone." | either | hub:plugin.poll.inboundTolerance | partial | The plugin as receiver is never sent a bad ts |
| 4 | "When `ts` is missing or unparseable it MUST substitute its own receipt time wherever it records one." | hub | - | ungraded | No check reads back where the hub records an envelope ts |
| 4 | "an implementation that decodes the field into a string-typed variable MUST still accept those" | hub | hub:plugin.poll.inboundTolerance | graded | |
| 4 | "since a sender MUST retransmit unacked envelopes unchanged (section 9.1)" | sender | hub:plugin.poll.retransmit, plugin:outage.bufferAndFlush | graded | |
| 4 | "Receivers MUST ack the highest contiguous `seq` they have durably processed" | hub | hub:plugin.poll.inbound, plugin:action.roundTrip | partial | Durability across a restart is not observable to a black box |
| 4 | "senders MUST retransmit anything above the ack." | sender | hub:plugin.poll.retransmit, plugin:outage.bufferAndFlush | graded | |
| 4 | "Unknown `type` values MUST be acked and ignored." | hub | hub:plugin.poll.inbound, plugin:compat.unknownType | graded | |

### 5 Enrollment and sessions

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 5 | "The enrollment token is burned: a second enroll attempt with it MUST fail." | hub | hub:plugin.enroll.singleUse | graded | |
| 5 | "A hub MUST store only an irreversible digest of every credential it issues." | hub | - | n/a | Storage layout is not observable over the wire |
| 5 | "Each secret is returned exactly once, in the response that mints it, and MUST NOT be retrievable afterwards through any API." | hub | hub:admin.servers.create, hub:admin.tokens.lifecycle | partial | Enrollment token and admin token lists searched, but the token list is decoded into modeled fields first, so an extra secret member is never seen; server secret and session token never searched |

### 5.1 Server records (Admin API)

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 5.1 | "`name` is REQUIRED" | hub | hub:admin.servers.validate | graded | |
| 5.1 | "MUST be non-empty. `game` is OPTIONAL: it declares what the operator expects to enroll here" | hub | - | ungraded | Only a missing name is sent, never an empty one |
| 5.1 | "the hub MUST reject a mismatching enrollment (`game_mismatch`)." | hub | - | ungraded | |
| 5.1 | "A hub MAY clamp it and MUST report the effective expiry in `expiresAt`." | hub | hub:admin.servers.create | partial | Only checks expiresAt parses and lies ahead; no requested TTL is compared |
| 5.1 | "any unused earlier token for that server MUST be invalidated" | hub | - | ungraded | Reissue is only tested after the first token was burned |

### 5.2 Enrollment (Plugin API)

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 5.2 | "`enrollmentToken` and `game` are REQUIRED; `plugin` and `transports` are OPTIONAL and recorded for display." | plugin | plugin:enroll.exchange | graded | |
| 5.2 | "Burning the token and issuing the secret MUST be atomic" | hub | - | ungraded | |
| 5.2 | "two concurrent enrollments with the same token MUST result in exactly one success." | hub | - | ungraded | |
| 5.2 | "Enrolling a server that already has credentials (with a freshly issued token) MUST replace the secret, invalidating the previous secret and every session derived from it." | hub | hub:plugin.enroll.recovery | partial | Only after revocation; re-enrolling a live server and killing its session is not tested |
| 5.2 | "The plugin MUST persist `serverId` and `serverSecret`" | plugin | - | ungraded | The suite never restarts the candidate |
| 5.2 | "and MUST NOT need the enrollment token again." | plugin | plugin:session.renumber, plugin:errors.credentialsRefused | partial | Graded within one process lifetime; a restart is not exercised |

### 5.3 Sessions (Plugin API)

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 5.3 | "`serverId` and `serverSecret` are REQUIRED." | plugin | plugin:session.start | graded | |
| 5.3 | "A hub MUST report it when it holds one" | hub | hub:plugin.session.manifestRevision | graded | |
| 5.3 | "and MUST omit it when it holds none" | hub | hub:plugin.session.manifestRevision | graded | |
| 5.3 | "a plugin MUST tolerate the field's absence, since a hub predating this draft never sends it." | plugin | plugin:poll.repolls | graded | The mock hub never sends manifestRevision; a plugin that rejects the session response stops polling |
| 5.3 | "A hub implementing section 13 MUST report it on every session response" | hub | hub:plugin.bans.pull | partial | One session response is checked |
| 5.3 | "A plugin MUST tolerate its absence, which means the hub serves no list" | plugin | - | ungraded | The mock hub always sends bansRevision |
| 5.3 | "A hub MUST support the current and previous major version (section 14)" | hub | - | ungraded | No check sends an explicit protocolVersion |
| 5.3 | "and MUST reject anything else with `protocol_version_unsupported`." | hub | - | ungraded | |
| 5.3 | "`features` is an object of hub-declared flags, which plugins MUST tolerate not recognizing." | plugin | - | ungraded | The mock hub sends only inlineErrors |
| 5.3 | "The one flag this document defines is `inlineErrors: true`, which a hub implementing section 2.3 MUST report." | hub | hub:plugin.errors.inlineSuccess | graded | |
| 5.3 | "A hub MUST hold **at most one live session per server**." | hub | hub:plugin.session.single | graded | |
| 5.3 | "Issuing a session MUST invalidate any earlier session for the same server" | hub | hub:plugin.session.single, hub:plugin.poll.supersededDuringHold | graded | |
| 5.3 | "A hub MUST reject an expired, unknown, or superseded session token with `401` and code `session_invalid`" | hub | hub:plugin.poll.auth, hub:plugin.session.single | partial | Unknown and superseded tokens graded; an expired token is not |
| 5.3 | "the plugin MUST respond by requesting a new session rather than by re-enrolling." | plugin | plugin:session.renumber | graded | |
| 5.3 | "The hub MUST update the server record's `lastSeenAt` on session creation." | hub | hub:plugin.session.exchange | partial | Checks lastSeenAt is set after a session, not that session creation moved it |

### 5.4 Revocation

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 5.4 | "Revoking a server's credentials MUST take effect immediately, not at the next session boundary" | hub | hub:plugin.session.revoked, hub:plugin.poll.revokedDuringHold | graded | |
| 5.4 | "a long-poll already held open MUST be answered with `401 session_invalid` rather than being left to expire" | hub | hub:plugin.poll.revokedDuringHold | graded | |

### 5.5 Queueing an envelope (Admin API)

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 5.5 | "`type` is REQUIRED" | hub | hub:admin.envelopes.queue | graded | |
| 5.5 | "MUST be non-empty after trimming surrounding whitespace." | hub | - | ungraded | Only a missing type is sent |
| 5.5 | "A hub MUST accept a `type` of at least 128 characters and MAY reject a longer one with `bad_request`." | hub | - | ungraded | |
| 5.5 | "`body` is OPTIONAL, MUST be a JSON object when present, and defaults to `{}`." | hub | - | ungraded | |
| 5.5 | "A hub MUST reject a `type` family the hub itself models (`action.*` via the dispatch endpoint of section 7" | hub | hub:admin.envelopes.queue, hub:state.guards, hub:plugin.bans.changed | partial | action, manifest, state, and bans families probed; context.* and event.* are not |
| 5.5 | "Queueing does not require a live session, and MUST NOT fail because the server has none." | hub | hub:plugin.poll.queueOutlivesSession | graded | |

### 6.1 Actions

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 6.1 | "Integer constants in a schema (bounds and `enum` members) MUST lie strictly within ±2^53" | plugin | plugin:manifest.publish | graded | Hub-side refusal of such a constant is not graded by the hub suite |
| 6.1 | "It is an annotation, not a constraint: a hub MUST NOT validate a dispatched value against an enumeration" | hub | hub:plugin.manifest.contextAnnotation | graded | |
| 6.1 | "A hub MUST reject a manifest whose `context` annotation names a context the manifest does not declare, or sits on a schema whose `type` is not `string`" | hub | hub:plugin.manifest.contextAnnotation | graded | |
| 6.1 | "a hub MUST NOT validate a dispatched value against the store" | hub | hub:plugin.manifest.kvNamespaceAnnotation | graded | |
| 6.1 | "A hub MUST reject a manifest whose `kvNamespace` names a namespace its `kvNamespaces` does not declare, is not a string, or sits on a schema whose `type` is not `string`" | hub | hub:plugin.manifest.kvNamespaceAnnotation | partial | Undeclared, absent list, and non-string schema tested; an annotation value that is not a string is not |
| 6.1 | "The hub MUST validate dispatch payloads against the schema **before** queueing, so schema-invalid input never reaches the game server." | hub | hub:action.dispatch.invalid, hub:action.dispatch.excluded | graded | |
| 6.1 | "The hub replaces its stored manifest when it receives a higher revision and MUST ignore an equal or lower one, whatever its content" | hub | hub:plugin.manifest.republish | graded | |

### 6.2 Contexts

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 6.2 | "For each custom context it declares, a plugin MUST answer a `context.enumerate` request (hub -> plugin) with a `context.entries` reply (plugin -> hub)" | plugin | plugin:context.enumerate | partial | At most five declared contexts are asked about; a sixth goes unasked |
| 6.2 | "`entries` is REQUIRED in the reply." | plugin | plugin:context.enumerate | graded | |
| 6.2 | "A plugin MUST NOT treat such a request as a fault of the link." | plugin | plugin:context.enumerate | partial | Only the reply to an undeclared context is required; a plugin that answers and then drops its session passes |
| 6.2 | "a hub that never sends `context.enumerate` is conformant, so a plugin MUST NOT wait for one" | plugin | - | ungraded | The harness always enumerates before dispatching, so a plugin waiting on it is never exposed |
| 6.2 | "`contextId` MUST name a context the server's stored manifest declares; anything else, a server the hub does not know, and a server with no manifest are `not_found` alike." | hub | hub:admin.contexts.enumerate | partial | Undeclared, built-in, and unknown-server reads tested; a server with no manifest is not |

### 6.3 Declared custom events

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 6.3 | "Hubs MUST accept undeclared custom events (storing them with a generic label)" | hub | hub:plugin.events.ingest | partial | Acceptance and storage graded; the generic label is not checked |

### 6.4 Validation and rejection

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 6.4 | "A hub MUST validate a `manifest.publish` body before storing it," | hub | hub:plugin.manifest.invalid, hub:plugin.manifest.contextAnnotation, hub:plugin.manifest.kvNamespaceAnnotation, hub:plugin.manifest.capabilities, hub:action.dispatch.excluded | partial | Missing or duplicated action codes and the length limits are never sent |
| 6.4 | "and MUST reject the whole manifest when any of its `params` or event `payload` schemas uses a keyword outside the section 6.1 subset" | hub | hub:plugin.manifest.invalid | partial | Only a `params` schema is tested, never an event `payload` schema |
| 6.4 | "A hub MUST NOT fail the poll or end the session over a manifest it rejected" | hub | hub:plugin.manifest.invalid, hub:plugin.manifest.rejectStorm | graded | |
| 6.4 | "a rejected manifest MUST NOT touch the stored one" | hub | hub:plugin.manifest.invalid, hub:plugin.manifest.contextAnnotation | partial | Only the stored revision is compared after a rejection, not the stored manifest body |
| 6.4 | "The rejection MUST NOT be silent:" | hub | hub:plugin.manifest.invalid, hub:plugin.manifest.rejectStorm | graded | |
| 6.4 | "the hub MUST queue a `manifest.reject` envelope (hub -> plugin) unless the server's outbound queue is at its bound (section 9.2)" | hub | hub:plugin.manifest.invalid | graded | |
| 6.4 | "A hub that suppresses a notice under this cap MUST record the suppression where the operator can see it." | hub | - | n/a | Where it is recorded (a log) is unspecified and not on the wire |
| 6.4 | "A plugin SHOULD surface a `manifest.reject` where the server operator will see it (a log line at least) and MUST NOT treat one as a transport error." | plugin | - | ungraded | The mock hub never sends the plugin a `manifest.reject` |

### 6.7 Capabilities

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 6.7 | "A hub MUST ignore an entry it does not know." | hub | hub:plugin.bans.changed | graded | |

### 7 Action lifecycle

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 7 | "The hub MUST validate a dispatch against the server's stored manifest before anything is queued" | hub | hub:action.dispatch.invalid | graded | |
| 7 | "Validation and queueing MUST be atomic with respect to manifest replacement" | hub | - | ungraded | No check races a dispatch against a republish |
| 7 | "a retry with the same key MUST return the original `actionId` instead of double-queueing" | hub | hub:action.dispatch.idempotent | graded | |
| 7 | "a retry of an action that was already accepted MUST answer with the original even when a fresh dispatch would be refused" | hub | - | ungraded | No check retries after the queue fills or the code is withdrawn |
| 7 | "so clients MUST use a fresh key per logical action" | admin client | - | n/a | Binds admin clients, which no suite plays |
| 7 | "including the deadline it MUST discard the action after" | plugin | - | ungraded | The harness never sends a dispatch whose `expiresAt` has passed |
| 7 | "The plugin MUST send `action.ack` on receipt (state becomes `running`) and `action.result` when done" | plugin | plugin:action.roundTrip | graded | |
| 7 | "`ok` is REQUIRED and decides the terminal state: `completed` or `failed`." | plugin | plugin:action.roundTrip | graded | |
| 7 | "An `actionId` is not a credential and MUST NOT act like one" | hub | hub:action.isolation | graded | |

### 8.1 Events

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 8.1 | "`events` \| array \| REQUIRED. MAY be empty, which stores nothing and is not an error." | plugin | plugin:telemetry.wellFormed | graded | |
| 8.1 | "`t` \| string \| REQUIRED. The event type (below)." | plugin | plugin:telemetry.wellFormed | graded | |
| 8.1 | "A hub MUST refuse an event whose `t` is outside that grammar." | hub | hub:plugin.events.oversizedBatch | partial | Only a type with no namespace is sent; length, characters, and empty segments are not |
| 8.1 | "a hub MUST refuse an event whose `t` begins with `action.`, `server.`, or `audit.`" | hub | hub:plugin.events.reservedNamespaces | graded | |
| 8.1 | "hubs MUST NOT privilege core events over custom events in routing or retention capability" | hub | hub:plugin.events.ingest, hub:webhooks.signedDelivery | partial | Storage, query, and webhook fan-out of custom events graded; retention capability is not observable |
| 8.1 | "A custom event MUST be accepted whether or not the manifest declared it (section 6.3)." | hub | hub:plugin.events.ingest | graded | |
| 8.1 | "A hub MUST NOT refuse an event over `ts` alone;" | hub | hub:plugin.events.ingest | graded | |
| 8.1 | "when `ts` is absent or unparseable it MUST substitute its own receipt time" | hub | hub:plugin.events.ingest | partial | Checks only that `occurredAt` parses, not that it is the receipt time |
| 8.1 | "A hub MUST record the substituted value where it records an event's time" | hub | hub:plugin.events.ingest | partial | Checks only that `occurredAt` parses, not that it holds the substituted value |
| 8.1 | "A hub MUST refuse a batch that carries more than 200 events, or any event whose `t` is outside the grammar, whose `data` is not an object" | hub | hub:plugin.events.oversizedBatch | partial | Over 200 events and a bad `t` tested; non-object and oversized `data` are not |
| 8.1 | "Refusal is whole-batch: a refused `event.batch` MUST store none of its events." | hub | hub:plugin.events.oversizedBatch | graded | |
| 8.1 | "and a hub MUST NOT fail the poll over it." | hub | hub:plugin.events.oversizedBatch, hub:plugin.events.reservedNamespaces | graded | |
| 8.1 | "The rejection MUST NOT be silent either" | hub | hub:plugin.events.oversizedBatch | graded | |
| 8.1 | "the hub MUST queue an `event.reject` envelope carrying the `id` of the refused envelope" | hub | hub:plugin.events.oversizedBatch | graded | |
| 8.1 | "A plugin SHOULD surface an `event.reject` where the operator will see it and MUST NOT treat one as a transport error." | plugin | - | ungraded | The mock hub never sends the plugin an `event.reject` |
| 8.1 | "A hub that suppresses a notice under this cap MUST record the suppression where the operator can see it." | hub | - | n/a | Where it is recorded (a log) is unspecified and not on the wire |
| 8.1 | "a hub MUST deduplicate an accepted `event.batch` on its `id` (per server)" | hub | hub:plugin.events.retransmitDedup, hub:admin.events.isolation | graded | Isolation catches dedup across servers, since fake plugins reuse envelope ids |
| 8.1 | "The dedup record MUST be kept at least as long as any event the batch stored" | hub | hub:plugin.events.retransmitDedup | partial | The replay comes seconds after acceptance; a record pruned early but not at once passes |

### 8.2 Player identity

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 8.2 | "A hub MUST accept any `platform` string within the" | hub | hub:state.latestReplaces | partial | One unregistered platform, in a snapshot only; events and profile reads use `steam` |

### 8.3 State snapshots

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 8.3 | "Player entries MUST carry `player`, the platform-qualified identity of section 8.2" | plugin | plugin:telemetry.wellFormed | graded | Hub enforcement of a missing `player` is graded by hub:state.rejectWhole |
| 8.3 | "Vehicle and entity entries MUST carry `id`, a non-empty string stable for the lifetime of the thing it names" | plugin | plugin:telemetry.wellFormed | partial | Shape and non-emptiness checked; stability over time is not |
| 8.3 | "Its `world` field MUST be a JSON object, whole in the same sense as a list" | plugin | plugin:telemetry.wellFormed | graded | Hub refusal of a bad world is graded by hub:state.world |
| 8.3 | "a hub MUST NOT reorder them by `capturedAt`, whose clock it does not own" | hub | hub:state.latestReplaces | graded | |
| 8.3 | "the latest snapshot per type MUST survive every retention pass" | hub | hub:state.replayAfterPrune | partial | Depth trim attempted only at the runner's configured depth, and the check never confirms a retention pass ran; the time window is not probed |
| 8.3 | "a hub MUST deduplicate an accepted `state.*` envelope on its `id` (per server)" | hub | hub:state.retransmitDedup | partial | Only state.players on one server; per-server isolation and the other state families are not probed |
| 8.3 | "The dedup record MUST outlive the snapshot's history row" | hub | hub:state.replayAfterPrune | partial | Needs -state-history-depth to match the hub's depth, and never confirms the history row was actually pruned before the replay |
| 8.3 | "a hub MUST remember an accepted `state.*` `id` for at least that window counted from acceptance" | hub | hub:state.retransmitDedup, hub:state.replayAfterPrune | partial | Replays come seconds after acceptance; a memory shorter than the window but not instant passes |

### 8.5 Reading events (Admin API)

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 8.5 | "It is opaque per section 2.1: clients MUST NOT parse it or derive one." | admin client | - | n/a | Binds admin clients, which no suite plays |
| 8.5 | "A hub MUST NOT return the same event on two pages of one walk, nor skip one that was present when the walk began" | hub | hub:admin.events.queryAndPaginate, hub:admin.players.events | partial | Fixtures carry distinct `occurredAt`, so ties are never paged; no event arrives mid-walk |
| 8.5 | "several events can share a millisecond, so the order MUST be total." | hub | - | ungraded | Every paginated fixture has distinct `occurredAt`, so a timestamp-only order passes |
| 8.5 | "A cursor whose event has since been pruned MUST still resume correctly" | hub | - | n/a | A black-box suite has no way to prune an event |
| 8.5 | "a hub MUST NOT silently ignore a filter it could not parse" | hub | hub:admin.events.queryAndPaginate | partial | Bad `type`, `since`, `limit`, and `cursor` tested; an unparseable `until` is not |

### 8.6 Player profiles (Admin API)

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 8.6 | "with the dots percent-encoded (`%2e`), which a hub MUST then read as the member it encodes" | hub | - | ungraded | No check sends a dot-segment identity |
| 8.6 | "`text` is REQUIRED: a string of at most 4000 code points that is not empty or whitespace alone and contains no U+0000." | hub | hub:admin.players.notes | partial | Missing, blank, and over-length text refused; U+0000 is not tested |

### 9.1 Sequence numbers and acks

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 9.1 | "A sender MUST send envelopes in ascending `seq` order." | sender | hub:plugin.poll.ackContiguous, plugin:poll.more | graded | |
| 9.1 | "A receiver MUST take them in the order they arrived" | either | - | ungraded | No check sends either party a descending batch |
| 9.1 | "and MUST NOT reorder them: a receiver that sorted first would ack a batch its sender never sent in that order" | either | - | ungraded | No check sends either party a descending batch |
| 9.1 | "A receiver MUST NOT advance its ack past a gap." | either | hub:plugin.poll.inbound, hub:plugin.poll.inboundRenumber, hub:plugin.poll.more | partial | The mock hub never sends a plugin a gap |
| 9.1 | "A receiver MUST treat an envelope at or below its ack as a duplicate" | either | hub:plugin.poll.inbound, hub:plugin.events.duplicate, plugin:action.redeliveryDedup | graded | |
| 9.1 | "A receiver MUST NOT lower an ack it has already reported" | either | hub:plugin.poll.inbound | partial | Hub ack held across a gap only; a plugin lowering its ack is ignored, not faulted |
| 9.1 | "a sender MUST ignore an ack below the one it has already recorded" | sender | hub:plugin.poll.ackContiguous | partial | Hub side only; the mock hub never reports a lower ack to a plugin |
| 9.1 | "A receiver that answers several requests concurrently MUST derive each reported ack from committed state" | hub | - | ungraded | No check sends concurrent polls |
| 9.1 | "**Within a session**, a sender MUST retransmit every envelope above the receiver's ack unchanged" | sender | hub:plugin.poll.retransmit, plugin:outage.bufferAndFlush | graded | |
| 9.1 | "**Across a session change**, `seq` is the one field that MUST change." | sender | hub:plugin.poll.queueOutlivesSession, plugin:session.renumber | partial | Plugin side graded by session.renumber; the hub check compares id and seq only, with an envelope whose seq is 1 on both sessions |
| 9.1 | "an envelope still unacked when a session ends MUST be renumbered into the new session's space, keeping its `id`, `type`, `ts` and `body`" | sender | hub:plugin.poll.queueOutlivesSession, plugin:session.renumber | partial | Plugin side compares every field; hub side compares only `id` and `seq` |

### 9.2 Hub -> plugin

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 9.2 | "an envelope that is unacked when a session ends MUST be delivered on the next session" | hub | hub:plugin.poll.queueOutlivesSession | graded | |
| 9.2 | "the plugin MUST keep a small LRU of executed action ids" | plugin | plugin:action.idDedup | partial | Graded through action.result messages: a plugin that executes twice but suppresses the second result passes; the LRU itself is internal |
| 9.2 | "and MUST NOT execute the same `actionId` twice." | plugin | plugin:action.idDedup, plugin:action.redeliveryDedup | partial | Both stages count action.result messages; a second execution with its result suppressed passes |
| 9.2 | "A hub MUST bound its per-server queue (reference default 5 000 envelopes)" | hub | - | ungraded | No check fills a queue to its bound |
| 9.2 | "and MUST refuse new work with `outbound_queue_full` when the bound is reached" | hub | - | ungraded | No check fills a queue to its bound |

### 9.3 Plugin -> hub

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 9.3 | "The plugin MUST persist an outbound ring buffer (file-backed where the engine allows, memory otherwise" | plugin | plugin:outage.bufferAndFlush | partial | Buffering through an outage graded; persistence across a game-server restart is not |
| 9.3 | "and MUST NOT drop an envelope until the hub has acked its `seq`." | plugin | plugin:outage.bufferAndFlush, plugin:session.renumber | graded | |
| 9.3 | "A hub MUST NOT ack an envelope it has not durably processed." | hub | hub:plugin.events.ingest, hub:plugin.poll.envelopeInvalid | partial | Effects are read back after the ack; durability across a hub restart cannot be tested |

### 9.4 Blocked-state honesty

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 9.4 | "the hub MUST expose the mirror on the server record: `lastSeenAt`, and `pendingEnvelopeCount`" | hub | hub:plugin.poll.lastSeen, hub:plugin.poll.queueOutlivesSession | graded | |

### 10.1 Scopes

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 10.1 | "A hub MUST refuse a scope it does not define, at mint time, rather than storing it" | hub | hub:admin.tokens.grammarEnforced, hub:admin.bans.scopes | graded | |
| 10.1 | "is not a glob this protocol defines, and MUST be refused rather than read as a literal" | hub | hub:webhooks.register, hub:webhooks.edit | partial | only a webhook filter (`core.*.death`) is probed; a mid-pattern `*` in a minted scope or an event query is not |
| 10.1 | "A hub MUST refuse at mint a binding naming a server it does not know, with `not_found`" | hub | hub:admin.tokens.serverBinding | graded | |
| 10.1 | "Some grants have no server to bind to, and a hub MUST refuse them on a bound token at mint, with `bad_request`" | hub | hub:admin.tokens.serverBinding, hub:admin.bans.scopes | graded | |
| 10.1 | "A hub MUST implement the binding." | hub | hub:admin.tokens.serverBinding, hub:admin.players.events, hub:admin.players.actions | graded | |
| 10.1 | "a hub that does not implement bindings MUST refuse a mint that asks for one" | hub | hub:admin.tokens.serverBinding | graded | a hub ignoring bindings fails the binding checks either way |

### 10.2 Enforcement

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 10.2 | "A hub MUST enforce scopes on every Admin API call." | hub | hub:admin.tokens.scopeEnforced, hub:state.guards, hub:kv.confinement, hub:webhooks.scope, hub:admin.players.notes, hub:admin.bans.scopes | partial | not every route is probed: server creation, enrollment tokens, credential revocation, and context entries are not |
| 10.2 | "Where the scope needed depends on a value in the request, the check MUST run against that value" | hub | hub:admin.tokens.scopeEnforced, hub:admin.tokens.idempotencyKeyIsNotAnOracle, hub:kv.confinement | partial | GET of an action is never refused on its code alone to an unbound token |
| 10.2 | "A route whose path names a server (`/api/v1/servers/{id}/...`) MUST refuse a bound token whose binding does not include that id, with `forbidden`" | hub | hub:admin.tokens.serverBinding | partial | serverBinding probes a fixed route list; context entries and other server routes are not in it |
| 10.2 | "`GET /api/v1/actions/{actionId}` MUST refuse with `forbidden` when the action's server is outside the binding" | hub | hub:admin.tokens.serverBinding | graded | |
| 10.2 | "`GET /api/v1/servers` MUST answer only the servers in the binding, never `forbidden`" | hub | hub:admin.tokens.serverBinding | graded | |
| 10.2 | "The dispatch check MUST run **before** the manifest is consulted" | hub | hub:admin.tokens.scopeEnforced, hub:admin.audit.recordsMutations | graded | |
| 10.2 | "A hub MUST therefore authorize the retry against **both** codes" | hub | hub:admin.tokens.idempotencyKeyIsNotAnOracle | partial | Allowed requested code with a forbidden stored code is probed; a forbidden requested code with an allowed stored code is not |
| 10.2 | "Authorizing a read MUST NOT itself change state." | hub | - | ungraded | |
| 10.2 | "Hubs that expire actions lazily on read (section 7) MUST establish the scope before applying that expiry" | hub | - | ungraded | |
| 10.2 | "A hub MUST NOT mint a token carrying a scope its minter does not itself hold." | hub | hub:admin.tokens.scopeEnforced | partial | only a dispatch-scoped token minting `admin` is probed |
| 10.2 | "A hub MUST NOT rely on that for anything an operator chose, such as an action `code` or an event type" | hub | hub:admin.tokens.scopeEnforced, hub:admin.tokens.eventScopeNarrowsFeed | graded | |

### 10.3 Reading a narrowed feed

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 10.3 | "A request carrying **no** `type` filter MUST be answered with the union of the token's granted patterns" | hub | hub:admin.tokens.eventScopeNarrowsFeed, hub:admin.players.events | partial | only single-pattern grants are probed; a union of several patterns is not |
| 10.3 | "A request carrying an explicit `type` term the grant does not **cover** MUST be refused with `forbidden`" | hub | hub:admin.tokens.eventScopeNarrowsFeed, hub:admin.players.events | graded | |

### 10.4 Token management (Admin API)

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 10.4 | "mints a token: `scopes` is REQUIRED" | hub | - | ungraded | Every fixture carries scopes (an empty list is a different refusal); a mint with the member absent is never sent |
| 10.4 | "MUST NOT be empty. `servers` is OPTIONAL and is the binding of section 10.1" | hub | hub:admin.tokens.grammarEnforced | graded | |
| 10.4 | "A hub MUST validate it before minting: an id it does not know is `not_found`" | hub | hub:admin.tokens.serverBinding, hub:admin.bans.scopes | graded | |
| 10.4 | "The `secret` is returned in this response and MUST NOT be retrievable afterwards" | hub | hub:admin.tokens.lifecycle | partial | The token list is decoded into modeled fields before the search, so an extra secret member is never seen; the audit record of the mint is not searched |
| 10.4 | "Revocation MUST take effect on the next request." | hub | hub:admin.tokens.lifecycle | graded | |
| 10.4 | "The record itself MUST survive, so that the audit log's references to it keep resolving" | hub | hub:admin.tokens.lifecycle | graded | |
| 10.4 | "revoking an already revoked token is idempotent and MUST NOT move the recorded revocation time" | hub | - | ungraded | |

### 10.5 The audit log

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 10.5 | "A hub MUST record every **authenticated** Admin API mutation, whether it succeeded or was refused" | hub | hub:admin.audit.recordsMutations, hub:admin.tokens.serverBinding, hub:webhooks.auditRecords | partial | only dispatches and a server creation are looked up; other mutation routes are not |
| 10.5 | "nothing could have happened. Each record MUST carry:" | hub | hub:admin.audit.recordsMutations | partial | `at` not checked as RFC 3339, digest not checked as SHA-256, `tokenName` after a rename not probed |
| 10.5 | "`sourceIp` MUST be the peer address of the connection" | hub | hub:admin.audit.recordsMutations | partial | only checked non-empty |
| 10.5 | "and MUST NOT be taken from a forwarded header, which is whatever the client typed" | hub | - | ungraded | |
| 10.5 | "`payloadDigest` MUST cover the whole body or be empty." | hub | hub:admin.audit.recordsMutations | partial | only two small bodies are shown to digest differently; never compared with the SHA-256 of the body |
| 10.5 | "A hub that caps request size MUST NOT record a digest of the truncated prefix it read" | hub | - | ungraded | |
| 10.5 | "the lever against it is revocation rather than silence: a hub MUST bound retention" | hub | - | n/a | retention is hub configuration measured in months, not observable in a run |
| 10.5 | "What a hub MUST NOT do is stop recording refusals" | hub | hub:admin.audit.recordsMutations | partial | one refusal is recorded; recording under a flood of refusals is not probed |
| 10.5 | "Retention is hub configuration rather than protocol, and a hub MUST NOT let it be unbounded in practice" | hub | - | n/a | retention is hub configuration measured in months, not observable in a run |

### 10.6 Bootstrap credentials

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 10.6 | "A hub that **generates** a bootstrap credential when none is configured MUST confine that to first run" | hub | - | n/a | boot-time behavior; the suite cannot restart the hub or read its logs |
| 10.6 | "it MUST NOT generate one while it already holds a usable minted token" | hub | - | n/a | boot-time behavior; the suite cannot restart the hub or read its logs |

### 11 Webhooks

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 11 | "A delivery, failed or not, MUST NOT affect the ingest, storage, or lifecycle of whatever triggered it" | hub | hub:webhooks.redact | partial | only the stored event surviving redaction is checked; ingest or action lifecycle under a failing target is not |
| 11 | "a hub MUST NOT lose a matching notification that arrives after the webhook was registered" | hub | hub:webhooks.pause, hub:webhooks.retryVisible | partial | a paused webhook and a failing target are probed; a busy or stopped delivery pipeline is not |

### 11.1 Subscribable notifications

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 11.1 | "A refused batch stores nothing and therefore notifies nothing, and a hub MUST NOT privilege core events over custom events here" | hub | hub:webhooks.signedDelivery | partial | custom events are delivered; core events are never delivered alongside them for comparison |
| 11.1 | "A hub MUST therefore deliver `audit.recorded` only to a webhook whose registration or latest edit was authorized" | hub | hub:webhooks.auditRecords | partial | catch-all and non-admin registration probed; a non-admin edit to `audit.*` is not |
| 11.1 | "a hub MAY derive differently but MUST have classified the link as lost no later than four times the `pollTimeout` plus 30 s" | hub | hub:webhooks.linkTransitions | partial | linkTransitions waits up to 70 s for the notification, so the classification bound itself is not established |
| 11.1 | "A hub MUST expose its current classification on the server record as `linkState`" | hub | hub:webhooks.linkTransitions | partial | only `up` is read; `down` after a loss and `unknown` are not |
| 11.1 | "a hub MUST have classified a reachable server `up`, and an unreachable one `down`" | hub | hub:webhooks.linkTransitions | partial | the `up` bound is read from `linkState`; `down` is inferred from the notification only |

### 11.2 Registration (Admin API)

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 11.2 | "so the registering token MUST also hold grants **covering** what it subscribes to" | hub | hub:webhooks.scope, hub:webhooks.edit, hub:webhooks.auditRecords | partial | the `actions:read` need of `action.completed` and the `servers:read` need of link filters are not probed |
| 11.2 | "`url` is REQUIRED" | hub | hub:webhooks.register | graded | |
| 11.2 | "MUST be `http` or `https`. - `events` is OPTIONAL" | hub | hub:webhooks.register, hub:webhooks.edit | graded | |
| 11.2 | "Registration is not a backfill request, and a hub MUST NOT replay stored history into a new webhook" | hub | - | ungraded | |
| 11.2 | "a request that cannot have been meant MUST NOT be answered as an edit that happened" | hub | hub:webhooks.edit | graded | |
| 11.2 | "The decision MUST be taken against the webhook as it stands when the edit is applied" | hub | - | ungraded | |
| 11.2 | "An edit that changes `url` MUST additionally be covered for the type of every delivery still pending on the webhook" | hub | hub:webhooks.retargetCoverage | graded | |
| 11.2 | "pausing an already paused webhook MUST NOT move it" | hub | hub:webhooks.pause | graded | |
| 11.2 | "While a webhook is paused a hub MUST NOT begin any delivery attempt for it, including retries" | hub | hub:webhooks.pause, hub:webhooks.retargetCoverage | partial | only first attempts are held; a retry of a delivery that failed before the pause is not probed |
| 11.2 | "a hub MUST make the pause decision and the booking one atomic step against the pause itself" | hub | - | ungraded | |

### 11.3 Delivery

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 11.3 | "The body MUST be byte-for-byte identical on every attempt of one delivery" | hub | hub:webhooks.retryVisible, hub:webhooks.replay | graded | |
| 11.3 | "A hub MUST NOT follow redirects: a 3xx answer is a failure like any other non-2xx status" | hub | - | ungraded | |
| 11.3 | "`allowed_mentions` MUST be present with an empty `parse` list" | hub | hub:webhooks.discordTemplate | graded | |
| 11.3 | "Text a player may have written MUST be escaped so it cannot carry Discord formatting into the embed" | hub | hub:webhooks.discordTemplate | partial | one bold-and-mention sample only |
| 11.3 | "every member MUST be cut to the length Discord accepts for it" | hub | - | ungraded | |
| 11.3 | "A hub MUST still render a type it has no wording for (a custom event above all) rather than skip the delivery" | hub | hub:webhooks.discordTemplate | graded | |

### 11.4 Signature

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 11.4 | "keyed with the webhook's secret. Every delivery MUST be signed." | hub | hub:webhooks.signedDelivery, hub:webhooks.discordTemplate, hub:webhooks.linkTransitions, hub:webhooks.auditRecords, hub:webhooks.replay | graded | |

### 11.5 Retries and the dead letter

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 11.5 | "A failed delivery MUST be retried with increasing delays" | hub | hub:webhooks.retryVisible | partial | one retry is required; the delays are never shown to increase |
| 11.5 | "the first retry MUST be **scheduled** no more than 60 s after the failed attempt" | hub | hub:webhooks.retryVisible | partial | retryVisible allows nextAttemptAt up to 70 s after the event, looser than 60 s after the failed attempt |
| 11.5 | "When its schedule is exhausted, the delivery MUST be marked **dead** and never tried again" | hub | - | ungraded | |
| 11.5 | "the delivery, its attempt count, and its last failure MUST be retained and readable through the deliveries page" | hub | - | ungraded | |
| 11.5 | "A replay whose delivery has an attempt in flight at that moment MUST win" | hub | - | ungraded | |
| 11.5 | "the token MUST additionally be covered for the delivery's type as for a filter naming exactly it" | hub | hub:webhooks.retargetCoverage, hub:webhooks.auditRecords | graded | |

### 12.1 Names and values

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 12.1 | "From the moment its expiry passes, the key MUST read as absent on every operation" | hub | hub:kv.ttl, hub:kv.listPrefix | partial | get, create-only set, and the listing are probed, with expiry tolerated for up to 10 s rather than from the instant it passes; create-only set runs only after get already reads the key absent, so its own view of an expired key is not established; incr and delete on an expired key are not |

### 12.2 Operations

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 12.2 | "A later draft MAY add one; a plugin MUST NOT assume it exists." | plugin | - | ungraded | |
| 12.2 | "`value` is REQUIRED" | hub | hub:kv.validate | graded | |
| 12.2 | "`ifRevision` is REQUIRED behavior, not an extension, because it is the whole concurrency model" | hub | hub:kv.cas, hub:kv.postSpellings | graded | |
| 12.2 | "When it exists, its value MUST be a JSON integer in `(-2^53, 2^53)`" | hub | hub:kv.incrAtomic | partial | a string value is probed; a stored integer outside the range is not |
| 12.2 | "and the sum MUST stay inside that range; otherwise the answer is `conflict` and nothing changes" | hub | - | ungraded | |
| 12.2 | "Concurrent incrs MUST each land exactly once" | hub | hub:kv.incrAtomic | graded | |
| 12.2 | "A hub MUST serve both spellings." | hub | hub:kv.postSpellings, hub:kv.pluginWrite | graded | |
| 12.2 | "A key whose expiry has passed MUST NOT appear, whether or not the hub has physically deleted it yet" | hub | hub:kv.listPrefix | partial | listPrefix tolerates the expired key until it disappears within 10 s, so a hub listing it briefly after expiry passes |
| 12.2 | "clients MUST NOT parse one or derive one" | admin client | - | n/a | no suite plays an admin client |
| 12.2 | "a hub MUST NOT return the same key on two pages of one walk nor skip one that was present and live when the walk began" | hub | hub:kv.listPaging | partial | only a walk over a namespace nobody writes during the walk |
| 12.2 | "A cursor whose key has since been deleted or expired MUST still resume correctly" | hub | - | ungraded | |
| 12.2 | "A namespace the token's grants do not cover MUST be omitted rather than refused" | hub | hub:kv.listNamespaces | graded | |
| 12.2 | "a client MUST NOT expect a `nextCursor` here" | admin client | - | n/a | no suite plays an admin client |

### 12.3 Confinement

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 12.3 | "this draft defines no read-only KV grant, and a hub MUST NOT invent one" | hub | - | ungraded | |

### 13 Installation ban list

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 13 | "A hub MUST implement this section." | hub | hub:admin.bans.lifecycle, hub:plugin.bans.pull | graded | |

### 13.1 Records and the revision

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 13.1 | "It MUST take a ban off the active list no later than 60 s after its `expiresAt`" | hub | hub:plugin.bans.expiry | partial | The 65 s deadline starts after the first read past expiry, so removal slightly past 60 s passes |

### 13.2 Managing the list (Admin API)

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 13.2 | "`player` and `reason` are REQUIRED, within the bounds of section 13.1" | hub | hub:admin.bans.lifecycle | graded | |

### 13.3 Pulling the list (Plugin API)

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 13.3 | "A hub MUST queue a `bans.changed` for every server whose stored manifest declares the `bans` capability" | hub | hub:plugin.bans.changed | partial | only a ban placed is probed; a lift and an expiry sweep are not |
| 13.3 | "and MUST NOT queue one for a server whose manifest does not" | hub | hub:plugin.bans.changed | partial | The incapable server is checked after a ban is created only; lift and expiry are not followed by a poll |
| 13.3 | "A hub MUST also queue one, carrying the current revision, when it accepts a manifest that declares the capability" | hub | - | ungraded | |
| 13.3 | "the acceptance and any change of the list MUST be ordered" | hub | - | ungraded | |
| 13.3 | "a hub MUST draw it from letters, digits, `-`, and `_` alone" | hub | hub:plugin.bans.pull | graded | |
| 13.3 | "A hub MUST serve both spellings." | hub | hub:plugin.bans.pull | graded | |

### 13.4 Enforcing the list (plugin)

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 13.4 | "A plugin that declares the `bans` capability MUST:" | plugin | plugin:bans.sync | partial | A complete walk before reporting, and the reported revision, are graded; persistence, own-clock expiry, platform filtering, and enforcement are not |
| 13.4 | "It MUST NOT report a revision it has not applied" | plugin | plugin:bans.sync | partial | only evidence is every page having been served; application itself is unobservable |

### 14 Versioning and compatibility

| § | Clause | Binds | Graded by | Status | Notes |
|---|---|---|---|---|---|
| 14 | "hubs MUST support the current and previous major version" | hub | - | n/a | only one major version exists, so there is no previous one to negotiate |
| 14 | "receivers MUST tolerate unknown fields" | either | hub:compat.unknownFields, hub:state.latestReplaces | partial | Hub probed on two requests and a snapshot, no plugin stage sends unknown fields |
| 14 | "Unknown envelope `type` values MUST be acked and ignored" | either | hub:plugin.poll.inboundTolerance, plugin:compat.unknownType | graded | |
