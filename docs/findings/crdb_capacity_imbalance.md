---
id: crdb_capacity_imbalance
severity: warn
critical_when: ""
dimension: risk
object: store
scope: infra
requires: [CockroachDB Admin API store metrics]
thresholds: [at least 3 live stores, fullest at least 70% used, utilization spread at least 20 percentage points]
related: [crdb_store_capacity, crdb_replica_imbalance]
---

# crdb_capacity_imbalance

**Severity:** warn · **Dimension:** risk · **Object identity:** `store`

## What pgbot observed

At least one live store is 70% used while another live store has at least 20 percentage points more capacity headroom.

## Why it matters

Healthy cluster-wide free space can hide one locally constrained store. That reduces allocator flexibility and can make the fullest store reach its limit first.

## How to verify it yourself

```sql
SELECT node_id, store_id, capacity, used, available,
       round(100.0 * used / nullif(capacity, 0), 1) AS used_percent,
       range_count, lease_count
FROM crdb_internal.kv_store_status
ORDER BY used_percent DESC;
```

## How to fix it

Check store and node health, replica constraints, decommissioning state, and allocator activity. Correct the blocking condition and let CockroachDB rebalance; add capacity if the fullest store lacks safe headroom.

## When to ignore it

Different store sizes, deliberate constraints, or a recently added node can temporarily produce this pattern. Ignore only while rebalancing is demonstrably progressing or the layout is intentional.

```toml
[[ignore]]
finding = "crdb_capacity_imbalance"
object  = "store:s<store-id>"
reason = "new store is actively receiving replicas"
expires = "2027-01-01"
```

## What pgbot cannot see

A point-in-time utilization spread does not reveal the allocator's trend or time remaining until stores converge.

## Related

- [crdb_store_capacity](crdb_store_capacity.md)
- [crdb_replica_imbalance](crdb_replica_imbalance.md)
