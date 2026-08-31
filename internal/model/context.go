// Package model defines Context — the single public contract every pgbot
// surface is built on: the terminal renderer, --json, the future MCP server,
// and the LLM layer all consume this shape. Treat it as an API. Field order is
// stable (encoding/json preserves struct order) so --json diffs cleanly.
package model

import "time"

// Exactness labels tell a consumer how much to trust a section's numbers.
const (
	ExactnessSampled     = "sampled"             // rate computed from a double-sample over Window.SampleSeconds
	ExactnessCumulative  = "cumulative"          // counters since stats_reset; trend comes from the baseline store
	ExactnessScraped     = "scraped"             // a point-in-time gauge read
	ExactnessUnavailable = "unavailable"         // capability/extension absent; see Reason
	ExactnessReset       = "reset_during_sample" // a counter reset between samples; rates omitted
)

// Severity levels for Finding, highest first.
const (
	SeverityCritical = "critical"
	SeverityWarn     = "warn"
	SeverityInfo     = "info"
)

// Section is embedded in every optional section so consumers can read its
// provenance uniformly.
type Section struct {
	Exactness string `json:"exactness"`
	Reason    string `json:"reason,omitempty"`
}

// Context is the whole picture of one database at one moment.
type Context struct {
	SchemaVersion string `json:"schema_version"`
	// Profile is "schema" for a schema-only run (--profile=schema); empty for the
	// default full run. It changes what the report is allowed to claim (D3-1).
	Profile     string         `json:"profile,omitempty"`
	CollectedAt time.Time      `json:"collected_at"`
	Fingerprint string         `json:"fingerprint"` // stable per target database (see store)
	Server      ServerInfo     `json:"server"`
	Window      Window         `json:"window"`
	Health      *Health        `json:"health,omitempty"`
	Activity    *Activity      `json:"activity,omitempty"`
	Cockroach   *CockroachDB   `json:"cockroachdb,omitempty"`
	Locks       *Locks         `json:"locks,omitempty"`
	Queries     *Queries       `json:"queries,omitempty"`
	Tables      *Tables        `json:"tables,omitempty"`
	Indexes     *Indexes       `json:"indexes,omitempty"`
	WAL         *WAL           `json:"wal,omitempty"`
	IO          *IO            `json:"io,omitempty"`
	Replication *Replication   `json:"replication,omitempty"`
	Settings    *Settings      `json:"settings,omitempty"`
	Limits      *Limits        `json:"limits,omitempty"`
	Horizon     *VacuumHorizon `json:"horizon,omitempty"`   // what pins the xmin/vacuum horizon (A1)
	Sequences   *Sequences     `json:"sequences,omitempty"` // sequence exhaustion headroom (A8)
	Progress    *Progress      `json:"progress,omitempty"`  // in-flight pg_stat_progress_* operations (A11)
	Archiver    *Archiver      `json:"archiver,omitempty"`  // WAL archiving health (A15)
	Checksums   *Checksums     `json:"checksums,omitempty"` // data-checksum failures cluster-wide (A16)
	Standby     *StandbyStatus `json:"standby,omitempty"`   // standby-side recovery conflicts (A17)
	Deltas      *Deltas        `json:"deltas,omitempty"`    // vs baseline; nil on first run
	// Set (with Deltas nil) when a stats reset / restart between runs makes any
	// comparison fiction — e.g. serverless scale-to-zero. See T2.
	DeltaSuppressedReason string       `json:"delta_suppressed_reason,omitempty"`
	Events                []Event      `json:"events,omitempty"`       // what changed since the last run (T7)
	WaitProfile           *WaitProfile `json:"wait_profile,omitempty"` // where time went, from ASH sampling (T8)
	Findings              []Finding    `json:"findings"`
	// ConfigWarnings are non-fatal problems in the loaded .pgbot.toml — an unknown
	// finding ID, an unknown threshold key, a malformed glob (B2-1). A typo'd rule
	// silently not applying is the exact failure suppression exists to prevent, so
	// warnings surface in --json, at the top of the report, and fail `config check`.
	ConfigWarnings []string `json:"config_warnings,omitempty"`

	// Schema is the current schema fingerprint — used to derive events and stored
	// separately; it is NOT part of the JSON contract (too large, and it can echo
	// object names repeatedly).
	Schema *SchemaFingerprint `json:"-"`
}

// Event is a derived change in the database's schema, configuration, or
// lifecycle since the previous run. Most schema changes are only known to have
// happened SOMEWHERE in a time range (OccurredAfter..OccurredBefore) — pgbot
// never renders a precise time it merely inferred.
type Event struct {
	Kind           string     `json:"kind"` // schema.index_dropped, config.changed, server.restarted, stats.reset, ...
	Object         string     `json:"object,omitempty"`
	Before         string     `json:"before,omitempty"`
	After          string     `json:"after,omitempty"`
	OccurredAfter  *time.Time `json:"occurred_after,omitempty"`
	OccurredBefore *time.Time `json:"occurred_before,omitempty"`
	Confidence     float64    `json:"confidence"` // 1.0 for real timestamps (reset/restart), lower for inferred ranges
}

// WaitProfile is the result of sampling pg_stat_activity many times over a short
// window (active-session history): where the database spent its time. It is the
// only signal that attributes TIME rather than counting events, and the only one
// that still works when the cumulative counters reset minutes ago (serverless).
// A profile is a distribution over a sample of moments — never a precise
// measurement — so it always carries its sample count, and a thin sample (<20)
// is flagged as noise rather than dressed up as percentages.
type WaitProfile struct {
	Available     bool         `json:"available"`
	Reason        string       `json:"reason,omitempty"`   // why unavailable (sampler disabled/failed)
	Samples       int          `json:"samples"`            // total active-session samples captured
	WindowSeconds float64      `json:"window_seconds"`     // wall-clock span the samples cover
	Buckets       []WaitBucket `json:"buckets,omitempty"`  // by wait_event_type, share-descending (CPU is synthetic)
	ByQuery       []QueryWaits `json:"by_query,omitempty"` // per query_id attribution (PG14+)
}

// WaitSamplerDisabledReason is the Reason set when the operator turned the sampler
// off (--ash-hz 0) — the one unavailable state the report need not explain, as
// opposed to a sampler that ran and failed.
const WaitSamplerDisabledReason = "sampler disabled (--ash-hz 0)"

// WaitSamplerUnsupportedCockroachReason distinguishes an engine capability gap
// from an operator explicitly disabling PostgreSQL wait-event sampling.
const WaitSamplerUnsupportedCockroachReason = "wait sampling not yet supported on CockroachDB"

// Thin marks a profile too sparse to read as a percentage breakdown.
func (w *WaitProfile) Thin() bool { return w != nil && w.Samples < WaitMinSamples }

// WaitMinSamples is the floor below which shares are noise: findings do not fire
// and the render says so rather than printing a confident-looking breakdown.
const WaitMinSamples = 20

type WaitBucket struct {
	Type   string      `json:"type"`  // Lock, LWLock, IO, Client, BufferPin, Timeout, Extension, CPU
	Count  int         `json:"count"` // samples in this bucket
	Share  float64     `json:"share"` // Count / total, 0..1
	Events []WaitEvent `json:"events,omitempty"`
}

type WaitEvent struct {
	Event string  `json:"event"` // specific wait_event, e.g. transactionid, DataFileRead
	Count int     `json:"count"`
	Share float64 `json:"share"` // Count / total, 0..1
}

