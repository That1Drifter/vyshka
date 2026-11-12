# Spike: hub capacity on a small VPS

Resolves issue #87. The design notes estimated, without measurement, that "one hub on a 2 GB
VPS should comfortably handle ~20 game servers at 100 players each". This spike drives a
real built hub with synthetic plugins and synthetic admin clients until it misses a stated
latency or ingest bound, and records where that happens and why. Date: 2026-11-11.

**Short answer.** The hub measured is `main` at `0e3a163`, with its rate limits on, the
hub an operator gets. Conditions:
- SQLite, confined to 1 vCPU and 2 GB;
- the database on an NVMe-backed ext4 disk;
- loopback with no TLS, inside a WSL VM on a desktop with no other workload running;
- an empty database at the start of every run.

Every criterion held in every run up to:

- **200 game servers of 100 players under the realistic profile** (an assumed mix of 1.2
  events/s and 1 action a minute per server): 3 of 3 runs at 150 and 3 of 3 at 200. On the
  hub before the limits, 250 failed 4 of 4 on the hub's connection cap.
- **50 servers under the busy profile** (an assumed mix of 12.3 events/s and 6 actions a
  minute per server): 3 of 3 runs at 25 and 3 of 3 at 50.

**The admin server list fails first.** The three busy 75-server runs failed on the server
list alone (p99 1.3 to 1.7 s against 1 s) while every other bound held, and by the
quiet-disk rule that step is undetermined. The list costs three database reads per server,
all on SQLite's single connection.

CPU averaged at most 19 % of the quota and was never throttled, and the hub's own memory
peaked at 51 MiB of the 2 GB. No request was refused by the rate limits inside a measured
window (setup, before the window, was refused and waited in 9 of the 16 runs).

**The disk is the first thing to size.** At the default retention a realistic 100-player
server holds about 4.7 GiB of database after 90 days and 5.5 GiB after a year, and keeps
growing; a busy one holds about 39 GiB and 44 GiB. A 40 GB disk therefore holds about 7.8
realistic servers at 90 days and 6.8 after a year, and an 80 GB disk 15.7 and 13.6. That is
below the old "about 20" estimate, and far below what the hub's latency allows. The
database grows by about 127 MiB a day per realistic server over its first 30 days, and
about 1.19 GiB a day per busy one. Retention is not configurable on the hub today (no flag
and no config-file key), so the disk has to be sized for it.

How this differs from the earlier rounds:
- **Round two** measured the hub before the limits, at `6910458`, and put the busy figure
  at 25. Its 50-server failures came on a noisy disk: in-run sync p90 of 12.1 and 61.9 ms.
  On a quiet disk all three 50-server runs here passed (2.4 to 3.2 ms).
- **Round one's** "75 busy servers" passed only because five admin reads were graded as
  one pool.
- **"A second vCPU does not raise it"** is withdrawn: round two's 2 vCPU runs passed 1 of 3
  at 75 and 1 of 3 at 100, which with that disk's spread settles nothing.

## Layout

| Path | What it is |
|---|---|
| `loadgen/main.go` | The load generator. HTTP only: it does not import `hub/`, so the numbers hold for the wire. |
| `run.sh` | The ramp runner: per step, a fresh hub and database in a confined transient systemd unit, the generator pinned to other CPUs, the report and the hub's warnings kept. |
| `split-hub-log.py` | Called by `run.sh`: splits the hub's non-INFO log lines into those inside the measured window and the setup and teardown ones. |
| `fsync-sampler.sh` | Samples synced-write latency on the database's disk every 5 s while a ramp runs. |
| `conn-sampler.sh` | Samples the hub's established connections and its listening socket's accept queue every 2 s. |
| `fsync-by-run.py`, `idle-sync.py`, `conns-by-run.py` | Match the samplers' lines to each run's window, or to the idle gap before it. |
| `pragmas/main.go` | Prints the SQLite durability settings the hub runs with (same driver, same pragmas). |
| `dbsize/main.go` | Breaks a kept database down by table, indexes included. |
| `growth.py` | Turns the measured composition into per-server disk figures at the default retention. |
| `table.py`, `rates.py` | Print the results tables and the per-credential rate table from the JSON reports. |
| `results/round3/` | **The headline runs**: `main` at `0e3a163`, rate limits on (below). |
| `results/round2/` | Measured at `6910458`, before the rate limits, with the same grading as round three except the scheduled-against-completed criterion. Kept as the record, and the source of the 2 vCPU, 100-server and 250-server findings. |
| `results/round1/` | Measured at `b367acb` with earlier generator builds and pooled admin-read grading. Kept as the record and not counted in the figures. |

| Series | What it holds |
|---|---|
| `round3/busy` | 1 vCPU at 25, 50, 75, three runs each, 600 s windows. |
| `round3/realistic` | 1 vCPU at 150 and 200, three runs each, 180 s windows. |
| `round3/sanity` | 2 busy servers with `-verify`: sent against stored. |
| `round3/samplers` | Synced-write samples, the idle gaps, and the connection samples, beside every run. |
| `round2/busy` | 1 vCPU at 25, 50, 75 (three runs each) and 100 (five); 2 vCPU at 75 and 100 (three each). |
| `round2/realistic` | 1 vCPU at 100 and 150 (three each), 200 and 250 (four each). |
| `round2/tmpfs`, `round2/sanity`, `round2/samplers` | The tmpfs 200-server bracket with throttle counters, the `-verify` check, and the samplers. |
| `round1/*` | Round one's series as they were, with `growth/dbsize.txt` and `tmpfs-busy-3/dbsize.txt`, the composition breakdowns the growth figures use. |

