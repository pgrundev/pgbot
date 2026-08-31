---
id: crdb_mvcc_garbage_pressure
severity: warn
critical_when: ""
dimension: storage
object: relation
scope: workload
requires: [CockroachDB Admin API table metadata]
thresholds: []
related: [crdb_store_capacity]
---

# crdb_mvcc_garbage_pressure

**Severity:** warn · **Dimension:** storage · **Object identity:** `relation`

## What pgbot observed

At most half of a table's cached MVCC data is live and at least 1 GiB is non-live.

## Why it matters

Heavy churn, a long GC TTL, or protected timestamps can retain old versions, increasing storage and scan work.

## How to verify it yourself

```sql
SELECT db_name, schema_name, table_name,
       total_live_data_bytes, total_data_bytes, perc_live_data, last_updated
FROM system.table_metadata
WHERE db_name = current_database()
ORDER BY perc_live_data ASC;
```

Also inspect the table's zone configuration, protected timestamps, long-running transactions, changefeeds, and backup activity.

## How to fix it

Address the retention cause and let CockroachDB garbage collection reclaim old versions. Do not use PostgreSQL `VACUUM`.

## When to ignore it

Retained MVCC history may be intentional when required by a long GC TTL, changefeed, backup, or recovery objective.

```toml
[[ignore]]
finding = "crdb_mvcc_garbage_pressure"
object = "public.<table>"
reason = "retention required by recovery policy"
expires = "2027-01-01"
```

## What pgbot cannot see

This is CockroachDB MVCC history, not PostgreSQL heap bloat. Cached MVCC bytes include replicas and do not map one-to-one to physical disk recovery.

## Related

- [crdb_store_capacity](crdb_store_capacity.md)
