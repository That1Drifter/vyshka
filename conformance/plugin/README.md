# Plugin conformance suite

Not built yet. This directory is reserved so the layout matches the shape the project commits
to in `spec/protocol.md` section 13.

When it lands, this is a **mock hub** that a candidate plugin points at instead of a real one.
It drives the plugin through enrollment, manifest publish, an action round trip including a
forced re-delivery to test dedup, a network outage to test buffering, and a schema-invalid
dispatch that must never crash the game server. A plugin that passes is compliant, with no
reading of hub source required.

Tracked in issue #9.
