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
- **[crdb_node_unavailable](crdb_node_unavailable.md)** · Critical — one or more CockroachDB nodes are dead, unavailable, or unknown
- **[crdb_ranges_unavailable](crdb_ranges_unavailable.md)** · Critical — range replicas report unavailable and may reject access to affected keys
- **[fsync_off](fsync_off.md)** · Critical — fsync is off — a crash can irrecoverably corrupt the database
- **[full_page_writes_off](full_page_writes_off.md)** · Critical — full_page_writes off — a crash can leave torn pages
- **[ignore_checksum_failure_on](ignore_checksum_failure_on.md)** · Critical — ignore_checksum_failure is on — corrupt pages are returned, not caught
- **[index_invalid](index_invalid.md)** · Critical — a failed CREATE INDEX CONCURRENTLY left an invalid index — critical if it's still maintained on writes, warn if it's failed-build debris
- **[sync_rep_degraded](sync_rep_degraded.md)** · Critical — fewer synchronous standbys connected than the config requires
- **[archiving_disabled](archiving_disabled.md)** · Warn — archive_mode is off — no continuous WAL archive for PITR
- **[connection_saturation](connection_saturation.md)** · Warn — connections approaching max_connections
- **[crdb_capacity_imbalance](crdb_capacity_imbalance.md)** · Warn — live stores have materially different capacity utilization
- **[crdb_external_disk_usage](crdb_external_disk_usage.md)** · Warn — full store volumes contain substantial usage outside CockroachDB's active store
- **[crdb_job_failed](crdb_job_failed.md)** · Warn — a recent CockroachDB background job failed or failed while reverting
- **[crdb_job_stalled](crdb_job_stalled.md)** · Warn — an active CockroachDB background job stopped reporting progress or status updates
- **[crdb_ranges_underreplicated](crdb_ranges_underreplicated.md)** · Warn — ranges have less replica redundancy than their configured target
- **[crdb_replica_imbalance](crdb_replica_imbalance.md)** · Warn — range replicas are unevenly distributed across comparable live stores
- **[crdb_replication_recovery](crdb_replication_recovery.md)** · Warn — a material replica recovery backlog is uninitialized or blocked
- **[crdb_store_capacity](crdb_store_capacity.md)** · Warn — the fullest CockroachDB store is running out of capacity headroom
- **[crdb_table_metadata_error](crdb_table_metadata_error.md)** · Warn — CockroachDB could not refresh cached size and range metadata for one or more tables
- **[crdb_version_skew](crdb_version_skew.md)** · Warn — live CockroachDB nodes are running different binary versions
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
- **[crdb_job_paused](crdb_job_paused.md)** · Info — a CockroachDB background job is paused and will not progress until acted upon
- **[crdb_job_reverting](crdb_job_reverting.md)** · Info — CockroachDB is actively reverting an operation that did not complete
- **[statement_timeout_unset](statement_timeout_unset.md)** · Info — no cluster-wide statement_timeout — a runaway query can run forever

## Storage

Disk wasted by bloat, dead tuples, and indexes that earn nothing.

- **[autovacuum_starved](autovacuum_starved.md)** · Warn — dead tuples past the trigger while autovacuum falls behind
- **[crdb_mvcc_garbage_pressure](crdb_mvcc_garbage_pressure.md)** · Warn — large CockroachDB tables retain a substantial non-live MVCC footprint
- **[crdb_replica_size_skew](crdb_replica_size_skew.md)** · Warn — logical MVCC bytes per replica are heavily skewed across live stores
- **[table_bloat](table_bloat.md)** · Warn — dead tuples make a table far larger on disk than its live rows
- **[unused_indexes](unused_indexes.md)** · Warn — indexes with zero scans — storage and write cost, no reads served
- **[vacuum_horizon_blocked](vacuum_horizon_blocked.md)** · Warn — something pins the xmin horizon so vacuum can't reclaim
- **[redundant_indexes](redundant_indexes.md)** · Info — an index whose columns are a leading prefix of another

## Latency

Individual queries made slow by missing indexes or stale plans.