// QueryWaits attributes a share of the window's time to one normalized query.
type QueryWaits struct {
	QueryID    int64   `json:"query_id"`
	SampleText string  `json:"sample_text,omitempty"` // scrubbed prefix, best-effort from the queries collector
	Count      int     `json:"count"`
	Share      float64 `json:"share"`      // Count / total window samples, 0..1
	LockShare  float64 `json:"lock_share"` // fraction of THIS query's samples on Lock:*
	IOShare    float64 `json:"io_share"`   // fraction on IO:*
	TopType    string  `json:"top_type,omitempty"`
	TopEvent   string  `json:"top_event,omitempty"`
}

// SchemaFingerprint is the set of schema objects at one moment, hashed for
// cheap diffing.
type SchemaFingerprint struct {
	Objects []SchemaObject
}

// SchemaObject identifies one catalog object and a hash of its definition.
// Column definitions encode type/nullability/has-default only — never the
// default EXPRESSION, which can contain literal values.
type SchemaObject struct {
	Kind           string // table | column | index | constraint | extension | sequence
	Identity       string // e.g. "public.orders" or "public.orders.orders_pkey"
	Definition     string
	DefinitionHash string
	Invalid        bool // indexes only: indisvalid = false (a failed CREATE INDEX CONCURRENTLY)
	// Indexes only. Together with Invalid these decide what an invalid index
	// actually costs (issue #11): IndexReady = pg_index.indisready — false means
	// PostgreSQL ignores it for INSERT/UPDATE, i.e. it is NOT maintained on
	// writes (a build that failed before the index was populated); IndexLive =
	// pg_index.indislive — false means it is being dropped and is ignored for all
	// purposes. Bytes = pg_relation_size, populated only for invalid indexes (a
	// build that never ran is 0 bytes). Valid indexes carry IndexReady/IndexLive
	// = true and Bytes = 0.
	IndexReady bool
	IndexLive  bool
	Bytes      int64
}

// Limits holds cluster-wide saturation gauges: connection slots and the oldest
// transaction-id age (wraparound risk).
type Limits struct {
	Section
	ConnectionsUsed int   `json:"connections_used"`
	ConnectionsMax  int   `json:"connections_max"`
	MaxXIDAge       int64 `json:"max_xid_age"`            // max age(datfrozenxid) across databases; ~2.1e9 is the wraparound wall
	MaxMXIDAge      int64 `json:"max_mxid_age,omitempty"` // max mxid_age(datminmxid); multixact wraparound, same wall
}

// ServerInfo is what we learned at connect time.
type ServerInfo struct {
	Engine          string     `json:"engine"` // postgresql or cockroachdb
	VersionNum      int        `json:"version_num"`
	VersionText     string     `json:"version_text"`
	Database        string     `json:"database"`
	Provider        string     `json:"provider,omitempty"`    // detected managed platform: rds/aurora/cloudsql/azure/supabase/neon/unknown
	InRecovery      bool       `json:"in_recovery,omitempty"` // true on a physical standby (A15-0)
	ViaPooler       bool       `json:"via_pooler,omitempty"`  // connected through a transaction pooler (rates still correct)
	StartedAt       *time.Time `json:"started_at,omitempty"`  // pg_postmaster_start_time()
	UptimeSeconds   int64      `json:"uptime_seconds"`
	Extensions      []string   `json:"extensions"`
	Capabilities    []string   `json:"capabilities"` // human-readable flags that were satisfied
	HasPgMonitor    bool       `json:"has_pg_monitor"`
	HasViewActivity bool       `json:"has_view_activity,omitempty"` // CockroachDB cluster activity visibility
}

// Window describes the sampling interval and how old the underlying cumulative
// statistics are — the latter matters because scale-to-zero serverless Postgres
// discards in-memory stats on each wake, making cache-hit ratios, unused-index
// findings and cross-run deltas meaningless on a cold window (see T2).
type Window struct {
	SampleSeconds     float64    `json:"sample_seconds"`                // gap between the two counter samples
	StatsResetAt      *time.Time `json:"stats_reset_at,omitempty"`      // pg_stat_database.stats_reset
	PostmasterStartAt *time.Time `json:"postmaster_start_at,omitempty"` // pg_postmaster_start_time()
	WindowAgeSeconds  *int64     `json:"window_age_seconds,omitempty"`  // age of the effective stats window (since reset, else start)
	StatsWindowDays   *float64   `json:"stats_window_days,omitempty"`   // now - stats_reset, in days
}

// ColdWindowThresholdSeconds is the age below which cumulative counters haven't
// accumulated enough to trust — counter-based findings degrade below it.
const ColdWindowThresholdSeconds = 900

// ColdWindow reports whether the stats window is too young to trust counters.
func (w Window) ColdWindow() bool {
	return w.WindowAgeSeconds != nil && *w.WindowAgeSeconds < ColdWindowThresholdSeconds
}

// Health is derived from pg_stat_database — the double-sampled aggregate rates.
type Health struct {
	Section
	Connections     int              `json:"connections"`
	TPS             *float64         `json:"tps,omitempty"` // commits+rollbacks per second
	CommitsPerSec   *float64         `json:"commits_per_sec,omitempty"`
	RollbacksPerSec *float64         `json:"rollbacks_per_sec,omitempty"`
	RollbackRatio   *float64         `json:"rollback_ratio,omitempty"`       // over the sample window
	CacheHitRatio   *float64         `json:"cache_hit_ratio,omitempty"`      // 0..1 over the sample window
	CacheBlocks     *int64           `json:"cache_blocks_sampled,omitempty"` // blks_hit+blks_read within the window — the ratio's denominator
	DeadlocksPerMin *float64         `json:"deadlocks_per_min,omitempty"`
	TempBytesPerSec *float64         `json:"temp_bytes_per_sec,omitempty"`
	TupReturnedPerS *float64         `json:"tuples_returned_per_sec,omitempty"`
	TupWrittenPerS  *float64         `json:"tuples_written_per_sec,omitempty"` // ins+upd+del
	Cockroach       *CockroachHealth `json:"cockroachdb,omitempty"`            // engine-native cluster health
}

// CockroachHealth is a cluster-wide point-in-time health snapshot assembled
// from the Admin API, the lightweight Prometheus load endpoint, and SQL jobs.
// Source sections make partial coverage explicit instead of turning a missing
// endpoint into a falsely clean report.
type CockroachHealth struct {
	AdminAPI     Section               `json:"admin_api"`
	Prometheus   Section               `json:"prometheus"`
	Jobs         Section               `json:"jobs"`
	HotRanges    Section               `json:"hot_ranges"`
	Distribution CockroachDistribution `json:"distribution"`
	Storage      CockroachStorage      `json:"storage_replication"`

	NodesTotal            int      `json:"nodes_total"`
	NodesLive             int      `json:"nodes_live"`
	NodesSuspect          int      `json:"nodes_suspect"`
	NodesDraining         int      `json:"nodes_draining"`
	NodesDecommissioned   int      `json:"nodes_decommissioned"`
	StoresTotal           int      `json:"stores_total"`
	RangeReplicas         int64    `json:"range_replicas"`
	UnavailableRanges     int64    `json:"unavailable_ranges"`
	UnderreplicatedRanges int64    `json:"underreplicated_ranges"`
	CapacityBytes         int64    `json:"capacity_bytes"`
	AvailableBytes        int64    `json:"available_bytes"`
	MaxStoreUsedRatio     float64  `json:"max_store_used_ratio"`
	MaxCPUPercent         float64  `json:"max_cpu_percent"`
	MaxMemoryUsedRatio    float64  `json:"max_memory_used_ratio"`
	SQLConnections        int      `json:"sql_connections"`
	QueriesPerSec         *float64 `json:"queries_per_sec,omitempty"`
	NewConnectionsPerSec  *float64 `json:"new_connections_per_sec,omitempty"`
	ServiceLatencyP99MS   float64  `json:"service_latency_p99_ms,omitempty"`
	AdmissionWaitP99MS    float64  `json:"admission_wait_p99_ms,omitempty"`
	AdmissionQueueMax     int64    `json:"admission_queue_max,omitempty"`

	Nodes       []CockroachNodeHealth  `json:"nodes,omitempty"`
	Stores      []CockroachStoreHealth `json:"stores,omitempty"`
	Hot         []CockroachHotRange    `json:"hottest_ranges,omitempty"`
	JobsTotal   int                    `json:"jobs_total"`
	JobsBounded bool                   `json:"jobs_bounded,omitempty"`
	JobItems    []CockroachJobHealth   `json:"job_items,omitempty"`
}

