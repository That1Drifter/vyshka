# Vyshka Arma Reforger plugin

The second reference game plugin: a server-side Enfusion script addon that enrolls an Arma
Reforger dedicated server with a Vyshka hub, long-polls it for work, publishes a manifest,
executes dispatched actions, and reports players: joining and leaving, and the list of
players online with their positions. Protocol: `spec/protocol.md`.

This is the tracer bullet of issue #15: the whole link (enrollment, sessions, the long-poll
loop, the persistent outbox, acks and renumbering, the executed-action log, refusals and
backoff) with two actions, `vyshka.info` and `vyshka.kick`. The protocol code is the DayZ
plugin's, ported: the two games share the Enforce language, so the JSON tree, the outbox,
the files, and the link logic carry over with their measured limits, and only the engine
calls differ. What differs, and why, is in the head of each Reforger-specific file
(`VyshkaTransport.c`, `VyshkaText.c`, `VyshkaGameMode.c`).

Clean-room: written from the engine's public script API and the measurements under
`spikes/reforger-server-script-limits` and `spikes/reforger-player-identity`, per
`CONTRIBUTING.md`.

**Game version.** Reforger 1.9.0.37, Windows dedicated server.

**Workshop.** Published unlisted as "Vyshka", version 0.1.1, Workshop ID
`8EF1E2D9CB0C677C` (the addon GUID). Unlisted keeps it out of search; a server loads it by
that ID.

## Layout

| Path | What it is |
|---|---|
| `addon/Vyshka/Vyshka.gproj` | The addon (GUID `8EF1E2D9CB0C677C`) |
| `addon/Vyshka/scripts/Game/Vyshka/` | The plugin. `VyshkaGameMode.c` hooks `SCR_BaseGameMode` to start and stop it, reports players, and declares the actions; `VyshkaTransport.c` is the HTTP layer on Reforger's `RestApi`; the rest is the DayZ plugin's protocol code |
| `cmd/vyshka-reforger/` | Developer tool: `harness` runs a dedicated server as a plugin conformance candidate |
| `workshop/preview.png` | The Workshop preview image |

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
   `snapshotIntervalSeconds` is optional (default 10, honored between 2 and 600; `0` turns
   the `state.players` snapshot off).
3. List the addon in the server config's `game.mods`, and start the server with `-config`
   as usual:

   ```json
   "mods": [ { "modId": "8EF1E2D9CB0C677C", "name": "Vyshka" } ]
   ```

   The server downloads it from the Workshop at boot, and a client downloads it when it
   joins, as with any server mod. Everything it does is behind `Replication.IsServer()`,
   so it is idle on a client connected to a server.

   For development, a local copy loads without the Workshop through
   `ArmaReforgerServer.exe -server worlds/GameMaster/GM_Arland.ent -addonsDir <dir holding Vyshka> -addons 8EF1E2D9CB0C677C -profile <profile>`,
   run from the server's install folder. A server started that way reads no player
   identities (below).

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
`vyshka.info` counts players online beside the ones identified. On such a server
`state.players` is an empty list, the spec's "nobody is online", whoever is connected.

- `core.player.connect` and `core.player.disconnect` carry `player` and `name`. The connect
  is sent at the audit; the disconnect only for a player whose connect was sent.
- `state.players` lists every identified player online, captured as each poll is built and
  at least `snapshotIntervalSeconds` apart. An entry carries `name` (at most 200
  characters), `data.spawned` (whether the player controls something; false in the
  deployment menu), and, when spawned, `position`, the controlled entity's origin as the
  engine's `[x, y, z]` with y up. `data.alive` comes with it when that entity is a
  character. Measured: a spawned, living character's position matched its map grid. Not
  measured: `alive: false`, and a character riding in a vehicle.
- The plugin records a player at the audit and forgets every record when a game ends. A
  player carried into a new game without a fresh audit would go unlisted until they
  reconnect; whether the engine ever does that is unmeasured.

## Actions

| Code | Context | What it does |
|---|---|---|
| `vyshka.kick` | player | Disconnects the player whose identity is the `referenceKey`. The game shows the player "Voted out of the game" (the engine's wording for a `KICK`) |
| `vyshka.info` | world | Reports the world, the players online and identified, and the uptime |

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

## Demo

`plugins/reforger/demo-live.sh <hub-url> <server install>` (with `VYSHKA_ADMIN_TOKEN` set)
creates a server record, boots a dedicated server with the addon through `harness`, waits
for the manifest, dispatches `vyshka.info`, prints the result the game returned, and
revokes the record's credentials. It lives here rather than under `scripts/demo-*`
because `scripts/e2e.sh` runs those without a game server.

## Not yet

The installation ban list (section 13, so the manifest does not declare `bans`), the
key/value store, the vehicle, entity and world snapshots, and custom contexts.
