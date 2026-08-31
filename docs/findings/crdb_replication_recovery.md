---
id: crdb_replication_recovery
severity: warn
critical_when: ""
dimension: risk
object: store
scope: infra
requires: [CockroachDB Admin API replication metrics]
thresholds: [at least 10 and 2% uninitialized replicas, or at least 10 replicas in purgatory or snapshot queue]
related: [crdb_ranges_underreplicated, crdb_raft_backlog, crdb_store_capacity]
---

# crdb_replication_recovery

**Severity:** warn · **Dimension:** risk · **Object identity:** `store`

## What pgbot observed

A material share of replicas is uninitialized, or repair work is waiting in replication purgatory or the Raft snapshot queue.

## Why it matters

This state can indicate that node recovery, rebalancing, or replica repair is not converging. Persistent recovery pressure consumes network and disk resources and can leave less failure tolerance.

## How to verify it yourself

```sql
SELECT node_id, store_id,
       metrics->>'replicas' AS initialized,
       metrics->>'replicas.uninitialized' AS uninitialized,
       metrics->>'replicas.reserved' AS reserved_for_snapshots,
       metrics->>'queue.replicate.pending' AS replicate_pending,
       metrics->>'queue.replicate.purgatory' AS replicate_purgatory,
       metrics->>'queue.raftsnapshot.pending' AS snapshot_pending
FROM crdb_internal.kv_store_status
ORDER BY (metrics->>'replicas.uninitialized')::DECIMAL DESC;
```

## How to fix it

Check node liveness, snapshot traffic, store capacity, allocator logs, and zone constraints. Restore the blocking resource or make constraints satisfiable, then allow CockroachDB to complete recovery automatically.

## When to ignore it

A backlog is expected briefly after adding, restarting, or decommissioning nodes. Ignore only while repeated measurements show it falling.

```toml
[[ignore]]
finding = "crdb_replication_recovery"
object  = "store:s<store-id>"
reason = "planned node replacement is converging"
expires = "2027-01-01"
```

## What pgbot cannot see

A point-in-time gauge does not provide the backlog's age or direction. pgbot also cannot prove which placement constraint blocks a particular replica.

## Related

- [crdb_ranges_underreplicated](crdb_ranges_underreplicated.md)
- [crdb_raft_backlog](crdb_raft_backlog.md)
- [crdb_store_capacity](crdb_store_capacity.md)
