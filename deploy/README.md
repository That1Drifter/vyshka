# Deploying the hub

The hub is one static binary; `Dockerfile` at the repository root wraps it in a distroless
image running as a non-root user, with the SQLite file on `/data`. This directory holds two
reference single-host layouts, both with the hub on loopback and a reverse proxy terminating
TLS in front of it (spec section 3.3): the container with compose, and the binary from a
release archive under systemd. Either can drop the proxy and let the hub get its own
certificate over ACME instead (`-auto-tls`, "Without a proxy" below). An operator who already runs Postgres sets `DATABASE_URL`
instead of using the SQLite file; the accepted URL forms are in the repository README under
"Database".

| File | What it is |
|---|---|
| `docker-compose.yml` | Builds the image from the repository and runs it on `127.0.0.1:8080` with the admin token read from a file, never from the environment |
| `nginx-vyshka.conf.example` | A server block with the two settings a hub needs from its proxy: a read timeout above the 60 s poll hold, and a body cap at or above the hub's 1 MiB |
| `vyshka-hub.service` | A systemd unit for the static binary: a dedicated user, the state under `/var/lib/vyshka`, the admin token as a systemd credential, confinement a static Go binary tolerates |
| `hub.env.example` | The unit's environment file, `/etc/vyshka/hub.env`: listen address, database URL, maps directory, log level, auto-TLS |
| `hub.toml.example` | The same settings as a TOML config file, every one commented, with `file:` references for secrets ("The config file" below) |
| `docker-compose.auto-tls.yml` | An override for `docker-compose.yml` that publishes 443 and 80 and turns on `-auto-tls`, for a host with no proxy |
| `branch-hub.compose.yml` | A disposable second hub beside a live one, running whatever branch `scripts/deploy-branch-hub.sh` last deployed |

## The container with compose

```
cd deploy
printf 'vya_%s' "$(openssl rand -hex 24)" > admin-token
chmod 0400 admin-token && sudo chown 65532:65532 admin-token
mkdir -p data && sudo chown 65532:65532 data
docker compose up -d --build
curl -s http://127.0.0.1:8080/healthz
```

`65532` is the distroless `nonroot` user: the secret file must be readable by it and the
data directory writable and searchable by it (the hub creates the database and its WAL
files there). Both are ignored by git and by the Docker build context, so neither can end
up in a commit or an image layer. The compose file also mounts `./maps` read-only as the
panel's map tileset directory (`panel/README.md`, "Map tilesets"): put a world's
`manifest.json` and `tiles/` under `maps/<world>/` and the live map draws on it; leave it
empty (compose creates it) and the map view lists players without imagery. It is ignored
like the data directory, because a tileset is large and derived from game assets whose
redistribution is the operator's call, not the repository's. Read the token back with `sudo cat admin-token` when
signing in to the panel or calling the Admin API; the hub never logs a configured token.

The compose file also sets what a shared host needs from a guest: a stop grace period above
the hub's 15 s drain, memory, CPU, and pid budgets, and rotated request logs. Adjust the
budgets to the host; the hub's own bounds are per connection and per request, not a memory
ceiling.

Upgrading is `git pull && docker compose up -d --build`. Migrations run on boot, and the
database file survives the container (it lives in `./data`). The base images are pinned by
digest in `Dockerfile`; bump them deliberately. To run a published image instead of a local
build, replace the `build:` block with `image: ghcr.io/that1drifter/vyshka-hub:<version>`
(`RELEASING.md` names the tags); upgrading is then a tag change and `docker compose up -d`.

To front the hub from an nginx that runs in another compose project (the staging layout on
the OVH box), add an override that joins that project's network instead of publishing a
port, and proxy to the container name from a server block that resolves it at request time,
so that nginx still starts when the hub is down.

A plain-HTTP port on the public internet is deliberately not part of this layout: every
bearer token the hub sees would travel in clear. Engines whose HTTP client cannot do TLS
are a reason to put a TLS-terminating proxy near the game server, not to open the hub.

## The binary under systemd

For a host without Docker. The Linux release archive
(`vyshka-hub_<version>_linux_<arch>.tar.gz` from the GitHub release, checked against
`SHA256SUMS`) carries the two binaries (`vyshka-hub`, and the `vyshka` command-line client
of `cmd/vyshka/README.md`), `vyshka-hub.service`, `hub.env.example`, and `hub.toml.example`; the same
files live here for a build from source.

```
tar -xzf vyshka-hub_<version>_linux_amd64.tar.gz && cd vyshka-hub_<version>_linux_amd64
sudo useradd --system --home-dir /var/lib/vyshka --shell /usr/sbin/nologin vyshka
sudo install -m 0755 vyshka-hub /usr/local/bin/vyshka-hub
sudo install -d -m 0750 /etc/vyshka
sudo install -m 0640 hub.env.example /etc/vyshka/hub.env
sudo sh -c 'umask 077; printf "vya_%s" "$(openssl rand -hex 24)" > /etc/vyshka/admin-token'
sudo install -m 0644 vyshka-hub.service /etc/systemd/system/vyshka-hub.service
sudo systemctl daemon-reload
sudo systemctl enable --now vyshka-hub
curl -s http://127.0.0.1:8080/healthz
```

The token file stays root-owned: the unit hands it to the hub as a systemd credential
(`LoadCredential`, systemd 247 or later), so the service user never reads `/etc/vyshka`
and the token is in neither the environment nor the process list. Read it back with
`sudo cat /etc/vyshka/admin-token` when signing in to the panel. `/var/lib/vyshka` is
created by systemd and owned by the service user; the SQLite file and its WAL files land
there, and map tilesets go under `/var/lib/vyshka/maps` with `VYSHKA_MAPS_DIR` set in
`hub.env`. Everything else in `hub.env` is optional and commented; a Postgres URL there
replaces the SQLite file.

Upgrading is the new binary over `/usr/local/bin/vyshka-hub` and `sudo systemctl restart
vyshka-hub`; migrations run on boot. The unit's stop timeout is above the hub's 15 s drain,
and the same nginx server block terminates TLS in front of it. Logs go to the journal
(`journalctl -u vyshka-hub`), one JSON line per request.

## The config file

Flags and environment variables are enough for one hub. A TOML file is for the operator who
runs more than one hub, or wants the settings in a file rather than in a unit.
`hub.toml.example` lists every setting with its comment; a key is a `serve` flag's name
without the dash, at the top level:

```toml
addr = "127.0.0.1:8080"
log-level = "info"
backup-keep = 14
auto-tls-hosts = ["hub.example.org", "hub2.example.org"]   # or one comma-separated string
```

**Naming it.** `-config /path/hub.toml` or `VYSHKA_HUB_CONFIG=/path/hub.toml`. The hub reads no
file it was not told about: there is no search path and no default location, so a file that
merely sits in the working directory changes nothing. The variable is `VYSHKA_HUB_CONFIG`
because `VYSHKA_CONFIG` belongs to the `vyshka` command-line client (its JSON profile file,
`cmd/vyshka/README.md`). The two programs share no variable, and a test holds every hub
setting's variable apart from the ones the client reads.

**Precedence, highest first:** a command-line flag, an environment variable, the config file,
the built-in default. A hub with no file behaves exactly as it always did, and a unit or
container can override one value of a file several hubs share. An empty environment variable
counts as unset; an explicitly empty flag (`-db=`) is an answer and wins.

