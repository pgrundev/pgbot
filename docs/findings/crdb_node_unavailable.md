---
id: crdb_node_unavailable
severity: critical
critical_when: ""
dimension: risk
object: cluster
scope: infra
requires: [CockroachDB Admin API]
thresholds: []
related: [crdb_ranges_unavailable, crdb_ranges_underreplicated]
---

# crdb_node_unavailable

**Severity:** critical · **Dimension:** risk · **Object identity:** `cluster`

## What pgbot observed

The Admin API reported at least one node as dead, unavailable, or unknown.

## Why it matters

Lost nodes reduce serving capacity and may remove replica redundancy.

## How to verify it yourself

```sql
SELECT * FROM crdb_internal.kv_node_liveness;
```

Also inspect the DB Console node list and `GET /api/v2/nodes/`.

## How to fix it

Restore the node process or network, or follow CockroachDB's documented decommission and replacement procedure.

## When to ignore it

Only during an understood transition whose availability impact has been accepted.

```toml
[[ignore]]
finding = "crdb_node_unavailable"
reason = "planned node replacement"
expires = "2027-01-01"
```

## What pgbot cannot see

It cannot determine the infrastructure-level reason that the node disappeared.

## Related

- [crdb_ranges_unavailable](crdb_ranges_unavailable.md)
- [crdb_ranges_underreplicated](crdb_ranges_underreplicated.md)
