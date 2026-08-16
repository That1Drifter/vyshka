# DayZ `RestApi` long-poll hold time: measurements

Resolves the pre-1.0 open question in `spec/protocol.md` section 3.1.

**Short answer:** with default settings a DayZ script HTTP request dies at **10 s**, so the
25 s hold the draft assumed does not work out of the box. The limit is configurable per
process from script (`RestApi.SetOption`), it is a **total budget for the entire response**
rather than an idle timer, and a 25 s hold is reliable once the plugin raises it. The floor
the engine will accept is 3 s and the documented ceiling is 120 s.

## Environment

| Item | Value |
|---|---|
| Game build | DayZ 1.29.0.163709, server binary `DayZServer_x64.exe` |
| Script defines | `DAYZ_1_29, SERVER_FOR_WINDOWS, SERVER, PLATFORM_WINDOWS, RELEASE, NO_GUI` |
| Host OS | Windows 11 Pro 26100 |
| Stub server | Go 1.26.4, `stub/main.go`, `127.0.0.1:8099`, plain HTTP over loopback |
| Harness | `harness/VyshkaPollProbe.c` in the `init.c` of a copied offline mission |
| Date | 2026-08-15 |

Loopback removes network latency and any intermediary, so each measured delay is the
engine's own behavior. Real deployments add TLS handshake time and reverse-proxy timeouts on
top; those are separate limits, noted at the end.

Behavior can change between game patches. Re-run the spike (see `../README.md`) when the
plugin is validated against a new major DayZ version.

## What the engine documents about itself

`scripts/3_game/http/restapi.c`, shipped with the game, declares:

- `ERESTOPTION_READOPERATION` - "read operation timeout (default 10sec)"
- `ERESTOPTION_CONNECTION` - "connection timeout (default 10sec)"
- "limit for timeout is between <3 .. 120> seconds, you cannot exceed this value"
- `RestCallback.OnError` - "May be called multiple times in case of (RetryCount > 1)"

Both are set through `RestApi.SetOption(int option, int value)`, which lives on the `RestApi`
singleton, not on a `RestContext`. The run below checks the runtime against those comments.

## Results

Twelve steps, one server boot, one shared `RestContext`. "Script" is the elapsed time the
probe's callback reported; "socket" is when the stub saw the connection end. `hold` sends
nothing until the delay elapses, `headers` sends response headers immediately and the body
after the delay, `drip` sends headers immediately and a body chunk every 4 s.

| # | Read timeout | Request | Outcome | Script | Socket |
|---|---|---|---|---|---|
| 0 | default | hold 5 s | `OnSuccess`, 50 B | 5086 ms | 5.001 s complete |
| 1 | default | hold 9 s | `OnSuccess`, 50 B | 9058 ms | 9.001 s complete |
| 2 | default | hold 12 s | `OnError(8)` | 10113 ms | 9.992 s client gone |
| 3 | default | hold 25 s | `OnError(8)` | 10063 ms | 9.992 s client gone |
| 4 | default | headers, body at 25 s | `OnError(8)` | 10022 ms | 10.003 s client gone |
| 5 | default | drip 30 s, chunk every 4 s | `OnError(8)` | 10073 ms | 9.994 s client gone, after chunks at 4 s and 8 s |
| 6 | default (connection set to 30) | hold 25 s | `OnError(8)` | 10024 ms | 9.982 s client gone |
| 7 | 30 s | hold 25 s | `OnSuccess`, 52 B | 25074 ms | 25.000 s complete |
| 8 | 30 s | hold 35 s | `OnError(8)` | 30024 ms | 29.980 s client gone |
| 9 | 120 s | hold 60 s | `OnSuccess`, 52 B | 60074 ms | 60.000 s complete |
| 10 | 200 s (out of documented range) | hold 25 s | `OnSuccess`, 52 B | 25124 ms | 25.000 s complete |
| 11 | 3 s | hold 5 s | `OnError(8)` | 3074 ms | 2.993 s client gone |

Raw logs: `probe-script.log` (script side), `stub-run.log` (socket side).

## Findings

1. **The default hold limit is 10 s.** Steps 0 to 3 bracket it: 9 s succeeds, 12 s fails, and
   the socket dies at 9.99 s. The engine comment is accurate.

