#!/usr/bin/env bash
# Walks manifest publish and validation (spec/protocol.md section 6) with
# nothing but curl: a plugin publishes a heal action, the operator reads it
# back, a runtime republish replaces it, a stale revision is ignored, and a
# schema outside the subset earns a manifest.reject instead of a dead session.
#
#   VYSHKA_ADMIN_TOKEN=... scripts/demo-manifest.sh [hub-url]
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

# publish frames one manifest.publish envelope at the given plugin seq and
# sends it on a poll. Acking as we go keeps the demo's sequence state honest.
publish() {
  local seq="$1" ack="$2" manifest="$3"
  api POST /plugin/v1/poll "$session_token" "$(jq -nc \
    --argjson seq "$seq" --argjson ack "$ack" --argjson m "$manifest" \
    '{ack:$ack, envelopes:[{v:1, id:("demo-manifest-" + ($seq|tostring)),
       type:"manifest.publish", seq:$seq, ts:"2026-08-16T18:00:00Z", body:$m}]}')"
}

step "Setup: a server record, an enrolled plugin, and a session"
created=$(api POST /api/v1/servers "$ADMIN_TOKEN" '{"name":"Manifest demo","game":"dayz"}')
server_id=$(echo "$created" | jq -r '.server.id')
enrolled=$(api POST /plugin/v1/enroll "" \
  "$(jq -nc --arg t "$(echo "$created" | jq -r '.enrollment.token')" \
      '{enrollmentToken:$t,game:"dayz",plugin:{name:"demo-plugin",version:"0.1.0"},transports:["poll"]}')")
session=$(api POST /plugin/v1/session "" \
  "$(jq -nc --arg id "$server_id" --arg secret "$(echo "$enrolled" | jq -r '.serverSecret')" \
      '{serverId:$id,serverSecret:$secret,pollTimeoutSeconds:5,plugin:{name:"demo-plugin",version:"0.1.0"},transports:["poll"]}')")
session_token=$(echo "$session" | jq -r '.sessionToken')
echo "session $(echo "$session" | jq -r '.sessionId') for server $server_id"

step "1. The plugin publishes revision 1: one heal action with a params schema"
heal='{
  "game": "dayz",
  "plugin": {"name": "demo-plugin", "version": "0.1.0"},
  "manifestRevision": 1,
  "actions": [{
    "code": "example-mod.heal", "name": "Heal player",
    "context": "player", "namespace": "example-mod", "danger": "warning",
    "params": {
      "type": "object", "required": ["amount"],
      "properties": {
        "amount": {"type": "integer", "minimum": 1, "maximum": 100},
        "item":   {"type": "string", "x-vyshka-widget": "itemlist"}
      }
    }
  }]
}'
publish 1 0 "$heal" | jq '{ack}'
echo "acked: the manifest is stored"

step "2. The operator lists it with one curl"
api GET "/api/v1/servers/$server_id/manifest" "$ADMIN_TOKEN" \
  | jq '{revision, publishedAt, actions: [.manifest.actions[] | {code, name, danger}]}'

step "3. A runtime republish at revision 2 replaces it: no reconnect, one message"
revised=$(echo "$heal" | jq '.manifestRevision = 2 | .actions[0].name = "Heal player (fully)"')
publish 2 0 "$revised" | jq '{ack}'
api GET "/api/v1/servers/$server_id/manifest" "$ADMIN_TOKEN" \
  | jq '{revision, action: .manifest.actions[0].name}'

step "4. A stale revision 1 arrives late and is ignored, though still acked"
publish 3 0 "$heal" | jq '{ack}'
api GET "/api/v1/servers/$server_id/manifest" "$ADMIN_TOKEN" | jq '{revision}'
echo "still revision 2: lower and equal revisions never replace the manifest"

step "5. Revision 3 smuggles in a 'pattern' keyword, outside the schema subset"
invalid=$(echo "$heal" | jq '.manifestRevision = 3
  | .actions[0].params.properties.item = {"type": "string", "pattern": "^med"}')
publish 4 0 "$invalid" | jq '{ack,
  reject: [.envelopes[] | select(.type == "manifest.reject")
           | {type, body: {envelopeId: .body.envelopeId, manifestRevision: .body.manifestRevision,
              errors: .body.errors}}][0]}'
echo "the poll succeeded and the envelope was acked; the manifest.reject names the keyword"

step "6. The stored manifest is untouched, and the session is still alive"
api GET "/api/v1/servers/$server_id/manifest" "$ADMIN_TOKEN" | jq '{revision}'
api GET /plugin/v1/session "$session_token" | jq '{sessionId}'

step "7. The corrected manifest lands at the very revision that was rejected"
corrected=$(echo "$heal" | jq '.manifestRevision = 3')
publish 5 0 "$corrected" | jq '{ack}'
api GET "/api/v1/servers/$server_id/manifest" "$ADMIN_TOKEN" | jq '{revision}'

printf '\n\033[1mdone\033[0m: server %s published, republished, and survived a rejection\n' "$server_id"