type CockroachNodeHealth struct {
	NodeID          int       `json:"node_id"`
	Status          string    `json:"status"`
	Locality        string    `json:"locality,omitempty"`
	Version         string    `json:"version,omitempty"`
	StartedAt       time.Time `json:"started_at,omitempty"`
	UpdatedAt       time.Time `json:"updated_at,omitempty"`
	CPUPercent      float64   `json:"cpu_percent,omitempty"`
	RSSBytes        int64     `json:"rss_bytes,omitempty"`
	MemoryBytes     int64     `json:"memory_bytes,omitempty"`
	MemoryUsedRatio float64   `json:"memory_used_ratio,omitempty"`
	SQLConnections  int       `json:"sql_connections"`
}

type CockroachStoreHealth struct {
	NodeID                int     `json:"node_id"`
	StoreID               int     `json:"store_id"`
	CapacityBytes         int64   `json:"capacity_bytes"`
	AvailableBytes        int64   `json:"available_bytes"`
	UsedRatio             float64 `json:"used_ratio"`
	RangeReplicas         int64   `json:"range_replicas"`
	Leaseholders          int64   `json:"leaseholders"`
	UnavailableRanges     int64   `json:"unavailable_ranges"`
	UnderreplicatedRanges int64   `json:"underreplicated_ranges"`
}

// CockroachStorage summarizes storage-engine and replication health across
// live stores. Counter fields are deltas over SampleSeconds; gauges and byte
// counts are point-in-time values from the second Admin API sample.
type CockroachStorage struct {
	Section
	LiveStores                  int                     `json:"live_stores"`
	MVCCMetricsAvailable        bool                    `json:"mvcc_metrics_available"`
	ReplicationMetricsAvailable bool                    `json:"replication_metrics_available"`
	CounterSampledStores        int                     `json:"counter_sampled_stores"`
	SampleSeconds               float64                 `json:"sample_seconds,omitempty"`
	FilesystemUsedBytes         int64                   `json:"filesystem_used_bytes"`
	CockroachUsedBytes          int64                   `json:"cockroach_used_bytes"`
	OtherUsedBytes              int64                   `json:"other_used_bytes"`
	MaxOtherUsedRatio           float64                 `json:"max_other_used_ratio"`
	MaxOtherUsedStoreID         int                     `json:"max_other_used_store_id,omitempty"`
	MVCCLiveBytes               int64                   `json:"mvcc_live_bytes"`
	MVCCTotalBytes              int64                   `json:"mvcc_total_bytes"`
	MVCCGarbageBytes            int64                   `json:"mvcc_garbage_bytes"`
	MVCCLiveRatio               float64                 `json:"mvcc_live_ratio"`
	BytesPerReplicaMin          float64                 `json:"bytes_per_replica_min"`
	BytesPerReplicaMean         float64                 `json:"bytes_per_replica_mean"`
	BytesPerReplicaMax          float64                 `json:"bytes_per_replica_max"`
	SmallestReplicaBytesStoreID int                     `json:"smallest_replica_bytes_store_id,omitempty"`
	LargestReplicaBytesStoreID  int                     `json:"largest_replica_bytes_store_id,omitempty"`
	RangeReplicas               int64                   `json:"range_replicas"`
	UninitializedReplicas       int64                   `json:"uninitialized_replicas"`
	ReservedReplicas            int64                   `json:"reserved_replicas"`
	OverreplicatedRanges        int64                   `json:"overreplicated_ranges"`
	DecommissioningRanges       int64                   `json:"decommissioning_ranges"`
	RaftCommandsPending         int64                   `json:"raft_commands_pending"`
	MaxRaftCommandsPending      int64                   `json:"max_raft_commands_pending"`
	MaxRaftPendingStoreID       int                     `json:"max_raft_pending_store_id,omitempty"`
	RaftProbeFlows              int64                   `json:"raft_probe_flows"`
	RaftSnapshotFlows           int64                   `json:"raft_snapshot_flows"`
	ReplicateQueuePending       int64                   `json:"replicate_queue_pending"`
	ReplicateQueuePurgatory     int64                   `json:"replicate_queue_purgatory"`
	RaftSnapshotQueuePending    int64                   `json:"raft_snapshot_queue_pending"`
	DiskSlowEvents              int64                   `json:"disk_slow_events"`
	DiskStalledEvents           int64                   `json:"disk_stalled_events"`
	DiskUnhealthySeconds        float64                 `json:"disk_unhealthy_seconds"`
	WriteStallEvents            int64                   `json:"write_stall_events"`
	WriteStallSeconds           float64                 `json:"write_stall_seconds"`
	RaftDroppedMessages         int64                   `json:"raft_dropped_messages"`
	Stores                      []CockroachStoreStorage `json:"stores,omitempty"`
}

type CockroachStoreStorage struct {
	NodeID                   int     `json:"node_id"`
	StoreID                  int     `json:"store_id"`
	Status                   string  `json:"status"`
	Locality                 string  `json:"locality,omitempty"`
	CapacityBytes            int64   `json:"capacity_bytes"`
	FilesystemUsedBytes      int64   `json:"filesystem_used_bytes"`
	CockroachUsedBytes       int64   `json:"cockroach_used_bytes"`
	OtherUsedBytes           int64   `json:"other_used_bytes"`
	OtherUsedRatio           float64 `json:"other_used_ratio"`
	MVCCLiveBytes            int64   `json:"mvcc_live_bytes"`
	MVCCTotalBytes           int64   `json:"mvcc_total_bytes"`
	MVCCGarbageBytes         int64   `json:"mvcc_garbage_bytes"`
	BytesPerReplica          float64 `json:"bytes_per_replica"`
	RangeReplicas            int64   `json:"range_replicas"`
	UninitializedReplicas    int64   `json:"uninitialized_replicas"`
	ReservedReplicas         int64   `json:"reserved_replicas"`
	OverreplicatedRanges     int64   `json:"overreplicated_ranges"`
	DecommissioningRanges    int64   `json:"decommissioning_ranges"`
	RaftCommandsPending      int64   `json:"raft_commands_pending"`
	RaftProbeFlows           int64   `json:"raft_probe_flows"`
	RaftSnapshotFlows        int64   `json:"raft_snapshot_flows"`
	ReplicateQueuePending    int64   `json:"replicate_queue_pending"`
	ReplicateQueuePurgatory  int64   `json:"replicate_queue_purgatory"`
	RaftSnapshotQueuePending int64   `json:"raft_snapshot_queue_pending"`
	DiskSlowEvents           int64   `json:"disk_slow_events"`
	DiskStalledEvents        int64   `json:"disk_stalled_events"`
	DiskUnhealthySeconds     float64 `json:"disk_unhealthy_seconds"`
	WriteStallEvents         int64   `json:"write_stall_events"`
	WriteStallSeconds        float64 `json:"write_stall_seconds"`
	RaftDroppedMessages      int64   `json:"raft_dropped_messages"`
}

