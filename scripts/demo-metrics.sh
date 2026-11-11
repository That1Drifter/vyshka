#!/usr/bin/env bash
# Shows the Prometheus endpoint (deploy/README.md, "Metrics") against a real
# hub: it drives traffic and checks that every promised metric moves by the
# amount the traffic implies, not merely that it exists. Poll latency (handler
# time apart from the hold), queue depths, action outcomes by code, dropped
# envelopes, webhook failures, and the sweeps.
#
#   VYSHKA_ADMIN_TOKEN=... VYSHKA_METRICS_URL=http://127.0.0.1:9090 \
#     scripts/demo-metrics.sh [hub-url]
#
# The hub must run with -metrics-addr (scripts/e2e.sh does that). Needs jq,
# python3 (a throwaway webhook receiver and the queue filler), curl, and awk.
# Every check compares a scrape before and after one step, so it holds on a hub
# that other demos have already used; it assumes nothing else is polling.
set -euo pipefail

HUB_URL="${1:-${VYSHKA_HUB_URL:-http://127.0.0.1:8080}}"
ADMIN_TOKEN="${VYSHKA_ADMIN_TOKEN:-}"
METRICS_URL="${VYSHKA_METRICS_URL:-}"
RECEIVER_PORT=8098

for tool in jq python3 curl awk; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "this demo needs $tool" >&2
    exit 2
  fi
done
if [ -z "$ADMIN_TOKEN" ]; then
  echo "set VYSHKA_ADMIN_TOKEN to the hub's admin token" >&2
  exit 2
fi
if [ -z "$METRICS_URL" ]; then
  echo "set VYSHKA_METRICS_URL to the hub's metrics listener, e.g. http://127.0.0.1:9090" >&2
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

scrape() { curl --silent --show-error --fail "$METRICS_URL/metrics"; }

# value SCRAPE SERIES: the sample whose name and labels are exactly SERIES, or
# 0 when the scrape has none (a counter that never moved is not exposed yet).
value() {
  printf '%s\n' "$1" | awk -v key="$2" 'index($0, key " ") == 1 { print $NF; found = 1 } END { if (!found) print 0 }'
}

failures=0
# check LABEL BEFORE AFTER WANT [ge]: the movement between two scrapes must be
# WANT (or at least WANT with ge).
check() {
  local label="$1" before="$2" after="$3" want="$4" mode="${5:-eq}"
  local moved ok
  moved=$(awk -v a="$before" -v b="$after" 'BEGIN { printf "%.6g", b - a }')
  ok=$(awk -v g="$moved" -v w="$want" -v m="$mode" \
    'BEGIN { if (m == "ge") print (g + 1e-9 >= w); else print ((g - w) < 1e-6 && (w - g) < 1e-6) }')
  if [ "$ok" = 1 ]; then
    printf '  PASS  %-58s %s -> %s  (moved %s, wanted %s%s)\n' "$label" "$before" "$after" "$moved" \
      "$([ "$mode" = ge ] && echo 'at least ')" "$want"
  else
    printf '  FAIL  %-58s %s -> %s  (moved %s, wanted %s%s)\n' "$label" "$before" "$after" "$moved" \
      "$([ "$mode" = ge ] && echo 'at least ')" "$want" >&2
    failures=$((failures + 1))
  fi
}
# at_most LABEL VALUE LIMIT: an upper bound.
at_most() {
  if awk -v v="$2" -v l="$3" 'BEGIN { exit !(v <= l) }'; then
    printf '  PASS  %-58s %s (limit %s)\n' "$1" "$2" "$3"
  else
    printf '  FAIL  %-58s %s (limit %s)\n' "$1" "$2" "$3" >&2
    failures=$((failures + 1))
  fi
}

