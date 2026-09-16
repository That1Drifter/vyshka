# DayZ plugin outbox across a server crash: measurements

Resolves issue #65.

**Short answer:** a process kill loses nothing the outbox had written. Across twenty kills
under event load, every record on disk was intact and was delivered by the next boot, no
event was stored twice, and the only loss was the event buffer's unflushed tail: between
0.1 s and 2.2 s of events, which is the 2 s flush interval plus the 200 ms plugin tick that
notices a flush is due. The number demanded no code change. What a power loss or an OS
crash takes from the page cache was not measured and cannot be shortened from script, so the
plugin's documentation now says exactly that.

## Environment

| Item | Value |
|---|---|
| Game build | DayZ 1.29.163709, server binary `DayZServer_x64.exe` |
| Script defines | `DAYZ_1_29, SERVER_FOR_WINDOWS, SERVER, PLATFORM_WINDOWS, RELEASE, NO_GUI` |
| Host OS | Windows 11 Pro 26100, NTFS |
| Plugin | `vyshka-dayz` 0.4.0 at commit `c0b37a0` (the mod as built from this tree) |
| Hub | the reference hub at the same commit, SQLite, loopback `127.0.0.1:8097` |
| Load generator | `harness/VyshkaOutboxLoad.c` in the `init.c` of a copied offline mission |
| Kill | `TerminateProcess` from the runner (`os.Process.Kill`), no notice to the engine |
| Date | 2026-09-16 |

Loopback removes network latency; the hub's own processing time is the only delay between a
poll and its ack. The kill is the closest reproducible stand-in for an engine crash: an
access violation ends the process the same way from the file system's point of view, with
every handle closed by the OS and nothing flushed that the process still held. A power loss
is a different event (the OS page cache is lost too) and is discussed at the end.

## Method

Each trial is one boot of the server carrying the mod and the load generator:

1. The hub is started once per series on a fresh database and one server is registered.
2. The server boots against a profile directory that keeps its credentials and its outbox
   from the previous boot. About 21 s after launch the plugin reports `link connected` and
   the generator starts emitting `spike.load` events, `{ "run": <boot epoch>, "n": 1, 2, 3...
   }`, through `VyshkaPlugin.Emit`, at a fixed rate. Every tick it also logs the range it
   emitted (`Print`) and rewrites a marker file with the last `n` through the same
   `OpenFile`/`FPrint`/`CloseFile` sequence the outbox uses.
3. After a random delay the runner kills the process and reads three things: the last `n`
   in the script log and in the marker file (how far the generator got), every record left
   in `Vyshka/outbox/` (what the outbox had written, and whether each file parses), and what
   the hub already held of this run (batches delivered but not yet acked are on disk and at
   the hub both).
4. The next boot loads the outbox, starts a session, and re-sends it. The runner waits until
   the hub holds everything the disk held, then counts rows, distinct `n`, gaps below the
   highest `n`, and duplicates. Loss is the generator's last `n` minus the distinct count.

Three series: A at 50 events/s with `pollTimeout` 5, B at 200 events/s with `pollTimeout`
5, and C at 50 events/s with the default `pollTimeout` 25. The runner and its flags are
described in `../README.md`; the raw records are in `a-50eps/`, `b-200eps/`, and
`c-poll25/` (`trials.jsonl`).

## Results

"Emitted" is the last `n` in the script log; the marker file agreed with it in every trial.
"On disk" is the range and count of the run's events in the outbox after the kill, "files"
how many outbox records there were (the start event, the manifest, and the earlier run's
batches count too), "torn" how many failed to parse. "Hub before" is what the hub already
held of the run at the kill, "hub after" the distinct count once the next boot had drained
the outbox. "Lost" is emitted minus hub after, in events and in seconds of load.

### Series A: 50 events/s, `pollTimeout` 5

