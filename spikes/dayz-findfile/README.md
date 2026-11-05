# Spike: FindFile under `$profile:` on DayZ 1.30

The plugin's outbox restore lists its record directory with `FindFile`. On DayZ 1.30 the
engine self-test's `files.outbox` check stopped restoring a record, and this spike measures
why: what `FindFile` hands back for a directory under `$profile:`, under each pattern and
flag set, and through the paths that avoid the alias (issue #149).

Findings: [`results/findings.md`](results/findings.md).

## Layout

| Path | What it is |
|---|---|
| `harness/VyshkaFindProbe.c` | Enforce Script probe appended to the spike mission's `init.c`: makes `$profile:VyshkaFind` with three files, then prints one line per `FindFile` pattern tried, with the handle and every name returned |
| `runner/main.go` | Derives the mission from the stock Chernarus one (without its `scripts/` folder), boots the server once with no mod, prints the probe's lines, and stops the server |
| `results/` | The per-run logs and the write-up; raw server profile output stays untracked |

## Reproducing

From the repository root, with the dedicated server installed at the default Steam path
and nothing else on game port 2402:

```
go run ./spikes/dayz-findfile/runner
go run ./spikes/dayz-findfile/runner -relative
```

`-relative` passes `-profiles` relative to the server directory instead of absolute. Each
run is one boot of under two minutes.

## Provenance

Clean-room: the probe, the runner, and the measurements are original work written from the
public engine script headers shipped with the game (`scripts/1_core/proto/ensystem.c`) and
observed behavior. The runner is adapted from `spikes/dayz-world-clock`. No third-party
integration product was consulted or inspected.