# --- a plugin, by hand ------------------------------------------------------
# enroll_server NAME sets SERVER_ID and SESSION_TOKEN.
enroll_server() {
  local created enrolled session
  created=$(api POST /api/v1/servers "$ADMIN_TOKEN" "$(jq -nc --arg n "$1" '{name:$n,game:"dayz"}')")
  SERVER_ID=$(echo "$created" | jq -r '.server.id')
  enrolled=$(api POST /plugin/v1/enroll "" \
    "$(jq -nc --arg t "$(echo "$created" | jq -r '.enrollment.token')" \
        '{enrollmentToken:$t,game:"dayz",plugin:{name:"demo-plugin",version:"0.1.0"},transports:["poll"]}')")
  session=$(api POST /plugin/v1/session "" \
    "$(jq -nc --arg id "$SERVER_ID" --arg secret "$(echo "$enrolled" | jq -r '.serverSecret')" \
        '{serverId:$id,serverSecret:$secret,pollTimeoutSeconds:5,plugin:{name:"demo-plugin",version:"0.1.0"},transports:["poll"]}')")
  SESSION_TOKEN=$(echo "$session" | jq -r '.sessionToken')
}

# poll TOKEN BODY: one poll. A poll with nothing to return would be held for
# the full pollTimeout (5 s, the protocol's floor), so every poll below carries
# a new envelope with "more": true, which a hub answers at once (section 3.1.2).
poll() { api POST /plugin/v1/poll "$1" "$2"; }

# envelope TYPE SEQ ID BODY
envelope() {
  jq -nc --arg type "$1" --argjson seq "$2" --arg id "$3" --argjson body "$4" \
    '{v:1,id:$id,type:$type,seq:$seq,ts:"2026-11-11T12:00:00Z",body:$body}'
}

WORK_DIR="$(mktemp -d)"
RECEIVER_PID=""
cleanup() {
  [ -n "$RECEIVER_PID" ] && kill "$RECEIVER_PID" 2>/dev/null || true
  [ -n "${WEBHOOK_FAIL:-}" ] && api DELETE "/api/v1/webhooks/$WEBHOOK_FAIL" "$ADMIN_TOKEN" >/dev/null 2>&1 || true
  [ -n "${WEBHOOK_OK:-}" ] && api DELETE "/api/v1/webhooks/$WEBHOOK_OK" "$ADMIN_TOKEN" >/dev/null 2>&1 || true
  rm -rf "$WORK_DIR"
}
trap cleanup EXIT

step "The endpoint is on its own listener; the Plugin API listener does not serve it"
code=$(curl --silent --output /dev/null --write-out '%{http_code}' "$HUB_URL/metrics")
echo "  GET $HUB_URL/metrics          -> $code (any unrouted path answers 404)"
[ "$code" = 404 ] || { echo "  FAIL  the main listener answered /metrics with $code" >&2; failures=$((failures + 1)); }
code=$(curl --silent --output /dev/null --write-out '%{http_code}' "$METRICS_URL/")
echo "  GET $METRICS_URL/             -> $code (the metrics listener serves /metrics and nothing else)"
[ "$code" = 404 ] || { echo "  FAIL  the metrics listener answered / with $code" >&2; failures=$((failures + 1)); }
first=$(scrape)
echo "  GET $METRICS_URL/metrics      -> 200, $(printf '%s\n' "$first" | grep -c '^# TYPE') metric families"
printf '%s\n' "$first" | grep -E '^vyshka_(build_info|start_time_seconds)' | sed 's/^/  /'

step "Setup: a server with an enrolled plugin and a manifest of three actions"
enroll_server "Metrics demo"
S="$SERVER_ID"; S_TOKEN="$SESSION_TOKEN"
echo "  server $S"
manifest='{
  "manifestRevision": 1,
  "actions": [
    {"code":"metrics-demo.ok",   "name":"Succeeds",       "context":"player", "namespace":"metrics-demo", "danger":"none", "params":{"type":"object","properties":{}}},
    {"code":"metrics-demo.fail", "name":"Fails",          "context":"player", "namespace":"metrics-demo", "danger":"none", "params":{"type":"object","properties":{}}},
    {"code":"metrics-demo.slow", "name":"Never executed", "context":"player", "namespace":"metrics-demo", "danger":"none", "params":{"type":"object","properties":{}}}
  ]
}'

