package cli

import (
	"fmt"
	"io"
)

// The help texts are written out by hand rather than generated from the flag
// sets: the flag package prints defaults, and a default is the one place a
// secret could slip into help output.

const topHelp = `vyshka, a terminal client for a Vyshka hub's Admin API.

Usage:
  vyshka [global flags] <command> [arguments] [flags]

Commands:
  version     print the client version and the protocol draft it speaks
  health      read the hub's health
  servers     list server records; servers show, create, and token
  actions     list the actions a server's manifest declares
  run         dispatch an action, optionally waiting for its outcome
  job         read one action, optionally waiting for its outcome
  state       read a server's latest players, vehicles, entities, or world
  events      read or follow a server's event feed
  kv          read and write the key/value store
  player      read one player's events, actions, and notes across servers
  contexts    enumerate the members of a custom context
  help        print this help, or a command's

Global flags, accepted before the command and after it:
  --url URL           hub base URL, such as http://127.0.0.1:8080
  --token TOKEN       Admin API token, or file:/path/to/token
  --profile NAME      config file profile to use
  --config PATH       config file to read
  --json              print the hub's JSON, one object per line, and nothing
                      else on stdout
  --http-timeout D    timeout for each HTTP request (default 30s)

Environment:
  VYSHKA_URL, VYSHKA_TOKEN, VYSHKA_PROFILE, VYSHKA_CONFIG
  A flag wins over the environment, and the environment over the config file.

Config file ($VYSHKA_CONFIG, or vyshka/config.json under the user config
directory: ~/.config on Linux, %AppData% on Windows):
  {"default": "home",
   "profiles": {"home": {"url": "http://127.0.0.1:8080",
                         "token": "file:/home/me/.vyshka-token"}}}
  Without --profile, the default profile is used, or the only one there is.
  A missing file is fine. A token from any source may be file:/path, read
  from that file and trimmed.

Exit codes:
  0  success; for run --wait and job --wait, the action completed
  1  usage or local error: bad arguments, config, a local schema violation,
     no or an ambiguous server or player match, a declined confirmation
  2  the action failed
  3  the action expired
  4  the hub refused the request (its error code is printed)
  5  the hub could not be reached, or its answer could not be read
  6  --wait ran out of time with the action still in flight

SERVER is a server id, an exact name, or a unique part of a name (any case).
Errors go to stderr as "vyshka: <message>"; progress and notices go there too.

Run "vyshka help <command>" for a command's arguments and flags.
`