// CockroachDistribution summarizes placement across live stores. Replica and
// lease comparisons use only stores whose capacities are within 25% of the
// live-store median, so a deliberately smaller store is not judged against a
// much larger peer. Zone constraints can still explain skew and are surfaced as
// a caveat by the findings layer.
type CockroachDistribution struct {
	Section
	LiveStores                 int                     `json:"live_stores"`
	ComparableStores           int                     `json:"comparable_stores"`
	ExcludedStores             int                     `json:"excluded_stores"`
	MultipleLocalities         bool                    `json:"multiple_localities,omitempty"`
	ReplicaMean                float64                 `json:"replica_mean"`
	ReplicaMin                 int64                   `json:"replica_min"`
	ReplicaMax                 int64                   `json:"replica_max"`
	ReplicaMinToMean           float64                 `json:"replica_min_to_mean"`
	ReplicaMaxToMean           float64                 `json:"replica_max_to_mean"`
	FewestReplicasStoreID      int                     `json:"fewest_replicas_store_id,omitempty"`
	MostReplicasStoreID        int                     `json:"most_replicas_store_id,omitempty"`
	LeaseMean                  float64                 `json:"lease_mean"`
	LeaseMin                   int64                   `json:"lease_min"`
	LeaseMax                   int64                   `json:"lease_max"`
	LeaseMinToMean             float64                 `json:"lease_min_to_mean"`
	LeaseMaxToMean             float64                 `json:"lease_max_to_mean"`
	FewestLeasesStoreID        int                     `json:"fewest_leases_store_id,omitempty"`
	MostLeasesStoreID          int                     `json:"most_leases_store_id,omitempty"`
	CapacityUsedMinRatio       float64                 `json:"capacity_used_min_ratio"`
	CapacityUsedMaxRatio       float64                 `json:"capacity_used_max_ratio"`
	CapacityUsedSpread         float64                 `json:"capacity_used_spread"`
	LeastUsedStoreID           int                     `json:"least_used_store_id,omitempty"`
	MostUsedStoreID            int                     `json:"most_used_store_id,omitempty"`
	HotRangeSampleCount        int                     `json:"hot_range_sample_count"`
	HotRangeLeaseholderSamples int                     `json:"hot_range_leaseholder_samples"`
	HotRangeCPUCores           float64                 `json:"hot_range_cpu_cores"`
	HottestLeaseholderNodeID   int                     `json:"hottest_leaseholder_node_id,omitempty"`
	HottestLeaseholderRanges   int                     `json:"hottest_leaseholder_ranges,omitempty"`
	HottestLeaseholderCPUCores float64                 `json:"hottest_leaseholder_cpu_cores,omitempty"`
	HottestLeaseholderCPUShare float64                 `json:"hottest_leaseholder_cpu_share,omitempty"`
	Stores                     []CockroachStoreBalance `json:"stores,omitempty"`
}

type CockroachStoreBalance struct {
	NodeID         int     `json:"node_id"`
	StoreID        int     `json:"store_id"`
	Status         string  `json:"status"`
	Locality       string  `json:"locality,omitempty"`
	Comparable     bool    `json:"comparable"`
	CapacityBytes  int64   `json:"capacity_bytes"`
	UsedRatio      float64 `json:"used_ratio"`
	RangeReplicas  int64   `json:"range_replicas"`
	Leaseholders   int64   `json:"leaseholders"`
	NodeCPUPercent float64 `json:"node_cpu_percent,omitempty"`
	TopHotRanges   int     `json:"top_hot_ranges,omitempty"`
	TopHotCPUCores float64 `json:"top_hot_cpu_cores,omitempty"`
}

type CockroachHotRange struct {
	RangeID           int64    `json:"range_id"`
	NodeID            int      `json:"node_id"`
	StoreID           int      `json:"store_id"`
	LeaseholderNodeID int      `json:"leaseholder_node_id"`
	QPS               float64  `json:"qps"`
	CPUCores          float64  `json:"cpu_cores"`
	ReadsPerSec       float64  `json:"reads_per_sec"`
	WritesPerSec      float64  `json:"writes_per_sec"`
	Databases         []string `json:"databases,omitempty"`
	Schema            string   `json:"schema,omitempty"`
	Tables            []string `json:"tables,omitempty"`
	Indexes           []string `json:"indexes,omitempty"`
}

