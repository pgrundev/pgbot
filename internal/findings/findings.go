// Package findings computes deterministic, rule-based diagnoses over a
// model.Context. This is where analysis lives — NOT the LLM. Every rule is
// computable in Go from signals already in the Context; the LLM layer (a later
// slice) explains and prioritises these findings, it does not generate them.
package findings

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pgrundev/pgbot/internal/model"
)

// Thresholds for the rules, in one place so they read like the docs.
const (
	unusedIndexMinBytes   = 1 << 20 // 1 MiB: below this an unused index isn't worth flagging
	deadRatioWarn         = 0.20    // 20% dead tuples on a sizable table
	deadRatioTableMinRows = 10000
	cacheHitWarn          = 0.90 // sustained cache hit below this is disk-bound
	longXactWarnSec       = 300  // a transaction open > 5 min
	idleInTxnWarnSec      = 60
	preparedXactWarnSec   = 300   // a prepared (2PC) transaction open > 5 min is likely abandoned
	preparedXactCritSec   = 3600  // > 1h — it never times out; blocks vacuum until resolved
	rollbackRatioWarn     = 0.10  // >10% of transactions rolling back
	rollbackMinTxns       = 20    // below this many transactions in the window the ratio is noise
	staleStatsWarnDays    = 30    // rates computed over a very old window are near-meaningless
	seqScanTableMinRows   = 50000 // only flag seq-scan-heavy on tables big enough to matter
	partitionSeqScanMin   = 1000  // aggregate seq scans across a partitioned table's partitions
	hotUpdateRatioWarn    = 0.50  // below half of updates being HOT points at fillfactor / an indexed hot column
	hotUpdateMinVolume    = 10000 // enough update volume to judge the ratio (not a cold table)
	autovacuumLongRunSec  = 3600  // an autovacuum worker running > 1h (info)

	// Wait-profile (ASH) thresholds. All gated on model.WaitMinSamples — below
	// that the shares are noise and no wait finding fires.
	waitLockContentionShare = 0.30 // a query with >30% of ITS samples on Lock:*
	waitLockQueryMinSamples = 5    // ignore a query seen in only a sample or two
	waitIOBoundShare        = 0.50 // >50% of the whole window on IO:*
	waitLWLockShare         = 0.30 // a single LWLock:* event dominating the window

	// Impact/confidence horizons (T9).
	shortStatsWindowSec = 7 * 24 * 3600 // < 7 days of stats: unused-index confidence capped low

	// Connection saturation (used / max_connections).
	connSaturationWarn = 0.85
	connSaturationCrit = 0.95

	// Transaction-ID wraparound: age(datfrozenxid) climbing toward the ~2.1B wall
	// past which Postgres refuses writes. Healthy clusters stay under autovacuum's
	// 200M freeze trigger, so these thresholds only fire when vacuum is failing.
	xidWraparoundWarn = 1_000_000_000
	xidWraparoundCrit = 1_800_000_000
	xidWraparoundWall = 2_147_483_647

	// Vacuum horizon: an xmin held this many transactions behind the current xid
	// is a real block on dead-tuple reclamation (not a momentary query).
	vacuumHorizonWarnXIDs = 1_000_000

	// Sequence exhaustion (last_value / effective ceiling).
	seqExhaustionWarn = 0.80
	seqExhaustionCrit = 0.90

	// WAL archiving: pg_wal size at which broken archiving becomes a compound
	// disk-fill emergency (unarchived WAL can't be recycled).
	walDirCompoundBytes int64 = 10 << 30 // 10 GiB
	archiveStallFloorS        = 3600     // stalled-archiving floor: 1h

	// Replica replay lag (time-based failover RPO).
	replicaLagWarnSec = 60.0
	replicaLagCritSec = 300.0

	// Query slowdown vs the baseline (the "what changed" finding).
	querySlowdownFactor       = 2.0  // at least 2× slower
	querySlowdownMinMs        = 10.0 // and the new mean ≥ 10ms, so micro-queries aren't noise
	crdbLongQuerySec          = 60.0 // live statement running for at least one minute
	crdbRetryHotspotMin       = 3    // max retries observed for one statement fingerprint
	crdbStoreWarn             = 0.80 // store capacity used
	crdbStoreCrit             = 0.90
	crdbCPUWarn               = 90.0 // normalized percent on the busiest node
	crdbMemoryWarn            = 0.90
	crdbAdmissionWarnMS       = 100.0
	crdbContentionTotalWarnMS = 5_000.0
	crdbContentionMaxWarnMS   = 1_000.0
	crdbSerializationWarn     = 5
	crdbStatsTableMinBytes    = int64(64 << 20)
	crdbMVCCGarbageMinBytes   = int64(1 << 30)
	crdbMVCCLiveRatioWarn     = 0.50
	crdbJobStalledSec         = int64(30 * 60)
	crdbBalanceMinStores      = 3
	crdbReplicaMeanMin        = 100.0
	crdbReplicaSpreadMin      = int64(100)
	crdbReplicaMaxMeanWarn    = 1.35
	crdbReplicaMinMeanWarn    = 0.65
	crdbLeaseMeanMin          = 50.0
	crdbLeaseSpreadMin        = int64(100)
	crdbLeaseMaxMeanWarn      = 1.50
	crdbLeaseMinMeanWarn      = 0.50
	crdbCapacityImbalanceMax  = 0.70
	crdbCapacitySpreadWarn    = 0.20
	crdbHotRangeMinSamples    = 5
	crdbHotRangeMinCPUCores   = 0.50
	crdbHotRangeShareWarn     = 0.60
	crdbHotRangeLeaderMin     = 3
	crdbExternalDiskUsedMin   = 0.80
	crdbExternalDiskRatioMin  = 0.20
	crdbExternalDiskBytesMin  = int64(10 << 30)
	crdbUninitializedMin      = int64(10)
	crdbUninitializedRatioMin = 0.02
	crdbReplicaPurgatoryMin   = int64(10)
	crdbSnapshotQueueMin      = int64(10)
	crdbRaftPendingStoreWarn  = int64(100)
	crdbRaftFlowRatioWarn     = 0.01
	crdbRaftDroppedPerSecWarn = 100.0
	crdbReplicaBytesMeanMin   = float64(64 << 20)
	crdbReplicaBytesMaxMean   = 1.75
	crdbReplicaBytesMinMean   = 0.60

	// Replication-slot WAL retention. An inactive slot pins WAL from its restart
	// point; the retained log grows until the consumer reconnects or the slot is
	// dropped — a classic way to fill the data disk. Small brief-reconnect gaps
	// aren't worth flagging, so warn only once retention is material.
	slotRetainWarnBytes int64 = 512 << 20 // 512 MiB retained by an inactive slot
	slotRetainCritBytes int64 = 8 << 30   // 8 GiB → serious disk-fill risk

	// Config-tuning thresholds (the `pgbot tune` findings).
	tuneTempSpillBytesPerSec = 1 << 20 // 1 MiB/s of temp files → work_mem too small
	tuneForcedCheckpointFrac = 0.30    // >30% of checkpoints forced by WAL → max_wal_size too small
	tuneForcedCheckpointMin  = 10      // need enough checkpoints to judge the ratio
	tuneConnOverprovMax      = 200     // only flag over-provisioning above this max_connections
	tuneConnOverprovUseFrac  = 0.15    // used < 15% of max → over-provisioned
)

// TuningIDs identifies the config-recommendation findings, surfaced together by
// `pgbot tune`.
var TuningIDs = map[string]bool{
	"work_mem_low":                true,
	"checkpoints_forced":          true,
	"connections_overprovisioned": true,
	"io_timing_off":               true,
	"fsync_off":                   true,
	"full_page_writes_off":        true,
	"autovacuum_off":              true,
	"random_page_cost_high":       true,
	"statement_timeout_unset":     true,
	"work_mem_overcommit":         true,
}

// knownIDs is every finding ID Compute can emit. It is the whitelist the config
// layer validates [severity]/[[ignore]] rules against — an ID not here is a typo
// (loud warning, never a hard error, so a config for a newer pgbot still loads).
// TestKnownIDs_matchesCompute guards it against drift as findings are added.
var knownIDs = map[string]bool{
	"blocking_chains": true, "index_invalid": true, "unused_indexes": true,
	"table_bloat": true, "redundant_indexes": true, "fk_unindexed": true,
	"partition_seq_scan_heavy": true, "seq_scan_heavy": true,
	"autovacuum_disabled_on_table": true, "table_never_vacuumed": true,
	"autovacuum_starved": true, "autovacuum_saturated": true,
	"autovacuum_long_running": true, "stale_statistics": true, "never_analyzed": true,
	"low_hot_update_ratio": true, "low_cache_hit": true, "idle_in_transaction": true,
	"long_running_transaction": true, "wait_lock_contention": true,
	"wait_io_bound": true, "wait_lwlock_pressure": true, "connection_saturation": true,
	"txid_wraparound": true, "sequence_exhaustion": true, "int4_identity_column": true,
	"mxid_wraparound":        true,
	"vacuum_horizon_blocked": true, "prepared_xact_abandoned": true,
	"sync_rep_degraded": true, "replica_lag_time": true, "recovery_conflicts": true,
	"replica_disconnected": true, "checksum_failures": true,
	"ignore_checksum_failure_on": true, "checksums_disabled": true,
	"archiving_failing": true, "archiving_stalled": true, "archiving_disabled": true,
	"replication_slot_inactive": true, "subscription_worker_down": true,
	"query_slowdown": true, "pgss_entries_evicted": true, "work_mem_low": true,
	"checkpoints_forced": true, "connections_overprovisioned": true, "fsync_off": true,
	"full_page_writes_off": true, "autovacuum_off": true, "random_page_cost_high": true,
	"work_mem_overcommit": true, "statement_timeout_unset": true, "io_timing_off": true,
	"high_rollback_ratio": true, "pg_stat_statements_missing": true,
	"stale_stats_window":      true,
	"crdb_execution_insights": true, "crdb_index_recommendations": true,
	"crdb_unused_indexes":       true,
	"crdb_table_metadata_error": true, "crdb_table_stats_missing": true,
	"crdb_auto_stats_disabled": true, "crdb_mvcc_garbage_pressure": true,
	"crdb_long_running_query": true, "crdb_retry_hotspot": true,
	"crdb_node_unavailable": true, "crdb_ranges_unavailable": true,
	"crdb_ranges_underreplicated": true, "crdb_store_capacity": true,
	"crdb_resource_pressure": true, "crdb_job_failed": true, "crdb_job_stalled": true,
	"crdb_job_reverting": true, "crdb_job_paused": true, "crdb_version_skew": true,
	"crdb_replica_imbalance": true, "crdb_leaseholder_imbalance": true,
	"crdb_capacity_imbalance": true, "crdb_hot_range_concentration": true,
	"crdb_external_disk_usage": true, "crdb_storage_stall": true,
	"crdb_replication_recovery": true, "crdb_raft_backlog": true,
	"crdb_replica_size_skew":  true,
	"crdb_contention_hotspot": true, "crdb_serialization_conflicts": true,
	"pgaudit_silent": true, "pgaudit_logs_parameters": true, "pgaudit_double_logging": true,
	// B2 meta-findings (the suppression system reporting on itself).
	"suppression_expired": true, "suppression_unused": true,
}

// KnownID reports whether id is a finding pgbot can emit (config validation).
func KnownID(id string) bool { return knownIDs[id] }

// Tunables are the thresholds a user config may override ([thresholds] in
// .pgbot.toml). Only these keys are wired — everything else stays a compiled-in
// const (an unknown key in a config is a loud warning, not a silent no-op).
// Applied BEFORE the finding is produced, so a raised threshold means the
// finding is never generated at all (B2-2 precedence rule 1).
type Tunables struct {
	UnusedIndexMinBytes int64   // unused_index_min_size_mb × 1MiB
	DeadRatioWarn       float64 // dead_ratio_warn
	ReplicaLagWarnSec   float64 // replica_lag_warn_seconds
}

// DefaultTunables returns the compiled-in thresholds.
func DefaultTunables() Tunables {
	return Tunables{
		UnusedIndexMinBytes: unusedIndexMinBytes,
		DeadRatioWarn:       deadRatioWarn,
		ReplicaLagWarnSec:   replicaLagWarnSec,
	}
}

// Compute returns findings sorted most-severe first, using the default
// thresholds. Order among equal severities is stable by ID.
func Compute(c *model.Context) []model.Finding {
	return ComputeWithTunables(c, DefaultTunables())
}

// ComputeWithTunables is Compute with caller-supplied threshold overrides.
func ComputeWithTunables(c *model.Context, tun Tunables) []model.Finding {
	var f []model.Finding
	add := func(x model.Finding) { f = append(f, x) }

	blockingChains(c, add)
	invalidIndexes(c, add)
	unusedIndexes(c, add, tun)
	redundantIndexes(c, add)
	unindexedForeignKeys(c, add)
	seqScanHeavy(c, add)
	partitionSeqScanHeavy(c, add)
	bloatedTables(c, add, tun)
	staleStatistics(c, add)
	autovacuumHealth(c, add)
	lowHotUpdateRatio(c, add)
	lowCacheHit(c, add)
	idleInTransaction(c, add)
	longRunningXact(c, add)
	connectionSaturation(c, add)
	txidWraparound(c, add)
	mxidWraparound(c, add)
	sequenceExhaustion(c, add)
	int4IdentityColumn(c, add)
	walArchiving(c, add)
	checksumFindings(c, add)
	failoverReadiness(c, add, tun)
	replicationSlotRisk(c, add)
	subscriptionDown(c, add)
	vacuumHorizonBlocked(c, add)
	preparedXactAbandoned(c, add)
	querySlowdown(c, add)
	workMemLow(c, add)
	checkpointsForced(c, add)
	connectionsOverprovisioned(c, add)
	ioTimingOff(c, add)
	configSanity(c, add)
	auditPosture(c, add)
	waitFindings(c, add)
	highRollbacks(c, add)
	missingPgss(c, add)
	pgssEntriesEvicted(c, add)
	staleStatsWindow(c, add)
	cockroachFindings(c, add)

	// T9 ordering: risk (time-to-incident) is pinned to the top — a wraparound or
	// an invalid index outranks any storage or latency win. Within that, sort by
	// Impact.Score descending. Severity breaks a score tie and still drives the
	// exit code, but it is no longer the primary key: an 8ms query run 40k times
	// can matter more than a warn that fires once.
	sort.SliceStable(f, func(i, j int) bool {
		ri, rj := isRisk(f[i]), isRisk(f[j])
		if ri != rj {
			return ri
		}
		if f[i].Impact.Score != f[j].Impact.Score {
			return f[i].Impact.Score > f[j].Impact.Score
		}
		if sev(f[i].Severity) != sev(f[j].Severity) {
			return sev(f[i].Severity) > sev(f[j].Severity)
		}
		return f[i].ID < f[j].ID
	})
	return f
}

func cockroachFindings(c *model.Context, add func(model.Finding)) {
	if c.Server.Engine != "cockroachdb" {
		return
	}
	crdbLongRunningQueries(c, add)
	crdbRetryHotspots(c, add)
	crdbExecutionInsights(c, add)
	crdbIndexRecommendations(c, add)
	crdbUnusedIndexes(c, add)
	crdbTableFindings(c, add)
	crdbClusterHealth(c, add)
	crdbStorageFindings(c, add)
	crdbDistributionFindings(c, add)
	crdbJobFindings(c, add)
	crdbContentionFindings(c, add)
}

func crdbStorageFindings(c *model.Context, add func(model.Finding)) {
	if c.Health == nil || c.Health.Cockroach == nil {
		return
	}
	s := &c.Health.Cockroach.Storage
	if s.Exactness == "" || s.Exactness == model.ExactnessUnavailable {
		return
	}

	var externalEvidence, externalObjects []string
	for _, store := range s.Stores {
		usedRatio := safeFindingRatio(float64(store.FilesystemUsedBytes), float64(store.CapacityBytes))
		if store.Status != "live" || usedRatio < crdbExternalDiskUsedMin || store.OtherUsedRatio < crdbExternalDiskRatioMin || store.OtherUsedBytes < crdbExternalDiskBytesMin {
			continue
		}
		externalEvidence = append(externalEvidence, fmt.Sprintf("s%d/n%d · filesystem %.1f%% used · CockroachDB %s · other use/overhead %s (%.1f%%)",
			store.StoreID, store.NodeID, usedRatio*100, humanBytes(store.CockroachUsedBytes), humanBytes(store.OtherUsedBytes), store.OtherUsedRatio*100))
		externalObjects = append(externalObjects, fmt.Sprintf("store:s%d", store.StoreID))
	}
	if len(externalEvidence) > 0 {
		add(model.Finding{
			ID: "crdb_external_disk_usage", Severity: model.SeverityWarn, ClusterScoped: true,
			Title:       fmt.Sprintf("%d full CockroachDB store volume(s) contain substantial non-CockroachDB usage", len(externalEvidence)),
			Detail:      "Filesystem usage materially exceeds CockroachDB's own capacity.used metric on these already-full volumes. Another process, retained files, filesystem overhead, or storage accounting outside the active CockroachDB store may be consuming the missing headroom.",
			Evidence:    externalEvidence,
			Objects:     externalObjects,
			Remediation: "Inspect filesystem usage on the affected nodes and mount points. Remove or relocate only confirmed non-CockroachDB data; do not manually delete files inside an active CockroachDB store. Add capacity if the extra usage is legitimate.",
			Impact:      impact(model.DimRisk, 80, humanBytes(s.OtherUsedBytes)+" non-CockroachDB use or filesystem overhead cluster-wide", "filesystem used bytes minus CockroachDB capacity.used on live stores"),
			Confidence:  0.9,
			Caveats:     []string{"the difference can include filesystem metadata, reserved space, snapshots, or accounting lag; pgbot cannot identify files or their owner"},
		})
	}

	if s.CounterSampledStores > 0 && (s.DiskSlowEvents > 0 || s.DiskStalledEvents > 0 || s.DiskUnhealthySeconds > 0 || s.WriteStallEvents > 0 || s.WriteStallSeconds > 0) {
		severity := model.SeverityWarn
		if s.DiskStalledEvents > 0 {
			severity = model.SeverityCritical
		}
		var evidence, objects []string
		for _, store := range s.Stores {
			if store.DiskSlowEvents == 0 && store.DiskStalledEvents == 0 && store.DiskUnhealthySeconds == 0 && store.WriteStallEvents == 0 && store.WriteStallSeconds == 0 {
				continue
			}
			evidence = append(evidence, fmt.Sprintf("s%d/n%d · %d slow · %d stalled · %.2fs unhealthy · %d write stalls / %.2fs",
				store.StoreID, store.NodeID, store.DiskSlowEvents, store.DiskStalledEvents, store.DiskUnhealthySeconds, store.WriteStallEvents, store.WriteStallSeconds))
			objects = append(objects, fmt.Sprintf("store:s%d", store.StoreID))
		}
		add(model.Finding{
			ID: "crdb_storage_stall", Severity: severity, ClusterScoped: true,
			Title:       "CockroachDB observed storage-engine stalls during the sample",
			Detail:      "Disk operations crossed CockroachDB's slow or stalled thresholds, or Pebble deliberately stalled writes to protect the storage engine. Sustained stalls directly increase KV and SQL latency and can destabilize Raft replication.",
			Evidence:    evidence,
			Objects:     objects,
			Remediation: "Inspect disk latency, bandwidth, IOPS, filesystem and cloud-volume health on the affected nodes. Correlate with L0/compaction pressure and write workload before resizing or replacing storage.",
			Impact:      impact(model.DimLatency, 90, fmt.Sprintf("%d slow, %d stalled, %.2fs write-stalled over %.2fs", s.DiskSlowEvents, s.DiskStalledEvents, s.WriteStallSeconds, s.SampleSeconds), "Admin API counter deltas across the pgbot sample"),
			Confidence:  1,
			Caveats:     []string{"the sample is short; a clean run does not prove there were no stalls before or after it"},
		})
	}

	uninitializedRatio := safeFindingRatio(float64(s.UninitializedReplicas), float64(s.RangeReplicas))
	if s.ReplicationMetricsAvailable && ((s.UninitializedReplicas >= crdbUninitializedMin && uninitializedRatio >= crdbUninitializedRatioMin) ||
		s.ReplicateQueuePurgatory >= crdbReplicaPurgatoryMin || s.RaftSnapshotQueuePending >= crdbSnapshotQueueMin) {
		var evidence, objects []string
		evidence = append(evidence, fmt.Sprintf("%s uninitialized / %s initialized replicas (%.1f%%) · %s reserved · %s replicate-queue purgatory · %s snapshot-queue pending",
			human(s.UninitializedReplicas), human(s.RangeReplicas), uninitializedRatio*100, human(s.ReservedReplicas), human(s.ReplicateQueuePurgatory), human(s.RaftSnapshotQueuePending)))
		stores := append([]model.CockroachStoreStorage(nil), s.Stores...)
		sort.Slice(stores, func(i, j int) bool { return stores[i].UninitializedReplicas > stores[j].UninitializedReplicas })
		for _, store := range stores {
			if store.Status != "live" || (store.UninitializedReplicas == 0 && store.ReplicateQueuePurgatory == 0 && store.RaftSnapshotQueuePending == 0) {
				continue
			}
			evidence = append(evidence, fmt.Sprintf("s%d/n%d · %d uninitialized · %d reserved · queue %d pending / %d purgatory · %d snapshot pending",
				store.StoreID, store.NodeID, store.UninitializedReplicas, store.ReservedReplicas, store.ReplicateQueuePending, store.ReplicateQueuePurgatory, store.RaftSnapshotQueuePending))
			objects = append(objects, fmt.Sprintf("store:s%d", store.StoreID))
			if len(evidence) == 6 {
				break
			}
		}
		add(model.Finding{
			ID: "crdb_replication_recovery", Severity: model.SeverityWarn, ClusterScoped: true,
			Title:       fmt.Sprintf("%s CockroachDB replicas are uninitialized or blocked in recovery", human(s.UninitializedReplicas)),
			Detail:      "A material share of replicas has not initialized, or repair work is waiting in replication purgatory or the snapshot queue. This commonly accompanies node recovery, rebalancing, or a placement constraint that prevents repair.",
			Evidence:    evidence,
			Objects:     objects,
			Remediation: "Check node liveness, snapshot traffic, store capacity, allocator logs, and zone constraints. Restore the blocking resource or make constraints satisfiable, then allow CockroachDB to complete recovery automatically.",
			Impact:      impact(model.DimRisk, 75, fmt.Sprintf("%.1f%% uninitialized replicas", uninitializedRatio*100), "live-store replication gauges from the Admin API"),
			Confidence:  0.85,
			Caveats:     []string{"a point-in-time recovery backlog can be expected immediately after adding, restarting, or decommissioning nodes; confirm that it is falling on repeated runs"},
		})
	}

	flowThreshold := math.Max(10, float64(s.RangeReplicas)*crdbRaftFlowRatioWarn)
	droppedPerSec := safeFindingRatio(float64(s.RaftDroppedMessages), s.SampleSeconds)
	if s.MaxRaftCommandsPending >= crdbRaftPendingStoreWarn || float64(s.RaftProbeFlows+s.RaftSnapshotFlows) >= flowThreshold || droppedPerSec >= crdbRaftDroppedPerSecWarn {
		add(model.Finding{
			ID: "crdb_raft_backlog", Severity: model.SeverityWarn, ClusterScoped: true,
			Title:  "CockroachDB Raft replication is showing a material backlog",
			Detail: "Pending proposals, follower flows in probe/snapshot state, or dropped Raft messages indicate replicas are struggling to keep up with leaders. This can increase commit latency and eventually reduce availability if it persists.",
			Evidence: []string{fmt.Sprintf("%d commands pending · busiest s%d has %d · %d probe flows · %d snapshot flows · %d dropped messages over %.2fs",
				s.RaftCommandsPending, s.MaxRaftPendingStoreID, s.MaxRaftCommandsPending, s.RaftProbeFlows, s.RaftSnapshotFlows, s.RaftDroppedMessages, s.SampleSeconds)},
			Objects:     []string{fmt.Sprintf("store:s%d", s.MaxRaftPendingStoreID)},
			Remediation: "Inspect the busiest store's disk and network latency, Raft scheduler latency, snapshots, and node health. Address the underlying resource or connectivity problem before attempting manual replica changes.",
			Impact:      impact(model.DimLatency, 75, fmt.Sprintf("%d pending commands; %.1f dropped messages/s", s.RaftCommandsPending, droppedPerSec), "Admin API Raft gauges and sampled counter deltas"),
			Confidence:  0.85,
			Caveats:     []string{"brief probe or snapshot states are normal during replica changes; repeated runs distinguish transient repair from sustained lag"},
		})
	}

	if s.LiveStores >= crdbBalanceMinStores && s.BytesPerReplicaMean >= crdbReplicaBytesMeanMin &&
		s.BytesPerReplicaMax/s.BytesPerReplicaMean >= crdbReplicaBytesMaxMean &&
		s.BytesPerReplicaMin/s.BytesPerReplicaMean <= crdbReplicaBytesMinMean {
		add(model.Finding{
			ID: "crdb_replica_size_skew", Severity: model.SeverityWarn, ClusterScoped: true,
			Title:  "Average MVCC bytes per replica are heavily skewed across live stores",
			Detail: "Replica counts can look balanced while a few stores own much larger ranges. Persistent byte skew concentrates storage, snapshot cost, and compaction work even when the number of replicas is even.",
			Evidence: []string{
				cockroachStorageEvidence(s, s.LargestReplicaBytesStoreID, "largest replicas"),
				cockroachStorageEvidence(s, s.SmallestReplicaBytesStoreID, "smallest replicas"),
			},
			Objects:     []string{fmt.Sprintf("store:s%d", s.LargestReplicaBytesStoreID), fmt.Sprintf("store:s%d", s.SmallestReplicaBytesStoreID)},
			Remediation: "Check range sizes, table/index placement, zone constraints, and split/rebalance activity. Correct persistent placement blockers and allow CockroachDB to split or rebalance ranges automatically.",
			Impact:      impact(model.DimStorage, 65, fmt.Sprintf("%s–%s MVCC bytes per replica", humanBytes(int64(s.BytesPerReplicaMin)), humanBytes(int64(s.BytesPerReplicaMax))), "logical MVCC total bytes divided by initialized replicas on live stores"),
			Confidence:  0.8,
			Caveats:     []string{"MVCC bytes are logical replicated bytes, not compressed on-disk bytes; locality constraints can intentionally create different range-size distributions"},
		})
	}
}

