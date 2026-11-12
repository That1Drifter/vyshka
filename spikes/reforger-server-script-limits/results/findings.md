# Findings: what a Reforger dedicated server's script can do (issue #159)

Measured 2026-11-12 on the Arma Reforger dedicated server **1.9.0.37** (engine 193322, built
2026-10-27, Steam build 26257316, `ArmaReforgerServer.exe` on Windows 11, `User-Agent:
Arma Reforger/1.9.0.37 (Headless; Windows)`). The server ran
`worlds/GameMaster/GM_Arland.ent` with the probe addon loaded via `-addonsDir`/`-addons`, the
same form Workbench's dedicated-server plugin uses. The other side of each request was one of:

- the stub on `127.0.0.1:8099`: cleartext HTTP/1.1, and Go's server ignores the engine's
  `h2c` upgrade offer;
- the branch hub at `https://vyshka-branch.solvaryx.cloud` behind its nginx: TLS, with a
  real held `/plugin/v1/poll` in runs 5 and 6, on a session minted from the shell for a
  throwaway server record whose credentials were revoked afterwards.

There were six runs, one boot each. Each figure cites its run. The raw lines are in
`runN-probe.log` (the server's `VYSHKA_RF` lines; session tokens replaced by
`<session token>`, the player's name by `<player>`), `runN-stub.log` (what reached the wire;
from run 5 on it also logs protocol, `Content-Length` and transfer encoding),
`run1-backend-errors.log` and `run2-crash.log`.

**Correction inside the series.** Runs 1 to 3 built their test strings with a helper that
trimmed with `string.Substring`, and run 3 found that `Substring` caps its result at 8191
characters. Every string longer than 8191 characters in runs 1 to 3 was really 8191: the
request bodies, file lines and JSON strings, and run 2's long URL. Run 4 re-measured the
sizes with a helper that only concatenates. Each figure below uses the run that was free of
the artifact.

## Summary

