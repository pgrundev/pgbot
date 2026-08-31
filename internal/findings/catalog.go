package findings

import (
	"strings"

	"github.com/pgrundev/pgbot/internal/model"
)

// Meta is the machine-readable metadata for one finding: the contract the
// docs/findings/<id>.md page must match, and the source of truth for the
// `explain-finding` command and the terminal doc references (B7). It is checked
// against what Compute actually emits (TestCatalog_matchesEmitted) so a stale
// Meta fails CI, and against every page's front-matter (the docs test) so a page
// that disagrees with the binary fails CI too.
type Meta struct {
	Severity     string   // base severity: critical | warn | info
	CriticalWhen string   // when the severity escalates to critical ("" if fixed)
	Dimension    string   // Impact.Dimension: risk | storage | latency | throughput
	ObjectClass  string   // suppression object class: cluster|relation|index|query|slot|sub|setting|db
	Scope        string   // what the finding depends on: schema|workload|history|infra (D3-0)
	Requires     []string // capabilities/versions the finding needs
	Thresholds   []string // overridable [thresholds] keys, if any
	Related      []string // finding ids that travel with this one
}

// The four Scopes classify a finding by what it needs to be meaningful — which is
// what makes `--profile=schema` (D3-1) safe on an empty CI database.
//
//	schema   — derivable from the catalog alone; valid on a freshly-migrated,
//	           never-queried database (the only scope --profile=schema keeps).
//	workload — needs query traffic or table activity (scans, dead tuples, waits).
//	history  — needs a baseline snapshot or cumulative-counter delta.
//	infra    — a property of the cluster/host or the server's *settings*, not the
//	           database's schema. Config findings live here on purpose: on a
//	           `services: postgres` container they read the image defaults, so
//	           firing them on a PR reports a throwaway container as the user's DB.
const (
	ScopeSchema   = "schema"
	ScopeWorkload = "workload"
	ScopeHistory  = "history"
	ScopeInfra    = "infra"
)

// validScopes is the closed set; the drift guard rejects anything else.
var validScopes = map[string]bool{
	ScopeSchema: true, ScopeWorkload: true, ScopeHistory: true, ScopeInfra: true,
}

// ValidScope reports whether s is one of the four scopes.
func ValidScope(s string) bool { return validScopes[s] }

// metaScope covers the config-hygiene meta-findings, which have no catalog entry
// (their docs live in configuration.md). They are never `schema` — a schema PR
// check must not fire on the user's .pgbot.toml rot — so they're excluded from
// --profile=schema like everything else non-schema.
var metaScope = map[string]string{
	"suppression_expired": ScopeInfra,
	"suppression_unused":  ScopeInfra,
}

// Scope returns the scope for any finding id pgbot can emit (catalog or meta).
func Scope(id string) string {
	if m, ok := catalog[id]; ok {
		return m.Scope
	}
	return metaScope[id]
}

// IsSchemaScoped reports whether a finding is derivable from the catalog alone
// (the membership --profile=schema keeps). This is a filter over the catalog, not
// a hardcoded list, so it can't drift as findings are added.
func IsSchemaScoped(id string) bool { return Scope(id) == ScopeSchema }

