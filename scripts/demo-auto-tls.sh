#!/usr/bin/env bash
# Demo: --auto-tls (issue #85). The hub gets a real certificate over ACME from
# Pebble, the Let's Encrypt project's test CA, run locally, and serves TLS
# itself with no proxy in front. Everything is on loopback and non-privileged
# ports; nothing talks to a public CA.
#
# It shows: issuance for the configured name; a handshake and /healthz from a
# client that trusts the test root; HTTP/1.1 even when h2 is offered; the
# plain listener answering only the ACME path and a redirect; both listeners
# capped; one ECDSA certificate whatever the client offers; a restart that
# reuses the cached certificate without a new order; a name outside the host
# policy getting no certificate and causing no order; and the failure modes
# (unwritable cache, CA unreachable at boot, validation failing, renewal
# failing near expiry while the cached certificate keeps being served).
#
# It boots its own hubs, so it ignores the e2e hub. Pebble is fetched at a
# pinned version with `go install`. When that fails (offline, no module
# proxy) the demo prints SKIP and exits 0 on a developer's machine, and FAIL
# with exit 1 under CI (the CI variable set), so a broken fetch cannot turn
# the only end-to-end TLS coverage off behind a green summary. Pebble's own
# TLS certificate is generated here with openssl for each run rather than
# taken from the pinned module, whose bundled one expires.
# Set AUTO_TLS_DEMO_KEEP=1 to keep the run directory for its logs.
#
# Needs Go, curl, openssl, and python3.
set -euo pipefail

cd "$(dirname "$0")/.."

PEBBLE_VERSION=v2.10.3
HOST=hub.vyshka.test
OTHER=intruder.vyshka.test

run_dir="$(mktemp -d "${TMPDIR:-/tmp}/vyshka-auto-tls.XXXXXX")"
exe=""
case "$(go env GOOS)" in windows) exe=.exe ;; esac
# Pebble and the hub are native programs: on Windows they need Windows paths.
native() { if command -v cygpath > /dev/null 2>&1; then cygpath -m "$1"; else printf '%s' "$1"; fi; }

pids=()
cleanup() {
  for pid in "${pids[@]}"; do kill "$pid" 2>/dev/null || true; done
  for pid in "${pids[@]}"; do wait "$pid" 2>/dev/null || true; done
  if [ "${AUTO_TLS_DEMO_KEEP:-}" = 1 ]; then echo "run directory kept: $run_dir"; else rm -rf "$run_dir"; fi
}
trap cleanup EXIT

