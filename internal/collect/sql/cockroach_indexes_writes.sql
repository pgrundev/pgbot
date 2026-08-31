-- Newer CockroachDB versions add cluster-wide index write counters. Keep this
-- query separate so older versions never parse columns they do not have.
WITH catalog AS (
  SELECT coalesce(t.database_name, current_database()) AS database_name,
         coalesce(t.schema_name, '')                   AS schema_name,
         ti.descriptor_name                           AS table_name,
         ti.index_name,
         ti.index_type,
         ti.is_unique,
         ti.is_inverted,
         ti.is_sharded,
         ti.is_visible,
         ti.created_at,
         us.total_reads,
         us.last_read,
         us.total_writes,
         us.last_write
  FROM crdb_internal.index_usage_statistics AS us
  JOIN crdb_internal.table_indexes AS ti
    ON ti.descriptor_id = us.table_id AND ti.index_id = us.index_id
  JOIN crdb_internal.tables AS t ON t.table_id = us.table_id
  WHERE t.database_name = current_database()
)
SELECT *,
       count(*) OVER ()::INT8 AS total_indexes,
       sum(CASE WHEN index_type = 'secondary' THEN 1 ELSE 0 END) OVER ()::INT8 AS secondary_indexes
FROM catalog
ORDER BY CASE WHEN index_type = 'secondary' THEN 0 ELSE 1 END,
         coalesce(last_read, created_at::TIMESTAMPTZ) ASC NULLS FIRST,
         total_reads ASC
LIMIT 500;
