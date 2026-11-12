"""Turns the measured database composition into steady-state disk figures.

    python3 spikes/hub-capacity/growth.py

Inputs are measurements from this spike, named where they are used; the
outputs are per 100-player server, in binary units (MiB, GiB) throughout.
Retention rules are the reference hub's defaults: events 30 days and
core.player.chat 90 (hub/events.go DefaultEventRetention), state snapshots
the last 500 per server and type (hub/server.go StateHistoryDepth) and 24 h,
webhook deliveries 7 days (hub/backup.go), audit records 365 days
(hub/audit.go). Action rows and acknowledged outbound envelopes are never
deleted (no DELETE reaches them in hub/store), so they grow without bound.
"""
MIB = 1 << 20
GIB = 1 << 30
DAY = 86400

# Bytes on disk per stored event, the events table, its identity rows, and
# the batch dedup record together, from the dbsize breakdowns:
# results/round1/growth/dbsize.txt (realistic, 152 513 events) and
# results/round1/tmpfs-busy-3/dbsize.txt (busy, 494 828 events).
BYTES_PER_EVENT = {
    "realistic": (117.2 + 45.5 + 9.6) * MIB / 152513,
    "busy": (401.7 + 146.0 + 3.6) * MIB / 494828,
}
# Events per second per server the hub stored, measured (eventsAcked over the
# window and the servers): results/round1/growth (100 realistic servers, 1200 s)
# and results/round1/disk-busy/1cpu-busy-100p-75s-r1 (75 busy servers, 180 s).
EVENTS_PER_SECOND = {"realistic": 147164 / 1200 / 100, "busy": 12.31}
# Share of player events that are chat, by the profile's definition
# (README, "Event rates"): 6 of 43.5 and 20 of 442 per player-hour.
CHAT_SHARE = {"realistic": 6 / 43.5, "busy": 20 / 442}
# Snapshots: rows per capture and bytes, from the growth run's breakdown
# (146.1 MiB over 15 756 rows). Three types per capture; 500 kept per type.
SNAPSHOT_BYTES_PER_ROW = 146.1 * MIB / 15756
SNAPSHOT_ROWS_KEPT = 3 * 500
# Rows that grow without bound or for a year, per dispatched action: one
# action row and one outbound envelope (about 570 bytes each: 1.1 MiB over
# 2024 rows of each in the growth breakdown), and an audit record (1.6 MiB
# over 2153 rows).
ACTION_BYTES = (1.1 + 1.1) * MIB / 2024
AUDIT_BYTES = 1.6 * MIB / 2153
ACTIONS_PER_MINUTE = {"realistic": 1, "busy": 6}
# Webhook deliveries: 6.6 MiB over 5115 rows; one per death to the one
# subscribed webhook; deaths per player-hour by the profile.
DELIVERY_BYTES = 6.6 * MIB / 5115
DEATHS_PER_HOUR = {"realistic": 1.5 * 100, "busy": 6 * 100}


def figures(profile, events_per_second):
    per_event = BYTES_PER_EVENT[profile]
    events_day = events_per_second * DAY * per_event
    events_steady = events_day * 30 + events_day * CHAT_SHARE[profile] * 60
    snapshots = SNAPSHOT_ROWS_KEPT * SNAPSHOT_BYTES_PER_ROW
    actions_day = ACTIONS_PER_MINUTE[profile] * 1440 * ACTION_BYTES
    audit_day = ACTIONS_PER_MINUTE[profile] * 1440 * AUDIT_BYTES
    deliveries = DEATHS_PER_HOUR[profile] * 24 * 7 * DELIVERY_BYTES
    deliveries_day = deliveries / 7
    return {
        "bytes per event": per_event,
        # What the file grows by per day once snapshot history is full (500
        # rows per server and type, reached within hours), until events start
        # expiring at 30 days: events, deliveries, audit, actions.
        "per day, days 1 to 30 (MiB)": (events_day + deliveries_day + audit_day + actions_day) / MIB,
        "  of which events (MiB)": events_day / MIB,
        "  of which webhook deliveries (MiB)": deliveries_day / MIB,
        "  of which audit records (MiB)": audit_day / MIB,
        "  of which actions and envelopes (MiB)": actions_day / MIB,
        "events per day (MiB)": events_day / MIB,
        "events at retention (GiB)": events_steady / GIB,
        "snapshots, bounded (MiB)": snapshots / MIB,
        "webhook deliveries, 7 days (MiB)": deliveries / MIB,
        "audit per day (MiB), kept 365 days": audit_day / MIB,
        "audit at retention (GiB)": audit_day * 365 / GIB,
        "actions + envelopes per day (MiB), never deleted": actions_day / MIB,
        "actions + envelopes per year (GiB)": actions_day * 365 / GIB,
        "at 90 days (GiB)": (events_steady + snapshots + deliveries + audit_day * 90 + actions_day * 90) / GIB,
        "after one year (GiB)": (events_steady + snapshots + deliveries + audit_day * 365 + actions_day * 365) / GIB,
    }


if __name__ == "__main__":
    for profile, eps in EVENTS_PER_SECOND.items():
        print("%s (%.3f events/s per server)" % (profile, eps))
        for key, value in figures(profile, eps).items():
            print("  %-52s %10.2f" % (key, value))
        out = figures(profile, eps)
        for disk_gb in (40, 80):
            usable = disk_gb * 1e9 / GIB
            print("  a %d GB disk (%.1f GiB) holds %.1f servers at 90 days, %.1f after a year" % (
                disk_gb, usable, usable / out["at 90 days (GiB)"], usable / out["after one year (GiB)"]))
