#!/usr/bin/env bash
# Shows the per-credential rate limits (spec section 2.2.1) doing their job on
# a real hub: one server's credential floods the Plugin API and is refused with
# 429 and a Retry-After, while a second server on the same hub keeps polling
# and taking deliveries at its usual latency; the flooding server's own
# envelope, refused mid-flood, is applied exactly once when it is sent again
# after the wait. Then the same for a flooding admin token beside a
# well-behaved one, including what the audit log keeps of the flood.
#
#   VYSHKA_ADMIN_TOKEN=... scripts/demo-rate-limits.sh [hub-url]
#
# The hub must run with its limits on (the default). Against a hub with them
# off (-plugin-rate-limit 0 -admin-rate-limit 0) the floods are not refused
# and the demo fails, which is its negative control. Needs jq, curl, awk, and
# python3 (the flooder: 16 keep-alive connections, because curl's parallel
# mode opens a fresh connection for most transfers and a flood of those
# exhausts the client's own ephemeral ports before it says anything about the
# hub). VYSHKA_METRICS_URL, when set, adds the vyshka_rate_limited_total
# counter to the evidence.
set -euo pipefail

HUB_URL="${1:-${VYSHKA_HUB_URL:-http://127.0.0.1:8080}}"
ADMIN_TOKEN="${VYSHKA_ADMIN_TOKEN:-}"
METRICS_URL="${VYSHKA_METRICS_URL:-}"
# How long each flood runs, and over how many connections.
FLOOD_SECONDS="${FLOOD_SECONDS:-6}"
FLOOD_WORKERS=16

for tool in jq curl awk python3; do
  command -v "$tool" >/dev/null 2>&1 || { echo "this demo needs $tool" >&2; exit 2; }
done
[ -n "$ADMIN_TOKEN" ] || { echo "set VYSHKA_ADMIN_TOKEN to the hub's admin token" >&2; exit 2; }

step() { printf '\n\033[1m%s\033[0m\n' "$*"; }
failures=0
pass() { printf '  PASS  %s\n' "$*"; }
fail() { printf '  FAIL  %s\n' "$*" >&2; failures=$((failures + 1)); }

api() {
  local method="$1" path="$2" auth="${3:-}" body="${4:-}"
  local args=(--silent --show-error --fail-with-body -X "$method" "$HUB_URL$path")
  [ -n "$auth" ] && args+=(-H "Authorization: Bearer $auth")
  [ -n "$body" ] && args+=(-H 'Content-Type: application/json' -d "$body")
  curl "${args[@]}"
}
now_ms() { date +%s%3N; }
median() { sort -n | awk '{ v[NR] = $1 } END { if (NR == 0) print "n/a"; else print v[int((NR + 1) / 2)] }'; }
maxof() { sort -n | tail -n 1; }

WORK_DIR="$(mktemp -d)"
FLOOD_PIDS=()
cleanup() {
  for pid in "${FLOOD_PIDS[@]}"; do kill "$pid" 2>/dev/null || true; done
  [ -n "${T1_ID:-}" ] && api DELETE "/api/v1/tokens/$T1_ID" "$ADMIN_TOKEN" >/dev/null 2>&1 || true
  [ -n "${T2_ID:-}" ] && api DELETE "/api/v1/tokens/$T2_ID" "$ADMIN_TOKEN" >/dev/null 2>&1 || true
  [ -n "${T3_ID:-}" ] && api DELETE "/api/v1/tokens/$T3_ID" "$ADMIN_TOKEN" >/dev/null 2>&1 || true
  [ -n "${T4_ID:-}" ] && api DELETE "/api/v1/tokens/$T4_ID" "$ADMIN_TOKEN" >/dev/null 2>&1 || true
  rm -rf "$WORK_DIR"
}
trap cleanup EXIT

# enroll NAME: prints "serverId sessionToken" for a fresh server with a live
# session whose polls hold for the protocol's floor of 5 s.
enroll() {
  local created enrolled session id
  created=$(api POST /api/v1/servers "$ADMIN_TOKEN" "$(jq -nc --arg n "$1" '{name:$n,game:"dayz"}')")
  id=$(echo "$created" | jq -r '.server.id')
  enrolled=$(api POST /plugin/v1/enroll "" \
    "$(jq -nc --arg t "$(echo "$created" | jq -r '.enrollment.token')" \
        '{enrollmentToken:$t,game:"dayz",plugin:{name:"demo-plugin",version:"0.1.0"},transports:["poll"]}')")
  session=$(api POST /plugin/v1/session "" \
    "$(jq -nc --arg id "$id" --arg secret "$(echo "$enrolled" | jq -r '.serverSecret')" \
        '{serverId:$id,serverSecret:$secret,pollTimeoutSeconds:5,plugin:{name:"demo-plugin",version:"0.1.0"},transports:["poll"]}')")
  echo "$id $(echo "$session" | jq -r '.sessionToken')"
}

