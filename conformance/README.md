# Conformance suites

The suites are the product. A hub or a plugin is compliant when it passes them, with no
reading of reference-implementation source required.

Both suites are black-box: they speak only the protocol over HTTP. Nothing here imports hub
or plugin code, because a suite that did could not grade a third-party implementation.

| Suite | Question it answers | Status |
|---|---|---|
| `hub/` | Is this hub compliant? | Runnable, one prefactor check group |
| `plugin/` | Is this plugin compliant? | Not built yet, see `plugin/README.md` |

## Hub suite

Point it at any running hub:

```
go run ./conformance/hub -url http://127.0.0.1:8080
```

```
conformance: hub suite against http://127.0.0.1:8080

PASS  health.responds    GET /healthz answers 200 with JSON
PASS  health.status      GET /healthz reports status ok

2 checks, 0 failed
```

The command exits 0 when every check passes and 1 when any check fails, so it drops straight
into CI.

| Flag | Default | Meaning |
|---|---|---|
| `-url` | `http://127.0.0.1:8080` | Base URL of the hub under test |
| `-timeout` | `10s` | Per-request timeout |
| `-wait` | `0` | Poll `/healthz` for up to this long before starting, for CI |
| `-json` | off | Machine-readable results instead of the text report |

### Grading the reference hub locally

```
go build -o bin/vyshka-hub ./hub/cmd/vyshka-hub
./bin/vyshka-hub serve &
go run ./conformance/hub -url http://127.0.0.1:8080 -wait 30s
```

CI runs exactly this against every push and pull request.

### Adding a check

Append a `Check` to the `checks` slice in `hub/checks.go`. Each one carries an `ID`, a
one-line `Title`, and a `Section` citing the clause of `spec/protocol.md` it enforces, so a
failure points at the rule rather than at the runner. Use `Env.get` rather than a bare HTTP
client so every check shares the same timeout and body-size handling.

Checks must fail loudly rather than skip. A check that cannot run is a failing check: silent
skips are how a suite ends up green against a hub that implements nothing.
