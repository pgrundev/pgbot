-- Two sets in one scan. Runs inside conn.UnpinLocal (settings.go) so setting /
-- current_setting() reflect the server, database and role configuration rather
-- than the timeouts and read-only pin pgbot puts on its own session.
--   'override' — parameters set away from their compiled-in default by a human
--                (postgresql.conf, ALTER SYSTEM, ALTER DATABASE/ROLE, command
--                line). Session/client-sourced rows are pgbot's own and are not
--                server configuration.
--   'tuning'   — a fixed whitelist of tuning-relevant parameters, ALWAYS, with their
--                display values, so the tuning rules can name the current setting.
SELECT name, setting AS value, 'override' AS kind
FROM pg_settings
WHERE setting IS DISTINCT FROM boot_val
  AND source NOT IN ('session', 'client')
UNION ALL
SELECT name, current_setting(name) AS value, 'tuning' AS kind
FROM pg_settings
WHERE name IN (
  'work_mem', 'maintenance_work_mem', 'max_wal_size', 'max_connections',
  'shared_buffers', 'effective_cache_size', 'autovacuum_max_workers',
  'autovacuum_vacuum_scale_factor', 'random_page_cost', 'track_io_timing',
  'fsync', 'full_page_writes', 'autovacuum', 'statement_timeout',
  'archive_mode', 'archive_timeout', 'wal_level',
  'data_checksums', 'ignore_checksum_failure',
  'synchronous_commit', 'synchronous_standby_names',
  'autovacuum_analyze_threshold', 'autovacuum_analyze_scale_factor', 'default_statistics_target',
  'autovacuum_max_workers', 'autovacuum_vacuum_threshold', 'autovacuum_naptime',
  'autovacuum_vacuum_cost_delay', 'autovacuum_vacuum_cost_limit',
  -- memory envelope, plan cache, slot retention, and the IO knobs (PG18 adds
  -- io_method / io_workers / io_max_concurrency; absent rows are simply absent)
  'hash_mem_multiplier', 'huge_pages', 'plan_cache_mode', 'max_slot_wal_keep_size',
  'effective_io_concurrency', 'maintenance_io_concurrency', 'io_method', 'io_workers',
  'io_max_concurrency', 'wal_compression',
  -- audit posture (rows exist only when pgaudit is loaded; log_statement always)
  'pgaudit.log', 'pgaudit.log_parameter', 'log_statement'
)
ORDER BY 1;
