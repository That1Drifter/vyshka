# DayZ plugin outbox across a server crash: measurements

Resolves issue #65.

**Short answer:** in twenty kills under event load, no record the outbox had finished
writing was lost: every record on disk was intact and was delivered by the next boot, no
event was stored twice, and the only loss was the event buffer's unflushed tail: between
0.1 s and 2.1 s of events, which is the 2 s flush interval plus the 200 ms plugin tick that
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

Loopback removes network latency; what remains between a poll and its ack is the hub's own
processing and the hold the hub keeps on purpose when it has nothing to deliver (which is
what the drain column measures). The kill is the closest reproducible stand-in for an
engine crash: an
access violation ends the process the same way from the file system's point of view, with
every handle closed by the OS and nothing flushed that the process still held. A power loss
is a different event (the OS page cache is lost too) and is discussed at the end.

## Method

Each trial is one boot of the server carrying the mod and the load generator:

1. The hub is started once per series on a fresh database and one server is registered.
2. The server boots against a profile directory that keeps its credentials and its outbox
   from the previous boot. About 21 s after launch the plugin reports `link connected`. If
   the previous boot was killed, its outbox rides the first polls now; the runner waits
   until the hub holds everything that was on disk and the acks have deleted the records,
   and only then writes a go file the generator is waiting for. Recovery and load never
   overlap, and the kill schedule below starts when the load does.
3. The generator emits `spike.load` events, `{ "run": <boot epoch>, "n": 1, 2, 3... }`,
   through `VyshkaPlugin.Emit`, at a fixed rate. Before each tick's `Emit` calls it
   announces the range it is about to emit (a `Print` line, and a marker file rewritten
   through the same `OpenFile`/`FPrint`/`CloseFile` sequence the outbox uses); after them it
   confirms the range. A kill inside a tick therefore leaves an interval, not a wrong
   count: the true number of emitted events lies between the last confirmed `n` and the
   last announced `n`, at most one tick apart, provided the three observations are readable
   and consistent; the runner stops the series rather than record a zero when they are not.
4. The runner sees the load start when the generator's `started` line appears in the
   script log, which it reads every 200 ms, draws a random delay, and kills the process
   when it elapses. Times in the tables are therefore measured from that observation, not
   from the engine's own clock, and carry up to 200 ms of observation latency. After the
   kill it reads three things: the last confirmed and announced `n` in the script log and
   the marker (how far the generator got), every record left in `Vyshka/outbox/` (what the
   outbox had written, and whether each file parses), and what the hub already held of this
   run (batches delivered but not yet acked are on disk and at the hub both).
5. The next boot loads the outbox, starts a session, and re-sends it. Once the hub holds
   everything the disk held and the acks have cleared the disk, the runner counts rows,
   distinct `n`, gaps below the highest `n`, and duplicates. Loss is the generator's count
   minus the distinct count, reported as an interval: confirmed minus received at the low
   end, announced minus received at the high end.

Three series: A at 50 events/s with `pollTimeout` 5, B at 200 events/s with `pollTimeout`
5, and C at 50 events/s with the default `pollTimeout` 25. The runner and its flags are
described in `../README.md`; the raw records are in `a-50eps/`, `b-200eps/`, and
`c-poll25/` (`trials.jsonl`).

## Results

"Kill after" is the actual delay from the runner seeing the load start, with the planned
delay in parentheses; agreement between the two says the schedule was kept, not that the
engine's clock was read. "Emitted" is the last confirmed and the last announced `n` in the script log;
the marker file carried the announced value in every trial. "On disk" is the range and
count of the run's events in the outbox after the kill, "files" how many outbox records
there were (the start event and the manifest count too), "torn" how many failed to parse.
"Hub before" is what the hub already held of the run at the kill, "hub after" the distinct
count once the next boot had delivered the outbox. "Lost" is emitted minus hub after, as the
interval the two emitted counts give, and "lost s" is its upper end in seconds of load.
"Drain" is the time from the runner seeing the next boot's link connect to the run being
delivered and acked, so it includes the poll cycles the recovery took.

### Series A: 50 events/s, `pollTimeout` 5

