# Vyshka DayZ plugin

The reference game plugin for DayZ: a server-side Enforce Script mod that enrolls a DayZ
dedicated server with a Vyshka hub, long-polls it for work, publishes a manifest, executes
dispatched actions, and publishes telemetry: the core player events a feed needs and the
`state.players` snapshots a live map needs. It ships nine built-in actions (heal, kick, ban,
unban, message, broadcast, teleport, spawn, set time), so an operator can moderate a server
and move things around it from the panel or a `curl` against the hub. Protocol:
`spec/protocol.md`.

Clean-room: written from the engine's public script headers and the measurements under
`spikes/`, per `CONTRIBUTING.md`.

## Layout

| Path | What it is |
|---|---|
| `mod/config.cpp` | Addon and script-module registration |
| `mod/scripts/3_Game/Vyshka/` | Protocol code: JSON, clock and ids, files, outbox, transport, the link itself, the action registry, the event buffer, the ban list |
| `mod/scripts/4_World/Vyshka/` | Game-facing code: the heal action, the moderation actions, the position and world actions, the player roster and telemetry (`VyshkaPlayerTelemetry`) |
| `mod/scripts/5_Mission/Vyshka/` | The `MissionServer` hooks that start and stop the plugin and feed it connects, disconnects, and chat |
| `pbo/` | Go package that packs and reads PBO archives |
| `cmd/vyshka-dayz/` | Developer tool: `build` packs the mod, `harness` runs a server as a conformance candidate |

## Building

```
go run ./plugins/dayz/cmd/vyshka-dayz build
```

writes `plugins/dayz/build/@Vyshka/addons/Vyshka.pbo` and a `mod.cpp`. No DayZ Tools are
needed: the packer is pure Go and the mod is script only. The PBO is unsigned, which is fine
for a server-side mod; clients never load it.

## Installing on a server

1. Copy `@Vyshka` into the DayZ dedicated server directory.
2. Create the server record on the hub and take its one-time enrollment token
   (`POST /api/v1/servers`, or the panel once it exists).
3. Create `<profiles>/Vyshka/config.json`, where `<profiles>` is the directory the server's
   `-profiles=` argument names:

   ```json
   {
     "hubUrl": "https://hub.example.net",
     "enrollmentToken": "<one-time enrollment token>",
     "pollTimeoutSeconds": 25
   }
   ```

   `pollTimeoutSeconds` is optional (default 25, honored between 5 and 60). `game` is optional
   and defaults to `dayz`. `snapshotIntervalSeconds` is optional (default 10, honored between
   2 and 600; `0` turns `state.players` snapshots off). `fpsIntervalSeconds` is optional
   (default 60, honored between 5 and 3600; `0` turns `core.server.fps` samples off).
4. Start the server with the mod as a server mod:

   ```
   DayZServer_x64.exe -config=serverDZ.cfg -profiles=<profiles> -serverMod=@Vyshka -dologs
   ```

On first boot the plugin exchanges the token for permanent credentials and writes them to
`<profiles>/Vyshka/credentials.json`. The token is burned at that point; the file is what
the server uses from then on. To re-enroll after a revocation, put a fresh token in
`config.json`: the plugin notices it differs from the one the credentials came from and
enrolls again.

Everything the plugin says goes to the server's script log (`script_*.log` under the
profile directory) prefixed `[Vyshka]`, including manifest rejections and the link state
(`connected`, `degraded`, `buffering`).

## Actions

With the server enrolled and a player online:

```
curl -X POST https://hub.example.net/api/v1/servers/<serverId>/actions \
  -H "Authorization: Bearer <admin token>" -H "Content-Type: application/json" \
  -d '{"code":"vyshka.heal","context":"player","referenceKey":"<Steam64 id>","params":{}}'
```

The `referenceKey` of a player-context action is the player's plain Steam64 id, the same
identity the telemetry publishes. The manifest (revision 3) declares:

