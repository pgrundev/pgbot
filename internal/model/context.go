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
	SchemaVersion string         `json:"schema_version"`
	CollectedAt   time.Time      `json:"collected_at"`
	Fingerprint   string         `json:"fingerprint"` // stable per target database (see store)
	Server        ServerInfo     `json:"server"`
	Window        Window         `json:"window"`
	Health        *Health        `json:"health,omitempty"`
	Activity      *Activity      `json:"activity,omitempty"`
	Locks         *Locks         `json:"locks,omitempty"`
	Queries       *Queries       `json:"queries,omitempty"`
	Tables        *Tables        `json:"tables,omitempty"`
	Indexes       *Indexes       `json:"indexes,omitempty"`
	WAL           *WAL           `json:"wal,omitempty"`
	IO            *IO            `json:"io,omitempty"`
	Replication   *Replication   `json:"replication,omitempty"`
	Settings      *Settings      `json:"settings,omitempty"`
	Limits        *Limits        `json:"limits,omitempty"`
	Horizon       *VacuumHorizon `json:"horizon,omitempty"`   // what pins the xmin/vacuum horizon (A1)
	Sequences     *Sequences     `json:"sequences,omitempty"` // sequence exhaustion headroom (A8)
	Progress      *Progress      `json:"progress,omitempty"`  // in-flight pg_stat_progress_* operations (A11)
	Archiver      *Archiver      `json:"archiver,omitempty"`  // WAL archiving health (A15)
	Checksums     *Checksums     `json:"checksums,omitempty"` // data-checksum failures cluster-wide (A16)
	Standby       *StandbyStatus `json:"standby,omitempty"`   // standby-side recovery conflicts (A17)
	Deltas        *Deltas        `json:"deltas,omitempty"`    // vs baseline; nil on first run
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

// WaitSamplerDisabledReason is the Reason set when the operator turned the
// sampler off (--ash-hz 0) — the one unavailable state the report need not
// explain, as opposed to a sampler that ran and failed.
const WaitSamplerDisabledReason = "sampler disabled (--ash-hz 0)"

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
	VersionNum    int        `json:"version_num"`
	VersionText   string     `json:"version_text"`
	Database      string     `json:"database"`
	Provider      string     `json:"provider,omitempty"`    // detected managed platform: rds/aurora/cloudsql/azure/supabase/neon/unknown
	InRecovery    bool       `json:"in_recovery,omitempty"` // true on a physical standby (A15-0)
	ViaPooler     bool       `json:"via_pooler,omitempty"`  // connected through a transaction pooler (rates still correct)
	StartedAt     *time.Time `json:"started_at,omitempty"`  // pg_postmaster_start_time()
	UptimeSeconds int64      `json:"uptime_seconds"`
	Extensions    []string   `json:"extensions"`
	Capabilities  []string   `json:"capabilities"` // human-readable flags that were satisfied
	HasPgMonitor  bool       `json:"has_pg_monitor"`
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
	Connections     int      `json:"connections"`
	TPS             *float64 `json:"tps,omitempty"` // commits+rollbacks per second
	CommitsPerSec   *float64 `json:"commits_per_sec,omitempty"`
	RollbacksPerSec *float64 `json:"rollbacks_per_sec,omitempty"`
	RollbackRatio   *float64 `json:"rollback_ratio,omitempty"`  // over the sample window
	CacheHitRatio   *float64 `json:"cache_hit_ratio,omitempty"` // 0..1 over the sample window
	DeadlocksPerMin *float64 `json:"deadlocks_per_min,omitempty"`
	TempBytesPerSec *float64 `json:"temp_bytes_per_sec,omitempty"`
	TupReturnedPerS *float64 `json:"tuples_returned_per_sec,omitempty"`
	TupWrittenPerS  *float64 `json:"tuples_written_per_sec,omitempty"` // ins+upd+del
}