func cockroachStorageEvidence(s *model.CockroachStorage, storeID int, label string) string {
	for _, store := range s.Stores {
		if store.StoreID == storeID {
			return fmt.Sprintf("%s s%d/n%d · %s per replica · %s total MVCC · %d replicas · %s",
				label, store.StoreID, store.NodeID, humanBytes(int64(store.BytesPerReplica)), humanBytes(store.MVCCTotalBytes), store.RangeReplicas, orText(store.Locality, "locality unknown"))
		}
	}
	return fmt.Sprintf("%s s%d", label, storeID)
}

func safeFindingRatio(numerator, denominator float64) float64 {
	if denominator <= 0 {
		return 0
	}
	return numerator / denominator
}

func crdbDistributionFindings(c *model.Context, add func(model.Finding)) {
	if c.Health == nil || c.Health.Cockroach == nil {
		return
	}
	h := c.Health.Cockroach
	d := &h.Distribution
	if d.Exactness == "" || d.Exactness == model.ExactnessUnavailable {
		return
	}
	caveats := []string{"zone constraints, store attributes, and multi-region placement can intentionally produce an uneven distribution; confirm the configured topology before changing it"}
	if d.MultipleLocalities {
		caveats = append(caveats, "the compared stores span multiple localities, so lease preferences or survival goals may explain part of the skew")
	}
	if d.ComparableStores >= crdbBalanceMinStores && d.ReplicaMean >= crdbReplicaMeanMin &&
		d.ReplicaMax-d.ReplicaMin >= crdbReplicaSpreadMin && d.ReplicaMaxToMean >= crdbReplicaMaxMeanWarn && d.ReplicaMinToMean <= crdbReplicaMinMeanWarn {
		add(model.Finding{
			ID: "crdb_replica_imbalance", Severity: model.SeverityWarn, ClusterScoped: true,
			Title:       "Range replicas are unevenly distributed across comparable live stores",
			Detail:      "The most-loaded comparable store holds substantially more range replicas than both the live-store mean and the least-loaded peer. Persistent replica skew can concentrate storage, repair work, and KV traffic.",
			Evidence:    []string{cockroachBalanceEvidence(d, d.MostReplicasStoreID, "most replicas"), cockroachBalanceEvidence(d, d.FewestReplicasStoreID, "fewest replicas")},
			Objects:     []string{fmt.Sprintf("store:s%d", d.MostReplicasStoreID), fmt.Sprintf("store:s%d", d.FewestReplicasStoreID)},
			Remediation: "Check allocator and rebalancing status, store health/capacity, and zone constraints. Restore allocator headroom or correct an unsatisfiable constraint, then allow CockroachDB to rebalance automatically.",
			Impact:      impact(model.DimRisk, 65, fmt.Sprintf("%d–%d replicas across %d comparable stores", d.ReplicaMin, d.ReplicaMax, d.ComparableStores), "per-store range-replica counts on live, similarly sized stores"),
			Confidence:  0.8, Caveats: caveats,
		})
	}
	if d.ComparableStores >= crdbBalanceMinStores && d.LeaseMean >= crdbLeaseMeanMin &&
		d.LeaseMax-d.LeaseMin >= crdbLeaseSpreadMin && d.LeaseMaxToMean >= crdbLeaseMaxMeanWarn && d.LeaseMinToMean <= crdbLeaseMinMeanWarn {
		add(model.Finding{
			ID: "crdb_leaseholder_imbalance", Severity: model.SeverityInfo, ClusterScoped: true,
			Title:       "Leaseholders are heavily skewed across comparable live stores",
			Detail:      "Leaseholders route most reads and coordinate writes. This skew may be an intentional locality preference, but without that intent it can concentrate request coordination on a subset of nodes.",
			Evidence:    []string{cockroachBalanceEvidence(d, d.MostLeasesStoreID, "most leases"), cockroachBalanceEvidence(d, d.FewestLeasesStoreID, "fewest leases")},
			Objects:     []string{fmt.Sprintf("store:s%d", d.MostLeasesStoreID), fmt.Sprintf("store:s%d", d.FewestLeasesStoreID)},
			Remediation: "Verify lease preferences and zone constraints first. If the skew is unintended and persistent, inspect lease rebalancing and node load rather than manually relocating individual leases from a one-time snapshot.",
			Impact:      impact(model.DimThroughput, 35, fmt.Sprintf("%d–%d leaseholders across %d comparable stores", d.LeaseMin, d.LeaseMax, d.ComparableStores), "per-store leaseholder counts on live, similarly sized stores"),
			Confidence:  0.65, Caveats: caveats,
		})
	}
	if d.LiveStores >= crdbBalanceMinStores && d.CapacityUsedMaxRatio >= crdbCapacityImbalanceMax && d.CapacityUsedSpread >= crdbCapacitySpreadWarn {
		add(model.Finding{
			ID: "crdb_capacity_imbalance", Severity: model.SeverityWarn, ClusterScoped: true,
			Title:       fmt.Sprintf("Live-store utilization differs by %.1f percentage points", d.CapacityUsedSpread*100),
			Detail:      "The fullest live store is already materially utilized while another live store has substantially more headroom. Cluster-wide free capacity can hide this local imbalance and reduce allocator flexibility.",
			Evidence:    []string{cockroachBalanceEvidence(d, d.MostUsedStoreID, "fullest"), cockroachBalanceEvidence(d, d.LeastUsedStoreID, "emptiest")},
			Objects:     []string{fmt.Sprintf("store:s%d", d.MostUsedStoreID), fmt.Sprintf("store:s%d", d.LeastUsedStoreID)},
			Remediation: "Check store/node health, replica constraints, decommissioning state, and allocator activity. Correct the blocking condition and let CockroachDB rebalance; add capacity if the fullest store lacks safe headroom.",
			Impact:      impact(model.DimRisk, 65, fmt.Sprintf("%.1f%%–%.1f%% used across %d live stores", d.CapacityUsedMinRatio*100, d.CapacityUsedMaxRatio*100, d.LiveStores), "per-store used capacity on live nodes"),
			Confidence:  0.9, Caveats: caveats,
		})
	}
	if d.HotRangeLeaseholderSamples >= crdbHotRangeMinSamples && d.HotRangeCPUCores >= crdbHotRangeMinCPUCores &&
		d.HottestLeaseholderRanges >= crdbHotRangeLeaderMin && d.HottestLeaseholderCPUShare >= crdbHotRangeShareWarn {
		evidence := []string{fmt.Sprintf("n%d leaseholds %d/%d attributed top hot ranges · %.3f/%.3f CPU cores (%.1f%%)",
			d.HottestLeaseholderNodeID, d.HottestLeaseholderRanges, d.HotRangeLeaseholderSamples,
			d.HottestLeaseholderCPUCores, d.HotRangeCPUCores, d.HottestLeaseholderCPUShare*100)}
		for _, r := range h.Hot {
			if r.LeaseholderNodeID != d.HottestLeaseholderNodeID {
				continue
			}
			name := strings.Join(r.Tables, ",")
			if len(r.Indexes) > 0 {
				name += "/" + strings.Join(r.Indexes, ",")
			}
			evidence = append(evidence, fmt.Sprintf("r%d · %.3f CPU cores · %.1f QPS · %s", r.RangeID, r.CPUCores, r.QPS, orText(name, "object unavailable")))
			if len(evidence) == 5 {
				break
			}
		}
		add(model.Finding{
			ID: "crdb_hot_range_concentration", Severity: model.SeverityWarn, ClusterScoped: true,
			Title:    fmt.Sprintf("One node leaseholds %.1f%% of sampled top-hot-range CPU", d.HottestLeaseholderCPUShare*100),
			Detail:   "A single leaseholder node dominates CPU across the bounded top-hot-range sample. This often points to hot keys, concentrated access to one table/index, or lease placement that is not spreading the active workload.",
			Evidence: evidence, Objects: []string{fmt.Sprintf("node:n%d", d.HottestLeaseholderNodeID)},
			Remediation: "Correlate the listed ranges with their tables and indexes, distribute hot keys or sequential writes, and verify lease preferences. Rebalance workload or capacity based on sustained observations, not one sample.",
			Impact:      impact(model.DimThroughput, 70, fmt.Sprintf("%.3f of %.3f sampled CPU cores on n%d", d.HottestLeaseholderCPUCores, d.HotRangeCPUCores, d.HottestLeaseholderNodeID), "CockroachDB bounded top-hot-range sample grouped by leaseholder"),
			Confidence:  0.85,
			Caveats:     []string{"the hot-range endpoint is a bounded point-in-time sample, not all ranges; rerun during the incident and confirm in DB Console before changing placement"},
		})
	}
}

func cockroachBalanceEvidence(d *model.CockroachDistribution, storeID int, label string) string {
	for _, s := range d.Stores {
		if s.StoreID != storeID {
			continue
		}
		return fmt.Sprintf("%s s%d/n%d · %.1f%% used · %d replicas · %d leases · node CPU %.1f%% · %s",
			label, s.StoreID, s.NodeID, s.UsedRatio*100, s.RangeReplicas, s.Leaseholders, s.NodeCPUPercent, orText(s.Locality, "locality unknown"))
	}
	return fmt.Sprintf("%s s%d", label, storeID)
}

func crdbContentionFindings(c *model.Context, add func(model.Finding)) {
	if c.Cockroach == nil || c.Cockroach.Contention.Exactness == model.ExactnessUnavailable {
		return
	}
	h := &c.Cockroach.Contention
	if h.TotalWaitMS >= crdbContentionTotalWarnMS || h.MaxWaitMS >= crdbContentionMaxWarnMS {
		var evidence []string
		for _, x := range h.Hotspots {
			if x.Type == "SERIALIZATION_CONFLICT" {
				continue
			}
			line := fmt.Sprintf("%s · %s · %d events · %s total · %s max · waiter q:%s",
				crdbContentionObject(x), x.Type, x.Events, durationMS(x.TotalWaitMS), durationMS(x.MaxWaitMS), x.WaitingStatementFingerprint)
			if x.BlockingTxnFingerprint != "" {
				line += " · blocker txn:" + x.BlockingTxnFingerprint
			} else if x.BlockerResolution == model.CockroachContentionNotResolved {
				line += " · blocker not resolved by CockroachDB"
			}
			if x.WaitingQuery != "" {
				line += " · waiting query: " + truncate(x.WaitingQuery, 80)
			}
			blockingQuery := x.BlockingQuery
			if blockingQuery == "" && len(x.BlockingQueries) > 0 {
				blockingQuery = x.BlockingQueries[0]
			}
			if blockingQuery != "" {
				line += " · blocking query: " + truncate(blockingQuery, 80)
			}
			evidence = append(evidence, line)
			if len(evidence) == 5 {
				break
			}
		}
		add(model.Finding{
			ID: "crdb_contention_hotspot", Severity: model.SeverityWarn, ClusterScoped: true,
			Title:       fmt.Sprintf("%d CockroachDB contention events accumulated %s of wait time", h.TotalEvents, durationMS(h.TotalWaitMS)),
			Detail:      fmt.Sprintf("CockroachDB's bounded contention event store recorded material lock waits during the last %d minutes. Repeated waits on the same table/index usually indicate a hot key or a transaction holding locks for too long.", h.WindowMinutes),
			Evidence:    evidence,
			Remediation: "Find the waiting statement and blocking transaction fingerprints in SQL Activity; shorten the holding transaction, access rows in a consistent order, and distribute writes away from hot keys.",
			Impact:      impact(model.DimLatency, math.Min(95, 45+math.Log10(h.TotalWaitMS+1)*10), fmt.Sprintf("%s total wait; %s longest", durationMS(h.TotalWaitMS), durationMS(h.MaxWaitMS)), "crdb_internal.transaction_contention_events over the last hour"),
			Confidence:  0.95,
			Caveats:     []string{"the source is an in-memory LRU, so counts are a lower bound and events can be evicted"},
		})
	}
	if h.SerializationConflicts >= crdbSerializationWarn {
		var evidence []string
		for _, x := range h.Hotspots {
			if x.Type != "SERIALIZATION_CONFLICT" {
				continue
			}
			evidence = append(evidence, fmt.Sprintf("%s · %d conflicts · waiter q:%s", crdbContentionObject(x), x.Events, x.WaitingStatementFingerprint))
			if len(evidence) == 5 {
				break
			}
		}
		add(model.Finding{
			ID: "crdb_serialization_conflicts", Severity: model.SeverityWarn, ClusterScoped: true,
			Title:  fmt.Sprintf("%d serialization conflicts were recorded in the last hour", h.SerializationConflicts),
			Detail: "CockroachDB recorded transaction conflicts that can surface as SQLSTATE 40001 or consume work through automatic retries.", Evidence: evidence,
			Remediation: "Keep transactions small, access rows consistently, avoid read-modify-write hot keys, and ensure the application retries retryable serialization errors with backoff.",
			Impact:      impact(model.DimThroughput, math.Min(90, 45+math.Log10(float64(h.SerializationConflicts)+1)*15), fmt.Sprintf("%d conflicts", h.SerializationConflicts), "SERIALIZATION_CONFLICT events in the CockroachDB contention store"), Confidence: 0.95,
		})
	}
}

func crdbContentionObject(h model.CockroachContentionHotspot) string {
	parts := []string{h.Database, h.Schema, h.Table}
	name := strings.Join(parts, ".")
	if h.Index != "" {
		name += "/" + h.Index
	}
	return name
}

func durationMS(ms float64) string {
	if ms <= 0 {
		return "0s"
	}
	return (time.Duration(ms * float64(time.Millisecond))).Round(time.Millisecond).String()
}

func crdbClusterHealth(c *model.Context, add func(model.Finding)) {
	if c.Health == nil || c.Health.Cockroach == nil {
		return
	}
	h := c.Health.Cockroach
	if h.NodesSuspect > 0 {
		var evidence []string
		for _, n := range h.Nodes {
			if n.Status != "live" && n.Status != "draining" && n.Status != "decommissioning" && n.Status != "decommissioned" {
				evidence = append(evidence, fmt.Sprintf("n%d · %s · %s", n.NodeID, n.Status, orText(n.Locality, "locality unknown")))
			}
		}
		add(model.Finding{
			ID: "crdb_node_unavailable", Severity: model.SeverityCritical, ClusterScoped: true,
			Title:    fmt.Sprintf("%d CockroachDB node(s) are not live", h.NodesSuspect),
			Detail:   "The Admin API reports one or more nodes as dead, unavailable, or unknown. The cluster may have lost replica redundancy or serving capacity.",
			Evidence: evidence, Remediation: "Check node process/network health and the DB Console node-liveness view; recover the node or follow the documented decommission/replacement procedure.",
			Impact: impact(model.DimRisk, 95, fmt.Sprintf("%d/%d nodes suspect", h.NodesSuspect, h.NodesTotal), "CockroachDB Admin API node liveness"), Confidence: 1,
		})
	}
	if h.UnavailableRanges > 0 {
		add(model.Finding{
			ID: "crdb_ranges_unavailable", Severity: model.SeverityCritical, ClusterScoped: true,
			Title:       fmt.Sprintf("%d range replica(s) report unavailable", h.UnavailableRanges),
			Detail:      "Unavailable ranges cannot reach quorum and may reject reads or writes for the affected keyspace.",
			Evidence:    []string{fmt.Sprintf("%d unavailable across %d stores", h.UnavailableRanges, h.StoresTotal)},
			Remediation: "Use the DB Console replication and range reports to identify affected ranges and missing replicas; restore node connectivity before considering replica surgery.",
			Impact:      impact(model.DimRisk, 100, fmt.Sprintf("%d unavailable range replicas", h.UnavailableRanges), "sum of store ranges.unavailable metrics"), Confidence: 1,
		})
	}
	if h.UnderreplicatedRanges > 0 {
		add(model.Finding{
			ID: "crdb_ranges_underreplicated", Severity: model.SeverityWarn, ClusterScoped: true,
			Title:       fmt.Sprintf("%d range replica(s) are under-replicated", h.UnderreplicatedRanges),
			Detail:      "Under-replicated ranges have less failure tolerance than their configured replication target.",
			Evidence:    []string{fmt.Sprintf("%d under-replicated across %d stores", h.UnderreplicatedRanges, h.StoresTotal)},
			Remediation: "Check node liveness, store capacity, allocator/rebalancing status, and zone constraints; allow replication to recover before planned maintenance.",
			Impact:      impact(model.DimRisk, 80, fmt.Sprintf("%d under-replicated range replicas", h.UnderreplicatedRanges), "sum of store ranges.underreplicated metrics"), Confidence: 1,
		})
	}
	if h.MaxStoreUsedRatio >= crdbStoreWarn {
		severity := model.SeverityWarn
		if h.MaxStoreUsedRatio >= crdbStoreCrit {
			severity = model.SeverityCritical
		}
		add(model.Finding{
			ID: "crdb_store_capacity", Severity: severity, ClusterScoped: true,
			Title:    fmt.Sprintf("CockroachDB's fullest store is %.1f%% used", h.MaxStoreUsedRatio*100),
			Detail:   "A full store reduces allocator headroom and can eventually prevent writes. Cluster-wide free space can hide one locally full store.",
			Evidence: fullestStoreEvidence(h.Stores), Remediation: "Add capacity or nodes, remove obsolete data, and verify range rebalancing and locality constraints before the store reaches the hard limit.",
			Impact: impact(model.DimRisk, math.Min(100, 55+h.MaxStoreUsedRatio*45), fmt.Sprintf("%.1f%% used on fullest store", h.MaxStoreUsedRatio*100), "CockroachDB Admin API per-store capacity"), Confidence: 1,
		})
	}
	var pressure []string
	if h.MaxCPUPercent >= crdbCPUWarn {
		pressure = append(pressure, fmt.Sprintf("peak node CPU %.1f%%", h.MaxCPUPercent))
	}
	if h.MaxMemoryUsedRatio >= crdbMemoryWarn {
		pressure = append(pressure, fmt.Sprintf("peak node RSS %.1f%% of system memory", h.MaxMemoryUsedRatio*100))
	}
	if h.AdmissionWaitP99MS >= crdbAdmissionWarnMS {
		pressure = append(pressure, fmt.Sprintf("admission wait p99 %.1fms", h.AdmissionWaitP99MS))
	}
	if len(pressure) > 0 {
		add(model.Finding{
			ID: "crdb_resource_pressure", Severity: model.SeverityWarn, ClusterScoped: true,
			Title: "CockroachDB is showing resource or admission pressure", Detail: "High node utilization or admission delay indicates demand is approaching available CPU or memory capacity.",
			Evidence: pressure, Remediation: "Correlate the pressured node with SQL fingerprints and hottest ranges; reduce the dominant workload, rebalance hot ranges, or add capacity.",
			Impact: impact(model.DimThroughput, 75, strings.Join(pressure, "; "), "Admin API node resource and admission-control metrics"), Confidence: 0.9,
		})
	}
	versions := map[string]bool{}
	for _, n := range h.Nodes {
		if n.Version != "" && n.Status != "decommissioned" {
			versions[n.Version] = true
		}
	}
	if len(versions) > 1 {
		var values []string
		for v := range versions {
			values = append(values, v)
		}
		sort.Strings(values)
		add(model.Finding{
			ID: "crdb_version_skew", Severity: model.SeverityWarn, ClusterScoped: true,
			Title: fmt.Sprintf("CockroachDB nodes run %d different versions", len(values)), Detail: "Version skew is expected during a rolling upgrade but should not remain indefinitely.",
			Evidence: []string{strings.Join(values, ", ")}, Remediation: "Confirm a rolling upgrade is in progress and finish it within CockroachDB's supported upgrade path; do not finalize until every intended node is healthy.",
			Impact: impact(model.DimRisk, 55, fmt.Sprintf("%d versions", len(values)), "Admin API node build tags"), Confidence: 1,
		})
	}
}

