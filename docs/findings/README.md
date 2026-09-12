# pgbot findings

Every finding pgbot can emit, grouped by the kind of trouble it points at. Each
page explains what pgbot observed, why it matters, how to verify it yourself, how
to fix it, when to ignore it, and what pgbot cannot see.

Read a page offline with `pgbot explain-finding <id>`. Suppress or tune any
finding via [`.pgbot.toml`](../configuration.md).

## Risk — time to incident

Lost durability, corruption, wraparound, replication — things that end in an outage or data loss.

- **[archiving_failing](archiving_failing.md)** · Critical — WAL archiving is failing — the archive is falling behind or broken
- **[archiving_stalled](archiving_stalled.md)** · Critical — WAL archiving hasn't advanced despite WAL being generated
- **[autovacuum_disabled_on_table](autovacuum_disabled_on_table.md)** · Critical — autovacuum_enabled=false in reloptions — a table rotting invisibly
- **[autovacuum_off](autovacuum_off.md)** · Critical — autovacuum is off — bloat and wraparound will follow
- **[blocking_chains](blocking_chains.md)** · Critical — one session is blocked waiting on locks held by another
- **[checksum_failures](checksum_failures.md)** · Critical — Postgres read a page whose checksum didn't match — likely corruption
- **[fsync_off](fsync_off.md)** · Critical — fsync is off — a crash can irrecoverably corrupt the database
- **[full_page_writes_off](full_page_writes_off.md)** · Critical — full_page_writes off — a crash can leave torn pages
- **[ignore_checksum_failure_on](ignore_checksum_failure_on.md)** · Critical — ignore_checksum_failure is on — corrupt pages are returned, not caught
- **[index_invalid](index_invalid.md)** · Critical — a failed CREATE INDEX CONCURRENTLY left an invalid index — critical if it's still maintained on writes, warn if it's failed-build debris
- **[sync_rep_degraded](sync_rep_degraded.md)** · Critical — fewer synchronous standbys connected than the config requires
- **[archiving_disabled](archiving_disabled.md)** · Warn — archive_mode is off — no continuous WAL archive for PITR
- **[connection_saturation](connection_saturation.md)** · Warn — connections approaching max_connections
- **[idle_in_transaction](idle_in_transaction.md)** · Warn — sessions idle inside an open transaction, holding locks and the xmin horizon
- **[int4_identity_column](int4_identity_column.md)** · Warn — a sequence-backed int2/int4 column that will wrap (int4 at 2.1B) regardless of current value
- **[long_running_transaction](long_running_transaction.md)** · Warn — a transaction open far too long, pinning vacuum
- **[mxid_wraparound](mxid_wraparound.md)** · Warn — multixact-id age climbing toward its own wraparound wall
- **[pgaudit_logs_parameters](pgaudit_logs_parameters.md)** · Warn — pgaudit.log_parameter=on writes bind parameters (passwords, PII) into the server log
- **[pgaudit_silent](pgaudit_silent.md)** · Warn — pgaudit is installed but pgaudit.log selects no classes — the audit trail does not exist
- **[prepared_xact_abandoned](prepared_xact_abandoned.md)** · Warn — a prepared (2PC) transaction left open, blocking vacuum forever
- **[recovery_conflicts](recovery_conflicts.md)** · Warn — queries on a standby cancelled by recovery conflicts
- **[replica_disconnected](replica_disconnected.md)** · Warn — a streaming standby that was present has dropped off
- **[replica_lag_time](replica_lag_time.md)** · Warn — a replica's replay lag has grown past the threshold
- **[replication_slot_inactive](replication_slot_inactive.md)** · Warn — an inactive replication slot is pinning WAL from removal
- **[sequence_exhaustion](sequence_exhaustion.md)** · Warn — a sequence near its ceiling — the next insert will error
- **[subscription_worker_down](subscription_worker_down.md)** · Warn — a logical subscription's apply worker isn't running
- **[table_never_vacuumed](table_never_vacuumed.md)** · Warn — a sizable table with no vacuum on record
- **[txid_wraparound](txid_wraparound.md)** · Warn — transaction-id age climbing toward the 2.1-billion read-only wall
- **[work_mem_overcommit](work_mem_overcommit.md)** · Warn — work_mem × max_connections exceeds effective_cache_size — OOM risk
- **[checksums_disabled](checksums_disabled.md)** · Info — data checksums are off, so this class of corruption is silent
- **[slot_wal_keep_unbounded](slot_wal_keep_unbounded.md)** · Info — max_slot_wal_keep_size = -1 with replication slots present — a stalled consumer can fill the disk
- **[statement_timeout_unset](statement_timeout_unset.md)** · Info — no cluster-wide statement_timeout — a runaway query can run forever

