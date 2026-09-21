# Ban list pull size on DayZ: measurements

Answers what the pull of issue #80 could not be designed around without measuring: what a
list of N entries costs the server's main thread once it has arrived, whether the plugin
can keep a copy on disk and read it back, and where the engine's HTTP client stops above
the 1 MiB the earlier spike reached.

**Short answers:** on this engine every single-character read and every append pays for
the whole string it touches, so the plugin's JSON parser and serializer are quadratic in
the input: a 1 000-entry list costs 16 s of frame stall and a 5 000-entry list 6.6
minutes. The plugin's file reader faults natively on a single line of 64 KiB or more,
which a few hundred bans reach. `Substring` silently caps its result at 8 191 characters.
The HTTP client is not a limit through 32 MiB, which arrived intact. The pull must be paged,
the on-disk copy written one entry per line, and the parser rewritten to read through
bounded windows before any payload larger than an action dispatch is routine.

## Environment

| Item | Value |
|---|---|
| Game build | DayZ 1.29 server binary `DayZServer_x64.exe` (1.29.163709) |
| Mod | `@Vyshka` built from `main` at 1073891 (plugin 0.8.x, protocol draft 0.25), no config, idle |
| Host OS | Windows 11 Pro 26100 |
| Stub | `runner/main.go`, `127.0.0.1:8096`, plain HTTP over loopback |
| Harness | `harness/VyshkaBansProbe.c` in the `init.c` of a mission derived from `dayzOffline.chernarusplus` |
| Server flags | `-dologs -adminlog`, no `-freezecheck` (the stall is the number) |
| Date | 2026-09-21 |

Timing is the engine's `TickCount` performance counter, which runs inside one frame. Its
unit is undocumented; against the frame clock it measured 10 000 ticks per millisecond
(a 10 MHz counter), which the runner uses to convert. It is a 32-bit value, so a phase
longer than about 215 s wraps once and is corrected by 2^32 (the 5 000-entry parse and
the 1 MiB padded parse); a phase longer than about 429 s would be reported modulo that
and the conversion could not tell, so the frame clock on the same line, which bounds the
whole step, is the check on every corrected value.

## Runs

Each run is one invocation of the runner; a step that faults the process is resumed past
on the next boot through the probe's progress file.

| Run | What happened | Logs |
|---|---|---|
| 1 | The mission did not compile: an expression continued on the next line with a leading `+` is a syntax error | `runner-run1-compile-fail.log`, `probe-run1-compile-fail.log` |
| 2 | 100 entries measured; the read-back of the 500-entry file (120 164 bytes, one line) faulted the process natively inside `VyshkaFiles.ReadAll` | `runner-run2-crash.log`, `probe-run2-crash.log`, `run2-crash-report.txt` |
| 3 | Read-back removed from the ban list series; 100 to 5 000 entries measured; the 10 000-entry step did not finish inside the 25-minute boot budget; the 20 000-entry step was stopped by hand | `runner-run3-bans.log`, `run3-boot1-script.log`, `run3-boot2-script.log`, `stub-run3-bans.log` |
| 4 | Line series void: the probe trimmed its test line with `Substring`, which capped it at 8 191 bytes, so every size read back "intact"; raw responses to 32 MiB | `runner-run4-line-raw.log`, `probe-run4-line-raw.log`, `table-run4-line-raw.md` |
| 5 | Line series with exact lengths: 65 520 read back, 65 536 faulted; the `Substring` cap confirmed; indexing at two positions | `runner-run5-line-index-raw.log`, `probe-run5-line-index-raw.log`, `crashes-run5.log`, `table-run5-line-index-raw.md` |
| 6 | Padded lists: 100 entries plus a 1 MiB or 256 KiB string value; allocation batches (the append and read numbers were lost to the 255-character `Print` limit) | `runner-run6-alloc-pad.log`, `probe-run6-alloc-pad.log`, `table-run6-alloc-pad.md` |
| 7 | Did not compile: `local` is a reserved word | `runner-run7-compile-fail.log` |
| 8 | Allocation, append, and read cost by string length, on a local, a member, a parameter, and a copy | `runner-run8-alloc-member.log`, `probe-run8-alloc-member.log` |
| 9 | Smoke run of the runner after the review fixes, from the indexing step on: the raw series repeated, the 1 MiB padded step timed out at the shorter boot budget and was skipped on the next boot, the rest finished | `runner-run9-smoke.log` |
| 10 | Smoke run of the runner after the second review round, from the last line-limit step on: one fault, one timeout, one resume | `runner-run10-smoke.log` |