## Reproducing

On Linux with systemd 255 or later and cgroup v2, as root. Build both binaries for Linux,
under a binary name of their own (never stop processes named `vyshka-hub` on a shared box):

```
GOOS=linux CGO_ENABLED=0 go build -o bin/vyshka-capacity-hub ./hub/cmd/vyshka-hub
GOOS=linux CGO_ENABLED=0 go build -o bin/capacity-loadgen ./spikes/hub-capacity/loadgen
taskset -c 0-11 spikes/hub-capacity/fsync-sampler.sh /var/tmp > fsync-samples.log &
taskset -c 0-11 spikes/hub-capacity/conn-sampler.sh 18787 > conns.log &
for round in 1 2 3; do
  for step in "busy 25 600s" "realistic 150 180s" "busy 50 600s" "realistic 200 180s" "busy 75 600s"; do
    set -- $step
    sleep 30   # the idle gap: the sampler reads the disk with no hub running
    RUNS=1 RUN_START=$round DURATION=$3 spikes/hub-capacity/run.sh bin results/$1 1 $1 100 $2
  done
done
RUNS=1 WARMUP=30s DURATION=60s EXTRA=-verify spikes/hub-capacity/run.sh bin results/sanity 1 busy 100 2
python3 spikes/hub-capacity/table.py results/busy/*.json
python3 spikes/hub-capacity/fsync-by-run.py fsync-samples.log results/*/*.json
```

That is round three's sequence. The batch runner logged each idle gap (`idle-*.log`) so
`idle-sync.py` can read the disk inside it. `run.sh` takes the CPU count, the profile,
players per server, and one or more server counts. Each step runs `RUNS` times (default 2)
with a 60 s warm-up and the measured window (`DURATION`, default 180 s), on a fresh hub and
database; its header lists the other knobs. The hub runs with its defaults, rate limits
included. Setup (creating servers, minting one token per admin client) goes through the
bootstrap token and waits out the hub's `Retry-After` when the limit refuses it, outside
the measured window.

On WSL, keep a `wsl.exe` session attached for the whole ramp: WSL powers the distribution
off shortly after the last session detaches, and a systemd unit alone does not keep it up.

## Environment

