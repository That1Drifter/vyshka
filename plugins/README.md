# Plugins

Reference plugins live here, one directory per game. None are built yet; this directory is
reserved so the layout matches `spec/protocol.md` section 13.

| Directory | Game | Language | Tracked in |
|---|---|---|---|
| `dayz/` | DayZ | Enforce Script | issue #14 |
| `reforger/` | Arma Reforger | Enfusion | issue #15 |

Both are clean-room work: they are written from `spec/protocol.md` and from the public engine
script headers shipped with each game. Engine behavior that constrains a plugin gets measured
first and recorded under `spikes/`, as the poll timeout limits were.
