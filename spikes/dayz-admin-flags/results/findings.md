# Invisibility and no-collision from the server: measurements

Part of issue #71.

**Short answer:** not shown. The two calls a server has, `Entity.SetInvisible(bool)` and
`dBodySetInteractionLayer(entity, layer)`, both run on a player character without error,
and neither produced anything the one connected client could observe. Invisibility needs a
second client to be seen at all, since a first-person camera never draws its own body, and
this rig had one client. No-collision changes a value on the server's copy of the
character's physics body, while the character walks and collides in the simulation the
owning client runs, so a server-side layer is not what it walks against. Both flags stay
parked; the roadmap's trigger is now the two-client measurement.

## Environment

| Item | Value |
|---|---|
| Game build | DayZ 1.29, `DayZDiag_x64.exe -server` (the diagnostic build, BattlEye off) and one `DayZDiag_x64.exe` client on the same PC |
| Host OS | Windows 11 Pro 26100 |
| Plugin | `vyshka-dayz` 0.8.0 as built from the `slice/71-admin-flags` branch |
| Rig | `harness/init.c`, trigger files under the server's profile directory |
| Date | 2026-09-17, 19:16 to 19:17 UTC |

## Results

`report` lines are the rig's, server-side; the layer is `dBodyGetInteractionLayer` on the
character, an integer the engine does not name in script.

| Step | Call | Server | Client (owner, first person) |
|---|---|---|---|
| 1 | `SetInvisible(true)` on the character | ran; `OnInvisibleSet` is the engine's event for it | no change in view; the body is not drawn in first person whether visible or not, and the HUD kept showing the held weapon |
| 2 | `SetInvisible(false)` | ran | no change |
| 3 | `dBodySetInteractionLayer(character, PhxInteractionLayers.NOCOLLISION)` | layer read 16 before, 2 after | no change in view; not walked into geometry (see below) |
| 4 | `dBodySetInteractionLayer(character, PhxInteractionLayers.CHARACTER)` | layer read 16 again | no change |

## Findings

1. **Invisibility is unmeasured, not refuted.** `SetInvisible` is a native on `Entity`
   with a script event, and the engine uses it on clothing and on the character itself in
   client-side code. Whether a server-side call replicates to other clients is exactly the
   question, and it cannot be answered by the character's own client, whose first-person
   camera draws no body. A second connected client, or a chase camera on the diagnostic
   client, is what the measurement needs. The plugin's live rig supports one client.

2. **No-collision from the server is the wrong place to look.** The character's movement and
   its collision with buildings run in the owning client's simulation; the server holds its
   own body and corrects the client. Changing the server body's interaction layer changes
   what the server's body interacts with (the value moved from 16 to 2 and back), not what
   the client's character walks against. Making a character pass through walls would need
   the client to do it, which a server-only mod cannot ask of a vanilla client. The walk
   into geometry was not performed: with the layer changed on the server alone there is no
   mechanism by which it could have succeeded, and a failed walk would have measured
   nothing this reasoning does not already say.

3. **What was learned about the rig on the way.** Scripted keyboard input reaches the
   diagnostic client (W moved the character 8.4 m in 2.5 s; R chambered a round), and
   scripted mouse buttons through `SendInput` did not fire the held rifle in four attempts
   with the raise held by the right mouse button; Space is the engine's jump, not a raise.
   A shot's server-side aftermath was exercised through the weapon's own fire event from
   the rig instead.

## Consequence for the roadmap

Both flags stay parked. The trigger reads: a two-client measurement shows a server-side
`SetInvisible` hides the character from another client, or the client-side simulation can
be told to ignore geometry from the server. Nothing on the roadmap depends on either.
