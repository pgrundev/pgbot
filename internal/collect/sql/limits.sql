-- Cluster-wide saturation gauges: current vs configured connections, and the
-- oldest transaction-id age across all databases (transaction-ID wraparound
-- risk). age(datfrozenxid) climbs toward the ~2.1-billion wall past which
-- Postgres refuses writes; a healthy cluster stays well under autovacuum's
-- 200M freeze trigger.
-- max_mxid_age tracks multixact-ID wraparound (mxid_age(datminmxid)); multixacts
-- are consumed by SELECT ... FOR SHARE / FOR UPDATE and FK-check workloads and
-- exhaust toward the same ~2.1B wall as regular transaction ids.
-- conn_used counts what max_connections actually limits: client backends.
-- Background workers, autovacuum, checkpointer/bgwriter/walwriter and PG18's io
-- workers appear in pg_stat_activity but draw on other limits; pgbot's own
-- sessions are transient and excluded (conn.AppName).
SELECT (SELECT count(*) FROM pg_stat_activity
         WHERE backend_type = 'client backend'
           AND application_name IS DISTINCT FROM 'pgbot')::int     AS conn_used,
       current_setting('max_connections')::int                    AS conn_max,
       (SELECT max(age(datfrozenxid)) FROM pg_database)::bigint    AS max_xid_age,
       (SELECT max(mxid_age(datminmxid)) FROM pg_database)::bigint AS max_mxid_age;
