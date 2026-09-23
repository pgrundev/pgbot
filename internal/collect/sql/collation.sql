-- Collation version drift (PG15+): the version of the collation library the
-- catalog recorded when this database (datcollversion) or a collation object
-- (collversion) was created, against what the running server's libc/ICU reports
-- now. A difference means the library that defines text sort order changed under
-- the data — every btree over text sorted by it may be silently out of order
-- until REINDEXed. Scoped to current_database(): pg_collation is per-database
-- and the database row is this one's. NULL versions (C/POSIX, or a provider that
-- reports none) cannot drift and are excluded.
SELECT 'database' AS kind,
       d.datname   AS name,
       CASE d.datlocprovider WHEN 'c' THEN 'libc' WHEN 'i' THEN 'icu' WHEN 'b' THEN 'builtin'
            ELSE d.datlocprovider::text END                       AS provider,
       d.datcollversion                                           AS recorded,
       coalesce(pg_database_collation_actual_version(d.oid), '') AS actual
FROM pg_database d
WHERE d.datname = current_database()
  AND d.datcollversion IS NOT NULL
  AND d.datcollversion IS DISTINCT FROM pg_database_collation_actual_version(d.oid)
UNION ALL
SELECT 'collation',
       n.nspname || '.' || c.collname,
       CASE c.collprovider WHEN 'c' THEN 'libc' WHEN 'i' THEN 'icu' WHEN 'b' THEN 'builtin'
            ELSE c.collprovider::text END,
       c.collversion,
       coalesce(pg_collation_actual_version(c.oid), '')
FROM pg_collation c
JOIN pg_namespace n ON n.oid = c.collnamespace
WHERE c.collversion IS NOT NULL
  AND c.collversion IS DISTINCT FROM pg_collation_actual_version(c.oid)
ORDER BY 1, 2;
