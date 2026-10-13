-- pg_stat_io (PG16+), double-sampled. Every backend_type × object × context row;
-- the collector rate-computes the deltas and keeps only rows that moved during
-- the window. *_time columns are cumulative milliseconds and stay 0 unless
-- track_io_timing is on — the collector reports latency only when it is.
SELECT backend_type, object, context,
       coalesce(reads, 0)::bigint       AS reads,
       coalesce(read_time, 0)::float8   AS read_time,
       coalesce(writes, 0)::bigint      AS writes,
       coalesce(write_time, 0)::float8  AS write_time,
       coalesce(fsyncs, 0)::bigint      AS fsyncs,
       coalesce(fsync_time, 0)::float8  AS fsync_time
FROM pg_stat_io
ORDER BY backend_type, object, context;
