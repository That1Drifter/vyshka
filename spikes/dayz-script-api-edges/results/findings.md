# DayZ script API edges: measurements

Five engine behaviors the plugin leans on that earlier spikes left bounded or unmeasured:
the read-timeout option above and below its documented range, the status codes the header
spike did not send, where a clothing class keeps its cargo grid, whether `FPrint` output
reaches the file before `CloseFile`, and the exact line length the file reader faults on.

**Short answers:** the read timeout is clamped to 3..120 s (200 behaves as 120, 1 as 3);
a `204` reaches script as `OnError(8)`, the code a timeout produces, and a `302` as
`OnError(7)` without the redirect being followed; clothing declares `itemsCargoSize` on the
class itself, so `<class> itemsCargoSize` reads it and `<class> Cargo itemsCargoSize` never
does; `FPrint` bytes are in the file at once and survive a kill with the handle still open;
`FGets` reads a 65 534-byte line and faults the process on 65 535 and 65 536.

## Environment

| Item | Value |
|---|---|
| Game build | DayZ 1.29.0.163709 server binary `DayZServer_x64.exe` |
| Host OS | Windows 11 Pro 26100 |
| Stub | in the runner (`runner/main.go`), Go 1.26.4, `127.0.0.1:8094`, plain HTTP over loopback |
| Harness | `harness/VyshkaEdgeProbe.c` appended to a derived `vyshkaEdgeSpike.chernarusplus` `init.c`; no mod, no client |
| Date | 2026-10-10 |

## Runs

| Run | What happened | Logs |
|---|---|---|
| 1 | Mission script did not compile: a local named `out` is a reserved word (the `out` parameter modifier) | `runner-run1-compile-fail.log` |
| 2 | Four boots: phase A, then one boot per faulting line length, then done | `runner-run2.log`, `probe-run2.log`, `stub-run2.log`, `crashes-run2.log` |

## Series 1: the read-timeout option (`SetOption(1, n)`)

Each step sets the option and then asks the stub for a response held longer than the
documented value. A value that was rejected would leave the 10 s default in force; one that
was accepted as given would let the hold complete; a clamped one fails at the clamp.

| Option | Hold | Script | Stub | Reading |
|---|---|---|---|---|
| 200 | 150 s | `OnError(8)` at 120.055 s | client gone after 120.003 s | clamped to 120, not rejected (would fail at 10 s) and not accepted (would succeed at 150 s) |
| 1 | 6 s | `OnError(8)` at 3.094 s | client gone after 3.010 s | clamped up to 3 |
| 10 | 5 s | `OnSuccess` at 5.044 s | answered after 5.000 s | control: an in-range value behaves as set |

This closes the open item in `../dayz-restapi-poll-timeout/results/findings.md` finding 5.
The plugin's own clamp in `VyshkaTransport.SetReadTimeout` (3..120) matches the engine's.

## Series 2: status codes (default read timeout)

| Status | Script | Time | Stub |
|---|---|---|---|
| 200, body `{"status":200}` | `OnSuccess`, 14 bytes | 28 ms | answered |
| 204, no body | `OnError(8)` | 76 ms | answered |
| 302, `Location: /ok` | `OnError(7)` | 29 ms | answered; `/ok` never requested |
| 403 | `OnError(5)` | 79 ms | answered |
| 404 | `OnError(5)` | 29 ms | answered |
| 429 | `OnError(5)` | 79 ms | answered |
| 502 | `OnError(6)` | 31 ms | answered |
| 503 | `OnError(6)` | 85 ms | answered |

403, 404 and 429 extend the header spike's 4xx row (400, 401, 409: all 5); 502 and 503 its
5xx row (500: 6). Distinct statuses in one class stay indistinguishable to script.

## Series 3: cargo grid config paths