| Question | Answer | Evidence |
|---|---|---|
| Outbound HTTP from server script | Yes. `GetGame().GetRestApi().GetContext(base)` gives a `RestContext`. Its `GET`, `POST`, `PUT` and `DELETE` take a `RestCallback` whose `SetOnSuccess`/`SetOnError` functions receive the callback. It is callback-only. | run 1 A |
| Blocking forms | Unusable. `GET_now`, `POST_now` and the rest return at once without the response: `GET_now` returned `""`, `POST_now` returned its own request body. The request is still sent. | run 1 `now.*`, run 1 stub 14:14:45 |
| TLS | HTTPS to a Let's Encrypt host works with no setup (first request 251 ms). | run 1 `tls.healthz` |
| Default timeout | 15 s. A 14 s hold succeeds; a 16 s hold times out at 15.09 s and a 25 s hold at 15.07 s. | run 2 `t.default.*`, run 1 `hold.default.25` |
| Settable timeout | `RestContext.SetTimeout(s)`. 3 s and up are honored: 3 gave 3.10 s, 5 gave 5.07 s, 40 gave 40.10 s, and 300 let a 150 s hold complete. 1 and 2 are ignored, leaving the 15 s default (20 s holds timed out at 15.07 s on fresh contexts). 0 is ignored too: after `SetTimeout(5)`, `SetTimeout(0)` left 5 s in place (a 10 s hold timed out at 5.07 s). | run 2 `t.fresh.3.*`, run 1 `hold.t*`, run 5 `t.fresh.*.hold20`, `t.5-then-0.hold10` |
| Idle or whole-response timeout | **Idle.** A response trickling one byte a second outlived the timeout: 20 s against 10 s (run 1), 12 s against 5 s (run 2). One byte every 7 s under a 5 s timeout timed out at 5.07 s, after the first byte had arrived at once (run 2). DayZ's limit is on the whole response. | run 1 `drip.t10.20`, run 2 `t.5.drip*`, run 2 stub |
| When a queued request's timeout starts | **At issue, not at send.** An echo with a 5 s timeout, issued behind a 10 s hold to the same cleartext host, timed out at 5.03 s and never reached the stub. | run 5 `local.queued.t5.echo`, run 5 stub |
| Non-2xx status and body | **Readable.** Each of 400, 401, 404, 409, 429, 500 and 503 reaches the error callback with `GetHttpCode()` equal to the status and `GetData()` equal to the body (result `EREST_ERROR_UNKNOWN`). 204 is a success with empty data. `?errors=inline` also works over TLS (200 carrying the error object). | run 1 `status.*`, `tls.session.*` |
| Redirects | Not followed. A 302 reaches the error callback with code 302 and its body. | run 1 `status.302` |
| Transport failures | Connection refused: an error after 2.07 s, http 0, `EREST_ERROR_UNKNOWN`. Unknown host: `EREST_ERROR_SEND` at once. Timeout: `EREST_ERROR_TIMEOUT`, http 0, empty data. | run 1 `fail.*`, `hold.*` |
| Engine log noise | The engine writes each failed asynchronous request to `console.log` as a `BACKEND (E): [RestApi]` line (the `GET_now` 404 left none). Run 1 had 17: nine non-2xx responses (`Error Code:<status> - <reason>` for 302, 400, 401, 404, 409, 415, 429, 500 and 503; a 429 reads `Error Code:429 - Unimplemented 4xx`), three client-side timeouts (`Timeout`), and five `Error - Cannot send request` (the oversize responses, the refused port, the unknown host). | `run1-backend-errors.log` |
| Request headers | `SetHeaders("Name,value,Name,value")` returns true and the headers reach the wire on GET, POST, PUT and DELETE. `Authorization: Bearer ...` can be sent, and `Content-Type` overridden. The `Name: value` form, one header or two separated by a newline, returns false and sends nothing. Headers belong to the context and persist on later requests. A second `SetHeaders` call **replaces** the first set: after `Authorization,Bearer first` then `X-Second,b`, only `X-Second` was sent. | run 1 `hdr.*` and stub lines 7 to 11, run 5 `ct.*`, `hdr.twice` and stub |
| Default request headers | `Accept: */*`, `User-Agent`, and an `Upgrade: h2c` offer (`Connection: Upgrade, HTTP2-Settings`). POST and DELETE default to `Content-Type: application/x-www-form-urlencoded`, and PUT sends no `Content-Type`. The hub answers 415 until the plugin sets `application/json`. POST bodies go out chunked, with no `Content-Length`. PUT and DELETE send `Content-Length`. | run 1 stub lines 2 to 5, `tls.session.401` (415), run 5 stub |
| Response headers | No API reads them. `RestCallback` offers the result, the status code, and the body. | `RestCallback.c` in the June 2026 script extract; the calls the probe used compiled and ran on 1.9 |
| Contexts | One per base URL string. `GetContext` with a base URL already in use returns that context; the count stays the same. Spellings of one host that differ (case, `:443`, a trailing slash) are distinct contexts. | run 1 `*.context`, run 6 `R6.context` |
| A second request while one is held | **Depends on the transport, and is not guaranteed.** Over **cleartext HTTP/1.1** (the stub) the second request never ran alongside the held one: in 5 pairs it reached the stub in the same millisecond the hold answered (same context, another context, fresh or already used), and in a 6th, with a 5 s timeout, it timed out while waiting and never reached the stub. Over **TLS** through the hub's nginx, with a real 10 s poll held, a healthz request to the same host gave different results depending on how the host was spelled. *Same spelling as the poll's context* (run 5): it waited for the poll (10.35 s) when it was the boot's first pair, and went through in 0.35 s on the poll's own context and 0.22 s on the now-used second context. *A different spelling* (upper case or `:443`, run 6): it went through in 0.22 s in 5 of 5 pairs, including one where both contexts were brand new on the boot's first request. So fresh versus used does not explain the one slow pair, and its cause is unknown. A request to a different host is not held up (run 3). The fast TLS pairs fit HTTP/2 multiplexing, but the probe cannot see the negotiated protocol. | run 1 `conc.*` and stub, run 3 `q.*`, run 5 `tls.*` and `local.*`, run 6 `r6.*` and stubs |
| Response size | Up to 1 048 575 bytes arrive. 1 048 576 and above fail with `EREST_ERROR_STORE`, whether the stub sends `Content-Length` (runs 1 and 2) or streams the body chunked with no `Content-Length`, as the hub's JSON responses go out (run 5). In the chunked case the stub wrote all 1 048 576 bytes and the client still failed. At 2 MiB the client closed the connection after the stub had handed 1 097 734 bytes to the socket, so the cap is on received bytes, not a header check. Measured with ASCII bodies and 200 responses; the TLS answers measured were all small. | run 2 `resp.1m-1`, run 1 `resp.1m`, run 5 `chunked.*` and stub `big-written` |
| Request size | Bodies arrive whole to at least 16 MiB: 64 KiB, 256 KiB, 1 MiB, 4 MiB and 16 MiB, with each digest matching `'a' × n`. Multi-byte UTF-8 arrives intact (10 000 bytes of `é`). | run 4 `req.*` and stub, run 3 `req.utf8.5000x2` |
| URL length | A 2055-character request path (`echo?q=` plus 2048) works. A path of 8198 characters (`echo?q=` plus 8191, the Substring artifact; 8224 with the base) **crashes the server** with an access violation (illegal write) inside `RestContext.GET`. The crash reporter then sent a report to Bohemia on its own, without asking. Nothing between 2055 and 8198 was tried, to avoid more crashes. | run 3 `req.query.*`, run 2 step 19 and `run2-crash.log` |
| Script strings | Hold at least 16 MiB. `Length()` counts bytes (4096 two-byte `é` give 8192). `Substring` caps its result at 8191 characters (`Substring(0, 8192)` of a 20 000-character string gives 8191). Building a string with `+=` in a loop is slow: about 6 s for a 100 000-number document. | run 4 `string.substring.8192`, run 3 `string.*`, run 1 timestamps |
| JSON | `JsonLoadContext.LoadFromString` with `ReadValue`, and `JsonSaveContext.WriteValue` with `SaveToString`. A 4 MiB string value parses in 6 ms and saves in 4 ms. A 1 MB array of 2000 strings parses in 1 ms, and 100 000 integers in 4 ms. Linear, and the lengths read back match. | run 4 `json.*`, run 1 `json.parse.ints.*` |
| Files | `$profile:` resolves to `<-profile>/profile/`: after run 1, `$profile:vyshka-probe/copy.txt` was on disk at `<-profile>/profile/vyshka-probe/copy.txt`. A bare relative path is refused (`OpenFile` returns null); absolute paths and `..` were not tried. `FileMode.WRITE` creates and truncates. `OPENW` neither creates a missing file nor truncates an existing one. A 1 MiB line written with `WriteLine` reads back with `ReadLine` at the same length; content was not compared. `WriteLine` ends lines with CRLF on Windows. There is no rename: `CopyFile` then `DeleteFile` works but is not atomic. | run 4 `file.line.*`, run 1 `file.*` |
| Lifecycle hooks | On `SCR_BaseGameMode`, `OnGameStart` runs on the server (`RplMode.Dedicated`), with the engine tick counter at 8.2 to 13.6 s across the six runs. At join: `OnPlayerConnected`, `OnPlayerAuditSuccess`, `OnPlayerRegistered`. When the client process was killed, `OnPlayerDisconnected` fired with the server's `L2 live connection timeout` for it (the kill time itself was not recorded). A graceful server stop was not measured; every run ended in a process kill. | runs 1 and 3 `hook.*`, `run3-probe.log` RPL lines |
| Player identity | **Empty on this launch.** `SCR_PlayerIdentityUtils.GetPlayerIdentityId`, the obsolete `BackendApi.GetPlayerIdentityId`, `GetPlayerPlatformKind` (`NONE`) and `GetPlayerPlatformId` all returned nothing, though `OnPlayerAuditSuccess` fired. The game's own `SCR_PlayerIdentityUtils` says a dedicated server needs a backend connection (`publicAddress` in the server config) to have identities. These runs did not use `-config`. The local Reforger knowledge base records, from an earlier live run (1.7, Linux), that a `-config` server loads mods only from the Workshop and refuses to start with an unpublished one. Reading a real identity therefore means publishing the probe as an unlisted Workshop item. **Open.** | run 3 `hook.OnPlayerAuditSuccess` |
| WebSocket or sockets | None found. `RestApi` is the only network surface script gets. Neither the 1.8.0.13 API index (`rwt api search`) nor the June 2026 script extract has a WebSocket or socket class. The 1.9 headers were not searched. | API index, `scripts/` extract |

