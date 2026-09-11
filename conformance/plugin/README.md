# Plugin conformance suite

The mock hub that answers "is this plugin compliant?" (spec section 14). A candidate plugin
points at it instead of a real hub, and the harness drives it through its whole lifecycle:
enrollment, sessions, long-poll, manifest publish, action round-trips, a forced re-delivery,
a transport outage, a session change with envelopes still unacked, and a schema-invalid
dispatch. A plugin that passes is compliant, with no reading of hub source required.

## Running it

One command, letting the harness launch the candidate (the hub URL and the one-time
enrollment token are passed in the environment as `VYSHKA_HUB_URL` and
`VYSHKA_ENROLLMENT_TOKEN`):

```
go run ./conformance/plugin -- <command that starts your plugin>
```

Or start the candidate yourself: run the harness with no command, and it prints the URL and
token to point the plugin at, then waits for it to enroll.

```
go run ./conformance/plugin
```

```
conformance: plugin suite against go run ./conformance/plugin/driver

PASS  enroll.exchange            The plugin exchanges the one-time token for credentials
PASS  session.start              The plugin trades its credentials for a session
...
PASS  manifest.publish           The plugin publishes a manifest declaring at least one action
PASS  telemetry.wellFormed       Any events and snapshots the plugin publishes are well formed
...
PASS  dispatch.invalidTolerated  A schema-invalid dispatch is survived, not fatal
PASS  errors.batchRefused        A refused batch is corrected, not answered with a session loop
PASS  errors.garbledSuccess      A 200 that is not JSON changes no session or delivery state
PASS  errors.credentialsRefused  Revoked credentials are retried slowly, never by re-enrolling

15 checks, 0 failed
```

A stage can also report `PART`: it passed everything it could assert but says, in a note
that the JSON report carries too, what it could not grade. That happens when the candidate
had more than one poll in flight during an error-recovery stage, which makes the pause
before a retry and the count of resends impossible to attribute (spec section 3.1 asks for
one poll at a time), and when the candidate published no telemetry inside the telemetry
stage's window (publishing any is a SHOULD, so a plugin without it is compliant, and there
was nothing to grade). A `PART` is not a full pass.

Exit code is 0 when every check passes, 1 when any check fails, and 2 when the suite could
not run at all.

| Flag | Default | Meaning |
|---|---|---|
| `-listen` | `127.0.0.1:0` | Address the mock hub listens on |
| `-enroll-wait` | `60s` | How long to wait for the candidate to enroll |
| `-check-timeout` | `20s` | Budget for each wait inside a check |
| `-legacy-errors` | off | Behave like a hub that predates inline errors (spec section 2.3): ignore `?errors=inline` and answer every refusal with an ordinary status, so the candidate's opaque-error fallback is what gets graded |
| `-json` | off | Machine-readable results on stdout instead of the text report |

The candidate's own output is forwarded to stderr, so a `-json` report on stdout stays
parseable. A candidate launched by the harness is asked to stop by closing its stdin and is
killed a few seconds later if it does not exit; a plugin that treats stdin EOF as shutdown
gets a clean exit in CI.

## What a candidate must be able to do

Beyond the protocol itself, the harness imposes one requirement: the published manifest must
declare **at least one action**, because the harness grades the action lifecycle by
dispatching the first declared action with params synthesized from its own schema (and, for
the crash check, params that violate it). An action whose execution can fail harmlessly is
ideal; the harness accepts `ok: false` results, since it grades the lifecycle rather than
the game outcome.

One grading convention follows from being black-box: when a dispatch arrives for an
actionId the plugin has already executed, the plugin should ack the envelope and send
nothing, because a repeated `action.result` is indistinguishable from a repeated execution
to an observer that cannot see the game. The hub treats messages about terminal actions as
no-ops either way (spec section 7), so silence costs nothing. The mirror limitation is
honest too: a plugin that re-executes the game-side effect while staying silent on the wire
is beyond what any black-box grader can catch.