step() { printf '\n--- %s\n' "$*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }
stop() { kill "$1" 2>/dev/null || true; wait "$1" 2>/dev/null || true; }

# wait_log FILE PATTERN SECONDS: wait for a line, failing if it never comes.
wait_log() {
  local i
  for i in $(seq 1 $(($3 * 10))); do
    grep -q -- "$2" "$1" 2>/dev/null && return 0
    sleep 0.1
  done
  echo "--- $1 (last 20 lines)" >&2; tail -n 20 "$1" >&2
  fail "no \"$2\" in $(basename "$1") within $3 s"
}
# show FILE PATTERN: print the hub log lines that match, trimmed to the
# fields that matter.
show() { { grep -- "$2" "$1" || true; } | sed -E 's/"time":"[^"]*",//; s/"level":"([A-Z]+)",/\1 /' | awk -v n="${3:-5}" 'NR <= n'; }
orders() { grep -c "Added order" "$run_dir/pebble.log" || true; }
compact() { tr -d ' \n'; }

step "Fetch Pebble $PEBBLE_VERSION and its DNS test server"
if ! GOBIN="$(native "$run_dir/bin")" GOFLAGS=-mod=mod go install \
    "github.com/letsencrypt/pebble/v2/cmd/pebble@$PEBBLE_VERSION" \
    "github.com/letsencrypt/pebble/v2/cmd/pebble-challtestsrv@$PEBBLE_VERSION" > "$run_dir/install.log" 2>&1; then
  cat "$run_dir/install.log"
  if [ -n "${CI:-}" ]; then
    echo "FAIL (CI is set): could not fetch Pebble $PEBBLE_VERSION; the auto-TLS demo needs it"
    exit 1
  fi
  echo "SKIP (CI is not set): could not fetch Pebble $PEBBLE_VERSION (offline or no module proxy); the auto-TLS demo needs it"
  exit 0
fi
echo "ok: $(ls "$run_dir/bin" | tr '\n' ' ')"

# Pebble's directory and management API serve TLS with this throwaway CA and
# leaf, made fresh each run. The hub trusts it through -auto-tls-ca-roots.
# MSYS2_ARG_CONV_EXCL keeps Git Bash from rewriting -subj as a Windows path.
minica="$run_dir/directory-ca.pem"
gen() { MSYS2_ARG_CONV_EXCL='/CN=' openssl "$@" >> "$run_dir/openssl.log" 2>&1 || { cat "$run_dir/openssl.log"; fail "openssl $1"; }; }
gen req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 2 -subj "/CN=vyshka demo directory CA" \
  -addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign,cRLSign" \
  -keyout "$(native "$run_dir/directory-ca.key")" -out "$(native "$minica")"
gen req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj "/CN=localhost" \
  -keyout "$(native "$run_dir/directory.key")" -out "$(native "$run_dir/directory.csr")"
printf 'subjectAltName=DNS:localhost,IP:127.0.0.1\nbasicConstraints=CA:FALSE\nkeyUsage=critical,digitalSignature\nextendedKeyUsage=serverAuth\n' > "$run_dir/directory.ext"
gen x509 -req -days 2 -in "$(native "$run_dir/directory.csr")" -CA "$(native "$minica")" -CAkey "$(native "$run_dir/directory-ca.key")" \
  -set_serial "0x$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')" -extfile "$(native "$run_dir/directory.ext")" -out "$(native "$run_dir/directory.pem")"
echo "Pebble's TLS certificate, made for this run: $(openssl x509 -in "$(native "$run_dir/directory.pem")" -noout -enddate)"

base=$((21000 + (RANDOM % 800) * 10))
acme_port=$base; mgmt_port=$((base + 1)); dns_port=$((base + 2)); dns_mgmt=$((base + 3))
tls_port=$((base + 4)); http_port=$((base + 5))
directory="https://127.0.0.1:$acme_port/dir"

# write_config FILE VALIDITY_SECONDS: Pebble validates TLS-ALPN-01 on tlsPort
# and HTTP-01 on httpPort, so those are the hub's two listeners.
write_config() {
  cat > "$1" <<EOF
{"pebble": {
  "listenAddress": "127.0.0.1:$acme_port",
  "managementListenAddress": "127.0.0.1:$mgmt_port",
  "certificate": "$(native "$run_dir/directory.pem")",
  "privateKey": "$(native "$run_dir/directory.key")",
  "httpPort": $http_port, "tlsPort": $tls_port,
  "ocspResponderURL": "", "externalAccountBindingRequired": false,
  "profiles": {"default": {"description": "demo", "validityPeriod": $2}}
}}
EOF
}
pebble_pid=""
start_pebble() {
  PEBBLE_VA_NOSLEEP=1 PEBBLE_WFE_NONCEREJECT=0 PEBBLE_AUTHZREUSE=0 \
    "$run_dir/bin/pebble$exe" -config "$(native "$1")" -dnsserver "127.0.0.1:$dns_port" > "$run_dir/$2" 2>&1 &
  pebble_pid=$!; pids+=("$pebble_pid")
  wait_log "$run_dir/$2" "ACME directory available" 20
}

hub_bin="$run_dir/vyshka-hub-autotls-$base$exe"
go build -o "$hub_bin" ./hub/cmd/vyshka-hub
export VYSHKA_ADMIN_TOKEN="vya_autotls_$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')"
hub_pid=""
# start_hub LOG [extra flags]: an auto-TLS hub on the ports Pebble validates.
start_hub() {
  local log="$1"; shift
  "$hub_bin" serve -db "$(native "$run_dir/hub.db")" -addr "127.0.0.1:$tls_port" \
    -auto-tls -auto-tls-hosts "$HOST" -auto-tls-directory "$directory" \
    -auto-tls-ca-roots "$(native "$minica")" "$@" > "$log" 2>&1 &
  hub_pid=$!; pids+=("$hub_pid")
}
# Windows' curl verifies with Schannel, which wants a revocation answer the
# test CA does not give; OpenSSL builds never ask.
revoke=()
curl -V 2>/dev/null | grep -qi schannel && revoke=(--ssl-no-revoke)
tls_curl() { curl --silent --show-error "${revoke[@]}" --cacert "$(native "$run_dir/root.pem")" --resolve "$HOST:$tls_port:127.0.0.1" "$@"; }

step "Start the DNS test server (every name -> 127.0.0.1) and Pebble"
"$run_dir/bin/pebble-challtestsrv$exe" -defaultIPv4 127.0.0.1 -defaultIPv6 "" \
  -dnsserver "127.0.0.1:$dns_port" -management "127.0.0.1:$dns_mgmt" \
  -http01 "" -https01 "" -tlsalpn01 "" -doh "" > "$run_dir/challtestsrv.log" 2>&1 &
pids+=($!)
write_config "$run_dir/pebble.json" 7776000
start_pebble "$run_dir/pebble.json" pebble.log
echo "Pebble directory $directory, DNS on 127.0.0.1:$dns_port"

step "Boot a hub with -auto-tls -auto-tls-hosts $HOST on 127.0.0.1:$tls_port (plain listener 127.0.0.1:$http_port)"
start_hub "$run_dir/hub1.log" -auto-tls-http-addr "127.0.0.1:$http_port"
wait_log "$run_dir/hub1.log" '"tls certificate ready"' 60
show "$run_dir/hub1.log" 'auto-tls enabled\|hub listening\|plain listener\|tls certificate ready'
echo "Pebble: $(grep "Issued certificate" "$run_dir/pebble.log" | tail -n 1 | sed 's/^Pebble [0-9/ :]*//')"
echo "orders so far: $(orders)"
[ "$(orders)" = 1 ] || fail "expected exactly one order"

step "Fetch the test CA's root and handshake as a client that trusts only it"
curl --silent --show-error --fail "${revoke[@]}" --cacert "$(native "$minica")" "https://127.0.0.1:$mgmt_port/roots/0" > "$run_dir/root.pem"
echo "/healthz over TLS: $(tls_curl --fail "https://$HOST:$tls_port/healthz" | compact)"
echo "openssl s_client offering h2,http/1.1:"
echo | openssl s_client -connect "127.0.0.1:$tls_port" -servername "$HOST" -CAfile "$(native "$run_dir/root.pem")" \
  -alpn h2,http/1.1 2>/dev/null | grep -E "^issuer=|ALPN protocol|^New,|Verify return code" | sed 's/^ */  /' | awk '!seen[$0]++'
alpn="$(echo | openssl s_client -connect "127.0.0.1:$tls_port" -servername "$HOST" -alpn h2,http/1.1 2>/dev/null | grep "ALPN protocol" || true)"
case "$alpn" in *http/1.1*) ;; *) fail "expected ALPN http/1.1 when h2 is offered, got: $alpn" ;; esac
if curl -V 2>/dev/null | grep -q HTTP2; then
  echo "curl offering h2: $(tls_curl --http2 -o /dev/null -w 'HTTP/%{http_version} %{http_code}' "https://$HOST:$tls_port/healthz")"
