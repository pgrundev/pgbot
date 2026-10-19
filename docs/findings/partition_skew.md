---
id: partition_skew
severity: info
critical_when: ""
dimension: throughput
object: relation
scope: workload
requires: []
thresholds: []
related: [partition_seq_scan_heavy, autovacuum_table_tuning]
---

# partition_skew

**Severity:** info · **Dimension:** throughput · **Object identity:** `schema.table` (the partitioned parent; see [configuration](../configuration.md)) · **Requires:** —

## What pgbot observed

For a partitioned table with at least **4** leaf partitions
(`partitionSkewMinParts`), the hottest leaf takes **≥ 4×** the per-partition
average scan count (`partitionSkewFactor`, over at least 1,000 total scans), or
the largest leaf holds ≥ 4× the average row count (over at least 100,000 rows).
Scan counts are `seq_scan + idx_scan` from `pg_stat_user_tables`, rolled up by
climbing `pg_inherits` to the root. The finding is suppressed on a cold stats
window.

## Why it matters

Partitioning spreads maintenance and scans only as far as the key spreads the
data. One leaf carrying most of the rows or reads is, for every purpose that
matters — vacuum duration, index build time, scan cost, lock scope — an
unpartitioned table with extra planning overhead. It is also the earliest
visible form of the hot-shard problem: the same key would put the same tenant or
value on one shard if the table were ever distributed, and no number of routers
or shards fixes a key that doesn't spread.

## How to verify it yourself

```sql
-- Per-leaf scans and rows for one partitioned parent, hottest first.
SELECT c.relname                                  AS partition,
       s.seq_scan + coalesce(s.idx_scan, 0)       AS scans,
       s.n_live_tup                               AS rows
FROM pg_inherits i
JOIN pg_class c              ON c.oid = i.inhrelid
JOIN pg_stat_user_tables s   ON s.relid = c.oid
WHERE i.inhparent = '<parent_table>'::regclass
ORDER BY scans DESC;
```

## How to fix it

- **Time-range key, hottest = newest partition:** expected. Ignore it (below).
- **List key with one dominant value** (a big tenant, a common status): sub-partition
  that value (`PARTITION OF … FOR VALUES IN ('big_tenant') PARTITION BY HASH (id)`),
  or move it to its own table with the same schema.
- **Hash key with low cardinality:** re-key on something with more distinct
  values, e.g. `(tenant_id, id)` hashed together, at the next rebuild.
- **Accept it:** give the hot leaf its own autovacuum settings
  ([autovacuum_table_tuning](autovacuum_table_tuning.md)) and confirm its indexes
  are the ones the hot queries need.

Re-partitioning is a table rewrite — plan it as a migration with
`CREATE … CONCURRENTLY` indexes and a cut-over, not an `ALTER`.

## When to ignore it

Time-based partitioning where the newest partition is hot by design, or an
archive layout where old partitions are deliberately cold.

```toml
[[ignore]]
finding = "partition_skew"
object  = "public.events"
reason  = "monthly range partitions; current month is hot by design"
expires = "2027-01-01"
```

## What pgbot cannot see

- Counters are cumulative since the stats reset: a partition attached last week
  looks cold next to one attached last year.
- Which *value* is hot — only which leaf. Map the leaf to its bound with
  `pg_get_expr(relpartbound, oid)`.
- Query-level routing: whether hot queries prune to one leaf or scan all of them
  ([partition_seq_scan_heavy](partition_seq_scan_heavy.md) covers the latter).

## Related

- [partition_seq_scan_heavy](partition_seq_scan_heavy.md) — the parent scanned
  end-to-end; the other way partitioning fails to pay off.
- [autovacuum_table_tuning](autovacuum_table_tuning.md) — the hot leaf is exactly
  the relation that needs its own vacuum trigger.
