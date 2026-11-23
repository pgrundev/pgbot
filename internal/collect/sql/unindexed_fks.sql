-- Foreign keys with no supporting index on the child. Without one, a DELETE or
-- UPDATE of a parent key sequentially scans the whole child to check references,
-- and holds locks longer. A supporting index must have the FK columns as its
-- leading columns, in order. Catalog-only; fk_columns are attnames (no data).
SELECT n.nspname   AS schema,
       c.relname   AS child_table,
       con.conname AS constraint_name,
       pg_relation_size(c.oid) AS child_bytes,
       (SELECT string_agg(a.attname, ', ' ORDER BY x.ord)
          FROM unnest(con.conkey) WITH ORDINALITY x(attnum, ord)
          JOIN pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = x.attnum) AS fk_columns
FROM pg_constraint con
JOIN pg_class c     ON c.oid = con.conrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE con.contype = 'f'
  AND n.nspname NOT IN ('pg_catalog', 'information_schema')
  AND NOT EXISTS (
    SELECT 1 FROM pg_index i
    WHERE i.indrelid = con.conrelid
      AND (string_to_array(i.indkey::text, ' ')::int2[])[1:cardinality(con.conkey)] = con.conkey
      -- A partial index only covers the FK when the RI lookup provably satisfies
      -- its predicate. The RI query is `WHERE $1 = fk_col [AND ...]` with strict
      -- operators, which implies `fk_col IS NOT NULL`, so the planner uses an
      -- index whose predicate is (a conjunction of) `<fk column> IS NOT NULL`
      -- (#129). Any other predicate (e.g. `deleted_at IS NULL`) leaves rows the
      -- RI check must still scan for. Matched on pg_get_expr's deparse, which
      -- renders these as `(c IS NOT NULL)` / `((a IS NOT NULL) AND (b IS NOT NULL))`;
      -- anything unrecognised stays "not covering".
      AND (i.indpred IS NULL OR NOT EXISTS (
        SELECT 1
        FROM regexp_split_to_table(
               substr(pg_get_expr(i.indpred, i.indrelid), 2,
                      length(pg_get_expr(i.indpred, i.indrelid)) - 2),
               '\) AND \(') AS conj(term)
        WHERE btrim(conj.term, '()') NOT IN (
          SELECT quote_ident(a.attname) || ' IS NOT NULL'
          FROM unnest(con.conkey) k(attnum)
          JOIN pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = k.attnum)
      ))
  )
ORDER BY child_bytes DESC
LIMIT 50;