## Storage

Disk wasted by bloat, dead tuples, and indexes that earn nothing.

- **[autovacuum_starved](autovacuum_starved.md)** · Warn — dead tuples past the trigger while autovacuum falls behind
- **[table_bloat](table_bloat.md)** · Warn — dead tuples make a table far larger on disk than its live rows
- **[unused_indexes](unused_indexes.md)** · Warn — indexes with zero scans — storage and write cost, no reads served
- **[vacuum_horizon_blocked](vacuum_horizon_blocked.md)** · Warn — something pins the xmin horizon so vacuum can't reclaim
- **[redundant_indexes](redundant_indexes.md)** · Info — an index whose columns are a leading prefix of another

## Latency

Individual queries made slow by missing indexes or stale plans.

- **[fk_unindexed](fk_unindexed.md)** · Warn — a foreign key with no index — slow joins and cascade checks
- **[io_read_latency_high](io_read_latency_high.md)** · Warn — physical reads take milliseconds each — the working set is on the device, not in memory
- **[never_analyzed](never_analyzed.md)** · Warn — a table with no statistics at all — the planner guesses
- **[partition_seq_scan_heavy](partition_seq_scan_heavy.md)** · Warn — a partitioned table read end-to-end across its partitions
- **[query_slowdown](query_slowdown.md)** · Warn — a query's mean time regressed sharply versus the baseline
- **[random_page_cost_high](random_page_cost_high.md)** · Warn — random_page_cost tuned for spinning disks on SSD storage
- **[stale_statistics](stale_statistics.md)** · Warn — planner statistics far behind the data — the usual cause of a plan flip
- **[wait_lock_contention](wait_lock_contention.md)** · Warn — time spent waiting on heavyweight locks (ASH)
- **[io_concurrency_low](io_concurrency_low.md)** · Info — effective_io_concurrency ≤ 1 on SSD-backed storage — scans read one block at a time
- **[plan_cache_mode_forced](plan_cache_mode_forced.md)** · Info — plan_cache_mode pinned cluster-wide — prepared statements can't switch generic/custom plans

## Throughput

Whole-database capacity lost to waits, cache misses, and write amplification.

- **[autovacuum_saturated](autovacuum_saturated.md)** · Warn — every autovacuum worker busy at the moment sampled
- **[checkpoints_forced](checkpoints_forced.md)** · Warn — too many checkpoints forced by WAL volume — raise max_wal_size
- **[high_rollback_ratio](high_rollback_ratio.md)** · Warn — an unusually high share of transactions roll back
- **[low_cache_hit](low_cache_hit.md)** · Warn — a sustained low buffer cache hit ratio — disk-bound reads
- **[low_hot_update_ratio](low_hot_update_ratio.md)** · Warn — few updates are HOT, so each rewrites every index
- **[pgss_entries_evicted](pgss_entries_evicted.md)** · Warn — pg_stat_statements is evicting entries, biasing the top list
- **[seq_scan_heavy](seq_scan_heavy.md)** · Warn — a large table read mostly by full scans instead of index lookups
- **[wait_io_bound](wait_io_bound.md)** · Warn — time spent waiting on storage IO (ASH)
- **[wait_lwlock_pressure](wait_lwlock_pressure.md)** · Warn — time spent on lightweight-lock contention (ASH)
- **[work_mem_low](work_mem_low.md)** · Warn — queries spill sorts/hashes to temp files — work_mem too small
- **[autovacuum_long_running](autovacuum_long_running.md)** · Info — an autovacuum worker has been running over an hour
- **[io_timing_off](io_timing_off.md)** · Info — track_io_timing off, so per-query IO time is unavailable
- **[pgaudit_double_logging](pgaudit_double_logging.md)** · Info — pgaudit and log_statement=all record every statement twice — duplicate log volume

## Cost & visibility

Over-provisioning and missing instrumentation.

- **[connections_overprovisioned](connections_overprovisioned.md)** · Info — max_connections far above what's used — wasted memory reservation
- **[pg_stat_statements_missing](pg_stat_statements_missing.md)** · Info — pg_stat_statements isn't enabled — no query-level visibility
- **[stale_stats_window](stale_stats_window.md)** · Info — the cumulative statistics window is very old — rates are near-meaningless
