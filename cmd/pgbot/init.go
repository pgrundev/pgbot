package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/spf13/cobra"
)

// newInitCmd generates the setup SQL for the read-only monitoring role — it
// NEVER executes anything. "pgbot never writes" is the product boundary, and
// init stays on the right side of it: the output is designed to be reviewed
// and piped to psql by the user's own admin session. With a connection string
// it connects (read-only, as always) only to learn the database name and
// provider so the SQL and the pg_stat_statements steps come out tailored;
// --verify instead checks an existing role against the prerequisites.
func newInitCmd() *cobra.Command {
	var role, database string
	var verify bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "init [connection-string]",
		Short: "Generate the read-only role setup SQL (nothing is executed)",
		Long: "Print the SQL that creates the read-only role pgbot connects as: pg_monitor\n" +
			"on PostgreSQL or VIEWACTIVITY on CockroachDB. pgbot never writes: review\n" +
			"the output, set a real password, and run it yourself, e.g.\n" +
			"  pgbot init \"postgres://admin@host/db\" | psql \"postgres://admin@host/db\"\n" +
			"With a connection string, the database name and provider are detected so the\n" +
			"output is tailored; without one, placeholders are used.\n" +
			"--verify connects as the monitoring role instead and checks the prerequisites\n" +
			"for the detected database engine.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			connString := firstNonEmpty(argAt(args, 0), os.Getenv("DATABASE_URL"), os.Getenv("PGBOT_DATABASE_URL"))

			if verify {
				if connString == "" {
					return fmt.Errorf("--verify needs a connection string (argument or $DATABASE_URL)")
				}
				ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
				defer cancel()
				t, err := conn.Connect(ctx, connString)
				if err != nil {
					return err
				}
				defer t.Close()
				lines, critical := initVerifyReport(t.Caps)
				fmt.Fprintln(cmd.OutOrStdout(), strings.Join(lines, "\n"))
				if critical {
					return fmt.Errorf("verification failed — fix the FAIL line above and re-run")
				}
				return nil
			}

			provider := conn.ProviderUnknown
			engine := conn.EnginePostgreSQL
			db := database
			if connString != "" {
				ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
				defer cancel()
				t, err := conn.Connect(ctx, connString)
				if err != nil {
					return err
				}
				db = firstNonEmpty(database, t.Caps.Database)
				provider = t.Caps.Provider
				engine = t.Caps.EngineName()
				t.Close()
			}
			if db == "" {
				db = "yourdb"
			}
			fmt.Fprint(cmd.OutOrStdout(), initSQLForEngine(role, db, provider, engine))
			return nil
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&role, "role", "pgbot_ro", "name of the monitoring role to create")
	fl.StringVar(&database, "database", "", "database to grant CONNECT on (default: detected, else a placeholder)")
	fl.BoolVar(&verify, "verify", false, "connect as the monitoring role and check the prerequisites instead")
	fl.DurationVar(&timeout, "timeout", 30*time.Second, "connection budget for detection/verification")
	return cmd
}

// initSQL renders the canonical Setup SQL (the same three statements the
// README documents), followed by the provider-appropriate pg_stat_statements
// step. Contract, pinned by tests: every line is a statement, a -- comment, or
// blank, so `pgbot init | psql` can never run anything unreviewed prose-shaped.
func initSQL(role, database string, p conn.Provider) string {
	return initSQLForEngine(role, database, p, conn.EnginePostgreSQL)
}

