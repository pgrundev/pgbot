---
id: crdb_storage_stall
severity: warn
critical_when: a disk-stalled event occurs
dimension: latency
object: store
scope: infra
requires: [two CockroachDB Admin API store-metric samples]
thresholds: [a slow disk operation, disk stall, unhealthy duration, or Pebble write stall occurs during the sample]
related: [crdb_raft_backlog, crdb_resource_pressure, crdb_store_capacity]
---

# crdb_storage_stall

**Severity:** warn · **Dimension:** latency · **Object identity:** `store`

## What pgbot observed

CockroachDB's cumulative disk-health or Pebble write-stall counters increased between pgbot's two samples.

## Why it matters

Slow disks and write stalls directly increase KV latency. They can also delay Raft log application and make followers require snapshots.

## How to verify it yourself

```sql
SELECT node_id, store_id,
       metrics->>'storage.disk-slow' AS disk_slow,
       metrics->>'storage.disk-stalled' AS disk_stalled,
       metrics->>'storage.disk-unhealthy.duration' AS unhealthy_nanos,
       metrics->>'storage.write-stalls' AS write_stalls,
       metrics->>'storage.write-stall-nanos' AS write_stall_nanos
FROM crdb_internal.kv_store_status
ORDER BY store_id;
```

Run the query twice and compare the cumulative values, or graph their rates in your metrics system.

## How to fix it

Inspect disk latency, IOPS, bandwidth, filesystem health, cloud-volume limits, Pebble L0 pressure, and compaction load on the affected node. Replace or resize unhealthy storage after confirming the bottleneck.

## When to ignore it

A single brief write stall may have little workload impact. Ignore only after confirming it does not recur and latency has recovered.

```toml
[[ignore]]
finding = "crdb_storage_stall"
object  = "store:s<store-id>"
reason = "one-off storage maintenance event"
expires = "2027-01-01"
```

## What pgbot cannot see

The default sample is short. A clean run does not prove there were no stalls before or after it, and the counters do not identify the underlying device cause.

## Related

- [crdb_raft_backlog](crdb_raft_backlog.md)
- [crdb_resource_pressure](crdb_resource_pressure.md)
- [crdb_store_capacity](crdb_store_capacity.md)
