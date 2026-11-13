# Vyshka Arma Reforger plugin

The second reference game plugin: a server-side Enfusion script addon that enrolls an Arma
Reforger dedicated server with a Vyshka hub, long-polls it for work, publishes a manifest,
executes dispatched actions, and reports players joining and leaving. Protocol:
`spec/protocol.md`.

This is the tracer bullet of issue #15: the whole link (enrollment, sessions, the long-poll
loop, the persistent outbox, acks and renumbering, the executed-action log, refusals and
backoff) with two actions, `vyshka.status` and `vyshka.kick`. The protocol code is the DayZ
plugin's, ported: the two games share the Enforce language, so the JSON tree, the outbox,
the files, and the link logic carry over with their measured limits, and only the engine
calls differ. What differs, and why, is in the head of each Reforger-specific file
(`VyshkaTransport.c`, `VyshkaText.c`, `VyshkaGameMode.c`).

Clean-room: written from the engine's public script API and the measurements under
`spikes/reforger-server-script-limits` and `spikes/reforger-player-identity`, per
`CONTRIBUTING.md`.

**Game version.** Reforger 1.9.0.37, Windows dedicated server.

## Layout

| Path | What it is |
|---|---|
| `addon/Vyshka/Vyshka.gproj` | The addon (GUID `8EF1E2D9CB0C677C`) |
| `addon/Vyshka/scripts/Game/Vyshka/` | The plugin. `VyshkaGameMode.c` hooks `SCR_BaseGameMode` to start and stop it, reports players, and declares the actions; `VyshkaTransport.c` is the HTTP layer on Reforger's `RestApi`; the rest is the DayZ plugin's protocol code |
| `cmd/vyshka-reforger/` | Developer tool: `harness` runs a dedicated server as a plugin conformance candidate |

## Installing on a server

1. Create the server record on the hub with `"game": "reforger"` and take its one-time
   enrollment token (`POST /api/v1/servers`, or the panel).
2. Create `$profile:Vyshka/config.json`, which is `<profile>/profile/Vyshka/config.json`
   for a server started with `-profile <profile>`:

   ```json
   {
     "hubUrl": "https://hub.example.net",
     "enrollmentToken": "<one-time enrollment token>",
     "pollTimeoutSeconds": 25
   }
   ```

   `pollTimeoutSeconds` is optional (default 25, honored between 5 and 60).
3. Load the addon. A server started with `-config` loads mods only from the Workshop, and
   this addon is not published yet, so today it loads through the development form:

   ```
   ArmaReforgerServer.exe -server worlds/GameMaster/GM_Arland.ent -addonsDir <dir holding Vyshka> -addons 8EF1E2D9CB0C677C -profile <profile>
   ```

   Run it from the server's install folder.

On first boot the plugin exchanges the token for permanent credentials and keeps them in
`$profile:Vyshka/credentials.json`. Everything it says goes to the server's
`<profile>/logs/<timestamp>/console.log`, prefixed `[Vyshka]`. The engine also writes each
failed request there as a `BACKEND (E): [RestApi]` line; an occasional poll `Timeout` among
them is expected.

## Players

A player is `{ "platform": "reforger", "id": "<backend identity UUID>" }` (spec section 8.2).
The server reads that identity only when it is registered with the game's backend (started
with `-config`) and only from the player's audit onward. A server started in the
development form reads none, so its players are not reported: the plugin logs once why, and
`vyshka.status` counts players online beside the ones identified.

## Actions

| Code | Context | What it does |
|---|---|---|
| `vyshka.kick` | player | Disconnects the player whose identity is the `referenceKey` |
| `vyshka.status` | world | Reports the world, the players online and identified, and the uptime |

A mod that loads after this addon adds its own by overriding
`SCR_BaseGameMode.VyshkaRegister(VyshkaRegistry registry)`, calling `super` first.

## Conformance

```
go run ./conformance/plugin -enroll-wait 240s -check-timeout 30s -- go run ./plugins/reforger/cmd/vyshka-reforger harness -server <server install>
```

`harness` reads the hub URL and token the suite puts in the environment, writes them into a
fresh profile under `plugins/reforger/build/`, starts the server on Game Master Arland with
the addon, mirrors the plugin's log lines to stderr, and stops the server when the suite
closes its stdin. `VYSHKA_REFORGER_SERVER` can stand in for `-server`. It needs a local
dedicated server install (Steam app 1874900), so CI does not run it.

## Not yet

The installation ban list (section 13, so the manifest does not declare `bans`), the
key/value store, state snapshots, custom contexts, and a Workshop release.
