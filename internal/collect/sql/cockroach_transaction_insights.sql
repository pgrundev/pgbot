SELECT 'transaction'                                                 AS kind,
       coalesce(encode(transaction_fingerprint_id, 'hex'), '')       AS fingerprint,
       CASE WHEN status = 'Failed' THEN 'FailedExecution'
            WHEN array_length(problems, 1) > 0 THEN array_to_string(problems, ',')
            ELSE 'TransactionInsight' END                            AS problem,
       coalesce(causes, ARRAY[]::STRING[])                            AS causes,
       coalesce(query, last_error_redactable, '')                    AS query,
       coalesce(status, '')                                          AS status,
       start_time                                                    AS started_at,
       end_time                                                      AS ended_at,
       false                                                         AS full_scan,
       coalesce(user_name, '')                                       AS user_name,
       coalesce(app_name, '')                                        AS app_name,
       coalesce(retries, 0)::int8                                    AS retries,
       coalesce(last_retry_reason, '')                               AS last_retry_reason,
       ARRAY[]::STRING[]                                             AS index_recommendations,
       coalesce(rows_read, 0)::int8                                  AS rows_read,
       coalesce(rows_written, 0)::int8                               AS rows_written,
       coalesce(extract(epoch FROM contention_time), 0)::float8      AS contention_s,
       (coalesce(extract(epoch FROM service_latency), 0) * 1000)::float8 AS service_latency_ms,
       (coalesce(extract(epoch FROM admission_wait_time), 0) * 1000)::float8 AS admission_wait_ms,
       coalesce(last_error_code, '')                                 AS error_code
FROM information_schema.crdb_transaction_execution_insights
WHERE transaction_id IS NOT NULL
  AND coalesce(app_name, '') NOT LIKE 'pgbot%'
  AND end_time >= now() - INTERVAL '24 hours'
ORDER BY end_time DESC
LIMIT 30;
