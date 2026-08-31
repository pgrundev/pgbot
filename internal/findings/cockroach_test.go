package findings

import (
	"strings"
	"testing"
	"time"

	"github.com/pgrundev/pgbot/internal/model"
)

func cockroachContext() *model.Context {
	return &model.Context{
		Server: model.ServerInfo{Engine: "cockroachdb"},
		Queries: &model.Queries{Enabled: true, Top: []model.QueryStat{{
			QueryID: 7, Fingerprint: "abc", AppName: "checkout", Query: "UPDATE accounts SET balance = $1",
			Calls: 20, MeanMS: 240, ContentionMS: 900, MaxRetries: 5,
		}}},
		Cockroach: &model.CockroachDB{
			LiveQueries: model.CockroachLiveQueries{Items: []model.CockroachLiveQuery{{
				Query: "SELECT * FROM orders", AppName: "reporting", AgeSec: 75, FullScan: true,
			}}},
			ExecutionInsights: model.CockroachExecutionInsights{Items: []model.CockroachInsight{{
				Kind: "statement", Fingerprint: "abc", Problem: "SlowExecution", Causes: []string{"HighContention"},
				Query: "UPDATE accounts SET balance = $1", AppName: "checkout", ServiceLatencyMS: 1400,
				Retries: 5, IndexRecommendations: []string{"CREATE INDEX ON accounts (customer_id)"},
			}}},
		},
	}
}

func TestCockroachFindings(t *testing.T) {
	fs := Compute(cockroachContext())
	for _, id := range []string{"crdb_long_running_query", "crdb_retry_hotspot", "crdb_execution_insights", "crdb_index_recommendations"} {
		if has(fs, id) == nil {
			t.Errorf("expected %s, got %+v", id, fs)
		}
	}
}

func TestCockroachFindingsDoNotFireOnPostgres(t *testing.T) {
	c := cockroachContext()
	c.Server.Engine = "postgresql"
	for _, f := range Compute(c) {
		if len(f.ID) >= 5 && f.ID[:5] == "crdb_" {
			t.Fatalf("CockroachDB finding fired on PostgreSQL: %+v", f)
		}
	}
}

func TestCockroachLiveRetriesFireWithoutPersistedStats(t *testing.T) {
	c := &model.Context{
		Server: model.ServerInfo{Engine: "cockroachdb"},
		Cockroach: &model.CockroachDB{
			LiveQueries: model.CockroachLiveQueries{Items: []model.CockroachLiveQuery{{
				QueryID: "live-1", AppName: "gitload", Query: "INSERT INTO file_latest VALUES ($1)", Retries: 8,
			}}},
		},
	}
	f := has(Compute(c), "crdb_retry_hotspot")
	if f == nil {
		t.Fatal("live retries should fire even when persisted query statistics are unavailable")
	}
	if len(f.Evidence) != 1 || f.Impact.Estimate != "up to 8 retries" {
		t.Fatalf("unexpected live-retry finding: %+v", f)
	}
}

func TestCockroachClusterHealthFindings(t *testing.T) {
	c := cockroachContext()
	c.Health = &model.Health{Cockroach: &model.CockroachHealth{
		NodesTotal: 3, NodesLive: 2, NodesSuspect: 1, StoresTotal: 3,
		UnavailableRanges: 2, UnderreplicatedRanges: 4, MaxStoreUsedRatio: .93,
		MaxCPUPercent: 94, AdmissionWaitP99MS: 150,
		Jobs:     model.Section{Exactness: model.ExactnessScraped},
		Nodes:    []model.CockroachNodeHealth{{NodeID: 1, Status: "live", Version: "v25.2.1"}, {NodeID: 2, Status: "dead", Version: "v25.2.0"}},
		Stores:   []model.CockroachStoreHealth{{NodeID: 1, StoreID: 1, UsedRatio: .93, AvailableBytes: 7 << 30}},
		JobItems: []model.CockroachJobHealth{{JobID: "42", Type: "BACKUP", State: "failed"}},
	}}
	fs := Compute(c)
	for _, id := range []string{"crdb_node_unavailable", "crdb_ranges_unavailable", "crdb_ranges_underreplicated", "crdb_store_capacity", "crdb_resource_pressure", "crdb_job_failed", "crdb_version_skew"} {
		if has(fs, id) == nil {
			t.Errorf("expected %s", id)
		}
	}
	if f := has(fs, "crdb_store_capacity"); f == nil || f.Severity != model.SeverityCritical {
		t.Fatalf("93%% store should be critical: %+v", f)
	}
}

