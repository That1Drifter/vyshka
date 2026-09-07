# Deploying the hub

The hub is one static binary; `Dockerfile` at the repository root wraps it in a distroless
image running as a non-root user, with the SQLite file on `/data`. This directory holds the
reference single-host layout: the container on loopback, a reverse proxy terminating TLS in
front of it (spec section 3.3).

| File | What it is |
|---|---|
| `docker-compose.yml` | Builds the image from the repository and runs it on `127.0.0.1:8080` with the admin token read from a file, never from the environment |
| `nginx-vyshka.conf.example` | A server block with the two settings a hub needs from its proxy: a read timeout above the 60 s poll hold, and a body cap at or above the hub's 1 MiB |

```
cd deploy
printf 'vya_%s' "$(openssl rand -hex 24)" > admin-token
chmod 0400 admin-token && sudo chown 65532:65532 admin-token
mkdir -p data && sudo chown 65532:65532 data
docker compose up -d --build
curl -s http://127.0.0.1:8080/healthz
```

`65532` is the distroless `nonroot` user; the bind-mounted data directory and the secret
file must be readable by it. Read the token back from `admin-token` when signing in to the
panel or calling the Admin API; the hub never logs a configured token.

Upgrading is `git pull && docker compose up -d --build`. Migrations run on boot, and the
database file survives the container (it lives in `./data`).

A plain-HTTP port on the public internet is deliberately not part of this layout: every
bearer token the hub sees would travel in clear. Engines whose HTTP client cannot do TLS
are a reason to put a TLS-terminating proxy near the game server, not to open the hub.
