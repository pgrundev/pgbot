package render

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/pgrundev/pgbot/internal/model"
)

func sampleContext() *model.Context {
	tps := 1200.0
	hit := 0.994
	blocks := int64(50_000)
	return &model.Context{
		SchemaVersion: model.SchemaVersion,
		CollectedAt:   time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC),
		Server:        model.ServerInfo{VersionNum: 170010, Database: "app", HasPgMonitor: true},
		Window:        model.Window{SampleSeconds: 1.0},
		Health:        &model.Health{Section: model.Section{Exactness: model.ExactnessSampled}, Connections: 24, TPS: &tps, CacheHitRatio: &hit, CacheBlocks: &blocks},
		Activity:      &model.Activity{Section: model.Section{Exactness: model.ExactnessScraped}, Total: 24, Active: 6},
		Findings: []model.Finding{
			{ID: "unused_indexes", Severity: model.SeverityWarn, Title: "1 unused index",
				Impact: model.Impact{Score: 60, Dimension: model.DimStorage, Estimate: "≈4.2 GiB reclaimable"}},
		},
	}
}

func TestTerminal_groupedIsDefault(t *testing.T) {
	var buf bytes.Buffer
	if err := Terminal(&buf, sampleContext(), Options{Color: false}); err != nil {
		t.Fatal(err)
	}
	if regexp.MustCompile("\x1b\\[").MatchString(buf.String()) {
		t.Error("no-color output must contain no ANSI escapes")
	}
	out := buf.String()
	// Grouped view: header, a health score, the warning group with the finding
	// title bulleted, a GOOD list, and the --full pointer. No section tables.
	for _, want := range []string{"connected", "postgres 17", "Database health:", "/100", "WARNING", "● 1 unused index", "GOOD", "--full"} {
		if !strings.Contains(out, want) {
			t.Errorf("grouped output missing %q", want)
		}
	}
	if strings.Contains(out, "HEALTH  ") || strings.Contains(out, "ACTIVITY  ") {
		t.Error("default grouped view must not print section tables")
	}
}

