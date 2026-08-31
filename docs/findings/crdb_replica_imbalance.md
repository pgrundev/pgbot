---
id: crdb_replica_imbalance
severity: warn
critical_when: ""
dimension: risk
object: store
scope: infra
requires: [CockroachDB Admin API store metrics]
thresholds: [at least 3 comparable live stores, mean at least 100 replicas, spread at least 100 replicas, max at least 1.35x mean, min at most 0.65x mean]
related: [crdb_ranges_underreplicated, crdb_capacity_imbalance]
---

# crdb_replica_imbalance

**Severity:** warn · **Dimension:** risk · **Object identity:** `store`

## What pgbot observed

Among at least three live stores within 25% of the median capacity, the most-loaded store has at least 100 more range replicas than the least-loaded store, at least 1.35 times the mean, while the least-loaded store has at most 0.65 times the mean.

## Why it matters

Persistent replica skew can concentrate storage, repair work, and KV traffic while leaving other eligible stores underused.

## How to verify it yourself

```sql
SELECT node_id, store_id, capacity, available, range_count, lease_count
FROM crdb_internal.kv_store_status
ORDER BY range_count DESC;
```

This internal view performs a cluster RPC. Also inspect allocator activity and replication in DB Console.

## How to fix it

Check allocator and rebalancing status, store health and capacity, and zone constraints. Restore allocator headroom or correct an unsatisfiable constraint, then allow CockroachDB to rebalance automatically.

## When to ignore it

Zone constraints, store attributes, or deliberate data placement may require an uneven replica distribution. Ignore only after confirming the skew matches the intended topology.

```toml
[[ignore]]
finding = "crdb_replica_imbalance"
object  = "store:s<store-id>"
reason = "expected placement from documented zone constraints"
expires = "2027-01-01"
```

## What pgbot cannot see

The Admin API snapshot does not prove why the allocator chose a placement. Range counts also do not measure equal bytes or equal traffic per range.

## Related

- [crdb_ranges_underreplicated](crdb_ranges_underreplicated.md)
- [crdb_capacity_imbalance](crdb_capacity_imbalance.md)
