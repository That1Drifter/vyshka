# Panel

Not built yet. This directory is reserved so the layout matches `spec/protocol.md` section 13.

The panel is an optional web UI embedded in the hub binary and served by it. It is a thin
client over the Admin API: it may not reach into hub internals, and anything it can do must be
possible with `curl` against `/api/v1` alone. Action forms are rendered from the plugin
manifest rather than hand-written per game, which is what keeps the hub game-agnostic.

Tracked in issue #13.