fi
echo "the panel over TLS: $(tls_curl -L -o /dev/null -w '%{http_code} %{content_type} (%{url_effective})' "https://$HOST:$tls_port/")"
echo "Admin API over TLS: $(tls_curl -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $VYSHKA_ADMIN_TOKEN" "https://$HOST:$tls_port/api/v1/servers")"

step "The plain listener: ACME challenge path and a redirect, never the APIs"
echo "GET  /healthz Host $HOST: $(curl -s -o /dev/null -w '%{http_code} -> %{redirect_url}' -H "Host: $HOST" "http://127.0.0.1:$http_port/healthz")"
echo "POST /plugin/v1/poll:      $(curl -s -w ' (%{http_code})' -X POST -H "Host: $HOST" -d '{}' "http://127.0.0.1:$http_port/plugin/v1/poll")"
echo "GET  /api/v1/servers:      $(curl -s -o /dev/null -w '%{http_code} -> %{redirect_url}' -H "Host: $HOST" "http://127.0.0.1:$http_port/api/v1/servers")"
echo "GET  Host $OTHER:  $(curl -s -w ' (%{http_code})' -H "Host: $OTHER" "http://127.0.0.1:$http_port/healthz")"
echo "GET  unknown challenge:    $(curl -s -w ' (%{http_code})' -H "Host: $HOST" "http://127.0.0.1:$http_port/.well-known/acme-challenge/nope")"
long="$(curl -s -w ' (%{http_code})' -H "Host: $HOST" "http://127.0.0.1:$http_port/.well-known/acme-challenge/$(printf 'A%.0s' $(seq 1 300))")"
echo "GET  300-char challenge:   $long"
case "$long" in *"(404)") ;; *) fail "an over-long challenge token was not answered 404" ;; esac
case "$long" in *acme-cache*|*"file name"*) fail "an over-long challenge token's answer carries a filesystem error" ;; esac
code="$(curl -s -o /dev/null -w '%{http_code} -> %{redirect_url}' --request-target 'x:@evil.example/p' -H "Host: $HOST" "http://127.0.0.1:$http_port/")"
echo "GET  x:@evil.example/p:    $code"
case "$code" in 400*) ;; *) fail "a non-path request target was answered with $code" ;; esac

