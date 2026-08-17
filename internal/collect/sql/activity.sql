-- Point-in-time connection breakdown. Grouped maxes let us derive the global
-- longest transaction / active query by taking max across rows in Go. Client
-- backends only, and never pgbot's own sessions (the pool + the wait sampler
-- sit in `idle in transaction` between BEGIN and each read — see conn.AppName).
SELECT coalesce(state, 'unknown')          AS state,
       coalesce(wait_event_type, '')       AS wait_event_type,
       count(*)                            AS n,
       coalesce(max(extract(epoch FROM now() - xact_start)), 0)                          AS max_xact_age_s,
       coalesce(max(extract(epoch FROM now() - query_start)) FILTER (WHERE state = 'active'), 0) AS max_active_age_s
FROM pg_stat_activity
WHERE backend_type = 'client backend'
  AND application_name IS DISTINCT FROM 'pgbot'
GROUP BY 1, 2;