func initSQLForEngine(role, database string, p conn.Provider, engine conn.Engine) string {
	var b strings.Builder
	b.WriteString("-- pgbot init — read-only monitoring role (generated, NOT executed)\n")
	b.WriteString("-- Review, replace the password placeholder, then run as an admin:\n")
	b.WriteString("--   pgbot init | psql \"$ADMIN_DSN\"\n")
	b.WriteString("\n")
	fmt.Fprintf(&b, "CREATE ROLE %s LOGIN PASSWORD 'REPLACE-WITH-A-STRONG-PASSWORD';\n", role)
	if engine == conn.EngineCockroachDB {
		fmt.Fprintf(&b, "GRANT CONNECT ON DATABASE %s TO %s;\n", database, role)
		fmt.Fprintf(&b, "GRANT SYSTEM VIEWACTIVITY TO %s;\n", role)
		b.WriteString("\n-- CockroachDB preview collects read-only SQL workload diagnostics.\n")
		fmt.Fprintf(&b, "-- export DATABASE_URL=\"postgresql://%s:REPLACE-WITH-A-STRONG-PASSWORD@HOST:26257/%s?sslmode=verify-full\"\n", role, database)
		return b.String()
	}
	fmt.Fprintf(&b, "GRANT pg_monitor TO %s;\n", role)
	fmt.Fprintf(&b, "GRANT CONNECT ON DATABASE %s TO %s;\n", database, role)
	b.WriteString("\n")

	b.WriteString("-- pg_stat_statements powers the queries section.\n")
	switch p {
	case conn.ProviderSupabase, conn.ProviderNeon:
		b.WriteString("-- Preloaded on this provider, so enabling it is one statement:\n")
		b.WriteString("CREATE EXTENSION IF NOT EXISTS pg_stat_statements;\n")
	default:
		// Needs shared_preload_libraries first — a bare CREATE EXTENSION here
		// would just error in the pipe, so the steps stay as comments.
		fmt.Fprintf(&b, "-- To enable it: %s\n", p.PgssInstructions())
	}
	b.WriteString("\n")

	b.WriteString("-- Then point pgbot at the database and take the first reading:\n")
	fmt.Fprintf(&b, "-- export DATABASE_URL=\"postgres://%s:<password>@<host>:5432/%s?sslmode=require\"\n", role, database)
	b.WriteString("-- pgbot inspect\n")
	return b.String()
}

// initVerifyReport turns the connect-time probe into a prerequisite checklist.
// pg_monitor is the one critical requirement — without it pgbot silently sees
// partial data everywhere. pg_stat_statements only degrades the queries
// section, so it warns; a standby only earns the per-node-counters note.
func initVerifyReport(caps conn.Capabilities) (lines []string, critical bool) {
	if caps.VersionText != "" {
		lines = append(lines, fmt.Sprintf("  ok    connected — %s, database %q", caps.VersionText, caps.Database))
	}
	if caps.Provider != "" && caps.Provider != conn.ProviderUnknown {
		lines = append(lines, fmt.Sprintf("  ok    provider detected: %s", caps.Provider))
	}
	if caps.IsCockroachDB() {
		if caps.HasViewActivity {
			lines = append(lines, "  ok    role holds VIEWACTIVITY or VIEWACTIVITYREDACTED — cluster activity is visible")
		} else {
			critical = true
			lines = append(lines, "  FAIL  role lacks VIEWACTIVITY — only its own sessions are visible.")
			lines = append(lines, "        Fix (as an admin): GRANT SYSTEM VIEWACTIVITY TO <role>;")
		}
		if caps.HasCRDBStmtStats {
			lines = append(lines, "  ok    persisted statement statistics available")
		} else {
			lines = append(lines, "  note  persisted statement statistics are unavailable on this CockroachDB version")
		}
		if caps.HasCRDBInsights {
			lines = append(lines, "  ok    persisted execution insights available")
		} else {
			lines = append(lines, "  note  persisted execution insights are unavailable on this CockroachDB version")
		}
		lines = append(lines, "  note  CockroachDB preview covers SQL workload diagnostics; cluster health coverage is partial")
		return lines, critical
	}

	if caps.HasPgMonitor {
		lines = append(lines, "  ok    role holds pg_monitor — full statistics visibility")
	} else {
		critical = true
		lines = append(lines, "  FAIL  role lacks pg_monitor — pgbot would see only partial data.")
		lines = append(lines, "        Fix (as an admin): GRANT pg_monitor TO <role>;")
	}

	if caps.HasStatStatements {
		lines = append(lines, "  ok    pg_stat_statements installed — queries section available")
	} else {
		lines = append(lines, "  warn  pg_stat_statements missing — the queries section will be unavailable.")
		lines = append(lines, fmt.Sprintf("        Fix: %s", caps.Provider.PgssInstructions()))
	}

	if caps.Standby() {
		lines = append(lines, "  note  this is a standby — index/table scan counters are per-node;")
		lines = append(lines, "        \"unused\" here can still be used on the primary (and vice versa).")
	} else if caps.Primary() {
		lines = append(lines, "  ok    primary node")
	}
	return lines, critical
}
