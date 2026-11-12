#!/usr/bin/env bash
# Samples, every 2 s, the hub's established connections and its listening
# socket's accept queue (Recv-Q on a listening socket is the number of
# connections the kernel completed that the hub has not accepted yet). One
# line per sample: unix time, established, accept queue.
#
#   spikes/hub-capacity/conn-sampler.sh 18787 > conns.log
set -u
port=${1:-18787}
while true; do
  est=$(ss -Htn state established "( sport = :$port )" | wc -l)
  queue=$(ss -Hltn "( sport = :$port )" | awk '{print $2}' | head -1)
  echo "$(date +%s) $est ${queue:-0}"
  sleep 2
done
