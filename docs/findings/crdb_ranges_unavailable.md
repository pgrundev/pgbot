---
id: crdb_ranges_unavailable
severity: critical
critical_when: ""
dimension: risk
object: cluster
scope: infra
requires: [CockroachDB Admin API]
thresholds: []
related: [crdb_node_unavailable, crdb_ranges_underreplicated]
---

# crdb_ranges_unavailable

**Severity:** critical · **Dimension:** risk · **Object identity:** `cluster`

## What pgbot observed

One or more store metrics report unavailable range replicas.

## Why it matters

An unavailable range lacks quorum and may reject reads or writes for its keyspace.

## How to verify it yourself

```sql
SELECT * FROM crdb_internal.kv_store_status;
```

Confirm affected ranges in the DB Console replication report.

## How to fix it

Restore missing nodes or connectivity before considering expert-guided replica recovery.

## When to ignore it

Do not ignore a sustained unavailable range.

```toml
[[ignore]]
finding = "crdb_ranges_unavailable"
reason = "brief, actively monitored recovery"
expires = "2027-01-01"
```

## What pgbot cannot see

The aggregate metric does not identify every affected key span.

## Related

- [crdb_node_unavailable](crdb_node_unavailable.md)
- [crdb_ranges_underreplicated](crdb_ranges_underreplicated.md)