step "1. Polls: the plugin publishes its manifest in the first one"
before=$(scrape)
poll "$S_TOKEN" "$(jq -nc --argjson e "$(envelope manifest.publish 1 demo-manifest "$manifest")" '{more:true,envelopes:[$e]}')" | jq -c '{ack}'
after=$(scrape)
check "vyshka_poll_handler_seconds_count (one poll)" "$(value "$before" vyshka_poll_handler_seconds_count)" "$(value "$after" vyshka_poll_handler_seconds_count)" 1
check "vyshka_poll_hold_seconds_count (one poll)" "$(value "$before" vyshka_poll_hold_seconds_count)" "$(value "$after" vyshka_poll_hold_seconds_count)" 1
check 'vyshka_inbound_envelopes_total{result="accepted"}' \
  "$(value "$before" 'vyshka_inbound_envelopes_total{result="accepted"}')" "$(value "$after" 'vyshka_inbound_envelopes_total{result="accepted"}')" 1

step "2. Queue depth: the operator dispatches three actions (the third with a 1 s TTL)"
for action in ok fail; do
  api POST "/api/v1/servers/$S/actions" "$ADMIN_TOKEN" "{\"code\":\"metrics-demo.$action\",\"params\":{}}" >/dev/null
done
api POST "/api/v1/servers/$S/actions" "$ADMIN_TOKEN" '{"code":"metrics-demo.slow","params":{},"ttlSeconds":1}' >/dev/null
sleep 0.2
queued=$(scrape)
depth='vyshka_outbound_queue_depth{server="'"$S"'"}'
oldest='vyshka_outbound_queue_oldest_seconds{server="'"$S"'"}'
check "$depth" 0 "$(value "$queued" "$depth")" 3
at_most "vyshka_outbound_queue_oldest_seconds (just queued)" "$(value "$queued" "$oldest")" 5
echo "  waiting 2.5 s: the expiry sweep retires the unsent, expired dispatch (section 9.2)"
sleep 2.5
expired=$(scrape)
check "$depth" "$(value "$queued" "$depth")" "$(value "$expired" "$depth")" -1
check 'vyshka_actions_finished_total{code="metrics-demo.slow",outcome="expired"}' 0 \
  "$(value "$expired" 'vyshka_actions_finished_total{code="metrics-demo.slow",outcome="expired"}')" 1
check 'vyshka_sweep_removed_total{sweep="action_expiry"}' \
  "$(value "$queued" 'vyshka_sweep_removed_total{sweep="action_expiry"}')" \
  "$(value "$expired" 'vyshka_sweep_removed_total{sweep="action_expiry"}')" 1 ge
check 'vyshka_sweep_runs_total{sweep="action_expiry",result="ok"} (every 0.5 s)' \
  "$(value "$queued" 'vyshka_sweep_runs_total{sweep="action_expiry",result="ok"}')" \
  "$(value "$expired" 'vyshka_sweep_runs_total{sweep="action_expiry",result="ok"}')" 3 ge

step "3. Delivery: the next poll carries the two live dispatches; queue wait is observed once each"
before=$(scrape)
delivery=$(poll "$S_TOKEN" "$(jq -nc --argjson e "$(envelope event.batch 2 demo-heartbeat '{"events":[{"t":"core.demo.tick","data":{}}]}')" '{more:true,envelopes:[$e]}')")
after=$(scrape)
echo "$delivery" | jq -c '[.envelopes[] | {type, seq}]'
out_ack=$(echo "$delivery" | jq '[.envelopes[].seq] | max')
check "vyshka_envelope_delivery_seconds_count (two first sends)" \
  "$(value "$before" vyshka_envelope_delivery_seconds_count)" "$(value "$after" vyshka_envelope_delivery_seconds_count)" 2
