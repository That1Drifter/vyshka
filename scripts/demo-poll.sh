#!/usr/bin/env bash
# Walks the envelope exchange of spec/protocol.md sections 3.1.2, 4 and 9 with
# nothing but curl: a plugin holds a long-poll, an operator queues an envelope
# mid-hold, the poll flushes at once, and the ack retires the work.
#
#   VYSHKA_ADMIN_TOKEN=... scripts/demo-poll.sh [hub-url]
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
expect_failure() { "$@" 2>/dev/null || true; }

api() {
  local method="$1" path="$2" auth="${3:-}" body="${4:-}"
  local args=(--silent --show-error --fail-with-body -X "$method" "$HUB_URL$path")
  [ -n "$auth" ] && args+=(-H "Authorization: Bearer $auth")
  [ -n "$body" ] && args+=(-H 'Content-Type: application/json' -d "$body")
  curl "${args[@]}"
}

poll() {
  # A plugin must give its own client a response timeout above the negotiated
  # pollTimeout, or it aborts the request the hub is about to answer.
  local token="$1" body="$2"
  curl --silent --show-error --fail-with-body --max-time 40 \
    -X POST "$HUB_URL/plugin/v1/poll" \
    -H "Authorization: Bearer $token" \
    -H 'Content-Type: application/json' -d "$body"
}

step "Setup: a server record, an enrolled plugin, and a session holding 25 s polls"
created=$(api POST /api/v1/servers "$ADMIN_TOKEN" '{"name":"Poll demo","game":"dayz"}')
server_id=$(echo "$created" | jq -r '.server.id')
enrolled=$(api POST /plugin/v1/enroll "" \
  "$(jq -nc --arg t "$(echo "$created" | jq -r '.enrollment.token')" \
      '{enrollmentToken:$t,game:"dayz",plugin:{name:"demo-plugin",version:"0.1.0"},transports:["poll"]}')")
session=$(api POST /plugin/v1/session "" \
  "$(jq -nc --arg id "$server_id" --arg secret "$(echo "$enrolled" | jq -r '.serverSecret')" \
      '{serverId:$id,serverSecret:$secret,pollTimeoutSeconds:25,plugin:{name:"demo-plugin",version:"0.1.0"},transports:["poll"]}')")
session_token=$(echo "$session" | jq -r '.sessionToken')
echo "$session" | jq '{sessionId, pollTimeoutSeconds, envelopeVersion}'

step "1. The plugin starts a poll. Nothing is queued, so the hub holds it open"
poll_output=$(mktemp)
( poll "$session_token" '{"ack":0}' > "$poll_output" ) &
poll_pid=$!
poll_started=$(date +%s%N)
sleep 1

step "2. Mid-hold, the operator queues an envelope for that server"
api POST "/api/v1/servers/$server_id/envelopes" "$ADMIN_TOKEN" \
  '{"type":"demo.announce","body":{"message":"hello from the operator"}}' \
  | jq '.envelope'

step "3. The held poll flushes at once rather than waiting the 25 s out"
wait "$poll_pid"
elapsed_ms=$(( ($(date +%s%N) - poll_started) / 1000000 ))
jq '{ack, pollTimeoutSeconds, envelopes: [.envelopes[] | {v, id, type, seq, ts, body}]}' < "$poll_output"
printf 'delivered %s ms after the poll began (1000 ms of that is this script sleeping)\n' "$elapsed_ms"

seq=$(jq -r '.envelopes[0].seq' < "$poll_output")

step "4. Without an ack, the same envelope comes back: same id, same seq, same ts"
poll "$session_token" '{"ack":0}' | jq '[.envelopes[] | {id, seq, ts}]'

step "5. The operator sees the work still queued"
api GET "/api/v1/servers/$server_id" "$ADMIN_TOKEN" | jq '{pendingEnvelopeCount, lastSeenAt}'

# From here the queue is empty, so every poll below is held for the full 25 s
# before answering. That pause is the protocol working, not the demo hanging.
step "6. The plugin acks seq $seq, and sends two envelopes of its own (holds 25 s)"
poll "$session_token" "$(jq -nc --argjson ack "$seq" '
  {ack:$ack, envelopes:[
    {v:1,id:"demo-1",type:"demo.ready",seq:1,ts:"2026-08-16T18:00:00Z",body:{}},
    {v:1,id:"demo-2",type:"demo.type.this.hub.has.never.heard.of",seq:2,ts:"2026-08-16T18:00:01Z",body:{}}
  ]}')" | jq '{ack, envelopes}'
echo "the hub acked seq 2: an unknown type is acked and ignored, never fatal"

step "7. The queue is empty again"
api GET "/api/v1/servers/$server_id" "$ADMIN_TOKEN" | jq '{pendingEnvelopeCount}'

step "8. An ack above what the hub has sent is refused, not silently accepted"
expect_failure poll "$session_token" '{"ack":9999}'

step "9. A gap in the plugin's own sequence does not move the hub's ack (holds 25 s)"
poll "$session_token" "$(jq -nc '
  {ack:1, envelopes:[
    {v:1,id:"demo-9",type:"demo.ready",seq:9,ts:"2026-08-16T18:00:02Z",body:{}}
  ]}')" | jq '{ack}'
echo "still 2: an envelope above a gap is discarded, and retransmission recovers it"

rm -f "$poll_output"
printf '\n\033[1mdone\033[0m: server %s round-tripped envelopes in both directions\n' "$server_id"
