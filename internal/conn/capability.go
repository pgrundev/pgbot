package conn

import (
	"time"

	"github.com/jackc/pgx/v5"
)

// Capabilities is built once at connect from server_version_num + a probe of
// installed extensions and the current role. Collectors declare what they need
// and are SKIPPED (not failed) when a capability is missing — a section is
// marked unavailable rather than blanking the whole run.
type Capabilities struct {
	Engine                Engine
	VersionNum            int
	VersionText           string
	Database              string
	Provider              Provider  // detected managed platform, for provider-specific fixes
	StartedAt             time.Time // pg_postmaster_start_time(); zero if not readable
	SystemIdentifier      string    // pg_control_system(); "" if not readable
	HasStatStatements     bool
	HasHypopg             bool
	HasPgMonitor          bool
	HasViewActivity       bool // CockroachDB VIEWACTIVITY or VIEWACTIVITYREDACTED
	HasCRDBStmtStats      bool // information_schema.crdb_statement_statistics
	HasCRDBStmtActivity   bool // bounded crdb_internal.statement_activity cache (requires allow_unsafe_internals)
	HasCRDBInsights       bool // persisted statement/transaction execution insights
	HasCRDBJobs           bool // information_schema.crdb_jobs_with_progress
	HasCRDBContention     bool // crdb_internal.transaction_contention_events
	HasCRDBStatementStore bool // system.statements (query-text store, CockroachDB 26.2+)
	HasCRDBIndexUsage     bool // cluster-wide crdb_internal.index_usage_statistics + descriptor metadata
	HasCRDBIndexWrites    bool // newer index-usage schema also exposes write counters
	InRecovery            bool // pg_is_in_recovery(): true on a standby (A15-0)
	RecoveryChecked       bool // the probe succeeded, so InRecovery is trustworthy (distinguishes "false" from "unknown")
	Extensions            []string
	// ExtensionSchemas maps an installed extension to the namespace its objects
	// live in (pg_extension.extnamespace). Supabase installs pg_stat_statements
	// (and hypopg) in "extensions", not public, and a dedicated read-only role
	// rarely has that on its search_path — so pgbot must address extension
	// objects by qualified name, never rely on resolution. Nil/absent → unknown.
	ExtensionSchemas map[string]string
}

// ExtensionSchema returns the namespace an installed extension's objects live
// in, or "" when the extension is absent or the probe could not read it.
func (c Capabilities) ExtensionSchema(ext string) string { return c.ExtensionSchemas[ext] }

// ExtObject returns the schema-qualified, identifier-quoted name of a fixed,
// allowlisted object (view / function / table) belonging to an extension —
// e.g. "extensions"."pg_stat_statements" — so a read works whatever the
// session's search_path is. The schema comes from the catalog, never user input,
// but it is quoted anyway (pgx.Identifier) so an unusual namespace name can't
// break the statement. When the schema is unknown it degrades to the bare name,
// i.e. exactly the pre-discovery behaviour.
func (c Capabilities) ExtObject(ext, object string) string {
	schema := c.ExtensionSchema(ext)
	if schema == "" {
		return object
	}
	return pgx.Identifier{schema, object}.Sanitize()
}

// Pgss is ExtObject for pg_stat_statements' objects: the view, the
// pg_stat_statements(showtext) set-returning function, and pg_stat_statements_info.
func (c Capabilities) Pgss(object string) string { return c.ExtObject("pg_stat_statements", object) }

// Standby reports whether this node is a physical standby (in recovery). Only
// meaningful when RecoveryChecked; when unknown, returns false so primary-side
// collectors don't wrongly suppress themselves.
func (c Capabilities) Standby() bool { return c.RecoveryChecked && c.InRecovery }

// Primary reports whether this node is a primary (not in recovery), known for
// certain. Unknown recovery state returns false — a standby-only collector
// stays off rather than firing on an unverified node.
func (c Capabilities) Primary() bool { return c.RecoveryChecked && !c.InRecovery }

// ManagedProvider reports whether a managed platform was detected — used to
// downgrade findings (e.g. archiving) whose mechanism the provider hides.
func (c Capabilities) ManagedProvider() bool {
	switch c.Provider {
	case ProviderRDS, ProviderAurora, ProviderCloudSQL, ProviderAzure, ProviderSupabase, ProviderNeon:
		return true
	}
	return false
}

// HasStatWAL and the flags below are version-derived feature gates, kept as
// methods so the thresholds live in one place and read like the docs.
func (c Capabilities) HasStatWAL() bool          { return !c.IsCockroachDB() && c.VersionNum >= 140000 } // pg_stat_wal
func (c Capabilities) HasStatIO() bool           { return !c.IsCockroachDB() && c.VersionNum >= 160000 } // pg_stat_io
func (c Capabilities) HasStatCheckpointer() bool { return !c.IsCockroachDB() && c.VersionNum >= 170000 } // pg_stat_checkpointer
func (c Capabilities) HasStatsFetchConsistency() bool {
	return !c.IsCockroachDB() && c.VersionNum >= 150000
}
func (c Capabilities) StatStatementsTotalCol() string { // renamed in PG13
	if c.VersionNum >= 130000 {
		return "total_exec_time"
	}
	return "total_time"
}

// Satisfied returns the human-readable capability flags that hold, for
// ServerInfo.Capabilities (so a consumer can see what was and wasn't collected).
func (c Capabilities) Satisfied() []string {
	var out []string
	add := func(cond bool, name string) {
		if cond {
			out = append(out, name)
		}
	}
	add(c.HasPgMonitor, "pg_monitor")
	add(c.HasViewActivity, "cockroachdb_view_activity")
	add(c.HasCRDBStmtStats, "cockroachdb_statement_statistics")
	add(c.HasCRDBStmtActivity, "cockroachdb_statement_activity_cache")
	add(c.HasCRDBInsights, "cockroachdb_execution_insights")
	add(c.HasCRDBJobs, "cockroachdb_jobs")
	add(c.HasCRDBContention, "cockroachdb_contention_events")
	add(c.HasCRDBStatementStore, "cockroachdb_statement_store")
	add(c.HasCRDBIndexUsage, "cockroachdb_index_usage")
	add(c.HasCRDBIndexWrites, "cockroachdb_index_write_statistics")
	add(c.HasStatStatements, "pg_stat_statements")
	add(c.HasHypopg, "hypopg")
	add(c.HasStatWAL(), "pg_stat_wal")
	add(c.HasStatIO(), "pg_stat_io")
	add(c.HasStatCheckpointer(), "pg_stat_checkpointer")
	add(c.HasStatsFetchConsistency(), "stats_fetch_consistency")
	return out
}
