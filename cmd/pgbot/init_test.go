package main

import (
	"strings"
	"testing"

	"github.com/pgrundev/pgbot/internal/conn"
)

// `pgbot init` GENERATES setup SQL; it never executes it. The output contract:
// pipeable straight into psql (every line is a statement, a -- comment, or
// blank), and it mirrors the canonical Setup section of the README — role,
// pg_monitor, CONNECT grant.

func TestInitSQLDefaults(t *testing.T) {
	sql := initSQL("pgbot_ro", "yourdb", conn.ProviderUnknown)

	for _, want := range []string{
		"CREATE ROLE pgbot_ro LOGIN PASSWORD",
		"GRANT pg_monitor TO pgbot_ro;",
		"GRANT CONNECT ON DATABASE yourdb TO pgbot_ro;",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("init SQL missing %q\n---\n%s", want, sql)
		}
	}

	// No real password is ever generated or printed — a placeholder the user
	// must replace, called out in a comment.
	if !strings.Contains(sql, "REPLACE-WITH-A-STRONG-PASSWORD") {
		t.Errorf("init SQL should carry an explicit password placeholder\n---\n%s", sql)
	}

	// The reward at the end: the DATABASE_URL the user will export, as a
	// comment so the output stays psql-safe.
	if !strings.Contains(sql, "-- export DATABASE_URL=") {
		t.Errorf("init SQL should end with the ready-to-use DATABASE_URL hint\n---\n%s", sql)
	}
}

func TestInitSQLCustomRoleAndDatabase(t *testing.T) {
	sql := initSQL("metrics_ro", "orders_prod", conn.ProviderUnknown)
	if !strings.Contains(sql, "GRANT pg_monitor TO metrics_ro;") {
		t.Errorf("custom role not honored\n---\n%s", sql)
	}
	if !strings.Contains(sql, "GRANT CONNECT ON DATABASE orders_prod TO metrics_ro;") {
		t.Errorf("custom database not honored\n---\n%s", sql)
	}
}

func TestInitSQLProviderPgss(t *testing.T) {
	// Supabase/Neon preload pg_stat_statements, so CREATE EXTENSION is safe to
	// run as-is: it must appear as an executable statement, not a comment.
	sql := initSQL("pgbot_ro", "yourdb", conn.ProviderSupabase)
	if !hasExecutableLine(sql, "CREATE EXTENSION IF NOT EXISTS pg_stat_statements;") {
		t.Errorf("supabase: CREATE EXTENSION should be executable\n---\n%s", sql)
	}

	// On RDS the extension needs shared_preload_libraries first — emitting a
	// bare CREATE EXTENSION would just error in the pipe. Instructions must be
	// present but commented.
	sql = initSQL("pgbot_ro", "yourdb", conn.ProviderRDS)
	if hasExecutableLine(sql, "CREATE EXTENSION IF NOT EXISTS pg_stat_statements;") {
		t.Errorf("rds: CREATE EXTENSION must not be executable before preload\n---\n%s", sql)
	}
	if !strings.Contains(sql, "parameter group") {
		t.Errorf("rds: provider-specific preload instructions missing\n---\n%s", sql)
	}
}

func TestInitSQLCockroachDB(t *testing.T) {
	sql := initSQLForEngine("pgbot_ro", "defaultdb", conn.ProviderUnknown, conn.EngineCockroachDB)
	for _, want := range []string{
		"GRANT CONNECT ON DATABASE defaultdb TO pgbot_ro;",
		"GRANT SYSTEM VIEWACTIVITY TO pgbot_ro;",
		"@HOST:26257/defaultdb",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("CockroachDB init SQL missing %q\n---\n%s", want, sql)
		}
	}
	if strings.Contains(sql, "pg_monitor") || strings.Contains(sql, "pg_stat_statements") {
		t.Errorf("CockroachDB init SQL must not contain PostgreSQL-only setup\n---\n%s", sql)
	}
}

