---
id: io_concurrency_low
severity: info
critical_when: ""
dimension: latency
object: setting
scope: infra
requires: []
thresholds: []
related: [random_page_cost_high, io_read_latency_high, wait_io_bound]
---

# io_concurrency_low

**Severity:** info · **Dimension:** latency · **Object identity:** `setting:effective_io_concurrency` (see [configuration](../configuration.md)) · **Requires:** —

## What pgbot observed

`effective_io_concurrency` is **0 or 1** and pgbot detected a managed, SSD-backed
provider (RDS, Aurora, Cloud SQL, Azure, Supabase, Neon). The condition is a direct
read of the GUC gated on the same provider detection as
[random_page_cost_high](random_page_cost_high.md); there is no tunable threshold.

## Why it matters

`effective_io_concurrency` is how many reads Postgres keeps in flight at once during
a bitmap heap scan — and on PostgreSQL 18, through the asynchronous-IO layer, for
sequential and index scans too. `1` was the pre-18 default, chosen for a rotational
disk that can only do one thing at a time. An SSD or a network volume serves dozens
of outstanding reads concurrently, so at `1` a scan waits for each block in turn
while the device sits idle. PostgreSQL 18 raised the default to **16** for this
reason.

## How to verify it yourself

```sql
SELECT name, setting, boot_val, source
FROM pg_settings
WHERE name IN ('effective_io_concurrency', 'maintenance_io_concurrency', 'io_method');
```

## How to fix it

```sql
ALTER SYSTEM SET effective_io_concurrency = 16;   -- then try 32
ALTER SYSTEM SET maintenance_io_concurrency = 16;
SELECT pg_reload_conf();
```

Reloadable, no restart. Raise in steps and re-measure read latency (`pg_stat_io`,
pgbot's `io_stats` section, or [io_read_latency_high](io_read_latency_high.md))
after each: past the device's useful queue depth, extra concurrency **raises**
latency for every session, and the documentation warns against unnecessarily high
values. On PostgreSQL 18 also compare `io_method = worker` against the default
on your storage before changing it.

## When to ignore it

- Storage you know to be rotational or a volume with a tiny queue depth.
- You already benchmarked and `1` measured best.

```toml
[[ignore]]
finding = "io_concurrency_low"
object  = "setting:effective_io_concurrency"
reason  = "benchmarked on this volume; higher values raised p99"
expires = "2027-01-01"
```

## What pgbot cannot see

- The actual device: provider detection is a heuristic, and a custom tablespace
  could sit on different storage.
- Whether the workload runs bitmap heap scans at all — on a pure point-read
  workload the setting rarely matters.

## Related

- [random_page_cost_high](random_page_cost_high.md) — the other rotational-era
  default that survives on SSD.
- [io_read_latency_high](io_read_latency_high.md) — the measurement to watch
  while changing this.
- [wait_io_bound](wait_io_bound.md)
