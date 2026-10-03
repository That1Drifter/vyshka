#!/usr/bin/env bash
# Shows the hub working end to end: builds it, boots it on a scratch port with
# a throwaway SQLite database, runs the hub conformance suite and then every
# scripts/demo-*.sh against it, and stops it. Exits nonzero if anything failed.
#
#   scripts/e2e.sh                       # the suite and every demo
#   scripts/e2e.sh --no-conformance kv   # only demo-kv.sh, no suite
#
# Arguments name demos to run (kv or demo-kv.sh); none means all of them.
# --no-conformance skips the suite, which takes about five minutes because it
# holds real long-polls. A slice that adds user-visible behavior adds or
# extends a demo, and this script's output for it is the evidence in the PR.
#
# Needs Go, jq, curl, python3, and openssl (the demos'). Set E2E_PORT to pick
# the port; the default is random in 18000-18999.
set -euo pipefail

cd "$(dirname "$0")/.."

conformance=1
wanted=()
for arg in "$@"; do
  case "$arg" in
    --no-conformance) conformance=0 ;;
    -h|--help) sed -n '2,15p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    -*) echo "unknown flag $arg" >&2; exit 2 ;;
    *) name="${arg#demo-}"; wanted+=("scripts/demo-${name%.sh}.sh") ;;
  esac
done
if [ ${#wanted[@]} -eq 0 ]; then
  wanted=(scripts/demo-*.sh)
fi
for demo in "${wanted[@]}"; do
  [ -f "$demo" ] || { echo "no such demo: $demo" >&2; exit 2; }
done

port="${E2E_PORT:-$((18000 + RANDOM % 1000))}"
hub_url="http://127.0.0.1:$port"
run_dir="$(mktemp -d "${TMPDIR:-/tmp}/vyshka-e2e.XXXXXX")"
# The binary carries the port in its name, so stopping this hub by name can
# never take down another one on the same machine.
hub_bin="$run_dir/vyshka-hub-e2e-$port"
case "$(go env GOOS)" in windows) hub_bin="$hub_bin.exe" ;; esac
export VYSHKA_ADMIN_TOKEN="vya_e2e_$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')"
export VYSHKA_HUB_URL="$hub_url"

hub_pid=""
cleanup() {
  if [ -n "$hub_pid" ]; then
    kill "$hub_pid" 2>/dev/null || true
    wait "$hub_pid" 2>/dev/null || true
  fi
  rm -rf "$run_dir"
}
trap cleanup EXIT

banner() { printf '\n\033[1m=== %s\033[0m\n' "$*"; }

banner "Build the hub ($(git rev-parse --short HEAD)$(git diff --quiet HEAD -- || echo ', with uncommitted changes'))"
go build -o "$hub_bin" ./hub/cmd/vyshka-hub
"$hub_bin" version

banner "Boot it at $hub_url on a fresh database"
"$hub_bin" serve -addr "127.0.0.1:$port" -db "$run_dir/e2e.db" > "$run_dir/hub.log" 2>&1 &
hub_pid=$!
for _ in $(seq 1 100); do
  curl --silent --fail "$hub_url/healthz" > /dev/null 2>&1 && break
  kill -0 "$hub_pid" 2>/dev/null || { cat "$run_dir/hub.log"; echo "the hub exited during boot" >&2; exit 1; }
  sleep 0.1
done
curl --silent --show-error --fail "$hub_url/healthz"; echo

results=()
failed=0
record() {
  results+=("$(printf '%-6s %s' "$1" "$2")")
  [ "$1" = PASS ] || failed=1
}

if [ "$conformance" = 1 ]; then
  banner "Hub conformance suite"
  if go run ./conformance/hub -url "$hub_url" -wait 10s; then
    record PASS "hub conformance suite"
  else
    record FAIL "hub conformance suite"
  fi
fi

for demo in "${wanted[@]}"; do
  banner "$demo"
  if [ "$demo" = scripts/demo-panel.sh ]; then
    # The panel demo serves a fake plugin until it is stopped. It passes if
    # its setup completes and the plugin is still serving a few seconds on.
    "$demo" > "$run_dir/panel.out" 2>&1 &
    panel_pid=$!
    sleep 6
    if kill -0 "$panel_pid" 2>/dev/null && grep -q "Ctrl-C to stop" "$run_dir/panel.out"; then
      kill "$panel_pid" 2>/dev/null || true
      wait "$panel_pid" 2>/dev/null || true
      cat "$run_dir/panel.out"
      record PASS "$demo"
    else
      kill "$panel_pid" 2>/dev/null || true
      wait "$panel_pid" 2>/dev/null || true
      cat "$run_dir/panel.out"
      record FAIL "$demo"
    fi
  elif "$demo"; then
    record PASS "$demo"
  else
    record FAIL "$demo"
  fi
done

if ! kill -0 "$hub_pid" 2>/dev/null; then
  record FAIL "the hub was still running at the end"
  echo "--- hub log (last 40 lines)"
  tail -n 40 "$run_dir/hub.log"
fi

banner "Summary"
printf '%s\n' "${results[@]}"
exit "$failed"
