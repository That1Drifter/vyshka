# Conformance suites

The suites are the product. A hub or a plugin is compliant when it passes them, with no
reading of reference-implementation source required.

Both suites are black-box: they speak only the protocol over HTTP. Nothing here imports hub
or plugin code, because a suite that did could not grade a third-party implementation.

| Suite | Question it answers | Status |
|---|---|---|
| `hub/` | Is this hub compliant? | Runnable: health, error model, enrollment, sessions, envelope exchange, manifests, actions |
| `plugin/` | Is this plugin compliant? | Not built yet, see `plugin/README.md` |

## Hub suite

Point it at any running hub, with that hub's admin token:

```
go run ./conformance/hub -url http://127.0.0.1:8080 -admin-token vya_...
```

```
conformance: hub suite against http://127.0.0.1:8080

PASS  health.responds                GET /healthz answers 200 with JSON
PASS  health.status                  GET /healthz reports status ok
PASS  errors.shape                   An unrouted path answers 404 in the protocol error shape
...
PASS  compat.unknownFields           Unknown request fields are tolerated, not rejected

42 checks, 0 failed
```

The command exits 0 when every check passes, 1 when any check fails, and 2 when the suite
could not run at all, so it drops straight into CI.

| Flag | Default | Meaning |
|---|---|---|
| `-url` | `http://127.0.0.1:8080` | Base URL of the hub under test |
| `-admin-token` | env `VYSHKA_ADMIN_TOKEN` | Admin credential of the hub under test; **required** |
| `-timeout` | `10s` | Per-request timeout for everything but a long-poll |
| `-check-timeout` | `90s` | Budget for one check, which may hold several long-polls |
| `-wait` | `0` | Poll `/healthz` for up to this long before starting, for CI |
| `-json` | off | Machine-readable results instead of the text report |

Long-polls get their own client, with a timeout of the protocol's maximum `pollTimeout` plus
the 5 s margin a plugin must also leave (spec section 3.1.1). A held request is the hub
behaving correctly, so it must never be mistaken for a hung one. Checks that intend to wait a
hold out negotiate the 5 s floor; the suite spends a few seconds there and nowhere else.

The admin token is required rather than optional because the suite grades both realms. A run
that quietly skipped every Admin API check would report green against a hub that implements
nothing, so the runner refuses to start instead.

The suite creates its own server records as it goes, under the game id `conformance` and names
prefixed `conformance:`. Point it at a scratch hub, not a production one.

### Grading the reference hub locally

```
go build -o bin/vyshka-hub ./hub/cmd/vyshka-hub
VYSHKA_ADMIN_TOKEN=vya_local_dev_token ./bin/vyshka-hub serve &
go run ./conformance/hub -url http://127.0.0.1:8080 -admin-token vya_local_dev_token -wait 30s
```

CI runs exactly this against every push and pull request.

### Adding a check

Append a `Check` to the `checks` slice in `hub/checks.go`. Each one carries an `ID`, a
one-line `Title`, and a `Section` citing the clause of `spec/protocol.md` it enforces, so a
failure points at the rule rather than at the runner. Use the `Env` helpers (`expect`,
`expectError`, `expectPoll`, `newServer`, `newEnrolled`, `startSession`) rather than a bare
HTTP client, so every check shares the same timeout, body-size handling, and failure wording.

A check that needs to act as a game server uses the fake plugin in `hub/plugin.go`:
`env.newFakePlugin` walks a fresh server through enrollment and a session, and the returned
value tracks both directions' sequence state the way a real plugin must. `pollAndAck` is the
loop a plugin actually runs; `poll` sends a request verbatim, for checks that need to lie to
the hub; `pollInBackground` starts a held poll so the check can go do something that should
wake it. It grows with each slice rather than being rewritten per check.

Checks must be independent: each one mints the server records it needs. They run in order, but
nothing may depend on state another check left behind.

The request and response shapes in `checks.go` are written out by hand rather than shared with
the hub package. That duplication is the point: if the reference implementation changes a field
name, the suite must fail, not follow along.

Checks must fail loudly rather than skip. A check that cannot run is a failing check: silent
skips are how a suite ends up green against a hub that implements nothing.
