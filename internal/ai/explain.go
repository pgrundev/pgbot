package ai

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/pgrundev/pgbot/internal/model"
)

// systemPrompt hard-constrains the model to the one job it's allowed: explaining
// facts pgbot already computed, in a terse spoken-style shape. The findings are
// ground truth; the model may not add to them, and it MUST carry every caveat.
const systemPrompt = `You are a senior database reliability expert giving a terse, spoken-style diagnosis to a
developer who is not a DBA. A tool called pgbot has already analyzed the database and computed the
findings and aggregate diagnostics below DETERMINISTICALLY — they are facts, not guesses. Explain
the findings; use the aggregate diagnostics only as supporting context, and never add new findings.

Write PLAIN TEXT in this exact shape — NO markdown (no #, no *, no bullet characters, no bold, no
tables, no code fences):

  <one sentence on overall health, e.g. "Your database is mostly healthy.">

  <then the findings that matter, worst first, at most three, each as a short block:>

  1 critical issue:          (the count and tier of this block — or "2 warnings:", etc.)
  <one line naming the problem, with the concrete number from the data>

  Likely cause:
  <one line — ONLY when the deltas or events actually support a cause; otherwise OMIT this
   "Likely cause:" block entirely. Never speculate a cause that the data does not show.>

  Recommended:
  <one line: a concrete, safe next step>

Rules you must follow:
- Every number comes from the data. Do NOT invent a multiplier, percentage, table, or query id.
- Each finding may carry "caveats". Carry them into the Recommended line — a caveat like
  "replication makes these per-node counts unreliable" means "verify on the replicas first" and
  is load-bearing. Never recommend a destructive action (dropping an index) without its caveat.
- A finding with confidence below 0.5 is a possibility — say "possibly" or "may", not a fact.
- Never recommend anything that EXECUTES a user's query to diagnose it (e.g. running it under
  timing); suggest only safe, non-executing steps.
- Respect the report's "engine" field. Never translate a finding into advice for a different engine.
  In particular, do not recommend PostgreSQL-only facilities such as VACUUM, autovacuum, WAL,
  pg_stat_statements, PostgreSQL extensions, or PostgreSQL configuration parameters for CockroachDB.
- Be brief: short lines, a blank line between blocks. If the database is healthy, say so in one
  line and stop.`

// explainPayload is the PII-free subset of the Context we send. The full Context
// is already PII-free by construction (normalized query text, no literal values),
// but we send a focused view so the model reasons about signals, not noise.
type explainPayload struct {
	Engine       string            `json:"engine"`
	Database     string            `json:"database"`
	Version      string            `json:"version"`
	ViaPooler    bool              `json:"via_pooler,omitempty"`
	WindowAgeSec *int64            `json:"window_age_seconds,omitempty"`
	ColdWindow   bool              `json:"cold_window,omitempty"`
	Suppressed   string            `json:"delta_suppressed_reason,omitempty"`
	Findings     []model.Finding   `json:"findings"`
	Waits        *waitSummary      `json:"wait_profile,omitempty"`
	Events       []eventSummary    `json:"recent_events,omitempty"`
	Deltas       *model.Deltas     `json:"changes_since_baseline,omitempty"` // temporal deltas — grounds causal claims
	Cockroach    *cockroachSummary `json:"cockroachdb_summary,omitempty"`
}

// cockroachSummary deliberately contains aggregate diagnostics only. It gives
// the model enough context to answer health questions even when no threshold
// fired, without sending query text, application/user names, localities, job
// descriptions, table names, or per-node/store rows.
type cockroachSummary struct {
	Cluster  *cockroachClusterSummary `json:"cluster,omitempty"`
	Workload cockroachWorkloadSummary `json:"workload"`
}

type cockroachClusterSummary struct {
	AdminAPIExactness    string                       `json:"admin_api_exactness,omitempty"`
	PrometheusExactness  string                       `json:"prometheus_exactness,omitempty"`
	JobsExactness        string                       `json:"jobs_exactness,omitempty"`
	HotRangesExactness   string                       `json:"hot_ranges_exactness,omitempty"`
	NodesTotal           int                          `json:"nodes_total"`
	NodesLive            int                          `json:"nodes_live"`
	NodesSuspect         int                          `json:"nodes_suspect"`
	NodesDraining        int                          `json:"nodes_draining"`
	StoresTotal          int                          `json:"stores_total"`
	UnavailableRanges    int64                        `json:"unavailable_ranges"`
	Underreplicated      int64                        `json:"underreplicated_ranges"`
	CapacityBytes        int64                        `json:"capacity_bytes"`
	AvailableBytes       int64                        `json:"available_bytes"`
	MaxStoreUsedRatio    float64                      `json:"max_store_used_ratio"`
	MaxCPUPercent        float64                      `json:"max_cpu_percent"`
	MaxMemoryUsedRatio   float64                      `json:"max_memory_used_ratio"`
	SQLConnections       int                          `json:"sql_connections"`
	QueriesPerSec        *float64                     `json:"queries_per_sec,omitempty"`
	NewConnectionsPerSec *float64                     `json:"new_connections_per_sec,omitempty"`
	ServiceLatencyP99MS  float64                      `json:"service_latency_p99_ms"`
	AdmissionWaitP99MS   float64                      `json:"admission_wait_p99_ms"`
	AdmissionQueueMax    int64                        `json:"admission_queue_max"`
	JobsTotal            int                          `json:"jobs_total"`
	HotRangesSampled     int                          `json:"hot_ranges_sampled"`
	Distribution         cockroachDistributionSummary `json:"distribution"`
	Storage              cockroachStorageSummary      `json:"storage_replication"`
}

