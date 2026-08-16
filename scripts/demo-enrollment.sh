#!/usr/bin/env bash
# Walks the enrollment flow of spec/protocol.md section 5 with nothing but
# curl: operator creates a server, plugin enrolls, plugin opens a session, and
# both sides read back what happened.
#
#   VYSHKA_ADMIN_TOKEN=... scripts/demo-enrollment.sh [hub-url]
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

# Steps that are meant to fail print the hub's error body and swallow curl's
# own exit-status noise.
expect_failure() { "$@" 2>/dev/null || true; }

api() {
  local method="$1" path="$2" auth="${3:-}" body="${4:-}"
  local args=(--silent --show-error --fail-with-body -X "$method" "$HUB_URL$path")
  [ -n "$auth" ] && args+=(-H "Authorization: Bearer $auth")
  [ -n "$body" ] && args+=(-H 'Content-Type: application/json' -d "$body")
  curl "${args[@]}"
}

step "1. Operator creates the server record (Admin API)"
created=$(api POST /api/v1/servers "$ADMIN_TOKEN" \
  '{"name":"Demo server","game":"dayz"}')
echo "$created" | jq '{server: .server.id, state: .server.credentialState, expires: .enrollment.expiresAt}'

server_id=$(echo "$created" | jq -r '.server.id')
enrollment_token=$(echo "$created" | jq -r '.enrollment.token')
echo "enrollment token (goes in the plugin config, once): $enrollment_token"

step "2. Plugin exchanges that one-time token for credentials (Plugin API)"
enrolled=$(api POST /plugin/v1/enroll "" \
  "$(jq -nc --arg t "$enrollment_token" \
      '{enrollmentToken:$t,game:"dayz",plugin:{name:"demo-plugin",version:"0.1.0"},transports:["poll"]}')")
echo "$enrolled" | jq '{serverId, server: .server.name}'

server_secret=$(echo "$enrolled" | jq -r '.serverSecret')
echo "server secret (the plugin persists this): $server_secret"

step "3. The token is burned: enrolling again with it fails"
expect_failure api POST /plugin/v1/enroll "" \
  "$(jq -nc --arg t "$enrollment_token" '{enrollmentToken:$t,game:"dayz"}')"

step "4. Plugin opens a session"
session=$(api POST /plugin/v1/session "" \
  "$(jq -nc --arg id "$server_id" --arg secret "$server_secret" \
      '{serverId:$id,serverSecret:$secret,pollTimeoutSeconds:25,plugin:{name:"demo-plugin",version:"0.1.0"},transports:["poll"]}')")
echo "$session" | jq '{sessionId, expiresAt, protocolVersion, envelopeVersion, pollTimeoutSeconds, transports}'

session_token=$(echo "$session" | jq -r '.sessionToken')

step "5. The session token authenticates Plugin API calls"
api GET /plugin/v1/session "$session_token" | jq '{sessionId, pollTimeoutSeconds}'

step "6. The operator sees the enrolled server and its live session"
api GET "/api/v1/servers/$server_id" "$ADMIN_TOKEN" \
  | jq '{id, name, game, credentialState, lastSeenAt, plugin, session}'

step "7. Revoking the credentials kills the session immediately"
api DELETE "/api/v1/servers/$server_id/credentials" "$ADMIN_TOKEN"
expect_failure api GET /plugin/v1/session "$session_token"
expect_failure api POST /plugin/v1/session "" \
  "$(jq -nc --arg id "$server_id" --arg secret "$server_secret" \
      '{serverId:$id,serverSecret:$secret}')"

printf '\n\033[1mdone\033[0m: server %s is enrolled, revoked, and recoverable with a fresh enrollment token\n' "$server_id"