**Mistakes stop the hub.** An unknown key (a typo), a key of the wrong type, a key set twice,
or a file that is not TOML makes `serve` refuse to start, with a message that names the file
and the key and never a value (a syntax error names only its line, because the parser would
otherwise quote the text it choked on, which for an unquoted secret is the secret; it adds a
hint only for a file that is not UTF-8 and for a backslash inside a double-quoted string, where
TOML wants a Windows path in single quotes: `"C:\temp"` is a tab, not a mistake the parser can
see). Types follow the flag: strings, integers, booleans, and durations as strings (`"30s"`); a
list setting (`auto-tls-hosts`) takes an array of strings or one comma-separated string. A key
is checked even when a flag or an environment variable overrides it. Because the mapping is
generic over the flag set, a flag added in a later release is accepted in the file with no
other change.

**Secrets.** `db` (a Postgres URL carries a password) and `admin-token` take the same
`file:/path` form from every source: a flag, an environment variable, or the file. The file
named is read at start and must be a secret in shape: one line of UTF-8 text of at most
4096 bytes, with no NUL byte; surrounding whitespace and the newline an editor leaves are
trimmed. An empty, larger, multi-line, or binary file, or an unreadable one, stops the hub with
a message naming the setting, where it came from, and the secret file's path, and nothing from
the file. For `db` the file must hold a `postgres://`, `postgresql://`, or `sqlite://` URL.

*`-db file:...` changed meaning.* Before the config file, a `db` value starting `file:` was a
SQLite URI. It now names a file that holds the database URL, and a SQLite URI is written
`sqlite://file:...` (`DATABASE_URL=sqlite://file:/var/lib/vyshka/vyshka.db`). That spelling
works for `serve`, which hands the text to the SQLite driver; `storage` and `vacuum` always
took the text as a plain path and still do, so give them `-db /var/lib/vyshka/vyshka.db`, never
a URI. An old `DATABASE_URL=file:/var/lib/vyshka/vyshka.db` is refused with this explanation
rather than read as a secret (a database is not a one-line text file).

From the config file, a relative `file:` path is relative to the config file's own directory;
from a flag or an environment variable it is relative to the working directory, as before.
Prefer references to inline values. If the file holds inline secrets (an admin token, or a
database URL with a password) and others can read it (the world bit), the hub logs one warning
naming the file and the keys, never the values; a file shared with a service group, or a
systemd credential, is a normal layout and does not warn (not checked on Windows). A plain
SQLite path is not a secret. No secret is printed in usage text, in an error, in a log line,
or in the output of the next command.

**What the hub will use.** `vyshka-hub config` takes the arguments `serve` takes, loads flags,
environment, and file exactly as `serve` would, runs the same checks (the auto-TLS
combinations, the maps directory, the secret files), and prints each setting with its source,
then exits; it opens no database and no listener. Secrets are redacted: `admin-token` shows as
`<redacted>` or as its `file:` path, and `db` shows a plain SQLite path as it is and a
Postgres URL as scheme, host, and database name with the user, password, and query hidden:

```
$ VYSHKA_LOG_LEVEL=warn vyshka-hub config -config /etc/vyshka/hub.toml -addr 127.0.0.1:9000
config file: /etc/vyshka/hub.toml (from flag)
precedence: flag, then environment variable, then config file, then default

SETTING             VALUE                                         SOURCE
addr                127.0.0.1:9000                                flag
admin-token         <redacted>                                    file
backup-keep         3                                             file
db                  postgres://<redacted>@db.example:5432/vyshka  file
log-level           warn                                          env VYSHKA_LOG_LEVEL
...
```

It sees the environment of the shell that runs it, not the unit's. The `storage` and `vacuum`
commands take `-config` too, for `db` only (the other keys are checked, not used): one file
points the hub and its maintenance at the same database when `db` is in the file and nothing
higher-ranked sets it.

### Under systemd

`/etc/vyshka` is root's and the service user never reads it, so anything the hub reads from
there reaches it as a systemd credential, which lands in the unit's private directory (`%d`).
The layout that keeps `vyshka-hub config` and the unit agreeing:

- **`hub.toml` holds the non-secret settings** and names no `file:` path. Install it as
  `/etc/vyshka/hub.toml` and pass it as a credential.
- **The admin token stays the unit's existing credential** (`VYSHKA_ADMIN_TOKEN=file:%d/admin_token`)
  and is not named in `hub.toml`.
- **A Postgres URL** stays in `hub.env` (`DATABASE_URL=...`, root-only, read by systemd), as
  `hub.env.example` has always said, or becomes a credential of its own:
  `LoadCredential=database_url:/etc/vyshka/database-url` with
  `Environment=DATABASE_URL=file:%d/database_url`.

```
sudo install -m 0644 hub.toml.example /etc/vyshka/hub.toml   # edit it: uncomment what you set
sudo systemctl edit vyshka-hub
```

```
[Service]
LoadCredential=hub.toml:/etc/vyshka/hub.toml
Environment=VYSHKA_HUB_CONFIG=%d/hub.toml
```

(The unit has the same two lines commented, ready to enable.) Check it twice, once as root on
the file itself and once the way the unit will see it, as the service user with both
credentials in place:

```
sudo vyshka-hub config -config /etc/vyshka/hub.toml
sudo systemd-run --pty --wait --collect -p User=vyshka -p WorkingDirectory=/var/lib/vyshka \
  -p EnvironmentFile=-/etc/vyshka/hub.env \
  -p LoadCredential=hub.toml:/etc/vyshka/hub.toml -p LoadCredential=admin_token:/etc/vyshka/admin-token \
  /bin/sh -c 'VYSHKA_ADMIN_TOKEN=file:$CREDENTIALS_DIRECTORY/admin_token \
    exec /usr/local/bin/vyshka-hub config -config "$CREDENTIALS_DIRECTORY/hub.toml"'
```

The second command does not carry the unit's own `Environment=` lines (`systemctl show -p
Environment vyshka-hub` lists them). After `systemctl restart vyshka-hub`, the journal
(`journalctl -u vyshka-hub -o cat`) has an info line `config file` naming
`/run/credentials/vyshka-hub.service/hub.toml`, and no warning.

One catch for installs of the shipped unit: it sets `VYSHKA_ADDR` and `DATABASE_URL` as
`Environment=` defaults, and an environment variable outranks the file. To set `addr` or `db`
in `hub.toml`, delete those two lines from the unit (`vyshka-hub config` shows `env
VYSHKA_ADDR` as the source while they are there). Both name what the hub would choose
anyway: the loopback address, and `vyshka.db` in the unit's working directory. While they are
in place, the unit's `DATABASE_URL` is what `storage` and `vacuum` should be given too:
`-db /var/lib/vyshka/vyshka.db`.

Which to use: `hub.env` is flat `NAME=value` lines and the right place for a value that differs
on this host only; the file is the better home for a set of settings, because it documents
them, checks them at start, and shows where each came from. A value in `hub.env` overrides the
same key in `hub.toml`.

### With compose

The image sets `VYSHKA_ADDR=0.0.0.0:8080` and `DATABASE_URL=/data/vyshka.db` itself
(`Dockerfile`), and the base compose file sets `VYSHKA_ADMIN_TOKEN` and `VYSHKA_MAPS_DIR`.
Environment variables outrank the file, so for those four keys the file applies only once the
variable is released. An empty variable counts as unset, so `environment:` can release it:

```yaml
    environment:
      VYSHKA_HUB_CONFIG: /etc/vyshka/hub.toml
      DATABASE_URL: ""               # let the file's db apply, e.g. a Postgres URL
      VYSHKA_ADDR: ""                # let the file's addr apply (keep it 0.0.0.0:... inside a container)
    volumes:
      - ./hub.toml:/etc/vyshka/hub.toml:ro