## Series 1: ban lists (run 3)

Every phase runs synchronously on the main thread, so the parse and serialize columns are
the frame stall a player would feel. The stub answers in the planned pull shape (about
240 bytes per entry: identity object, name, reason, timestamps, ban id).

| Entries | Body bytes | Fetch (frames) | Parse | Build map | 1 000 lookups | Serialize | Write |
|---|---|---|---|---|---|---|---|
| 100 | 23 964 | 20 ms | 184 ms | 0.6 ms | 0.3 ms | 20 ms | 0.4 ms |
| 500 | 120 164 | 75 ms | 3.5 s | 3.6 ms | 0.3 ms | 420 ms | 0.4 ms |
| 1 000 | 240 414 | 100 ms | 14.6 s | 7.5 ms | 0.3 ms | 1.6 s | 0.5 ms |
| 2 000 | 481 914 | 101 ms | 57.3 s | 13.7 ms | 0.4 ms | 6.6 s | 0.7 ms |
| 5 000 | 1 206 414 | 17 ms | 353 s | 34 ms | 0.4 ms | 45.4 s | truncated |
| 10 000 | 2 413 914 | 102 ms | the step did not finish in 16 min | | | | |
| 20 000 | 4 838 914 | 161 ms | stopped | | | | |

Parse time grows 4.2x from 500 to 1 000 entries, 3.9x from 1 000 to 2 000, and 6.2x from
2 000 to 5 000 (2.5x the input; 6.25 expected for a square). The serializer follows the
same law. Two caveats on the 5 000 row: its write field is the last on a log line that
`Print` cut at 255 characters, so its digits are not known to be complete and the cell
is left blank; and its parse counter wrapped once, which the runner corrects, with the
step's frame clock (399 s for parse and serialize together) confirming the value. The
10 000 row has no checkpoint between parse and serialize, so what it records is that the
whole measurement did not finish, with the parse the plausible unfinished phase.

## Series 2: the reader's line limit (run 5)

One line written with `VyshkaFiles.WriteAll` (`FPrint`), read back with
`VyshkaFiles.ReadAll` (an `FGets` loop).

| Line bytes | Written | Read back | Intact |
|---|---|---|---|
| 16 384 | yes | 16 384 | yes |
| 32 768 | yes | 32 768 | yes |
| 49 152 | yes | 49 152 | yes |
| 65 520 | yes | 65 520 | yes |
| 65 536 | yes | **process faulted** inside `FGets` | |

The crash report names `ReadAll` at `vyshkafiles.c:55`, the `FGets` line, with a fault
address in an unknown module: the engine, not the script VM. The first failing length is
somewhere in 65 521 through 65 536 inclusive, which reads as a 64 KiB buffer; the exact
value is inferred, not measured.

## Series 3: raw responses (runs 4 and 5)

| Body bytes | Arrived | String length | Time |
|---|---|---|---|
| 2 097 152 | yes | 2 097 152 | 97 ms |
| 4 194 304 | yes | 4 194 304 | 150 ms |
| 8 388 608 | yes | 8 388 608 | 304 ms |
| 16 777 216 | yes | 16 777 216 | 1.1 s |
| 33 554 432 | yes | 33 554 432 | 4 to 6 s |

## Series 4: where the cost is (runs 5, 6, and 8)

**Padded lists (run 6).** The same 100 entries with one extra string member of the given
size, so the object count is fixed and only the byte count moves:

| Pad | Body bytes | Parse | Serialize (pad truncated to 8 191) |
|---|---|---|---|
| none | 23 964 | 184 ms | 20 ms |
| 256 KiB | 286 117 | 16.6 s | 44 ms |
| 1 MiB | 1 072 549 | 232 s | 44 ms |

Parse time tracks the byte count, not the object count, and 4x the bytes cost 14x the
time.