# flood TOKEN PATH OUT: GETs PATH with TOKEN as fast as FLOOD_WORKERS
# keep-alive connections go, for FLOOD_SECONDS, one status per line into OUT
# (000 for a request that failed below HTTP). The script travels on the
# command line and the statuses on stdout, so no path crosses between bash
# and a Windows-native python.
FLOODER='
import http.client, sys, threading, time, urllib.parse
base, path, token, seconds, workers = sys.argv[1], sys.argv[2], sys.argv[3], float(sys.argv[4]), int(sys.argv[5])
url = urllib.parse.urlsplit(base)
make = http.client.HTTPSConnection if url.scheme == "https" else http.client.HTTPConnection
deadline = time.monotonic() + seconds
results = []
def work():
    mine, conn = [], None
    while time.monotonic() < deadline:
        try:
            if conn is None:
                conn = make(url.hostname, url.port, timeout=10)
            conn.request("GET", url.path.rstrip("/") + path, headers={"Authorization": "Bearer " + token})
            response = conn.getresponse()
            response.read()
            mine.append(str(response.status))
            if response.getheader("Connection", "").lower() == "close":
                conn.close(); conn = None
        except Exception:
            mine.append("000")
            if conn is not None:
                conn.close()
            conn = None
    results.extend(mine)
threads = [threading.Thread(target=work) for _ in range(workers)]
for t in threads: t.start()
for t in threads: t.join()
sys.stdout.write("\n".join(results) + "\n")
'
# KEEPUP: a plugin that keeps one poll in flight and answers every action
# dispatch with an ack and a result, beside an admin dispatching to it at a
# steady rate, for a while; prints one JSON summary. Arguments: hub URL,
# server id, the plugin's session token, the dispatcher's token, seconds,
# dispatches a second.
KEEPUP_SECONDS="${KEEPUP_SECONDS:-20}"
DISPATCH_RATE="${DISPATCH_RATE:-10}"
KEEPUP='
import http.client, json, sys, threading, time, urllib.parse
base, server, session, admin = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4]
seconds, rate = float(sys.argv[5]), float(sys.argv[6])
url = urllib.parse.urlsplit(base)
make = http.client.HTTPSConnection if url.scheme == "https" else http.client.HTTPConnection
prefix = url.path.rstrip("/")
stop = threading.Event()
stats = {"polls": 0, "pollsRefused": 0, "dispatched": 0, "dispatchesRefused": 0, "resultsAcked": 0}

def call(conn, method, path, token, body):
    conn.request(method, prefix + path, body=json.dumps(body),
                 headers={"Authorization": "Bearer " + token, "Content-Type": "application/json"})
    response = conn.getresponse()
    return response.status, response.getheader("Retry-After"), json.loads(response.read() or b"{}")

def dispatcher():
    conn = make(url.hostname, url.port, timeout=15)
    started, sent = time.monotonic(), 0
    while time.monotonic() - started < seconds:
        status, _, _ = call(conn, "POST", "/api/v1/servers/" + server + "/actions", admin,
                            {"code": "rl-demo.ping", "params": {}})
        stats["dispatched"] += 1
        if status == 429:
            stats["dispatchesRefused"] += 1
        sent += 1
        time.sleep(max(0, started + sent / rate - time.monotonic()))
    stop.set()

def plugin():
    conn = make(url.hostname, url.port, timeout=15)
    ack, seq, outbox = 0, 1, []
    done_at = None
    while True:
        if stop.is_set():
            done_at = done_at or time.monotonic()
            if (not outbox and stats["resultsAcked"] >= stats["dispatched"] - stats["dispatchesRefused"]) or time.monotonic() - done_at > 15:
                return
        status, retry, body = call(conn, "POST", "/plugin/v1/poll", session, {"ack": ack, "envelopes": outbox})
        stats["polls"] += 1
        if status == 429:
            stats["pollsRefused"] += 1
            time.sleep(int(retry or 1))
            continue
        acked = body.get("ack", 0)
        stats["resultsAcked"] += sum(1 for e in outbox if e["seq"] <= acked and e["type"] == "action.result")
        outbox = [e for e in outbox if e["seq"] > acked]
        for e in body.get("envelopes", []):
            if e["seq"] != ack + 1:
                continue
            ack = e["seq"]
            if e["type"] != "action.dispatch":
                continue
            action = e["body"]["actionId"]
            now = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
            for kind, payload in (("action.ack", {"actionId": action}),
                                  ("action.result", {"actionId": action, "ok": True, "result": {}, "durationMs": 1})):
                seq += 1
                outbox.append({"v": 1, "id": "keepup-" + str(seq), "type": kind, "seq": seq, "ts": now, "body": payload})

