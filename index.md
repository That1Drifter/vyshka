---
title: Home
layout: default
nav_order: 1
---

# Vyshka

*Vyshka (вышка): Russian for "watchtower". The hub that watches the server and relays what
it sees.*

Vyshka is an open source, self-hosted integration hub for game servers: a single-binary
backend that connects in-game plugins to a public admin API for remote actions, telemetry,
live state, webhooks, and per-mod storage. DayZ first, Arma Reforger second, and the
protocol is game-agnostic by design.

The project is **spec-first**: the [protocol specification](spec/protocol.html) and its
conformance suites are the product; the hub and plugins are reference implementations.

**Status:** early implementation, protocol at draft 0.23 (2026-09-15). The hub implements
every protocol surface: enrollment, sessions, the envelope exchange over long-poll,
manifest publish with schema-subset validation, the full action lifecycle (dispatch,
execute, observe, expire), telemetry ingest with a queryable event feed, full-list state
snapshots with a live state endpoint, webhooks with signed delivery, retries, a dead letter,
and a Discord template, a per-mod key/value store, and scoped Admin API tokens with an
audit log. It runs on embedded SQLite by default or on Postgres, and an embedded panel
covers sign-in, the server list, manifest-driven action forms, the event feed, and a live
map. Black-box conformance suites grade the hub and any candidate plugin in CI, including
the session-change renumbering rule that only shows up when a game server restarts with
traffic in flight. The clean-room DayZ plugin passes that harness against a live DayZ 1.29
server with heal and moderation actions, core player and server telemetry, and player
snapshots. The [roadmap](https://github.com/That1Drifter/vyshka/blob/main/ROADMAP.md) lists
what comes next.

## Start here

- [Protocol specification](spec/protocol.html): the normative document.
- [Repository](https://github.com/That1Drifter/vyshka): source, issues, discussion.
- [Contributing](https://github.com/That1Drifter/vyshka/blob/main/CONTRIBUTING.md):
  including the clean-room provenance policy, which is binding on all contributions.
- [Changelog](https://github.com/That1Drifter/vyshka/blob/main/CHANGELOG.md).