type cockroachDistributionSummary struct {
	Exactness               string  `json:"exactness,omitempty"`
	LiveStores              int     `json:"live_stores"`
	ComparableStores        int     `json:"comparable_stores"`
	ReplicaMin              int64   `json:"replica_min"`
	ReplicaMax              int64   `json:"replica_max"`
	LeaseMin                int64   `json:"lease_min"`
	LeaseMax                int64   `json:"lease_max"`
	CapacityUsedSpread      float64 `json:"capacity_used_spread"`
	HottestLeaseholderShare float64 `json:"hottest_leaseholder_cpu_share"`
}

type cockroachStorageSummary struct {
	Exactness                string  `json:"exactness,omitempty"`
	FilesystemUsedBytes      int64   `json:"filesystem_used_bytes"`
	CockroachUsedBytes       int64   `json:"cockroach_used_bytes"`
	OtherUsedBytes           int64   `json:"other_used_bytes"`
	MVCCLiveBytes            int64   `json:"mvcc_live_bytes"`
	MVCCTotalBytes           int64   `json:"mvcc_total_bytes"`
	MVCCGarbageBytes         int64   `json:"mvcc_garbage_bytes"`
	MVCCLiveRatio            float64 `json:"mvcc_live_ratio"`
	UninitializedReplicas    int64   `json:"uninitialized_replicas"`
	ReservedReplicas         int64   `json:"reserved_replicas"`
	OverreplicatedRanges     int64   `json:"overreplicated_ranges"`
	RaftCommandsPending      int64   `json:"raft_commands_pending"`
	ReplicateQueuePending    int64   `json:"replicate_queue_pending"`
	ReplicateQueuePurgatory  int64   `json:"replicate_queue_purgatory"`
	RaftSnapshotQueuePending int64   `json:"raft_snapshot_queue_pending"`
	DiskSlowEvents           int64   `json:"disk_slow_events"`
	DiskStalledEvents        int64   `json:"disk_stalled_events"`
	WriteStallEvents         int64   `json:"write_stall_events"`
	RaftDroppedMessages      int64   `json:"raft_dropped_messages"`
}

type cockroachWorkloadSummary struct {
	ActivityExactness       string  `json:"activity_exactness,omitempty"`
	SessionsTotal           int     `json:"sessions_total"`
	SessionsActive          int     `json:"sessions_active"`
	SessionsWaiting         int     `json:"sessions_waiting"`
	LongestActiveSeconds    float64 `json:"longest_active_seconds"`
	LiveQueries             int     `json:"live_queries"`
	ExecutionInsights       int     `json:"execution_insights"`
	ContentionExactness     string  `json:"contention_exactness,omitempty"`
	ContentionWindowMinutes int     `json:"contention_window_minutes"`
	ContentionEvents        int64   `json:"contention_events"`
	ContentionWaitMS        float64 `json:"contention_wait_ms"`
	SerializationConflicts  int64   `json:"serialization_conflicts"`
	QueryStatsExactness     string  `json:"query_stats_exactness,omitempty"`
	QueryStatsSource        string  `json:"query_stats_source,omitempty"`
	QueryStatsWindowHours   int     `json:"query_stats_window_hours"`
	PersistedQueryCount     int     `json:"persisted_query_count"`
	TablesTotal             int     `json:"tables_total"`
	IndexesTotal            int     `json:"indexes_total"`
	SecondaryIndexes        int     `json:"secondary_indexes"`
	UnusedSecondaryIndexes  int     `json:"unused_secondary_indexes"`
}

type waitSummary struct {
	Samples int                `json:"samples"`
	Buckets []model.WaitBucket `json:"top_buckets,omitempty"`
	ByQuery []model.QueryWaits `json:"top_queries,omitempty"`
}