check "$depth (sent, not yet acked)" "$(value "$before" "$depth")" "$(value "$after" "$depth")" 0

step "4. Action outcomes by code: one completes, one fails"
ok_id=$(echo "$delivery" | jq -r '[.envelopes[] | select(.body.code == "metrics-demo.ok")][0].body.actionId')
fail_id=$(echo "$delivery" | jq -r '[.envelopes[] | select(.body.code == "metrics-demo.fail")][0].body.actionId')
before=$(scrape)
poll "$S_TOKEN" "$(jq -nc --argjson ack "$out_ack" --arg ok "$ok_id" --arg fail "$fail_id" \
  --argjson e1 "$(envelope action.ack 3 demo-ack '{"actionId":"'"$ok_id"'"}')" \
  --argjson e2 "$(envelope action.result 4 demo-result-ok '{"actionId":"'"$ok_id"'","ok":true,"durationMs":5}')" \
  --argjson e3 "$(envelope action.result 5 demo-result-fail '{"actionId":"'"$fail_id"'","ok":false,"error":"boom"}')" \
  '{ack:$ack,more:true,envelopes:[$e1,$e2,$e3]}')" | jq -c '{ack}'
after=$(scrape)
for series in 'vyshka_actions_finished_total{code="metrics-demo.ok",outcome="completed"}' \
              'vyshka_actions_finished_total{code="metrics-demo.fail",outcome="failed"}'; do
  check "$series" "$(value "$before" "$series")" "$(value "$after" "$series")" 1
done
check "$depth (both dispatches acked: empty again)" "$(value "$before" "$depth")" "$(value "$after" "$depth")" -2

step "5. Inbound results: a retransmission is a duplicate, a seq beyond a hole is gapped"
before=$(scrape)
poll "$S_TOKEN" "$(jq -nc --argjson dup "$(envelope action.ack 5 demo-dup '{"actionId":"'"$ok_id"'"}')" \
  --argjson gap "$(envelope action.ack 9 demo-gap '{"actionId":"'"$ok_id"'"}')" \
  '{more:true,envelopes:[$dup,$gap]}')" | jq -c '{ack}'
after=$(scrape)
check 'vyshka_inbound_envelopes_total{result="duplicate"}' \
  "$(value "$before" 'vyshka_inbound_envelopes_total{result="duplicate"}')" "$(value "$after" 'vyshka_inbound_envelopes_total{result="duplicate"}')" 1
check 'vyshka_inbound_envelopes_total{result="gapped"}' \
  "$(value "$before" 'vyshka_inbound_envelopes_total{result="gapped"}')" "$(value "$after" 'vyshka_inbound_envelopes_total{result="gapped"}')" 1
check 'vyshka_inbound_envelopes_total{result="accepted"} (unchanged)' \
  "$(value "$before" 'vyshka_inbound_envelopes_total{result="accepted"}')" "$(value "$after" 'vyshka_inbound_envelopes_total{result="accepted"}')" 0

step "6. Dropped envelopes: 25 refused event batches owe 25 notices; one poll may queue 20"
bad_batches=()
for i in $(seq 1 25); do
  bad_batches+=("$(envelope event.batch $((5 + i)) "demo-bad-$i" '{"events":[{"data":{}}]}')")
done
before=$(scrape)
refused=$(poll "$S_TOKEN" "$(printf '%s\n' "${bad_batches[@]}" | jq -sc '{more:true,envelopes:.}')")
after=$(scrape)
echo "$refused" | jq -c '{ack, notices: ([.envelopes[] | select(.type == "event.reject")] | length)}'
check 'vyshka_inbound_envelopes_refused_total{kind="event_batch"}' \
  "$(value "$before" 'vyshka_inbound_envelopes_refused_total{kind="event_batch"}')" \
  "$(value "$after" 'vyshka_inbound_envelopes_refused_total{kind="event_batch"}')" 25
