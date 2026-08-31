---
id: crdb_long_running_query
severity: warn
critical_when: ""
dimension: latency
object: cluster
scope: workload
requires: [SHOW CLUSTER QUERIES]
thresholds: []
related: [crdb_execution_insights]
---

# crdb_long_running_query

**Severity:** warn · **Dimension:** latency · **Object identity:** `cluster`

## What pgbot observed

At least one statement in `SHOW CLUSTER QUERIES` has been running for 60 seconds or longer.

## Why it matters

Unexpected long-running statements can indicate contention, a full scan, or a request whose caller has already timed out.

## How to verify it yourself

```sql
SELECT application_name, now() - start AS age, phase, full_scan, query
FROM [SHOW CLUSTER QUERIES]
ORDER BY start;
```

## How to fix it

Identify the owning application and inspect the plan with `EXPLAIN (DISTSQL)`. Cancel the query only after confirming it is no longer needed.

## When to ignore it

Long analytical or maintenance queries may be expected.

```toml
[[ignore]]
finding = "crdb_long_running_query"
reason = "scheduled analytical workload"
expires = "2027-01-01"
```

## What pgbot cannot see

A point-in-time sample cannot determine whether the application still needs the query.

## Related

- [crdb_execution_insights](crdb_execution_insights.md)