func crdbJobFindings(c *model.Context, add func(model.Finding)) {
	if c.Health == nil || c.Health.Cockroach == nil || c.Health.Cockroach.Jobs.Exactness == "" || c.Health.Cockroach.Jobs.Exactness == model.ExactnessUnavailable {
		return
	}
	var failed, stalled, reverting, paused []string
	var failedObjects, stalledObjects, revertingObjects, pausedObjects []string
	revertFailed := false
	for _, j := range c.Health.Cockroach.JobItems {
		object := "job:" + j.JobID
		evidence := crdbJobEvidence(j, c.CollectedAt)
		switch j.State {
		case "failed", "revert-failed":
			failed = append(failed, evidence)
			failedObjects = append(failedObjects, object)
			revertFailed = revertFailed || j.State == "revert-failed"
		case "paused":
			paused = append(paused, evidence)
			pausedObjects = append(pausedObjects, object)
		default:
			if cockroachJobStalled(j, c.CollectedAt) {
				stalled = append(stalled, evidence)
				stalledObjects = append(stalledObjects, object)
			} else if j.State == "reverting" {
				reverting = append(reverting, evidence)
				revertingObjects = append(revertingObjects, object)
			}
		}
	}
	if len(failed) > 0 {
		severity := model.SeverityWarn
		score := 70.0
		if revertFailed {
			severity = model.SeverityCritical
			score = 90
		}
		add(model.Finding{
			ID: "crdb_job_failed", Severity: severity, ClusterScoped: true,
			Title:    fmt.Sprintf("%d recent CockroachDB job(s) failed", len(failed)),
			Detail:   "Failed schema-change, backup, restore, import, changefeed, or maintenance jobs can leave an operation incomplete. A revert-failed state means CockroachDB also failed to clean up the original operation.",
			Evidence: cap10(failed), Objects: failedObjects,
			Remediation: "Inspect SHOW JOB <id> for the full error and job-specific retry guidance, correct the underlying cause, then retry or resume only when the operation supports it.",
			Impact:      impact(model.DimRisk, score, fmt.Sprintf("%d recent failed jobs", len(failed)), "information_schema.crdb_jobs_with_progress; revert-failed is critical"), Confidence: 1,
		})
	}
	if len(stalled) > 0 {
		add(model.Finding{
			ID: "crdb_job_stalled", Severity: model.SeverityWarn, ClusterScoped: true,
			Title:    fmt.Sprintf("%d CockroachDB job(s) have not reported progress for at least 30 minutes", len(stalled)),
			Detail:   "A running, pending, reverting, pause-requested, or cancel-requested job has not updated its progress or status timestamp. This can indicate admission pressure, an unavailable dependency, a stuck coordinator, or cleanup that cannot advance.",
			Evidence: cap10(stalled), Objects: stalledObjects,
			Remediation: "Inspect SHOW JOB <id>, DB Console job details, node liveness, admission pressure, and the job's external dependency. Do not cancel or retry it until its current state and operation-specific recovery semantics are understood.",
			Impact:      impact(model.DimRisk, 75, fmt.Sprintf("%d jobs without an update for at least 30 minutes", len(stalled)), "last_updated (or created when no progress row exists) from information_schema.crdb_jobs_with_progress"),
			Confidence:  0.85,
			Caveats:     []string{"some jobs update progress in coarse steps; pgbot requires a 30-minute silent period and does not flag duration alone"},
		})
	}
	if len(reverting) > 0 {
		add(model.Finding{
			ID: "crdb_job_reverting", Severity: model.SeverityInfo, ClusterScoped: true,
			Title:    fmt.Sprintf("%d CockroachDB job(s) are reverting", len(reverting)),
			Detail:   "CockroachDB is actively undoing an operation that did not complete. A recently updating revert is not classified as stalled, but the original operation still failed and may need follow-up after cleanup finishes.",
			Evidence: cap10(reverting), Objects: revertingObjects,
			Remediation: "Let the revert complete while monitoring its progress. Then inspect the original job error and correct the cause before retrying the operation.",
			Impact:      impact(model.DimRisk, 35, fmt.Sprintf("%d jobs reverting", len(reverting)), "current CockroachDB job state and recent progress timestamp"), Confidence: 1,
		})
	}
	if len(paused) > 0 {
		add(model.Finding{
			ID: "crdb_job_paused", Severity: model.SeverityInfo, ClusterScoped: true,
			Title:    fmt.Sprintf("%d CockroachDB job(s) are paused", len(paused)),
			Detail:   "Paused jobs make no progress until resumed or canceled. This may be intentional, especially for changefeeds, but it can also leave a schema, backup, restore, import, or maintenance operation incomplete.",
			Evidence: cap10(paused), Objects: pausedObjects,
			Remediation: "Confirm the pause is intentional and has an owner. Resume or cancel the job only after checking its type-specific operational consequences.",
			Impact:      impact(model.DimRisk, 20, fmt.Sprintf("%d paused jobs", len(paused)), "current CockroachDB job state"), Confidence: 1,
		})
	}
}

func cockroachJobStalled(j model.CockroachJobHealth, collectedAt time.Time) bool {
	switch j.State {
	case "running", "pending", "reverting", "pause-requested", "cancel-requested":
	default:
		return false
	}
	if cockroachJobExpectedSilent(j.Type) {
		return false
	}
	if collectedAt.IsZero() {
		return false
	}
	last := j.CreatedAt
	if j.LastUpdatedAt != nil && !j.LastUpdatedAt.IsZero() {
		last = *j.LastUpdatedAt
	}
	return !last.IsZero() && collectedAt.Sub(last) >= time.Duration(crdbJobStalledSec)*time.Second
}

func cockroachJobExpectedSilent(jobType string) bool {
	switch strings.ToUpper(strings.TrimSpace(jobType)) {
	case "SCHEMA CHANGE GC",
		"AUTO SPAN CONFIG RECONCILIATION",
		"POLL JOBS STATS",
		"AUTO CONFIG RUNNER",
		"AUTO CONFIG ENV RUNNER",
		"AUTO CONFIG TASK",
		"KEY VISUALIZER",
		"AUTO UPDATE SQL ACTIVITY",
		"MVCC STATISTICS UPDATE",
		"UPDATE TABLE METADATA CACHE",
		"SQL ACTIVITY FLUSH",
		"HOT RANGES LOGGER",
		"AUTO ASH COMPACTION":
		return true
	default:
		return false
	}
}

func crdbJobEvidence(j model.CockroachJobHealth, collectedAt time.Time) string {
	parts := []string{"job " + j.JobID, orText(j.Type, "type unknown"), orText(j.State, "state unknown")}
	if j.ProgressKnown {
		parts = append(parts, fmt.Sprintf("%.1f%%", j.Progress*100))
	}
	if !j.CreatedAt.IsZero() && !collectedAt.IsZero() {
		parts = append(parts, "age "+shortDur(max(0, int64(collectedAt.Sub(j.CreatedAt).Seconds()))))
	}
	if j.LastUpdatedAt != nil && !collectedAt.IsZero() {
		parts = append(parts, "last update "+shortDur(max(0, int64(collectedAt.Sub(*j.LastUpdatedAt).Seconds())))+" ago")
	}
	if j.StatusMessage != "" {
		parts = append(parts, "status: "+truncate(j.StatusMessage, 100))
	}
	if j.Error != "" {
		parts = append(parts, "error: "+truncate(j.Error, 120))
	}
	if j.Operation != "" {
		parts = append(parts, truncate(j.Operation, 140))
	}
	return strings.Join(parts, " · ")
}

func fullestStoreEvidence(stores []model.CockroachStoreHealth) []string {
	if len(stores) == 0 {
		return nil
	}
	worst := stores[0]
	for _, s := range stores[1:] {
		if s.UsedRatio > worst.UsedRatio {
			worst = s
		}
	}
	return []string{fmt.Sprintf("n%d/s%d · %.1f%% used · %s available", worst.NodeID, worst.StoreID, worst.UsedRatio*100, humanBytes(worst.AvailableBytes))}
}

func crdbLongRunningQueries(c *model.Context, add func(model.Finding)) {
	if c.Cockroach == nil {
		return
	}
	var evidence []string
	var longest float64
	for _, q := range c.Cockroach.LiveQueries.Items {
		if q.AgeSec < crdbLongQuerySec {
			continue
		}
		longest = math.Max(longest, q.AgeSec)
		evidence = append(evidence, fmt.Sprintf("%s · app %s · running %.0fs · phase %s · %s",
			truncate(q.Query, 80), orText(q.AppName, "(none)"), q.AgeSec, orText(q.Phase, "unknown"),
			map[bool]string{true: "full scan", false: "no full scan reported"}[q.FullScan]))
	}
	if len(evidence) == 0 {
		return
	}
	add(model.Finding{
		ID: "crdb_long_running_query", Severity: model.SeverityWarn, ClusterScoped: true,
		Title:       fmt.Sprintf("%d CockroachDB query(s) running over one minute", len(evidence)),
		Detail:      "These statements were still executing when pgbot sampled SHOW CLUSTER QUERIES. Long runtime can be expected for analytical work, but it can also signal a full scan, contention, or a stalled client request.",
		Evidence:    evidence,
		Remediation: "Inspect the query in the CockroachDB SQL Activity page and run EXPLAIN (DISTSQL) with representative parameters; cancel it only after confirming the owning application no longer needs it.",
		Impact:      impact(model.DimLatency, math.Min(85, 35+longest/15), fmt.Sprintf("longest running %.0fs", longest), "SHOW CLUSTER QUERIES start time"),
		Confidence:  0.9,
		Caveats:     []string{"a long analytical or maintenance query may be intentional"},
	})
}

func crdbRetryHotspots(c *model.Context, add func(model.Finding)) {
	var evidence, objects []string
	var worst int64
	if c.Queries != nil && c.Queries.Enabled {
		for _, q := range c.Queries.Top {
			if q.MaxRetries < crdbRetryHotspotMin {
				continue
			}
			worst = max(worst, q.MaxRetries)
			evidence = append(evidence, fmt.Sprintf("persisted max %d retries · app %s · %.1fms mean · %.1fms contention · %s",
				q.MaxRetries, orText(q.AppName, "(none)"), q.MeanMS, q.ContentionMS, truncate(q.Query, 80)))
			objects = append(objects, crdbQueryObject(q.Fingerprint, q.QueryID))
		}
	}
	if c.Cockroach != nil {
		for _, q := range c.Cockroach.LiveQueries.Items {
			retries := max(q.Retries, q.AutoRetries)
			if retries < crdbRetryHotspotMin {
				continue
			}
			worst = max(worst, retries)
			evidence = append(evidence, fmt.Sprintf("live now · %d retries · app %s · running %.1fs · %s",
				retries, orText(q.AppName, "(none)"), q.AgeSec, truncate(q.Query, 80)))
			if q.QueryID != "" {
				objects = append(objects, "q:live:"+q.QueryID)
			}
		}
	}
	if len(evidence) == 0 {
		return
	}
	add(model.Finding{
		ID: "crdb_retry_hotspot", Severity: model.SeverityWarn,
		Title:    fmt.Sprintf("%d CockroachDB query execution(s) needed repeated retries", len(evidence)),
		Detail:   "CockroachDB reported repeated retries in live queries or persisted statement statistics. Automatic serializable retries add latency and consume work that is later discarded; hot keys or broad transactions are common causes.",
		Evidence: evidence, Objects: objects,
		Remediation: "Use the transaction execution insights and contention details to identify the contended keys; keep transactions small, access rows in a consistent order, and add application retry handling for errors CockroachDB cannot auto-retry.",
		Impact:      impact(model.DimThroughput, math.Min(85, 35+float64(worst)*4), fmt.Sprintf("up to %d retries", worst), "SHOW CLUSTER QUERIES and persisted statement statistics"),
		Confidence:  0.85,
	})
}

func crdbExecutionInsights(c *model.Context, add func(model.Finding)) {
	if c.Cockroach == nil {
		return
	}
	var evidence, objects []string
	var worstMS float64
	for _, in := range c.Cockroach.ExecutionInsights.Items {
		if in.Problem == "" || in.Problem == "None" || in.Problem == "TransactionInsight" {
			continue
		}
		worstMS = math.Max(worstMS, in.ServiceLatencyMS)
		cause := strings.Join(in.Causes, ", ")
		if cause == "" {
			cause = "cause not classified"
		}
		evidence = append(evidence, fmt.Sprintf("%s %s · %s · %s · %.1fms · %d retries · %s",
			in.Kind, in.Problem, cause, orText(in.AppName, "(none)"), in.ServiceLatencyMS, in.Retries, truncate(in.Query, 80)))
		objects = append(objects, "q:"+orText(in.Fingerprint, "unknown"))
	}
	if len(evidence) == 0 {
		return
	}
	add(model.Finding{
		ID: "crdb_execution_insights", Severity: model.SeverityWarn,
		Title:    fmt.Sprintf("%d recent CockroachDB execution insight(s) need attention", len(evidence)),
		Detail:   "CockroachDB classified these executions as slow or failed and recorded their likely causes, including plan regressions, suboptimal plans, contention, or high retry counts.",
		Evidence: evidence, Objects: objects,
		Remediation: "Open SQL Activity → Insights for the recorded execution, inspect its plan and contention details, then address the classified cause rather than tuning a cluster-wide setting blindly.",
		Impact:      impact(model.DimLatency, math.Min(90, 50+worstMS/200), fmt.Sprintf("%d recorded problematic executions", len(evidence)), "CockroachDB persisted execution insights"),
		Confidence:  0.95,
	})
}

func crdbIndexRecommendations(c *model.Context, add func(model.Finding)) {
	if c.Cockroach == nil {
		return
	}
	seen := map[string]bool{}
	var evidence, objects []string
	for _, in := range c.Cockroach.ExecutionInsights.Items {
		for _, rec := range in.IndexRecommendations {
			key := in.Fingerprint + "\x00" + rec
			if rec == "" || seen[key] {
				continue
			}
			seen[key] = true
			evidence = append(evidence, fmt.Sprintf("app %s · %s · %s", orText(in.AppName, "(none)"), truncate(in.Query, 70), rec))
			objects = append(objects, "q:"+orText(in.Fingerprint, "unknown"))
		}
	}
	if len(evidence) == 0 {
		return
	}
	add(model.Finding{
		ID: "crdb_index_recommendations", Severity: model.SeverityInfo,
		Title:    fmt.Sprintf("%d CockroachDB index recommendation(s) recorded", len(evidence)),
		Detail:   "CockroachDB's optimizer attached index recommendations to problematic statement executions. They are candidates, not instructions to execute automatically.",
		Evidence: evidence, Objects: objects,
		Remediation: "Validate each recommendation with EXPLAIN on representative queries, check for overlap with existing indexes, and estimate write/storage cost before creating it.",
		Impact:      impact(model.DimLatency, 35, fmt.Sprintf("%d candidate indexes", len(evidence)), "CockroachDB statement execution insights"),
		Confidence:  0.75,
		Caveats:     []string{"an index can reduce read latency while increasing write amplification and storage"},
	})
}

func crdbUnusedIndexes(c *model.Context, add func(model.Finding)) {
	if c.Indexes == nil || c.Indexes.Exactness == model.ExactnessUnavailable || len(c.Indexes.Unused) == 0 {
		return
	}
	thresholdHours := c.Indexes.UnusedThresholdHours
	if thresholdHours == 0 {
		thresholdHours = 7 * 24
	}
	var evidence, objects []string
	var totalWrites int64
	for _, ix := range c.Indexes.Unused {
		age := shortDur(int64(ix.UnusedForSeconds))
		line := fmt.Sprintf("%s · no reads for %s · %s reads", cockroachIndexObject(ix), age, human(ix.Scans))
		if c.Indexes.WriteCountersAvailable {
			line += " · " + human(ix.Writes) + " writes"
			totalWrites += ix.Writes
		}
		var flags []string
		if ix.Inverted {
			flags = append(flags, "inverted")
		}
		if ix.Sharded {
			flags = append(flags, "sharded")
		}
		if ix.Invisible {
			flags = append(flags, "not visible")
		}
		if len(flags) > 0 {
			line += " · " + strings.Join(flags, ", ")
		}
		evidence = append(evidence, line)
		objects = append(objects, ix.Schema+"."+ix.Name)
	}

	confidence, uptimeCaveat := crdbIndexCounterConfidence(c, time.Duration(thresholdHours)*time.Hour)
	caveats := []string{
		"CockroachDB index-usage counters are cluster-wide but in-memory and non-durable; node restarts can erase evidence of reads",
		"index sizes are not available from this collector, so storage recovery is not estimated",
	}
	if uptimeCaveat != "" {
		caveats = append(caveats, uptimeCaveat)
	}
	if !c.Indexes.WriteCountersAvailable {
		caveats = append(caveats, "this CockroachDB version does not expose index write counters")
	}

	estimate := fmt.Sprintf("%d aged-unused secondary indexes", len(c.Indexes.Unused))
	score := 45.0
	if c.Indexes.WriteCountersAvailable {
		estimate = fmt.Sprintf("%d secondary indexes maintained across %s writes", len(c.Indexes.Unused), human(totalWrites))
		score = math.Min(85, score+math.Log10(float64(totalWrites)+1)*8)
	}
	add(model.Finding{
		ID: "crdb_unused_indexes", Severity: model.SeverityWarn,
		Title:       fmt.Sprintf("%d CockroachDB secondary index(es) unused for at least %s", len(c.Indexes.Unused), shortDur(int64(thresholdHours)*3600)),
		Detail:      "These non-unique secondary indexes have no recorded reads since their last read or creation time. pgbot applies a conservative seven-day age rule and never classifies primary, unique, or unknown-age indexes as unused.",
		Evidence:    evidence,
		Objects:     objects,
		Remediation: "Validate each candidate across a complete workload cycle in SQL Activity, preserve its DDL, and drop it only after confirming that no interactive, scheduled, or disaster-recovery path needs it.",
		Impact:      impact(model.DimThroughput, score, estimate, "cluster-wide CockroachDB index usage; non-unique secondary indexes only"),
		Confidence:  confidence,
		Caveats:     caveats,
		Safety: safety(precondition("crdb_unused_index.observation_window", model.ActionDropIndex,
			"The cluster-wide counters are in-memory, so a node restart or an off-window workload can make a required index appear unused.",
			"every live node's statistics window covers at least one complete workload cycle, including scheduled and recovery jobs, and SQL Activity shows no reads before DROP INDEX")),
	})
}

