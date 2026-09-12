#!/usr/bin/env bash
# Stands up something for the panel to drive: a server record and a fake
# plugin, in bash, that publishes a heal manifest, a players snapshot, and a
# batch of events, and then executes whatever the panel dispatches. Run it
# beside a hub, open the panel, sign in, click through to a completed heal,
# and open the event feed.
#
#   VYSHKA_ADMIN_TOKEN=... scripts/demo-panel.sh [hub-url]
#
# The plugin runs until Ctrl-C. It is a demo stand-in, not a reference: it
# keeps no outbox across restarts and never renumbers across sessions. The
# conformance driver under conformance/plugin/driver is the correct one.
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

step "Setup: a server record, an enrolled demo plugin, and a session"
created=$(api POST /api/v1/servers "$ADMIN_TOKEN" '{"name":"Panel demo","game":"demo"}')
server_id=$(echo "$created" | jq -r '.server.id')
enrolled=$(api POST /plugin/v1/enroll "" \
  "$(jq -nc --arg t "$(echo "$created" | jq -r '.enrollment.token')" \
      '{enrollmentToken:$t,game:"demo",plugin:{name:"demo-plugin",version:"0.1.0"},transports:["poll"]}')")
session=$(api POST /plugin/v1/session "" \
  "$(jq -nc --arg id "$server_id" --arg secret "$(echo "$enrolled" | jq -r '.serverSecret')" \
      '{serverId:$id,serverSecret:$secret,pollTimeoutSeconds:5,plugin:{name:"demo-plugin",version:"0.1.0"},transports:["poll"]}')")
session_token=$(echo "$session" | jq -r '.sessionToken')
echo "server $server_id enrolled with a live session"

# The plugin's outbound state: the highest hub seq it has processed, the last
# seq it assigned, and every envelope of its own the hub has not acked yet.
in_ack=0
out_seq=0
pending='[]'

send() {
  out_seq=$((out_seq + 1))
  pending=$(jq -c --arg type "$1" --argjson body "$2" --argjson seq "$out_seq" --arg id "demo-$out_seq" \
    '. + [{v:1,id:$id,type:$type,seq:$seq,ts:(now|todate),body:$body}]' <<<"$pending")
}

send manifest.publish '{
  "manifestRevision": 1,
  "plugin": {"name": "demo-plugin", "version": "0.1.0"},
  "actions": [{
    "code": "example-mod.heal", "name": "Heal player",
    "context": "player", "namespace": "example-mod", "danger": "warning",
    "params": {
      "type": "object", "required": ["amount"],
      "properties": {
        "amount":       {"type": "integer", "minimum": 1, "maximum": 100, "default": 100},
        "restoreBlood": {"type": "boolean", "default": true},
        "reason":       {"type": "string", "enum": ["admin", "event", "test"], "default": "admin"},
        "position":     {"type": "array", "items": {"type": "number"}, "x-vyshka-widget": "vector"}
      }
    }
  }, {
    "code": "example-mod.broadcast", "name": "Broadcast a message",
    "context": "world", "namespace": "example-mod", "danger": "none",
    "params": {
      "type": "object", "required": ["message"],
      "properties": { "message": {"type": "string"} }
    }
  }]
}'
# Positions are DayZ-shaped ([x, y, z], y the elevation) so the map view
# plots them when a chernarusplus tileset is installed on the hub; without
# one they are listed as numbers.
send state.players '{
  "players": [
    {"player": {"platform": "steam", "id": "76561198000000001"}, "name": "Alice",
     "position": [6778.6, 6.2, 2321.8], "data": {"alive": true, "health": 100}},
    {"player": {"platform": "steam", "id": "76561198000000002"}, "name": "Bob",
     "position": [4827.0, 339.5, 9595.0], "data": {"alive": true, "health": 64}}
  ]
}'
# A few events for the feed view, stamped over the last few minutes. The
# start event names the world, which is how the map view picks its tileset.
send event.batch "$(jq -nc '
  def at(m): (now - m * 60 | todate);
  {events: [
    {t: "core.server.start", ts: at(6), data: {game: "demo", plugin: {name: "demo-plugin", version: "0.1.0"}, world: "chernarusplus"}},
    {t: "core.player.connect", ts: at(5), data: {player: {platform: "steam", id: "76561198000000001"}, name: "Alice"}},
    {t: "core.player.connect", ts: at(4), data: {player: {platform: "steam", id: "76561198000000002"}, name: "Bob"}},
    {t: "core.player.death", ts: at(2), data: {player: {platform: "steam", id: "76561198000000001"}, name: "Alice",
      cause: "player", killer: {platform: "steam", id: "76561198000000002"}, killerName: "Bob", weapon: "M4A1", distance: 312.5}},
    {t: "example-mod.raid.started", ts: at(1), data: {territoryId: "t-19", attackers: 4}}
  ]}')"

step "Open the panel and sign in"
echo "  $HUB_URL/"
echo "  sign in with the admin token you gave this script (it is not printed here)"
echo "Pick 'Panel demo', then 'Heal player'. Every dispatch is executed below. Ctrl-C to stop."
echo "'Live map' lists Alice and Bob; with a chernarusplus tileset installed (-maps-dir) it plots them."

trap 'printf "\nstopping\n"; exit 0' INT TERM

while :; do
  response=$(api POST /plugin/v1/poll "$session_token" \
    "$(jq -nc --argjson ack "$in_ack" --argjson envelopes "$pending" '{ack:$ack,envelopes:$envelopes}')") || {
    echo "poll failed; retrying" >&2
    sleep 1
    continue
  }
  hub_ack=$(jq -r '.ack // 0' <<<"$response")
  pending=$(jq -c --argjson ack "$hub_ack" '[.[] | select(.seq > $ack)]' <<<"$pending")

  while IFS= read -r envelope; do
    [ -n "$envelope" ] || continue
    seq=$(jq -r '.seq' <<<"$envelope")
    [ "$seq" -gt "$in_ack" ] && in_ack=$seq
    type=$(jq -r '.type' <<<"$envelope")
    [ "$type" = "action.dispatch" ] || continue
    action_id=$(jq -r '.body.actionId' <<<"$envelope")
    code=$(jq -r '.body.code' <<<"$envelope")
    echo "executing $code ($action_id): $(jq -c '.body | {referenceKey, params}' <<<"$envelope")"
    send action.ack "$(jq -nc --arg id "$action_id" '{actionId:$id}')"
    send action.result "$(jq -c --arg id "$action_id" \
      '{actionId:$id, ok:true, durationMs:12,
        result:{executed:.body.code, player:(.body.referenceKey // null), params:.body.params}}' <<<"$envelope")"
  done < <(jq -c '.envelopes[]?' <<<"$response")
done
