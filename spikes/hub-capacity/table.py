"""Prints a results table of spikes/hub-capacity from the loadgen JSON reports.

    python3 spikes/hub-capacity/table.py spikes/hub-capacity/results/round2/busy/*.json

Every p99 is printed with the number of samples it was taken over; a p99 of n
samples is the ceil(0.01 n)-th largest, so under 100 samples it is the
maximum. "none returned" means the run scheduled that request and nothing came
back. The database column comes from the series directory: a series with
"tmpfs" in its name ran with WORK_BASE=/dev/shm, every other one on the ext4
disk. Round-one reports predate per-route grading; their "list" column is the
server list's p99 recomputed from the report, which their own verdict pooled
with the four other reads.
"""
import json
import sys

COLS = ("run", "database", "verdict", "tripped", "deliver p99", "poll ovh p99", "list p99",
        "other reads p99 (worst)", "hook p99", "cpu mean", "anon peak", "db MiB/h")
print("| " + " | ".join(COLS) + " |")
print("|" + "---|" * len(COLS))


def cell(d):
    if d is None:
        return "-"
    if d["n"] == 0:
        return "none returned"
    return "%.0f ms (%d)" % (d["p99Ms"], d["n"])


for path in sys.argv[1:]:
    r = json.load(open(path))
    lat, res = r["latency"], r["resources"]
    tripped = ", ".join(c["name"].replace(" p99", "").split(" /")[0].split(" (")[0]
                        for c in r["criteria"] if c["failed"]) or "none"
    others = [lat[k] for k in ("readEventsPage", "readStatePlayers", "readStateVehicles", "readStateWorld") if k in lat]
    worst = None
    if others:
        empty = [d for d in others if d["n"] == 0]
        worst = empty[0] if empty else max(others, key=lambda d: d["p99Ms"])
    anon = res.get("peakAnonBytes")
    print("| " + " | ".join([
        r["label"], "tmpfs" if "tmpfs" in path else "ext4 disk", "PASS" if r["pass"] else "FAIL", tripped,
        cell(lat.get("actionQueueToDelivery")), cell(lat.get("pollOverhead")), cell(lat.get("readServersList")),
        cell(worst), cell(lat.get("webhookPollToReceipt")),
        "%.0f%%" % res["cpuPercentMean"], "%d MiB" % (anon >> 20) if anon is not None else "-",
        "%.0f" % (res["dbBytesPerHour"] / (1 << 20)),
    ]) + " |")