type eventSummary struct {
	Kind       string  `json:"kind"`
	Object     string  `json:"object,omitempty"`
	Confidence float64 `json:"confidence"`
}

// payloadJSON is the curated, PII-free report we send. The full Context is
// already PII-free; this is a focused view so the model reasons about signals.
func payloadJSON(c *model.Context) string {
	p := explainPayload{
		Engine:     reportEngine(c),
		Database:   c.Server.Database,
		Version:    c.Server.VersionText,
		ViaPooler:  c.Server.ViaPooler,
		ColdWindow: c.Window.ColdWindow(),
		Suppressed: c.DeltaSuppressedReason,
		Findings:   c.Findings,
	}
	if c.Server.Engine == "cockroachdb" {
		p.Cockroach = buildCockroachSummary(c)
	}
	if c.Window.WindowAgeSeconds != nil {
		p.WindowAgeSec = c.Window.WindowAgeSeconds
	}
	if w := c.WaitProfile; w != nil && w.Available && w.Samples > 0 {
		ws := &waitSummary{Samples: w.Samples}
		ws.Buckets = topBuckets(w.Buckets, 5)
		ws.ByQuery = topQueries(w.ByQuery, 3)
		p.Waits = ws
	}
	for _, e := range c.Events {
		p.Events = append(p.Events, eventSummary{Kind: e.Kind, Object: e.Object, Confidence: e.Confidence})
	}
	p.Deltas = c.Deltas // what changed vs the baseline — lets the model ground "3.2× slower"
	blob, _ := json.MarshalIndent(p, "", "  ")
	return string(blob)
}

func reportEngine(c *model.Context) string {
	if c != nil && c.Server.Engine != "" {
		return c.Server.Engine
	}
	return "postgresql"
}

func systemPromptFor(c *model.Context) string {
	engine := reportEngine(c)
	display := "PostgreSQL"
	if engine == "cockroachdb" {
		display = "CockroachDB"
	}
	return systemPrompt + "\n\nThis report is for " + display + " (engine: " + engine + "). Use only " + display + "-appropriate terminology and remediation."
}

func buildCockroachSummary(c *model.Context) *cockroachSummary {
	summary := &cockroachSummary{}
	if c.Activity != nil {
		a := c.Activity
		summary.Workload.ActivityExactness = a.Exactness
		summary.Workload.SessionsTotal = a.Total
		summary.Workload.SessionsActive = a.Active
		summary.Workload.SessionsWaiting = a.Waiting
		summary.Workload.LongestActiveSeconds = a.LongestActiveSec
	}
	if c.Queries != nil {
		q := c.Queries
		summary.Workload.QueryStatsExactness = q.Exactness
		summary.Workload.QueryStatsSource = q.StatsSource
		summary.Workload.QueryStatsWindowHours = q.WindowHours
		summary.Workload.PersistedQueryCount = len(q.Top)
	}
	if c.Tables != nil {
		summary.Workload.TablesTotal = c.Tables.Total
	}
	if c.Indexes != nil {
		summary.Workload.IndexesTotal = c.Indexes.Total
		summary.Workload.SecondaryIndexes = c.Indexes.SecondaryTotal
		summary.Workload.UnusedSecondaryIndexes = len(c.Indexes.Unused)
	}
	if c.Cockroach != nil {
		crdb := c.Cockroach
		summary.Workload.LiveQueries = len(crdb.LiveQueries.Items)
		summary.Workload.ExecutionInsights = len(crdb.ExecutionInsights.Items)
		summary.Workload.ContentionExactness = crdb.Contention.Exactness
		summary.Workload.ContentionWindowMinutes = crdb.Contention.WindowMinutes
		summary.Workload.ContentionEvents = crdb.Contention.TotalEvents
		summary.Workload.ContentionWaitMS = crdb.Contention.TotalWaitMS
		summary.Workload.SerializationConflicts = crdb.Contention.SerializationConflicts
	}
	if c.Health == nil || c.Health.Cockroach == nil {
		return summary
	}
	h := c.Health.Cockroach
	d := h.Distribution
	s := h.Storage
	summary.Cluster = &cockroachClusterSummary{
		AdminAPIExactness: h.AdminAPI.Exactness, PrometheusExactness: h.Prometheus.Exactness,
		JobsExactness: h.Jobs.Exactness, HotRangesExactness: h.HotRanges.Exactness,
		NodesTotal: h.NodesTotal, NodesLive: h.NodesLive, NodesSuspect: h.NodesSuspect,
		NodesDraining: h.NodesDraining, StoresTotal: h.StoresTotal,
		UnavailableRanges: h.UnavailableRanges, Underreplicated: h.UnderreplicatedRanges,
		CapacityBytes: h.CapacityBytes, AvailableBytes: h.AvailableBytes,
		MaxStoreUsedRatio: h.MaxStoreUsedRatio, MaxCPUPercent: h.MaxCPUPercent,
		MaxMemoryUsedRatio: h.MaxMemoryUsedRatio, SQLConnections: h.SQLConnections,
		QueriesPerSec: h.QueriesPerSec, NewConnectionsPerSec: h.NewConnectionsPerSec,
		ServiceLatencyP99MS: h.ServiceLatencyP99MS, AdmissionWaitP99MS: h.AdmissionWaitP99MS,
		AdmissionQueueMax: h.AdmissionQueueMax, JobsTotal: h.JobsTotal, HotRangesSampled: len(h.Hot),
		Distribution: cockroachDistributionSummary{
			Exactness: d.Exactness, LiveStores: d.LiveStores, ComparableStores: d.ComparableStores,
			ReplicaMin: d.ReplicaMin, ReplicaMax: d.ReplicaMax, LeaseMin: d.LeaseMin, LeaseMax: d.LeaseMax,
			CapacityUsedSpread: d.CapacityUsedSpread, HottestLeaseholderShare: d.HottestLeaseholderCPUShare,
		},
		Storage: cockroachStorageSummary{
			Exactness: s.Exactness, FilesystemUsedBytes: s.FilesystemUsedBytes,
			CockroachUsedBytes: s.CockroachUsedBytes, OtherUsedBytes: s.OtherUsedBytes,
			MVCCLiveBytes: s.MVCCLiveBytes, MVCCTotalBytes: s.MVCCTotalBytes,
			MVCCGarbageBytes: s.MVCCGarbageBytes, MVCCLiveRatio: s.MVCCLiveRatio,
			UninitializedReplicas: s.UninitializedReplicas, ReservedReplicas: s.ReservedReplicas,
			OverreplicatedRanges: s.OverreplicatedRanges, RaftCommandsPending: s.RaftCommandsPending,
			ReplicateQueuePending: s.ReplicateQueuePending, ReplicateQueuePurgatory: s.ReplicateQueuePurgatory,
			RaftSnapshotQueuePending: s.RaftSnapshotQueuePending, DiskSlowEvents: s.DiskSlowEvents,
			DiskStalledEvents: s.DiskStalledEvents, WriteStallEvents: s.WriteStallEvents,
			RaftDroppedMessages: s.RaftDroppedMessages,
		},
	}
	return summary
}

