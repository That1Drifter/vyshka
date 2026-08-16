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

**Status:** early implementation. The protocol is at draft 0.4. The hub runs today with
enrollment, sessions, and the envelope exchange over long-poll, graded by thirty-three
black-box conformance checks in CI. Manifests and the action lifecycle come next.

## Start here

- [Protocol specification](spec/protocol.html): the normative document.
- [Repository](https://github.com/That1Drifter/vyshka): source, issues, discussion.
- [Contributing](https://github.com/That1Drifter/vyshka/blob/main/CONTRIBUTING.md):
  including the clean-room provenance policy, which is binding on all contributions.
- [Changelog](https://github.com/That1Drifter/vyshka/blob/main/CHANGELOG.md).