// catalog holds Meta for every finding. B7-0 seeds the two exemplars plus the
// critical review case; B7-1 completes it to all knownIDs (guarded by the docs
// coverage test). Keep entries in sync with the code — the consistency test is
// the backstop.
var catalog = map[string]Meta{
	"blocking_chains": {
		Severity: "critical", CriticalWhen: "",
		Dimension: "risk", ObjectClass: "cluster",
		Scope:   "workload",
		Related: []string{"long_running_transaction", "idle_in_transaction"},
	},
	"index_invalid": {
		Severity: "critical", CriticalWhen: "an invalid index is still indisready (maintained on every write); failed-build debris (indisready = false, typically 0 bytes) is warn, and any state is warn while a CREATE INDEX CONCURRENTLY is still building",
		Dimension: "risk", ObjectClass: "relation",
		Scope:   "schema",
		Related: []string{"unused_indexes"},
	},
	"unused_indexes": {
		Severity: "warn", CriticalWhen: "",
		Dimension: "storage", ObjectClass: "relation",
		Scope:      "workload",
		Thresholds: []string{"unused_index_min_size_mb"},
		Related:    []string{"redundant_indexes", "low_hot_update_ratio"},
	},
	"table_bloat": {
		Severity: "warn", CriticalWhen: "",
		Dimension: "storage", ObjectClass: "relation",
		Scope:      "workload",
		Thresholds: []string{"dead_ratio_warn"},
		Related:    []string{"low_hot_update_ratio", "vacuum_horizon_blocked", "autovacuum_starved"},
	},
	"redundant_indexes": {
		Severity: "info", CriticalWhen: "",
		Dimension: "storage", ObjectClass: "relation",
		Scope:   "schema",
		Related: []string{"unused_indexes", "low_hot_update_ratio"},
	},
	"fk_unindexed": {
		Severity: "warn", CriticalWhen: "",
		Dimension: "latency", ObjectClass: "relation",
		Scope:   "schema",
		Related: []string{"seq_scan_heavy"},
	},
	"partition_seq_scan_heavy": {
		Severity: "warn", CriticalWhen: "",
		Dimension: "latency", ObjectClass: "relation",
		Scope:   "workload",
		Related: []string{"seq_scan_heavy"},
	},
	"seq_scan_heavy": {
		Severity: "warn", CriticalWhen: "",
		Dimension: "throughput", ObjectClass: "relation",
		Scope:   "workload",
		Related: []string{"fk_unindexed", "stale_statistics"},
	},
	"autovacuum_disabled_on_table": {
		Severity: "critical", CriticalWhen: "",
		Dimension: "risk", ObjectClass: "relation",
		Scope:   "schema",
		Related: []string{"table_never_vacuumed", "autovacuum_starved", "txid_wraparound"},
	},
	"table_never_vacuumed": {
		Severity: "warn", CriticalWhen: "",
		Dimension: "risk", ObjectClass: "relation",
		Scope:   "workload",
		Related: []string{"autovacuum_disabled_on_table", "never_analyzed"},
	},
	"autovacuum_starved": {
		Severity: "warn", CriticalWhen: "",
		Dimension: "storage", ObjectClass: "relation",
		Scope:   "workload",
		Related: []string{"table_bloat", "autovacuum_saturated"},
	},
	"autovacuum_saturated": {
		Severity: "warn", CriticalWhen: "point-in-time; confirm it holds across runs",
		Dimension: "throughput", ObjectClass: "cluster",
		Scope:   "workload",
		Related: []string{"autovacuum_starved", "autovacuum_long_running"},
	},
	"autovacuum_long_running": {
		Severity: "info", CriticalWhen: "",
		Dimension: "throughput", ObjectClass: "cluster",
		Scope:   "workload",
		Related: []string{"autovacuum_saturated"},
	},
	"stale_statistics": {
		Severity: "warn", CriticalWhen: "",
		Dimension: "latency", ObjectClass: "relation",
		Scope:   "workload",
		Related: []string{"query_slowdown", "never_analyzed"},
	},
	"never_analyzed": {
		Severity: "warn", CriticalWhen: "",
		Dimension: "latency", ObjectClass: "relation",
		Scope:   "workload",
		Related: []string{"stale_statistics", "table_never_vacuumed"},
	},
	"low_hot_update_ratio": {
		Severity: "warn", CriticalWhen: "",
		Dimension: "throughput", ObjectClass: "relation",
		Scope:    "workload",
		Requires: []string{"track_counts (default on)"},
		Related:  []string{"table_bloat", "unused_indexes"},
	},
	"low_cache_hit": {
		Severity: "warn", CriticalWhen: "",
		Dimension: "throughput", ObjectClass: "cluster",
		Scope:   "workload",
		Related: []string{"seq_scan_heavy", "wait_io_bound"},
	},
	"idle_in_transaction": {
		Severity: "warn", CriticalWhen: "escalates with the number and age of idle-in-transaction sessions",
		Dimension: "risk", ObjectClass: "cluster",
		Scope:   "workload",
		Related: []string{"long_running_transaction", "vacuum_horizon_blocked"},
	},
	"long_running_transaction": {
		Severity: "warn", CriticalWhen: "",
		Dimension: "risk", ObjectClass: "cluster",
		Scope:   "workload",
		Related: []string{"idle_in_transaction", "vacuum_horizon_blocked"},
	},
	"wait_lock_contention": {
		Severity: "warn", CriticalWhen: "",
		Dimension: "latency", ObjectClass: "cluster",
		Scope:    "workload",
		Requires: []string{"ASH sampling (ash-hz>0)"},
		Related:  []string{"blocking_chains"},
	},
	"wait_io_bound": {
		Severity: "warn", CriticalWhen: "",
		Dimension: "throughput", ObjectClass: "cluster",
		Scope:    "workload",
		Requires: []string{"ASH sampling (ash-hz>0)", "track_io_timing"},
		Related:  []string{"low_cache_hit", "seq_scan_heavy"},
	},
	"wait_lwlock_pressure": {
		Severity: "warn", CriticalWhen: "",
		Dimension: "throughput", ObjectClass: "cluster",
		Scope:    "workload",
		Requires: []string{"ASH sampling (ash-hz>0)"},
		Related:  []string{"checkpoints_forced"},
	},
	"connection_saturation": {
		Severity: "warn", CriticalWhen: "at/near max_connections",
		Dimension: "risk", ObjectClass: "cluster",
		Scope:   "workload",
		Related: []string{"connections_overprovisioned", "idle_in_transaction"},
	},
	"txid_wraparound": {
		Severity: "warn", CriticalWhen: "XID age past the critical threshold (approaching the 2.1B wall)",
		Dimension: "risk", ObjectClass: "cluster",
		Scope:   "infra",
		Related: []string{"autovacuum_off", "vacuum_horizon_blocked", "mxid_wraparound"},
	},
	"sequence_exhaustion": {
		Severity: "warn", CriticalWhen: "a sequence is ≥90% consumed",
		Dimension: "risk", ObjectClass: "relation",
		Scope:   "workload",
		Related: []string{"txid_wraparound", "int4_identity_column"},
	},
	"int4_identity_column": {
		Severity: "warn", CriticalWhen: "the column is int2 (wraps at 32767)",
		Dimension: "risk", ObjectClass: "relation",
		Scope:   "schema",
		Related: []string{"sequence_exhaustion"},
	},
	"mxid_wraparound": {
		Severity: "warn", CriticalWhen: "multixact age past the critical threshold",
		Dimension: "risk", ObjectClass: "cluster",
		Scope:   "infra",
		Related: []string{"txid_wraparound"},
	},
	"vacuum_horizon_blocked": {
		Severity: "warn", CriticalWhen: "the pinned XID age grows large (dimension escalates from storage to risk)",
		Dimension: "storage", ObjectClass: "cluster",
		Scope:   "infra",
		Related: []string{"idle_in_transaction", "long_running_transaction", "prepared_xact_abandoned", "replication_slot_inactive"},
	},
	"prepared_xact_abandoned": {
		Severity: "warn", CriticalWhen: "a prepared (2PC) transaction open over an hour",
		Dimension: "risk", ObjectClass: "cluster",
		Scope:   "infra",
		Related: []string{"vacuum_horizon_blocked"},
	},
	"sync_rep_degraded": {
		Severity: "critical", CriticalWhen: "",
		Dimension: "risk", ObjectClass: "cluster",
		Scope:    "infra",
		Requires: []string{"replication", "synchronous_standby_names set"},
		Related:  []string{"replica_lag_time", "replica_disconnected"},
	},
	"replica_lag_time": {
		Severity: "warn", CriticalWhen: "replay lag past the critical threshold",
		Dimension: "risk", ObjectClass: "cluster",
		Scope:      "infra",
		Thresholds: []string{"replica_lag_warn_seconds"},
		Requires:   []string{"replication", "WAL flowing"},
		Related:    []string{"sync_rep_degraded", "recovery_conflicts"},
	},
	"recovery_conflicts": {
		Severity: "warn", CriticalWhen: "",
		Dimension: "risk", ObjectClass: "cluster",
		Scope:    "infra",
		Requires: []string{"standby"},
		Related:  []string{"replica_lag_time"},
	},
	"replica_disconnected": {
		Severity: "warn", CriticalWhen: "",
		Dimension: "risk", ObjectClass: "cluster",
		Scope:    "history",
		Requires: []string{"replication"},
		Related:  []string{"sync_rep_degraded", "replication_slot_inactive"},
	},
	"checksum_failures": {
		Severity: "critical", CriticalWhen: "",
		Dimension: "risk", ObjectClass: "db",
		Scope:    "infra",
		Requires: []string{"PG12+", "data_checksums=on"},
		Related:  []string{"ignore_checksum_failure_on", "checksums_disabled"},
	},
	"ignore_checksum_failure_on": {
		Severity: "critical", CriticalWhen: "",
		Dimension: "risk", ObjectClass: "setting",
		Scope:   "infra",
		Related: []string{"checksum_failures", "checksums_disabled"},
	},
	"checksums_disabled": {
		Severity: "info", CriticalWhen: "",
		Dimension: "risk", ObjectClass: "setting",
		Scope:    "infra",
		Requires: []string{"PG12+"},
		Related:  []string{"checksum_failures"},
	},
	"pgaudit_silent": {
		Severity: "warn", CriticalWhen: "",
		Dimension: "risk", ObjectClass: "setting",
		Scope:    "infra",
		Requires: []string{"pgaudit"},
		Related:  []string{"pgaudit_logs_parameters", "pgaudit_double_logging"},
	},
	"pgaudit_logs_parameters": {
		Severity: "warn", CriticalWhen: "",
		Dimension: "risk", ObjectClass: "setting",
		Scope:    "infra",
		Requires: []string{"pgaudit"},
		Related:  []string{"pgaudit_silent"},
	},
	"pgaudit_double_logging": {
		Severity: "info", CriticalWhen: "",
		Dimension: "throughput", ObjectClass: "setting",
		Scope:    "infra",
		Requires: []string{"pgaudit"},
		Related:  []string{"pgaudit_silent"},
	},
	"archiving_failing": {
		Severity: "critical", CriticalWhen: "downgraded to info on a managed provider",
		Dimension: "risk", ObjectClass: "cluster",
		Scope:    "infra",
		Requires: []string{"primary", "archive_mode=on"},
		Related:  []string{"archiving_stalled", "replication_slot_inactive"},
	},
	"archiving_stalled": {
		Severity: "critical", CriticalWhen: "downgraded to info on a managed provider",
		Dimension: "risk", ObjectClass: "cluster",
		Scope:    "infra",
		Requires: []string{"primary", "archive_mode=on"},
		Related:  []string{"archiving_failing"},
	},
	"archiving_disabled": {
		Severity: "warn", CriticalWhen: "downgraded to info on a managed provider",
		Dimension: "risk", ObjectClass: "cluster",
		Scope:    "infra",
		Requires: []string{"primary"},
		Related:  []string{"archiving_failing"},
	},
	"replication_slot_inactive": {
		Severity: "warn", CriticalWhen: "retained WAL past the critical size",
		Dimension: "risk", ObjectClass: "slot",
		Scope:   "infra",
		Related: []string{"vacuum_horizon_blocked", "replica_disconnected"},
	},
	"subscription_worker_down": {
		Severity: "warn", CriticalWhen: "",
		Dimension: "risk", ObjectClass: "sub",
		Scope:    "infra",
		Requires: []string{"logical replication"},
		Related:  []string{"replica_disconnected"},
	},
	"query_slowdown": {
		Severity: "warn", CriticalWhen: "",
		Dimension: "latency", ObjectClass: "cluster",
		Scope:    "history",
		Requires: []string{"pg_stat_statements", "a baseline snapshot"},
		Related:  []string{"stale_statistics"},
	},
	"crdb_execution_insights": {
		Severity: "warn", CriticalWhen: "",
		Dimension: "latency", ObjectClass: "query", Scope: ScopeWorkload,
		Requires: []string{"CockroachDB execution insights"},
		Related:  []string{"crdb_retry_hotspot", "crdb_index_recommendations"},
	},
	"crdb_index_recommendations": {
		Severity: "info", CriticalWhen: "",
		Dimension: "latency", ObjectClass: "query", Scope: ScopeWorkload,
		Requires: []string{"CockroachDB execution insights"},
		Related:  []string{"crdb_execution_insights"},
	},
	"crdb_unused_indexes": {
		Severity: "warn", CriticalWhen: "",
		Dimension: "throughput", ObjectClass: "relation", Scope: ScopeWorkload,
		Requires: []string{"CockroachDB cluster-wide index usage statistics"},
		Related:  []string{"crdb_index_recommendations"},
	},
	"crdb_table_metadata_error": {
		Severity: "warn", CriticalWhen: "",
		Dimension: "risk", ObjectClass: "relation", Scope: ScopeInfra,
		Requires: []string{"CockroachDB Admin API table metadata"},
		Related:  []string{"crdb_job_failed"},
	},
	"crdb_table_stats_missing": {
		Severity: "warn", CriticalWhen: "",
		Dimension: "latency", ObjectClass: "relation", Scope: ScopeWorkload,
		Requires: []string{"CockroachDB Admin API table metadata"},
		Related:  []string{"crdb_auto_stats_disabled", "crdb_execution_insights", "crdb_job_failed"},
	},
	"crdb_auto_stats_disabled": {
		Severity: "info", CriticalWhen: "",
		Dimension: "latency", ObjectClass: "relation", Scope: ScopeInfra,
		Requires: []string{"CockroachDB Admin API table metadata"},
		Related:  []string{"crdb_table_stats_missing"},
	},
	"crdb_mvcc_garbage_pressure": {
		Severity: "warn", CriticalWhen: "",
		Dimension: "storage", ObjectClass: "relation", Scope: ScopeWorkload,
		Requires: []string{"CockroachDB Admin API table metadata"},
		Related:  []string{"crdb_store_capacity"},
	},
	"crdb_long_running_query": {
		Severity: "warn", CriticalWhen: "",
		Dimension: "latency", ObjectClass: "cluster", Scope: ScopeWorkload,
		Requires: []string{"SHOW CLUSTER QUERIES"},
		Related:  []string{"crdb_execution_insights"},
	},
	"crdb_retry_hotspot": {
		Severity: "warn", CriticalWhen: "",
		Dimension: "throughput", ObjectClass: "query", Scope: ScopeWorkload,
		Requires: []string{"SHOW CLUSTER QUERIES or CockroachDB statement statistics"},
		Related:  []string{"crdb_execution_insights"},
	},
	"crdb_node_unavailable": {
		Severity: "critical", CriticalWhen: "", Dimension: "risk", ObjectClass: "cluster", Scope: ScopeInfra,
		Requires: []string{"CockroachDB Admin API"}, Related: []string{"crdb_ranges_unavailable", "crdb_ranges_underreplicated"},
	},
	"crdb_ranges_unavailable": {
		Severity: "critical", CriticalWhen: "", Dimension: "risk", ObjectClass: "cluster", Scope: ScopeInfra,
		Requires: []string{"CockroachDB Admin API"}, Related: []string{"crdb_node_unavailable", "crdb_ranges_underreplicated"},
	},
	"crdb_ranges_underreplicated": {
		Severity: "warn", CriticalWhen: "", Dimension: "risk", ObjectClass: "cluster", Scope: ScopeInfra,
		Requires: []string{"CockroachDB Admin API"}, Related: []string{"crdb_node_unavailable", "crdb_ranges_unavailable", "crdb_store_capacity"},
	},
	"crdb_store_capacity": {
		Severity: "warn", CriticalWhen: "fullest store is at least 90% used", Dimension: "risk", ObjectClass: "cluster", Scope: ScopeInfra,
		Requires: []string{"CockroachDB Admin API"}, Related: []string{"crdb_ranges_underreplicated"},
	},
	"crdb_resource_pressure": {
		Severity: "warn", CriticalWhen: "", Dimension: "throughput", ObjectClass: "cluster", Scope: ScopeInfra,
		Requires: []string{"CockroachDB Admin API"}, Related: []string{"crdb_retry_hotspot"},
	},
	"crdb_job_failed": {
		Severity: "warn", CriticalWhen: "job state is revert-failed", Dimension: "risk", ObjectClass: "job", Scope: ScopeInfra,
		Requires: []string{"information_schema.crdb_jobs_with_progress"},
		Related:  []string{"crdb_job_stalled", "crdb_job_reverting"},
	},
	"crdb_job_stalled": {
		Severity: "warn", CriticalWhen: "", Dimension: "risk", ObjectClass: "job", Scope: ScopeInfra,
		Requires: []string{"information_schema.crdb_jobs_with_progress"},
		Related:  []string{"crdb_job_failed", "crdb_job_reverting", "crdb_resource_pressure"},
	},
	"crdb_job_reverting": {
		Severity: "info", CriticalWhen: "", Dimension: "risk", ObjectClass: "job", Scope: ScopeInfra,
		Requires: []string{"information_schema.crdb_jobs_with_progress"},
		Related:  []string{"crdb_job_failed", "crdb_job_stalled"},
	},
	"crdb_job_paused": {
		Severity: "info", CriticalWhen: "", Dimension: "risk", ObjectClass: "job", Scope: ScopeInfra,
		Requires: []string{"information_schema.crdb_jobs_with_progress"},
		Related:  []string{"crdb_job_stalled"},
	},
	"crdb_version_skew": {
		Severity: "warn", CriticalWhen: "", Dimension: "risk", ObjectClass: "cluster", Scope: ScopeInfra,
		Requires: []string{"CockroachDB Admin API"}, Related: []string{"crdb_node_unavailable"},
	},
	"crdb_replica_imbalance": {
		Severity: "warn", CriticalWhen: "", Dimension: "risk", ObjectClass: "store", Scope: ScopeInfra,
		Requires: []string{"CockroachDB Admin API store metrics"},
		Related:  []string{"crdb_ranges_underreplicated", "crdb_capacity_imbalance"},
	},
	"crdb_leaseholder_imbalance": {
		Severity: "info", CriticalWhen: "", Dimension: "throughput", ObjectClass: "store", Scope: ScopeInfra,
		Requires: []string{"CockroachDB Admin API store metrics"},
		Related:  []string{"crdb_hot_range_concentration", "crdb_resource_pressure"},
	},
	"crdb_capacity_imbalance": {
		Severity: "warn", CriticalWhen: "", Dimension: "risk", ObjectClass: "store", Scope: ScopeInfra,
		Requires: []string{"CockroachDB Admin API store metrics"},
		Related:  []string{"crdb_store_capacity", "crdb_replica_imbalance"},
	},
	"crdb_hot_range_concentration": {
		Severity: "warn", CriticalWhen: "", Dimension: "throughput", ObjectClass: "node", Scope: ScopeWorkload,
		Requires: []string{"CockroachDB Admin API hot-ranges endpoint"},
		Related:  []string{"crdb_contention_hotspot", "crdb_leaseholder_imbalance", "crdb_resource_pressure"},
	},
	"crdb_external_disk_usage": {
		Severity: "warn", CriticalWhen: "", Dimension: "risk", ObjectClass: "store", Scope: ScopeInfra,
		Requires: []string{"CockroachDB Admin API store metrics"},
		Related:  []string{"crdb_store_capacity", "crdb_capacity_imbalance", "crdb_storage_stall"},
	},
	"crdb_storage_stall": {
		Severity: "warn", CriticalWhen: "a disk-stalled event occurs", Dimension: "latency", ObjectClass: "store", Scope: ScopeInfra,
		Requires: []string{"two CockroachDB Admin API store-metric samples"},
		Related:  []string{"crdb_raft_backlog", "crdb_resource_pressure", "crdb_store_capacity"},
	},
	"crdb_replication_recovery": {
		Severity: "warn", CriticalWhen: "", Dimension: "risk", ObjectClass: "store", Scope: ScopeInfra,
		Requires: []string{"CockroachDB Admin API replication metrics"},
		Related:  []string{"crdb_ranges_underreplicated", "crdb_raft_backlog", "crdb_store_capacity"},
	},
	"crdb_raft_backlog": {
		Severity: "warn", CriticalWhen: "", Dimension: "latency", ObjectClass: "store", Scope: ScopeInfra,
		Requires: []string{"CockroachDB Admin API Raft metrics"},
		Related:  []string{"crdb_replication_recovery", "crdb_storage_stall", "crdb_node_unavailable"},
	},
	"crdb_replica_size_skew": {
		Severity: "warn", CriticalWhen: "", Dimension: "storage", ObjectClass: "store", Scope: ScopeInfra,
		Requires: []string{"CockroachDB Admin API MVCC and replica metrics"},
		Related:  []string{"crdb_replica_imbalance", "crdb_capacity_imbalance", "crdb_mvcc_garbage_pressure"},
	},
	"crdb_contention_hotspot": {
		Severity: "warn", CriticalWhen: "", Dimension: "latency", ObjectClass: "cluster", Scope: ScopeWorkload,
		Requires: []string{"crdb_internal.transaction_contention_events", "VIEWACTIVITY"},
		Related:  []string{"crdb_retry_hotspot", "crdb_serialization_conflicts"},
	},
	"crdb_serialization_conflicts": {
		Severity: "warn", CriticalWhen: "", Dimension: "throughput", ObjectClass: "cluster", Scope: ScopeWorkload,
		Requires: []string{"crdb_internal.transaction_contention_events", "VIEWACTIVITY"},
		Related:  []string{"crdb_contention_hotspot", "crdb_retry_hotspot"},
	},
	"pgss_entries_evicted": {
		Severity: "warn", CriticalWhen: "",
		Dimension: "throughput", ObjectClass: "cluster",
		Scope:    "history",
		Requires: []string{"pg_stat_statements"},
		Related:  []string{"pg_stat_statements_missing"},
	},
	"work_mem_low": {
		Severity: "warn", CriticalWhen: "",
		Dimension: "throughput", ObjectClass: "setting",
		Scope:   "workload",
		Related: []string{"work_mem_overcommit", "wait_io_bound"},
	},
	"checkpoints_forced": {
		Severity: "warn", CriticalWhen: "",
		Dimension: "throughput", ObjectClass: "setting",
		Scope:   "workload",
		Related: []string{"wait_lwlock_pressure"},
	},
	"connections_overprovisioned": {
		Severity: "info", CriticalWhen: "",
		Dimension: "cost", ObjectClass: "cluster",
		Scope:   "workload",
		Related: []string{"connection_saturation", "work_mem_overcommit"},
	},
	"fsync_off": {
		Severity: "critical", CriticalWhen: "",
		Dimension: "risk", ObjectClass: "setting",
		Scope:   "infra",
		Related: []string{"full_page_writes_off"},
	},
	"full_page_writes_off": {
		Severity: "critical", CriticalWhen: "",
		Dimension: "risk", ObjectClass: "setting",
		Scope:   "infra",
		Related: []string{"fsync_off"},
	},
	"autovacuum_off": {
		Severity: "critical", CriticalWhen: "",
		Dimension: "risk", ObjectClass: "setting",
		Scope:   "infra",
		Related: []string{"txid_wraparound", "autovacuum_disabled_on_table"},
	},
	"random_page_cost_high": {
		Severity: "warn", CriticalWhen: "",
		Dimension: "latency", ObjectClass: "setting",
		Scope:    "infra",
		Requires: []string{"a managed/SSD provider"},
		Related:  []string{"seq_scan_heavy"},
	},
	"work_mem_overcommit": {
		Severity: "warn", CriticalWhen: "",
		Dimension: "risk", ObjectClass: "setting",
		Scope:   "infra",
		Related: []string{"work_mem_low", "connections_overprovisioned"},
	},
	"statement_timeout_unset": {
		Severity: "info", CriticalWhen: "",
		Dimension: "risk", ObjectClass: "setting",
		Scope:   "infra",
		Related: []string{"long_running_transaction", "idle_in_transaction"},
	},
	"io_timing_off": {
		Severity: "info", CriticalWhen: "",
		Dimension: "throughput", ObjectClass: "setting",
		Scope:   "infra",
		Related: []string{"wait_io_bound"},
	},
	"high_rollback_ratio": {
		Severity: "warn", CriticalWhen: "",
		Dimension: "throughput", ObjectClass: "cluster",
		Scope: "workload",
	},
	"pg_stat_statements_missing": {
		Severity: "info", CriticalWhen: "",
		Dimension: "cost", ObjectClass: "cluster",
		Scope:   "infra",
		Related: []string{"pgss_entries_evicted"},
	},
	"stale_stats_window": {
		Severity: "info", CriticalWhen: "",
		Dimension: "cost", ObjectClass: "cluster",
		Scope:   "workload",
		Related: []string{"query_slowdown"},
	},
}