// BuildExplainPrompt returns the (system, user) prompt for a Context. The user
// prompt is the curated JSON payload; nothing PII-bearing is included.
func BuildExplainPrompt(c *model.Context) (system, user string) {
	return systemPromptFor(c), "Here is the pgbot report as JSON. Explain it per your rules.\n\n" + payloadJSON(c)
}

// BuildAskPrompt grounds a user's question in the same report. The model must
// answer ONLY from the report and its rules — it can't reach into the database.
func BuildAskPrompt(c *model.Context, question string) (system, user string) {
	user = "A user asked: \"" + question + "\"\n\n" +
		"Answer it using ONLY the pgbot report below and your rules. If the report doesn't\n" +
		"contain the answer, say what pgbot would need to collect to find out — do not guess.\n\n" +
		payloadJSON(c)
	return systemPromptFor(c), user
}

// Explain builds the prompt and calls the model, returning the labeled-elsewhere
// explanation text. A nil/empty findings set still gets an explanation (the model
// is told to confirm health briefly). The Provider may be OpenAI or Gemini.
func Explain(ctx context.Context, p Provider, mc *model.Context) (string, error) {
	if p == nil {
		return "", fmt.Errorf("no AI client")
	}
	system, user := BuildExplainPrompt(mc)
	return p.Generate(ctx, system, user)
}

// Ask answers a specific question grounded on the report.
func Ask(ctx context.Context, p Provider, mc *model.Context, question string) (string, error) {
	if p == nil {
		return "", fmt.Errorf("no AI client")
	}
	system, user := BuildAskPrompt(mc, question)
	return p.Generate(ctx, system, user)
}

func topBuckets(b []model.WaitBucket, n int) []model.WaitBucket {
	if len(b) > n {
		b = b[:n]
	}
	// Trim per-bucket event lists to the single top event to keep the prompt tight.
	out := make([]model.WaitBucket, 0, len(b))
	for _, bk := range b {
		if len(bk.Events) > 1 {
			bk.Events = bk.Events[:1]
		}
		out = append(out, bk)
	}
	return out
}

func topQueries(q []model.QueryWaits, n int) []model.QueryWaits {
	if len(q) > n {
		return q[:n]
	}
	return q
}