step "Both listeners are capped: 80 idle connections on the plain listener (cap 64), and TLS still answers"
python3 - "$http_port" 80 15 > "$run_dir/idle.log" 2>&1 <<'PY' &
import socket, sys, time
port, count, hold = int(sys.argv[1]), int(sys.argv[2]), float(sys.argv[3])
socks = [socket.create_connection(("127.0.0.1", port)) for _ in range(count)]
print("opened", len(socks), flush=True)
time.sleep(hold)
PY
idle_pid=$!; pids+=("$idle_pid")
wait_log "$run_dir/idle.log" "opened" 20
echo "  $(cat "$run_dir/idle.log")"
plain="$(curl -s -o /dev/null --max-time 3 -w '%{http_code}' -H "Host: $HOST" "http://127.0.0.1:$http_port/healthz" || true)"
echo "  plain listener meanwhile: ${plain} (000 = not accepted within 3 s: the cap is full)"
over_tls="$(tls_curl -o /dev/null --max-time 5 -w '%{http_code}' "https://$HOST:$tls_port/healthz" || true)"
echo "  TLS listener meanwhile:   $over_tls"
[ "$plain" = 000 ] || fail "the plain listener accepted a connection past its cap"
[ "$over_tls" = 200 ] || fail "the TLS listener stopped answering while the plain listener's cap was full"
stop "$idle_pid"
after="$(curl -s -o /dev/null --max-time 5 -w '%{http_code}' -H "Host: $HOST" "http://127.0.0.1:$http_port/healthz" || true)"
echo "  plain listener after the idle connections close: $after"
[ "$after" = 302 ] || fail "the plain listener did not release its slots when the idle connections closed"

step "One certificate per name, whatever the client offers"
before=$(orders)
for flag in -tls1_3 -tls1_2; do
  key="$(echo | openssl s_client -connect "127.0.0.1:$tls_port" -servername "$HOST" "$flag" 2>/dev/null | openssl x509 -noout -text 2>/dev/null \
    | grep -m 1 "Public Key Algorithm" | sed 's/^ *//')"
  echo "  openssl s_client $flag: $key"
  case "$key" in *id-ecPublicKey*) ;; *) fail "$flag got $key" ;; esac
done
sleep 1
echo "  orders before $before, after $(orders); cache: $(ls "$run_dir/acme-cache" | tr '\n' ' ')"
[ "$(orders)" = "$before" ] || fail "a client's hello caused an order"

step "A name outside the host policy: no certificate, no order"
before=$(orders)
out="$(echo | openssl s_client -connect "127.0.0.1:$tls_port" -servername "$OTHER" 2>&1 || true)"
echo "$out" | grep -E -m 2 "alert|errno|error" | sed 's/^/  /'
echo "$out" | grep -q "BEGIN CERTIFICATE" && fail "the hub served a certificate for $OTHER"
sleep 1
echo "hub log: $(show "$run_dir/hub1.log" "TLS handshake error" 1 | sed -E 's/.*"msg":"//; s/"}$//')"
echo "orders before $before, after $(orders)"
[ "$(orders)" = "$before" ] || fail "a handshake for $OTHER caused an order"

step "Restart the hub: the cached certificate is reused, no new order"
stop "$hub_pid"
before=$(orders)
ls -l "$run_dir/acme-cache" | sed 's/^/  /'
start_hub "$run_dir/hub2.log" -auto-tls-http-addr "127.0.0.1:$http_port"
wait_log "$run_dir/hub2.log" '"tls certificate ready"' 30
show "$run_dir/hub2.log" 'tls certificate ready'
echo "/healthz over TLS: $(tls_curl --fail "https://$HOST:$tls_port/healthz" | compact)"
echo "orders before $before, after $(orders)"
[ "$(orders)" = "$before" ] || fail "the restart placed a new order"

