# Spike: Reforger dedicated-server script limits (issue #159)

What a Reforger dedicated server's script can do over HTTP, with files and JSON, at its
lifecycle hooks, and with player identity, measured before the Reforger plugin (#15) is
designed. This follows the DayZ pattern in `spikes/dayz-restapi-poll-timeout`.

Findings: [`results/findings.md`](results/findings.md).

## Layout

| Path | What it is |
|---|---|
| `stub/main.go` | HTTP stub: `/echo` (returns what arrived), `/hold` (silence for `s` seconds), `/drip` (a byte every `every` seconds), `/status` (any code with a JSON body), `/big` (an exact-size body, chunked with `chunked=1`), `/redirect`, `/nocontent`. Logs one line per request with every header. A leading `/c<N>/` path segment is stripped, so the probe can use a separate REST context per test group (the log shows the path after the strip). |
| `addon/VyshkaRfProbe/` | The probe addon (`VyshkaRfProbe.gproj`, GUID `9F8A511CD8E849D7`). `Scripts/Game/VyshkaRfProbe.c` holds six step lists (`RUN` selects one) and a `modded class SCR_BaseGameMode` that starts the probe on the server and prints the lifecycle and identity hooks. |
| `results/` | `runN-probe.log` (the server's `VYSHKA_RF` lines), `runN-stub.log`, `run1-backend-errors.log`, `run2-crash.log`, and the write-up. |

## Reproducing

1. Install the dedicated server (Steam app 1874900, anonymous):

   ```
   steamcmd +force_install_dir C:\reforger-server +login anonymous +app_update 1874900 validate +quit
   ```

2. Start the stub:

   ```
   go run ./spikes/reforger-server-script-limits/stub -addr 127.0.0.1:8099
   ```

3. Runs 5 and 6 hold a real poll on a hub over TLS (`TLS` in the probe names the host).
   Register a throwaway server there (`POST /api/v1/servers`), enroll it, open a session
   with `"pollTimeoutSeconds": 10`, and write the session token on one line to
   `<scratch>\profile\profile\vyshka-probe\session.txt`. Revoke the server's credentials
   afterwards (`DELETE /api/v1/servers/{id}/credentials`).

4. Set `RUN` in `VyshkaRfProbe.c` to the run to repeat, then start the server with the addon.
   This is the Workbench dedicated-server plugin's form. A server started with `-config`
   loads mods only from the Workshop.

   ```
   ArmaReforgerServer.exe -server "worlds/GameMaster/GM_Arland.ent" ^
     -addonsDir <repo>\spikes\reforger-server-script-limits\addon -addons 9F8A511CD8E849D7 ^
     -profile <scratch>\profile -nothrow -maxFPS 60
   ```

5. Read the results from the engine's log (stdout is buffered):

   ```
   findstr VYSHKA_RF <scratch>\profile\logs\<timestamp>\console.log
   ```

   Run 1 takes about six minutes and the others one to three, each ending with `probe.done`.
   Run 3 waits up to ten minutes for a client. To provide one, start the game with
   `ArmaReforgerSteam.exe -addonsDir <same> -addons 9F8A511CD8E849D7 -window -client -profile <scratch>\client`
   while Steam is running.

Run 2 ends by crashing the server: step 19's request path was 8198 characters long, cut
down from the intended 10 000 by the `Substring` cap that run 3 found. The crash reporter
sends a report to Bohemia without asking, so don't repeat it without reason.

## Provenance

Clean-room: the probe, the stub, and the measurements are original work. They were written
from the script API the game ships (`scripts/GameLib/generated/online/Rest*.c`, the
`SCR_BaseGameMode` hooks) and from observed behavior. No third-party integration product was
consulted.
