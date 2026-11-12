#!/usr/bin/env bash
# Runs the hub capacity ramp (issue #87) on Linux with systemd: each step boots
# a fresh hub with a fresh SQLite database inside a transient systemd unit
# confined like a small VPS, drives it with the load generator pinned to other
# CPUs, and keeps the report. Run as root (systemd-run needs it).
#
#   spikes/hub-capacity/run.sh <bin-dir> <results-dir> <vcpus> <profile> <players> <servers...>
#
#   bin-dir      holds vyshka-capacity-hub and capacity-loadgen, built for linux
#   vcpus        1 or 2: the hub gets CPUQuota=<vcpus>00% on that many pinned CPUs
#   servers...   one step per value; each step runs RUNS times (default 2)
#
# Environment: RUNS (2), WARMUP (60s), DURATION (180s), HUB_CPUS (14 or 14-15),
# LOADGEN_CPUS (0-11), MEMORY_MAX (2G), PORT (18787), WEBHOOK_PORT (18799),
# STOP_ON_FAIL (0), WORK_BASE (/var/tmp; /dev/shm puts the database on tmpfs),
# KEEP_DB (a directory to keep each run's database in), EXTRA (extra loadgen
# flags, for example -verify), RUN_START (1; the first run number, so rounds of
# RUNS=1 across several steps can be interleaved without reusing a label).
set -euo pipefail

bin=$(realpath "$1"); results=$(realpath -m "$2"); vcpus=$3; profile=$4; players=$5
shift 5
runs=${RUNS:-2}
warmup=${WARMUP:-60s}
duration=${DURATION:-180s}
memory_max=${MEMORY_MAX:-2G}
port=${PORT:-18787}
webhook_port=${WEBHOOK_PORT:-18799}
loadgen_cpus=${LOADGEN_CPUS:-0-11}
case "$vcpus" in
  1) hub_cpus=${HUB_CPUS:-14} ;;
  2) hub_cpus=${HUB_CPUS:-14-15} ;;
  *) echo "vcpus must be 1 or 2" >&2; exit 2 ;;
esac
unit=vyshka-capacity-hub
cgroup=/sys/fs/cgroup/system.slice/$unit.service
mkdir -p "$results"

stop_hub() {
  systemctl stop "$unit" 2>/dev/null || true
  systemctl reset-failed "$unit" 2>/dev/null || true
}
trap stop_hub EXIT

for servers in "$@"; do
  first=${RUN_START:-1}
  for run in $(seq "$first" "$((first + runs - 1))"); do
    label="${vcpus}cpu-${profile}-${players}p-${servers}s-r${run}"
    work=$(mktemp -d "${WORK_BASE:-/var/tmp}/hubcap.XXXXXX")
    token="cap_$(head -c 18 /dev/urandom | od -An -tx1 | tr -d ' \n')"
    stop_hub
    systemd-run --unit="$unit" --collect --quiet \
      -p CPUQuota="${vcpus}00%" -p AllowedCPUs="$hub_cpus" \
      -p MemoryMax="$memory_max" -p MemorySwapMax=0 \
      -p WorkingDirectory="$work" \
      -p StandardOutput="file:$work/hub.log" -p StandardError="file:$work/hub.log" \
      -E VYSHKA_ADMIN_TOKEN="$token" \
      "$bin/vyshka-capacity-hub" serve -addr "127.0.0.1:$port" -panel=false
    for _ in $(seq 1 50); do
      curl -fsS "http://127.0.0.1:$port/healthz" >/dev/null 2>&1 && break
      sleep 0.2
    done
    echo "== $label (hub pid $(systemctl show -p MainPID --value "$unit"), work $work)"
    VYSHKA_ADMIN_TOKEN="$token" taskset -c "$loadgen_cpus" "$bin/capacity-loadgen" \
      -url "http://127.0.0.1:$port" -webhook-addr "127.0.0.1:$webhook_port" \
      -servers "$servers" -players "$players" -profile "$profile" \
      -warmup "$warmup" -duration "$duration" \
      -cgroup "$cgroup" -db-dir "$work" -mem-limit "$(numfmt --from=iec "$memory_max")" \
      -label "$label" -out "$results/$label.json" ${EXTRA:-} | tee "$results/$label.txt" || true
    # Keep the hub's warnings and errors, split at the measured window: what
    # happened under load apart from what the run's own teardown caused.
    if [ -f "$results/$label.json" ]; then
      python3 "$(dirname "$(realpath "$0")")/split-hub-log.py" "$work/hub.log" "$results/$label.json" \
        "$results/$label" | tee -a "$results/$label.txt" || true
    fi
    stop_hub
    # KEEP_DB=<dir> keeps each run's database there for a dbsize breakdown.
    if [ -n "${KEEP_DB:-}" ]; then
      mkdir -p "$KEEP_DB/$label"
      cp "$work"/vyshka.db* "$KEEP_DB/$label/"
    fi
    rm -rf "$work"
  done
  # STOP_ON_FAIL=1 ends the ramp after a step where every run failed.
  if [ "${STOP_ON_FAIL:-0}" = 1 ] && ! grep -qh '^PASS' "$results/${vcpus}cpu-${profile}-${players}p-${servers}s-r"*.txt; then
    echo "== every run at $servers servers failed; stopping the ramp"
    break
  fi
done