| trial | kill after | emitted | on disk | files | torn | hub before | hub after | dup | gaps | lost | lost s |
|---|---|---|---|---|---|---|---|---|---|---|---|
| 1 | 15.7 s | 805 | 436..765 (330) | 3 | 0 | 655 | 765 | 0 | 0 | 40 | 0.8 |
| 2 | 16.1 s | 805 | 431..760 (330) | 3 | 0 | 650 | 760 | 0 | 0 | 45 | 0.9 |
| 3 | 12.1 s | 625 | 211..540 (330) | 3 | 0 | 430 | 540 | 0 | 0 | 85 | 1.7 |
| 4 | 16.9 s | 865 | 431..760 (330) | 3 | 0 | 650 | 760 | 0 | 0 | 105 | 2.1 |
| 5 | 19.7 s | 990 | 431..980 (550) | 5 | 0 | 650 | 980 | 0 | 0 | 10 | 0.2 |
| 6 | 15.2 s | 770 | 431..760 (330) | 3 | 0 | 650 | 760 | 0 | 0 | 10 | 0.2 |
| 7 | 8.7 s | 440 | 1..430 (430) | 4 | 0 | 210 | 430 | 0 | 0 | 10 | 0.2 |
| 8 | 13.9 s | 700 | 211..650 (440) | 4 | 0 | 430 | 650 | 0 | 0 | 50 | 1.0 |

### Series B: 200 events/s, `pollTimeout` 5

| trial | kill after | emitted | on disk | files | torn | hub before | hub after | dup | gaps | lost | lost s |
|---|---|---|---|---|---|---|---|---|---|---|---|
| 1 | 11.0 s | 2240 | 801..2200 (1400) | 7 | 0 | 1800 | 2200 | 0 | 0 | 40 | 0.2 |
| 2 | 12.3 s | 2540 | 401..2400 (2000) | 10 | 0 | 1400 | 2400 | 0 | 0 | 140 | 0.7 |
| 3 | 12.3 s | 2460 | 1..2420 (2420) | 15 | 0 | 820 | 2420 | 0 | 0 | 40 | 0.2 |
| 4 | 11.0 s | 2200 | 1..2000 (2000) | 15 | 0 | 200 | 2000 | 0 | 0 | 200 | 1.0 |
| 5 | 18.9 s | 3860 | 1..3800 (3800) | 19 | 0 | 1000 | 3800 | 0 | 0 | 60 | 0.3 |
| 6 | 17.7 s | 3560 | 1..3400 (3400) | 23 | 0 | 0 | 3400 | 0 | 0 | 160 | 0.8 |
| 7 | 22.2 s | 4420 | 1..4400 (4400) | 26 | 0 | 400 | 4400 | 0 | 0 | 20 | 0.1 |
| 8 | 20.2 s | 4100 | 1..4000 (4000) | 27 | 0 | 0 | 4000 | 0 | 0 | 100 | 0.5 |

### Series C: 50 events/s, `pollTimeout` 25 (the default)

| trial | kill after | emitted | on disk | files | torn | hub before | hub after | dup | gaps | lost | lost s |
|---|---|---|---|---|---|---|---|---|---|---|---|
| 1 | 38.8 s | 1955 | 1..1865 (1865) | 17 | 0 | 985 | 1865 | 0 | 0 | 90 | 1.8 |
| 2 | 26.1 s | 1325 | 1..1310 (1310) | 22 | 0 | 100 | 1310 | 0 | 0 | 15 | 0.3 |
| 3 | 50.2 s | 2510 | 1..2410 (2410) | 26 | 0 | 760 | 2410 | 0 | 0 | 100 | 2.0 |
| 4 | 50.3 s | 2515 | 1..2405 (2405) | 30 | 0 | 315 | 2405 | 0 | 0 | 110 | 2.2 |

At the default `pollTimeout` the outbox holds more between acks (a 25 s hold is 25 s of
batches, delivered or not), so the disk range is wider and the drain after the restart
takes two or three poll cycles; the outcome is the same.

## What the numbers say

