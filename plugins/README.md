# Plugins

Reference plugins live here, one directory per game, matching `spec/protocol.md` section 13.

| Directory | Game | Language | Status |
|---|---|---|---|
| `dayz/` | DayZ | Enforce Script | Built: enrollment, actions, telemetry, and moderation (issues #14, #49, #59) |
| `reforger/` | Arma Reforger | Enfusion | Not started (issue #15, on hold) |

Both are clean-room work: they are written from `spec/protocol.md` and from the public engine
script headers shipped with each game. Engine behavior that constrains a plugin gets measured
first and recorded under `spikes/`, as the poll timeout limits were.
