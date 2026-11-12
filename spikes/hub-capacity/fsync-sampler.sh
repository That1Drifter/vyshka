#!/usr/bin/env bash
# Samples synced-write latency on the disk the hub's database lives on, every
# 5 s, while a ramp runs: 20 synced 4 KiB writes per sample, reported as the
# mean microseconds per write after a Unix timestamp. Run it beside run.sh,
# pinned off the hub's CPUs, and match its lines to a report's window.
#
#   taskset -c 0-11 spikes/hub-capacity/fsync-sampler.sh /var/tmp > fsync.log
set -u
dir=${1:-/var/tmp}
probe="$dir/fsync-sampler.$$"
trap 'rm -f "$probe"' EXIT
while true; do
  start=$(date +%s%N)
  dd if=/dev/zero of="$probe" bs=4k count=20 oflag=dsync 2>/dev/null
  end=$(date +%s%N)
  rm -f "$probe"
  echo "$(date +%s) $(( (end - start) / 20000 ))"
  sleep 5
done