## What this means for the Reforger plugin (#15)

- **The poll transport works as specified.** Over TLS with a real session, a held
  `/plugin/v1/poll` answered after its 10 s `pollTimeout` every time (runs 5 and 6). Set
  `SetTimeout(pollTimeout + 5)` on the poll's context. The protocol's 5 s to 60 s range is
  reachable, and so is the default of 25 s. Never pass a value below 3, and don't use 0 to
  reset: both are ignored. The timeout is on idle time, so nothing changes, because the
  protocol already forbids keepalive tricks.
- **Auth goes in the real header.** Make one `SetHeaders("Authorization,Bearer <token>,Content-Type,application/json")`
  call per context, and repeat the whole set when the token rotates, since a second call
  replaces the first. Appendix A's content-type header smuggle is not needed. Without the
  `Content-Type` override the hub answers 415.
- **Errors are readable.** The plugin can branch on `error.code` from the body (section 2.2)
  directly. `?errors=inline` works too but isn't needed. Each refusal and each client-side
  timeout leaves a `BACKEND (E)` line in the operator's console, so the README should say
  that a `429 - Unimplemented 4xx`, a 409, or an occasional poll `Timeout` there is expected.
- **Don't count on a second request during a held poll.** Over TLS one went through at
  once in 7 of 8 pairs, but not in the one pair that used the poll's own host spelling on
  fresh contexts, which is the case a real plugin is in. Over a cleartext URL (a hub on a LAN or on localhost) it always
  waits for the poll to answer. Time spent waiting counts against the request's own
  timeout. So a side request (a key/value call, a ban-list page) needs a timeout of at least
  `pollTimeout` + 5 s, or it must go out between polls.
