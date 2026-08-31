---
id: crdb_resource_pressure
severity: warn
critical_when: ""
dimension: throughput
object: cluster
scope: infra
requires: [CockroachDB Admin API]
thresholds: []
related: [crdb_retry_hotspot]
---

# crdb_resource_pressure

**Severity:** warn · **Dimension:** throughput · **Object identity:** `cluster`

## What pgbot observed

Peak CPU is at least 90%, RSS is at least 90% of system memory, or admission wait p99 is at least 100 ms.

## Why it matters

Resource saturation and admission delay directly reduce throughput and increase latency.

## How to verify it yourself

```sql
SELECT name, value FROM crdb_internal.node_metrics
WHERE name IN ('sys.cpu.combined.percent-normalized', 'sys.rss');
```

## How to fix it

Correlate the pressured node with SQL fingerprints and hot ranges, reduce the dominant workload, rebalance, or add capacity.

## When to ignore it

A very brief planned load test may be acceptable if latency objectives remain satisfied.

```toml
[[ignore]]
finding = "crdb_resource_pressure"
reason = "planned load test"
expires = "2027-01-01"
```

## What pgbot cannot see

A point-in-time scrape does not distinguish a transient spike from sustained pressure.

## Related

- [crdb_retry_hotspot](crdb_retry_hotspot.md)
