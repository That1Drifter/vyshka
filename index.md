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

**Status:** early implementation. The protocol is at draft 0.10. The hub runs today with
enrollment, sessions, the envelope exchange over long-poll, manifest publish with
schema-subset validation, the full action lifecycle (dispatch, execute, observe, expire),
telemetry ingest with a queryable event feed, and scoped Admin API tokens with an audit log,
graded by fifty-five black-box conformance checks in CI. The plugin conformance harness now
runs too: a mock hub that grades any candidate plugin black-box, including the
session-change renumbering rule that only shows up when a game server restarts with traffic
in flight. Webhooks come next.

## Start here

- [Protocol specification](spec/protocol.html): the normative document.
- [Repository](https://github.com/That1Drifter/vyshka): source, issues, discussion.
- [Contributing](https://github.com/That1Drifter/vyshka/blob/main/CONTRIBUTING.md):
  including the clean-room provenance policy, which is binding on all contributions.
- [Changelog](https://github.com/That1Drifter/vyshka/blob/main/CHANGELOG.md).