Dispatches carry an `expiresAt` of now plus `-check-timeout`, and the harness waits just
past that deadline for the result, so a slow action is never failed while still inside the
deadline it was given. A candidate whose action legitimately needs longer, or whose
reconnect backoff is longer than the default window, should raise `-check-timeout`.

The three error-recovery stages (spec section 2.3) provoke a refusal the way a real hub
would and watch what the candidate does: a batch refused as `envelope_invalid`, a `200`
whose body is not JSON, and `credentials_revoked` on a session request. A candidate that
can read the refusal (it asked for inline errors, or its HTTP client shows it a 4xx body)
is expected to set the named envelope aside and resend the rest; one that cannot (run the
harness with `-legacy-errors` to stand in for such a hub) is expected to open at most one
replacement session and then back off. Either way it must never re-enroll, must keep every
envelope the refusal or the garbage did not acknowledge, and must wait at least 1 s before
retrying the same request. The mock keeps a copy of what it refused or swallowed and faults
a return whose type, ts or body changed. The reference DayZ plugin waits 30 s between
retries on a fresh session, which these stages allow for; a candidate that waits longer can
raise `-check-timeout`.

Telemetry (spec section 8) is graded on arrival rather than by provocation: every
`event.batch` and `state.*` envelope the candidate sends, in whatever stage it arrives, is
checked against the bounds a conformant hub enforces (the `{namespace}.{name}` grammar and
the reserved `action.` and `server.` namespaces for event types, the 200-event batch, the
16 KiB `data` cap, the required list field and the deeply enforced player identity of a
snapshot, the shape of `position`), and a violation is a fault against the stage it landed
in. The `telemetry.wellFormed` stage itself only waits, for `-check-timeout`, for the first
batch or snapshot to arrive; a candidate that publishes telemetry on a slower cadence
should raise that flag rather than accept the `PART`.

## How the checks work

Unlike the hub suite, the checks here are **stages**: the candidate is one long-lived
process being walked through its lifecycle, so each stage builds on the last, and a failed
prerequisite stage fails the stages that depended on it rather than letting them lie.

The mock hub is conformant enough that a correct plugin behaves normally against it, and it
misbehaves only on purpose: it can withhold acks (so envelopes stay unacked however often
they are retransmitted), redeliver a delivered envelope verbatim, reset every connection to
simulate an outage, and invalidate the live session. Alongside the staged assertions it
validates everything the plugin sends, and any violation is recorded as a **fault** naming
the spec section it breaks; faults fail the stage they occurred in even when that stage's
own assertions passed.

The stage that matters most is `session.renumber` (spec section 9.1). An envelope still
unacked when its session ends must be renumbered into the new session's sequence space,
keeping its `id`, `type`, `ts` and `body`: every other retransmission rule says "resend
exactly what you sent", and this one case says the opposite about `seq` alone. A plugin that
replays its buffer verbatim after reconnecting strands the whole buffer above a gap the new
session can never close, and the symptom only appears after a game server restarts with
traffic still in flight. The harness forces exactly that situation and names a verbatim
replay explicitly in the report.

## The reference candidate

`driver/` is a minimal but correct autonomous plugin: it enrolls, keeps a session, polls,
publishes a one-action manifest and one batch of events plus one `state.players` snapshot,
executes dispatches behind an executed-actionId LRU, buffers unacked envelopes across
outages, renumbers them across session changes, and follows the recovery table of spec
section 2.3. It asks for inline errors unless started
with `-inline=false`, and with `-opaque` it discards the status and body of every non-2xx,
keeping only the class, which is what an engine like DayZ's leaves a plugin with. CI runs
the harness against it on every push in three configurations (as is, `-legacy-errors`
against `-opaque`, and `-inline=false`), which is what keeps the suite honest in the green
direction; `harness_test.go` points deliberately broken clients at the mock hub to keep it
honest in the red direction.

Like everything under `conformance/`, neither the harness nor the driver imports hub or
plugin code: both speak only HTTP, so the suite can grade an implementation that is not this
repository's.