check 'vyshka_outbound_envelopes_dropped_total{reason="notice_cap"}' \
  "$(value "$before" 'vyshka_outbound_envelopes_dropped_total{reason="notice_cap"}')" \
  "$(value "$after" 'vyshka_outbound_envelopes_dropped_total{reason="notice_cap"}')" 5
check "$depth (20 notices queued, sent, unacked)" 0 "$(value "$after" "$depth")" 20

step "7. Dropped envelopes, the other way: a server whose queue is at its bound of 5000"
enroll_server "Metrics demo, full queue"
S2="$SERVER_ID"; S2_TOKEN="$SESSION_TOKEN"
python3 - "$HUB_URL" "$ADMIN_TOKEN" "$S2" <<'PY'
import http.client, json, sys, urllib.parse
hub, token, server = sys.argv[1:4]
u = urllib.parse.urlparse(hub)
c = http.client.HTTPConnection(u.hostname, u.port)
headers = {"Authorization": "Bearer " + token, "Content-Type": "application/json"}
for i in range(5000):
    c.request("POST", f"/api/v1/servers/{server}/envelopes", json.dumps({"type": "demo.filler", "body": {"n": i}}), headers)
    r = c.getresponse(); r.read()
    if r.status != 202:
        sys.exit(f"queueing envelope {i} answered {r.status}")
c.request("POST", f"/api/v1/servers/{server}/envelopes", json.dumps({"type": "demo.filler"}), headers)
r = c.getresponse(); body = json.loads(r.read())
print(f"  5000 queued; the 5001st is refused: {r.status} {body['error']['code']}")
PY
depth2='vyshka_outbound_queue_depth{server="'"$S2"'"}'
before=$(scrape)
poll "$S2_TOKEN" "$(jq -nc --argjson e "$(envelope event.batch 1 demo-bad-full '{"events":[{"data":{}}]}')" '{more:true,envelopes:[$e]}')" \
  | jq -c '{ack, delivered: (.envelopes | length)}'
after=$(scrape)
check "$depth2" 0 "$(value "$after" "$depth2")" 5000
check 'vyshka_outbound_envelopes_dropped_total{reason="queue_full"} (the notice)' \
  "$(value "$before" 'vyshka_outbound_envelopes_dropped_total{reason="queue_full"}')" \
  "$(value "$after" 'vyshka_outbound_envelopes_dropped_total{reason="queue_full"}')" 1
echo "  one scrape with a full queue:" \
  "$(curl --silent --output /dev/null --write-out '%{time_total}s' "$METRICS_URL/metrics")"

step "8. Webhook failure rate: one webhook whose receiver answers 500, one that answers 204"
python3 - "$RECEIVER_PORT" <<'PY' &
import http.server, sys