func crdbTableFindings(c *model.Context, add func(model.Finding)) {
	if c.Tables == nil || c.Tables.Exactness == model.ExactnessUnavailable {
		return
	}
	var metadataErrors, missingStats, autoStatsOff, mvccGarbage []string
	var metadataObjects, missingObjects, autoStatsObjects, garbageObjects []string
	var worstMissingBytes, totalGarbage int64
	for _, table := range c.Tables.Top {
		object := table.Schema + "." + table.Name
		name := cockroachTableObject(table)
		if table.MetadataError != "" {
			metadataErrors = append(metadataErrors, fmt.Sprintf("%s · %s", name, truncate(table.MetadataError, 140)))
			metadataObjects = append(metadataObjects, object)
		}
		if table.DataBytes >= crdbStatsTableMinBytes && table.StatsLastUpdated == nil {
			state := "automatic stats enabled"
			if !table.AutoStatsEnabled {
				state = "automatic stats disabled"
			}
			missingStats = append(missingStats, fmt.Sprintf("%s · %s MVCC data · %s", name, humanBytes(table.DataBytes), state))
			missingObjects = append(missingObjects, object)
			worstMissingBytes = max(worstMissingBytes, table.DataBytes)
		}
		if table.DataBytes >= crdbStatsTableMinBytes && !table.AutoStatsEnabled {
			autoStatsOff = append(autoStatsOff, fmt.Sprintf("%s · %s MVCC data", name, humanBytes(table.DataBytes)))
			autoStatsObjects = append(autoStatsObjects, object)
		}
		garbageBytes := table.DataBytes - table.LiveDataBytes
		if table.DataBytes >= crdbMVCCGarbageMinBytes && garbageBytes >= crdbMVCCGarbageMinBytes && table.LiveDataRatio <= crdbMVCCLiveRatioWarn {
			mvccGarbage = append(mvccGarbage, fmt.Sprintf("%s · %.1f%% live · %s non-live MVCC data", name, table.LiveDataRatio*100, humanBytes(garbageBytes)))
			garbageObjects = append(garbageObjects, object)
			totalGarbage += garbageBytes
		}
	}
	if len(metadataErrors) > 0 {
		add(model.Finding{
			ID: "crdb_table_metadata_error", Severity: model.SeverityWarn,
			Title:    fmt.Sprintf("CockroachDB could not refresh metadata for %d table(s)", len(metadataErrors)),
			Detail:   "The table metadata cache retained an update error for these tables, so their size, range, replica, and live-data values may describe an older successful refresh.",
			Evidence: cap10(metadataErrors), Objects: metadataObjects,
			Remediation: "Inspect the UPDATE TABLE METADATA CACHE job and node connectivity, correct the recorded error, then refresh the cache from DB Console and rerun pgbot.",
			Impact:      impact(model.DimRisk, 45, fmt.Sprintf("%d tables with stale or missing metadata", len(metadataErrors)), "CockroachDB table-metadata cache update errors"),
			Confidence:  1.0,
			Caveats:     []string{"this finding grades diagnostic coverage; it does not by itself prove that SQL traffic is failing"},
		})
	}
	if len(missingStats) > 0 {
		add(model.Finding{
			ID: "crdb_table_stats_missing", Severity: model.SeverityWarn,
			Title:    fmt.Sprintf("%d substantial CockroachDB table(s) have no optimizer statistics", len(missingStats)),
			Detail:   "Without table statistics, the cost-based optimizer estimates row counts and selectivity from defaults, which can produce poor join orders and scan choices.",
			Evidence: cap10(missingStats), Objects: missingObjects,
			Remediation: "Check recent automatic statistics jobs and table-level settings; after fixing the cause, run CREATE STATISTICS for the affected table if an immediate refresh is needed.",
			Impact:      impact(model.DimLatency, math.Max(50, sizeScore(worstMissingBytes)), fmt.Sprintf("%d tables without optimizer statistics", len(missingStats)), "stats_last_updated is absent on tables with at least 64 MiB of MVCC data"),
			Confidence:  0.8,
			Caveats:     []string{"the table metadata API is cached; confirm with SHOW STATISTICS before changing production settings"},
		})
	}
	if len(autoStatsOff) > 0 {
		add(model.Finding{
			ID: "crdb_auto_stats_disabled", Severity: model.SeverityInfo,
			Title:    fmt.Sprintf("automatic statistics are disabled on %d substantial table(s)", len(autoStatsOff)),
			Detail:   "These tables will not refresh optimizer statistics automatically as their data changes. This can be intentional for a controlled bulk-loading workflow, but otherwise allows plans to drift away from the data distribution.",
			Evidence: cap10(autoStatsOff), Objects: autoStatsObjects,
			Remediation: "If the override is not intentional, enable it with ALTER TABLE ... SET (sql_stats_automatic_collection_enabled = true); otherwise schedule and verify manual CREATE STATISTICS runs.",
			Impact:      impact(model.DimLatency, 30, fmt.Sprintf("%d tables require manual statistics maintenance", len(autoStatsOff)), "table-level automatic statistics state from CockroachDB metadata"),
			Confidence:  0.9,
		})
	}
	if len(mvccGarbage) > 0 {
		add(model.Finding{
			ID: "crdb_mvcc_garbage_pressure", Severity: model.SeverityWarn,
			Title:    fmt.Sprintf("%d CockroachDB table(s) retain a large non-live MVCC footprint", len(mvccGarbage)),
			Detail:   "At most half of the cached MVCC data for these tables is live, and at least 1 GiB is non-live. Heavy churn, a long GC TTL, or protected timestamps can retain old versions and increase storage and scan work.",
			Evidence: cap10(mvccGarbage), Objects: garbageObjects,
			Remediation: "Check the table's GC TTL, protected timestamps, long transactions, changefeeds, and backup activity. Address the retention cause and let CockroachDB garbage collection reclaim old versions; do not run PostgreSQL VACUUM.",
			Impact:      impact(model.DimStorage, sizeScore(totalGarbage), humanBytes(totalGarbage)+" non-live MVCC data across replicas", "cached total_data_bytes minus total_live_data_bytes"),
			Confidence:  0.75,
			Caveats:     []string{"this is CockroachDB MVCC history, not PostgreSQL heap bloat; retained versions may be intentional, and MVCC bytes do not map one-to-one to physical disk recovery"},
		})
	}
}

func cockroachTableObject(table model.TableStat) string {
	parts := []string{table.Database, table.Schema, table.Name}
	var qualified []string
	for _, part := range parts {
		if part != "" {
			qualified = append(qualified, part)
		}
	}
	return strings.Join(qualified, ".")
}

func cockroachIndexObject(ix model.IndexStat) string {
	parts := []string{ix.Database, ix.Schema, ix.Table}
	var qualified []string
	for _, part := range parts {
		if part != "" {
			qualified = append(qualified, part)
		}
	}
	return strings.Join(qualified, ".") + "/" + ix.Name
}

func crdbIndexCounterConfidence(c *model.Context, threshold time.Duration) (float64, string) {
	if c.Health == nil || c.Health.Cockroach == nil || c.CollectedAt.IsZero() {
		return 0.5, "node uptime is unavailable, so pgbot cannot prove that the observation window survived restarts"
	}
	known := 0
	recent := 0
	for _, node := range c.Health.Cockroach.Nodes {
		if node.Status != "" && node.Status != "live" {
			continue
		}
		if node.StartedAt.IsZero() || node.StartedAt.After(c.CollectedAt) {
			continue
		}
		known++
		if c.CollectedAt.Sub(node.StartedAt) < threshold {
			recent++
		}
	}
	if recent > 0 {
		return 0.35, fmt.Sprintf("%d live node(s) started within the %s observation threshold, so missing reads may have been reset", recent, shortDur(int64(threshold.Seconds())))
	}
	if known == 0 {
		return 0.5, "node uptime is unavailable, so pgbot cannot prove that the observation window survived restarts"
	}
	return 0.7, "counter continuity is inferred from node start times; it is not durable proof of a complete workload cycle"
}

func crdbQueryObject(fingerprint string, id int64) string {
	if fingerprint != "" {
		return "q:" + fingerprint
	}
	return fmt.Sprintf("q:%d", id)
}

func isRisk(f model.Finding) bool { return f.Impact.Dimension == model.DimRisk }

// StableObject reports whether s is a suppression-safe object identity (B2-0):
// empty (cluster-scoped), a typed identifier (setting:/slot:/sub:/q:/db:), or a
// schema-qualified relation ("schema.name"). It REJECTS a bare all-digit string,
// which would be an ephemeral pid/oid/LSN — a suppression rule keyed on one of
// those matches a different session tomorrow and silences the wrong thing.
func StableObject(s string) bool {
	if s == "" {
		return true
	}
	for _, p := range []string{"setting:", "slot:", "sub:", "q:", "db:"} {
		if strings.HasPrefix(s, p) {
			return len(s) > len(p)
		}
	}
	// Relation form: must contain a dot and not be all digits/dots.
	if !strings.Contains(s, ".") {
		return false
	}
	for _, r := range s {
		if r != '.' && (r < '0' || r > '9') {
			return true // has a non-digit → a real relation name, not a PID
		}
	}
	return false
}

// impact builds a scored Impact. score is clamped to [0,100].
func impact(dim string, score float64, estimate, basis string) model.Impact {
	if score < 0 {
		score = 0
	}
	if score > 100 {
		score = 100
	}
	return model.Impact{Score: score, Dimension: dim, Estimate: estimate, Basis: basis}
}

// safety, prohibition, and precondition build the structured guards for a finding
// whose remediation involves a destructive or irreversible action. Emitted here,
// in code — never left to a summarizing model — so they are guaranteed in --json,
// SARIF, and MCP, and asserted by model-free tests keyed on guard ID.
func safety(guards ...model.SafetyGuard) *model.Safety {
	return &model.Safety{BlockingCaveats: guards}
}

// prohibition: never do Action while the finding's state holds (Verify is nil).
func prohibition(id, action, text string) model.SafetyGuard {
	return model.SafetyGuard{ID: id, Kind: model.GuardProhibition, Action: action, Text: text}
}

// precondition: Action is permitted only after the Verify check passes.
func precondition(id, action, text, verify string) model.SafetyGuard {
	return model.SafetyGuard{ID: id, Kind: model.GuardPrecondition, Action: action, Text: text, Verify: &verify}
}

// sizeScore maps a byte count onto 0..100 on a log scale: ~1 MiB scores near 0,
// ~100 GiB scores near 100. Storage wins are naturally logarithmic — the gap
// between 8 KB and 12 GB should dwarf the gap between 12 GB and 20 GB.
func sizeScore(bytes int64) float64 {
	if bytes <= 0 {
		return 0
	}
	l := math.Log10(float64(bytes))
	s := (l - 6) / (11 - 6) * 100 // 1e6 → 0, 1e11 → 100
	if s < 0 {
		return 0
	}
	if s > 100 {
		return 100
	}
	return s
}

func sev(s string) int {
	switch s {
	case model.SeverityCritical:
		return 3
	case model.SeverityWarn:
		return 2
	default:
		return 1
	}
}

func blockingChains(c *model.Context, add func(model.Finding)) {
	if c.Locks == nil || c.Locks.BlockedCount == 0 {
		return
	}
	ev := make([]string, 0, len(c.Locks.Chains))
	for _, ch := range c.Locks.Chains {
		ev = append(ev, fmt.Sprintf("pid %d blocked %.0fs", ch.BlockedPID, ch.WaitSeconds))
	}
	var maxWait float64
	for _, ch := range c.Locks.Chains {
		if ch.WaitSeconds > maxWait {
			maxWait = ch.WaitSeconds
		}
	}
	add(model.Finding{
		ID: "blocking_chains", Severity: model.SeverityCritical,
		Title:       fmt.Sprintf("%d session(s) blocked on locks right now", c.Locks.BlockedCount),
		Detail:      "One or more sessions are waiting on locks held by others. Sustained blocking stalls throughput and can cascade.",
		Evidence:    ev,
		Remediation: "Find the lock holder and let it commit, or terminate it with pg_terminate_backend() if it's stuck.",
		Impact: impact(model.DimRisk, math.Min(100, 80+float64(c.Locks.BlockedCount)*3+maxWait/10),
			fmt.Sprintf("%d blocked, longest %.0fs", c.Locks.BlockedCount, maxWait),
			"live pg_locks blocked/blocking chains"),
		Confidence: 1.0, // it is happening right now
	})
}

// invalidIndexes flags indexes with indisvalid=false — a failed CREATE INDEX
// CONCURRENTLY (or REINDEX CONCURRENTLY). This is a gauge (valid immediately,
// even on a cold window). What an invalid index COSTS depends on the rest of its
// pg_index state (issue #11), so each one is classified, and the severity
// follows the worst class present:
//
//   - indisready = true  → PostgreSQL still maintains it on every INSERT/UPDATE/
//     DELETE and never reads it: full write + WAL cost for nothing → critical.
//   - indisready = false → the build failed BEFORE the index was populated
//     (typically 0 bytes); INSERT/UPDATE ignore it, so there is NO write cost.
//     It is failed-build debris: it occupies the name, wastes whatever was written,
//     and the index you meant to have doesn't exist → warn.
//   - indislive = false  → being dropped, ignored for all purposes → warn.
func invalidIndexes(c *model.Context, add func(model.Finding)) {
	if c.Schema == nil {
		return
	}
	var ev, objs []string
	var maintained, debris, dying int
	var maintainedBytes, debrisBytes int64
	for _, o := range c.Schema.Objects {
		if o.Kind != "index" || !o.Invalid {
			continue
		}
		objs = append(objs, o.Identity)
		switch {
		case !o.IndexLive:
			dying++
			ev = append(ev, fmt.Sprintf("%s — indisvalid = false, indislive = false: being dropped, ignored for all purposes (%s)", o.Identity, humanBytes(o.Bytes)))
		case o.IndexReady:
			maintained++
			maintainedBytes += o.Bytes
			ev = append(ev, fmt.Sprintf("%s — indisvalid = false, indisready = true: maintained on every write, never read (%s)", o.Identity, humanBytes(o.Bytes)))
		default:
			debris++
			debrisBytes += o.Bytes
			ev = append(ev, fmt.Sprintf("%s — indisvalid = false, indisready = false: failed-build debris, NOT maintained on writes (%s)", o.Identity, humanBytes(o.Bytes)))
		}
	}
	if len(ev) == 0 {
		return
	}

	// Detail states the cost the catalog actually supports — never a blanket
	// "maintained on every write" for an index Postgres ignores on writes.
	const maintainedText = "An invalid index that is still indisready is maintained on every write (INSERT/UPDATE/DELETE, plus WAL) but never used to serve reads — full write cost for zero benefit. It's the leftover of a CREATE INDEX CONCURRENTLY that failed after the build phase (or a REINDEX CONCURRENTLY that failed after the swap)."
	const debrisText = "An invalid index with indisready = false is failed-build debris: it is not maintained on writes (INSERT/UPDATE ignore it, per indisready), so it costs no write overhead — a CREATE INDEX CONCURRENTLY that failed before or during the build (a duplicate key on a unique build, a timeout, a cancelled session). It still needs cleanup: it occupies the index name (a retry with the same name fails), wastes any pages written before the failure, and — most importantly — the index you meant to have does not exist, so the queries it was for run unindexed."
	var detail string
	switch {
	case maintained > 0 && debris+dying > 0:
		detail = fmt.Sprintf("%d maintained on writes (indisready = true) and %d not maintained (failed-build debris or being dropped). %s %s",
			maintained, debris+dying, maintainedText, debrisText)
	case maintained > 0:
		detail = maintainedText
	default:
		detail = debrisText
	}
	sev := model.SeverityWarn
	score := 45.0
	estimate := fmt.Sprintf("%d invalid index(es), none maintained on writes, %s of debris", len(ev), humanBytes(debrisBytes))
	if maintained > 0 {
		sev = model.SeverityCritical
		score = 85
		estimate = fmt.Sprintf("%d invalid index(es), %d maintained on every write (%s)", len(ev), maintained, humanBytes(maintainedBytes))
	}

	// A CREATE INDEX CONCURRENTLY still RUNNING shows an invalid index too — but
	// it's building, not failed. If pg_stat_progress_create_index has a row, don't
	// cry wolf: caveat it and drop confidence rather than telling the user to drop
	// an index that's about to become valid.
	var caveats []string
	conf := 1.0
	guard := precondition("invalid_index.no_build_running", model.ActionDropIndex,
		"An invalid index left by a stopped build can be dropped and rebuilt — but a plain DROP INDEX takes an ACCESS EXCLUSIVE lock.",
		"no CREATE INDEX build is running, then DROP INDEX CONCURRENTLY and rebuild")
	if createIndexInProgress(c) {
		caveats = append(caveats, "a CREATE INDEX CONCURRENTLY is currently running (see progress) — an index invalid because it's still building is normal; only a build that has stopped needs the drop-and-rebuild")
		conf = 0.5
		sev = model.SeverityWarn
		guard = prohibition("invalid_index.build_in_progress", model.ActionDropIndex,
			"A CREATE INDEX CONCURRENTLY is still building this index — dropping it now kills a build seconds before it goes valid.")
	}
	add(model.Finding{
		ID: "index_invalid", Severity: sev,
		Title:       fmt.Sprintf("%d invalid index(es) — failed CREATE INDEX CONCURRENTLY", len(ev)),
		Detail:      detail,
		Evidence:    ev,
		Objects:     objs,
		Remediation: "If no build is running, drop and recreate it: DROP INDEX CONCURRENTLY <name>; then rebuild.",
		Impact: impact(model.DimRisk, score, estimate,
			"pg_index.indisvalid = false, classified by indisready / indislive + pg_relation_size"),
		Confidence: conf,
		Caveats:    caveats,
		Safety:     safety(guard),
	})
}

// createIndexInProgress reports whether a CREATE INDEX [CONCURRENTLY] is running.
func createIndexInProgress(c *model.Context) bool {
	if c.Progress == nil {
		return false
	}
	for _, op := range c.Progress.Operations {
		if op.Operation == "create_index" {
			return true
		}
	}
	return false
}

// unusedIndex represents one flagged index with its per-index score, so the
// aggregate finding's evidence lists the biggest, write-taxing targets first.
type unusedIndex struct {
	stat      model.IndexStat
	writeHeav bool
	score     float64
}

func unusedIndexes(c *model.Context, add func(model.Finding), tun Tunables) {
	if c.Server.Engine == "cockroachdb" || c.Indexes == nil {
		return
	}
	// Cold window (serverless just woke, or stats reset < 15 min ago): index-scan
	// counts start from zero, so "unused" is meaningless and acting on it is
	// actively dangerous. Suppress entirely (T2). Constraint-backing indexes are
	// already excluded upstream in the collector (T9.3).
	if c.Window.ColdWindow() {
		return
	}
	writeTables := writeHeavyTables(c)
	var found []unusedIndex
	var total int64
	anyWriteHeavy := false
	for _, ix := range c.Indexes.Unused {
		if ix.Bytes < tun.UnusedIndexMinBytes {
			continue // below the floor: not worth a recommendation (the 8 KB case)
		}
		wh := writeTables[ix.Schema+"."+ix.Table]
		// Storage score: size (log scale), lifted when the parent table is
		// write-heavy — there the index also taxes every INSERT/UPDATE.
		sc := sizeScore(ix.Bytes)
		if wh {
			sc = math.Min(100, sc*1.3)
		}
		found = append(found, unusedIndex{stat: ix, writeHeav: wh, score: sc})
		total += ix.Bytes
		anyWriteHeavy = anyWriteHeavy || wh
	}
	if len(found) == 0 {
		return
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].score > found[j].score })

	var ev, objs []string
	partialSeen, exprSeen := false, false
	for _, u := range found {
		tag := ""
		switch {
		case u.stat.Partial:
			tag, partialSeen = " [partial]", true
		case u.stat.Expression:
			tag, exprSeen = " [expression]", true
		}
		wh := ""
		if u.writeHeav {
			wh = " · write-heavy table"
		}
		ev = append(ev, fmt.Sprintf("%s.%s (%s%s)%s", u.stat.Table, u.stat.Name, humanBytes(u.stat.Bytes), wh, tag))
		objs = append(objs, u.stat.Schema+"."+u.stat.Name)
	}

	// Confidence + caveats from the counter-evidence checks (T9.3).
	confidence := 0.8
	var caveats []string
	if winSec := windowAgeSeconds(c); winSec > 0 && winSec < shortStatsWindowSec {
		confidence = math.Min(confidence, 0.4)
		caveats = append(caveats, fmt.Sprintf("stats span only %s — an index used by a less-frequent path may look unused", shortDur(winSec)))
	}
	if replicationInUse(c) {
		// NEVER optional: index stats are per-node; a primary cannot see reads a
		// replica serves. The single most likely way pgbot causes an outage.
		caveats = append(caveats, "replication is active — these scan counts are from THIS node only; a replica may be using an index that looks unused here")
	}
	if partialSeen {
		caveats = append(caveats, "one or more are partial indexes — they may serve a narrow but critical path")
	}
	if exprSeen {
		caveats = append(caveats, "one or more are expression indexes — they may serve a specific query shape")
	}
	if anyWriteHeavy {
		caveats = append(caveats, "some sit on write-heavy tables — confirm no month-end or scheduled job relies on them outside this window")
	}

	estimate := "≈" + humanBytes(total) + " reclaimable"
	basis := fmt.Sprintf("%d zero-scan index(es), %s total, size×write-rate weighted", len(found), humanBytes(total))
	rem := fmt.Sprintf("Reclaims %s of storage.", humanBytes(total))
	if anyWriteHeavy {
		rem += " Those on write-heavy tables also tax every INSERT/UPDATE."
	}
	rem += " Drop with DROP INDEX CONCURRENTLY after confirming the caveats."
	add(model.Finding{
		ID: "unused_indexes", Severity: model.SeverityWarn,
		Title:       fmt.Sprintf("%d unused index(es) · %s", len(found), humanBytes(total)),
		Detail:      "These indexes have zero scans since stats began. They cost storage and slow writes without serving reads.",
		Evidence:    ev,
		Objects:     objs,
		Remediation: rem,
		Impact:      impact(model.DimStorage, found[0].score, estimate, basis),
		Confidence:  confidence,
		Caveats:     caveats,
		Safety: safety(precondition("unused_index.per_node", model.ActionDropIndex,
			"These are zero scans in ONE window on THIS node — a rarely-run or replica-only query still needs the index.",
			"the index is unused on EVERY node in the cluster (including replicas) and by any periodic/off-window job, then DROP INDEX CONCURRENTLY")),
	})
}

func bloatedTables(c *model.Context, add func(model.Finding), tun Tunables) {
	if c.Server.Engine == "cockroachdb" || c.Tables == nil {
		return
	}
	var ev, objs []string
	var worstDeadBytes float64
	autovacKeepingPace := true
	for _, t := range c.Tables.Top {
		if t.DeadRatio >= tun.DeadRatioWarn && t.LiveTuples+t.DeadTuples >= deadRatioTableMinRows {
			ev = append(ev, fmt.Sprintf("%s.%s %.0f%% dead (%d rows)", t.Schema, t.Name, t.DeadRatio*100, t.DeadTuples))
			objs = append(objs, t.Schema+"."+t.Name)
			if db := t.DeadRatio * float64(t.TotalBytes); db > worstDeadBytes {
				worstDeadBytes = db
			}
			if t.LastAutovac == nil { // no recent autovacuum on a bloated table → not keeping pace
				autovacKeepingPace = false
			}
		}
	}
	if len(ev) == 0 {
		return
	}
	score := sizeScore(int64(worstDeadBytes))
	if autovacKeepingPace {
		score *= 0.6 // autovacuum has run recently — the bloat is likely transient
	}
	rem := "VACUUM the worst tables, and tune autovacuum (scale factor / cost limit) if the ratio stays high."
	if c.Horizon != nil && len(c.Horizon.Holders) > 0 && c.Horizon.Holders[0].XminAge >= vacuumHorizonWarnXIDs {
		rem = "First check vacuum_horizon_blocked — an xmin holder is pinning the horizon, so VACUUM can't reclaim these until it's released. " + rem
	}
	add(model.Finding{
		ID: "table_bloat", Severity: model.SeverityWarn,
		Title:       fmt.Sprintf("%d table(s) with high dead-tuple ratio", len(ev)),
		Detail:      "Dead tuples inflate table size and slow scans until autovacuum reclaims them. A persistently high ratio suggests vacuum isn't keeping up.",
		Evidence:    ev,
		Objects:     objs,
		Remediation: rem,
		Impact: impact(model.DimStorage, score,
			"≈"+humanBytes(int64(worstDeadBytes))+" dead in the worst table",
			"max(dead_ratio × table size)"+map[bool]string{true: ", discounted (autovacuum recent)", false: ""}[autovacKeepingPace]),
		Confidence: 0.7,
	})
}

