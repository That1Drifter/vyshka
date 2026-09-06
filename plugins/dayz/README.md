# Vyshka DayZ plugin

The reference game plugin for DayZ: a server-side Enforce Script mod that enrolls a DayZ
dedicated server with a Vyshka hub, long-polls it for work, publishes a manifest, and
executes dispatched actions. It ships one built-in action, `vyshka.heal`, so an operator can
heal a player from a `curl` against the hub. Protocol: `spec/protocol.md`.

Clean-room: written from the engine's public script headers and the measurements under
`spikes/`, per `CONTRIBUTING.md`.

## Layout

| Path | What it is |
|---|---|
| `mod/config.cpp` | Addon and script-module registration |
| `mod/scripts/3_Game/Vyshka/` | Protocol code: JSON, clock and ids, files, outbox, transport, the link itself, the action registry |
| `mod/scripts/4_World/Vyshka/` | Game-facing actions (`VyshkaHealAction`) |
| `mod/scripts/5_Mission/Vyshka/` | The `MissionServer` hook that starts and stops the plugin |
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
   and defaults to `dayz`.
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
it; CI runs the Go tooling's tests and the reference driver instead.

## What the engine imposes

The live-player heal acceptance demo passed on 2026-09-05; [issue #14](https://github.com/That1Drifter/vyshka/issues/14)
records the results and validation limits. Readable Plugin API transport errors are explicitly
deferred from M2 to [issue #43](https://github.com/That1Drifter/vyshka/issues/43). The current
error-class fallback can mistake a malformed poll batch for a lost session and repeatedly
start sessions without correcting the data. That follow-up remains a hardening gate before
claiming readiness for unattended operation; the successful heal does not validate this path.

Measured under `spikes/` rather than assumed; the details are in each spike's findings.

- **Poll hold time.** The engine aborts a request after 10 s by default, as a budget for the
  whole response. The plugin raises the read timeout to `pollTimeout` + 5 s at session start
  (`spikes/dayz-restapi-poll-timeout`).
- **Bearer token.** `RestContext` exposes only a content-type header, written to the wire
  verbatim, so the plugin appends a CRLF and the `Authorization` line to it. Undocumented
  engine behavior; a patch that changed it would make every poll fail with `401`, which the
  log shows as "poll refused" (`spikes/dayz-restapi-headers`).
- **Error bodies are invisible.** A 4xx reaches script as error code 5 with no body, a 5xx
  as code 6, an unreachable hub as 7, a timeout as 8. The plugin cannot read the protocol's
  `error.code`, so a client error on a poll means "start a new session", on a session request
  "credentials rejected, retry slowly", on enrollment "token refused, tell the operator".
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
