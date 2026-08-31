---
id: crdb_store_capacity
severity: warn
critical_when: "fullest store is at least 90% used"
dimension: risk
object: cluster
scope: infra
requires: [CockroachDB Admin API]
thresholds: []
related: [crdb_ranges_underreplicated]
---

# crdb_store_capacity

**Severity:** warn · **Dimension:** risk · **Object identity:** `cluster`

## What pgbot observed

The fullest store is at least 80% used; 90% is critical.

## Why it matters

One locally full store can constrain allocation even when cluster-wide free space looks adequate.

## How to verify it yourself

```sql
SELECT * FROM crdb_internal.kv_store_status;
```

## How to fix it

Add capacity or nodes, remove obsolete data, and verify rebalancing and locality constraints.

## When to ignore it

Ignore only when an imminent, monitored capacity operation supplies sufficient headroom.

```toml
[[ignore]]
finding = "crdb_store_capacity"
reason = "capacity expansion scheduled"
expires = "2027-01-01"
```

## What pgbot cannot see

It does not forecast growth without historical snapshots.

## Related

- [crdb_ranges_underreplicated](crdb_ranges_underreplicated.md)