started = time.monotonic()
threads = [threading.Thread(target=plugin), threading.Thread(target=dispatcher)]
for t in threads: t.start()
for t in threads: t.join()
stats["seconds"] = round(time.monotonic() - started, 1)
stats["pollsPerSecond"] = round(stats["polls"] / seconds, 1)
print(json.dumps(stats))
'
# BANIMPORT: two servers declaring the bans capability, each a plugin that
# walks the list after every bans.changed (spec section 13.4) and reports what
# it applied, beside a bans:manage token importing bans as fast as its own
# limit allows; prints one JSON summary. Arguments: hub URL, the admin token to
# set up with, how many bans to import.
IMPORT_BANS="${IMPORT_BANS:-101}"
BANIMPORT='
import http.client, json, sys, threading, time, urllib.parse
base, admin, total = sys.argv[1], sys.argv[2], int(sys.argv[3])
url = urllib.parse.urlsplit(base)
prefix = url.path.rstrip("/")
def conn():
    make = http.client.HTTPSConnection if url.scheme == "https" else http.client.HTTPConnection
    return make(url.hostname, url.port, timeout=30)
def call(c, method, path, token, body=None):
    headers = {"Content-Type": "application/json"}
    if token:
        headers["Authorization"] = "Bearer " + token
    c.request(method, prefix + path, None if body is None else json.dumps(body), headers)
    r = c.getresponse()
    data = r.read()
    return r.status, r.getheader("Retry-After"), (json.loads(data) if data else {})
def patient(c, method, path, token, body=None):
    while True:
        s, ra, b = call(c, method, path, token, body)
        if s != 429:
            return s, b
        time.sleep(int(ra or 1))
stats = {"imported": 0, "editorRefused": 0, "walks": 0, "pages": 0, "polls": 0, "pluginsRefused": 0}
lock = threading.Lock()
stop = threading.Event()
c = conn()
plugins = []
for i in range(2):
    _, created = patient(c, "POST", "/api/v1/servers", admin, {"name": "Rate limit demo: bans " + str(i), "game": "dayz"})
    _, enrolled = patient(c, "POST", "/plugin/v1/enroll", "", {"enrollmentToken": created["enrollment"]["token"], "game": "dayz"})
    _, session = patient(c, "POST", "/plugin/v1/session", "", {"serverId": enrolled["serverId"], "serverSecret": enrolled["serverSecret"], "pollTimeoutSeconds": 5})
    plugins.append(session["sessionToken"])
_, minted = patient(c, "POST", "/api/v1/tokens", admin, {"name": "demo ban importer", "scopes": ["bans:manage"]})
stats["tokenId"] = minted["token"]["id"]
def plugin(token, label):
    c = conn()
    seq, ack = 1, 0
    outbox = [{"v": 1, "id": label + "-manifest", "type": "manifest.publish", "seq": 1, "ts": "2026-11-11T12:00:00Z",
               "body": {"manifestRevision": 1, "capabilities": ["bans"], "actions": [], "contexts": [], "events": []}}]
    def request(method, path, body=None):
        while True:
            s, ra, b = call(c, method, path, token, body)
            if s != 429:
                return s, b
            with lock:
                stats["pluginsRefused"] += 1
            time.sleep(int(ra or 1))
    while not stop.is_set():
        _, b = request("POST", "/plugin/v1/poll", {"ack": ack, "envelopes": outbox})
        with lock:
            stats["polls"] += 1
        acked = b.get("ack", 0)
        outbox = [e for e in outbox if e["seq"] > acked]
        changed = False
        for e in b.get("envelopes", []):
            if e["seq"] != ack + 1:
                continue
            ack = e["seq"]
            changed = changed or e["type"] == "bans.changed"
        if not changed:
            continue
        cursor, revision = "", None
        while True:
            s, page = request("GET", "/plugin/v1/bans?limit=100" + ("&cursor=" + urllib.parse.quote(cursor) if cursor else ""))
            with lock:
                stats["pages"] += 1
            if s != 200:
                break
            revision, cursor = page.get("revision"), page.get("nextCursor") or ""
            if not cursor:
                break
        with lock:
            stats["walks"] += 1
        seq += 1
        outbox.append({"v": 1, "id": label + "-applied-" + str(seq), "type": "bans.applied", "seq": seq,
                       "ts": "2026-11-11T12:00:00Z", "body": {"revision": revision}})
threads = [threading.Thread(target=plugin, args=(token, "demo-rl-bans-" + str(i))) for i, token in enumerate(plugins)]
for t in threads: t.start()
time.sleep(2)
editor, started, stamp = conn(), time.monotonic(), str(int(time.time()))
while stats["imported"] < total:
    s, ra, _ = call(editor, "POST", "/api/v1/bans", minted["secret"],
                    {"player": {"platform": "steam", "id": "7656119" + stamp[-6:] + str(stats["imported"]).zfill(4)}, "reason": "demo import"})
    if s == 429:
        stats["editorRefused"] += 1
        time.sleep(int(ra or 1))
        continue
    stats["imported"] += 1
