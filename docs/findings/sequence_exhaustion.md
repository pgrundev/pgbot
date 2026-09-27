---
id: sequence_exhaustion
severity: warn
critical_when: "recorded position is ≥90% toward the effective terminal bound"
dimension: risk
object: relation
scope: workload
requires: []
thresholds: []
related: [txid_wraparound]
---

# sequence_exhaustion

**Severity:** warn (critical when recorded position is ≥90% toward the effective terminal bound) · **Dimension:** risk · **Object identity:** `schema.sequence` (see [configuration](../configuration.md)) · **Requires:** —

## What pgbot observed

At least one non-cycling sequence, or a cycling sequence narrowed by its owning
integer column, has a recorded position **≥80%** toward its effective terminal
bound (`warn`), or **≥90%** (`critical`). For an ascending sequence pgbot measures
`(last_value - floor) / (ceiling - floor)`; for a descending sequence it measures
`(ceiling - last_value) / (ceiling - floor)`.

The effective floor and ceiling are the intersection of the sequence's configured
range and an owning `int2`/`int4` column's representable range. This catches both
the familiar ascending `int4` ceiling and a descending floor. It also catches a
cycling sequence whose opposite wrap value cannot fit its owning column.

## Why it matters

A non-cycling sequence eventually rejects `nextval()` after it reaches its
terminal bound. A column-limited sequence can instead produce a value that the
owning column cannot represent, either before the sequence reaches its own bound
or after a cycle wraps to the opposite end. Inserts that depend on it can then
fail.

This is a bounds-position heuristic, not a countdown. `pg_sequences.last_value`
is the value written to disk and can run ahead of values handed out when sequence
caching is enabled. The percentage therefore does not state an exact number of
calls remaining or when an insert will fail.

## How to verify it yourself

```sql
-- Position within each sequence's configured range. Check owning-column bounds
-- separately below; they may narrow this range.
SELECT schemaname || '.' || sequencename AS sequence,
       last_value,
       min_value,
       max_value,
       increment_by,
       cycle,
       round(100.0 * CASE
         WHEN increment_by > 0 THEN
           (last_value::numeric - min_value::numeric) /
             nullif(max_value::numeric - min_value::numeric, 0)
         ELSE
           (max_value::numeric - last_value::numeric) /
             nullif(max_value::numeric - min_value::numeric, 0)
       END, 2) AS pct_through_range
FROM pg_sequences
WHERE last_value IS NOT NULL
ORDER BY pct_through_range DESC NULLS LAST, schemaname, sequencename
LIMIT 20;
```

To see which columns are still `int4` (the ones that wrap early):

```sql
SELECT format('%I.%I.%I', n.nspname, c.relname, a.attname) AS column, t.typname
FROM pg_attribute a
JOIN pg_class c   ON c.oid = a.attrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
JOIN pg_type t    ON t.oid = a.atttypid
WHERE a.attnum > 0 AND NOT a.attisdropped
  AND pg_get_serial_sequence(format('%I.%I', n.nspname, c.relname), a.attname) IS NOT NULL
  AND t.typname = 'int4';
```

## How to fix it

First inspect the sequence's `INCREMENT`, `MINVALUE`, `MAXVALUE`, and `CYCLE`
settings together with the owning column type. For an owner-limited `int2` or
`int4` column, widen it to `bigint`. `ALTER TABLE … ALTER COLUMN … TYPE bigint`
rewrites the whole table under an `ACCESS EXCLUSIVE` lock — acceptable for a small
table in a maintenance window, but for a large, hot table do it **online** instead.
(`pg_repack` can rewrite a bloated table but **cannot** change a column's type, so
it is not the tool for this.) The online pattern:

1. Add a new `bigint` column (a fast metadata-only change on PG11+).
2. Backfill it in **batches** — bounded `UPDATE`s by primary-key range — so you
   never lock the whole table at once, keeping a trigger in sync for new writes.
3. Once caught up, swap in a short transaction: drop/rename the old column, repoint
   the sequence's `OWNED BY`, and set the column default.

**Widen the foreign-key columns in child tables too.** If `orders.id` becomes
`bigint` while `order_items.order_id` stays `int4`, you hit the same 2.1-billion
wall from the child side once its values grow — and the type mismatch also defeats
some join optimizations. Find the child columns that still need it:

```sql
SELECT conrelid::regclass AS child_table, a.attname AS fk_column, t.typname
FROM pg_constraint c
JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = ANY (c.conkey)
JOIN pg_type t      ON t.oid = a.atttypid
WHERE c.contype = 'f'
  AND c.confrelid = 'public.orders'::regclass
  AND t.typname = 'int4';
```

For a sequence that is not column-limited, changing its range or cycle mode can
affect application uniqueness assumptions. Treat that as a key-design change,
not a mechanical way to silence the finding.

## When to ignore it

You've confirmed a **specific** sequence's configured terminal behavior is safe
for its application and its owning column. Scope the rule to that sequence by
name — another sequence that crosses the threshold tomorrow still surfaces,
because the rule only drops this one object:

```toml
[[ignore]]
finding = "sequence_exhaustion"
object  = "public.legacy_events_id_seq"
reason  = "bounds and cycle behavior reviewed with the key allocation design"
expires = "2027-01-01"
```

Do **not** omit `object` here — a bare `finding = "sequence_exhaustion"` mutes the
check for *every* sequence, including ones you add later, which is exactly how a
future `int4` overflow gets hidden.

## What pgbot cannot see

- It reads the on-disk `last_value`. With `CACHE n`, that value can be ahead of
  values already handed out, so pgbot cannot infer exact calls remaining or the
  next call that will fail.
- A cycle wholly inside the effective range is safe from numeric bound exhaustion,
  not from duplicate-key or uniqueness failures after values repeat.
- It cannot see application-managed or sharded ID allocation that bypasses the
  sequence, nor a hi/lo allocator that reserves large blocks.
- The `int4`-column ceiling is inferred from the column type; a `domain` over
  `int4` or an unusual cast can hide it.

## Related

- [txid_wraparound](txid_wraparound.md) — a different 2.1-billion wall (transaction
  ids), also a hard write stop, but unrelated in cause.