| Item | Value |
|---|---|
| Host | AMD Ryzen 7 9800X3D (8 cores, 16 threads), 32 GB, Windows 11 Pro 10.0.26200 |
| Linux | WSL 2, Ubuntu 24.04.3 LTS, kernel 6.18.33.2-microsoft-standard-WSL2, systemd 255, cgroup v2; 16 vCPUs and 30 GB visible to the VM |
| Database disk | `/var/tmp` on the distribution's ext4 root (`/dev/sde`), a virtual disk file on the host's C: drive, a WD Blue SN580 NVMe SSD. Not `/mnt/c`. |
| Host load | Round three: nothing else ran on the host. Round two: other agents' Docker and Go work, with the C: drive at about 1800 writes/s. |
| Hub, round three | `main` at `0e3a163` (this branch adds no hub code), with its rate limits at their defaults: 5 requests/s with a burst of 60 per server credential, 10/s with a burst of 60 per admin token. Built with Go 1.26.4 for linux/amd64, cgo off, as `vyshka-capacity-hub`; `serve -addr 127.0.0.1:18787 -panel=false`, SQLite, every retention default, metrics and auto-TLS off, log level info to a file. |
| Hub, round two | the same recipe with hub code at `6910458` (after #82 and #85, before #84 config and #86 rate limits): no rate limits |
| Hub, round one | the same recipe at `b367acb` |
| SQLite | `modernc.org/sqlite` 1.56.0; `journal_mode=wal`, `synchronous=2` (FULL: every commit syncs the WAL), `wal_autocheckpoint=1000`, 4 KiB pages, one connection (`SetMaxOpenConns(1)`), as `pragmas/main.go` prints them |
| Confinement | a transient unit per run: `systemd-run -p CPUQuota=100% -p AllowedCPUs=14` (1 vCPU) or `CPUQuota=200% AllowedCPUs=14-15` (2 vCPU, round two), `-p MemoryMax=2G -p MemorySwapMax=0`. Go 1.26 sizes `GOMAXPROCS` from the quota. |
| Generator | `taskset -c 0-11`, so it never shares a CPU with the hub; CPUs 12 and 13 idle |
| Network | loopback, plain HTTP, no TLS proxy in front |

## The synthetic load

**A server** enrolls, opens a session at `pollTimeout` 25 s, publishes a manifest of 28
actions on its first poll, and then does what `plugins/dayz/README.md` ("Telemetry")
describes the DayZ plugin doing:

- one poll in flight, re-polled at once after every answer, framed at most 200 envelopes
  and 1000 events, `more: true` when it left some behind;
- events buffered and flushed as one `event.batch` every 2 s or at 200 events;
- `state.players` (one entry per player, shaped like the README's example, about 190
  bytes each), `state.vehicles` (50 vehicles, about 11.5 KB: the README measured 52
  vehicles at 11 464 bytes on a live run), and `state.world`, captured as a poll is built,
  at most every 10 s and never while the previous capture is unacked;
- a `core.server.fps` sample every 60 s (the plugin's default);
- an `action.ack` and an `action.result` on the poll after a dispatch arrives.

Each synthetic server owns one HTTP connection to the hub (its own transport, at most one
connection), as a game server's one HTTP context does.

**Event rates** are an assumed mix, per player per hour:

| Event | realistic | busy | Where the figure comes from |
|---|---|---|---|
| `core.player.damage` | 30 | 400 | The README: "an infected fight is a hit every second or two, and a firefight more". Busy is a fifth of the population in a fight at a hit every 1.8 s; realistic is a few fights per player-hour. |
| `core.player.death` | 1.5 | 6 | Assumed: a survival server's lifespan of tens of minutes to hours. |
| `core.player.chat` | 6 | 20 | Assumed. |
| `core.player.connect` / `disconnect` | 1 / 1 | 2 / 2 | Assumed: a session of an hour or two, one or two reconnects. |
| `vyshka.vehicle.enter` / `exit` | 2 / 2 | 6 / 6 | Assumed. |
| total | 43.5 | 442 | 1.2 events/s per 100-player server realistic, 12.3 busy |

Only the damage cadence comes from a measurement. No populated server has run the plugin
against a hub yet (the staging DayZ server is public but nearly empty), so the mix is an
assumption, to be replaced when one does. The busy profile is meant as a heavy evening,
not an average.

**Admin load** is shaped on the panel (`panel/static/app.js`). There are max(2,
servers/10) admin clients, each with its own `admin`-scope token and its own HTTP transport
of at most six connections, as a browser has. Each client:

- re-reads the server list, one server's event feed (`limit=100`), and that server's
  `state.players`, `state.vehicles` and `state.world` every 5 s, moving to another server
  every minute;
- dispatches actions at 1 per server per minute (realistic) or 6 (busy), Poisson arrivals,
  each from a random client;
- reads each dispatched action's record every second until it is final, as the panel's
  action form does.

Every admin request has a 10 s deadline. One webhook subscribed to `core.player.death`
delivers to a sink inside the generator.

## Failure criteria

Checked by the generator over the measured window:

| Criterion | Limit | Why that limit |
|---|---|---|
| Action queue-to-delivery p99 (dispatch request sent to envelope in the plugin's hands) | 2 s | A held poll is answered as soon as work is queued; a healthy hub delivers in milliseconds, and an admin clicking a button expects it to happen now. |
| Held poll overhead p99 (round trip of a poll held to term, minus `pollTimeout`) | 1 s | The hub ingests a poll before it starts the hold, so the excess is ingest plus queueing. The DayZ client's read timeout is `pollTimeout` + 5 s; one second keeps four in hand. |
| Admin read p99, **each of the five routes on its own**: server list, event page, `state.players`, `state.vehicles`, `state.world` | 1 s | The panel re-reads every 5 s. |
| Non-2xx, transport error or timeout rate, **a 429 included** | 0.1 % | Any 5xx, 409, 429, dropped connection or expired deadline under a legitimate load is a fault. The rate limits refusing this load would be one too. |
| **Scheduled admin requests not completed** (round three): every read due on each client's 5 s schedule and every dispatch due on the Poisson schedule, less those the run's own end cut off | 0 | A read that times out drops out of its route's p99, so the p99 alone cannot see it. Each report also prints, per request kind, scheduled, started, completed, cut off, missing, and timeouts (`requests` in the JSON). |
| Ingest backlog at the window's end, as a multiple of one poll cycle of events (27 s) | 2x | A backlog that grows instead of draining. |
| Webhook delivery p99 (poll carrying the death sent to the sink's receipt) | 10 s | Measured from the hub's receipt rather than the game's emission, which includes the plugin's own 2 s buffer and up to 25 s hold. |
| Hub anonymous memory peak | 80 % of 2 GiB | Page cache is excluded: the kernel reclaims it under the limit. |
| OOM kills | 0 | |

A latency criterion with no samples fails ("none returned") rather than reading "n/a".

What changed between rounds:
- the admin reads graded per route, admin deadlines, no-samples-fails, one transport per
  client, and the 600 s busy window arrived in round two;
- the scheduled-against-completed criterion and the setup that waits out a 429 arrived in
  round three.

The memory criterion changed once, before round one's counted runs: the exploratory runs
graded `memory.current`, which counts page cache, and every later run grades anonymous
memory.

**What a p99 selects.** A p99 over n samples is the ceil(0.01 n)-th largest. Under 100
samples it is the maximum; at 270 to 273 samples (busy, 50 servers) it is the third
largest; at 840 (the busy 75-server server list) the ninth. The sample count sits beside
every p99 in the tables. Only polls held to full term measure ingest overhead, and under
the busy profile dispatches end most holds early. So the busy window is 600 s, rather than
grading the early polls, whose round trip mixes an unknown part of the hold with ingest.

**Quiet disk.** A run counts as on a quiet disk when the sync sampler's p90 over the run's
window is 3 ms or less, the same statistic for passes and fails. The sampler's synced
writes share the disk with the hub's, so the reading includes whatever the hub's own
writes do to the disk's latency. For that reason every round-three run also has a reading
from a 30 s gap before it with no hub running ("idle" below).

## Results

Round three: `main` at `0e3a163`, rate limits on, 1 vCPU, the database on the ext4 disk,
100 players per server.

### Summary per step

| Profile | Servers | Runs passed | In-run sync p90, each run | Idle max before each run | What failed |
|---|---|---|---|---|---|
| busy | 25 | 3 of 3 | 1.4, 1.5, 1.6 ms | 1.4, 1.5, 2.1 ms | |
| busy | **50** | **3 of 3** | 2.4, 3.2, 2.7 ms | 1.3, 2.4, 1.3 ms | |
| busy | 75 | 0 of 3 | 8.6, 9.6, 9.1 ms | 3.9, 3.7, 2.5 ms | the server list alone in all three (p99 1688, 1316, 1475 ms over 840 samples); delivery, poll overhead, the other four reads, webhooks, and the scheduled-request check all held |
| realistic | 150 | 3 of 3 | 2.2, 2.7, 2.5 ms | 1.4, 1.3, 1.6 ms | |
| realistic | **200** | **3 of 3** | 1.6, 7.9, 8.1 ms | 1.3, 1.4, 1.5 ms | |

At the headline steps, 50 busy servers offer 614 events/s and 5 dispatches/s in all; 200
realistic servers offer 242 events/s and 3.3 dispatches/s. The most this hub was measured
to ingest on `main` is the 75-server busy step: about 920 events/s in all, with every
ingest bound held in 3 of 3 runs (delivery p99 at most 266 ms, held-poll overhead at most
245 ms) while the admin server list alone failed. That is the figure the rate-limit notes
in `deploy/README.md`, `spec/protocol.md` section 2.2.1, and `hub/ratelimit.go` quote.

**Busy 50.** All three runs passed: two with a quiet in-run reading (2.4 and 2.7 ms), one
at 3.2 ms, just over the rule. Round two's two 50-server failures had in-run p90s of 12.1
and 61.9 ms on a shared host. They followed the disk, and the headline moves from 25 to 50.

**Busy 75 is undetermined by the quiet rule.** No 75-server run read 3 ms or less in-run:
they read 8.6 to 9.6 ms. Each began on a quiet disk by the idle-gap reading, with nothing
else running on the host. The data cannot say whether the hub's own write load at 75
servers raised the sampler's reading or something else did. What it does show is the same
in every run: the server list failed, and nothing else did. The busiest step that passed
is 50; 75 failed on the list in every run.

**Realistic 200.** All three passed, two of them with in-run p90s of 7.9 and 8.1 ms, over
the rule. The realistic profile writes a tenth as much as the busy one and its server list
stayed under 0.25 s at 200. The step above it, 250, was measured only before the limits
(round two) and failed every run on the connection cap (finding 3).

**No other hub errors.** In every round-three run the hub logged nothing at warning or
error level inside the window: no request failures, no dropped audit records, and no
sweep failures (finding 7).

### Disk state per run

`fsync-by-run.py` over each run's window and `idle-sync.py` over the 30 s gap before it
are in `results/round3/samplers`. The table above carries both readings for every run.

### Every run

Generated with `table.py`. Each p99 is followed by its sample count. "poll ovh" is a held
poll's round trip minus the 25 s hold, "list" the server list, "other reads" the worst
of the other four routes, "hook" a webhook from the poll that carried the death to the
sink, "anon peak" the hub's anonymous memory (page cache excluded), "db MiB/h" the main
database file's growth over the window (the WAL stayed at 4 to 5 MiB).

**`round3/busy`**

| run | database | verdict | tripped | deliver p99 | poll ovh p99 | list p99 | other reads p99 (worst) | hook p99 | cpu mean | anon peak | db MiB/h |
|---|---|---|---|---|---|---|---|---|---|---|---|
| 1cpu-busy-100p-25s-r1 | ext4 disk | PASS | none | 27 ms (1462) | 45 ms (131) | 27 ms (240) | 6 ms (240) | 57 ms (2504) | 4% | 17 MiB | 1422 |
| 1cpu-busy-100p-25s-r2 | ext4 disk | PASS | none | 42 ms (1462) | 87 ms (132) | 24 ms (240) | 17 ms (240) | 83 ms (2510) | 4% | 17 MiB | 1428 |
| 1cpu-busy-100p-25s-r3 | ext4 disk | PASS | none | 54 ms (1462) | 53 ms (131) | 35 ms (240) | 13 ms (240) | 112 ms (2502) | 4% | 17 MiB | 1432 |
| 1cpu-busy-100p-50s-r1 | ext4 disk | PASS | none | 71 ms (2925) | 85 ms (271) | 72 ms (600) | 14 ms (600) | 139 ms (5152) | 9% | 22 MiB | 2843 |
| 1cpu-busy-100p-50s-r2 | ext4 disk | PASS | none | 126 ms (2925) | 132 ms (273) | 133 ms (600) | 15 ms (600) | 294 ms (5126) | 9% | 20 MiB | 2846 |
| 1cpu-busy-100p-50s-r3 | ext4 disk | PASS | none | 106 ms (2925) | 110 ms (270) | 133 ms (600) | 24 ms (600) | 176 ms (5137) | 9% | 20 MiB | 2835 |
| 1cpu-busy-100p-75s-r1 | ext4 disk | FAIL | admin read: servers list | 238 ms (4479) | 190 ms (392) | 1688 ms (840) | 39 ms (840) | 381 ms (7601) | 14% | 26 MiB | 4279 |
| 1cpu-busy-100p-75s-r2 | ext4 disk | FAIL | admin read: servers list | 232 ms (4479) | 245 ms (390) | 1316 ms (840) | 45 ms (840) | 375 ms (7577) | 14% | 26 MiB | 4276 |
| 1cpu-busy-100p-75s-r3 | ext4 disk | FAIL | admin read: servers list | 266 ms (4479) | 184 ms (389) | 1475 ms (840) | 61 ms (840) | 389 ms (7596) | 14% | 28 MiB | 4281 |

**`round3/realistic`**

| run | database | verdict | tripped | deliver p99 | poll ovh p99 | list p99 | other reads p99 (worst) | hook p99 | cpu mean | anon peak | db MiB/h |
|---|---|---|---|---|---|---|---|---|---|---|---|
| 1cpu-realistic-100p-150s-r1 | ext4 disk | PASS | none | 35 ms (426) | 45 ms (892) | 56 ms (540) | 14 ms (540) | 82 ms (1116) | 13% | 33 MiB | 1390 |
| 1cpu-realistic-100p-150s-r2 | ext4 disk | PASS | none | 76 ms (426) | 63 ms (892) | 50 ms (540) | 16 ms (540) | 86 ms (1109) | 12% | 41 MiB | 1389 |
| 1cpu-realistic-100p-150s-r3 | ext4 disk | PASS | none | 57 ms (426) | 55 ms (892) | 72 ms (540) | 15 ms (540) | 72 ms (1118) | 13% | 36 MiB | 1399 |
| 1cpu-realistic-100p-200s-r1 | ext4 disk | PASS | none | 91 ms (578) | 83 ms (1166) | 137 ms (720) | 32 ms (720) | 130 ms (1561) | 18% | 42 MiB | 1866 |
| 1cpu-realistic-100p-200s-r2 | ext4 disk | PASS | none | 125 ms (579) | 91 ms (1167) | 176 ms (720) | 22 ms (721) | 185 ms (1568) | 18% | 50 MiB | 1878 |
| 1cpu-realistic-100p-200s-r3 | ext4 disk | PASS | none | 97 ms (578) | 81 ms (1168) | 221 ms (720) | 27 ms (720) | 121 ms (1555) | 19% | 47 MiB | 1870 |

**`round3/sanity`**

| run | database | verdict | tripped | deliver p99 | poll ovh p99 | list p99 | other reads p99 (worst) | hook p99 | cpu mean | anon peak | db MiB/h |
|---|---|---|---|---|---|---|---|---|---|---|---|
| 1cpu-busy-100p-2s-r1 | ext4 disk | PASS | none | 5 ms (11) | 5 ms (2) | 3 ms (24) | 5 ms (24) | 31 ms (21) | 1% | 15 MiB | 150 |

Every round-three run's `requests` block shows nothing missing: every scheduled read and
dispatch completed, and no request timed out.

### Round two: before the rate limits

Round two measured hub code at `6910458`, before #84 and #86, with the same generator
grading except the scheduled-against-completed criterion. Its step results were:

| vCPU | Profile | Servers | Runs passed | In-run sync p90, failing runs | What failed |
|---|---|---|---|---|---|
| 1 | busy | 25 | 3 of 3 | | |
| 1 | busy | 50 | 1 of 3 | 12.1, 61.9 ms | server list, delivery and poll overhead in both; every route in one |
| 1 | busy | 75 | 0 of 3 | 11.4, 41.5, 32.6 ms | server list in all three, alone in one |
| 1 | busy | 100 | 0 of 5 | 12.7, 43.9, 8.2, 11.6, 11.4 ms | server list in all five, alone in the last two |
| 2 | busy | 75 | 1 of 3 | 6.5, 10.7 ms | server list alone, both |
| 2 | busy | 100 | 1 of 3 | 14.0, 12.1 ms | server list in both, alone in one |
| 1 | realistic | 100 | 3 of 3 | | |
| 1 | realistic | 150 | 3 of 3 | | |
| 1 | realistic | 200 | 3 of 4 | 21.2 ms | server list, delivery, poll overhead |
| 1 | realistic | 250 | 0 of 4 | 11.8, 2.7, 2.8, 2.1 ms | the connection cap (finding 3) |
| 1 | busy, database on tmpfs | 200 | 1 of 1 | (no disk) | |

The host was shared with other agents' work throughout round two. Across its busy runs
from 50 to 100, every run whose in-run p90 read 4.2 ms or less passed (1 vCPU 50 r1 at 2.9,
2 vCPU 75 r3 at 4.2, 2 vCPU 100 r2 at 3.4), and every one that read 6.4 ms or more failed:
pass and fail split on the disk's latency, not on server count.

The busy 100-server step was measured only in round two and none of its runs met the quiet
rule, so it is undetermined. The 2 vCPU steps are in the same state: they passed 1 of 3 at
both 75 and 100, against 0 of 3 and 0 of 5 on 1 vCPU, and this write-up claims nothing about
a second vCPU. Round two's tables, samplers and per-run warnings are in `results/round2/`,
and `table.py` prints them.

### Findings

1. **The binding resource is SQLite's one connection, and the server list is what spends
   it.**
   - **Not CPU or memory.** On disk, CPU averaged at most 19 % of the 1 vCPU quota in
     round three (23 % in round two), and the hub's anonymous memory peaked at 50.6 MiB.
   - **Throttling.** The cgroup counted no throttled period in any round-three run. In
     round two it counted one in about 6000 periods in each of two runs, busy 50 r2 and
     75 r2, and none in the rest.
   - **Why the disk matters.** Every request, read or write, waits for the one connection,
     and every commit waits for a synced WAL write. Under the busy profile, pass and fail
     followed the disk's sync latency (round two), and on a quiet host the server list
     was what failed at 75 (round three).
2. **`GET /api/v1/servers` makes three store calls per server on top of the list query**
   (`LiveSession`, `PendingEnvelopeCount`, `ManifestCapabilities` in `handleListServers`).
   That is 226 queries at 75 servers, all on the one connection and queued behind ingest.
   - It failed in every failing run below the connection cap in rounds two and three: 18
     runs, alone in 9.
   - At 75 busy servers on a quiet host its p99 was 1.3 to 1.7 s while every other route
     stayed under 65 ms.
   - Round one saw the same: 2.7 s at 75 busy servers (`round1/exploratory-diag-disk`).

   A single joined query, or the counts cached per poll, would take it off the critical
   path. This spike changes no hub code; the finding is for whoever takes it.
3. **The connection cap: 256 connections, less every other open connection.**
   `Config.MaxConns` defaults to 256 (`hub/server.go`), and neither `vyshka-hub serve` nor
   the config file has a setting for it. Every connected plugin keeps one connection,
   every admin client holds a few, and connections past the cap wait unaccepted in the
   kernel's backlog.
   - **How many servers that leaves.** At 200 realistic servers the hub held 226 to 232
     established connections (200 plugins and the rest for 20 admin clients), with an
     empty accept queue. At that admin share the cap is reached near 225 servers.
   - **What it did at 250** (round two). There were 306 established connections and a
     median of 395 unaccepted (627 at most). Every plugin held its poll, the admin
     clients' requests timed out (about 21 % of requests failed, 541 to 552 of some 705
     dispatches per run), and the hub's own latency stayed in bounds on a quiet disk.
   - **Where it bites.** The cap fell between 200 and 250 servers in these runs, and
     nearer 225 at their admin share. A real deployment has more holding connections: a
     reverse proxy's upstream connections, and a plugin that opens a second HTTP context
     (the DayZ plugin's key/value calls) both count. Round one's 400-server runs on tmpfs,
     where 138 of 400 servers never polled, are the cap seen from the plugin side.
   - **What the hub logs.** When plugins are starved it logs one `server link lost`
     warning per starved server (`hub/internal/webhook/dispatch.go`). When admin clients
     are starved it logs the requests it accepted only after the client had given up, as
     `request failed ... context canceled`. Nothing names the cap.

   Until the cap is configurable, it is the real trigger for the parked multi-instance
   hub.
4. **Every held poll re-reads the database once a second** (`pollBackstop` in
   `hub/poll.go`): a session lookup and an outbound read per held poll per second, so 100
   servers cost about 200 queries a second on the one connection before any telemetry
   arrives. It is cheap on its own (25 busy servers ran at 4 % CPU), but it grows with
   servers rather than with activity.
5. **Without the disk, nothing here reached the CPU limit.** In round two the database on
   tmpfs, 1 vCPU, 200 busy servers passed at 34 % mean CPU with zero throttled periods.
   Round one failed the same step on a held-poll p99 of 1058 ms over 201 samples, at a 1 s
   p90 of 43.7 % CPU, and explained it as quota throttling. No counter was sampled then,
   and round two's counters do not support that, so the explanation is withdrawn. These
   are single runs that bracket the step, not a measured ceiling.
6. **Disk space binds before any of this** (next section).
7. **Under load on a noisy disk, the hub dropped audit records for writes it performed.**
   `recordAudit` (`hub/auth.go`) gives the audit write 5 s to get SQLite's one connection.
   In round two, on the shared host's noisy disk, that ran out four times. Each time the
   dispatch the record was for had already been applied, so the hub logged
   `audit record could not be written` (`begin audit record: context deadline exceeded`)
   and dropped the record. The action-expiry sweep, also on a 5 s budget, failed once
   (`action expiry sweep failed`).

   | Run (round two) | Audit records dropped | Expiry sweep failures |
   |---|---|---|
   | busy 50 r3 | 1 | 0 |
   | busy 75 r2 | 1 | 0 |
   | busy 75 r3 | 2 | 1 |
   | every other round-two run | 0 | 0 |
   | every round-three run | 0 | 0 |

   The audit log is the record an operator reaches for after the fact, and here it is
   short exactly when the hub is under strain. A write that cannot be audited should
   either wait or fail before it applies, rather than apply and lose its record.

### Database growth

**Units.** Binary throughout: MiB and GiB. Disk sizes are named the way they are sold, in
decimal GB, and converted: 40 GB is 37.3 GiB, 80 GB is 74.5 GiB.

**What the 3-minute windows measure.** The main database file grew 9.3 to 9.4 MiB an hour
per realistic server, and 56.5 to 57.9 MiB an hour per busy one (rounds two and three).
That is not a daily rate. In those minutes the snapshot history is still filling: the hub
keeps the last 500 snapshots per server and type (`StateHistoryDepth`), reached after
about 3.3 hours at the realistic profile's capture rate and 2.6 at the busy one. After
that, snapshots stop adding to the file.

**What it is made of.** `dbsize` of round one's 20-minute realistic growth run, 100
servers (`round1/growth/dbsize.txt`), and of a busy tmpfs run
(`round1/tmpfs-busy-3/dbsize.txt`):

| Table, with its indexes | Rows (realistic run) | MiB | Retention on the hub today |
|---|---|---|---|
| `state_snapshots` | 15 756 | 146.1 | the last 500 per server and type (`StateHistoryDepth`), and 24 h |
| `events` | 152 513 | 117.2 | 30 days, `core.player.chat` 90 |
| `event_identities` | 155 628 | 45.5 | deleted with its event |
| `event_batches` (the dedup record) | 50 252 | 9.6 | with the events |
| `webhook_deliveries` | 5 115 | 6.6 | 7 days |
| `audit_records` | 2 153 | 1.6 | 365 days |
| `actions` | 2 024 | 1.1 | **never deleted** |
| `outbound_envelopes` | 2 024 | 1.1 | **never deleted once acked** (only expired, unsent dispatches are) |

An event costs 1185 bytes on disk with its identity rows and dedup record in the realistic
mix, and 1168 bytes in the busy mix, each measured, not scaled. `growth.py` turns the
breakdown into what one 100-player server adds and holds:

| Per server | Realistic | Busy |
|---|---|---|
| Events stored per day | 106 000 (1.23/s) | 1.06 million (12.3/s) |
| **Growth per day, days 1 to 30** (after snapshot history fills, before events expire) | **127 MiB** | **1219 MiB (1.19 GiB)** |
| of which events | 119.7 MiB | 1185.0 MiB |
| of which webhook deliveries (one death webhook) | 4.6 MiB | 18.6 MiB |
| of which audit records | 1.1 MiB | 6.4 MiB |
| of which actions and acked envelopes | 1.6 MiB | 9.4 MiB |
| Snapshot history, once, in the first hours | 14 MiB | 14 MiB |
| Events at retention (30 days, chat 90 days) | 4.47 GiB | 37.9 GiB |
| **Total at 90 days** | **4.75 GiB** | **39.4 GiB** |
| **Total after a year** | **5.46 GiB**, then +0.56 GiB a year | **43.6 GiB**, then +3.4 GiB a year |

The daily figure and the totals agree: 127 MiB a day for 30 days is 3.7 GiB; chat keeps
growing to day 90; then deliveries, audit and actions add theirs. Two things grow
without bound or close to it: the `actions` rows and the acknowledged
`outbound_envelopes`, which nothing deletes, and the audit records, kept 365 days. The
database never settles.

**What a disk holds.**

| Disk | Realistic 100-player servers at 90 days | After a year | Busy, at 90 days |
|---|---|---|---|
| 40 GB (37.3 GiB) | **7.8** | 6.8 | 0.9 |
| 80 GB (74.5 GiB) | **15.7** | 13.6 | 1.9 |

That is less than the old estimate of about 20, and an order of magnitude less than what
the hub's latency allows (200 realistic). The figures assume the whole disk is the
database's. The OS, backups taken with `POST /api/v1/admin/backup`, and the WAL all come
out of the same disk.

**Retention is not configurable today.** The retention rules are hub defaults.
`vyshka-hub serve` has no flag for them, and the config file (`deploy/hub.toml.example`)
has no key for them, so an operator cannot shorten them. They are what binds disk, and the
first thing a sizing decision runs into.

These are extrapolations from windows of minutes, not a 30-day run. The sweeps never
deleted anything during the runs. SQLite reuses freed pages without shrinking the file.

### Request rates per credential

The measured window opens after setup and warm-up, so enrollment and session start are not
in these figures. Over round three's passing runs (`rates.py`):

| Credential | Profile | Runs | Mean req/s | Busiest credential's mean | Busiest 1 s | Busiest 10 s, per s | Busiest 60 s, per s |
|---|---|---|---|---|---|---|---|
| server (plugin session) | realistic | 6 | 0.048 to 0.049 | 0.072 | 2 to 3 | 0.30 to 0.50 | 0.12 to 0.13 |
| server (plugin session) | busy | 6 | 0.106 to 0.107 | 0.125 to 0.133 | 3 to 4 | 0.70 | 0.27 |
| admin token (one admin client) | realistic | 6 | 1.31 to 1.32 | 1.41 to 1.43 | 8 to 9 | 2.00 to 2.50 | 1.53 |
| admin token (one admin client) | busy | 6 | 2.95 to 3.43 | 3.02 to 3.49 | 12 to 15 | 5.40 to 6.90 | 3.62 to 4.22 |

**Admin busiest second, across all runs.** In round three, failing runs included, no
admin client's busiest second passed 15. In round two, which had no limits, it was 15 in
passing runs and 24 across all runs. Round one's figures agree with these: its realistic
plugin's busiest second was 3 and its realistic admin's busiest mean 1.47.

**Where these rates come from.**
- A plugin's rate is set by the poll cycle and by dispatches: one held poll per 25 s when
  quiet, and two polls back to back when an action arrives (the early answer, then the
  one carrying its ack and result).
- An admin client's rate is mostly the dispatch model. Of a busy client's three requests
  a second, the five 5 s reads are one, and the rest are dispatches and the 1 s re-reads
  of each pending action.
- Not modeled: a plugin's backlog drain after an outage (with `more` answered at once it
  re-polls as fast as round trips allow, 1000 events a poll), session renewal (one
  request an hour), key/value calls, and scripted admin clients walking the event feed.

**The rate limits on `main` and this load.** Round three ran with the defaults on: 5
requests/s with a burst of 60 per server credential, and 10 requests/s with a burst of 60
per admin token.
- No request in any measured window was refused, by plugin or by admin client. The
  reports' failure tallies are empty, and the hub logged no rate-limit warning inside a
  window.
- Setup went over the bootstrap token's allowance (creating every server and minting
  every admin client's token), was refused, and waited out the hub's `Retry-After`,
  outside the window.
- Every admin client has a token of its own, so no step depends on one token carrying
  every dispatch.

**Dispatches and a plugin's polls.** Every dispatch ends a held poll, so dispatches to one
server make its plugin poll. `main` refunds a poll answered with the first send of work an
admin queued (`hub/poll.go`, `adminCaused` in `hub/ratelimit.go`), so the poll that
delivers a dispatch is not charged to the server. The poll that follows, carrying the
action's ack and result, is charged as the plugin's own unless it is itself answered with
newly queued work, which is what happens under sustained dispatching. The generator spreads dispatches
over all servers (0.1 a second per server at most), and no plugin was refused.

This spike's own recommendation was 2 requests/s with a burst of 30 per server, plus a
bound on events per credential (about 50 events/s with a burst of a few thousand, since 2
polls/s of 1000 events is more than this hub ingests), and 10 requests/s with a burst of 60
per admin token. Slice #86 set the server default at 5/s with a burst of 60, wider for
key/value traffic this spike did not measure.

## Runs, builds, and what changed between them

- **Round three** (`results/round3/`) used the committed generator against `main` at
  `0e3a163`, on a host with no other workload. Rounds were interleaved across steps (25,
  150, 50, 200, 75 in each round) so every step saw the same host conditions.
- **Round two** (`results/round2/`) used the generator as it stood before round three's
  changes (no scheduled-against-completed criterion, and setup that exits on a 429)
  against hub code at `6910458`, before the rate limits, on a host shared with other
  agents.
- **Round one** (`results/round1/`) came from several generator builds:
  - the exploratory runs predate per-route timing, anonymous memory, and the per-dispatch
    random source;
  - batch 1 predates the error code in the failure tally;
  - batches 2 and 3 have it but predate poll-deadline counting and the emitter's backlog
    report;
  - batch 4 has those.

  None of them has per-route grading, admin deadlines, or per-client transports, and
  their verdicts pooled the five admin reads; the review recomputed all 45 without a
  mismatch. Graded per route, four of round one's passes fail on the server list. One
  "250 realistic passed" run had an admin client whose requests never returned (4325 of
  4500 scheduled reads completed).
- **Round one's busy-disk story is unsampled.** It explained its batch-1 failures by a
  busy host disk on the strength of one 11 ms probe that left no artifact. That is
  consistent with host noise, which round two then measured, but it is not shown.
- **WSL** powers a distribution off shortly after the last `wsl.exe` session detaches.
  That killed one round-one batch twice before it was run attached; nothing from the
  killed attempts is in `results/`.
- **What `run.sh` keeps of the hub's log.** Since round two, the non-INFO lines inside the
  measured window go to `<run>.hub-warnings.log`, and the lines before and after it go to
  `<run>.hub-teardown.log` with a count. Round one kept the last 50 non-INFO lines of the
  whole log, which mixed the two.

## Sanity check: the load is real

`results/round3/sanity` ran 2 servers × 100 players on the busy profile with `-verify`,
against `main`. Once the window closed, the generator drained every outbox and counted what
the hub held through the Admin API:

```
verify {"actionsCompletedAtHub":16,"actionsDispatchedAccepted":16,"deathsEmittedByPlugins":31,
        "deathsStoredAtHub":31,"eventsEmittedByPlugins":2212,"eventsStoredAtHub":2212,
        "outboxLeftAfterDrain":0,"sinkReceiptsTotal":31,"webhookDeliveredAtHub":31,
        "webhookDeliveryRecords":31}
```

Every emitted event was stored once, every accepted dispatch completed, and every death
became one delivered webhook that the sink received.

## What was not measured

- **A real VPS.**
  - Loopback, no TLS (the hub's `-auto-tls` or a proxy), no network latency.
  - A WSL VM on a desktop. Its virtual disk sits on an NVMe drive whose synced writes
    took 1.2 to 1.4 ms with the host idle, and tens of milliseconds when the host was
    busy (round two).

  A small VPS's storage is often slower than that disk at its best. Compare with
  `dd if=/dev/zero of=probe bs=4k count=500 oflag=dsync` on the target before trusting the
  busy figure there.
- **Postgres.** Skipped. The single SQLite connection is where this hub falls over and
  Postgres removes it, so the Postgres figure is likely higher; it is not measured here.
  The connection cap applies to both.
- **A database with history.** Every run starts from an empty database. The retention
  sweeps' cost at steady state is missing, as is any slowdown of the event feed or the
  server list on large tables.
- **Real players.** The event mix is an assumption (above).
- **The plugin's store context.** The DayZ plugin opens a second HTTP context for
  key/value calls; the generator makes none, so a real plugin holds one more connection at
  times.
- **Busy 75 and 100 on a disk the rule calls quiet**, and a second vCPU, are undetermined
  (above). Realistic 250 was measured only before the rate limits.
- **WebSocket**, which the hub does not implement.
- **More than 100 players per server.** DayZ's own ceiling is 127, and every per-server
  cost here scales with players except the poll itself.

## Provenance

The load generator, the runner, and the measurements are original work written from this
repository's protocol document and the DayZ plugin's README. No third-party integration
product was consulted.
