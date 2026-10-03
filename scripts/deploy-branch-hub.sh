#!/usr/bin/env bash
# Deploys the current checkout as the branch hub: a disposable second hub on a
# box that already runs a live one (deploy/branch-hub.compose.yml). Builds a
# static Linux binary here, copies it and the compose file over ssh, restarts
# the container, and waits until /healthz reports the version just built.
#
#   VYSHKA_DEPLOY_HOST=staging scripts/deploy-branch-hub.sh [--reset]
#
# --reset starts from an empty database; the old one is kept beside it as
# data.<timestamp>. Uncommitted changes are deployed too, and the version
# says so.
#
# VYSHKA_DEPLOY_HOST  ssh destination (required)
# VYSHKA_DEPLOY_DIR   directory on the box (default /opt/vyshka-branch)
# VYSHKA_DEPLOY_ARCH  the box's GOARCH (default amd64)
# VYSHKA_BRANCH_PORT  the loopback port in the compose file (default 8090)
#
# The box needs Docker, sudo for the first run (the data directory and the
# admin token belong to the container's user, 65532), and `.env` in the
# directory naming the proxy's network (VYSHKA_PROXY_NETWORK=...).
set -euo pipefail

cd "$(dirname "$0")/.."

reset=0
for arg in "$@"; do
  case "$arg" in
    --reset) reset=1 ;;
    -h|--help) sed -n '2,22p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown argument $arg" >&2; exit 2 ;;
  esac
done

host="${VYSHKA_DEPLOY_HOST:?set VYSHKA_DEPLOY_HOST to the ssh destination}"
dir="${VYSHKA_DEPLOY_DIR:-/opt/vyshka-branch}"
arch="${VYSHKA_DEPLOY_ARCH:-amd64}"
port="${VYSHKA_BRANCH_PORT:-8090}"

branch="$(git rev-parse --abbrev-ref HEAD | tr -c 'A-Za-z0-9._\n-' '-')"
version="$branch-$(git rev-parse --short HEAD)"
git diff --quiet HEAD -- || version="$version-dirty"

work="$(mktemp -d "${TMPDIR:-/tmp}/vyshka-deploy.XXXXXX")"
trap 'rm -rf "$work"' EXIT

echo "== build $version for linux/$arch"
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath \
  -ldflags="-s -w -X github.com/That1Drifter/vyshka/hub.Version=$version" \
  -o "$work/vyshka-hub" ./hub/cmd/vyshka-hub

echo "== copy to $host:$dir"
ssh "$host" "mkdir -p '$dir/bin'"
scp -q "$work/vyshka-hub" "$host:$dir/bin/vyshka-hub.new"
scp -q deploy/branch-hub.compose.yml "$host:$dir/compose.yml"

echo "== restart"
ssh "$host" "DIR='$dir' RESET='$reset' PORT='$port' VERSION='$version' bash -s" <<'REMOTE'
set -euo pipefail
cd "$DIR"
[ -f .env ] || { echo "$DIR/.env is missing; it names the proxy network (VYSHKA_PROXY_NETWORK=...)" >&2; exit 2; }
grep -q '^VYSHKA_BRANCH_PORT=' .env || echo "VYSHKA_BRANCH_PORT=$PORT" >> .env
if [ ! -f admin-token ]; then
  printf 'vya_%s' "$(openssl rand -hex 24)" > admin-token.tmp
  sudo -n chown 65532:65532 admin-token.tmp
  sudo -n chmod 0400 admin-token.tmp
  sudo -n mv admin-token.tmp admin-token
  echo "new admin token in $DIR/admin-token (sudo cat it)"
fi
if [ "$RESET" = 1 ] && [ -d data ]; then
  docker compose down
  sudo -n mv data "data.$(date +%Y%m%d-%H%M%S)"
  echo "old database moved aside"
fi
if [ ! -d data ]; then
  mkdir data
  sudo -n chown 65532:65532 data
fi
chmod 0755 bin/vyshka-hub.new
mv bin/vyshka-hub.new bin/vyshka-hub
docker compose up -d --force-recreate --remove-orphans
for _ in $(seq 1 60); do
  if health="$(curl -fsS "http://127.0.0.1:$PORT/healthz" 2>/dev/null)"; then
    echo "$health"
    case "$health" in
      *"\"$VERSION\""*) exit 0 ;;
      *) echo "the hub answers with another version" >&2; exit 1 ;;
    esac
  fi
  sleep 1
done
echo "the hub did not answer /healthz within 60 s" >&2
docker compose logs --tail 40 hub >&2
exit 1
REMOTE
echo "== deployed $version"
