---
id: autovacuum_table_tuning
severity: info
critical_when: ""
dimension: storage
object: relation
scope: workload
requires: []
thresholds: []
related: [autovacuum_starved, table_bloat, txid_wraparound]
---

# autovacuum_table_tuning

**Severity:** info · **Dimension:** storage · **Object identity:** `schema.table` (see [configuration](../configuration.md)) · **Requires:** —

## What pgbot observed

A table with **≥ 1,000,000** live rows (`avTuneMinRows`), write activity (dead
tuples or updates on record), autovacuum enabled, and **no per-table
`autovacuum_vacuum_scale_factor`** override, while the global scale factor is
**≥ 0.1** (`avTuneMinScale`; the default is 0.2). pgbot reports the trigger the
table currently waits for — `autovacuum_vacuum_threshold + scale × n_live_tup` —
and the trigger a per-table override of scale 0.02 / threshold 1000
(`avTuneSuggestedScale`, `avTuneSuggestedThres`) would give. Up to ten tables
are listed, largest first.

## Why it matters

`autovacuum_vacuum_scale_factor` is a fraction of the table, so the same 20%
that is fine for a 10k-row lookup table means a 50M-row `orders` table
accumulates 10M dead rows before autovacuum even starts. Until then those rows
bloat the heap and every index, slow every scan, and let the table's
transaction-id age climb. When the vacuum finally runs it is a big one that
holds a worker for a long time. The documented remedy is a per-table override
that fires on a small fraction plus a fixed threshold — set on the relation, so
every other table keeps the default and the global cost budget is not spent on
tiny ones.

## How to verify it yourself

```sql
-- Current effective trigger per large table (global settings + reloptions).
SELECT s.schemaname, s.relname, s.n_live_tup, s.n_dead_tup,
       coalesce((SELECT option_value::float
                 FROM pg_options_to_table(c.reloptions)
                 WHERE option_name = 'autovacuum_vacuum_scale_factor'),
                current_setting('autovacuum_vacuum_scale_factor')::float) AS scale,
       coalesce((SELECT option_value::int
                 FROM pg_options_to_table(c.reloptions)
                 WHERE option_name = 'autovacuum_vacuum_threshold'),
                current_setting('autovacuum_vacuum_threshold')::int)       AS threshold
FROM pg_stat_user_tables s
JOIN pg_class c ON c.oid = s.relid
WHERE s.n_live_tup >= 1000000
ORDER BY s.n_live_tup DESC;
```

The trigger is `threshold + scale × n_live_tup`.

## How to fix it

```sql
ALTER TABLE public.orders SET (
    autovacuum_vacuum_scale_factor  = 0.02,
    autovacuum_vacuum_threshold     = 1000,
    autovacuum_analyze_scale_factor = 0.01,
    autovacuum_analyze_threshold    = 500
);
```

Takes effect at the next autovacuum cycle; no restart, and no rewrite (a brief
`SHARE UPDATE EXCLUSIVE` lock). Derive the numbers from the table: a queue-like
table with a few thousand live rows and constant churn wants a threshold-driven
trigger; a billion-row fact table wants an even smaller scale factor. More
frequent vacuums on big tables need cost budget — watch
[autovacuum_saturated](autovacuum_saturated.md) and raise
`autovacuum_vacuum_cost_limit` if workers fall behind.

Rollback: `ALTER TABLE public.orders RESET (autovacuum_vacuum_scale_factor,
autovacuum_vacuum_threshold, autovacuum_analyze_scale_factor,
autovacuum_analyze_threshold);`

## When to ignore it

- Append-only tables with occasional deletes — pgbot already skips tables with no
  dead tuples and no updates, but a rare bulk delete can trip it.
- You lowered the global scale factor deliberately (below 0.1 the finding stays quiet).

```toml
[[ignore]]
finding = "autovacuum_table_tuning"
object  = "public.audit_log"
reason  = "append-only; monthly partition drop handles retention"
expires = "2027-01-01"
```

## What pgbot cannot see

- The write *rate*: it sees dead tuples and cumulative updates, not how fast they
  arrive, so it cannot say how long the table waits between vacuums.
- Whether a manual `VACUUM` schedule already covers the table.
- `autovacuum_vacuum_insert_*` (PG13+) for insert-only tables — a separate trigger
  this finding does not model.

## Related

- [autovacuum_starved](autovacuum_starved.md) — the trigger was reached and
  autovacuum still didn't run; this finding is the trigger being too far away.
- [table_bloat](table_bloat.md) — what accumulates while waiting for the trigger.
- [txid_wraparound](txid_wraparound.md) — the eventual cost of vacuums that come too late.
