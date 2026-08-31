---
id: crdb_leaseholder_imbalance
severity: info
critical_when: ""
dimension: throughput
object: store
scope: infra
requires: [CockroachDB Admin API store metrics]
thresholds: [at least 3 comparable live stores, mean at least 50 leases, spread at least 100 leases, max at least 1.5x mean, min at most 0.5x mean]
related: [crdb_hot_range_concentration, crdb_resource_pressure]
---

# crdb_leaseholder_imbalance

**Severity:** info · **Dimension:** throughput · **Object identity:** `store`

## What pgbot observed

Leaseholders are heavily skewed among at least three live stores of similar capacity. The rule is informational because lease preferences are often intentional.

## Why it matters

Leaseholders serve most reads and coordinate writes. Unintended skew can concentrate request coordination and CPU on a subset of nodes.

## How to verify it yourself

```sql
SELECT node_id, store_id, capacity, range_count, lease_count,
       writes_per_second
FROM crdb_internal.kv_store_status
ORDER BY lease_count DESC;
```

Check configured lease preferences and the per-node CPU view at the same time.

## How to fix it

Verify lease preferences and zone constraints first. If the skew is unintended and persistent, investigate lease rebalancing and node load rather than manually relocating individual leases from one snapshot.

## When to ignore it

Multi-region follower-read and lease-preference designs commonly place leases unevenly by intent.

```toml
[[ignore]]
finding = "crdb_leaseholder_imbalance"
object  = "store:s<store-id>"
reason = "expected lease preference for primary region"
expires = "2027-01-01"
```

## What pgbot cannot see

Store counts alone cannot distinguish an intentional locality preference from an allocator problem, and ranges do not all carry equal traffic.

## Related

- [crdb_hot_range_concentration](crdb_hot_range_concentration.md)
- [crdb_resource_pressure](crdb_resource_pressure.md)
