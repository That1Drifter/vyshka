# DayZ `RestApi` request shape: measurements

Answers four questions the DayZ plugin (issue #14) could not be designed around without
measuring: whether script can send an `Authorization` header at all, what script sees of an
error response, how large requests and responses can be, and what a refused connection
looks like.

**Short answers:** an `Authorization` header can be sent, but only by smuggling it through
the one header the API exposes; an error status never reaches script as anything but an
error code, so the protocol's error bodies are invisible on DayZ; 64 KiB bodies and 1 MiB
responses pass intact; a refused connection is error code 7 after about 2 s.

## Environment

| Item | Value |
|---|---|
| Game build | DayZ 1.29 server binary `DayZServer_x64.exe` |
| Host OS | Windows 11 Pro 26100 |
| Stub server | Go 1.26, `stub/main.go`, `127.0.0.1:8098`, plain HTTP over loopback |
| Harness | `harness/VyshkaHeaderProbe.c` in the `init.c` of the spike mission from `../dayz-restapi-poll-timeout` |
| Date | 2026-09-03 |

## Results

"Script" is what the probe's `RestCallback` reported; "socket" is what the stub logged for
the same request. Raw logs: `probe-script.log` (script side), `stub.log` (socket side).

| # | Step | `SetHeader` value | Script | Socket |
|---|---|---|---|---|
| 0 | baseline POST | `application/json` | `OnSuccess`, body echoed | `Accept: */*`, `Content-Type: application/json`, `Content-Length` |
| 1 | CRLF smuggle | `application/json` + CRLF + `Authorization: Bearer crlf-token` | `OnSuccess` | `Content-Type: application/json` **and** `Authorization: Bearer crlf-token` as separate headers |
| 2 | LF smuggle | `application/json` + LF + `Authorization: Bearer lf-token` | `OnSuccess` | same: two separate headers |
| 3 | whole header | `Authorization: Bearer whole-token` | `OnSuccess` | `Content-Type: Authorization: Bearer whole-token` (the value is not parsed) |
| 4 | restore | `application/json` | `OnSuccess` | back to baseline |
| 5 | GET, 401 with JSON error body | | `OnError(5)` after 65 ms | request arrived, body sent |
| 6 | GET, 400 | | `OnError(5)` | |
| 7 | GET, 409 | | `OnError(5)` | |
| 8 | GET, 500 | | `OnError(6)` | |
| 9 | POST, 401 | | `OnError(5)` | |
| 10 | POST 4 010 B to `/echo` | | `OnSuccess`, 4 010 B back | `Expect: 100-continue` added by the client |
| 11 | POST 65 546 B | | `OnSuccess`, stub counted 65 546 B | `Expect: 100-continue` |
| 12 | GET 262 144 B response | | `OnSuccess`, `dataSize` 262 144, string length 262 144 | |
| 13 | GET 1 048 576 B response | | `OnSuccess`, 1 048 576 B, 76 ms | |
| 14 | GET non-ASCII JSON | | `OnSuccess`, `{"text":"žluťoučký kůň"}` intact | |
| 15 | POST to a port with no listener | | `OnError(7)` after 2 275 ms | never arrived |
| 16 | POST after the refusal | | `OnSuccess` | context still usable |

## Findings

1. **`SetHeader` is written to the wire verbatim, so it can carry a second header.** The
   engine prefixes the value with `Content-Type: ` and appends it to the request as-is. A
   value containing a line break followed by `Authorization: Bearer <token>` therefore
   arrives as two well-formed header lines (steps 1 and 2), which is the only way script can
   satisfy spec section 2.1's bearer rule. Step 3 shows the value is never parsed: a bare
   `Authorization: ...` becomes the `Content-Type`. The plugin uses the CRLF form, which is
   what a strict proxy in front of the hub expects between header lines. This is undocumented
   engine behavior; the plugin logs and re-measures it if a future game patch breaks it, and
   the poll fails closed with `401` rather than silently, because a hub rejects a poll with no
   credential.

2. **An error status is an opaque error code. The body is lost.** Every 4xx response reached
   script as `OnError(5)` and the 500 as `OnError(6)`, never as `OnSuccess` with the body. The
   protocol's `error.code` (`session_invalid`, `envelope_invalid`, `credentials_revoked`, and
   the rest of spec section 2.2) is therefore invisible to a DayZ plugin. Combined with the
   earlier spike, the runtime numbering is: 5 client error (4xx), 6 server error (5xx),
   7 request never completed (connection refused; the header calls it "app error"), 8 timeout.
   These match the header's enum only if `EREST_ERROR` and `EREST_ERROR_CLIENTERROR` share a
   value, as its comment says; do not map codes by the script enum's ordinal position.

   Consequence for the plugin: a client error on a poll is treated as "the session is gone",
   because starting a new session is legal at any time (section 5.3) and is the correct
   answer to `session_invalid`; a client error on a session request is treated as bad or
   revoked credentials and retried with a long backoff; a client error on enrollment is
   logged for the operator and retried with a long backoff. A `400` caused by the plugin's own
   batch is indistinguishable from a `401`, so it produces a re-session loop with backoff and
   a log line rather than a diagnosis. Section 2.2 asks clients to branch on `error.code`;
   this engine cannot, and the spec's "fall back on the HTTP status" clause cannot apply
   either, because the status is not exposed. Recorded as a pre-1.0 open question in the
   design companion: a hub-offered soft-error mode (a `2xx` carrying the error body when the
   plugin asks for it at session start) would close the gap without touching the envelope.

3. **Request and response sizes are not the constraint.** A 64 KiB POST and a 1 MiB response
   both arrived whole, and the string handed to `OnSuccess` had the full length. The client
   adds `Expect: 100-continue` on bodies above a few kilobytes; Go's server handles that
   transparently, and a reverse proxy in front of the hub must too. The protocol's own caps
   (256 KiB request body, 200 envelopes per poll) are the binding limits, not the engine.

4. **UTF-8 survives.** Non-ASCII bytes in a response reached script byte-for-byte, so the
   plugin's JSON layer can treat strings as UTF-8 byte strings and pass them through.

5. **A refused connection fails fast and cleanly.** About 2.3 s to `OnError(7)`, and the
   same `RestContext` served the next request normally (step 16), so an outage needs no
   context rebuild, only a re-poll after backoff.

7. **Query strings pass through on GET and POST.** Added 2026-09-10 from the same logs while
   designing the inline-errors opt-in (issue #43): every step whose path carried a query
   string reached the stub with it intact, including the POST in step 9
   (`>> POST /status?code=401` in `stub.log`, request 010). A per-request opt-in expressed as a
   query parameter therefore works from script without touching the header smuggle.

6. **Callback latency is 12 to 112 ms**, consistent with the tick-dispatched callbacks
   measured in the earlier spike.

## Incidental engine notes

- `out` is a reserved word in Enforce Script. A local variable named `out` fails the whole
  mission script with "Broken expression (missing ';'?)" at the declaring line.
- `int.AsciiToString()` builds a one-byte string from a byte value, which is how the probe
  produced CR and LF without relying on `\r` escape support in string literals.
