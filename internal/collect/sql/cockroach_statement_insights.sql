SELECT 'statement'                                                   AS kind,
       coalesce(encode(statement_fingerprint_id, 'hex'), '')         AS fingerprint,
       coalesce(problem, '')                                         AS problem,
       coalesce(causes, ARRAY[]::STRING[])                            AS causes,
       coalesce(query, '')                                           AS query,
       coalesce(status, '')                                          AS status,
       start_time                                                    AS started_at,
       end_time                                                      AS ended_at,
       coalesce(full_scan = 'YES', false)                            AS full_scan,
       coalesce(user_name, '')                                       AS user_name,
       coalesce(app_name, '')                                        AS app_name,
       coalesce(retries, 0)::int8                                    AS retries,
       coalesce(last_retry_reason, '')                               AS last_retry_reason,
       coalesce(index_recommendations, ARRAY[]::STRING[])            AS index_recommendations,
       coalesce(rows_read, 0)::int8                                  AS rows_read,
       coalesce(rows_written, 0)::int8                               AS rows_written,
       coalesce(extract(epoch FROM contention_time), 0)::float8      AS contention_s,
       (coalesce(extract(epoch FROM service_latency), 0) * 1000)::float8 AS service_latency_ms,
       (coalesce(extract(epoch FROM admission_wait_time), 0) * 1000)::float8 AS admission_wait_ms,
       coalesce(error_code, '')                                      AS error_code
FROM information_schema.crdb_statement_execution_insights
WHERE statement_id IS NOT NULL
  AND database_name = current_database()
  AND coalesce(app_name, '') NOT LIKE 'pgbot%'
  AND end_time >= now() - INTERVAL '24 hours'
ORDER BY end_time DESC
LIMIT 30;
