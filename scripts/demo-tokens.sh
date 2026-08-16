#!/usr/bin/env bash
# Walks scoped tokens and the audit log (spec/protocol.md section 10) with
# nothing but curl: two tokens, one narrowed to a single action code, the narrow
# one refused everywhere outside its grant, and the audit log showing what each
# of them did.
#
#   VYSHKA_ADMIN_TOKEN=... scripts/demo-tokens.sh [hub-url]
#
# The hub prints an ephemeral admin token at boot when none is configured, but
# only while it holds no minted token of its own. Once this demo has run, that
# hub authenticates with its scoped tokens unless -admin-token is set, so start
# it with -admin-token to keep a way in.
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
# to fail, which is most of what this demo has to show.
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

plugin_poll() {
  api POST /plugin/v1/poll "$session_token" "$1"
}

step "Setup: server, enrolled plugin, session, and a published heal manifest"
created=$(api POST /api/v1/servers "$ADMIN_TOKEN" '{"name":"Tokens demo","game":"dayz"}')
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
  "actions": [
    {"code": "example-mod.heal", "name": "Heal player", "context": "player",
     "namespace": "example-mod", "danger": "warning",
     "params": {"type": "object", "required": ["amount"],
                "properties": {"amount": {"type": "integer", "minimum": 1, "maximum": 100}}}},
    {"code": "example-mod.wipe", "name": "Wipe territory", "context": "world",
     "namespace": "example-mod", "danger": "destructive"}
  ]
}'
plugin_poll "$(jq -nc --argjson m "$manifest" \
  '{envelopes:[{v:1,id:"demo-manifest",type:"manifest.publish",seq:1,
     ts:"2026-08-16T18:00:00Z",body:$m}]}')" > /dev/null
plugin_poll "$(jq -nc '{ack:0,envelopes:[{v:1,id:"demo-telemetry",type:"event.batch",seq:2,
  ts:"2026-08-16T18:00:01Z",body:{events:[
    {t:"core.player.death",data:{weapon:"M4A1"}},
    {t:"example-mod.raid.started",data:{territoryId:"t-19"}}
  ]}}]}')" > /dev/null
echo "the manifest declares two actions, and the plugin has pushed one core and one custom event"

step "1. Two tokens, one narrowly scoped"
healer=$(api POST /api/v1/tokens "$ADMIN_TOKEN" \
  '{"name":"heal bot","scopes":["actions:dispatch:example-mod.heal"]}')
healer_secret=$(echo "$healer" | jq -r '.secret')
healer_id=$(echo "$healer" | jq -r '.token.id')
watcher=$(api POST /api/v1/tokens "$ADMIN_TOKEN" \
  '{"name":"raid watcher","scopes":["servers:read","events:read:example-mod.*"]}')
watcher_secret=$(echo "$watcher" | jq -r '.secret')
api GET /api/v1/tokens "$ADMIN_TOKEN" | jq -c '.tokens[] | {name, scopes}'
echo "(the secret came back once, in the mint response; the hub stores only a digest)"

step "2. A scope the hub does not define is refused at mint, not stored"
refused POST /api/v1/tokens "$ADMIN_TOKEN" '{"name":"typo","scopes":["event:read"]}'
echo "  a token minted from a typo would grant nothing, and its holder would find out later"

step "3. The narrow token dispatches the one code it holds"
dispatched=$(api POST "/api/v1/servers/$server_id/actions" "$healer_secret" \
  '{"code":"example-mod.heal","params":{"amount":50}}')
echo "$dispatched" | jq .
action_id=$(echo "$dispatched" | jq -r '.actionId')
echo "and can read back what it dispatched, because dispatch implies read:"
api GET "/api/v1/actions/$action_id" "$healer_secret" | jq -c '{id, code, state}'

step "4. Everything outside its grant is refused"
refused POST "/api/v1/servers/$server_id/actions" "$healer_secret" '{"code":"example-mod.wipe"}'
refused GET "/api/v1/servers/$server_id/events" "$healer_secret"
refused GET /api/v1/servers "$healer_secret"
refused GET /api/v1/audit "$healer_secret"
refused POST /api/v1/tokens "$healer_secret" '{"name":"escalation","scopes":["admin"]}'
echo "  note the wipe: refused before the manifest is even consulted, so the"
echo "  difference between forbidden and unknown_action cannot map a manifest"

step "5. A namespace-scoped read narrows the feed rather than gating it"
echo "the watcher asks for no filter at all and gets only what it may see:"
api GET "/api/v1/servers/$server_id/events" "$watcher_secret" | jq -c '[.events[].type]'
echo "the same query with the operator's own token sees both:"
api GET "/api/v1/servers/$server_id/events" "$ADMIN_TOKEN" | jq -c '[.events[].type]'
echo "an explicit filter reaching outside the grant is refused, never quietly narrowed:"
refused GET "/api/v1/servers/$server_id/events?type=core.*" "$watcher_secret"
refused GET "/api/v1/servers/$server_id/events?type=*" "$watcher_secret"

step "6. The audit log has every mutation, successful or refused"
api GET "/api/v1/audit?tokenId=$healer_id" "$ADMIN_TOKEN" \
  | jq -c '.records[] | {at, tokenName, method, path, status, detail}'
echo "(the 202 names the code and the action it created; the 403s are the attempts"
echo " to use the credential outside its grant, which is what this log is read for)"

step "7. Reads are not audited, so a panel's polling cannot bury the changes"
api GET "/api/v1/audit?tokenId=$healer_id" "$ADMIN_TOKEN" \
  | jq -c '{recorded: [.records[].method] | unique}'

step "8. Revocation takes effect on the next request; the record survives it"
api DELETE "/api/v1/tokens/$healer_id" "$ADMIN_TOKEN" > /dev/null
refused POST "/api/v1/servers/$server_id/actions" "$healer_secret" \
  '{"code":"example-mod.heal","params":{"amount":1}}'
api GET /api/v1/tokens "$ADMIN_TOKEN" | jq -c '.tokens[] | select(.id == "'"$healer_id"'") | {name, revokedAt}'
echo "the record outlives the credential, so the audit entries above keep resolving"

rm -f /tmp/vyshka-demo-body
printf '\n\033[1mdone\033[0m: least privilege enforced on every route, every mutation audited\n'
