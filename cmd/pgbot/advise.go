package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pgrundev/pgbot/internal/advisor"
	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/render"
	"github.com/spf13/cobra"
)

type adviseFlags struct {
	json           bool
	noColor        bool
	top            int
	minImprovement float64
	timeout        time.Duration
}

func newAdviseCmd() *cobra.Command {
	var f adviseFlags
	cmd := &cobra.Command{
		Use:   "advise <connection-string>",
		Short: "Suggest indexes, each validated by the planner with hypopg (nothing is built)",
		Long: "Reads the slowest queries from pg_stat_statements, derives candidate indexes from\n" +
			"the planner's own sequential-scan filters, and VALIDATES each one: it creates the\n" +
			"index hypothetically with hypopg, re-plans the query, and only recommends it if the\n" +
			"planner actually switches to it and the estimated cost drops. Nothing is ever built.\n\n" +
			"Requires the hypopg extension, pg_stat_statements, and PostgreSQL 16+ (for\n" +
			"EXPLAIN (GENERIC_PLAN)). Everything runs in a READ ONLY transaction; pgbot only\n" +
			"plans your query — it never executes it.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAdvise(cmd, args, f)
		},
	}
	fl := cmd.Flags()
	fl.BoolVar(&f.json, "json", false, "emit recommendations as JSON")
	fl.BoolVar(&f.noColor, "no-color", false, "disable ANSI color")
	fl.IntVar(&f.top, "top", 15, "how many of the slowest queries to consider")
	fl.Float64Var(&f.minImprovement, "min-improvement", 0.5, "required estimated cost drop (0.5 = 50%)")
	fl.DurationVar(&f.timeout, "timeout", 60*time.Second, "total wall-clock budget")
	return cmd
}

// adviseResult bundles what an advise run produces, for both the CLI and MCP.
type adviseResult struct {
	Recs       []advisor.Recommendation
	Stats      advisor.Stats
	Database   string
	VersionNum int
	Reason     string // non-empty when the advisor can't run (degrade, not error)
}

// adviseRun connects, checks availability, and runs the deterministic-candidate →
// hypopg-validation loop inside one READ ONLY transaction. Shared by `pgbot
// advise` and the MCP suggest_indexes tool.
func adviseRun(ctx context.Context, connString string, top int, minImpr float64) (adviseResult, error) {
	target, err := conn.Connect(ctx, connString)
	if err != nil {
		return adviseResult{}, fmt.Errorf("connect: %s", conn.RedactConnString(err.Error()))
	}
	defer target.Close()

	res := adviseResult{Database: target.Caps.Database, VersionNum: target.Caps.VersionNum}
	if reason := adviseUnavailable(target.Caps); reason != "" {
		res.Reason = reason
		return res, nil
	}
	inputs, err := topSlowSelects(ctx, target, top)
	if err != nil {
		return adviseResult{}, fmt.Errorf("read pg_stat_statements: %s", conn.RedactConnString(err.Error()))
	}
	// A cost estimate is only as good as the stats behind it, and a new index taxes
	// every write — so gather which tables have stale stats (refuse) and which are
	// write-heavy (caveat). Best-effort: an error just means no extra guards.
	stale, writeHeavy := relationStats(ctx, target)

	// One READ ONLY transaction for the whole loop: hypopg state is per-connection,
	// so the hypothetical indexes and their reset must share a single connection.
	err = target.ReadOnlyTx(ctx, func(tx pgx.Tx) error {
		p := pgxPlanner{tx: tx, caps: target.Caps}
		defer p.ResetHypo(context.Background()) //nolint:errcheck // belt-and-braces cleanup
		res.Recs, res.Stats = advisor.Advise(ctx, p, inputs, advisor.Options{
			MinImprovement: minImpr, StaleRelations: stale, WriteHeavy: writeHeavy,
		})
		return nil
	})
	if err != nil {
		return adviseResult{}, fmt.Errorf("advise: %s", conn.RedactConnString(err.Error()))
	}
	return res, nil
}

