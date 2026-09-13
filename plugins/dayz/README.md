# Vyshka DayZ plugin

The reference game plugin for DayZ: a server-side Enforce Script mod that enrolls a DayZ
dedicated server with a Vyshka hub, long-polls it for work, publishes a manifest, executes
dispatched actions, and publishes telemetry: the core player events a feed needs and the
`state.players` snapshots a live map needs. It ships one built-in action, `vyshka.heal`, so
an operator can heal a player from a `curl` against the hub. Protocol: `spec/protocol.md`.

Clean-room: written from the engine's public script headers and the measurements under
`spikes/`, per `CONTRIBUTING.md`.

## Layout

| Path | What it is |
|---|---|
| `mod/config.cpp` | Addon and script-module registration |
| `mod/scripts/3_Game/Vyshka/` | Protocol code: JSON, clock and ids, files, outbox, transport, the link itself, the action registry, the event buffer |
| `mod/scripts/4_World/Vyshka/` | Game-facing code: the heal action, the player roster and telemetry (`VyshkaPlayerTelemetry`) |
| `mod/scripts/5_Mission/Vyshka/` | The `MissionServer` hooks that start and stop the plugin and feed it connects and disconnects |
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
   2 and 600; `0` turns `state.players` snapshots off).
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

## Healing a player

With the server enrolled and a player online:

```
curl -X POST https://hub.example.net/api/v1/servers/<serverId>/actions \
  -H "Authorization: Bearer <admin token>" -H "Content-Type: application/json" \
  -d '{"code":"vyshka.heal","context":"player","referenceKey":"<Steam64 id>","params":{}}'
```

The `referenceKey` is the player's plain Steam64 id. The result carries the player's health,
blood, and shock after the heal; `restoreBlood: false` in `params` leaves blood alone.

## Telemetry

The plugin publishes events (protocol section 8.1) and `state.players` snapshots (section
8.3) as soon as it is running; nothing needs configuring. Player identity everywhere is
`{ "platform": "steam", "id": "<Steam64>" }` (section 8.2), the same id the heal action
takes as its `referenceKey`.

**Events.** Buffered and flushed as one `event.batch` every 2 s or at 200 events, then
persisted, acked, and renumbered like every other envelope. Up to 2 s of events can be lost
to a crash before the flush. A poll carries at most 1000 events across its batches, the
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

`cause` is one of `player` (another player, bare hands or a held item; `killer` names them,
`weapon` is the item's display name, `distance` in metres is present for a ranged weapon),
`self` (the engine names the character as its own killer: starvation, dehydration, bleeding
out, drowning, a fall), `infected`, `animal`, `explosion` (`weapon` is the device),
`vehicle`, `other` (with `killerType`, the engine class of the killer), or `unknown`. This
reading of the killer object matches the one the engine's own admin log makes.

**Snapshots.** Every `snapshotIntervalSeconds` (default 10) the plugin *attempts* a capture
of the full list of characters with an identity attached, alive or not. An attempt made
while the previous snapshot is still unacked is skipped, so the interval sets how often a
capture is tried, not how often one lands (see the cadence below):

```json
{ "capturedAt": "2026-09-11T14:00:00Z",
  "players": [ { "player": { "platform": "steam", "id": "7656..." }, "name": "Survivor",
                 "position": [4231.5, 300.2, 10620.0],
                 "data": { "alive": true, "health": 100, "blood": 5000 } } ] }
```

A snapshot is only queued when the previous one has been acked. A snapshot says what *is*,
so a stale one waiting behind an outage is worth nothing, and a buffer full of them would
crowd out the events and action results that are worth keeping; the hub keeps the latest
per type regardless, with `capturedAt` saying how stale it is. The cost is that
`snapshotIntervalSeconds` sets how often a capture is *tried*, while the link decides how
often one lands. The exact rule is: a capture happens at the first scheduled attempt that
finds no unacked snapshot. A skipped attempt still advances the timer, and the timer is set
from the end of each attempt, so attempts drift rather than sit on a fixed grid.

Two waits stand between a capture and the moment its ack is processed. The envelope cannot
be sent until the poll already in flight returns: nothing to a full `pollTimeout`, depending
on where in the cycle it was queued. Then the ack rides the response of the poll that
carried it, which the hub holds until it has something to send or the timeout expires. So
with an interval well under `pollTimeout`, on a healthy link with quiet, fully held polls
and no backlog:

```
capture gap  ~=  snapshotIntervalSeconds * ceil((send wait + ack hold) / snapshotIntervalSeconds)
```

Approximate, because transport, hub processing, and callback time are in that delay too, and
anything that pushes the ack past an attempt costs a whole interval more. With an interval
longer than the two waits (the config allows up to 600) the interval alone sets the cadence,
and lowering an interval already well under the poll cycle mostly moves where the rounding
lands rather than shortening the waits.

Measured on a live server with `pollTimeout` 25 and `snapshotIntervalSeconds` 10: nineteen
snapshots, so eighteen gaps, of which 50 s fourteen times, 60 s twice, 51 s once, and 30 s
once; receipt lagged capture by 3.0 to 26.5 s. The 30 s gap is consistent with a short send
wait followed by the ack hold, the snapshot opening it having been received 3.0 s after
capture, though receipt times alone cannot separate that from a hold the hub ended early. A
run of held-back ticks long enough to mean an outage is logged.

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
  engine exposes no fsync, so a game-server crash can lose what the OS had not flushed.
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