```

Leave the ones you do not set in the file alone. A file that holds no secrets is read by the
container's user (uid 65532, not you), so make it readable by others:

```
cp hub.toml.example hub.toml                 # uncomment what you set; deploy/hub.toml is ignored by git and docker
chmod 0644 hub.toml                          # no secrets inside: it names them by file: path
# or, with a secret inline: sudo install -o 65532 -g 65532 -m 0600 hub.toml.example hub.toml
docker compose run --rm --no-deps hub config # what the container will use
```

A mode-0600 file owned by your own user cannot be read by uid 65532, and the hub says so at
start. Secrets in the file are `file:` paths of the container's own secret mounts, absolute
(`admin-token = "file:/run/secrets/admin_token"`, `db = "file:/run/secrets/database_url"`),
since the file's directory is `/etc/vyshka`.

## Backup and restore

### Taking a backup (SQLite)

`POST /api/v1/admin/backup` with an `admin` token writes a snapshot of the running hub's
database and answers once the file is complete and synced:

```
curl -s -X POST -H "Authorization: Bearer $(sudo cat /etc/vyshka/admin-token)" \
  http://127.0.0.1:8080/api/v1/admin/backup
{
  "backup": {
    "name": "vyshka-20261111T163538.508Z.db",
    "path": "/var/lib/vyshka/backups/vyshka-20261111T163538.508Z.db",
    "sizeBytes": 364544, "takenAt": "2026-11-11T16:35:38.508Z", "durationMs": 69
  },
  "kept": ["vyshka-20261111T163538.508Z.db"],
  "pruned": []
}
```

- **What it is.** The database as of one committed instant, taken with SQLite's
  `VACUUM INTO` on a read connection of its own, so the hub keeps serving while it runs
  (measured: a 60 MiB database copied in 100 ms with admin writes landing throughout, none
  refused). The copy is compacted and is a plain SQLite file a hub boots on directly. One
  thing in it differs from the live database on purpose: every plugin session open at that
  instant is closed, so after a restore each plugin opens a new session (see below).
- **Where it lands.** `-backup-dir` (env `VYSHKA_BACKUP_DIR`), by default a `backups`
  directory beside the database file: `/var/lib/vyshka/backups` under the systemd unit,
  `deploy/data/backups` under compose. Under the unit, a directory outside
  `/var/lib/vyshka` also needs a `ReadWritePaths=` line, because the unit mounts the rest
  of the file system read-only. Give each hub its own backup directory: rotation and the
  cleanup of `.partial` files assume one hub writes there. The hub names the file
  (`vyshka-<UTC time>.db`, or one millisecond after the newest snapshot already there when
  the clock is behind it, so name order is always creation order and the snapshot just
  taken is never the one rotated out). Nothing in the request is used: a body, if sent, is
  read only to digest it into the audit record, so no caller can choose a path. A snapshot
  being written is named
  `*.db.partial` and is never counted as a backup; one left by a crash is removed at the
  next backup.
- **How many are kept.** `-backup-keep` (env `VYSHKA_BACKUP_KEEP`, default 7). After each
  snapshot the oldest beyond the bound are deleted, and only files named like a snapshot
  are counted or touched. The directory therefore never holds more than that many
  databases plus the one being written, whoever holds the token; one snapshot runs at a
  time, and a second request meanwhile answers `409 conflict`.
- **Off the box.** The directory sits on the same disk as the database, which protects
  against a bad change or a corrupted file, not against losing the disk. Copy finished
  snapshots elsewhere (`rsync` or `rclone` the `*.db` files, never the `.partial` ones).
- **Scheduling.** Anything that can run `curl` on a timer will do: a cron line, or a
  systemd timer beside the unit. Use a token minted for the purpose (`POST /api/v1/tokens`
  with the `admin` scope, the only one this route accepts) so it can be revoked on its own.

Do not copy `vyshka.db` by hand while the hub runs. In WAL mode the recent writes live in
`vyshka.db-wal` until a checkpoint folds them in; measured after a hub was killed, the main
file was 4 KiB and everything was in a 1 MiB WAL.

### Taking a backup (Postgres)

The endpoint answers `409 conflict` with `details.backend` set to `postgres`: the hub has
no file to copy, and Postgres ships the right tool. Run `pg_dump` against the database in
`DATABASE_URL`:

```
pg_dump --format=custom --file=vyshka-$(date -u +%Y%m%dT%H%M%SZ).dump "$DATABASE_URL"
```

`pg_dump` reads one consistent snapshot while the hub serves. To restore, stop the hub,
restore into an empty database (`pg_restore --no-owner --dbname=<empty database URL>
<file>`), and before booting the hub close the sessions the dump carried, which the
SQLite snapshot does for itself (the reason is under "What a restore means"):

```
psql "<restored database URL>" -c "UPDATE sessions
  SET ended_at = to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD\"T\"HH24:MI:SS.MS\"Z\"'),
      end_reason = 'restored-from-backup'
  WHERE ended_at IS NULL"
```

Then point `DATABASE_URL` at it and boot. Performed on 2026-11-11 against Postgres 16: a
dump taken while the hub served restored into an empty database, the statement closed the
one live session, and the hub booted on the restored database answered the old session
token `401 session_invalid`, had the key written before the dump and not the one written
after it, and gave the plugin a new session for its stored secret. Restarting every game
server's plugin after the restore, so each opens a new session, reaches the same state
without the statement.
Everything else under "What a restore means" applies to Postgres too; step 4's checks of
the SQLite restore are plain SQL over the same columns, run against the restored database
and the one it replaced.

### Restoring (SQLite)

This is the procedure as performed on 2026-11-11, on a hub holding a server with a session,
events, a key, a ban, and a webhook, with a second key and ban written after the snapshot.
A second drill the same day exercised step 4 against server credentials, enrollment tokens,
and admin tokens changed after the snapshot. Both drills ran the commands' bare equivalents
on a local hub. The systemd and compose spellings below were not run as written (no Linux
host or Docker engine was at hand), so read each output before going on.

**Under systemd** (state in `/var/lib/vyshka`, service user `vyshka`):

1. Stop the hub: `sudo systemctl stop vyshka-hub`.
2. Move the current database aside, **with its `-wal` and `-shm` files**, into a directory
   named for this restore. Do not delete it: it is the only record of what happened after
   the snapshot, and step 4 reads it. Let any error stop you; a failed move here followed by
   step 3 would overwrite the live database.
   ```
   cd /var/lib/vyshka
   aside="before-restore-$(date -u +%Y%m%dT%H%M%SZ)"
   sudo -u vyshka mkdir "$aside"
   sudo -u vyshka mv vyshka.db "$aside/"
   for f in vyshka.db-wal vyshka.db-shm; do
     if [ -e "$f" ]; then sudo -u vyshka mv "$f" "$aside/"; fi
   done
   ls -l vyshka.db* 2>&1     # must report that nothing is left
   ```
   The `-wal` file must not stay beside the restored database. SQLite replays a WAL it
   finds next to a database without checking that the two belong together: measured,
   a snapshot restored next to the old WAL booted as the newer database, with the key and
   ban written after the snapshot back in place, which is not the restore that was asked
   for and, with different page layouts, can be a corrupt one.
3. Put the snapshot in place, owned by the service user:
   ```
   sudo install -o vyshka -g vyshka -m 0600 backups/vyshka-<time>.db vyshka.db
   ```
4. Before booting, find what the restore undid (below, "What a restore means"). This needs
   the `sqlite3` command-line tool on the host and only reads:
   ```
   sudo -u vyshka sqlite3 -readonly vyshka.db "ATTACH '$aside/vyshka.db' AS before;
     SELECT 'revoke server credentials again', r.id, r.name FROM servers r JOIN before.servers b ON b.id = r.id
      WHERE b.revoked_at IS NOT NULL AND r.revoked_at IS NULL;
     SELECT 'server secret set or rotated since: re-enroll', r.id, r.name FROM servers r JOIN before.servers b ON b.id = r.id
      WHERE b.revoked_at IS NULL AND b.secret_hash <> r.secret_hash;
     SELECT 'enrollment token live again: issue a new one', r.server_id, r.expires_at
       FROM enrollment_tokens r LEFT JOIN before.enrollment_tokens b ON b.token_hash = r.token_hash
      WHERE r.used_at IS NULL AND r.expires_at > strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
        AND (b.token_hash IS NULL OR b.used_at IS NOT NULL);
     SELECT 'revoke admin token again', r.id, r.name FROM admin_tokens r JOIN before.admin_tokens b ON b.id = r.id
      WHERE b.revoked_at IS NOT NULL AND r.revoked_at IS NULL;"
   ```
   The drill's output, for a server revoked, a server re-enrolled, a server enrolled with a
   token, and an admin token revoked, all after the snapshot:
   ```
   revoke server credentials again  01M75S9VHKF1GQPV51806H0EN1  revoked-after
   server secret set or rotated since: re-enroll  01M75S9VSHYCGQ4QZPA352BEYB  re-enrolled-after
   server secret set or rotated since: re-enroll  01M75S9W1PM1RHEVWMGTWHA9YX  token-used-after
   enrollment token live again: issue a new one  01M75S9W1PM1RHEVWMGTWHA9YX  2026-11-12T17:08:18.103Z
   revoke admin token again  01M75S9W6XYZFCQZJQAKK53VBH  revoked-after
   ```
5. Boot: `sudo systemctl start vyshka-hub`. Then act on step 4's list at once, before
   anything else: `DELETE /api/v1/servers/{id}/credentials` for each server to revoke
   again, `DELETE /api/v1/tokens/{id}` for each admin token, and
   `POST /api/v1/servers/{id}/enrollment-token` for each server whose secret changed or
   whose old enrollment token is live again. For a server whose secret changed, revoke its
   credentials first and issue the token second: a new token alone leaves the snapshot's
   secret accepted until the plugin enrolls, and enrolling clears the revocation (traced in
   the store, not run in the drill). Issuing a new token deletes the server's unused
   ones, so the resurrected token dies with it. Hand the new token to that server's plugin
   to enroll again. In the drill, the revoked server's secret went from `200` back to
   `401 credentials_revoked` and the revoked admin token from `200` to `401`.
6. Check: `curl -s http://127.0.0.1:8080/healthz` shows
   `"database":{"driver":"sqlite","ok":true,...}`, and `sudo -u vyshka vyshka-hub storage
   -db /var/lib/vyshka/vyshka.db` gives the row counts. Migrations run on boot as usual, so
   a snapshot taken by an older hub restores into a newer one.