stats["importSeconds"] = round(time.monotonic() - started, 1)
time.sleep(6)
stop.set()
for t in threads: t.join(timeout=15)
print(json.dumps(stats))
'
flood() {
  # Git Bash rewrites an argument that looks like a POSIX path for a native
  # program; the request path must reach python as it is.
  MSYS_NO_PATHCONV=1 MSYS2_ARG_CONV_EXCL='*' \
    python3 -c "$FLOODER" "$HUB_URL" "$2" "$1" "$FLOOD_SECONDS" "$FLOOD_WORKERS" >> "$3" || true
}
tally() { awk '{ n[$1]++ } END { for (c in n) printf "%s x%d  ", c, n[c]; print "" }' "$1"; }
count_status() { grep -c "^$2\$" "$1" || true; }

scrape_limited() {
  [ -n "$METRICS_URL" ] || { echo "-"; return; }
  curl --silent "$METRICS_URL/metrics" | awk -v k="vyshka_rate_limited_total{api=\"$1\"}" \
    'index($0, k " ") == 1 { print $NF; f = 1 } END { if (!f) print 0 }'
}

step "Setup: two servers with live sessions, and two admin tokens"
read -r A_ID A_TOKEN <<< "$(enroll "Rate limit demo: flooder")"
read -r B_ID B_TOKEN <<< "$(enroll "Rate limit demo: bystander")"
NS="demo-rate-limits-$(date +%s)"
t1=$(api POST /api/v1/tokens "$ADMIN_TOKEN" "$(jq -nc --arg ns "kv:rw:$NS" '{name:"demo flooder",scopes:["servers:read",$ns]}')")
t2=$(api POST /api/v1/tokens "$ADMIN_TOKEN" '{"name":"demo bystander","scopes":["servers:read"]}')
T1_ID=$(echo "$t1" | jq -r '.token.id'); T1=$(echo "$t1" | jq -r '.secret')
T2_ID=$(echo "$t2" | jq -r '.token.id'); T2=$(echo "$t2" | jq -r '.secret')
echo "  server A (will flood)  $A_ID"
echo "  server B (bystander)   $B_ID"
echo "  token T1 (will flood)  $T1_ID   servers:read kv:rw:$NS"
echo "  token T2 (bystander)   $T2_ID   servers:read"
plugin_before=$(scrape_limited plugin); admin_before=$(scrape_limited admin)

# --- server B's health, measured the same way before and during the flood ---
B_SEQ=0
B_ACK=0
# take_delivered FILE: B acks whatever the hub delivered in that answer.
take_delivered() {
  local top
  top=$(jq -r '[.envelopes[]?.seq] | max // empty' "$1" 2>/dev/null || true)
  if [ -n "$top" ] && [ "$top" -gt "$B_ACK" ]; then B_ACK=$top; fi
}
# b_poll: one poll from B carrying a fresh event, said with more:true so the hub
# answers at once instead of holding; prints "status milliseconds".
b_poll() {
  B_SEQ=$((B_SEQ + 1))
  local body result
  body=$(jq -nc --argjson seq "$B_SEQ" --argjson ack "$B_ACK" --arg now "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    '{ack:$ack,more:true,envelopes:[{v:1,id:("demo-rl-b-" + ($seq|tostring)),type:"event.batch",seq:$seq,ts:$now,
      body:{events:[{t:"demo.rate-limits.heartbeat",ts:$now,data:{seq:$seq}}]}}]}')
  result=$(curl --silent -o "$WORK_DIR/b.json" --write-out '%{http_code} %{time_total}' -X POST \
    -H "Authorization: Bearer $B_TOKEN" -H 'Content-Type: application/json' -d "$body" "$HUB_URL/plugin/v1/poll" || true)
  take_delivered "$WORK_DIR/b.json"
  echo "$result" | awk '{ printf "%s %d\n", $1, $2 * 1000 }'
}
# b_delivery: B parks a poll, the hub is given an envelope for it, and the
# held poll answers; prints "status milliseconds" from queueing to delivery.
b_delivery() {
  local body t0 t1 status
  body=$(jq -nc --argjson ack "$B_ACK" '{ack:$ack}')
  curl --silent -o "$WORK_DIR/held.json" --write-out '%{http_code}' -X POST \
    -H "Authorization: Bearer $B_TOKEN" -H 'Content-Type: application/json' -d "$body" \
    "$HUB_URL/plugin/v1/poll" > "$WORK_DIR/held.status" &
  local held=$!
  sleep 0.5
  t0=$(now_ms)
  api POST "/api/v1/servers/$B_ID/envelopes" "$ADMIN_TOKEN" '{"type":"demo-rate-limits.ping","body":{}}' > /dev/null
  wait "$held" || true
  t1=$(now_ms)
  status=$(cat "$WORK_DIR/held.status")
  take_delivered "$WORK_DIR/held.json"
  echo "$status $((t1 - t0))"
}
# measure LABEL: five polls and three deliveries for B, into $WORK_DIR/LABEL.*
measure() {
  : > "$WORK_DIR/$1.poll"; : > "$WORK_DIR/$1.delivery"
  for _ in 1 2 3 4 5; do b_poll >> "$WORK_DIR/$1.poll"; sleep 0.2; done
  for _ in 1 2 3; do b_delivery >> "$WORK_DIR/$1.delivery"; done
}
report_b() {
  printf '  %-7s B polls      %s   median %s ms, max %s ms\n' "$1" "$(awk '{printf "%s/%sms ", $1, $2}' "$WORK_DIR/$1.poll")" \
    "$(awk '{print $2}' "$WORK_DIR/$1.poll" | median)" "$(awk '{print $2}' "$WORK_DIR/$1.poll" | maxof)"
  printf '  %-7s B deliveries %s   median %s ms, max %s ms\n' "$1" "$(awk '{printf "%s/%sms ", $1, $2}' "$WORK_DIR/$1.delivery")" \
    "$(awk '{print $2}' "$WORK_DIR/$1.delivery" | median)" "$(awk '{print $2}' "$WORK_DIR/$1.delivery" | maxof)"
}

