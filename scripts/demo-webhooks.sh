#!/usr/bin/env bash
# Walks webhooks (spec/protocol.md section 11) end to end: a local receiver, a
# registered webhook, a plugin pushing core.player.death, and the signed
# delivery arriving, verified against the registration secret with openssl.
#
#   VYSHKA_ADMIN_TOKEN=... scripts/demo-webhooks.sh [hub-url]
#
# Needs jq, python3 (the throwaway receiver), and openssl (the signature
# check). The receiver binds 127.0.0.1:8099; the hub must be able to reach it.
set -euo pipefail

HUB_URL="${1:-${VYSHKA_HUB_URL:-http://127.0.0.1:8080}}"
ADMIN_TOKEN="${VYSHKA_ADMIN_TOKEN:-}"
RECEIVER_PORT=8099
CAPTURE_DIR="$(mktemp -d)"

for tool in jq python3 openssl; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "this demo needs $tool" >&2
    exit 2
  fi
done
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

step "A local receiver on 127.0.0.1:$RECEIVER_PORT, capturing every delivery"
python3 - "$RECEIVER_PORT" "$CAPTURE_DIR" <<'PY' &
import http.server, json, pathlib, sys

port, capture = int(sys.argv[1]), pathlib.Path(sys.argv[2])

class Receiver(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        n = len(list(capture.glob("delivery-*.json")))
        (capture / f"delivery-{n}.json").write_bytes(body)
        (capture / f"delivery-{n}.headers").write_text(json.dumps({
            "X-Vyshka-Signature": self.headers.get("X-Vyshka-Signature", ""),
            "X-Vyshka-Delivery": self.headers.get("X-Vyshka-Delivery", ""),
            "X-Vyshka-Attempt": self.headers.get("X-Vyshka-Attempt", ""),
        }))
        self.send_response(204)
        self.end_headers()
    def log_message(self, *args):
        pass

http.server.HTTPServer(("127.0.0.1", port), Receiver).serve_forever()
PY
RECEIVER_PID=$!
trap 'kill "$RECEIVER_PID" 2>/dev/null || true; rm -rf "$CAPTURE_DIR"' EXIT
sleep 0.5

step "Register a webhook for core.player.death; the secret is returned once"
registered=$(api POST /api/v1/webhooks "$ADMIN_TOKEN" \
  "{\"url\":\"http://127.0.0.1:$RECEIVER_PORT/hook\",\"events\":[\"core.player.death\"]}")
webhook_id=$(echo "$registered" | jq -r '.webhook.id')
secret=$(echo "$registered" | jq -r '.secret')
echo "$registered" | jq '{webhook: .webhook, secret: "(kept for the signature check)"}'

step "Setup: server, enrolled plugin, session"
created=$(api POST /api/v1/servers "$ADMIN_TOKEN" '{"name":"Webhooks demo","game":"dayz"}')
server_id=$(echo "$created" | jq -r '.server.id')
enrolled=$(api POST /plugin/v1/enroll "" \
  "$(jq -nc --arg t "$(echo "$created" | jq -r '.enrollment.token')" \
      '{enrollmentToken:$t,game:"dayz",plugin:{name:"demo-plugin",version:"0.1.0"},transports:["poll"]}')")
session=$(api POST /plugin/v1/session "" \
  "$(echo "$enrolled" | jq -c '{serverId,serverSecret,protocolVersion:1,pollTimeoutSeconds:5}')")
session_token=$(echo "$session" | jq -r '.sessionToken')
echo "  server $server_id enrolled, session live"

step "The plugin pushes a core.player.death event"
now=$(date -u +%Y-%m-%dT%H:%M:%SZ)
api POST /plugin/v1/poll "$session_token" "$(jq -nc --arg ts "$now" '{
  ack: 0,
  envelopes: [{v:1, id:"demo-death-1", type:"event.batch", seq:1, ts:$ts,
    body:{events:[{t:"core.player.death",
      data:{victim:{platform:"steam",id:"76561198000000000"},weapon:"M4A1",distance:312.5}}]}}]
}')" | jq -c '{ack}'

step "The signed delivery arrives"
for _ in $(seq 1 60); do
  [ -f "$CAPTURE_DIR/delivery-0.json" ] && break
  sleep 0.5
done
if [ ! -f "$CAPTURE_DIR/delivery-0.json" ]; then
  echo "no delivery arrived within 30s" >&2
  exit 1
fi
jq . < "$CAPTURE_DIR/delivery-0.json"
jq . < "$CAPTURE_DIR/delivery-0.headers"

step "The signature verifies against the registration secret"
signature=$(jq -r '."X-Vyshka-Signature"' < "$CAPTURE_DIR/delivery-0.headers")
computed="sha256=$(openssl dgst -sha256 -hmac "$secret" < "$CAPTURE_DIR/delivery-0.json" | awk '{print $NF}')"
echo "  received: $signature"
echo "  computed: $computed"
[ "$signature" = "$computed" ] && echo "  MATCH" || { echo "  MISMATCH" >&2; exit 1; }

step "The delivery record agrees (state, attempts)"
api GET "/api/v1/webhooks/$webhook_id/deliveries" "$ADMIN_TOKEN" \
  | jq '.deliveries[0] | {id, type, state, attempts, deliveredAt}'

step "Cleanup: delete the webhook"
api DELETE "/api/v1/webhooks/$webhook_id" "$ADMIN_TOKEN"
echo "  deleted"
