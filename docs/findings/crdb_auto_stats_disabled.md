---
id: crdb_auto_stats_disabled
severity: info
critical_when: ""
dimension: latency
object: relation
scope: infra
requires: [CockroachDB Admin API table metadata]
thresholds: []
related: [crdb_table_stats_missing]
---

# crdb_auto_stats_disabled

**Severity:** info · **Dimension:** latency · **Object identity:** `relation`

## What pgbot observed

Automatic optimizer-statistics collection is disabled on a table with at least 64 MiB of cached MVCC data.

## Why it matters

As the table changes, its optimizer statistics will remain fixed unless another process refreshes them.

## How to verify it yourself

```sql
SELECT statistics_name, column_names, created, row_count
FROM [SHOW STATISTICS FOR TABLE <database>.<schema>.<table>]
ORDER BY created DESC;
```

## How to fix it

If the override is not intentional:

```sql
ALTER TABLE <table> SET (sql_stats_automatic_collection_enabled = true);
```

Otherwise schedule and verify manual `CREATE STATISTICS` runs.

## When to ignore it

Ignore it for a controlled bulk-load or manually maintained statistics workflow with explicit ownership and monitoring.

```toml
[[ignore]]
finding = "crdb_auto_stats_disabled"
object = "public.<table>"
reason = "statistics are refreshed by the bulk-load pipeline"
expires = "2027-01-01"
```

## What pgbot cannot see

pgbot cannot determine whether an external process intentionally owns statistics maintenance.

## Related

- [crdb_table_stats_missing](crdb_table_stats_missing.md)
