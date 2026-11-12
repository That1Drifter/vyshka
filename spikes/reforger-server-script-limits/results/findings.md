# Findings: what a Reforger dedicated server's script can do (issue #159)

Measured 2026-11-12 on the Arma Reforger dedicated server **1.9.0.37** (engine 193322, built
2026-10-27, Steam build 26257316, `ArmaReforgerServer.exe` on Windows 11). The server ran
`worlds/GameMaster/GM_Arland.ent` with the probe addon loaded via `-addonsDir`/`-addons`, the
same form Workbench's dedicated-server plugin uses. The other side of each request was the
stub on `127.0.0.1:8099` or, for TLS, the branch hub at `https://vyshka-branch.solvaryx.cloud`.
There were four runs, one boot each. Each figure cites its run, and the raw lines are in
`runN-probe.log` (the server's `VYSHKA_RF` lines) and `runN-stub.log` (what reached the wire).

**Correction inside the series.** Runs 1 to 3 built their test strings with a helper that
trimmed with `string.Substring`, and run 3 found that `Substring` caps its result at 8191
characters. Every run 1 to 3 figure for a request body, a file line, or a JSON string over
8191 characters is therefore that cap, not the engine's. Run 4 re-measured all of them with
a helper that only concatenates. The figures below are run 4's.

## Summary

| Question | Answer | Evidence |
|---|---|---|
| Outbound HTTP from server script | Yes. `GetGame().GetRestApi().GetContext(base)` gives a `RestContext`. Its `GET`, `POST`, `PUT` and `DELETE` take a `RestCallback` whose `SetOnSuccess`/`SetOnError` functions receive the callback. It is callback-only. | run 1 A |
| Blocking forms | Unusable. `GET_now`, `POST_now` and the rest return at once without the response: `GET_now` returned `""`, `POST_now` returned its own request body. The request is still sent. | run 1 `now.*`, stub 14:14:45 |
| TLS | HTTPS to a Let's Encrypt host works with no setup (first request 251 ms). | run 1 `tls.healthz` |
| Default timeout | 15 s. A 14 s hold succeeds; a 16 s hold times out at 15.09 s. | run 2 `t.default.*`, run 1 `hold.default.25` (15.03 s) |
| Settable timeout | `RestContext.SetTimeout(s)`. Values of 3 s and up are honored (3 gave 3.10 s, 5 gave 5.07 s, 40 gave 40.10 s), and 300 let a 150 s hold complete. Below 3 the call is ignored without error (1 and 2 kept the default, so 3 s and 4 s holds succeeded). 0 means the default of 15 s. | run 2 `t.fresh.*`, run 1 `hold.t*` |
| Idle or whole-response timeout | **Idle.** A response trickling one byte a second for 20 s outlived a 10 s timeout (run 1), and one for 12 s outlived 5 s (run 2). One byte every 7 s under a 5 s timeout timed out at 5.07 s (run 2). DayZ's limit is on the whole response. | run 1 `drip.t10.20`, run 2 `t.5.drip*` |
| Non-2xx status and body | **Readable.** Each of 400, 401, 404, 409, 429, 500 and 503 reaches the error callback with `GetHttpCode()` equal to the status and `GetData()` equal to the body (result `EREST_ERROR_UNKNOWN`). 204 is a success with empty data. `?errors=inline` also works over TLS (200 carrying the error object). | run 1 `status.*`, `tls.session.*` |
| Redirects | Not followed. A 302 reaches the error callback with code 302 and its body. | run 1 `status.302` |
| Transport failures | Connection refused: an error after 2.07 s, http 0, `EREST_ERROR_UNKNOWN`. Unknown host: `EREST_ERROR_SEND` at once. Timeout: `EREST_ERROR_TIMEOUT`, http 0, empty data. | run 1 `fail.*`, `hold.*` |
| Engine log noise | The engine writes every non-2xx response to `console.log` as `BACKEND (E): [RestApi] ... Error Code:401 - Unauthorized` (17 lines in run 1). | run 1 console |
| Request headers | `SetHeaders("Name,value,Name,value")` returns true and the headers reach the wire. `Authorization: Bearer ...` can be sent, and `Content-Type` overridden. The `Name: value` form, one header or two separated by a newline, returns false and sends nothing. Headers belong to the context and persist on later requests. | run 1 `hdr.*`, stub lines 6 to 11 |
| Default request headers | `Accept: */*`, `User-Agent`, and an `Upgrade: h2c` offer (`Connection: Upgrade, HTTP2-Settings`). POST, PUT and DELETE default to `Content-Type: application/x-www-form-urlencoded`, which the hub refuses with 415 until the plugin sets `application/json`. | run 1 `get.echo`, `tls.session.401` (415) |
| Response headers | No API reads them. `RestCallback` offers the result, the status code, and the body. | `RestCallback.c` in the June 2026 script extract; the calls the probe used compiled and ran on 1.9 |
| Contexts | One per base URL. `GetContext` with a base URL already in use returns that context; the count stays the same. | run 1 `*.context` |
| Requests in flight | **One per host.** On two contexts against the same host and port, a POST issued alongside a 6 s hold reached the stub only after the hold answered (stub 14:14:33.430 then 14:14:39.430). A TLS request to a different host completed in 262 ms while a local 4 s hold was open. | run 1 `conc.*` and stub, run 3 `q.*` |
| Response size | Up to 1 048 575 bytes arrive. 1 048 576 and above fail with `EREST_ERROR_STORE`. | run 2 `resp.1m-1`, run 1 `resp.1m` |
| Request size | Bodies arrive whole to at least 16 MiB: 64 KiB, 256 KiB, 1 MiB, 4 MiB and 16 MiB, with each digest matching `'a' × n`. Multi-byte UTF-8 arrives intact (10 000 bytes of `é`). | run 4 `req.*` and stub, run 3 `req.utf8.5000x2` |
| URL length | A 2048-character request path works. A 10 000-character one **crashes the server** with an access violation inside `RestContext.GET`. The crash reporter then sent a report to Bohemia on its own, without asking. Lengths in between were not tried, to avoid more crashes. | run 3 `req.query.*`, `run2-crash.log` |
| Script strings | Hold at least 16 MiB. `Length()` counts bytes (4096 two-byte `é` give 8192). `Substring` caps its result at 8191 characters (`Substring(0, 8192)` of a 20 000-character string gives 8191). Building a string with `+=` in a loop is slow: about 6 s for a 100 000-number document. | run 4 `string.substring.8192`, run 3 `string.*`, run 1 timestamps |
| JSON | `JsonLoadContext.LoadFromString` with `ReadValue`, and `JsonSaveContext.WriteValue` with `SaveToString`. A 4 MiB string value parses in 6 ms and saves in 4 ms. A 1 MB array of 2000 strings parses in 1 ms, and 100 000 integers in 4 ms. Linear and complete. | run 4 `json.*`, run 1 `json.parse.ints.*` |
| Files | `$profile:` resolves to `<-profile>/profile/`. A path outside the sandbox roots is refused (`OpenFile` returns null). `FileMode.WRITE` creates and truncates. `OPENW` neither creates a missing file nor truncates an existing one. A 1 MiB line round-trips through `WriteLine`/`ReadLine` (`WriteLine` ends lines with CRLF on Windows). There is no rename: `CopyFile` then `DeleteFile` works but is not atomic. | run 4 `file.line.*`, run 1 `file.*` |
| Lifecycle hooks | On `SCR_BaseGameMode`, `OnGameStart` runs on the server (`RplMode.Dedicated`) about 14 s after the process starts. At join: `OnPlayerConnected`, `OnPlayerAuditSuccess`, `OnPlayerRegistered`. `OnPlayerDisconnected` fires as soon as a killed client drops. A graceful server stop was not measured; every run ended in a process kill. | runs 1 and 3 `hook.*` |
| Player identity | **Empty on this launch.** `SCR_PlayerIdentityUtils.GetPlayerIdentityId`, the obsolete `BackendApi.GetPlayerIdentityId`, `GetPlayerPlatformKind` (`NONE`) and `GetPlayerPlatformId` all returned nothing, though `OnPlayerAuditSuccess` fired. The game's own `SCR_PlayerIdentityUtils` says a dedicated server needs a backend connection (`publicAddress` in the server config) to have identities. A server started with `-config` loads mods only from the Workshop, so reading a real identity means publishing the probe as an unlisted Workshop item. **Open.** | run 3 `hook.OnPlayerAuditSuccess` |
| WebSocket or sockets | None found. `RestApi` is the only network surface script gets. Neither the 1.8.0.13 API index (`rwt api search`) nor the June 2026 script extract has a WebSocket or socket class. The 1.9 headers were not searched. | API index, `scripts/` extract |

## What this means for the Reforger plugin (#15)

- **The poll transport works as specified.** Set `SetTimeout(pollTimeout + 5)` on the hub's
  context. The protocol's 5 s to 60 s range is reachable, and so is the default of 25 s. The
  timeout is on idle time, so nothing changes: the protocol already forbids keepalive tricks.
- **Auth goes in the real header.** `SetHeaders("Authorization,Bearer <token>,Content-Type,application/json")`
  once per context. The header workarounds in `spec/protocol.md` Appendix A (query parameter,
  content-type smuggling) are not needed. Without the `Content-Type` override the hub
  answers 415.
- **Errors are readable.** The plugin can branch on `error.code` from the body (section 2.2)
  directly. `?errors=inline` works too but isn't needed. Each refusal leaves a
  `BACKEND (E)` line in the operator's console, so the README should say that a 429 or 409
  there is expected.
- **One request in flight per host.** While the poll is held, any other request to the hub
  (a key/value call, a ban-list page) waits for the poll to answer. The poll already carries
  both directions. A side call that cannot wait for `pollTimeout` would need the plugin to
  hold its next poll until the call answers.
- **Protocol change first: a response size bound.** A Reforger plugin cannot receive a
  response of 1 MiB or more, and nothing in the protocol caps a poll response in bytes. A
  batch may hold up to the hub's envelope cap (reference 200, section 9.2). The spec bounds
  no action's `params`; the reference hub's only limit is its 1 MiB request body on dispatch,
  so even one action can bring a response to the size Reforger rejects. Before the plugin
  is written, #15 needs a byte bound the plugin can ask for (or a protocol-wide one), with
  the hub cutting the batch where it would cross it, and a rule for one envelope too big to
  send at all. The same bound covers the ban-list page and any other large answer.
- **Keep URLs short.** A long request path crashes the server, so no data goes in a query
  string. The plugin's paths are a few dozen characters.
- **Strings: avoid `Substring` on long strings.** `Substring` returns at most 8191 characters,
  the same cap the DayZ plugin hit in #108. JSON is linear, so the windowed parser the DayZ
  plugin needed is not, but building a large string with `+=` is slow and should collect
  pieces and join once.
- **Files: `WRITE` to rewrite, never `OPENW`.** A crash-safe rewrite needs the plugin's own
  scheme, since there is no atomic rename (the DayZ plugin solved this in #121).
- **The platform registry entry waits.** The identifier a Reforger server sees is still
  unmeasured, so the entry in section 8.2 cannot be chosen from evidence yet. It needs one run
  on a backend-registered server with the probe published as an unlisted Workshop item.
- **WebSocket stays parked.** No socket API was found for Reforger script, so this game
  does not fire its trigger. A sidecar remains the only customer.
