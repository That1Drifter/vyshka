# Spike: Reforger dedicated-server script limits (issue #159)

What a Reforger dedicated server's script can do over HTTP, with files and JSON, at its
lifecycle hooks, and with player identity, measured before the Reforger plugin (#15) is
designed. This follows the DayZ pattern in `spikes/dayz-restapi-poll-timeout`.

Findings: [`results/findings.md`](results/findings.md).

## Layout

| Path | What it is |
|---|---|
| `stub/main.go` | HTTP stub: `/echo` (returns what arrived), `/hold` (silence for `s` seconds), `/drip` (a byte every `every` seconds), `/status` (any code with a JSON body), `/big` (an exact-size body), `/redirect`, `/nocontent`. Logs one line per request with every header. A leading `/c<N>/` path segment is stripped, so the probe can use a separate REST context per test group. |
| `addon/VyshkaRfProbe/` | The probe addon (`VyshkaRfProbe.gproj`, GUID `9F8A511CD8E849D7`). `scripts/Game/VyshkaRfProbe.c` holds four step lists (`RUN` selects one) and a `modded class SCR_BaseGameMode` that starts the probe on the server and prints the lifecycle and identity hooks. |
| `results/` | `runN-probe.log` (the server's `VYSHKA_RF` lines), `runN-stub.log`, `run2-crash.log`, and the write-up. |

## Reproducing

1. Install the dedicated server (Steam app 1874900, anonymous):

   ```
   steamcmd +force_install_dir C:\reforger-server +login anonymous +app_update 1874900 validate +quit
   ```

2. Start the stub:

   ```
   go run ./spikes/reforger-server-script-limits/stub -addr 127.0.0.1:8099
   ```

3. Set `RUN` in `VyshkaRfProbe.c` to the run to repeat, then start the server with the addon.
   This is the Workbench dedicated-server plugin's form. A server started with `-config`
   loads mods only from the Workshop.

   ```
   ArmaReforgerServer.exe -server "worlds/GameMaster/GM_Arland.ent" ^
     -addonsDir <repo>\spikes\reforger-server-script-limits\addon -addons 9F8A511CD8E849D7 ^
     -profile <scratch>\profile -nothrow -maxFPS 60
   ```

4. Read the results from the engine's log (stdout is buffered):

   ```
   findstr VYSHKA_RF <scratch>\profile\logs\<timestamp>\console.log
   ```

   Run 1 takes about six minutes and ends with `probe.done`. Run 3 waits up to ten minutes for
   a client. To provide one, start the game with
   `ArmaReforgerSteam.exe -addonsDir <same> -addons 9F8A511CD8E849D7 -window -client -profile <scratch>\client`
   while Steam is running.

Run 2 ends by crashing the server on purpose (a 10 000-character request path), and the
crash reporter sends a report to Bohemia without asking. Don't repeat it without reason.

## Provenance

Clean-room: the probe, the stub, and the measurements are original work. They were written
from the script API the game ships (`scripts/GameLib/generated/online/Rest*.c`, the
`SCR_BaseGameMode` hooks) and from observed behavior. No third-party integration product was
consulted.
