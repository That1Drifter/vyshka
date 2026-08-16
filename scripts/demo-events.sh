#!/usr/bin/env bash
# Walks telemetry ingest and query (spec/protocol.md section 8) with nothing but
# curl: the plugin pushes a core event and a mod's own event on one channel, and
# both come back through the same filter.
#
#   VYSHKA_ADMIN_TOKEN=... scripts/demo-events.sh [hub-url]
#
# The hub prints an ephemeral admin token at boot when none is configured; copy
# that, or start the hub with -admin-token to keep it stable.
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

# Event timestamps are computed against the real clock rather than hardcoded.
# A hub substitutes its own receipt time for a `ts` implausibly far ahead of it
# (spec section 8.1), so a fixed timestamp in this file would quietly stop
# demonstrating anything the day after it was written.
ago() { date -u -d "-$1 seconds" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -v-"$1"S +%Y-%m-%dT%H:%M:%SZ; }

api() {
  local method="$1" path="$2" auth="${3:-}" body="${4:-}"
  local args=(--silent --show-error --fail-with-body -X "$method" "$HUB_URL$path")
  [ -n "$auth" ] && args+=(-H "Authorization: Bearer $auth")
  [ -n "$body" ] && args+=(-H 'Content-Type: application/json' -d "$body")
  curl "${args[@]}"
}

# One poll as the plugin. seq tracking is by hand: this demo sends exactly the
# envelopes it prints.
plugin_poll() {
  api POST /plugin/v1/poll "$session_token" "$1"
}

step "Setup: server, enrolled plugin, session"
created=$(api POST /api/v1/servers "$ADMIN_TOKEN" '{"name":"Telemetry demo","game":"dayz"}')
server_id=$(echo "$created" | jq -r '.server.id')
enrolled=$(api POST /plugin/v1/enroll "" \
  "$(jq -nc --arg t "$(echo "$created" | jq -r '.enrollment.token')" \
      '{enrollmentToken:$t,game:"dayz",plugin:{name:"demo-plugin",version:"0.1.0"},transports:["poll"]}')")
session=$(api POST /plugin/v1/session "" \
  "$(jq -nc --arg id "$server_id" --arg secret "$(echo "$enrolled" | jq -r '.serverSecret')" \
      '{serverId:$id,serverSecret:$secret,pollTimeoutSeconds:5,plugin:{name:"demo-plugin",version:"0.1.0"},transports:["poll"]}')")
session_token=$(echo "$session" | jq -r '.sessionToken')
echo "server $server_id has a live session; no manifest is needed to push telemetry"

step "1. One batch carries a core event and a mod's own event, on the same channel"
plugin_poll "$(jq -nc --arg death "$(ago 90)" --arg raid "$(ago 60)" --arg now "$(ago 0)" \
  '{envelopes:[{
    v:1,id:"demo-events-1",type:"event.batch",seq:1,ts:$now,
    body:{events:[
      {t:"core.player.death",ts:$death,
       data:{victim:{platform:"steam",id:"76561198000000000"},weapon:"M4A1",distance:312.5}},
      {t:"example-mod.raid.started",ts:$raid,
       data:{territoryId:"t-19",attackers:4}},
      {t:"core.player.chat",data:{message:"gg"}}
    ]}}]}')" | jq '{ack}'
echo "(example-mod.raid.started was never declared in a manifest: undeclared custom events are still stored)"

step "2. Both come back through one query, newest first"
echo "(occurredAt is what the game server said, receivedAt what the hub witnessed;"
echo " the chat event sent no ts at all, so the hub stood in its own receipt time)"
api GET "/api/v1/servers/$server_id/events" "$ADMIN_TOKEN" \
  | jq '.events[] | {type, occurredAt, receivedAt, data}'

step "3. The same filter syntax narrows to a namespace or to one type"
echo "type=core.player.* ->"
api GET "/api/v1/servers/$server_id/events?type=core.player.*" "$ADMIN_TOKEN" \
  | jq -c '[.events[].type]'
echo "type=example-mod.* ->"
api GET "/api/v1/servers/$server_id/events?type=example-mod.*" "$ADMIN_TOKEN" \
  | jq -c '[.events[].type]'
echo "both, ORed ->"
api GET "/api/v1/servers/$server_id/events?type=core.player.death&type=example-mod.raid.started" \
  "$ADMIN_TOKEN" | jq -c '[.events[].type]'

step "4. Pagination is an opaque cursor, absent on the last page"
page=$(api GET "/api/v1/servers/$server_id/events?limit=2" "$ADMIN_TOKEN")
echo "$page" | jq -c '{types: [.events[].type], nextCursor}'
cursor=$(echo "$page" | jq -r '.nextCursor')
api GET "/api/v1/servers/$server_id/events?limit=2&cursor=$cursor" "$ADMIN_TOKEN" \
  | jq -c '{types: [.events[].type], nextCursor}'

step "5. An event whose clock the hub cannot use is stored anyway, at receipt time"
plugin_poll "$(jq -nc --arg now "$(ago 0)" \
  '{ack:0,envelopes:[{
    v:1,id:"demo-events-2",type:"event.batch",seq:2,ts:$now,
    body:{events:[{t:"core.server.fps",ts:"not a timestamp",data:{fps:42}}]}}]}')" | jq '{ack}'
api GET "/api/v1/servers/$server_id/events?type=core.server.fps" "$ADMIN_TOKEN" \
  | jq '.events[0] | {type, occurredAt, receivedAt}'

step "6. A batch with a malformed event is refused whole, and the poll still succeeds"
plugin_poll "$(jq -nc --arg now "$(ago 0)" \
  '{ack:0,envelopes:[{
    v:1,id:"demo-events-3",type:"event.batch",seq:3,ts:$now,
    body:{events:[
      {t:"core.player.connect",data:{}},
      {t:"no-namespace",data:{}}
    ]}}]}')" | jq '{ack}'
echo "acked, because a rejected batch's durable effect is nothing. Nothing was stored:"
api GET "/api/v1/servers/$server_id/events?type=core.player.connect" "$ADMIN_TOKEN" \
  | jq -c '{stored: (.events | length)}'

step "7. The refusal is not silent: an event.reject names the envelope and the fault"
plugin_poll '{"ack":0}' \
  | jq '.envelopes[] | select(.type == "event.reject") | {type, seq, body}'

printf '\n\033[1mdone\033[0m: core and custom events shared one channel, one store, and one query\n'
