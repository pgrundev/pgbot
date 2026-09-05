-- One slow-plane snapshot of the lock graph: every blocked backend joined to
-- every backend blocking it, WITH the holder's transaction context — an
-- idle-in-transaction holder is invisible to the active-only fast plane, and
-- it is the single most common blocker. Query text is RAW and MUST be
-- scrubbed in Go before entering any report.
WITH blocked AS (
  SELECT a.pid                                                  AS blocked_pid,
         pg_blocking_pids(a.pid)                                AS blocking_pids,
         coalesce(a.wait_event, '')                             AS wait_event,
         coalesce(extract(epoch FROM now() - a.query_start), 0) AS blocked_wait_s,
         left(coalesce(a.query, ''), 300)                       AS victim_query
  FROM pg_stat_activity a
  WHERE cardinality(pg_blocking_pids(a.pid)) > 0
    AND a.pid <> pg_backend_pid()
)
SELECT b.blocked_pid,
       h.pid                                                    AS holder_pid,
       b.wait_event,
       b.blocked_wait_s,
       b.victim_query,
       coalesce(h.state, '')                                    AS holder_state,
       coalesce(extract(epoch FROM now() - h.xact_start), 0)    AS holder_xact_age_s,
       left(coalesce(h.query, ''), 300)                         AS holder_query,
       coalesce(h.usename::text, '')                            AS holder_user,
       coalesce(h.application_name, '')                         AS holder_app
FROM blocked b
JOIN pg_stat_activity h ON h.pid = ANY (b.blocking_pids);