- **Protocol change first: a response size bound.** A Reforger plugin cannot receive a
  response of 1 MiB or more, and nothing in the protocol caps a poll response in bytes. A
  batch may hold up to the hub's envelope cap (reference 200, section 9.2). The spec bounds
  no action's `params`; the reference hub's only limit is its 1 MiB request body on dispatch,
  so even one action can bring a response to the size Reforger rejects. Before the plugin
  is written, #15 needs a byte bound the plugin can ask for (or a protocol-wide one), with
  the hub cutting the batch where it would cross it, and a rule for one envelope too big to
  send at all. The same bound covers the ban-list page and any other large answer.
- **Keep URLs short.** A request path of 8198 characters crashes the server, so no data goes
  in a query string. The plugin's paths are a few dozen characters.
- **Strings: avoid `Substring` on long strings.** `Substring` returns at most 8191 characters,
  the same cap the DayZ plugin hit in #108. JSON is linear, so the windowed parser the DayZ
  plugin needed is not, but building a large string with `+=` is slow and should collect
  pieces and join once.
- **Files: `WRITE` to rewrite, never `OPENW`.** A crash-safe rewrite needs the plugin's own
  scheme, since there is no atomic rename (the DayZ plugin solved this in #121).
- **The platform registry entry waits.** The identifier a Reforger server sees is still
  unmeasured, so the entry in section 8.2 cannot be chosen from evidence yet. It needs one run
  on a backend-registered server with the probe published as an unlisted Workshop item.
- **WebSocket stays parked.** No socket API was found for Reforger script, so this game does
  not fire its trigger. A sidecar remains the only customer.
