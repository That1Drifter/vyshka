#!/usr/bin/env bash
# Walks state snapshots (spec/protocol.md section 8.3) with nothing but curl:
# a plugin pushes a full player snapshot, the Admin API answers with the live
# list, a newer (empty) snapshot replaces it whole, and history reads newest
# first.
#
#   VYSHKA_ADMIN_TOKEN=... scripts/demo-state.sh [hub-url]
set -euo pipefail

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

step() { printf '\n\033[1m%s\033[0m\n' "$*"; }

api() {
  local method="$1" path="$2" auth="${3:-}" body="${4:-}"
  local args=(--silent --show-error --fail-with-body -X "$method" "$HUB_URL$path")
  [ -n "$auth" ] && args+=(-H "Authorization: Bearer $auth")
  [ -n "$body" ] && args+=(-H 'Content-Type: application/json' -d "$body")
  curl "${args[@]}"
}

refused() {
  local method="$1" path="$2" auth="${3:-}" body="${4:-}"
  local args=(--silent --show-error -o /tmp/vyshka-demo-body -w '%{http_code}'
              -X "$method" "$HUB_URL$path")
  [ -n "$auth" ] && args+=(-H "Authorization: Bearer $auth")
  [ -n "$body" ] && args+=(-H 'Content-Type: application/json' -d "$body")
  local status
  status=$(curl "${args[@]}")
  printf '  %-58s -> %s %s\n' "$method $path" "$status" \
    "$(jq -r '.error.code // "no error code"' < /tmp/vyshka-demo-body)"
}

step "Setup: server, enrolled plugin, session"
created=$(api POST /api/v1/servers "$ADMIN_TOKEN" '{"name":"State demo","game":"dayz"}')
server_id=$(echo "$created" | jq -r '.server.id')
enrolled=$(api POST /plugin/v1/enroll "" \
  "$(jq -nc --arg t "$(echo "$created" | jq -r '.enrollment.token')" \
      '{enrollmentToken:$t,game:"dayz",plugin:{name:"demo-plugin",version:"0.1.0"},transports:["poll"]}')")
session=$(api POST /plugin/v1/session "" \
  "$(jq -nc --arg id "$server_id" --arg secret "$(echo "$enrolled" | jq -r '.serverSecret')" \
      '{serverId:$id,serverSecret:$secret,pollTimeoutSeconds:5,plugin:{name:"demo-plugin",version:"0.1.0"},transports:["poll"]}')")
session_token=$(echo "$session" | jq -r '.sessionToken')

step "1. Nothing yet: a real server with no snapshot answers not_found"
refused GET "/api/v1/servers/$server_id/state/players" "$ADMIN_TOKEN"

step "2. The plugin pushes a full player snapshot on its poll"
now=$(date -u +%Y-%m-%dT%H:%M:%SZ)
api POST /plugin/v1/poll "$session_token" "$(jq -nc --arg now "$now" \
  '{envelopes:[{v:1,id:"demo-state-1",type:"state.players",seq:1,
     ts:$now,
     body:{capturedAt:$now,players:[
       {player:{platform:"steam",id:"76561198000000001"},name:"Alice",
        position:[4231.5,300.2,10620.0],data:{health:82}},
       {player:{platform:"steam",id:"76561198000000002"},name:"Bob",
        position:[5100.0,290.1,9800.5]}
     ]}}]}')" > /dev/null
echo "pushed a snapshot of two players"

step "3. The Admin API answers with the live list"
api GET "/api/v1/servers/$server_id/state/players" "$ADMIN_TOKEN" \
  | jq '{capturedAt, players: [.snapshot.players[] | {id: .player.id, name, position}]}'

step "4. A newer snapshot replaces the older whole: everyone logged off"
api POST /plugin/v1/poll "$session_token" "$(jq -nc \
  --arg now "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  '{envelopes:[{v:1,id:"demo-state-2",type:"state.players",seq:2,
     ts:$now,body:{players:[]}}]}')" > /dev/null
api GET "/api/v1/servers/$server_id/state/players" "$ADMIN_TOKEN" \
  | jq -c '{capturedAt, players: .snapshot.players}'
echo "an empty list is a real answer (nobody online), not a missing one"

step "5. History reads newest first"
api GET "/api/v1/servers/$server_id/state/players/history" "$ADMIN_TOKEN" \
  | jq -c '[.snapshots[] | {capturedAt, count: (.snapshot.players | length)}]'

step "6. The state family cannot be forged through the raw queue"
refused POST "/api/v1/servers/$server_id/envelopes" "$ADMIN_TOKEN" '{"type":"state.reject"}'

rm -f /tmp/vyshka-demo-body
printf '\n\033[1mdone\033[0m: the live map has its feed, and stale state says how stale it is\n'