// metaFindings are config-hygiene notes the suppression system emits about
// ITSELF (B2-3), not diagnoses of the database. They live in the findings list a
// user sees, but their home documentation is docs/configuration.md ("Keeping
// suppressions honest"), not the per-finding catalogue — so the catalogue
// coverage guard exempts them.
var metaFindings = map[string]bool{
	"suppression_expired": true,
	"suppression_unused":  true,
}

// IsMetaFinding reports whether id is a config-hygiene meta-finding (documented
// in configuration.md rather than the findings catalogue).
func IsMetaFinding(id string) bool { return metaFindings[id] }

// MetaFor returns the metadata for a finding id.
func MetaFor(id string) (Meta, bool) { m, ok := catalog[id]; return m, ok }

// CatalogIDs returns the ids that currently have a Meta entry.
func CatalogIDs() []string {
	ids := make([]string, 0, len(catalog))
	for id := range catalog {
		ids = append(ids, id)
	}
	return ids
}

// FindingObjectClass returns the suppression class a finding is keyed by: for an
// aggregate (per-row Objects populated) it's the row class; otherwise the
// finding-level Object's class.
func FindingObjectClass(f model.Finding) string {
	if len(f.Objects) > 0 {
		return ObjectClass(f.Objects[0])
	}
	return ObjectClass(f.Object)
}