| trial | kill after (planned) | emitted | on disk | files | torn | hub before | hub after | dup | gaps | lost | lost s | drain |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 1 | 15.1 s (15.1) | 760..760 | 436..655 (220) | 2 | 0 | 655 | 655 | 0 | 0 | 105 | 2.1 | 6 s |
| 2 | 13.8 s (13.8) | 700..700 | 106..655 (550) | 5 | 0 | 435 | 655 | 0 | 0 | 45 | 0.9 | 6 s |
| 3 | 16.1 s (16.1) | 815..815 | 441..770 (330) | 3 | 0 | 660 | 770 | 0 | 0 | 45 | 0.9 | 6 s |
| 4 | 16.1 s (16.1) | 815..815 | 436..765 (330) | 3 | 0 | 655 | 765 | 0 | 0 | 50 | 1.0 | 6 s |
| 5 | 16.9 s (16.9) | 855..855 | 441..770 (330) | 3 | 0 | 660 | 770 | 0 | 0 | 85 | 1.7 | 6 s |
| 6 | 15.5 s (15.5) | 785..785 | 436..765 (330) | 3 | 0 | 655 | 765 | 0 | 0 | 20 | 0.4 | 6 s |
| 7 | 12.3 s (12.3) | 625..625 | 106..545 (440) | 4 | 0 | 435 | 545 | 0 | 0 | 80 | 1.6 | 6 s |
| 8 | 16.2 s (16.2) | 820..820 | 441..770 (330) | 3 | 0 | 660 | 770 | 0 | 0 | 50 | 1.0 | 6 s |

### Series B: 200 events/s, `pollTimeout` 5

| trial | kill after (planned) | emitted | on disk | files | torn | hub before | hub after | dup | gaps | lost | lost s | drain |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 1 | 18.0 s (18.0) | 3620..3620 | 1821..3420 (1600) | 8 | 0 | 2820 | 3420 | 0 | 0 | 200 | 1.0 | 10 s |
| 2 | 14.5 s (14.5) | 2940..2940 | 801..2800 (2000) | 10 | 0 | 1800 | 2800 | 0 | 0 | 140 | 0.7 | 10 s |
| 3 | 15.3 s (15.3) | 3100..3100 | 1801..3000 (1200) | 6 | 0 | 2800 | 3000 | 0 | 0 | 100 | 0.5 | 10 s |
| 4 | 18.2 s (18.2) | 3680..3680 | 1821..3620 (1800) | 9 | 0 | 2820 | 3620 | 0 | 0 | 60 | 0.3 | 10 s |
| 5 | 8.6 s (8.6) | 1760..1760 | 1..1600 (1600) | 8 | 0 | 800 | 1600 | 0 | 0 | 160 | 0.8 | 10 s |
| 6 | 8.2 s (8.2) | 1680..1680 | 1..1600 (1600) | 8 | 0 | 800 | 1600 | 0 | 0 | 80 | 0.4 | 10 s |
| 7 | 13.1 s (13.1) | 2640..2640 | 801..2600 (1800) | 9 | 0 | 1800 | 2600 | 0 | 0 | 40 | 0.2 | 10 s |
| 8 | 7.0 s (7.0) | 1440..1440 | 1..1420 (1420) | 8 | 0 | 820 | 1420 | 0 | 0 | 20 | 0.1 | 10 s |

### Series C: 50 events/s, `pollTimeout` 25 (the default)

| trial | kill after (planned) | emitted | on disk | files | torn | hub before | hub after | dup | gaps | lost | lost s | drain |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 1 | 17.6 s (17.6) | 885..885 | 1..870 (870) | 10 | 0 | 0 | 870 | 0 | 0 | 15 | 0.3 | 26 s |
| 2 | 24.0 s (24.0) | 1210..1210 | 1..1205 (1205) | 11 | 0 | 985 | 1205 | 0 | 0 | 5 | 0.1 | 50 s |
| 3 | 24.6 s (24.6) | 1240..1240 | 1..1205 (1205) | 11 | 0 | 985 | 1205 | 0 | 0 | 35 | 0.7 | 50 s |
| 4 | 33.4 s (33.4) | 1680..1680 | 1..1650 (1650) | 15 | 0 | 990 | 1650 | 0 | 0 | 30 | 0.6 | 50 s |

At the default `pollTimeout` the outbox holds more between acks (a 25 s hold is 25 s of
batches, delivered or not), so the disk range is wider: a kill inside the first hold (trial
1) leaves the whole run on disk and nothing at the hub. A run that fits one poll is applied
as that poll arrives and acked by the response that ends its hold, one cycle (26 s in the
drain column); a run that needs two polls is applied across two and acked at the end of the
second (50 s). The outcome is the same.

## What the numbers say

1. **Nothing written to the outbox was lost, and nothing was torn, in these trials.**
   Twenty kills, up to fifteen records on disk at a time, several written per
   second: every file parsed, every event in them reached the hub. The outbox writes each
   record with `OpenFile`, `FPrint`, `CloseFile` in one script call; once `CloseFile`
   returns the bytes are in the OS page cache, which a process kill does not touch. A kill
   landing inside that one call would leave an empty or partial file, and the events of
   that batch would be gone: `FlushEvents` has already taken them out of the buffer, and
   `VyshkaOutbox.LoadRecord` discards and counts the unreadable record on the next boot.
   That path exists, and no kill in this run landed in it; how long the call takes, and so
   how wide the window is, was not timed. The plugin documentation therefore says what was
   measured and names that path, rather than promising that a crash can only ever lose the
   buffer.
