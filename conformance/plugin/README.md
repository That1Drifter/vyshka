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
PASS  dispatch.invalidTolerated  A schema-invalid dispatch is survived, not fatal

11 checks, 0 failed
```

Exit code is 0 when every check passes, 1 when any check fails, and 2 when the suite could
not run at all.

| Flag | Default | Meaning |
|---|---|---|
| `-listen` | `127.0.0.1:0` | Address the mock hub listens on |
| `-enroll-wait` | `60s` | How long to wait for the candidate to enroll |
| `-check-timeout` | `20s` | Budget for each wait inside a check |
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
publishes a one-action manifest, executes dispatches behind an executed-actionId LRU,
buffers unacked envelopes across outages, and renumbers them across session changes. CI runs
the harness against it on every push, which is what keeps the suite honest in the green
direction; `harness_test.go` points deliberately broken clients at the mock hub to keep it
honest in the red direction.

Like everything under `conformance/`, neither the harness nor the driver imports hub or
plugin code: both speak only HTTP, so the suite can grade an implementation that is not this
repository's.