**Under compose** (state in `deploy/data`, owned by the image's user `65532`). Run these
from `deploy/`; the steps are the same, with these spellings:

```
docker compose stop hub                                           # 1
aside="before-restore-$(date -u +%Y%m%dT%H%M%SZ)"                 # 2
sudo install -d -o 65532 -g 65532 -m 0700 "data/$aside"
sudo mv data/vyshka.db "data/$aside/"
for f in vyshka.db-wal vyshka.db-shm; do
  if sudo test -e "data/$f"; then sudo mv "data/$f" "data/$aside/"; fi
done
sudo ls -l data/                                                   # no vyshka.db* left
sudo install -o 65532 -g 65532 -m 0600 data/backups/vyshka-<time>.db data/vyshka.db   # 3
(cd data && sudo sqlite3 -readonly vyshka.db "ATTACH '$aside/vyshka.db' AS before; ...")   # 4, the queries above
docker compose start hub                                          # 5, then the same API calls
docker compose run --rm --no-deps hub storage -db /data/vyshka.db # 6
```

The database must stay owned by `65532`: a root-owned `0600` file is one the hub cannot
open, and the container then restarts in a loop.

What the first drill observed after booting: the server, both events, the key written
before the snapshot, the ban, and the webhook were back; the key written after it
answered 404; the plugin's old session token answered `401 session_invalid`, and its
stored server secret opened a new session at once, which polled normally.

### What a restore means

- **Everything after the snapshot is gone**: events, actions, key writes, bans, notes,
  webhook changes, and servers registered since. A server registered after the snapshot is
  unknown to the restored hub: register it again and enroll its plugin.
- **Credentials go back to the snapshot too**, and some of that reopens doors. Step 4 lists
  each case and step 5 says what to do about it:
  - A server whose credentials were **revoked** after the snapshot authenticates again with
    its old secret: the restored row has no `revoked_at`. Measured: `200` on a session
    start with the revoked secret.
  - A server that **re-enrolled** after the snapshot (a rotated secret), or enrolled for
    the first time, has the snapshot's secret back: its plugin's current secret answers
    `credentials_invalid` and the old one works. Measured for a re-enrollment; a first
    enrollment after the snapshot shows in step 4's query the same way.
  - An **enrollment token** used after the snapshot is unused again. It enrolls a plugin,
    replacing that server's credentials, until it expires (24 h by default). Measured: a
    used token enrolled again with `201`. A token that a newer one replaced after the
    snapshot comes back the same way (issuing a token deletes the server's unused ones;
    traced in the code, not run), and step 4's query lists it too.
  - An **Admin API token** minted after the snapshot no longer exists, and one **revoked**
    after it is valid again. Measured: `200` with the revoked token.
- **Actions in flight.** An action dispatched before the snapshot and completed after it is
  `delivered` or `running` in the copy. Its outcome was acknowledged to the hub that is
  gone, so no plugin sends it again, and the restored hub marks it `expired` when its
  deadline passes. Traced in the code, not run.
- **Plugin sessions.** Every session open at the snapshot is closed in it, so each plugin's
  next poll answers `session_invalid` and the plugin opens a new session with its stored
  secret (spec/protocol.md section 5.3). That is the only safe outcome: the session's
  sequence state moved on after the snapshot, and a plugin carrying on in it would hold
  an ack the restored hub is behind, leaving a gap neither side can close. Measured with
  the closing disabled: the plugin's next two envelopes were answered `ack: 1` forever and
  never stored. In a new session both sides renumber whatever is unacked (section 9.1), so
  events a plugin had not yet had acknowledged are delivered again.
- **The ban list** goes back with the data, and its revision with it. Plugins walk the list
  whenever the revision they learn differs from the one they enforce, not only when it is
  higher (section 13.4), so they converge on the restored list; a list cursor from after
  the snapshot answers `409 conflict` and the walk starts over (section 13.3). The hub
  mints revisions from its clock (section 13.1), so its next change lands above every
  revision handed out before the restore (the demo checks this) **provided the clock was
  not set back**: restore onto a host with a correct clock.
- **Webhooks.** Deliveries pending at the snapshot are attempted again, under the same
  `deliveryId`, so a receiver that deduplicates on it (section 11.3) sees repeats, not new
  notifications.

### Retention and size

`vyshka-hub storage` reports each table's rows and bytes on disk and what each background
pass would do if it ran now. It opens the database read-only and never creates or
migrates one, so it is safe against a database a running hub has open (measured under
write load: no request refused) and changes no byte of a snapshot it is pointed at
(measured: the same checksum before and after). One case touches the directory, not the
database: a hub's own database after a clean stop is in WAL mode with no `-wal` beside it,
and SQLite creates the `-wal` and `-shm` files to read it (SQLite's rule for a read-only
open, not measured here), so that open needs a writable directory. On SQLite it also reports the file size, the free pages a vacuum would return,
and the WAL's size:

```
$ sudo -u vyshka vyshka-hub storage -db /var/lib/vyshka/vyshka.db
sqlite database /var/lib/vyshka/vyshka.db, at 2026-11-11T16:28:06Z
file 59.1 MiB, of which free 29.3 MiB (a vacuum returns it), WAL 2.0 MiB

                    TABLE  ROWS      SIZE
                  ...
What the background passes would do if they ran now (each pass works in bounded batches):
PASS                        TABLE                 EFFECT  ROWS
action expiry               actions               update  0
event retention             events                delete  0
...
```

`-json` prints the same as JSON. Webhook delivery retention is counted at the hub's default
of seven days. The `event_batches` row counts every expired dedup marker, but the event
pass removes a server's markers only once that server has no expired event left, so while
expired events remain it is an upper bound on what one pass removes; the output says so.

`vyshka-hub vacuum -db <file>` rebuilds a SQLite database to hand freed pages back to the
file system (Postgres reclaims space on its own with autovacuum). It needs the database to
itself: run against a live hub, a plain vacuum made one of the hub's writes fail with
`SQLITE_BUSY`, so the command takes SQLite's exclusive lock first and refuses while any
other process has the file open:

```
$ vyshka-hub vacuum -db /var/lib/vyshka/vyshka.db        # hub running
vyshka-hub: vacuum: another process has this database open; stop the hub first (database is locked (5) (SQLITE_BUSY))
$ sudo systemctl stop vyshka-hub
$ sudo -u vyshka vyshka-hub vacuum -db /var/lib/vyshka/vyshka.db
vacuumed /var/lib/vyshka/vyshka.db in 308ms: 59.1 MiB -> 29.8 MiB
$ sudo systemctl start vyshka-hub
```

A snapshot is already compacted, so taking a backup also shows what a vacuum would gain.

## Metrics

The hub keeps Prometheus metrics and serves them as `GET /metrics` in the text exposition
format (0.0.4) from a listener of its own, off by default:

```
vyshka-hub serve -metrics-addr 127.0.0.1:9190      # or VYSHKA_METRICS_ADDR=127.0.0.1:9190
curl -s http://127.0.0.1:9190/metrics
```

**The endpoint has no authentication, which is why it is not on the Plugin API listener.**
That listener faces the internet, and the numbers below say which servers are busy, how
long their queues are, and which webhooks fail. The main listener never routes `/metrics`:
it answers 404 like any unknown path, and the metrics listener answers 404 for every path
but `/metrics`. Bind the metrics listener to loopback (and scrape it from an agent on the
box, or over an SSH tunnel) or to a private network address; the hub logs a warning at
boot when it is bound anywhere else. Under compose, publish it on loopback the way the
main port is (`127.0.0.1:9190:9190` with `VYSHKA_METRICS_ADDR: 0.0.0.0:9190` inside the
container, which only the published mapping exposes outside the compose network); the
branch hub's compose file does this on `127.0.0.1:9190`, where the unauthenticated port is also
reachable from the other containers on its proxy network. The listener accepts at most 16
connections at once.

```yaml
# prometheus.yml
scrape_configs:
  - job_name: vyshka
    static_configs:
      - targets: ["127.0.0.1:9190"]
```

A scrape reads the queue depths from the database in one statement (about a millisecond, two at worst,
on a 295 MiB database with 780 000 rows, measured in
`spikes/metrics-scrape`), and every other value is memory. The statement is index-only (SQLite
plans a covering search for each half) but linear in what it counts: the outbound queues are
bounded at 5000 per server, and the webhook backlog is counted on every scrape, up to 1000
pending deliveries per webhook (a paused webhook stays at that bound until it is resumed;
replays are exempt from it). A very large backlog therefore makes scrapes slower, by about
60 microseconds per thousand pending deliveries: 7 ms at 100 000, which is a hundred paused
webhooks. Counters and histograms start at zero when
the hub starts, as Prometheus expects, and a labelled series appears the first time its
event happens, so use `rate()` and `increase()` rather than reading raw values.

### Naming and labels

Every metric starts with `vyshka_`, uses base units (seconds, bytes), and a counter ends
in `_total`. Label values come from a fixed vocabulary or from an identifier the operator
created, never from anything a plugin or a caller can mint:

- **`server`** is the server's id. One hub serves a handful of servers (the design persona,
  `CONTEXT.md`), so a series per server is cheap and is what an operator needs: which
  server's queue is backing up. Join to names with `GET /api/v1/servers`. A hub meant to
  hold thousands of servers would have to drop this label first; that is not a supported
  deployment.