2. **The loss was the event buffer's tail, and its size follows the flush rule plus one
   tick.** The buffer flushes on the plugin's 200 ms tick once 2 s have passed since its
   oldest event or 200 events are pending (`VyshkaEventBuffer`). The largest loss at 50/s,
   105 events (2.1 s), is a kill that landed just before the tick that would
   have flushed a 2 s old buffer; the smallest is a kill just after a flush. At 200/s the
   count rule dominates and the largest loss was 200 events (1.0 s). So
   under load the tail is `min(2 s, 200 events)` plus one tick, and a rate above
   100 events/s lowers it in seconds. That is the bound for a script thread that keeps
   ticking; a server whose script thread has stalled holds its pending events for as long
   as the stall, and a crash then takes them all. The tick is a call-queue timer, not a
   deadline.
3. **A batch delivered but unacked when the server dies is not stored twice.** In
   nineteen of the twenty trials the hub already held part of the run at the kill
   while the same batches were still on disk: the hub applies a poll's envelopes before it
   holds the request (`hub/poll.go`), so a batch is stored the moment it is sent and stays
   on disk until the response that ends the hold carries the ack. The restart re-sent those
   batches with their original ids in the new session and the hub dropped them as
   duplicates (spec section 8.1; the store's claim is keyed on server and envelope id, not
   on the session): rows equal distinct in every trial, and the second boot's log reads
   `N unacked envelope(s) restored from disk` followed by a clean session.
4. **The script log and the marker file agree with each other.** The marker carried the
   last announced `n` in every trial, and the confirmed line carried the same value: no
   kill landed between a tick's announcement and its confirmation, so every loss below is a
   single number rather than an interval. `Print` lines reach the log file's bytes on disk promptly, and a file rewritten every
   tick through `OpenFile`/`CloseFile` shows the same value. Neither alone says exactly how
   far a killed script got, because a kill inside a tick lands between the announcement
   and the confirmation; the two together bound it to one tick, which is why the loss is
   reported as an interval.
5. **A backlog drains at the per-poll budget per poll cycle.** Series B shows it: at 200
   events/s the outbox grew past what one poll carries (1000 events, `EVENTS_PER_POLL`),
   and the hub held each poll for the full `pollTimeout` even though the plugin had more
   queued behind the budget, because the hub answers early only when it has something to
   deliver. So a backlog of `E` queued events, whatever runs they belong to, takes about
   `E / 1000` poll cycles, subject to how the batches pack: each poll's batch is applied
   as the poll arrives and acked by the response that ends its hold, so the last batch is
   applied at the start of the last cycle and acked at its end. At `pollTimeout` 25 that
   is 40 events/s sustained. The drain column in series B and C is that arithmetic on the
   killed run's own events plus the boot's start event and manifest (two polls at
   `pollTimeout` 5 for 2000 events in B2, 10 s; one at 25 for 870 events in C1, 26 s).
   Core telemetry on a real server is orders of magnitude below 40 events/s, so
   this is a throughput bound to know about, not a durability hole, and it is out of this
   spike's scope; a poll request that says "I have more" and is answered at once would
   lift it, which is a protocol change and has its own issue (#91).

## What was not measured

- **Power loss and OS crash.** Both lose the page cache. NTFS on Windows writes dirty pages
  back within seconds, but script exposes no fsync (`ensystem.c` declares none) so the
  plugin cannot shorten that window. The honest statement is that a process kill did not
  lose a finished record in these trials and can lose the one being written, and a power
  loss can lose the last few seconds of outbox records plus the same event-buffer tail. An operator who needs less runs the game server on a machine
  with a battery and a journaling file system, which is the same advice the game's own
  persistence gets.
- **A kill during the outbox write itself.** The `OpenFile` to `CloseFile` span is one
  synchronous script call; twenty random kills did not land in it, and its width was
  not timed. The load path handles the empty or partial file it would leave, and the batch
  in it is lost (finding 1).
- **Disk full or a locked file.** Not a crash question. `WriteAll` reports only a file that
  could not be opened (`OpenFile` returning zero), and the outbox then logs that the
  envelope will be lost on restart; `FPrint` and `CloseFile` return nothing, so a write
  that fails after the open is not detected by the plugin and the record is found
  unreadable on the next boot instead.

## Consequences

- The plugin README's "Events" and "Durability" paragraphs and the outbox's header comment
  now state the measured window and name the power-loss case as unmeasured.
- No plugin or hub code changed. Lowering the 2 s flush would cut the window in proportion
  and cost one outbox file per flush; at the event rates a DayZ server produces, 2 s is
  fine, and an operator who wants less can ask for a knob later.
- The drain-rate bound (finding 5) is filed as issue #91.

Behavior can change between game patches. Re-run the spike (see `../README.md`) when the
plugin is validated against a new major DayZ version.
