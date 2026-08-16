# Vyshka

*Vyshka (вышка): Russian for "watchtower". The hub that watches the server and relays what it sees.*

Vyshka is an open source, self-hosted integration hub for game servers: a single-binary
backend (the hub) that connects in-game plugins to a public admin API for remote actions,
telemetry, live state, webhooks, and per-mod storage. DayZ is the first supported game;
Arma Reforger is the explicit second target, and the protocol is designed to be
game-agnostic from day one.

## Status: early implementation

The protocol spec, [`spec/protocol.md`](spec/protocol.md) (draft 0.4), is the primary product,
together with the black-box conformance suites in [`conformance/`](conformance/README.md); the
hub and plugins are reference implementations of it. The implemented endpoints also have
machine-readable companions: [`spec/openapi-admin.yaml`](spec/openapi-admin.yaml),
[`spec/openapi-plugin.yaml`](spec/openapi-plugin.yaml), and
[`spec/envelopes.schema.json`](spec/envelopes.schema.json).

What runs today: the hub boots on an embedded SQLite database, serves `/healthz`, and
implements the first two protocol surfaces. An operator can register a game server, a plugin
can trade its one-time enrollment token for permanent credentials and those credentials for a
session, and revoking a server kills its session immediately. On top of that sits the
transport: a plugin holds a long-poll, the hub flushes queued envelopes into it the moment
they arrive, and per-direction sequence numbers with cumulative acks make delivery
at-least-once both ways. Thirty-three conformance checks grade all of it in CI. Manifests and
the action lifecycle come next.

```
go build -o bin/vyshka-hub ./hub/cmd/vyshka-hub
VYSHKA_ADMIN_TOKEN=vya_local_dev_token ./bin/vyshka-hub serve
curl http://127.0.0.1:8080/healthz
```

`serve` takes `-addr` (env `VYSHKA_ADDR`), `-db` (env `DATABASE_URL`, empty means a local
SQLite file), `-admin-token` (env `VYSHKA_ADMIN_TOKEN`, also accepts `file:/path/to/secret`),
and `-log-level`. With no admin token configured the hub mints one at boot and logs it, which
keeps first run to a single command; set the flag to keep it stable across restarts. Logs are
structured JSON on stdout.

To watch the protocol flows in curl:

```
VYSHKA_ADMIN_TOKEN=vya_local_dev_token scripts/demo-enrollment.sh
VYSHKA_ADMIN_TOKEN=vya_local_dev_token scripts/demo-poll.sh
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

Three components: a Go **hub**, per-game **plugins** speaking a game-agnostic Plugin API
(HTTP long-poll baseline, optional WebSocket upgrade), and an optional embedded web
**panel** that is a thin client over the Admin API. Plugin API and Admin API are separate
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