- **`webhook`** is the webhook's id, for the same reason: which target is failing.
- **`code`** on action outcomes is the action's code, such as `dayz.player.kick`. It is
  declared in the plugin's manifest, but a series exists only for an action an operator
  dispatched, so a plugin cannot mint series by publishing manifests. The action's `error`
  text, which a plugin writes freely, is **not** a label.
- Everything else (`outcome`, `result`, `reason`, `kind`, `sweep`) is a closed set listed
  below. No player identifier appears anywhere.

As a backstop, one metric keeps at most 1000 distinct label sets; the one that would be
the 1001st is folded into a series whose labels all read `other`.

### Poll latency

A long-poll is held open on purpose (up to the negotiated `pollTimeout`, 25 s for DayZ), so
a request's total duration measures the plugin's cadence, not the hub. The metrics split
it three ways:

- `vyshka_poll_handler_seconds` is the time an authenticated poll spent in the hub **outside
  the hold**: what runs before the poll first parks (reading the request, applying acks and
  envelopes in one database transaction, the first queue read) and after it last wakes (the
  final queue read and the response). This is the latency to alert on. On SQLite it
  includes waiting for the single connection, so it rises when anything else (a sweep, an
  admin write) holds it. It does not grow with how long a poll is held.
- `vyshka_poll_hold_seconds` is the hold: from the first time the poll parks to the last
  time it wakes, which includes the one-second rechecks a held poll makes of its session
  and queue in between. Near `pollTimeout` means an idle server; near 0 means work arrived
  or the poll was answered at once.
- `vyshka_envelope_delivery_seconds` is the time an envelope waited between being queued
  (an action dispatched, a notice, a raw envelope) and the first poll response that carried
  it. It is the operator's "how long until my kick reaches the game server" less the
  plugin's own cycle, and for a server whose plugin is down it is how long the queue has
  been waiting. A retransmission is not observed again.

### Reference

