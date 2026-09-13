# Vyshka

*Vyshka (вышка): Russian for "watchtower". The hub that watches the server and relays what it sees.*

Vyshka is an open source, self-hosted integration hub for game servers: a single-binary
backend (the hub) that connects in-game plugins to a public admin API for remote actions,
telemetry, live state, webhooks, and per-mod storage. DayZ is the first supported game;
Arma Reforger is the explicit second target, and nothing in the protocol is DayZ-specific.

## Status: early implementation

The protocol spec, [`spec/protocol.md`](spec/protocol.md) (draft 0.10), is the primary product,
together with the black-box conformance suites in [`conformance/`](conformance/README.md); the
hub and plugins are reference implementations of it. The implemented endpoints also have
machine-readable companions: [`spec/openapi-admin.yaml`](spec/openapi-admin.yaml),
[`spec/openapi-plugin.yaml`](spec/openapi-plugin.yaml),
[`spec/envelopes.schema.json`](spec/envelopes.schema.json),
[`spec/manifest.schema.json`](spec/manifest.schema.json),
[`spec/actions.schema.json`](spec/actions.schema.json), and
[`spec/events.schema.json`](spec/events.schema.json).

What runs today: the hub boots on an embedded SQLite database, serves `/healthz`, and
implements every protocol surface below. The conformance suites grade those surfaces in CI,
a headless browser test grades the panel, and a clean-room DayZ plugin under `plugins/dayz`
passes the plugin conformance harness against a live server.

Control plane first. An operator registers a game server. The plugin trades its one-time
enrollment token for permanent credentials, and those for a session; revoking the server
kills its session immediately. The plugin then holds a long-poll, the hub flushes queued
envelopes into it as they arrive, and per-direction sequence numbers with cumulative acks
make delivery at-least-once both ways.

Hub to plugin. The plugin publishes an action manifest. The hub validates each params
schema against the protocol's closed JSON Schema subset, stores accepted manifests
revision-gated, answers invalid ones with `manifest.reject` rather than an error, and
serves the result at `GET /api/v1/servers/{id}/manifest`. An operator dispatches an
action, validated against that schema before anything is queued. The plugin runs it and
reports back, and every state of `queued -> delivered -> running -> completed/failed/expired`
is observable at `GET /api/v1/actions/{id}`, with idempotent retries and TTL expiry.

Plugin to hub. The plugin pushes `event.batch` envelopes; core and mod-defined events land
in one append-only store with per-type retention, queryable at
`GET /api/v1/servers/{id}/events` with type patterns and cursor pagination. Full-list
state snapshots (players, vehicles, entities) are read at
`GET /api/v1/servers/{id}/state/{type}`. Signed webhooks push events and action outcomes
to other systems with retries and a dead letter, and a per-mod key/value store serves
both realms.

Everything is behind scoped Admin API tokens. A credential can be narrowed to one action
code or one event namespace, every route enforces its scope, and every authenticated
mutation lands in an append-only audit log at `GET /api/v1/audit`. The embedded panel at
`/panel/` turns the whole thing into a page: sign in with a token, pick a server and an
action, dispatch from a form generated from the manifest schema, and watch the result
arrive.

```
go build -o bin/vyshka-hub ./hub/cmd/vyshka-hub
VYSHKA_ADMIN_TOKEN=vya_local_dev_token ./bin/vyshka-hub serve
curl http://127.0.0.1:8080/healthz
```

`serve` takes `-addr` (env `VYSHKA_ADDR`), `-db` (env `DATABASE_URL`, empty means a local
SQLite file), `-admin-token` (env `VYSHKA_ADMIN_TOKEN`, also accepts `file:/path/to/secret`),
`-log-level`, `-panel` (env `VYSHKA_PANEL`; `false` serves no panel), and `-maps-dir` (env
`VYSHKA_MAPS_DIR`, a directory of map tilesets for the panel's live map; see
`panel/README.md`). With no admin token configured the hub mints one at boot and logs it,
which keeps first run to a single command; set the flag to keep it stable across restarts.
That generated credential is first-run behavior only: once the hub holds a scoped token of
its own it stops minting one, because a fresh superuser token on every boot would mean
revocation never survived a restart. Logs are structured JSON on stdout.

To run it as a container behind a reverse proxy, see [`deploy/`](deploy/README.md): a
`Dockerfile` (static binary, distroless, non-root), a compose file, and an nginx block with
the proxy timeouts a long-polling hub needs.

To watch the protocol flows in curl:

```
VYSHKA_ADMIN_TOKEN=vya_local_dev_token scripts/demo-enrollment.sh
VYSHKA_ADMIN_TOKEN=vya_local_dev_token scripts/demo-poll.sh
VYSHKA_ADMIN_TOKEN=vya_local_dev_token scripts/demo-manifest.sh
VYSHKA_ADMIN_TOKEN=vya_local_dev_token scripts/demo-action.sh
VYSHKA_ADMIN_TOKEN=vya_local_dev_token scripts/demo-events.sh
VYSHKA_ADMIN_TOKEN=vya_local_dev_token scripts/demo-tokens.sh
VYSHKA_ADMIN_TOKEN=vya_local_dev_token scripts/demo-panel.sh    # then open http://127.0.0.1:8080/
```

## Why

Server admins who want remote actions, live maps, and event feeds today mostly rely on
hosted third-party services: an account with someone else, per-seat billing, and an opaque
pipeline between the game server and the tools. Vyshka's goals are the opposite:

- **Self-hosted, single binary.** `./vyshka-hub` with embedded SQLite is the whole install
  story. Postgres via `DATABASE_URL` for those who need it. No account with anyone, no
  phone-home.
- **Spec-first.** A published protocol plus black-box conformance suites, so anyone can
  write a plugin for any game without reading hub source.
- **Everything is a tracked job.** Actions have observable lifecycles
  (queued, delivered, running, completed/failed/expired), delivery is at-least-once in both
  directions, and nothing is dropped without a counter incrementing somewhere visible.
- **Least privilege by default.** Scoped admin tokens down to per-action granularity, and a
  built-in audit log.

## Architecture

```
+------------------+        Plugin API         +-----------+       Admin API        +----------------+
| Game server      |  <== long-poll / WS ==>   |  Vyshka   |  <== REST + webhooks ==| Admin tools,   |
|  + Vyshka        |                           |    hub    |                        | bots, panel,   |
|    plugin (mod)  |                           | (1 binary)|                        | Discord bridge |
+------------------+                           +-----------+                        +----------------+
                                                    |
                                              SQLite / Postgres
```

Three components: a Go hub, per-game plugins speaking a game-agnostic Plugin API
(HTTP long-poll baseline, optional WebSocket upgrade), and an optional embedded web
panel that is a thin client over the Admin API. Plugin API and Admin API are separate
auth realms.

## Roadmap

| Milestone | Deliverable |
|---|---|
| M0 | Spec, envelope schemas, OpenAPI; conformance suites run against stubs |
| M1 | Hub core: enrollment, sessions, long-poll, manifests, action lifecycle, SQLite |
| M2 | DayZ plugin |
| M3 | Telemetry, state snapshots, webhooks, KV store |
| M4 | Scoped tokens, audit log, panel v1 |
| M5 | Custom contexts, Arma Reforger plugin, `--auto-tls` |

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Note the clean-room provenance policy there before
touching anything protocol- or plugin-related.

## License

[Apache-2.0](LICENSE).
