SELECT coalesce(nullif(application_name, ''), '(none)')                  AS app_name,
       coalesce(user_name, '(none)')                                     AS username,
       CASE WHEN active_query_start IS NULL THEN 'idle' ELSE 'active' END AS state,
       count(*)::int                                                      AS n
FROM [SHOW CLUSTER SESSIONS]
WHERE application_name <> 'pgbot'
GROUP BY 1, 2, 3
ORDER BY n DESC
LIMIT 10;
