# Plugins

Reference plugins live here, one directory per game, each written against `spec/protocol.md`.

| Directory | Game | Language | Status |
|---|---|---|---|
| `dayz/` | DayZ | Enforce Script | Built: enrollment, actions, telemetry, moderation, the position and world actions, vehicles, vitals, damage telemetry, admin flags, and the mod surface with its sample under `dayz/sample/` (issues #14, #49, #59, #66, #67, #68, #70, #71, #72) |
| `reforger/` | Arma Reforger | Enforce Script (Enfusion) | Tracer bullet (issue #15): the link, the persistent outbox, `vyshka.info` and `vyshka.kick`, and player join and leave events; the protocol code is the DayZ plugin's, ported |

Both are clean-room work: they are written from `spec/protocol.md` and from the public engine
script headers shipped with each game. Engine behavior that constrains a plugin gets measured
first and recorded under `spikes/`, as the poll timeout limits were.
