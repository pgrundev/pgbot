---
id: crdb_unused_indexes
severity: warn
critical_when: ""
dimension: throughput
object: relation
scope: workload
requires: [CockroachDB cluster-wide index usage statistics]
thresholds: []
related: [crdb_index_recommendations]
---

# crdb_unused_indexes

**Severity:** warn · **Dimension:** throughput · **Object identity:** `relation`

## What pgbot observed

One or more non-unique secondary indexes had no recorded read for at least seven days, measured from the last read or index creation time.

## Why it matters

An unused secondary index adds write amplification. Newer CockroachDB versions expose write counts directly; older versions still expose cluster-wide read counts and last-read times.

## How to verify it yourself

```sql
SELECT t.database_name, t.schema_name, ti.descriptor_name AS table_name,
       ti.index_name, us.total_reads, us.last_read, ti.created_at
FROM crdb_internal.index_usage_statistics AS us
JOIN crdb_internal.table_indexes AS ti
  ON ti.descriptor_id = us.table_id AND ti.index_id = us.index_id
JOIN crdb_internal.tables AS t ON t.table_id = us.table_id
WHERE t.database_name = current_database()
ORDER BY coalesce(us.last_read, ti.created_at) ASC;
```

## How to fix it

Validate each candidate across a complete workload cycle in SQL Activity, preserve its DDL, and drop it only after confirming that interactive, scheduled, and recovery paths do not need it.

## When to ignore it

Keep an index used by an infrequent business process, recovery procedure, or workload cycle not covered by the retained counters.

```toml
[[ignore]]
finding = "crdb_unused_indexes"
object = "public.<index>"
reason = "required by quarterly workload"
expires = "2027-01-01"
```

## What pgbot cannot see

CockroachDB index-usage counters are cluster-wide but in-memory and non-durable. Node restarts can erase read history, and pgbot cannot prove that the observation covers every periodic workload. Index sizes are not included, so pgbot does not estimate reclaimable storage. Primary, unique, and unknown-age indexes are never candidates.

## Related

- [crdb_index_recommendations](crdb_index_recommendations.md)
