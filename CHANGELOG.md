# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Until the
first release, entries accumulate under **Unreleased**; dates mark when a change landed on
`main`. The protocol document, the hub, and each plugin will be versioned independently
(SemVer) once implementation starts, and this file will gain per-artifact sections at that
point if needed.

## [Unreleased]

### Added

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

### Changed

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