step "scripts/demo-poll.sh over TLS: the 25 s hold, the flush mid-hold, and the ack"
# The poll demo runs unchanged; its curl trusts the test root and resolves
# the name through a .curlrc (_curlrc is the name some Windows builds read).
mkdir -p "$run_dir/curlhome"
{
  echo "cacert = \"$(native "$run_dir/root.pem")\""
  echo "resolve = $HOST:$tls_port:127.0.0.1"
  if [ ${#revoke[@]} -gt 0 ]; then echo "ssl-no-revoke"; fi
} > "$run_dir/curlhome/.curlrc"
cp "$run_dir/curlhome/.curlrc" "$run_dir/curlhome/_curlrc"
if ! CURL_HOME="$(native "$run_dir/curlhome")" bash scripts/demo-poll.sh "https://$HOST:$tls_port" > "$run_dir/demo-poll.out" 2>&1; then
  cat "$run_dir/demo-poll.out"
  fail "demo-poll.sh failed over TLS"
fi
sed 's/^/  /' "$run_dir/demo-poll.out"

# SIGTERM cannot reach a native Windows process from this shell (kill there
# terminates it outright), so the drain is compared where it can be: on
# Linux, which is where CI runs this. AUTO_TLS_DEMO_DRAIN=1 forces it.
if [ "$(go env GOOS)" != windows ] || [ "${AUTO_TLS_DEMO_DRAIN:-}" = 1 ]; then
  step "SIGTERM with a poll held: the TLS hub drains exactly as a plain one does"
  # drain_probe NAME PID URL: enroll a server, hold a 25 s poll, SIGTERM the
  # hub a second in, and report how the hub and the poll ended.
  drain_probe() {
    local name="$1" pid="$2" url="$3" created enrolled session token rc=0 t0 t1
    created=$(tls_curl --fail -X POST "$url/api/v1/servers" -H "Authorization: Bearer $VYSHKA_ADMIN_TOKEN" \
      -H 'Content-Type: application/json' -d '{"name":"Drain demo","game":"dayz"}')
    enrolled=$(tls_curl --fail -X POST "$url/plugin/v1/enroll" -H 'Content-Type: application/json' \
      -d "$(jq -nc --arg t "$(echo "$created" | jq -r '.enrollment.token')" \
        '{enrollmentToken:$t,game:"dayz",plugin:{name:"drain",version:"0.1.0"},transports:["poll"]}')")
    session=$(tls_curl --fail -X POST "$url/plugin/v1/session" -H 'Content-Type: application/json' \
      -d "$(jq -nc --arg id "$(echo "$created" | jq -r '.server.id')" --arg s "$(echo "$enrolled" | jq -r '.serverSecret')" \
        '{serverId:$id,serverSecret:$s,pollTimeoutSeconds:25,plugin:{name:"drain",version:"0.1.0"},transports:["poll"]}')")
    token=$(echo "$session" | jq -r '.sessionToken')
    ( tls_curl --max-time 40 -o /dev/null -w '%{http_code} after %{time_total} s' -X POST "$url/plugin/v1/poll" \
        -H "Authorization: Bearer $token" -H 'Content-Type: application/json' -d '{"ack":0}' \
        > "$run_dir/held-$name.out" 2>&1 || true ) &
    local poll_pid=$!
    sleep 1
    t0=$(date +%s)
    kill -TERM "$pid"
    wait "$pid" || rc=$?
    t1=$(date +%s)
    wait "$poll_pid" || true
    printf '  %-5s hub exit %s after %s s; held poll: %s\n' "$name" "$rc" $((t1 - t0)) "$(cat "$run_dir/held-$name.out")"
  }
  "$hub_bin" serve -db "$(native "$run_dir/hub-plain.db")" -addr "127.0.0.1:$((base + 8))" > "$run_dir/hub-plain.log" 2>&1 &
  plain_pid=$!; pids+=("$plain_pid")
  wait_log "$run_dir/hub-plain.log" '"hub listening"' 20
  drain_probe plain "$plain_pid" "http://127.0.0.1:$((base + 8))"
  drain_probe tls "$hub_pid" "https://$HOST:$tls_port"
  show "$run_dir/hub2.log" 'shutting down\|graceful' 2
else
  stop "$hub_pid"
fi

step "Failure: the cache directory cannot be created or written"
: > "$run_dir/not-a-dir"
if "$hub_bin" serve -db "$(native "$run_dir/hub.db")" -addr "127.0.0.1:$tls_port" -auto-tls -auto-tls-hosts "$HOST" \
    -auto-tls-cache "$(native "$run_dir/not-a-dir")/cache" > "$run_dir/hub-cache.log" 2>&1; then
  fail "the hub started with an unusable cache"
fi
echo "exit nonzero: $(tail -n 1 "$run_dir/hub-cache.log")"

step "Failure: incoherent flags are refused before anything starts"
for args in "-auto-tls" "-auto-tls-hosts $HOST" "-auto-tls -auto-tls-hosts *.vyshka.test" \
    "-auto-tls -auto-tls-hosts $HOST -db postgres://u@db/vyshka"; do
  # shellcheck disable=SC2086
  if "$hub_bin" serve $args > "$run_dir/flags.log" 2>&1; then fail "serve $args started"; fi
  printf '  serve %s\n    %s\n' "$args" "$(tail -n 1 "$run_dir/flags.log")"
done

step "Failure: the CA is unreachable at boot (the hub serves on, retries, and logs it)"
rm -rf "$run_dir/cache-unreachable"
"$hub_bin" serve -db "$(native "$run_dir/hub-unreachable.db")" -addr "127.0.0.1:$tls_port" -auto-tls -auto-tls-hosts "$HOST" \
  -auto-tls-http-addr off -auto-tls-directory "https://127.0.0.1:$((base + 7))/dir" \
  -auto-tls-cache "$(native "$run_dir/cache-unreachable")" > "$run_dir/hub-unreachable.log" 2>&1 &
hub_pid=$!; pids+=("$hub_pid")
wait_log "$run_dir/hub-unreachable.log" '"tls certificate unavailable' 60
show "$run_dir/hub-unreachable.log" 'tls certificate unavailable' 1
kill -0 "$hub_pid" 2>/dev/null || fail "the hub exited"
echo "still running; a handshake meanwhile: $(tls_curl -o /dev/null -w '%{http_code}' "https://$HOST:$tls_port/healthz" 2>&1 | head -c 120)"
stop "$hub_pid"

step "Failure: validation fails (the hub answers neither challenge where the CA looks)"
before=$(orders)
rm -rf "$run_dir/cache-invalid"
"$hub_bin" serve -db "$(native "$run_dir/hub-invalid.db")" -addr "127.0.0.1:$((base + 6))" -auto-tls -auto-tls-hosts "$HOST" \
  -auto-tls-http-addr off -auto-tls-directory "$directory" -auto-tls-ca-roots "$(native "$minica")" \
  -auto-tls-cache "$(native "$run_dir/cache-invalid")" > "$run_dir/hub-invalid.log" 2>&1 &
hub_pid=$!; pids+=("$hub_pid")
wait_log "$run_dir/hub-invalid.log" '"tls certificate unavailable' 60
show "$run_dir/hub-invalid.log" 'tls certificate unavailable' 1
echo "Pebble: $(grep -m 1 "set INVALID" "$run_dir/pebble.log" | sed 's/^Pebble [0-9/ :]*//')"
stop "$hub_pid"

step "Failure: renewal fails near expiry (short-lived certificate, then the CA goes away)"
stop "$pebble_pid"
write_config "$run_dir/pebble-short.json" 120
start_pebble "$run_dir/pebble-short.json" pebble-short.log
curl --silent --show-error --fail "${revoke[@]}" --cacert "$(native "$minica")" "https://127.0.0.1:$mgmt_port/roots/0" > "$run_dir/root.pem"
rm -rf "$run_dir/cache-short"
"$hub_bin" serve -db "$(native "$run_dir/hub-short.db")" -addr "127.0.0.1:$tls_port" -auto-tls -auto-tls-hosts "$HOST" \
  -auto-tls-http-addr "127.0.0.1:$http_port" -auto-tls-directory "$directory" -auto-tls-ca-roots "$(native "$minica")" \
  -auto-tls-cache "$(native "$run_dir/cache-short")" > "$run_dir/hub-short.log" 2>&1 &
hub_pid=$!; pids+=("$hub_pid")
wait_log "$run_dir/hub-short.log" '"tls certificate ready"' 60
show "$run_dir/hub-short.log" 'tls certificate ready' 1
echo "stopping Pebble so renewal cannot succeed"
stop "$pebble_pid"
wait_log "$run_dir/hub-short.log" 'renewal overdue' 130
show "$run_dir/hub-short.log" 'renewal overdue' 1
echo "/healthz now, with the CA gone: $(tls_curl --fail "https://$HOST:$tls_port/healthz" | compact)"
served="$(echo | openssl s_client -connect "127.0.0.1:$tls_port" -servername "$HOST" 2>/dev/null | openssl x509 -noout -serial -enddate | tr '\n' ' ')"
echo "served certificate: $served"
stop "$hub_pid"

echo
echo "auto-TLS demo passed"