1. **Nothing written to the outbox was lost, and nothing was torn.** Twenty kills, up to
   thirty records on disk at a time, several written per second: every file parsed,
   every event in them reached the hub. The outbox writes each record with `OpenFile`,
   `FPrint`, `CloseFile` in one script call; once `CloseFile` returns the bytes are in the
   OS page cache, which a process kill does not touch. A kill landing inside that one call
   would leave an empty or partial file, which `VyshkaOutbox.LoadRecord` already discards
   and counts; it did not happen in twenty tries and the window is microseconds wide.
2. **The loss is the event buffer's tail, bounded by the flush rule plus one tick.** The
   buffer flushes on the plugin's 200 ms tick once 2 s have passed since its oldest event or
   200 events are pending (`VyshkaEventBuffer`). The largest loss, 110 events at 50/s
   (2.2 s), is a kill that landed just before the tick that would have flushed a 2 s old
   buffer; the smallest, 10 events, is a kill just after a flush. At 200/s the count rule
   dominates and the largest loss was 200 events (1.0 s): a buffer that had reached the
   cap between two ticks. So the window is `min(2 s, 200 events)` plus up to one 200 ms
   tick, and a rate above 100 events/s lowers it in seconds.
3. **A batch delivered but unacked when the server dies is not stored twice.** In every
   trial the hub already held part of the run at the kill (up to 1800 events) while the
   same batches were still on disk: the hub applies a poll's envelopes before it holds the
   request, so a batch is stored the moment it is sent and stays on disk until the response
   that ends the hold carries the ack. The restart re-sent those batches with their original
   ids in the new session and the hub dropped them as duplicates (spec section 8.1): rows
   equal distinct in every trial, and the second boot's log reads `N unacked envelope(s)
   restored from disk` followed by a clean session.
4. **The script log and the marker file agreed in every trial.** `Print` lines reach the
   log file's bytes on disk before a kill can take them, and a file rewritten every tick
   through `OpenFile`/`CloseFile` shows the same count. Either is a valid post-mortem source
   for "how far did script get".
5. **A backlog drains at the per-poll budget per poll cycle.** Series B shows it: at 200
   events/s the outbox grew past what one poll carries (1000 events, `EVENTS_PER_POLL`),
   and the hub held each poll for the full `pollTimeout` even though the plugin had more
   queued behind the budget, because the hub answers early only when it has something to
   deliver. So a backlog of `E` events takes about `E / 1000` poll cycles to drain: at
   `pollTimeout` 25 that is 40 events/s sustained, and the drain columns in series B
   (6 s to 26 s at `pollTimeout` 5) are that arithmetic. Core telemetry on a real server is
   orders of magnitude below 40 events/s, so this is a throughput bound to know about, not
   a durability hole, and it is out of this spike's scope; a poll request that says "I have
   more" and is answered at once would lift it, which is a protocol change and gets its own
   issue.

## What was not measured

- **Power loss and OS crash.** Both lose the page cache. NTFS on Windows writes dirty pages
  back within seconds, but script exposes no fsync (`ensystem.c` declares none) so the
  plugin cannot shorten that window. The honest statement is that a process kill loses
  nothing written, and a power loss can lose the last few seconds of outbox records plus
  the same event-buffer tail. An operator who needs less runs the game server on a machine
  with a battery and a journaling file system, which is the same advice the game's own
  persistence gets.
- **A kill during the outbox write itself.** The `OpenFile` to `CloseFile` span is one
  synchronous script call; twenty random kills did not land in it. The load path already
  handles the empty or partial file it would leave.
- **Disk full or a locked file.** Not a crash question; `WriteAll` returns false and the
  outbox logs that the envelope will be lost on restart.

## Consequences

- The plugin README's "Events" and "Durability" paragraphs and the outbox's header comment
  now state the measured window and name the power-loss case as unmeasured.
- No plugin or hub code changed. Lowering the 2 s flush would cut the window in proportion
  and cost one outbox file per flush; at the event rates a DayZ server produces, 2 s is
  fine, and an operator who wants less can ask for a knob later.
- The drain-rate bound (finding 5) is filed separately.

Behavior can change between game patches. Re-run the spike (see `../README.md`) when the
plugin is validated against a new major DayZ version.
