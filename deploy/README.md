# Deploying the hub

The hub is one static binary; `Dockerfile` at the repository root wraps it in a distroless
image running as a non-root user, with the SQLite file on `/data`. This directory holds two
reference single-host layouts, both with the hub on loopback and a reverse proxy terminating
TLS in front of it (spec section 3.3): the container with compose, and the binary from a
release archive under systemd. An operator who already runs Postgres sets `DATABASE_URL`
instead of using the SQLite file; the accepted URL forms are in the repository README under
"Database".

| File | What it is |
|---|---|
| `docker-compose.yml` | Builds the image from the repository and runs it on `127.0.0.1:8080` with the admin token read from a file, never from the environment |
| `nginx-vyshka.conf.example` | A server block with the two settings a hub needs from its proxy: a read timeout above the 60 s poll hold, and a body cap at or above the hub's 1 MiB |
| `vyshka-hub.service` | A systemd unit for the static binary: a dedicated user, the state under `/var/lib/vyshka`, the admin token as a systemd credential, confinement a static Go binary tolerates |
| `hub.env.example` | The unit's environment file, `/etc/vyshka/hub.env`: listen address, database URL, maps directory, log level |
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
of `cmd/vyshka/README.md`), `vyshka-hub.service`, and `hub.env.example`; the same two files
live here for a build from source.

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
