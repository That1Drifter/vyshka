# vyshka, the command-line client

`vyshka` is a command-line client for the hub's Admin API: one binary that operators and
scripts use instead of hand-written curl. It registers servers, lists what a server can
do, dispatches an action and waits for its outcome, follows the event feed, reads state
snapshots, and works the key/value store, player profiles, and custom contexts.

It is built on the Go client package at `client/`, a hand-written client of the Admin API
that covers the endpoints the command uses (tokens, audit, bans, and webhooks are left to
issue #96). It speaks only the public Admin API and imports nothing from `hub/`, so it is a
client like any other: whatever it does, a bot or a script can do with the same token, and
the token's scopes bound it the same way.

- [Install](#install)
- [Configuration](#configuration)
- [Quick start](#quick-start)
- [Commands](#commands)
- [Output and `--json`](#output-and---json)
- [Exit codes](#exit-codes)
- [Compatibility](#compatibility)
- [Tests](#tests)
- [Why a command-line client](#why-a-command-line-client)

## Install

Any one of:

```
# The binary from a hub release archive: vyshka (vyshka.exe in the Windows zip) sits
# beside vyshka-hub, checked by the release's SHA256SUMS like the hub.
tar -xzf vyshka-hub_<version>_linux_amd64.tar.gz
./vyshka-hub_<version>_linux_amd64/vyshka version

# From source
go install github.com/That1Drifter/vyshka/cmd/vyshka@latest

# From a clone
go build -o bin/vyshka ./cmd/vyshka
```

The binaries in a release archive are built from the release tag and report it. The release
workflow unpacks the Linux archive and checks the version `vyshka version` reports before
it publishes anything (`RELEASING.md`).

```
$ vyshka version
<version>
protocol draft 0.33
```

The first line is the client's version. The second is the draft of `spec/protocol.md` the
client was written against. `vyshka version --json` prints
`{"version": ..., "protocolDraft": ...}` on one line.

## Configuration

Three sources, in this order of precedence: a flag, then an environment variable, then the
config file.

| Setting | Flag | Environment |
|---|---|---|
| Hub URL | `--url` | `VYSHKA_URL` |
| Admin API token | `--token` | `VYSHKA_TOKEN` |
| Profile in the config file | `--profile` | `VYSHKA_PROFILE` |
| Config file path | `--config` | `VYSHKA_CONFIG` |

Two more global flags have no environment variable: `--json` (see below) and
`--http-timeout`, the timeout of each request to the hub, 30s by default. A global flag
goes before the command or on any subcommand: `vyshka --json servers` and
`vyshka servers --json` are the same call.

### The config file

A JSON file at `$VYSHKA_CONFIG` (or `--config`), else at `vyshka/config.json` in the user
config directory: `$XDG_CONFIG_HOME` or `~/.config` on Linux, `~/Library/Application
Support` on macOS, `%AppData%` on Windows.

```json
{
  "default": "home",
  "profiles": {
    "home": {
      "url": "http://127.0.0.1:8080",
      "token": "file:/home/me/.vyshka-token"
    },
    "staging": {
      "url": "https://hub.example.net",
      "token": "file:/home/me/.vyshka-staging-token"
    }
  }
}
```

`--profile` (or `VYSHKA_PROFILE`) picks a profile. With none named, the profile that
`"default"` names is used, else the only profile if the file holds exactly one. A missing file is
not an error: flags and environment alone are enough.

### The token

A token from any source, flag, environment, or file, can be written `file:/path/to/secret`,
in which case the client reads the token from that file. It is the indirection the hub's
`-admin-token` takes. Prefer it to a token typed into a shell, where history and the
process list keep it. The client never prints the token.

### Naming a server

Every command that takes `SERVER` accepts an id or a name: the id or the name exactly, else
a case-insensitive substring of a name when exactly one server matches. A name that matches
none, or more than one, is a local error (exit 1).

## Quick start

A hub, a server record, a plugin, then the commands. Start a hub as the repository README
describes and point the client at it:

```
VYSHKA_ADMIN_TOKEN=vya_local_dev_token ./vyshka-hub serve &

export VYSHKA_URL=http://127.0.0.1:8080
export VYSHKA_TOKEN=vya_local_dev_token
vyshka health
```

`health` prints what the hub's `/healthz` reports: status, version, uptime, database. It
exits 4 when the hub says it is degraded.

Register the game server and take its one-time enrollment token:

```
vyshka servers create Livonia --game dayz
```

The output has the server's id and the token. `--enrollment-ttl` sets how long the token
stays valid; if it lapses or is lost, `vyshka servers token Livonia` issues a fresh one
(`--ttl` sets its lifetime the same way).

Give the token to the plugin. For the DayZ plugin that is `enrollmentToken` in
`<profiles>/Vyshka/config.json` (`plugins/dayz/README.md`, "Installing on a server"), and
then the server starts. The plugin trades the token for credentials and holds a session;
`vyshka servers` shows the state in its LINK column, and `vyshka servers show Livonia` has
the whole record.

Once the plugin has published its manifest, ask what the server can do:

```
vyshka actions Livonia
vyshka actions Livonia --code vyshka.vitals
```

The first prints one row per action (CODE, CONTEXT, DANGER, NAME, PARAMS). The second prints
the full params schema of one action.

Dispatch one and wait for it:

```
vyshka run Livonia vyshka.vitals --player Ivan stat=health value=100 --wait
vyshka run Livonia vyshka.teleport --player Ivan position=7500,7600 --wait
vyshka run Livonia vyshka.weather preset=storm transitionSeconds=60 --wait
```

`stat=health` and `value=100` are typed from the manifest's schema before anything is sent:
`value` is a number there, so `100` goes out as one, and `position=7500,7600` becomes the
two-element vector. `--player Ivan` is resolved against the players snapshot. With `--wait`
the command follows the action through queued, delivered, and running, prints the plugin's
result when it completes, and exits 0; a failed action exits 2 and an expired one 3.

Watch the feed:

```
vyshka events Livonia --limit 20
vyshka events Livonia --type 'core.player.*' --follow
```

The first is the newest 20 events, newest first. The second tails: it prints events as they
arrive until Ctrl-C.

Keep small shared state in the key/value store (spec section 12):

```
vyshka kv namespaces
vyshka kv set example-mod territory.t-19 '{"owner":"clan-a"}'
vyshka kv get example-mod territory.t-19
vyshka kv incr example-mod raids.count --delta 5
```

## Commands

`vyshka help` lists them and `vyshka help COMMAND` prints one command's flags. In the
synopses below, `SERVER` is an id or a name (see above), brackets are optional, and `...`
repeats.

### `vyshka version`

Two lines: the client's version, then `protocol draft 0.33`. `--json` prints
`{"version", "protocolDraft"}`.

### `vyshka health`

```
vyshka health
```

The hub's `/healthz`: status, version, uptime, database. Exits 4 when the hub reports
itself degraded. This is the command that shows the hub's version; `vyshka version` shows
the client's.

### `vyshka servers`

```
vyshka servers
vyshka servers show SERVER
vyshka servers create NAME [--game G] [--enrollment-ttl D]
vyshka servers token SERVER [--ttl D]
```

`servers` lists every server as a table: ID, NAME, GAME, LINK, CREDENTIALS, PENDING,
LAST SEEN, PLUGIN. `show` prints one server's record. `create` registers a server and
prints its id and the one-time enrollment token. `token` reissues the enrollment token.

### `vyshka actions`

```
vyshka actions [SERVER] [--code CODE]
```

The manifest listing as a table: CODE, CONTEXT, DANGER, NAME, PARAMS. Params read
`name:type`, with a `*` after the required ones and `number[]` for a vector. With no
`SERVER` every server's manifest is listed. `--code CODE` prints that action's full params
schema instead.

### `vyshka run`

```
vyshka run SERVER CODE [--target REF | --player FRAGMENT] [--ttl D] [--idempotency-key K]
           [--wait] [--timeout D] [--yes] [k=v | k:=json ...]
```

Dispatches an action. The action's context (player, vehicle, world, or a custom one) comes
from the manifest, so there is no flag for it.

**The target.** A world action takes none. `--target REF` names the target as the hub knows
it (the action's reference key: for the DayZ plugin a player's Steam64 id or a vehicle's id
from `vyshka state SERVER vehicles`). `--player FRAGMENT` finds a player in the players
snapshot: an exact id, else an exact name, else a case-insensitive substring that matches
exactly one player. A fragment that matches more than one is refused and the matches are
listed. The command prints the snapshot's capture time and age next to the resolution, so a
choice made on a stale snapshot is visible.

**The params.** Each `k=v` is coerced by the type the manifest declares for `k`: integer,
number, boolean, string, or null. An array takes its elements split on commas, so a vector
is `x,y` or `x,y,z`. A key the schema does not declare, or a value that breaks the schema,
fails locally with exit 1 before any request goes out. For what `k=v` cannot express, such
as an object, `k:=<json>` passes the JSON as written:

```
vyshka run Livonia vyshka.weather 'dynamicFog:={"distanceDensity":0.4,"heightDensity":0.2}' --wait
```

**Danger.** The manifest's `danger` decides what happens before the request. A
`destructive` action asks `run CODE on SERVER (destructive)? [y/N]` and goes ahead on `y`;
`--yes` skips the question. When stdin is not a terminal it refuses without `--yes` (exit
1), so a script never answers by accident. A `warning` action prints a notice and
proceeds.

**Waiting.** Without `--wait` the command ends once the hub has accepted the action;
`vyshka job` follows it later. With `--wait` it follows the action through queued,
delivered, and running to a terminal state, writing progress to stderr, and prints the
result when the action completes. It waits until `--timeout`, or by default until the
action's own expiry plus a few seconds. `--ttl` sets the action's lifetime. If the wait runs
out with the action still in flight the command exits 6 and prints the action id for
`vyshka job`.

`--idempotency-key K` gives the hub a key that makes a retried dispatch the same action
rather than a second one.

The client does not read a snapshot back to confirm a move it has just made: a teleport
completes when the plugin says so, and the next `state` shows the new position when the
plugin's next capture arrives.

### `vyshka job`

```
vyshka job ACTION_ID [--wait] [--timeout D]
```

One action's record and result. With `--wait` it follows the action to a terminal state the
way `run --wait` does, with the same exit codes and the same timeout rule.

### `vyshka state`

```
vyshka state SERVER players|vehicles|entities|world [--history N]
```

The latest snapshot of that type. It opens with a header,
`captured <time> (<age> ago), received <time>`: the plugin's capture time, how old that is,
and when the hub received it. Players print as NAME, PLATFORM, ID, POSITION; vehicles and
entities as ID, KIND, POSITION; the world as its time and its data. `--history N` lists
the recent snapshots instead of printing the latest one; the number is how many.

### `vyshka events`

```
vyshka events SERVER [--type PATTERN]... [--since T] [--until T] [--limit N] [--all]
              [--follow] [--interval D]
```

One page of the event feed, newest first. `--type` is repeatable and each is an exact
event type, a `namespace.*` pattern, or `*`; the terms are ORed, and none means every type
(spec section 8.5). `--since` and `--until` bound the time range, and `--limit` sets the
page size. `--all` walks every page instead of stopping at the first.

`--follow` tails the feed: events print oldest first as they arrive, and with `--json` each
is one object on its own line. Every read looks back 60 seconds so that an event that
reaches the hub late is not missed. `--interval` sets how often the client asks the hub.
Ctrl-C ends it.

### `vyshka kv`

```
vyshka kv get NS KEY
vyshka kv set NS KEY VALUE [--if-revision R] [--ttl D] [--string]
vyshka kv incr NS KEY [--delta N]
vyshka kv delete NS KEY
vyshka kv list NS [--prefix P] [--limit N] [--all]
vyshka kv namespaces
```

`get` writes the value to stdout as JSON and the revision to stderr, so command
substitution gets the value alone:

```
owner="$(vyshka kv get example-mod territory.t-19)"
```

`set` takes `VALUE` as JSON when it parses as JSON and as a string otherwise, so `12` is a
number and `clan-a` a string; `--string` forces a string, so `kv set NS KEY 12 --string`
stores the string `"12"`. `--ttl` makes the key expire. `--if-revision R` writes only when the key is
at revision `R`: a mismatch exits 4 and shows the key's current revision, which is the
number to retry with. `incr` adds `--delta` to a counter, atomically. `delete` succeeds
when the key is already absent. `list` walks a namespace's keys, `--prefix` narrowing them
and `--all` following every page, and `namespaces` lists the namespaces.

### `vyshka player`

```
vyshka player PLATFORM ID [events|actions|notes] [--limit N] [--all] [--type PATTERN]...
```

A player's profile, gathered across every server: the events that name the identity, the
actions taken against it, and the notes operators keep on it (spec section 8.2 has the
identity shape). With no subcommand it prints all three under headings; `events`,
`actions`, or `notes` prints just that one. `--limit` and `--all` page it as in `events`,
and `--type` narrows the events.

### `vyshka contexts`

```
vyshka contexts SERVER CONTEXT_ID [--refresh]
```

The entries of a custom context, as KEY, LABEL, POSITION, with when they were enumerated. A
custom context is a set of targets a plugin enumerates for the hub (the DayZ item catalog
is one); `--refresh` asks for a fresh enumeration where the plain command answers from what
the hub already holds.

## Output and `--json`

By default the output is for a person: tables, and headings where a command prints more
than one thing. Progress (a `--wait` moving from queued to running) and notices (a
`warning` action's, the age of the snapshot a player was resolved against) go to stderr, so
stdout stays the data.

`--json`, on every command, prints the hub's object as one line. A command that follows,
`events --follow`, prints one object per line. Progress and notices still go to stderr, so
`vyshka ... --json | jq ...` reads clean data. `kv get` is the one command whose plain
output is already JSON: the value alone on stdout.

A script that acts on the outcome of a dispatch reads the exit code:

```
vyshka run Livonia vyshka.broadcast message="Restart in five minutes" --wait
case $? in
  0) ;;
  2) echo "the plugin ran it and it failed" ;;
  3) echo "it expired before the server took it" ;;
  6) echo "still in flight, see vyshka job" ;;
  *) echo "could not dispatch it" ;;
esac
```

## Exit codes

| Code | Meaning |
|---|---|
| 0 | Success. With `--wait`: the action completed |
| 1 | Usage or local error: bad arguments, a config problem, a local schema violation, an ambiguous or missing server or player, a declined confirmation |
| 2 | The action failed |
| 3 | The action expired |
| 4 | The hub refused the request: any Admin API error, unauthorized and forbidden included |
| 5 | Transport error: the hub is unreachable or its answer cannot be read |
| 6 | `--wait` ran out of time with the action still in flight; the id is printed for `vyshka job` |

## Compatibility

The client tolerates fields it does not know and prints a state or danger value it does not
know verbatim, so a hub newer than the client keeps working for what the client understands.
`vyshka health` shows the hub's version and `vyshka version` the protocol draft the client
was written against (`spec/protocol.md` carries the draft in its header), which is the pair
to compare when something looks off. The client version and the hub version are the same
number in a release archive; a build from source reports what its build stamps.

## Tests

Go tests under `cmd/vyshka` boot a hub in process and drive the command against it: `run
--wait` through every terminal state and its exit code, events, key/value, state, player
resolution, and the confirmation prompt. A smoke test builds the binary and runs it against
a hub with a fake plugin that long-polls, the way a real one does. They run with
`go test ./...`, in the CI test job.

## Why a command-line client

Some servers run a story: a storm rolls in at 20:00, a broadcast follows at 20:10, a crate
is spawned at 20:15. Those beats are fired from a shell, a scheduler, or a bot, and the
author wants the outcome and not only the acceptance. The
Admin API accepts a dispatch and answers at once; whether the server ran it is a separate
fact, and a tracked action is where it lives. `vyshka run --wait` follows that fact to the
end and turns it into an exit code, so the script that fires the beat can branch on "the
plugin did it", "the plugin tried and failed", and "the server was down and the action
expired" without writing the polling loop each time.
