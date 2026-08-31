---
id: crdb_replica_size_skew
severity: warn
critical_when: ""
dimension: storage
object: store
scope: infra
requires: [CockroachDB Admin API MVCC and replica metrics]
thresholds: [at least 3 stores, mean at least 64 MiB per replica, max at least 1.75x mean and min at most 0.60x mean]
related: [crdb_replica_imbalance, crdb_capacity_imbalance, crdb_mvcc_garbage_pressure]
---

# crdb_replica_size_skew

**Severity:** warn · **Dimension:** storage · **Object identity:** `store`

## What pgbot observed

Average logical MVCC bytes per initialized replica differ sharply across live stores even after replica counts are considered.

## Why it matters

Balanced replica counts can conceal a few stores holding much larger ranges. Those stores bear more storage, snapshot, and compaction work.

## How to verify it yourself

```sql
SELECT node_id, store_id,
       (metrics->>'totalbytes')::DECIMAL AS total_mvcc_bytes,
       (metrics->>'replicas')::DECIMAL AS replicas,
       (metrics->>'totalbytes')::DECIMAL
         / nullif((metrics->>'replicas')::DECIMAL, 0) AS bytes_per_replica
FROM crdb_internal.kv_store_status
ORDER BY bytes_per_replica DESC;
```

## How to fix it

Check range sizes, table and index placement, zone constraints, and split or rebalance activity. Correct persistent placement blockers and allow CockroachDB to split or rebalance ranges automatically.

## When to ignore it

Locality constraints or deliberately concentrated datasets can make the distribution intentional. Ignore only after confirming the placement policy explains it.

```toml
[[ignore]]
finding = "crdb_replica_size_skew"
object  = "store:s<store-id>"
reason = "intentional locality-constrained dataset"
expires = "2027-01-01"
```

## What pgbot cannot see

MVCC bytes are logical replicated bytes rather than compressed on-disk bytes, and this aggregate cannot name the largest individual ranges.

## Related

- [crdb_replica_imbalance](crdb_replica_imbalance.md)
- [crdb_capacity_imbalance](crdb_capacity_imbalance.md)
- [crdb_mvcc_garbage_pressure](crdb_mvcc_garbage_pressure.md)