class Receiver(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        self.rfile.read(int(self.headers.get("Content-Length", 0)))
        self.send_response(500 if self.path == "/fail" else 204)
        self.end_headers()
    def log_message(self, *args):
        pass

http.server.HTTPServer(("127.0.0.1", int(sys.argv[1])), Receiver).serve_forever()
PY
RECEIVER_PID=$!
sleep 0.5
WEBHOOK_FAIL=$(api POST /api/v1/webhooks "$ADMIN_TOKEN" \
  "{\"url\":\"http://127.0.0.1:$RECEIVER_PORT/fail\",\"events\":[\"core.player.death\"],\"serverIds\":[\"$S\"]}" | jq -r '.webhook.id')
WEBHOOK_OK=$(api POST /api/v1/webhooks "$ADMIN_TOKEN" \
  "{\"url\":\"http://127.0.0.1:$RECEIVER_PORT/ok\",\"events\":[\"core.player.death\"],\"serverIds\":[\"$S\"]}" | jq -r '.webhook.id')
echo "  failing webhook $WEBHOOK_FAIL, healthy webhook $WEBHOOK_OK"
before=$(scrape)
poll "$S_TOKEN" "$(jq -nc --argjson e "$(envelope event.batch 31 demo-death \
  '{"events":[{"t":"core.player.death","data":{"victim":{"platform":"steam","id":"76561198000000000"},"weapon":"M4A1"}}]}')" \
  '{ack:0,more:true,envelopes:[$e]}')" >/dev/null
failed='vyshka_webhook_attempts_total{webhook="'"$WEBHOOK_FAIL"'",result="retry"}'
delivered='vyshka_webhook_attempts_total{webhook="'"$WEBHOOK_OK"'",result="delivered"}'
for _ in $(seq 1 60); do
  after=$(scrape)
  [ "$(value "$after" "$failed")" != 0 ] && [ "$(value "$after" "$delivered")" != 0 ] && break
  sleep 0.5
done
check "$failed" 0 "$(value "$after" "$failed")" 1
check "$delivered" 0 "$(value "$after" "$delivered")" 1
check 'vyshka_webhook_deliveries_pending (the failed one waits for its retry)' \
  "$(value "$before" vyshka_webhook_deliveries_pending)" "$(value "$after" vyshka_webhook_deliveries_pending)" 1 ge
echo "  failure rate for the failing webhook: 1 failed of 1 attempt; for the healthy one: 0 of 1"

step "9. Poll latency: a poll that is held for its full timeout adds to the hold, not to handler time"
# Both queues are drained here: S has nothing queued but the notices, so ack them.
notice_ack=$(echo "$refused" | jq '[.envelopes[].seq] | max')
before=$(scrape)
started=$(date +%s)
poll "$S_TOKEN" "{\"ack\":$notice_ack}" | jq -c '{envelopes: (.envelopes | length)}'
after=$(scrape)
echo "  that poll took $(( $(date +%s) - started )) s on the wire"
hold_moved=$(awk -v a="$(value "$before" vyshka_poll_hold_seconds_sum)" -v b="$(value "$after" vyshka_poll_hold_seconds_sum)" 'BEGIN { print b - a }')
handler_moved=$(awk -v a="$(value "$before" vyshka_poll_handler_seconds_sum)" -v b="$(value "$after" vyshka_poll_handler_seconds_sum)" 'BEGIN { print b - a }')
check "vyshka_poll_hold_seconds_sum (a 5 s hold)" "$(value "$before" vyshka_poll_hold_seconds_sum)" "$(value "$after" vyshka_poll_hold_seconds_sum)" 4 ge
at_most "vyshka_poll_handler_seconds_sum for the same poll" "$handler_moved" 1
check "$depth (notices acked)" 0 "$(value "$after" "$depth")" 0

step "10. Sweeps: the ban sweep and the expiry sweep report when they last succeeded"
final=$(scrape)
now=$(date +%s)
for sweep in action_expiry ban_expiry; do
  last=$(value "$final" 'vyshka_sweep_last_success_timestamp_seconds{sweep="'"$sweep"'"}')
  at_most "age of the last $sweep success, seconds" "$(awk -v n="$now" -v l="$last" 'BEGIN { print n - l }')" 30
done

step "11. The exposition is well formed"
bad=$(printf '%s\n' "$final" | grep -vE '^(#|[a-zA-Z_:][a-zA-Z0-9_:]*(\{[^}]*\})? [-+0-9.eEInfNa]+$)' || true)
if [ -n "$bad" ]; then
  echo "  FAIL  lines that are neither comments nor samples:" >&2
  printf '%s\n' "$bad" >&2
  failures=$((failures + 1))
else
  echo "  PASS  every line is a comment or a sample"
fi
if command -v promtool >/dev/null 2>&1; then
  if printf '%s\n' "$final" | promtool check metrics; then
    echo "  PASS  promtool check metrics"
  else
    failures=$((failures + 1))
  fi
else
  echo "  (promtool is not installed here; skipped)"
fi

if [ "$failures" -ne 0 ]; then
  printf '\n\033[1m%d check(s) failed\033[0m\n' "$failures" >&2
  exit 1
fi
printf '\n\033[1mdone\033[0m: every metric moved by the amount the traffic implies\n'
