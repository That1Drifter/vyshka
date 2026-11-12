"""Per-credential request rates over the passing runs, by profile.

    python3 spikes/hub-capacity/rates.py results/round2/*/*.json

Each report holds, per credential kind (a plugin session; an admin client's
token), the mean over credentials, the busiest credential's mean, and the
busiest 1 s, 10 s and 60 s of any credential inside the measured window. The
window opens after setup and warm-up, so enrollment and session start are not
in these figures. Prints the range over the passing runs of each profile.
"""
import collections
import json
import sys

rows = collections.defaultdict(list)
for path in sys.argv[1:]:
    r = json.load(open(path))
    if not r["pass"] or r["servers"] < 10:
        continue
    for kind in ("pluginRates", "adminRates"):
        rows[(r["profile"], kind)].append(r[kind])

print("| Credential | Profile | Runs | Mean req/s | Busiest credential's mean | Busiest 1 s | Busiest 10 s, per s | Busiest 60 s, per s |")
print("|---|---|---|---|---|---|---|---|")
for (profile, kind), stats in sorted(rows.items()):
    def span(key, fmt):
        lo, hi = min(s[key] for s in stats), max(s[key] for s in stats)
        return fmt % hi if lo == hi else (fmt + " to " + fmt) % (lo, hi)
    print("| %s | %s | %d | %s | %s | %s | %s | %s |" % (
        "server (plugin session)" if kind == "pluginRates" else "admin token (one admin client)",
        profile, len(stats), span("meanRps", "%.3f"), span("maxMeanRps", "%.3f"),
        span("peak1sMax", "%d"), span("peak10sMax", "%.2f"), span("peak60sMax", "%.2f")))
