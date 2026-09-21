# Spike: what an installation ban list pull costs the DayZ server

Part of issue #80 (installation-wide ban list). The settled design has the plugin pull the
whole active list over plain HTTP, parse it, keep a per-identity map, write a copy to disk,
and answer the hub with the revision it enforces. The earlier request-shape spike
(`../dayz-restapi-headers`) showed a 1 MiB response reaching script intact and stopped
there, without parsing anything. Two things were unmeasured and both decide the pull's
shape:

1. What parsing, mapping, serializing, writing, and reading back a list of N entries costs
   on the server's main thread, which is the frame stall every player feels.
2. Where the engine's HTTP client or its string stops above 1 MiB.

Findings: [`results/findings.md`](results/findings.md). Short answer: every character
read and every append on this engine pays for the whole string, so the plugin's parser and
serializer are quadratic (1 000 entries cost 16 s), the file reader faults the process on
a 64 KiB line, `Substring` caps at 8 191 characters, and the HTTP client passes 32 MiB.
The pull is paged, the on-disk copy is written one entry per line, and the parser is
rewritten linear.

## Layout

| Path | What it is |
|---|---|
| `harness/VyshkaBansProbe.c` | Enforce Script probe appended to the spike mission's `init.c`. Six series: synthetic ban lists of 100 to 50 000 entries run through the plugin's own classes (`VyshkaJson`, `VyshkaBanEntry`, `VyshkaFiles`, `VyshkaClock`); the reader's line limit; a string indexing check; raw bodies of 2 to 32 MiB; padded lists that grow the bytes without the objects; allocation, append, and read cost by string length and by how the string is held. Every phase is timed with the engine's performance counter. It persists its progress under `$profile:VyshkaSpike/progress.txt` so a step that faults the process is skipped on the next boot. |
| `runner/main.go` | Serves the stub (`/bans?n=N&pad=B` in the planned pull shape, `/big?n=B` raw), derives the mission, boots the dedicated server with the mod, reboots after a fault until the probe reports finished, and prints the table with ticks converted to milliseconds. `-start-at` seeds the progress file to run one series alone. |
| `results/` | The write-up, one runner and probe log per run, the crash reports' script sections, and the per-run tables. |

## Reproducing

From the repository root, with the mod built (`go run ./plugins/dayz/cmd/vyshka-dayz build`)
and the DayZ dedicated server installed under Steam:

```
go run ./spikes/dayz-bans-pull-size/runner                 # the whole series
go run ./spikes/dayz-bans-pull-size/runner -start-at 8     # from the line-limit series on
```

The runner derives `mpmissions/vyshkaBansSpike.chernarusplus` from the stock offline
mission on first use and writes `vyshka_bans_spike_serverDZ.cfg` beside `serverDZ.cfg`.
The plugin runs with no config and idles; only its classes are used. No game client is
needed. The full ban list series is not worth repeating past 5 000 entries: the parse is
quadratic and the 10 000-entry step alone takes about 25 minutes.

The performance counter's unit is undocumented, so every probe line carries it beside the
frame clock and the runner derives ticks per millisecond from the pairs. The server runs
without `-freezecheck` on purpose: the stall is the number. `Print` truncates a line at
255 characters, so the probe emits one line per group of numbers.

## Provenance

Clean-room: the probe, the stub, and the measurements are original work written from the
public engine script headers shipped with the game (`scripts/3_game/http/restapi.c`,
`scripts/1_core/proto/ensystem.c`) and from observed behavior. No third-party integration
product was consulted or inspected.