func TestTerminal_fullShowsSectionsAndCaveats(t *testing.T) {
	c := sampleContext()
	c.Findings = []model.Finding{{
		ID: "unused_indexes", Severity: model.SeverityWarn, Title: "3 unused indexes",
		Confidence: 0.4, Caveats: []string{"replication is active — per-node counts only"},
		Impact: model.Impact{Score: 50, Dimension: model.DimStorage, Estimate: "≈43 GB"},
	}}
	var buf bytes.Buffer
	if err := Terminal(&buf, c, Options{Color: false, Full: true}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	// --full carries the detail the dashboard omits: sections, caveats inline,
	// and the low-confidence "(possible)" marker.
	for _, want := range []string{"HEALTH", "cache hit", "3 unused indexes", "replication is active", "(possible)"} {
		if !strings.Contains(out, want) {
			t.Errorf("--full output missing %q", want)
		}
	}
}

func TestFull_leadsWithStatusBoard(t *testing.T) {
	c := sampleContext()
	c.Locks = &model.Locks{BlockedCount: 2, Chains: []model.BlockingRow{{BlockedPID: 91}}}
	c.Findings = append(c.Findings, model.Finding{
		ID: "blocking_chains", Severity: model.SeverityCritical, Title: "2 blocked",
		Impact: model.Impact{Dimension: model.DimRisk, Score: 90},
	})
	var buf bytes.Buffer
	if err := Terminal(&buf, c, Options{Color: false, Full: true}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	// Box-drawn header + subsystem rows; locks reads "fail" (its finding is critical).
	for _, want := range []string{"┌", "┼", "subsystem", "status", "connections", "cache", "locks", "2 blocked", "fail"} {
		if !strings.Contains(out, want) {
			t.Errorf("status board missing %q", want)
		}
	}
	// The board is the LEAD of --full: it must appear before the section tables.
	if bi, hi := strings.Index(out, "subsystem"), strings.Index(out, "HEALTH"); bi < 0 || (hi >= 0 && bi > hi) {
		t.Error("status board should lead --full, before the section tables")
	}
}

func TestTerminal_cleanGroupedScoresHighAndListsGood(t *testing.T) {
	c := sampleContext()
	c.Findings = nil
	var buf bytes.Buffer
	if err := Terminal(&buf, c, Options{Color: false}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	// No findings → perfect score, no CRITICAL/WARNING groups, a GOOD list that
	// names the healthy cache hit with its value.
	for _, want := range []string{"100/100", "GOOD", "cache hit ratio 99.4%"} {
		if !strings.Contains(out, want) {
			t.Errorf("clean grouped view missing %q", want)
		}
	}
	if strings.Contains(out, "CRITICAL") || strings.Contains(out, "WARNING") {
		t.Error("a clean database must show no CRITICAL/WARNING groups")
	}
}

func TestTerminalCockroachPreviewIsHonestAndShowsActivity(t *testing.T) {
	p99 := 74.1
	c := &model.Context{
		Server:   model.ServerInfo{Engine: "cockroachdb", VersionText: "CockroachDB CCL v26.4.0", Database: "defaultdb", HasViewActivity: true},
		Activity: &model.Activity{Section: model.Section{Exactness: model.ExactnessScraped}, Total: 7, Active: 2, Idle: 5},
		Queries: &model.Queries{Enabled: true, Section: model.Section{Exactness: model.ExactnessCumulative}, WindowHours: 24, Bounded: true, Top: []model.QueryStat{{
			AppName: "checkout", Query: "SELECT * FROM orders WHERE id = $1", Calls: 42, MeanMS: 18.2, P99MS: &p99, MaxRetries: 2,
		}}},
		Cockroach: &model.CockroachDB{LiveQueries: model.CockroachLiveQueries{
			Section: model.Section{Exactness: model.ExactnessScraped},
			Items:   []model.CockroachLiveQuery{{AppName: "worker", Query: "UPDATE jobs SET claimed = true", AgeSec: 65, Retries: 1}},
		}},
	}
	var buf bytes.Buffer
	if err := Terminal(&buf, c, Options{Color: false}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"cockroachdb v26.4.0", "COCKROACHDB PREVIEW", "Cluster health: unavailable", "Workload health: 100/100", "Coverage: SQL workload diagnostics", "7 total · 2 active · 5 idle", "LIVE QUERIES", "worker", "TOP QUERIES — CACHED 24H", "checkout"} {
		if !strings.Contains(out, want) {
			t.Errorf("CockroachDB output missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "Database health:") || strings.Contains(out, "CockroachDB health:") || strings.Contains(out, "pg_monitor") {
		t.Errorf("CockroachDB preview must separate cluster/workload health and not request pg_monitor\n---\n%s", out)
	}
}

func TestTerminalCockroachClusterHealthSummary(t *testing.T) {
	qps := 13200.0
	c := &model.Context{
		Server: model.ServerInfo{Engine: "cockroachdb", VersionText: "CockroachDB CCL v26.4.0", Database: "defaultdb", HasViewActivity: true},
		Health: &model.Health{Section: model.Section{Exactness: model.ExactnessSampled}, Cockroach: &model.CockroachHealth{
			AdminAPI: model.Section{Exactness: model.ExactnessScraped}, Prometheus: model.Section{Exactness: model.ExactnessSampled},
			NodesTotal: 6, NodesLive: 6, StoresTotal: 24, CapacityBytes: 9 << 40, AvailableBytes: 5 << 40,
			MaxStoreUsedRatio: .526, MaxCPUPercent: 62, SQLConnections: 385, QueriesPerSec: &qps,
		}},
	}
	var buf bytes.Buffer
	if err := Terminal(&buf, c, Options{Color: false}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"Cluster health: 100/100", "Workload health: 100/100", "Admin API + Prometheus", "CLUSTER HEALTH", "6/6 nodes live", "24 stores", "13.2k queries/s", "GOOD", "all 6 nodes live"} {
		if !strings.Contains(out, want) {
			t.Errorf("CockroachDB health output missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "coverage is still partial") {
		t.Errorf("configured cluster coverage mislabeled partial\n%s", out)
	}
}

func TestCockroachScoresSeparateInfrastructureAndWorkload(t *testing.T) {
	c := &model.Context{Findings: []model.Finding{
		{ID: "crdb_resource_pressure", Severity: model.SeverityWarn},
		{ID: "crdb_contention_hotspot", Severity: model.SeverityWarn},
		{ID: "crdb_retry_hotspot", Severity: model.SeverityWarn},
	}}
	cluster, workload := computeCockroachScores(c)
	if cluster != 97 || workload != 94 {
		t.Fatalf("cluster=%d workload=%d, want 97/94", cluster, workload)
	}
}

func TestCockroachFailedJobAffectsWorkloadNotServingCluster(t *testing.T) {
	c := &model.Context{Findings: []model.Finding{{ID: "crdb_job_failed", Severity: model.SeverityWarn}}}
	cluster, workload := computeCockroachScores(c)
	if cluster != 100 || workload != 97 {
		t.Fatalf("cluster=%d workload=%d, want 100/97", cluster, workload)
	}
}

func TestTerminalCockroachJobScreens(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	updated := now.Add(-35 * time.Minute)
	highWater := now.Add(-40 * time.Second)
	c := &model.Context{
		CollectedAt: now,
		Server:      model.ServerInfo{Engine: "cockroachdb", Database: "gitload", HasViewActivity: true},
		Health: &model.Health{Section: model.Section{Exactness: model.ExactnessUnavailable, Reason: "Admin API not configured"}, Cockroach: &model.CockroachHealth{
			Jobs: model.Section{Exactness: model.ExactnessScraped}, JobsTotal: 30, JobsBounded: true,
			JobItems: []model.CockroachJobHealth{{
				JobID: "991", Type: "SCHEMA CHANGE", State: "running", CreatedAt: now.Add(-2 * time.Hour), Progress: .42, ProgressKnown: true,
				Operation: "CREATE INDEX orders_customer_idx ON gitload.public.orders (customer_id)", StatusMessage: "backfilling indexes", Error: "retrying after node unavailable",
				LastUpdatedAt: &updated, HighWaterAt: &highWater,
			}},
		}},
	}
	for _, full := range []bool{false, true} {
		var buf bytes.Buffer
		if err := Terminal(&buf, c, Options{Color: false, Full: full}); err != nil {
			t.Fatal(err)
		}
		out := buf.String()
		for _, want := range []string{"JOBS & SCHEMA CHANGES", "SCHEMA CHANGE", "running", "42.0%", "CREATE INDEX orders_customer_idx", "showing"} {
			if !strings.Contains(out, want) {
				t.Errorf("full=%v missing %q\n%s", full, want, out)
			}
		}
		if full {
			for _, want := range []string{"jobs", "30 tracked", "35m0s ago", "40s ago", "status: backfilling indexes", "error: retrying after node unavailable", "operation, status, and error text are redacted"} {
				if !strings.Contains(out, want) {
					t.Errorf("full job screen missing %q\n%s", want, out)
				}
			}
		}
	}
}

func TestTerminalCockroachDistributionScreens(t *testing.T) {
	c := &model.Context{
		Server: model.ServerInfo{Engine: "cockroachdb", Database: "gitload", HasViewActivity: true},
		Health: &model.Health{Section: model.Section{Exactness: model.ExactnessScraped}, Cockroach: &model.CockroachHealth{
			AdminAPI: model.Section{Exactness: model.ExactnessScraped},
			Distribution: model.CockroachDistribution{
				Section: model.Section{Exactness: model.ExactnessScraped}, LiveStores: 3, ComparableStores: 3,
				ReplicaMean: 166.7, ReplicaMin: 100, ReplicaMax: 300, LeaseMean: 100, LeaseMin: 40, LeaseMax: 220,
				CapacityUsedMinRatio: .3, CapacityUsedMaxRatio: .8, CapacityUsedSpread: .5,
				HotRangeLeaseholderSamples: 5, HotRangeCPUCores: .9, HottestLeaseholderNodeID: 1, HottestLeaseholderRanges: 4, HottestLeaseholderCPUShare: .8889,
				Stores: []model.CockroachStoreBalance{{NodeID: 1, StoreID: 1, Status: "live", Comparable: true, Locality: "region=east,zone=a", UsedRatio: .8, RangeReplicas: 300, Leaseholders: 220, NodeCPUPercent: 82, TopHotRanges: 4, TopHotCPUCores: .8}},
			},
			Hot: []model.CockroachHotRange{{RangeID: 7, LeaseholderNodeID: 1, CPUCores: .4, QPS: 900, ReadsPerSec: 700, WritesPerSec: 200, Tables: []string{"orders"}, Indexes: []string{"orders_pkey"}}},
		}},
	}
	for _, full := range []bool{false, true} {
		var buf bytes.Buffer
		if err := Terminal(&buf, c, Options{Color: false, Full: full}); err != nil {
			t.Fatal(err)
		}
		out := buf.String()
		for _, want := range []string{"DISTRIBUTION & BALANCE", "replicas 100–300", "leases 40–220", "30.0%–80.0%", "n1", "88.9%"} {
			if !strings.Contains(out, want) {
				t.Errorf("full=%v missing %q\n%s", full, want, out)
			}
		}
		if full {
			for _, want := range []string{"s1", "82.0%", "4 / 0.800 CPU", "region=east,zone=a", "HOTTEST PHYSICAL RANGES", "orders/orders_pkey", "±25%"} {
				if !strings.Contains(out, want) {
					t.Errorf("full distribution screen missing %q\n%s", want, out)
				}
			}
		}
	}
}

func TestTerminalCockroachStorageScreens(t *testing.T) {
	c := &model.Context{
		Server: model.ServerInfo{Engine: "cockroachdb", Database: "gitload", HasViewActivity: true},
		Health: &model.Health{Section: model.Section{Exactness: model.ExactnessSampled}, Cockroach: &model.CockroachHealth{
			AdminAPI: model.Section{Exactness: model.ExactnessScraped},
			Storage: model.CockroachStorage{
				Section: model.Section{Exactness: model.ExactnessSampled}, LiveStores: 1,
				MVCCMetricsAvailable: true, ReplicationMetricsAvailable: true, CounterSampledStores: 1, SampleSeconds: 2,
				FilesystemUsedBytes: 900 << 30, CockroachUsedBytes: 500 << 30, OtherUsedBytes: 400 << 30,
				MVCCLiveBytes: 400 << 30, MVCCTotalBytes: 500 << 30, MVCCGarbageBytes: 100 << 30, MVCCLiveRatio: .8,
				BytesPerReplicaMin: 50 << 20, BytesPerReplicaMean: 100 << 20, BytesPerReplicaMax: 200 << 20,
				RangeReplicas: 100, UninitializedReplicas: 20, ReservedReplicas: 2, OverreplicatedRanges: 1,
				RaftCommandsPending: 12, MaxRaftCommandsPending: 12, MaxRaftPendingStoreID: 1,
				RaftProbeFlows: 2, RaftSnapshotFlows: 3, ReplicateQueuePending: 40, ReplicateQueuePurgatory: 4, RaftSnapshotQueuePending: 5,
				DiskSlowEvents: 1, WriteStallEvents: 2, WriteStallSeconds: .5, RaftDroppedMessages: 7,
				Stores: []model.CockroachStoreStorage{{
					NodeID: 1, StoreID: 1, Status: "live", Locality: "region=east,zone=a",
					CapacityBytes: 1000 << 30, FilesystemUsedBytes: 900 << 30, CockroachUsedBytes: 500 << 30, OtherUsedBytes: 400 << 30, OtherUsedRatio: .4,
					MVCCLiveBytes: 400 << 30, MVCCTotalBytes: 500 << 30, MVCCGarbageBytes: 100 << 30, BytesPerReplica: 200 << 20,
					RangeReplicas: 100, UninitializedReplicas: 20, ReservedReplicas: 2, OverreplicatedRanges: 1,
					RaftCommandsPending: 12, RaftProbeFlows: 2, RaftSnapshotFlows: 3, ReplicateQueuePending: 40, ReplicateQueuePurgatory: 4, RaftSnapshotQueuePending: 5,
					DiskSlowEvents: 1, WriteStallEvents: 2, WriteStallSeconds: .5, RaftDroppedMessages: 7,
				}},
			},
		}},
	}
	for _, full := range []bool{false, true} {
		var buf bytes.Buffer
		if err := Terminal(&buf, c, Options{Color: false, Full: full}); err != nil {
			t.Fatal(err)
		}
		out := buf.String()
		for _, want := range []string{"STORAGE & REPLICATION", "filesystem 900.0 GiB used", "CockroachDB 500.0 GiB", "400.0 GiB", "20 uninitialized", "12 commands pending"} {
			if !strings.Contains(out, want) {
				t.Errorf("full=%v missing %q\n%s", full, want, out)
			}
		}
		if full {
			for _, want := range []string{"STORAGE", "REPLICATION", "s1", "90.0%", "400.0 GiB/500.0 GiB", "2/3", "40/4", "EVENTS DURING SAMPLE", "0.50s", "logical replicated bytes"} {
				if !strings.Contains(out, want) {
					t.Errorf("full storage screen missing %q\n%s", want, out)
				}
			}
		}
	}
}

func TestCockroachScreenRendersOnlyRequestedSection(t *testing.T) {
	c := &model.Context{
		Server: model.ServerInfo{Engine: "cockroachdb", VersionText: "CockroachDB CCL v26.4.0", Database: "gitload"},
		Health: &model.Health{Cockroach: &model.CockroachHealth{
			Storage: model.CockroachStorage{
				Section: model.Section{Exactness: model.ExactnessSampled}, LiveStores: 3,
				FilesystemUsedBytes: 900 << 30, CockroachUsedBytes: 500 << 30, OtherUsedBytes: 400 << 30,
			},
			Jobs: model.Section{Exactness: model.ExactnessScraped}, JobsTotal: 7,
		}},
	}
	var buf bytes.Buffer
	if err := CockroachScreen(&buf, c, "storage", Options{Color: false, Host: "roach.example"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"connected", "roach.example", "cockroachdb v26.4.0", "STORAGE & REPLICATION", "3 live stores"} {
		if !strings.Contains(out, want) {
			t.Errorf("storage screen missing %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "JOBS & SCHEMA CHANGES") {
		t.Errorf("storage screen rendered an unrelated jobs section\n%s", out)
	}
}

func TestCockroachScreenRejectsPostgres(t *testing.T) {
	err := CockroachScreen(&bytes.Buffer{}, &model.Context{Server: model.ServerInfo{Engine: "postgres"}}, "health", Options{})
	if err == nil || !strings.Contains(err.Error(), "requires a CockroachDB connection") {
		t.Fatalf("error = %v, want CockroachDB requirement", err)
	}
}

func TestTerminalCockroachContentionScreens(t *testing.T) {
	c := &model.Context{
		Server: model.ServerInfo{Engine: "cockroachdb", Database: "defaultdb", HasViewActivity: true},
		Cockroach: &model.CockroachDB{Contention: model.CockroachContention{
			Section: model.Section{Exactness: model.ExactnessScraped}, WindowMinutes: 60, Bounded: true,
			TotalEvents: 162, TotalWaitMS: 482_843, MaxWaitMS: 22_057,
			Hotspots: []model.CockroachContentionHotspot{
				{Database: "gitload", Schema: "public", Table: "file_latest", Index: "file_latest_pkey", Type: "LOCK_WAIT", WaitingStatementFingerprint: "396486b75e763906", WaitingQuery: "INSERT INTO file_latest VALUES (_, __more__)", WaitingApplications: []string{"gitload"}, WaiterResolution: model.CockroachContentionResolved, BlockerResolution: model.CockroachContentionNotResolved, Events: 120, TotalWaitMS: 402_843, MaxWaitMS: 22_057},
				{Database: "tpcc", Schema: "public", Table: "warehouse", Index: "warehouse_pkey", Type: "LOCK_WAIT", WaitingStatementFingerprint: "51b529815800f098", WaitingQuery: "UPDATE warehouse SET w_ytd = w_ytd + _", WaitingApplications: []string{"tpcc"}, WaiterResolution: model.CockroachContentionResolved, BlockingTxnFingerprint: "18ebfa771365852a", BlockingQueries: []string{"INSERT INTO history VALUES (_, __more__)", "UPDATE district SET d_ytd = d_ytd + _"}, BlockingApplications: []string{"tpcc"}, BlockerResolution: model.CockroachContentionResolved, Events: 42, TotalWaitMS: 80_000, MaxWaitMS: 12_000},
			},
		}},
	}
	for _, full := range []bool{false, true} {
		var buf bytes.Buffer
		if err := Terminal(&buf, c, Options{Color: false, Full: full}); err != nil {
			t.Fatal(err)
		}
		out := buf.String()
		for _, want := range []string{"CONTENTION", "162 events", "gitload.public.file_latest/file_latest_pkey", "396486b75e76", "waiter · app gitload", "not resolved by CockroachDB", "blocker · app tpcc", "+1 statements"} {
			if !strings.Contains(out, want) {
				t.Errorf("full=%v missing %q\n%s", full, want, out)
			}
		}
		if strings.Contains(out, "contending_key") || strings.Contains(out, "txn_id") {
			t.Errorf("sensitive/ephemeral keys leaked\n%s", out)
		}
	}
}

func TestTerminalCockroachIndexUsageScreens(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	lastRead := now.Add(-2 * time.Hour)
	created := now.Add(-20 * 24 * time.Hour)
	c := &model.Context{
		CollectedAt: now,
		Server:      model.ServerInfo{Engine: "cockroachdb", Database: "gitload"},
		Indexes: &model.Indexes{
			Section: model.Section{Exactness: model.ExactnessScraped}, Total: 10, Scanned: 10, SecondaryTotal: 3,
			UnusedThresholdHours: 168,
			Unused:               []model.IndexStat{{Database: "gitload", Schema: "public", Table: "commits", Name: "old_idx", CreatedAt: &created, UnusedForSeconds: 20 * 24 * 3600}},
			Usage:                []model.IndexStat{{Database: "gitload", Schema: "public", Table: "file_latest", Name: "idx_file_latest_last_commit", Scans: 38989, LastRead: &lastRead}},
		},
		Findings: []model.Finding{{ID: "crdb_unused_indexes", Severity: model.SeverityWarn, Title: "1 CockroachDB secondary index unused", Impact: model.Impact{Dimension: model.DimThroughput, Score: 45}}},
	}
	for _, full := range []bool{false, true} {
		var buf bytes.Buffer
		if err := Terminal(&buf, c, Options{Color: false, Full: full}); err != nil {
			t.Fatal(err)
		}
		out := buf.String()
		for _, want := range []string{"INDEX", "10 total · 3 secondary · 1 unused ≥7d", "gitload.public.commits/old_idx", "write counters unavailable"} {
			if !strings.Contains(out, want) {
				t.Errorf("full=%v missing %q\n%s", full, want, out)
			}
		}
		if full {
			for _, want := range []string{"in-memory and non-durable", "SECONDARY INDEX USAGE", "idx_file_latest_last_commit", "39.0k", "2h0m ago"} {
				if !strings.Contains(out, want) {
					t.Errorf("full screen missing %q\n%s", want, out)
				}
			}
		}
	}
}

func TestTerminalCockroachTableHealthScreens(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	metadataAt := now.Add(-17 * time.Hour)
	statsAt := now.Add(-2 * time.Hour)
	c := &model.Context{
		CollectedAt: now,
		Server:      model.ServerInfo{Engine: "cockroachdb", Database: "gitload"},
		Tables: &model.Tables{
			Section: model.Section{Exactness: model.ExactnessScraped}, Total: 2, Scanned: 2,
			DBSizeBytes: 8 << 30, StatsSource: "cockroachdb_table_metadata_api", SizeKind: "replicated_disk_estimate", MetadataOldestAt: &metadataAt,
			Top: []model.TableStat{{Database: "gitload", Schema: "public", Name: "blobs", ReplicatedBytes: 6 << 30, LiveDataBytes: 3 << 30, DataBytes: 4 << 30, LiveDataRatio: .75, RangeCount: 30, ReplicaCount: 90, StoreIDs: []int64{1, 2, 3}, AutoStatsEnabled: true, StatsLastUpdated: &statsAt, TopHotRangeCount: 2, TopHotRangeQPS: 125.5}},
		},
	}
	for _, full := range []bool{false, true} {
		var buf bytes.Buffer
		if err := Terminal(&buf, c, Options{Color: false, Full: full}); err != nil {
			t.Fatal(err)
		}
		out := buf.String()
		for _, want := range []string{"TABLE", "2 tables", "8.0 GiB", "gitload.public.blobs", "75.0%", "30"} {
			if !strings.Contains(out, want) {
				t.Errorf("full=%v missing %q\n%s", full, want, out)
			}
		}
		if full {
			for _, want := range []string{"cached Admin API metadata", "oldest row 17h0m ago", "MVCC live/total", "3.0x", "2 / 125.5 qps"} {
				if !strings.Contains(out, want) {
					t.Errorf("full table screen missing %q\n%s", want, out)
				}
			}
		}
	}
}

func TestTerminalFullCockroachCapsLiveQueries(t *testing.T) {
	items := make([]model.CockroachLiveQuery, 18)
	for i := range items {
		items[i] = model.CockroachLiveQuery{AppName: fmt.Sprintf("app-%02d", i), Query: "SELECT 1", AgeSec: float64(i)}
	}
	c := &model.Context{
		Server: model.ServerInfo{Engine: "cockroachdb", Database: "defaultdb", HasViewActivity: true},
		Cockroach: &model.CockroachDB{LiveQueries: model.CockroachLiveQueries{
			Section: model.Section{Exactness: model.ExactnessScraped}, Items: items,
		}},
	}
	var buf bytes.Buffer
	if err := Terminal(&buf, c, Options{Color: false, Full: true}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "app-14") || strings.Contains(out, "app-15") || !strings.Contains(out, "… and 3 more running queries") {
		t.Fatalf("live-query cap not rendered correctly\n---\n%s", out)
	}
}

func TestTerminalFullCockroachQualifiesEmptyFindings(t *testing.T) {
	c := &model.Context{
		Server:   model.ServerInfo{Engine: "cockroachdb", VersionText: "CockroachDB CCL v26.4.0", Database: "defaultdb", HasViewActivity: true},
		Activity: &model.Activity{Section: model.Section{Exactness: model.ExactnessScraped}, Total: 1, Active: 1},
	}
	var buf bytes.Buffer
	if err := Terminal(&buf, c, Options{Color: false, Full: true}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "COCKROACHDB PREVIEW") || !strings.Contains(out, "no findings from supported CockroachDB checks") {
		t.Fatalf("full CockroachDB report must qualify its scope\n---\n%s", out)
	}
	if strings.Contains(out, "no findings — nothing stood out") {
		t.Fatalf("full CockroachDB report made an unqualified clean claim\n---\n%s", out)
	}
}

// DoD 13: a schema-profile report states it is a schema check and makes no claim
// about a running database's health — no GOOD list (it infers health from a
// finding's absence), and the score is relabeled.
func TestTerminal_schemaProfileIsHonest(t *testing.T) {
	c := sampleContext()
	c.Profile = "schema"
	var buf bytes.Buffer
	if err := Terminal(&buf, c, Options{Color: false}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"SCHEMA CHECK", "health of a running database", "Schema check:"} {
		if !strings.Contains(out, want) {
			t.Errorf("schema-profile header missing %q", want)
		}
	}
	if strings.Contains(out, "GOOD") {
		t.Error("schema profile must not print a GOOD list (it never ran those checks)")
	}
	if strings.Contains(out, "Database health:") {
		t.Error("schema profile must relabel the score, not claim overall database health")
	}
}

func TestSparkline(t *testing.T) {
	if s := sparkline(nil); s != "" {
		t.Errorf("empty series -> empty spark, got %q", s)
	}
	s := sparkline([]float64{1, 2, 3, 4, 5})
	if len([]rune(s)) != 5 {
		t.Errorf("expected 5 spark runes, got %q", s)
	}
}

func TestFull_rendersDetailSections(t *testing.T) {
	c := sampleContext()
	c.Findings = nil
	c.Queries = &model.Queries{
		Section: model.Section{Exactness: model.ExactnessCumulative}, Enabled: true,
		Top: []model.QueryStat{{QueryID: 42, Query: "SELECT pg_database_size(current_database())", Calls: 12, TotalMS: 221.7, MeanMS: 18.48}},
	}
	c.Tables = &model.Tables{
		Section: model.Section{Exactness: model.ExactnessScraped}, DBSizeBytes: 8 << 30,
		Top: []model.TableStat{{Schema: "public", Name: "orders", TotalBytes: 1 << 30, LiveTuples: 1_000_000, SeqScans: 10, IndexScans: 900}},
	}
	wal, bw := 1024.0, 5.0
	c.WAL = &model.WAL{Section: model.Section{Exactness: model.ExactnessSampled}, BytesPerSec: &wal}
	c.IO = &model.IO{Section: model.Section{Exactness: model.ExactnessSampled}, CheckpointsTimed: 8, CheckpointsReq: 1, BuffersWrittenPerS: &bw}
	c.Replication = &model.Replication{Section: model.Section{Exactness: model.ExactnessScraped}, Replicas: []model.ReplicaRow{{ClientAddr: "10.0.0.2", State: "streaming"}}}
	c.Settings = &model.Settings{Section: model.Section{Exactness: model.ExactnessScraped}, Overrides: map[string]string{"work_mem": "64MB", "shared_buffers": "8GB"}}

	var buf bytes.Buffer
	if err := Terminal(&buf, c, Options{Color: false, Full: true}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	// Each detail section must render its header and at least one real value.
	for _, want := range []string{
		"QUERIES", "pg_database_size", "18.48 ms",
		"TABLES", "database size", "public.orders",
		"WAL", "IO", "checkpoints 8 timed / 1 req",
		"REPLICATION", "standby", "SETTINGS", "2 non-default",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("--full detail sections missing %q", want)
		}
	}
}
