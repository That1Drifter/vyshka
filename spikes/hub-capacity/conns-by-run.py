"""Summarizes conn-sampler.sh lines over each report's measured window.

    python3 spikes/hub-capacity/conns-by-run.py conns.log results/round2/realistic/*.json

Prints, per run, the established connections on the hub's port (median and
maximum) and the listening socket's accept queue (median and maximum): a
queue above zero means connections the kernel completed and the hub had not
accepted.
"""
import datetime
import json
import sys

lines = [tuple(int(x) for x in line.split()) for line in open(sys.argv[1]) if len(line.split()) == 3]


def epoch(stamp):
    return datetime.datetime.fromisoformat(stamp.replace("Z", "+00:00")).timestamp()


print("| run | verdict | samples | established, median / max | accept queue, median / max |")
print("|---|---|---|---|---|")
for path in sys.argv[2:]:
    r = json.load(open(path))
    start, end = epoch(r["windowStart"]), epoch(r["windowEnd"])
    inside = [(est, q) for at, est, q in lines if start <= at <= end]
    if not inside:
        continue
    est = sorted(e for e, _ in inside)
    q = sorted(q for _, q in inside)
    print("| %s | %s | %d | %d / %d | %d / %d |" % (
        r["label"], "PASS" if r["pass"] else "FAIL", len(inside), est[len(est) // 2], est[-1], q[len(q) // 2], q[-1]))
