#!/usr/bin/env bash
# Walks the backup endpoint (issue #83) with curl: a hub with real data takes a
# snapshot while it serves, a second hub boots on that snapshot and has the
# data back with the plugin able to resume, the directory rotates down to its
# bound, and a caller cannot choose where a snapshot goes.
#
#   VYSHKA_ADMIN_TOKEN=... scripts/demo-backup.sh [hub-url]
#
# The restore step needs the snapshot on this machine (the hub is local, as
# under scripts/e2e.sh) and Go to build the second hub; it is skipped otherwise.
set -euo pipefail

cd "$(dirname "$0")/.."

HUB_URL="${1:-${VYSHKA_HUB_URL:-http://127.0.0.1:8080}}"
ADMIN_TOKEN="${VYSHKA_ADMIN_TOKEN:-}"

if ! command -v jq >/dev/null 2>&1; then
  echo "this demo needs jq to read the JSON responses" >&2
  exit 2
fi
if [ -z "$ADMIN_TOKEN" ]; then
  echo "set VYSHKA_ADMIN_TOKEN to the hub's admin token" >&2
  exit 2
fi

work="$(mktemp -d "${TMPDIR:-/tmp}/vyshka-demo-backup.XXXXXX")"
restored_pid=""
cleanup() {
  if [ -n "$restored_pid" ]; then
    kill "$restored_pid" 2>/dev/null || true
    wait "$restored_pid" 2>/dev/null || true
  fi
  rm -rf "$work"
}
trap cleanup EXIT

step() { printf '\n\033[1m%s\033[0m\n' "$*"; }

api_at() {
  local base="$1" method="$2" path="$3" auth="${4:-}" body="${5:-}"
  local args=(--silent --show-error --fail-with-body -X "$method" "$base$path")
  [ -n "$auth" ] && args+=(-H "Authorization: Bearer $auth")
  [ -n "$body" ] && args+=(-H 'Content-Type: application/json' -d "$body")
  curl "${args[@]}"
}
api() { api_at "$HUB_URL" "$@"; }

refused() {
  local method="$1" path="$2" auth="${3:-}" body="${4:-}"
  local args=(--silent --show-error -o "$work/body" -w '%{http_code}' -X "$method" "$HUB_URL$path")
  [ -n "$auth" ] && args+=(-H "Authorization: Bearer $auth")
  [ -n "$body" ] && args+=(-H 'Content-Type: application/json' -d "$body")
  local status
  status=$(curl "${args[@]}")
  printf '  %-40s -> %s %s\n' "$method $path" "$status" "$(jq -r '.error.code // "no error code"' < "$work/body")"
}

step "Setup: a server with a session, events, a KV key, a ban, and a webhook"
created=$(api POST /api/v1/servers "$ADMIN_TOKEN" '{"name":"Backup demo","game":"dayz"}')
server_id=$(echo "$created" | jq -r '.server.id')
enrolled=$(api POST /plugin/v1/enroll "" \
  "$(jq -nc --arg t "$(echo "$created" | jq -r '.enrollment.token')" \
      '{enrollmentToken:$t,game:"dayz",plugin:{name:"demo-plugin",version:"0.1.0"},transports:["poll"]}')")
server_secret=$(echo "$enrolled" | jq -r '.serverSecret')
open_session() {
  api_at "$1" POST /plugin/v1/session "" \
    "$(jq -nc --arg id "$server_id" --arg secret "$server_secret" \
        '{serverId:$id,serverSecret:$secret,pollTimeoutSeconds:1,capabilities:["bans"],
          plugin:{name:"demo-plugin",version:"0.1.0"},transports:["poll"]}')"
}
session_token=$(open_session "$HUB_URL" | jq -r '.sessionToken')
now=$(date -u +%Y-%m-%dT%H:%M:%SZ)
api POST /plugin/v1/poll "$session_token" "$(jq -nc --arg now "$now" \
  '{envelopes:[
     {v:1,id:"demo-backup-manifest",type:"manifest.publish",seq:1,ts:$now,
      body:{manifestRevision:1,actions:[],kvNamespaces:["backup-demo"]}},
     {v:1,id:"demo-backup-events",type:"event.batch",seq:2,ts:$now,
      body:{events:[{t:"core.player.chat",data:{message:"before the backup"}},
                    {t:"backup-demo.raid.started",data:{territoryId:"t-7"}}]}}]}')" > /dev/null
api PUT /plugin/v1/kv/backup-demo/territory.t-7 "$session_token" '{"value":{"owner":"clan-a"}}' > /dev/null
player="backup-demo-$(date +%s%N | tail -c 9)"
api POST /api/v1/bans "$ADMIN_TOKEN" \
  "$(jq -nc --arg id "$player" '{player:{platform:"steam",id:$id},reason:"before the backup"}')" > /dev/null
webhook_id=$(api POST /api/v1/webhooks "$ADMIN_TOKEN" \
  "$(jq -nc --arg s "$server_id" '{url:"http://127.0.0.1:9/backup-demo",events:["core.*"],serverIds:[$s]}')" \
  | jq -r '.webhook.id')
revision_before=$(api GET "/plugin/v1/bans?limit=100" "$session_token" | jq '.revision')
echo "server $server_id, 2 events, kv backup-demo/territory.t-7, a ban (list revision $revision_before), webhook $webhook_id"

step "1. Take a snapshot while the hub serves; the caller sends no path, the hub names the file"
taken=$(api POST /api/v1/admin/backup "$ADMIN_TOKEN")
echo "$taken" | jq '{backup: (.backup | {name, sizeBytes, takenAt, durationMs}), kept: (.kept | length), pruned}'
snapshot=$(echo "$taken" | jq -r '.backup.path')
first_name=$(echo "$taken" | jq -r '.backup.name')
echo "the audit log records it like any admin write:"
api GET "/api/v1/audit?limit=1" "$ADMIN_TOKEN" | jq -c '.records[0] | {method, path, status, detail}'