func TestCockroachOperationalJobFindings(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-time.Minute)
	stale := now.Add(-31 * time.Minute)
	c := &model.Context{
		CollectedAt: now,
		Server:      model.ServerInfo{Engine: "cockroachdb"},
		Health: &model.Health{Cockroach: &model.CockroachHealth{
			Jobs: model.Section{Exactness: model.ExactnessScraped},
			JobItems: []model.CockroachJobHealth{
				{JobID: "1", Type: "BACKUP", State: "revert-failed", CreatedAt: now.Add(-time.Hour), Error: "storage unavailable"},
				{JobID: "2", Type: "SCHEMA CHANGE", State: "running", CreatedAt: now.Add(-2 * time.Hour), LastUpdatedAt: &stale, Operation: "CREATE INDEX idx ON app.public.orders (id)"},
				{JobID: "3", Type: "SCHEMA CHANGE", State: "reverting", CreatedAt: now.Add(-time.Hour), LastUpdatedAt: &recent},
				{JobID: "4", Type: "CHANGEFEED", State: "paused", CreatedAt: now.Add(-24 * time.Hour)},
			},
		}},
	}
	fs := Compute(c)
	for _, id := range []string{"crdb_job_failed", "crdb_job_stalled", "crdb_job_reverting", "crdb_job_paused"} {
		if has(fs, id) == nil {
			t.Errorf("expected %s: %+v", id, fs)
		}
	}
	if f := has(fs, "crdb_job_failed"); f.Severity != model.SeverityCritical {
		t.Fatalf("revert-failed job must be critical: %+v", f)
	}
	if f := has(fs, "crdb_job_stalled"); len(f.Evidence) != 1 || !strings.Contains(f.Evidence[0], "app.public.orders") || len(f.Objects) != 1 || f.Objects[0] != "job:2" {
		t.Fatalf("stalled job attribution=%+v", f)
	}
}

func TestCockroachLongRunningJobWithRecentProgressIsNotStalled(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-time.Minute)
	c := &model.Context{CollectedAt: now, Server: model.ServerInfo{Engine: "cockroachdb"}, Health: &model.Health{Cockroach: &model.CockroachHealth{
		Jobs:     model.Section{Exactness: model.ExactnessScraped},
		JobItems: []model.CockroachJobHealth{{JobID: "9", Type: "CHANGEFEED", State: "running", CreatedAt: now.Add(-30 * 24 * time.Hour), LastUpdatedAt: &recent}},
	}}}
	if f := has(Compute(c), "crdb_job_stalled"); f != nil {
		t.Fatalf("duration alone must not classify a job as stalled: %+v", f)
	}
}

func TestCockroachExpectedSilentSystemAndGCJobsAreNotStalled(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-24 * time.Hour)
	for _, typ := range []string{"SCHEMA CHANGE GC", "KEY VISUALIZER", "UPDATE TABLE METADATA CACHE", "AUTO SPAN CONFIG RECONCILIATION"} {
		j := model.CockroachJobHealth{JobID: "100", Type: typ, State: "running", CreatedAt: stale, LastUpdatedAt: &stale}
		if cockroachJobStalled(j, now) {
			t.Errorf("expected-silent job %q must not use generic stalled heuristic", typ)
		}
	}
}