- **[crdb_contention_hotspot](crdb_contention_hotspot.md)** · Warn — recent lock contention accumulated material wait time on a table or index
- **[crdb_execution_insights](crdb_execution_insights.md)** · Warn — CockroachDB recorded slow or failed executions and their classified causes
- **[crdb_long_running_query](crdb_long_running_query.md)** · Warn — a CockroachDB statement has remained active for over one minute
- **[crdb_raft_backlog](crdb_raft_backlog.md)** · Warn — Raft proposals or follower replication are materially backlogged
- **[crdb_storage_stall](crdb_storage_stall.md)** · Warn — CockroachDB observed slow, stalled, or write-stalled storage during the sample
- **[crdb_table_stats_missing](crdb_table_stats_missing.md)** · Warn — substantial CockroachDB tables have no optimizer statistics
- **[fk_unindexed](fk_unindexed.md)** · Warn — a foreign key with no index — slow joins and cascade checks
- **[never_analyzed](never_analyzed.md)** · Warn — a table with no statistics at all — the planner guesses
- **[partition_seq_scan_heavy](partition_seq_scan_heavy.md)** · Warn — a partitioned table read end-to-end across its partitions
- **[query_slowdown](query_slowdown.md)** · Warn — a query's mean time regressed sharply versus the baseline
- **[random_page_cost_high](random_page_cost_high.md)** · Warn — random_page_cost tuned for spinning disks on SSD storage
- **[stale_statistics](stale_statistics.md)** · Warn — planner statistics far behind the data — the usual cause of a plan flip
- **[wait_lock_contention](wait_lock_contention.md)** · Warn — time spent waiting on heavyweight locks (ASH)
- **[crdb_auto_stats_disabled](crdb_auto_stats_disabled.md)** · Info — automatic optimizer-statistics collection is disabled on substantial tables
- **[crdb_index_recommendations](crdb_index_recommendations.md)** · Info — CockroachDB's optimizer attached candidate indexes to problematic executions

## Throughput

Whole-database capacity lost to waits, cache misses, and write amplification.

- **[autovacuum_saturated](autovacuum_saturated.md)** · Warn — every autovacuum worker busy at the moment sampled
- **[checkpoints_forced](checkpoints_forced.md)** · Warn — too many checkpoints forced by WAL volume — raise max_wal_size
- **[crdb_hot_range_concentration](crdb_hot_range_concentration.md)** · Warn — one leaseholder node dominates the bounded top-hot-range CPU sample
- **[crdb_resource_pressure](crdb_resource_pressure.md)** · Warn — node CPU, memory, or admission delay indicates resource pressure
- **[crdb_retry_hotspot](crdb_retry_hotspot.md)** · Warn — live or persisted query executions repeatedly retried serializable transactions
- **[crdb_serialization_conflicts](crdb_serialization_conflicts.md)** · Warn — transactions repeatedly encountered serialization conflicts
- **[crdb_unused_indexes](crdb_unused_indexes.md)** · Warn — non-unique CockroachDB secondary indexes have no reads across the observation threshold
- **[high_rollback_ratio](high_rollback_ratio.md)** · Warn — an unusually high share of transactions roll back
- **[low_cache_hit](low_cache_hit.md)** · Warn — a sustained low buffer cache hit ratio — disk-bound reads
- **[low_hot_update_ratio](low_hot_update_ratio.md)** · Warn — few updates are HOT, so each rewrites every index
- **[pgss_entries_evicted](pgss_entries_evicted.md)** · Warn — pg_stat_statements is evicting entries, biasing the top list
- **[seq_scan_heavy](seq_scan_heavy.md)** · Warn — a large table read mostly by full scans instead of index lookups
- **[wait_io_bound](wait_io_bound.md)** · Warn — time spent waiting on storage IO (ASH)
- **[wait_lwlock_pressure](wait_lwlock_pressure.md)** · Warn — time spent on lightweight-lock contention (ASH)
- **[work_mem_low](work_mem_low.md)** · Warn — queries spill sorts/hashes to temp files — work_mem too small
- **[autovacuum_long_running](autovacuum_long_running.md)** · Info — an autovacuum worker has been running over an hour
- **[crdb_leaseholder_imbalance](crdb_leaseholder_imbalance.md)** · Info — leaseholders are heavily skewed across comparable live stores
- **[io_timing_off](io_timing_off.md)** · Info — track_io_timing off, so per-query IO time is unavailable
- **[pgaudit_double_logging](pgaudit_double_logging.md)** · Info — pgaudit and log_statement=all record every statement twice — duplicate log volume

## Cost & visibility

Over-provisioning and missing instrumentation.

- **[connections_overprovisioned](connections_overprovisioned.md)** · Info — max_connections far above what's used — wasted memory reservation
- **[pg_stat_statements_missing](pg_stat_statements_missing.md)** · Info — pg_stat_statements isn't enabled — no query-level visibility
- **[stale_stats_window](stale_stats_window.md)** · Info — the cumulative statistics window is very old — rates are near-meaningless
