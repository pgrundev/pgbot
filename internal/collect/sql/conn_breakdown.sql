-- Where connections come from: top (application_name, user, state) groups. Turns
-- "87% of connections used" into an action — usually one app or user with a pool
-- leak or a burst of idle connections. application_name and role names are not
-- data (no PII); '(none)' where application_name is blank.
SELECT coalesce(nullif(application_name, ''), '(none)') AS app_name,
       coalesce(usename, '(none)')                       AS username,
       coalesce(state, 'unknown')                        AS state,
       count(*)                                          AS n
FROM pg_stat_activity
WHERE backend_type = 'client backend' AND application_name IS DISTINCT FROM 'pgbot'
GROUP BY 1, 2, 3
ORDER BY n DESC
LIMIT 10;
