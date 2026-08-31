SELECT query_id::string                                  AS query_id,
       coalesce(user_name, '')                           AS user_name,
       coalesce(application_name, '')                    AS app_name,
       coalesce(query, '')                               AS query,
       coalesce(extract(epoch FROM now() - start), 0)::float8 AS age_s,
       coalesce(distributed, false)                      AS distributed,
       coalesce(full_scan, false)                        AS full_scan,
       coalesce(phase, '')                               AS phase,
       coalesce(isolation_level, '')                     AS isolation_level,
       coalesce(num_txn_retries, 0)::int8                AS retries,
       coalesce(num_txn_auto_retries, 0)::int8           AS auto_retries
FROM [SHOW CLUSTER QUERIES]
WHERE application_name <> 'pgbot'
ORDER BY start
LIMIT 50;
