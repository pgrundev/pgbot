package collect

import (
	"regexp"
	"strings"
	"testing"

	"github.com/pgrundev/pgbot/internal/conn"
)

// TestPgStatActivityReadsExcludeSelf pins the self-observation invariant: every
// read of pg_stat_activity that feeds a user-facing count (connections,
// idle-in-transaction, xmin holders, the wait profile) must exclude ALL of
// pgbot's own sessions by application_name — not just the backend running the
// query. pgbot opens a pool plus a sampler connection, and each collector's
// `BEGIN READ ONLY … COMMIT` leaves those sessions `idle in transaction`
// between round trips; a `pid <> pg_backend_pid()` filter hides one of them and
// counts the rest as the database's activity.
func TestPgStatActivityReadsExcludeSelf(t *testing.T) {
	filter := "application_name IS DISTINCT FROM '" + conn.AppName + "'"
	for name, sql := range map[string]string{
		"activity.sql":       sqlActivity,
		"conn_breakdown.sql": sqlConnBreakdown,
		"horizon.sql":        sqlHorizon,
		"limits.sql":         sqlLimits,
		"ash":                ashSQL(conn.Capabilities{VersionNum: 140000}),
	} {
		if !strings.Contains(sql, "pg_stat_activity") {
			t.Errorf("%s: expected a pg_stat_activity read (test is stale)", name)
			continue
		}
		if !strings.Contains(sql, filter) {
			t.Errorf("%s reads pg_stat_activity without excluding pgbot's own sessions (%q)", name, filter)
		}
	}
	// max_connections limits client backends only; counting every backend_type
	// (autovacuum, checkpointer, io workers, …) overstates saturation.
	if !regexp.MustCompile(`(?s)count\(\*\)\s+FROM pg_stat_activity\s+WHERE backend_type = 'client backend'`).MatchString(sqlLimits) {
		t.Errorf("limits.sql conn_used must count client backends only")
	}
}