step "1. Server B before any flood: polls answered at once, a queued envelope delivered to its held poll"
measure before
report_b before

step "2. Server A floods the Plugin API for ${FLOOD_SECONDS} s (16 requests in flight, session reads), while B goes on"
flood "$A_TOKEN" /plugin/v1/session "$WORK_DIR/flood-a" &
FLOOD_PIDS+=($!)
sleep 1
# A's own envelope, sent mid-flood: one event in a poll, refused as a whole.
# The flood holds A's bucket empty, but a probe can still win a token the
# moment one refills (more likely on a slow runner); an admitted probe is an
# ordinary delivery, so the next probe carries the next seq, until one is
# refused. Every probe must end up stored exactly once.
PROBE_SEQ=0
probe_status=""
plugin_seen_429=0
for _ in $(seq 1 10); do
  PROBE_SEQ=$((PROBE_SEQ + 1))
  PROBE=$(jq -nc --argjson seq "$PROBE_SEQ" --arg now "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    '{envelopes:[{v:1,id:("demo-rl-a-" + ($seq|tostring)),type:"event.batch",seq:$seq,ts:$now,
      body:{events:[{t:"demo.rate-limits.note",ts:$now,data:{seq:$seq}}]}}]}')
  probe_status=$(curl --silent -D "$WORK_DIR/probe.headers" -o "$WORK_DIR/probe.json" --write-out '%{http_code}' -X POST \
    -H "Authorization: Bearer $A_TOKEN" -H 'Content-Type: application/json' -d "$PROBE" "$HUB_URL/plugin/v1/poll" || true)
  [ "$probe_status" = 429 ] && break
  echo "  A's poll carrying event seq $PROBE_SEQ won a refilled token and was admitted ($probe_status); trying the next seq"
done
[ "$probe_status" = 429 ] && plugin_seen_429=$((plugin_seen_429 + 1))
retry_after=$(awk 'tolower($1) == "retry-after:" { gsub(/\r/, "", $2); print $2 }' "$WORK_DIR/probe.headers")
echo "  A's poll carrying event seq $PROBE_SEQ, mid-flood -> $probe_status, Retry-After: ${retry_after:-none}"
jq -c '.error // .' "$WORK_DIR/probe.json" | sed 's/^/    /'
measure during
wait "${FLOOD_PIDS[@]}" 2>/dev/null || true
FLOOD_PIDS=()
echo "  A's flood: $(wc -l < "$WORK_DIR/flood-a") requests; $(tally "$WORK_DIR/flood-a")"
report_b before
report_b during

flood_429=$(count_status "$WORK_DIR/flood-a" 429)
[ "$flood_429" -gt 0 ] && pass "A's flood was refused with 429 ($flood_429 times)" || fail "A's flood was never refused: is the Plugin API limit off?"
[ "$probe_status" = 429 ] && [ "$(jq -r '.error.code' "$WORK_DIR/probe.json")" = rate_limited ] && [ -n "$retry_after" ] \
  && [ "$(jq -r '.error.details.retryAfterSeconds' "$WORK_DIR/probe.json")" = "$retry_after" ] \
  && pass "A's mid-flood poll: 429 rate_limited, Retry-After $retry_after, details.retryAfterSeconds $retry_after" \
  || fail "A's mid-flood poll was not a well-formed rate_limited refusal (status $probe_status)"
if awk '{ if ($1 != 200) bad = 1 } END { exit bad }' "$WORK_DIR/during.poll" "$WORK_DIR/during.delivery"; then
  pass "every request B made during the flood was answered 200"
else
  fail "B was refused or failed during A's flood"
