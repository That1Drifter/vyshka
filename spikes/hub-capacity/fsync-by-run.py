"""Matches fsync-sampler.sh samples to each report's measured window.

    python3 spikes/hub-capacity/fsync-by-run.py fsync-samples.log results/*/*.json

Prints, per run, the sample count, the median and the 90th percentile of the
synced-write latency sampled on the database's disk while that run was being
measured, and whether the disk was quiet by the spike's rule: a p90 of 3 ms
or less over the window. The same statistic grades passes and fails alike.
Several sampler logs can be given as a comma-separated first argument.
"""
import datetime
import json
import sys

QUIET_P90_US = 3000

samples = []
for log in sys.argv[1].split(","):
    for line in open(log):
        parts = line.split()
        if len(parts) == 2:
            samples.append((int(parts[0]), int(parts[1])))


def epoch(stamp):
    return datetime.datetime.fromisoformat(stamp.replace("Z", "+00:00")).timestamp()


print("| run | verdict | sync samples | median | p90 | disk |")
print("|---|---|---|---|---|---|")
for path in sys.argv[2:]:
    r = json.load(open(path))
    start, end = epoch(r["windowStart"]), epoch(r["windowEnd"])
    inside = sorted(us for at, us in samples if start <= at <= end)
    if not inside:
        continue
    median = inside[len(inside) // 2]
    p90 = inside[min(len(inside) - 1, int(0.9 * len(inside)))]
    print("| %s | %s | %d | %.1f ms | %.1f ms | %s |" % (
        r["label"], "PASS" if r["pass"] else "FAIL", len(inside), median / 1000, p90 / 1000,
        "quiet" if p90 <= QUIET_P90_US else "noisy"))
