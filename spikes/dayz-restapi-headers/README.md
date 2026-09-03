# Spike: DayZ `RestApi` request shape

Measures what a DayZ script HTTP client can and cannot do beyond holding a poll open (that
question is `../dayz-restapi-poll-timeout`): whether it can send an `Authorization` header,
what it sees of a 4xx or 5xx response, how large a request and a response can be, and how a
refused connection surfaces. The DayZ plugin (issue #14) is designed around the answers.

Findings: [`results/findings.md`](results/findings.md).

## Layout

| Path | What it is |
|---|---|
| `stub/main.go` | Stub HTTP server that logs every header and body it receives and answers `/post`, `/echo`, `/big`, `/status`, `/utf8`. |
| `harness/VyshkaHeaderProbe.c` | Enforce Script probe that fires the request matrix and prints one tab-separated line per event. |
| `results/` | Captured stub log, server script log, and the write-up. |

## Reproducing

1. Start the stub:

   ```
   go run ./stub -addr 127.0.0.1:8098
   ```

2. Reuse the spike mission from `../dayz-restapi-poll-timeout` (its README describes making
   it): replace the probe appended to its `init.c` with `harness/VyshkaHeaderProbe.c` and
   call `VyshkaHeaderProbe.Run();` as the last statement of `main()`.

3. Start the server as in that README, with `-profiles=<this spike>/results/profile`.

4. Read the results with:

   ```
   findstr VYSHKA_HPROBE <spike>\results\profile\script_*.log
   ```

The matrix runs unattended in about a minute after mission init. No game client and no mod
PBO are needed.

## Provenance

Clean-room: the probe, the stub, and the measurements are original work written from the
public engine script headers shipped with the game (`scripts/3_game/http/restapi.c`,
`scripts/1_core/proto/ensystem.c`) and from observed behavior. No third-party integration
product was consulted or inspected.
