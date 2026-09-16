# Spike: DayZ plugin outbox across a server crash

Resolves issue #65: the plugin's file-backed outbox (`VyshkaOutbox`, spec section 9.3) is
written through the engine's `OpenFile`, `FPrint`, `CloseFile`, with no fsync exposed to
script, and the README documented the loss window as "whatever the OS had not flushed"
without a number. This spike kills the server under event load, reads what the outbox
holds on disk, restarts, and counts what the hub received against what the plugin emitted.

Findings: [`results/findings.md`](results/findings.md).

## Layout

| Path | What it is |
|---|---|
| `harness/VyshkaOutboxLoad.c` | Enforce Script load generator, appended to a mission `init.c`: once the plugin's link is connected it emits numbered `spike.load` events through the plugin's own `Emit`, and records how far it got in the script log and in a marker file written the way the outbox is. |
| `runner/main.go` | Go orchestration: builds and starts a hub on loopback with a fresh SQLite database, registers a server, derives the spike mission, boots the DayZ server, kills it with `TerminateProcess` at a random moment, inspects the outbox directory, boots again, and counts at the hub. |
| `results/` | The write-up, and one directory per series (`a-50eps`, `b-200eps`, `c-poll25`) holding `trials.jsonl`, one line per kill. The profile directory, the hub database, and the hub log are written next to it and ignored by git. |

## Reproducing

From the repository root, with a local DayZ dedicated server install:

```
go run ./plugins/dayz/cmd/vyshka-dayz build
go run ./spikes/dayz-outbox-crash/runner -trials 8 -per-tick 5 -tick-ms 100 -poll-timeout 5
```

The runner derives `mpmissions/vyshkaOutboxSpike.chernarusplus` from the stock offline
mission (a full copy the first time, an `init.c` rewrite every time), writes its own server
config next to `serverDZ.cfg`, and removes the config when it is done. Each boot takes about
a minute; a run of eight trials is nine boots. `-per-tick` and `-tick-ms` set the load
(5 per 100 ms is 50 events/s, above the 2 s flush's 200-event cap every 4 s, so batches
flush on time; 20 per 100 ms flushes on count every second). `-kill-min` and `-kill-max`
bound the random delay between the load starting and the kill; `-seed` makes the delays
repeatable; `-results` names the directory a series writes to (the three series under
`results/` were `-results spikes/dayz-outbox-crash/results/a-50eps` and so on). No game
client is needed.

## What is measured

For each killed boot the runner records:

- **emitted**: the highest event number in the script log and in the marker file, two
  independent views of how far the generator got before the process died;
- **on disk**: the outbox records left in `Vyshka/outbox/`, how many parse, how many are
  torn, and the range of event numbers they hold;
- **at the hub before the kill**: what the hub had already stored of that boot's run;
- **at the hub after the next boot**: what it holds once the outbox has been redelivered,
  with duplicates and gaps below the highest number;
- **lost**: emitted minus received, in events and in seconds of load.

## Provenance

Clean-room: the load generator, the runner, and the measurements are original work written
from the public engine script headers shipped with the game (`scripts/1_core/proto/ensystem.c`)
and from observed behavior. No third-party integration product was consulted or inspected.