| Metric | Type | Labels | What moves it |
|---|---|---|---|
| `vyshka_build_info` | gauge | `version`, `goversion`, `database` | Always 1; the labels say what is running |
| `vyshka_start_time_seconds` | gauge | none | Set at boot; `time() - vyshka_start_time_seconds` is uptime |
| `vyshka_poll_handler_seconds` | histogram | none | Every authenticated poll, once, when it ends, for the time outside its hold; refused credentials are not counted |
| `vyshka_poll_hold_seconds` | histogram | none | The same polls, for the time held; 0 for one answered without a hold |
| `vyshka_envelope_delivery_seconds` | histogram | none | An envelope's first appearance in a poll response |
| `vyshka_outbound_queue_depth` | gauge | `server` | Read at scrape time: envelopes queued for the server and not yet acked, sent or not (the bound is 5000, spec section 9.2). One series per server that is enrolled and not revoked, 0 when empty; a revoked server's series disappears at the next scrape (and comes back if it enrolls again), and a server that never enrolled has none |
| `vyshka_outbound_queue_oldest_seconds` | gauge | `server` | Read at scrape time: age of the longest-waiting of those; 0 when empty |
| `vyshka_webhook_deliveries_pending` | gauge | none | Read at scrape time: deliveries waiting for an attempt or a retry, across all webhooks |
| `vyshka_actions_finished_total` | counter | `code`, `outcome` = `completed`, `failed`, `expired` | An action leaving its last non-terminal state: the plugin's result, the expiry sweep, or a read that finds the deadline passed. Counted once, where the transition commits |
| `vyshka_inbound_envelopes_total` | counter | `result` = `accepted`, `duplicate`, `gapped` | Envelopes in plugin polls: taken in order, already acked and ignored, or ahead of a missing seq and not taken (section 9.1) |
| `vyshka_inbound_envelopes_refused_total` | counter | `kind` = `manifest`, `event_batch`, `state_snapshot` | An accepted envelope the hub would not apply and answered with a rejection notice (sections 6.4, 8.1, 8.3); also counted in `accepted` |
| `vyshka_outbound_envelopes_dropped_total` | counter | `reason` = `queue_full`, `notice_cap` | A hub-originated notice that was never queued: the server's queue was at its bound (rejection notices, `bans.changed`), or one poll owed more than 20 rejection notices. A dispatch refused with `outbound_queue_full` is an error to its caller, not a drop, and is not counted here |
| `vyshka_webhook_attempts_total` | counter | `webhook`, `result` = `delivered`, `retry`, `dead` | A delivery attempt the store booked: a 2xx; a failure with another attempt scheduled; a failure that exhausted the schedule (section 11.5) |
| `vyshka_rate_limited_total` | counter | `api` = `plugin`, `admin` | A request refused `429 rate_limited` because its server or token was over its limit ("Rate limits" below). No per-credential label: the hub's log names the server or token, once a minute per credential while it is refused |
| `vyshka_sweep_runs_total` | counter | `sweep`, `result` = `ok`, `error` | One run of a background sweep |
| `vyshka_sweep_removed_total` | counter | `sweep` | Rows the run changed: actions expired by the sweep, bans lifted, rows deleted by retention |
| `vyshka_sweep_duration_seconds` | histogram | `sweep` | Each run; on SQLite this is how long the run held the database's only connection |
| `vyshka_sweep_last_success_timestamp_seconds` | gauge | `sweep` | Each run that did not fail |
| `vyshka_collect_errors_total` | counter | `collector` | A scrape-time database read failed; its gauges are missing from that scrape |

The `sweep` values are `action_expiry` (every 0.5 s), `ban_expiry` (every 5 s),
and the retention passes `prune_events`, `prune_audit`, `prune_kv`,
`prune_state_snapshots`, and `prune_webhook_deliveries` (every 5 minutes). An action the
read path expired (`GET /api/v1/actions/{id}` after its deadline) is counted in
`vyshka_actions_finished_total` but not in `vyshka_sweep_removed_total`, because the sweep
never saw it.

Webhook failure rate is derived: the rate of attempts with `result!="delivered"` over the
rate of all attempts, per webhook. Some starting alerts:

```yaml
# A server that is up but not polling. Revoking a server removes its series, so
# this stops firing for it; a down server that is still enrolled keeps firing,
# which is the point.
- alert: VyshkaQueueNotDraining
  expr: vyshka_outbound_queue_oldest_seconds > 300
- alert: VyshkaWebhookFailing
  expr: >
    sum by (webhook) (rate(vyshka_webhook_attempts_total{result!="delivered"}[15m]))
      / sum by (webhook) (rate(vyshka_webhook_attempts_total[15m])) > 0.5
- alert: VyshkaSlowPolls
  expr: histogram_quantile(0.99, sum by (le) (rate(vyshka_poll_handler_seconds_bucket[5m]))) > 1
- alert: VyshkaSweepStalled
  expr: time() - vyshka_sweep_last_success_timestamp_seconds{sweep="action_expiry"} > 60
# Some credential has been over its limit for a while: a leaked or runaway one,
# or a limit set below what a real client needs. The log names which.
- alert: VyshkaRateLimited
  expr: sum by (api) (rate(vyshka_rate_limited_total[10m])) > 1
```

Not here, deliberately: Go runtime and process metrics, per-route HTTP counters, and
anything about players. A later metric is a field in `hub/metrics.go` and a one-line call
where the event happens.

## Rate limits

The hub limits how fast each credential may call it (spec section 2.2.1), so a leaked
server secret or a runaway script is bounded in what it can do to the hub and to the other
servers on it. Each enrolled server has a token bucket for its Plugin API requests, and each
admin token, the bootstrap one included, has one for its Admin API requests. A request over
the limit is answered `429 rate_limited` with a `Retry-After` in whole seconds (and the same
number in `details.retryAfterSeconds`), and nothing in it is applied: no envelope taken, no
ack moved, no action dispatched, no session started. The plugin sends it again after the
wait and loses nothing.

| Flag | Environment | Default | Meaning |
|---|---|---|---|
| `-plugin-rate-limit` | `VYSHKA_PLUGIN_RATE_LIMIT` | 5 | Plugin API requests per second each server may sustain; 0 turns the limit off |
| `-plugin-rate-burst` | `VYSHKA_PLUGIN_RATE_BURST` | 60 | Requests a server may send back to back before the rate applies |
| `-admin-rate-limit` | `VYSHKA_ADMIN_RATE_LIMIT` | 10 | Admin API requests per second each token may sustain; 0 turns the limit off |
| `-admin-rate-burst` | `VYSHKA_ADMIN_RATE_BURST` | 60 | Requests a token may send back to back before the rate applies |

What counts, and what does not:

- A held poll is one request, however long it is held. A plugin idling on the default
  25 s hold spends one token per 25 s.
