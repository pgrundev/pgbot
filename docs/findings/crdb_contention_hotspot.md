---
id: crdb_contention_hotspot
severity: warn
critical_when: ""
dimension: latency
object: cluster
scope: workload
requires: [crdb_internal.transaction_contention_events, VIEWACTIVITY]
thresholds: []
related: [crdb_retry_hotspot, crdb_serialization_conflicts]
---

# crdb_contention_hotspot

**Severity:** warn · **Dimension:** latency · **Object identity:** `cluster`

## What pgbot observed

Contention in the last hour accumulated at least five seconds of wait time, or one event waited at least one second.

## Why it matters

Lock waits add request latency and often concentrate on one hot key or a transaction that holds locks too long.

## How to verify it yourself

```sql
SELECT database_name, schema_name, table_name, index_name,
       count(*) AS events,
       sum(contention_duration) AS total_wait,
       max(contention_duration) AS longest_wait
FROM crdb_internal.transaction_contention_events
WHERE collection_ts >= now() - INTERVAL '1 hour'
  AND contention_type = 'LOCK_WAIT'
GROUP BY database_name, schema_name, table_name, index_name
ORDER BY total_wait DESC;
```

## How to fix it

Use the normalized waiter SQL and the blocking transaction's constituent statements reported by pgbot. Shorten the holding transaction, access rows consistently, and distribute writes away from hot keys.

## When to ignore it

A bounded batch may tolerate known contention when its latency objective is still met.

```toml
[[ignore]]
finding = "crdb_contention_hotspot"
reason = "known bounded batch contention"
expires = "2027-01-01"
```

## What pgbot cannot see

The source is an in-memory LRU, so counts are lower bounds. Query attribution is looked up only for the retained hotspot fingerprints, uses the last 24 hours of persisted SQL statistics, and caps each blocking transaction at five unordered statements. A zero blocker fingerprint is reported as “not resolved by CockroachDB”; that is different from a nonzero fingerprint no longer present in retained statistics. pgbot deliberately omits contended keys and transaction IDs.

## Related

- [crdb_retry_hotspot](crdb_retry_hotspot.md)
- [crdb_serialization_conflicts](crdb_serialization_conflicts.md)