2. **It is a budget for the whole response, not an idle timer.** Sending headers immediately
   (step 4) buys nothing, and sending body chunks every 4 s (step 5) buys nothing either: the
   request still died at ~10 s with two chunks already delivered. **Keepalive bytes cannot
   extend a held poll on DayZ.** This kills the usual "drip whitespace to hold the connection"
   trick, and it means the hub cannot rescue a hold that is about to exceed the plugin's
   configured limit; it must respond before that deadline.

3. **The limit is script-configurable and takes effect immediately.** `SetOption(1, 30)`
   raised it to exactly 30 s (step 8 died at 29.98 s), and 120 s allowed a clean 60 s hold.
   The option was changed after the `RestContext` already existed and applied to the next
   request, so a plugin can negotiate a `pollTimeout` at session start and then set the
   matching client timeout without rebuilding its context.

4. **The option is the read timeout, not the connection timeout.** Step 6 is the control:
   raising `ERESTOPTION_CONNECTION` to 30 s left the 10 s wall exactly where it was. Only
   `ERESTOPTION_READOPERATION` matters for a held response.

5. **A 3 s value is honored** (step 11 died at 2.993 s), matching the documented floor. Above
   the documented ceiling, `SetOption(1, 200)` did not break anything and a 25 s hold still
   succeeded; whether 200 was clamped to 120 or accepted was not measured, and nothing in the
   protocol should depend on values above 120.

6. **Timeouts arrive as `OnError`, never `OnTimeout`.** Every failed step called
   `OnError(errorCode = 8)`; `OnTimeout()` was not called once. Code 8 is
   `EREST_ERROR_TIMEOUT` only if `EREST_ERROR` and `EREST_ERROR_CLIENTERROR` share a value,
   as the header comment states; read positionally against the script enum, 8 would be
   `EREST_ERROR_APPERROR`. Since the failures are unambiguously timeouts, the C++ numbering
   must be the one where those two constants coincide. **Plugins must not map `errorCode` by
   the script enum's ordinal position**, and must treat the timeout as an `OnError` case.

7. **No implicit retry.** The stub saw exactly one connection per `GET`, 12 for 12 steps, and
   each callback fired once. Retransmission is entirely the plugin's job.

8. **Callback delivery lags the socket by roughly 20 to 120 ms**, because callbacks are
   dispatched on the script tick. That is the plugin's own overhead on top of any hub-side
   hold, and it is well inside the safety margin proposed below.

## Incidental engine notes

Not the question being answered, but discovered while building the harness and worth
recording for the plugin work:

- `RestApi`, `RestContext`, `RestCallback` and `CreateRestApi`/`GetRestApi` all resolve in
  server-side script. `GetRestApi()` returned a live instance during mission init with no
  `CreateRestApi()` call needed, and `GetContextCount()` reported 2, so the engine already
  holds a context of its own.
- The `ERestOption` enum constants do **not** resolve from a mission `init.c` even though
  the classes do. The probe passes the raw option ids (1 = read, 2 = connection) instead.
  Whether they resolve inside a packed mod's script module is untested.
- `RestApi.EnableDebug(true)` produced no output in the server `.RPT` or script log.
- The mission script parser rejects a string concatenation broken across lines inside a call
  argument list. Keep such expressions on one line.
- A loose mission `init.c` cannot see classes from the mission's own `scripts/` folder
  (`LootDebug` from the stock offline mission failed to resolve), so the probe drops that
  call.

## What this means for the protocol

- The hub cannot assume a 25 s hold works; an unconfigured DayZ plugin dies at 10 s.
- A plugin must be able to tell the hub the hold it can survive, and the hub must respect it.
  The negotiated `pollTimeout` becomes the contract, and the plugin sets its read timeout to
  that value plus a margin so the hub's own response always wins the race.
- The margin has to cover the hub's response write plus the script-tick lag. 5 s is generous
  at the measured lag of ~0.1 s and still leaves the DayZ maximum (120 s) far away.
- A conforming floor of 5 s is safe: the engine accepts 3 s, so 5 s plus margin is
  comfortably inside the accepted range.
- Deployments behind a reverse proxy need the proxy's read timeout above the negotiated
  `pollTimeout` too. That is a deployment note, not a protocol rule.
