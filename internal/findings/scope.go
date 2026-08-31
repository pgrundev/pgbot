package findings

// clusterWide is the set of findings whose SOURCE is cluster-wide, not the
// connected database — so in an --all-databases run they are identical for every
// database and should be reported once, not N times (B3). The rest read
// per-database catalogs (pg_stat_user_tables/indexes, pg_sequences, this
// database's pg_stat_database row — health.sql filters on current_database())
// and genuinely differ per database.
//
// The classification follows the finding's SQL, not the view name: pg_stat_database
// appears on both sides. checksums.sql reads ALL of its rows (cluster-wide);
// health.sql reads only current_database()'s row (per-database). When adding a
// finding here, check which filter its query uses.
//
// Cluster-wide sources: pg_settings; pg_stat_activity (all backends, any db);
// pg_stat_archiver; pg_stat_replication / pg_replication_slots /
// pg_stat_subscription; pg_stat_statements (tracks all databases);
// pg_stat_bgwriter / pg_stat_checkpointer; the max transaction/multixact age
// across pg_database; the xmin horizon (backend_xmin + slots); unfiltered
// pg_stat_database sweeps (checksum_failures).
var clusterWide = map[string]bool{
	// settings
	"fsync_off": true, "full_page_writes_off": true, "autovacuum_off": true,
	"random_page_cost_high": true, "work_mem_overcommit": true, "statement_timeout_unset": true,
	"io_timing_off": true, "checkpoints_forced": true,
	"pgaudit_silent": true, "pgaudit_logs_parameters": true, "pgaudit_double_logging": true,
	"connections_overprovisioned": true, "checksums_disabled": true, "ignore_checksum_failure_on": true,
	// cluster activity (pg_stat_activity)
	"blocking_chains": true, "idle_in_transaction": true, "long_running_transaction": true,
	"connection_saturation": true, "prepared_xact_abandoned": true, "vacuum_horizon_blocked": true,
	"wait_lock_contention": true, "wait_io_bound": true, "wait_lwlock_pressure": true,
	"autovacuum_saturated": true, "autovacuum_long_running": true,
	// wraparound (max across pg_database)
	"txid_wraparound": true, "mxid_wraparound": true,
	// archiving / replication
	"archiving_failing": true, "archiving_stalled": true, "archiving_disabled": true,
	"sync_rep_degraded": true, "replica_lag_time": true, "replica_disconnected": true,
	"replication_slot_inactive": true, "subscription_worker_down": true,
	// checksum failures read ALL of pg_stat_database (its own SQL says so), so the
	// finding is identical from every database — report it once, not N times.
	"checksum_failures": true,
	// CockroachDB Admin API, Prometheus, jobs, and cluster-query sources.
	"crdb_node_unavailable": true, "crdb_ranges_unavailable": true,
	"crdb_ranges_underreplicated": true, "crdb_store_capacity": true,
	"crdb_resource_pressure": true, "crdb_job_failed": true, "crdb_job_stalled": true,
	"crdb_job_reverting": true, "crdb_job_paused": true, "crdb_version_skew": true,
	"crdb_replica_imbalance": true, "crdb_leaseholder_imbalance": true,
	"crdb_capacity_imbalance": true, "crdb_hot_range_concentration": true,
	"crdb_external_disk_usage": true, "crdb_storage_stall": true,
	"crdb_replication_recovery": true, "crdb_raft_backlog": true,
	"crdb_replica_size_skew":  true,
	"crdb_long_running_query": true,
	"crdb_contention_hotspot": true, "crdb_serialization_conflicts": true,
	// NOTE: work_mem_low is deliberately NOT here. It is driven by THIS
	// database's pg_stat_database.temp_bytes rate (health.sql filters on
	// current_database()), so a second database that spills genuinely needs its
	// own finding — deduping it against the first database would hide that.
	// NOTE: the pg_stat_statements findings (query_slowdown, pgss_entries_evicted,
	// pg_stat_statements_missing) are deliberately NOT here. The extension is
	// created PER DATABASE, so whether it's available differs per database — a
	// database that genuinely lacks it must report pg_stat_statements_missing, and
	// deduping against a "first" database that happens to have it would hide that.
}

// ClusterWide reports whether a finding comes from a cluster-wide source and so
// need only be reported once across an --all-databases run.
func ClusterWide(id string) bool { return clusterWide[id] }