// seqScanHeavy flags a large table doing far more sequential scans than index
// scans — often a query that lost (or never had) an index path.
// redundantIndexes flags an index whose leading columns are a prefix of (or
// identical to) another index on the same table — the wider one already serves
// the same lookups, so the narrower one is pure write/storage cost. Detected
// structurally (indkey/indclass containment) in the collector, with the same
// constraint exclusions the unused-index rule uses; carries the same per-node
// replica caveat, since a replica's access paths can differ.
func redundantIndexes(c *model.Context, add func(model.Finding)) {
	if c.Indexes == nil || len(c.Indexes.Redundant) == 0 {
		return
	}
	var ev, objs []string
	var total int64
	for _, r := range c.Indexes.Redundant {
		ev = append(ev, fmt.Sprintf("%s.%s (%s) — prefix of %s", r.Table, r.Name, humanBytes(r.Bytes), r.CoveredBy))
		objs = append(objs, r.Schema+"."+r.Name)
		total += r.Bytes
	}
	var caveats []string
	if replicationInUse(c) {
		caveats = append(caveats, "index usage is per-node — confirm the covering index serves the same queries on any replicas before dropping")
	}
	add(model.Finding{
		ID: "redundant_indexes", Severity: model.SeverityInfo,
		Title:       fmt.Sprintf("%d redundant index(es) · %s", len(ev), humanBytes(total)),
		Detail:      "Each of these has leading columns that are a prefix of a wider index on the same table, so that index already serves the same lookups. The narrower one costs writes and storage for no extra read benefit.",
		Evidence:    ev,
		Objects:     objs,
		Remediation: fmt.Sprintf("After confirming the covering index serves its queries, DROP INDEX CONCURRENTLY the redundant one. Reclaims ≈%s.", humanBytes(total)),
		Impact:      impact(model.DimStorage, sizeScore(total), "≈"+humanBytes(total)+" reclaimable", fmt.Sprintf("%d prefix-redundant indexes (indkey/indclass containment)", len(ev))),
		Confidence:  0.75,
		Caveats:     caveats,
		Safety: safety(precondition("redundant_index.covering_equivalent", model.ActionDropIndex,
			"A leading-prefix match can still miss a difference the covering index does not cover (extra INCLUDE columns, a different opclass or collation), and index choice is per-node.",
			"the covering index has the same INCLUDE columns, opclass, and collation, and is the one used on any replicas, then DROP INDEX CONCURRENTLY")),
	})
}

// unindexedForeignKeys flags FK constraints with no supporting index on the
// child. A parent DELETE/UPDATE then sequentially scans the whole child to check
// references and holds locks longer — cheap to miss, expensive at scale.
func unindexedForeignKeys(c *model.Context, add func(model.Finding)) {
	if c.Indexes == nil || len(c.Indexes.UnindexedFKs) == 0 {
		return
	}
	var ev, objs []string
	var worst int64
	for _, fk := range c.Indexes.UnindexedFKs {
		ev = append(ev, fmt.Sprintf("%s.%s (%s) on (%s)", fk.Schema, fk.Table, humanBytes(fk.ChildBytes), fk.Columns))
		objs = append(objs, fk.Schema+"."+fk.Table)
		if fk.ChildBytes > worst {
			worst = fk.ChildBytes
		}
	}
	add(model.Finding{
		ID: "fk_unindexed", Severity: model.SeverityWarn,
		Title:       fmt.Sprintf("%d foreign key(s) without a supporting index", len(ev)),
		Detail:      "A foreign key with no index on the child means every DELETE or UPDATE of a referenced parent row sequentially scans the whole child to check references, escalating lock duration. It stays invisible until the child grows.",
		Evidence:    ev,
		Objects:     objs,
		Remediation: "Add an index on the child's FK columns: CREATE INDEX CONCURRENTLY ON child (fk_columns).",
		Impact:      impact(model.DimLatency, sizeScore(worst), "largest child "+humanBytes(worst), fmt.Sprintf("%d FKs with no leading-column index", len(ev))),
		Confidence:  0.85,
	})
}

// partitionSeqScanHeavy catches a partitioned table scanned end-to-end: across
// all partitions the aggregate seq scans dominate index scans, even though each
// partition's own count looks harmless (so seqScanHeavy, which is per-relation,
// misses it). Usually a missing index or a query that can't prune partitions.
func partitionSeqScanHeavy(c *model.Context, add func(model.Finding)) {
	if c.Server.Engine == "cockroachdb" || c.Tables == nil || c.Window.ColdWindow() {
		return
	}
	var ev, objs []string
	for _, p := range c.Tables.Partitioned {
		if p.LiveTuples < seqScanTableMinRows || p.SeqScans < partitionSeqScanMin {
			continue
		}
		if p.IndexScans > 0 && p.SeqScans < p.IndexScans {
			continue // index-dominated access → healthy
		}
		ev = append(ev, fmt.Sprintf("%s.%s (%d partitions, %s): %s seq vs %s index scans",
			p.Schema, p.Name, p.Partitions, humanBytes(p.TotalBytes), human(p.SeqScans), human(p.IndexScans)))
		objs = append(objs, p.Schema+"."+p.Name)
	}
	if len(ev) == 0 {
		return
	}
	add(model.Finding{
		ID: "partition_seq_scan_heavy", Severity: model.SeverityWarn,
		Title:       fmt.Sprintf("%d partitioned table(s) scanned end-to-end", len(ev)),
		Detail:      "Rolled up across its partitions, this table takes far more sequential than index scans. Each partition's own count looks harmless, so a per-relation view misses it — the parent is being read end to end, usually a missing index or a query that can't prune partitions.",
		Evidence:    ev,
		Objects:     objs,
		Remediation: "Add an index matching the predicate (create it on the parent; it propagates to partitions), and confirm queries include the partition key so the planner can prune.",
		Impact:      impact(model.DimLatency, 55, "partitioned end-to-end scan", "rolled-up seq vs index scans across partitions"),
		Confidence:  0.7,
	})
}

func seqScanHeavy(c *model.Context, add func(model.Finding)) {
	if c.Server.Engine == "cockroachdb" || c.Tables == nil || c.Window.ColdWindow() { // scan counts are cold-window-sensitive
		return
	}
	var ev, objs []string
	var worstScanRows int64
	for _, t := range c.Tables.Top {
		total := t.SeqScans + t.IndexScans
		if t.LiveTuples >= seqScanTableMinRows && total >= 100 && t.SeqScans > t.IndexScans*2 {
			ev = append(ev, fmt.Sprintf("%s.%s %s seq scans vs %s index (%s rows)",
				t.Schema, t.Name, human(t.SeqScans), human(t.IndexScans), human(t.LiveTuples)))
			objs = append(objs, t.Schema+"."+t.Name)
			if r := t.SeqScans * t.LiveTuples; r > worstScanRows {
				worstScanRows = r
			}
		}
	}
	if len(ev) == 0 {
		return
	}
	f := model.Finding{
		ID: "seq_scan_heavy", Severity: model.SeverityWarn,
		Title:       fmt.Sprintf("%d table(s) sequential-scanning heavily", len(ev)),
		Detail:      "These tables are read mostly by full scans rather than index lookups. On a large table that's CPU and IO the database repeats on every query.",
		Evidence:    ev,
		Objects:     objs,
		Remediation: "Add an index for the hot predicate, or confirm the full scans are intended (small lookup tables, analytics).",
		Impact: impact(model.DimThroughput, math.Min(90, 40+math.Log10(float64(worstScanRows)+1)*8),
			fmt.Sprintf("%d table(s) scanning full", len(ev)),
			"seq_scans ≫ index_scans on tables ≥ 50k rows"),
		Confidence: 0.6,
	}
	// If hypopg is installed, pgbot can turn "add an index for the hot predicate"
	// from advice into evidence: `pgbot advise` proposes indexes and only reports
	// the ones the planner confirms it would use (B1).
	if hasExtension(c, "hypopg") {
		f.Caveats = append(f.Caveats, "run `pgbot advise` — with hypopg installed it validates candidate indexes against the planner (nothing is built) instead of guessing")
	}
	add(f)
}

// hasExtension reports whether the named extension is installed on the target.
func hasExtension(c *model.Context, name string) bool {
	for _, e := range c.Server.Extensions {
		if e == name {
			return true
		}
	}
	return false
}

// autovacuumHealth answers whether autovacuum is being OUTRUN (distinct from A1's
// "why can't it reclaim"): a table excluded from it, one never vacuumed, dead
// tuples piling past the trigger, worker saturation, and a long-running worker.
func autovacuumHealth(c *model.Context, add func(model.Finding)) {
	if c.Server.Engine == "cockroachdb" {
		return
	}
	if c.Tables != nil {
		gThresh := settingFloat(c, "autovacuum_vacuum_threshold", 50)
		gScale := settingFloat(c, "autovacuum_vacuum_scale_factor", 0.2)
		var disabled, never, starved []string
		var disabledObj, neverObj, starvedObj []string
		for _, t := range c.Tables.Top {
			if t.AutovacuumDisabled {
				disabled = append(disabled, fmt.Sprintf("%s.%s", t.Schema, t.Name))
				disabledObj = append(disabledObj, t.Schema+"."+t.Name)
			}
			if t.LiveTuples < deadRatioTableMinRows {
				continue
			}
			if t.LastVacuum == nil && t.LastAutovac == nil {
				never = append(never, fmt.Sprintf("%s.%s (%s rows)", t.Schema, t.Name, human(t.LiveTuples)))
				neverObj = append(neverObj, t.Schema+"."+t.Name)
				continue
			}
			th, sf := gThresh, gScale
			if t.VacuumThresholdOverride != nil {
				th = *t.VacuumThresholdOverride
			}
			if t.VacuumScaleOverride != nil {
				sf = *t.VacuumScaleOverride
			}
			trigger := th + sf*float64(t.LiveTuples)
			// Dead tuples well past the trigger while no autovacuum has run: it's behind.
			if trigger > 0 && float64(t.DeadTuples) >= 1.5*trigger && t.LastAutovac == nil {
				starved = append(starved, fmt.Sprintf("%s.%s: %s dead (trigger ≈%s), no autovacuum recorded", t.Schema, t.Name, human(t.DeadTuples), human(int64(trigger))))
				starvedObj = append(starvedObj, t.Schema+"."+t.Name)
			}
		}
		if len(disabled) > 0 {
			add(model.Finding{
				ID: "autovacuum_disabled_on_table", Severity: model.SeverityCritical,
				Title:       fmt.Sprintf("autovacuum is DISABLED on %d table(s)", len(disabled)),
				Detail:      "These tables have autovacuum_enabled=false in their reloptions — almost always disabled 'temporarily' during a migration and never re-enabled. Dead tuples and transaction-id age accumulate on them unchecked, invisible in every global setting and dashboard, until bloat or wraparound forces a reckoning.",
				Evidence:    disabled,
				Objects:     disabledObj,
				Remediation: "Re-enable it: ALTER TABLE … SET (autovacuum_enabled = true); then VACUUM the table to catch up.",
				Impact:      impact(model.DimRisk, 80, fmt.Sprintf("%d tables excluded from autovacuum", len(disabled)), "reloptions autovacuum_enabled=false"),
				Confidence:  1.0,
			})
		}
		if len(never) > 0 {
			add(model.Finding{
				ID: "table_never_vacuumed", Severity: model.SeverityWarn,
				Title:       fmt.Sprintf("%d table(s) never vacuumed", len(never)),
				Detail:      "These tables above the size floor have no record of a manual or automatic vacuum. Dead tuples and transaction-id age accumulate until the first one runs.",
				Evidence:    never,
				Objects:     neverObj,
				Remediation: "VACUUM them, and check autovacuum isn't disabled globally or per-table.",
				Impact:      impact(model.DimRisk, 40, fmt.Sprintf("%d unvacuumed tables", len(never)), "last_vacuum and last_autovacuum both null"),
				Confidence:  0.75,
			})
		}
		if len(starved) > 0 {
			add(model.Finding{
				ID: "autovacuum_starved", Severity: model.SeverityWarn,
				Title:       fmt.Sprintf("%d table(s) with dead tuples past the trigger and no autovacuum", len(starved)),
				Detail:      "Dead tuples on these tables are well past the autovacuum trigger, yet no autovacuum has run — autovacuum is being outrun (not enough workers or cost budget), not blocked at the horizon.",
				Evidence:    starved,
				Objects:     starvedObj,
				Remediation: "Raise autovacuum_max_workers / autovacuum_vacuum_cost_limit or lower autovacuum_vacuum_cost_delay so vacuum keeps pace; VACUUM the worst tables now.",
				Impact:      impact(model.DimStorage, 45, fmt.Sprintf("%d tables past the vacuum trigger", len(starved)), "dead_tuples vs threshold + scale × n_live_tup"),
				Confidence:  0.6,
			})
		}
	}

	// autovacuum_saturated — point-in-time worker count vs the cap (a single glance,
	// so a caveat and modest confidence; the ASH-window version is a refinement).
	if c.Activity != nil && c.Activity.AutovacuumWorkers > 0 {
		if maxW, err := strconv.Atoi(settingParam(c, "autovacuum_max_workers")); err == nil && maxW > 0 && c.Activity.AutovacuumWorkers >= maxW {
			add(model.Finding{
				ID: "autovacuum_saturated", Severity: model.SeverityWarn,
				Title:       fmt.Sprintf("autovacuum workers saturated (%d/%d in use)", c.Activity.AutovacuumWorkers, maxW),
				Detail:      "Every autovacuum worker is busy. If this is sustained, vacuum falls behind on the tables waiting in line — dead tuples and bloat grow while workers are tied up elsewhere.",
				Remediation: "Raise autovacuum_max_workers, or reduce per-worker throttling (cost_delay/cost_limit) so each finishes faster.",
				Caveats:     []string{"this is a single-moment count, not sustained saturation — confirm it holds across several runs before acting"},
				Impact:      impact(model.DimThroughput, 35, fmt.Sprintf("%d/%d workers", c.Activity.AutovacuumWorkers, maxW), "point-in-time autovacuum worker count vs autovacuum_max_workers"),
				Confidence:  0.5,
			})
		}
		if c.Activity.AutovacuumMaxAgeSec >= autovacuumLongRunSec {
			add(model.Finding{
				ID: "autovacuum_long_running", Severity: model.SeverityInfo,
				Title:       fmt.Sprintf("an autovacuum worker has run for %.0fs", c.Activity.AutovacuumMaxAgeSec),
				Detail:      "A long-running autovacuum is usually a large table catching up, not a problem — but it ties up a worker. See pg_stat_progress_vacuum (progress) for its phase and how far along it is before considering a cancel.",
				Remediation: "Let it finish if it's making progress; only cancel a stuck one. Consider per-table cost settings for very large tables.",
				Impact:      impact(model.DimThroughput, 15, fmt.Sprintf("%.0fs", c.Activity.AutovacuumMaxAgeSec), "longest autovacuum worker xact age"),
				Confidence:  0.7,
			})
		}
	}
}

// staleStatistics flags tables whose planner statistics are far behind the data
// — the most common cause of the plan flip query_slowdown detects. Uses n_live_tup
// (NOT reltuples, which is -1 for never-analyzed tables on PG14+) and per-table
// reloption overrides of the analyze threshold/scale. never_analyzed is the
// stronger case: a table above the floor that has never been analyzed at all.
func staleStatistics(c *model.Context, add func(model.Finding)) {
	if c.Server.Engine == "cockroachdb" || c.Tables == nil {
		return
	}
	gThresh := settingFloat(c, "autovacuum_analyze_threshold", 50)
	gScale := settingFloat(c, "autovacuum_analyze_scale_factor", 0.1)

	var staleEv, neverEv []string
	var staleObj, neverObj2 []string
	for _, t := range c.Tables.Top {
		if t.LiveTuples < deadRatioTableMinRows {
			continue
		}
		if t.LastAnalyze == nil && t.LastAutoanalyze == nil {
			neverEv = append(neverEv, fmt.Sprintf("%s.%s (%s rows)", t.Schema, t.Name, human(t.LiveTuples)))
			neverObj2 = append(neverObj2, t.Schema+"."+t.Name)
			continue // never-analyzed is reported by its own finding, not stale
		}
		th, sf := gThresh, gScale
		if t.AnalyzeThresholdOverride != nil {
			th = *t.AnalyzeThresholdOverride
		}
		if t.AnalyzeScaleOverride != nil {
			sf = *t.AnalyzeScaleOverride
		}
		trigger := th + sf*float64(t.LiveTuples)
		if trigger > 0 && float64(t.ModsSinceAnalyze) >= 2*trigger {
			staleEv = append(staleEv, fmt.Sprintf("%s.%s: %s rows modified since analyze (trigger ≈%s)", t.Schema, t.Name, human(t.ModsSinceAnalyze), human(int64(trigger))))
			staleObj = append(staleObj, t.Schema+"."+t.Name)
		}
	}

	if len(staleEv) > 0 {
		f := model.Finding{
			ID: "stale_statistics", Severity: model.SeverityWarn,
			Title:       fmt.Sprintf("%d table(s) with stale planner statistics", len(staleEv)),
			Detail:      "These tables have changed far more than the autoanalyze threshold since their statistics were last refreshed, so the planner is estimating from an out-of-date picture — the usual cause of a sudden plan flip to a bad plan.",
			Evidence:    staleEv,
			Objects:     staleObj,
			Remediation: "ANALYZE the affected tables; if it recurs, lower autovacuum_analyze_scale_factor (globally or per-table).",
			Impact:      impact(model.DimLatency, 45, fmt.Sprintf("%d tables past 2× the analyze trigger", len(staleEv)), "n_mod_since_analyze vs threshold + scale × n_live_tup"),
			Confidence:  0.7,
		}
		// Cross-reference, NOT causation: pgbot can't map a normalized query to its
		// tables, so it only notes the two travel together.
		if querySlowdownPresent(c) {
			f.Related = append(f.Related, "query_slowdown")
			f.Caveats = append(f.Caveats, "a query regressed this run too (query_slowdown) — stale stats often cause plan flips, but pgbot can't confirm the slow query touches these tables; check whether it does")
		}
		add(f)
	}
	if len(neverEv) > 0 {
		add(model.Finding{
			ID: "never_analyzed", Severity: model.SeverityWarn,
			Title:       fmt.Sprintf("%d table(s) never analyzed", len(neverEv)),
			Detail:      "These tables have no statistics at all — the planner is guessing from defaults, which produces bad plans on anything non-trivial.",
			Evidence:    neverEv,
			Objects:     neverObj2,
			Remediation: "Run ANALYZE on them; check why autovacuum hasn't (disabled globally or per-table, or the table is newer than the last cycle).",
			Impact:      impact(model.DimLatency, 50, fmt.Sprintf("%d unanalyzed tables", len(neverEv)), "last_analyze and last_autoanalyze both null"),
			Confidence:  0.8,
		})
	}
}

// querySlowdownPresent reports whether a query.mean_ms regression exists this run
// (the same condition query_slowdown fires on).
func querySlowdownPresent(c *model.Context) bool {
	if c.Deltas == nil {
		return false
	}
	for i := range c.Deltas.Changes {
		d := &c.Deltas.Changes[i]
		if d.ID == "query.mean_ms" && d.Before > 0 && d.After >= querySlowdownMinMs && d.After/d.Before >= querySlowdownFactor {
			return true
		}
	}
	return false
}

// settingFloat reads a numeric setting, falling back to def.
func settingFloat(c *model.Context, name string, def float64) float64 {
	if v, err := strconv.ParseFloat(settingParam(c, name), 64); err == nil {
		return v
	}
	return def
}

// lowHotUpdateRatio flags a heavily-updated table where few updates are HOT
// (heap-only). A non-HOT update rewrites every index, so a low ratio means extra
// WAL, index bloat, and vacuum work — usually fillfactor 100 (no page free space)
// or an index on a frequently-updated column. Gated on update volume.
func lowHotUpdateRatio(c *model.Context, add func(model.Finding)) {
	if c.Server.Engine == "cockroachdb" || c.Tables == nil {
		return
	}
	var ev, objs []string
	worst := 1.0
	for _, t := range c.Tables.Top {
		if t.Updates < hotUpdateMinVolume {
			continue
		}
		ratio := float64(t.HotUpdates) / float64(t.Updates)
		if ratio >= hotUpdateRatioWarn {
			continue
		}
		if ratio < worst {
			worst = ratio
		}
		ev = append(ev, fmt.Sprintf("%s.%s: %.0f%% HOT (%s updates)", t.Schema, t.Name, ratio*100, human(t.Updates)))
		objs = append(objs, t.Schema+"."+t.Name)
	}
	if len(ev) == 0 {
		return
	}
	add(model.Finding{
		ID: "low_hot_update_ratio", Severity: model.SeverityWarn,
		Title:       fmt.Sprintf("%d table(s) with low HOT-update ratio (worst %.0f%%)", len(ev), worst*100),
		Detail:      "A HOT (heap-only tuple) update avoids rewriting every index. A low ratio means most updates either touch an indexed column or land on a page with no free space (fillfactor 100), so each update writes to every index — extra WAL, index bloat, and vacuum work.",
		Evidence:    ev,
		Objects:     objs,
		Remediation: "Lower the table's fillfactor (e.g. 90) to leave room for HOT updates, and avoid indexing columns that are updated frequently.",
		Impact:      impact(model.DimThroughput, 40, fmt.Sprintf("%.0f%% HOT on the worst table", worst*100), "n_tup_hot_upd / n_tup_upd"),
		Confidence:  0.7,
	})
}

