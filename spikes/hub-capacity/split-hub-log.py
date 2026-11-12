"""Splits a hub log's warnings and errors by a run's measured window.

    python3 split-hub-log.py hub.log report.json out-prefix

Writes <out-prefix>.hub-warnings.log with every non-INFO line logged inside
the window, and <out-prefix>.hub-teardown.log with a count and the first 20
non-INFO lines logged after it (requests the generator cancelled when the run
ended, webhook retries after its sink closed). Lines before the window (setup
and warm-up) go to the teardown file's count too, marked as such. Prints the
in-window count per message.
"""
import collections
import datetime
import json
import sys


def epoch(stamp):
    return datetime.datetime.fromisoformat(stamp.replace("Z", "+00:00")).timestamp()


log, report, prefix = sys.argv[1:4]
r = json.load(open(report))
start, end = epoch(r["windowStart"]), epoch(r["windowEnd"])
inside, before, after = [], [], []
for line in open(log, encoding="utf-8", errors="replace"):
    try:
        entry = json.loads(line)
    except ValueError:
        continue
    if entry.get("level") == "INFO":
        continue
    at = epoch(entry["time"])
    (inside if start <= at <= end else before if at < start else after).append(line)
with open(prefix + ".hub-warnings.log", "w") as out:
    out.writelines(inside)
with open(prefix + ".hub-teardown.log", "w") as out:
    out.write("# %d non-INFO lines before the window (setup, warm-up), %d after it (teardown); the first 20 after:\n"
              % (len(before), len(after)))
    out.writelines(after[:20])
counts = collections.Counter(json.loads(line)["msg"] for line in inside)
for msg, n in counts.most_common():
    print("  hub in window: %d x %s" % (n, msg))
