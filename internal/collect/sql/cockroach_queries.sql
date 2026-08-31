-- Public-view fallback: persisted statement statistics over the most recent
-- hour. Rows are grouped by
-- fingerprint and application across aggregation buckets and nodes. Since a
-- global percentile cannot be reconstructed from bucket percentiles, p99_ms is
-- the maximum persisted bucket p99 (a deliberately conservative signal).
WITH grouped AS (
  SELECT coalesce(encode(fingerprint_id, 'hex'), '')           AS fingerprint,
         coalesce(app_name, '')                               AS app_name,
         coalesce(max(query), '')                             AS query,
         coalesce(sum(execution_count), 0)::int8              AS calls,
         (coalesce(sum(svc_lat_sum), 0) * 1000)::float8       AS total_ms,
         CASE WHEN coalesce(sum(execution_count), 0) > 0
              THEN (coalesce(sum(svc_lat_sum), 0) / sum(execution_count)::float8 * 1000)::float8
              ELSE 0::float8 END                              AS mean_ms,
         (max(p99_latency) * 1000)::float8                    AS p99_ms,
         coalesce(sum(rows_read_sum), 0)::int8                AS rows_read,
         coalesce(sum(rows_written_sum), 0)::int8             AS rows_written,
         coalesce(sum(bytes_read_sum), 0)::int8               AS bytes_read,
         (coalesce(sum(contention_time_sum), 0) * 1000)::float8 AS contention_ms,
         coalesce(max(max_retries), 0)::int8                  AS max_retries
  FROM information_schema.crdb_statement_statistics
  WHERE database = current_database()
    AND aggregated_ts >= now() - INTERVAL '1 hour'
    AND coalesce(app_name, '') NOT LIKE 'pgbot%'
  GROUP BY fingerprint_id, app_name
)
SELECT *, sum(total_ms) OVER ()::float8 AS total_exec_all
FROM grouped
WHERE calls > 0
ORDER BY total_ms DESC
LIMIT 20;