func lowCacheHit(c *model.Context, add func(model.Finding)) {
	if c.Health == nil || c.Health.CacheHitRatio == nil {
		return
	}
	// Cache-hit over a cold window is dominated by cold-cache misses at wake and
	// says nothing about steady state. Suppress.
	if c.Window.ColdWindow() {
		return
	}
	// A ratio over a few hundred blocks is noise, not a signal — a few cold reads
	// swing it by tens of points between runs (the same reasoning as
	// rollbackMinTxns for high_rollback_ratio). PR#1.
	if !c.Health.CacheHitUsable() {
		return
	}
	if *c.Health.CacheHitRatio >= cacheHitWarn {
		return
	}
	miss := 1 - *c.Health.CacheHitRatio
	add(model.Finding{
		ID: "low_cache_hit", Severity: model.SeverityWarn,
		Title:       fmt.Sprintf("Cache hit ratio %.1f%% over the sample window", *c.Health.CacheHitRatio*100),
		Detail:      "A low buffer cache hit ratio means many reads are hitting disk. Sustained, it points to an undersized shared_buffers or a working set larger than RAM.",
		Remediation: "Confirm over a longer window, then consider raising shared_buffers or adding RAM.",
		Impact: impact(model.DimThroughput, math.Min(85, miss*300),
			fmt.Sprintf("%.0f%% of reads miss cache", miss*100),
			"1 − blks_hit/(blks_hit+blks_read) over the window"),
		Confidence: 0.6,
	})
}

func idleInTransaction(c *model.Context, add func(model.Finding)) {
	if c.Activity == nil || c.Activity.IdleInTransaction == 0 {
		return
	}
	sevr := model.SeverityInfo
	score := 30.0
	if c.Activity.LongestXactSec >= idleInTxnWarnSec {
		sevr = model.SeverityWarn
		score = math.Min(80, 50+c.Activity.LongestXactSec/30)
	}
	add(model.Finding{
		ID: "idle_in_transaction", Severity: sevr,
		Title:       fmt.Sprintf("%d session(s) idle in transaction", c.Activity.IdleInTransaction),
		Detail:      "Idle-in-transaction sessions hold locks and pin the xmin horizon, blocking vacuum. Long-lived ones are a common source of bloat and lock waits.",
		Remediation: "Find the session and fix the app's transaction handling; consider idle_in_transaction_session_timeout.",
		Impact: impact(model.DimRisk, score,
			fmt.Sprintf("%d session(s), longest %.0fs", c.Activity.IdleInTransaction, c.Activity.LongestXactSec),
			"pg_stat_activity idle-in-transaction count + age"),
		Confidence: 0.7,
	})
}

func longRunningXact(c *model.Context, add func(model.Finding)) {
	if c.Activity == nil || c.Activity.LongestXactSec < longXactWarnSec {
		return
	}
	add(model.Finding{
		ID: "long_running_transaction", Severity: model.SeverityWarn,
		Title:       fmt.Sprintf("Longest transaction open %.0fs", c.Activity.LongestXactSec),
		Detail:      "A long-running transaction holds back the xmin horizon so autovacuum can't remove dead rows created since it started.",
		Remediation: "Identify and end the transaction; long-lived read transactions should use a shorter scope.",
		Impact: impact(model.DimRisk, math.Min(85, 55+c.Activity.LongestXactSec/60),
			fmt.Sprintf("open %.0fs", c.Activity.LongestXactSec),
			"pg_stat_activity longest xact_start age"),
		Confidence: 0.8,
	})
}

// waitFindings reads the ASH profile (T8). It attributes TIME, not events, and
// works even when the cumulative counters reset minutes ago. Everything here is
// gated on a minimum sample count: a thin profile is noise, so nothing fires.
func waitFindings(c *model.Context, add func(model.Finding)) {
	w := c.WaitProfile
	if w == nil || !w.Available || w.Thin() {
		return
	}
	share := func(typ string) float64 {
		for _, b := range w.Buckets {
			if b.Type == typ {
				return b.Share
			}
		}
		return 0
	}

	// Confidence scales with how many samples backed the profile — 20 samples is
	// suggestive, 200 is solid.
	conf := math.Min(0.9, 0.4+float64(w.Samples)/400)

	// Per-query lock contention: a query spending most of its time waiting on
	// row/transaction locks. Names the query_id so it's actionable.
	for _, q := range w.ByQuery {
		if q.Count >= waitLockQueryMinSamples && q.LockShare > waitLockContentionShare {
			title := fmt.Sprintf("Query %s spends %.0f%% of its time waiting on locks", queryTag(q.QueryID), q.LockShare*100)
			ev := []string{fmt.Sprintf("query_id %d · %d samples · top wait %s", q.QueryID, q.Count, orNone(q.TopEvent))}
			if q.SampleText != "" {
				ev = append(ev, truncate(q.SampleText, 80))
			}
			add(model.Finding{
				ID: "wait_lock_contention", Severity: model.SeverityWarn,
				Title:       title,
				Detail:      "This query is mostly blocked on locks held by other sessions, not doing work. Look for a conflicting long transaction, hot-row updates, or a coarse lock.",
				Evidence:    ev,
				Remediation: "Reduce contention: shorten the holding transaction, avoid hot-row updates, or lower lock granularity.",
				Impact: impact(model.DimLatency, math.Min(95, q.LockShare*100*q.Share+40),
					fmt.Sprintf("%.0f%% of query %s on locks", q.LockShare*100, queryTag(q.QueryID)),
					fmt.Sprintf("ASH: %d/%d samples of this query on Lock:*", int(q.LockShare*float64(q.Count)+0.5), q.Count)),
				Confidence: conf,
			})
		}
	}

	// IO-bound: the whole window dominated by storage reads/writes.
	if io := share("IO"); io > waitIOBoundShare {
		add(model.Finding{
			ID: "wait_io_bound", Severity: model.SeverityWarn,
			Title:       fmt.Sprintf("%.0f%% of active time was spent waiting on IO", io*100),
			Detail:      "Most active samples were waiting on the storage layer, not on CPU or locks. The working set may not fit in cache, or a few queries are scanning far more than they return.",
			Evidence:    []string{ioEvidence(w)},
			Remediation: "Add RAM/shared_buffers or better indexes; check for large scans returning few rows.",
			Impact: impact(model.DimThroughput, math.Min(90, io*100),
				fmt.Sprintf("%.0f%% of active time on IO", io*100),
				"ASH: share of samples with wait_event_type = IO"),
			Confidence: conf,
		})
	}

	// LWLock pressure: a single lightweight-lock event concentrating the window
	// (e.g. BufferMapping, WALWrite) — an internal-contention smell.
	if typ, ev, sh := dominantLWLock(w); sh > waitLWLockShare {
		add(model.Finding{
			ID: "wait_lwlock_pressure", Severity: model.SeverityWarn,
			Title:       fmt.Sprintf("%.0f%% of active time on a single lightweight lock (%s)", sh*100, ev),
			Detail:      "Concentration on one LWLock points at internal contention (buffer mapping, WAL, lock manager) rather than user locks. Often a sign of an undersized buffer cache or very high write concurrency.",
			Evidence:    []string{fmt.Sprintf("%s:%s · %.0f%% of the window", typ, ev, sh*100)},
			Remediation: "The fix depends on the lock — buffer mapping points at cache size, WAL locks at write concurrency.",
			Impact: impact(model.DimThroughput, math.Min(88, sh*100),
				fmt.Sprintf("%.0f%% on %s", sh*100, ev),
				"ASH: share of samples on a single LWLock event"),
			Confidence: conf,
		})
	}
}

// connectionSaturation warns as open connections approach max_connections —
// past which new sessions are refused and the app locks out. A risk, not a
// gradual degradation.
func connectionSaturation(c *model.Context, add func(model.Finding)) {
	if c.Limits == nil || c.Limits.ConnectionsMax <= 0 {
		return
	}
	used, max := c.Limits.ConnectionsUsed, c.Limits.ConnectionsMax
	frac := float64(used) / float64(max)
	if frac < connSaturationWarn {
		return
	}
	sev := model.SeverityWarn
	if frac >= connSaturationCrit {
		sev = model.SeverityCritical
	}
	// Where the connections come from turns the warning into an action (A13).
	var ev []string
	if c.Activity != nil {
		for _, g := range c.Activity.Connections {
			ev = append(ev, fmt.Sprintf("%d × %s / %s (%s)", g.Count, g.AppName, g.User, g.State))
		}
	}
	add(model.Finding{
		ID: "connection_saturation", Severity: sev,
		Title:       fmt.Sprintf("Connection usage at %.0f%% (%d/%d)", frac*100, used, max),
		Detail:      "New connections are refused once max_connections is reached. A pool leak or a traffic burst can exhaust the slots and lock the application out.",
		Evidence:    cap10(ev),
		Remediation: "Put a pooler (PgBouncer) in front, lower per-service pool sizes, or raise max_connections. The breakdown above points at the biggest contributor.",
		Impact: impact(model.DimRisk, frac*100,
			fmt.Sprintf("%d of %d slots", used, max),
			"count(pg_stat_activity) / max_connections"),
		Confidence: 1.0,
	})
}

// txidWraparound flags the oldest transaction-id age climbing toward the ~2.1B
// wall past which Postgres stops accepting writes — i.e. autovacuum isn't
// freezing fast enough. One of the few genuine "database goes down" risks.
func txidWraparound(c *model.Context, add func(model.Finding)) {
	if c.Limits == nil || c.Limits.MaxXIDAge < xidWraparoundWarn {
		return
	}
	age := c.Limits.MaxXIDAge
	sev := model.SeverityWarn
	if age >= xidWraparoundCrit {
		sev = model.SeverityCritical
	}
	pct := float64(age) / float64(xidWraparoundWall) * 100
	add(model.Finding{
		ID: "txid_wraparound", Severity: sev,
		Title:       fmt.Sprintf("Transaction-ID age %s — %.0f%% toward wraparound", human(age), pct),
		Detail:      "The oldest unfrozen transaction id is approaching the 2.1-billion wraparound limit, past which Postgres refuses writes. It means vacuum isn't freezing fast enough — a long-running transaction, disabled autovacuum, or a stuck worker.",
		Remediation: "Clear what blocks vacuum (long transactions, replication slots, disabled autovacuum), then VACUUM (FREEZE) the oldest tables.",
		Impact: impact(model.DimRisk, pct,
			fmt.Sprintf("age %s / 2.1B", human(age)),
			"max(age(datfrozenxid)) across databases"),
		Confidence: 1.0,
		Safety: safety(prohibition("wraparound.no_vacuum_full", model.ActionVacuumFull,
			"VACUUM FULL / CLUSTER / pg_repack take ACCESS EXCLUSIVE and CONSUME transaction IDs — exactly what is running out. Use VACUUM (FREEZE) only.")),
	})
}

// sequenceExhaustion flags a sequence approaching its effective ceiling — the
// lesser of its max_value and the owning column's integer range. At the ceiling
// the next nextval() errors: a full write outage. An int4 identity/serial column
// wraps at 2.1B even if the sequence's own max reads 2^63. DimRisk (top of report).
func sequenceExhaustion(c *model.Context, add func(model.Finding)) {
	if c.Sequences == nil || len(c.Sequences.Items) == 0 {
		return
	}
	var ev, objs []string
	var worst float64
	for _, s := range c.Sequences.Items {
		if s.PctUsed < seqExhaustionWarn {
			continue
		}
		if s.PctUsed > worst {
			worst = s.PctUsed
		}
		owned := ""
		if s.OwnedBy != "" {
			owned = " (" + s.OwnedBy + ")"
		}
		ev = append(ev, fmt.Sprintf("%s.%s%s: %.0f%% used (%s / %s)", s.Schema, s.Name, owned, s.PctUsed*100, human(s.LastValue), human(s.Ceiling)))
		objs = append(objs, s.Schema+"."+s.Name)
	}
	if len(ev) == 0 {
		return
	}
	sev := model.SeverityWarn
	if worst >= seqExhaustionCrit {
		sev = model.SeverityCritical
	}
	add(model.Finding{
		ID: "sequence_exhaustion", Severity: sev,
		Title:       fmt.Sprintf("%d sequence(s) near exhaustion (worst %.0f%%)", len(ev), worst*100),
		Detail:      "A sequence at its ceiling raises an error on the next nextval() — a write outage for anything that inserts. An int4 identity/serial column wraps at 2.1 billion even when the sequence's own max_value is higher.",
		Evidence:    ev,
		Objects:     objs,
		Remediation: "Migrate the owning column to bigint (ALTER TABLE … ALTER COLUMN … TYPE bigint — plan for the table rewrite). If the column is already bigint, exhaustion is astronomically far off.",
		Impact:      impact(model.DimRisk, worst*100, fmt.Sprintf("%.0f%% of range used", worst*100), "last_value / min(max_value, column type max)"),
		Confidence:  0.9,
		Safety: safety(precondition("narrow_column.table_rewrite", model.ActionAlterColumnType,
			"ALTER ... TYPE bigint rewrites the whole table under an ACCESS EXCLUSIVE lock.",
			"you have planned for the lock/downtime (or use a phased add-column/backfill/swap), and widen any int4 foreign-key columns that reference it in the same change")),
	})
}

// int4IdentityColumn is the STRUCTURAL half of sequence exhaustion, split out as a
// schema-scoped finding (D3-0): a narrow (int2/int4) column backed by a sequence
// will wrap — int4 at 2.1B, int2 at 32767 — no matter its current value. It fires
// on a freshly-migrated, never-inserted database where sequence_exhaustion (which
// needs last_value) cannot, which is exactly when a migration PR can still fix it
// cheaply. int2 is almost always a mistake, so it escalates to critical.
func int4IdentityColumn(c *model.Context, add func(model.Finding)) {
	if c.Sequences == nil || len(c.Sequences.NarrowIdentity) == 0 {
		return
	}
	var ev, objs []string
	sawInt2 := false
	for _, n := range c.Sequences.NarrowIdentity {
		width := "2.1 billion"
		if n.Type == "int2" {
			width = "32767"
			sawInt2 = true
		}
		ev = append(ev, fmt.Sprintf("%s.%s.%s is %s — wraps at %s", n.Schema, n.Table, n.Column, n.Type, width))
		objs = append(objs, n.Schema+"."+n.Table)
	}
	sev := model.SeverityWarn
	if sawInt2 {
		sev = model.SeverityCritical
	}
	add(model.Finding{
		ID: "int4_identity_column", Severity: sev,
		Title:       fmt.Sprintf("%d sequence-backed column(s) too narrow for their lifetime", len(ev)),
		Detail:      "A sequence-backed int4 column wraps at 2.1 billion rows and an int2 at 32767, after which the next insert errors — a write outage. The current value doesn't matter; the type is the ceiling. This is a common migration mistake and invisible to review of the migration file.",
		Evidence:    ev,
		Objects:     objs,
		Remediation: "Define identity/serial columns as bigint. To widen an existing one: ALTER TABLE … ALTER COLUMN … TYPE bigint (a table rewrite — plan for it, and widen any int4 foreign-key columns that reference it in the same change).",
		Impact:      impact(model.DimRisk, 55, fmt.Sprintf("%d column(s)", len(ev)), "column type of sequence-backed columns (pg_attribute)"),
		Confidence:  0.95,
		Safety: safety(precondition("narrow_column.table_rewrite", model.ActionAlterColumnType,
			"ALTER ... TYPE bigint rewrites the whole table under an ACCESS EXCLUSIVE lock.",
			"you have planned for the lock/downtime (or use a phased add-column/backfill/swap), and widen any int4 foreign-key columns that reference it in the same change")),
	})
}

// mxidWraparound mirrors txid_wraparound for MULTIXACT ids. Multixacts are
// consumed by SELECT ... FOR SHARE/FOR UPDATE and foreign-key checks and exhaust
// toward the same ~2.1B wall; they wrap independently of regular transaction ids,
// so a workload heavy in shared row locks can hit this while datfrozenxid is fine.
func mxidWraparound(c *model.Context, add func(model.Finding)) {
	if c.Limits == nil || c.Limits.MaxMXIDAge < xidWraparoundWarn {
		return
	}
	age := c.Limits.MaxMXIDAge
	sev := model.SeverityWarn
	if age >= xidWraparoundCrit {
		sev = model.SeverityCritical
	}
	pct := float64(age) / float64(xidWraparoundWall) * 100
	add(model.Finding{
		ID: "mxid_wraparound", Severity: sev,
		Title:       fmt.Sprintf("Multixact-ID age %s — %.0f%% toward wraparound", human(age), pct),
		Detail:      "The oldest unfrozen multixact id is approaching the 2.1-billion wraparound limit, past which Postgres refuses writes. Multixacts come from shared row locks (SELECT … FOR SHARE/UPDATE) and FK checks; a workload heavy in those can wrap this while transaction-id age looks fine.",
		Remediation: "Clear what blocks vacuum (long transactions, replication slots, disabled autovacuum), then VACUUM (FREEZE) the oldest tables to advance datminmxid.",
		Impact: impact(model.DimRisk, pct,
			fmt.Sprintf("mxid age %s / 2.1B", human(age)),
			"max(mxid_age(datminmxid)) across databases"),
		Confidence: 1.0,
		Safety: safety(prohibition("mxid_wraparound.no_vacuum_full", model.ActionVacuumFull,
			"VACUUM FULL / CLUSTER / pg_repack take ACCESS EXCLUSIVE and make the wraparound race worse, not better. Use VACUUM (FREEZE) only to advance datminmxid.")),
	})
}

// vacuumHorizonBlocked names WHY dead tuples aren't being reclaimed: the oldest
// xmin still in use pins the horizon, and VACUUM frees nothing newer. It reports
// which of the four holders (open transaction, standby feedback, replication
// slot, prepared 2PC txn) is oldest and how far behind it is.
func vacuumHorizonBlocked(c *model.Context, add func(model.Finding)) {
	if c.Horizon == nil || len(c.Horizon.Holders) == 0 {
		return
	}
	top := c.Horizon.Holders[0] // collector orders oldest-xmin first
	if top.XminAge < vacuumHorizonWarnXIDs {
		return
	}
	sev := model.SeverityWarn
	dim := model.DimStorage
	score := 45.0
	if top.XminAge >= xidWraparoundWarn { // old enough to threaten wraparound
		sev = model.SeverityCritical
		dim = model.DimRisk
		score = float64(top.XminAge) / float64(xidWraparoundWall) * 100
	}
	ev := make([]string, 0, len(c.Horizon.Holders))
	for _, h := range c.Horizon.Holders {
		d := ""
		if h.Detail != "" {
			d = " — " + h.Detail
		}
		ev = append(ev, fmt.Sprintf("%s %s: xmin %s txns behind%s", h.Source, h.Holder, human(h.XminAge), d))
	}
	add(model.Finding{
		ID: "vacuum_horizon_blocked", Severity: sev,
		Title:       fmt.Sprintf("Vacuum horizon pinned by %s %s (%s transactions behind)", top.Source, top.Holder, human(top.XminAge)),
		Detail:      "Dead tuples can't be reclaimed past the oldest xmin still in use. Until this holder releases it, VACUUM frees nothing newer — bloat grows, and if it persists, transaction-id age climbs toward wraparound.",
		Evidence:    cap10(ev),
		Remediation: horizonRemediation(top.Source),
		Impact:      impact(dim, score, human(top.XminAge)+" transactions pinned", "oldest backend_xmin / slot xmin / prepared-xact age"),
		Confidence:  0.9,
	})
}

// preparedXactAbandoned flags a prepared (2PC) transaction left open. Unlike an
// idle backend it is invisible in pg_stat_activity and never times out, so it
// blocks vacuum and pins the xmin horizon indefinitely. Reads the prepared-xact
// holders the horizon collector already gathered.
func preparedXactAbandoned(c *model.Context, add func(model.Finding)) {
	if c.Horizon == nil {
		return
	}
	for _, h := range c.Horizon.Holders {
		if h.Source != "prepared_xact" || h.AgeSec < preparedXactWarnSec {
			continue
		}
		sev := model.SeverityWarn
		score := 55.0
		if h.AgeSec >= preparedXactCritSec {
			sev = model.SeverityCritical
			score = 78
		}
		add(model.Finding{
			ID: "prepared_xact_abandoned", Severity: sev,
			Title:       fmt.Sprintf("Prepared transaction %q open for %.0fs", h.Holder, h.AgeSec),
			Detail:      "An abandoned prepared (2PC) transaction is invisible in pg_stat_activity and never times out. It holds back the xmin horizon and vacuum indefinitely until it is committed or rolled back.",
			Evidence:    []string{fmt.Sprintf("gid %q, prepared %.0fs ago, xmin %s txns behind", h.Holder, h.AgeSec, human(h.XminAge))},
			Remediation: fmt.Sprintf("Confirm the transaction manager is finished, then COMMIT PREPARED '%s' or ROLLBACK PREPARED '%s'.", h.Holder, h.Holder),
			Impact:      impact(model.DimRisk, score, fmt.Sprintf("open %.0fs", h.AgeSec), "pg_prepared_xacts"),
			Confidence:  0.9,
		})
	}
}

func horizonRemediation(source string) string {
	switch source {
	case "backend":
		return "End or commit the long-open transaction (find the pid in pg_stat_activity); set idle_in_transaction_session_timeout so it can't recur."
	case "replication_slot":
		return "If the slot's consumer is gone for good, drop it: SELECT pg_drop_replication_slot('…'). Otherwise let the consumer catch up."
	case "standby_feedback":
		return "A replica's hot_standby_feedback is holding the horizon. Shorten long queries on the standby, or weigh turning hot_standby_feedback off (risks query cancellations there)."
	case "prepared_xact":
		return "An abandoned prepared (2PC) transaction blocks vacuum forever. COMMIT PREPARED / ROLLBACK PREPARED the gid once you confirm its transaction manager is done."
	default:
		return "Identify and release the holder of the oldest xmin."
	}
}

