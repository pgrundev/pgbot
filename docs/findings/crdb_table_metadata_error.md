---
id: crdb_table_metadata_error
severity: warn
critical_when: ""
dimension: risk
object: relation
scope: infra
requires: [CockroachDB Admin API table metadata]
thresholds: []
related: [crdb_job_failed]
---

# crdb_table_metadata_error

**Severity:** warn · **Dimension:** risk · **Object identity:** `relation`

## What pgbot observed

CockroachDB's table metadata cache retained an update error for one or more tables.

## Why it matters

Size, range, replica, and MVCC values may remain from an older successful refresh, weakening storage and distribution diagnostics.

## How to verify it yourself

```sql
SELECT db_name, schema_name, table_name, last_updated, last_update_error
FROM system.table_metadata
WHERE last_update_error IS NOT NULL
ORDER BY db_name, schema_name, table_name;
```

Also inspect the `UPDATE TABLE METADATA CACHE` job in DB Console.

## How to fix it

Correct the recorded node or span-statistics error, refresh the table metadata cache from DB Console, and rerun pgbot.

## When to ignore it

Ignore it only when the affected table is being dropped and its cached metadata is no longer operationally relevant.

```toml
[[ignore]]
finding = "crdb_table_metadata_error"
object = "public.<table>"
reason = "table is being dropped"
expires = "2027-01-01"
```

## What pgbot cannot see

The error grades diagnostic coverage; by itself it does not prove that SQL requests to the table are failing.

## Related

- [crdb_job_failed](crdb_job_failed.md)