| Code | Context | Danger | Params | Result |
|---|---|---|---|---|
| `vyshka.heal` | player | none | `restoreBlood` (default true) | `health`, `blood`, `shock`, `name` after the heal |
| `vyshka.kick` | player | warning | `reason` | `name`, `reason`; the player is disconnected through the engine's own disconnect call and `core.player.kick` is emitted |
| `vyshka.ban` | player | destructive | `reason`, `durationMinutes` (0, the default, is permanent) | `player`, `name` (when known), `kicked`, `expiresAt`, `activeBans`; the identity goes on the ban list, the player is kicked if online, and `core.player.ban` is emitted. The player need not be online: an offline identity is banned by its plain Steam64 id |
| `vyshka.unban` | player | warning | none | `removed` (the entry), `activeBans`; fails when the identity is not banned. Emits `vyshka.player.unban` |
| `vyshka.message` | player | none | `message` (required), `title`, `seconds` (1 to 60, default 10), `style` (`notification`, the default, or `chat`) | `name`, `style` |
| `vyshka.broadcast` | world | none | the same | `recipients`, `style` |
| `vyshka.teleport` | player | warning | exactly one of `position` (`[x, y, z]`, or `[x, z]` placed on the terrain), `toPlayer` (a Steam64 id), `previous` (true) | `name`, `mode` (`position`, `player`, `previous`), `from`, `to`, and `toPlayer` with `toPlayerName` when a player was the destination, `vehicle` when the player's vehicle was moved with them |
| `vyshka.spawn` | player | warning | `className` (required) | `className` (as the engine reports it), `displayName`, `config` (the tree that declares it), `position`, `name`; one item is created on the ground in front of the player |
| `vyshka.settime` | world | warning | `hour` (0 to 23, required), `minute` (0 to 59, default 0) | `before` and `after`, each `{ year, month, day, hour, minute }` read from the world clock |

Kick, message, teleport, spawn, and the ban's own kick need the player online and fail with
`player <id> is not online` otherwise.

**Kicks** run the mission's own logout finalization (the same code a logout timer running
out reaches): the disconnect hook fires, so `core.player.disconnect` follows the kick event,
the character is saved, and the body is handled before the engine drops the client. A
player already counting down a logout is retired from the logout queues first, and a queued
logout registration whose player is no longer pending (withdrawn by a kick, or by the
engine's own logout cancellation) is refused, so the timer cannot finalize the same
character again. The
engine's bare disconnect call alone does none of that, measured on DayZ 1.29 while building
this slice: it drops the connection, fires no disconnect event, and leaves the plugin's
roster believing the player is still there. A `reason` is cut to 200 characters, a `message` to 1000, a `title` to
100: the schema subset of protocol section 6.1 has no length keyword, so the plugin bounds
them itself rather than refuse.

**Messages** use what every vanilla client already renders, so nothing needs installing on
the client: the `notification` style is the engine's own pop-up (the title over the message,
or the message alone as the headline, for `seconds`), and the `chat` style is a line in the
player's chat window in the engine's "important" color, `title: message` when a title is
given.

**Bans** are the plugin's own list, because the engine exposes no scripted ban API (only a
disconnect call): `<profiles>/Vyshka/bans.json`, one entry per identity with the name the
player had, the reason, when the ban was made, when it expires (`null` for permanent), and
the `actionId` that made it. A banned identity that connects is kicked as soon as its
character attaches, the earliest hook that has something to disconnect, so the feed shows
the attempt (`core.player.connect`) and the refusal (`core.player.kick` with `cause: "ban"`)
in order; the ban is looked up again at that moment, so one lifted or expired in between is
no ban. Expired entries are dropped when the list is loaded, when the identity is next
looked up, and before a result counts `activeBans`. The list is independent of the engine's
and BattlEye's own ban lists and an operator can edit it while the server is down: a `null`
or absent `expiresAt` is permanent, a timestamp is honored whatever it says (one in the past
lifts the ban), one that does not parse is treated as permanent so a typo cannot lift a ban,
and every text member is cut to the same bounds a dispatch gets. A file that does not parse
is left alone, enforces nothing, and makes the ban and unban actions refuse until it is
fixed or removed, which the log says at boot. The plugin's clock is a 32-bit epoch: a
duration that would end after 2038-01-19T03:14:07Z, or a timestamp written past it, is read
as that instant rather than wrapped into the past.