func runAdvise(cmd *cobra.Command, args []string, f adviseFlags) error {
	connString := firstNonEmpty(argAt(args, 0), os.Getenv("DATABASE_URL"), os.Getenv("PGBOT_DATABASE_URL"), pgServiceFallback())
	if connString == "" {
		return fmt.Errorf("no connection string (pass one or set $DATABASE_URL)")
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), f.timeout)
	defer cancel()

	res, err := adviseRun(ctx, connString, f.top, f.minImprovement)
	if err != nil {
		return err
	}
	if res.Reason != "" {
		fmt.Fprintln(os.Stderr, "pgbot advise: "+res.Reason)
		return nil
	}
	recs, stats := res.Recs, res.Stats

	if f.json {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{
			"recommendations": recs,
			"considered":      stats.QueriesConsidered,
			"planned":         stats.QueriesPlanned,
			"candidates":      stats.CandidatesTested,
			"skipped_stale":   stats.SkippedStaleStats,
		})
	}
	render.AdvisorReport(os.Stdout, render.AdvisorInput{
		Color: useColor(f.noColor), Database: res.Database, VersionNum: res.VersionNum,
		Recommendations: recs, Considered: stats.QueriesConsidered, Planned: stats.QueriesPlanned,
		Candidates: stats.CandidatesTested, SkippedStale: stats.SkippedStaleStats,
	})
	return nil
}

