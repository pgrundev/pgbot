---
id: fk_unindexed
severity: warn
critical_when: ""
dimension: latency
object: relation
scope: schema
requires: []
thresholds: []
related: [seq_scan_heavy]
---

# fk_unindexed

**Severity:** warn · **Dimension:** latency · **Object identity:** `schema.relation` (see [configuration](../configuration.md)) · **Requires:** —

## What pgbot observed

One or more foreign-key constraints (`pg_constraint.contype = 'f'`) have **no index
on the child** whose leading columns match the FK columns. pgbot collects these
into `c.Indexes.UnindexedFKs` and reports each as
`schema.child_table (child_size) on (fk_columns)`. There is no numeric threshold —
any FK with no leading-column index on the child trips the rule; the child's size
only affects the ranking (`worst = max child bytes`).

## Why it matters

Postgres automatically indexes the **parent's** referenced key (it must be unique)
but **not** the child's FK columns. So when a referenced parent row is `DELETE`d,
or its key is `UPDATE`d, the referential-integrity trigger has to prove no child
row references it — and with no index that means a **sequential scan of the entire
child table**, performed while holding a row lock (`FOR KEY SHARE`) that keeps lock
duration high. It stays invisible while the child is small and turns into a
production stall the moment the child grows. The missing index also slows ordinary
joins along the foreign key.

## How to verify it yourself

```sql
-- FK constraints whose columns are NOT a leading prefix of any index on the child:
SELECT c.conrelid::regclass                                AS child_table,
       c.conname                                           AS fk_constraint,
       pg_size_pretty(pg_relation_size(c.conrelid))        AS child_size
FROM pg_constraint c
WHERE c.contype = 'f'
  AND NOT EXISTS (
        SELECT 1
        FROM pg_index i
        WHERE i.indrelid = c.conrelid
          AND i.indisvalid
          -- the FK's columns are a leading prefix of this index's columns:
          AND (string_to_array(i.indkey::text, ' ')::int[])[1 : cardinality(c.conkey)]
              = c.conkey::int[]
      )
ORDER BY pg_relation_size(c.conrelid) DESC;
```

## How to fix it

Create an index on the child's FK columns, in the same order the constraint
declares them, without locking the table:

```sql
CREATE INDEX CONCURRENTLY ON schema.child_table (fk_col1, fk_col2);
```

A plain b-tree is the right choice — it turns the RI check from a full child scan
into a single index probe, and it shortens the lock the parent `DELETE`/`UPDATE`
holds. After it builds, parent deletes and key updates stop scanning the child.

## When to ignore it

The child table is small enough that a full scan is genuinely cheap, or the parent
key is immutable and never deleted (a reference/dimension table), so the RI check
never actually runs. Scope the rule to that table:

```toml
[[ignore]]
finding = "fk_unindexed"
object  = "public.orders"
reason  = "parent country_code is never deleted or updated; RI check never scans"
expires = "2027-01-01"
```

Do **not** omit `object` — a bare `finding = "fk_unindexed"` mutes the check for
*every* table, including ones you add later, so the next big child table shipped
without its FK index silently carries a full-scan-on-delete landmine. Scope it to
the one relation you've reasoned about.

## What pgbot cannot see

- It sees the **missing index**, not your workload. It cannot tell whether you ever
  actually `DELETE` or key-`UPDATE` parent rows. If you never do, the index only
  helps joins — real, but a lower priority than the RI-scan risk implies.
- The coverage check is **structural leading-prefix** matching. An index in a
  different column order won't be recognized as covering it. A partial index counts
  only when its predicate is `<fk column> IS NOT NULL` (or an `AND` of those): the
  RI lookup `WHERE $1 = fk_col` implies that predicate, so the planner can use it.
  A partial index on anything else (e.g. `WHERE deleted_at IS NULL`) does not cover
  the FK — the RI check must still find the rows it excludes — and other
  equivalent spellings of `IS NOT NULL` are conservatively not recognized.
- It doesn't measure lock waits caused by this — those only appear (in
  `blocking_chains`) at the moment a parent delete actually stalls behind the scan.

## Related

- [seq_scan_heavy](seq_scan_heavy.md) — the full child-table scan an unindexed FK
  forces on every parent delete is exactly the sequential-scan cost that finding
  measures at the table level.