**Reads by string length (runs 5 and 8).** 1 000 calls of `string.Get(16)`:

| String | Local | Member | Member via method | By-value parameter | Fresh copy | `Substring` |
|---|---|---|---|---|---|---|
| 32 bytes | 0.05 ms | | | | | |
| 256 KiB | 51 ms | 51 ms | 52 ms | 51 ms | 52 ms | 51 ms (`Substring(16, 20)`) |
| 384 KiB | 92 ms | | | | | |
| 1 MiB, position 16 | 212 ms | | | | | 214 ms (`Substring(16, 1)`) |
| 1 MiB, position 524 288 | 205 ms | | | | | 206 ms (`Substring(524288, 1)`) |

The cost is about 0.2 µs per KiB of the string per call, independent of the position and
of how the string is held. Whether the engine scans the string or copies it on each call
is not something these probes can tell apart: an assignment of the 256 KiB string cost
61 µs, which is the same order as one read on it, so a per-call copy is consistent with
every number above. What is established is the cost, not the mechanism.

**Appends (run 8).** Batches of 4 096 sixteen-byte appends to one string: 39 ms while it
grew to 64 KiB, then 97, 145, 201, and 262 ms for the next four batches; 1 000 appends to
a 256 KiB string cost 57 ms through an `out string` parameter and 58 ms on a local. Each
append pays for the string's current length.

**Allocation (run 8).** Six batches of 10 000 `VyshkaJsonValue.NewObject()` kept alive:
7.4, 5.6, 5.5, 5.4, 4.0, and 4.6 ms; 6.7 ms after releasing them all. Six batches of
10 000 plain `VyshkaBanEntry`: 2.2 to 2.3 ms each. Constant.

**`Substring` cap (run 5).** On a 1 MiB string, `Substring(0, 8191)` returned 8 191
characters, `Substring(0, 8192)` returned 8 191, `Substring(0, 100000)` returned 8 191,
and a 100-character window near the end returned 100. No error is raised.

## Findings

1. **Reading one character of a string costs time proportional to the whole string's
   length.** `string.Get(i)` and `Substring` pay about 0.2 µs per KiB of the string on
   every call, whatever the position. A parser that reads an n-byte input one character
   at a time is therefore O(n²) on this engine, and the plugin's `VyshkaJson.Parse` is
   exactly that: 184 ms for 24 KB, 14.6 s for 240 KB, 353 s for 1.2 MB, and 232 s for a
   100-object document whose size comes from one string value. Object allocation is not
   a factor.

2. **Appending to a string pays for its current length.** Whether `+=` copies the string
   or does something else proportional to it is not determined, as with the reads; what
   the batches show is the cost growing with the length. The serializer, which appends
   every token to one result string, is O(n²) as a result: 45 s for 1.2 MB. The fix is the usual one, collect pieces and join once, or write pieces as they
   are produced.

3. **The plugin's file reader faults the process on a line of 64 KiB or more.**
   `VyshkaFiles.ReadAll` is an `FGets` loop and `WriteAll` writes its content as one line.
   This is a production defect today: `bans.json` is one line and grows by about 200 bytes
   per ban, so a server with roughly 300 local bans crashes at the next boot when the list
   loads; the manifest and an outbox record holding a large event batch can cross the same
   line. Writing one array element per line keeps the file valid (a newline between JSON
   tokens is whitespace) and every line short.

4. **`Substring` silently returns at most 8 191 characters.** The parser copies each
   unescaped run of a string value out with one `Substring`, so a run longer than that is
   truncated without error; a value with escapes every few thousand characters is
   assembled from several runs and can exceed the cap, so the limit is per run, not per
   value. No current payload carries such a run; a chat line or a mod's event payload
   could.

5. **The HTTP client is not the constraint.** 32 MiB arrived intact in a few seconds.

6. **Engine facts recorded in the DayZ KB:** the two syntax rules (`local` reserved, no
   leading operator on a continuation line), the 255-character `Print` limit, the 10 MHz
   `TickCount`, and the five performance facts above.

## Consequences for issue #80

