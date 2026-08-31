-- CockroachDB's supported cluster-session surface. active_query_start is NULL
-- for an idle SQL session. Query text is deliberately not selected.
SELECT CASE WHEN active_query_start IS NULL THEN 'idle' ELSE 'active' END AS state,
       ''                                                               AS wait_event_type,
       count(*)::int                                                     AS n,
       0::float8                                                         AS max_xact_age_s,
       coalesce(max(extract(epoch FROM now() - active_query_start))
                  FILTER (WHERE active_query_start IS NOT NULL), 0)::float8 AS max_active_age_s
FROM [SHOW CLUSTER SESSIONS]
WHERE application_name <> 'pgbot'
GROUP BY 1;
