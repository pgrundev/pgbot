---
id: crdb_retry_hotspot
severity: warn
critical_when: ""
dimension: throughput
object: query
scope: workload
requires: [SHOW CLUSTER QUERIES or CockroachDB statement statistics]
thresholds: []
related: [crdb_execution_insights]
---

# crdb_retry_hotspot

**Severity:** warn · **Dimension:** throughput · **Object identity:** `query`

## What pgbot observed

A live query or persisted statement fingerprint recorded at least three retries during one execution.

## Why it matters

Retries add latency and repeat work that is subsequently discarded. They often point to hot keys or transactions that touch too much data.

## How to verify it yourself

```sql
SELECT encode(fingerprint_id, 'hex'), app_name, max(max_retries), sum(contention_time_sum)
FROM information_schema.crdb_statement_statistics
WHERE database = current_database()
GROUP BY fingerprint_id, app_name
ORDER BY max(max_retries) DESC;

SHOW CLUSTER QUERIES;
```

## How to fix it

Use transaction insights to locate contention, keep transactions small, access rows consistently, and retain application retry handling.

## When to ignore it

An infrequent retry burst may be acceptable when latency remains within the service objective.

```toml
[[ignore]]
finding = "crdb_retry_hotspot"
object = "q:<fingerprint>"
reason = "known low-frequency batch conflict"
expires = "2027-01-01"
```

## What pgbot cannot see

Live data disappears when execution finishes. Persisted statistics report the maximum retry count, not every retry sequence or its contended keys.

## Related

- [crdb_execution_insights](crdb_execution_insights.md)
