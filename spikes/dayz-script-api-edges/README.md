# Spike: DayZ script API edges

Measures five engine behaviors that earlier spikes bounded or left open: the `RestApi`
read-timeout option outside its documented 3..120 s range, the callbacks for `204`, `302`,
and further `4xx`/`5xx` statuses, the config path of a clothing class's cargo grid, whether
`FPrint` output reaches the file before `CloseFile`, and the exact line length at which
`FGets` faults.

Findings: [`results/findings.md`](results/findings.md).

## Layout

| Path | What it is |
|---|---|
| `harness/VyshkaEdgeProbe.c` | Enforce Script probe appended to the spike mission's `init.c`. Phase A (first boot) runs the config, status, timeout, and file series and ends by asking to be killed; phase B (later boots) tries one line length at a time, persisting progress so a length that faults is skipped next boot. |
| `runner/main.go` | Serves the stub endpoints (`/status`, `/hold`, `/ok`), derives the mission, boots the server until the probe reports done, reads file sizes from outside the process at the probe's markers, and kills the process when asked. |
| `results/` | Distilled per-run logs and the write-up; raw server profile output stays untracked. |

## Reproducing

From the repository root, with the dedicated server installed at the default Steam path
and nothing else on game port 2402 or loopback port 8094:

```
go run ./spikes/dayz-script-api-edges/runner
```

The runner writes `vyshka_edge_spike_serverDZ.cfg` next to `serverDZ.cfg`, derives
`mpmissions/vyshkaEdgeSpike.chernarusplus` from the stock Chernarus mission, and takes four
boots and about five minutes. `-fresh=false` keeps the probe's progress file so an
interrupted series resumes.

## Provenance

Clean-room: the probe, the runner, and the measurements are original work written from the
public engine script headers shipped with the game (`scripts/3_game/http/restapi.c`,
`scripts/1_core/proto/ensystem.c`), the game's shipped config data, and observed behavior.
No third-party integration product was consulted or inspected.