// Activity is a point-in-time read of pg_stat_activity.
type Activity struct {
	Section
	Total               int            `json:"total"`
	Active              int            `json:"active"`
	Idle                int            `json:"idle"`
	IdleInTransaction   int            `json:"idle_in_transaction"`
	Waiting             int            `json:"waiting"` // blocked inside the server (any wait_event_type except Client)
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

// Queries is the top of pg_stat_statements — cumulative since stats_reset; the
// temporal view comes from Deltas, not from the short in-process sample.
type Queries struct {
	Section
	Enabled     bool        `json:"enabled"`
	TotalExecMS float64     `json:"total_exec_ms,omitempty"` // cumulative exec time across ALL statements (prop_exec_time denominator)
	PgssDealloc int64       `json:"pgss_dealloc,omitempty"`  // pg_stat_statements evictions (PG14+); >0 means the top list is a biased sample
	PgssCount   int         `json:"pgss_count,omitempty"`    // current entry count
	PgssMax     int         `json:"pgss_max,omitempty"`      // pg_stat_statements.max
	Top         []QueryStat `json:"top,omitempty"`
}

type QueryStat struct {
	QueryID  int64    `json:"queryid"`
	Query    string   `json:"query"` // normalized DML ($1); utility statements are stored verbatim by pgss, so the collector scrubs this
	Calls    int64    `json:"calls"`
	TotalMS  float64  `json:"total_ms"`
	MeanMS   float64  `json:"mean_ms"`
	MaxMS    float64  `json:"max_ms"`
	Rows     int64    `json:"rows"`
	CacheHit *float64 `json:"cache_hit,omitempty"`
	WALBytes int64    `json:"wal_bytes"`
}

// Tables is pg_stat_user_tables (cumulative counters + gauges).
type Tables struct {
	Section
	DBSizeBytes int64             `json:"db_size_bytes"`
	Top         []TableStat       `json:"top,omitempty"`         // by total size
	Partitioned []PartitionRollup `json:"partitioned,omitempty"` // leaf partitions rolled up to their root (A10)
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
}

// Indexes is pg_stat_user_indexes.
type Indexes struct {
	Section
	Total        int              `json:"total"`
	Unused       []IndexStat      `json:"unused,omitempty"`
	Largest      []IndexStat      `json:"largest,omitempty"`
	Redundant    []RedundantIndex `json:"redundant,omitempty"`
	UnindexedFKs []UnindexedFK    `json:"unindexed_fks,omitempty"`
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
	Schema     string `json:"schema"`
	Table      string `json:"table"`
	Name       string `json:"index"`
	Scans      int64  `json:"scans"`
	Bytes      int64  `json:"bytes"`
	Definition string `json:"definition,omitempty"`
	Partial    bool   `json:"partial,omitempty"`    // has a WHERE predicate — may serve a narrow but critical path
	Expression bool   `json:"expression,omitempty"` // indexes an expression, not bare columns
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

// Finding is deterministic, rule-based analysis computed in Go — never by the
// LLM. The model layer explains and prioritises Findings; it does not create them.
type Finding struct {
	ID string `json:"id"` // stable slug, e.g. "unused_indexes"
	// Object is a STABLE, human-writable identity for suppression keying (B2-0):
	// "public.issues" (relation/index), "q:<queryid>", "slot:<name>",
	// "sub:<name>", "setting:<name>", "db:<name>", or "" for cluster-scoped.
	// NEVER an ephemeral id (pid, LSN, lock address) — that would silence a
	// different session tomorrow, so PID-scoped findings are cluster-scoped.
	Object      string   `json:"object,omitempty"`
	Severity    string   `json:"severity"`
	Title       string   `json:"title"`
	Detail      string   `json:"detail"`
	Evidence    []string `json:"evidence,omitempty"`
	Remediation string   `json:"remediation,omitempty"` // the actionable "what to do", human-facing
	// Impact ranks findings against each other; Confidence says how sure we are;
	// Caveats are the load-bearing "but…" clauses that render inline, never in a
	// footnote — the ones that stop pgbot from confidently recommending an outage.
	Impact     Impact   `json:"impact"`
	Confidence float64  `json:"confidence"` // 0.0–1.0; below 0.5 renders as "possible", not an assertion
	Caveats    []string `json:"caveats,omitempty"`
	Related    []string `json:"related,omitempty"` // ids of findings that travel with this one (rendered adjacently)
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
