---
id: crdb_execution_insights
severity: warn
critical_when: ""
dimension: latency
object: query
scope: workload
requires: [CockroachDB execution insights]
thresholds: []
related: [crdb_retry_hotspot, crdb_index_recommendations]
---

# crdb_execution_insights

**Severity:** warn · **Dimension:** latency · **Object identity:** `query`

## What pgbot observed

CockroachDB recorded recent slow or failed executions and classified causes such as plan regression, suboptimal plan, high contention, or high retries.

## Why it matters

These are execution-level diagnoses rather than an inference from a cluster average, so they point directly to a statement and likely mechanism.

## How to verify it yourself

```sql
SELECT problem, causes, status, service_latency, retries, query
FROM information_schema.crdb_statement_execution_insights
WHERE database_name = current_database()
ORDER BY end_time DESC;
```

## How to fix it

Open SQL Activity → Insights, inspect the recorded plan and contention details, and address the classified cause.

## When to ignore it

Suppress a fingerprint only when its observed latency or failure is expected and accepted.

```toml
[[ignore]]
finding = "crdb_execution_insights"
object = "q:<fingerprint>"
reason = "expected bounded batch execution"
expires = "2027-01-01"
```

## What pgbot cannot see

Insights are retained and flushed on CockroachDB's cadence; very recent or evicted executions may be absent.

## Related

- [crdb_retry_hotspot](crdb_retry_hotspot.md)
- [crdb_index_recommendations](crdb_index_recommendations.md)