- **The pull is paged, from the first draft.** A page of 100 entries costs 184 ms of
   parse and 20 ms of serialize on the parser as it stands, a visible hitch at 20 frames
   per second; a page of 50 costs a quarter of that, since the cost is quadratic in the
   page. `GET /plugin/v1/bans` takes a cursor and a `limit`, the plugin fetches one page
   per poll cycle and applies the list only once the last page has arrived, so a hub
   outage mid-pull leaves the previous list enforced, and `bans.applied` is sent once the
   whole revision is on disk. With a linear parser the page can grow, but paging stays: a
   list of any size must never be one frame's work.
- **The on-disk copy is written one entry per line**, and `bans.json` and the outbox
   records, whose large parts are arrays, get the same treatment as their own fix, since
   the reader's limit is a crash in production today. The manifest record is different:
   it stores the whole serialized manifest as one JSON string member, and a newline
   cannot be put inside a JSON string without escaping it, so that file needs a
   representation change rather than a line-breaking writer.
- **The parser and serializer are rewritten** before any payload larger than an action
   dispatch is routine. Cutting the input once into windows of at most 8 191 characters
   and reading characters from a window divides the quadratic term by 8 191 (about 35 ms
   of cutting for 1.2 MB, extrapolated, not measured; by the same model the 32 MiB body
   the HTTP client delivered would cost about 27 s to cut). That is a mitigation, not a
   linear parser, and it is adequate only up to a size that has to be measured with the
   windowed parser against a frame budget before any document of that size is relied on;
   a genuinely linear one needs a primitive that does not pay per string length per
   call, such as reading a file through `ReadFile` into an array. The rewrite covers the serializer's `Quote`
   loop and per-escape appends as well as the outer append, and replaces the per-run
   `Substring` that caps at 8 191. That is a plugin slice of its own, filed with the
   reader fix (#108).

## Follow-up: the plugin after issue #108 (2026-09-21)

The fixes the findings asked for landed the same day as issue #108: the parser reads its
input through windows cut once (`VyshkaTextCursor`, outer windows of 8 191 characters,
inner pieces of 256), string values are assembled from pieces `Substring` can return whole,
the serializer collects pieces and joins once (`VyshkaJsonWriter`), and every JSON file
the plugin writes goes out one array element and one object member per line, refused
rather than written when a single string value would still make a line over 60 000 bytes.
The manifest record keeps its content as the manifest object, so its arrays break across
lines too.

Measured by the plugin's new self-test (`vyshka-dayz selftest`, which runs
`plugins/dayz/selftest/VyshkaSelfTest.c` on the same server build; its run log is the
provenance; the numbers are from the run of the final build), on the same host and
game build as the runs above:

| Check | Input | Result |
|---|---|---|
| `json.speed` | the pull shape at 5 000 entries, 1 117 804 bytes | serialize 577 ms, parse 439 ms, write 698 ms, read back 374 ms (series 1 above: 45.4 s and 353 s for 1 206 414 bytes) |
| `files.bans` | 400 entries, one per line | saved in 63 ms, loaded in 40 ms; the 500-entry read that faulted run 2 is the same shape |
| `files.manifest` | a 200-action manifest record, 250 718 bytes compact | saved in 124 ms, read back equal in 198 ms |
| `files.outbox` | a 200-event batch record, 83 902 bytes compact | appended in 77 ms, restored equal in 69 ms |
| `files.oversizedLine` | one 70 000-character string | refused, not written |
| `json.longString` | a 100 000-character string value | parsed whole in 24 ms (the first parser returned 8 191 characters) |
| `json.escapes` | 56 000 characters with an escape every 3.5 | quoted in 45 ms, parsed back equal in 38 ms (210 ms before the decoded text was gathered in pieces) |

The parse is about 0.4 µs a byte, which is the windowed reads plus the script VM's own
cost per character; the residual quadratic term (one `Substring` on the whole input per
8 191-character window) is a few tens of milliseconds at this size and would be about 27 s
for the 32 MiB body of series 3, so a document of that size is still not one to parse in a
frame. The negative control was run once with the ban list written as plugin 0.8.0 wrote
it, as one line: the read faulted the server inside `FGets`, and the self-test tool
reported the exit as the failure.
