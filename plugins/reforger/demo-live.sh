#!/usr/bin/env bash
# One action round trip through a live Arma Reforger dedicated server, the
# definition of done of issue #15: the operator creates a server record, the
# plugin on a real server enrolls and publishes its manifest, the operator
# dispatches vyshka.info with one curl, and the result comes back from the
# game.
#
#   VYSHKA_ADMIN_TOKEN=... plugins/reforger/demo-live.sh <hub-url> <server install dir>
#
# It needs a Reforger dedicated server install and Windows, so scripts/e2e.sh
# does not run it (it lives here, not under scripts/demo-*). The server is
# started by `vyshka-reforger harness`, which writes the plugin's config and
# stops the server when this script closes its stdin. The server record's
# credentials are revoked at the end.
set -euo pipefail

HUB_URL="${1:?hub url}"
SERVER_DIR="${2:?Reforger dedicated server install directory}"
ADMIN_TOKEN="${VYSHKA_ADMIN_TOKEN:?set VYSHKA_ADMIN_TOKEN to the admin token of the hub}"

cd "$(dirname "$0")/../.."

step() { printf '\n\033[1m%s\033[0m\n' "$*"; }
api() {
  local method="$1" path="$2" body="${3:-}"
  local args=(--silent --show-error --fail-with-body -X "$method" "$HUB_URL$path" -H "Authorization: Bearer $ADMIN_TOKEN")
  [ -n "$body" ] && args+=(-H 'Content-Type: application/json' -d "$body")
  curl "${args[@]}"
}

step "1. The operator creates a server record for a Reforger server"
created=$(api POST /api/v1/servers '{"name":"Reforger demo","game":"reforger"}')
server_id=$(echo "$created" | jq -r '.server.id')
token=$(echo "$created" | jq -r '.enrollment.token')
echo "$created" | jq '{server: .server | {id, name, game}, enrollment: {expiresAt: .enrollment.expiresAt}}'

step "2. A Reforger dedicated server starts with the plugin and that token"
go build -o "${TMPDIR:-/tmp}/vyshka-reforger-demo.exe" ./plugins/reforger/cmd/vyshka-reforger
# The harness reads its stdin until it closes; fd 3 is that stdin.
exec 3> >("${TMPDIR:-/tmp}/vyshka-reforger-demo.exe" harness -server "$SERVER_DIR" -url "$HUB_URL" -token "$token" -profile plugins/reforger/build/demo-profile 2>&1 | grep -E 'vyshka-reforger:|\[Vyshka\]' | sed 's/^/    /')
cleanup() {
  exec 3>&- || true
  api DELETE "/api/v1/servers/$server_id/credentials" > /dev/null || true
}
trap cleanup EXIT

started=$(date +%s)
until api GET "/api/v1/servers/$server_id/manifest" 2>/dev/null | jq -e '.manifest.actions | length > 0' > /dev/null 2>&1; do
  if [ $(( $(date +%s) - started )) -gt 240 ]; then
    echo "the plugin did not publish a manifest within 240 s" >&2
    exit 1
  fi
  sleep 3
done
echo "manifest published $(( $(date +%s) - started )) s after the server started"
api GET "/api/v1/servers/$server_id" | jq '{name, game, linkState, plugin}'
api GET "/api/v1/servers/$server_id/manifest" | jq '{revision, actions: [.manifest.actions[] | {code, name, context}]}'

step "3. The operator dispatches vyshka.info with one curl"
dispatched=$(api POST "/api/v1/servers/$server_id/actions" '{"code":"vyshka.info","context":"world","params":{}}')
echo "$dispatched" | jq .
action_id=$(echo "$dispatched" | jq -r '.actionId')

step "4. The result comes back from the game"
dispatched_at=$(date +%s%N)
until api GET "/api/v1/actions/$action_id" | jq -e '.state == "completed" or .state == "failed" or .state == "expired"' > /dev/null; do
  sleep 0.5
done
elapsed_ms=$(( ($(date +%s%N) - dispatched_at) / 1000000 ))
api GET "/api/v1/actions/$action_id" | jq '{state, ok, result, durationMs, createdAt, deliveredAt, runningAt, finishedAt}'
echo "answered about $elapsed_ms ms after the dispatch"

step "5. The server stops and its credentials are revoked"
