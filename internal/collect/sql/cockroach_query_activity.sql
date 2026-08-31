-- CockroachDB maintains this bounded top-query cache for SQL Activity. Unlike
-- aggregating the full persisted statistics table, its cost stays predictable
-- on a busy cluster. Access requires allow_unsafe_internals=true; capability
-- probing selects the public one-hour fallback when that opt-in is absent.
WITH grouped AS (
  SELECT coalesce(encode(fingerprint_id, 'hex'), '')                    AS fingerprint,
         coalesce(app_name, '')                                        AS app_name,
         coalesce(max(query), '')                                      AS query,
         coalesce(sum(execution_count), 0)::int8                       AS calls,
         (coalesce(sum(execution_total_seconds), 0) * 1000)::float8    AS total_ms,
         CASE WHEN coalesce(sum(execution_count), 0) > 0
              THEN (coalesce(sum(execution_total_seconds), 0) /
                    sum(execution_count)::float8 * 1000)::float8
              ELSE 0::float8 END                                       AS mean_ms,
         (nullif(max(service_latency_p99_seconds), 0) * 1000)::float8  AS p99_ms,
         sum(coalesce((statistics->'statistics'->'rowsRead'->>'mean')::float8, 0) *
             execution_count::float8)::int8                            AS rows_read,
         sum(coalesce((statistics->'statistics'->'rowsWritten'->>'mean')::float8, 0) *
             execution_count::float8)::int8                            AS rows_written,
         sum(coalesce((statistics->'statistics'->'bytesRead'->>'mean')::float8, 0) *
             execution_count::float8)::int8                            AS bytes_read,
         (sum(contention_time_avg_seconds * execution_count::float8) * 1000)::float8 AS contention_ms,
         max(coalesce((statistics->'statistics'->>'maxRetries')::int8, 0))::int8 AS max_retries
  FROM crdb_internal.statement_activity
  WHERE database = current_database()
    AND aggregated_ts >= now() - INTERVAL '24 hours'
    AND coalesce(app_name, '') NOT LIKE 'pgbot%'
  GROUP BY fingerprint_id, app_name
)
SELECT *, sum(total_ms) OVER ()::float8 AS total_exec_all
FROM grouped
WHERE calls > 0
ORDER BY total_ms DESC
LIMIT 20;