- A poll the hub answers with work an admin or the hub itself queued (an action dispatch,
  a context question, a raw envelope, a `bans.changed` after a ban edit or a ban's expiry),
  sent for the first time, is not charged: the plugin is answering the hub. (A
  `bans.changed`'s cause is kept in memory, so the one poll that carries a notice queued
  before a restart is charged.) Without that,
  an admin token dispatching at its own 10 a second to one server would throttle that
  server's plugin about 12 s in, and the refusals would delay the very actions being
  dispatched. Retransmissions, rejection notices, and the `bans.changed` a plugin's own
  manifest causes are charged, because a plugin's own traffic can make the hub queue
  those; a leaked server credential cannot earn an uncharged poll without an admin
  queueing work for it.
- The walk of the ban list a `bans.changed` asks for is the plugin's own requests, one per
  100 entries, and is charged. So a bulk ban import costs every server declaring `bans`
  one walk per change it sees. Measured at the defaults, with two such plugins walking
  after every notice and one `bans:manage` token importing as fast as it is allowed: up to
  200 changes cost the plugins no refusals, and some from about 300 (3 to 6 each at 300,
  25 to 33 at 500), each a one-second delay to that plugin's next request, nothing lost. An operator importing
  thousands of bans sees `vyshka_rate_limited_total{api="plugin"}` move while the import
  runs; raising `-plugin-rate-burst` for the import is the remedy.
- A request counts against the server or token it authenticated as, and nothing else does.
  A flooding server cannot spend another server's allowance, and a request with a wrong
  or unknown credential counts against nobody: keying on a server id the request merely
  claims would let anyone who has seen the id lock that server out. Those requests are
  bounded by the connection cap and by the proxy in front (below).
- Enrollment is never limited; the enrollment token is spent by its first success. A
  session request counts once its secret has checked out.
- Admin requests are checked before scope, so a token flooding requests it may not even
  make is refused here. Its refused writes are audited as a run (spec section 10.5): the
  first at once, then at least one a minute while the refusals go on, then a closing record
  (`detail.runClosed: true`) within a second of the run ending, a minute after its last
  refusal, or at a clean shutdown. Each record names one refused request and carries in
  `detail.unrecordedRefusals` how many more the token had refused since the run's previous
  record, on any path and any server, so the records and their counts add up to every
  refused write; filter the audit log by `tokenId`, not `serverId`, to see a run whole. A
  record the hub fails to write keeps its count for the next one; only a hub that stops
  abruptly loses the count since a run's last record. The request log demotes 429s to
  `debug`; the warning `... is over its rate limit`, once a minute per credential, names
  the server or token and counts what it refused since, and `a run of rate-limited
  requests ended` closes it.

Where the defaults come from: the capacity spike of issue #87
(`spikes/hub-capacity/README.md`, "Request rates per credential") measured a legitimate
DayZ-shaped plugin at 0.8 requests/s per server over its busiest 10 s and 4 requests in its
busiest second (3 for the realistic profile), and a panel-like admin client at 6.9 requests/s per token over its busiest
10 s and 15 in its busiest second in passing runs (24 in a run that failed its bounds, on a
hub without limits). With these defaults on, the spike's final runs had no request refused inside a measured window. The plugin default is about six times that busiest 10 s,
and its burst of 60 covers a backlog drain of a full 5000-envelope queue (25 polls of 200);
it is wider than the spike's recommended 2/s and 30 because a plugin's key/value traffic was
not measured and the mod surface hands the store to third-party mods. Session start was
outside the spike's window; measured on its own, the reference driver's is four requests
(enrollment, which is never charged, a session, one page of the ban list, a poll), and a
plugin walking the ban list spends one request per 100 entries, so the burst covers a list
of a few thousand entries read back to back. The admin default is
the spike's recommendation, about 1.4 times the busiest panel client's 10 s and four times
its busiest second in a passing run. A panel with more views open than the spike modeled, several people sharing
one token, or a script paging through a long event feed can need more: give each admin and
each script its own token, which is also what keeps one runaway from throttling the others,
or raise the flags. Every default is in `hub/ratelimit.go`, which cites the same source.

Who the limit bounds: a leaked server credential, and a leaked or runaway token without the
`admin` scope. An `admin`-scope token (the bootstrap one included) can mint tokens, each
with an allowance of its own, as it can already do anything else; revoke it, or remove a
bootstrap token from the hub's configuration.

The DayZ plugin (0.9.0 and 0.10.0) reads the 429 and keeps its session, but it does not yet
read the wait the hub names, fails a mod's key/value call outright, and restarts a ban-list
walk from its first page on a refusal, so an installation list of roughly 8000 entries or
more cannot finish walking at the default plugin limit; for such a hub raise
`-plugin-rate-burst` or `-plugin-rate-limit` (`plugins/dayz/README.md`, "Rate limits").

What the limit does not bound is how much a request carries. A poll may carry 200 envelopes
and 1000 events, so a credential within 5 polls a second can still send far more events than
the hub was measured to ingest: about 920 events/s in total, across 75 servers on one
SQLite connection, with every ingest bound held. The largest step where every bound held
was lower, 614 events/s at 50 busy servers, because the admin server list fell behind first. The request limit bounds how often a credential can make the hub do
work, not how much work each request is; an event or byte budget per credential is a
separate, later change, and until then a leaked server credential can still load the hub's
ingest path. Revoke it (`DELETE /api/v1/servers/{id}/credentials`); that ends its session at
once.

The connection cap comes first. The hub accepts at most 256 connections at once (no flag;
`Config.MaxConns` for an embedder), and a connection over it waits unaccepted in the
kernel's backlog. Every connected plugin holds one connection for its held poll, and the
panel a few. The rate limit only runs after a connection has been accepted and a request on
it has been read and authenticated, so a flood of connections, authenticated or not, meets
the cap before it meets any limit here, and a flood that fills the cap delays every plugin's
next poll whatever its credential. Bounding connections per client is the proxy's job.

The limits are not a substitute for the proxy's own. nginx sees every connection, including
ones that never authenticate, and `limit_conn` there (`nginx-vyshka.conf.example`) bounds
what an anonymous client can hold; the hub sees only who a request authenticated as, and
bounds what each credential can make it do. Keep both. If you add `limit_req` at the proxy,
set it well above the hub's per-credential figures times the servers behind one address,
and give it `limit_req_status 429`: the proxy's refusal carries no `Retry-After` and no
protocol body, and a plugin on an engine that cannot read a status (the DayZ one) sees it
as an opaque client error, which the hub's own refusal never is.

## Without a proxy: `-auto-tls`

Game servers usually run on other machines than the hub, so every plugin reaches it over
TLS. With `-auto-tls` the hub obtains a certificate over ACME (Let's Encrypt production by
default), renews it, and terminates TLS itself, so the proxy becomes optional. Turning it on
accepts the CA's subscriber agreement.

| Flag | Env | Meaning |
|---|---|---|
| `-auto-tls` | `VYSHKA_AUTO_TLS` | Turn it on. `-addr` is then the TLS listener and defaults to `:443` when not given |
| `-auto-tls-hosts` | `VYSHKA_AUTO_TLS_HOSTS` | Required. Comma-separated DNS names; the hub requests certificates for these and no others, whatever name a client asks for |
| `-auto-tls-cache` | `VYSHKA_AUTO_TLS_CACHE` | Where the ACME account key and certificates live, created 0700 with 0600 files. Default: `acme-cache` beside the SQLite file; required with Postgres |
| `-auto-tls-email` | `VYSHKA_AUTO_TLS_EMAIL` | Optional contact address for the CA |
| `-auto-tls-directory` | `VYSHKA_AUTO_TLS_DIRECTORY` | The ACME directory URL: Let's Encrypt staging to rehearse, or a private CA |
| `-auto-tls-ca-roots` | `VYSHKA_AUTO_TLS_CA_ROOTS` | A PEM file of extra roots for the directory's own TLS, for a private or test CA |
| `-auto-tls-http-addr` | `VYSHKA_AUTO_TLS_HTTP_ADDR` | The plain listener, default `:80`; `off` drops it |

The CA validates over TLS-ALPN-01 on the TLS listener, which is enough on its own when the
CA can reach port 443. The plain listener adds HTTP-01, so issuance should still work when
only port 80 is reachable from the CA, and it redirects a browser that typed `http://` to
the panel over https. It answers the ACME challenge path and that redirect (GET and HEAD
for a configured name, with a path as the target) and nothing else: the Plugin and Admin
APIs exist only on the TLS listener, so a plugin misconfigured with `http://` gets an error
instead of a hub that answers in clear. Set it `off` when port 80 belongs to something
else.

Issuance through HTTP-01 is traced from the ACME library's source, not demonstrated:
`scripts/demo-auto-tls.sh` issues over TLS-ALPN-01 every time, because the library tries
that first and the test CA can reach the TLS port. The test rig cannot show HTTP-01 as it
stands: the test CA puts the port in the Host header of its validation request
(`name:<port>`, since its HTTP-01 port is not 80 there), and the library matches the Host
against the configured names exactly, so it would refuse. A real CA validates on port 80
and sends the bare name, which the library accepts.

The TLS listener is the same server as the plain one: the same handler, timeouts,
connection cap (256 connections), and drain on shutdown, TLS 1.2 or later. The plain
listener has a cap of its own, 64 connections, so idle connections there cannot use up
the memory or file descriptors the TLS listener needs. The TLS listener speaks HTTP/1.1
only, also to clients that offer HTTP/2: the hub's bounds are per connection, and over
HTTP/2 one connection carries many requests, so they would stop bounding what they bound.

Each configured name gets one ECDSA certificate, served to every client whatever it
offers. A client that cannot verify ECDSA at all gets no certificate; every current
browser and TLS library can. A handshake never orders a certificate or waits on the CA: it
is answered from the certificate already in memory, and ordering happens only at boot and
on the retry schedule below (plus the library's own renewal timer).

What the hub does when things go wrong:

- **Incoherent flags** (on without hosts, a wildcard or IP as a host, auto-TLS flags set
  with `-auto-tls` off, Postgres without `-auto-tls-cache`): `serve` refuses to start and
  says which.
- **Cache directory not creatable or not writable:** `serve` refuses to start.
- **CA unreachable at boot, or validation failing:** the hub starts and serves, logs
  `tls certificate unavailable` with the CA's error and the time to the next attempt, and
  handshakes fail until a certificate is obtained. It retries after 5 minutes, then 10, 20,
  40, and every hour from then on, starting over at 5 once it succeeds; handshakes do not
  retry. Each attempt with DNS or a port wrong is a failed validation, or two with the plain
  listener on (TLS-ALPN-01, then HTTP-01), and Let's Encrypt allows only five of those per
  name per hour, so rehearse against its staging directory
  rather than spend production's limits while debugging.
- **The host's clock behind:** if the clock is more than a little behind, the hub rejects a
  certificate the CA did issue, because it is not valid yet by the hub's clock, and logs
  `tls certificate unavailable`. Every retry is then a real issuance counted against the
  CA's duplicate-certificate limit (five a week for the same names at Let's Encrypt), which
  the schedule above spends in a few hours. Keep the host on NTP.
- **Renewal failing:** renewal starts when a third of the certificate's lifetime is left
  (about 30 days for Let's Encrypt's 90-day certificates). While it fails the hub keeps
  serving the cached certificate and logs `tls certificate renewal overdue` with the expiry;
  it logs `tls certificate renewed` once renewal succeeds.
- **A name outside `-auto-tls-hosts`:** the handshake fails and no order is placed.

Key material is never logged. The cache directory holds it, so back it up with the
database and keep it private.

**With compose,** point the DNS name at the host, set it in `docker-compose.auto-tls.yml`,
and layer that file over the base one. It publishes 443 and 80 on all interfaces in place of
loopback 8080, and the cache lands in `./data/acme-cache`:

```
docker compose -f docker-compose.yml -f docker-compose.auto-tls.yml up -d --build
docker compose logs hub | grep 'tls certificate'
curl -s https://hub.example.org/healthz
```

**Under systemd,** the service user needs to bind 443 and 80. Grant it exactly that
capability with a drop-in (`sudo systemctl edit vyshka-hub`), which leaves the rest of the
unit's confinement as it is:

```
[Service]
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
```

The unit's empty `CapabilityBoundingSet=` would otherwise strip the capability; a non-empty
assignment in a drop-in adds to it. Then set `VYSHKA_ADDR=:443`, `VYSHKA_AUTO_TLS=true`, and
`VYSHKA_AUTO_TLS_HOSTS` in `/etc/vyshka/hub.env` (the commented block in `hub.env.example`)
and restart. The cache defaults to `/var/lib/vyshka/acme-cache`, inside the unit's state
directory.

**Still prefer a proxy** when the host already serves other sites on 80 and 443 (two
programs cannot both bind them), when the hub shares a name or certificate with other sites,
when you want per-client connection limits or access logs at the edge (the hub's connection
cap is global), or when the certificate cannot come from an ACME CA that reaches this host
over HTTP-01 or TLS-ALPN-01: a wildcard or DNS-01 setup, or an internal PKI.

## Sizing

Measured in `spikes/hub-capacity` (issue #87). The figures hold for these conditions:
- this hub (`main` at `0e3a163`, rate limits on) on SQLite, confined to 1 vCPU and 2 GB;
- the database on an ext4 disk backed by an NVMe SSD;
- loopback with no TLS, inside a WSL VM on a desktop with nothing else running;
- an empty database at the start of every run.

The load was one of two assumed mixes, per 100-player server:

| Profile (assumed) | Events/s | Actions/min | Largest step where every run passed |
|---|---|---|---|
| Realistic | 1.2 | 1 | **200 servers**: 3 of 3 runs at 150 and at 200 |
| Busy | 12.3 | 6 | **50 servers**: 3 of 3 runs at 25 and at 50; 0 of 3 at 75 |

"Passed" means every latency, error, and backlog bound the spike set held, with no
request refused by the rate limits inside the measured window. CPU averaged at most a fifth of the quota and the hub
used about 50 MiB of its own memory.

**What fails first** is the admin server list, `GET /api/v1/servers`. It reads the database
three times per server on SQLite's single connection, and every busy 75-server run failed
on it alone. On a busier or slower disk the failure comes sooner: every commit waits for a
synced write (`synchronous=FULL`). To compare a box with the disk measured, run
`dd if=/dev/zero of=probe bs=4k count=500 oflag=dsync` in the data directory; with the
host idle it took 1.2 to 1.4 ms a write.

**The connection cap.** The hub accepts at most 256 connections at once. That is shared
with every other open connection: each connected plugin keeps one, each admin client or
browser holds a few, and a reverse proxy's upstream connections and a plugin that opens a
second HTTP context hold more. In the spike, 200 servers and 20 admin clients used about
230. At that share the cap is reached near 225 servers, and the spike's 250-server runs
failed on it. Neither `serve` nor the config file has a setting for the cap.

**Size the disk first.** At the default retention (events 30 days, `core.player.chat` 90,
audit records 365, action rows never deleted), measured composition gives:

| Per 100-player server | Realistic | Busy |
|---|---|---|
| Growth per day, days 1 to 30 | about 127 MiB | about 1.19 GiB |
| Database after 90 days | about 4.7 GiB | about 39 GiB |
| Database after a year | about 5.5 GiB, still growing | about 44 GiB, still growing |

A 40 GB disk therefore holds about 7.8 realistic 100-player servers at 90 days and 6.8
after a year, and an 80 GB disk 15.7 and 13.6. That is well below what the hub's latency
allows. Retention has no flag and no config-file key today, so it cannot be shortened to
trade history for servers. Size the disk to it, and leave room for the OS and for
backups; `vyshka-hub storage` (above) shows what a running database holds.

## A branch hub beside a live one

To try unreleased work over a real network without touching a hub that serves live game
servers, run a second, disposable hub on the same box from `branch-hub.compose.yml`. It
has its own container (`vyshka-hub-branch`), loopback port (8090), database, and admin
token, and it joins the reverse proxy's network so a subdomain of its own can point at it.
Nothing is built on the box: `scripts/deploy-branch-hub.sh` cross-compiles the current
checkout here, copies the binary and the compose file over ssh, restarts the container,
and waits until `/healthz` reports the version it built (the branch name and commit,
with `-dirty` for uncommitted changes).

```
# once, on the box
sudo mkdir -p /opt/vyshka-branch && sudo chown "$USER" /opt/vyshka-branch
echo VYSHKA_PROXY_NETWORK=<proxy-project>_default > /opt/vyshka-branch/.env

# from any branch, here
VYSHKA_DEPLOY_HOST=<ssh-host> scripts/deploy-branch-hub.sh            # keep the data
VYSHKA_DEPLOY_HOST=<ssh-host> scripts/deploy-branch-hub.sh --reset    # start empty
```

The first run creates the admin token (`sudo cat /opt/vyshka-branch/admin-token`).
`--reset` keeps the old database beside the new one as `data.<timestamp>`. The proxy
needs a server block like `nginx-vyshka.conf.example` with its own `limit_conn_zone`
names and `proxy_pass` to `http://vyshka-hub-branch:8080`.