fi
during_max=$(cat "$WORK_DIR/during.poll" "$WORK_DIR/during.delivery" | awk '{print $2}' | maxof)
[ "$during_max" -lt 3000 ] && pass "B's slowest request during the flood took $during_max ms (a sanity bound of 3000 ms: on loopback B is healthy with or without the limiter, so this guards against a hub stalled by the flood, nothing finer)" \
  || fail "B's slowest request during the flood took $during_max ms"

step "3. A backs off as told and sends the refused poll again: applied once, and only once"
sleep "${retry_after:-1}"
for attempt in 1 2 3 4 5; do
  status=$(curl --silent -D "$WORK_DIR/retry.headers" -o "$WORK_DIR/retry.json" --write-out '%{http_code}' -X POST \
    -H "Authorization: Bearer $A_TOKEN" -H 'Content-Type: application/json' -d "$PROBE" "$HUB_URL/plugin/v1/poll" || true)
  echo "  retransmission $attempt -> $status $(jq -c '{ack} // .' "$WORK_DIR/retry.json" 2>/dev/null)"
  [ "$status" = 429 ] || break
  plugin_seen_429=$((plugin_seen_429 + 1))
  sleep "$(awk 'tolower($1) == "retry-after:" { gsub(/\r/, "", $2); print $2 }' "$WORK_DIR/retry.headers")"
done
first_ack=$(jq -r '.ack // -1' "$WORK_DIR/retry.json")
again=$(api POST /plugin/v1/poll "$A_TOKEN" "$PROBE" | jq -r '.ack')
echo "  the same poll once more (a duplicate now) -> ack $again"
notes=$(api GET "/api/v1/servers/$A_ID/events?type=demo.rate-limits.note" "$ADMIN_TOKEN" | jq '.events | length')
echo "  GET /api/v1/servers/A/events?type=demo.rate-limits.note -> $notes event(s)"
[ "$first_ack" = "$PROBE_SEQ" ] && [ "$again" = "$PROBE_SEQ" ] && [ "$notes" = "$PROBE_SEQ" ] \
  && pass "the refused envelope was applied exactly once after the back-off (ack $PROBE_SEQ, $notes event(s) stored for $PROBE_SEQ probe(s))" \
  || fail "the refused envelope: ack $first_ack then $again, $notes event(s) stored; want $PROBE_SEQ each"

step "4. Token T1 floods the Admin API for ${FLOOD_SECONDS} s while T2 reads at its own pace"
t2_read() { curl --silent -o /dev/null --write-out '%{http_code} %{time_total}' -H "Authorization: Bearer $T2" \
  "$HUB_URL/api/v1/servers/$B_ID" | awk '{ printf "%s %d\n", $1, $2 * 1000 }' || true; }
: > "$WORK_DIR/t2.before"; for _ in 1 2 3 4 5; do t2_read >> "$WORK_DIR/t2.before"; sleep 0.2; done
flood "$T1" "/api/v1/servers/$A_ID" "$WORK_DIR/flood-t1" &
FLOOD_PIDS+=($!)
sleep 1
# As with A's probe: a write that wins a refilled token is simply written, and
# the next key is tried, until one is refused.
admin_seen_429=0
for k in $(seq 1 10); do
  KV_KEY="greeting-$k"
  kv_status=$(curl --silent -D "$WORK_DIR/kv.headers" -o "$WORK_DIR/kv.json" --write-out '%{http_code}' -X PUT \
    -H "Authorization: Bearer $T1" -H 'Content-Type: application/json' -d '{"value":"written once"}' \
    "$HUB_URL/api/v1/kv/$NS/$KV_KEY" || true)
  [ "$kv_status" = 429 ] && break
  echo "  T1's write of $KV_KEY won a refilled token and was written ($kv_status); trying the next key"
done
[ "$kv_status" = 429 ] && admin_seen_429=$((admin_seen_429 + 1))
kv_retry=$(awk 'tolower($1) == "retry-after:" { gsub(/\r/, "", $2); print $2 }' "$WORK_DIR/kv.headers")
echo "  T1's write of $KV_KEY, mid-flood -> $kv_status, Retry-After: ${kv_retry:-none}"
kv_absent=$(curl --silent -o /dev/null --write-out '%{http_code}' -H "Authorization: Bearer $ADMIN_TOKEN" "$HUB_URL/api/v1/kv/$NS/$KV_KEY" || true)
echo "  the key, read with another token straight after -> $kv_absent"
# More writes while the flood goes on, to show that a run of refused writes
# collapses in the audit log instead of writing a record each.
burst_codes=""
for _ in $(seq 1 40); do
  burst_codes+=$(curl --silent -o /dev/null --write-out '%{http_code} ' -X PUT -H "Authorization: Bearer $T1" \
    -H 'Content-Type: application/json' -d '{"value":"burst"}' "$HUB_URL/api/v1/kv/$NS/burst" || true)