// ObjectClass maps a finding's Object string to its suppression class — the
// value a docs page declares and `config` reasons about.
func ObjectClass(object string) string {
	switch {
	case object == "":
		return "cluster"
	case strings.HasPrefix(object, "setting:"):
		return "setting"
	case strings.HasPrefix(object, "slot:"):
		return "slot"
	case strings.HasPrefix(object, "sub:"):
		return "sub"
	case strings.HasPrefix(object, "q:"):
		return "query"
	case strings.HasPrefix(object, "db:"):
		return "db"
	case strings.Contains(object, "."):
		return "relation"
	default:
		return "cluster"
	}
}

// summaries are one-line descriptions for the catalogue index (docs/findings/
// README.md), grouped by dimension. Kept terse — the page has the depth.
var summaries = map[string]string{
	"blocking_chains":              "one session is blocked waiting on locks held by another",
	"index_invalid":                "a failed CREATE INDEX CONCURRENTLY left an invalid index — critical if it's still maintained on writes, warn if it's failed-build debris",
	"unused_indexes":               "indexes with zero scans — storage and write cost, no reads served",
	"table_bloat":                  "dead tuples make a table far larger on disk than its live rows",
	"redundant_indexes":            "an index whose columns are a leading prefix of another",
	"fk_unindexed":                 "a foreign key with no index — slow joins and cascade checks",
	"partition_seq_scan_heavy":     "a partitioned table read end-to-end across its partitions",
	"seq_scan_heavy":               "a large table read mostly by full scans instead of index lookups",
	"autovacuum_disabled_on_table": "autovacuum_enabled=false in reloptions — a table rotting invisibly",
	"table_never_vacuumed":         "a sizable table with no vacuum on record",
	"autovacuum_starved":           "dead tuples past the trigger while autovacuum falls behind",
	"autovacuum_saturated":         "every autovacuum worker busy at the moment sampled",
	"autovacuum_long_running":      "an autovacuum worker has been running over an hour",
	"stale_statistics":             "planner statistics far behind the data — the usual cause of a plan flip",
	"never_analyzed":               "a table with no statistics at all — the planner guesses",
	"low_hot_update_ratio":         "few updates are HOT, so each rewrites every index",
	"low_cache_hit":                "a sustained low buffer cache hit ratio — disk-bound reads",
	"idle_in_transaction":          "sessions idle inside an open transaction, holding locks and the xmin horizon",
	"long_running_transaction":     "a transaction open far too long, pinning vacuum",
	"wait_lock_contention":         "time spent waiting on heavyweight locks (ASH)",
	"wait_io_bound":                "time spent waiting on storage IO (ASH)",
	"wait_lwlock_pressure":         "time spent on lightweight-lock contention (ASH)",
	"connection_saturation":        "connections approaching max_connections",
	"txid_wraparound":              "transaction-id age climbing toward the 2.1-billion read-only wall",
	"sequence_exhaustion":          "a sequence near its ceiling — the next insert will error",
	"int4_identity_column":         "a sequence-backed int2/int4 column that will wrap (int4 at 2.1B) regardless of current value",
	"mxid_wraparound":              "multixact-id age climbing toward its own wraparound wall",
	"vacuum_horizon_blocked":       "something pins the xmin horizon so vacuum can't reclaim",
	"prepared_xact_abandoned":      "a prepared (2PC) transaction left open, blocking vacuum forever",
	"sync_rep_degraded":            "fewer synchronous standbys connected than the config requires",
	"replica_lag_time":             "a replica's replay lag has grown past the threshold",
	"recovery_conflicts":           "queries on a standby cancelled by recovery conflicts",
	"replica_disconnected":         "a streaming standby that was present has dropped off",
	"checksum_failures":            "Postgres read a page whose checksum didn't match — likely corruption",
	"ignore_checksum_failure_on":   "ignore_checksum_failure is on — corrupt pages are returned, not caught",
	"checksums_disabled":           "data checksums are off, so this class of corruption is silent",
	"pgaudit_silent":               "pgaudit is installed but pgaudit.log selects no classes — the audit trail does not exist",
	"pgaudit_logs_parameters":      "pgaudit.log_parameter=on writes bind parameters (passwords, PII) into the server log",
	"pgaudit_double_logging":       "pgaudit and log_statement=all record every statement twice — duplicate log volume",
	"archiving_failing":            "WAL archiving is failing — the archive is falling behind or broken",
	"archiving_stalled":            "WAL archiving hasn't advanced despite WAL being generated",
	"archiving_disabled":           "archive_mode is off — no continuous WAL archive for PITR",
	"replication_slot_inactive":    "an inactive replication slot is pinning WAL from removal",
	"subscription_worker_down":     "a logical subscription's apply worker isn't running",
	"query_slowdown":               "a query's mean time regressed sharply versus the baseline",
	"pgss_entries_evicted":         "pg_stat_statements is evicting entries, biasing the top list",
	"work_mem_low":                 "queries spill sorts/hashes to temp files — work_mem too small",
	"checkpoints_forced":           "too many checkpoints forced by WAL volume — raise max_wal_size",
	"connections_overprovisioned":  "max_connections far above what's used — wasted memory reservation",
	"fsync_off":                    "fsync is off — a crash can irrecoverably corrupt the database",
	"full_page_writes_off":         "full_page_writes off — a crash can leave torn pages",
	"autovacuum_off":               "autovacuum is off — bloat and wraparound will follow",
	"random_page_cost_high":        "random_page_cost tuned for spinning disks on SSD storage",
	"work_mem_overcommit":          "work_mem × max_connections exceeds effective_cache_size — OOM risk",
	"statement_timeout_unset":      "no cluster-wide statement_timeout — a runaway query can run forever",
	"io_timing_off":                "track_io_timing off, so per-query IO time is unavailable",
	"high_rollback_ratio":          "an unusually high share of transactions roll back",
	"pg_stat_statements_missing":   "pg_stat_statements isn't enabled — no query-level visibility",
	"stale_stats_window":           "the cumulative statistics window is very old — rates are near-meaningless",
	"crdb_execution_insights":      "CockroachDB recorded slow or failed executions and their classified causes",
	"crdb_index_recommendations":   "CockroachDB's optimizer attached candidate indexes to problematic executions",
	"crdb_unused_indexes":          "non-unique CockroachDB secondary indexes have no reads across the observation threshold",
	"crdb_table_metadata_error":    "CockroachDB could not refresh cached size and range metadata for one or more tables",
	"crdb_table_stats_missing":     "substantial CockroachDB tables have no optimizer statistics",
	"crdb_auto_stats_disabled":     "automatic optimizer-statistics collection is disabled on substantial tables",
	"crdb_mvcc_garbage_pressure":   "large CockroachDB tables retain a substantial non-live MVCC footprint",
	"crdb_long_running_query":      "a CockroachDB statement has remained active for over one minute",
	"crdb_retry_hotspot":           "live or persisted query executions repeatedly retried serializable transactions",
	"crdb_node_unavailable":        "one or more CockroachDB nodes are dead, unavailable, or unknown",
	"crdb_ranges_unavailable":      "range replicas report unavailable and may reject access to affected keys",
	"crdb_ranges_underreplicated":  "ranges have less replica redundancy than their configured target",
	"crdb_store_capacity":          "the fullest CockroachDB store is running out of capacity headroom",
	"crdb_resource_pressure":       "node CPU, memory, or admission delay indicates resource pressure",
	"crdb_job_failed":              "a recent CockroachDB background job failed or failed while reverting",
	"crdb_job_stalled":             "an active CockroachDB background job stopped reporting progress or status updates",
	"crdb_job_reverting":           "CockroachDB is actively reverting an operation that did not complete",
	"crdb_job_paused":              "a CockroachDB background job is paused and will not progress until acted upon",
	"crdb_version_skew":            "live CockroachDB nodes are running different binary versions",
	"crdb_replica_imbalance":       "range replicas are unevenly distributed across comparable live stores",
	"crdb_leaseholder_imbalance":   "leaseholders are heavily skewed across comparable live stores",
	"crdb_capacity_imbalance":      "live stores have materially different capacity utilization",
	"crdb_hot_range_concentration": "one leaseholder node dominates the bounded top-hot-range CPU sample",
	"crdb_external_disk_usage":     "full store volumes contain substantial usage outside CockroachDB's active store",
	"crdb_storage_stall":           "CockroachDB observed slow, stalled, or write-stalled storage during the sample",
	"crdb_replication_recovery":    "a material replica recovery backlog is uninitialized or blocked",
	"crdb_raft_backlog":            "Raft proposals or follower replication are materially backlogged",
	"crdb_replica_size_skew":       "logical MVCC bytes per replica are heavily skewed across live stores",
	"crdb_contention_hotspot":      "recent lock contention accumulated material wait time on a table or index",
	"crdb_serialization_conflicts": "transactions repeatedly encountered serialization conflicts",
}

// Summary returns the one-line catalogue description for a finding id.
func Summary(id string) string { return summaries[id] }
