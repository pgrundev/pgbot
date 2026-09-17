---
id: slot_wal_keep_unbounded
severity: info
critical_when: ""
dimension: risk
object: setting
scope: infra
requires: []
thresholds: []
related: [replication_slot_inactive]
---

# slot_wal_keep_unbounded

**Severity:** info · **Dimension:** risk · **Object identity:** `setting:max_slot_wal_keep_size` (see [configuration](../configuration.md)) · **Requires:** —

## What pgbot observed

`max_slot_wal_keep_size` is `-1` (the default: no limit) **and** at least one row
exists in `pg_replication_slots`. Both are direct reads; there is no threshold.

## Why it matters

A replication slot guarantees its consumer will find the WAL it needs, by
preventing removal from the slot's restart point onward. With no ceiling, a
standby that is down for a weekend, a logical subscriber whose worker crashed, or
a slot left behind by a decommissioned tool retains WAL indefinitely — until
`pg_wal` fills the data disk and the primary stops accepting writes. A bound turns
that outage into the far cheaper failure of invalidating the slot
(`wal_status = 'lost'`) and re-seeding one consumer.

## How to verify it yourself

```sql
SELECT current_setting('max_slot_wal_keep_size') AS max_slot_wal_keep_size,
       slot_name, active, wal_status,
       pg_size_pretty(pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn)) AS retained
FROM pg_replication_slots;
```

## How to fix it

```sql
ALTER SYSTEM SET max_slot_wal_keep_size = '50GB';   -- reloadable
SELECT pg_reload_conf();
```

Size it above the longest consumer outage you intend to survive (WAL rate ×
hours — `pgbot inspect --full` shows the WAL rate) and below the disk headroom.
Alert on `wal_status = 'unreserved'` so you act before `lost`. On PostgreSQL 17+
`idle_replication_slot_timeout` additionally invalidates slots nobody has used.

## When to ignore it

Disk is monitored with a hard alert, and losing a standby's slot would be worse
than filling the disk (e.g. a re-seed takes days over a slow link).

```toml
[[ignore]]
finding = "slot_wal_keep_unbounded"
object  = "setting:max_slot_wal_keep_size"
reason  = "pg_wal on its own volume with disk alerts; re-seeding the DR standby takes 3 days"
expires = "2027-01-01"
```

## What pgbot cannot see

- Disk size or free space — it cannot compute a safe ceiling for you.
- Whether the slots' consumers are healthy right now; see
  [replication_slot_inactive](replication_slot_inactive.md) for the one that is not.

## Related

- [replication_slot_inactive](replication_slot_inactive.md) — the incident this
  setting bounds.