**Teleports** move the character with the engine's own position call, the one its restricted
area enforcement and its developer tooling use; a player seated in a vehicle is moved with the
vehicle and everyone in it, which the result reports as `vehicle`. A `position` is in the
frame the snapshots publish (`[x, y, z]`, `y` the elevation in metres); given as `[x, z]` it
is placed on the terrain at that point. A position off the map (outside `0` to the world
size on `x` and `z`) or above 10 000 m is refused, and one below the terrain, or below sea
level, is lifted up to it: measured on DayZ 1.29 while building this slice, the engine
leaves a character exactly where an explicit `y` puts it, 280 m under the ground included,
where it is stranded rather than moved. The engine's own teleport lifts to sea level only;
the terrain lift is the plugin's, and its cost is that an interior below the heightmap
cannot be a teleport destination. `toPlayer` puts the player 1.5 m beside the other player,
at that player's elevation rather than the terrain's, so a player standing on a floor is
not placed under it, and a target standing somewhere the plugin would refuse to send a
player (off the map after the step aside is refused too) fails the action rather than
copying the bad position. Every teleport records where what moved stood (the vehicle's
position when a vehicle was moved, the character's otherwise, so the record and the move
are in one frame and repeated undos swap between two places instead of drifting by a seat
offset) as that identity's previous position, one per identity, and `previous` goes back
there: the undo of a teleport, and a second `previous` undoes the undo. The record is in
memory only; a restart forgets it, and `previous` then fails until something has teleported
the player again.
The schema subset of protocol section 6.1 cannot say "exactly one of", so a dispatch naming
no destination, or more than one, is refused by the plugin.

**Spawn** creates one object of `className` on the ground 1.5 m in front of the player, with
the placement flags the central economy uses (traced to the surface, physics on, navmesh
updated) and the item's default quantity and condition; the inventory target, quantity,
health, and attachments belong to the spawning extension slice. The name must be made of
letters, digits, and underscores, and must be a public class (`scope` 2) declared in
`CfgVehicles`, `CfgWeapons`, or `CfgMagazines`; anything else is refused before the engine
is asked, with the reason in the result. That admits every vanilla and modded item, and also
vehicles, infected, and animals, which is what an event host wants; a blocklist arrives with
the spawning extension. `className` in the result is the class as the engine reports it.

**Set time** writes the world clock through the engine's `SetDate`, keeping the year, month,
and day and replacing the hour and minute; the clients follow the server's clock as they
always do. The result carries the clock as read back after the write, so what the engine
made of the request is what is reported: on DayZ 1.29 a request for 03:15 read back as
03:14 and 14:30 as 14:30, so expect the minute to land within one of what was asked.

## Telemetry

The plugin publishes events (protocol section 8.1) and `state.players` snapshots (section
8.3) as soon as it is running; nothing needs configuring. Player identity everywhere is
`{ "platform": "steam", "id": "<Steam64>" }` (section 8.2), the same id the heal action
takes as its `referenceKey`.

**Events.** Buffered and flushed as one `event.batch` every 2 s or at 200 events, then
persisted, acked, and renumbered like every other envelope. A crash loses the unflushed
tail of that buffer: up to 2 s or 200 events, plus the 200 ms tick that notices a flush is
due, for a script thread that keeps ticking (a stalled server holds its pending events for
the length of the stall). Measured by killing the server under load
(`spikes/dayz-outbox-crash`), that tail was the only loss: every batch that had reached
the outbox was delivered after the restart, none was torn, and the loss ran from
0.1 s to 2.1 s of events. A kill that lands inside the outbox's own
write of a batch would lose that batch too; it did not happen in those trials and the
restart discards the unreadable record. A poll carries at most 1000 events across its batches, the
reference hub's per-poll budget, so a backlog flushed after an outage is never refused
over it. A batch or snapshot the hub does refuse (`event.reject`, `state.reject`) is gone;
the plugin logs the hub's reasons as `ERROR` lines and carries on.

