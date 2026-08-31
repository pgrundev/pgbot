---
id: crdb_ranges_underreplicated
severity: warn
critical_when: ""
dimension: risk
object: cluster
scope: infra
requires: [CockroachDB Admin API]
thresholds: []
related: [crdb_node_unavailable, crdb_ranges_unavailable, crdb_store_capacity]
---

# crdb_ranges_underreplicated

**Severity:** warn · **Dimension:** risk · **Object identity:** `cluster`

## What pgbot observed

One or more ranges have fewer replicas than their configured target.

## Why it matters

The affected ranges have less tolerance for another failure.

## How to verify it yourself

```sql
SELECT * FROM crdb_internal.kv_store_status;
```

## How to fix it

Check liveness, capacity, allocator progress, and zone constraints, then allow replication to recover.

## When to ignore it

A short-lived count can be expected during rebalancing after planned maintenance.

```toml
[[ignore]]
finding = "crdb_ranges_underreplicated"
reason = "planned rebalance"
expires = "2027-01-01"
```

## What pgbot cannot see

The snapshot cannot prove whether the allocator is making progress.

## Related

- [crdb_node_unavailable](crdb_node_unavailable.md)
- [crdb_ranges_unavailable](crdb_ranges_unavailable.md)
- [crdb_store_capacity](crdb_store_capacity.md)