func TestCockroachDistributionFindings(t *testing.T) {
	c := &model.Context{Server: model.ServerInfo{Engine: "cockroachdb"}, Health: &model.Health{Cockroach: &model.CockroachHealth{
		Distribution: model.CockroachDistribution{
			Section: model.Section{Exactness: model.ExactnessScraped}, LiveStores: 3, ComparableStores: 3, MultipleLocalities: true,
			ReplicaMean: 166.67, ReplicaMin: 100, ReplicaMax: 300, ReplicaMinToMean: .6, ReplicaMaxToMean: 1.8, FewestReplicasStoreID: 2, MostReplicasStoreID: 1,
			LeaseMean: 100, LeaseMin: 40, LeaseMax: 220, LeaseMinToMean: .4, LeaseMaxToMean: 2.2, FewestLeasesStoreID: 2, MostLeasesStoreID: 1,
			CapacityUsedMinRatio: .3, CapacityUsedMaxRatio: .8, CapacityUsedSpread: .5, LeastUsedStoreID: 3, MostUsedStoreID: 1,
			HotRangeSampleCount: 5, HotRangeLeaseholderSamples: 5, HotRangeCPUCores: .9, HottestLeaseholderNodeID: 1, HottestLeaseholderRanges: 4, HottestLeaseholderCPUCores: .8, HottestLeaseholderCPUShare: .8889,
			Stores: []model.CockroachStoreBalance{
				{NodeID: 1, StoreID: 1, Comparable: true, Locality: "region=east", UsedRatio: .8, RangeReplicas: 300, Leaseholders: 220, NodeCPUPercent: 82},
				{NodeID: 2, StoreID: 2, Comparable: true, Locality: "region=east", UsedRatio: .4, RangeReplicas: 100, Leaseholders: 40, NodeCPUPercent: 35},
				{NodeID: 3, StoreID: 3, Comparable: true, Locality: "region=west", UsedRatio: .3, RangeReplicas: 100, Leaseholders: 40, NodeCPUPercent: 28},
			},
		},
		Hot: []model.CockroachHotRange{{RangeID: 7, LeaseholderNodeID: 1, CPUCores: .4, QPS: 900, Tables: []string{"orders"}, Indexes: []string{"orders_pkey"}}},
	}}}
	fs := Compute(c)
	for _, id := range []string{"crdb_replica_imbalance", "crdb_leaseholder_imbalance", "crdb_capacity_imbalance", "crdb_hot_range_concentration"} {
		if has(fs, id) == nil {
			t.Errorf("expected %s: %+v", id, fs)
		}
	}
	if f := has(fs, "crdb_leaseholder_imbalance"); f.Severity != model.SeverityInfo || len(f.Caveats) < 2 {
		t.Fatalf("lease skew must be informational and topology-qualified: %+v", f)
	}
	if f := has(fs, "crdb_hot_range_concentration"); len(f.Evidence) < 2 || !strings.Contains(f.Evidence[1], "orders/orders_pkey") {
		t.Fatalf("hot-range attribution=%+v", f)
	}
}

func TestCockroachDistributionIgnoresThinOrMildSkew(t *testing.T) {
	c := &model.Context{Server: model.ServerInfo{Engine: "cockroachdb"}, Health: &model.Health{Cockroach: &model.CockroachHealth{
		Distribution: model.CockroachDistribution{
			Section: model.Section{Exactness: model.ExactnessScraped}, LiveStores: 3, ComparableStores: 3,
			ReplicaMean: 105, ReplicaMin: 100, ReplicaMax: 110, ReplicaMinToMean: .95, ReplicaMaxToMean: 1.05,
			LeaseMean: 30, LeaseMin: 20, LeaseMax: 40, LeaseMinToMean: .67, LeaseMaxToMean: 1.33,
			CapacityUsedMinRatio: .3, CapacityUsedMaxRatio: .45, CapacityUsedSpread: .15,
			HotRangeLeaseholderSamples: 3, HotRangeCPUCores: .2, HottestLeaseholderRanges: 3, HottestLeaseholderCPUShare: 1,
		},
	}}}
	for _, f := range Compute(c) {
		if strings.Contains(f.ID, "imbalance") || f.ID == "crdb_hot_range_concentration" {
			t.Fatalf("thin/mild distribution signal fired: %+v", f)
		}
	}
}

