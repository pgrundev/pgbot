---
id: crdb_hot_range_concentration
severity: warn
critical_when: ""
dimension: throughput
object: node
scope: workload
requires: [CockroachDB Admin API hot-ranges endpoint]
thresholds: [at least 5 leaseholder-attributed top hot ranges, at least 0.5 sampled CPU cores, one node holds at least 3 ranges and 60% of sampled CPU]
related: [crdb_contention_hotspot, crdb_leaseholder_imbalance, crdb_resource_pressure]
---

# crdb_hot_range_concentration

**Severity:** warn · **Dimension:** throughput · **Object identity:** `node`

## What pgbot observed

One node leaseholds at least three of five or more attributed top hot ranges and accounts for at least 60% of at least 0.5 CPU cores in the bounded sample.

## Why it matters

Concentrated hot-range CPU often indicates hot keys, sequential writes, a dominant table or index, or lease placement that is not spreading the active workload.

## How to verify it yourself

```sql
SELECT node_id, store_id, range_count, lease_count, writes_per_second
FROM crdb_internal.kv_store_status
ORDER BY writes_per_second DESC;
```

Then open DB Console's Hot Ranges view during the incident and confirm the leaseholders, QPS, CPU, and named tables or indexes reported by pgbot.

## How to fix it

Correlate the listed ranges with tables and indexes, distribute hot keys or sequential writes, and verify lease preferences. Act on sustained observations rather than manually moving leases from one sample.

## When to ignore it

A brief batch or intentionally pinned regional workload can dominate one point-in-time sample without being a persistent health problem.

```toml
[[ignore]]
finding = "crdb_hot_range_concentration"
object  = "node:n<node-id>"
reason = "short controlled batch pinned to this region"
expires = "2027-01-01"
```

## What pgbot cannot see

The endpoint returns a bounded top-hot-range sample, not every range. pgbot cannot infer a durable trend from one inspection.

## Related

- [crdb_contention_hotspot](crdb_contention_hotspot.md)
- [crdb_leaseholder_imbalance](crdb_leaseholder_imbalance.md)
- [crdb_resource_pressure](crdb_resource_pressure.md)
