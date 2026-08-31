---
id: crdb_raft_backlog
severity: warn
critical_when: ""
dimension: latency
object: store
scope: infra
requires: [CockroachDB Admin API Raft metrics]
thresholds: [100 pending commands on one store, 1% of replicas in probe or snapshot flow, or 100 dropped messages per second]
related: [crdb_replication_recovery, crdb_storage_stall, crdb_node_unavailable]
---

# crdb_raft_backlog

**Severity:** warn · **Dimension:** latency · **Object identity:** `store`

## What pgbot observed

One store has a large pending-proposal backlog, many follower flows need probing or snapshots, or Raft messages are being dropped rapidly.

## Why it matters

Followers that cannot keep up increase commit latency, recovery traffic, and the chance that another failure reduces availability.

## How to verify it yourself

```sql
SELECT node_id, store_id,
       metrics->>'raft.commands.pending' AS commands_pending,
       metrics->>'raft.flows.state_probe' AS probe_flows,
       metrics->>'raft.flows.state_snapshot' AS snapshot_flows,
       metrics->>'raft.dropped' AS dropped_messages
FROM crdb_internal.kv_store_status
ORDER BY (metrics->>'raft.commands.pending')::DECIMAL DESC;
```

Compare `raft.dropped` across two samples because it is cumulative.

## How to fix it

Inspect disk and network latency, Raft scheduler latency, snapshot traffic, and node health for the busiest store. Address the resource or connectivity problem before attempting manual replica changes.

## When to ignore it

Brief probe or snapshot states are normal during replica changes. Ignore only when repeated runs show the backlog clearing.

```toml
[[ignore]]
finding = "crdb_raft_backlog"
object  = "store:s<store-id>"
reason = "planned recovery is converging"
expires = "2027-01-01"
```

## What pgbot cannot see

The Admin API gauges do not identify the exact range or remote follower responsible for every pending command.

## Related

- [crdb_replication_recovery](crdb_replication_recovery.md)
- [crdb_storage_stall](crdb_storage_stall.md)
- [crdb_node_unavailable](crdb_node_unavailable.md)
