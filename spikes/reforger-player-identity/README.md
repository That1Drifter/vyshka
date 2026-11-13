# Spike: the player identity a Reforger server reads (issue #159)

The one question `spikes/reforger-server-script-limits` left open: what player identity a
Reforger dedicated server's script can read once the server is registered with the game's
backend. A server started the way Workbench starts one reads every identity empty.

Findings: [`results/findings.md`](results/findings.md).

## Layout

| Path | What it is |
|---|---|
| `addon/VyshkaRfIdentity/` | The probe addon (GUID `A8949793F2092C16`, published unlisted to the Workshop as "Vyshka Identity Probe"). A `modded class SCR_BaseGameMode` prints the identity reads at each join hook, 10 s after the audit, and at disconnect. It sends nothing and adds no content. The committed source differs from the published 1.0.0 only in its header comment. |
| `preview.png` | The Workshop preview image. |
| `results/run1-server.log` | The measured boot: its `VYSHKA_RFID` lines and its backend-registration and connection lines, with the identity, the Steam id, the name, and the LAN address replaced by placeholders. |
| `results/setup-boots.log` | The same lines from the three earlier boots of the session, and the client side of the failed `-client` join, redacted the same way. |

## Reproducing

1. A server config with the mod listed by GUID, the server hidden, and `publicAddress` set
   to the machine's LAN address:

   ```json
   {
     "bindAddress": "0.0.0.0", "bindPort": 2001, "publicAddress": "<lan ip>", "publicPort": 2001,
     "game": {
       "name": "identity probe", "scenarioId": "{2BBBE828037C6F4B}Missions/22_GM_Arland.conf",
       "maxPlayers": 4, "visible": false, "gameProperties": { "battlEye": false },
       "mods": [ { "modId": "A8949793F2092C16" } ]
     },
     "operating": { "disableCrashReporter": true }
   }
   ```

2. Start the server from its install folder:

   ```
   cd <server install>
   ArmaReforgerServer.exe -config <config.json> -profile <scratch>\profile -addonDownloadDir <scratch>\dl -maxFPS 60
   ```

   Wait for `Server registered with address` in `<scratch>\profile\logs\<timestamp>\console.log`.

3. Start the game normally (a `-client` launch did not join, see the findings), open
   Multiplayer, Direct join, and enter the LAN address and port 2001. The client downloads
   the mod from the Workshop.

4. `findstr VYSHKA_RFID <scratch>\profile\logs\<timestamp>\console.log`.

To publish a changed probe, open the project in Workbench and use File > Publish to
Workshop. `rwt workshop preflight` checks the addon first.

## Provenance

Clean-room: the probe and the measurement are original work, written from the script API the
game ships (`BackendApi`, `SCR_PlayerIdentityUtils`, the `SCR_BaseGameMode` hooks) and from
observed behavior. No third-party integration product was consulted.