// The whole point of generate-don't-execute: `pgbot init | psql "$ADMIN_DSN"`
// must be safe. Every line is blank, a comment, or one of the known statements.
func TestInitSQLPipeSafe(t *testing.T) {
	for _, p := range []conn.Provider{conn.ProviderUnknown, conn.ProviderSupabase, conn.ProviderRDS, conn.ProviderNeon, conn.ProviderAzure} {
		sql := initSQL("pgbot_ro", "yourdb", p)
		for _, line := range strings.Split(sql, "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "--") {
				continue
			}
			ok := false
			for _, prefix := range []string{"CREATE ROLE ", "GRANT ", "CREATE EXTENSION "} {
				if strings.HasPrefix(trimmed, prefix) {
					ok = true
					break
				}
			}
			if !ok {
				t.Errorf("provider %s: non-comment line is not a known statement: %q", p, line)
			}
		}
	}
}

func hasExecutableLine(sql, stmt string) bool {
	for _, line := range strings.Split(sql, "\n") {
		if strings.TrimSpace(line) == stmt {
			return true
		}
	}
	return false
}

// --verify: a checklist computed from the connect-time probe. pg_monitor is
// the critical prerequisite; pg_stat_statements is a warning with the
// provider's own fix; a standby gets the per-node caveat note.

func TestInitVerifyReportAllGood(t *testing.T) {
	lines, critical := initVerifyReport(conn.Capabilities{
		VersionText:       "PostgreSQL 17.4",
		Database:          "yourdb",
		HasPgMonitor:      true,
		HasStatStatements: true,
		RecoveryChecked:   true,
	})
	if critical {
		t.Errorf("healthy caps flagged critical:\n%s", strings.Join(lines, "\n"))
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "pg_monitor") {
		t.Errorf("verify report should name pg_monitor:\n%s", joined)
	}
}

func TestInitVerifyReportMissingPgMonitor(t *testing.T) {
	lines, critical := initVerifyReport(conn.Capabilities{
		Database:        "yourdb",
		HasPgMonitor:    false,
		RecoveryChecked: true,
	})
	if !critical {
		t.Error("missing pg_monitor must be critical")
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "GRANT pg_monitor TO") {
		t.Errorf("verify report should hand the user the exact GRANT:\n%s", joined)
	}
}

func TestInitVerifyReportMissingPgss(t *testing.T) {
	lines, critical := initVerifyReport(conn.Capabilities{
		Database:          "yourdb",
		HasPgMonitor:      true,
		HasStatStatements: false,
		Provider:          conn.ProviderRDS,
		RecoveryChecked:   true,
	})
	if critical {
		t.Errorf("missing pg_stat_statements is a warning, not critical:\n%s", strings.Join(lines, "\n"))
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "parameter group") {
		t.Errorf("verify report should carry the provider fix:\n%s", joined)
	}
}

func TestInitVerifyReportStandby(t *testing.T) {
	lines, _ := initVerifyReport(conn.Capabilities{
		Database:          "yourdb",
		HasPgMonitor:      true,
		HasStatStatements: true,
		RecoveryChecked:   true,
		InRecovery:        true,
	})
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "standby") {
		t.Errorf("verify report should note the standby (per-node counters caveat):\n%s", joined)
	}
}

func TestInitVerifyReportCockroachDB(t *testing.T) {
	lines, critical := initVerifyReport(conn.Capabilities{
		Engine: conn.EngineCockroachDB, VersionText: "CockroachDB CCL v26.4.0",
		Database: "defaultdb", HasViewActivity: true, HasCRDBStmtStats: true, HasCRDBInsights: true,
	})
	joined := strings.Join(lines, "\n")
	if critical || !strings.Contains(joined, "VIEWACTIVITY") || !strings.Contains(joined, "statement statistics") || !strings.Contains(joined, "execution insights") {
		t.Fatalf("CockroachDB activity privilege should verify: critical=%v\n%s", critical, strings.Join(lines, "\n"))
	}
}
