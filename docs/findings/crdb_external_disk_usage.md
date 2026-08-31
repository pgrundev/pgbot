---
id: crdb_external_disk_usage
severity: warn
critical_when: ""
dimension: risk
object: store
scope: infra
requires: [CockroachDB Admin API store metrics]
thresholds: [filesystem at least 80% used, other use or overhead at least 20% and 10 GiB]
related: [crdb_store_capacity, crdb_capacity_imbalance, crdb_storage_stall]
---

# crdb_external_disk_usage

**Severity:** warn · **Dimension:** risk · **Object identity:** `store`

## What pgbot observed

An already-full store volume has at least 10 GiB and 20% of capacity not accounted for by CockroachDB's `capacity.used` metric.

## Why it matters

CockroachDB cannot rebalance into filesystem space consumed by another process, old files, snapshots, or filesystem overhead. Cluster-wide capacity can therefore look adequate while one node is close to exhausting its volume.

## How to verify it yourself

```sql
SELECT node_id, store_id,
       (metrics->>'capacity')::DECIMAL AS capacity_bytes,
       (metrics->>'capacity.available')::DECIMAL AS available_bytes,
       (metrics->>'capacity.used')::DECIMAL AS cockroach_used_bytes,
       (metrics->>'capacity')::DECIMAL
         - (metrics->>'capacity.available')::DECIMAL
         - (metrics->>'capacity.used')::DECIMAL AS other_or_overhead_bytes
FROM crdb_internal.kv_store_status
ORDER BY other_or_overhead_bytes DESC;
```

Inspect the affected node's mount point with operating-system or cloud-volume tools before removing anything.

## How to fix it

Remove or relocate only confirmed non-CockroachDB data, correct an unexpected mount or snapshot policy, or add volume capacity. Never manually delete files inside an active CockroachDB store.

## When to ignore it

Ignore only when the difference is understood—for example filesystem reservation or a managed snapshot—and sufficient free-space headroom remains.

```toml
[[ignore]]
finding = "crdb_external_disk_usage"
object  = "store:s<store-id>"
reason = "known filesystem reservation"
expires = "2027-01-01"
```

## What pgbot cannot see

The metric difference cannot identify files or ownership and can include filesystem metadata, reserved space, snapshots, and accounting lag.

## Related

- [crdb_store_capacity](crdb_store_capacity.md)
- [crdb_capacity_imbalance](crdb_capacity_imbalance.md)
- [crdb_storage_stall](crdb_storage_stall.md)