// failoverReadiness answers "if I promote right now, what do I lose, and will
// writes hang?" — sync-standby consistency, time-based replay lag, a vanished
// standby, and standby recovery conflicts.
func failoverReadiness(c *model.Context, add func(model.Finding), tun Tunables) {
	// sync_rep_degraded — the important one. Not visible in any latency graph.
	sc := settingParam(c, "synchronous_commit")
	required := requiredSyncStandbys(settingParam(c, "synchronous_standby_names"))
	if required > 0 && (sc == "on" || sc == "remote_write" || sc == "remote_apply") {
		got := 0
		if c.Replication != nil {
			for _, r := range c.Replication.Replicas {
				if r.SyncState == "sync" || r.SyncState == "quorum" {
					got++
				}
			}
		}
		if got < required {
			add(model.Finding{
				ID: "sync_rep_degraded", Severity: model.SeverityCritical,
				Title:       fmt.Sprintf("Synchronous replication degraded — %d of %d required sync standbys present", got, required),
				Detail:      "synchronous_standby_names requires sync standbys and synchronous_commit waits for them, but fewer are connected in sync/quorum state than required. Either writes are about to hang waiting for a standby that isn't there, or — if synchronous_commit was quietly relaxed — the RPO guarantee everyone believes in has silently stopped applying.",
				Remediation: "Reconnect or replace the missing sync standby, and confirm synchronous_standby_names matches the standbys actually running.",
				Impact:      impact(model.DimRisk, 88, fmt.Sprintf("%d/%d sync standbys", got, required), "sync_state count vs synchronous_standby_names"),
				Confidence:  0.9,
			})
		}
	}

	// replica_lag_time — gated on observed WAL generation (replay_lag is stale on
	// an idle primary, and a naive read there says a reassuring "zero").
	walFlowing := c.WAL != nil && c.WAL.BytesPerSec != nil && *c.WAL.BytesPerSec > 0
	if c.Replication != nil && walFlowing {
		var ev []string
		worst, crit := 0.0, false
		for _, r := range c.Replication.Replicas {
			if r.ReplayLagSec == nil || *r.ReplayLagSec < tun.ReplicaLagWarnSec {
				continue
			}
			if *r.ReplayLagSec > worst {
				worst = *r.ReplayLagSec
			}
			if *r.ReplayLagSec >= replicaLagCritSec {
				crit = true
			}
			ev = append(ev, fmt.Sprintf("%s: %.0fs replay lag", orText(r.AppName, r.ClientAddr), *r.ReplayLagSec))
		}
		if len(ev) > 0 {
			sev := model.SeverityWarn
			if crit {
				sev = model.SeverityCritical
			}
			add(model.Finding{
				ID: "replica_lag_time", Severity: sev,
				Title:       fmt.Sprintf("Replica replay lag %.0fs — the writes you'd lose on failover", worst),
				Detail:      "A standby's replay is behind by this much wall-clock time — that IS the RPO: promote now and you lose that much committed work. Byte lag can't tell you this; it needs the write rate, which pgbot has from WAL sampling.",
				Evidence:    cap10(ev),
				Remediation: "Check the standby's apply rate (IO, CPU, recovery conflicts) and the network between primary and standby.",
				Impact:      impact(model.DimRisk, math.Min(90, 40+worst/10), fmt.Sprintf("%.0fs behind", worst), "pg_stat_replication.replay_lag, gated on WAL generation"),
				Confidence:  0.85,
			})
		}
	}

	// recovery_conflicts — standby side.
	if c.Standby != nil && c.Standby.Total() > 0 {
		add(model.Finding{
			ID: "recovery_conflicts", Severity: model.SeverityWarn,
			Title:       fmt.Sprintf("%d recovery conflict(s) on this standby", c.Standby.Total()),
			Detail:      "Queries on this standby have been cancelled because recovery had to apply a change that conflicted with them. Frequent conflicts make read queries here unreliable.",
			Evidence:    []string{fmt.Sprintf("snapshot=%d lock=%d bufferpin=%d deadlock=%d tablespace=%d (cumulative)", c.Standby.ConflSnapshot, c.Standby.ConflLock, c.Standby.ConflBufferpin, c.Standby.ConflDeadlock, c.Standby.ConflTablespace)},
			Remediation: "Raise max_standby_streaming_delay for long read queries, or enable hot_standby_feedback (at the cost of bloat on the primary — see vacuum_horizon_blocked).",
			Impact:      impact(model.DimRisk, 40, fmt.Sprintf("%d conflicts", c.Standby.Total()), "pg_stat_database_conflicts"),
			Confidence:  0.7,
		})
	}

	// replica_disconnected — a standby that was streaming last run is gone now.
	if gone := replicaDisconnected(c); gone != "" {
		add(model.Finding{
			ID: "replica_disconnected", Severity: model.SeverityWarn,
			Title:       fmt.Sprintf("A standby stopped connecting: %s", gone),
			Detail:      "A standby that was streaming at the last run is absent from pg_stat_replication now. A silently disconnected standby is invisible to any point-in-time tool — only run-over-run history sees it.",
			Remediation: "Check whether the standby is down, was reconfigured, or had its replication slot dropped.",
			Impact:      impact(model.DimRisk, 55, "standby gone", "present in baseline, absent now"),
			Confidence:  0.8,
		})
	}
}

// requiredSyncStandbys parses synchronous_standby_names for the count of sync
// standbys it requires: "ANY N (...)"/"FIRST N (...)" → N, a leading "N (...)"
// → N, a bare list → 1, empty → 0 (not using sync rep).
func requiredSyncStandbys(names string) int {
	fields := strings.Fields(strings.TrimSpace(names))
	if len(fields) == 0 {
		return 0
	}
	switch strings.ToUpper(fields[0]) {
	case "ANY", "FIRST":
		if len(fields) >= 2 {
			if n := leadingInt(fields[1]); n > 0 {
				return n
			}
		}
		return 1
	}
	if n := leadingInt(fields[0]); n > 0 {
		return n
	}
	return 1
}

// leadingInt parses the leading run of digits ("3(a,b)" → 3, "s1" → 0).
func leadingInt(s string) int {
	d := ""
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			break
		}
		d += string(ch)
	}
	if n, err := strconv.Atoi(d); err == nil {
		return n
	}
	return 0
}

// replicaDisconnected returns the name of a standby that was in the baseline but
// is gone now (from the diff's replication.standby_gone delta), or "".
func replicaDisconnected(c *model.Context) string {
	if c.Deltas == nil {
		return ""
	}
	for _, ch := range c.Deltas.Changes {
		if ch.ID == "replication.standby_gone" {
			return ch.Subject
		}
	}
	return ""
}

// checksumFindings covers data-integrity: actual checksum failures (corruption),
// corruption detection being silenced, and checksums being off entirely.
func checksumFindings(c *model.Context, add func(model.Finding)) {
	if c.Checksums != nil && len(c.Checksums.Failures) > 0 {
		var ev, objs []string
		var total int64
		for _, f := range c.Checksums.Failures {
			total += f.Count
			ev = append(ev, fmt.Sprintf("%s: %d failure(s), last %s", f.Database, f.Count, tsOr(f.LastFailure)))
			objs = append(objs, "db:"+f.Database)
		}
		add(model.Finding{
			ID: "checksum_failures", Severity: model.SeverityCritical, Objects: objs,
			Title:       fmt.Sprintf("%d data-checksum failure(s) — likely corruption", total),
			Detail:      "Postgres read one or more pages whose checksum did not match what was written: storage corruption, bad memory, or a filesystem that acknowledged a write it didn't persist. It does not heal itself, and reads of the affected pages return wrong data.",
			Evidence:    ev,
			Remediation: "Identify the affected relation and take it out of write service; investigate storage and hardware; restore the affected data from a known-good backup.",
			// Load-bearing: the instinct is to VACUUM FULL / REINDEX, which is the wrong move.
			Caveats:    []string{"Do NOT VACUUM FULL or REINDEX the affected relation — rewriting the pages destroys the evidence and can propagate the damage. Preserve state, then restore from backup."},
			Impact:     impact(model.DimRisk, 96, fmt.Sprintf("%d checksum failures", total), "pg_stat_database.checksum_failures > 0"),
			Confidence: 1.0,
			Safety: safety(
				prohibition("checksum.no_vacuum_full", model.ActionVacuumFull,
					"Rewriting the pages (VACUUM FULL / CLUSTER / pg_repack) destroys the evidence and can turn a recoverable incident into an unrecoverable one. Preserve state, then restore from a known-good backup."),
				prohibition("checksum.no_reindex", model.ActionReindex,
					"REINDEX rewrites index pages over corrupt data — it destroys evidence and can propagate the damage. Preserve state, then restore from a known-good backup."),
			),
		})
	}
	if settingParam(c, "ignore_checksum_failure") == "on" {
		add(model.Finding{
			ID: "ignore_checksum_failure_on", Object: "setting:ignore_checksum_failure", Severity: model.SeverityCritical,
			Title:       "ignore_checksum_failure is ON — corruption is being silenced",
			Detail:      "With ignore_checksum_failure on, Postgres reads past a failed page checksum instead of erroring, so corruption is treated as valid data and can spread into query results and backups.",
			Remediation: "Set ignore_checksum_failure = off (a per-session GUC — check for a global default) unless you are mid-recovery deliberately extracting data from a damaged page.",
			Impact:      impact(model.DimRisk, 90, "corruption detection disabled", "ignore_checksum_failure=on"),
			Confidence:  1.0,
		})
	}
	if settingParam(c, "data_checksums") == "off" {
		add(model.Finding{
			ID: "checksums_disabled", Object: "setting:data_checksums", Severity: model.SeverityInfo,
			Title:       "data checksums are off",
			Detail:      "Without data checksums, silent page corruption is undetectable — a bad read returns wrong data with no error. Most managed providers enable checksums by default.",
			Remediation: "Enabling checksums needs pg_checksums offline or a re-initdb, so it can't be flipped on a running system — note it for the next maintenance window or rebuild.",
			Impact:      impact(model.DimRisk, 15, "no checksum protection", "data_checksums=off"),
			Confidence:  1.0,
		})
	}
}

// walArchiving flags the WAL-archiving / PITR failure modes. On a detected
// managed provider the backup mechanism is usually outside archive_command, so
// every archiving finding is downgraded to info with wording that says pgbot
// cannot see the provider's backups (A15-0 rule 2). The archiver collector only
// runs on a primary/unknown node, so this is inherently primary-scoped.
func walArchiving(c *model.Context, add func(model.Finding)) {
	if c.Archiver == nil {
		return
	}
	a := c.Archiver
	managed := cloudProvider(c)
	managedNote := "pgbot can't see this managed provider's backup mechanism — snapshots/WAL are handled outside archive_command; verify backups in the provider console"
	sev := func(base string) string {
		if managed {
			return model.SeverityInfo
		}
		return base
	}
	archiveMode := settingParam(c, "archive_mode")
	walLevel := settingParam(c, "wal_level")

	// archiving_failing: the most recent archive attempt was a failure and no
	// success has followed. Timestamp comparison, so an old transient failure two
	// months ago (last_failed < last_archived since) never fires — no baseline
	// needed and no stale-counter false positive.
	// Two triggers: the point-in-time comparison (baseline-free, catches a
	// persistent failure and ignores an old transient one), and the run-over-run
	// failed_count delta (catches intermittent failure the timestamp misses).
	failing := a.LastFailedTime != nil && (a.LastArchivedTime == nil || a.LastFailedTime.After(*a.LastArchivedTime))
	failedDelta := archiverFailedDelta(c)
	if failing || failedDelta > 0 {
		ev := []string{}
		basis := "pg_stat_archiver: last_failed_time > last_archived_time"
		if failing {
			ev = append(ev, fmt.Sprintf("last failure %s is newer than last success %s", tsOr(a.LastFailedTime), tsOr(a.LastArchivedTime)))
		}
		if failedDelta > 0 {
			ev = append(ev, fmt.Sprintf("%d new archiving failures since the last run", failedDelta))
			basis = "archiving failures increased since the baseline"
		} else {
			// Lifetime count is corroborating only — never the trigger (spec discipline:
			// don't fire critical on a stale cumulative counter without a baseline).
			ev = append(ev, fmt.Sprintf("%d failures since %s (lifetime, corroborating)", a.FailedCount, tsOr(a.StatsReset)))
		}
		f := model.Finding{
			ID: "archiving_failing", Severity: sev(model.SeverityCritical),
			Title:       "WAL archiving is failing — the PITR window is broken",
			Detail:      "An archive_command attempt failed and was not (or not consistently) followed by success. Unarchived WAL means point-in-time recovery is broken from that point, silently — no client error, no symptom until a restore is attempted.",
			Evidence:    ev,
			Remediation: "Check the archive_command target (permissions, disk, network) in the server log; failed WAL is retried, so fixing the cause resumes archiving.",
			Impact:      impact(model.DimRisk, 95, "archiving broken", basis),
			Confidence:  0.95,
		}
		if managed {
			f.Caveats = append(f.Caveats, managedNote)
		}
		crossLinkWAL(c, &f)
		add(f)
	}

	// archiving_stalled: mode on, WAL flowing, nothing archived recently.
	if (archiveMode == "on" || archiveMode == "always") && !failing {
		walFlowing := c.WAL != nil && c.WAL.BytesPerSec != nil && *c.WAL.BytesPerSec > 0
		if walFlowing && a.LastArchivedTime != nil && time.Since(*a.LastArchivedTime) > archiveStallThreshold(c) {
			age := int64(time.Since(*a.LastArchivedTime).Seconds())
			f := model.Finding{
				ID: "archiving_stalled", Severity: sev(model.SeverityCritical),
				Title:       fmt.Sprintf("WAL archiving stalled — nothing archived in %s while WAL is being written", shortDur(age)),
				Detail:      "archive_mode is on and WAL is being generated, but no segment has been archived recently. WAL can't be recycled until it's archived, so this both breaks PITR and fills the disk.",
				Evidence:    []string{fmt.Sprintf("last archived %s ago; archive_timeout=%s", shortDur(age), orUnknownSetting(settingParam(c, "archive_timeout")))},
				Remediation: "Check that the archiver process is running and the archive_command target is reachable.",
				Impact:      impact(model.DimRisk, 90, "archiving stalled", "last_archived_time age vs max(archive_timeout×3, 1h)"),
				Confidence:  0.85,
			}
			if managed {
				f.Caveats = append(f.Caveats, managedNote)
			}
			crossLinkWAL(c, &f)
			add(f)
		}
	}

	// archiving_disabled: no PITR by this mechanism.
	if archiveMode == "off" && walLevel != "minimal" && !replicationInUse(c) {
		s := model.SeverityWarn
		if managed {
			s = model.SeverityInfo
		}
		f := model.Finding{
			ID: "archiving_disabled", Severity: s,
			Title:       "WAL archiving is off — no point-in-time recovery",
			Detail:      "archive_mode is off, so no WAL is archived. Without it (and with no replication streaming elsewhere) there is no PITR: recovery is limited to your last base backup.",
			Remediation: "If you rely on PITR, set archive_mode=on with an archive_command (or use pgBackRest / a streaming replica). If backups are handled elsewhere, this is expected.",
			Impact:      impact(model.DimRisk, 50, "no PITR", "archive_mode=off, wal_level≠minimal, no slots"),
			Confidence:  0.8,
		}
		if managed {
			f.Caveats = append(f.Caveats, managedNote)
		}
		add(f)
	}
}

// crossLinkWAL enriches an archiving finding when pg_wal (A14) is large: broken
// archiving PLUS a filling pg_wal is a compound emergency — unarchived WAL can't
// be recycled, so the disk fills on top of the broken recovery window.
func crossLinkWAL(c *model.Context, f *model.Finding) {
	if c.WAL != nil && c.WAL.DirBytes != nil && *c.WAL.DirBytes >= walDirCompoundBytes {
		f.Evidence = append(f.Evidence, fmt.Sprintf("pg_wal is now %s and cannot be recycled while archiving is broken — compound disk-fill risk", humanBytes(*c.WAL.DirBytes)))
	}
}

func tsOr(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return t.UTC().Format("2006-01-02 15:04Z")
}

// archiverFailedDelta returns the number of NEW archiving failures since the
// baseline (0 if none / no baseline), from the diff's archiver.failed_count delta.
func archiverFailedDelta(c *model.Context) int64 {
	if c.Deltas == nil {
		return 0
	}
	for _, ch := range c.Deltas.Changes {
		if ch.ID == "archiver.failed_count" {
			return int64(ch.After - ch.Before)
		}
	}
	return 0
}

// archiveStallThreshold is max(archive_timeout × 3, 1h).
func archiveStallThreshold(c *model.Context) time.Duration {
	base := time.Duration(archiveStallFloorS) * time.Second
	// current_setting('archive_timeout') returns a unit-suffixed string ("5min",
	// "1h", "0"), not a bare integer — strconv.Atoi always failed on it, so this
	// branch was dead and the threshold silently stayed at the 1h floor. Parse
	// the real duration so a long archive_timeout actually widens the window.
	if t, ok := parsePGDuration(settingParam(c, "archive_timeout"), time.Second); ok && t > 0 {
		if scaled := 3 * t; scaled > base {
			base = scaled
		}
	}
	return base
}

// parsePGDuration parses a PostgreSQL time-unit GUC value: an optional sign,
// then one or more "<number><unit>" tokens ("30s", "5min", "1h 30min", "0").
// Units follow the server's own table (us/ms/s/min/h/d). A bare number scales
// by base — PostgreSQL's base unit is per-GUC (ms for statement_timeout, s for
// archive_timeout, min for autovacuum_naptime), and pg_settings.setting holds
// exactly such raw base-unit integers, so every caller must say which GUC
// family it is reading. It returns ok=false for empty or unparseable input,
// which callers treat as "use the default" rather than an error.
func parsePGDuration(s string, base time.Duration) (time.Duration, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	neg := false
	switch s[0] {
	case '-':
		neg, s = true, s[1:]
	case '+':
		s = s[1:]
	}
	unit := map[string]time.Duration{
		"us": time.Microsecond, "ms": time.Millisecond, "s": time.Second,
		"min": time.Minute, "h": time.Hour, "d": 24 * time.Hour,
	}
	var total time.Duration
	matched := false
	for len(s) > 0 {
		// Consume the leading digits.
		i := 0
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == 0 {
			return 0, false // no digits where a number was expected
		}
		n, err := strconv.ParseInt(s[:i], 10, 64)
		if err != nil {
			return 0, false
		}
		s = s[i:]
		// Consume the unit letters that follow. A bare number with no unit (the
		// server prints "0" for a disabled time GUC; pg_settings.setting holds
		// raw integers) scales by the caller's base unit.
		j := 0
		for j < len(s) && (s[j] < '0' || s[j] > '9') && s[j] != ' ' && s[j] != '\t' {
			j++
		}
		u := base
		if unitStr := s[:j]; unitStr != "" {
			var ok bool
			u, ok = unit[unitStr]
			if !ok {
				return 0, false
			}
		}
		total += time.Duration(n) * u
		matched = true
		s = strings.TrimLeft(s[j:], " \t")
	}
	if !matched {
		return 0, false
	}
	if neg {
		total = -total
	}
	return total, true
}

// replicationSlotRisk flags an inactive replication slot that is holding back WAL
// removal — the retained log grows until the slot's consumer reconnects or the
// slot is dropped, and unbounded growth fills the data disk and stops the primary.
// wal_status='lost' means required WAL is already gone: the slot is broken.
func replicationSlotRisk(c *model.Context, add func(model.Finding)) {
	if c.Replication == nil {
		return
	}
	for _, s := range c.Replication.Slots {
		lost := s.WALStatus == "lost"
		// An active, healthy slot is fine. Only inactive slots (or an already-lost
		// one) are a risk, and an inactive slot below the floor is just a brief gap.
		if s.Active && !lost {
			continue
		}
		if !lost && s.RetainedBytes < slotRetainWarnBytes {
			continue
		}
		sev := model.SeverityWarn
		score := 55.0
		note := "inactive — no consumer connected"
		switch {
		case lost:
			sev = model.SeverityCritical
			score = 92
			note = "wal_status=lost — required WAL has already been removed"
		case s.RetainedBytes >= slotRetainCritBytes:
			sev = model.SeverityCritical
			score = 85
		}
		ev := []string{
			fmt.Sprintf("slot %q (%s) on %s", s.Name, orText(s.Type, "unknown"), orText(s.Database, "cluster")),
			note,
			fmt.Sprintf("WAL retained: %s", humanBytes(s.RetainedBytes)),
		}
		if s.WALStatus != "" && !lost {
			ev = append(ev, "wal_status="+s.WALStatus)
		}
		// Pair with the pg_wal directory size (A14): retention plus the actual
		// on-disk total is a far stronger disk-fill signal than either alone.
		if c.WAL != nil && c.WAL.DirBytes != nil {
			ev = append(ev, "pg_wal directory now "+humanBytes(*c.WAL.DirBytes))
		}
		add(model.Finding{
			ID: "replication_slot_inactive", Object: "slot:" + s.Name, Severity: sev,
			Title:       fmt.Sprintf("Inactive replication slot %q pinning %s of WAL", s.Name, humanBytes(s.RetainedBytes)),
			Detail:      "An inactive replication slot holds back WAL removal from its restart point. The retained WAL grows until the slot's consumer reconnects or the slot is dropped — a classic way to fill the data disk and take the primary down.",
			Evidence:    ev,
			Remediation: fmt.Sprintf("First find the slot's consumer: if it should be connected, restart it, and set max_slot_wal_keep_size to cap retention. Only once you've confirmed no standby or subscriber still depends on it, drop it: SELECT pg_drop_replication_slot('%s').", s.Name),
			Impact:      impact(model.DimRisk, score, humanBytes(s.RetainedBytes)+" WAL retained", "pg_replication_slots"),
			Confidence:  0.9,
			Safety: safety(prohibition("replication_slot.live_consumer", model.ActionDropReplicationSlot,
				"Dropping a slot a live standby or logical subscriber still depends on permanently breaks its replication and forces a full resync. Confirm the consumer is truly gone before pg_drop_replication_slot.")),
		})
	}
}

// subscriptionDown flags a logical subscription whose apply worker isn't running:
// changes from the publisher aren't being applied, so this subscriber is drifting.
func subscriptionDown(c *model.Context, add func(model.Finding)) {
	if c.Replication == nil {
		return
	}
	for _, s := range c.Replication.Subscriptions {
		if s.WorkerRunning {
			continue
		}
		add(model.Finding{
			ID: "subscription_worker_down", Object: "sub:" + s.Name, Severity: model.SeverityWarn,
			Title:       fmt.Sprintf("Logical subscription %q has no running apply worker", s.Name),
			Detail:      "The subscription exists but its apply worker isn't running, so changes from the publisher aren't being replicated. Data on this subscriber is drifting from the source.",
			Evidence:    []string{fmt.Sprintf("subscription %q: apply worker not running", s.Name)},
			Remediation: "Check the subscriber logs for apply errors, confirm the subscription is enabled (ALTER SUBSCRIPTION … ENABLE), and that the publisher's slot still exists.",
			Impact:      impact(model.DimRisk, 60, "apply worker not running", "pg_stat_subscription"),
			Confidence:  0.75,
		})
	}
}