// suggestIndexesTool is the MCP counterpart to `pgbot advise`: validated index
// recommendations as JSON (or a `reason` when the advisor can't run).
func suggestIndexesTool(ctx context.Context, args json.RawMessage) (string, error) {
	dsn, err := dsnFromArgs(args)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 50*time.Second)
	defer cancel()
	res, err := adviseRun(ctx, dsn, 15, 0.5)
	if err != nil {
		return "", err
	}
	out := map[string]any{
		"database":        res.Database,
		"recommendations": res.Recs,
		"considered":      res.Stats.QueriesConsidered,
		"planned":         res.Stats.QueriesPlanned,
		"candidates":      res.Stats.CandidatesTested,
		"skipped_stale":   res.Stats.SkippedStaleStats,
		"note":            "hypothetical validation with hypopg — nothing was built; recommend, don't act",
	}
	if res.Reason != "" {
		out["available"] = false
		out["reason"] = res.Reason
	} else {
		out["available"] = true
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// adviseUnavailable returns a human reason the advisor can't run, or "".
func adviseUnavailable(caps conn.Capabilities) string {
	switch {
	case caps.VersionNum < 160000:
		return fmt.Sprintf("needs PostgreSQL 16+ for EXPLAIN (GENERIC_PLAN); this server is %s", caps.VersionText)
	case !caps.HasStatStatements:
		return "needs pg_stat_statements — " + caps.Provider.PgssInstructions()
	case !caps.HasHypopg:
		return "needs the hypopg extension (CREATE EXTENSION hypopg;) to validate candidate indexes without building them"
	}
	return ""
}

// topSlowSelects pulls the slowest plain-SELECT queries from pg_stat_statements
// with their RAW normalized text (used only to plan — never stored) and a
// scrubbed copy for display. Utility statements and DML are excluded at the SQL
// level, so no verbatim literal text is fetched.
func topSlowSelects(ctx context.Context, t *conn.Target, top int) ([]advisor.QueryInput, error) {
	if top <= 0 {
		top = 15
	}
	col := t.Caps.StatStatementsTotalCol()
	// col comes from a fixed allowlist (StatStatementsTotalCol), never user input;
	// the view is addressed by its schema-qualified name (Pgss) so a
	// pg_stat_statements installed outside public (Supabase's "extensions") is
	// found regardless of the role's search_path (issue #10).
	sql := fmt.Sprintf(`
		SELECT queryid, left(query, 4000) AS q, calls,
		       100.0 * %[1]s / nullif(sum(%[1]s) OVER (), 0) AS share
		FROM %[2]s
		WHERE queryid IS NOT NULL AND calls > 0
		  AND query ~* '^\s*select\M'
		ORDER BY %[1]s DESC
		LIMIT $1`, col, t.Caps.Pgss("pg_stat_statements"))

	rows, err := t.Pool.Query(ctx, sql, top)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []advisor.QueryInput
	for rows.Next() {
		var qid int64
		var text string
		var calls int64
		var share float64
		if err := rows.Scan(&qid, &text, &calls, &share); err != nil {
			return nil, err
		}
		out = append(out, advisor.QueryInput{
			QueryID: qid, Text: text, Scrubbed: conn.ScrubQueryText(text), Calls: calls, SharePct: share,
		})
	}
	return out, rows.Err()
}

// relationStats returns, from pg_stat_user_tables, the tables whose planner
// statistics are stale or missing (advise refuses them — a cost estimate over bad
// stats is meaningless) and those that are write-heavy (advise caveats them — a
// new index is maintained on every write). Both keyed "schema.table".
func relationStats(ctx context.Context, t *conn.Target) (stale, writeHeavy map[string]bool) {
	stale, writeHeavy = map[string]bool{}, map[string]bool{}
	rows, err := t.Pool.Query(ctx, `
		SELECT schemaname || '.' || relname AS rel,
		       (last_analyze IS NULL AND last_autoanalyze IS NULL)
		         OR n_mod_since_analyze > 0.2 * n_live_tup + 50           AS stale,
		       (n_tup_ins + n_tup_upd + n_tup_del) > 100000               AS write_heavy
		FROM pg_stat_user_tables`)
	if err != nil {
		return stale, writeHeavy
	}
	defer rows.Close()
	for rows.Next() {
		var rel string
		var isStale, isWrite bool
		if rows.Scan(&rel, &isStale, &isWrite) != nil {
			continue
		}
		if isStale {
			stale[rel] = true
		}
		if isWrite {
			writeHeavy[rel] = true
		}
	}
	return stale, writeHeavy
}

// pgxPlanner implements advisor.Planner over a READ ONLY pgx transaction. Every
// statement is plan-only or hypothetical — none executes the inspected query.
//
// Each fallible operation is wrapped in a SAVEPOINT: a query pgbot can't plan (or
// a candidate hypopg rejects) raises a server-side error that would otherwise
// abort the whole transaction and fail the run. Rolling back to the savepoint
// clears the error and lets the loop continue to the next query. hypopg indexes
// live in backend memory, not transaction state, so they survive a savepoint
// rollback — only hypopg_reset drops them.
type pgxPlanner struct {
	tx   pgx.Tx
	caps conn.Capabilities // for hypopg's schema — its functions are called by qualified name
}

// hypo returns the schema-qualified name of a hypopg function. Like
// pg_stat_statements, hypopg lands in Supabase's "extensions" schema (issue #10)
// — off a read-only role's search_path — so the fixed function names are
// qualified with the namespace the probe read from pg_extension.
func (p pgxPlanner) hypo(fn string) string { return p.caps.ExtObject("hypopg", fn) }

func (p pgxPlanner) GenericPlan(ctx context.Context, query string) ([]byte, error) {
	// GENERIC_PLAN plans a normalized $N query without values; VERBOSE preserves
	// each resolved relation's schema in FORMAT JSON, which gives one row. This
	// MUST use the raw simple-query protocol (PgConn.Exec): both the
	// extended protocol and pgx's SimpleProtocol mode treat the $1/$2 inside the
	// EXPLAIN'd query as bind parameters of the OUTER statement and demand values
	// ("expected 2 arguments, got 0"). GENERIC_PLAN exists precisely to plan those
	// placeholders WITHOUT values, so the SQL must reach the server byte-for-byte.
	var js []byte
	err := p.inSavepoint(ctx, func() error {
		res, err := p.tx.Conn().PgConn().Exec(ctx, "EXPLAIN (GENERIC_PLAN, VERBOSE, FORMAT JSON) "+query).ReadAll()
		if err != nil {
			return err
		}
		for _, r := range res {
			if len(r.Rows) > 0 && len(r.Rows[0]) > 0 {
				js = r.Rows[0][0]
				return nil
			}
		}
		return fmt.Errorf("EXPLAIN returned no rows")
	})
	return js, err
}

func (p pgxPlanner) CreateHypoIndex(ctx context.Context, ddl string) (string, int64, error) {
	var oid uint32
	var name string
	var estBytes int64
	err := p.inSavepoint(ctx, func() error {
		// The DDL is passed as a bind parameter to hypopg_create_index — not spliced.
		if err := p.tx.QueryRow(ctx, "SELECT indexrelid, indexname FROM "+p.hypo("hypopg_create_index")+"($1)", ddl).Scan(&oid, &name); err != nil {
			return err
		}
		// hypopg's size estimate for the index that would be built (best-effort).
		_ = p.tx.QueryRow(ctx, "SELECT "+p.hypo("hypopg_relation_size")+"($1)", oid).Scan(&estBytes)
		return nil
	})
	return name, estBytes, err
}

func (p pgxPlanner) ResetHypo(ctx context.Context) error {
	_, err := p.tx.Exec(ctx, "SELECT "+p.hypo("hypopg_reset")+"()")
	return err
}

// inSavepoint runs fn between a SAVEPOINT and its RELEASE, rolling back to the
// savepoint on error so a single failed statement doesn't poison the transaction.
func (p pgxPlanner) inSavepoint(ctx context.Context, fn func() error) error {
	if _, err := p.tx.Exec(ctx, "SAVEPOINT pgbot_adv"); err != nil {
		return err
	}
	if err := fn(); err != nil {
		_, _ = p.tx.Exec(ctx, "ROLLBACK TO SAVEPOINT pgbot_adv")
		_, _ = p.tx.Exec(ctx, "RELEASE SAVEPOINT pgbot_adv")
		return err
	}
	_, err := p.tx.Exec(ctx, "RELEASE SAVEPOINT pgbot_adv")
	return err
}