| Class | `<class> itemsCargoSize` | `<class> Cargo itemsCargoSize` |
|---|---|---|
| `AliceBag_Green` | 8x8 | nothing |
| `HighCapacityVest_Black` | 5x4 | nothing |
| `HuntingJacket_Brown` | 6x4 | nothing |
| `HunterPants_Brown` | 5x4 | nothing |
| `SeaChest` (container control) | nothing | 10x10 |

Across all 786 public clothing classes in `CfgVehicles`: 345 return a grid at
`<class> itemsCargoSize`, 0 at `<class> Cargo itemsCargoSize`. The remaining 441 return
nothing at either path. A container such as `SeaChest` keeps the grid in a `Cargo` subclass, which is why the
two paths differ.

## Series 4: file output before `CloseFile`

The runner read the file size from outside the process at each probe marker.

| Moment | `unclosed.txt` / `killed.txt` size |
|---|---|
| right after `FPrint` of 4 096 bytes, handle open | 4 096 |
| 3 s later, handle still open | 4 096 |
| after `CloseFile` | 4 096 |
| `killed.txt`: after `FPrint` of 4 096 bytes, handle open, before the kill | 4 096 |
| `killed.txt`: after `TerminateProcess` | 4 096 |

## Series 5: the file reader's line limit

One line of exactly the given length, no newline, written with `FPrint` and read back with
one `FGets`. A length whose `attempt` marker has no `ok` marker on the next boot faulted.

| Length | Result |
|---|---|
| 65 534 | `FGets` returned 65 534, content intact |
| 65 535 | process faulted inside `FGets` (`init.c:508`), access violation, illegal write |
| 65 536 | the same fault at the same code address |

## Findings

1. **The read-timeout option is clamped to 3..120 s.** Above the ceiling the engine uses
   120; below the floor, 3. A plugin can request any value without breaking the context,
   but nothing above 120 buys hold time.

2. **A `204` is indistinguishable from a timeout.** It arrives as `OnError(8)`, the code
   every measured read timeout produced, after 76 ms rather than after the timeout.
   Consequence, from the store's code (this spike did not run the plugin): the plugin's
   store treats a delete's `204` as an empty `OnSuccess`
   (`VyshkaStore.c`, "A delete answers 204 with no body"), which never happens on this
   engine. A successful delete is logged as a timeout and retried; the retry finds the key
   gone and succeeds through the inline `not_found` answer, or, with too little deadline
   left, the caller is told a delete that happened failed. Any Plugin API answer a DayZ
   plugin must read as success needs a `2xx` with a body.

3. **Redirects are not followed.** A `302` is `OnError(7)`, the code a refused connection
   produces. A hub URL that redirects (for example, `http` to `https` at a proxy) looks to
   the plugin like an unreachable hub.

4. **Clothing cargo lives on the class.** `ConfigGetIntArray("CfgVehicles <class>
   itemsCargoSize")` returns the grid for every clothing class that has one; the `Cargo`
   subclass path is for containers. This replaces the inventory-tree spike's conclusion
   that the config reader "does not return that key" (its probe used the container path).
   The plugin's item catalog reads clothing cargo at the `Cargo` path and so publishes no
   cargo for any clothing item.

5. **`FPrint` output is not held in the process.** 4 096 bytes were in the file the moment
   `FPrint` returned and survived `TerminateProcess` with the handle never closed, so a
   process kill loses nothing `FPrint` has returned from. Power loss is a different
   question (the OS page cache) and was not measured.

6. **The reader's line limit is 65 534 bytes.** 65 535 faults the process, which narrows
   the bans spike's 65 521..65 536 bracket to one value. The plugin's `LINE_MAX` of 60 000
   stays below it.

## What was not measured

- Larger `FPrint` writes. Only 4 096 bytes were checked for in-process buffering.
- Other `2xx` and `3xx` codes (201, 301, 307), and whether a `204` with a body behaves
  differently.
- Power loss or OS crash for the file series.
- `GET_now`/`POST_now`/`FILE_now` and `OnFileCreated`; this spike used `GET` only.