func TestCockroachStorageFindings(t *testing.T) {
	const mib = float64(1 << 20)
	c := &model.Context{Server: model.ServerInfo{Engine: "cockroachdb"}, Health: &model.Health{Cockroach: &model.CockroachHealth{
		Storage: model.CockroachStorage{
			Section: model.Section{Exactness: model.ExactnessSampled}, LiveStores: 3,
			MVCCMetricsAvailable: true, ReplicationMetricsAvailable: true, CounterSampledStores: 3, SampleSeconds: 2,
			OtherUsedBytes: 400 << 30, RangeReplicas: 300, UninitializedReplicas: 30, ReservedReplicas: 3,
			ReplicateQueuePurgatory: 10, RaftSnapshotQueuePending: 10,
			RaftCommandsPending: 170, MaxRaftCommandsPending: 150, MaxRaftPendingStoreID: 1,
			RaftProbeFlows: 5, RaftSnapshotFlows: 5, RaftDroppedMessages: 250,
			DiskStalledEvents:  1,
			BytesPerReplicaMin: 50 * mib, BytesPerReplicaMean: 100 * mib, BytesPerReplicaMax: 200 * mib,
			SmallestReplicaBytesStoreID: 2, LargestReplicaBytesStoreID: 1,
			Stores: []model.CockroachStoreStorage{
				{NodeID: 1, StoreID: 1, Status: "live", Locality: "region=east", CapacityBytes: 1000 << 30, FilesystemUsedBytes: 900 << 30, CockroachUsedBytes: 500 << 30, OtherUsedBytes: 400 << 30, OtherUsedRatio: .4, MVCCTotalBytes: 200 << 20, BytesPerReplica: 200 * mib, RangeReplicas: 100, UninitializedReplicas: 20, ReplicateQueuePurgatory: 10, RaftSnapshotQueuePending: 10, RaftCommandsPending: 150, DiskStalledEvents: 1},
				{NodeID: 2, StoreID: 2, Status: "live", Locality: "region=west", CapacityBytes: 1000 << 30, FilesystemUsedBytes: 500 << 30, CockroachUsedBytes: 450 << 30, OtherUsedBytes: 50 << 30, OtherUsedRatio: .05, MVCCTotalBytes: 50 << 20, BytesPerReplica: 50 * mib, RangeReplicas: 100, UninitializedReplicas: 10, RaftCommandsPending: 10},
				{NodeID: 3, StoreID: 3, Status: "live", Locality: "region=west", CapacityBytes: 1000 << 30, FilesystemUsedBytes: 500 << 30, CockroachUsedBytes: 450 << 30, OtherUsedBytes: 50 << 30, OtherUsedRatio: .05, MVCCTotalBytes: 50 << 20, BytesPerReplica: 50 * mib, RangeReplicas: 100, RaftCommandsPending: 10},
			},
		},
	}}}
	fs := Compute(c)
	for _, id := range []string{"crdb_external_disk_usage", "crdb_storage_stall", "crdb_replication_recovery", "crdb_raft_backlog", "crdb_replica_size_skew"} {
		if has(fs, id) == nil {
			t.Errorf("expected %s: %+v", id, fs)
		}
	}
	if f := has(fs, "crdb_storage_stall"); f.Severity != model.SeverityCritical {
		t.Fatalf("disk-stalled event must be critical: %+v", f)
	}
	if f := has(fs, "crdb_external_disk_usage"); len(f.Evidence) != 1 || !strings.Contains(f.Evidence[0], "other use/overhead") {
		t.Fatalf("external usage evidence=%+v", f)
	}
}

