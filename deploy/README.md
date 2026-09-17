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
`SHA256SUMS`) carries the binary, `vyshka-hub.service`, and `hub.env.example`; the same two
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