var helpTexts = map[string]string{
	"version": `Usage: vyshka version

Prints the client version, then "protocol draft N" naming the draft of
spec/protocol.md this client was written against. Needs no hub.
--json prints {"version": ..., "protocolDraft": ...}.
`,

	"health": `Usage: vyshka health

Reads the hub's /healthz: status, version, uptime, and the database. A
degraded hub exits 4, after printing what it reported.
`,

	"servers": `Usage:
  vyshka servers                      list server records
  vyshka servers show SERVER          one record, in full
  vyshka servers create NAME [--game G] [--enrollment-ttl 24h]
  vyshka servers token SERVER [--ttl 24h]

servers lists ID, NAME, GAME, LINK, CREDENTIALS, PENDING (envelopes queued and
not yet acked), LAST SEEN, and PLUGIN.

create makes a server record and prints its id, then its one-time enrollment
token on a line of its own, then the token's expiry. The hub keeps only a
digest of the token: this is the one time it is shown. Give it to the game
server's plugin to enroll.

token mints a replacement enrollment token, invalidating any unused one.

Flags:
  --game G              the game the server must enroll as (create)
  --enrollment-ttl D    enrollment token lifetime (create; default the hub's, 24h)
  --ttl D               enrollment token lifetime (token; default the hub's, 24h)
`,

	"actions": `Usage: vyshka actions [SERVER] [--code CODE]

Lists the actions a server's manifest declares: CODE, CONTEXT, DANGER, NAME,
and PARAMS, which summarises each param as name:type with * after a required
one (a vector reads position:number[]). Without SERVER, every server is
listed under a heading, and one whose plugin never published a manifest says
"no manifest".

Flags:
  --code CODE    print that one action with its full params schema

--json prints the manifest record as the hub stores it; with --code, that one
action's declaration; without SERVER, one JSON array the command assembles,
{"server": <record>, "manifest": <record or null>} per server.
`,

	"run": `Usage: vyshka run SERVER CODE [k=v | k:=json ...] [flags]

Dispatches an action. The manifest is read from the hub first, and params
are checked against the action's declared schema before the action is
dispatched, so a typo or an out-of-range value is caught at the terminal
(exit 1) and never reaches the game server.

  key=value     typed by the schema: integer, number, boolean, string, null,
                or an array as comma-separated items (position=4501.2,320.1,9800.4;
                an empty value is an empty array)
  key:=<json>   raw JSON, for objects and anything key=value cannot express

An action marked destructive asks for confirmation on a terminal, and needs
--yes when stdin is not one. One marked warning prints a notice and proceeds.

Without --wait, prints the action id and its state and exits 0. With --wait,
prints each state change to stderr and ends with the outcome: completed
prints the result payload (exit 0), failed exits 2, expired exits 3, and
running out of time exits 6 with the action id, which "vyshka job ID" reads
later. --json prints the hub's record only.

Flags:
  --target REF              the referenceKey: the entity in the action's context
  --player FRAGMENT         resolve the referenceKey from the latest players
                            snapshot: an exact player id, an exact name, or a
                            unique part of a name. The snapshot's capture time
                            is printed beside the match, since it can lag.
  --ttl D                   deadline for a terminal state (at least 1s;
                            default the hub's, 120s)
  --idempotency-key K       a retry with the same key returns the original action;
                            with a key, an action the manifest no longer declares,
                            or params its schema now refuses, go out as written
                            (after a notice) for the hub to answer
  --wait                    wait for the outcome
  --timeout D               how long --wait waits (default: until the action's
                            deadline, plus 5s)
  --yes                     dispatch a destructive action without asking
`,

	"job": `Usage: vyshka job ACTION_ID [--wait] [--timeout D]

Reads one action: its state, timestamps, params, and result. With --wait,
waits for the outcome with the exit codes of "vyshka run --wait".

Flags:
  --wait         wait for the outcome
  --timeout D    how long --wait waits (default: until the action's deadline,
                 plus 5s)
`,

	"state": `Usage: vyshka state SERVER players|vehicles|entities|world [--history N]

Reads the latest snapshot of one kind. The header says when the game
captured it and how long ago, and when the hub received it: a snapshot is
whatever the plugin last sent, however old, so read the age before trusting
it. A snapshot is never confirmation that an action took effect.

players prints NAME, PLATFORM, ID, POSITION; vehicles and entities print ID,
KIND, POSITION; world prints the game's time and its data.

Flags:
  --history N    list the last N snapshots instead, newest first
`,

	"events": `Usage: vyshka events SERVER [--type PATTERN]... [flags]

Reads a server's event feed, newest first: OCCURRED, TYPE, and DATA (the
payload as JSON, cut to one line). A PATTERN is an exact type, a
namespace.* pattern, or *; repeat --type to OR several.

Flags:
  --type PATTERN    filter by type; repeatable
  --since T         only events at or after T (RFC 3339, or a duration ago: 15m)
  --until T         only events before T (RFC 3339, or a duration ago)
  --limit N         page size (the hub caps it)
  --all             walk every page, not just the first
  --follow          print the latest page oldest first, then every new event as
                    it arrives, until interrupted
  --interval D      how often --follow asks (default 2s)

--json prints each page as the hub sends it, one per line; with --follow, one
event per line.
`,

	"kv": `Usage:
  vyshka kv get NS KEY
  vyshka kv set NS KEY VALUE [--if-revision R] [--ttl D] [--string]
  vyshka kv incr NS KEY [--delta N]
  vyshka kv delete NS KEY
  vyshka kv list NS [--prefix P] [--limit N] [--all]
  vyshka kv namespaces

get prints the value as JSON on stdout, so $(vyshka kv get ns key) works, and
its revision and expiry on stderr.

set takes VALUE as JSON when it parses as JSON, otherwise as a string, and
prints the new revision. With --if-revision the write happens only if the key
is at that revision (0: only if the key does not exist); a mismatch exits 4
and prints the current revision.

incr adds --delta (default 1; negative to subtract) and prints the new value.
delete succeeds when the key is already gone, and says so; the hub answers it
with no body, so --json prints nothing for it.

Flags:
  --if-revision R    compare-and-swap guard (set)
  --ttl D            expire the key after D (set; default: never)
  --string           store VALUE as a string even when it parses as JSON (set)
  --delta N          amount to add (incr)
  --prefix P         list only keys starting with P (list)
  --limit N          page size (list)
  --all              walk every page (list)
`,

	"player": `Usage: vyshka player PLATFORM ID [events|actions|notes] [flags]

Reads one player's profile across every server: the events naming them
(OCCURRED, TYPE, ROLES, SERVER), the actions dispatched against them
(CREATED, CODE, STATE, SERVER, ID), and operator notes (CREATED, BY, TEXT).
Without a section, prints all three, ten each. The profile is as deep as the
hub's event retention, not a lifetime record.

Flags:
  --limit N         page size
  --all             walk every page
  --type PATTERN    filter events by type; repeatable
`,

	"contexts": `Usage: vyshka contexts SERVER CONTEXT_ID [--refresh]

Enumerates the members of a custom context the server's manifest declares:
KEY (what an action in that context takes as its --target), LABEL, and
POSITION. The hub asks the plugin, or answers from a cache younger than
about 10s; the header says when the enumeration was taken.

Flags:
  --refresh    skip the hub's cache and ask the plugin now
`,
}

// writeHelp prints the top-level help, or one command's.
func writeHelp(w io.Writer, command string) {
	if text, ok := helpTexts[command]; ok {
		fmt.Fprint(w, text)
		fmt.Fprint(w, "\nGlobal flags (--url, --token, --profile, --config, --json, --http-timeout)\n"+
			"are accepted too; \"vyshka help\" describes them.\n")
		return
	}
	fmt.Fprint(w, topHelp)
}