func TestCockroachStorageIgnoresHealthyTransientState(t *testing.T) {
	c := &model.Context{Server: model.ServerInfo{Engine: "cockroachdb"}, Health: &model.Health{Cockroach: &model.CockroachHealth{
		Storage: model.CockroachStorage{
			Section: model.Section{Exactness: model.ExactnessSampled}, LiveStores: 3,
			MVCCMetricsAvailable: true, ReplicationMetricsAvailable: true, CounterSampledStores: 3, SampleSeconds: 2,
			RangeReplicas: 3000, UninitializedReplicas: 3, ReplicateQueuePending: 100,
			RaftCommandsPending: 25, MaxRaftCommandsPending: 10, RaftSnapshotFlows: 5,
			BytesPerReplicaMin: 90 << 20, BytesPerReplicaMean: 100 << 20, BytesPerReplicaMax: 110 << 20,
			Stores: []model.CockroachStoreStorage{{NodeID: 1, StoreID: 1, Status: "live", CapacityBytes: 1000, FilesystemUsedBytes: 600, CockroachUsedBytes: 550, OtherUsedBytes: 50, OtherUsedRatio: .05}},
		},
	}}}
	for _, f := range Compute(c) {
		if strings.HasPrefix(f.ID, "crdb_storage") || strings.HasPrefix(f.ID, "crdb_replication") || f.ID == "crdb_external_disk_usage" || f.ID == "crdb_raft_backlog" || f.ID == "crdb_replica_size_skew" {
			t.Fatalf("healthy/transient storage signal fired: %+v", f)
		}
	}
}

func TestCockroachContentionFindings(t *testing.T) {
	c := &model.Context{Server: model.ServerInfo{Engine: "cockroachdb"}, Cockroach: &model.CockroachDB{
		Contention: model.CockroachContention{
			Section: model.Section{Exactness: model.ExactnessScraped}, WindowMinutes: 60,
			TotalEvents: 80, TotalWaitMS: 72_000, MaxWaitMS: 12_000, SerializationConflicts: 7,
			Hotspots: []model.CockroachContentionHotspot{
				{Database: "app", Schema: "public", Table: "accounts", Index: "accounts_pkey", Type: "LOCK_WAIT", WaitingStatementFingerprint: "abc", WaitingQuery: "UPDATE accounts SET balance = _", BlockingTxnFingerprint: "def", BlockingQueries: []string{"SELECT balance FROM accounts WHERE id = _"}, Events: 73, TotalWaitMS: 70_000, MaxWaitMS: 12_000},
				{Database: "app", Schema: "public", Table: "accounts", Type: "SERIALIZATION_CONFLICT", WaitingStatementFingerprint: "abc", Events: 7},
			},
		},
	}}
	fs := Compute(c)
	for _, id := range []string{"crdb_contention_hotspot", "crdb_serialization_conflicts"} {
		f := has(fs, id)
		if f == nil {
			t.Fatalf("expected %s: %+v", id, fs)
		}
		if !f.ClusterScoped {
			t.Errorf("%s should be cluster-scoped", id)
		}
	}
	if f := has(fs, "crdb_contention_hotspot"); len(f.Evidence) == 0 || !strings.Contains(f.Evidence[0], "app.public.accounts/accounts_pkey") {
		t.Fatalf("contention evidence=%+v", f)
	} else if !strings.Contains(f.Evidence[0], "waiting query:") || !strings.Contains(f.Evidence[0], "blocking query:") {
		t.Fatalf("contention attribution missing from evidence=%+v", f)
	}
}

func TestCockroachContentionIgnoresThinNoise(t *testing.T) {
	c := &model.Context{Server: model.ServerInfo{Engine: "cockroachdb"}, Cockroach: &model.CockroachDB{
		Contention: model.CockroachContention{Section: model.Section{Exactness: model.ExactnessScraped}, TotalEvents: 1, TotalWaitMS: 50, MaxWaitMS: 50},
	}}
	if f := has(Compute(c), "crdb_contention_hotspot"); f != nil {
		t.Fatalf("thin contention should not fire: %+v", f)
	}
}

