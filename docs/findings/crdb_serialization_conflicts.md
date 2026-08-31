---
id: crdb_serialization_conflicts
severity: warn
critical_when: ""
dimension: throughput
object: cluster
scope: workload
requires: [crdb_internal.transaction_contention_events, VIEWACTIVITY]
thresholds: []
related: [crdb_contention_hotspot, crdb_retry_hotspot]
---

# crdb_serialization_conflicts

**Severity:** warn · **Dimension:** throughput · **Object identity:** `cluster`

## What pgbot observed

CockroachDB recorded at least five serialization conflicts in the last hour.

## Why it matters

Conflicts can surface as SQLSTATE 40001 or consume throughput through automatic retries and discarded work.

## How to verify it yourself

```sql
SELECT database_name, table_name,
       encode(waiting_stmt_fingerprint_id, 'hex') AS waiting_fingerprint,
       count(*) AS conflicts
FROM crdb_internal.transaction_contention_events
WHERE collection_ts >= now() - INTERVAL '1 hour'
  AND contention_type = 'SERIALIZATION_CONFLICT'
GROUP BY database_name, table_name, waiting_stmt_fingerprint_id
ORDER BY conflicts DESC;
```

## How to fix it

Keep transactions small, access rows in a consistent order, avoid read-modify-write hot keys, and retry SQLSTATE 40001 with backoff.

## When to ignore it

An occasional successfully retried conflict may be acceptable when latency remains within the service objective.

```toml
[[ignore]]
finding = "crdb_serialization_conflicts"
reason = "known low-rate retried conflicts"
expires = "2027-01-01"
```

## What pgbot cannot see

The in-memory event store can evict older conflicts and therefore supplies a lower bound.

## Related

- [crdb_contention_hotspot](crdb_contention_hotspot.md)
- [crdb_retry_hotspot](crdb_retry_hotspot.md)