step "2. The hub moves on after the snapshot: a new key and a second ban"
api PUT /api/v1/kv/backup-demo/after-backup "$ADMIN_TOKEN" '{"value":true}' | jq -c .
api POST /api/v1/bans "$ADMIN_TOKEN" \
  "$(jq -nc --arg id "$player-2" '{player:{platform:"steam",id:$id},reason:"after the backup"}')" | jq -c '{revision}'
revision_live=$(api GET "/plugin/v1/bans?limit=100" "$session_token" | jq '.revision')
echo "the live hub's ban list is now at revision $revision_live"

step "3. Restore: a second hub boots on a copy of the snapshot"
if [ ! -f "$snapshot" ] || ! command -v go >/dev/null 2>&1; then
  echo "skipped: the snapshot is not on this machine ($snapshot) or Go is missing"
else
  port=$((19000 + RANDOM % 1000))
  restored_url="http://127.0.0.1:$port"
  bin="$work/vyshka-hub-restore-$port"
  case "$(go env GOOS)" in windows) bin="$bin.exe" ;; esac
  go build -o "$bin" ./hub/cmd/vyshka-hub
  cp "$snapshot" "$work/vyshka.db"
  "$bin" serve -addr "127.0.0.1:$port" -db "$work/vyshka.db" -panel=false > "$work/restored.log" 2>&1 &
  restored_pid=$!
  for _ in $(seq 1 100); do
    curl --silent --fail "$restored_url/healthz" > /dev/null 2>&1 && break
    kill -0 "$restored_pid" 2>/dev/null || { cat "$work/restored.log"; echo "the restored hub exited during boot" >&2; exit 1; }
    sleep 0.1
  done
  echo "restored hub up at $restored_url"
  echo "the server and its events:"
  api_at "$restored_url" GET "/api/v1/servers/$server_id" "$ADMIN_TOKEN" | jq -c '{id, name}'
  api_at "$restored_url" GET "/api/v1/servers/$server_id/events" "$ADMIN_TOKEN" | jq -c '[.events[].type]'
  echo "the key written before the snapshot is there, the one written after is not:"
  api_at "$restored_url" GET /api/v1/kv/backup-demo/territory.t-7 "$ADMIN_TOKEN" | jq -c '{key, value, revision}'
  status=$(curl --silent -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $ADMIN_TOKEN" \
    "$restored_url/api/v1/kv/backup-demo/after-backup")
  echo "  GET /api/v1/kv/backup-demo/after-backup -> $status"
  echo "the webhook:"
  api_at "$restored_url" GET /api/v1/webhooks "$ADMIN_TOKEN" | jq -c "[.webhooks[] | select(.id == \"$webhook_id\") | {id, url}]"
  echo "the plugin resumes. Its old session was closed in the snapshot, because that session's"
  echo "sequence state moved on after the snapshot; reusing it would leave a gap neither side can close:"
  old_status=$(curl --silent -o "$work/body" -w '%{http_code}' -H "Authorization: Bearer $session_token" \
    "$restored_url/plugin/v1/session")
  echo "  GET /plugin/v1/session (old token) -> $old_status $(jq -r '.error.code' < "$work/body")"
  [ "$old_status" = 401 ]
  echo "so it opens a new one with its stored secret, and both sides renumber what is unacked (spec 9.1):"
  resumed=$(open_session "$restored_url" | jq -r '.sessionToken')
  api_at "$restored_url" POST /plugin/v1/poll "$resumed" '{"envelopes":[]}' | jq -c '{ack, envelopes: [.envelopes[]?.type]}'
  revision_restored=$(api_at "$restored_url" GET "/plugin/v1/bans?limit=100" "$resumed" | jq '.revision')
  echo "the ban list went back with the data: revision $revision_restored on the restored hub, $revision_live on the live one"
  echo "(a plugin walks whenever the revision differs, not only when it rises: spec/protocol.md 13.4)"
  next=$(api_at "$restored_url" POST /api/v1/bans "$ADMIN_TOKEN" \
    "$(jq -nc --arg id "$player-3" '{player:{platform:"steam",id:$id},reason:"after the restore"}')" | jq '.revision')
  echo "the restored hub's next change mints revision $next, above every revision the live hub handed out ($revision_live): $([ "$next" -gt "$revision_live" ] && echo yes || echo NO)"
  [ "$next" -gt "$revision_live" ]
fi

step "4. The directory is bounded: older snapshots rotate out"
for _ in 1 2 3 4 5 6 7; do
  last=$(api POST /api/v1/admin/backup "$ADMIN_TOKEN")
done
echo "$last" | jq -c '{kept: (.kept | length), pruned}'
echo "the first snapshot ($first_name) is gone: $(echo "$last" | jq --arg n "$first_name" '[.kept[] | select(. == $n)] | length == 0')"

step "5. Refusals"
reader=$(api POST /api/v1/tokens "$ADMIN_TOKEN" '{"name":"backup demo reader","scopes":["servers:read"]}' | jq -r '.secret')
echo "a token without admin:"
refused POST /api/v1/admin/backup "$reader"
echo "a body naming a path is not read; the hub still chooses the name and directory:"
api POST /api/v1/admin/backup "$ADMIN_TOKEN" '{"path":"/etc/cron.d/x","name":"../../evil.db"}' \
  | jq -c '{name: .backup.name}'
echo "and the route takes only POST:"
refused GET /api/v1/admin/backup "$ADMIN_TOKEN"

printf '\n\033[1mdone\033[0m: a consistent snapshot of a running hub, restorable as one file, in a bounded directory\n'
