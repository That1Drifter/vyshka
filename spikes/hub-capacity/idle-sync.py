"""Reads the disk's synced-write latency in the idle gap before each run.

    python3 spikes/hub-capacity/idle-sync.py fsync-samples.log idle.log

The batch runner sleeps 30 s with no hub running before every run and logs
the gap as "idle <start> <end> <label>". The sampler's lines inside a gap
measure the disk without the hub's own writes on it, so they say whether the
host was quiet when the run began, which a reading taken during the run
cannot: under load the sampler's writes queue behind the hub's.
"""
import sys

samples = []
for log in sys.argv[1].split(","):
    for line in open(log):
        parts = line.split()
        if len(parts) == 2:
            samples.append((int(parts[0]), int(parts[1])))
print("| run | idle samples | idle median | idle max |")
print("|---|---|---|---|")
for log in sys.argv[2].split(","):
    for line in open(log):
        _, start, end, label = line.split(maxsplit=3)
        label = label.strip()
        inside = sorted(us for at, us in samples if int(start) <= at <= int(end))
        if inside:
            print("| %s | %d | %.1f ms | %.1f ms |" % (label, len(inside), inside[len(inside) // 2] / 1000, inside[-1] / 1000))
