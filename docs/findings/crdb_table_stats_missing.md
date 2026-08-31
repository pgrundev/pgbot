---
id: crdb_table_stats_missing
severity: warn
critical_when: ""
dimension: latency
object: relation
scope: workload
requires: [CockroachDB Admin API table metadata]
thresholds: []
related: [crdb_auto_stats_disabled, crdb_execution_insights, crdb_job_failed]
---

# crdb_table_stats_missing

**Severity:** warn · **Dimension:** latency · **Object identity:** `relation`

## What pgbot observed

A table with at least 64 MiB of cached MVCC data has no recorded optimizer-statistics timestamp.

## Why it matters

Without statistics, the cost-based optimizer estimates cardinality and selectivity from defaults, which can produce poor join orders and scan choices.

## How to verify it yourself

```sql
SELECT statistics_name, column_names, created, row_count
FROM [SHOW STATISTICS FOR TABLE <database>.<schema>.<table>]
ORDER BY created DESC;
```

Also inspect recent automatic statistics jobs with `SHOW JOBS`.

## How to fix it

Repair failed automatic statistics collection or enable it at table level. Run `CREATE STATISTICS <name> FROM <table>` when an immediate refresh is needed.

## When to ignore it

A newly loaded table may be briefly missing statistics before its first automatic collection completes.

```toml
[[ignore]]
finding = "crdb_table_stats_missing"
object = "public.<table>"
reason = "initial automatic statistics job is pending"
expires = "2027-01-01"
```

## What pgbot cannot see

The Admin API is cached. Confirm the live state with `SHOW STATISTICS` before changing production settings.

## Related

- [crdb_auto_stats_disabled](crdb_auto_stats_disabled.md)
- [crdb_execution_insights](crdb_execution_insights.md)
- [crdb_job_failed](crdb_job_failed.md)