func TestCockroachUnusedIndexFindingIsRestartAwareAndGuarded(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	started := now.Add(-30 * 24 * time.Hour)
	c := &model.Context{
		CollectedAt: now,
		Server:      model.ServerInfo{Engine: "cockroachdb"},
		Health: &model.Health{Cockroach: &model.CockroachHealth{Nodes: []model.CockroachNodeHealth{
			{NodeID: 1, Status: "live", StartedAt: started},
		}}},
		Indexes: &model.Indexes{
			Section: model.Section{Exactness: model.ExactnessScraped}, UnusedThresholdHours: 168,
			WriteCountersAvailable: true,
			Unused:                 []model.IndexStat{{Database: "app", Schema: "public", Table: "orders", Name: "orders_old_idx", IndexType: "secondary", Writes: 1200, UnusedForSeconds: 14 * 24 * 3600}},
		},
	}
	f := has(Compute(c), "crdb_unused_indexes")
	if f == nil {
		t.Fatal("expected CockroachDB unused-index finding")
	}
	if has(Compute(c), "unused_indexes") != nil {
		t.Fatal("PostgreSQL per-node unused-index finding must not fire on CockroachDB")
	}
	if f.Confidence != 0.7 || len(f.Evidence) != 1 || !strings.Contains(f.Evidence[0], "1.2k writes") {
		t.Fatalf("unexpected evidence/confidence: %+v", f)
	}
	if f.Safety == nil || len(f.Safety.BlockingCaveats) != 1 || f.Safety.BlockingCaveats[0].ID != "crdb_unused_index.observation_window" {
		t.Fatalf("DROP INDEX safety precondition missing: %+v", f.Safety)
	}

	c.Health.Cockroach.Nodes[0].StartedAt = now.Add(-2 * 24 * time.Hour)
	if recent := has(Compute(c), "crdb_unused_indexes"); recent == nil || recent.Confidence != 0.35 {
		t.Fatalf("recent restart should lower confidence: %+v", recent)
	}
}

func TestCockroachNativeTableFindings(t *testing.T) {
	statsAt := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	c := &model.Context{
		Server: model.ServerInfo{Engine: "cockroachdb"},
		Tables: &model.Tables{Section: model.Section{Exactness: model.ExactnessScraped}, Top: []model.TableStat{
			{Database: "app", Schema: "public", Name: "metadata_bad", DataBytes: 128 << 20, AutoStatsEnabled: true, StatsLastUpdated: &statsAt, MetadataError: "node 2 unavailable"},
			{Database: "app", Schema: "public", Name: "stats_missing", DataBytes: 128 << 20, AutoStatsEnabled: false},
			{Database: "app", Schema: "public", Name: "history_heavy", DataBytes: 6 << 30, LiveDataBytes: 1 << 30, LiveDataRatio: 1.0 / 6.0, AutoStatsEnabled: true, StatsLastUpdated: &statsAt},
		}},
	}
	fs := Compute(c)
	for _, id := range []string{"crdb_table_metadata_error", "crdb_table_stats_missing", "crdb_auto_stats_disabled", "crdb_mvcc_garbage_pressure"} {
		if has(fs, id) == nil {
			t.Errorf("expected %s: %+v", id, fs)
		}
	}
	for _, id := range []string{"table_bloat", "never_analyzed", "stale_statistics", "autovacuum_starved", "seq_scan_heavy", "low_hot_update_ratio"} {
		if has(fs, id) != nil {
			t.Errorf("PostgreSQL table finding %s fired on CockroachDB", id)
		}
	}
	if f := has(fs, "crdb_mvcc_garbage_pressure"); f == nil || !strings.Contains(f.Remediation, "do not run PostgreSQL VACUUM") {
		t.Fatalf("MVCC finding must reject PostgreSQL vacuum semantics: %+v", f)
	}
}