// orText returns s, or fallback when s is empty.
func orText(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// querySlowdown surfaces the flagship "what changed": a query whose mean time
// regressed sharply versus the baseline. Temporal — needs a prior run, which is
// exactly what a stats reader can't do.
func querySlowdown(c *model.Context, add func(model.Finding)) {
	if c.Deltas == nil {
		return
	}
	var worst *model.Delta
	var worstFactor float64
	for i := range c.Deltas.Changes {
		d := &c.Deltas.Changes[i]
		if d.ID != "query.mean_ms" || d.Before <= 0 || d.After < querySlowdownMinMs {
			continue
		}
		if factor := d.After / d.Before; factor >= querySlowdownFactor && factor > worstFactor {
			worst, worstFactor = d, factor
		}
	}
	if worst == nil {
		return
	}
	// If pg_stat_statements is evicting entries, this query may have been evicted
	// and re-entered (stats reset to zero) — the "regression" could be an artifact.
	// Carry that as a load-bearing caveat and drop confidence below the assertion line.
	conf := 0.8
	var caveats []string
	basis := "query mean time vs the baseline"
	if c.Server.Engine != "cockroachdb" && pgssEvicting(c) {
		caveats = append(caveats, "pg_stat_statements is evicting entries — this query may have been evicted and re-entered (stats reset), so the regression could be an artifact (see pgss_entries_evicted)")
		conf = 0.4
		basis = "pg_stat_statements mean time vs the baseline"
	}
	add(model.Finding{
		ID: "query_slowdown", Severity: model.SeverityWarn,
		Title:       fmt.Sprintf("Query %s is %.1f× slower (%.0f → %.0f ms mean)", queryTagStr(worst.Subject), worstFactor, worst.Before, worst.After),
		Detail:      "A query's mean execution time regressed sharply versus pgbot's baseline. Often a plan flip after the table grew, a dropped or invalidated index, or stale statistics.",
		Remediation: "Check for a missing/invalid index and run ANALYZE on the table; compare the current plan against before.",
		Impact: impact(model.DimLatency, math.Min(90, 30+worstFactor*10),
			fmt.Sprintf("%.1f× slower", worstFactor),
			basis),
		Confidence: conf,
		Caveats:    caveats,
	})
}

// pgssEvicting reports whether pg_stat_statements is discarding entries — which
// biases the top list and can reset a query's stats between runs.
func pgssEvicting(c *model.Context) bool {
	if c.Queries == nil {
		return false
	}
	q := c.Queries
	return q.PgssDealloc > 0 || (q.PgssMax > 0 && q.PgssCount >= q.PgssMax)
}

// pgssEntriesEvicted is a trust finding: when pg_stat_statements fills, the
// top-queries list is a biased sample and query_slowdown deltas can be fiction.
// It also stamps Queries.Section.Reason so any consumer sees the caveat.
func pgssEntriesEvicted(c *model.Context, add func(model.Finding)) {
	if c.Queries == nil || !c.Queries.Enabled || !pgssEvicting(c) {
		return
	}
	q := c.Queries
	c.Queries.Reason = fmt.Sprintf("pg_stat_statements at capacity (%d/%d entries, %s evictions) — the top-queries list is a biased sample and cross-run query deltas may compare against an evicted-and-re-entered query", q.PgssCount, q.PgssMax, human(q.PgssDealloc))
	add(model.Finding{
		ID: "pgss_entries_evicted", Severity: model.SeverityWarn,
		Title:       fmt.Sprintf("pg_stat_statements is evicting entries (%d/%d, %s discarded)", q.PgssCount, q.PgssMax, human(q.PgssDealloc)),
		Detail:      "When pg_stat_statements fills, it discards least-used entries. The top-queries view becomes a biased sample, and a query that was evicted and re-entered resets to zero — so query_slowdown deltas can be fiction.",
		Remediation: "Raise pg_stat_statements.max (needs a restart) so the workload fits, or accept that low-frequency queries won't appear.",
		Impact:      impact(model.DimThroughput, 25, fmt.Sprintf("%d/%d entries", q.PgssCount, q.PgssMax), "pg_stat_statements_info.dealloc + count vs max"),
		Confidence:  0.85,
	})
}

// queryTagStr renders a query_id string as the short low-4-hex handle, or a
// truncated fallback when it isn't numeric.
func queryTagStr(s string) string {
	if id, err := strconv.ParseInt(s, 10, 64); err == nil {
		return queryTag(id)
	}
	return truncate(s, 12)
}

// workMemLow — sorts/hashes are spilling to disk because they don't fit in
// work_mem. A config-tuning recommendation, not a defect.
func workMemLow(c *model.Context, add func(model.Finding)) {
	if c.Health == nil || c.Health.TempBytesPerSec == nil || *c.Health.TempBytesPerSec < tuneTempSpillBytesPerSec {
		return
	}
	rate := int64(*c.Health.TempBytesPerSec)
	cur := settingParam(c, "work_mem")
	add(model.Finding{
		ID: "work_mem_low", Object: "setting:work_mem", Severity: model.SeverityWarn,
		Title:       fmt.Sprintf("Queries spilling to disk — work_mem is %s", orUnknownSetting(cur)),
		Detail:      fmt.Sprintf("Sorts and hashes are writing %s/s of temporary files because they don't fit in work_mem, forcing slower on-disk operations.", humanBytes(rate)),
		Remediation: "Raise work_mem — but it's per-operation, so budget total ≈ work_mem × active connections; or add an index so the sort/hash is avoided.",
		Impact: impact(model.DimThroughput, math.Min(80, math.Log10(float64(rate))*10),
			humanBytes(rate)+"/s temp files", "pg_stat_database.temp_bytes rate"),
		Confidence: 0.7,
	})
}

// checkpointsForced — WAL is filling max_wal_size before the timed checkpoint
// interval, so checkpoints fire on volume (IO spikes, more full-page writes).
func checkpointsForced(c *model.Context, add func(model.Finding)) {
	if c.IO == nil {
		return
	}
	total := c.IO.CheckpointsReq + c.IO.CheckpointsTimed
	if total < tuneForcedCheckpointMin {
		return
	}
	frac := float64(c.IO.CheckpointsReq) / float64(total)
	if frac < tuneForcedCheckpointFrac {
		return
	}
	cur := settingParam(c, "max_wal_size")
	add(model.Finding{
		ID: "checkpoints_forced", Object: "setting:max_wal_size", Severity: model.SeverityWarn,
		Title:       fmt.Sprintf("%.0f%% of checkpoints forced by WAL pressure (%d of %d)", frac*100, c.IO.CheckpointsReq, total),
		Detail:      "Checkpoints are triggered by max_wal_size filling rather than the timed interval — WAL is written faster than the checkpoint spacing expects, causing IO spikes and extra full-page writes.",
		Remediation: fmt.Sprintf("Raise max_wal_size (currently %s) so checkpoints are paced by time, not WAL volume.", orUnknownSetting(cur)),
		Impact: impact(model.DimThroughput, math.Min(75, frac*100),
			fmt.Sprintf("%d of %d forced", c.IO.CheckpointsReq, total), "pg_stat_checkpointer forced vs timed"),
		Confidence: 0.7,
	})
}

// connectionsOverprovisioned — max_connections far above real usage. Each slot
// reserves backend memory whether used or not; better handled by a pooler.
func connectionsOverprovisioned(c *model.Context, add func(model.Finding)) {
	if c.Limits == nil || c.Limits.ConnectionsMax < tuneConnOverprovMax {
		return
	}
	if float64(c.Limits.ConnectionsUsed) >= float64(c.Limits.ConnectionsMax)*tuneConnOverprovUseFrac {
		return
	}
	add(model.Finding{
		ID: "connections_overprovisioned", Severity: model.SeverityInfo,
		Title:       fmt.Sprintf("max_connections is %d but only %d in use", c.Limits.ConnectionsMax, c.Limits.ConnectionsUsed),
		Detail:      "Each connection slot reserves backend memory (roughly work_mem plus overhead) whether used or not. A high max_connections with low real usage wastes RAM and invites connection storms.",
		Remediation: "Put a pooler (PgBouncer) in front and lower max_connections to match real concurrency.",
		Impact: impact(model.DimCost, 20,
			fmt.Sprintf("%d/%d slots used", c.Limits.ConnectionsUsed, c.Limits.ConnectionsMax), "max_connections vs observed usage"),
		Confidence: 0.6,
	})
}

// configSanity emits the config-value red flags: the two that risk data loss
// (fsync/full_page_writes off), autovacuum off, a rotational random_page_cost on
// an SSD-backed managed provider, an implausible work_mem × max_connections vs
// effective_cache_size, and a cluster with no statement_timeout.
func configSanity(c *model.Context, add func(model.Finding)) {
	if c.Settings == nil {
		return
	}
	if settingParam(c, "fsync") == "off" {
		add(model.Finding{
			ID: "fsync_off", Object: "setting:fsync", Severity: model.SeverityCritical,
			Title:       "fsync is OFF — a crash can corrupt the database",
			Detail:      "With fsync off, Postgres doesn't flush writes to durable storage. An OS crash or power loss can leave the database irrecoverably corrupt — not just losing recent transactions.",
			Remediation: "Set fsync = on unless this is a throwaway instance you can rebuild from scratch.",
			Impact:      impact(model.DimRisk, 95, "durability disabled", "fsync setting"),
			Confidence:  1.0,
		})
	}
	if settingParam(c, "full_page_writes") == "off" {
		add(model.Finding{
			ID: "full_page_writes_off", Object: "setting:full_page_writes", Severity: model.SeverityCritical,
			Title:       "full_page_writes is OFF — torn pages can corrupt on crash",
			Detail:      "With full_page_writes off, a crash during a page write can leave a torn (half-written) page that recovery can't repair. Safe only on storage that guarantees atomic page writes.",
			Remediation: "Set full_page_writes = on unless your storage guarantees atomic 8 KB writes.",
			Impact:      impact(model.DimRisk, 90, "torn-page protection disabled", "full_page_writes setting"),
			Confidence:  1.0,
		})
	}
	if settingParam(c, "autovacuum") == "off" {
		add(model.Finding{
			ID: "autovacuum_off", Object: "setting:autovacuum", Severity: model.SeverityCritical,
			Title:       "autovacuum is OFF — bloat and wraparound will follow",
			Detail:      "With autovacuum off, dead tuples are never reclaimed and transaction-id/multixact age climbs unchecked toward wraparound, which eventually forces the database read-only.",
			Remediation: "Set autovacuum = on. If it was disabled to control load, tune the cost limits instead of turning it off.",
			Impact:      impact(model.DimRisk, 88, "no automatic vacuuming", "autovacuum setting"),
			Confidence:  1.0,
		})
	}
	// random_page_cost=4 is the rotational-disk default; on SSD-backed managed
	// providers the planner then systematically under-uses index scans.
	if rpc, err := strconv.ParseFloat(settingParam(c, "random_page_cost"), 64); err == nil && rpc >= 4 && cloudProvider(c) {
		add(model.Finding{
			ID: "random_page_cost_high", Object: "setting:random_page_cost", Severity: model.SeverityWarn,
			Title:       fmt.Sprintf("random_page_cost=%g on %s (SSD) — planner under-uses indexes", rpc, orUnknownSetting(c.Server.Provider)),
			Detail:      "random_page_cost=4 assumes a rotational disk. On SSD-backed storage (all managed cloud providers), random reads are far cheaper, so the planner over-weights sequential scans and skips index scans it should use.",
			Remediation: "Lower random_page_cost toward 1.1 (a reload). Re-check plans afterward.",
			Impact:      impact(model.DimLatency, 40, fmt.Sprintf("rpc=%g on SSD", rpc), "random_page_cost vs detected provider"),
			Confidence:  0.7,
		})
	}
	// Implausible worst-case sort memory: work_mem is per operation, so a busy
	// cluster can allocate up to work_mem × max_connections; above effective_cache_size
	// that's a mis-set knob inviting OOM.
	if wm, ok1 := parseMemBytes(settingParam(c, "work_mem")); ok1 {
		if ecs, ok2 := parseMemBytes(settingParam(c, "effective_cache_size")); ok2 && ecs > 0 {
			if mc, err := strconv.Atoi(settingParam(c, "max_connections")); err == nil && mc > 0 {
				worst := wm * int64(mc)
				if worst > ecs {
					add(model.Finding{
						ID: "work_mem_overcommit", Object: "setting:work_mem", Severity: model.SeverityWarn,
						Title:       fmt.Sprintf("work_mem × max_connections (%s) exceeds effective_cache_size (%s)", humanBytes(worst), humanBytes(ecs)),
						Detail:      "work_mem is allocated per sort/hash operation, so worst-case memory under load is roughly work_mem × max_connections. When that exceeds the memory you've told the planner exists, a burst of concurrent sorts can push the host into OOM.",
						Remediation: "Lower work_mem, cap concurrency with a pooler, or raise host memory. work_mem can also be raised per-session for the few queries that need it.",
						Impact:      impact(model.DimRisk, 45, humanBytes(worst)+" worst-case", "work_mem × max_connections vs effective_cache_size"),
						Confidence:  0.55,
					})
				}
			}
		}
	}
	if settingParam(c, "statement_timeout") == "0" {
		add(model.Finding{
			ID: "statement_timeout_unset", Object: "setting:statement_timeout", Severity: model.SeverityInfo,
			Title:       "statement_timeout is unset cluster-wide",
			Detail:      "With no statement_timeout, a runaway query can run indefinitely, holding locks and the xmin horizon. A cluster-wide cap is a cheap safety net (set a generous one and override per-session where needed).",
			Remediation: "Consider a generous cluster default, e.g. statement_timeout = '60s', with longer per-session values for known long jobs.",
			Impact:      impact(model.DimRisk, 20, "no statement timeout", "statement_timeout setting"),
			Confidence:  0.8,
		})
	}
}

// cloudProvider reports whether pgbot detected a managed (SSD-backed) platform.
func cloudProvider(c *model.Context) bool {
	switch c.Server.Provider {
	case "rds", "aurora", "cloudsql", "azure", "supabase", "neon":
		return true
	}
	return false
}

// parseMemBytes parses a Postgres memory setting ("4MB", "8192kB", "1GB") to bytes.
func parseMemBytes(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	i := len(s)
	for i > 0 && (s[i-1] < '0' || s[i-1] > '9') {
		i--
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s[:i]), 10, 64)
	if err != nil {
		return 0, false
	}
	switch strings.ToLower(strings.TrimSpace(s[i:])) {
	case "", "b":
		return n, true
	case "kb":
		return n * 1024, true
	case "mb":
		return n * 1024 * 1024, true
	case "gb":
		return n * 1024 * 1024 * 1024, true
	case "tb":
		return n * 1024 * 1024 * 1024 * 1024, true
	}
	return 0, false
}

// ioTimingOff flags track_io_timing=off. With it off, blk_read_time/blk_write_time
// are zero, so pgbot can't tell an IO-bound query from a CPU-bound one — which
// degrades query analysis and the wait profile. Measured overhead is negligible
// on modern hardware. Info severity.
func ioTimingOff(c *model.Context, add func(model.Finding)) {
	if settingParam(c, "track_io_timing") != "off" {
		return
	}
	add(model.Finding{
		ID: "io_timing_off", Object: "setting:track_io_timing", Severity: model.SeverityInfo,
		Title:       "track_io_timing is off — no per-query IO time",
		Detail:      "With track_io_timing off, block read/write times are zero, so pgbot (and pg_stat_statements) can't separate IO-bound queries from CPU-bound ones. That weakens query analysis and the wait profile.",
		Remediation: "Set track_io_timing = on (a reload, no restart). Overhead is negligible on modern hardware — verify with pg_test_timing if unsure.",
		Impact:      impact(model.DimThroughput, 15, "no IO timing", "track_io_timing setting"),
		Confidence:  1.0,
	})
}

func settingParam(c *model.Context, name string) string {
	if c.Settings == nil || c.Settings.Params == nil {
		return ""
	}
	return c.Settings.Params[name]
}

func orUnknownSetting(s string) string {
	if s == "" {
		return "(unknown)"
	}
	return s
}

func highRollbacks(c *model.Context, add func(model.Finding)) {
	if c.Health == nil || c.Health.RollbackRatio == nil || *c.Health.RollbackRatio < rollbackRatioWarn {
		return
	}
	// A ratio computed over a handful of transactions is noise (2 rollbacks out of
	// 4 reads as 50%). Require enough volume in the window to trust it — TPS × the
	// sample seconds ≈ the transactions actually observed.
	if c.Health.TPS == nil || *c.Health.TPS*c.Window.SampleSeconds < rollbackMinTxns {
		return
	}
	add(model.Finding{
		ID: "high_rollback_ratio", Severity: model.SeverityWarn,
		Title:  fmt.Sprintf("Rollback ratio %.1f%% over the sample window", *c.Health.RollbackRatio*100),
		Detail: "A high share of transactions are rolling back. Often application error handling or failed constraint checks; worth confirming it's intended.",
		Impact: impact(model.DimThroughput, math.Min(35, *c.Health.RollbackRatio*100),
			fmt.Sprintf("%.1f%% rolling back", *c.Health.RollbackRatio*100),
			"xact_rollback/(commit+rollback) over the window"),
		Confidence: 0.5,
	})
}

func missingPgss(c *model.Context, add func(model.Finding)) {
	if c.Server.Engine == "cockroachdb" {
		return
	}
	if c.Queries != nil && c.Queries.Enabled {
		return
	}
	add(model.Finding{
		ID: "pg_stat_statements_missing", Severity: model.SeverityInfo,
		Title:       "pg_stat_statements not enabled",
		Detail:      "Without it, per-query performance analysis is unavailable. It's the single most useful Postgres monitoring extension.",
		Remediation: "Enable with: CREATE EXTENSION pg_stat_statements; (and add it to shared_preload_libraries if not already).",
		Impact:      impact(model.DimCost, 15, "no per-query stats", "pg_stat_statements not in the catalog"),
		Confidence:  1.0,
	})
}

func staleStatsWindow(c *model.Context, add func(model.Finding)) {
	if c.Window.StatsWindowDays == nil || *c.Window.StatsWindowDays < staleStatsWarnDays {
		return
	}
	add(model.Finding{
		ID: "stale_stats_window", Severity: model.SeverityInfo,
		Title:       fmt.Sprintf("Cumulative stats span %.0f days", *c.Window.StatsWindowDays),
		Detail:      "Ratios like cache hit and cumulative query totals average over the whole window since the last stats reset. Over a very long window they hide recent regressions.",
		Remediation: "Consider pg_stat_reset() to get a fresh baseline, or rely on pgbot's own deltas for recent change.",
		Impact:      impact(model.DimCost, 12, fmt.Sprintf("%.0f-day window", *c.Window.StatsWindowDays), "stats_reset age"),
		Confidence:  1.0,
	})
}

// windowAgeSeconds is how long the cumulative stats have been accumulating
// (since the last reset / restart), or 0 when unknown.
func windowAgeSeconds(c *model.Context) int64 {
	if c.Window.WindowAgeSeconds == nil {
		return 0
	}
	return *c.Window.WindowAgeSeconds
}

// replicationInUse reports whether this cluster is part of a replication setup —
// either a primary with connected standbys or a replica itself. When true,
// per-node scan counts cannot be trusted to prove an index is globally unused.
func replicationInUse(c *model.Context) bool {
	return c.Replication != nil && (len(c.Replication.Replicas) > 0 || c.Replication.IsReplica)
}

// shortDur renders a seconds count as a coarse human duration for caveats.
func shortDur(sec int64) string {
	switch {
	case sec >= 86400:
		return fmt.Sprintf("%.0fd", float64(sec)/86400)
	case sec >= 3600:
		return fmt.Sprintf("%.0fh", float64(sec)/3600)
	case sec >= 60:
		return fmt.Sprintf("%.0fm", float64(sec)/60)
	default:
		return fmt.Sprintf("%ds", sec)
	}
}

// writeHeavyTables returns the set of tables with meaningful write activity,
// used to flag unused indexes that also tax writes.
func writeHeavyTables(c *model.Context) map[string]bool {
	out := map[string]bool{}
	if c.Tables == nil {
		return out
	}
	for _, t := range c.Tables.Top {
		if t.ModsSinceAnalyze > 1000 {
			out[t.Schema+"."+t.Name] = true
		}
	}
	return out
}

func cap10(s []string) []string {
	if len(s) > 10 {
		return append(s[:10:10], "…")
	}
	return s
}

func human(v int64) string {
	switch {
	case v >= 1e9:
		return fmt.Sprintf("%.1fG", float64(v)/1e9)
	case v >= 1e6:
		return fmt.Sprintf("%.1fM", float64(v)/1e6)
	case v >= 1e3:
		return fmt.Sprintf("%.1fk", float64(v)/1e3)
	default:
		return fmt.Sprintf("%d", v)
	}
}

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

// queryTag is a short stable handle for a query_id (pg_stat_statements prints
// the full 64-bit id, which is unreadable). We show the low 4 hex digits.
func queryTag(id int64) string { return fmt.Sprintf("%04x", uint64(id)&0xffff) }

func orNone(s string) string {
	if s == "" {
		return "(unknown)"
	}
	return s
}

// truncate shortens s to n runes (not bytes) so a multibyte character is never
// split into invalid UTF-8 — a byte slice here would corrupt the JSON output.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n || n < 1 {
		return s
	}
	return string(r[:n-1]) + "…"
}

// ioEvidence names the top specific IO wait event for the finding's evidence.
func ioEvidence(w *model.WaitProfile) string {
	for _, b := range w.Buckets {
		if b.Type == "IO" && len(b.Events) > 0 {
			return fmt.Sprintf("top IO wait: %s (%.0f%% of the window)", b.Events[0].Event, b.Events[0].Share*100)
		}
	}
	return "IO waits dominate the window"
}

// dominantLWLock returns the single largest LWLock:event and its share of the
// whole window, or ("","",0) if there are no LWLock samples.
func dominantLWLock(w *model.WaitProfile) (typ, event string, share float64) {
	for _, b := range w.Buckets {
		if b.Type != "LWLock" {
			continue
		}
		if len(b.Events) > 0 {
			return "LWLock", b.Events[0].Event, b.Events[0].Share
		}
		// No specific event names (rare) — fall back to the bucket share.
		return "LWLock", "LWLock", b.Share
	}
	return "", "", 0
}