type CockroachJobHealth struct {
	JobID         string     `json:"job_id"`
	Type          string     `json:"type"`
	State         string     `json:"state"`
	CreatedAt     time.Time  `json:"created_at"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
	Progress      float64    `json:"progress"`
	ProgressKnown bool       `json:"progress_known,omitempty"`
	Operation     string     `json:"operation,omitempty"`
	StatusMessage string     `json:"status_message,omitempty"`
	Error         string     `json:"error,omitempty"`
	LastUpdatedAt *time.Time `json:"last_updated_at,omitempty"`
	HighWaterAt   *time.Time `json:"high_water_at,omitempty"`
}

// CacheHitMinBlocks is the block traffic (blks_hit + blks_read, 8 KB each) a
// sample window must contain before its cache-hit ratio is a signal: 10,000
// blocks = 80 MB. Below that a few cold reads swing the ratio by tens of points
// between consecutive runs, flipping the finding, the health score and the exit
// code on noise (PR#1).
const CacheHitMinBlocks = 10_000

// CacheHitUsable reports whether the sampled cache-hit ratio rests on enough block
// traffic to be graded (see CacheHitMinBlocks). Callers that print or judge the
// ratio must check this, not just != nil. Nil-safe.
func (h *Health) CacheHitUsable() bool {
	return h != nil && h.CacheHitRatio != nil && h.CacheBlocks != nil && *h.CacheBlocks >= CacheHitMinBlocks
}

// Activity is a point-in-time read of pg_stat_activity.
type Activity struct {
	Section
	Total               int            `json:"total"`
	Active              int            `json:"active"`
	Idle                int            `json:"idle"`
	IdleInTransaction   int            `json:"idle_in_transaction"`
	Waiting             int            `json:"waiting"`
	ByState             map[string]int `json:"by_state"`
	WaitEvents          map[string]int `json:"wait_events,omitempty"`
	LongestXactSec      float64        `json:"longest_xact_sec"`
	LongestActiveSec    float64        `json:"longest_active_sec"`
	Connections         []ConnGroup    `json:"connections,omitempty"`            // top contributors by app/user/state (A13)
	AutovacuumWorkers   int            `json:"autovacuum_workers,omitempty"`     // running autovacuum workers (A19)
	AutovacuumMaxAgeSec float64        `json:"autovacuum_max_age_sec,omitempty"` // longest-running autovacuum worker
}

// ConnGroup is a group of connections by application, role, and state — the
// answer to "who is using the connections" when the pool is near saturation.
type ConnGroup struct {
	AppName string `json:"app_name"`
	User    string `json:"user"`
	State   string `json:"state"`
	Count   int    `json:"count"`
}

// Locks holds current blocking chains (scrubbed query text).
type Locks struct {
	Section
	BlockedCount int           `json:"blocked_count"`
	Chains       []BlockingRow `json:"chains,omitempty"`
}

type BlockingRow struct {
	BlockedPID   int     `json:"blocked_pid"`
	BlockingPIDs []int64 `json:"blocking_pids"`
	WaitEvent    string  `json:"wait_event,omitempty"`
	WaitSeconds  float64 `json:"wait_seconds"`
	BlockedQuery string  `json:"blocked_query"` // scrubbed
}

// Queries is the engine's top persisted statement statistics: cumulative since
// stats_reset on PostgreSQL, and aggregated over the last 24h on CockroachDB.
type Queries struct {
	Section
	Enabled     bool        `json:"enabled"`
	TotalExecMS float64     `json:"total_exec_ms,omitempty"` // exec time across all statements in the engine's statistics window
	PgssDealloc int64       `json:"pgss_dealloc,omitempty"`  // pg_stat_statements evictions (PG14+); >0 means the top list is a biased sample
	PgssCount   int         `json:"pgss_count,omitempty"`    // current entry count
	PgssMax     int         `json:"pgss_max,omitempty"`      // pg_stat_statements.max
	StatsSource string      `json:"stats_source,omitempty"`  // CockroachDB source: activity_cache or public_statistics
	WindowHours int         `json:"window_hours,omitempty"`  // CockroachDB persisted-statistics lookback
	Bounded     bool        `json:"bounded,omitempty"`       // source is a cached top set, not every fingerprint
	Top         []QueryStat `json:"top,omitempty"`
}

type QueryStat struct {
	QueryID      int64    `json:"queryid"`
	Fingerprint  string   `json:"fingerprint,omitempty"` // CockroachDB's stable statement fingerprint (hex)
	AppName      string   `json:"application_name,omitempty"`
	Query        string   `json:"query"` // normalized when the engine supports it; always scrubbed by the collector
	Calls        int64    `json:"calls"`
	TotalMS      float64  `json:"total_ms"`
	MeanMS       float64  `json:"mean_ms"`
	MaxMS        float64  `json:"max_ms"`
	P99MS        *float64 `json:"p99_ms,omitempty"`
	Rows         int64    `json:"rows"`
	RowsRead     int64    `json:"rows_read,omitempty"`
	RowsWritten  int64    `json:"rows_written,omitempty"`
	BytesRead    int64    `json:"bytes_read,omitempty"`
	ContentionMS float64  `json:"contention_ms,omitempty"`
	MaxRetries   int64    `json:"max_retries,omitempty"`
	CacheHit     *float64 `json:"cache_hit,omitempty"`
	WALBytes     int64    `json:"wal_bytes"`
}

// CockroachDB carries engine-native workload signals that do not have honest
// PostgreSQL equivalents. Statement aggregates still populate Queries above so
// baseline comparison and query-slowdown detection remain shared.
type CockroachDB struct {
	LiveQueries       CockroachLiveQueries       `json:"live_queries"`
	ExecutionInsights CockroachExecutionInsights `json:"execution_insights"`
	Contention        CockroachContention        `json:"contention"`
}

type CockroachLiveQueries struct {
	Section
	Items []CockroachLiveQuery `json:"items,omitempty"`
}

type CockroachLiveQuery struct {
	QueryID     string  `json:"query_id"`
	User        string  `json:"user"`
	AppName     string  `json:"application_name,omitempty"`
	Query       string  `json:"query"`
	AgeSec      float64 `json:"age_seconds"`
	Distributed bool    `json:"distributed"`
	FullScan    bool    `json:"full_scan"`
	Phase       string  `json:"phase,omitempty"`
	Isolation   string  `json:"isolation_level,omitempty"`
	Retries     int64   `json:"retries"`
	AutoRetries int64   `json:"auto_retries"`
}

type CockroachExecutionInsights struct {
	Section
	Items []CockroachInsight `json:"items,omitempty"`
}

type CockroachInsight struct {
	Kind                 string     `json:"kind"` // statement or transaction
	Fingerprint          string     `json:"fingerprint,omitempty"`
	Problem              string     `json:"problem"`
	Causes               []string   `json:"causes,omitempty"`
	Query                string     `json:"query,omitempty"`
	Status               string     `json:"status"`
	StartedAt            *time.Time `json:"started_at,omitempty"`
	EndedAt              *time.Time `json:"ended_at,omitempty"`
	FullScan             bool       `json:"full_scan,omitempty"`
	User                 string     `json:"user,omitempty"`
	AppName              string     `json:"application_name,omitempty"`
	Retries              int64      `json:"retries,omitempty"`
	LastRetryReason      string     `json:"last_retry_reason,omitempty"`
	IndexRecommendations []string   `json:"index_recommendations,omitempty"`
	RowsRead             int64      `json:"rows_read,omitempty"`
	RowsWritten          int64      `json:"rows_written,omitempty"`
	ContentionSec        float64    `json:"contention_seconds,omitempty"`
	ServiceLatencyMS     float64    `json:"service_latency_ms,omitempty"`
	AdmissionWaitMS      float64    `json:"admission_wait_ms,omitempty"`
	ErrorCode            string     `json:"error_code,omitempty"`
}

// CockroachContention is a bounded, key-redacted view of CockroachDB's
// cluster-wide in-memory contention event store. It intentionally excludes raw
// keys and transaction IDs, which may reveal row-level data and are ephemeral.
type CockroachContention struct {
	Section
	WindowMinutes          int                          `json:"window_minutes"`
	Bounded                bool                         `json:"bounded"`
	TotalEvents            int64                        `json:"total_events"`
	TotalWaitMS            float64                      `json:"total_wait_ms"`
	MaxWaitMS              float64                      `json:"max_wait_ms"`
	SerializationConflicts int64                        `json:"serialization_conflicts"`
	Hotspots               []CockroachContentionHotspot `json:"hotspots,omitempty"`
}

type CockroachContentionHotspot struct {
	Database                     string    `json:"database"`
	Schema                       string    `json:"schema"`
	Table                        string    `json:"table"`
	Index                        string    `json:"index,omitempty"`
	Type                         string    `json:"type"`
	WaitingStatementFingerprint  string    `json:"waiting_statement_fingerprint"`
	BlockingTxnFingerprint       string    `json:"blocking_transaction_fingerprint,omitempty"`
	BlockingStatementFingerprint string    `json:"blocking_statement_fingerprint,omitempty"`
	WaitingQuery                 string    `json:"waiting_query,omitempty"`
	BlockingQuery                string    `json:"blocking_query,omitempty"`
	BlockingQueries              []string  `json:"blocking_queries,omitempty"`
	WaitingApplications          []string  `json:"waiting_applications,omitempty"`
	BlockingApplications         []string  `json:"blocking_applications,omitempty"`
	WaiterResolution             string    `json:"waiter_resolution"`
	BlockerResolution            string    `json:"blocker_resolution"`
	Events                       int64     `json:"events"`
	TotalWaitMS                  float64   `json:"total_wait_ms"`
	MaxWaitMS                    float64   `json:"max_wait_ms"`
	LastSeen                     time.Time `json:"last_seen"`
}

// CockroachDB contention events do not always carry a blocker fingerprint.
// Keep that source limitation distinct from a fingerprint that simply aged
// out of persisted SQL statistics or from a failed attribution lookup.
const (
	CockroachContentionResolved         = "resolved"
	CockroachContentionNotResolved      = "not_resolved_by_cockroachdb"
	CockroachContentionNotFound         = "not_found_in_statistics"
	CockroachContentionStatsUnavailable = "statistics_unavailable"
)

// Tables is PostgreSQL pg_stat_user_tables or CockroachDB's cached Admin API
// table metadata.
type Tables struct {
	Section
	DBSizeBytes      int64             `json:"db_size_bytes"`
	Total            int               `json:"total,omitempty"`
	Scanned          int               `json:"scanned,omitempty"`
	StatsSource      string            `json:"stats_source,omitempty"`
	SizeKind         string            `json:"size_kind,omitempty"`
	MetadataBounded  bool              `json:"metadata_bounded"`
	MetadataOldestAt *time.Time        `json:"metadata_oldest_at,omitempty"`
	MetadataNewestAt *time.Time        `json:"metadata_newest_at,omitempty"`
	Top              []TableStat       `json:"top,omitempty"`         // by total size
	Partitioned      []PartitionRollup `json:"partitioned,omitempty"` // leaf partitions rolled up to their root (A10)
}

// PartitionRollup aggregates all leaf partitions of one partitioned table — so a
// parent scanned end-to-end is visible even when each partition looks harmless.
type PartitionRollup struct {
	Schema     string `json:"schema"`
	Name       string `json:"table"`
	Partitions int    `json:"partitions"`
	TotalBytes int64  `json:"total_bytes"`
	LiveTuples int64  `json:"live_tuples"`
	SeqScans   int64  `json:"seq_scans"`
	IndexScans int64  `json:"index_scans"`
}

type TableStat struct {
	Database         string     `json:"database,omitempty"`
	TableID          int64      `json:"table_id,omitempty"`
	Schema           string     `json:"schema"`
	Name             string     `json:"table"`
	TotalBytes       int64      `json:"total_bytes"`
	LiveTuples       int64      `json:"live_tuples"`
	DeadTuples       int64      `json:"dead_tuples"`
	DeadRatio        float64    `json:"dead_ratio"`
	SeqScans         int64      `json:"seq_scans"`
	IndexScans       int64      `json:"index_scans"`
	ModsSinceAnalyze int64      `json:"mods_since_analyze"`
	Updates          int64      `json:"updates,omitempty"`
	HotUpdates       int64      `json:"hot_updates,omitempty"`
	LastAnalyze      *time.Time `json:"last_analyze,omitempty"`
	LastAutoanalyze  *time.Time `json:"last_autoanalyze,omitempty"`
	// Per-table reloption overrides of the global analyze/vacuum knobs (nil = global).
	AnalyzeScaleOverride     *float64   `json:"analyze_scale_override,omitempty"`
	AnalyzeThresholdOverride *float64   `json:"analyze_threshold_override,omitempty"`
	AutovacuumCount          int64      `json:"autovacuum_count,omitempty"`
	AutovacuumDisabled       bool       `json:"autovacuum_disabled,omitempty"` // autovacuum_enabled=false in reloptions (A19)
	VacuumScaleOverride      *float64   `json:"vacuum_scale_override,omitempty"`
	VacuumThresholdOverride  *float64   `json:"vacuum_threshold_override,omitempty"`
	LastVacuum               *time.Time `json:"last_vacuum,omitempty"`
	LastAutovac              *time.Time `json:"last_autovacuum,omitempty"`
	ReplicatedBytes          int64      `json:"replicated_bytes,omitempty"`
	LiveDataBytes            int64      `json:"live_data_bytes,omitempty"`
	DataBytes                int64      `json:"data_bytes,omitempty"`
	LiveDataRatio            float64    `json:"live_data_ratio,omitempty"`
	RangeCount               int64      `json:"range_count,omitempty"`
	ReplicaCount             int64      `json:"replica_count,omitempty"`
	StoreIDs                 []int64    `json:"store_ids,omitempty"`
	ColumnCount              int64      `json:"column_count,omitempty"`
	IndexCount               int64      `json:"index_count,omitempty"`
	AutoStatsEnabled         bool       `json:"auto_stats_enabled,omitempty"`
	StatsLastUpdated         *time.Time `json:"stats_last_updated,omitempty"`
	MetadataLastUpdated      *time.Time `json:"metadata_last_updated,omitempty"`
	MetadataError            string     `json:"metadata_error,omitempty"`
	TopHotRangeCount         int        `json:"top_hot_range_count,omitempty"`
	TopHotRangeQPS           float64    `json:"top_hot_range_qps,omitempty"`
	TopHotRangeCPUCores      float64    `json:"top_hot_range_cpu_cores,omitempty"`
}

// Indexes is PostgreSQL pg_stat_user_indexes or CockroachDB's cluster-wide,
// in-memory index usage statistics.
type Indexes struct {
	Section
	Total                  int              `json:"total"`   // all user indexes
	Scanned                int              `json:"scanned"` // how many were examined for the bounded lists
	SecondaryTotal         int              `json:"secondary_total,omitempty"`
	StatsSource            string           `json:"stats_source,omitempty"`
	CountersDurable        bool             `json:"counters_durable"`
	WriteCountersAvailable bool             `json:"write_counters_available"`
	UnusedThresholdHours   int              `json:"unused_threshold_hours,omitempty"`
	Unused                 []IndexStat      `json:"unused,omitempty"`
	Usage                  []IndexStat      `json:"usage,omitempty"`
	MostWritten            []IndexStat      `json:"most_written,omitempty"`
	Largest                []IndexStat      `json:"largest,omitempty"`
	Redundant              []RedundantIndex `json:"redundant,omitempty"`
	UnindexedFKs           []UnindexedFK    `json:"unindexed_fks,omitempty"`
}

// UnindexedFK is a foreign key with no supporting index on the child table —
// every parent DELETE/UPDATE seq-scans the child to check references.
type UnindexedFK struct {
	Schema     string `json:"schema"`
	Table      string `json:"table"`
	Constraint string `json:"constraint"`
	Columns    string `json:"columns"`
	ChildBytes int64  `json:"child_bytes"`
}

// RedundantIndex is an index whose columns are a leading prefix of (or identical
// to) CoveredBy on the same table — usually safe to drop.
type RedundantIndex struct {
	Schema    string `json:"schema"`
	Table     string `json:"table"`
	Name      string `json:"index"`
	CoveredBy string `json:"covered_by"`
	Bytes     int64  `json:"bytes"`
}

type IndexStat struct {
	Database         string     `json:"database,omitempty"`
	Schema           string     `json:"schema"`
	Table            string     `json:"table"`
	Name             string     `json:"index"`
	Scans            int64      `json:"scans"`
	Bytes            int64      `json:"bytes"`
	Definition       string     `json:"definition,omitempty"`
	Columns          []string   `json:"columns,omitempty"`    // bare key columns (empty for a pure-expression index) — feeds code correlation
	Method           string     `json:"method,omitempty"`     // access method: btree, gin, gist, brin, hash, spgist
	Unique           bool       `json:"unique,omitempty"`     // enforces uniqueness
	Primary          bool       `json:"primary,omitempty"`    // the table's primary-key index
	Partial          bool       `json:"partial,omitempty"`    // has a WHERE predicate — may serve a narrow but critical path
	Expression       bool       `json:"expression,omitempty"` // indexes an expression, not bare columns
	Writes           int64      `json:"writes,omitempty"`
	LastRead         *time.Time `json:"last_read,omitempty"`
	LastWrite        *time.Time `json:"last_write,omitempty"`
	CreatedAt        *time.Time `json:"created_at,omitempty"`
	IndexType        string     `json:"index_type,omitempty"`
	Inverted         bool       `json:"inverted,omitempty"`
	Sharded          bool       `json:"sharded,omitempty"`
	Invisible        bool       `json:"invisible,omitempty"`
	UnusedForSeconds float64    `json:"unused_for_seconds,omitempty"`
}

// WAL is pg_stat_wal (PG14+), double-sampled.
type WAL struct {
	Section
	BytesPerSec   *float64 `json:"bytes_per_sec,omitempty"`
	RecordsPerSec *float64 `json:"records_per_sec,omitempty"`
	BuffersFull   int64    `json:"buffers_full"`
	DirBytes      *int64   `json:"dir_bytes,omitempty"` // pg_ls_waldir() total; nil if the call was denied (A14)
	DirFiles      int64    `json:"dir_files,omitempty"`
}

// IO summarizes pg_stat_io (PG16+) or the bgwriter/checkpointer fallback.
type IO struct {
	Section
	CheckpointsTimed   int64    `json:"checkpoints_timed"`
	CheckpointsReq     int64    `json:"checkpoints_requested"`
	BuffersWrittenPerS *float64 `json:"buffers_written_per_sec,omitempty"`
	BackendFsyncs      int64    `json:"backend_fsyncs"`
}

// Replication is pg_stat_replication (primary) / pg_stat_wal_receiver (replica).
type Replication struct {
	Section
	IsReplica      bool              `json:"is_replica"`
	Replicas       []ReplicaRow      `json:"replicas,omitempty"`
	ReceiverLagSec *float64          `json:"receiver_lag_sec,omitempty"`
	Slots          []ReplicationSlot `json:"slots,omitempty"`
	Subscriptions  []Subscription    `json:"subscriptions,omitempty"`
}

// StandbyStatus holds standby-only recovery-conflict counters (queries cancelled
// because recovery needed to apply a conflicting change). Cumulative.
type StandbyStatus struct {
	Section
	ConflTablespace int64 `json:"confl_tablespace"`
	ConflLock       int64 `json:"confl_lock"`
	ConflSnapshot   int64 `json:"confl_snapshot"`
	ConflBufferpin  int64 `json:"confl_bufferpin"`
	ConflDeadlock   int64 `json:"confl_deadlock"`
}

// Total is the sum of all recovery-conflict counters.
func (s StandbyStatus) Total() int64 {
	return s.ConflTablespace + s.ConflLock + s.ConflSnapshot + s.ConflBufferpin + s.ConflDeadlock
}

// Checksums lists data-checksum failures per database (cluster-wide). Any entry
// means Postgres read a page whose checksum didn't match — likely corruption.
type Checksums struct {
	Section
	Failures []ChecksumFailure `json:"failures,omitempty"`
}

type ChecksumFailure struct {
	Database    string     `json:"database"`
	Count       int64      `json:"count"`
	LastFailure *time.Time `json:"last_failure,omitempty"`
}

// Archiver is WAL archiving health from pg_stat_archiver. HasArchiveCommand is
// only whether archive_command/library is set — never the value (credentials).
type Archiver struct {
	Section
	ArchivedCount     int64      `json:"archived_count"`
	LastArchivedWAL   string     `json:"last_archived_wal,omitempty"`
	LastArchivedTime  *time.Time `json:"last_archived_time,omitempty"`
	FailedCount       int64      `json:"failed_count"`
	LastFailedWAL     string     `json:"last_failed_wal,omitempty"`
	LastFailedTime    *time.Time `json:"last_failed_time,omitempty"`
	StatsReset        *time.Time `json:"stats_reset,omitempty"`
	HasArchiveCommand bool       `json:"has_archive_command"`
}

// Progress lists in-flight maintenance operations from the pg_stat_progress_*
// views — live truth (a vacuum 60% through heap-scan), not inference.
type Progress struct {
	Section
	Operations []ProgressOp `json:"operations,omitempty"`
}

type ProgressOp struct {
	PID       int      `json:"pid"`
	Operation string   `json:"operation"` // vacuum | analyze | create_index | cluster | basebackup | copy
	Relation  string   `json:"relation,omitempty"`
	Phase     string   `json:"phase,omitempty"`
	Pct       *float64 `json:"pct,omitempty"` // 0..100 where the view exposes a total
}

// Sequences reports how close each sequence is to exhausting its effective
// ceiling (the lesser of its max_value and the owning column's integer range).
type Sequences struct {
	Section
	Items []SequenceUsage `json:"items,omitempty"`
	// NarrowIdentity is the structural (schema-scoped) half: int2/int4 columns
	// backed by a sequence, detectable regardless of current value (D3-0).
	NarrowIdentity []NarrowIdentityColumn `json:"narrow_identity,omitempty"`
}

// NarrowIdentityColumn is a sequence-backed column whose integer type will wrap
// well before a bigint would — int4 at 2.1B, int2 at 32767.
type NarrowIdentityColumn struct {
	Schema string `json:"schema"`
	Table  string `json:"table"`
	Column string `json:"column"`
	Type   string `json:"type"` // int2 | int4
}

type SequenceUsage struct {
	Schema    string  `json:"schema"`
	Name      string  `json:"sequence"`
	LastValue int64   `json:"last_value"`
	Ceiling   int64   `json:"ceiling"`
	PctUsed   float64 `json:"pct_used"`
	OwnedBy   string  `json:"owned_by,omitempty"`
}

// VacuumHorizon lists what pins the xmin horizon — the reason dead tuples aren't
// being reclaimed. Holders are ordered oldest-xmin first.
type VacuumHorizon struct {
	Section
	Holders []HorizonHolder `json:"holders,omitempty"`
}

// HorizonHolder is one thing holding back the vacuum horizon. Source is one of
// backend | replication_slot | standby_feedback | prepared_xact.
type HorizonHolder struct {
	Source  string  `json:"source"`
	Holder  string  `json:"holder"`          // pid / slot name / client / gid
	XminAge int64   `json:"xmin_age"`        // transactions behind the current xid
	AgeSec  float64 `json:"age_s,omitempty"` // wall-clock age where a timestamp exists (backend / prepared xact)
	Detail  string  `json:"detail,omitempty"`
}

// ReplicationSlot is one row of pg_replication_slots. An inactive slot holds
// back WAL removal from RetainedBytes' worth of log — the classic disk-fill.
type ReplicationSlot struct {
	Name          string `json:"name"`
	Type          string `json:"type"` // physical | logical
	Active        bool   `json:"active"`
	Database      string `json:"database,omitempty"`
	RetainedBytes int64  `json:"retained_bytes"`
	WALStatus     string `json:"wal_status,omitempty"` // reserved|extended|unreserved|lost (PG13+)
}

// Subscription is subscriber-side logical-replication health from
// pg_stat_subscription. WorkerRunning=false means changes aren't being applied.
type Subscription struct {
	Name          string  `json:"name"`
	WorkerRunning bool    `json:"worker_running"`
	LastMsgAgeSec float64 `json:"last_msg_age_sec,omitempty"`
}

type ReplicaRow struct {
	ClientAddr   string   `json:"client_addr"`
	AppName      string   `json:"application_name,omitempty"`
	State        string   `json:"state"`
	SyncState    string   `json:"sync_state"`
	SyncPriority int      `json:"sync_priority,omitempty"`
	ReplayLagSec *float64 `json:"replay_lag_sec,omitempty"` // seconds of writes lost if promoted now; null on an idle primary
	WriteLagB    int64    `json:"write_lag_bytes"`
	FlushLagB    int64    `json:"flush_lag_bytes"`
	ReplayLagB   int64    `json:"replay_lag_bytes"`
}

// Settings is non-default pg_settings plus sizes worth surfacing.
type Settings struct {
	Section
	Overrides map[string]string `json:"overrides"`        // name -> current value where != boot_val
	Params    map[string]string `json:"params,omitempty"` // tuning-relevant params (display values), always present
}

// SafetyGuard is one machine-actionable guard against a destructive action. An
// agent about to run Action matches it directly; ID is the stable key tests and
// consumers branch on (never the Text — wording can improve without breaking a
// consumer). Kind separates a prohibition (never do this while the state holds;
// Verify is nil) from a precondition (permitted only after the Verify check passes).
type SafetyGuard struct {
	ID     string  `json:"id"`
	Kind   string  `json:"kind"`   // GuardProhibition | GuardPrecondition
	Action string  `json:"action"` // one of the Action* constants below
	Text   string  `json:"text"`   // the sentence a human reads — never the source of truth for machines
	Verify *string `json:"verify"` // what clears a precondition; nil for a prohibition
}

// Safety is the structured, guaranteed carrier for a finding's destructive-action
// guards — deterministic, never model-generated, present in --json / SARIF / MCP
// and asserted by model-free tests. A non-empty BlockingCaveats IS the "this
// finding guards a destructive action" signal; there is no separate boolean.
type Safety struct {
	BlockingCaveats []SafetyGuard `json:"blocking_caveats"`
}

// Guard kinds.
const (
	GuardProhibition  = "prohibition"  // never do this while the state holds
	GuardPrecondition = "precondition" // permitted only after Verify passes
)

// Destructive-action vocabulary — the Action an agent matches a guard on. Defined
// in one place so a consumer and pgbot agree on the exact strings.
const (
	ActionDropIndex           = "DROP INDEX"
	ActionVacuumFull          = "VACUUM FULL"
	ActionReindex             = "REINDEX"
	ActionDropReplicationSlot = "DROP REPLICATION SLOT"
	ActionAlterColumnType     = "ALTER TABLE ... TYPE"
)

// Finding is deterministic, rule-based analysis computed in Go — never by the
// LLM. The model layer explains and prioritises Findings; it does not create them.
type Finding struct {
	ID string `json:"id"` // stable slug, e.g. "unused_indexes"
	// Object is a STABLE, human-writable identity for suppression keying (B2-0):
	// "public.issues" (relation/index), "q:<queryid>", "slot:<name>",
	// "sub:<name>", "setting:<name>", "db:<name>", or "" for cluster-scoped.
	// NEVER an ephemeral id (pid, LSN, lock address) — that would silence a
	// different session tomorrow, so PID-scoped findings are cluster-scoped.
	Object   string   `json:"object,omitempty"`
	Severity string   `json:"severity"`
	Title    string   `json:"title"`
	Detail   string   `json:"detail"`
	Evidence []string `json:"evidence,omitempty"`
	// Objects is the per-Evidence-row stable object identity for an AGGREGATE
	// finding (one finding listing many objects), aligned index-for-index with
	// Evidence. It lets an object-scoped [[ignore]] rule drop just the matching
	// rows and keep the finding on the rest (B2 per-object suppression for
	// aggregates). Empty for single-object findings (their top-level Object covers
	// them) and for aggregates whose rows have no stable name (PIDs, wait events).
	Objects     []string `json:"objects,omitempty"`
	Remediation string   `json:"remediation,omitempty"` // the actionable "what to do", human-facing
	// Impact ranks findings against each other; Confidence says how sure we are;
	// Caveats are the load-bearing "but…" clauses that render inline, never in a
	// footnote — the ones that stop pgbot from confidently recommending an outage.
	Impact     Impact   `json:"impact"`
	Confidence float64  `json:"confidence"` // 0.0–1.0; below 0.5 renders as "possible", not an assertion
	Caveats    []string `json:"caveats,omitempty"`
	Related    []string `json:"related,omitempty"` // ids of findings that travel with this one (rendered adjacently)
	// Safety carries the DETERMINISTIC, machine-actionable guards against a
	// destructive or irreversible action (DROP INDEX, VACUUM FULL, dropping a slot,
	// a table rewrite). Unlike Caveats (free-form context a summarizing model may
	// reword away), these are guaranteed present in --json, SARIF, and the MCP
	// payloads, and asserted by model-free tests keyed on guard ID. Text rendering
	// is a view over this field, never its origin. nil when the finding guards no
	// destructive action.
	Safety *Safety `json:"safety,omitempty"`
	// Suppression state, set by a matching [[ignore]] rule (B2-2). A suppressed
	// finding is never DELETED — it stays in --json (so an agent can explain why
	// it isn't reporting it) and does not affect the exit code. A suppressed
	// CRITICAL still renders in the report (visibly marked); lesser severities
	// drop to a footer/--full section.
	Suppressed        bool   `json:"suppressed,omitempty"`
	SuppressionReason string `json:"suppression_reason,omitempty"`
	SuppressionRule   string `json:"suppression_rule,omitempty"` // which rule matched, e.g. `checksums_disabled object=*`
	// SeverityRemapped, when non-empty, is the original severity before a
	// [severity] override changed it — so the change is auditable in --json.
	SeverityRemapped string `json:"severity_remapped,omitempty"`
	// ClusterScoped marks a finding that comes from a cluster-wide source (settings,
	// replication, archiving, wraparound, cluster activity) rather than the
	// connected database. In an --all-databases run it is reported once, on the
	// first database, and omitted from the rest (B3) — this flag says so.
	ClusterScoped bool `json:"cluster_scoped,omitempty"`
	// Preexisting is set by --fail-on-new (D3-2): this finding (or all of an
	// aggregate's rows) was already present in the base report, so it is NOT a
	// regression this change introduced. Preexisting findings stay in --json (so
	// nothing is hidden) but are excluded from SARIF and the exit code, and not
	// annotated — only what the change newly introduced is acted on.
	Preexisting bool `json:"preexisting,omitempty"`
}

// Impact is why a finding matters and how much, in ONE dimension. Two findings
// in different dimensions (43 GB of storage vs 2.1s/min of latency) are not
// commensurable, so the renderer sorts within a dimension and labels which one.
type Impact struct {
	Score     float64 `json:"score"`     // 0–100 sort key WITHIN a dimension
	Dimension string  `json:"dimension"` // latency | throughput | storage | risk | cost
	Estimate  string  `json:"estimate"`  // magnitude, e.g. "≈43 GB", "≈2.1s/min of query time"
	Basis     string  `json:"basis"`     // how it was derived — must be human-checkable
}

// Impact dimensions.
const (
	DimLatency    = "latency"
	DimThroughput = "throughput"
	DimStorage    = "storage"
	DimRisk       = "risk" // time-to-incident; pinned to the top of the report
	DimCost       = "cost"
)

// Deltas is the temporal differentiator: what changed vs the baseline store.
type Deltas struct {
	Against       time.Time  `json:"against"` // timestamp of the baseline compared to
	YesterdayHour *time.Time `json:"yesterday_hour,omitempty"`
	Changes       []Delta    `json:"changes"`
}

type Delta struct {
	ID            string     `json:"id"`      // e.g. "query.mean_ms", "table.seq_scans"
	Subject       string     `json:"subject"` // object name / queryid
	Severity      string     `json:"severity"`
	Before        float64    `json:"before"`
	After         float64    `json:"after"`
	PctChange     *float64   `json:"pct_change,omitempty"` // nil when Before == 0 (new)
	FirstObserved *time.Time `json:"first_observed,omitempty"`
	Note          string     `json:"note,omitempty"`
}