done
burst_429=$(echo "$burst_codes" | tr ' ' '\n' | grep -c '^429$' || true)
echo "  40 more writes from T1, mid-flood -> $(echo "$burst_codes" | tr ' ' '\n' | awk 'NF { n[$1]++ } END { for (c in n) printf "%s x%d  ", c, n[c] }')"
: > "$WORK_DIR/t2.during"; for _ in $(seq 1 10); do t2_read >> "$WORK_DIR/t2.during"; sleep 0.2; done
wait "${FLOOD_PIDS[@]}" 2>/dev/null || true
FLOOD_PIDS=()
echo "  T1's flood: $(wc -l < "$WORK_DIR/flood-t1") requests; $(tally "$WORK_DIR/flood-t1")"
printf '  before  T2 reads %s  median %s ms\n' "$(awk '{printf "%s/%sms ", $1, $2}' "$WORK_DIR/t2.before")" "$(awk '{print $2}' "$WORK_DIR/t2.before" | median)"
printf '  during  T2 reads %s  median %s ms, max %s ms\n' "$(awk '{printf "%s/%sms ", $1, $2}' "$WORK_DIR/t2.during")" \
  "$(awk '{print $2}' "$WORK_DIR/t2.during" | median)" "$(awk '{print $2}' "$WORK_DIR/t2.during" | maxof)"

t1_429=$(count_status "$WORK_DIR/flood-t1" 429)
[ "$t1_429" -gt 0 ] && pass "T1's flood was refused with 429 ($t1_429 times)" || fail "T1's flood was never refused: is the Admin API limit off?"
[ "$kv_status" = 429 ] && [ "$kv_absent" = 404 ] && pass "T1's mid-flood write was refused and not applied (the key does not exist)" \
  || fail "T1's mid-flood write: status $kv_status, key read $kv_absent; want 429 and 404"
awk '{ if ($1 != 200) bad = 1 } END { exit bad }' "$WORK_DIR/t2.during" && pass "every read T2 made during T1's flood was answered 200" \
  || fail "T2 was refused or failed during T1's flood"

step "5. T1 backs off and writes again; the audit log holds one record for its run of refused writes"
sleep "${kv_retry:-1}"
written=$(api PUT "/api/v1/kv/$NS/$KV_KEY" "$T1" '{"value":"written once"}' | jq -c '{revision}')
echo "  T1's write after Retry-After -> $written"
audit=$(api GET "/api/v1/audit?tokenId=$T1_ID" "$ADMIN_TOKEN")
echo "  GET /api/v1/audit?tokenId=T1 (newest first):"
echo "$audit" | jq -c '.records[] | {method, path, status, detail}' | sed 's/^/    /'
recorded_429=$(echo "$audit" | jq '[.records[] | select(.status == 429)] | length')
echo "  (a run going on past a minute adds one record a minute, whose detail.unrecordedRefusals"
echo "   counts the refusals left out since the record before it)"
stored=$(api GET "/api/v1/kv/$NS/$KV_KEY" "$ADMIN_TOKEN" | jq -r '.revision')
[ "$stored" = 1 ] && pass "the refused write was applied exactly once after the back-off (revision 1)" \
  || fail "the key's revision is $stored, want 1"
if [ "$recorded_429" -ge 1 ] && [ "$recorded_429" -lt "$((burst_429 + 1))" ]; then
  pass "the audit log holds $recorded_429 record(s) for T1's $((burst_429 + 1)) rate-limited writes: the first is recorded, the run collapses"
else
  fail "the audit log holds $recorded_429 record(s) for T1's $((burst_429 + 1)) rate-limited writes; want the first, and fewer than all"
fi

step "6. An admin dispatches at its full allowed rate to server C for ${KEEPUP_SECONDS} s; C's plugin answers every delivery"
# Every dispatch ends C's held poll and C polls again at once, so C polls
# about as fast as the admin dispatches, faster than the plugin limit's
# sustained rate. A poll that delivers an admin's dispatch is not charged
# (spec section 2.2.1), so C is never refused for keeping up.
read -r C_ID C_TOKEN <<< "$(enroll "Rate limit demo: dispatched to")"
api POST /plugin/v1/poll "$C_TOKEN" "$(jq -nc '{more:true,envelopes:[{v:1,id:"demo-rl-c-manifest",type:"manifest.publish",seq:1,ts:"2026-11-11T12:00:00Z",
  body:{manifestRevision:1,actions:[{code:"rl-demo.ping",name:"Ping",context:"player",namespace:"rl-demo",danger:"none",
    params:{type:"object",properties:{}}}]}}]}')" > /dev/null
