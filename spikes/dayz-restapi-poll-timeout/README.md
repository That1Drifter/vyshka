# Spike: DayZ `RestApi` long-poll hold time

Resolves the pre-1.0 open question in `spec/protocol.md` section 3.1: how long can the hub
hold a `/plugin/v1/poll` response open before the DayZ script HTTP client gives up, and what
is the safe `pollTimeout` floor.

Findings: [`results/findings.md`](results/findings.md).

## Layout

| Path | What it is |
|---|---|
| `stub/main.go` | Stub HTTP server that holds responses open in three shapes (`/hold`, `/headers`, `/drip`) and logs when the client disconnects. |
| `harness/VyshkaPollProbe.c` | Enforce Script probe that drives the test matrix and prints one tab-separated line per event. |
| `vyshka_spike_serverDZ.cfg` | Minimal offline server config pointing at the probe mission. |
| `results/` | Captured stub log, server script log, and the write-up. |

## Reproducing

1. Start the stub:

   ```
   go run stub/main.go -addr 127.0.0.1:8099
   ```

2. Copy a vanilla offline mission to `<DayZServer>/mpmissions/vyshkaSpike.chernarusplus`,
   append `harness/VyshkaPollProbe.c` to its `init.c`, and add `VyshkaPollProbe.Run();` as
   the last statement of `main()`.

3. Copy `vyshka_spike_serverDZ.cfg` next to `serverDZ.cfg` and start the server:

   ```
   DayZServer_x64.exe -config=vyshka_spike_serverDZ.cfg -port=2402 ^
     -profiles=<spike>/results/profile -dologs -adminlog -freezecheck
   ```

4. The probe starts a few seconds after mission init. Read the results with:

   ```
   findstr VYSHKA_PROBE <spike>\results\profile\script_*.log
   ```

The matrix runs unattended in a single boot and takes about five minutes. No game client and
no mod PBO are needed: mission `init.c` is loose script, so the probe compiles at server
start.

## Provenance

Clean-room: the probe, the stub, and the measurements are original work written from the
public engine script headers shipped with the game (`scripts/3_game/http/restapi.c`) and from
observed behavior. No third-party integration product was consulted or inspected.
