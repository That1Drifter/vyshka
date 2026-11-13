# Findings: the player identity a Reforger server reads (issue #159)

Measured 2026-11-13 on the Arma Reforger dedicated server **1.9.0.37** (Windows 11), started
with `-config` so that it registered with the game's backend, and one game client (Steam,
1.9.0.37) that joined it through the game's own Direct Join. The server loaded the probe from
the Workshop: `VyshkaRfIdentity`, GUID `A8949793F2092C16`, published unlisted for this run.

There is one join. Its lines are in `run1-server.log`. `setup-boots.log` holds the three
earlier boots of the same session and the failed `-client` attempt. Personal values in both
files are replaced by placeholders. The replacement was an exact-string substitution of one
UUID and one SteamID64, so every `<uuid A: ...>` stands for the same value, and every
`<steamid64 A: ...>` for the same value. The placeholder text records what was checked
on the original before it was replaced. Both the `identity=` and the `legacy=` reads held
that one UUID, it was all lowercase, its version digit was `4`, and the SteamID64 had 17
digits.

## Summary

| Question | Answer | Evidence |
|---|---|---|
| Identity on a backend-registered server | **Present.** `SCR_PlayerIdentityUtils.GetPlayerIdentityId` (a `UUID`) and `BackendApi.GetPlayerIdentityId` (a `string`) return the same value: a 36-character UUID in lowercase, hyphenated 8-4-4-4-12 form, version 4. | `run1-server.log`, `hook.OnPlayerAuditSuccess` |
| Platform kind and id | `BackendApi.GetPlayerPlatformKind` gives `STEAM`. `BackendApi.GetPlayerPlatformId` gives the player's SteamID64 in decimal, 17 digits. | `hook.OnPlayerAuditSuccess` |
| When it becomes readable | **At `OnPlayerAuditSuccess`, not before.** In `OnPlayerConnected` (3 ms earlier) and `OnPlayerRegistered` (1 ms earlier), every read was empty and the kind was `NONE`. The game's own `SCR_PlayerIdentityUtils` says the same in a diagnostic message: the identity "can only be used on the server and after OnPlayerAuditSuccess". | `run1-server.log` ticks 190065, 190067, 190068 |
| Hook order | Connected, Registered, AuditSuccess on this server. The dev-launched server of the earlier spike logged Connected, AuditSuccess, Registered (`spikes/reforger-server-script-limits/results/run3-probe.log`). A plugin must not rely on the order of the last two. | both logs |
| Stable within a session | Yes. The same values were read 10 s after the audit and in `OnPlayerDisconnected`. That hook ran after the server's `Disconnected` event (`reason=4`), which followed the client process being killed. | `later.10s`, `hook.OnPlayerDisconnected` |
| Stable across sessions | **Not measured.** A second join was attempted and abandoned (see below). | |
| Console players | **Not measured.** No console client was available. The `GetPlayerPlatformId` doc comment in the script API says Xbox and PlayStation ids come back SHA-256 hashed. | `scripts/GameLib/generated/online/BackendApi.c` |
| Audit failure, timeout, revival | **Not observed.** `OnPlayerAuditFail` was hooked and did not fire. `OnPlayerAuditTimeouted` and `OnPlayerAuditRevived` were not hooked. The engine's comment on `OnPlayerAuditFail` says a player can stay in the game unverified when authentication "is not required or possible". | `BaseGameMode.c` in the script extract |

The dev-launched server of the earlier spike (`-server` world, `-addons`, no `-config`) read
every value empty, even in `OnPlayerAuditSuccess` (run 3). The difference here is the
backend registration.

**What the script API says, read alongside the run.** Every read used here goes through an
engine call marked `[Obsolete]`. `BackendApi.GetPlayerIdentityId`, `GetPlayerPlatformKind`
and `GetPlayerPlatformId` carry the attribute. `SCR_PlayerIdentityUtils.GetPlayerIdentityId`,
the game's own wrapper, calls the first of them. `PlayerManager.GetPlatformKind` is a
non-obsolete way to read the platform kind. The game's ban service takes the UUID:
`ServerBanManifest.SetIdentityId(UUID)` and `ServerBanManagerApi.IsPlayerActionBanned(UUID, ...)`.
The server config's `admins` list accepts either the UUID or a 17-digit SteamID
(`backend/DSConfigSchema.json`).

## What this means for the protocol and the plugin (#15)

- **Registry entry: `reforger`.** The Reforger plugin identifies a player as
  `{ "platform": "reforger", "id": "<identity UUID>" }`, lowercased. Two reasons. The UUID
  is the key the game's own ban service uses, so a ban the hub holds maps onto it
  directly. And a SteamID64 exists only for Steam players, while the UUID is what the game
  gives a player of any platform. That second reason rests on the API's shape, not on a
  measurement, because no console player was measured. Obsolescence is not a reason: both
  values come through obsolete calls. The cost: a person who plays DayZ (`steam`) and
  Reforger is two identities in one hub, and a `steam` ban does not reach a Reforger
  server.
- **Report a player only after the audit.** Before `OnPlayerAuditSuccess` the identity is
  empty, and an empty `id` is outside section 8.3's bounds: a snapshot entry with one
  rejects the whole snapshot. Player-join events and the player list are built from the
  audit hook, not from `OnPlayerConnected`.
- **A server that is not backend-registered has no identities.** That covers a server
  started the way Workbench starts one, and probably one whose registration failed (not
  measured). On such a server every player reads empty. The plugin leaves such players
  out of `state.players` and logs that it cannot identify them. No ban can reach a player
  without an identity, so the plugin logs each one it admits unidentified (section 13.4,
  item 7).
- **Bans are enforced at the audit**, the first moment the identity exists.

## How the run was set up, and what did not work

Each item cites `setup-boots.log` unless it says otherwise.

- The server config (kept out of the repo; the README has its shape) named the scenario
  `{2BBBE828037C6F4B}Missions/22_GM_Arland.conf` and listed the mod by GUID under
  `game.mods`. It also set `game.visible` to false, turned BattlEye off, and disabled the
  crash reporter. The server downloaded the mod (`Addon Download started
  A8949793F2092C16`, boot 09:21:20) and registered with the backend.
- The server must start with its install folder as the working directory. Started from
  the run folder, it reported `Game addon '58D0FB3206B6F859' not found` (boot 09:21:10;
  the log does not show the working directory, and the next boot, from the install
  folder, found it).
- **`ArmaReforgerSteam.exe -client <ip>` did not join the backend-registered server.** The
  server logged `Server L2 authentication timeout` and `reason=3` 30 s after the
  connection began, and the client reported `Unable to connect` (boot 09:21:20, and the
  client lines at the end). The likely cause, inferred and not logged, is that this
  launch skips the backend login. It was tried once, against the boot registered under
  the public address, and not against the LAN-registered one. The join that worked went
  through the game's menu: Multiplayer, then Direct join.
- **Direct join looks the address up in the backend.** With `publicAddress` left to
  auto-detection, the server registered its public address (boot 09:21:20). Direct join to
  `127.0.0.1:2001` answered "No server found", which was seen on screen and is not in any
  log. Registering the LAN address (boot 09:29:04) and joining that address worked.
- **The menu's text fields ignore synthetic keys.** Under scripted input, the join-code
  field and the password field stayed in edit mode and their buttons stayed disabled (seen
  on screen). So the measured boot (09:30) ran with no password. It was hidden from the
  browser and registered under a LAN address.
- **A second join was abandoned.** A scripted click landed on a public server in the list,
  and the client began downloading that server's mods into the run's scratch folder before
  it was stopped. The folder was deleted. The client never connected to that server.