t3=$(api POST /api/v1/tokens "$ADMIN_TOKEN" '{"name":"demo dispatcher","scopes":["actions:dispatch:rl-demo.ping"]}')
T3_ID=$(echo "$t3" | jq -r '.token.id'); T3=$(echo "$t3" | jq -r '.secret')
echo "  server C $C_ID, its manifest declares rl-demo.ping; token T3 $T3_ID may dispatch it"
keepup=$(MSYS_NO_PATHCONV=1 MSYS2_ARG_CONV_EXCL='*' \
  python3 -c "$KEEPUP" "$HUB_URL" "$C_ID" "$C_TOKEN" "$T3" "$KEEPUP_SECONDS" "$DISPATCH_RATE" || echo '{}')
echo "  $keepup" | jq -c . | sed 's/^/  /'
c_polls=$(echo "$keepup" | jq -r '.polls // 0')
c_refused=$(echo "$keepup" | jq -r '.pollsRefused // -1')
c_rate=$(echo "$keepup" | jq -r '.pollsPerSecond // 0')
d_sent=$(echo "$keepup" | jq -r '.dispatched // 0')
d_refused=$(echo "$keepup" | jq -r '.dispatchesRefused // -1')
d_done=$(echo "$keepup" | jq -r '.resultsAcked // 0')
[ "$c_refused" = 0 ] && pass "C polled $c_polls times ($c_rate a second) and was never refused" \
  || fail "C's plugin was refused $c_refused time(s) while it only answered deliveries"
awk -v r="$c_rate" 'BEGIN { exit !(r > 5) }' && pass "C polled faster than the plugin limit's 5 a second, so the exemption is what kept it unrefused" \
  || fail "C polled only $c_rate a second, which does not exercise the exemption"
[ "$d_refused" = 0 ] && [ "$d_sent" -gt 0 ] && pass "T3 dispatched $d_sent actions at its full rate, none refused" \
  || fail "T3's dispatches: $d_sent sent, $d_refused refused"
[ "$d_done" = "$d_sent" ] && pass "every dispatched action's result reached the hub ($d_done of $d_sent)" \
  || fail "$d_done of $d_sent dispatched actions' results reached the hub"
step "7. A bans:manage token imports ${IMPORT_BANS} bans as fast as it is allowed; two servers declaring bans walk the list after every notice"
# Each ban sends both servers a bans.changed; the poll carrying it is not
# charged (an admin caused it), and the walk of the list that follows is the
# plugin's own requests, which are. At the defaults this size of import costs
# neither plugin a refusal (deploy/README.md, "Rate limits").
import=$(MSYS_NO_PATHCONV=1 MSYS2_ARG_CONV_EXCL='*' \
  python3 -c "$BANIMPORT" "$HUB_URL" "$ADMIN_TOKEN" "$IMPORT_BANS" || echo '{}')
T4_ID=$(echo "$import" | jq -r '.tokenId // empty')
echo "  $import" | jq -c 'del(.tokenId)' | sed 's/^/  /'
i_refused=$(echo "$import" | jq -r '.pluginsRefused // -1')
i_posted=$(echo "$import" | jq -r '.imported // 0')
i_walks=$(echo "$import" | jq -r '.walks // 0')
[ "$i_posted" = "$IMPORT_BANS" ] && [ "$i_walks" -gt 0 ] && pass "$i_posted bans imported; the plugins walked the list $i_walks times between them" \
  || fail "the import placed $i_posted of $IMPORT_BANS bans, with $i_walks walks"
[ "$i_refused" = 0 ] && pass "neither bans-capable plugin was refused while the import ran" \
  || fail "the bans-capable plugins were refused $i_refused time(s) during a $IMPORT_BANS-ban import"
admin_seen_429=$((admin_seen_429 + $(echo "$import" | jq -r '.editorRefused // 0')))
plugin_seen_429=$((plugin_seen_429 + ${i_refused#-}))

if [ -n "$METRICS_URL" ]; then
  step "8. vyshka_rate_limited_total moved by exactly the 429s this demo was answered"
  plugin_seen_429=$((plugin_seen_429 + flood_429 + ${c_refused#-}))
  admin_seen_429=$((admin_seen_429 + t1_429 + burst_429 + ${d_refused#-}))
  plugin_after=$(scrape_limited plugin); admin_after=$(scrape_limited admin)
  echo "  plugin: $plugin_before -> $plugin_after (the demo saw $plugin_seen_429)"
  echo "  admin:  $admin_before -> $admin_after (the demo saw $admin_seen_429)"
  [ "$((plugin_after - plugin_before))" = "$plugin_seen_429" ] && pass "the plugin counter moved by the $plugin_seen_429 plugin refusals the demo saw" \
    || fail "the plugin counter moved by $((plugin_after - plugin_before)), the demo saw $plugin_seen_429"
  [ "$((admin_after - admin_before))" = "$admin_seen_429" ] && pass "the admin counter moved by the $admin_seen_429 admin refusals the demo saw" \
    || fail "the admin counter moved by $((admin_after - admin_before)), the demo saw $admin_seen_429"
fi

echo
if [ "$failures" -gt 0 ]; then
  echo "$failures check(s) failed" >&2
  exit 1
fi
echo "all checks passed"
