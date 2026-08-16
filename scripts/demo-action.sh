#!/usr/bin/env bash
# Walks the action lifecycle (spec/protocol.md section 7) with nothing but
# curl: the operator dispatches a heal, the plugin executes and reports, and
# every state is observable in between. Then the failure and expiry paths.
#
#   VYSHKA_ADMIN_TOKEN=... scripts/demo-action.sh [hub-url]
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

api() {
  local method="$1" path="$2" auth="${3:-}" body="${4:-}"
  local args=(--silent --show-error --fail-with-body -X "$method" "$HUB_URL$path")
  [ -n "$auth" ] && args+=(-H "Authorization: Bearer $auth")
  [ -n "$body" ] && args+=(-H 'Content-Type: application/json' -d "$body")
  curl "${args[@]}"
}
expect_failure() { "$@" 2>/dev/null || true; }

# One poll as the plugin. seq tracking is by hand: this demo sends exactly the
# envelopes it prints.
plugin_poll() {
  api POST /plugin/v1/poll "$session_token" "$1"
}

step "Setup: server, enrolled plugin, session, and a published heal manifest"
created=$(api POST /api/v1/servers "$ADMIN_TOKEN" '{"name":"Action demo","game":"dayz"}')
server_id=$(echo "$created" | jq -r '.server.id')
enrolled=$(api POST /plugin/v1/enroll "" \
  "$(jq -nc --arg t "$(echo "$created" | jq -r '.enrollment.token')" \
      '{enrollmentToken:$t,game:"dayz",plugin:{name:"demo-plugin",version:"0.1.0"},transports:["poll"]}')")
session=$(api POST /plugin/v1/session "" \
  "$(jq -nc --arg id "$server_id" --arg secret "$(echo "$enrolled" | jq -r '.serverSecret')" \
      '{serverId:$id,serverSecret:$secret,pollTimeoutSeconds:5,plugin:{name:"demo-plugin",version:"0.1.0"},transports:["poll"]}')")
session_token=$(echo "$session" | jq -r '.sessionToken')

manifest='{
  "manifestRevision": 1,
  "actions": [{
    "code": "example-mod.heal", "name": "Heal player",
    "context": "player", "namespace": "example-mod", "danger": "warning",
    "params": {
      "type": "object", "required": ["amount"],
      "properties": { "amount": {"type": "integer", "minimum": 1, "maximum": 100} }
    }
  }]
}'
plugin_poll "$(jq -nc --argjson m "$manifest" \
  '{envelopes:[{v:1,id:"demo-manifest",type:"manifest.publish",seq:1,
     ts:"2026-08-16T18:00:00Z",body:$m}]}')" | jq '{ack}'
echo "manifest published: the hub now knows what example-mod.heal accepts"

step "1. The operator dispatches a heal with one curl"
dispatched=$(api POST "/api/v1/servers/$server_id/actions" "$ADMIN_TOKEN" '{
  "code": "example-mod.heal", "context": "player",
  "referenceKey": "76561198000000000",
  "params": {"amount": 100},
  "idempotencyKey": "demo-heal-1"
}')
echo "$dispatched" | jq .
action_id=$(echo "$dispatched" | jq -r '.actionId')

step "2. Schema-invalid input never reaches the game server"
expect_failure api POST "/api/v1/servers/$server_id/actions" "$ADMIN_TOKEN" \
  '{"code": "example-mod.heal", "params": {"amount": 500}}'
echo ""
echo "(refused with params_invalid: 500 is above the declared maximum of 100)"

step "3. A retry with the same idempotencyKey returns the original actionId"
api POST "/api/v1/servers/$server_id/actions" "$ADMIN_TOKEN" '{
  "code": "example-mod.heal", "params": {"amount": 100}, "idempotencyKey": "demo-heal-1"
}' | jq .

step "4. The plugin polls and receives the action.dispatch"
delivery=$(plugin_poll '{"ack":0}')
echo "$delivery" | jq '.envelopes[] | select(.type == "action.dispatch") | {type, seq, body}'
dispatch_seq=$(echo "$delivery" | jq -r '[.envelopes[] | select(.type == "action.dispatch")][0].seq')

step "5. Acking the envelope is the delivery receipt: the state is now delivered"
plugin_poll "$(jq -nc --argjson ack "$dispatch_seq" '{ack:$ack}')" > /dev/null
api GET "/api/v1/actions/$action_id" "$ADMIN_TOKEN" | jq '{state, deliveredAt}'

step "6. action.ack moves it to running, action.result completes it"
plugin_poll "$(jq -nc --argjson ack "$dispatch_seq" --arg id "$action_id" \
  '{ack:$ack, envelopes:[
    {v:1,id:"demo-ack",type:"action.ack",seq:2,ts:"2026-08-16T18:00:01Z",
     body:{actionId:$id}},
    {v:1,id:"demo-result",type:"action.result",seq:3,ts:"2026-08-16T18:00:02Z",
     body:{actionId:$id, ok:true, result:{healedTo:100.0}, durationMs:12}}
  ]}')" | jq '{ack}'

step "7. The operator reads the full record back"
api GET "/api/v1/actions/$action_id" "$ADMIN_TOKEN" \
  | jq '{state, ok, result, durationMs, createdAt, deliveredAt, runningAt, finishedAt}'

step "8. An action nobody executes expires at its TTL (waiting ~2 s)"
expiring=$(api POST "/api/v1/servers/$server_id/actions" "$ADMIN_TOKEN" '{
  "code": "example-mod.heal", "params": {"amount": 1}, "ttlSeconds": 1
}')
expiring_id=$(echo "$expiring" | jq -r '.actionId')
sleep 2
api GET "/api/v1/actions/$expiring_id" "$ADMIN_TOKEN" | jq '{state, finishedAt}'
echo "a result arriving now would be acked and ignored: expiry is final"

printf '\n\033[1mdone\033[0m: action %s round-tripped queued -> delivered -> running -> completed\n' "$action_id"
