-- Published tables whose replica identity cannot identify a row, so an UPDATE or
-- DELETE on them fails outright ("cannot update table … because it does not have
-- a replica identity and publishes updates"). Only publications that replicate
-- UPDATE or DELETE matter; an insert-only publication needs no identity.
-- pg_publication_tables expands FOR ALL TABLES and FOR TABLES IN SCHEMA, so both
-- reach this list. Only the catalog is read — valid on an empty database.
WITH published AS (
  SELECT DISTINCT pt.schemaname, pt.tablename, p.pubname
  FROM pg_publication p
  JOIN pg_publication_tables pt ON pt.pubname = p.pubname
  WHERE p.pubupdate OR p.pubdelete
)
SELECT n.nspname                                        AS schema,
       c.relname                                        AS "table",
       c.relreplident::text                             AS identity,
       string_agg(DISTINCT pb.pubname, ', ')            AS publications
FROM published pb
JOIN pg_namespace n ON n.nspname = pb.schemaname
JOIN pg_class c ON c.relnamespace = n.oid AND c.relname = pb.tablename
WHERE c.relkind IN ('r', 'p')
  AND (
    -- 'd' (default) resolves to the primary key; without one there is nothing to use.
    (c.relreplident = 'd' AND NOT EXISTS (
       SELECT 1 FROM pg_index i WHERE i.indrelid = c.oid AND i.indisprimary AND i.indisvalid))
    -- 'n' (nothing) was set explicitly.
    OR c.relreplident = 'n'
    -- 'i' (index) whose nominated index is gone or invalid behaves like nothing.
    OR (c.relreplident = 'i' AND NOT EXISTS (
       SELECT 1 FROM pg_index i WHERE i.indrelid = c.oid AND i.indisreplident AND i.indisvalid))
  )
GROUP BY 1, 2, 3
ORDER BY 1, 2;