| Type | When | `data` |
|---|---|---|
| `core.server.start` | The plugin starts with the mission | `game`, `plugin { name, version }`, `world` |
| `core.server.stop` | The mission finishes; written to the outbox and delivered by the next boot, ahead of that boot's start event | same |
| `core.player.connect` | A character is attached to a newly connected identity (first join); a respawn or a reconnect inside the logout window is not a second connect | `player`, `name` |
| `core.player.disconnect` | The logout is final (a cancelled logout never fires it) | `player`, `name` |
| `core.player.death` | The character dies | `player`, `name`, `position`, `cause`, and where known `killer`, `killerName`, `weapon`, `distance`, `killerType` |
| `core.player.chat` | A chat line reaches the server mission | `name`, `channel` (`direct`, `megaphone`, `transmitter`, `publicAddress`, `admin`, `system`, `battleye`, or `other`), `channelId` (the engine's raw channel value), `text`, and `player` when exactly one online player has that name (the engine names the sender, it does not identify them). A retail client's direct chat arrived with a channel value outside the engine's documented set on DayZ 1.29, so it reads `other`; `channelId` carries what the engine said |
| `core.player.kick` | A player is disconnected by `vyshka.kick`, or a banned identity is refused at connect | `player`, `name`, `reason`, `cause` (`action` or `ban`), `actionId` |
| `core.player.ban` | `vyshka.ban` records an identity | `player`, `name` when known, `reason`, `expiresAt` when not permanent, `actionId` |
| `core.server.fps` | Every `fpsIntervalSeconds` after the first interval | `fps` (the server's frame rate over the interval, one decimal, counted from the mission's update frames because the engine's own `GetFps()` reads a constant 0.1 on a dedicated server), `players` |
| `vyshka.player.unban` | `vyshka.unban` lifts a ban (a custom type: the core set has no unban) | `player`, `name` when known, `actionId` |

A moderation event's `actionId` is the hub's id for the dispatch that caused it, so a feed
entry can be joined to the action record and the audit log.

`cause` is one of `player` (another player, bare hands or a held item; `killer` names them,
`weapon` is the item's display name, `distance` in metres is present for a ranged weapon),
`self` (the engine names the character as its own killer: starvation, dehydration, bleeding
out, drowning, a fall), `infected`, `animal`, `explosion` (`weapon` is the device),
`vehicle`, `other` (with `killerType`, the engine class of the killer), or `unknown`. This
reading of the killer object matches the one the engine's own admin log makes.

**Snapshots.** The plugin captures the full list of characters with an identity attached,
alive or not, as it builds each poll request, so the sample rides that very request instead
of waiting behind a held poll. `snapshotIntervalSeconds` (default 10) is a floor on the
spacing between captures, not a timer: a poll built sooner than that after the last capture
carries none, and when the interval is shorter than the poll cycle it is the poll cycle that
sets the cadence (see below):

```json
{ "capturedAt": "2026-09-11T14:00:00Z",
  "players": [ { "player": { "platform": "steam", "id": "7656..." }, "name": "Survivor",
                 "position": [4231.5, 300.2, 10620.0],
                 "data": { "alive": true, "health": 100, "blood": 5000 } } ] }
```

No capture is made while the previous snapshot is still unacked, or while the outbox holds
more than one poll can carry. A snapshot says what *is*, so a stale one waiting behind an
outage is worth nothing, an envelope already in the outbox cannot be replaced by a fresher
one (it may already have been stored, and the retransmission is then deduplicated), and a
buffer full of superseded snapshots would crowd out the events and action results that are
worth keeping; the hub keeps the latest per type regardless, with `capturedAt` saying how
stale it is. On a healthy link the ack for the previous snapshot arrives in the response that
ended the last poll, so a skipped capture means an outage or a backlog, and a run of them
long enough to matter is logged.

The cadence on a healthy link with quiet, fully held polls is therefore one snapshot per
poll cycle (`pollTimeout` plus a round trip) when the interval is shorter than that, and
otherwise the interval rounded up to the next poll:

```
capture gap  ~=  poll cycle * ceil(snapshotIntervalSeconds / poll cycle)
```

Receipt lags capture by about a round trip, because the hub applies a poll's envelopes
before it begins to hold. A poll the hub answers early (because it had something to
deliver) shifts that grid rather than shortening it: the re-poll that follows may be built
before the interval has elapsed, so it carries no snapshot and is then held in full, and the
gap runs to the interval plus a cycle. So with successful polls, room for a snapshot at
every poll, and no session renewal in between, a gap lies between the interval and the
interval plus the longest time between two polls being built, which for held polls is about
`pollTimeout` plus transport and processing time; only quiet, fully held polls sit on the grid
above. A backlog that fills the batch, a failed poll, or a session renewal extends the gap
past that, and the floor is kept by the running plugin, not across a restart.

Measured on a live server against a local hub with `pollTimeout` 25 and
`snapshotIntervalSeconds` 10, no players online: twenty-one snapshots, so twenty gaps, of
which 25 s eighteen times and 26 s twice; receipt lagged capture by under a second
(`capturedAt` has whole-second resolution), and no capture was held.

Plugin 0.2.0 captured on a timer instead and skipped the capture while the previous snapshot
was unacked, which put two waits between captures (the poll already in flight, then the hold
on the poll that carried the ack) and measured 50 s between snapshots at `pollTimeout` 25 and
`snapshotIntervalSeconds` 10 (issue #55).

`core.server.stop` is emitted when the mission finishes, which a graceful shutdown reaches
and a process kill does not: a server killed from the outside leaves no stop event.

**Positions** are the engine's own vector, `[x, y, z]` with `y` the elevation in metres, so
a DayZ map plots `x` against `z`. That is the game's own map frame of section 8.3; the hub
never interprets it and a map view has to know the game.

## Conformance

The plugin conformance harness (`conformance/plugin`) can drive a real DayZ server:

```
go run ./plugins/dayz/cmd/vyshka-dayz build
go run ./conformance/plugin -enroll-wait 240s -check-timeout 30s -- go run ./plugins/dayz/cmd/vyshka-dayz harness
```

`harness` reads the hub URL and token the harness puts in the environment, writes them into
a fresh profile directory under `plugins/dayz/build/`, starts the server from its default
install location (`-server` overrides it) with the built mod, mirrors the plugin's log lines
to stderr, and stops the server when the harness closes its stdin. The `-enroll-wait` covers
the server's boot time. This needs a local DayZ dedicated server install, so CI does not run
it; CI runs the Go tooling's tests and the reference driver instead. With no player on the
server the telemetry stage grades the start event and the empty snapshots; the player events
need a client to join, which the staging demo in issue #49 covers.

## What the engine imposes

The live-player heal acceptance demo passed on 2026-09-05; [issue #14](https://github.com/That1Drifter/vyshka/issues/14)
records the results and validation limits. Readable Plugin API errors landed on 2026-09-10
([issue #43](https://github.com/That1Drifter/vyshka/issues/43)); see "Errors and recovery"
below for what the plugin does with each refusal and what an operator will find in the log.

Measured under `spikes/` rather than assumed; the details are in each spike's findings.

- **TLS works.** The engine's REST client speaks HTTPS to a TLS-terminating reverse proxy
  with a publicly trusted certificate and needs nothing configured for it; the `deploy/`
  layout, which exposes no plain-HTTP port, was exercised end to end on 2026-09-10 (issue
  #13). Whether it accepts a private CA is unmeasured. When testing with a local client on
  the same machine, launch it through `DayZ_BE.exe`: starting `DayZ_x64.exe` directly gets
  the player kicked by BattlEye about 20 s after joining, even with `BattlEye = 0;` in the
  server config.
- **Poll hold time.** The engine aborts a request after 10 s by default, as a budget for the
  whole response. The plugin raises the read timeout to `pollTimeout` + 5 s at session start
  (`spikes/dayz-restapi-poll-timeout`).
- **Bearer token.** `RestContext` exposes only a content-type header, written to the wire
  verbatim, so the plugin appends a CRLF and the `Authorization` line to it. Undocumented
  engine behavior; a patch that changed it would make every poll fail with `401`, which the
  log shows as "poll refused" (`spikes/dayz-restapi-headers`).
- **Error bodies are invisible, so the plugin asks for them inline.** A 4xx reaches script as
  error code 5 with no body, a 5xx as code 6, an unreachable hub as 7, a timeout as 8. Every
  request therefore carries `?errors=inline` (protocol section 2.3), which makes a hub deliver
  its refusals as a `200` whose body is the protocol error with the status inside; the query
  string survives the engine's client on both `GET` and `POST` (`spikes/dayz-restapi-headers`,
  finding 7). Against a hub that predates the option, or a proxy answering in its place, the
  refusal is still opaque and the plugin falls back to reasoning from the error class. Both
  paths are described under "Errors and recovery".

## Errors and recovery

What the plugin does with each refusal, per protocol section 2.3. Every line below goes to
the script log with the hub's own message.

| Refusal | What the plugin does |
|---|---|
| `session_invalid` on a poll (superseded, expired, revoked) | Starts one new session. The outbox is kept and renumbered. |
| `envelope_invalid` on a poll | The hub applied nothing. The envelope at `details.index` is moved from `outbox/` to `rejected/<time>-<n>.json` with the hub's reason, the rest of the batch closes the gap and is resent at once. An `ERROR` line names the envelope and the file. It is never resent and never counted as delivered. |
| A `200` whose `error` member is not an object with a code | Treated as malformed, not as success: no ack applied, same session, retry after backoff. |
| `ack_out_of_range` on a poll | Starts a new session, which resets the sequence space. |
| `bad_request` on a poll | Halves the batch size and retries the same session. |
| `credentials_invalid`, `credentials_revoked` | Stays on the same credentials and retries every 30 s, logging that a fresh enrollment token in `config.json` is the fix. Never re-enrolls on its own. |
| `enrollment_token_invalid`, `enrollment_token_used`, `game_mismatch` | Logs that the token needs replacing and retries every 60 s. |
| `protocol_version_unsupported` | Logs that the hub or the plugin needs upgrading and retries every 5 min. |
| Any 5xx | Retries the same request with backoff (1 s doubling to 30 s). |
| A `200` that is not JSON | Retries after backoff on the same session; nothing changes. |
| An unrecognized code | By status: 401 as `session_invalid`; any other 4xx is backed off and retried on the same session; 5xx as above. |

**Opaque refusals** (an older hub, or a proxy's own error page): script sees "client error"
and nothing else. A poll refused on a session that has already polled successfully is taken
as a lost session and answered with one new session. A poll refused on a session that has
never polled is more likely the plugin's own batch, so the plugin backs off 30 s and retries
the same session, and only opens another session after a second refusal there. The worst
case is one new session a minute with a log line saying why; a malformed envelope cannot be
identified without the inline error, so it stays in the outbox until the hub is upgraded.
Session and enrollment refusals are handled as before: credentials rejected, retry every
30 s; token refused, tell the operator, retry every 60 s.

The `rejected/` directory is never read by the plugin. Delete its files once you have looked
at them.
- **Durability.** The outbox is one file per unacked envelope under the profile directory,
  written before the envelope is first sent and deleted when the hub's ack covers it. The
  engine exposes no fsync. Against a process kill (a crash, a `taskkill`, a watchdog) the
  file is in the OS page cache once `CloseFile` returns, and the spike under
  `spikes/dayz-outbox-crash` found every record intact and delivered after the restart in
  twenty kills, with the loss confined to the event buffer's unflushed tail (see
  "Events" above). A kill inside the write itself would leave an unreadable record, which
  the next boot discards and counts, and the batch in it is gone. A batch delivered but
  not yet acked when the server dies is on disk and at the hub both; the restart re-sends
  it and the hub drops it as a duplicate, so nothing is stored twice. What a power loss or
  an OS crash takes from the page cache is not measured and cannot be shortened from
  script.
  Sequence numbers are assigned at send time, never stored, which is what makes renumbering
  across a session change (spec section 9.1) automatic. Executed action ids are persisted to
  `executed.log` (append-only, JSON-encoded so an opaque id cannot split a record) and reloaded
  on boot, so a crash between executing an action and acking its dispatch does not let the
  re-delivery run it again. Two residual windows the engine cannot close, and which the hub
  absorbs by deduplicating: if a file deletion fails (an antivirus or backup lock), an acked
  envelope may be re-sent after a restart and the hub drops it as a duplicate; and if the OS
  loses the tail of `executed.log` in a crash, a re-delivered action may run twice.
- **Whole-second clock.** The engine's UTC clock is whole-second, so an action's `expiresAt`
  is compared at second granularity. The plugin discards an action once the current second
  reaches the deadline second, which never runs an action past its deadline but can discard one
  up to a second early. Harmless against a TTL measured in seconds; the hub is the authority on
  expiry regardless.
- **Integers are 32-bit.** Sequence numbers, acks, and epoch seconds live in script ints.
  Sequence spaces restart with every session, so this is not a practical bound; epoch seconds
  are for deadline comparisons only.
