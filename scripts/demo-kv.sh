#!/usr/bin/env bash
# Walks the key/value store (spec/protocol.md section 12) with nothing but
# curl: a plugin writes a key through its own realm, an admin bot's stale
# compare-and-swap is rejected with the current revision, a fresh one wins,
# counters bump atomically, and namespace confinement holds on both realms.
#
#   VYSHKA_ADMIN_TOKEN=... scripts/demo-kv.sh [hub-url]
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

# refused prints the status and the protocol error code of a call that is meant
# to fail.
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

step "Setup: server, enrolled plugin, session, and a manifest declaring one KV namespace"
created=$(api POST /api/v1/servers "$ADMIN_TOKEN" '{"name":"KV demo","game":"dayz"}')
server_id=$(echo "$created" | jq -r '.server.id')
enrolled=$(api POST /plugin/v1/enroll "" \
  "$(jq -nc --arg t "$(echo "$created" | jq -r '.enrollment.token')" \
      '{enrollmentToken:$t,game:"dayz",plugin:{name:"demo-plugin",version:"0.1.0"},transports:["poll"]}')")
session=$(api POST /plugin/v1/session "" \
  "$(jq -nc --arg id "$server_id" --arg secret "$(echo "$enrolled" | jq -r '.serverSecret')" \
      '{serverId:$id,serverSecret:$secret,pollTimeoutSeconds:5,plugin:{name:"demo-plugin",version:"0.1.0"},transports:["poll"]}')")
session_token=$(echo "$session" | jq -r '.sessionToken')

api POST /plugin/v1/poll "$session_token" "$(jq -nc \
  '{envelopes:[{v:1,id:"demo-kv-manifest",type:"manifest.publish",seq:1,
     ts:"2026-08-25T18:00:00Z",
     body:{manifestRevision:1,actions:[],kvNamespaces:["example-mod"]}}]}')" > /dev/null
echo "the manifest's kvNamespaces is the sole source of the plugin's KV access"

step "1. The plugin writes a key through its own realm"
api PUT /plugin/v1/kv/example-mod/territory.t-19 "$session_token" \
  '{"value":{"owner":"clan-a","flags":3}}' | jq -c .
echo "and the same key reads back over the Admin API, revision and all:"
api GET /api/v1/kv/example-mod/territory.t-19 "$ADMIN_TOKEN" | jq -c .

step "2. Both writers move; the stale compare-and-swap loses"
echo "the plugin writes again (revision 2):"
api PUT /plugin/v1/kv/example-mod/territory.t-19 "$session_token" \
  '{"value":{"owner":"clan-b","flags":3}}' | jq -c .
echo "an admin bot still holding revision 1 tries a CAS and is told the current revision:"
refused PUT /api/v1/kv/example-mod/territory.t-19 "$ADMIN_TOKEN" \
  '{"value":{"owner":"bot"},"ifRevision":1}'
jq -c '.error.details' < /tmp/vyshka-demo-body
echo "re-armed with the revision the rejection reported, the fresh CAS wins:"
api PUT /api/v1/kv/example-mod/territory.t-19 "$ADMIN_TOKEN" \
  '{"value":{"owner":"bot"},"ifRevision":2}' | jq -c .

step "3. Counters bump atomically; decrement is a negative delta"
api POST /api/v1/kv/example-mod/raids.count/incr "$ADMIN_TOKEN" '{"delta":5}' | jq -c .
api POST /plugin/v1/kv/example-mod/raids.count/incr "$session_token" '' | jq -c .
api POST /api/v1/kv/example-mod/raids.count/incr "$ADMIN_TOKEN" '{"delta":-2}' | jq -c .
echo "(an empty body means delta 1; concurrent incrs each land exactly once)"

step "4. A TTL key expires into absence"
api PUT /api/v1/kv/example-mod/vote.session "$ADMIN_TOKEN" \
  '{"value":"open","ttlSeconds":1}' | jq -c .
sleep 2
refused GET /api/v1/kv/example-mod/vote.session "$ADMIN_TOKEN"
echo "  from the moment expiry passes, the key reads as absent everywhere"

step "5. Confinement holds on both realms"
echo "the plugin is confined to what its manifest declares:"
refused PUT /plugin/v1/kv/other-mod/anything "$session_token" '{"value":1}'
echo "an admin token scoped to another namespace is confined by its grant:"
scoped=$(api POST /api/v1/tokens "$ADMIN_TOKEN" \
  '{"name":"kv bot","scopes":["kv:rw:other-mod"]}')
scoped_secret=$(echo "$scoped" | jq -r '.secret')
refused GET /api/v1/kv/example-mod/territory.t-19 "$scoped_secret"
echo "while its own namespace answers:"
api PUT /api/v1/kv/other-mod/config "$scoped_secret" '{"value":true}' | jq -c .

rm -f /tmp/vyshka-demo-body
printf '\n\033[1mdone\033[0m: shared state with revisions doing the arbitration, no mod database required\n'
