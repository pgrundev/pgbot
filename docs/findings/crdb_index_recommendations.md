---
id: crdb_index_recommendations
severity: info
critical_when: ""
dimension: latency
object: query
scope: workload
requires: [CockroachDB execution insights]
thresholds: []
related: [crdb_execution_insights]
---

# crdb_index_recommendations

**Severity:** info · **Dimension:** latency · **Object identity:** `query`

## What pgbot observed

CockroachDB's optimizer attached one or more index recommendations to a problematic execution.

## Why it matters

A suitable index may avoid a full scan and reduce latency, but every added index consumes storage and increases write amplification.

## How to verify it yourself

```sql
SELECT query, index_recommendations
FROM information_schema.crdb_statement_execution_insights
WHERE database_name = current_database()
  AND array_length(index_recommendations, 1) > 0;
```

## How to fix it

Validate the recommendation with `EXPLAIN`, check for overlap with existing indexes, and estimate write and storage cost before creating it.

## When to ignore it

Ignore a recommendation when an existing index already covers it or the read benefit does not justify write amplification.

```toml
[[ignore]]
finding = "crdb_index_recommendations"
object = "q:<fingerprint>"
reason = "write cost exceeds read benefit"
expires = "2027-01-01"
```

## What pgbot cannot see

The recommendation is a candidate, not proof of production benefit under representative parameters and concurrency.

## Related

- [crdb_execution_insights](crdb_execution_insights.md)
