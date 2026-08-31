---
id: crdb_version_skew
severity: warn
critical_when: ""
dimension: risk
object: cluster
scope: infra
requires: [CockroachDB Admin API]
thresholds: []
related: [crdb_node_unavailable]
---

# crdb_version_skew

**Severity:** warn · **Dimension:** risk · **Object identity:** `cluster`

## What pgbot observed

Non-decommissioned nodes report more than one binary build tag.

## Why it matters

Version skew is normal during rolling upgrades but should not persist indefinitely.

## How to verify it yourself

```sql
SELECT node_id, build_tag FROM crdb_internal.gossip_nodes ORDER BY node_id;
```

## How to fix it

Confirm a supported rolling upgrade is active and finish it only after all intended nodes are healthy.

## When to ignore it

During an actively monitored rolling upgrade.

```toml
[[ignore]]
finding = "crdb_version_skew"
reason = "rolling upgrade in progress"
expires = "2027-01-01"
```

## What pgbot cannot see

It cannot determine whether the upgrade is intentionally paused.

## Related

- [crdb_node_unavailable](crdb_node_unavailable.md)
